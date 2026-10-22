package faces

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/surface/native"
	"github.com/bdubs00/featherweight-workbench/surface/mcp"
	"github.com/bdubs00/featherweight-workbench/titles"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

type tokenMap map[string]*fw.Principal

func (m tokenMap) Authenticate(r *http.Request) (*fw.Principal, error) {
	tok := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	if p, ok := m[tok]; ok && tok != "" {
		return p, nil
	}
	return nil, fw.ErrUnauthorized
}

func (m tokenMap) Challenge(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusUnauthorized)
}

// callers: alice owns reviewer, bob operates it, carol has no role, and
// fleet is the static token of the fleet.
var callers = tokenMap{
	"alice": {Subject: "alice", Kind: fw.KindUser},
	"bob":   {Subject: "bob", Kind: fw.KindUser},
	"carol": {Subject: "carol", Kind: fw.KindUser},
	"fleet": {Subject: fw.KindToken, Kind: fw.KindToken},
}

func newAgent(t *testing.T) *fw.Agent {
	t.Helper()
	t.Setenv(titles.EnvTitles, titles.ModeInput)
	t.Setenv("FW_ATESPACE", "")
	cfg := fw.DefaultConfig()
	cfg.Home = t.TempDir()
	cfg.Access.Default = "closed"
	a, err := fw.New(
		fw.WithConfig(cfg),
		fw.WithoutDefaults(),
		fw.WithAuthn(callers),
		fw.WithModel(fw.ModelFunc{GenerateFn: func(context.Context, *fw.Request) (*fw.Response, error) {
			return nil, errors.New("the tests of faces start no run")
		}}),
		fw.WithLedger(fw.NewMemLedger()),
		fw.WithBus(fw.NewMemBus()),
		fw.WithLogger(quiet()),
		fw.WithSurfaces(native.New()),
		fw.WithServices(Start),
		fw.WithProfile(fw.AgentProfile{Name: "reviewer", Access: &fw.Access{Owners: []string{"alice"}, Operators: []string{"bob"}}}),
	)
	if err != nil {
		t.Fatalf("fw.New: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		a.Close(ctx)
	})
	return a
}

func serve(t *testing.T, a *fw.Agent) string {
	t.Helper()
	h, err := a.Handler()
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv.URL
}

func do(t *testing.T, method, url, token, body string, header ...string) (*http.Response, string) {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, url, rd)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

func TestRoutes(t *testing.T) {
	a := newAgent(t)
	base := serve(t, a) + "/v1/faces"
	icon := Identicon("reviewer")

	// Without a face, the identicon, with its hash.
	resp, out := do(t, "GET", base+"/reviewer", "bob", "")
	if resp.StatusCode != http.StatusOK || out != string(icon) || resp.Header.Get("Content-Type") != "image/svg+xml" ||
		resp.Header.Get("ETag") != strconv.Quote(hashOf(icon)) || !strings.Contains(resp.Header.Get("Content-Security-Policy"), "default-src 'none'") ||
		resp.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("GET = %d %v %s", resp.StatusCode, resp.Header, out)
	}

	// The owner sets a face; a hostile face is refused with the reason.
	if resp, out = do(t, "PUT", base+"/reviewer", "alice", `<svg xmlns="http://www.w3.org/2000/svg" onload="alert(1)"/>`, "Content-Type", "image/svg+xml"); resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(out, "onload") {
		t.Fatalf("PUT of a hostile face = %d %s", resp.StatusCode, out)
	}
	resp, out = do(t, "PUT", base+"/reviewer", "alice", fox, "Content-Type", "image/svg+xml")
	var f Face
	if err := json.Unmarshal([]byte(out), &f); err != nil || resp.StatusCode != http.StatusOK || !f.Stored || f.By != "alice" {
		t.Fatalf("PUT = %d %s", resp.StatusCode, out)
	}
	if resp, _ = do(t, "PUT", base+"/reviewer", "alice", strings.Repeat(" ", MaxSVGBytes+1)); resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("PUT of 32 KB and more = %d", resp.StatusCode)
	}
	clean, _ := Sanitize([]byte(fox))
	resp, out = do(t, "GET", base+"/reviewer", "bob", "")
	if resp.StatusCode != http.StatusOK || out != string(clean) || resp.Header.Get("ETag") != strconv.Quote(f.Hash) {
		t.Fatalf("GET of the face = %d %s", resp.StatusCode, out)
	}
	if resp, _ = do(t, "GET", base+"/reviewer", "bob", "", "If-None-Match", strconv.Quote(f.Hash)); resp.StatusCode != http.StatusNotModified {
		t.Fatalf("GET with the hash = %d", resp.StatusCode)
	}

	// The display name and the tagline.
	resp, out = do(t, "PATCH", base+"/reviewer", "alice", `{"displayName": "Reviewer Fox", "tagline": "Reads twice."}`, "Content-Type", "application/json")
	if resp.StatusCode != http.StatusOK || !strings.Contains(out, `"displayName":"Reviewer Fox"`) {
		t.Fatalf("PATCH = %d %s", resp.StatusCode, out)
	}
	if resp, out = do(t, "PATCH", base+"/reviewer", "alice", `{"displayName": "`+strings.Repeat("n", 65)+`"}`); resp.StatusCode != http.StatusBadRequest || !strings.Contains(out, "64") {
		t.Fatalf("PATCH of a long name = %d %s", resp.StatusCode, out)
	}
	if resp, _ = do(t, "PATCH", base+"/reviewer", "alice", `{"avatar": "x"}`); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("PATCH of an unknown field = %d", resp.StatusCode)
	}
	resp, out = do(t, "GET", base+"/reviewer", "bob", "", "Accept", "application/json")
	if resp.StatusCode != http.StatusOK || !strings.Contains(out, `"tagline":"Reads twice."`) || strings.Contains(out, "<svg") {
		t.Fatalf("GET as JSON = %d %s", resp.StatusCode, out)
	}

	// An operator reads a face and cannot change it; the token changes it.
	if resp, _ = do(t, "PUT", base+"/reviewer", "bob", fox); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("PUT of an operator = %d", resp.StatusCode)
	}
	if resp, _ = do(t, "PATCH", base+"/reviewer", "carol", `{"tagline": "x"}`); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("PATCH without a role = %d", resp.StatusCode)
	}
	// A caller who cannot view the agent gets the identicon.
	if resp, out = do(t, "GET", base+"/reviewer", "carol", ""); resp.StatusCode != http.StatusOK || out != string(icon) {
		t.Fatalf("GET without a role = %d %s", resp.StatusCode, out)
	}
	if resp, out = do(t, "PUT", base+"/nobody", "fleet", fox); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("PUT for an agent that does not exist = %d %s", resp.StatusCode, out)
	}
	if resp, out = do(t, "GET", base+"/nobody", "bob", ""); resp.StatusCode != http.StatusOK || out != string(Identicon("nobody")) {
		t.Fatalf("GET for an agent that does not exist = %d %s", resp.StatusCode, out)
	}
	if resp, _ = do(t, "GET", base+"/.hidden", "bob", ""); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("GET of a bad name = %d", resp.StatusCode)
	}

	// Back to the identicon.
	if resp, _ = do(t, "DELETE", base+"/reviewer", "fleet", ""); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE = %d", resp.StatusCode)
	}
	if resp, out = do(t, "GET", base+"/reviewer", "bob", ""); out != string(icon) {
		t.Fatalf("GET after DELETE = %d %s", resp.StatusCode, out)
	}
}

