package work

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/bridges"
	"github.com/bdubs00/featherweight-workbench/duties"
	"github.com/bdubs00/featherweight-workbench/fleet"
	"github.com/bdubs00/featherweight-workbench/sources"
)

var t0 = time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)

func ago(d time.Duration) time.Time { return t0.Add(-d) }

// run makes a run of alice, created and changed at the given times.
func run(id, agent string, state fw.RunState, created, updated time.Time, md map[string]string) *fw.Run {
	return &fw.Run{ID: id, Agent: agent, State: state, Principal: "alice", Origin: fw.OriginHTTP, CreatedAt: created, UpdatedAt: updated, Metadata: md, Title: "Title of " + id}
}

// mineAll marks every run as a run of the caller.
func mineAll(rs ...Run) map[string]bool {
	m := map[string]bool{}
	for _, r := range rs {
		m[r.ID] = true
	}
	return m
}

// cardOf returns the card id of the answer, or fails.
func cardOf(t *testing.T, ans Answer, id string) Card {
	t.Helper()
	for _, c := range ans.Cards {
		if c.ID == id {
			return c
		}
	}
	ids := []string{}
	for _, c := range ans.Cards {
		ids = append(ids, c.ID+"="+c.Stage)
	}
	t.Fatalf("no card %s in %v", id, ids)
	return Card{}
}

// A bridge is one card, whatever the runs of its seats. A question of a
// seat makes the card need a person, with the text of the question; a
// seat that works makes it work, with the stopped seat in its parts; a
// bridge whose seats ended their turns waits for review; and a closed
// bridge is done.
func TestBridgeCards(t *testing.T) {
	waiting := run("R-ASK", "writer", fw.StateCompleted, ago(3*time.Hour), ago(2*time.Hour), map[string]string{fw.MetaBatch: "bridge:B1"})
	w1 := run("R-W1", "writer", fw.StateRunning, ago(time.Hour), ago(time.Minute), map[string]string{fw.MetaBatch: "bridge:B2"})
	w2 := run("R-W2", "tester", fw.StateFailed, ago(time.Hour), ago(30*time.Minute), map[string]string{fw.MetaBatch: "bridge:B2"})
	w2.Usage.CostUSD, w1.Usage.CostUSD = 0.25, 0.5
	last := ago(4 * 24 * time.Hour)
	closed := ago(time.Hour)
	in := Input{
		Me: "alice", Now: t0,
		Runs: []Run{{Run: waiting}, {Run: w1}, {Run: w2}},
		Bridges: []bridges.Item{
			{Bridge: bridges.Bridge{ID: "B1", Title: "Notes", Operator: "alice", State: bridges.StateOpen, CreatedAt: ago(5 * time.Hour), Members: []bridges.Member{
				{Handle: "scribe", Agent: "writer", Runs: []string{"R-ASK"}, Presence: bridges.PresenceNeedsYou, Question: 7},
				{Handle: "lint", Agent: "tester", Presence: bridges.PresenceIdle},
			}}},
			{Bridge: bridges.Bridge{ID: "B2", Title: "Build", Operator: "alice", State: bridges.StateOpen, CreatedAt: ago(2 * time.Hour), Members: []bridges.Member{
				{Handle: "dev", Agent: "writer", Runs: []string{"R-W1"}, Presence: bridges.PresenceWorking},
				{Handle: "qa", Agent: "tester", Runs: []string{"R-W2"}, Presence: bridges.PresenceError},
			}}},
			{Bridge: bridges.Bridge{ID: "B3", Title: "Old", Operator: "alice", State: bridges.StateOpen, CreatedAt: ago(9 * 24 * time.Hour), Members: []bridges.Member{
				{Handle: "a", Agent: "writer", Presence: bridges.PresenceIdle, Gone: true},
				{Handle: "b", Agent: "writer", Presence: bridges.PresenceAsleep},
			}}, LastAt: &last},
			{Bridge: bridges.Bridge{ID: "B4", Title: "Shipped", Operator: "alice", State: bridges.StateClosed, CreatedAt: ago(3 * time.Hour), ClosedAt: &closed}},
			{Bridge: bridges.Bridge{ID: "B5", Title: "Stalled", Operator: "alice", State: bridges.StateOpen, CreatedAt: ago(48 * time.Hour), Members: []bridges.Member{
				{Handle: "m", Agent: "migrator", Presence: bridges.PresenceNotStarted},
			}}},
		},
		Waiting: []bridges.Waiting{{Bridge: "B1", Handle: "scribe", Question: 7, Text: "Who reads these notes?", At: ago(90 * time.Minute)}},
	}
	in.Mine = mineAll(in.Runs...)
	ans := Assemble(in)

	b1 := cardOf(t, ans, "bridge:B1")
	if b1.Stage != StageNeedsYou || b1.Next != NextYou || !strings.Contains(b1.Reason, "scribe asks: Who reads these notes?") || !b1.Since.Equal(ago(90*time.Minute)) || b1.Links.Run != "R-ASK" || b1.Kind != KindBridge {
		t.Fatalf("the bridge with a question = %+v", b1)
	}
	if b1.Parts.NeedsYou != 1 || b1.Parts.Review != 1 || b1.Parts.Total != 2 {
		t.Fatalf("the parts of B1 = %+v", b1.Parts)
	}
	b2 := cardOf(t, ans, "bridge:B2")
	if b2.Stage != StageWorking || b2.Next != NextAgent || b2.Parts.Stopped != 1 || b2.Parts.Working != 1 || b2.Reason != "1 working, 1 stopped." || b2.CostUSD != 0.75 {
		t.Fatalf("the bridge that works = %+v", b2)
	}
	if strings.Join(b2.Agents, ",") != "tester,writer" || strings.Join(b2.Links.Runs, ",") != "R-W2,R-W1" && strings.Join(b2.Links.Runs, ",") != "R-W1,R-W2" {
		t.Fatalf("the agents and the runs of B2 = %v %v", b2.Agents, b2.Links.Runs)
	}
	b3 := cardOf(t, ans, "bridge:B3")
	if b3.Stage != StageReview || !b3.Since.Equal(last) || b3.Next != NextYou {
		t.Fatalf("an open bridge whose seats ended = %+v", b3)
	}
	b4 := cardOf(t, ans, "bridge:B4")
	if b4.Stage != StageDone || b4.Ended == nil || !b4.Ended.Equal(closed) || b4.Next != "" {
		t.Fatalf("a closed bridge = %+v", b4)
	}
	b5 := cardOf(t, ans, "bridge:B5")
	if b5.Stage != StageStopped || b5.Reason != "m never started." {
		t.Fatalf("a bridge whose seat never started = %+v", b5)
	}
}

