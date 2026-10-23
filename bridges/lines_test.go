package bridges

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/fleet"
	"github.com/bdubs00/featherweight-workbench/internal/bucket"
)

// presenceOf reads the presence of each seat of a bridge as alice.
func presenceOf(t *testing.T, url, id string) map[string]string {
	t.Helper()
	resp, body := do(t, http.MethodGet, url+"/v1/bridges/"+id, "alice", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("read the bridge = %d %s", resp.StatusCode, body)
	}
	var it Item
	decodeJSON(t, body, &it)
	out := map[string]string{}
	for _, m := range it.Members {
		out[m.Handle] = m.Presence
	}
	return out
}

// A seat whose newest run the fleet lost is not working once the view
// settled. The channel says so once per seat, as after a pool replaced the
// Task of the seats. A seat whose run finished first is idle, and a seat
// whose run worked is in error. A view that has not settled yet posts
// nothing, and the seat waits for its run there. A patch with its task
// seats the seat in error again.
func TestSeatThatLostItsRun(t *testing.T) {
	a := newAgent(t, nil)
	s := newStore(t, a, nil)
	v, fm := withFleet(t, a)
	v.state("default/crew-0", fleet.MemberLive, "migrator", "reviewer")
	url := serve(t, a)
	q := captureQueues(t, a, "fw.jobs.migrator")
	it := open(t, url)
	ctx := context.Background()
	v.put(workerRun("r-mig", it.ID, "migrator-1", "migrator", fw.StateRunning))
	rev := workerRun("r-rev", it.ID, "reviewer", "reviewer", fw.StateCompleted)
	fm.setEvents("r-rev", ev(fw.EvRunQueued, nil), ev(fw.EvRunFinished, fw.RunFinishedData{Outcome: fw.OutcomeCompleted, Output: "Looks good."}))
	rev.Seq = 2
	v.put(rev)
	s.poll(ctx)
	if got := presenceOf(t, url, it.ID); got["migrator-1"] != PresenceWorking || got["reviewer"] != PresenceIdle {
		t.Fatalf("presence before = %v", got)
	}

	// A pool replaces the Task crew-0: the fleet holds neither run now.
	v.drop("r-mig")
	v.drop("r-rev")
	s.poll(ctx)
	if got := withEvent(channel(t, s, it.ID), EventLost); len(got) != 0 {
		t.Fatalf("a view that has not settled posted %+v", got)
	}
	if got := presenceOf(t, url, it.ID); got["migrator-1"] != PresenceWaiting {
		t.Fatalf("presence before the view settled = %v", got)
	}
	v.settle(true)
	s.poll(ctx)
	s.poll(ctx)
	lost := withEvent(channel(t, s, it.ID), EventLost)
	if len(lost) != 2 || lost[0].Text != "migrator-1 lost its run r-mig: no worker of the fleet holds it" || lost[1].Text != "reviewer lost its run r-rev: no worker of the fleet holds it" || lost[0].From.Kind != KindSystem {
		t.Fatalf("lost lines = %+v", lost)
	}
	if got := presenceOf(t, url, it.ID); got["migrator-1"] != PresenceError || got["reviewer"] != PresenceIdle {
		t.Fatalf("presence after = %v", got)
	}

	// Seat again: a patch with the same task gives the seat a new run.
	resp, body := do(t, http.MethodPatch, url+"/v1/bridges/"+it.ID+"/members/migrator-1", "alice", `{"task":"Migrate the logrotate module"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("seat again = %d %s", resp.StatusCode, body)
	}
	eventually(t, "the seat runs again", func() bool {
		r, _, ok := q.get("migrator-1")
		return ok && r.IdempotencyKey == "bridge:"+it.ID+":migrator-1:2"
	})
	next := workerRun("r-mig-2", it.ID, "migrator-1", "migrator", fw.StateRunning)
	next.CreatedAt = time.Now().UTC().Add(time.Second)
	v.put(next)
	s.poll(ctx)
	if got := presenceOf(t, url, it.ID); got["migrator-1"] != PresenceWorking {
		t.Fatalf("presence of the seat on its new run = %v", got)
	}
	if got := withEvent(channel(t, s, it.ID), EventLost); len(got) != 2 {
		t.Fatalf("lost lines after the new run = %+v", got)
	}
}

// The watch waits sleepHold before it says that a seat lost its run, so a
// run that another replica claimed a moment before this view got it posts
// nothing, and a seat whose run stays missing gets one line.
func TestLostRunWaitsForTheHold(t *testing.T) {
	a := newAgent(t, nil)
	s := newStore(t, a, nil)
	clock := &testClock{}
	start := time.Date(2026, 10, 22, 4, 0, 0, 0, time.UTC)
	clock.set(start)
	s.sleepHold, s.now = sleepHold, clock.now
	v, _ := withFleet(t, a)
	v.state("default/crew-0", fleet.MemberLive, "migrator")
	v.settle(true)
	rec, err := s.open(context.Background(), OpenSpec{Title: "Held", Members: []SeatSpec{{Handle: "migrator-1", Agent: "migrator", Task: "Migrate"}}}, callers["alice"], nil)
	if err != nil {
		t.Fatal(err)
	}
	r := workerRun("r-mig", rec.ID, "migrator-1", "migrator", fw.StateRunning)
	v.put(r)
	s.poll(context.Background())
	at := func(sec int) {
		clock.set(start.Add(time.Duration(sec) * time.Second))
		s.poll(context.Background())
	}
	// Missing for less than the hold, then back.
	v.drop("r-mig")
	at(1)
	at(20)
	v.put(r)
	at(25)
	v.drop("r-mig")
	at(40)
	at(60)
	if got := withEvent(channel(t, s, rec.ID), EventLost); len(got) != 0 {
		t.Fatalf("a run missing for less than the hold posted %+v", got)
	}
	at(71)
	at(90)
	if got := withEvent(channel(t, s, rec.ID), EventLost); len(got) != 1 {
		t.Fatalf("lost lines = %+v", got)
	}
}

// The line of a decision states who decided what, in one sentence with
// no period: the page words it from the event, and the card keeps the note.
func TestDecidedLines(t *testing.T) {
	cases := []struct {
		name     string
		tool     string
		d        fw.ApprovalDecidedData
		question bool
		answer   string
		want     string
	}{
		{"the token denies a command", "bash", fw.ApprovalDecidedData{Decision: DecisionDeny, Decider: fw.KindToken}, false, "", "token denied migrator-1's command"},
		{"a person allows a file change with a note", "files", fw.ApprovalDecidedData{Decision: DecisionAllow, Decider: "bob", Note: "fine\nby me"}, false, "", "bob allowed migrator-1's file change, with the note: fine by me"},
		{"another tool", "front.pages_write", fw.ApprovalDecidedData{Decision: DecisionAllow, Decider: "alice"}, false, "", "alice allowed migrator-1's call of front.pages_write"},
		{"no tool and no decider", "", fw.ApprovalDecidedData{Decision: DecisionAllow}, false, "", "somebody allowed migrator-1's call"},
		{"an answer", "ask_user", fw.ApprovalDecidedData{Decision: DecisionAllow, Decider: "alice"}, true, "ops", "alice answered migrator-1's question: ops"},
		{"a declined question", "ask_user", fw.ApprovalDecidedData{Decision: DecisionDeny, Decider: "alice", Note: "later"}, true, "", "alice declined migrator-1's question, with the note: later"},
	}
	for _, c := range cases {
		if got := decidedText("migrator-1", c.tool, c.d, c.question, c.answer, c.d.Decider); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}

// A front that starts again knows a finished run of a sleeping Task from
// the index of the runs. The channel holds the end of the run already, so
// the watch reads nothing from the owner, and the Task sleeps on.
func TestRestartReadsNoEndThatTheChannelHolds(t *testing.T) {
	ctx := context.Background()
	rc, cc := bucketConfigs()
	records, channels := bucket.NewMem(rc), bucket.NewMem(cc)
	a1 := newAgent(t, nil)
	s1 := newStoreOn(t, a1, records, channels, nil)
	v1, fm1 := withFleet(t, a1)
	v1.state("default/crew-0", fleet.MemberLive, "migrator")
	rec, err := s1.open(ctx, OpenSpec{Title: "Restarted", Members: []SeatSpec{{Handle: "migrator-1", Agent: "migrator", Task: "Migrate"}}}, callers["alice"], nil)
	if err != nil {
		t.Fatal(err)
	}
	r := workerRun("r-mig", rec.ID, "migrator-1", "migrator", fw.StateCompleted)
	r.Seq = 2
	fm1.setEvents("r-mig", ev(fw.EvRunQueued, nil), ev(fw.EvRunFinished, fw.RunFinishedData{Outcome: fw.OutcomeCompleted, Output: "Done."}))
	v1.put(r)
	s1.poll(ctx)
	before := channel(t, s1, rec.ID)
	if len(fm1.find(http.MethodGet, "/v1/runs/r-mig/events")) != 1 || before[len(before)-1].Text != "Done." {
		t.Fatalf("the first replica did not mirror the end: %+v", before)
	}

	a2 := newAgent(t, nil)
	s2 := newStoreOn(t, a2, records, channels, nil)
	v2, fm2 := withFleet(t, a2)
	v2.state("default/crew-0", fleet.MemberQuiet, "migrator")
	v2.settle(true)
	v2.put(r)
	s2.poll(ctx)
	if n := len(fm2.find(http.MethodGet, "/v1/runs/r-mig/events")); n != 0 {
		t.Fatalf("the new replica read the events of the run %d times", n)
	}
	for _, m := range channel(t, s2, rec.ID)[len(before):] {
		if m.Event != EventAsleep {
			t.Fatalf("the new replica posted %+v", m)
		}
	}
}
