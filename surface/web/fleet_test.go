package web

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/surface/view"
	"github.com/bdubs00/featherweight-workbench/dashboard"
	"github.com/bdubs00/featherweight-workbench/fleet"
)

// fakeFleet is a fleet with one other member, "owner", that a test server
// stands for. Forward sends each request to that server, as the fleet
// driver does, and records it.
type fakeFleet struct {
	name    string
	owner   string // the URL of the owner
	members []fleet.Member

	mu      sync.Mutex
	runs    []*fw.Run
	err     error  // Forward returns it when set
	cred    string // a credential that replaces the one of the caller
	forward []http.Header
}

func (f *fakeFleet) Name() string { return f.name }

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
	ferr, cred := f.err, f.cred
	f.mu.Unlock()
	if ferr != nil {
		return nil, ferr
	}
	if member != "owner" {
		return nil, errors.Join(fleet.ErrNoRoute, errors.New("unknown member "+member))
	}
	if cred == "" {
		cred = fw.CredentialFrom(ctx)
	}
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

func (f *fakeFleet) fail(err error, cred string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err, f.cred = err, cred
}

func (f *fakeFleet) forwarded() []http.Header {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]http.Header(nil), f.forward...)
}

// fleetPair is a view with a fake fleet and an owner agent that serves the
// runs of the member "owner". Alice owns the agent reviewer on both.
type fleetPair struct {
	view, owner       *fw.Agent
	viewURL, ownerURL string
	fleet             *fakeFleet
	calls             *atomic.Int32
	remote            *fw.Run // a run on the owner that waits for an approval
	local             *fw.Run // a run on the view that completed
}

// The options apply to both agents.
func newFleetPair(t *testing.T, opts ...fw.Option) *fleetPair {
	t.Helper()
	p := &fleetPair{calls: &atomic.Int32{}}
	p.owner = manageAgent(t, newScript(callTool("c1", "rm", `{"path":"x"}`), endTurn("done"), endTurn("again")).model(), append([]fw.Option{fw.WithTools(rmTool(p.calls))}, opts...)...)
	p.ownerURL = serve(t, p.owner).URL
	p.fleet = &fakeFleet{name: "view", owner: p.ownerURL, members: []fleet.Member{
		{Name: "view", Self: true, State: fleet.MemberLive, AccessMode: fw.AccessModeRules},
		{Name: "owner", State: fleet.MemberLive, CanForward: true, AccessMode: fw.AccessModeRules, Agents: []fleet.RecordAgent{{Name: "reviewer", Role: "owner"}}},
	}}
	p.view = attach(manageAgent(t, newScript(endTurn("local")).model(), opts...), p.fleet)
	p.viewURL = serve(t, p.view).URL

	resp, b := do(t, "POST", p.ownerURL+"/v1/runs", `{"agent":"reviewer","input":"clean up","wait":true}`, "Authorization", asAlice)
	p.remote = decodeRun(t, resp, b, http.StatusOK)
	if p.remote.State != fw.StateInputRequired || p.remote.Pending == nil {
		t.Fatalf("remote run = %+v", p.remote)
	}
	p.fleet.set(p.remote)
	resp, b = do(t, "POST", p.viewURL+"/v1/runs", `{"agent":"reviewer","input":"here","wait":true}`, "Authorization", asAlice)
	p.local = decodeRun(t, resp, b, http.StatusOK)
	return p
}

func runIDs(runs []*fw.Run) string {
	var ids []string
	for _, r := range runs {
		id := r.ID
		if r.Member != "" {
			id += "@" + r.Member
		}
		ids = append(ids, id)
	}
	return strings.Join(ids, ",")
}

func TestFleetOff(t *testing.T) {
	srv := serve(t, newAgent(t, newScript().model()))
	resp, b := do(t, "GET", srv.URL+"/v1/fleet", "")
	if p := decodeProblem(t, resp, b, http.StatusNotFound); !strings.Contains(p.Detail, "fleet.subject") {
		t.Errorf("detail = %q", p.Detail)
	}
	resp, b = do(t, "GET", srv.URL+"/v1/fleet/runs", "")
	decodeProblem(t, resp, b, http.StatusNotFound)
	resp, b = do(t, "GET", srv.URL+"/v1/fleet/runs?member=owner", "")
	decodeProblem(t, resp, b, http.StatusNotFound)
	resp, b = do(t, "POST", srv.URL+"/v1/runs?member=owner", `{"input":"x"}`)
	decodeProblem(t, resp, b, http.StatusBadRequest)
	// The run list of the kernel knows no fleet and ignores its parameters.
	resp, b = do(t, "GET", srv.URL+"/v1/runs?scope=local&member=owner", "")
	var list struct{ Runs []*fw.Run }
	decodeJSON(t, resp, b, http.StatusOK, &list)
}

