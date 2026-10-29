package away

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/duties"
	"github.com/bdubs00/featherweight-workbench/fleet"
	"github.com/bdubs00/featherweight-workbench/internal/bucket"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// clock is the time of a test, which the test moves.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) Add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// fleetOf is the fleet of a test: its runs, and what the front did to
// them.
type fleetOf struct {
	mu       sync.Mutex
	runs     map[string]*fw.Run
	decided  []string // run/approval=decision
	notes    []string
	canceled []string
	asked    []fw.RunRequest
	deciders map[string][2]string // approval to decider and verdict
	failQ    error
}

func (f *fleetOf) set(r *fw.Run) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := *r
	f.runs[r.ID] = &c
}

func (f *fleetOf) waiting(context.Context) ([]*fw.Run, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*fw.Run
	for _, r := range f.runs {
		c := *r
		out = append(out, &c)
	}
	return out, nil
}

func (f *fleetOf) run(_ context.Context, id string) (*fw.Run, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.runs[id]
	if !ok {
		return nil, fw.ErrRunNotFound
	}
	c := *r
	return &c, nil
}

func (f *fleetOf) decide(_ context.Context, r *fw.Run, approval, decision, note string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	cur := f.runs[r.ID]
	if cur == nil || cur.Pending == nil || cur.Pending.ID != approval {
		return errGone
	}
	cur.State, cur.Pending = fw.StateRunning, nil
	f.decided = append(f.decided, r.ID+"/"+approval+"="+decision)
	f.notes = append(f.notes, note)
	return nil
}

func (f *fleetOf) cancel(_ context.Context, r *fw.Run) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	cur := f.runs[r.ID]
	if cur == nil {
		return errGone
	}
	cur.State, cur.Pending = fw.StateCanceled, nil
	f.canceled = append(f.canceled, r.ID)
	return nil
}

func (f *fleetOf) enqueue(_ context.Context, agent string, req fw.RunRequest) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failQ != nil {
		return f.failQ
	}
	req.Agent = agent
	f.asked = append(f.asked, req)
	f.runs[req.ID] = &fw.Run{ID: req.ID, Agent: agent, State: fw.StateQueued, Metadata: req.Metadata}
	return nil
}

func (f *fleetOf) decider(_ context.Context, _ *fw.Run, approval string) (string, string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	d, ok := f.deciders[approval]
	return d[0], d[1], ok
}

func (f *fleetOf) lists() (decided, notes, canceled []string, asked []fw.RunRequest) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.decided...), append([]string(nil), f.notes...), append([]string(nil), f.canceled...), append([]fw.RunRequest(nil), f.asked...)
}

// notices collects the notices of the duties, by agent.
type notices struct {
	mu   sync.Mutex
	list map[*fw.Agent][]duties.Notice
}

func (n *notices) add(_ context.Context, a *fw.Agent, x duties.Notice) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.list == nil {
		n.list = map[*fw.Agent][]duties.Notice{}
	}
	n.list[a] = append(n.list[a], x)
}

func (n *notices) of(a *fw.Agent) []duties.Notice {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]duties.Notice(nil), n.list[a]...)
}

var allNotices = &notices{}

func init() { duties.OnNotice(allNotices.add) }

// front is a process with the duties in memory and the watch of a test,
// on the fleet of the test. alice owns the agent triage. The watch does
// not run by itself: a test calls pass.
type front struct {
	a     *fw.Agent
	s     *Service
	fleet *fleetOf
	clock *clock
	mgr   string
}

