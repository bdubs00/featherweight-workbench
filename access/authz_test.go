package access

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"gopkg.in/yaml.v3"
)

// plane answers decision requests on a bus subject, like a control plane
// outside the process. It records every request.
type plane struct {
	mu     sync.Mutex
	reqs   []fw.AuthzRequest
	decide func(p fw.Principal, q fw.AuthzQuery) fw.AuthzDecision
}

func (c *plane) serve(t *testing.T, bus fw.Bus, subject string) {
	t.Helper()
	stop, err := bus.Subscribe(context.Background(), subject, func(e fw.Event) error {
		raw, err := fw.DecisionRequestOf(e)
		if err != nil {
			t.Errorf("request: %v", err)
			return nil
		}
		var req fw.AuthzRequest
		if json.Unmarshal(raw, &req) != nil || req.Kind != fw.DecisionAuthz {
			return nil
		}
		c.mu.Lock()
		c.reqs = append(c.reqs, req)
		var ans fw.AuthzAnswer
		for _, q := range req.Checks {
			ans.Decisions = append(ans.Decisions, c.decide(req.Principal, q))
		}
		c.mu.Unlock()
		to, reply, err := fw.AnswerDecision(e, ans)
		if err != nil {
			t.Errorf("answer: %v", err)
			return nil
		}
		return bus.Publish(context.Background(), to, reply)
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stop)
}

func (c *plane) requests() []fw.AuthzRequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]fw.AuthzRequest(nil), c.reqs...)
}

// model ends every turn at once with one word.
func model(out string) fw.Model {
	return fw.ModelFunc{StreamFn: func(context.Context, *fw.Request, func(fw.Delta)) (*fw.Response, error) {
		return &fw.Response{Content: []fw.Block{fw.Text(out)}, StopReason: fw.StopEndTurn}, nil
	}}
}

// withBlock puts a workbench block into the configuration, as the loader
// keeps it.
func withBlock(t *testing.T, cfg *fw.Config, text string) {
	t.Helper()
	var n yaml.Node
	if err := yaml.Unmarshal([]byte(text), &n); err != nil {
		t.Fatal(err)
	}
	if cfg.Extensions == nil {
		cfg.Extensions = map[string]yaml.Node{}
	}
	cfg.Extensions[Block] = *n.Content[0]
}

// newAgent builds an agent whose authorization goes to the control plane on
// the bus subject fw.authz, through Build, as the binary wires it.
func newAgent(t *testing.T, bus fw.Bus, m fw.Model, mutate func(*fw.Config), opts ...fw.Option) *fw.Agent {
	t.Helper()
	t.Setenv("FW_ATESPACE", "")
	cfg := fw.DefaultConfig()
	cfg.Home = t.TempDir()
	cfg.Agents = map[string]fw.AgentProfile{"default": {}}
	withBlock(t, cfg, "access:\n  external: {bus: fw.authz, timeout: 2s}\n")
	if mutate != nil {
		mutate(cfg)
	}
	base := []fw.Option{
		fw.WithConfig(cfg), fw.WithoutDefaults(), fw.WithAuthn(fw.NoAuthn{}), fw.WithModel(m),
		fw.WithLedger(fw.NewMemLedger()), fw.WithBus(bus), fw.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))),
		fw.WithAuthzFrom(Build),
	}
	a, err := fw.New(append(base, opts...)...)
	if err != nil {
		t.Fatalf("fw.New: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		a.Close(ctx)
	})
	return a
}

var (
	alice = &fw.Principal{Subject: "alice", Kind: fw.KindUser, Groups: []string{"platform"}}
	bob   = &fw.Principal{Subject: "bob", Kind: fw.KindUser}
)

func as(p *fw.Principal) context.Context { return fw.WithPrincipal(context.Background(), p) }

func TestLoad(t *testing.T) {
	cfg := fw.DefaultConfig()
	if e, err := Load(cfg); e != nil || err != nil {
		t.Fatalf("without a block = %+v, %v", e, err)
	}
	cfg.Tools.MCP = []fw.MCPServerConfig{{Name: "control-plane", Endpoint: "https://cp.example.com/mcp"}}
	withBlock(t, cfg, "access:\n  external: {mcp: Control_Plane}\n")
	e, err := Load(cfg)
	if err != nil || e.MCP != "Control_Plane" || e.Tool != fw.DefaultAuthzTool || e.Timeout != fw.DefaultDecisionTimeout {
		t.Fatalf("defaults = %+v, %v", e, err)
	}
	if got := ControlServers(cfg); len(got) != 1 || got[0] != "Control_Plane" {
		t.Fatalf("ControlServers = %v", got)
	}
	// The variables win over the file.
	t.Setenv(EnvBus, "fw.authz")
	t.Setenv(EnvMCP, " ")
	t.Setenv(EnvTimeout, "250ms")
	e, err = Load(cfg)
	if err == nil || !strings.Contains(err.Error(), "sets both mcp") {
		t.Fatalf("both transports = %+v, %v", e, err)
	}
	withBlock(t, cfg, "access: {}\n")
	e, err = Load(cfg)
	if err != nil || e.Bus != "fw.authz" || e.Timeout != 250*time.Millisecond || e.Tool != fw.DefaultAuthzTool {
		t.Fatalf("from the variables = %+v, %v", e, err)
	}
	t.Setenv(EnvTimeout, "soon")
	if _, err := Load(cfg); err == nil || !strings.Contains(err.Error(), EnvTimeout) {
		t.Fatalf("bad timeout = %v", err)
	}
	if Of(cfg) != nil {
		t.Fatal("Of reports a control plane for a block that does not load")
	}
	t.Setenv(EnvTimeout, "")
	t.Setenv(EnvBus, "")
	withBlock(t, cfg, "access:\n  external: {mcp: nowhere}\n")
	if _, err := Load(cfg); err == nil || !strings.Contains(err.Error(), `workbench.access.external.mcp names "nowhere"`) {
		t.Fatalf("unknown server = %v", err)
	}
	withBlock(t, cfg, "access:\n  external: {bus: 'fw.authz.>'}\n")
	if _, err := Load(cfg); err == nil || !strings.Contains(err.Error(), "not a subject") {
		t.Fatalf("wildcard = %v", err)
	}
}

