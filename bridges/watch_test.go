package bridges

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/fleet"
	"github.com/bdubs00/featherweight-workbench/internal/bucket"
)

func ev(typ string, data any) fw.Event { return fw.MustEvent("", typ, data) }

// hookLog records what the hooks get.
type hookLog struct {
	mu       sync.Mutex
	messages map[uint64][]Message
	members  map[string][]Member
}

func hooked(t *testing.T) *hookLog {
	h := &hookLog{messages: map[uint64][]Message{}, members: map[string][]Member{}}
	OnMessage(func(_ string, m Message) {
		h.mu.Lock()
		h.messages[m.Seq] = append(h.messages[m.Seq], m)
		h.mu.Unlock()
	})
	OnMember(func(_ string, m Member) {
		h.mu.Lock()
		h.members[m.Handle] = append(h.members[m.Handle], m)
		h.mu.Unlock()
	})
	t.Cleanup(func() {
		hooks.mu.Lock()
		hooks.message, hooks.member = nil, nil
		hooks.mu.Unlock()
	})
	return h
}

func (h *hookLog) message(seq uint64) []Message {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]Message(nil), h.messages[seq]...)
}

func (h *hookLog) member(handle string) []Member {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]Member(nil), h.members[handle]...)
}

// The watch posts one line per fact of each run of a bridge: the start,
// the card of an approval, its decision with the card written again in
// place, the card of a question with its answer, the answer of a finished
// run as a message of the member, a failure, a cancel, and the sleep and
// the wake of a Task. A second look posts nothing again.
func TestSystemLines(t *testing.T) {
	h := hooked(t)
	a := newAgent(t, nil)
	s := newStore(t, a, nil)
	v, fm := withFleet(t, a)
	v.state("default/crew-0", fleet.MemberLive, "migrator", "reviewer")
	url := serve(t, a)
	it := open(t, url)
	ctx := context.Background()
	poll := func() []Message {
		t.Helper()
		before := len(channel(t, s, it.ID))
		s.poll(ctx)
		return channel(t, s, it.ID)[before:]
	}

	mig := workerRun("r-mig", it.ID, "migrator-1", "migrator", fw.StateRunning)
	mig.Activity = &fw.RunActivity{Turn: 2, Since: time.Now().UTC()}
	v.put(mig)
	got := poll()
	if len(got) != 1 || got[0].Event != EventStarted || got[0].Text != "migrator-1 started: Migrate the logrotate module" || got[0].Mirrors == nil || got[0].Mirrors.Type != fw.EvRunQueued {
		t.Fatalf("start = %+v", got)
	}
	if again := poll(); len(again) != 0 {
		t.Fatalf("a second look posts %+v", again)
	}
	if ms := h.member("migrator-1"); len(ms) == 0 || ms[len(ms)-1].Presence != PresenceWorking || ms[len(ms)-1].Activity == nil || ms[len(ms)-1].Activity.Phrase != "Thinking in turn 2" {
		t.Fatalf("member hooks = %+v", ms)
	}

	// An approval: a card with the decision of the result.
	mig.State, mig.Seq, mig.Activity = fw.StateInputRequired, 5, nil
	mig.Pending = &fw.Approval{ID: "ap1", Tool: "bash", Input: json.RawMessage(`{"command":"git push origin migrate/cron"}`), Reason: "rule: ask bash"}
	v.put(mig)
	got = poll()
	if len(got) != 1 || got[0].Event != EventNeedsYou || got[0].Text != "migrator-1 wants to run a command." || got[0].Decision == nil {
		t.Fatalf("card = %+v", got)
	}
	card := got[0]
	d := card.Decision
	if d.Run != "r-mig" || d.ID != "ap1" || d.State != DecisionPending || d.Label != "Allow" || d.Effect != `Runs "git push origin migrate/cron".` || !d.Always || card.Mirrors.Seq != 5 {
		t.Fatalf("decision = %+v", d)
	}
	if ms := h.member("migrator-1"); ms[len(ms)-1].Presence != PresenceNeedsYou {
		t.Fatalf("presence = %s", ms[len(ms)-1].Presence)
	}
	// The decision lands on another surface; the owner holds it.
	fm.setEvents("r-mig",
		ev(fw.EvRunQueued, fw.RunQueuedData{Agent: "migrator"}), ev(fw.EvRunStarted, nil), ev(fw.EvTurnStarted, fw.TurnData{Turn: 1}), ev(fw.EvToolRequested, nil),
		ev(fw.EvApprovalRequested, fw.ApprovalRequestedData{ApprovalID: "ap1", Tool: "bash"}),
		ev(fw.EvApprovalDecided, fw.ApprovalDecidedData{ApprovalID: "ap1", Decision: "allow", Decider: "bob", Note: "go"}))
	mig.State, mig.Seq, mig.Pending = fw.StateRunning, 6, nil
	v.put(mig)
	got = poll()
	if len(got) != 1 || got[0].Event != EventDecided || got[0].Text != "bob allowed the call of migrator-1, with the note: go." || got[0].Mirrors.Seq != 6 {
		t.Fatalf("decided = %+v", got)
	}
	msgs := channel(t, s, it.ID)
	rewritten := msgs[card.Seq-1]
	if rewritten.Seq != card.Seq || rewritten.Decision.State != DecisionAllow || rewritten.Decision.DecidedBy != "bob" || rewritten.Decision.Note != "go" || rewritten.Decision.DecidedAt == nil || rewritten.Decision.Effect == "" {
		t.Fatalf("the card after the decision = %+v", rewritten.Decision)
	}
	if hm := h.message(card.Seq); len(hm) != 2 || hm[1].Decision.State != DecisionAllow {
		t.Fatalf("the hook got the card %d times: %+v", len(hm), hm)
	}

	// A question, answered with a form.
	mig.State, mig.Seq = fw.StateInputRequired, 8
	mig.Pending = &fw.Approval{ID: "ap2", Tool: "ask_user", Input: json.RawMessage(`{"question":"Which audience?"}`), Schema: json.RawMessage(`{"type":"object","properties":{"audience":{"type":"string"}}}`), Reason: "question"}
	v.put(mig)
	got = poll()
	if len(got) != 1 || got[0].Event != EventAsked || got[0].Text != "migrator-1 asks: Which audience?" || !got[0].Decision.Question || got[0].Decision.Label != "Answer" || got[0].Decision.Always {
		t.Fatalf("question = %+v", got)
	}
	evs := append(fm.events["r-mig"],
		ev(fw.EvApprovalRequested, fw.ApprovalRequestedData{ApprovalID: "ap2"}),
		ev(fw.EvApprovalDecided, fw.ApprovalDecidedData{ApprovalID: "ap2", Decision: "allow", Decider: "alice", Content: json.RawMessage(`{"audience":"ops"}`)}))
	fm.setEvents("r-mig", evs...)
	mig.State, mig.Seq, mig.Pending = fw.StateRunning, 9, nil
	v.put(mig)
	got = poll()
	if len(got) != 1 || got[0].Text != "alice answered the question of migrator-1: ops." {
		t.Fatalf("answer = %+v", got)
	}

	// The answer of a finished turn is a message of the member, cut at 4 KB.
	long := strings.Repeat("a", 5000)
	evs = append(fm.events["r-mig"], ev(fw.EvRunFinished, fw.RunFinishedData{Outcome: fw.OutcomeCompleted, Output: long}))
	fm.setEvents("r-mig", evs...)
	finSeq := uint64(len(evs))
	mig.State, mig.Seq = fw.StateCompleted, finSeq
	v.put(mig)
	got = poll()
	if len(got) != 1 || got[0].From != (From{Kind: KindAgent, Handle: "migrator-1", Run: "r-mig"}) || got[0].Event != "" || !got[0].Cut || len(got[0].Text) != entryTextBytes ||
		got[0].Mirrors == nil || *got[0].Mirrors != (Mirror{Run: "r-mig", Seq: finSeq, Type: fw.EvRunFinished}) {
		t.Fatalf("finished = %+v", got)
	}
	if again := poll(); len(again) != 0 {
		t.Fatalf("a second look posts %+v", again)
	}
	// A follow-up turn ends again: a second message.
	evs = append(fm.events["r-mig"], ev(fw.EvMessageReceived, nil), ev(fw.EvRunFinished, fw.RunFinishedData{Outcome: fw.OutcomeCompleted, Output: "Second answer."}))
	fm.setEvents("r-mig", evs...)
	mig.Seq = uint64(len(evs))
	v.put(mig)
	if got = poll(); len(got) != 1 || got[0].Text != "Second answer." {
		t.Fatalf("second answer = %+v", got)
	}

	// A failure and a cancel.
	rev := workerRun("r-rev", it.ID, "reviewer", "reviewer", fw.StateFailed)
	fm.setEvents("r-rev", ev(fw.EvRunQueued, nil), ev(fw.EvRunFinished, fw.RunFinishedData{Outcome: fw.OutcomeFailed, Error: "the model refused"}))
	rev.Seq = 2
	v.put(rev)
	got = poll()
	if len(got) != 2 || got[0].Event != EventStarted || got[1].Event != EventFailed || got[1].Text != "reviewer failed: the model refused" {
		t.Fatalf("failure = %+v", got)
	}
	fm.setEvents("r-rev", ev(fw.EvRunQueued, nil), ev(fw.EvRunFinished, fw.RunFinishedData{Outcome: fw.OutcomeFailed, Error: "the model refused"}), ev(fw.EvMessageReceived, nil), ev(fw.EvRunFinished, fw.RunFinishedData{Outcome: fw.OutcomeCanceled}))
	rev.State, rev.Seq = fw.StateCanceled, 4
	v.put(rev)
	if got = poll(); len(got) != 1 || got[0].Event != EventCanceled || got[0].Text != "The run of reviewer was canceled." {
		t.Fatalf("cancel = %+v", got)
	}

	// The Task sleeps, then wakes.
	v.state("default/crew-0", fleet.MemberQuiet)
	got = poll()
	if len(got) != 2 || got[0].Event != EventAsleep || got[0].Text != "migrator-1 went to sleep. A message wakes its Task." {
		t.Fatalf("asleep = %+v", got)
	}
	if again := poll(); len(again) != 0 {
		t.Fatalf("a second look posts %+v", again)
	}
	if ms := h.member("migrator-1"); ms[len(ms)-1].Presence != PresenceAsleep {
		t.Fatalf("presence = %s", ms[len(ms)-1].Presence)
	}
	v.state("default/crew-0", fleet.MemberLive)
	if got = poll(); len(got) != 2 || got[0].Event != EventStarted || got[0].Text != "migrator-1 woke up." {
		t.Fatalf("awake = %+v", got)
	}
	// The record of the bridge counts the runs of each seat.
	resp, body := do(t, http.MethodGet, url+"/v1/bridges/"+it.ID, "alice", "")
	var got2 Item
	decodeJSON(t, body, &got2)
	if resp.StatusCode != http.StatusOK || got2.Members[0].Runs[0] != "r-mig" || got2.Members[0].Presence != PresenceIdle || got2.Members[1].Presence != PresenceIdle {
		t.Fatalf("record = %d %+v", resp.StatusCode, got2.Members)
	}
}

