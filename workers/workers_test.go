package workers

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/surface/native"
	"github.com/bdubs00/featherweight-workbench/fleet"
	"github.com/bdubs00/featherweight-workbench/internal/bucket"
	"github.com/bdubs00/featherweight-workbench/pools"
	"github.com/bdubs00/featherweight-workbench/surface/mcp"
	"github.com/bdubs00/featherweight-workbench/titles"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"gopkg.in/yaml.v3"
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

// callers: alice can create agents, bob has no role, and carol is an admin
// of the front.
var callers = tokenMap{
	"alice": {Subject: "alice", Kind: fw.KindUser},
	"bob":   {Subject: "bob", Kind: fw.KindUser},
	"carol": {Subject: "carol", Kind: fw.KindUser},
}

// block holds the default template base and a pool triage of the file.
const block = `
templates:
  - name: base
    default: true
    image: ghcr.io/example/fw@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
    egress:
      rules:
        - http: {hostnames: [nats.agents.svc.cluster.local], ports: [8080]}
    fw:
      model: openai/some-model
      bus: {url: "ws://nats.agents.svc.cluster.local:8080"}
      access: {admins: ["kind:token"]}
pools:
  - name: triage
    template: base
    replicas: 1
    fw:
      agents:
        triage:
          description: Sorts new tickets.
`

// front is a front with the templates and the pools of block, the
// overlays and the runtime pools in memory. The ax API does not answer, so
// no pass runs.
type front struct {
	a     *fw.Agent
	url   string
	store *pools.Store
}

func newFront(t *testing.T) *front {
	t.Helper()
	t.Setenv("FW_API_TOKEN", "fleet-secret-token")
	t.Setenv(titles.EnvTitles, titles.ModeInput)
	t.Setenv("FW_ATESPACE", "")
	overlays := pools.NewMemOverlays()
	store := pools.NewStore(bucket.NewMem(pools.StoreBucket), nil)
	oldO, oldR := fleet.OpenOverlays, fleet.OpenRuntime
	fleet.OpenOverlays = func(context.Context, *fw.Config, *slog.Logger) (pools.OverlayStore, func(), error) {
		return overlays, nil, nil
	}
	fleet.OpenRuntime = func(context.Context, *fw.Config, *slog.Logger, bool) (*pools.Store, func(), error) {
		return store, nil, nil
	}
	t.Cleanup(func() { fleet.OpenOverlays, fleet.OpenRuntime = oldO, oldR })
	cfg := fw.DefaultConfig()
	cfg.Home = t.TempDir()
	cfg.Access.Creators, cfg.Access.Admins = []string{"alice"}, []string{"carol"}
	cfg.Agents = map[string]fw.AgentProfile{"default": {}}
	cfg.Bus.Driver, cfg.Bus.URL = "nats", "nats://nats.agents.svc.cluster.local:4222"
	cfg.AX.Server, cfg.AX.Router = "127.0.0.1:1", "127.0.0.1:1"
	var n yaml.Node
	if err := yaml.Unmarshal([]byte(block), &n); err != nil {
		t.Fatal(err)
	}
	cfg.Extensions = map[string]yaml.Node{fleet.Block: *n.Content[0]}
	a, err := fw.New(fw.WithConfig(cfg), fw.WithoutDefaults(), fw.WithAuthn(callers),
		fw.WithModel(fw.ModelFunc{GenerateFn: func(context.Context, *fw.Request) (*fw.Response, error) { return nil, errors.New("no run") }}),
		fw.WithLedger(fw.NewMemLedger()), fw.WithBus(fw.NewMemBus()), fw.WithLogger(slog.New(slog.DiscardHandler)), fw.WithSurfaces(native.New()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		a.Close(ctx)
	})
	off := time.Duration(0)
	fc := fleet.Config{Name: "front", Egress: "fw.events", Atespaces: []string{"team"}, Interval: time.Hour, Expire: fleet.DefaultExpire, SuspendAfter: &off}
	if fc.Pools, fc.Templates, err = fleet.LoadAll(cfg, fc); err != nil {
		t.Fatal(err)
	}
	f, err := fleet.Open(context.Background(), a, fc)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	fleet.Attach(a, f)
	t.Cleanup(func() { fleet.Attach(a, nil) })
	h, err := a.Handler()
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return &front{a: a, url: srv.URL, store: store}
}

func do(t *testing.T, method, url, token, body string) (*http.Response, string) {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, url, rd)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

func decode[T any](t *testing.T, body string) T {
	t.Helper()
	var v T
	if err := json.Unmarshal([]byte(body), &v); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	return v
}

