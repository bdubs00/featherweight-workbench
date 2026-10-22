package pages

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/surface/native"
	"github.com/bdubs00/featherweight-workbench/surface/mcp"
	"github.com/bdubs00/featherweight-workbench/titles"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
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

// callers: alice owns the agent reviewer, bob operates it, carol has no
// role, and fleet is the static token of the fleet.
var callers = tokenMap{
	"alice": {Subject: "alice", Kind: fw.KindUser},
	"bob":   {Subject: "bob", Kind: fw.KindUser},
	"carol": {Subject: "carol", Kind: fw.KindUser},
	"fleet": {Subject: fw.KindToken, Kind: fw.KindToken},
}

// model is a fake model: no test of this package starts a run.
func model() fw.Model {
	return fw.ModelFunc{GenerateFn: func(context.Context, *fw.Request) (*fw.Response, error) {
		return nil, errors.New("the tests of pages start no run")
	}}
}

// newAgent returns an agent with the wiki in memory, the native surface,
// and two closed profiles: reviewer, which alice owns and bob operates,
// and private, which nobody of the callers can see.
func newAgent(t *testing.T, mut func(*fw.Config)) *fw.Agent {
	t.Helper()
	t.Setenv(titles.EnvTitles, titles.ModeInput)
	t.Setenv("FW_ATESPACE", "")
	for _, env := range []string{EnvMaxPageBytes, EnvMaxPages, EnvHistory} {
		t.Setenv(env, "")
	}
	cfg := fw.DefaultConfig()
	cfg.Home = t.TempDir()
	cfg.Access.Default = "closed"
	if mut != nil {
		mut(cfg)
	}
	a, err := fw.New(
		fw.WithConfig(cfg),
		fw.WithoutDefaults(),
		fw.WithAuthn(callers),
		fw.WithModel(model()),
		fw.WithLedger(fw.NewMemLedger()),
		fw.WithBus(fw.NewMemBus()),
		fw.WithLogger(quiet()),
		fw.WithSurfaces(native.New()),
		fw.WithServices(Start),
		fw.WithProfile(fw.AgentProfile{Name: "reviewer", Access: &fw.Access{Owners: []string{"alice"}, Operators: []string{"bob"}}}),
		fw.WithProfile(fw.AgentProfile{Name: "private", Access: &fw.Access{Owners: []string{"dave"}}}),
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

// serve serves the native surface of the agent.
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

// do sends a request as the caller with the token and returns the
// response with its body read. header holds name and value pairs.
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
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, string(b)
}

func decodeJSON(t *testing.T, body string, v any) {
	t.Helper()
	if err := json.Unmarshal([]byte(body), v); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
}

// connectMCP serves the MCP surface of the agent and connects a client
// with the token.
func connectMCP(t *testing.T, a *fw.Agent, token string) *sdk.ClientSession {
	t.Helper()
	h, err := mcp.Handler(a)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	hc := &http.Client{Transport: bearer{token: token}}
	client := sdk.NewClient(&sdk.Implementation{Name: "test-client", Version: "v1"}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	cs, err := client.Connect(ctx, &sdk.StreamableClientTransport{Endpoint: srv.URL + mcp.DefaultPath, HTTPClient: hc, MaxRetries: -1}, nil)
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

// call calls a tool with optional _meta.
func call(t *testing.T, cs *sdk.ClientSession, name string, args any, meta sdk.Meta) *sdk.CallToolResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := cs.CallTool(ctx, &sdk.CallToolParams{Meta: meta, Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("call %s: %v", name, err)
	}
	return res
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

func structured(t *testing.T, res *sdk.CallToolResult, v any) {
	t.Helper()
	b, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatalf("structuredContent %s: %v", b, err)
	}
}
