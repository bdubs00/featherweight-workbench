package connections

// This file keeps the grants of people: a Grant is a refresh token and what
// it was issued for, and a Store holds grants durably (docs/ARCHITECTURE.md
// section 11). The file store encrypts each grant, and the memory store
// serves tests and a process that must forget every grant when it stops.
// No grant, refresh token, or access token enters a ledger event, a log
// line, or an error message.

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// Errors of the store.
var (
	// ErrGrantNotFound means that the store holds no grant for the key.
	ErrGrantNotFound = errors.New("connections: grant not found")
	// ErrGrantConflict means that the grant changed after the caller read
	// it, so a Put with that revision did not write.
	ErrGrantConflict = errors.New("connections: grant changed")
)

// Names of the file store.
const (
	keyFile  = "grant.key"
	keyLabel = "fw grant store v1"
)

// Grant is the authority that a person gave the workbench at one authorization
// server: a refresh token and what it was issued for. Subject is the
// fw.Principal.Subject of the person, which is also the principal of the
// runs that the person starts. Account is the name of the person at the
// service, for display only. Expires is zero when the server did not say
// when the refresh token ends.
type Grant struct {
	Subject      string    `json:"subject"`
	Issuer       string    `json:"issuer"`
	ClientID     string    `json:"clientId"`
	RefreshToken string    `json:"refreshToken"`
	Scopes       []string  `json:"scopes,omitempty"`
	Account      string    `json:"account,omitempty"`
	Created      time.Time `json:"created"`
	Refreshed    time.Time `json:"refreshed,omitzero"`
	Expires      time.Time `json:"expires,omitzero"`
}

// Key returns the key of the grant.
func (g Grant) Key() GrantKey {
	return GrantKey{Subject: g.Subject, Issuer: g.Issuer, ClientID: g.ClientID}
}

// LogValue keeps the refresh token out of every log line.
func (g Grant) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("subject", g.Subject),
		slog.String("issuer", g.Issuer),
		slog.String("clientId", g.ClientID),
	)
}

// String keeps the refresh token out of every formatted value.
func (g Grant) String() string {
	return fmt.Sprintf("grant{subject=%s issuer=%s clientId=%s}", g.Subject, g.Issuer, g.ClientID)
}

// GoString keeps the refresh token out of %#v.
func (g Grant) GoString() string { return g.String() }

// GrantKey names one grant: a person, an authorization server, and the
// client that the server issued the grant to.
type GrantKey struct {
	Subject  string `json:"subject"`
	Issuer   string `json:"issuer"`
	ClientID string `json:"clientId"`
}

// Store keeps grants. Every revision is a number that grows with each
// write of a key. Get returns ErrGrantNotFound for a missing key. Put with
// prev 0 writes in any case. Put with prev other than 0 writes only when
// the stored revision is prev, else it returns ErrGrantConflict. Put
// returns the new revision only after the grant is durable. List returns
// the grants of one person, sorted by issuer and client.
type Store interface {
	Get(ctx context.Context, key GrantKey) (Grant, uint64, error)
	Put(ctx context.Context, g Grant, prev uint64) (uint64, error)
	Delete(ctx context.Context, key GrantKey) error
	List(ctx context.Context, subject string) ([]Grant, error)
}

// MemStore keeps grants in memory. Tests use it, and a process that
// must forget every grant when it stops.
type MemStore struct {
	mu   sync.Mutex
	m    map[GrantKey]Grant
	revs map[GrantKey]uint64
}

// NewMemStore returns an empty in-memory grant store.
func NewMemStore() *MemStore {
	return &MemStore{m: map[GrantKey]Grant{}, revs: map[GrantKey]uint64{}}
}

// Get implements Store.
func (s *MemStore) Get(_ context.Context, key GrantKey) (Grant, uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	g, ok := s.m[key]
	if !ok {
		return Grant{}, 0, ErrGrantNotFound
	}
	return cloneGrant(g), s.revs[key], nil
}