// The control plane decides, and the local rules do not apply. A call from
// the process itself never asks.
func TestControlPlaneDecides(t *testing.T) {
	bus := fw.NewMemBus()
	cp := &plane{decide: func(p fw.Principal, q fw.AuthzQuery) fw.AuthzDecision {
		if p.Subject == "alice" {
			return fw.AuthzDecision{Allow: true}
		}
		return fw.AuthzDecision{Reason: "only the platform group runs agents"}
	}}
	cp.serve(t, bus, "fw.authz")
	a := newAgent(t, bus, model("hi"), func(c *fw.Config) {
		// The local rules would refuse alice and allow bob.
		c.Access.Default = "closed"
		c.Access.Admins = []string{"bob"}
	})
	if a.AccessMode() != fw.AccessModeExternal {
		t.Fatalf("mode = %q", a.AccessMode())
	}
	r, err := a.Run(as(alice), "hi")
	if err != nil || r.State != fw.StateCompleted {
		t.Fatalf("alice = %+v, %v", r, err)
	}
	_, err = a.Start(as(bob), "hi")
	if !errors.Is(err, fw.ErrForbidden) || !strings.Contains(err.Error(), "only the platform group runs agents") {
		t.Fatalf("bob = %v", err)
	}
	reqs := cp.requests()
	if len(reqs) < 2 || reqs[0].Version != fw.DecisionVersion || reqs[0].Principal.Subject != "alice" ||
		len(reqs[0].Checks) != 1 || reqs[0].Checks[0].Action != fw.ActionAgentRun || reqs[0].Checks[0].Agent != "default" {
		t.Fatalf("requests = %+v", reqs)
	}
	n := len(cp.requests())
	if _, err := a.List(context.Background()); err != nil || len(cp.requests()) != n {
		t.Fatalf("an internal call asked the control plane: %v", err)
	}
	st := a.ProbeExternal(context.Background())
	if len(st) != 1 || st[0].Use != Use || st[0].Probed || st[0].Target != "the bus subject fw.authz" {
		t.Fatalf("status = %+v", st)
	}
}

// A listing sends its checks in one request, each with the lineage of its
// run, and a refusal is final.
func TestControlPlaneBatchesAndSendsLineage(t *testing.T) {
	bus := fw.NewMemBus()
	cp := &plane{decide: func(p fw.Principal, q fw.AuthzQuery) fw.AuthzDecision {
		if q.Run == nil {
			return fw.AuthzDecision{Allow: true}
		}
		owner := q.Run.Principal
		if n := len(q.Run.Lineage); n > 0 {
			owner = q.Run.Lineage[n-1].Principal
		}
		return fw.AuthzDecision{Allow: owner == p.Subject}
	}}
	cp.serve(t, bus, "fw.authz")
	a := newAgent(t, bus, model("x"), nil)
	parent, err := a.Run(as(alice), "parent")
	if err != nil || parent.State != fw.StateCompleted {
		t.Fatalf("parent = %+v, %v", parent, err)
	}
	child, err := a.Run(context.Background(), "child", fw.WithParent(parent.ID))
	if err != nil || child.State != fw.StateCompleted {
		t.Fatalf("child = %+v, %v", child, err)
	}
	if _, err := a.Run(context.Background(), "other", fw.WithPrincipalName("bob")); err != nil {
		t.Fatal(err)
	}
	before := len(cp.requests())
	runs, err := a.List(as(alice))
	if err != nil {
		t.Fatal(err)
	}
	reqs := cp.requests()[before:]
	if len(reqs) != 1 || len(reqs[0].Checks) != 3 {
		t.Fatalf("listing sent %d requests: %+v", len(reqs), reqs)
	}
	if len(runs) != 2 {
		t.Fatalf("alice sees %d runs, want the parent and the child", len(runs))
	}
	for _, q := range reqs[0].Checks {
		if q.Run.ID == child.ID && (len(q.Run.Lineage) != 1 || q.Run.Lineage[0].ID != parent.ID || q.Run.Lineage[0].Principal != "alice") {
			t.Fatalf("child facts = %+v", q.Run)
		}
	}
	before = len(cp.requests())
	if _, err := a.Get(as(bob), child.ID); !errors.Is(err, fw.ErrForbidden) {
		t.Fatalf("bob reads the child: %v", err)
	}
	if got := len(cp.requests()) - before; got != 1 {
		t.Fatalf("one refusal sent %d requests", got)
	}
}

