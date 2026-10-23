package mcp

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/surface/view"
	"github.com/bdubs00/featherweight-workbench/dashboard"
	"github.com/bdubs00/featherweight-workbench/fleet"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// listed returns the listed tools by name.
func listed(t *testing.T, ctx context.Context, cs *sdk.ClientSession) map[string]*sdk.Tool {
	t.Helper()
	res, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]*sdk.Tool{}
	for _, tool := range res.Tools {
		out[tool.Name] = tool
	}
	return out
}

func TestWorkViewToolsAreListed(t *testing.T) {
	ctx := testCtx(t)
	a := newAgent(t, newScript().model())
	cs := mustConnect(t, ctx, serveAgent(t, a, nil).URL, clientSetup{})
	tools := listed(t, ctx, cs)
	for _, name := range []string{toolRunResult, toolAgentBuilder, toolCapabilities} {
		tool := tools[name]
		if tool == nil {
			t.Fatalf("no tool %s", name)
		}
		// Only dashboard names the page, so a host shows it once.
		if tool.Meta != nil {
			t.Errorf("%s _meta = %v", name, tool.Meta)
		}
		if tool.Description == "" || tool.Annotations == nil || tool.Title == "" {
			t.Errorf("%s has no title, description, or annotations", name)
		}
		var props struct {
			Properties map[string]struct {
				Description string `json:"description"`
			} `json:"properties"`
		}
		schema, _ := json.Marshal(tool.InputSchema)
		if err := json.Unmarshal(schema, &props); err != nil {
			t.Fatal(err)
		}
		for arg, p := range props.Properties {
			if p.Description == "" {
				t.Errorf("%s argument %s has no description", name, arg)
			}
		}
		if !reservedName(name) {
			t.Errorf("an exposed tool can take the name %s", name)
		}
	}
	for _, name := range []string{toolRunResult, toolCapabilities} {
		if an := tools[name].Annotations; !an.ReadOnlyHint || !an.IdempotentHint {
			t.Errorf("%s annotations = %+v", name, an)
		}
	}
	if an := tools[toolAgentBuilder].Annotations; an.ReadOnlyHint || an.DestructiveHint == nil || *an.DestructiveHint {
		t.Errorf("agent_builder annotations = %+v", an)
	}
	// The builder tells a host that shows only text how to go on.
	desc := tools[toolAgentBuilder].Description
	for _, want := range []string{"send", "run_result", "run_events", "propose_agent", "profile", "agent_put", "dryRun"} {
		if !strings.Contains(desc, want) {
			t.Errorf("agent_builder description lacks %q: %s", want, desc)
		}
	}
	for _, want := range []string{`"input"`, `"fromRun"`} {
		if s := toolSchema(t, ctx, cs, toolAgentBuilder); !strings.Contains(s, want) {
			t.Errorf("agent_builder schema lacks %s: %s", want, s)
		}
	}
	for _, want := range []string{`"before"`, `"from"`} {
		if s := toolSchema(t, ctx, cs, toolRunEvents); !strings.Contains(s, want) {
			t.Errorf("run_events schema lacks %s: %s", want, s)
		}
	}
	if s := toolSchema(t, ctx, cs, toolRunFile); !strings.Contains(s, `"raw"`) {
		t.Errorf("run_file schema lacks raw: %s", s)
	}
	if s := toolSchema(t, ctx, cs, toolRun); !strings.Contains(s, `"title":{"description"`) || !strings.Contains(s, `"maxLength":200`) {
		t.Errorf("run schema lacks a bounded title: %s", s)
	}

	// Usage is deprecated and no other tool advertises it.
	if d := tools[toolUsage].Description; !strings.HasPrefix(d, "Deprecated:") || !strings.Contains(d, "Prometheus") || !strings.Contains(d, "/metrics") {
		t.Errorf("usage description = %q", d)
	}
	for _, name := range []string{toolDashboard, toolAgentsList, toolAgentGet} {
		if d := strings.ToLower(tools[name].Description); strings.Contains(d, "usage") {
			t.Errorf("%s description advertises usage: %s", name, d)
		}
	}
	r := cs.InitializeResult()
	if r == nil {
		t.Fatal("no initialize result")
	}
	for _, want := range []string{"run_result", "agent_builder", "capabilities", "before", "fw://runs/{id}/files/{path}", "this process only", "usage is deprecated"} {
		if !strings.Contains(r.Instructions, want) {
			t.Errorf("instructions lack %q", want)
		}
	}
	if strings.Contains(r.Instructions, "owners and usage") || strings.Contains(r.Instructions, "run_file, and usage") {
		t.Errorf("instructions still advertise usage:\n%s", r.Instructions)
	}
}