// The routes create, list, change, and delete a runtime pool, with the
// checks of who can: the creators create, the owner of the agent and the
// admins change and delete, and a pool of the file changes in fw.yaml.
func TestRoutes(t *testing.T) {
	fr := newFront(t)
	resp, body := do(t, "GET", fr.url+"/v1/templates", "bob", "")
	ts := decode[struct{ Templates []pools.TemplateInfo }](t, body)
	if resp.StatusCode != 200 || len(ts.Templates) != 1 || ts.Templates[0].Name != "base" || !ts.Templates[0].Default || ts.Templates[0].Model != "openai/some-model" ||
		strings.Contains(body, "sha256") || strings.Contains(body, "egress") {
		t.Fatalf("GET /v1/templates = %d %s", resp.StatusCode, body)
	}
	if resp, body := do(t, "GET", fr.url+"/v1/templates", "", ""); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("GET /v1/templates without a token = %d %s", resp.StatusCode, body)
	}

	create := `{"name":"helper","agent":"helper","replicas":1}`
	if resp, body := do(t, "POST", fr.url+"/v1/pools", "bob", create); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("POST of bob = %d %s", resp.StatusCode, body)
	}
	resp, body = do(t, "POST", fr.url+"/v1/pools", "alice", create)
	st := decode[pools.Status](t, body)
	if resp.StatusCode != http.StatusCreated || resp.Header.Get("Location") != "/v1/pools/helper" || st.Source != pools.SourceRuntime || st.Template != "base" ||
		st.By != "alice" || st.Generation != 1 || len(st.Gaps) != 1 || !strings.Contains(st.Gaps[0], "waits for the profile helper") {
		t.Fatalf("POST = %d %s", resp.StatusCode, body)
	}
	for _, c := range []struct {
		body string
		code int
		want string
	}{
		{create, http.StatusConflict, "the store holds a runtime pool named helper"},
		{`{"name":"triage","agent":"triage","replicas":1}`, http.StatusConflict, "the configuration file of the front holds a pool named triage"},
		{`{"name":"other","agent":"other"}`, http.StatusBadRequest, "set replicas"},
		{`{"name":"other","agent":"other","replicas":1,"image":"x"}`, http.StatusBadRequest, "unknown field"},
		{`{"name":"other","agent":"other","replicas":1,"env":[{"name":"FW_X","value":"1"}]}`, http.StatusBadRequest, "remove FW_X"},
	} {
		if resp, body := do(t, "POST", fr.url+"/v1/pools", "alice", c.body); resp.StatusCode != c.code || !strings.Contains(body, c.want) {
			t.Errorf("POST %s = %d %s, want %d %q", c.body, resp.StatusCode, body, c.code, c.want)
		}
	}

	resp, body = do(t, "GET", fr.url+"/v1/pools", "alice", "")
	list := decode[struct{ Pools []pools.Status }](t, body)
	if resp.StatusCode != 200 || len(list.Pools) != 2 || list.Pools[0].Name != "triage" || list.Pools[0].Source != pools.SourceFile || list.Pools[0].Template != "base" ||
		list.Pools[1].Name != "helper" || list.Pools[1].Source != pools.SourceRuntime {
		t.Fatalf("GET /v1/pools = %d %s", resp.StatusCode, body)
	}
	if resp, body := do(t, "GET", fr.url+"/v1/pools/helper", "alice", ""); resp.StatusCode != 200 || decode[pools.Status](t, body).Name != "helper" {
		t.Fatalf("GET /v1/pools/helper = %d %s", resp.StatusCode, body)
	}
	if resp, _ := do(t, "GET", fr.url+"/v1/pools/nope", "alice", ""); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET /v1/pools/nope = %d", resp.StatusCode)
	}

	if resp, body := do(t, "PATCH", fr.url+"/v1/pools/helper", "bob", `{"replicas":3}`); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("PATCH of bob = %d %s", resp.StatusCode, body)
	}
	resp, body = do(t, "PATCH", fr.url+"/v1/pools/helper", "alice", `{"replicas":3,"resources":{"limits":{"memory":"4Gi"}}}`)
	if st := decode[pools.Status](t, body); resp.StatusCode != 200 || st.Wanted != 3 || st.Generation != 2 {
		t.Fatalf("PATCH = %d %s", resp.StatusCode, body)
	}
	if e, _ := fr.store.Get(context.Background(), "helper"); e.Resources == nil || e.Resources.Limits.Memory != "4Gi" || e.By != "alice" {
		t.Fatalf("the entry = %+v", e)
	}
	if resp, body := do(t, "PATCH", fr.url+"/v1/pools/helper", "carol", `{"template":"nope"}`); resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, "no template is named nope") {
		t.Fatalf("PATCH with an unknown template = %d %s", resp.StatusCode, body)
	}
	if resp, body := do(t, "PATCH", fr.url+"/v1/pools/triage", "carol", `{"replicas":3}`); resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, "change it in workbench.pools") {
		t.Fatalf("PATCH of a pool of the file = %d %s", resp.StatusCode, body)
	}
	if resp, _ := do(t, "PATCH", fr.url+"/v1/pools/nope", "carol", `{"replicas":3}`); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("PATCH of no pool = %d", resp.StatusCode)
	}

	if resp, _ := do(t, "DELETE", fr.url+"/v1/pools/helper", "bob", ""); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("DELETE of bob = %d", resp.StatusCode)
	}
	resp, body = do(t, "DELETE", fr.url+"/v1/pools/helper", "carol", "")
	if st := decode[pools.Status](t, body); resp.StatusCode != http.StatusAccepted || !st.Deleting || st.Wanted != 0 {
		t.Fatalf("DELETE of an admin = %d %s", resp.StatusCode, body)
	}
	if resp, body := do(t, "PATCH", fr.url+"/v1/pools/helper", "alice", `{"replicas":1}`); resp.StatusCode != http.StatusConflict {
		t.Fatalf("PATCH of a deleted pool = %d %s", resp.StatusCode, body)
	}
}