// The runs of a conversation are one card: a run that continues an
// earlier one, a branch, and a child run. The newest run speaks for the
// card, and an answer that nobody checked is done and unchecked.
func TestConversation(t *testing.T) {
	first := run("R1", "helper", fw.StateCompleted, ago(5*time.Hour), ago(5*time.Hour), nil)
	first.Output = "first answer"
	again := run("R2", "helper", fw.StateRunning, ago(time.Hour), ago(time.Minute), map[string]string{fleet.MetaContinues: "R1"})
	child := run("R3", "helper", fw.StateFailed, ago(50*time.Minute), ago(40*time.Minute), nil)
	child.Parent = "R2"
	branch := run("R4", "helper", fw.StateCompleted, ago(30*time.Minute), ago(20*time.Minute), map[string]string{fw.MetaForkOf: "R1"})
	alone := run("R5", "helper", fw.StateCompleted, ago(2*time.Hour), ago(2*time.Hour), nil)
	alone.Output = "an answer"
	in := Input{Me: "alice", Now: t0, Runs: []Run{{Run: again}, {Run: child}, {Run: first}, {Run: branch}, {Run: alone}}}
	in.Mine = mineAll(in.Runs...)
	ans := Assemble(in)
	if len(ans.Cards) != 2 {
		t.Fatalf("the cards = %+v", ans.Cards)
	}
	c := cardOf(t, ans, "run:R1")
	// The branch is the newest top-level run, and it ended with nothing.
	if c.Stage != StageDone || c.Parts.Total != 4 || !c.Opened.Equal(ago(5*time.Hour)) || c.Kind != KindConversation || c.From.Kind != "person" || c.From.Via != "page" {
		t.Fatalf("the conversation = %+v", c)
	}
	if a := cardOf(t, ans, "run:R5"); a.Stage != StageDone || a.Verdict != VerdictUnchecked {
		t.Fatalf("an answered conversation = %+v", a)
	}
	// Without the branch, the run that continues it works.
	in.Runs = in.Runs[:3]
	if c := cardOf(t, Assemble(in), "run:R1"); c.Stage != StageWorking || c.Links.Run != "R2" {
		t.Fatalf("the conversation that works = %+v", c)
	}
}

// A run whose worker the pool replaced before it ended stops, and the card
// says that it is lost and that the agent that held it is gone, never the
// worker. A run that waits for a person links its approval.
func TestLostAndWaiting(t *testing.T) {
	lost := run("R-LOST", "reviewer", fw.StateInputRequired, ago(5*24*time.Hour), ago(5*24*time.Hour), nil)
	lost.Pending = &fw.Approval{ID: "ap-old", Tool: "bash"}
	asks := run("R-ASK", "reviewer", fw.StateInputRequired, ago(2*time.Hour), ago(time.Hour), nil)
	asks.Pending = &fw.Approval{ID: "ap-1", Tool: "git.push", RequestedAt: ago(time.Hour)}
	q := run("R-Q", "reviewer", fw.StateInputRequired, ago(3*time.Hour), ago(2*time.Hour), nil)
	q.Pending = &fw.Approval{ID: "ap-2", Schema: json.RawMessage(`{"type":"object"}`), Reason: "Which branch?", RequestedAt: ago(2 * time.Hour)}
	in := Input{Me: "alice", Now: t0, Runs: []Run{{Run: lost, Lost: true}, {Run: asks}, {Run: q}}}
	in.Mine = mineAll(in.Runs...)
	ans := Assemble(in)
	l := cardOf(t, ans, "run:R-LOST")
	if l.Stage != StageStopped || !l.Lost || l.Reason != "The agent that held it is gone." || l.Links.Approval != "" || l.Next != NextYou {
		t.Fatalf("the lost run = %+v", l)
	}
	a := cardOf(t, ans, "run:R-ASK")
	if a.Stage != StageNeedsYou || a.Links.Approval != "ap-1" || a.Reason != "Asks before it calls push in git." || !a.Since.Equal(ago(time.Hour)) {
		t.Fatalf("the approval = %+v", a)
	}
	if c := cardOf(t, ans, "run:R-Q"); c.Reason != "Asks: Which branch?" || c.Links.Approval != "ap-2" {
		t.Fatalf("the question = %+v", c)
	}
	// The lanes count the cards, the cards of the caller, and the oldest
	// wait; the cards that wait longest come first in their stage.
	if ans.Lanes[2].Stage != StageNeedsYou || ans.Lanes[2].Count != 2 || ans.Lanes[2].Yours != 2 || !ans.Lanes[2].Oldest.Equal(ago(2*time.Hour)) {
		t.Fatalf("the lane needs-you = %+v", ans.Lanes[2])
	}
	if ans.Cards[0].ID != "run:R-Q" || ans.Cards[1].ID != "run:R-ASK" || ans.Cards[2].ID != "run:R-LOST" {
		t.Fatalf("the order = %s %s %s", ans.Cards[0].ID, ans.Cards[1].ID, ans.Cards[2].ID)
	}
	if want := "3 tasks are open. 2 need you, the oldest for 2 hours. 1 stopped, the oldest for 5 days."; ans.Sentence != want {
		t.Fatalf("the sentence = %q", ans.Sentence)
	}
}