// reservedName reports whether an exposed tool cannot take the name.
func reservedName(name string) bool { return workTools[name] }

func TestRunTitle(t *testing.T) {
	ctx := testCtx(t)
	a := newAgent(t, newScript(endTurn("one"), endTurn("two"), endTurn("three")).model())
	cs := mustConnect(t, ctx, serveAgent(t, a, nil).URL, clientSetup{})

	res := callTool(t, ctx, cs, toolRun, map[string]any{"input": "fix the retries", "title": "  Fix checkout retries  "})
	if res.IsError {
		t.Fatal(textOf(res))
	}
	r := runOf(t, res)
	if r.Metadata[fw.MetaTitle] != "Fix checkout retries" || r.Title != "Fix checkout retries" {
		t.Fatalf("title = %q, metadata = %v", r.Title, r.Metadata)
	}
	// The title wins over a title in the metadata, and the rest stays.
	res = callTool(t, ctx, cs, toolRun, map[string]any{"input": "x", "title": "Mine", "metadata": map[string]string{fw.MetaTitle: "Theirs", "screen": "home"}})
	if r := runOf(t, res); r.Metadata[fw.MetaTitle] != "Mine" || r.Metadata["screen"] != "home" {
		t.Fatalf("metadata = %v", r.Metadata)
	}
	// Without a title, the run takes one from its input.
	res = callTool(t, ctx, cs, toolRun, map[string]any{"input": "Review the plan. Then stop."})
	if r := runOf(t, res); r.Metadata[fw.MetaTitle] != "" || r.Title != "Review the plan" {
		t.Fatalf("title = %q, metadata = %v", r.Title, r.Metadata)
	}
	// A title of more than 200 characters is refused before a run starts.
	long := strings.Repeat("é", 201)
	before, _ := a.List(ctx)
	got, err := cs.CallTool(ctx, &sdk.CallToolParams{Name: toolRun, Arguments: map[string]any{"input": "x", "title": long}})
	if err == nil && !got.IsError {
		t.Fatalf("a long title passed: %s", textOf(got))
	}
	if after, _ := a.List(ctx); len(after) != len(before) {
		t.Errorf("a run started with a long title")
	}
	if _, err := runMetadata(runArgs{Title: long}); err == nil || !strings.Contains(err.Error(), "201") {
		t.Errorf("runMetadata = %v", err)
	}
	if md, err := runMetadata(runArgs{Title: strings.Repeat("é", 200)}); err != nil || md[fw.MetaTitle] == "" {
		t.Errorf("a title of 200 characters = %v, %v", md, err)
	}
}