func connect(t *testing.T, a *fw.Agent, token string) *sdk.ClientSession {
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

func call(t *testing.T, cs *sdk.ClientSession, name string, args map[string]any) (*sdk.CallToolResult, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := cs.CallTool(ctx, &sdk.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	var parts []string
	for _, c := range res.Content {
		if tc, ok := c.(*sdk.TextContent); ok {
			parts = append(parts, tc.Text)
		}
	}
	return res, strings.Join(parts, "\n")
}

// The tools create, list, change, and delete a runtime pool with the
// checks of the routes, and the tool fleet lists the runtime pools with
// their source and their template.
func TestTools(t *testing.T) {
	fr := newFront(t)
	alice, bob, carol := connect(t, fr.a, "alice"), connect(t, fr.a, "bob"), connect(t, fr.a, "carol")

	if res, text := call(t, bob, ToolTemplatesList, nil); res.IsError || !strings.Contains(text, "base in the atespace team, the default for a new agent, with the model openai/some-model") {
		t.Fatalf("templates_list = %q", text)
	}
	if res, text := call(t, bob, ToolPoolPut, map[string]any{"name": "helper", "agent": "helper", "replicas": 2}); !res.IsError || !strings.Contains(text, "Forbidden") {
		t.Fatalf("pool_put of bob = %q", text)
	}
	res, text := call(t, alice, ToolPoolPut, map[string]any{"name": "helper", "agent": "helper", "replicas": 2})
	if res.IsError || !strings.Contains(text, "Created the runtime pool helper of the template base for the agent helper, with 2 workers") || !strings.Contains(text, "waits for the profile helper") {
		t.Fatalf("pool_put = %q", text)
	}
	if res, text := call(t, alice, ToolPoolPut, map[string]any{"name": "helper", "agent": "helper", "replicas": 2}); !res.IsError || !strings.Contains(text, "call pool_put without agent") {
		t.Fatalf("a second create = %q", text)
	}
	if res, text := call(t, alice, ToolPoolsList, nil); res.IsError || !strings.Contains(text, "helper (a runtime pool of the template base)") || !strings.Contains(text, "triage (of the file, from the template base)") {
		t.Fatalf("pools_list = %q", text)
	}
	if res, text := call(t, alice, "fleet", nil); res.IsError || !strings.Contains(text, "helper (a runtime pool of the template base)") {
		t.Fatalf("fleet = %q", text)
	}
	if res, text := call(t, bob, ToolPoolPut, map[string]any{"name": "helper", "replicas": 1}); !res.IsError || !strings.Contains(text, "Forbidden") {
		t.Fatalf("a change of bob = %q", text)
	}
	if res, text := call(t, carol, ToolPoolPut, map[string]any{"name": "helper", "replicas": 1, "env": []map[string]string{{"name": "MODE", "value": "fast"}}}); res.IsError || !strings.Contains(text, "Changed the runtime pool helper: it wants 1 worker at generation 2") {
		t.Fatalf("a change of an admin = %q", text)
	}
	if e, _ := fr.store.Get(context.Background(), "helper"); len(e.Env) != 1 || e.Env[0].Value != "fast" {
		t.Fatalf("the entry = %+v", e)
	}
	if res, text := call(t, alice, ToolPoolPut, map[string]any{"name": "nope", "replicas": 1}); !res.IsError || !strings.Contains(text, "pass agent and replicas") {
		t.Fatalf("a change of no pool = %q", text)
	}
	if res, text := call(t, alice, ToolPoolDelete, map[string]any{"name": "helper"}); res.IsError || !strings.Contains(text, "Deleted the runtime pool helper") || !strings.Contains(text, "being deleted") {
		t.Fatalf("pool_delete = %q", text)
	}
	if res, text := call(t, alice, ToolPoolDelete, map[string]any{"name": "nope"}); !res.IsError || !strings.Contains(text, "Not found") {
		t.Fatalf("pool_delete of no pool = %q", text)
	}
}
