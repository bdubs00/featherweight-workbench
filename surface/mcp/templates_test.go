package mcp

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/fleet"
	"github.com/bdubs00/featherweight-workbench/internal/bucket"
	"github.com/bdubs00/featherweight-workbench/pools"
	"gopkg.in/yaml.v3"
)

// templatesBlock holds the default template base, with a model behind a
// proxy, and no pool.
const templatesBlock = `
templates:
  - name: base
    default: true
    image: ghcr.io/example/fw@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
    egress:
      hosts:
        - http://nats.agents.svc.cluster.local:8080
    fw:
      model: openai/some-model
      bus: {url: "ws://nats.agents.svc.cluster.local:8080"}
      access: {admins: ["kind:token"]}
`

// templateFront is a front with the templates of block, the overlays and
// the runtime pools in memory. The ax API does not answer, so no pass runs.
type templateFront struct {
	url      string
	store    *pools.Store
	overlays *pools.MemOverlays
}

func newTemplateFront(t *testing.T, block string) *templateFront {
	t.Helper()
	t.Setenv("FW_API_TOKEN", "fleet-secret-token")
	ctx := testCtx(t)
	tf := &templateFront{store: pools.NewStore(bucket.NewMem(pools.StoreBucket), nil), overlays: pools.NewMemOverlays()}
	oldO, oldR := fleet.OpenOverlays, fleet.OpenRuntime
	fleet.OpenOverlays = func(context.Context, *fw.Config, *slog.Logger) (pools.OverlayStore, func(), error) {
		return tf.overlays, nil, nil
	}
	fleet.OpenRuntime = func(context.Context, *fw.Config, *slog.Logger, bool) (*pools.Store, func(), error) {
		return tf.store, nil, nil
	}
	t.Cleanup(func() { fleet.OpenOverlays, fleet.OpenRuntime = oldO, oldR })
	cfg := testConfig(t)
	cfg.Agents = map[string]fw.AgentProfile{"default": {}}
	cfg.Bus.Driver, cfg.Bus.URL = "nats", "nats://nats.agents.svc.cluster.local:4222"
	cfg.AX.Server, cfg.AX.Router = "127.0.0.1:1", "127.0.0.1:1"
	var n yaml.Node
	if err := yaml.Unmarshal([]byte(block), &n); err != nil {
		t.Fatal(err)
	}
	cfg.Extensions = map[string]yaml.Node{fleet.Block: *n.Content[0]}
	front := newAgent(t, newScript().model(), fw.WithConfig(cfg), fw.WithAuthn(people))
	off := time.Duration(0)
	fc := fleet.Config{Name: "front", Egress: "fw.events", Atespaces: []string{"team"}, Interval: time.Hour, Expire: fleet.DefaultExpire, SuspendAfter: &off}
	var err error
	if fc.Pools, fc.Templates, err = fleet.LoadAll(cfg, fc); err != nil {
		t.Fatal(err)
	}
	f, err := fleet.Open(ctx, front, fc)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	attach(front, f)
	tf.url = serveAgent(t, front, nil).URL
	return tf
}