func TestRunResultTool(t *testing.T) {
	ctx := testCtx(t)
	a := manageAgent(t, newScript(endTurn("the review is done"), endTurn("shared")).model())
	url := serveAgent(t, a, nil).URL
	alice := as(t, ctx, url, "alice-token")
	bob := as(t, ctx, url, "bob-token")
	actx := fw.WithPrincipal(ctx, people["alice-token"])
	r, err := a.Run(actx, "review this", fw.AsAgent("reviewer"))
	if err != nil {
		t.Fatal(err)
	}
	hidden, err := a.Run(actx, "shared work", fw.AsAgent("shared"))
	if err != nil {
		t.Fatal(err)
	}

	res := callTool(t, ctx, bob, toolRunResult, map[string]any{"runId": r.ID})
	var got resultView
	structured(t, res, &got)
	want, err := dashboard.ResultOf(fw.WithPrincipal(ctx, people["bob-token"]), a, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError || got.View != viewResult || got.Result.RunID != r.ID || got.Result.Agent != "reviewer" || got.Result.Summary != "the review is done" {
		t.Fatalf("run_result = %+v: %s", got, textOf(res))
	}
	if textOf(res) != want.String() {
		t.Errorf("text = %q, want %q", textOf(res), want.String())
	}
	// The reader rules of the run apply.
	res = callTool(t, ctx, bob, toolRunResult, map[string]any{"runId": hidden.ID})
	if !res.IsError || !strings.Contains(textOf(res), "Forbidden") {
		t.Fatalf("bob reads a hidden result: %s", textOf(res))
	}
	res = callTool(t, ctx, alice, toolRunResult, map[string]any{"runId": "nope"})
	if !res.IsError || !strings.Contains(textOf(res), "No run has that id") {
		t.Fatalf("unknown run = %s", textOf(res))
	}
}

// fakeOwner is a member that answers some routes of the native API with
// canned bodies and records what it received.
type fakeOwner struct {
	mu     sync.Mutex
	paths  []string
	bodies []string
	auth   []string
}

func (o *fakeOwner) serve(t *testing.T, routes map[string]any) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	for pattern, body := range routes {
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			o.mu.Lock()
			o.paths = append(o.paths, r.Method+" "+r.URL.Path)
			o.bodies = append(o.bodies, string(b))
			o.auth = append(o.auth, r.Header.Get("Authorization"))
			o.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			if s, ok := body.(int); ok {
				w.WriteHeader(s)
				_, _ = w.Write([]byte(`{"detail":"the run is not here"}`))
				return
			}
			_ = json.NewEncoder(w).Encode(body)
		})
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func (o *fakeOwner) last() (path, body, auth string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if len(o.paths) == 0 {
		return "", "", ""
	}
	n := len(o.paths) - 1
	return o.paths[n], o.bodies[n], o.auth[n]
}

// remoteLedger is the ledger of a completed run of reviewer on a worker:
// alice asked to clean up, the model removed a file with rm, and the run
// answered done there.
func remoteLedger(id, principal string) (*fw.Run, []fw.Event) {
	run := &fw.Run{ID: id, Agent: "reviewer", State: fw.StateCompleted, Principal: principal, Input: "clean up", Output: "done there", Seq: 5, Origin: fw.OriginBus}
	evs := []fw.Event{
		fw.MustEvent("", fw.EvRunQueued, fw.RunQueuedData{Agent: "reviewer", Input: "clean up", Origin: fw.OriginBus, Principal: principal}),
		fw.MustEvent("", fw.EvRunStarted, fw.RunStartedData{Model: "fake/m"}),
		fw.MustEvent("", fw.EvTurnStarted, fw.TurnData{Turn: 1}),
		fw.MustEvent("", fw.EvModelResponded, fw.ModelRespondedData{Content: []fw.Block{fw.Text("done there"), {Type: fw.BlockToolUse, ID: "c1", Name: "rm", Input: []byte(`{"path":"x"}`)}}}),
		fw.MustEvent("", fw.EvRunFinished, fw.RunFinishedData{Outcome: fw.OutcomeCompleted, Output: "done there"}),
	}
	for i := range evs {
		evs[i].Subject, evs[i].Seq = id, uint64(i+1)
	}
	return run, evs
}

