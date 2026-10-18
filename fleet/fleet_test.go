package fleet

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
)

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// textModel ends every turn with the same text.
func textModel(out string) fw.Model {
	return fw.ModelFunc{StreamFn: func(context.Context, *fw.Request, func(fw.Delta)) (*fw.Response, error) {
		return &fw.Response{Content: []fw.Block{fw.Text(out)}, StopReason: fw.StopEndTurn, Usage: fw.Usage{InputTokens: 3, OutputTokens: 2}}, nil
	}}
}

// blockModel holds every turn until release closes or the run ends.
func blockModel(release <-chan struct{}) fw.Model {
	return fw.ModelFunc{StreamFn: func(ctx context.Context, _ *fw.Request, _ func(fw.Delta)) (*fw.Response, error) {
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return &fw.Response{Content: []fw.Block{fw.Text("ok")}, StopReason: fw.StopEndTurn}, nil
	}}
}

type member struct {
	a *fw.Agent
	f *Fleet
}

// newMember builds a front that reads the egress stream on bus. mut changes
// the configuration of the kernel and of the fleet, and opts come after the
// defaults, so they win.
func newMember(t *testing.T, bus fw.Bus, name string, mut func(*fw.Config, *Config), opts ...fw.Option) *member {
	t.Helper()
	cfg := fw.DefaultConfig()
	cfg.Home = t.TempDir()
	fc := Config{Name: name, Interval: 40 * time.Millisecond, Expire: DefaultExpire, Egress: "fw.events"}
	// The front offers configured agents only, so the members of a test
	// configure the default agent that their runs use.
	cfg.Agents = map[string]fw.AgentProfile{"default": {}}
	if mut != nil {
		mut(cfg, &fc)
	}
	base := []fw.Option{
		fw.WithConfig(cfg),
		fw.WithoutDefaults(),
		fw.WithAuthn(fw.NoAuthn{}),
		fw.WithModel(textModel("done")),
		fw.WithLedger(fw.NewMemLedger()),
		fw.WithBus(bus),
		fw.WithLogger(quietLogger()),
	}
	a, err := fw.New(append(base, opts...)...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	f, err := Open(context.Background(), a, fc)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	Attach(a, f)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		Attach(a, nil)
		_ = f.Close()
		a.Close(ctx)
	})
	return &member{a: a, f: f}
}

// newWorkerWith is a bare kernel on the bus with a model of its own: no
// fleet, an egress stream, and a node name.
func newWorkerWith(t *testing.T, bus fw.Bus, node string, m fw.Model) *fw.Agent {
	t.Helper()
	cfg := fw.DefaultConfig()
	cfg.Home = t.TempDir()
	cfg.Node = node
	cfg.Bus.Egress = "fw.events"
	cfg.Bus.EgressTypes = EgressTypes
	cfg.Agents = map[string]fw.AgentProfile{"default": {}}
	a, err := fw.New(fw.WithConfig(cfg), fw.WithoutDefaults(), fw.WithAuthn(fw.NoAuthn{}), fw.WithModel(m),
		fw.WithLedger(fw.NewMemLedger()), fw.WithBus(bus), fw.WithLogger(quietLogger()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		a.Close(ctx)
	})
	return a
}