// A card whose run ended without a decision is written again as denied,
// and the watch reads nothing of a run that the channel already mirrored.
func TestCardOfAnEndedRun(t *testing.T) {
	a := newAgent(t, nil)
	s := newStore(t, a, nil)
	v, fm := withFleet(t, a)
	v.state("default/crew-0", fleet.MemberLive)
	url := serve(t, a)
	it := open(t, url)
	r := workerRun("r-mig", it.ID, "migrator-1", "migrator", fw.StateInputRequired)
	r.Pending = &fw.Approval{ID: "ap1", Tool: "files", Input: json.RawMessage(`{"command":"create","path":"app.env"}`), Reason: "rule: ask destructive"}
	v.put(r)
	s.poll(context.Background())
	cards := withEvent(channel(t, s, it.ID), EventNeedsYou)
	if len(cards) != 1 || cards[0].Decision.Always || cards[0].Decision.Effect != "Changes app.env." || cards[0].Text != "migrator-1 wants to change a file." {
		t.Fatalf("card = %+v", cards)
	}
	fm.setEvents("r-mig", ev(fw.EvRunQueued, nil), ev(fw.EvRunFinished, fw.RunFinishedData{Outcome: fw.OutcomeCanceled}))
	r.State, r.Pending, r.Seq = fw.StateCanceled, nil, 2
	v.put(r)
	s.poll(context.Background())
	msgs := channel(t, s, it.ID)
	card := msgs[cards[0].Seq-1]
	if card.Decision.State != DecisionDeny || card.Decision.Note == "" || len(withEvent(msgs, EventDecided)) != 0 || len(withEvent(msgs, EventCanceled)) != 1 {
		t.Fatalf("after the end: card %+v, lines %+v", card.Decision, msgs)
	}
	reads := len(fm.find(http.MethodGet, "/v1/runs/r-mig/events"))
	s.poll(context.Background())
	if n := len(fm.find(http.MethodGet, "/v1/runs/r-mig/events")); n != reads {
		t.Fatalf("the watch read the run again: %d reads after %d", n, reads)
	}
}

