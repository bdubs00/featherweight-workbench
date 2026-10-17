package mcp

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// keyLeaks reports whether a log holds a key in any common encoding.
func keyLeaks(log string, key []byte) bool {
	for _, enc := range []string{string(key), hex.EncodeToString(key), base64.StdEncoding.EncodeToString(key), base64.RawURLEncoding.EncodeToString(key)} {
		if strings.Contains(log, enc) {
			return true
		}
	}
	return false
}

// keyAgent is an agent with a home and a log that the test reads.
func keyAgent(t *testing.T, home string, opts ...fw.Option) (*fw.Agent, *syncBuffer) {
	t.Helper()
	cfg := testConfig(t)
	cfg.Home = home
	var buf syncBuffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	return newAgent(t, newScript().model(), append([]fw.Option{fw.WithConfig(cfg), fw.WithLogger(log)}, opts...)...), &buf
}

func TestStateKeyFile(t *testing.T) {
	t.Setenv(stateKeySecret, "")
	t.Setenv(stateKeySecret+"_FILE", "")
	home := filepath.Join(t.TempDir(), "home")
	a, buf := keyAgent(t, home)
	if _, err := NewServer(a); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, stateKeyFile)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("the key file: %v", err)
	}
	if info.Mode().Perm() != 0o600 || info.Size() != stateKeyBytes {
		t.Fatalf("the key file has mode %v and %d bytes", info.Mode().Perm(), info.Size())
	}
	key, _ := os.ReadFile(path)
	log := buf.String()
	if strings.Count(log, "replicas behind one endpoint must share "+stateKeySecret) != 1 || !strings.Contains(log, "level=WARN") || !strings.Contains(log, path) {
		t.Errorf("log = %q", log)
	}
	if keyLeaks(log, key) {
		t.Fatal("the log holds the key")
	}
	if left, _ := filepath.Glob(filepath.Join(home, "."+stateKeyFile+"-*")); len(left) != 0 {
		t.Errorf("temporary files stay: %v", left)
	}

	// A restart of the process reads the same key, so a signed retry from
	// before the restart opens.
	before := &server{a: a, key: stateKey(a, quietLogger()), stateTTL: time.Hour}
	state := before.sealState("run1", "ap1", "alice")
	b, _ := keyAgent(t, home)
	after := &server{a: b, key: stateKey(b, quietLogger()), stateTTL: time.Hour}
	if !bytes.Equal(before.key, key) || !bytes.Equal(after.key, key) {
		t.Fatal("the key changed across a restart")
	}
	if got, err := after.openState(state, "alice"); err != nil || got.Run != "run1" {
		t.Fatalf("open after a restart = %+v, %v", got, err)
	}
	// Another home is another replica: it has its own key.
	other, _ := keyAgent(t, t.TempDir())
	if k := stateKey(other, quietLogger()); bytes.Equal(k, key) {
		t.Fatal("two homes share a key")
	}
}

func TestStateKeySecretWins(t *testing.T) {
	t.Setenv(stateKeySecret, "a shared secret")
	home := t.TempDir()
	a, buf := keyAgent(t, home, fw.WithSecrets(fw.EnvSecrets{}))
	key := stateKey(a, a.Logger())
	sum := sha256.Sum256([]byte("a shared secret"))
	if !bytes.Equal(key, sum[:]) {
		t.Fatal("the key is not the hash of the secret")
	}
	if _, err := os.Stat(filepath.Join(home, stateKeyFile)); !os.IsNotExist(err) {
		t.Errorf("a key file exists with the secret set: %v", err)
	}
	if log := buf.String(); strings.Contains(log, "replicas") || strings.Contains(log, "a shared secret") {
		t.Errorf("log = %q", log)
	}
}

func TestStateKeyInMemory(t *testing.T) {
	t.Setenv(stateKeySecret, "")
	t.Setenv(stateKeySecret+"_FILE", "")
	// The home is a file, so no key file can live under it.
	blocked := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocked, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	a, buf := keyAgent(t, blocked)
	key := stateKey(a, a.Logger())
	if len(key) != stateKeyBytes {
		t.Fatalf("key has %d bytes", len(key))
	}
	log := buf.String()
	if !strings.Contains(log, "in memory only") || !strings.Contains(log, "replicas behind one endpoint must share") || !strings.Contains(log, "error=") {
		t.Errorf("log = %q", log)
	}
	if keyLeaks(log, key) {
		t.Fatal("the log holds the key")
	}

	// A damaged key file is never used and never replaced.
	home := t.TempDir()
	path := filepath.Join(home, stateKeyFile)
	if err := os.WriteFile(path, []byte("short"), 0o600); err != nil {
		t.Fatal(err)
	}
	b, buf := keyAgent(t, home)
	if k := stateKey(b, b.Logger()); bytes.Equal(k[:5], []byte("short")) {
		t.Fatal("a damaged key signs")
	}
	if got, _ := os.ReadFile(path); string(got) != "short" {
		t.Errorf("the damaged file changed: %q", got)
	}
	if log := buf.String(); !strings.Contains(log, "holds 5 bytes, not 32") {
		t.Errorf("log = %q", log)
	}
}

func TestLoadStateKeyAgreesUnderRace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "deep", stateKeyFile)
	const n = 8
	keys := make([][]byte, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			keys[i], errs[i] = loadStateKey(path)
		}()
	}
	wg.Wait()
	for i := range n {
		if errs[i] != nil || !bytes.Equal(keys[i], keys[0]) || len(keys[i]) != stateKeyBytes {
			t.Fatalf("process %d: a key of %d bytes that differs from the first, err %v", i, len(keys[i]), errs[i])
		}
	}
	if info, err := os.Stat(filepath.Dir(path)); err != nil || info.Mode().Perm() != 0o700 {
		t.Errorf("the directory = %v, %v", info, err)
	}
}
