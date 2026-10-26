package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/surface/native"
	"github.com/bdubs00/featherweight-workbench/dashboard"
	"github.com/bdubs00/featherweight-workbench/fleet"
	"github.com/bdubs00/featherweight-workbench/pools"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// fakeFleet is a fleet with one other member, "owner", that a test server
// with the native API stands for. Forward sends each request there, as the
// fleet driver does, and records its headers. It decides an action for a
// person with the rules of rules (fleet.Authorizer), as the view decides
// with the rules of the front, and without rules it decides nothing.
type fakeFleet struct {
	name    string
	owner   string
	members []fleet.Member
	rules   *fw.Agent

	mu      sync.Mutex
	runs    []*fw.Run
	err     error
	forward []http.Header
}

func (f *fakeFleet) Name() string { return f.name }

// AllowRun implements fleet.Authorizer.
func (f *fakeFleet) AllowRun(ctx context.Context, id, action string) error {
	r, err := f.Run(ctx, id)
	if err != nil {
		return err
	}
	if f.rules == nil {
		return fmt.Errorf("%w: the fake decides nothing", fw.ErrForbidden)
	}
	return f.rules.Authorize(ctx, action, fw.Target{Agent: r.Agent, Run: r})
}

// AllowAgent implements fleet.Authorizer.
func (f *fakeFleet) AllowAgent(ctx context.Context, _, agent, action string) error {
	if f.rules == nil {
		return fmt.Errorf("%w: the fake decides nothing", fw.ErrForbidden)
	}
	return f.rules.Authorize(ctx, action, fw.Target{Agent: agent})
}

// tokenOwner is manageAgent for the member that owns the runs of a fleet
// test: the static token of the fleet, fleet-token, is an admin there, as
// the token of a pool is on its workers, and the front holds the token.
func tokenOwner(t *testing.T, m fw.Model, opts ...fw.Option) *fw.Agent {
	t.Helper()
	t.Setenv("FW_API_TOKEN", "fleet-token")
	return manageAgentWith(t, m, func(c *fw.Config) { c.Access.Admins = []string{"kind:token"} }, opts...)
}

func (f *fakeFleet) Members(context.Context) ([]fleet.Member, error) { return f.members, nil }

func (f *fakeFleet) Runs(context.Context) ([]*fw.Run, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]*fw.Run, len(f.runs))
	for i, r := range f.runs {
		c := *r
		out[i] = &c
	}
	return out, nil
}

func (f *fakeFleet) Run(_ context.Context, id string) (*fw.Run, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.runs {
		if r.ID == id {
			c := *r
			return &c, nil
		}
	}
	return nil, fw.ErrRunNotFound
}

func (f *fakeFleet) Forward(ctx context.Context, member string, req *http.Request) (*http.Response, error) {
	f.mu.Lock()
	ferr := f.err
	f.mu.Unlock()
	if ferr != nil {
		return nil, ferr
	}
	cred := fw.CredentialFrom(ctx)
	if cred == "" {
		return nil, fleet.ErrNoCredential
	}
	u, err := url.Parse(f.owner + req.URL.String())
	if err != nil {
		return nil, err
	}
	out := req.Clone(ctx)
	out.URL, out.Host, out.RequestURI = u, u.Host, ""
	out.Header.Set("Authorization", cred)
	out.Header.Set(fleet.HeaderVia, f.name)
	// As the fleet does, the person comes from the context alone.
	out.Header.Del(fleet.HeaderOnBehalfOf)
	if who := fleet.OnBehalfOf(ctx); who != "" {
		out.Header.Set(fleet.HeaderOnBehalfOf, who)
	}
	f.mu.Lock()
	f.forward = append(f.forward, out.Header.Clone())
	f.mu.Unlock()
	return http.DefaultClient.Do(out)
}

func (f *fakeFleet) set(runs ...*fw.Run) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.runs = nil
	for _, r := range runs {
		c := *r
		c.Member = "owner"
		f.runs = append(f.runs, &c)
	}
}

// forwards counts the requests that Forward sent.
func (f *fakeFleet) forwards() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.forward)
}

func (f *fakeFleet) lastForward() http.Header {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.forward) == 0 {
		return nil
	}
	return f.forward[len(f.forward)-1]
}

