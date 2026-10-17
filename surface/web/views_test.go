package web

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/authn/oidc"
	"github.com/bdubs00/featherweight-workbench/dashboard"
)

// tokenMap is a test Authn that maps a bearer token to a principal.
type tokenMap map[string]*fw.Principal

func (m tokenMap) Authenticate(r *http.Request) (*fw.Principal, error) {
	tok := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	if p, ok := m[tok]; ok && tok != "" {
		return p, nil
	}
	return nil, fw.ErrUnauthorized
}

func (m tokenMap) Challenge(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="fw"`)
	w.WriteHeader(http.StatusUnauthorized)
}

var people = tokenMap{
	"alice-token": {Subject: "alice", Name: "alice@example.com", Kind: fw.KindUser, Groups: []string{"platform"}},
	"bob-token":   {Subject: "bob", Kind: fw.KindUser},
}

const (
	asAlice = "Bearer alice-token"
	asBob   = "Bearer bob-token"
)

// manageAgent is an agent with three profiles: reviewer, which alice owns
// and bob can view, shared, which every platform member operates, and
// private, which neither can see. The configuration is closed, and alice is
// a creator.
func manageAgent(t *testing.T, m fw.Model, opts ...fw.Option) *fw.Agent {
	t.Helper()
	cfg := fw.DefaultConfig()
	cfg.Home = t.TempDir()
	cfg.Access.Default = "closed"
	cfg.Access.Creators = []string{"alice"}
	// The profiles stand in for files under <home>/agents, so a person can
	// change them. A profile that the configuration defines is locked.
	cfg.FileProfiles = map[string]bool{"reviewer": true, "shared": true, "private": true}
	base := []fw.Option{
		fw.WithConfig(cfg),
		fw.WithAuthn(people),
		fw.WithProfile(fw.AgentProfile{Name: "reviewer", Description: "Reviews code", Access: &fw.Access{Owners: []string{"alice"}, Viewers: []string{"bob"}}}),
		fw.WithProfile(fw.AgentProfile{Name: "shared", Access: &fw.Access{Operators: []string{"group:platform"}}}),
		fw.WithProfile(fw.AgentProfile{Name: "private", Access: &fw.Access{Owners: []string{"carol"}}}),
	}
	return newAgent(t, m, append(base, opts...)...)
}

func decodeJSON(t *testing.T, resp *http.Response, b []byte, status int, v any) {
	t.Helper()
	if resp.StatusCode != status {
		t.Fatalf("status = %d, want %d: %s", resp.StatusCode, status, b)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content type = %q", ct)
	}
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatalf("decode: %v: %s", err, b)
	}
}

func TestUIPage(t *testing.T) {
	a := manageAgent(t, newScript().model())
	srv := serve(t, a)
	resp, b := do(t, "GET", srv.URL+"/ui", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("content type = %q", ct)
	}
	if string(b) != string(dashboard.Page()) || !strings.HasPrefix(strings.ToLower(string(b)), "<!doctype html>") {
		t.Fatalf("body is not the dashboard: %d bytes", len(b))
	}
	if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "connect-src 'self'") {
		t.Errorf("policy = %q", csp)
	}
	// The protected-resource document is not served without the oidc mode.
	resp, _ = do(t, "GET", srv.URL+"/.well-known/oauth-protected-resource", "")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("protected resource without oidc: status %d", resp.StatusCode)
	}
}

func TestProtectedResource(t *testing.T) {
	cfg := fw.DefaultConfig()
	cfg.Home = t.TempDir()
	cfg.Auth.Mode = "oidc,token"
	cfg.Auth.OIDC = &fw.OIDCConfig{Issuer: "https://idp.example.com", Scopes: []string{"openid", "fw"}}
	cfg.MCP.Auth.AuthorizationServers = []string{"https://idp.example.com", "https://other.example.com"}
	cfg.MCP.Auth.Audience = "https://fw.example.com/mcp"
	a := newAgent(t, newScript().model(), fw.WithConfig(cfg), fw.WithAuthn(people))
	srv := serve(t, a)
	var doc map[string]any
	resp, b := do(t, "GET", srv.URL+"/.well-known/oauth-protected-resource", "")
	decodeJSON(t, resp, b, http.StatusOK, &doc)
	servers, _ := doc["authorization_servers"].([]any)
	scopes, _ := doc["scopes_supported"].([]any)
	methods, _ := doc["bearer_methods_supported"].([]any)
	if doc["resource"] != "https://fw.example.com/mcp" || len(servers) != 2 || servers[1] != "https://other.example.com" || len(scopes) != 2 || scopes[1] != "fw" || len(methods) != 1 || methods[0] != "header" {
		t.Fatalf("document = %s", b)
	}
	// The challenge of the oidc mode names the document with the path of
	// the resource after the well-known path. That URL answers too.
	named, err := url.Parse(oidc.ResourceMetadataURL(cfg))
	if err != nil || named.Path != "/.well-known/oauth-protected-resource/mcp" {
		t.Fatalf("named URL = %v, %v", named, err)
	}
	var again map[string]any
	resp, b = do(t, "GET", srv.URL+named.Path, "")
	decodeJSON(t, resp, b, http.StatusOK, &again)
	if again["resource"] != "https://fw.example.com/mcp" {
		t.Fatalf("named document = %s", b)
	}

	// Without an audience the resource is the A2A URL, then the request origin,
	// and the issuer stands in for an empty server list.
	cfg.MCP.Auth.Audience = ""
	cfg.MCP.Auth.AuthorizationServers = nil
	cfg.A2A.URL = "https://agent.example.com"
	cfg.Auth.OIDC.Scopes = nil
	doc = ProtectedResource(cfg, "http://fallback")
	servers, _ = doc["authorization_servers"].([]any)
	if _, ok := doc["scopes_supported"]; ok || doc["resource"] != "https://agent.example.com" {
		t.Fatalf("document = %+v", doc)
	}
	if s, _ := doc["authorization_servers"].([]string); len(s) != 1 || s[0] != "https://idp.example.com" {
		t.Fatalf("servers = %+v", doc["authorization_servers"])
	}
	cfg.A2A.URL = ""
	resp, b = do(t, "GET", srv.URL+"/.well-known/oauth-protected-resource", "", "X-Forwarded-Proto", "https", "X-Forwarded-Host", "fw.example.com")
	decodeJSON(t, resp, b, http.StatusOK, &doc)
	if doc["resource"] != "https://fw.example.com" {
		t.Fatalf("resource = %v", doc["resource"])
	}
}

func TestTranscriptRoute(t *testing.T) {
	ctx := testCtx(t)
	a := manageAgent(t, newScript(endTurn("one"), endTurn("two")).model())
	srv := serve(t, a)
	actx := fw.WithPrincipal(ctx, people["alice-token"])
	r, err := a.Run(actx, "review this", fw.AsAgent("reviewer"))
	if err != nil {
		t.Fatal(err)
	}
	type chat struct {
		Run   *fw.Run
		Items []dashboard.Item
		Next  uint64
	}
	var c chat
	resp, b := do(t, "GET", srv.URL+"/v1/runs/"+r.ID+"/transcript", "", "Authorization", asAlice)
	decodeJSON(t, resp, b, http.StatusOK, &c)
	var kinds []string
	for _, it := range c.Items {
		kinds = append(kinds, it.Kind)
	}
	if c.Run == nil || c.Run.ID != r.ID || c.Next != 0 || strings.Join(kinds, " ") != "user assistant end" || c.Items[1].Text != "one" {
		t.Fatalf("transcript = %s", b)
	}
	// from and limit page through the events, and next names the rest.
	resp, b = do(t, "GET", srv.URL+"/v1/runs/"+r.ID+"/transcript?from=1&limit=2", "", "Authorization", asAlice)
	decodeJSON(t, resp, b, http.StatusOK, &c)
	if len(c.Items) != 1 || c.Items[0].Kind != dashboard.ItemUser || c.Next != 3 {
		t.Fatalf("page = %s", b)
	}
	// The read authorizes as the events do: bob views reviewer, not shared.
	resp, _ = do(t, "GET", srv.URL+"/v1/runs/"+r.ID+"/transcript", "", "Authorization", asBob)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("bob on reviewer: status %d", resp.StatusCode)
	}
	shared, err := a.Run(actx, "shared work", fw.AsAgent("shared"))
	if err != nil {
		t.Fatal(err)
	}
	resp, b = do(t, "GET", srv.URL+"/v1/runs/"+shared.ID+"/transcript", "", "Authorization", asBob)
	decodeProblem(t, resp, b, http.StatusForbidden)
	resp, b = do(t, "GET", srv.URL+"/v1/runs/01NOPE/transcript", "", "Authorization", asAlice)
	decodeProblem(t, resp, b, http.StatusNotFound)
	resp, b = do(t, "GET", srv.URL+"/v1/runs/"+r.ID+"/transcript?from=x", "", "Authorization", asAlice)
	decodeProblem(t, resp, b, http.StatusBadRequest)
	resp, b = do(t, "GET", srv.URL+"/v1/runs/"+r.ID+"/transcript", "")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("no credential: status %d", resp.StatusCode)
	}
}
