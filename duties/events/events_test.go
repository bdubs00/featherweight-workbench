package events

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/bridges"
	"github.com/bdubs00/featherweight-workbench/duties"
	"github.com/bdubs00/featherweight-workbench/fleet"
	"github.com/bdubs00/featherweight-workbench/internal/bucket"
	"github.com/bdubs00/featherweight-workbench/pages"
	"github.com/bdubs00/featherweight-workbench/routines"
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

// runsOf is the fleet of a test.
type runsOf struct {
	mu   sync.Mutex
	runs map[string]*fw.Run
}

func (f *runsOf) set(r *fw.Run) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := *r
	f.runs[r.ID] = &c
}

func (f *runsOf) list(context.Context) ([]*fw.Run, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*fw.Run
	for _, r := range f.runs {
		c := *r
		out = append(out, &c)
	}
	return out, nil
}

func (f *runsOf) get(_ context.Context, id string) (*fw.Run, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r, ok := f.runs[id]; ok {
		c := *r
		return &c, nil
	}
	return nil, fw.ErrRunNotFound
}

// front is a process with the duties in memory and the events of a test
// on the fleet of the test. alice owns the agent triage.
type front struct {
	a     *fw.Agent
	s     *Service
	fleet *runsOf
	clock *clock
	b     bucket.Bucket
}

