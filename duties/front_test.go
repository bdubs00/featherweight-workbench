package duties

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// putEvent writes a duty that starts on an event straight into the store,
// as a write does once a package serves the events.
func (f *front) putEvent(t *testing.T, name, agent string) {
	t.Helper()
	d := Duty{
		Name: name, Agent: agent, Owner: "alice",
		When:   When{Event: &Event{Kind: EventRunFailed}},
		Do:     Do{Ask: &Ask{Message: "Look at {event}."}},
		Limits: Limits{Budget: &Budget{MaxCostUSD: 0.5}},
	}
	d.normalize("UTC")
	now := f.clock.Now().UTC()
	if err := f.s.st.create(context.Background(), &stored{Duty: d, OwnerKind: fw.KindUser, Since: now, Created: now, Changed: now}); err != nil {
		t.Fatal(err)
	}
}

// The daily cap of an agent counts the duties of that agent, and the cap
// of the fleet counts every duty. At a cap the duty skips, the history
// says why, and the owner hears of it once that day.
func TestCapsAcrossDuties(t *testing.T) {
	f := newFront(t, "duties:\n  caps:\n    perAgentUSD: 1\n    fleetUSD: 1.5\n", nil)
	f.put(t, "alice", daily("first"))
	second := daily("second")
	second.When.Schedule.Times = []string{"10:00"}
	f.put(t, "alice", second)
	f.clock.Set(nine.Add(time.Second))
	f.pass(t)
	f.view.set(f.queue.last(t).ID, fw.StateCompleted, 1.2)
	f.pass(t)
	f.clock.Set(nine.Add(time.Hour + time.Second))
	f.pass(t)
	h := f.history(t, "second")
	if len(h) != 1 || h[0].State != StateSkipped || !strings.Contains(h[0].Reason, "The duties of the agent triage spent $1.20 today, and the daily cap of an agent is $1.00") {
		t.Fatalf("the second duty at the cap of its agent = %+v", h)
	}
	if got := allNotices.kinds(f.a, "second"); !slices.Equal(got, []string{NoticeCap}) {
		t.Fatalf("notices = %v", got)
	}

	g := newFront(t, "duties:\n  caps:\n    fleetUSD: 1\n", nil)
	g.put(t, "alice", daily("first"))
	other := daily("other")
	other.Agent = "private"
	other.When.Schedule.Times = []string{"10:00"}
	g.put(t, "dave", other)
	g.clock.Set(nine.Add(time.Second))
	g.pass(t)
	g.view.set(g.queue.last(t).ID, fw.StateCompleted, 1)
	g.pass(t)
	g.clock.Set(nine.Add(time.Hour + time.Second))
	g.pass(t)
	if h := g.history(t, "other"); len(h) != 1 || h[0].State != StateSkipped || !strings.Contains(h[0].Reason, "The duties of the fleet spent $1.00 today, and the daily cap of the fleet is $1.00") {
		t.Fatalf("a duty of another agent at the cap of the fleet = %+v", h)
	}
	// The next day starts again.
	g.clock.Set(nine.AddDate(0, 0, 1).Add(time.Second))
	g.pass(t)
	if h := g.history(t, "first"); h[0].State != StateQueued {
		t.Fatalf("the next day = %+v", h[0])
	}
}

// A negative cap does not load.
func TestCapsConfig(t *testing.T) {
	cfg := fw.DefaultConfig()
	cfg.Extensions = workbench(t, "duties:\n  caps:\n    perAgentUSD: -1\n")
	if _, err := LoadConfig(cfg); err == nil || !strings.Contains(err.Error(), "workbench.duties.caps.perAgentUSD") {
		t.Fatalf("LoadConfig = %v", err)
	}
}

