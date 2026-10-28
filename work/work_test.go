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
// says that it is lost, however long ago it changed. A run that waits for
// a person links its approval.
func TestLostAndWaiting(t *testing.T) {
	lost := run("R-LOST", "reviewer", fw.StateInputRequired, ago(10*24*time.Hour), ago(10*24*time.Hour), nil)
	lost.Pending = &fw.Approval{ID: "ap-old", Tool: "bash"}
	asks := run("R-ASK", "reviewer", fw.StateInputRequired, ago(2*time.Hour), ago(time.Hour), nil)
	asks.Pending = &fw.Approval{ID: "ap-1", Tool: "git.push", RequestedAt: ago(time.Hour)}
	q := run("R-Q", "reviewer", fw.StateInputRequired, ago(3*time.Hour), ago(2*time.Hour), nil)
	q.Pending = &fw.Approval{ID: "ap-2", Schema: json.RawMessage(`{"type":"object"}`), Reason: "Which branch?", RequestedAt: ago(2 * time.Hour)}
	in := Input{Me: "alice", Now: t0, Runs: []Run{{Run: lost, Lost: true}, {Run: asks}, {Run: q}}}
	in.Mine = mineAll(in.Runs...)
	ans := Assemble(in)
	l := cardOf(t, ans, "run:R-LOST")
	if l.Stage != StageStopped || !l.Lost || !strings.Contains(l.Reason, "worker was replaced") || l.Links.Approval != "" || l.Next != NextYou {
		t.Fatalf("the lost run = %+v", l)
	}
	a := cardOf(t, ans, "run:R-ASK")
	if a.Stage != StageNeedsYou || a.Links.Approval != "ap-1" || a.Reason != "Asks to run git.push." || !a.Since.Equal(ago(time.Hour)) {
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
	if want := "3 tasks are open. 2 need you, the oldest for 2 hours. 1 stopped, the oldest for 10 days."; ans.Sentence != want {
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

// The board keeps the cards of the caller unless all is asked, keeps a
// card that did not end however old it is, and drops a card that ended
// before the window.
func TestScopeAndWindow(t *testing.T) {
	mine := run("M1", "helper", fw.StateCompleted, ago(9*24*time.Hour), ago(9*24*time.Hour), nil)
	stale := run("M2", "helper", fw.StateFailed, ago(30*24*time.Hour), ago(30*24*time.Hour), nil)
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
	plain := mk("PLAIN", nil)
	notDone := run("NOTDONE", "writer", fw.StateFailed, ago(time.Hour), ago(time.Hour), nil)
	notDone.Done = &fw.RunDone{State: fw.DoneFailed, Checks: []fw.DoneResult{{Name: "tests", Tool: "bash"}, {Name: "lint", Tool: "bash", Passed: true}}}
	limit := run("LIMIT", "writer", fw.StateFailed, ago(time.Hour), ago(time.Hour), nil)
	limit.Error = "budget exceeded: maxTurns"
	in := Input{Me: "alice", Now: t0, All: true, Runs: []Run{
		{Run: asked}, {Run: upped}, {Run: downed}, {Run: receipt, Work: fleet.Work{Verdict: fleet.VerdictAccepted}},
		{Run: checked}, {Run: plain}, {Run: notDone}, {Run: limit},
	}}
	ans := Assemble(in)
	if c := cardOf(t, ans, "run:ASK"); c.Stage != StageReview || c.Next != NextPerson || c.Reason != "A result to check." || c.Verdict != "" {
		t.Fatalf("a result to check = %+v", c)
	}
	for id, want := range map[string]string{"UP": VerdictAccepted, "DOWN": VerdictSentBack, "RECEIPT": VerdictAccepted, "CHECKED": VerdictChecked, "PLAIN": VerdictUnchecked} {
		if c := cardOf(t, ans, "run:"+id); c.Stage != StageDone || c.Verdict != want {
			t.Errorf("the card %s = %s %q, want done %q", id, c.Stage, c.Verdict, want)
		}
	}
	if c := cardOf(t, ans, "run:NOTDONE"); c.Stage != StageStopped || c.Reason != "Not done: the check tests failed." {
		t.Fatalf("a run that is not done = %+v", c)
	}
	if c := cardOf(t, ans, "run:LIMIT"); c.Stage != StageStopped || c.Reason != "Stopped at its limit of turns." {
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
	if c := cardOf(t, ans, dutyKey("report", other)); c.Stage != StageWorking || c.Reason != "Works." {
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
		"budget exceeded: maxTurns":     "its limit of turns",
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