// A start of a duty is one card: the runs of its attempts join it, its
// run speaks for it while it is on the queue, and the manager moves an
// approval of a duty that lets the manager decide. A start that found no
// work is no card.
func TestDutyCards(t *testing.T) {
	slot := ago(2 * time.Hour)
	md := map[string]string{duties.MetaDuty: "report", duties.MetaSlot: slot.Format(time.RFC3339Nano), fw.MetaBatch: duties.BatchOf("report")}
	first := run("R-A1", "triage", fw.StateFailed, slot, slot.Add(time.Minute), md)
	second := run("R-A2", "triage", fw.StateInputRequired, slot.Add(5*time.Minute), slot.Add(10*time.Minute), md)
	second.Principal = "duty:report"
	second.Pending = &fw.Approval{ID: "ap-9", Tool: "git.push", RequestedAt: slot.Add(10 * time.Minute)}
	d := duties.Duty{Name: "report", Agent: "triage", Owner: "alice", WhileAway: &duties.WhileAway{Decider: "manager", Timeout: "2h", Then: "deny"}}
	other := duties.Duty{Name: "watch", Agent: "triage", Owner: "bob"}
	in := Input{
		Me: "alice", Now: t0,
		Runs:   []Run{{Run: first}, {Run: second}},
		Duties: []duties.Duty{d, other},
		Starts: []Start{
			{Duty: d, Entry: duties.RunEntry{At: slot, Slot: slot, Run: "R-A2", State: duties.StateQueued, Start: duties.StartSchedule, Attempt: 2, CostUSD: 0.3}},
			{Duty: d, Entry: duties.RunEntry{At: ago(time.Hour), Slot: ago(time.Hour), State: duties.StateQuiet, Start: duties.StartSchedule}},
			{Duty: other, Entry: duties.RunEntry{At: ago(time.Hour), Slot: ago(time.Hour), State: duties.StateFailed, Reason: "The queue did not take the run", Start: duties.StartEvent}},
		},
	}
	next := t0.Add(3 * time.Hour)
	in.Next = map[string]time.Time{"report": next, "watch": next}
	ans := Assemble(in)
	if len(ans.Cards) != 2 {
		t.Fatalf("the cards of alice = %+v", ans.Cards)
	}
	// The start that comes next waits in the first lane, with since at
	// its time, after the cards that wait now. It counts in no wait.
	up := ans.Cards[0]
	if up.ID != dutyKey("report", next) || up.Stage != StageWaiting || !up.Since.Equal(next) || up.From.Via != duties.StartSchedule || up.Title != "report" {
		t.Fatalf("the next start = %+v", up)
	}
	if ans.Lanes[0].Count != 1 || !ans.Lanes[0].Oldest.IsZero() || strings.Contains(ans.Sentence, "coming up, the oldest") {
		t.Fatalf("the lane of the next starts = %+v, %q", ans.Lanes[0], ans.Sentence)
	}
	c := ans.Cards[1]
	if c.ID != dutyKey("report", slot) || c.Kind != KindDuty || c.Stage != StageNeedsYou || c.Next != NextManager || c.Links.Approval != "ap-9" || c.Links.Duty != "report" {
		t.Fatalf("the start = %+v", c)
	}
	if c.From != (From{Kind: "duty", Name: "report", Via: duties.StartSchedule}) || c.CostUSD != 0.3 || len(c.Links.Runs) != 2 {
		t.Fatalf("the start = %+v", c)
	}
	in.All = true
	all := Assemble(in)
	cardOf(t, all, dutyKey("watch", next))
	f := cardOf(t, all, dutyKey("watch", ago(time.Hour)))
	if f.Stage != StageStopped || f.Reason != "The queue did not take the run" || f.Next != NextPerson || f.From.Via != duties.StartEvent {
		t.Fatalf("the failed start of bob = %+v", f)
	}
}

// The items of a job are one card. It works while an item works, counts
// the items that wait in the queue, and keeps its stopped items in its
// parts. An event of a source is a card of its own.
func TestJobAndEvent(t *testing.T) {
	var runs []Run
	for i, st := range []fw.RunState{fw.StateRunning, fw.StateFailed, fw.StateCompleted, fw.StateCompleted} {
		r := run("J"+string(rune('0'+i)), "migrator", st, ago(time.Hour), ago(time.Duration(i)*time.Minute), map[string]string{fw.MetaBatch: "migrate-42"})
		r.Origin = fw.OriginBus
		if st == fw.StateCompleted {
			r.Output = "done"
		}
		runs = append(runs, Run{Run: r})
	}
	ev := run("E1", "triage", fw.StateCompleted, ago(time.Hour), ago(time.Hour), map[string]string{sources.MetaSource: "tickets", fw.MetaBatch: "tickets"})
	ev.Principal, ev.Output = "source:tickets", "Triaged."
	runs = append(runs, Run{Run: ev})
	in := Input{Me: "alice", Now: t0, Runs: runs, Queued: map[string]int{"migrate-42": 6}, All: true}
	ans := Assemble(in)
	j := cardOf(t, ans, "job:alice/migrate-42")
	if j.Kind != KindJob || j.Stage != StageWorking || j.Parts.Waiting != 6 || j.Parts.Stopped != 1 || j.Parts.Done != 2 || j.Parts.Total != 10 || j.From.Via != "queue" {
		t.Fatalf("the job = %+v", j)
	}
	if j.Reason != "6 coming up, 1 working, 1 stopped, 2 done." {
		t.Fatalf("the reason of the job = %q", j.Reason)
	}
	e := cardOf(t, ans, "run:E1")
	if e.Kind != KindEvent || e.Stage != StageDone || e.Verdict != VerdictUnchecked || e.From != (From{Kind: "app", Name: "tickets", Via: "call"}) || e.Links.Source != "tickets" {
		t.Fatalf("the event = %+v", e)
	}
}

// The runs of the after-work review are one card that reads Review of
// what it learned, and it names Know-how, not a person knowhow:review.
func TestReviewCard(t *testing.T) {
	var runs []Run
	for i, agent := range []string{"writer", "clerk"} {
		r := run("K"+string(rune('0'+i)), "review", fw.StateCompleted, ago(time.Hour), ago(time.Minute), map[string]string{fw.MetaBatch: "knowhow:review", fw.MetaTitle: "After-work review of " + agent})
		r.Origin, r.Principal, r.Output = fw.OriginBus, "knowhow:review", "Kept one note."
		runs = append(runs, Run{Run: r})
	}
	ans := Assemble(Input{Me: "alice", Now: t0, Runs: runs, All: true})
	c := cardOf(t, ans, "job:knowhow:review/knowhow:review")
	if c.Title != "Review of what it learned" || c.From != (From{Kind: "app", Name: "Know-how", Via: "queue"}) || c.Parts.Total != 2 {
		t.Fatalf("the card of the review = %+v", c)
	}
}