// toolSchema returns the input schema of a listed tool as text.
func toolSchema(t *testing.T, ctx context.Context, cs *sdk.ClientSession, name string) string {
	t.Helper()
	res, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range res.Tools {
		if tool.Name == name {
			b, _ := json.Marshal(tool.InputSchema)
			return string(b)
		}
	}
	return ""
}

func TestFleetTools(t *testing.T) {
	ctx := testCtx(t)
	var calls atomic.Int32
	owner := tokenOwner(t, newScript(toolUse("c1", "deploy", `{"env":"prod"}`), endTurn("done"), endTurn("again"), endTurn("third")).model(),
		fw.WithTools(deployTool(&calls)), fw.WithSurfaces(native.New()))
	h, err := owner.Handler()
	if err != nil {
		t.Fatal(err)
	}
	ownerSrv := httptest.NewServer(h)
	t.Cleanup(ownerSrv.Close)

	// A run on the owner that waits for an approval of deploy.
	ownerCS := as(t, ctx, serveAgent(t, owner, nil).URL, "alice-token")
	remote := runOf(t, callTool(t, ctx, ownerCS, toolRun, map[string]any{"agent": "reviewer", "input": "ship it", "wait": false}))
	r, err := owner.WaitFor(ctx, remote.ID, func(r *fw.Run) bool { return r.Pending != nil || r.State.Terminal() })
	if err != nil || r.Pending == nil {
		t.Fatalf("owner run = %+v, %v", r, err)
	}

	fake := &fakeFleet{name: "view", owner: ownerSrv.URL, members: []fleet.Member{
		{Name: "view", Self: true, State: fleet.MemberLive, AccessMode: fw.AccessModeRules},
		{Name: "owner", State: fleet.MemberLive, CanForward: true, AccessMode: fw.AccessModeRules, Agents: []fleet.RecordAgent{{Name: "reviewer", Role: "owner"}}},
	}}
	fake.set(r)
	view := attach(manageAgent(t, newScript(endTurn("local")).model()), fake)
	fake.rules = view
	cs := as(t, ctx, serveAgent(t, view, nil).URL, "alice-token")

	// The fleet tool and the fleet arguments exist.
	for name, want := range map[string]string{toolRunsList: `"scope"`, toolRun: `"member"`, toolDashboard: `"fleet"`, toolFleet: `"object"`} {
		if s := toolSchema(t, ctx, cs, name); !strings.Contains(s, want) {
			t.Errorf("%s schema lacks %s: %s", name, want, s)
		}
	}
	res := callTool(t, ctx, cs, toolFleet, map[string]any{})
	var fl fleetResult
	structured(t, res, &fl)
	if res.IsError || fl.View != viewFleet || fl.Name != "view" || len(fl.Members) != 2 || !strings.Contains(textOf(res), "reviewer (you are owner)") {
		t.Fatalf("fleet = %+v: %s", fl, textOf(res))
	}

	// runs_list holds the runs of the fleet, and scope and member narrow it.
	local := runOf(t, callTool(t, ctx, cs, toolRun, map[string]any{"agent": "reviewer", "input": "here"}))
	list := func(args map[string]any) runsResult {
		t.Helper()
		var out runsResult
		structured(t, callTool(t, ctx, cs, toolRunsList, args), &out)
		return out
	}
	if l := list(map[string]any{}); l.Total != 2 {
		t.Fatalf("fleet runs = %+v", l)
	}
	if l := list(map[string]any{"scope": "local"}); l.Total != 1 || l.Runs[0].ID != local.ID {
		t.Fatalf("local runs = %+v", l)
	}
	if l := list(map[string]any{"member": "owner"}); l.Total != 1 || l.Runs[0].ID != remote.ID || l.Runs[0].Member != "owner" {
		t.Fatalf("owner runs = %+v", l)
	}
	if res := callTool(t, ctx, cs, toolRunsList, map[string]any{"scope": "nowhere"}); !res.IsError {
		t.Errorf("an unknown scope passed: %s", textOf(res))
	}

	// run_status and run_events reach the owner, with the static token for
	// alice.
	st := runOf(t, callTool(t, ctx, cs, toolRunStatus, map[string]any{"runId": remote.ID}))
	if st.Member != "owner" || st.Pending == nil {
		t.Fatalf("remote status = %+v", st)
	}
	if h := fake.lastForward(); h.Get("Authorization") != "Bearer fleet-token" || h.Get(fleet.HeaderOnBehalfOf) != "alice" || h.Get(fleet.HeaderVia) != "view" {
		t.Fatalf("forwarded headers = %v", h)
	}
	var ev eventsResult
	res = callTool(t, ctx, cs, toolRunEvents, map[string]any{"runId": remote.ID, "from": 1, "limit": 2})
	structured(t, res, &ev)
	if ev.Run == nil || ev.Run.Member != "owner" || len(ev.Events) != 2 || ev.Next != 3 || !strings.Contains(ev.Events[0].Line, "queued for the agent reviewer") {
		t.Fatalf("remote events = %+v: %s", ev, textOf(res))
	}
	// The view builds the chat items from the events of the owner.
	if len(ev.Items) != 1 || ev.Items[0].Kind != dashboard.ItemUser || ev.Items[0].Text != "ship it" {
		t.Fatalf("remote items = %+v", ev.Items)
	}
	// before reads back on the owner too, through its events route.
	ev = eventsResult{}
	res = callTool(t, ctx, cs, toolRunEvents, map[string]any{"runId": remote.ID, "before": 3, "limit": 1})
	structured(t, res, &ev)
	if res.IsError || ev.Run == nil || ev.Run.Member != "owner" || len(ev.Events) != 1 || ev.Events[0].Seq != 2 || ev.Prev != 2 || ev.Next != 0 {
		t.Fatalf("remote before = %+v: %s", ev, textOf(res))
	}
	ev = eventsResult{}
	res = callTool(t, ctx, cs, toolRunEvents, map[string]any{"runId": remote.ID, "before": 3, "limit": 10})
	structured(t, res, &ev)
	if res.IsError || len(ev.Events) != 2 || ev.Events[0].Seq != 1 || ev.Prev != 0 {
		t.Fatalf("remote first page = %+v: %s", ev, textOf(res))
	}
	ev = eventsResult{}
	res = callTool(t, ctx, cs, toolRunEvents, map[string]any{"runId": remote.ID, "before": 1})
	structured(t, res, &ev)
	if res.IsError || ev.Run == nil || ev.Run.Member != "owner" || len(ev.Events) != 0 || ev.Prev != 0 {
		t.Fatalf("remote page before the first event = %+v: %s", ev, textOf(res))
	}

	// Bob can only view the reviewer agent: the front refuses his approval,
	// and nothing reaches the owner.
	bob := as(t, ctx, serveAgent(t, view, nil).URL, "bob-token")
	before := fake.forwards()
	res = callTool(t, ctx, bob, toolApprove, map[string]any{"runId": remote.ID, "approvalId": st.Pending.ID, "decision": "allow"})
	if !res.IsError || !strings.Contains(textOf(res), "Forbidden: bob run.operate needs an operator entry on the agent reviewer") || fake.forwards() != before {
		t.Fatalf("bob approve = %s", textOf(res))
	}

	// Alice approves through the view, with the static token that names
	// her, so an owner that knows the header records her as the decider.
	res = callTool(t, ctx, cs, toolApprove, map[string]any{"runId": remote.ID, "approvalId": st.Pending.ID, "decision": "allow", "note": "go"})
	if res.IsError || runOf(t, res).Member != "owner" {
		t.Fatalf("approve = %s", textOf(res))
	}
	if h := fake.lastForward(); h.Get("Authorization") != "Bearer fleet-token" || h.Get(fleet.HeaderOnBehalfOf) != "alice" {
		t.Fatalf("the approval carries %v", h)
	}
	done, err := owner.Wait(ctx, remote.ID)
	if err != nil || done.State != fw.StateCompleted || calls.Load() != 1 {
		t.Fatalf("owner run = %+v, %v, calls %d", done, err, calls.Load())
	}
	if d := decided(t, owner, remote.ID); d.Note != "go" {
		t.Fatalf("decided = %+v", d)
	}

	// send and cancel reach the owner too.
	if res := callTool(t, ctx, cs, toolSend, map[string]any{"runId": remote.ID, "content": "more"}); res.IsError || runOf(t, res).Member != "owner" {
		t.Fatalf("send = %s", textOf(res))
	}
	if _, err := owner.WaitFor(ctx, remote.ID, func(r *fw.Run) bool { return r.State.Terminal() }); err != nil {
		t.Fatal(err)
	}
	if res := callTool(t, ctx, cs, toolCancel, map[string]any{"runId": remote.ID}); res.IsError {
		t.Fatalf("cancel = %s", textOf(res))
	}

	// run with member starts on the owner and does not wait.
	res = callTool(t, ctx, cs, toolRun, map[string]any{"agent": "shared", "input": "remote work", "member": "owner", "requestId": "r-1", "title": "Remote work"})
	started := runOf(t, res)
	if res.IsError || started.Member != "owner" || !strings.Contains(textOf(res), "run_status") {
		t.Fatalf("remote run = %+v: %s", started, textOf(res))
	}
	if h := fake.lastForward(); h.Get("Idempotency-Key") != "r-1" || h.Get(fleet.HeaderOnBehalfOf) != "alice" {
		t.Errorf("forwarded headers = %v", h)
	}
	if got, err := owner.Get(context.Background(), started.ID); err != nil || got.Metadata[fw.MetaTitle] != "Remote work" || got.Principal != "alice" {
		t.Fatalf("the owner run = %+v, %v", got, err)
	}

	// The old fleet view opens home, which carries the members.
	var dash dashboardResult
	res = callTool(t, ctx, cs, toolDashboard, map[string]any{"view": "fleet"})
	structured(t, res, &dash)
	if dash.View != viewHome || dash.Whoami.Fleet != "view" || len(dash.Members) != 2 || dash.Glance.Sentence == "" {
		t.Fatalf("dashboard = %+v", dash)
	}
	if txt := textOf(res); dash.Home == nil || !strings.HasPrefix(txt, dash.Home.Sentence) || !strings.Contains(txt, "This process is the fleet member view.") {
		t.Errorf("dashboard text = %q", txt)
	}

	// A fleet that cannot route answers with an error result.
	fake.mu.Lock()
	fake.err = errors.Join(fleet.ErrNoRoute, errors.New("the member owner has no URL"))
	fake.mu.Unlock()
	if res := callTool(t, ctx, cs, toolRunStatus, map[string]any{"runId": remote.ID}); !res.IsError || !strings.Contains(textOf(res), "No route") {
		t.Fatalf("no route = %s", textOf(res))
	}
	// An unknown run is still unknown.
	if res := callTool(t, ctx, cs, toolRunStatus, map[string]any{"runId": "01NOPE"}); !res.IsError {
		t.Fatalf("unknown run = %s", textOf(res))
	}
}