// A worker serves the native API of the kernel alone. run_result builds the
// result of its run from the run and the events that the worker serves,
// and agent_builder builds the first message of the builder the same way
// and starts the builder here; neither asks the worker for a route that
// only the workbench serves.
func TestRunResultAndBuilderReadTheOwner(t *testing.T) {
	ctx := testCtx(t)
	remote, events := remoteLedger("01REMOTE", "alice")
	other, otherEvents := remoteLedger("01OTHER", "bob")
	owner := &fakeOwner{}
	srv := owner.serve(t, map[string]any{
		"GET /v1/runs/01REMOTE/events": map[string]any{"run": remote, "events": events},
		"GET /v1/runs/01REMOTE":        remote,
		"GET /v1/runs/01OTHER":         other,
		"GET /v1/runs/01OTHER/events":  map[string]any{"run": other, "events": otherEvents},
		"GET /v1/runs/01GONE/events":   http.StatusNotFound,
	})
	fake := &fakeFleet{name: "view", owner: srv.URL, members: []fleet.Member{
		{Name: "view", Self: true, State: fleet.MemberLive, AccessMode: fw.AccessModeRules},
		{Name: "owner", State: fleet.MemberLive, CanForward: true, AccessMode: fw.AccessModeRules},
	}}
	fake.set(remote, other, &fw.Run{ID: "01GONE", Agent: "reviewer", State: fw.StateCompleted})
	builderAccess := fw.WithProfile(fw.AgentProfile{Name: "default", Access: &fw.Access{Operators: []string{"alice"}}})
	view := attach(manageAgent(t, newScript().model(), builderAccess), fake)
	fake.rules = view
	t.Setenv("FW_API_TOKEN", "fleet-token")
	cs := as(t, ctx, serveAgent(t, view, nil).URL, "alice-token")

	res := callTool(t, ctx, cs, toolRunResult, map[string]any{"runId": remote.ID})
	var got resultView
	structured(t, res, &got)
	if res.IsError || got.View != viewResult || got.Result.RunID != remote.ID || got.Result.Summary != "done there" || got.Result.Status != dashboard.StatusReady {
		t.Fatalf("remote run_result = %+v: %s", got, textOf(res))
	}
	if path, _, auth := owner.last(); path != "GET /v1/runs/01REMOTE/events" || auth != "Bearer fleet-token" {
		t.Errorf("read %q with %q", path, auth)
	}
	res = callTool(t, ctx, cs, toolRunResult, map[string]any{"runId": "01GONE"})
	if !res.IsError || !strings.Contains(textOf(res), "the fleet member owner") || !strings.Contains(textOf(res), "the run is not here") {
		t.Fatalf("a refused result = %s", textOf(res))
	}

	// agent_builder with a run of another member reads it there and starts
	// the builder here.
	res = callTool(t, ctx, cs, toolAgentBuilder, map[string]any{"fromRun": remote.ID})
	if res.IsError {
		t.Fatal(textOf(res))
	}
	started := runOf(t, res)
	if started.Member != "" || started.Metadata[dashboard.ViewKey] != dashboard.ViewBuilder || !strings.Contains(started.Input, "clean up") ||
		!strings.Contains(started.Input, "done there") || !strings.Contains(started.Input, "rm") || strings.Contains(textOf(res), "on the fleet member") {
		t.Fatalf("builder from a remote run = %+v: %s", started, textOf(res))
	}
	if _, err := view.Get(ctx, started.ID); err != nil {
		t.Fatalf("the builder does not run here: %v", err)
	}
	// The terms of Save as agent hold for a remote run too.
	res = callTool(t, ctx, cs, toolAgentBuilder, map[string]any{"fromRun": other.ID})
	if !res.IsError || !strings.Contains(textOf(res), "only the person who started") {
		t.Fatalf("builder from the run of bob = %s", textOf(res))
	}
	owner.mu.Lock()
	defer owner.mu.Unlock()
	for _, p := range owner.paths {
		if strings.HasSuffix(p, "/result") || strings.Contains(p, "/v1/builder") {
			t.Fatalf("a worker got a route that only the workbench serves: %s", p)
		}
	}
}