// A card whose run the front cannot read: after decisionTries failed
// reads the card shows the state unknown with a note, it no longer counts
// as unread, and the channel gets one line with the event failed. The
// watch reads the run again at most once per lostEvery, and a read that
// works restores the real state: the decision with its line, or a deny
// for a run that ended without one.
func TestCardOfAnUnreadableRun(t *testing.T) {
	h := hooked(t)
	a := newAgent(t, nil)
	s := newStore(t, a, nil)
	v, fm := withFleet(t, a)
	v.state("default/crew-0", fleet.MemberLive)
	url := serve(t, a)
	it := open(t, url)
	ctx := context.Background()
	mig := workerRun("r-mig", it.ID, "migrator-1", "migrator", fw.StateInputRequired)
	mig.Pending = &fw.Approval{ID: "ap1", Tool: "bash", Input: json.RawMessage(`{"command":"make deploy"}`), Reason: "rule: ask bash"}
	rev := workerRun("r-rev", it.ID, "reviewer", "reviewer", fw.StateInputRequired)
	rev.Pending = &fw.Approval{ID: "ap2", Tool: "bash", Input: json.RawMessage(`{"command":"make check"}`), Reason: "rule: ask bash"}
	v.put(mig)
	v.put(rev)
	s.poll(ctx)
	cards := withEvent(channel(t, s, it.ID), EventNeedsYou)
	if len(cards) != 2 {
		t.Fatalf("cards = %+v", cards)
	}
	seqOf := map[string]uint64{}
	for _, c := range cards {
		seqOf[c.Decision.Run] = c.Seq
	}
	unread := func() int {
		t.Helper()
		_, body := do(t, http.MethodGet, url+"/v1/bridges", "bob", "")
		var out struct{ Bridges []Item }
		decodeJSON(t, body, &out)
		return out.Bridges[0].Unread
	}
	before := unread()

	// The decisions land elsewhere, and the owner answers no read.
	fm.mu.Lock()
	fm.status["/v1/runs/r-mig/events"] = http.StatusServiceUnavailable
	fm.status["/v1/runs/r-rev/events"] = http.StatusServiceUnavailable
	fm.mu.Unlock()
	mig.State, mig.Pending, mig.Seq = fw.StateRunning, nil, 6
	rev.State, rev.Pending, rev.Seq = fw.StateCompleted, nil, 6
	v.put(mig)
	v.put(rev)
	for i := 1; i < decisionTries; i++ {
		s.poll(ctx)
	}
	msgs := channel(t, s, it.ID)
	if c := msgs[seqOf["r-mig"]-1]; c.Decision.State != DecisionPending || len(withEvent(msgs, EventFailed)) != 0 {
		t.Fatalf("after %d failed reads: card %+v, lines %+v", decisionTries-1, c.Decision, withEvent(msgs, EventFailed))
	}
	s.poll(ctx)
	msgs = channel(t, s, it.ID)
	for run, seq := range seqOf {
		c := msgs[seq-1]
		if c.Decision.State != DecisionUnknown || !strings.Contains(c.Decision.Note, "failed to read the run") || c.Decision.DecidedBy != "" || c.Decision.DecidedAt != nil || c.Decision.Effect == "" {
			t.Fatalf("the card of %s after %d failed reads = %+v", run, decisionTries, c.Decision)
		}
		if hm := h.message(seq); len(hm) != 2 || hm[1].Decision.State != DecisionUnknown {
			t.Fatalf("the hook got the card of %s as %+v", run, hm)
		}
	}
	failed := withEvent(msgs, EventFailed)
	if len(failed) != 2 || !strings.Contains(failed[0].Text, "failed to read the run") || len(withEvent(msgs, EventDecided)) != 0 {
		t.Fatalf("lines = %+v", failed)
	}
	if n := unread(); n != before-2 {
		t.Fatalf("unread = %d, want %d: a card that shows unknown waits for nobody", n, before-2)
	}
	// The next look reads the runs once more, and then waits lostEvery.
	s.poll(ctx)
	reads := len(fm.find(http.MethodGet, "/v1/runs/r-mig/events"))
	s.poll(ctx)
	if n := len(fm.find(http.MethodGet, "/v1/runs/r-mig/events")); n != reads || reads != decisionTries+1 {
		t.Fatalf("reads = %d, then %d, want %d", reads, n, decisionTries+1)
	}
	if again := len(channel(t, s, it.ID)); again != len(msgs) {
		t.Fatalf("a look without a read posted %d lines", again-len(msgs))
	}

	// The owner answers again: the migrator was allowed, and the reviewer
	// ended without a decision.
	fm.mu.Lock()
	delete(fm.status, "/v1/runs/r-mig/events")
	delete(fm.status, "/v1/runs/r-rev/events")
	fm.mu.Unlock()
	fm.setEvents("r-mig", ev(fw.EvRunQueued, nil), ev(fw.EvRunStarted, nil), ev(fw.EvTurnStarted, fw.TurnData{Turn: 1}),
		ev(fw.EvApprovalRequested, fw.ApprovalRequestedData{ApprovalID: "ap1", Tool: "bash"}),
		ev(fw.EvApprovalDecided, fw.ApprovalDecidedData{ApprovalID: "ap1", Decision: "allow", Decider: "bob", Note: "ship it"}))
	fm.setEvents("r-rev", ev(fw.EvRunQueued, nil), ev(fw.EvApprovalRequested, fw.ApprovalRequestedData{ApprovalID: "ap2"}), ev(fw.EvRunFinished, fw.RunFinishedData{Outcome: fw.OutcomeCompleted}))
	s.lostEvery = 0
	s.poll(ctx)
	msgs = channel(t, s, it.ID)
	if c := msgs[seqOf["r-mig"]-1]; c.Decision.State != DecisionAllow || c.Decision.DecidedBy != "bob" || c.Decision.Note != "ship it" || c.Decision.DecidedAt == nil {
		t.Fatalf("the card of the migrator after the read = %+v", c.Decision)
	}
	if c := msgs[seqOf["r-rev"]-1]; c.Decision.State != DecisionDeny || c.Decision.Note != "The run no longer waits for this decision." {
		t.Fatalf("the card of the reviewer after the read = %+v", c.Decision)
	}
	if d := withEvent(msgs, EventDecided); len(d) != 1 || d[0].Text != "bob allowed the call of migrator-1, with the note: ship it." {
		t.Fatalf("decided lines = %+v", d)
	}
	if hm := h.message(seqOf["r-mig"]); len(hm) != 3 || hm[2].Decision.State != DecisionAllow {
		t.Fatalf("the hook got the card %+v", hm)
	}
	// A restored card is done: no more reads, and no more lines.
	reads = len(fm.find(http.MethodGet, "/v1/runs/r-mig/events"))
	n := len(msgs)
	s.poll(ctx)
	if got := len(fm.find(http.MethodGet, "/v1/runs/r-mig/events")); got != reads || len(channel(t, s, it.ID)) != n {
		t.Fatalf("after the restore: %d reads after %d, %d entries after %d", got, reads, len(channel(t, s, it.ID)), n)
	}
}

