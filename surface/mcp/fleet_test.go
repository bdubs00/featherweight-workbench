package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/surface/native"
	"github.com/bdubs00/featherweight-workbench/dashboard"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// fakeFleet is a fleet with one other member, "owner", that a test server
// with the native API stands for. Forward sends each request there, as the
// fleet driver does, and records its headers.
type fakeFleet struct {
	name    string
	owner   string
	members []fw.FleetMember

	mu      sync.Mutex
	runs    []*fw.Run
	err     error
	forward []http.Header
}

func (f *fakeFleet) Name() string { return f.name }

func (f *fakeFleet) Members(context.Context) ([]fw.FleetMember, error) { return f.members, nil }

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
		return nil, fw.ErrFleetNoCredential
	}
	u, err := url.Parse(f.owner + req.URL.String())
	if err != nil {
		return nil, err
	}
	out := req.Clone(ctx)
	out.URL, out.Host, out.RequestURI = u, u.Host, ""
	out.Header.Set("Authorization", cred)
	out.Header.Set(fw.HeaderFleetVia, f.name)
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
	owner := manageAgent(t, newScript(toolUse("c1", "deploy", `{"env":"prod"}`), endTurn("done"), endTurn("again"), endTurn("third")).model(),
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

	fleet := &fakeFleet{name: "view", owner: ownerSrv.URL, members: []fw.FleetMember{
		{Name: "view", Self: true, State: fw.MemberLive, AccessMode: fw.AccessModeRules},
		{Name: "owner", State: fw.MemberLive, CanForward: true, AccessMode: fw.AccessModeRules, Agents: []fw.FleetAgent{{Name: "reviewer", Role: "owner"}}},
	}}
	fleet.set(r)
	view := manageAgent(t, newScript(endTurn("local")).model(), fw.WithFleet(fleet))
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

	// run_status and run_events reach the owner.
	st := runOf(t, callTool(t, ctx, cs, toolRunStatus, map[string]any{"runId": remote.ID}))
	if st.Member != "owner" || st.Pending == nil {
		t.Fatalf("remote status = %+v", st)
	}
	if h := fleet.lastForward(); h.Get("Authorization") != "Bearer alice-token" || h.Get(fw.HeaderFleetVia) != "view" {
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

	// Bob can only view the reviewer agent: the owner refuses his approval.
	bob := as(t, ctx, serveAgent(t, view, nil).URL, "bob-token")
	res = callTool(t, ctx, bob, toolApprove, map[string]any{"runId": remote.ID, "approvalId": st.Pending.ID, "decision": "allow"})
	if !res.IsError || !strings.Contains(textOf(res), "Forbidden: the fleet member owner refused") {
		t.Fatalf("bob approve = %s", textOf(res))
	}

	// Alice approves through the view; the owner records her as the decider.
	res = callTool(t, ctx, cs, toolApprove, map[string]any{"runId": remote.ID, "approvalId": st.Pending.ID, "decision": "allow", "note": "go"})
	if res.IsError || runOf(t, res).Member != "owner" {
		t.Fatalf("approve = %s", textOf(res))
	}
	done, err := owner.Wait(ctx, remote.ID)
	if err != nil || done.State != fw.StateCompleted || calls.Load() != 1 {
		t.Fatalf("owner run = %+v, %v, calls %d", done, err, calls.Load())
	}
	if d := decided(t, owner, remote.ID); d.Decider != "alice" || d.Note != "go" {
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
	if h := fleet.lastForward(); h.Get("Idempotency-Key") != "r-1" {
		t.Errorf("forwarded headers = %v", h)
	}
	if got, err := owner.Get(context.Background(), started.ID); err != nil || got.Metadata[fw.MetaTitle] != "Remote work" {
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
	fleet.mu.Lock()
	fleet.err = errors.Join(fw.ErrFleetNoRoute, errors.New("the member owner has no URL"))
	fleet.mu.Unlock()
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
