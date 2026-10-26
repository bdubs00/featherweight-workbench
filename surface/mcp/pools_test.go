package mcp

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/dashboard"
	"github.com/bdubs00/featherweight-workbench/fleet"
	"github.com/bdubs00/featherweight-workbench/pools"
	"gopkg.in/yaml.v3"
)

// poolsBlock is a pool of one worker of the profile triage, which alice
// owns. The policy of the workers asks before a ticket is created.
const poolsBlock = `
pools:
  - name: triage
    replicas: 1
    image: ghcr.io/example/fw@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
    egress:
      hosts:
        - http://nats.agents.svc.cluster.local:8080
    fw:
      model: openai/some-model
      bus: {url: "ws://nats.agents.svc.cluster.local:8080"}
      access: {admins: ["kind:token"]}
      policy: {default: allow, ask: [tickets.create_ticket]}
      agents:
        triage:
          description: Sorts new tickets.
          access: {owners: [alice]}
`

// pooledFront is a front with the pool of poolsBlock. The ax API does not
// answer, so no pass runs, and the router is a test server that plays the
// worker team/triage-0 for a decision and records it. The overlays live in
// memory.
type pooledFront struct {
	url     string
	bus     fw.Bus
	fleet   *fleet.Fleet
	store   *pools.MemOverlays
	mu      sync.Mutex
	decided []string // "<actor> <path> <decision>"
}

func newPooledFront(t *testing.T) *pooledFront {
	t.Helper()
	t.Setenv("FW_API_TOKEN", "fleet-secret-token")
	ctx := testCtx(t)
	pf := &pooledFront{bus: fw.NewMemBus(), store: pools.NewMemOverlays()}
	old := fleet.OpenOverlays
	fleet.OpenOverlays = func(context.Context, *fw.Config, *slog.Logger) (pools.OverlayStore, func(), error) {
		return pf.store, nil, nil
	}
	t.Cleanup(func() { fleet.OpenOverlays = old })
	router := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !strings.Contains(r.URL.Path, "/approvals/") {
			http.Error(w, "unexpected", http.StatusNotFound)
			return
		}
		var body struct {
			Decision string `json:"decision"`
		}
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &body)
		pf.mu.Lock()
		pf.decided = append(pf.decided, r.Header.Get("ate-target-actor")+" "+r.URL.Path+" "+body.Decision)
		pf.mu.Unlock()
		id := strings.Split(strings.TrimPrefix(r.URL.Path, "/v1/runs/"), "/")[0]
		_ = json.NewEncoder(w).Encode(fw.Run{ID: id, Agent: "triage", State: fw.StateRunning})
	}))
	t.Cleanup(router.Close)

	cfg := testConfig(t)
	cfg.Agents = map[string]fw.AgentProfile{"default": {}}
	cfg.Bus.Driver, cfg.Bus.URL = "nats", "nats://nats.agents.svc.cluster.local:4222"
	cfg.AX.Server, cfg.AX.Router = "127.0.0.1:1", strings.TrimPrefix(router.URL, "http://")
	var n yaml.Node
	if err := yaml.Unmarshal([]byte(poolsBlock), &n); err != nil {
		t.Fatal(err)
	}
	cfg.Extensions = map[string]yaml.Node{fleet.Block: *n.Content[0]}
	front := newAgent(t, newScript().model(), fw.WithConfig(cfg), fw.WithBus(pf.bus), fw.WithAuthn(people))
	off := time.Duration(0)
	fc := fleet.Config{Name: "front", Egress: "fw.events", Atespaces: []string{"team"}, Interval: time.Hour, Expire: fleet.DefaultExpire, SuspendAfter: &off}
	list, err := fleet.LoadPools(cfg, fc)
	if err != nil {
		t.Fatal(err)
	}
	fc.Pools = list
	f, err := fleet.Open(ctx, front, fc)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	attach(front, f)
	pf.fleet, pf.url = f, serveAgent(t, front, nil).URL
	return pf
}