func TestFleetOffKeepsTheTools(t *testing.T) {
	ctx := testCtx(t)
	a := newAgent(t, newScript().model())
	cs := mustConnect(t, ctx, serveAgent(t, a, nil).URL, clientSetup{})
	if s := toolSchema(t, ctx, cs, toolFleet); s != "" {
		t.Errorf("the fleet tool exists without the fleet")
	}
	if s := toolSchema(t, ctx, cs, toolRunsList); strings.Contains(s, `"scope"`) {
		t.Errorf("runs_list has scope without the fleet: %s", s)
	}
	res := callTool(t, ctx, cs, toolRun, map[string]any{"input": "x", "member": "owner"})
	if !res.IsError || !strings.Contains(textOf(res), "fleet is off") {
		t.Fatalf("run with member = %s", textOf(res))
	}
	// Without the fleet, the old fleet view opens home with no members.
	var dash dashboardResult
	res = callTool(t, ctx, cs, toolDashboard, map[string]any{"view": "fleet"})
	structured(t, res, &dash)
	if res.IsError || dash.View != viewHome || len(dash.Members) != 0 || dash.Name != "" {
		t.Fatalf("dashboard fleet = %+v, %s", dash, textOf(res))
	}
}

func TestWithCredential(t *testing.T) {
	req := func(h string) *sdk.CallToolRequest {
		return &sdk.CallToolRequest{Extra: &sdk.RequestExtra{Header: http.Header{"Authorization": {h}}}}
	}
	if got := fw.CredentialFrom(withCredential(context.Background(), req("Bearer abc"))); got != "Bearer abc" {
		t.Errorf("credential = %q", got)
	}
	kept := fw.WithCredential(context.Background(), "Bearer first")
	if got := fw.CredentialFrom(withCredential(kept, req("Bearer second"))); got != "Bearer first" {
		t.Errorf("credential = %q", got)
	}
	if got := fw.CredentialFrom(withCredential(context.Background(), req("Basic eDp5"))); got != "" {
		t.Errorf("a basic credential = %q", got)
	}
	if got := fw.CredentialFrom(withCredential(context.Background(), &sdk.CallToolRequest{})); got != "" {
		t.Errorf("no header = %q", got)
	}
}

