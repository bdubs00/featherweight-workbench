package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/dashboard"
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

var people = tokenMap{
	"alice-token": {Subject: "alice", Name: "alice@example.com", Kind: fw.KindUser, Groups: []string{"platform"}},
	"bob-token":   {Subject: "bob", Kind: fw.KindUser},
}

// as connects a client as one of the people.
func as(t *testing.T, ctx context.Context, url, token string) *sdk.ClientSession {
	t.Helper()
	return mustConnect(t, ctx, url, clientSetup{header: http.Header{"Authorization": {"Bearer " + token}}})
}

// structured decodes the structured content of a result into v.
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

// manageAgent is an agent with three profiles: reviewer, which alice owns
// and bob can view, shared, which every platform member operates, and
// private, which neither can see. The configuration is closed, and alice is
// a creator.
func manageAgent(t *testing.T, m fw.Model, opts ...fw.Option) *fw.Agent {
	t.Helper()
	cfg := testConfig(t)
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

func TestManageToolsCarryTheDashboard(t *testing.T) {
	ctx := testCtx(t)
	a := newAgent(t, newScript().model())
	cs := mustConnect(t, ctx, serveAgent(t, a, nil).URL, clientSetup{})
	res, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	tools := map[string]*sdk.Tool{}
	for _, tool := range res.Tools {
		tools[tool.Name] = tool
	}
	for _, name := range []string{toolWhoami, toolAgentsList, toolAgentGet, toolAgentPut, toolAgentDelete, toolRunsList, toolRunEvents, toolUsage, toolDashboard} {
		tool := tools[name]
		if tool == nil {
			t.Fatalf("no tool %s", name)
		}
		// Only dashboard names the page, so a host shows it once.
		ui, _ := tool.Meta[dashboard.MetaKey].(map[string]any)
		if linked := ui != nil && ui["resourceUri"] == dashboard.URI; linked != (name == toolDashboard) {
			t.Errorf("%s _meta = %v", name, tool.Meta)
		}
		if tool.Description == "" || tool.Annotations == nil {
			t.Errorf("%s has no description or annotations", name)
		}
		schema, _ := json.Marshal(tool.InputSchema)
		var props struct {
			Properties map[string]struct {
				Description string `json:"description"`
			} `json:"properties"`
		}
		if err := json.Unmarshal(schema, &props); err != nil {
			t.Fatalf("%s schema: %v", name, err)
		}
		for arg, p := range props.Properties {
			if p.Description == "" {
				t.Errorf("%s argument %s has no description", name, arg)
			}
		}
	}
	for _, name := range []string{toolWhoami, toolAgentsList, toolAgentGet, toolRunsList, toolRunEvents, toolUsage, toolDashboard} {
		if !tools[name].Annotations.ReadOnlyHint {
			t.Errorf("%s is not read-only", name)
		}
	}
	for _, name := range []string{toolAgentPut, toolAgentDelete} {
		if an := tools[name].Annotations; an.ReadOnlyHint || an.DestructiveHint == nil || !*an.DestructiveHint {
			t.Errorf("%s annotations = %+v", name, an)
		}
	}
	if tools[toolRun].Meta != nil {
		t.Errorf("run carries a UI link: %v", tools[toolRun].Meta)
	}
	if r := cs.InitializeResult(); r == nil || !strings.Contains(r.Instructions, "dashboard") {
		t.Errorf("instructions do not mention the dashboard: %+v", r)
	} else if ext, _ := r.Capabilities.Extensions[dashboard.Extension].(map[string]any); ext == nil {
		t.Errorf("capabilities lack the extension %s: %+v", dashboard.Extension, r.Capabilities)
	}

	// The dashboard resource is listed and served as an MCP App.
	list, err := cs.ListResources(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var found *sdk.Resource
	for _, r := range list.Resources {
		if r.URI == dashboard.URI {
			found = r
		}
	}
	if found == nil || found.MIMEType != dashboard.MIMEType {
		t.Fatalf("dashboard resource = %+v", found)
	}
	read, err := cs.ReadResource(ctx, &sdk.ReadResourceParams{URI: dashboard.URI})
	if err != nil {
		t.Fatal(err)
	}
	c := read.Contents[0]
	if c.MIMEType != dashboard.MIMEType || c.URI != dashboard.URI || !strings.HasPrefix(strings.ToLower(c.Text), "<!doctype html>") || c.Text != string(dashboard.Page()) {
		t.Fatalf("dashboard contents = %q, %q, %d bytes", c.URI, c.MIMEType, len(c.Text))
	}
	if ui, _ := c.Meta[dashboard.MetaKey].(map[string]any); ui == nil {
		t.Errorf("dashboard contents _meta = %v", c.Meta)
	}
}

func TestWhoamiAndAgentsList(t *testing.T) {
	ctx := testCtx(t)
	a := manageAgent(t, newScript(endTurn("ok")).model())
	url := serveAgent(t, a, nil).URL
	alice := as(t, ctx, url, "alice-token")
	bob := as(t, ctx, url, "bob-token")
	if _, err := a.Run(fw.WithPrincipal(ctx, people["alice-token"]), "review", fw.AsAgent("reviewer")); err != nil {
		t.Fatal(err)
	}

	res := callTool(t, ctx, alice, toolWhoami, map[string]any{})
	var who whoamiResult
	structured(t, res, &who)
	if res.IsError || who.View != viewWhoami || who.Principal.Subject != "alice" || who.Principal.Name != "alice@example.com" ||
		who.Roles["reviewer"] != "owner" || who.Roles["shared"] != "operator" || who.Roles["private"] != "" || !who.CanCreate || who.Admin {
		t.Fatalf("whoami = %+v, %s", who, textOf(res))
	}
	if txt := textOf(res); !strings.Contains(txt, "alice") || !strings.Contains(txt, "reviewer: owner") || !strings.Contains(txt, "platform") {
		t.Errorf("whoami text = %q", txt)
	}

	res = callTool(t, ctx, bob, toolAgentsList, map[string]any{})
	var agents agentsResult
	structured(t, res, &agents)
	if res.IsError || agents.View != viewAgents || len(agents.Agents) != 1 || agents.Agents[0].Name != "reviewer" || agents.Agents[0].Role != "viewer" ||
		agents.Agents[0].Usage.Runs != 1 || agents.Agents[0].Usage.Tokens != 15 || agents.Agents[0].Description != "Reviews code" || agents.Agents[0].Access.Owners[0] != "alice" {
		t.Fatalf("bob agents = %+v", agents)
	}
	if txt := textOf(res); !strings.Contains(txt, "reviewer (you are a viewer)") || !strings.Contains(txt, "1 runs") {
		t.Errorf("agents text = %q", txt)
	}
	res = callTool(t, ctx, alice, toolAgentsList, map[string]any{})
	structured(t, res, &agents)
	if len(agents.Agents) != 2 || agents.Agents[0].Name != "reviewer" || agents.Agents[0].Role != "owner" || agents.Agents[1].Name != "shared" || agents.Agents[1].Role != "operator" {
		t.Fatalf("alice agents = %+v", agents)
	}

	res = callTool(t, ctx, bob, toolAgentGet, map[string]any{"name": "reviewer"})
	var one agentResult
	structured(t, res, &one)
	if res.IsError || one.View != viewAgent || one.Agent.Name != "reviewer" || one.Agent.Role != "viewer" || one.Agent.Description != "Reviews code" || !strings.Contains(textOf(res), `"description": "Reviews code"`) {
		t.Fatalf("agent_get = %+v, %s", one, textOf(res))
	}
	for _, name := range []string{"private", "nope"} {
		res = callTool(t, ctx, bob, toolAgentGet, map[string]any{"name": name})
		if !res.IsError || !strings.Contains(textOf(res), "No agent profile is named") || !strings.Contains(textOf(res), name) {
			t.Errorf("agent_get %s = %q, isError %v", name, textOf(res), res.IsError)
		}
	}
}

func TestAgentPutAndDelete(t *testing.T) {
	ctx := testCtx(t)
	a := manageAgent(t, newScript().model())
	url := serveAgent(t, a, nil).URL
	alice := as(t, ctx, url, "alice-token")
	bob := as(t, ctx, url, "bob-token")

	// A stranger cannot replace the profile of alice, and the error names the role.
	res := callTool(t, ctx, bob, toolAgentPut, map[string]any{"name": "reviewer", "description": "mine now"})
	if !res.IsError || !strings.Contains(textOf(res), "Forbidden") || !strings.Contains(textOf(res), "bob") || !strings.Contains(textOf(res), "owner entry on the agent reviewer") {
		t.Fatalf("bob put = %q, isError %v", textOf(res), res.IsError)
	}
	// A stranger cannot create either: the configuration is closed and names no creators.
	res = callTool(t, ctx, bob, toolAgentPut, map[string]any{"name": "bobs"})
	if !res.IsError || !strings.Contains(textOf(res), "Forbidden") || !strings.Contains(textOf(res), "access.creators") {
		t.Fatalf("bob create = %q, isError %v", textOf(res), res.IsError)
	}

	// The owner replaces the profile without an access block and keeps her ownership.
	res = callTool(t, ctx, alice, toolAgentPut, map[string]any{
		"name": "reviewer", "description": "Reviews pull requests", "system": "You review.", "instructions": []string{"Be kind."},
		"tools": []string{"files", "git.*"}, "skills": []string{}, "effort": "low", "timeZone": "Europe/Berlin",
		"policy": map[string]any{"default": "ask", "deny": []string{"bash"}},
		"budget": map[string]any{"maxTurns": 5, "deadline": "10m"},
	})
	var put agentResult
	structured(t, res, &put)
	p := put.Agent
	if res.IsError || put.Created || put.View != viewAgent || p.Description != "Reviews pull requests" || p.System != "You review." || p.Role != "owner" ||
		len(p.Tools) != 2 || p.Skills == nil || len(p.Skills) != 0 || p.Effort != "low" || p.TimeZone != "Europe/Berlin" || p.Policy == nil || p.Policy.Default != "ask" ||
		p.Budget == nil || p.Budget.MaxTurns != 5 || p.Budget.Deadline.Minutes() != 10 || p.Access == nil || p.Access.Owners[0] != "alice" || p.Access.Viewers[0] != "bob" {
		t.Fatalf("put = %+v, %s", p, textOf(res))
	}
	if !strings.HasPrefix(textOf(res), "Replaced the agent profile reviewer.") {
		t.Errorf("put text = %q", textOf(res))
	}
	// A creator makes a new profile and becomes its owner.
	res = callTool(t, ctx, alice, toolAgentPut, map[string]any{"name": "fresh", "description": "new"})
	structured(t, res, &put)
	if res.IsError || !put.Created || put.Agent.Access == nil || put.Agent.Access.Owners[0] != "alice" || put.Agent.Role != "owner" || !strings.HasPrefix(textOf(res), "Created") {
		t.Fatalf("create = %+v, %s", put, textOf(res))
	}
	// An explicit access block is kept as given, trimmed.
	res = callTool(t, ctx, alice, toolAgentPut, map[string]any{"name": "fresh", "access": map[string]any{"owners": []string{"alice", " group:platform "}, "viewers": []string{"*"}}})
	structured(t, res, &put)
	if res.IsError || len(put.Agent.Access.Owners) != 2 || put.Agent.Access.Owners[1] != "group:platform" || put.Agent.Access.Viewers[0] != "*" {
		t.Fatalf("access = %+v", put.Agent.Access)
	}
	// Bad arguments are explained.
	for name, args := range map[string]map[string]any{
		"effort": {"name": "fresh", "effort": "turbo"},
		"policy": {"name": "fresh", "policy": map[string]any{"default": "maybe"}},
		"budget": {"name": "fresh", "budget": map[string]any{"deadline": "soon"}},
		"model":  {"name": "fresh", "model": "nope"},
		"name":   {"name": "../etc"},
		"zone":   {"name": "fresh", "timeZone": "Mars/Olympus"},
	} {
		res = callTool(t, ctx, alice, toolAgentPut, args)
		if !res.IsError || strings.Contains(textOf(res), "fw:") {
			t.Errorf("bad %s = %q, isError %v", name, textOf(res), res.IsError)
		}
		if name == "zone" && (!strings.Contains(textOf(res), "timeZone") || strings.Contains(textOf(res), "profile.")) {
			t.Errorf("bad zone = %q", textOf(res))
		}
	}

	// Only an owner deletes; the stranger gets the role it lacks, and an
	// unknown name gets a clear answer.
	res = callTool(t, ctx, bob, toolAgentDelete, map[string]any{"name": "reviewer"})
	if !res.IsError || !strings.Contains(textOf(res), "Forbidden: bob agent.delete needs an owner entry on the agent reviewer") {
		t.Fatalf("bob delete = %q", textOf(res))
	}
	res = callTool(t, ctx, alice, toolAgentDelete, map[string]any{"name": "nope"})
	if !res.IsError || !strings.Contains(textOf(res), `No agent profile is named "nope"`) {
		t.Fatalf("delete nope = %q", textOf(res))
	}
	res = callTool(t, ctx, alice, toolAgentDelete, map[string]any{"name": "fresh"})
	var del deletedResult
	structured(t, res, &del)
	if res.IsError || del.Deleted != "fresh" || del.View != viewAgents {
		t.Fatalf("delete = %+v, %s", del, textOf(res))
	}
	if _, ok := a.ProfileNamed("fresh"); ok {
		t.Error("the profile is still there")
	}
}

func TestRunsListEventsAndUsage(t *testing.T) {
	ctx := testCtx(t)
	a := manageAgent(t, newScript(endTurn("one"), endTurn("two"), endTurn("three")).model())
	url := serveAgent(t, a, nil).URL
	alice := as(t, ctx, url, "alice-token")
	bob := as(t, ctx, url, "bob-token")
	actx := fw.WithPrincipal(ctx, people["alice-token"])
	r1, err := a.Run(actx, "review this", fw.AsAgent("reviewer"))
	if err != nil {
		t.Fatal(err)
	}
	r2, err := a.Run(actx, "shared work", fw.AsAgent("shared"))
	if err != nil {
		t.Fatal(err)
	}
	child, err := a.Run(ctx, "part", fw.AsAgent("shared"), fw.WithParent(r2.ID))
	if err != nil {
		t.Fatal(err)
	}

	res := callTool(t, ctx, bob, toolRunsList, map[string]any{})
	var runs runsResult
	structured(t, res, &runs)
	if res.IsError || runs.View != viewRuns || len(runs.Runs) != 1 || runs.Runs[0].ID != r1.ID || runs.Total != 1 {
		t.Fatalf("bob runs = %+v, %s", runs, textOf(res))
	}
	res = callTool(t, ctx, alice, toolRunsList, map[string]any{})
	structured(t, res, &runs)
	if len(runs.Runs) != 3 || runs.Runs[0].ID != child.ID || runs.Runs[0].Depth != 1 || runs.Runs[2].ID != r1.ID {
		t.Fatalf("alice runs = %+v", runs)
	}
	if txt := textOf(res); !strings.Contains(txt, "3 of 3 runs") || !strings.Contains(txt, "-   "+child.ID) || !strings.Contains(txt, "by alice") {
		t.Errorf("runs text = %q", txt)
	}
	// The glance counts the top-level runs only.
	if g := runs.Glance; g.Finished != 2 || g.Working != 0 || g.Sentence != "Nothing is running right now." {
		t.Errorf("alice glance = %+v", g)
	}
	res = callTool(t, ctx, alice, toolRunsList, map[string]any{"agent": "shared", "state": "completed", "principal": "alice", "limit": 1})
	structured(t, res, &runs)
	if len(runs.Runs) != 1 || runs.Total != 2 || runs.Runs[0].ID != child.ID {
		t.Fatalf("filtered runs = %+v", runs)
	}
	// The glance covers every run that matches, before the limit.
	if g := runs.Glance; g.Finished != 1 {
		t.Errorf("filtered glance = %+v", g)
	}
	// parent lists the child runs of one run.
	res = callTool(t, ctx, alice, toolRunsList, map[string]any{"parent": r2.ID})
	structured(t, res, &runs)
	if res.IsError || len(runs.Runs) != 1 || runs.Runs[0].ID != child.ID || runs.Total != 1 {
		t.Fatalf("children = %+v, %s", runs, textOf(res))
	}
	if res := callTool(t, ctx, bob, toolRunsList, map[string]any{"parent": r2.ID}); !res.IsError || !strings.Contains(textOf(res), "Forbidden") {
		t.Fatalf("bob children = %s", textOf(res))
	}
	res = callTool(t, ctx, alice, toolRunsList, map[string]any{"state": "active"})
	structured(t, res, &runs)
	if len(runs.Runs) != 0 || textOf(res) != "No run matches." {
		t.Fatalf("active runs = %+v, %q", runs, textOf(res))
	}

	res = callTool(t, ctx, alice, toolRunEvents, map[string]any{"runId": r1.ID, "limit": 2})
	var ev eventsResult
	structured(t, res, &ev)
	if res.IsError || ev.View != viewRun || ev.Run == nil || ev.Run.ID != r1.ID || len(ev.Events) != 2 || ev.Events[1].Type != fw.EvRunFinished || ev.Next != 0 {
		t.Fatalf("tail = %+v, %s", ev, textOf(res))
	}
	if txt := textOf(res); !strings.Contains(txt, "Run "+r1.ID+" is completed on reviewer") || !strings.Contains(txt, "finished: completed") {
		t.Errorf("events text = %q", txt)
	}
	if n := len(ev.Items); n == 0 || ev.Items[n-1].Kind != dashboard.ItemEnd || ev.Items[n-1].Text != fw.OutcomeCompleted {
		t.Fatalf("tail items = %+v", ev.Items)
	}
	res = callTool(t, ctx, alice, toolRunEvents, map[string]any{"runId": r1.ID, "from": 1, "limit": 2})
	structured(t, res, &ev)
	if len(ev.Events) != 2 || ev.Events[0].Seq != 1 || ev.Next != 3 || !strings.Contains(textOf(res), "from 3") {
		t.Fatalf("page = %+v, %s", ev, textOf(res))
	}
	// The chat items come from the same page of events.
	if len(ev.Items) != 1 || ev.Items[0].Kind != dashboard.ItemUser || ev.Items[0].Text != "review this" || ev.Items[0].Seq != 1 {
		t.Fatalf("page items = %+v", ev.Items)
	}
	res = callTool(t, ctx, bob, toolRunEvents, map[string]any{"runId": r2.ID})
	if !res.IsError || !strings.Contains(textOf(res), "Forbidden") || !strings.Contains(textOf(res), "bob") {
		t.Fatalf("bob events = %q", textOf(res))
	}
	res = callTool(t, ctx, alice, toolRunEvents, map[string]any{"runId": "nope"})
	if !res.IsError || !strings.Contains(textOf(res), "No run has that id") {
		t.Fatalf("unknown run = %q", textOf(res))
	}

	res = callTool(t, ctx, alice, toolUsage, map[string]any{})
	var usage usageResult
	structured(t, res, &usage)
	if res.IsError || usage.View != viewUsage || usage.Total.Runs != 3 || usage.ByAgent["shared"].Runs != 2 || usage.ByPrincipal["alice"].Runs != 3 || usage.Total.Turns != 3 {
		t.Fatalf("usage = %+v", usage)
	}
	if txt := textOf(res); !strings.Contains(txt, "By agent") || !strings.Contains(txt, "- shared: 2 runs") {
		t.Errorf("usage text = %q", txt)
	}
	res = callTool(t, ctx, bob, toolUsage, map[string]any{})
	var bobs usageResult
	structured(t, res, &bobs)
	if bobs.Total.Runs != 1 || len(bobs.ByAgent) != 1 || bobs.ByAgent["reviewer"].Runs != 1 {
		t.Fatalf("bob usage = %+v", bobs)
	}
}

func TestDashboardTool(t *testing.T) {
	ctx := testCtx(t)
	a := manageAgent(t, newScript(endTurn("one")).model())
	url := serveAgent(t, a, nil).URL
	alice := as(t, ctx, url, "alice-token")
	r, err := a.Run(fw.WithPrincipal(ctx, people["alice-token"]), "review this", fw.AsAgent("reviewer"))
	if err != nil {
		t.Fatal(err)
	}
	res := callTool(t, ctx, alice, toolDashboard, map[string]any{})
	var d dashboardResult
	structured(t, res, &d)
	if res.IsError || d.View != viewHome || d.Whoami.Principal.Subject != "alice" || len(d.Agents) != 2 || len(d.Runs) != 1 ||
		d.Glance.Finished != 1 || d.Glance.Sentence != "Nothing is running right now." || d.PageURL != "" {
		t.Fatalf("dashboard = %+v, %s", d, textOf(res))
	}
	txt := textOf(res)
	if d.Home == nil || !strings.HasPrefix(txt, d.Home.Sentence) || !strings.Contains(txt, dashboard.URI) ||
		!strings.Contains(txt, "reviewer (you are an owner)") || strings.Contains(txt, "Open it in a browser") {
		t.Errorf("dashboard text = %q", txt)
	}
	res = callTool(t, ctx, alice, toolDashboard, map[string]any{"view": "run", "runId": r.ID})
	structured(t, res, &d)
	if res.IsError || d.View != viewRun || d.Run == nil || d.Run.ID != r.ID || len(d.Events) == 0 || len(d.Items) == 0 || d.Items[0].Kind != dashboard.ItemUser {
		t.Fatalf("run view = %+v, %s", d, textOf(res))
	}
	res = callTool(t, ctx, alice, toolDashboard, map[string]any{"view": "agent", "agent": "reviewer"})
	structured(t, res, &d)
	if res.IsError || d.View != viewAgent || d.Agent == nil || d.Agent.Name != "reviewer" || d.Agent.Usage.Runs != 1 {
		t.Fatalf("agent view = %+v, %s", d, textOf(res))
	}
	if !strings.HasPrefix(textOf(res), "At a glance: Nothing is running right now.") {
		t.Errorf("agent view text = %q", textOf(res))
	}
	// The older names open home.
	for _, v := range []string{"agents", "runs", "approvals", "fleet", "home"} {
		d = dashboardResult{}
		res = callTool(t, ctx, alice, toolDashboard, map[string]any{"view": v})
		structured(t, res, &d)
		if res.IsError || d.View != viewHome || len(d.Runs) != 1 || d.Home == nil {
			t.Fatalf("view %s = %+v", v, d)
		}
	}
	res = callTool(t, ctx, alice, toolDashboard, map[string]any{"view": "nope"})
	if !res.IsError {
		t.Fatalf("an unknown view = %q", textOf(res))
	}
	res = callTool(t, ctx, alice, toolDashboard, map[string]any{"view": "agent", "agent": "private"})
	if !res.IsError || !strings.Contains(textOf(res), "private") {
		t.Fatalf("hidden agent = %q", textOf(res))
	}
}

func TestManageOverStdioUsesThePrincipal(t *testing.T) {
	ctx := testCtx(t)
	a := manageAgent(t, newScript().model())
	st, ct := sdk.NewInMemoryTransports()
	sctx, stop := context.WithCancel(fw.WithPrincipal(ctx, people["bob-token"]))
	defer stop()
	go func() { _ = serve(sctx, a, st) }()
	client := sdk.NewClient(&sdk.Implementation{Name: "stdio-client", Version: "v1"}, nil)
	cs, err := client.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	res := callTool(t, ctx, cs, toolWhoami, map[string]any{})
	var who whoamiResult
	structured(t, res, &who)
	if who.Principal.Subject != "bob" || who.Roles["reviewer"] != "viewer" || len(who.Roles) != 1 {
		t.Fatalf("whoami over stdio = %+v", who)
	}
	res = callTool(t, ctx, cs, toolAgentDelete, map[string]any{"name": "reviewer"})
	if !res.IsError || !strings.Contains(textOf(res), "Forbidden: bob") {
		t.Fatalf("delete over stdio = %q", textOf(res))
	}
}

func TestExplain(t *testing.T) {
	for _, tc := range []struct{ err, want string }{
		{"fw: forbidden: bob agent.edit needs an owner entry on the agent x", "Forbidden: bob agent.edit needs an owner entry on the agent x."},
		{"fw: forbidden", "Forbidden: the caller is not allowed to do this."},
	} {
		if got := explain(wrapForbidden(tc.err)); !strings.HasPrefix(got, tc.want) {
			t.Errorf("explain(%q) = %q, want prefix %q", tc.err, got, tc.want)
		}
	}
}

// wrapForbidden builds an error with that text that is fw.ErrForbidden.
func wrapForbidden(text string) error {
	return &textErr{text: text, is: fw.ErrForbidden}
}

type textErr struct {
	text string
	is   error
}

func (e *textErr) Error() string { return e.text }
func (e *textErr) Unwrap() error { return e.is }

// TestAuthzUnavailable checks that a control plane that does not answer
// reaches the client as an error result that says so, from the work tools
// and from the management tools.
func TestAuthzUnavailable(t *testing.T) {
	ctx := testCtx(t)
	down := fw.AuthzFunc(func(context.Context, *fw.Principal, string, fw.Target) error {
		return fmt.Errorf("%w: test", fw.ErrAuthzUnavailable)
	})
	a := manageAgent(t, newScript(endTurn("never")).model(), fw.WithAuthz(down))
	r, err := a.Run(ctx, "seed", fw.AsAgent("reviewer"))
	if err != nil {
		t.Fatal(err)
	}
	alice := as(t, ctx, serveAgent(t, a, nil).URL, "alice-token")
	for _, c := range []struct {
		tool string
		args map[string]any
	}{
		{toolRun, map[string]any{"input": "hi", "agent": "reviewer", "wait": false}},
		{toolRunStatus, map[string]any{"runId": r.ID}},
		{toolWhoami, map[string]any{}},
		{toolAgentsList, map[string]any{}},
		{toolAgentGet, map[string]any{"name": "reviewer"}},
		{toolDashboard, map[string]any{}},
	} {
		res := callTool(t, ctx, alice, c.tool, c.args)
		if text := textOf(res); !res.IsError || !strings.Contains(text, "the authorization service did not answer") || !strings.Contains(text, "test") || strings.Contains(text, "Forbidden") {
			t.Errorf("%s = %q, isError %v", c.tool, text, res.IsError)
		}
	}
}

func TestDashboardPageURL(t *testing.T) {
	ctx := testCtx(t)
	cfg := testConfig(t)
	cfg.MCP.Auth.Audience = "https://fleet.example.com/mcp"
	a := newAgent(t, newScript().model(), fw.WithConfig(cfg))
	cs := mustConnect(t, ctx, serveAgent(t, a, nil).URL, clientSetup{})
	res := callTool(t, ctx, cs, toolDashboard, map[string]any{})
	var d dashboardResult
	structured(t, res, &d)
	if res.IsError || d.PageURL != "https://fleet.example.com/ui" {
		t.Fatalf("dashboard = %+v, %s", d, textOf(res))
	}
	if txt := textOf(res); !strings.HasSuffix(txt, "\n\nOpen it in a browser: https://fleet.example.com/ui") {
		t.Errorf("dashboard text = %q", txt)
	}
	if got := withPageURL(a, "Members:\n"); got != "Members:\n\nOpen it in a browser: https://fleet.example.com/ui" {
		t.Errorf("withPageURL = %q", got)
	}
}

func TestAgentPutDryRun(t *testing.T) {
	ctx := testCtx(t)
	a := manageAgent(t, newScript().model())
	alice := as(t, ctx, serveAgent(t, a, nil).URL, "alice-token")
	res := callTool(t, ctx, alice, toolAgentPut, map[string]any{"name": "fresh", "description": "new", "tools": []string{"files"}, "dryRun": true})
	if res.IsError {
		t.Fatal(textOf(res))
	}
	var got draftResult
	structured(t, res, &got)
	if got.View != viewDraft || got.Draft.Name != "fresh" || got.Draft.Exists || !strings.Contains(textOf(res), "Nothing is saved") {
		t.Fatalf("dry run = %+v: %s", got, textOf(res))
	}
	if _, ok := a.ProfileNamed("fresh"); ok {
		t.Fatal("a dry run saved the profile")
	}
}

func TestRunMetadata(t *testing.T) {
	ctx := testCtx(t)
	a := newAgent(t, newScript(endTurn("ok")).model())
	cs := mustConnect(t, ctx, serveAgent(t, a, nil).URL, clientSetup{})
	res := callTool(t, ctx, cs, toolRun, map[string]any{"input": "hi", "metadata": map[string]string{"fw.view": "builder"}})
	if res.IsError {
		t.Fatal(textOf(res))
	}
	if r := runOf(t, res); r.Metadata["fw.view"] != "builder" {
		t.Fatalf("metadata = %v", r.Metadata)
	}
	res = callTool(t, ctx, cs, toolRun, map[string]any{"input": "hi", "metadata": map[string]string{"": "x"}})
	if !res.IsError || !strings.Contains(textOf(res), "metadata key") {
		t.Fatalf("an empty metadata key = %s", textOf(res))
	}
}

// TestAgentPutTakesNullSkills puts a profile as agent_get and a proposal
// give it, with "skills": null for every skill.
func TestAgentPutTakesNullSkills(t *testing.T) {
	ctx := testCtx(t)
	a := manageAgent(t, newScript().model())
	alice := as(t, ctx, serveAgent(t, a, nil).URL, "alice-token")
	res := callTool(t, ctx, alice, toolAgentPut, map[string]any{"name": "fresh", "tools": []string{"files"}, "skills": nil, "dryRun": true})
	if res.IsError {
		t.Fatal(textOf(res))
	}
}