// The board keeps the cards of the caller unless all is asked, keeps a
// card that waits for a person however old it is, and drops a card that
// ended before the window.
func TestScopeAndWindow(t *testing.T) {
	mine := run("M1", "helper", fw.StateCompleted, ago(9*24*time.Hour), ago(9*24*time.Hour), nil)
	stale := run("M2", "helper", fw.StateInputRequired, ago(30*24*time.Hour), ago(30*24*time.Hour), nil)
	theirs := run("T1", "helper", fw.StateRunning, ago(time.Hour), ago(time.Minute), nil)
	theirs.Principal = "bob"
	in := Input{Me: "alice", Now: t0, Runs: []Run{{Run: mine}, {Run: stale}, {Run: theirs}}, Mine: map[string]bool{"M1": true, "M2": true}}
	ans := Assemble(in)
	if len(ans.Cards) != 1 || ans.Cards[0].ID != "run:M2" || ans.Days != DefaultDays {
		t.Fatalf("the cards of alice = %+v", ans.Cards)
	}
	in.All = true
	all := Assemble(in)
	if len(all.Cards) != 2 || cardOf(t, all, "run:T1").From.Name != "bob" {
		t.Fatalf("every card = %+v", all.Cards)
	}
	in.Window = 10 * 24 * time.Hour
	if wide := Assemble(in); len(wide.Cards) != 3 || wide.Days != 10 {
		t.Fatalf("a window of ten days = %+v", wide.Cards)
	}
	if empty := Assemble(Input{Me: "carol", Now: t0}); empty.Sentence != "No work in the last 7 days." || len(empty.Lanes) != len(Stages) {
		t.Fatalf("an empty board = %+v", empty)
	}
}

// A result goes to review only when its owner asked to check it, and it
// leaves review on the first reaction. A done card says who checked it:
// a person, the checks of the definition of done, or nobody. A run whose
// checks failed stops as not done, and a run at a limit says the limit.
func TestReviewAndVerdicts(t *testing.T) {
	mk := func(id string, md map[string]string) *fw.Run {
		r := run(id, "writer", fw.StateCompleted, ago(3*time.Hour), ago(2*time.Hour), md)
		r.Output = "The draft."
		return r
	}
	review := map[string]string{"workbench.review": "1"}
	asked := mk("ASK", review)
	upped := mk("UP", review)
	upped.Reactions = map[string]int{fw.ReactionUp: 1}
	downed := mk("DOWN", nil)
	downed.Reactions = map[string]int{fw.ReactionDown: 1}
	receipt := mk("RECEIPT", review)
	checked := mk("CHECKED", nil)
	checked.Done = &fw.RunDone{State: fw.DoneChecked}
	// A weak pass, such as a check that only reads a file that was there
	// before the run, shows nothing about the work, so nobody checked it.
	weak := mk("WEAK", nil)
	weak.Done = &fw.RunDone{State: fw.DoneChecked, Weak: true}
	plain := mk("PLAIN", nil)
	// A heart keeps a result on the wiki, and it is no verdict.
	hearted := mk("HEART", nil)
	hearted.Reactions = map[string]int{fw.ReactionHeart: 1}
	notDone := run("NOTDONE", "writer", fw.StateFailed, ago(time.Hour), ago(time.Hour), nil)
	notDone.Done = &fw.RunDone{State: fw.DoneFailed, Checks: []fw.DoneResult{{Name: "tests", Tool: "bash"}, {Name: "lint", Tool: "bash", Passed: true}}}
	limit := run("LIMIT", "writer", fw.StateFailed, ago(time.Hour), ago(time.Hour), nil)
	limit.Error = "budget exceeded: maxTurns"
	in := Input{Me: "alice", Now: t0, All: true, Runs: []Run{
		{Run: asked}, {Run: upped}, {Run: downed}, {Run: receipt, Work: fleet.Work{Verdict: fleet.VerdictAccepted}},
		{Run: checked}, {Run: weak}, {Run: plain}, {Run: hearted}, {Run: notDone}, {Run: limit},
	}}
	ans := Assemble(in)
	if c := cardOf(t, ans, "run:ASK"); c.Stage != StageReview || c.Next != NextPerson || c.Reason != "A result to check." || c.Verdict != "" {
		t.Fatalf("a result to check = %+v", c)
	}
	for id, want := range map[string]string{"UP": VerdictAccepted, "DOWN": VerdictSentBack, "RECEIPT": VerdictAccepted, "CHECKED": VerdictChecked, "WEAK": VerdictUnchecked, "PLAIN": VerdictUnchecked, "HEART": VerdictUnchecked} {
		if c := cardOf(t, ans, "run:"+id); c.Stage != StageDone || c.Verdict != want {
			t.Errorf("the card %s = %s %q, want done %q", id, c.Stage, c.Verdict, want)
		}
	}
	if c := cardOf(t, ans, "run:NOTDONE"); c.Stage != StageStopped || c.Reason != "Not done: the check tests failed." {
		t.Fatalf("a run that is not done = %+v", c)
	}
	if c := cardOf(t, ans, "run:LIMIT"); c.Stage != StageStopped || c.Reason != "Stopped at its limit of steps." {
		t.Fatalf("a run at a limit = %+v", c)
	}
	if !fleet.InReview(asked) || fleet.InReview(upped) || fleet.InReview(plain) {
		t.Fatal("fleet.InReview")
	}
}

// A start of a duty takes the state of its run while the run works, such
// as running or input-required. Such a start works or needs a person; it
// never stopped. Without its run in the view it still works.
func TestDutyStartThatWorks(t *testing.T) {
	slot := ago(time.Minute)
	md := map[string]string{duties.MetaDuty: "report", duties.MetaSlot: slot.Format(time.RFC3339Nano), fw.MetaBatch: duties.BatchOf("report")}
	working := run("R-W", "triage", fw.StateRunning, slot, slot, md)
	d := duties.Duty{Name: "report", Agent: "triage", Owner: "alice"}
	other := slot.Add(-time.Hour)
	in := Input{
		Me: "alice", Now: t0, Runs: []Run{{Run: working}}, Duties: []duties.Duty{d},
		Starts: []Start{
			{Duty: d, Entry: duties.RunEntry{At: slot, Slot: slot, Run: "R-W", State: string(fw.StateRunning), Start: duties.StartNow, Attempt: 1}},
			{Duty: d, Entry: duties.RunEntry{At: other, Slot: other, Run: "R-GONE", State: string(fw.StateRunning), Start: duties.StartSchedule, Attempt: 1}},
			{Duty: d, Entry: duties.RunEntry{At: other.Add(-time.Hour), Slot: other.Add(-time.Hour), Run: "R-ASKS", State: string(fw.StateInputRequired), Start: duties.StartSchedule, Attempt: 1}},
		},
	}
	ans := Assemble(in)
	if c := cardOf(t, ans, dutyKey("report", slot)); c.Stage != StageWorking {
		t.Fatalf("the start whose run works = %+v", c)
	}
	if c := cardOf(t, ans, dutyKey("report", other)); c.Stage != StageWorking || c.Reason != "The agent works on it." {
		t.Fatalf("the start whose run the view does not hold = %+v", c)
	}
	if c := cardOf(t, ans, dutyKey("report", other.Add(-time.Hour))); c.Stage != StageNeedsYou {
		t.Fatalf("the start whose run asks = %+v", c)
	}
}