func TestAgentBuilderTool(t *testing.T) {
	ctx := testCtx(t)
	a := newAgent(t, newScript(endTurn("the plan is ready"), endTurn("What does it handle?"), endTurn("And this one?")).model())
	cs := mustConnect(t, ctx, serveAgent(t, a, nil).URL, clientSetup{})
	src, err := a.Run(ctx, "Plan the release of version 2.")
	if err != nil {
		t.Fatal(err)
	}

	// Exactly one of input and fromRun.
	for _, args := range []map[string]any{{}, {"input": "x", "fromRun": src.ID}, {"input": "   "}} {
		res := callTool(t, ctx, cs, toolAgentBuilder, args)
		if !res.IsError || !strings.Contains(textOf(res), "exactly one of input and fromRun") {
			t.Errorf("agent_builder %v = %s", args, textOf(res))
		}
	}

	res := callTool(t, ctx, cs, toolAgentBuilder, map[string]any{"input": "An agent that triages issues."})
	if res.IsError {
		t.Fatal(textOf(res))
	}
	r := runOf(t, res)
	if r.Input != "An agent that triages issues." || r.Origin != fw.OriginMCP || r.Metadata[dashboard.ViewKey] != dashboard.ViewBuilder {
		t.Fatalf("builder run = %+v", r)
	}
	if r.Title != "New agent" {
		t.Errorf("builder title = %q", r.Title)
	}
	txt := textOf(res)
	for _, want := range []string{"Started the agent builder in run " + r.ID, "send", "run_result", "agent_put", "dryRun"} {
		if !strings.Contains(txt, want) {
			t.Errorf("builder text lacks %q: %s", want, txt)
		}
	}
	// It returns at once, like run with wait false.
	if r.State.Terminal() {
		t.Errorf("the builder call waited: %s", r.State)
	}
	if _, err := a.Wait(ctx, r.ID); err != nil {
		t.Fatal(err)
	}

	// fromRun builds the first message from the run.
	want, err := dashboard.SaveAsAgentInput(ctx, a, src.ID)
	if err != nil {
		t.Fatal(err)
	}
	res = callTool(t, ctx, cs, toolAgentBuilder, map[string]any{"fromRun": src.ID})
	if res.IsError {
		t.Fatal(textOf(res))
	}
	if r := runOf(t, res); want == "" || r.Input != want || r.Metadata[dashboard.ViewKey] != dashboard.ViewBuilder {
		t.Fatalf("builder from a run = %+v, want input %q", r, want)
	}
	res = callTool(t, ctx, cs, toolAgentBuilder, map[string]any{"fromRun": "nope"})
	if !res.IsError || !strings.Contains(textOf(res), "No run has that id") {
		t.Fatalf("builder from an unknown run = %s", textOf(res))
	}
}

// A view of a run is not enough to turn it into an agent: the builder run
// belongs to the caller and the teams of the caller. agent_builder with
// fromRun takes a completed top-level run that the caller started, for a
// caller who can create agents.
func TestAgentBuilderFromRunTerms(t *testing.T) {
	ctx := testCtx(t)
	cfg := testConfig(t)
	cfg.Access.Default = "closed"
	cfg.Access.Creators = []string{"alice", "bob"}
	a := manageAgent(t, newScript(endTurn("I reviewed it."), endTurn("ok")).model(), fw.WithConfig(cfg),
		fw.WithProfile(fw.AgentProfile{Name: "default", Access: &fw.Access{Operators: []string{"alice", "bob"}}}))
	done, err := a.Run(fw.WithPrincipal(ctx, people["alice-token"]), "Review pull request 42", fw.AsAgent("reviewer"))
	if err != nil || done.State != fw.StateCompleted {
		t.Fatalf("run = %+v, %v", done, err)
	}
	url := serveAgent(t, a, nil).URL
	bob := as(t, ctx, url, "bob-token")
	// bob views the run of alice, he can create agents, and he operates
	// the agent of the builder.
	if res := callTool(t, ctx, bob, toolRunResult, map[string]any{"runId": done.ID}); res.IsError {
		t.Fatalf("bob reads the result: %s", textOf(res))
	}
	res := callTool(t, ctx, bob, toolAgentBuilder, map[string]any{"fromRun": done.ID})
	if !res.IsError || !strings.Contains(textOf(res), "Forbidden: only the person who started run "+done.ID) {
		t.Fatalf("bob saves the run of alice = %s", textOf(res))
	}
	// alice, who started it, can.
	alice := as(t, ctx, url, "alice-token")
	if res := callTool(t, ctx, alice, toolAgentBuilder, map[string]any{"fromRun": done.ID}); res.IsError {
		t.Fatalf("alice saves her run = %s", textOf(res))
	}
}