// Put implements Store.
func (s *MemStore) Put(_ context.Context, g Grant, prev uint64) (uint64, error) {
	key := g.Key()
	s.mu.Lock()
	defer s.mu.Unlock()
	if prev != 0 && s.revs[key] != prev {
		return 0, ErrGrantConflict
	}
	s.revs[key]++
	s.m[key] = cloneGrant(g)
	return s.revs[key], nil
}

// Delete implements Store.
func (s *MemStore) Delete(_ context.Context, key GrantKey) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, key)
	s.revs[key]++ // a later Put with an old revision fails
	return nil
}

// List implements Store.
func (s *MemStore) List(_ context.Context, subject string) ([]Grant, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Grant
	for k, g := range s.m {
		if k.Subject == subject {
			out = append(out, cloneGrant(g))
		}
	}
	sortGrants(out)
	return out, nil
}

func cloneGrant(g Grant) Grant {
	g.Scopes = slices.Clone(g.Scopes)
	return g
}

func sortGrants(gs []Grant) {
	sort.Slice(gs, func(i, j int) bool {
		if gs[i].Issuer != gs[j].Issuer {
			return gs[i].Issuer < gs[j].Issuer
		}
		return gs[i].ClientID < gs[j].ClientID
	})
}

// FileStore keeps each grant in one file, encrypted with AES-256-GCM.
// A write goes to a temporary file, which is synced and then renamed over
// the old file, and the directory is synced after. A grant is therefore
// whole and durable when Put returns. The directory and the key are
// created on first use, with modes 0700 and 0600.
//
// Under ax, the home directory is on the durable volume, and a suspend
// archives it before it stops the process. A refresh after the archive is
// lost on resume. The server then sees the old refresh token again, and a
// server with rotation revokes the grant: the person connects again.
type FileStore struct {
	dir string
	key func() ([]byte, error)

	mu sync.Mutex // one write at a time, so a revision check holds
}

// NewFileStore returns a store in dir. key returns the 32-byte
// encryption key. It runs on first use, not here.
func NewFileStore(dir string, key func() ([]byte, error)) *FileStore {
	var once sync.Once
	var k []byte
	var kerr error
	return &FileStore{dir: dir, key: func() ([]byte, error) {
		once.Do(func() { k, kerr = key() })
		return k, kerr
	}}
}

// grantFile is the content of one grant file.
type grantFile struct {
	Version int    `json:"v"`
	Rev     uint64 `json:"rev"`
	Nonce   []byte `json:"nonce"`
	Data    []byte `json:"data"`
}

func (s *FileStore) path(key GrantKey) string {
	return filepath.Join(s.dir, grantName(key)+".grant")
}

// grantName is the file name of a key: a hash, so no subject or issuer
// appears in a directory listing.
func grantName(key GrantKey) string {
	h := sha256.Sum256([]byte(key.Subject + "\x00" + key.Issuer + "\x00" + key.ClientID))
	return hex.EncodeToString(h[:16])
}

func (s *FileStore) aead() (cipher.AEAD, error) {
	k, err := s.key()
	if err != nil {
		return nil, fmt.Errorf("connections: grant key: %w", err)
	}
	b, err := aes.NewCipher(k)
	if err != nil {
		return nil, fmt.Errorf("connections: grant key: %w", err)
	}
	return cipher.NewGCM(b)
}

// read decrypts one grant file. A missing file is ErrGrantNotFound.
func (s *FileStore) read(path string) (Grant, uint64, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Grant{}, 0, ErrGrantNotFound
	}
	if err != nil {
		return Grant{}, 0, fmt.Errorf("connections: read grant: %w", err)
	}
	var f grantFile
	if err := json.Unmarshal(b, &f); err != nil || f.Version != 1 {
		return Grant{}, 0, fmt.Errorf("connections: grant file %s is not readable", filepath.Base(path))
	}
	aead, err := s.aead()
	if err != nil {
		return Grant{}, 0, err
	}
	plain, err := aead.Open(nil, f.Nonce, f.Data, []byte(filepath.Base(path)))
	if err != nil {
		return Grant{}, 0, fmt.Errorf("connections: grant file %s does not open with the grant key", filepath.Base(path))
	}
	var g Grant
	if err := json.Unmarshal(plain, &g); err != nil {
		return Grant{}, 0, fmt.Errorf("connections: grant file %s is not readable", filepath.Base(path))
	}
	return g, f.Rev, nil
}