// A card says why a run stopped at its budget in words, never with the
// name of a key, and shows a question without the marks of Markdown.
func TestReasonWords(t *testing.T) {
	for err, want := range map[string]string{
		"budget exceeded: maxTurns":     "its limit of steps",
		"budget exceeded: maxToolCalls": "its limit of tool calls",
		"budget exceeded: maxCostUSD":   "its limit of cost",
		"budget exceeded: deadline":     "its time limit",
		"budget exceeded":               "a limit of its budget",
	} {
		if got := limitWords(err); got != want {
			t.Errorf("limitWords(%q) = %q, want %q", err, got, want)
		}
	}
	q := "**Answer to @operator, on the `ca-certificates` package**\n- **What it is:** see [the docs](https://example.com/x) for node_exporter"
	want := "Answer to @operator, on the ca-certificates package What it is: see the docs for node_exporter"
	if got := oneLine(plainText(q), 200); got != want {
		t.Errorf("plainText = %q, want %q", got, want)
	}
}

// A run that ended keeps the run of the archive when the view knows it
// from the index of the runs alone, so its card keeps its title and its
// verdict after a start of the front. A run that works takes the view.
func TestMergeRunsKeepsTheArchiveOfAnEnd(t *testing.T) {
	full := &fw.Run{ID: "R1", Agent: "checker", State: fw.StateCompleted, Seq: 45, Title: "Write a note", Output: "READY",
		Done: &fw.RunDone{State: fw.DoneChecked}, Metadata: map[string]string{"fw.title": "Write a note"}}
	thin := &fw.Run{ID: "R1", Agent: "checker", State: fw.StateCompleted, Seq: 45}
	workingArch := &fw.Run{ID: "R2", Agent: "checker", State: fw.StateRunning, Seq: 3, Title: "Old"}
	working := &fw.Run{ID: "R2", Agent: "checker", State: fw.StateRunning, Seq: 9, Activity: &fw.RunActivity{Tool: "bash"}}
	got := mergeRuns([]fleet.WorkRun{{Run: full}, {Run: workingArch, Lost: true}}, []*fw.Run{thin, working, {ID: "R3", State: fw.StateQueued}})
	if len(got) != 3 {
		t.Fatalf("the runs = %d, want 3", len(got))
	}
	if got[0].Run != full || got[0].Done == nil || got[0].Title != "Write a note" {
		t.Errorf("an ended run = %+v, want the run of the archive", got[0].Run)
	}
	if got[1].Run != working || got[1].Lost {
		t.Errorf("a run that works = %+v lost %v, want the run of the view", got[1].Run, got[1].Lost)
	}
	if got[2].ID != "R3" {
		t.Errorf("a run of the view alone = %+v", got[2].Run)
	}
}

// A stopped card that a person let go is done with the verdict let-go,
// and its reason names the person, or you for the caller. A let-go older
// than the window drops out with the card. A card that moved on since its
// let-go keeps its stage.
func TestLetGoCards(t *testing.T) {
	lost := run("R-LOST", "helper", fw.StateRunning, ago(10*24*time.Hour), ago(10*24*time.Hour), nil)
	failed := run("R-FAIL", "helper", fw.StateFailed, ago(2*time.Hour), ago(time.Hour), nil)
	failed.Error = "budget exceeded: maxTurns"
	old := run("R-OLD", "helper", fw.StateFailed, ago(20*24*time.Hour), ago(20*24*time.Hour), nil)
	again := run("R-AGAIN", "helper", fw.StateRunning, ago(time.Hour), ago(time.Minute), nil)
	in := Input{Me: "alice", Now: t0, Runs: []Run{{Run: lost, Lost: true}, {Run: failed}, {Run: old, Lost: true}, {Run: again}},
		LetGo: map[string]LetGo{
			"run:R-LOST":  {Card: "run:R-LOST", By: "alice", Name: "Alice", At: ago(time.Hour)},
			"run:R-FAIL":  {Card: "run:R-FAIL", By: "bob", Name: "Bob", At: ago(30 * time.Minute)},
			"run:R-OLD":   {Card: "run:R-OLD", By: "bob", At: ago(9 * 24 * time.Hour)},
			"run:R-AGAIN": {Card: "run:R-AGAIN", By: "fd1e8a14-ac12-5a00-b4d6-bb6e50d15c10", At: ago(2 * time.Hour)},
		}}
	in.Mine = mineAll(in.Runs...)
	ans := Assemble(in)
	l := cardOf(t, ans, "run:R-LOST")
	if l.Stage != StageDone || l.Verdict != VerdictLetGo || l.Reason != "Let go by you." || l.Next != "" || l.Ended == nil || !l.Ended.Equal(ago(time.Hour)) || !l.Lost {
		t.Fatalf("a lost card that alice let go = %+v", l)
	}
	if f := cardOf(t, ans, "run:R-FAIL"); f.Stage != StageDone || f.Reason != "Let go by Bob." {
		t.Fatalf("a failed card that bob let go = %+v", f)
	}
	for _, c := range ans.Cards {
		if c.ID == "run:R-OLD" {
			t.Fatalf("a let-go older than the window shows %+v", c)
		}
	}
	if a := cardOf(t, ans, "run:R-AGAIN"); a.Stage != StageWorking {
		t.Fatalf("a card that works again = %+v", a)
	}
	if got := letGoName(LetGo{By: "fd1e8a14-ac12-5a00-b4d6-bb6e50d15c10"}, "alice"); got != "a person" {
		t.Fatalf("the name of a person with an id = %q", got)
	}
	if ans.Lanes[5].Count != 2 || ans.Lanes[3].Count != 0 {
		t.Fatalf("the lanes = %+v", ans.Lanes)
	}
	// A card that was let go is in Done, but it is no done task, as in
	// Results (plan A13).
	if ans.Done != 0 || !strings.HasSuffix(ans.Sentence, "0 done in the last 7 days, and 2 canceled or let go.") {
		t.Fatalf("the done tasks = %d, the sentence = %q", ans.Done, ans.Sentence)
	}
}