// testClock is a clock that a test moves, which the stores of two
// replicas share.
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) set(t time.Time) {
	c.mu.Lock()
	c.t = t
	c.mu.Unlock()
}

// replicas is two stores over the same buckets, each with the view of its
// own replica of the front, and the bridge of one seat migrator-1, whose
// run works on the Task default/crew-0 of both views.
type replicas struct {
	stores [2]*Store
	views  [2]*fakeView
	clock  *testClock
	bridge string
	start  time.Time
}

func newReplicas(t *testing.T) *replicas {
	t.Helper()
	rc, cc := bucketConfigs()
	records, channels := bucket.NewMem(rc), bucket.NewMem(cc)
	r := &replicas{clock: &testClock{}, start: time.Date(2026, 10, 22, 4, 0, 0, 0, time.UTC)}
	r.clock.set(r.start)
	for i := range r.stores {
		a := newAgent(t, nil)
		s := newStoreOn(t, a, records, channels, nil)
		s.sleepHold, s.now = sleepHold, r.clock.now
		v, _ := withFleet(t, a)
		v.state("default/crew-0", fleet.MemberLive, "migrator")
		r.stores[i], r.views[i] = s, v
	}
	rec, err := r.stores[0].open(context.Background(), OpenSpec{Title: "Flapping", Members: []SeatSpec{{Handle: "migrator-1", Agent: "migrator", Task: "Migrate"}}}, callers["alice"], nil)
	if err != nil {
		t.Fatal(err)
	}
	r.bridge = rec.ID
	for _, v := range r.views {
		v.put(workerRun("r-mig", rec.ID, "migrator-1", "migrator", fw.StateRunning))
	}
	return r
}