func newFront(t *testing.T) *front {
	t.Helper()
	t.Setenv(fleet.EnvIntake, "fw.jobs")
	t.Setenv("FW_ATESPACE", "")
	cfg := fw.DefaultConfig()
	cfg.Home = t.TempDir()
	cfg.Auth.Mode = "none"
	cfg.Access.Default = "closed"
	cfg.Agents = map[string]fw.AgentProfile{"triage": {Access: &fw.Access{Owners: []string{"alice"}}}}
	f := &front{fleet: &runsOf{runs: map[string]*fw.Run{}}, clock: &clock{t: time.Now().Add(time.Second)}, b: bucket.NewMem(eventsBucket)}
	start := func(_ context.Context, a *fw.Agent) (io.Closer, error) {
		s := f.service(a, "front-0")
		services.Store(a, s)
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

// service builds a replica of the events of the front on its bucket.
func (f *front) service(a *fw.Agent, replica string) *Service {
	s := newService(a, f.b, quiet())
	s.now, s.replica, s.runs, s.run = f.clock.Now, replica, f.fleet.list, f.fleet.get
	return s
}

type closerFunc func()

func (c closerFunc) Close() error {
	c()
	return nil
}

func as(who string) context.Context {
	return fw.WithPrincipal(context.Background(), &fw.Principal{Subject: who, Kind: fw.KindUser})
}

// duty writes a duty of triage owned by alice that starts on the event.
func (f *front) duty(t *testing.T, name string, e duties.Event) {
	t.Helper()
	d := duties.Duty{
		Name: name, Agent: "triage",
		When:   duties.When{Event: &e},
		Do:     duties.Do{Ask: &duties.Ask{Message: "Look at {event}."}},
		Limits: duties.Limits{Budget: &duties.Budget{MaxCostUSD: 0.5}, Overlap: duties.OverlapAllow},
	}
	if _, err := duties.Of(f.a).Put(as("alice"), name, duties.DutyBody{Duty: d}, duties.PutOptions{}); err != nil {
		t.Fatalf("put the duty %s: %v", name, err)
	}
}

// history returns the history of a duty, newest first.
func (f *front) history(t *testing.T, name string) []duties.RunEntry {
	t.Helper()
	v, err := duties.Of(f.a).Get(as("alice"), name)
	if err != nil {
		t.Fatal(err)
	}
	return v.History
}

func (f *front) pass(t *testing.T) {
	t.Helper()
	f.s.pass(context.Background())
}

// The check of when.event names each field that does not fit.
func TestCheck(t *testing.T) {
	for _, c := range []struct {
		e    duties.Event
		want string
	}{
		{duties.Event{Kind: duties.EventRunFailed, After: "2h"}, ""},
		{duties.Event{Kind: duties.EventRunFailed, After: "48h"}, "when.event.after: 48h is out of bounds"},
		{duties.Event{Kind: duties.EventBridgeClosed, Agent: "triage"}, "when.event.agent: a closed bridge names no agent"},
		{duties.Event{Kind: duties.EventRunFailed, Agent: "a b"}, "when.event.agent: \"a b\" is not the name of an agent"},
		{duties.Event{Kind: duties.EventRunFailed, Path: "x"}, "when.event.path: only page.changed takes a path"},
		{duties.Event{Kind: duties.EventPageChanged, Path: "["}, "when.event.path: \"[\" is not a glob"},
		{duties.Event{Kind: duties.EventPageChanged, Path: "reports/*.md"}, ""},
	} {
		got := strings.Join(Check(&c.e), " ")
		if (c.want == "" && got != "") || (c.want != "" && !strings.Contains(got, c.want)) {
			t.Errorf("Check(%+v) = %q, want %q", c.e, got, c.want)
		}
	}
	events, _, _ := duties.Offered()
	if strings.Join(events, ",") != strings.Join(Kinds, ",") {
		t.Fatalf("Offered events = %v", events)
	}
}

// A failed run of the agent starts the duty once, also with two replicas.
// An old failure, a failure of another agent, and a failure of a run of
// the duty itself start nothing. A failure deep in a chain stops there.
func TestRunFailed(t *testing.T) {
	f := newFront(t)
	f.duty(t, "watch", duties.Event{Kind: duties.EventRunFailed, Agent: "triage"})
	now := f.clock.Now()
	f.fleet.set(&fw.Run{ID: "R1", Agent: "triage", State: fw.StateFailed, Error: "boom", UpdatedAt: now})
	f.fleet.set(&fw.Run{ID: "OLD", Agent: "triage", State: fw.StateFailed, UpdatedAt: now.Add(-time.Hour)})
	f.fleet.set(&fw.Run{ID: "OTHER", Agent: "other", State: fw.StateFailed, UpdatedAt: now})
	f.fleet.set(&fw.Run{ID: "SELF", Agent: "triage", State: fw.StateFailed, UpdatedAt: now, Metadata: map[string]string{duties.MetaDuty: "watch"}})
	f.fleet.set(&fw.Run{ID: "CHILD", Parent: "R1", Agent: "triage", State: fw.StateFailed, UpdatedAt: now})
	other := f.service(f.a, "front-1")
	f.pass(t)
	other.pass(context.Background())
	f.pass(t)
	h := f.history(t, "watch")
	if len(h) != 1 || h[0].Start != duties.StartEvent || h[0].Run == "" {
		t.Fatalf("the history = %+v", h)
	}
	f.fleet.set(&fw.Run{ID: "DEEP", Agent: "triage", State: fw.StateFailed, UpdatedAt: now, Metadata: map[string]string{duties.MetaChain: "3", duties.MetaDuty: "other"}})
	f.pass(t)
	h = f.history(t, "watch")
	if len(h) != 2 || h[0].State != duties.StateSkipped || !strings.Contains(h[0].Reason, "stops at a depth of 3") {
		t.Fatalf("the history after a deep failure = %+v", h)
	}
}

// A question that waits starts a duty once it waited for after, and only
// while it still waits.
func TestQuestionWaiting(t *testing.T) {
	f := newFront(t)
	f.duty(t, "nudge", duties.Event{Kind: duties.EventQuestionWaiting, After: "1h"})
	now := f.clock.Now()
	f.fleet.set(&fw.Run{ID: "R1", Agent: "triage", State: fw.StateInputRequired, Pending: &fw.Approval{ID: "A1", Tool: "git.push", RequestedAt: now}})
	f.fleet.set(&fw.Run{ID: "R2", Agent: "triage", State: fw.StateInputRequired, Pending: &fw.Approval{ID: "A2", Tool: "git.push", RequestedAt: now}})
	f.pass(t)
	if h := f.history(t, "nudge"); len(h) != 0 {
		t.Fatalf("a question started a duty before it waited an hour: %+v", h)
	}
	f.fleet.set(&fw.Run{ID: "R2", Agent: "triage", State: fw.StateRunning})
	f.clock.Add(time.Hour)
	f.pass(t)
	h := f.history(t, "nudge")
	if len(h) != 1 || h[0].Start != duties.StartEvent {
		t.Fatalf("the history = %+v", h)
	}
}

// The hooks of the wiki, the bridges, the routines, and the duties start
// the duties that wait for their events, with the filters of each duty.
func TestHooks(t *testing.T) {
	f := newFront(t)
	f.duty(t, "pages", duties.Event{Kind: duties.EventPageChanged, Path: "reports/*"})
	f.duty(t, "closed", duties.Event{Kind: duties.EventBridgeClosed})
	f.duty(t, "ready", duties.Event{Kind: duties.EventRoutineReady, Agent: "triage"})
	f.duty(t, "capped", duties.Event{Kind: duties.EventCapReached})
	f.s.routine = func(_ context.Context, agent, name string) (routines.Record, error) {
		return routines.Record{Agent: agent, Routine: fw.Routine{Name: name, Version: 2}, Ready: name == "triage-weekly", UpdatedAt: f.clock.Now()}, nil
	}
	now := f.clock.Now()
	onPage(pages.PageInfo{Path: "notes/a.md", Version: 1, By: "alice", At: now}, false)
	onPage(pages.PageInfo{Path: "reports/a.md", Version: 2, By: "agent:triage", At: now}, false)
	onMessage("B1", bridges.Message{Seq: 4, At: now, Event: bridges.EventOpened})
	onMessage("B1", bridges.Message{Seq: 9, At: now, Event: bridges.EventClosed, Text: "alice closed the bridge"})
	onRoutine("triage", "triage-daily")
	onRoutine("triage", "triage-weekly")
	if err := duties.Of(f.a).Tell(context.Background(), duties.NoticeCap, "pages", "", "The duty spent $2.00 today."); err != nil {
		t.Fatal(err)
	}
	f.s.work.Wait()
	f.pass(t)
	for name, want := range map[string]int{"pages": 1, "closed": 1, "ready": 1, "capped": 1} {
		if h := f.history(t, name); len(h) != want {
			t.Errorf("the duty %s started %d times: %+v", name, len(h), h)
		}
	}
	// A cap of the duty itself does not start it.
	if err := duties.Of(f.a).Tell(context.Background(), duties.NoticeCap, "capped", "", "The duty spent $2.00 today."); err != nil {
		t.Fatal(err)
	}
	f.s.work.Wait()
	f.pass(t)
	if h := f.history(t, "capped"); len(h) != 1 {
		t.Fatalf("a cap of the duty itself started it: %+v", h)
	}
}