// Get implements Store.
func (s *FileStore) Get(_ context.Context, key GrantKey) (Grant, uint64, error) {
	return s.read(s.path(key))
}

// Put implements Store.
func (s *FileStore) Put(_ context.Context, g Grant, prev uint64) (uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	path := s.path(g.Key())
	_, rev, err := s.read(path)
	switch {
	case errors.Is(err, ErrGrantNotFound):
		rev = 0
	case err != nil && prev != 0:
		return 0, err
	}
	if prev != 0 && rev != prev {
		return 0, ErrGrantConflict
	}
	aead, err := s.aead()
	if err != nil {
		return 0, err
	}
	plain, err := json.Marshal(g)
	if err != nil {
		return 0, fmt.Errorf("connections: encode grant: %w", err)
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return 0, fmt.Errorf("connections: grant nonce: %w", err)
	}
	f := grantFile{Version: 1, Rev: rev + 1, Nonce: nonce, Data: aead.Seal(nil, nonce, plain, []byte(filepath.Base(path)))}
	b, err := json.Marshal(f)
	if err != nil {
		return 0, fmt.Errorf("connections: encode grant: %w", err)
	}
	if err := writeDurable(s.dir, path, b); err != nil {
		return 0, fmt.Errorf("connections: write grant: %w", err)
	}
	return f.Rev, nil
}

// Delete implements Store.
func (s *FileStore) Delete(_ context.Context, key GrantKey) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	err := os.Remove(s.path(key))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("connections: delete grant: %w", err)
	}
	syncDir(s.dir)
	return nil
}

// List implements Store.
func (s *FileStore) List(_ context.Context, subject string) ([]Grant, error) {
	entries, err := os.ReadDir(s.dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("connections: list grants: %w", err)
	}
	var out []Grant
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".grant") {
			continue
		}
		g, _, err := s.read(filepath.Join(s.dir, e.Name()))
		if err != nil {
			return nil, err
		}
		if g.Subject == subject {
			out = append(out, g)
		}
	}
	sortGrants(out)
	return out, nil
}

// writeDurable writes b to path through a synced temporary file and a
// rename, then syncs the directory.
func writeDurable(dir, path string, b []byte) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".grant-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name) // a no-op after the rename
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return err
	}
	syncDir(dir)
	return nil
}

func syncDir(dir string) {
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
}

// grantKeyLoader returns the key of the file store: derived from the
// secret name when fw.Secrets holds it, else from a random key in file, which
// it creates with mode 0600 when it is missing.
func grantKeyLoader(secrets fw.Secrets, name, file string) func() ([]byte, error) {
	derive := func(src []byte) []byte {
		m := hmac.New(sha256.New, src)
		m.Write([]byte(keyLabel))
		return m.Sum(nil)
	}
	return func() ([]byte, error) {
		if secrets != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			v, err := secrets.Get(ctx, name)
			cancel()
			if err == nil && v != "" {
				return derive([]byte(v)), nil
			}
		}
		src, err := loadOrCreateKey(file)
		if err != nil {
			return nil, err
		}
		return derive(src), nil
	}
}

// loadOrCreateKey reads 32 random bytes from path, or creates the file.
// Two processes that create it at once agree on one key, because the
// file appears whole through O_EXCL on a synced temporary file and a link.
func loadOrCreateKey(path string) ([]byte, error) {
	if b, err := os.ReadFile(path); err == nil && len(b) >= 32 {
		return b, nil
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	tmp, err := os.CreateTemp(dir, ".key-*")
	if err != nil {
		return nil, err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return nil, err
	}
	if _, err := tmp.Write(key); err != nil {
		tmp.Close()
		return nil, err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return nil, err
	}
	if err := tmp.Close(); err != nil {
		return nil, err
	}
	if err := os.Link(name, path); err != nil && !errors.Is(err, fs.ErrExist) {
		return nil, err
	}
	syncDir(dir)
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(b) < 32 {
		return nil, fmt.Errorf("connections: key file %s is too short", path)
	}
	return b, nil
}