// poolFleet is a fake fleet that keeps pools.
type poolFleet struct {
	*fakeFleet
	pools []pools.Status
}

func (f *poolFleet) Pools(context.Context) ([]pools.Status, error) { return f.pools, nil }

// The fleet tool lists the pools after the members: the ready and the
// wanted Tasks, and each reason for a gap.
func TestFleetToolShowsPools(t *testing.T) {
	ctx := testCtx(t)
	fake := &poolFleet{
		fakeFleet: &fakeFleet{name: "view", members: []fleet.Member{{Name: "view", Self: true, State: fleet.MemberLive, AccessMode: fw.AccessModeRules}}},
		pools: []pools.Status{{Name: "triage", Atespace: "team", Agent: "triage", Intake: "fw.jobs.triage", Wanted: 3, Actual: 3, Ready: 2, CheckedAt: time.Now(),
			Gaps: []string{"triage-1 runs an older configuration and holds the active run 01RUN, so the pool replaces it once it is idle"}}},
	}
	view := attach(manageAgent(t, newScript(endTurn("local")).model()), fake)
	cs := as(t, ctx, serveAgent(t, view, nil).URL, "alice-token")
	res := callTool(t, ctx, cs, toolFleet, map[string]any{})
	var fl fleetResult
	structured(t, res, &fl)
	text := textOf(res)
	for _, want := range []string{"1 group of workers (pool) of the server:", "- triage in team: 2 of 3 workers ready, 3 exist; agent triage on the queue fw.jobs.triage", "  - triage-1 runs an older configuration and holds the active run 01RUN"} {
		if !strings.Contains(text, want) {
			t.Errorf("the text has no %q: %s", want, text)
		}
	}
	if res.IsError || len(fl.Pools) != 1 || fl.Pools[0].Ready != 2 {
		t.Fatalf("fleet = %+v", fl)
	}
}