func as(subject string, groups ...string) context.Context {
	return fw.WithPrincipal(context.Background(), &fw.Principal{Subject: subject, Kind: fw.KindUser, Groups: groups})
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out: %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// inject puts a member and its runs into the view of a front, as the Task
// list and the stream would, so a test can state a fleet.
func inject(f *Fleet, name, url, atespace, state string, seen time.Time, runs ...*fw.Run) {
	p := f.platform
	p.mu.Lock()
	defer p.mu.Unlock()
	m := p.members[name]
	if m == nil {
		m = &platformMember{name: name, url: url, atespace: atespace}
		p.members[name] = m
	}
	m.state, m.seen = state, seen
	for _, r := range runs {
		c := *r
		p.runs[r.ID] = &platformRun{run: &c, member: name, updated: seen}
		m.serves(r.Agent)
	}
}

func runIDs(runs []*fw.Run) []string {
	var out []string
	for _, r := range runs {
		out = append(out, r.ID)
	}
	return out
}

func sameSet(got []string, want ...string) bool {
	if len(got) != len(want) {
		return false
	}
	have := map[string]bool{}
	for _, g := range got {
		have[g] = true
	}
	for _, w := range want {
		if !have[w] {
			return false
		}
	}
	return true
}

// rulesRuns are the runs of a member in a fleet whose front is in the rules
// mode:
//
//	r1: agent team, started by alice; team names alice as owner and the
//	    group ops as viewers.
//	r2: agent bare (no access block), started by bob.
//	r3: agent bare, a child of r1 with no principal.
func rulesRuns() []*fw.Run {
	now := time.Now().UTC()
	return []*fw.Run{
		{ID: "r3", Agent: "bare", State: fw.StateRunning, Parent: "r1", Depth: 1, CreatedAt: now.Add(3 * time.Second)},
		{ID: "r2", Agent: "bare", State: fw.StateCompleted, Principal: "bob", CreatedAt: now.Add(2 * time.Second)},
		{ID: "r1", Agent: "team", State: fw.StateInputRequired, Principal: "alice", CreatedAt: now.Add(time.Second)},
	}
}

// rulesFront configures a front in the rules mode with the agents that
// rulesRuns name.
func rulesFront(def string) func(*fw.Config, *Config) {
	return func(c *fw.Config, _ *Config) {
		c.Access.Default = def
		c.Access.Admins = []string{"root"}
		c.Agents = map[string]fw.AgentProfile{
			"default": {},
			"bare":    {},
			"team":    {Access: &fw.Access{Owners: []string{"alice"}, Viewers: []string{"group:ops"}}},
		}
	}
}

func roles(t *testing.T, f *Fleet, ctx context.Context, member string) map[string]string {
	t.Helper()
	ms, err := f.Members(ctx)
	if err != nil {
		t.Fatalf("Members: %v", err)
	}
	out := map[string]string{}
	for _, m := range ms {
		if m.Name == member {
			for _, ag := range m.Agents {
				out[ag.Name] = ag.Role
			}
		}
	}
	return out
}

// The front decides for a worker with its own rules: the person who started
// a run, the owners and the viewers of the agent, and the admins see a run,
// and a child is visible with its parent.
func TestRulesFiltering(t *testing.T) {
	t.Setenv("FW_ATESPACE", "")
	view := newMember(t, fw.NewMemBus(), "view", rulesFront("closed"))
	inject(view.f, "team/gamma", "ax:team/gamma", "team", MemberLive, time.Now(), rulesRuns()...)
	cases := []struct {
		who  context.Context
		name string
		want []string
	}{
		{as("alice"), "owner sees her run and its child", []string{"r1", "r3"}},
		{as("bob"), "the starter sees his own run", []string{"r2"}},
		{as("carol", "ops"), "a viewer group sees the run and its child", []string{"r1", "r3"}},
		{as("root"), "an admin sees all", []string{"r1", "r2", "r3"}},
		{as("dave"), "a stranger sees nothing", nil},
		{context.Background(), "no principal sees all", []string{"r1", "r2", "r3"}},
	}
	for _, c := range cases {
		runs, err := view.f.Runs(c.who)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if !sameSet(runIDs(runs), c.want...) {
			t.Errorf("%s: runs %v, want %v", c.name, runIDs(runs), c.want)
		}
	}
	if _, err := view.f.Run(as("bob"), "r1"); !errors.Is(err, fw.ErrRunNotFound) {
		t.Fatalf("bob reads r1: %v", err)
	}
	ms, err := view.f.Members(as("alice"))
	if err != nil {
		t.Fatalf("Members: %v", err)
	}
	if len(ms) != 2 || ms[1].Name != "team/gamma" || len(ms[1].Agents) != 1 || ms[1].Agents[0].Name != "team" {
		t.Fatalf("alice sees %+v", ms)
	}
	if ms[1].Active != 2 || ms[1].InputRequired != 1 || !ms[1].CanForward {
		t.Fatalf("alice counts active %d, input %d, forward %v", ms[1].Active, ms[1].InputRequired, ms[1].CanForward)
	}
	if ms, _ := view.f.Members(as("dave")); len(ms) != 1 {
		t.Fatalf("dave sees members %+v", ms)
	}
	if got := roles(t, view.f, as("alice"), "team/gamma"); len(got) != 1 || got["team"] != "owner" {
		t.Fatalf("alice roles %v", got)
	}
	if got := roles(t, view.f, as("carol", "ops"), "team/gamma"); len(got) != 1 || got["team"] != "viewer" {
		t.Fatalf("carol roles %v", got)
	}
	if got := roles(t, view.f, as("root"), "team/gamma"); got["team"] != "owner" || got["bare"] != "owner" {
		t.Fatalf("root roles %v", got)
	}
	if got := roles(t, view.f, context.Background(), "team/gamma"); got["team"] != "owner" || got["bare"] != "owner" {
		t.Fatalf("process roles %v", got)
	}
	// The admin owns the agents of the front itself too.
	if got := roles(t, view.f, as("root"), "view"); got["default"] != "owner" {
		t.Fatalf("root roles here %v", got)
	}

	// With access.default open, an agent without an access block is open.
	open := newMember(t, fw.NewMemBus(), "open", rulesFront("open"))
	inject(open.f, "team/delta", "ax:team/delta", "team", MemberLive, time.Now(), rulesRuns()...)
	runs, err := open.f.Runs(as("dave"))
	if err != nil || !sameSet(runIDs(runs), "r2", "r3") {
		t.Fatalf("dave on the open front sees %v, %v", runIDs(runs), err)
	}
	if got := roles(t, open.f, as("dave"), "team/delta"); len(got) != 1 || got["bare"] != "operator" {
		t.Fatalf("dave roles on the open front %v", got)
	}
}

// A front with a custom Authz decides for every worker with it, and the
// targets name the member, the access block of the agent, and the lineage
// of a child.
func TestCustomAuthzDecidesForTheWorkers(t *testing.T) {
	t.Setenv("FW_ATESPACE", "")
	var mu sync.Mutex
	var seen []fw.Target
	authz := fw.AuthzFunc(func(_ context.Context, p *fw.Principal, action string, tg fw.Target) error {
		if tg.Member == "" {
			return nil // this process
		}
		mu.Lock()
		seen = append(seen, tg)
		mu.Unlock()
		switch {
		case p.Subject == "alice":
			return nil
		case p.Subject == "carol" && action != fw.ActionAgentEdit:
			return nil
		}
		return fmt.Errorf("%w: no", fw.ErrForbidden)
	})
	custom := newMember(t, fw.NewMemBus(), "custom", rulesFront(""), fw.WithAuthz(authz))
	inject(custom.f, "team/ext", "ax:team/ext", "team", MemberLive, time.Now(), rulesRuns()...)
	runs, err := custom.f.Runs(as("alice"))
	if err != nil || !sameSet(runIDs(runs), "r1", "r2", "r3") {
		t.Fatalf("alice sees %v, %v", runIDs(runs), err)
	}
	if runs, _ := custom.f.Runs(as("bob")); len(runs) != 0 {
		t.Fatalf("bob sees %v", runIDs(runs))
	}
	for who, want := range map[string]string{"alice": "owner", "carol": "operator"} {
		ms, err := custom.f.Members(as(who))
		var ext *Member
		for i := range ms {
			if ms[i].Name == "team/ext" {
				ext = &ms[i]
			}
		}
		if err != nil || ext == nil || len(ext.Agents) != 2 {
			t.Fatalf("%s sees %+v, %v", who, ms, err)
		}
		for _, ag := range ext.Agents {
			if ag.Role != want {
				t.Fatalf("%s has role %q on %s", who, ag.Role, ag.Name)
			}
		}
		if ms[0].Agents[0].Role != "owner" {
			t.Fatalf("%s has role %q here", who, ms[0].Agents[0].Role)
		}
	}
	ms, _ := custom.f.Members(as("bob"))
	for _, m := range ms {
		if m.Name == "team/ext" {
			t.Fatalf("bob sees the member ext: %+v", m)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	var child *fw.Target
	for i := range seen {
		if seen[i].Member != "team/ext" {
			t.Fatalf("target names member %q", seen[i].Member)
		}
		if seen[i].Run != nil && seen[i].Run.ID == "r3" {
			child = &seen[i]
		}
		if seen[i].Agent == "team" && (seen[i].Access == nil || seen[i].Access.Owners[0] != "alice") {
			t.Fatalf("the team target lacks the access block: %+v", seen[i])
		}
	}
	if child == nil || len(child.Lineage) != 1 || child.Lineage[0].ID != "r1" || child.Lineage[0].Principal != "alice" {
		t.Fatalf("the child target has lineage %+v", child)
	}
}

func TestAuthzUnavailable(t *testing.T) {
	t.Setenv("FW_ATESPACE", "")
	authz := fw.AuthzFunc(func(_ context.Context, _ *fw.Principal, _ string, tg fw.Target) error {
		if tg.Member == "" {
			return nil
		}
		return fmt.Errorf("%w: the control plane is down", fw.ErrAuthzUnavailable)
	})
	view := newMember(t, fw.NewMemBus(), "view", nil, fw.WithAuthz(authz))
	inject(view.f, "team/ext", "ax:team/ext", "team", MemberLive, time.Now(), rulesRuns()...)
	if _, err := view.f.Runs(as("alice")); !errors.Is(err, fw.ErrAuthzUnavailable) {
		t.Fatalf("Runs: %v", err)
	}
	if _, err := view.f.Members(as("alice")); !errors.Is(err, fw.ErrAuthzUnavailable) {
		t.Fatalf("Members: %v", err)
	}
	if _, err := view.f.Run(as("alice"), "r1"); !errors.Is(err, fw.ErrAuthzUnavailable) {
		t.Fatalf("Run: %v", err)
	}
}

// capture is an httptest server that keeps the last request.
type capture struct {
	mu   sync.Mutex
	req  *http.Request
	body string
	srv  *httptest.Server
}

func newCapture(t *testing.T) *capture {
	c := &capture{}
	c.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		c.mu.Lock()
		c.req, c.body = r, string(b)
		c.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(c.srv.Close)
	return c
}

func (c *capture) last() (*http.Request, string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.req, c.body
}

// A request to a Task goes through the router with the credential of the
// caller, the headers that matter, and the actor of the Task. A member
// without an address, this process, and a request that was forwarded once
// have no route.
func TestForward(t *testing.T) {
	t.Setenv("FW_ATESPACE", "")
	t.Setenv("FW_API_TOKEN", "fleet-secret")
	router := newCapture(t)
	ax := newFakeAX(t, map[string]map[string]string{"team": {"rev": "Running"}})
	view := newMember(t, fw.NewMemBus(), "view", func(c *fw.Config, fc *Config) {
		fc.Atespaces = []string{"team"}
		c.AX.Server = ax.addr
		c.AX.Router = strings.TrimPrefix(router.srv.URL, "http://")
	})
	eventually(t, "the view sees the Task", func() bool {
		ms, err := view.f.Members(context.Background())
		return err == nil && memberStates(ms)["team/rev"] == MemberLive
	})
	inject(view.f, "box:8080", "", "", "", time.Now())
	cred := fw.WithCredential(context.Background(), "Bearer tok")
	newReq := func() *http.Request {
		r, _ := http.NewRequest(http.MethodPost, "/v1/runs/r1/approvals/ap1?x=1", strings.NewReader(`{"decision":"allow"}`))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Accept", "application/json")
		r.Header.Set("Idempotency-Key", "k1")
		r.Header.Set("X-Other", "secret")
		r.Header.Set("Authorization", "Bearer someone-else")
		return r
	}
	resp, err := view.f.Forward(cred, "team/rev", newReq())
	if err != nil {
		t.Fatalf("Forward: %v", err)
	}
	resp.Body.Close()
	got, body := router.last()
	if got.Header.Get(headerTargetActor) != "team/rev" || got.URL.Path != "/v1/runs/r1/approvals/ap1" || got.URL.RawQuery != "x=1" || got.Method != http.MethodPost || body != `{"decision":"allow"}` {
		t.Fatalf("router got actor %q %s %s?%s %q", got.Header.Get(headerTargetActor), got.Method, got.URL.Path, got.URL.RawQuery, body)
	}
	if got.Header.Get("Authorization") != "Bearer tok" || got.Header.Get(HeaderVia) != "view" {
		t.Fatalf("router got credential %q via %q", got.Header.Get("Authorization"), got.Header.Get(HeaderVia))
	}
	if got.Header.Get("Idempotency-Key") != "k1" || got.Header.Get("Accept") != "application/json" || got.Header.Get("X-Other") != "" {
		t.Fatalf("router got headers %v", got.Header)
	}
	noRoute := []struct {
		name   string
		member string
		req    *http.Request
	}{
		{"unknown member", "nobody", newReq()},
		{"this member", "view", newReq()},
		{"member without an address", "box:8080", newReq()},
		{"a forwarded request", "team/rev", func() *http.Request { r := newReq(); r.Header.Set(HeaderVia, "other"); return r }()},
	}
	for _, c := range noRoute {
		if _, err := view.f.Forward(cred, c.member, c.req); !errors.Is(err, ErrNoRoute) {
			t.Errorf("%s: %v", c.name, err)
		}
	}
	if _, err := view.f.Forward(context.Background(), "team/rev", newReq()); !errors.Is(err, ErrNoCredential) {
		t.Fatalf("no credential: %v", err)
	}
	r, _ := http.NewRequest(http.MethodGet, "/metrics", nil)
	if _, err := view.f.Forward(cred, "team/rev", r); err == nil {
		t.Fatal("a path outside /v1/ was forwarded")
	}
	ms, err := view.f.Members(context.Background())
	if err != nil {
		t.Fatalf("Members: %v", err)
	}
	can := map[string]bool{}
	for _, m := range ms {
		can[m.Name] = m.CanForward
	}
	if !can["team/rev"] || can["box:8080"] || can["view"] {
		t.Fatalf("CanForward = %v", can)
	}
}

// A worker that only the stream knows is live while its events are fresh,
// quiet after QuietAfter intervals, and gone after expire.
func TestStreamMemberStatesAndExpire(t *testing.T) {
	t.Setenv("FW_ATESPACE", "")
	view := newMember(t, fw.NewMemBus(), "view", nil)
	now := time.Now()
	inject(view.f, "fresh:8080", "", "", "", now)
	inject(view.f, "stale:8080", "", "", "", now.Add(-time.Second))
	inject(view.f, "old:8080", "", "", "", now.Add(-8*24*time.Hour))
	ms, err := view.f.Members(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	st := memberStates(ms)
	if st["fresh:8080"] != MemberLive || st["stale:8080"] != MemberQuiet {
		t.Fatalf("states = %v", st)
	}
	if _, ok := st["old:8080"]; ok {
		t.Fatalf("an expired member is still in the view: %v", st)
	}
}

// The front counts the active run of a worker and shows the run as not
// finished while the worker works.
func TestActiveRunAndCounts(t *testing.T) {
	t.Setenv("FW_ATESPACE", "")
	bus := fw.NewMemBus()
	release := make(chan struct{})
	defer close(release)
	worker := newWorkerWith(t, bus, "ax:team/w1", blockModel(release))
	view := newMember(t, bus, "view", nil)
	run, err := worker.Start(context.Background(), "wait")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	eventually(t, "the view counts the active run", func() bool {
		ms, err := view.f.Members(context.Background())
		if err != nil {
			return false
		}
		for _, m := range ms {
			if m.Name == "team/w1" && m.Active == 1 && m.State == MemberLive {
				return true
			}
		}
		return false
	})
	r, err := view.f.Run(context.Background(), run.ID)
	if err != nil || r.State.Terminal() || r.Member != "team/w1" {
		t.Fatalf("Run = %+v, %v", r, err)
	}
}

func TestShorten(t *testing.T) {
	long := strings.Repeat("é", TextLimit) // two bytes each
	big := `"` + strings.Repeat("x", PendingInputLimit) + `"`
	r := &fw.Run{ID: "r", Input: long, Output: "short", Member: "m", Pending: &fw.Approval{ID: "ap", Input: []byte(big)}}
	c := shorten(r)
	if len(c.Input) > TextLimit || !strings.HasSuffix(c.Input, cutMark) || !strings.HasPrefix(c.Input, "éé") {
		t.Fatalf("input cut to %d bytes", len(c.Input))
	}
	if strings.ContainsRune(c.Input, '�') {
		t.Fatal("the cut split a rune")
	}
	if c.Output != "short" || c.Member != "" || c.Pending.Input != nil {
		t.Fatalf("copy = %+v", c)
	}
	if r.Input != long || r.Pending.Input == nil {
		t.Fatal("shorten changed the original run")
	}
	r.Pending.Input = []byte(`{"command":"ls"}`)
	if c := shorten(r); string(c.Pending.Input) != `{"command":"ls"}` {
		t.Fatal("a small pending input was dropped")
	}
}

func TestPickRuns(t *testing.T) {
	now := time.Now()
	var runs []*fw.Run
	for i := 0; i < 60; i++ {
		runs = append(runs, &fw.Run{ID: fmt.Sprintf("f%02d", i), State: fw.StateCompleted, CreatedAt: now.Add(-time.Duration(i) * time.Minute)})
	}
	runs = append(runs, &fw.Run{ID: "active-old", State: fw.StateRunning, CreatedAt: now.Add(-time.Hour)})
	kept, omitted := pickRuns(runs)
	if len(kept) != FinishedRuns+1 || omitted != 10 || kept[len(kept)-1].ID != "active-old" {
		t.Fatalf("kept %d, omitted %d", len(kept), omitted)
	}
}

func TestMemberName(t *testing.T) {
	host, _ := os.Hostname()
	if host == "" {
		host = "fw"
	}
	cfg := fw.DefaultConfig()
	cfg.Listen = ":8080"
	if got := defaults("", cfg); got != host+":8080" {
		t.Fatalf("default name = %q", got)
	}
	if got := defaults("team/rev", cfg); got != "team/rev" {
		t.Fatalf("given name = %q", got)
	}
	cfg.Listen = ""
	if got := defaults("", cfg); got != host {
		t.Fatalf("default name without a port = %q", got)
	}
	// The variables win over the file, and a bad value is an error.
	for _, e := range []string{EnvAtespaces, EnvEgress, EnvName, EnvInterval, EnvSuspendAfter} {
		t.Setenv(e, "")
	}
	t.Setenv(EnvEgress, "fw.events")
	t.Setenv(EnvName, "me")
	t.Setenv(EnvInterval, "5s")
	c, err := Load(fw.DefaultConfig())
	if err != nil || !c.On() || c.Name != "me" || c.Interval != 5*time.Second || c.Expire != DefaultExpire || c.suspendAfter() != 0 {
		t.Fatalf("Load = %+v, %v", c, err)
	}
	t.Setenv(EnvInterval, "soon")
	if _, err := Load(fw.DefaultConfig()); err == nil || !strings.Contains(err.Error(), EnvInterval) {
		t.Fatalf("a bad interval = %v", err)
	}
	t.Setenv(EnvInterval, "")
	t.Setenv(EnvEgress, "")
	if c, err := Load(fw.DefaultConfig()); err != nil || c.On() {
		t.Fatalf("without a source the fleet is off: %+v, %v", c, err)
	}
}

// A front without agents offers none, and a member without runs and
// agents stays out of the sight of a person, while the process sees it.
func TestFrontWithoutAgentsStaysOutOfSight(t *testing.T) {
	t.Setenv("FW_ATESPACE", "")
	front := newMember(t, fw.NewMemBus(), "front-1", func(c *fw.Config, _ *Config) { c.Agents = map[string]fw.AgentProfile{} })
	inject(front.f, "team/idle", "ax:team/idle", "team", MemberQuiet, time.Now())
	ms, err := front.f.Members(as("alice"))
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) != 1 || !ms[0].Self || len(ms[0].Agents) != 0 {
		t.Fatalf("members for alice = %+v", ms)
	}
	ms, err = front.f.Members(context.Background())
	if err != nil || len(ms) != 2 || ms[1].Name != "team/idle" || ms[1].State != MemberQuiet {
		t.Fatalf("members for the process = %+v, %v", ms, err)
	}
}

// A run that a worker started a moment ago reaches the view through the
// stream, and the view waits a little for it; an old unknown id does not
// wait.
func TestViewWaitsForANewRun(t *testing.T) {
	t.Setenv("FW_ATESPACE", "")
	bus := fw.NewMemBus()
	worker := newWorkerWith(t, bus, "ax:team/owner", textModel("done"))
	view := newMember(t, bus, "view", nil)
	r, err := worker.Start(context.Background(), "hello")
	if err != nil {
		t.Fatal(err)
	}
	got, err := view.f.Run(context.Background(), r.ID)
	if err != nil || got.Member != "team/owner" {
		t.Fatalf("a run started a moment ago = %+v, %v", got, err)
	}
	start := time.Now()
	if _, err := view.f.Run(context.Background(), "00000000000000000000000000"); !errors.Is(err, fw.ErrRunNotFound) {
		t.Fatalf("an old unknown id = %v", err)
	}
	if d := time.Since(start); d > 200*time.Millisecond {
		t.Fatalf("an old unknown id took %v", d)
	}
}