// A start on an event carries its depth in the chain. A start deeper than
// MaxChain, and a start past MaxEventStartsPerHour in an hour, do not run,
// and the history says why.
func TestFireChain(t *testing.T) {
	f := newFront(t, "", nil)
	f.putEvent(t, "watch", "triage")
	ctx := context.Background()
	out, err := f.s.FireChain(ctx, "watch", json.RawMessage(`{"run":"R1"}`), 2)
	if err != nil || out.Run == "" {
		t.Fatalf("FireChain = %+v, %v", out, err)
	}
	req := f.queue.last(t)
	if req.Metadata[MetaChain] != "2" || req.Metadata[MetaDuty] != "watch" || req.Input != `Look at {"run":"R1"}.` {
		t.Fatalf("the request = %+v", req)
	}
	f.view.set(req.ID, fw.StateCompleted, 0)
	f.pass(t)
	out, err = f.s.FireChain(ctx, "watch", json.RawMessage(`{}`), MaxChain+1)
	if err != nil || !strings.Contains(out.Skipped, "stops at a depth of 3") {
		t.Fatalf("a start too deep = %+v, %v", out, err)
	}
	if h := f.history(t, "watch"); h[0].State != StateSkipped || h[0].Start != StartEvent || !strings.Contains(h[0].Reason, "a chain of duties that events start stops at a depth of 3") {
		t.Fatalf("the history of a start too deep = %+v", h[0])
	}
	if n := len(f.queue.list()); n != 1 {
		t.Fatalf("%d runs went out", n)
	}

	g := newFront(t, "", nil)
	g.putEvent(t, "busy", "triage")
	if _, err := g.s.st.update(ctx, "busy", 0, func(x *stored) error { x.Duty.Limits.Overlap = OverlapAllow; return nil }); err != nil {
		t.Fatal(err)
	}
	for i := range MaxEventStartsPerHour {
		out, err := g.s.FireChain(ctx, "busy", json.RawMessage(`{}`), 1)
		if err != nil || out.Run == "" {
			t.Fatalf("start %d = %+v, %v", i, out, err)
		}
		g.view.set(out.Run, fw.StateCompleted, 0)
		g.pass(t)
		g.clock.Add(time.Second)
	}
	out, err = g.s.FireChain(ctx, "busy", json.RawMessage(`{}`), 1)
	if err != nil || !strings.Contains(out.Skipped, "Events started the duty 60 times in the last hour") {
		t.Fatalf("the start past the hour = %+v, %v", out, err)
	}
	g.clock.Add(time.Hour)
	if out, err := g.s.FireChain(ctx, "busy", json.RawMessage(`{}`), 1); err != nil || out.Run == "" {
		t.Fatalf("a start an hour later = %+v, %v", out, err)
	}
	if _, err := g.s.FireChain(ctx, "nothing", nil, 1); err == nil {
		t.Fatal("a start of no duty worked")
	}
}

// A decision lands on the start of its run, and a read of the duty shows
// it. Tell sends a notice to the people of the list of its kind.
func TestRecordDecisionAndTell(t *testing.T) {
	f := newFront(t, "", nil)
	d := daily("report")
	d.Notify = &Notify{Failure: []string{"owner"}, NeedsYou: []string{"owner", "erin"}}
	f.put(t, "alice", d)
	f.clock.Set(nine.Add(time.Second))
	f.pass(t)
	run := f.queue.last(t).ID
	ctx := context.Background()
	f.clock.Add(2 * time.Hour)
	if err := f.s.RecordDecision(ctx, "report", run, Decided{By: DecidedByManager, Verdict: "allow", Tool: "git.push", Reason: "The push serves\nthe report."}); err != nil {
		t.Fatal(err)
	}
	v, err := f.s.Get(as("carol"), "report")
	if err != nil {
		t.Fatal(err)
	}
	got := v.History[0].Decided
	if len(got) != 1 || got[0].By != "manager" || got[0].Verdict != "allow" || got[0].Tool != "git.push" || got[0].Reason != "The push serves the report." || got[0].At.IsZero() {
		t.Fatalf("decided = %+v", got)
	}
	if err := f.s.RecordDecision(ctx, "report", "NORUN", Decided{By: "alice", Verdict: "deny"}); err == nil {
		t.Fatal("a decision for no start landed")
	}
	if err := f.s.Tell(ctx, NoticeDecided, "report", run, "The server denied the call."); err != nil {
		t.Fatal(err)
	}
	if err := f.s.Tell(ctx, NoticeFailed, "report", run, "It failed."); err != nil {
		t.Fatal(err)
	}
	list := allNotices.of(f.a, "report")
	if len(list) != 2 || list[0].Kind != NoticeDecided || !slices.Equal(list[0].Who, []string{"alice", "erin"}) || list[1].Kind != NoticeFailed || !slices.Equal(list[1].Who, []string{"alice"}) {
		t.Fatalf("notices = %+v", list)
	}
	starts, err := f.s.Starts(ctx, "report", nine)
	if err != nil || len(starts) != 1 || starts[0].Run != run {
		t.Fatalf("Starts = %+v, %v", starts, err)
	}
	if starts, _ := f.s.Starts(ctx, "report", nine.Add(time.Hour)); len(starts) != 1 {
		t.Fatalf("a start that changed after the moment = %+v", starts)
	}
	recs, err := f.s.Records(ctx)
	if err != nil || len(recs) != 1 || recs[0].Duty.Name != "report" || recs[0].OwnerKind != fw.KindUser || recs[0].Since.IsZero() {
		t.Fatalf("Records = %+v, %v", recs, err)
	}
}