// A card that stopped a week ago and that nobody moved lets itself go: it
// is done with the verdict let-go and one line in Done, from the day it
// let itself go, and it leaves the window a week later. A card that
// stopped less than a week ago stays stopped. A card that waits for a
// person never lets itself go. (plan A8)
func TestStoppedLetsItselfGo(t *testing.T) {
	week := StoppedFor
	stale := run("R-STALE", "helper", fw.StateFailed, ago(week+2*24*time.Hour), ago(week+24*time.Hour), nil)
	fresh := run("R-FRESH", "helper", fw.StateFailed, ago(6*24*time.Hour), ago(6*24*time.Hour), nil)
	gone := run("R-GONE", "helper", fw.StateFailed, ago(3*week), ago(3*week), nil)
	waits := run("R-WAITS", "helper", fw.StateInputRequired, ago(3*week), ago(3*week), nil)
	in := Input{Me: "alice", Now: t0, Runs: []Run{{Run: stale}, {Run: fresh}, {Run: gone}, {Run: waits}}}
	in.Mine = mineAll(in.Runs...)
	ans := Assemble(in)
	c := cardOf(t, ans, "run:R-STALE")
	if c.Stage != StageDone || c.Verdict != VerdictLetGo || c.Next != "" || c.Ended == nil || !c.Ended.Equal(ago(24*time.Hour)) {
		t.Fatalf("a card stopped for 8 days = %+v", c)
	}
	if c.Reason != "It stopped 7 days ago and nobody moved it, so it let itself go." {
		t.Fatalf("the line in Done = %q", c.Reason)
	}
	if f := cardOf(t, ans, "run:R-FRESH"); f.Stage != StageStopped {
		t.Fatalf("a card stopped for 6 days = %+v", f)
	}
	if w := cardOf(t, ans, "run:R-WAITS"); w.Stage != StageNeedsYou {
		t.Fatalf("a card that waits for a person = %+v", w)
	}
	for _, x := range ans.Cards {
		if x.ID == "run:R-GONE" {
			t.Fatalf("a card that let itself go two weeks ago shows %+v", x)
		}
	}
	if ans.Lanes[3].Count != 1 || ans.Lanes[5].Count != 1 {
		t.Fatalf("the lanes = %+v", ans.Lanes)
	}
}

// One count of the work that needs a person (plan A7): the cards of Needs
// you and of To check whose next move is the caller's, and the notes of
// Know-how that wait for the caller. A card of another person, a stopped
// card, and a card that works do not count.
func TestOneCount(t *testing.T) {
	review := map[string]string{fleet.MetaReview: "1"}
	asks := run("R-ASK", "writer", fw.StateInputRequired, ago(2*time.Hour), ago(time.Hour), nil)
	asks.Pending = &fw.Approval{ID: "ap-1", Tool: "git.push"}
	mine := run("R-MINE", "writer", fw.StateCompleted, ago(time.Hour), ago(time.Minute), review)
	theirs := run("R-THEIRS", "writer", fw.StateCompleted, ago(time.Hour), ago(time.Minute), review)
	theirs.Principal = "bob"
	stopped := run("R-STOP", "writer", fw.StateFailed, ago(time.Hour), ago(time.Hour), nil)
	works := run("R-WORK", "writer", fw.StateRunning, ago(time.Hour), ago(time.Minute), nil)
	in := Input{Me: "alice", Now: t0, All: true, Notes: 2, Runs: []Run{{Run: asks}, {Run: mine}, {Run: theirs}, {Run: stopped}, {Run: works}},
		Mine: map[string]bool{"R-ASK": true, "R-MINE": true, "R-STOP": true, "R-WORK": true}}
	ans := Assemble(in)
	if ans.Waiting != (Waiting{Count: 4, NeedsYou: 1, ToCheck: 1, Notes: 2}) {
		t.Fatalf("the one count = %+v", ans.Waiting)
	}
	// The board of the caller alone gives the same count.
	in.All = false
	if mineOnly := Assemble(in); mineOnly.Waiting != ans.Waiting {
		t.Fatalf("the one count of the board of the caller = %+v, of every card = %+v", mineOnly.Waiting, ans.Waiting)
	}
}

// To check shows the newest result first, so a fresh result never sinks
// under the old results of teams (plan A8). Needs you keeps the longest
// wait first.
func TestToCheckNewestFirst(t *testing.T) {
	review := map[string]string{fleet.MetaReview: "1"}
	older := run("R-OLD", "writer", fw.StateCompleted, ago(3*24*time.Hour), ago(3*24*time.Hour), review)
	newer := run("R-NEW", "writer", fw.StateCompleted, ago(time.Hour), ago(time.Minute), review)
	middle := run("R-MID", "writer", fw.StateCompleted, ago(24*time.Hour), ago(24*time.Hour), review)
	in := Input{Me: "alice", Now: t0, Runs: []Run{{Run: older}, {Run: newer}, {Run: middle}}}
	in.Mine = mineAll(in.Runs...)
	var order []string
	for _, c := range Assemble(in).Cards {
		order = append(order, c.ID)
	}
	if strings.Join(order, " ") != "run:R-NEW run:R-MID run:R-OLD" {
		t.Fatalf("the order of To check = %v", order)
	}
}

// A start of a duty that tells only of what it finds and whose run found
// something waits in To check with the words of what it found. The card
// is the move of a person of notify.result. A start that found nothing is
// done.
func TestFoundCard(t *testing.T) {
	slot := ago(time.Hour)
	md := map[string]string{duties.MetaDuty: "drift", duties.MetaSlot: slot.Format(time.RFC3339Nano), fleet.MetaFound: "1"}
	found := run("R-F", "ops", fw.StateCompleted, slot, ago(50*time.Minute), md)
	found.Principal, found.Output = "duty:drift", "**Found:** web-3 drifts from its role\nThe diff is in the wiki."
	slot2 := ago(25 * time.Hour)
	md2 := map[string]string{duties.MetaDuty: "drift", duties.MetaSlot: slot2.Format(time.RFC3339Nano), fleet.MetaFound: "1"}
	nothing := run("R-N", "ops", fw.StateCompleted, slot2, slot2, md2)
	nothing.Principal, nothing.Output = "duty:drift", "Found: nothing"
	d := duties.Duty{Name: "drift", Title: "Drift check", Agent: "ops", Owner: "alice", Notify: &duties.Notify{Result: []string{"bob"}, ResultWhen: duties.ResultFound}}
	in := Input{Me: "bob", Now: t0, Runs: []Run{{Run: found}, {Run: nothing}}, Duties: []duties.Duty{d},
		Starts: []Start{{Duty: d, Entry: duties.RunEntry{Slot: slot, At: slot, Run: "R-F", State: duties.StateCompleted}}, {Duty: d, Entry: duties.RunEntry{Slot: slot2, At: slot2, Run: "R-N", State: duties.StateCompleted}}}}
	ans := Assemble(in)
	c := cardOf(t, ans, dutyKey("drift", slot))
	if c.Stage != StageReview || c.Reason != "Found: web-3 drifts from its role" || c.Next != NextYou {
		t.Fatalf("a found result = %+v", c)
	}
	if n := cardOf(t, ans, dutyKey("drift", slot2)); n.Stage != StageDone || n.Verdict != VerdictUnchecked {
		t.Fatalf("a start that found nothing = %+v", n)
	}
}

