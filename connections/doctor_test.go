package connections

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
)

func TestDoctorCheck(t *testing.T) {
	ctx := context.Background()
	for _, e := range []string{"FW_GRANT_KEY", "FW_GRANT_KEY_FILE", EnvStore, EnvDir, EnvKey} {
		t.Setenv(e, "")
	}
	home := t.TempDir()
	dir := filepath.Join(home, "grants")
	cfg := fw.DefaultConfig()
	cfg.Home = home
	check := func() (bool, string) { return DoctorCheck(ctx, cfg, fw.EnvSecrets{}, time.Second) }

	// No service acts for a person. The line names the store and writes
	// nothing.
	ok, line := check()
	want := "connections: no service acts for a person; grants: file " + dir + ", key in " + filepath.Join(dir, "grant.key") + " (set FW_GRANT_KEY to share one key)"
	if !ok || line != want {
		t.Errorf("line = %v %q\nwant   %q", ok, line, want)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the check made the grant directory: %v", err)
	}

	// The key secret wins, and its value never shows.
	t.Setenv("FW_GRANT_KEY", "grant-key-secret")
	if ok, line := check(); !ok || !strings.HasSuffix(line, "grants: file "+dir+", key from FW_GRANT_KEY") || strings.Contains(line, "grant-key-secret") {
		t.Errorf("line = %v %q", ok, line)
	}

	// A service that acts for a person needs oidc.
	cfg.Tools.MCP = []fw.MCPServerConfig{{Name: "tickets", Endpoint: "https://tickets.example.com/mcp", Auth: fw.MCPAuthConfig{OAuth: &fw.OAuthClientConfig{Grant: "user"}}}}
	cfg.Auth.Mode = "token"
	if ok, line := check(); ok || line != "connections: tickets acts for a person, but auth.mode (FW_AUTH_MODE) has no oidc, so no person can connect" {
		t.Errorf("line = %v %q", ok, line)
	}

	// Without a client of its own, it needs the sign-in client.
	cfg.Auth.Mode = "oidc"
	cfg.Auth.OIDC = &fw.OIDCConfig{Issuer: "https://id.example.com"}
	if ok, line := check(); ok || line != "connections: tickets uses the sign-in client, but auth.oidc.clientId (FW_AUTH_OIDC_CLIENT_ID) is not set" {
		t.Errorf("line = %v %q", ok, line)
	}

	// A service with a client of its own works without the sign-in client,
	// and the line says what that costs.
	cfg.Tools.MCP = []fw.MCPServerConfig{
		{Name: "docs", Endpoint: "https://docs.example.com/mcp", Auth: fw.MCPAuthConfig{OAuth: &fw.OAuthClientConfig{Grant: "user", ClientID: "fw-docs"}}},
		{Name: "tickets", Endpoint: "https://tickets.example.com/mcp", Auth: fw.MCPAuthConfig{OAuth: &fw.OAuthClientConfig{Grant: "user", ClientID: "fw-tickets"}}},
	}
	if ok, line := check(); !ok || !strings.HasPrefix(line, "connections: docs, tickets act for a person; grants: file ") ||
		!strings.HasSuffix(line, "browser sign-in is off (FW_AUTH_OIDC_CLIENT_ID), so a person who opens a connect link pastes a token first") {
		t.Errorf("line = %v %q", ok, line)
	}

	// With the sign-in client, the line is plain, and the memory store
	// says that grants end with the process.
	cfg.Tools.MCP[1].Auth.OAuth.ClientID = ""
	cfg.Auth.OIDC.ClientID = "fw"
	t.Setenv(EnvStore, "memory")
	if ok, line := check(); !ok || line != "connections: docs, tickets act for a person; grants: memory, so they end with the process" {
		t.Errorf("line = %v %q", ok, line)
	}

	// A store that no driver provides fails the line.
	t.Setenv(EnvStore, "bogus")
	if ok, line := check(); ok || !strings.HasPrefix(line, "connections: ") || !strings.Contains(line, `workbench.grants.store "bogus"`) {
		t.Errorf("line = %v %q", ok, line)
	}
}
