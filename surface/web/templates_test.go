package web

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/agents"
	"github.com/bdubs00/featherweight-workbench/fleet"
	"github.com/bdubs00/featherweight-workbench/internal/bucket"
	"github.com/bdubs00/featherweight-workbench/pools"
	"gopkg.in/yaml.v3"
)

// templatesBlock holds the default template base and no pool.
const templatesBlock = `
templates:
  - name: base
    default: true
    image: ghcr.io/example/fw@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
    egress:
      hosts:
        - http://nats.agents.svc.cluster.local:8080
        - http://ansible.agents.svc.cluster.local:8083
    fw:
      model: openai/some-model
      bus: {url: "ws://nats.agents.svc.cluster.local:8080"}
      access: {admins: ["kind:token"]}
      tools:
        builtin: [bash, files]
        mcp:
          - {name: ansible, endpoint: "http://ansible.agents.svc.cluster.local:8083/mcp"}
      policy: {default: allow, ask: ["ansible.playbook: check=false"]}
`

// templateServer serves a front with the templates of block, the
// overlays and the runtime pools in memory. The ax API does not answer, so
// no pass runs.
func templateServer(t *testing.T, block string) (*httptest.Server, *pools.Store) {
	t.Helper()
	t.Setenv("FW_API_TOKEN", "fleet-secret-token")
	store := pools.NewStore(bucket.NewMem(pools.StoreBucket), nil)
	overlays := pools.NewMemOverlays()
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
	cfg.Agents = map[string]fw.AgentProfile{"default": {}}
	cfg.Bus.Driver, cfg.Bus.URL = "nats", "nats://nats.agents.svc.cluster.local:4222"
	cfg.AX.Server, cfg.AX.Router = "127.0.0.1:1", "127.0.0.1:1"
	var n yaml.Node
	if err := yaml.Unmarshal([]byte(block), &n); err != nil {
		t.Fatal(err)
	}
	cfg.Extensions = map[string]yaml.Node{fleet.Block: *n.Content[0]}
	a := newAgent(t, newScript().model(), fw.WithConfig(cfg), fw.WithAuthn(people))
	off := time.Duration(0)
	fc := fleet.Config{Name: "front", Egress: "fw.events", Atespaces: []string{"team"}, Interval: time.Hour, Expire: fleet.DefaultExpire, SuspendAfter: &off}
	var err error
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
	return serve(t, a), store
}

// PUT /v1/agents/{name} of a new agent on a front with a default template
// creates the agent and a runtime pool of two workers named after it, and
// the answer carries the pool. workers in the body sets the replicas, and
// a dry run plans the pool without a write.
func TestPutAgentCreatesARuntimePool(t *testing.T) {
	srv, store := templateServer(t, templatesBlock)
	ctx := context.Background()

	var draft struct {
		agents.Draft
		Pool         *pools.Status `json:"pool"`
		WorkerTools  []string      `json:"workerTools"`
		WorkerPolicy *fw.Rules     `json:"workerPolicy"`
	}
	resp, b := do(t, "PUT", srv.URL+"/v1/agents/writer?dryRun=true", `{"description":"Writes."}`, "Authorization", asAlice)
	decodeJSON(t, resp, b, http.StatusOK, &draft)
	if draft.Pool == nil || draft.Pool.Name != "writer" || draft.Pool.Wanted != 2 || draft.Exists {
		t.Fatalf("a dry run = %s", b)
	}
	// The dry run says what the workers give the new agent, so the page can
	// word it before the save.
	if strings.Join(draft.WorkerTools, ",") != "bash,files,ansible.*" || draft.WorkerPolicy == nil || len(draft.WorkerPolicy.Ask) != 1 || draft.WorkerPolicy.Ask[0] != "ansible.playbook: check=false" {
		t.Fatalf("a dry run offers tools %v and policy %+v", draft.WorkerTools, draft.WorkerPolicy)
	}
	if list, _ := store.List(ctx); len(list) != 0 {
		t.Fatalf("a dry run made a pool: %+v", list)
	}

	var got struct {
		Name string        `json:"name"`
		Role string        `json:"role"`
		Pool *pools.Status `json:"pool"`
	}
	resp, b = do(t, "PUT", srv.URL+"/v1/agents/writer", `{"description":"Writes."}`, "Authorization", asAlice)
	decodeJSON(t, resp, b, http.StatusCreated, &got)
	if got.Name != "writer" || got.Role != "owner" || got.Pool == nil || got.Pool.Name != "writer" || got.Pool.Source != pools.SourceRuntime || got.Pool.Wanted != 2 || got.Pool.Template != "base" {
		t.Fatalf("PUT = %s", b)
	}
	resp, b = do(t, "PUT", srv.URL+"/v1/agents/big", `{"workers":{"replicas":3}}`, "Authorization", asAlice)
	decodeJSON(t, resp, b, http.StatusCreated, &got)
	if got.Pool == nil || got.Pool.Wanted != 3 {
		t.Fatalf("PUT with workers = %s", b)
	}
	// A change of the profile answers without a pool.
	resp, b = do(t, "PUT", srv.URL+"/v1/agents/writer", `{"description":"Writes more."}`, "Authorization", asAlice)
	if resp.StatusCode != http.StatusOK || strings.Contains(string(b), `"pool"`) {
		t.Fatalf("a second PUT = %d %s", resp.StatusCode, b)
	}
	// The read of the agent answers the profile that its workers run.
	resp, b = do(t, "GET", srv.URL+"/v1/agents/writer", "", "Authorization", asAlice)
	var read fleet.PoolAgent
	decodeJSON(t, resp, b, http.StatusOK, &read)
	if read.Pool != "writer" || read.Description != "Writes more." || read.Role != "owner" {
		t.Fatalf("GET = %s", b)
	}
	resp, b = do(t, "PUT", srv.URL+"/v1/agents/other?pool=writer", `{"workers":{}}`, "Authorization", asAlice)
	if p := decodeProblem(t, resp, b, http.StatusBadRequest); !strings.Contains(p.Detail, "not both") {
		t.Fatalf("PUT with pool and workers = %+v", p)
	}
	resp, b = do(t, "DELETE", srv.URL+"/v1/agents/big", "", "Authorization", asAlice)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE = %d %s", resp.StatusCode, b)
	}
	if e, err := store.Get(ctx, "big"); err != nil || !e.Deleting {
		t.Fatalf("the pool of big = %+v, %v", e, err)
	}
}