// A card names the person who asked by a display name beside the subject
// (people.Display): the name that the front keeps for the subject, else
// the name that a bridge keeps in readable form, else the subject when it
// is no id. A subject that the issuer made as an id, with no name known,
// reads as "A person", and the static token as "Admin token" (live walk
// M3).
func TestFromDisplay(t *testing.T) {
	const erin = "fd1e8a14-ac12-5a00-b4d6-bb6e50d15c10"
	mine := run("R-A", "helper", fw.StateCompleted, ago(time.Hour), ago(time.Hour), nil)
	known := run("R-E", "helper", fw.StateCompleted, ago(time.Hour), ago(time.Hour), nil)
	known.Principal = erin
	stranger := run("R-X", "helper", fw.StateCompleted, ago(time.Hour), ago(time.Hour), nil)
	stranger.Principal = "0b6e1f7c-4d1a-4a8e-9a63-2f0d61c5a9e1"
	admin := run("R-T", "helper", fw.StateCompleted, ago(time.Hour), ago(time.Hour), nil)
	admin.Principal = "token"
	in := Input{Me: "alice", All: true, Now: t0, Runs: []Run{{Run: mine}, {Run: known}, {Run: stranger}, {Run: admin}},
		Names: map[string]string{erin: "Erin"},
		Bridges: []bridges.Item{
			{Bridge: bridges.Bridge{ID: "B1", Title: "Miners", Operator: erin, OperatorName: "erin@example.com", State: bridges.StateClosed, CreatedAt: ago(time.Hour)}},
			{Bridge: bridges.Bridge{ID: "B2", Title: "Backups", Operator: "0c1d2e3f-1b7d-4e2a-9c1f-2a3b4c5d6e7f", OperatorName: "frank.miller@example.com", State: bridges.StateClosed, CreatedAt: ago(time.Hour)}},
		}}
	ans := Assemble(in)
	for id, want := range map[string]string{"run:R-A": "alice", "run:R-E": "Erin", "run:R-X": "A person", "run:R-T": "Admin token", "bridge:B1": "Erin", "bridge:B2": "Frank Miller"} {
		if c := cardOf(t, ans, id); c.From.Display != want || c.From.Kind != "person" {
			t.Errorf("the from of %s = %+v, want the display %q", id, c.From, want)
		}
	}
}

// A question of a seat whose run no worker holds still waits for the
// person who asked for the bridge: an answer seats the member again, so
// the card needs the person, with the question as its summary (live walk
// m1). A decision of a seat whose run the archive holds as lost stops the
// card, which is lost: nobody can decide it any more.
func TestLostQuestion(t *testing.T) {
	lostRun := run("R-L", "writer", fw.StateInputRequired, ago(4*24*time.Hour), ago(4*24*time.Hour), map[string]string{fw.MetaBatch: "bridge:B2"})
	in := Input{Me: "alice", Now: t0, Runs: []Run{{Run: lostRun, Lost: true}},
		Bridges: []bridges.Item{
			{Bridge: bridges.Bridge{ID: "B1", Title: "node_exporter, second try", Operator: "alice", State: bridges.StateOpen, CreatedAt: ago(4 * 24 * time.Hour), Members: []bridges.Member{
				{Handle: "ops", Agent: "ops", Runs: []string{"R-GONE"}, Presence: bridges.PresenceNeedsYou, Question: 9, Gone: true},
				{Handle: "rev", Agent: "reviewer", Presence: bridges.PresenceIdle},
			}}},
			{Bridge: bridges.Bridge{ID: "B2", Title: "Roles", Operator: "alice", State: bridges.StateOpen, CreatedAt: ago(4 * 24 * time.Hour), Members: []bridges.Member{
				{Handle: "writer", Agent: "writer", Runs: []string{"R-L"}, Presence: bridges.PresenceNeedsYou},
			}}},
		},
		Waiting: []bridges.Waiting{{Bridge: "B1", Handle: "ops", Question: 9, Text: "Which port?", At: ago(4 * 24 * time.Hour), Gone: true}},
	}
	in.Mine = mineAll(in.Runs...)
	ans := Assemble(in)
	b1 := cardOf(t, ans, "bridge:B1")
	if b1.Stage != StageNeedsYou || b1.Next != NextYou || b1.Lost || b1.Reason != "ops asks: Which port?" || b1.Summary != "Which port?" || b1.Parts.NeedsYou != 1 {
		t.Fatalf("a bridge whose seat asked and went = %+v", b1)
	}
	if b2 := cardOf(t, ans, "bridge:B2"); b2.Stage != StageStopped || !b2.Lost || b2.Links.Approval != "" {
		t.Fatalf("a bridge whose seat run is lost = %+v", b2)
	}
	if ans.Lanes[2].Count != 1 || ans.Waiting.NeedsYou != 1 {
		t.Fatalf("the lane needs you = %+v, waiting %+v", ans.Lanes[2], ans.Waiting)
	}
}