func newFront(t *testing.T, b bucket.Bucket, fl *fleetOf, c *clock) *front {
	t.Helper()
	t.Setenv(fleet.EnvIntake, "fw.jobs")
	t.Setenv("FW_ATESPACE", "")
	cfg := fw.DefaultConfig()
	cfg.Home = t.TempDir()
	cfg.Access.Default = "closed"
	cfg.Auth.Mode = "none"
	cfg.Agents = map[string]fw.AgentProfile{
		"triage": {Access: &fw.Access{Owners: []string{"alice"}}},
	}
	f := &front{fleet: fl, clock: c, mgr: "manager"}
	start := func(_ context.Context, a *fw.Agent) (io.Closer, error) {
		s := newService(a, b, quiet())
		s.now, s.replica = c.Now, "front-"+fw.NewID()[20:]
		s.waiting, s.run, s.decide, s.cancelRun, s.enqueue, s.decider = fl.waiting, fl.run, fl.decide, fl.cancel, fl.enqueue, fl.decider
		s.manager = func() string { return f.mgr }
		services.Store(a, s)
		duties.SetAwayHandler(true)
		return closerFunc(func() { s.Close() }), nil
	}
	a, err := fw.New(fw.WithConfig(cfg), fw.WithoutDefaults(),
		fw.WithModel(fw.ModelFunc{GenerateFn: func(context.Context, *fw.Request) (*fw.Response, error) {
			return &fw.Response{Content: []fw.Block{fw.Text("done")}, StopReason: fw.StopEndTurn}, nil
		}}),
		fw.WithLedger(fw.NewMemLedger()), fw.WithBus(fw.NewMemBus()),
		fw.WithServices(duties.Start, start), fw.WithLogger(quiet()))
	if err != nil {
		t.Fatalf("fw.New: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		a.Close(ctx)
	})
	f.a, f.s = a, Of(a)
	return f
}

type closerFunc func()

func (c closerFunc) Close() error {
	c()
	return nil
}

func newFleet() *fleetOf {
	return &fleetOf{runs: map[string]*fw.Run{}, deciders: map[string][2]string{}}
}

// t0 is Friday, October 23, 2026, at 12:00 in New York.
var t0 = time.Date(2026, 10, 23, 16, 0, 0, 0, time.UTC)

func as(who string) context.Context {
	return fw.WithPrincipal(context.Background(), &fw.Principal{Subject: who, Kind: fw.KindUser})
}

// duty writes a duty of triage owned by alice that starts on a call, with
// whileAway, and starts it now. It returns the run of the start.
// byManager lets the manager decide while the owner is away. Nobody is the
// default, so a test of the manager names it.
var byManager = &duties.WhileAway{Decider: duties.DeciderManager, Timeout: "2h", Then: "deny"}

func (f *front) duty(t *testing.T, name string, away *duties.WhileAway) string {
	t.Helper()
	d := duties.Duty{
		Name: name, Agent: "triage",
		When:      duties.When{Webhook: &duties.Webhook{Auth: duties.WebhookAuth{Mode: "none"}}},
		Do:        duties.Do{Ask: &duties.Ask{Message: "Push the report of {date}."}},
		Limits:    duties.Limits{Budget: &duties.Budget{MaxCostUSD: 0.5}},
		WhileAway: away,
		Notify:    &duties.Notify{NeedsYou: []string{"owner", "erin"}},
	}
	svc := duties.Of(f.a)
	if _, err := svc.Put(as("alice"), name, duties.DutyBody{Duty: d}, duties.PutOptions{}); err != nil {
		t.Fatalf("put the duty %s: %v", name, err)
	}
	out, err := svc.RunNow(as("alice"), name)
	if err != nil || out.Run == "" {
		t.Fatalf("RunNow = %+v, %v", out, err)
	}
	return out.Run
}

// asks makes the run of a duty wait for an approval of a tool.
func (f *front) asks(run, duty, approval, tool, reason string, schema json.RawMessage) {
	f.fleet.set(&fw.Run{
		ID: run, Agent: "triage", State: fw.StateInputRequired, Metadata: map[string]string{duties.MetaDuty: duty},
		Pending: &fw.Approval{ID: approval, Tool: tool, Input: json.RawMessage(`{"command":"git push origin main"}`), Reason: reason, Schema: schema, RequestedAt: f.clock.Now()},
	})
}

func (f *front) pass(t *testing.T) {
	t.Helper()
	f.s.pass(context.Background())
}

// decided returns the decisions in the history of a duty, newest start
// first.
func (f *front) decided(t *testing.T, name string) []duties.Decided {
	t.Helper()
	v, err := duties.Of(f.a).Get(as("alice"), name)
	if err != nil {
		t.Fatal(err)
	}
	var out []duties.Decided
	for _, e := range v.History {
		out = append(out, e.Decided...)
	}
	return out
}

// entry returns the entry of an approval.
func entryOf(t *testing.T, b bucket.Bucket, run, approval string) wait {
	t.Helper()
	e, err := b.Get(context.Background(), waitKey(run, approval))
	if err != nil {
		t.Fatalf("the entry of %s/%s: %v", run, approval, err)
	}
	var w wait
	if err := json.Unmarshal(e.Value, &w); err != nil {
		t.Fatal(err)
	}
	return w
}