// at moves the clock to the second sec after the start, gives each view
// its state of the Task, and lets each replica look once, in turn.
func (r *replicas) at(sec int, states ...string) {
	r.clock.set(r.start.Add(time.Duration(sec) * time.Second))
	for i, v := range r.views {
		v.state("default/crew-0", states[i%len(states)])
	}
	for _, s := range r.stores {
		s.poll(context.Background())
	}
}

// lines returns the lines asleep and woke up of the channel.
func (r *replicas) lines(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, m := range channel(t, r.stores[0], r.bridge) {
		if m.Event == EventAsleep || strings.HasSuffix(m.Text, " woke up.") {
			out = append(out, m.Text)
		}
	}
	return out
}

// Two replicas see the Task of a seat flap between quiet and live, and
// for a while they disagree. The channel says nothing until the Task
// stayed quiet for sleepHold in the record that both share, then one line
// asleep, then one line woke up when the Task runs again, and nothing for
// the flaps after that: one pair of lines between the two replicas.
func TestSleepLinesFollowTheSharedRecord(t *testing.T) {
	r := newReplicas(t)
	quiet, live := fleet.MemberQuiet, fleet.MemberLive
	r.at(0, live)
	if got := withEvent(channel(t, r.stores[0], r.bridge), EventStarted); len(got) != 1 {
		t.Fatalf("starts = %+v", got)
	}
	// The flaps of both views, each shorter than the hold.
	for sec := 1; sec <= 20; sec++ {
		if sec%2 == 1 {
			r.at(sec, quiet)
		} else {
			r.at(sec, live)
		}
	}
	// The replicas disagree for longer than the hold: one sees the Task
	// quiet, the other live.
	for sec := 21; sec <= 70; sec += 5 {
		r.at(sec, quiet, live)
	}
	if got := r.lines(t); len(got) != 0 {
		t.Fatalf("lines before the Task held quiet = %q", got)
	}
	// Both see the Task quiet, for the hold and longer.
	for sec := 71; sec <= 130; sec += 5 {
		r.at(sec, quiet)
		if sec < 71+30 && len(r.lines(t)) != 0 {
			t.Fatalf("a line asleep at %ds, %ds after the Task went quiet", sec, sec-71)
		}
	}
	// It runs again, and flaps once more.
	r.at(131, live)
	for sec := 132; sec <= 150; sec++ {
		if sec%2 == 0 {
			r.at(sec, quiet)
		} else {
			r.at(sec, live)
		}
	}
	r.at(151, live, quiet)
	want := []string{"migrator-1 went to sleep. A message wakes its Task.", "migrator-1 woke up."}
	if got := r.lines(t); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("lines = %q, want %q", got, want)
	}
}