// agent_put of a new agent on a front with a default template creates the
// agent and a runtime pool of two workers named after it, writes the
// profile into the overlay of that pool, and returns the pool. workers
// sets the template and the replicas, a dry run creates nothing, a second
// put changes the profile in the pool, and agent_delete deletes the pool
// with the profile.
func TestAgentPutCreatesARuntimePool(t *testing.T) {
	ctx := testCtx(t)
	tf := newTemplateFront(t, templatesBlock)
	alice := as(t, ctx, tf.url, "alice-token")

	res := callTool(t, ctx, alice, toolAgentPut, map[string]any{"name": "planned", "dryRun": true})
	var draft draftResult
	structured(t, res, &draft)
	if res.IsError || !strings.Contains(textOf(res), "A save creates the agent and its runtime pool planned from the template base, with 2 workers. Nothing is saved.") ||
		draft.Pool == nil || draft.Pool.Wanted != 2 {
		t.Fatalf("a dry run = %q", textOf(res))
	}
	if list, _ := tf.store.List(ctx); len(list) != 0 {
		t.Fatalf("a dry run made a pool: %+v", list)
	}

	res = callTool(t, ctx, alice, toolAgentPut, map[string]any{"name": "writer", "description": "Writes answers."})
	if res.IsError || !strings.Contains(textOf(res), "Created the agent profile writer and its runtime pool writer from the template base, with 2 workers.") {
		t.Fatalf("agent_put = %q", textOf(res))
	}
	var got agentResult
	structured(t, res, &got)
	if !got.Created || got.Pool == nil || got.Pool.Name != "writer" || got.Pool.Source != pools.SourceRuntime || got.Pool.Template != "base" || got.Pool.Wanted != 2 ||
		got.Agent.Pool != "writer" || got.Agent.Overlay != fleet.OverlayAdded {
		t.Fatalf("the result = %+v", got)
	}
	if e, err := tf.store.Get(ctx, "writer"); err != nil || e.Replicas != 2 || e.Agent != "writer" || e.By != "alice" {
		t.Fatalf("the entry = %+v, %v", e, err)
	}
	if o, _ := tf.overlays.Read(ctx, "writer"); len(o.Profiles) != 1 || o.Profiles[0].Profile.Description != "Writes answers." {
		t.Fatalf("the overlay = %+v", o)
	}

	res = callTool(t, ctx, alice, toolAgentPut, map[string]any{"name": "big", "workers": map[string]any{"template": "base", "replicas": 3}})
	structured(t, res, &got)
	if res.IsError || got.Pool == nil || got.Pool.Wanted != 3 || !strings.Contains(textOf(res), "with 3 workers") {
		t.Fatalf("agent_put with workers = %q", textOf(res))
	}
	res = callTool(t, ctx, alice, toolAgentPut, map[string]any{"name": "writer", "description": "Writes more.", "workers": map[string]any{"replicas": 4}})
	if res.IsError || !strings.Contains(textOf(res), "Replaced the agent profile writer in the pool writer") || !strings.Contains(textOf(res), "runs the agent already, so workers changes nothing") {
		t.Fatalf("a second agent_put = %q", textOf(res))
	}
	if e, _ := tf.store.Get(ctx, "writer"); e.Replicas != 2 {
		t.Fatalf("workers changed the pool: %+v", e)
	}
	if res := callTool(t, ctx, alice, toolAgentPut, map[string]any{"name": "x", "workers": map[string]any{"template": "nope"}}); !res.IsError || !strings.Contains(textOf(res), "no template is named nope; the templates are base (the default)") {
		t.Fatalf("agent_put with an unknown template = %q", textOf(res))
	}

	var list agentsResult
	structured(t, callTool(t, ctx, alice, toolAgentsList, map[string]any{}), &list)
	pooled := map[string]string{}
	for _, ag := range list.Agents {
		pooled[ag.Name] = ag.Pool
	}
	if pooled["writer"] != "writer" || pooled["big"] != "big" {
		t.Fatalf("agents_list = %+v", list.Agents)
	}
	res = callTool(t, ctx, alice, toolAgentDelete, map[string]any{"name": "writer"})
	if res.IsError || !strings.Contains(textOf(res), "Deleted the agent profile writer and its runtime pool writer") {
		t.Fatalf("agent_delete = %q", textOf(res))
	}
	if e, err := tf.store.Get(ctx, "writer"); err != nil || !e.Deleting || e.Replicas != 0 {
		t.Fatalf("the entry after agent_delete = %+v, %v", e, err)
	}
}

// Without a default template, agent_put of a new agent needs workers, and
// the error names the templates.
func TestAgentPutWithoutADefaultTemplate(t *testing.T) {
	ctx := testCtx(t)
	tf := newTemplateFront(t, strings.Replace(templatesBlock, "    default: true\n", "", 1))
	alice := as(t, ctx, tf.url, "alice-token")
	res := callTool(t, ctx, alice, toolAgentPut, map[string]any{"name": "writer"})
	if !res.IsError || !strings.Contains(textOf(res), "no template is the default, so pass workers with a template") || !strings.Contains(textOf(res), "The templates are base.") {
		t.Fatalf("agent_put = %q", textOf(res))
	}
	if res := callTool(t, ctx, alice, toolAgentPut, map[string]any{"name": "writer", "workers": map[string]any{"template": "base", "replicas": 1}}); res.IsError {
		t.Fatalf("agent_put with workers = %q", textOf(res))
	}
	if e, err := tf.store.Get(ctx, "writer"); err != nil || e.Replicas != 1 {
		t.Fatalf("the entry = %+v, %v", e, err)
	}
}