func TestCapabilitiesTool(t *testing.T) {
	ctx := testCtx(t)
	probe := fw.NewTool(fw.ToolSpec{Name: "probe", Title: "Probe the network", ReadOnly: true}, func(context.Context, json.RawMessage) (fw.ToolResult, error) {
		return fw.TextResult("ok"), nil
	})
	a := newAgent(t, newScript().model(), fw.WithTools(probe))
	cs := mustConnect(t, ctx, serveAgent(t, a, nil).URL, clientSetup{})
	res := callTool(t, ctx, cs, toolCapabilities, map[string]any{})
	var got capabilitiesResult
	structured(t, res, &got)
	want, err := view.Catalog(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError || got.View != viewCapabilities || got.Tools == nil || len(got.Tools) != len(want) {
		t.Fatalf("capabilities = %+v: %s", got, textOf(res))
	}
	var found *view.Capability
	for i := range got.Tools {
		if got.Tools[i].Name == "probe" {
			found = &got.Tools[i]
		}
	}
	if found == nil || found.Text != "Probe the network" || !found.ReadOnly {
		t.Fatalf("probe = %+v in %+v", found, got.Tools)
	}
	if txt := textOf(res); !strings.Contains(txt, "- Probe the network (probe)") {
		t.Errorf("capabilities text = %q", txt)
	}
}

func TestCapabilitiesText(t *testing.T) {
	tools := []view.Capability{
		{ToolSpec: fw.ToolSpec{Name: "gh.issue", Source: "github"}, Text: "Read an issue"},
		{ToolSpec: fw.ToolSpec{Name: "bash", Source: "builtin"}, Text: "Run shell commands"},
		{ToolSpec: fw.ToolSpec{Name: "peer.ask", Source: "atlas"}, Text: "Ask the atlas agent"},
		{ToolSpec: fw.ToolSpec{Name: "files"}, Text: ""},
	}
	want := "4 tools in the runtime.\n\n" +
		"Built-in:\n- Run shell commands (bash)\n- files (files)\n\n" +
		"From atlas:\n- Ask the atlas agent (peer.ask)\n\n" +
		"From github:\n- Read an issue (gh.issue)"
	if got := capabilitiesText(tools); got != want {
		t.Errorf("text =\n%s\nwant\n%s", got, want)
	}
	if got := capabilitiesText(nil); !strings.Contains(got, "no tool") {
		t.Errorf("empty = %q", got)
	}
}

func TestRunEventsBefore(t *testing.T) {
	ctx := testCtx(t)
	a := manageAgent(t, newScript(endTurn("one")).model())
	alice := as(t, ctx, serveAgent(t, a, nil).URL, "alice-token")
	r, err := a.Run(fw.WithPrincipal(ctx, people["alice-token"]), "review this", fw.AsAgent("reviewer"))
	if err != nil {
		t.Fatal(err)
	}
	last := r.Seq
	if last < 4 {
		t.Fatalf("the run has %d events", last)
	}

	// The last page names prev, the before of the page in front of it.
	res := callTool(t, ctx, alice, toolRunEvents, map[string]any{"runId": r.ID, "limit": 2})
	var ev eventsResult
	ev = eventsResult{}
	structured(t, res, &ev)
	if res.IsError || len(ev.Events) != 2 || ev.Events[0].Seq != last-1 || ev.Prev != last-1 || ev.Next != 0 {
		t.Fatalf("tail = %+v", ev)
	}
	if !strings.Contains(textOf(res), "call run_events with before "+itoa(last-1)) {
		t.Errorf("tail text = %q", textOf(res))
	}
	// before reads the page in front of it.
	res = callTool(t, ctx, alice, toolRunEvents, map[string]any{"runId": r.ID, "before": ev.Prev, "limit": 2})
	ev = eventsResult{}
	structured(t, res, &ev)
	if res.IsError || len(ev.Events) != 2 || ev.Events[0].Seq != last-3 || ev.Events[1].Seq != last-2 || ev.Prev != last-3 {
		t.Fatalf("before page = %+v: %s", ev, textOf(res))
	}
	// Reading back reaches the first event, and that page has no prev.
	res = callTool(t, ctx, alice, toolRunEvents, map[string]any{"runId": r.ID, "before": 3, "limit": 5})
	ev = eventsResult{}
	structured(t, res, &ev)
	if res.IsError || len(ev.Events) != 2 || ev.Events[0].Seq != 1 || ev.Prev != 0 || strings.Contains(textOf(res), "with before") {
		t.Fatalf("first page = %+v: %s", ev, textOf(res))
	}
	if len(ev.Items) == 0 || ev.Items[0].Kind != dashboard.ItemUser {
		t.Errorf("first page items = %+v", ev.Items)
	}
	// A page read forward names prev too.
	res = callTool(t, ctx, alice, toolRunEvents, map[string]any{"runId": r.ID, "from": 2, "limit": 1})
	ev = eventsResult{}
	structured(t, res, &ev)
	if ev.Prev != 2 || ev.Next != 3 {
		t.Fatalf("forward page = %+v", ev)
	}
	// from and before together are refused.
	res = callTool(t, ctx, alice, toolRunEvents, map[string]any{"runId": r.ID, "from": 1, "before": 3})
	if !res.IsError || !strings.Contains(textOf(res), "not both") {
		t.Fatalf("from and before = %s", textOf(res))
	}
}

func itoa(n uint64) string {
	b, _ := json.Marshal(n)
	return string(b)
}

func TestDashboardHome(t *testing.T) {
	ctx := testCtx(t)
	a := manageAgent(t, newScript(endTurn("one"), endTurn("two")).model())
	alice := as(t, ctx, serveAgent(t, a, nil).URL, "alice-token")
	actx := fw.WithPrincipal(ctx, people["alice-token"])
	r, err := a.Run(actx, "review this", fw.AsAgent("reviewer"), fw.WithMetadata(map[string]string{fw.MetaTitle: "Review the patch"}))
	if err != nil {
		t.Fatal(err)
	}
	all, err := dashboard.Runs(actx, a, "", "")
	if err != nil {
		t.Fatal(err)
	}
	want, err := dashboard.HomeOf(actx, a, all, time.Now())
	if err != nil {
		t.Fatal(err)
	}

	res := callTool(t, ctx, alice, toolDashboard, map[string]any{"view": "home"})
	var d dashboardResult
	structured(t, res, &d)
	if res.IsError || d.View != viewHome || d.Home == nil {
		t.Fatalf("home = %+v: %s", d, textOf(res))
	}
	// The old fields stay for the page in the field.
	if len(d.Runs) != 1 || d.Glance.Finished != 1 || len(d.Agents) != 2 || d.Whoami.Principal.Subject != "alice" {
		t.Fatalf("old fields = %+v", d)
	}
	if d.Home.Sentence != want.Sentence || len(d.Home.Recent) != len(want.Recent) {
		t.Fatalf("home = %+v, want %+v", d.Home, want)
	}
	if len(d.Home.Recent) != 1 || d.Home.Recent[0].ID != r.ID || d.Home.Recent[0].Title != "Review the patch" || d.Home.Recent[0].Status != dashboard.StatusReady {
		t.Fatalf("recent = %+v", d.Home.Recent)
	}
	txt := textOf(res)
	if !strings.HasPrefix(txt, want.Sentence) || !strings.Contains(txt, "Review the patch") || !strings.Contains(txt, "reviewer (you are an owner): Reviews code") {
		t.Errorf("home text = %q", txt)
	}
	if strings.Contains(txt, "tokens") {
		t.Errorf("home text shows usage: %q", txt)
	}

	// The other views carry no home.
	res = callTool(t, ctx, alice, toolDashboard, map[string]any{"view": "agent", "agent": "reviewer"})
	var other map[string]any
	structured(t, res, &other)
	if _, ok := other["home"]; ok || res.IsError {
		t.Errorf("agent view = %v", other)
	}

	// The usage view still answers and says first that it is deprecated.
	res = callTool(t, ctx, alice, toolDashboard, map[string]any{"view": "usage"})
	structured(t, res, &d)
	txt = textOf(res)
	if res.IsError || d.View != viewUsage || !strings.HasPrefix(txt, usageDeprecated+"\n") || !strings.Contains(txt, "Prometheus") || !strings.Contains(txt, "By agent") {
		t.Fatalf("usage view = %q", txt)
	}
	// The usage tool keeps its result.
	res = callTool(t, ctx, alice, toolUsage, map[string]any{})
	var u usageResult
	structured(t, res, &u)
	if res.IsError || u.View != viewUsage || u.Total.Runs != 1 {
		t.Fatalf("usage = %+v", u)
	}
}
