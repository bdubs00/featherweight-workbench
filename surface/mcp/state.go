package mcp

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// stateKeySecret names the secret that signs requestState. Replicas behind
// one endpoint share it, so that a retry reaches any replica.
const stateKeySecret = "FW_MCP_STATE_KEY"

// stateKeyFile is the file under the home directory that holds the key
// when the secret is not set, and stateKeyBytes the length of that key.
const (
	stateKeyFile  = "mcp-state.key"
	stateKeyBytes = 32
)

// approvalState is the context of a pending approval that travels to the
// client in requestState and back in the retry of run.
type approvalState struct {
	Run       string `json:"r"`
	Approval  string `json:"a"`
	Principal string `json:"p,omitempty"`
	Expires   int64  `json:"e"` // Unix seconds
}

// stateKey returns the key that signs requestState: a hash of the secret
// FW_MCP_STATE_KEY when it is set. Else it is the random key in the file
// mcp-state.key under the home directory, which stateKey creates with mode
// 0600 when it is missing, so a requestState survives a restart of one
// process. When the file cannot be read or written, the key is random and
// lives in memory only. Log lines name the file and the cause, never the
// key.
func stateKey(a *fw.Agent, log *slog.Logger) []byte {
	if sec := a.Secrets(); sec != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if v, err := sec.Get(ctx, stateKeySecret); err == nil && v != "" {
			sum := sha256.Sum256([]byte(v))
			return sum[:]
		}
	}
	home := a.Config().Home
	if home == "" {
		home = fw.DefaultHome()
	}
	path := filepath.Join(home, stateKeyFile)
	log.Warn("mcp: the secret "+stateKeySecret+" is not set, so the key of requestState comes from a file; replicas behind one endpoint must share "+stateKeySecret,
		"file", path)
	key, err := loadStateKey(path)
	if err != nil {
		log.Warn("mcp: the key of requestState lives in memory only, so a retry after a restart fails; decide with the approve tool then",
			"file", path, "error", err)
		return randomKey()
	}
	return key
}

// loadStateKey reads the key in path, or creates it when the file does not
// exist. Two processes that create it at once agree on one key: the file
// appears whole through a hard link, and the process that loses reads the
// key of the other.
func loadStateKey(path string) ([]byte, error) {
	key, err := readStateKey(path)
	if !errors.Is(err, fs.ErrNotExist) {
		return key, err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("mcp: create the directory of the state key: %w", err)
	}
	tmp, err := os.CreateTemp(dir, "."+stateKeyFile+"-*")
	if err != nil {
		return nil, fmt.Errorf("mcp: create the state key: %w", err)
	}
	defer os.Remove(tmp.Name())
	key = randomKey()
	_, werr := tmp.Write(key)
	if werr == nil {
		werr = tmp.Sync()
	}
	if cerr := tmp.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		return nil, fmt.Errorf("mcp: write the state key: %w", werr)
	}
	switch err := os.Link(tmp.Name(), path); {
	case err == nil:
		return key, nil
	case errors.Is(err, fs.ErrExist):
		return readStateKey(path) // another process created it first
	}
	// A file system without hard links gets a rename, which replaces a
	// file that another process created in the same instant.
	if err := os.Rename(tmp.Name(), path); err != nil {
		return nil, fmt.Errorf("mcp: write the state key: %w", err)
	}
	return key, nil
}

// readStateKey reads a key file. A file of another length is an error, so
// a damaged key never signs.
func readStateKey(path string) ([]byte, error) {
	key, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
		return nil, fmt.Errorf("mcp: read the state key: %w", err)
	}
	if len(key) != stateKeyBytes {
		return nil, fmt.Errorf("mcp: the state key file holds %d bytes, not %d; remove it to make a new key", len(key), stateKeyBytes)
	}
	return key, nil
}

// randomKey returns a random key of stateKeyBytes.
func randomKey() []byte {
	key := make([]byte, stateKeyBytes)
	if _, err := rand.Read(key); err != nil {
		panic(err)
	}
	return key
}

// sealState signs the context of a pending approval.
func (s *server) sealState(runID, approvalID, principal string) string {
	b, _ := json.Marshal(approvalState{
		Run:       runID,
		Approval:  approvalID,
		Principal: principal,
		Expires:   time.Now().Add(s.stateTTL).Unix(),
	})
	body := base64.RawURLEncoding.EncodeToString(b)
	return body + "." + base64.RawURLEncoding.EncodeToString(s.mac(body))
}

// openState verifies a requestState. It rejects a bad signature, an expired
// state, and a state that another principal received.
func (s *server) openState(state, principal string) (approvalState, error) {
	body, sig, ok := strings.Cut(state, ".")
	if !ok {
		return approvalState{}, errors.New("mcp: requestState is malformed")
	}
	got, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil || !hmac.Equal(got, s.mac(body)) {
		return approvalState{}, errors.New("mcp: requestState has a bad signature")
	}
	raw, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil {
		return approvalState{}, errors.New("mcp: requestState is malformed")
	}
	var st approvalState
	if err := json.Unmarshal(raw, &st); err != nil || st.Run == "" || st.Approval == "" {
		return approvalState{}, errors.New("mcp: requestState is malformed")
	}
	if time.Now().Unix() > st.Expires {
		return approvalState{}, errors.New("mcp: requestState has expired; read the run with run_status and decide with approve")
	}
	if st.Principal != principal {
		return approvalState{}, errors.New("mcp: requestState belongs to another principal")
	}
	return st, nil
}

func (s *server) mac(body string) []byte {
	m := hmac.New(sha256.New, s.key)
	m.Write([]byte(body))
	return m.Sum(nil)
}
