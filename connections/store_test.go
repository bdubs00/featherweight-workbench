package connections

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
)

func testKey(b byte) func() ([]byte, error) {
	return func() ([]byte, error) { return bytes.Repeat([]byte{b}, 32), nil }
}

func TestFileStore(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "grants")
	s := NewFileStore(dir, testKey(7))
	g := Grant{Subject: "ada", Issuer: "https://id.example", ClientID: "fw", RefreshToken: "rt-secret-1", Scopes: []string{"offline_access"}, Created: time.Now().UTC()}
	if _, _, err := s.Get(ctx, g.Key()); !errors.Is(err, ErrGrantNotFound) {
		t.Fatalf("Get before Put = %v", err)
	}
	rev, err := s.Put(ctx, g, 0)
	if err != nil || rev != 1 {
		t.Fatalf("Put = %d, %v", rev, err)
	}
	got, rev2, err := s.Get(ctx, g.Key())
	if err != nil || rev2 != 1 || got.RefreshToken != "rt-secret-1" || got.Scopes[0] != "offline_access" {
		t.Fatalf("Get = %+v %d %v", got, rev2, err)
	}
	// The token never appears in clear on disk, and no name gives the person away.
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		b, _ := os.ReadFile(filepath.Join(dir, e.Name()))
		if bytes.Contains(b, []byte("rt-secret")) || strings.Contains(e.Name(), "ada") {
			t.Fatalf("file %s leaks the grant", e.Name())
		}
		if info, _ := e.Info(); info.Mode().Perm() != 0o600 {
			t.Fatalf("file %s has mode %v", e.Name(), info.Mode().Perm())
		}
	}
	// Compare-and-set.
	g.RefreshToken = "rt-secret-2"
	if _, err := s.Put(ctx, g, 5); !errors.Is(err, ErrGrantConflict) {
		t.Fatalf("Put with a stale revision = %v", err)
	}
	if rev, err = s.Put(ctx, g, 1); err != nil || rev != 2 {
		t.Fatalf("Put with the revision = %d, %v", rev, err)
	}
	// Another store with the same key reads it, one with another key does not.
	if got, _, err := NewFileStore(dir, testKey(7)).Get(ctx, g.Key()); err != nil || got.RefreshToken != "rt-secret-2" {
		t.Fatalf("reopen = %+v %v", got, err)
	}
	if _, _, err := NewFileStore(dir, testKey(8)).Get(ctx, g.Key()); err == nil || strings.Contains(err.Error(), "rt-secret") {
		t.Fatalf("wrong key = %v", err)
	}
	other := Grant{Subject: "bob", Issuer: "https://id.example", ClientID: "fw", RefreshToken: "rt-bob"}
	if _, err := s.Put(ctx, other, 0); err != nil {
		t.Fatal(err)
	}
	list, err := s.List(ctx, "ada")
	if err != nil || len(list) != 1 || list[0].Subject != "ada" {
		t.Fatalf("List = %+v %v", list, err)
	}
	if err := s.Delete(ctx, g.Key()); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Get(ctx, g.Key()); !errors.Is(err, ErrGrantNotFound) {
		t.Fatalf("Get after Delete = %v", err)
	}
}

func TestKeyFile(t *testing.T) {
	dir := t.TempDir()
	load := grantKeyLoader(fw.EnvSecrets{}, "FW_TEST_GRANT_KEY_UNSET", filepath.Join(dir, keyFile))
	k1, err := load()
	if err != nil || len(k1) != 32 {
		t.Fatalf("key = %d bytes, %v", len(k1), err)
	}
	info, err := os.Stat(filepath.Join(dir, keyFile))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("key file = %v, %v", info, err)
	}
	k2, _ := grantKeyLoader(fw.EnvSecrets{}, "FW_TEST_GRANT_KEY_UNSET", filepath.Join(dir, keyFile))()
	if !bytes.Equal(k1, k2) {
		t.Fatal("a second loader read another key")
	}
	t.Setenv("FW_TEST_GRANT_KEY", "from-secret")
	k3, _ := grantKeyLoader(fw.EnvSecrets{}, "FW_TEST_GRANT_KEY", filepath.Join(dir, keyFile))()
	if bytes.Equal(k1, k3) {
		t.Fatal("the secret did not win over the key file")
	}
}

func TestGrantNeverFormatsTheToken(t *testing.T) {
	g := Grant{Subject: "ada", RefreshToken: "rt-secret"}
	for _, s := range []string{g.String(), g.GoString(), g.LogValue().String()} {
		if strings.Contains(s, "rt-secret") {
			t.Fatalf("%q shows the token", s)
		}
	}
}