// A refusal of the static token that the front sent for a person is the
// problem of an operator, and its words say so. Without a person, the
// words speak of the credential of the caller, as before.
func TestMemberErrorOfARefusedToken(t *testing.T) {
	forAlice := fleet.WithOnBehalfOf(context.Background(), "alice")
	if got := tokenRefusal(forAlice, &memberError{member: "owner", status: http.StatusUnauthorized, detail: "no"}).text(); got != "Forbidden: the worker owner refused the token of the fleet; check FW_API_TOKEN on that worker." {
		t.Errorf("a 401 for alice = %q", got)
	}
	if got := tokenRefusal(forAlice, &memberError{member: "owner", status: http.StatusForbidden, detail: "forbidden: token run.operate needs a role"}).text(); got != "Forbidden: the worker owner refused the token of the fleet: forbidden: token run.operate needs a role." {
		t.Errorf("a 403 for alice = %q", got)
	}
	if got := tokenRefusal(forAlice, &memberError{member: "owner", status: http.StatusNotFound, detail: "no run"}).text(); !strings.HasPrefix(got, "Not found:") {
		t.Errorf("a 404 for alice = %q", got)
	}
	if got := tokenRefusal(context.Background(), &memberError{member: "owner", status: http.StatusUnauthorized, detail: "no"}).text(); !strings.Contains(got, "did not accept your credential") {
		t.Errorf("a 401 for the token = %q", got)
	}
}