// A Task that sleeps and wakes within sleepHold posts no line, while the
// roster shows the presence of the view at once.
func TestShortSleepPostsNothing(t *testing.T) {
	h := hooked(t)
	r := newReplicas(t)
	r.at(0, fleet.MemberLive)
	r.at(1, fleet.MemberQuiet)
	if ms := h.member("migrator-1"); len(ms) == 0 || ms[len(ms)-1].Presence != PresenceAsleep {
		t.Fatalf("the presence of a quiet Task = %+v", ms)
	}
	r.at(29, fleet.MemberQuiet)
	r.at(30, fleet.MemberLive)
	if ms := h.member("migrator-1"); ms[len(ms)-1].Presence != PresenceWorking {
		t.Fatalf("the presence of a live Task = %s", ms[len(ms)-1].Presence)
	}
	r.at(45, fleet.MemberLive)
	if got := r.lines(t); len(got) != 0 {
		t.Fatalf("a short sleep posted %q", got)
	}
	rec, _, err := r.stores[1].b.get(context.Background(), r.bridge)
	if err != nil {
		t.Fatal(err)
	}
	if sm := rec.Seats["migrator-1"]; sm == nil || sm.Asleep || sm.QuietAt != nil || sm.QuietOn != "" {
		t.Fatalf("the mark of the seat = %+v", sm)
	}
}