// A check about a run of another member carries the member, its access
// block, and its lineage from the view.
func TestControlPlaneSendsMemberFacts(t *testing.T) {
	bus := fw.NewMemBus()
	cp := &plane{decide: func(fw.Principal, fw.AuthzQuery) fw.AuthzDecision { return fw.AuthzDecision{Allow: true} }}
	cp.serve(t, bus, "fw.authz")
	a := newAgent(t, bus, model("hi"), nil)
	run := &fw.Run{ID: "r2", Agent: "reviewer", Principal: "carol", State: fw.StateInputRequired, Parent: "r1"}
	errs := a.AuthorizeAll(as(bob), []fw.AuthzCheck{{Action: fw.ActionRunView, Target: fw.Target{
		Agent: "reviewer", Run: run, Member: "team/other",
		Access:  &fw.Access{Viewers: []string{"bob"}},
		Lineage: []fw.RunRef{{ID: "r1", Agent: "planner", Principal: "carol"}},
	}}})
	if errs[0] != nil {
		t.Fatalf("decision = %v", errs[0])
	}
	reqs := cp.requests()
	if len(reqs) != 1 || len(reqs[0].Checks) != 1 {
		t.Fatalf("requests = %+v", reqs)
	}
	q := reqs[0].Checks[0]
	if q.Member != "team/other" || q.Agent != "reviewer" || q.Access == nil || q.Access.Viewers[0] != "bob" ||
		q.Run == nil || q.Run.ID != "r2" || q.Run.Principal != "carol" || len(q.Run.Lineage) != 1 || q.Run.Lineage[0].ID != "r1" {
		t.Fatalf("check = %+v run = %+v", q, q.Run)
	}
}

// ttlMs lets the plug reuse a decision; zero means no reuse.
func TestControlPlaneCache(t *testing.T) {
	bus := fw.NewMemBus()
	var ttl atomic.Int64
	ttl.Store(60_000)
	cp := &plane{decide: func(fw.Principal, fw.AuthzQuery) fw.AuthzDecision {
		return fw.AuthzDecision{Allow: true, TTLMs: ttl.Load()}
	}}
	cp.serve(t, bus, "fw.authz")
	a := newAgent(t, bus, model("hi"), nil, fw.WithProfile(fw.AgentProfile{Name: "reviewer"}))
	now := time.Now()
	z := a.Authz().(*controlPlane)
	z.now = func() time.Time { return now }
	ctx := as(alice)
	for i := 0; i < 3; i++ {
		_ = a.ProfilesFor(ctx)
	}
	if n := len(cp.requests()); n != 1 {
		t.Fatalf("cached decision sent %d requests", n)
	}
	now = now.Add(2 * time.Minute)
	ttl.Store(0)
	_ = a.ProfilesFor(ctx)
	_ = a.ProfilesFor(ctx)
	if n := len(cp.requests()); n != 3 {
		t.Fatalf("expired and uncached decisions sent %d requests, want 3", n)
	}
}

// A control plane that does not answer refuses the caller with
// ErrAuthzUnavailable, never opens access, and a listing reports the outage
// instead of an empty list. The process itself keeps working.
func TestControlPlaneUnavailable(t *testing.T) {
	a := newAgent(t, fw.NewMemBus(), model("internal"), func(c *fw.Config) {
		withBlock(t, c, "access:\n  external: {bus: fw.authz, timeout: 30ms}\n")
	}, fw.WithProfile(fw.AgentProfile{Name: "reviewer"}))
	_, err := a.Start(as(alice), "hi")
	if !errors.Is(err, fw.ErrAuthzUnavailable) || errors.Is(err, fw.ErrForbidden) || !strings.Contains(err.Error(), "fw.authz") {
		t.Fatalf("Start without a control plane = %v", err)
	}
	r, err := a.Run(context.Background(), "hi")
	if err != nil || r.State != fw.StateCompleted {
		t.Fatalf("internal run = %+v, %v", r, err)
	}
	if _, err := a.List(as(alice)); !errors.Is(err, fw.ErrAuthzUnavailable) {
		t.Fatalf("List = %v", err)
	}
	if _, err := a.Children(as(alice), r.ID); !errors.Is(err, fw.ErrAuthzUnavailable) {
		t.Fatalf("Children = %v", err)
	}
	if _, err := a.ListProfiles(as(alice)); !errors.Is(err, fw.ErrAuthzUnavailable) {
		t.Fatalf("ListProfiles = %v", err)
	}
}