// The reasons of the cards say what happens in plain words: no card says
// "Works." alone or names the seats or the turns of a team, and a tool
// reads as what its call does.
func TestPlainReasons(t *testing.T) {
	files := run("R-F", "writer", fw.StateRunning, ago(time.Hour), ago(time.Minute), nil)
	files.Activity = &fw.RunActivity{Turn: 2, Tool: "files"}
	thinks := run("R-T", "writer", fw.StateRunning, ago(time.Hour), ago(time.Minute), nil)
	wiki := run("R-W", "writer", fw.StateInputRequired, ago(time.Hour), ago(time.Minute), nil)
	wiki.Pending = &fw.Approval{ID: "ap", Tool: "front.pages_write"}
	limit := run("R-L", "writer", fw.StateFailed, ago(time.Hour), ago(time.Minute), nil)
	limit.Error = "budget exceeded: maxCallsPerTurn"
	in := Input{Me: "alice", Now: t0, Runs: []Run{{Run: files}, {Run: thinks}, {Run: wiki}, {Run: limit}},
		Bridges: []bridges.Item{
			{Bridge: bridges.Bridge{ID: "B1", Title: "Email", Operator: "alice", State: bridges.StateOpen, CreatedAt: ago(time.Hour), Members: []bridges.Member{
				{Handle: "writer", Agent: "writer", Presence: bridges.PresenceIdle}, {Handle: "reviewer", Agent: "reviewer", Presence: bridges.PresenceAsleep}}}},
			{Bridge: bridges.Bridge{ID: "B2", Title: "Docs", Operator: "alice", State: bridges.StateOpen, CreatedAt: ago(time.Hour), Members: []bridges.Member{
				{Handle: "writer", Agent: "writer", Presence: bridges.PresenceWorking}}}},
			{Bridge: bridges.Bridge{ID: "B3", Title: "Empty", Operator: "alice", State: bridges.StateOpen, CreatedAt: ago(time.Hour)}},
		}}
	in.Mine = mineAll(in.Runs...)
	ans := Assemble(in)
	want := map[string]string{
		"run:R-F":   "Works with files.",
		"run:R-T":   "The agent works on it.",
		"run:R-W":   "Asks before it writes a page of the wiki.",
		"run:R-L":   "Stopped at its limit of tool calls in one step.",
		"bridge:B1": "The agents finished, and the result waits for a check.",
		"bridge:B2": "writer works on its part.",
		"bridge:B3": "The channel has no agent.",
	}
	for id, w := range want {
		if c := cardOf(t, ans, id); c.Reason != w {
			t.Errorf("the reason of %s = %q, want %q", id, c.Reason, w)
		}
	}
	for _, c := range ans.Cards {
		l := strings.ToLower(c.Reason)
		if c.Reason == "Works." || strings.Contains(l, "seat") || strings.Contains(l, "turn") || strings.Contains(l, "front.") {
			t.Errorf("the reason of %s = %q", c.ID, c.Reason)
		}
	}
	for tool, w := range map[string]string{"bash": "runs a shell command", "front.pages_write": "writes a page of the wiki", "front.pages_trash": "uses the wiki",
		"servicenow.update_incident": "calls update incident in servicenow", "front.duty_put": "saves a job", "front.duties_list": "uses a job", "": "uses a tool"} {
		if got := toolWords(tool); got != w {
			t.Errorf("toolWords(%q) = %q, want %q", tool, got, w)
		}
	}
	if got := strings.Join(asksOf([]string{"front.pages_write", "bash"}, nil), "|"); got != "Before it writes a page of the wiki|Before it runs a shell command" {
		t.Errorf("the asks = %q", got)
	}
}

// A card carries a summary in one line: the first line of the result to
// check, what a decision asks, and for a team whose agents finished the
// first line of the newest message of an agent in its channel.
func TestCardSummary(t *testing.T) {
	result := run("R-R", "writer", fw.StateCompleted, ago(time.Hour), ago(time.Minute), map[string]string{fleet.MetaReview: "1"})
	result.Output = "## The notes of 2.4\n\nThey are in the wiki."
	asks := run("R-A", "writer", fw.StateInputRequired, ago(time.Hour), ago(time.Minute), nil)
	asks.Pending = &fw.Approval{ID: "ap", Tool: "bash", Reason: "Restart sshd on both hosts."}
	in := Input{Me: "alice", Now: t0, Runs: []Run{{Run: result}, {Run: asks}},
		Bridges: []bridges.Item{{Bridge: bridges.Bridge{ID: "B1", Title: "Backup policy", Operator: "alice", State: bridges.StateOpen, CreatedAt: ago(time.Hour), Members: []bridges.Member{
			{Handle: "writer", Agent: "writer", Presence: bridges.PresenceIdle}}}, Summary: "The policy is in backups/policy.md."}},
	}
	in.Mine = mineAll(in.Runs...)
	ans := Assemble(in)
	if c := cardOf(t, ans, "run:R-R"); c.Stage != StageReview || c.Summary != "The notes of 2.4" {
		t.Fatalf("the result = %+v", c)
	}
	if c := cardOf(t, ans, "run:R-A"); c.Stage != StageNeedsYou || c.Summary != "Asks before it runs a shell command: Restart sshd on both hosts." {
		t.Fatalf("the decision = %+v", c)
	}
	if c := cardOf(t, ans, "bridge:B1"); c.Stage != StageReview || c.Summary != "The policy is in backups/policy.md." {
		t.Fatalf("the team = %+v", c)
	}
}

// After Needs work, the card of the run works on the note at once, also
// while the view still holds the end before it: the facts of the archive
// saw the note reach the agent (tester finding 2).
func TestReworkingCard(t *testing.T) {
	r := run("R-N", "writer", fw.StateCompleted, ago(time.Hour), ago(time.Minute), map[string]string{fleet.MetaReview: "1"})
	r.Output = "The first answer."
	w := fleet.Work{Facts: true, Ends: 1, Reviewed: 1, Verdict: fleet.VerdictRejected, LastVerdict: fleet.VerdictRejected, Note: "Shorter.", Reworking: true}
	in := Input{Me: "alice", Now: t0, Runs: []Run{{Run: r, Work: w}}}
	in.Mine = mineAll(in.Runs...)
	if c := cardOf(t, Assemble(in), "run:R-N"); c.Stage != StageWorking || c.Reason != "The agent works on your note." || c.Next != NextAgent {
		t.Fatalf("the card after Needs work = %+v", c)
	}
	// The view holds the end before the note, and the archive the events
	// after it: the run of the archive wins.
	live := *r
	live.Seq = 3
	arch := *r
	arch.Seq, arch.State = 5, fw.StateRunning
	got := mergeRuns([]fleet.WorkRun{{Run: &arch, Work: w}}, []*fw.Run{&live})
	if len(got) != 1 || got[0].Run.State != fw.StateRunning {
		t.Fatalf("the merged run = %+v", got)
	}
}