// While a package handles whileAway, the engine sends no needs-you notice
// itself.
func TestAwayHandlerTakesNeedsYou(t *testing.T) {
	SetAwayHandler(true)
	t.Cleanup(func() { SetAwayHandler(false) })
	f := newFront(t, "", nil)
	f.put(t, "alice", daily("asks"))
	f.clock.Set(nine.Add(time.Second))
	f.pass(t)
	f.view.set(f.queue.last(t).ID, fw.StateInputRequired, 0)
	f.pass(t)
	if got := allNotices.of(f.a, "asks"); len(got) != 0 {
		t.Fatalf("notices = %+v", got)
	}
	if h := f.history(t, "asks"); h[0].State != string(fw.StateInputRequired) {
		t.Fatalf("history = %+v", h[0])
	}
}

// A notify list names a shared channel as channel:<name>. A write checks
// the name through the hook of the notifications, and a notice carries
// the entry to the notifications, which reach the channel without a
// person.
func TestNotifyChannel(t *testing.T) {
	f := newFront(t, "", nil)
	d := daily("report")
	d.Notify = &Notify{Failure: []string{"owner", " channel: ops-room "}, NeedsYou: []string{"owner"}}
	_, err := f.s.Put(as("alice"), "report", DutyBody{Duty: d}, PutOptions{})
	var pe *ProblemsError
	if !errors.As(err, &pe) || len(pe.Problems) != 1 || !strings.HasPrefix(pe.Problems[0], "notify.failure: Not available yet") {
		t.Fatalf("a shared channel without the notifications = %v", err)
	}
	RegisterChannels(func(_ *fw.Agent, name string) bool { return name == "ops-room" })
	t.Cleanup(func() { RegisterChannels(nil) })
	v := f.put(t, "alice", d)
	if got := v.Notify.Failure; !slices.Equal(got, []string{"owner", "channel:ops-room"}) {
		t.Fatalf("notify.failure = %q", got)
	}
	d.Notify.NeedsYou = []string{"channel:nowhere"}
	_, err = f.s.Put(as("alice"), "report", DutyBody{Duty: d}, PutOptions{})
	if !errors.As(err, &pe) || len(pe.Problems) != 1 || pe.Problems[0] != "notify.needsYou: channel:nowhere is not a shared channel of this server. An operator names it in workbench.notifications.shared." {
		t.Fatalf("an unknown shared channel = %v", err)
	}
	if err := f.s.Tell(context.Background(), NoticeFailed, "report", "", "It failed."); err != nil {
		t.Fatal(err)
	}
	list := allNotices.of(f.a, "report")
	if len(list) != 1 || !slices.Equal(list[0].Who, []string{"alice", "channel:ops-room"}) {
		t.Fatalf("notices = %+v", list)
	}
	if name, ok := ChannelOf(list[0].Who[1]); !ok || name != "ops-room" {
		t.Fatalf("ChannelOf = %q, %v", name, ok)
	}
}