func connectMCP(t *testing.T, a *fw.Agent, token string) *sdk.ClientSession {
	t.Helper()
	h, err := mcp.Handler(a)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	hc := &http.Client{Transport: bearer{token: token}}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	cs, err := sdk.NewClient(&sdk.Implementation{Name: "test-client", Version: "v1"}, nil).
		Connect(ctx, &sdk.StreamableClientTransport{Endpoint: srv.URL + mcp.DefaultPath, HTTPClient: hc, MaxRetries: -1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

type bearer struct{ token string }

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return http.DefaultTransport.RoundTrip(r)
}

func textOf(res *sdk.CallToolResult) string {
	var parts []string
	for _, c := range res.Content {
		if tc, ok := c.(*sdk.TextContent); ok {
			parts = append(parts, tc.Text)
		}
	}
	return strings.Join(parts, "\n")
}

func TestFacesPut(t *testing.T) {
	a := newAgent(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	manager := connectMCP(t, a, "fleet")
	list, err := manager.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var tool *sdk.Tool
	for _, tl := range list.Tools {
		if tl.Name == ToolPut {
			tool = tl
		}
	}
	if tool == nil || tool.Title != "Set face" || tool.Annotations == nil || tool.Annotations.ReadOnlyHint ||
		tool.Annotations.DestructiveHint == nil || *tool.Annotations.DestructiveHint {
		t.Fatalf("faces_put = %+v", tool)
	}
	meta := sdk.Meta{mcp.MetaRun: "r-9", mcp.MetaAgent: "manager"}
	call := func(cs *sdk.ClientSession, args map[string]any) *sdk.CallToolResult {
		t.Helper()
		res, err := cs.CallTool(ctx, &sdk.CallToolParams{Meta: meta, Name: ToolPut, Arguments: args})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	res := call(manager, map[string]any{"agent": "reviewer", "svg": `<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`})
	if !res.IsError || !strings.Contains(textOf(res), "<script>") || !strings.Contains(textOf(res), "call faces_put again") {
		t.Fatalf("a face with a script = %s", textOf(res))
	}
	res = call(manager, map[string]any{"agent": "reviewer", "svg": fox})
	if res.IsError || !strings.Contains(textOf(res), "GET /v1/faces/reviewer") {
		t.Fatalf("a clean face = %s", textOf(res))
	}
	if f, _ := Of(a).Get(ctx, "reviewer"); !f.Stored || f.By != "agent:manager" {
		t.Fatalf("the face = %+v", f)
	}
	// An operator cannot set a face.
	bob := connectMCP(t, a, "bob")
	if res = call(bob, map[string]any{"agent": "reviewer", "svg": fox}); !res.IsError || !strings.Contains(textOf(res), "Forbidden") {
		t.Fatalf("faces_put of an operator = %s", textOf(res))
	}
	if res = call(manager, map[string]any{"agent": "nobody", "svg": fox}); !res.IsError || !strings.Contains(textOf(res), "nobody") {
		t.Fatalf("faces_put for no agent = %s", textOf(res))
	}
}
