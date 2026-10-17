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

const subj = "test.fleet"

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

// newMember builds an agent that joins the fleet on bus. mut changes the
// configuration of the kernel and of the fleet, and opts come after the
// defaults, so they win.
func newMember(t *testing.T, bus fw.Bus, name string, mut func(*fw.Config, *Config), opts ...fw.Option) *member {
	t.Helper()
	cfg := fw.DefaultConfig()
	cfg.Home = t.TempDir()
	fc := Config{Subject: subj, Name: name, Interval: 40 * time.Millisecond, Expire: DefaultExpire}
	// The fleet offers configured agents only, so the members of a test
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

// retain puts a record of a fake member on the bus.
func retain(t *testing.T, bus fw.Retainer, rec Record) {
	t.Helper()
	if rec.Version == 0 {
		rec.Version = Version
	}
	if rec.UpdatedAt.IsZero() {
		rec.UpdatedAt = time.Now().UTC()
	}
	if err := bus.Retain(context.Background(), RecordSubject(subj, rec.Name), fw.MustEvent(rec.Name, EvRecord, rec)); err != nil {
		t.Fatalf("Retain: %v", err)
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

func TestPublishAndLazyView(t *testing.T) {
	t.Setenv("FW_ATESPACE", "")
	bus := fw.NewMemBus()
	alpha := newMember(t, bus, "alpha", nil)
	beta := newMember(t, bus, "beta", nil)
	if beta.f.viewOn {
		t.Fatal("the view started before a request needed it")
	}
	run, err := alpha.a.Start(context.Background(), "hello")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	ctx := context.Background()
	eventually(t, "beta sees the finished run of alpha", func() bool {
		r, err := beta.f.Run(ctx, run.ID)
		return err == nil && r.State == fw.StateCompleted
	})
	if !beta.f.viewOn {
		t.Fatal("the view did not start")
	}
	r, _ := beta.f.Run(ctx, run.ID)
	if r.Member != "alpha" || r.Output != "done" {
		t.Fatalf("run = member %q output %q", r.Member, r.Output)
	}
	runs, err := beta.f.Runs(ctx)
	if err != nil || len(runs) != 1 || runs[0].Member != "alpha" {
		t.Fatalf("Runs = %v, %v", runs, err)
	}
	if _, err := beta.f.Run(ctx, "01NOPE"); !errors.Is(err, fw.ErrRunNotFound) {
		t.Fatalf("unknown run: %v", err)
	}
	ms, err := beta.f.Members(ctx)
	if err != nil {
		t.Fatalf("Members: %v", err)
	}
	if len(ms) != 2 || !ms[0].Self || ms[0].Name != "beta" || ms[1].Name != "alpha" || ms[1].State != MemberLive {
		t.Fatalf("Members = %+v", ms)
	}
	if ms[1].AccessMode != fw.AccessModeRules || len(ms[1].Agents) != 1 || ms[1].Agents[0].Name != "default" {
		t.Fatalf("alpha = %+v", ms[1])
	}
	// The own record never shows as another member.
	for _, m := range ms[1:] {
		if m.Name == "beta" {
			t.Fatal("beta lists itself as another member")
		}
	}
}

func TestActiveRunAndCounts(t *testing.T) {
	t.Setenv("FW_ATESPACE", "")
	bus := fw.NewMemBus()
	release := make(chan struct{})
	defer close(release)
	alpha := newMember(t, bus, "alpha", nil, fw.WithModel(blockModel(release)))
	beta := newMember(t, bus, "beta", nil)
	run, err := alpha.a.Start(context.Background(), "wait")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	eventually(t, "beta counts the active run", func() bool {
		ms, err := beta.f.Members(context.Background())
		return err == nil && len(ms) == 2 && ms[1].Active == 1
	})
	r, err := beta.f.Run(context.Background(), run.ID)
	if err != nil || r.State.Terminal() {
		t.Fatalf("Run = %+v, %v", r, err)
	}
}

// rulesRecord is a member in the rules mode with access.default closed:
//
//	r1: agent team, started by alice; team names alice as owner and the
//	    group ops as viewers.
//	r2: agent bare (no access block), started by bob.
//	r3: agent bare, a child of r1 with no principal.
func rulesRecord(name, def string) Record {
	now := time.Now().UTC()
	return Record{
		Name:   name,
		Access: Access{Mode: fw.AccessModeRules, Default: def, Admins: []string{"root"}},
		Agents: []RecordAgent{
			{Name: "bare"},
			{Name: "team", Access: &fw.Access{Owners: []string{"alice"}, Viewers: []string{"group:ops"}}},
		},
		Runs: []*fw.Run{
			{ID: "r3", Agent: "bare", State: fw.StateRunning, Parent: "r1", Depth: 1, CreatedAt: now.Add(3 * time.Second)},
			{ID: "r2", Agent: "bare", State: fw.StateCompleted, Principal: "bob", CreatedAt: now.Add(2 * time.Second)},
			{ID: "r1", Agent: "team", State: fw.StateInputRequired, Principal: "alice", CreatedAt: now.Add(time.Second)},
		},
	}
}

func TestRulesFiltering(t *testing.T) {
	t.Setenv("FW_ATESPACE", "")
	bus := fw.NewMemBus()
	retain(t, bus, rulesRecord("gamma", "closed"))
	view := newMember(t, bus, "view", nil)
	cases := []struct {
		who  context.Context
		name string
		want []string
	}{
		{as("alice"), "owner sees her run and its child", []string{"r1", "r3"}},
		{as("bob"), "the starter sees his own run", []string{"r2"}},
		{as("carol", "ops"), "a viewer group sees the run and its child", []string{"r1", "r3"}},
		{as("root"), "an admin of the member sees all", []string{"r1", "r2", "r3"}},
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
	if len(ms) != 2 || ms[1].Name != "gamma" || len(ms[1].Agents) != 1 || ms[1].Agents[0].Name != "team" {
		t.Fatalf("alice sees %+v", ms)
	}
	if ms[1].Active != 2 || ms[1].InputRequired != 1 {
		t.Fatalf("alice counts active %d, input %d", ms[1].Active, ms[1].InputRequired)
	}
	ms, _ = view.f.Members(as("dave"))
	if len(ms) != 1 {
		t.Fatalf("dave sees members %+v", ms)
	}
	roles := func(ctx context.Context, member string) map[string]string {
		ms, err := view.f.Members(ctx)
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
	if got := roles(as("alice"), "gamma"); len(got) != 1 || got["team"] != "owner" {
		t.Fatalf("alice roles %v", got)
	}
	if got := roles(as("carol", "ops"), "gamma"); len(got) != 1 || got["team"] != "viewer" {
		t.Fatalf("carol roles %v", got)
	}
	if got := roles(as("root"), "gamma"); got["team"] != "owner" || got["bare"] != "owner" {
		t.Fatalf("root roles %v", got)
	}
	if got := roles(context.Background(), "gamma"); got["team"] != "owner" || got["bare"] != "owner" {
		t.Fatalf("process roles %v", got)
	}
	// This process is open by default, so alice operates its default agent.
	if got := roles(as("alice"), "view"); got["default"] != "operator" {
		t.Fatalf("alice roles here %v", got)
	}
	// With access.default open, an agent without an access block is open.
	retain(t, bus, rulesRecord("delta", "open"))
	eventually(t, "the open member arrives", func() bool {
		runs, err := view.f.Runs(as("dave"))
		return err == nil && sameSet(runIDs(runs), "r2", "r3")
	})
	if got := roles(as("dave"), "delta"); len(got) != 1 || got["bare"] != "operator" {
		t.Fatalf("dave roles on the open member %v", got)
	}
}

func TestExternalMemberNeedsDecisionPoint(t *testing.T) {
	t.Setenv("FW_ATESPACE", "")
	bus := fw.NewMemBus()
	rec := rulesRecord("ext", "")
	rec.Access = Access{Mode: fw.AccessModeExternal}
	retain(t, bus, rec)

	// The default rules of this process know nothing about the member.
	plain := newMember(t, bus, "plain", nil)
	if runs, err := plain.f.Runs(as("alice")); err != nil || len(runs) != 0 {
		t.Fatalf("rules view shows %v, %v", runIDs(runs), err)
	}

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
	custom := newMember(t, bus, "custom", nil, fw.WithAuthz(authz))
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
			if ms[i].Name == "ext" {
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
		if m.Name == "ext" {
			t.Fatalf("bob sees the member ext: %+v", m)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	var child *fw.Target
	for i := range seen {
		if seen[i].Member != "ext" {
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
	bus := fw.NewMemBus()
	rec := rulesRecord("ext", "")
	rec.Access = Access{Mode: fw.AccessModeCustom}
	retain(t, bus, rec)
	authz := fw.AuthzFunc(func(_ context.Context, _ *fw.Principal, _ string, tg fw.Target) error {
		if tg.Member == "" {
			return nil
		}
		return fmt.Errorf("%w: the control plane is down", fw.ErrAuthzUnavailable)
	})
	view := newMember(t, bus, "view", nil, fw.WithAuthz(authz))
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

func TestForward(t *testing.T) {
	t.Setenv("FW_ATESPACE", "")
	owner := newCapture(t)
	router := newCapture(t)
	bus := fw.NewMemBus()
	retain(t, bus, Record{Name: "web", URL: owner.srv.URL, Access: Access{Mode: fw.AccessModeRules}})
	retain(t, bus, Record{Name: "team/rev", URL: "ax:team/rev", Access: Access{Mode: fw.AccessModeRules}})
	retain(t, bus, Record{Name: "nourl", Access: Access{Mode: fw.AccessModeRules}})
	retain(t, bus, Record{Name: "trick", URL: "http://x@evil.example/.127.0.0.1:1", Access: Access{Mode: fw.AccessModeRules}})
	view := newMember(t, bus, "view", func(c *fw.Config, fc *Config) {
		fc.Forward = []string{"http://127.0.0.1:*"}
		c.AX.Router = strings.TrimPrefix(router.srv.URL, "http://")
	})
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
	resp, err := view.f.Forward(cred, "web", newReq())
	if err != nil {
		t.Fatalf("Forward: %v", err)
	}
	resp.Body.Close()
	got, body := owner.last()
	if got.URL.Path != "/v1/runs/r1/approvals/ap1" || got.URL.RawQuery != "x=1" || got.Method != http.MethodPost || body != `{"decision":"allow"}` {
		t.Fatalf("owner got %s %s?%s %q", got.Method, got.URL.Path, got.URL.RawQuery, body)
	}
	if got.Header.Get("Authorization") != "Bearer tok" || got.Header.Get(HeaderVia) != "view" {
		t.Fatalf("owner got credential %q via %q", got.Header.Get("Authorization"), got.Header.Get(HeaderVia))
	}
	if got.Header.Get("Idempotency-Key") != "k1" || got.Header.Get("Accept") != "application/json" || got.Header.Get("X-Other") != "" {
		t.Fatalf("owner got headers %v", got.Header)
	}

	resp, err = view.f.Forward(cred, "team/rev", newReq())
	if err != nil {
		t.Fatalf("Forward ax: %v", err)
	}
	resp.Body.Close()
	got, _ = router.last()
	if got.Header.Get(headerTargetActor) != "team/rev" || got.URL.Path != "/v1/runs/r1/approvals/ap1" {
		t.Fatalf("router got actor %q path %q", got.Header.Get(headerTargetActor), got.URL.Path)
	}

	noRoute := []struct {
		name   string
		ctx    context.Context
		member string
		req    *http.Request
	}{
		{"unknown member", cred, "nobody", newReq()},
		{"this member", cred, "view", newReq()},
		{"member without URL", cred, "nourl", newReq()},
		{"URL outside the globs", cred, "trick", newReq()},
		{"a forwarded request", cred, "web", func() *http.Request { r := newReq(); r.Header.Set(HeaderVia, "other"); return r }()},
	}
	for _, c := range noRoute {
		if _, err := view.f.Forward(c.ctx, c.member, c.req); !errors.Is(err, ErrNoRoute) {
			t.Errorf("%s: %v", c.name, err)
		}
	}
	if _, err := view.f.Forward(context.Background(), "web", newReq()); !errors.Is(err, ErrNoCredential) {
		t.Fatalf("no credential: %v", err)
	}
	r, _ := http.NewRequest(http.MethodGet, "/metrics", nil)
	if _, err := view.f.Forward(cred, "web", r); err == nil {
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
	if !can["web"] || !can["team/rev"] || can["nourl"] || can["trick"] || can["view"] {
		t.Fatalf("CanForward = %v", can)
	}

	// Without a glob, an http member is read-only.
	closed := newMember(t, bus, "closed", nil)
	if _, err := closed.f.Forward(cred, "web", newReq()); !errors.Is(err, ErrNoRoute) {
		t.Fatalf("no glob: %v", err)
	}
}

func TestGlobMatch(t *testing.T) {
	cases := []struct {
		glob, s string
		want    bool
	}{
		{"http://*.agents.svc:8080", "http://reviewer.agents.svc:8080", true},
		{"http://*.agents.svc:8080", "http://evil.example/x.agents.svc:8080", false},
		{"http://*.agents.svc:8080", "http://reviewer.agents.svc:9090", false},
		{"https://*", "https://a.b.c", true},
		{"https://*", "https://a.b/c", false},
		{"https://*/*", "https://a.b/c", true},
		{"HTTP://A.example", "http://a.example", true},
		{"http://127.0.0.1:*", "http://127.0.0.1:4567", true},
	}
	for _, c := range cases {
		if got := globMatch(c.glob, c.s); got != c.want {
			t.Errorf("globMatch(%q, %q) = %v", c.glob, c.s, got)
		}
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

func TestPickAndFit(t *testing.T) {
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

	text := strings.Repeat("y", TextLimit)
	rec := Record{Name: "m", Access: Access{Mode: fw.AccessModeRules}}
	for i := 0; i < 120; i++ {
		rec.Runs = append(rec.Runs, &fw.Run{ID: fmt.Sprintf("b%03d", i), State: fw.StateCompleted, Input: text, Output: text, Error: text})
	}
	rec.Runs = append(rec.Runs, &fw.Run{ID: "active", State: fw.StateRunning, Input: text})
	if err := fit(&rec); err != nil {
		t.Fatalf("fit: %v", err)
	}
	b, _ := contentKey(rec)
	if len(b) > RecordLimit {
		t.Fatalf("record is %d bytes", len(b))
	}
	if rec.Omitted == 0 || rec.Runs[0].ID != "b000" || rec.Runs[len(rec.Runs)-1].ID != "active" {
		t.Fatalf("fit kept %d runs, omitted %d", len(rec.Runs), rec.Omitted)
	}
}

func TestRecordCutsLongInput(t *testing.T) {
	t.Setenv("FW_ATESPACE", "")
	bus := fw.NewMemBus()
	alpha := newMember(t, bus, "alpha", nil)
	beta := newMember(t, bus, "beta", nil)
	run, err := alpha.a.Start(context.Background(), strings.Repeat("z", 5000))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	eventually(t, "beta sees the run", func() bool {
		_, err := beta.f.Run(context.Background(), run.ID)
		return err == nil
	})
	r, _ := beta.f.Run(context.Background(), run.ID)
	if len(r.Input) > TextLimit || !strings.HasSuffix(r.Input, cutMark) {
		t.Fatalf("input is %d bytes", len(r.Input))
	}
	local, _ := alpha.a.Get(context.Background(), run.ID)
	if len(local.Input) != 5000 {
		t.Fatal("the record changed the run of the kernel")
	}
}

func TestHeartbeatWhenNothingChanged(t *testing.T) {
	t.Setenv("FW_ATESPACE", "")
	bus := fw.NewMemBus()
	var mu sync.Mutex
	records, alive := 0, 0
	stop1, _ := bus.Watch(context.Background(), subj+".members.alpha", func(fw.Event) error { mu.Lock(); records++; mu.Unlock(); return nil })
	stop2, _ := bus.Watch(context.Background(), subj+".alive.alpha", func(fw.Event) error { mu.Lock(); alive++; mu.Unlock(); return nil })
	defer stop1()
	defer stop2()
	count := func() (int, int) { mu.Lock(); defer mu.Unlock(); return records, alive }

	alpha := newMember(t, bus, "alpha", nil)
	eventually(t, "three heartbeats", func() bool { _, a := count(); return a >= 3 })
	if r, _ := count(); r != 1 {
		t.Fatalf("an unchanged member published %d records", r)
	}
	if _, err := alpha.a.Start(context.Background(), "hi"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	eventually(t, "a record after the run", func() bool { r, _ := count(); return r >= 2 })
}

// plainBus hides the Retainer of the mem bus.
type plainBus struct{ fw.Bus }

func TestBusWithoutRetainer(t *testing.T) {
	t.Setenv("FW_ATESPACE", "")
	bus := plainBus{fw.NewMemBus()}
	beta := newMember(t, bus, "beta", nil)
	if beta.f.ret != nil {
		t.Fatal("the plain bus counts as a Retainer")
	}
	if _, err := beta.f.Runs(context.Background()); err != nil {
		t.Fatalf("Runs: %v", err)
	}
	alpha := newMember(t, bus, "alpha", nil)
	run, err := alpha.a.Start(context.Background(), "hi")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	eventually(t, "beta sees alpha at an interval", func() bool {
		_, err := beta.f.Run(context.Background(), run.ID)
		return err == nil
	})
}

func TestStatesAndExpire(t *testing.T) {
	t.Setenv("FW_ATESPACE", "")
	bus := fw.NewMemBus()
	now := time.Now().UTC()
	run := []*fw.Run{{ID: "x1", Agent: "default", State: fw.StateRunning}}
	retain(t, bus, Record{Name: "old", Access: Access{Mode: fw.AccessModeRules, Default: "open"}, Runs: run, UpdatedAt: now.Add(-8 * 24 * time.Hour)})
	retain(t, bus, Record{Name: "sleepy", Access: Access{Mode: fw.AccessModeRules, Default: "open"}, Runs: run, UpdatedAt: now.Add(-time.Hour)})
	retain(t, bus, Record{Name: "gone", Access: Access{Mode: fw.AccessModeRules, Default: "open"}, Runs: run, Stopped: true})
	// A member with a long interval of its own stays live for three of its
	// intervals, whatever the interval of the view.
	retain(t, bus, Record{Name: "slow", Interval: time.Hour, Access: Access{Mode: fw.AccessModeRules, Default: "open"}, Runs: run, UpdatedAt: now.Add(-time.Hour)})
	// A long interval keeps a member live for three minutes.
	view := newMember(t, bus, "view", func(_ *fw.Config, fc *Config) { fc.Interval = time.Minute })
	states := func() map[string]string {
		ms, err := view.f.Members(context.Background())
		if err != nil {
			t.Fatalf("Members: %v", err)
		}
		out := map[string]string{}
		for _, m := range ms[1:] {
			out[m.Name] = m.State
		}
		return out
	}
	got := states()
	if len(got) != 3 || got["sleepy"] != MemberQuiet || got["gone"] != MemberStopped || got["slow"] != MemberLive {
		t.Fatalf("states = %v", got)
	}
	// A heartbeat brings an old member back.
	if err := bus.Retain(context.Background(), AliveSubject(subj, "old"), fw.MustEvent("old", EvAlive, Alive{Name: "old", At: time.Now().UTC()})); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the old member is live again", func() bool { return states()["old"] == MemberLive })
}

// TestCloseSendsStopped starts alpha as the kernel does, through the service
// Start, so the close of the agent stops the fleet and sends the last
// record.
func TestCloseSendsStopped(t *testing.T) {
	t.Setenv("FW_ATESPACE", "")
	t.Setenv(EnvSubject, subj)
	t.Setenv(EnvName, "alpha")
	t.Setenv(EnvInterval, "40ms")
	bus := fw.NewMemBus()
	cfg := fw.DefaultConfig()
	cfg.Home = t.TempDir()
	cfg.Agents = map[string]fw.AgentProfile{"default": {}}
	alpha, err := fw.New(fw.WithConfig(cfg), fw.WithoutDefaults(), fw.WithAuthn(fw.NoAuthn{}),
		fw.WithModel(textModel("done")), fw.WithLedger(fw.NewMemLedger()), fw.WithBus(bus),
		fw.WithLogger(quietLogger()), fw.WithServices(Start))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	f, ok := Of(alpha).(*Fleet)
	if !ok || f == nil || f.Name() != "alpha" {
		t.Fatalf("the service attached %T", Of(alpha))
	}
	beta := newMember(t, bus, "beta", nil)
	eventually(t, "beta sees alpha", func() bool {
		ms, err := beta.f.Members(context.Background())
		return err == nil && len(ms) == 2 && ms[1].State == MemberLive
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	alpha.Close(ctx)
	eventually(t, "alpha is stopped", func() bool {
		ms, err := beta.f.Members(context.Background())
		return err == nil && len(ms) == 2 && ms[1].State == MemberStopped
	})
	if Of(alpha) != nil {
		t.Fatal("the service left its view attached")
	}
	if err := f.Close(); err != nil {
		t.Fatalf("a second Close: %v", err)
	}
	if _, err := f.Runs(context.Background()); err == nil {
		t.Fatal("a closed fleet started its view")
	}
}

func TestMemberName(t *testing.T) {
	host, _ := os.Hostname()
	if host == "" {
		host = "fw"
	}
	cfg := fw.DefaultConfig()
	cfg.Listen = ":8080"
	if got, _ := defaults("", "", cfg); got != host+":8080" {
		t.Fatalf("default name = %q", got)
	}
	if got, u := defaults("team/rev", "", cfg); got != "team/rev" || u != "" {
		t.Fatalf("given name = %q, url %q", got, u)
	}
	cfg.Listen = ""
	if got, _ := defaults("", "", cfg); got != host {
		t.Fatalf("default name without a port = %q", got)
	}
	// The variables win over the file, and a bad value is an error.
	t.Setenv(EnvSubject, "fw.lab")
	t.Setenv(EnvName, "me")
	t.Setenv(EnvInterval, "5s")
	c, err := Load(fw.DefaultConfig())
	if err != nil || c.Subject != "fw.lab" || c.Name != "me" || c.Interval != 5*time.Second || c.Expire != DefaultExpire {
		t.Fatalf("Load = %+v, %v", c, err)
	}
	t.Setenv(EnvURL, "ftp://x")
	if _, err := Load(fw.DefaultConfig()); err == nil || !strings.Contains(err.Error(), "workbench.fleet.url") {
		t.Fatalf("a bad URL = %v", err)
	}
}

func TestRecordAccess(t *testing.T) {
	t.Setenv("FW_ATESPACE", "team")
	bus := fw.NewMemBus()
	alpha := newMember(t, bus, "alpha", func(c *fw.Config, _ *Config) {
		c.Access.Default = " Closed "
		c.Access.Admins = []string{"root"}
		c.Agents = map[string]fw.AgentProfile{"rev": {Description: "reviews", Access: &fw.Access{Owners: []string{"alice"}}}}
	})
	rec, err := alpha.f.record()
	if err != nil {
		t.Fatalf("record: %v", err)
	}
	if rec.Access.Mode != fw.AccessModeRules || rec.Access.Default != "closed" || len(rec.Access.Admins) != 1 || rec.Atespace != "team" {
		t.Fatalf("access = %+v, atespace %q", rec.Access, rec.Atespace)
	}
	if len(rec.Agents) != 1 || rec.Agents[0].Name != "rev" || rec.Agents[0].Description != "reviews" || rec.Agents[0].Access.Owners[0] != "alice" {
		t.Fatalf("agents = %+v", rec.Agents)
	}
	custom := newMember(t, bus, "custom", nil, fw.WithAuthz(fw.AuthzFunc(func(context.Context, *fw.Principal, string, fw.Target) error { return nil })))
	rec, _ = custom.f.record()
	if rec.Access.Mode != fw.AccessModeCustom || rec.Access.Default != "" || rec.Access.Admins != nil {
		t.Fatalf("custom access = %+v", rec.Access)
	}
}

// A process that configures no agent, such as a front that only serves the
// view, offers no agent to the fleet, so a person does not see it.
func TestFrontWithoutAgentsStaysOutOfSight(t *testing.T) {
	t.Setenv("FW_ATESPACE", "")
	bus := fw.NewMemBus()
	front := newMember(t, bus, "front-1", func(c *fw.Config, _ *Config) { c.Agents = map[string]fw.AgentProfile{} })
	view := newMember(t, bus, "view", func(c *fw.Config, _ *Config) {
		c.Agents = map[string]fw.AgentProfile{"reviewer": {Description: "Reviews"}}
	})
	eventually(t, "the view hears the front", func() bool {
		ms, err := view.f.Members(context.Background())
		return err == nil && len(ms) == 2
	})
	rec, err := front.f.record()
	if err != nil || len(rec.Agents) != 0 {
		t.Fatalf("front record agents = %+v, %v", rec.Agents, err)
	}
	ms, err := view.f.Members(as("alice"))
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) != 1 || !ms[0].Self || len(ms[0].Agents) != 1 || ms[0].Agents[0].Name != "reviewer" {
		t.Fatalf("members for alice = %+v", ms)
	}
	// The view of the front itself lists no agent of its own.
	self, err := front.f.Members(as("alice"))
	if err != nil || len(self) == 0 || len(self[0].Agents) != 0 {
		t.Fatalf("front self = %+v, %v", self, err)
	}
}

// slowBus is a bus whose retained publishes hang until their context ends,
// like a bus that is not reachable yet.
type slowBus struct{ *fw.MemBus }

func (s slowBus) Retain(ctx context.Context, _ string, _ fw.Event) error {
	<-ctx.Done()
	return ctx.Err()
}

func TestStartDoesNotWaitForTheBus(t *testing.T) {
	t.Setenv("FW_ATESPACE", "")
	start := time.Now()
	newMember(t, slowBus{fw.NewMemBus()}, "slow", func(_ *fw.Config, fc *Config) { fc.Interval = time.Second })
	if d := time.Since(start); d > 500*time.Millisecond {
		t.Fatalf("New took %v with a bus that hangs", d)
	}
}

func TestViewWaitsForANewRun(t *testing.T) {
	t.Setenv("FW_ATESPACE", "")
	bus := fw.NewMemBus()
	// A long interval, so only the publish of the change can name the run.
	owner := newMember(t, bus, "owner", func(_ *fw.Config, fc *Config) { fc.Interval = time.Minute })
	view := newMember(t, bus, "view", func(_ *fw.Config, fc *Config) { fc.Interval = time.Minute })
	if _, err := view.f.Members(context.Background()); err != nil {
		t.Fatal(err)
	}
	r, err := owner.a.Start(context.Background(), "hello")
	if err != nil {
		t.Fatal(err)
	}
	got, err := view.f.Run(context.Background(), r.ID)
	if err != nil || got.Member != "owner" {
		t.Fatalf("a run started a moment ago = %+v, %v", got, err)
	}
	// An id from long ago that no record names is not found at once.
	start := time.Now()
	if _, err := view.f.Run(context.Background(), "00000000000000000000000000"); !errors.Is(err, fw.ErrRunNotFound) {
		t.Fatalf("an old unknown id = %v", err)
	}
	if d := time.Since(start); d > 200*time.Millisecond {
		t.Fatalf("an old unknown id took %v", d)
	}
}