func TestFleetListAndRead(t *testing.T) {
	ctx := testCtx(t)
	p := newFleetPair(t)

	resp, b := do(t, "GET", p.viewURL+"/v1/fleet", "", "Authorization", asAlice)
	var fl struct {
		Name    string
		Members []fleet.Member
	}
	decodeJSON(t, resp, b, http.StatusOK, &fl)
	if fl.Name != "view" || len(fl.Members) != 2 || fl.Members[1].Agents[0].Role != "owner" {
		t.Fatalf("fleet = %s", b)
	}

	list := func(query string) []*fw.Run {
		t.Helper()
		resp, b := do(t, "GET", p.viewURL+"/v1/fleet/runs"+query, "", "Authorization", asAlice)
		var out struct{ Runs []*fw.Run }
		decodeJSON(t, resp, b, http.StatusOK, &out)
		return out.Runs
	}
	both := list("")
	if len(both) != 2 || runIDs(both) != p.local.ID+","+p.remote.ID+"@owner" && runIDs(both) != p.remote.ID+"@owner,"+p.local.ID {
		t.Fatalf("fleet runs = %s", runIDs(both))
	}
	if got := runIDs(list("?scope=local")); got != p.local.ID {
		t.Errorf("local runs = %s", got)
	}
	if got := runIDs(list("?member=owner")); got != p.remote.ID+"@owner" {
		t.Errorf("owner runs = %s", got)
	}
	if got := runIDs(list("?member=view")); got != p.local.ID {
		t.Errorf("own runs = %s", got)
	}
	if got := runIDs(list("?state=input-required")); got != p.remote.ID+"@owner" {
		t.Errorf("waiting runs = %s", got)
	}
	resp, b = do(t, "GET", p.viewURL+"/v1/fleet/runs?scope=both", "", "Authorization", asAlice)
	decodeProblem(t, resp, b, http.StatusBadRequest)
	// The run list of the kernel covers this process only.
	resp, b = do(t, "GET", p.viewURL+"/v1/runs", "", "Authorization", asAlice)
	var local struct{ Runs []*fw.Run }
	decodeJSON(t, resp, b, http.StatusOK, &local)
	if got := runIDs(local.Runs); got != p.local.ID {
		t.Errorf("kernel runs = %s", got)
	}

	// A read of the remote run goes to the owner with the credential of the
	// caller and the via header, and comes back with the member.
	resp, b = do(t, "GET", p.viewURL+"/v1/runs/"+p.remote.ID, "", "Authorization", asAlice)
	r := decodeRun(t, resp, b, http.StatusOK)
	if r.ID != p.remote.ID || r.Member != "owner" || r.Pending == nil {
		t.Fatalf("remote run = %+v", r)
	}
	h := p.fleet.forwarded()
	if len(h) != 1 || h[0].Get("Authorization") != asAlice || h[0].Get(fleet.HeaderVia) != "view" || h[0].Get("Accept") != "" {
		t.Fatalf("forwarded headers = %v", h)
	}
	// A local run never goes out.
	resp, b = do(t, "GET", p.viewURL+"/v1/runs/"+p.local.ID, "", "Authorization", asAlice)
	if r := decodeRun(t, resp, b, http.StatusOK); r.Member != "" || len(p.fleet.forwarded()) != 1 {
		t.Fatalf("local run = %+v, forwarded %d", r, len(p.fleet.forwarded()))
	}

	// A page of events keeps its shape, and the run names the member.
	resp, b = do(t, "GET", p.viewURL+"/v1/runs/"+p.remote.ID+"/events?limit=2", "", "Authorization", asAlice, "Accept", "application/json")
	var page struct {
		Run    *fw.Run
		Events []fw.Event
	}
	decodeJSON(t, resp, b, http.StatusOK, &page)
	if page.Run == nil || page.Run.Member != "owner" || len(page.Events) != 2 {
		t.Fatalf("page = %s", b)
	}

	// The event stream flows through.
	sctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stream := openStream(t, sctx, "GET", p.viewURL+"/v1/runs/"+p.remote.ID+"/events", "", "Authorization", asAlice)
	sse := newSSE(stream.Body)
	f, ok := sse.next()
	if !ok {
		t.Fatal("the stream ended at once")
	}
	if e := checkFrame(t, f); e.Seq != 1 || e.Type != fw.EvRunQueued {
		t.Fatalf("first event = %+v", e)
	}
	cancel()

	// A request that a member forwarded is never forwarded again.
	resp, b = do(t, "GET", p.viewURL+"/v1/runs/"+p.remote.ID, "", "Authorization", asAlice, fleet.HeaderVia, "elsewhere")
	decodeProblem(t, resp, b, http.StatusNotFound)
	// Children of a remote run come from the fleet.
	resp, b = do(t, "GET", p.viewURL+"/v1/fleet/runs?parent="+p.remote.ID, "", "Authorization", asAlice)
	var kids struct{ Runs []*fw.Run }
	decodeJSON(t, resp, b, http.StatusOK, &kids)
	if len(kids.Runs) != 0 {
		t.Errorf("children = %s", b)
	}

	// The glance covers the runs of the whole fleet.
	resp, b = do(t, "GET", p.viewURL+"/v1/fleet/runs", "", "Authorization", asAlice)
	var glanced struct{ Glance view.Glance }
	decodeJSON(t, resp, b, http.StatusOK, &glanced)
	if g := glanced.Glance; g.Waiting != 1 || g.Finished != 1 || g.Sentence != "One run needs you." {
		t.Errorf("glance = %+v", g)
	}

	// The chat view of a remote run comes from the owner, with the member.
	resp, b = do(t, "GET", p.viewURL+"/v1/runs/"+p.remote.ID+"/transcript", "", "Authorization", asAlice)
	var chat struct {
		Run   *fw.Run
		Items []dashboard.Item
	}
	decodeJSON(t, resp, b, http.StatusOK, &chat)
	if chat.Run == nil || chat.Run.Member != "owner" || len(chat.Items) < 3 || chat.Items[0].Kind != dashboard.ItemUser ||
		chat.Items[0].Text != "clean up" || chat.Items[len(chat.Items)-1].Kind != dashboard.ItemAsk {
		t.Fatalf("remote transcript = %s", b)
	}
	if h := p.fleet.forwarded(); len(h) == 0 || h[len(h)-1].Get("Authorization") != asAlice {
		t.Fatalf("forwarded headers = %v", h)
	}
}