// ask puts a run of triage on the worker team/triage-0 that waits for an
// approval of tickets.create_ticket, as the egress stream of the worker
// tells it, and waits until the view holds it.
func (pf *pooledFront) ask(t *testing.T, id string) {
	t.Helper()
	ctx := testCtx(t)
	for i, ev := range []egressEvent{
		{fw.EvRunQueued, fw.RunQueuedData{Agent: "triage", Input: "sort T-1", Origin: fw.OriginBus, Principal: "alice"}},
		{fw.EvApprovalRequested, fw.ApprovalRequestedData{ApprovalID: id + "-ask", Tool: "tickets.create_ticket", CallID: "c1", Input: json.RawMessage(`{"title":"T-1"}`), Reason: "rule: ask tickets.create_ticket"}},
	} {
		e := fw.MustEvent(id, ev.typ, ev.data)
		e.Seq, e.Node, e.Source = uint64(i+1), "ax:team/triage-0", "fw://team/triage-0/"+id
		if err := pf.bus.Publish(ctx, "fw.events.team.triage-0."+ev.typ, e); err != nil {
			t.Fatal(err)
		}
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if r, err := pf.fleet.Run(context.Background(), id); err == nil && r.Pending != nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the view does not hold the run %s", id)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// With pools, the agent tools read and write the profiles that the workers
// of a pool run: agents_list and agent_get name the pool and the policy of
// the workers, agent_put and agent_delete change the overlay of the pool
// for its owner only, and a profile of the front stays marked as such.
func TestAgentToolsWithPools(t *testing.T) {
	ctx := testCtx(t)
	pf := newPooledFront(t)
	alice, bob := as(t, ctx, pf.url, "alice-token"), as(t, ctx, pf.url, "bob-token")

	var list agentsResult
	res := callTool(t, ctx, alice, toolAgentsList, map[string]any{})
	structured(t, res, &list)
	if len(list.Agents) != 2 || list.Agents[0].Name != "triage" || list.Agents[0].Pool != "triage" || list.Agents[0].Role != "owner" ||
		list.Agents[1].Name != "default" || !list.Agents[1].Front || len(list.Agents[0].WorkerTools) == 0 {
		t.Fatalf("agents_list = %+v", list.Agents)
	}
	if text := textOf(res); !strings.Contains(text, "- triage (you are an owner) on the workers of the pool triage") || strings.Contains(text, "agent profiles") {
		t.Fatalf("agents_list text = %q", text)
	}
	res = callTool(t, ctx, alice, toolAgentGet, map[string]any{"name": "triage"})
	if text := textOf(res); res.IsError || !strings.Contains(text, "on the workers of the pool triage") || !strings.Contains(text, "asks first for tickets.create_ticket") || !strings.Contains(text, "The profile names no tools, so it has the tools of the workers of its pool: bash, files") {
		t.Fatalf("agent_get = %q", text)
	}
	if res := callTool(t, ctx, bob, toolAgentGet, map[string]any{"name": "triage", "pool": "nope"}); !res.IsError {
		t.Fatalf("agent_get in an unknown pool = %q", textOf(res))
	}

	if res := callTool(t, ctx, bob, toolAgentPut, map[string]any{"name": "triage", "system": "Take over."}); !res.IsError || !strings.Contains(textOf(res), "Forbidden") {
		t.Fatalf("agent_put of bob = %q", textOf(res))
	}
	res = callTool(t, ctx, alice, toolAgentPut, map[string]any{"name": "triage", "description": "Sorts every ticket.", "dryRun": true})
	if res.IsError || !strings.Contains(textOf(res), "Checked the agent profile triage in the pool triage") {
		t.Fatalf("a dry run = %q", textOf(res))
	}
	if o, _ := pf.store.Read(ctx, "triage"); !o.Empty() {
		t.Fatalf("a dry run wrote the overlay: %+v", o)
	}
	res = callTool(t, ctx, alice, toolAgentPut, map[string]any{"name": "triage", "description": "Sorts every ticket."})
	if res.IsError || !strings.Contains(textOf(res), "Replaced the agent profile triage in the pool triage") {
		t.Fatalf("agent_put = %q", textOf(res))
	}
	var got agentResult
	structured(t, res, &got)
	if got.Agent.Pool != "triage" || got.Agent.Overlay != fleet.OverlayReplaced || got.Agent.By != "alice" || got.Agent.Description != "Sorts every ticket." {
		t.Fatalf("the saved agent = %+v", got.Agent)
	}
	if o, _ := pf.store.Read(ctx, "triage"); len(o.Profiles) != 1 || o.Profiles[0].By != "alice" {
		t.Fatalf("the overlay = %+v", o)
	}
	if res := callTool(t, ctx, alice, toolAgentPut, map[string]any{"name": "newbie"}); !res.IsError || !strings.Contains(textOf(res), "triage (agent triage)") {
		t.Fatalf("agent_put of a new name = %q", textOf(res))
	}
	if res := callTool(t, ctx, alice, toolAgentDelete, map[string]any{"name": "triage"}); res.IsError || !strings.Contains(textOf(res), "the pool triage") {
		t.Fatalf("agent_delete = %q", textOf(res))
	}
	if o, _ := pf.store.Read(ctx, "triage"); !o.Empty() {
		t.Fatalf("the overlay after the delete = %+v", o)
	}
}

// approve with rule writes the allow rule into the policy of the pool for
// the owner of the profile, and then allows the call on the worker. Bob is
// no owner, so for him nothing is written and nothing is decided.
func TestApproveWithARule(t *testing.T) {
	ctx := testCtx(t)
	pf := newPooledFront(t)
	pf.ask(t, "01RULERUN")
	bob, alice := as(t, ctx, pf.url, "bob-token"), as(t, ctx, pf.url, "alice-token")
	args := map[string]any{"runId": "01RULERUN", "approvalId": "01RULERUN-ask", "decision": "allow", "rule": true, "note": "every ticket is reviewed"}

	if res := callTool(t, ctx, bob, toolApprove, args); !res.IsError || !strings.Contains(textOf(res), "Forbidden") || !strings.Contains(textOf(res), "Nothing is decided") {
		t.Fatalf("approve of bob = %q", textOf(res))
	}
	deny := map[string]any{"runId": "01RULERUN", "approvalId": "01RULERUN-ask", "decision": "deny", "rule": true}
	if res := callTool(t, ctx, alice, toolApprove, deny); !res.IsError {
		t.Fatalf("a rule with deny = %q", textOf(res))
	}
	pf.mu.Lock()
	n := len(pf.decided)
	pf.mu.Unlock()
	if o, _ := pf.store.Read(ctx, "triage"); n != 0 || !o.Empty() {
		t.Fatalf("a refusal decided %d times or wrote %+v", n, o)
	}

	res := callTool(t, ctx, alice, toolApprove, args)
	if res.IsError || !strings.Contains(textOf(res), "Added the rule allow tickets.create_ticket to the policy of the pool triage") {
		t.Fatalf("approve with a rule = %q", textOf(res))
	}
	o, _ := pf.store.Read(ctx, "triage")
	if len(o.Allow) != 1 || o.Allow[0].Rule != "tickets.create_ticket" || o.Allow[0].By != "alice" || o.Allow[0].Run != "01RULERUN" || o.Allow[0].Note != "every ticket is reviewed" {
		t.Fatalf("the rule = %+v", o.Allow)
	}
	pf.mu.Lock()
	defer pf.mu.Unlock()
	if len(pf.decided) != 1 || pf.decided[0] != "team/triage-0 /v1/runs/01RULERUN/approvals/01RULERUN-ask allow" {
		t.Fatalf("the decisions on the worker = %v", pf.decided)
	}
}

// Without pools, approve with rule explains that a rule needs a pool and
// decides nothing, agent_get refuses pool, and the tools work as before.
func TestApproveRuleNeedsAPool(t *testing.T) {
	ctx := testCtx(t)
	var calls atomic.Int32
	a := newAgent(t, newScript(toolUse("c1", "deploy", `{"target":"prod"}`), endTurn("done")).model(), fw.WithTools(deployTool(&calls)))
	cs := mustConnect(t, ctx, serveAgent(t, a, nil).URL, clientSetup{})
	r := runOf(t, callTool(t, ctx, cs, "run", map[string]any{"input": "ship it"}))
	if r.Pending == nil {
		t.Fatalf("run = %+v", r)
	}
	res := callTool(t, ctx, cs, toolApprove, map[string]any{"runId": r.ID, "approvalId": r.Pending.ID, "decision": "allow", "rule": true})
	if !res.IsError || !strings.Contains(textOf(res), "Approval rules need a pool") {
		t.Fatalf("approve with a rule = %q", textOf(res))
	}
	if now, _ := a.Get(ctx, r.ID); now.State != fw.StateInputRequired || calls.Load() != 0 {
		t.Fatalf("the run went on: %+v", now)
	}
	if res := callTool(t, ctx, cs, toolAgentGet, map[string]any{"name": "default", "pool": "triage"}); !res.IsError {
		t.Fatalf("agent_get with a pool = %q", textOf(res))
	}
	var list agentsResult
	structured(t, callTool(t, ctx, cs, toolAgentsList, map[string]any{}), &list)
	for _, ag := range list.Agents {
		if ag.Front || ag.Pool != "" {
			t.Fatalf("an agent without pools = %+v", ag)
		}
	}
	if res := callTool(t, ctx, cs, toolApprove, map[string]any{"runId": r.ID, "approvalId": r.Pending.ID, "decision": "allow"}); res.IsError {
		t.Fatalf("approve = %q", textOf(res))
	}
}

// run_events with compact keeps the first 200 characters of each tool
// output and its size, and without compact the whole output.
func TestRunEventsCompact(t *testing.T) {
	ctx := testCtx(t)
	long := strings.Repeat("0123456789", 100)
	echo := fw.NewTool(fw.ToolSpec{Name: "echo", ReadOnly: true}, func(context.Context, json.RawMessage) (fw.ToolResult, error) {
		return fw.TextResult(long), nil
	})
	a := newAgent(t, newScript(toolUse("c1", "echo", `{}`), endTurn("done")).model(), fw.WithTools(echo))
	cs := mustConnect(t, ctx, serveAgent(t, a, nil).URL, clientSetup{})
	r := runOf(t, callTool(t, ctx, cs, "run", map[string]any{"input": "go"}))
	output := func(args map[string]any) dashboard.Item {
		t.Helper()
		var res eventsResult
		structured(t, callTool(t, ctx, cs, toolRunEvents, args), &res)
		for _, it := range res.Items {
			if it.Kind == dashboard.ItemResult {
				return it
			}
		}
		t.Fatalf("no tool output in %+v", res.Items)
		return dashboard.Item{}
	}
	if it := output(map[string]any{"runId": r.ID, "compact": true}); len(it.Text) != 200 || it.Size != 1000 || !it.Cut {
		t.Fatalf("a compact output = %d characters, size %d, cut %v", len(it.Text), it.Size, it.Cut)
	}
	if it := output(map[string]any{"runId": r.ID}); it.Text != long {
		t.Fatalf("a full output = %d characters", len(it.Text))
	}
}