// labBlock is templatesBlock with the workers of the lab: the built-in
// tools bash, files, and skill, and the MCP servers front and ansible.
var labBlock = strings.NewReplacer(
	"        - http://ansible.agents.svc.cluster.local:8083\n", "        - http://ansible.agents.svc.cluster.local:8083\n        - http://front.agents.svc.cluster.local:8080\n",
	"builtin: [bash, files]\n        mcp:\n", "builtin: [bash, files, skill]\n        mcp:\n          - {name: front, endpoint: \"http://front.agents.svc.cluster.local:8080/mcp\"}\n",
).Replace(templatesBlock)

// PUT /v1/agents/{name} refuses a tool that the workers of the pool of the
// agent do not offer, with 400 and a detail that names each missing tool
// and what the pool offers. A dry run refuses it too, and neither creates
// a pool.
func TestPutAgentRefusesAToolThatThePoolLacks(t *testing.T) {
	if labBlock == templatesBlock {
		t.Fatal("the block of the lab is the block of the templates")
	}
	srv, store := templateServer(t, labBlock)
	want := "The pool of away-test offers no tool clock or ask_user: it offers the tools bash, files, and skill, and the tools of the servers front and ansible"
	body := `{"tools":["clock","ask_user","bash","front.*","clock"]}`
	for _, url := range []string{srv.URL + "/v1/agents/away-test?dryRun=true", srv.URL + "/v1/agents/away-test"} {
		resp, b := do(t, "PUT", url, body, "Authorization", asAlice)
		if p := decodeProblem(t, resp, b, http.StatusBadRequest); p.Detail != want {
			t.Fatalf("PUT %s = %+v", url, p)
		}
	}
	if list, _ := store.List(context.Background()); len(list) != 0 {
		t.Fatalf("a refused PUT made a pool: %+v", list)
	}
	resp, b := do(t, "PUT", srv.URL+"/v1/agents/away-test", `{"tools":["bash","files","skill","front.*","ansible.playbook"]}`, "Authorization", asAlice)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("PUT with the tools of the pool = %d %s", resp.StatusCode, b)
	}
	resp, b = do(t, "PUT", srv.URL+"/v1/agents/away-test", `{"tools":["bash","github.*"]}`, "Authorization", asAlice)
	if p := decodeProblem(t, resp, b, http.StatusBadRequest); p.Detail != "The pool of away-test offers no tool github.*: it offers the tools bash, files, and skill, and the tools of the servers front and ansible" {
		t.Fatalf("PUT of a server that the pool lacks = %+v", p)
	}
}
