package fleet

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/internal/bucket"
)

func ev(seq uint64, typ string, at time.Time, data any) fw.Event {
	raw, _ := json.Marshal(data)
	return fw.Event{Type: typ, Seq: seq, Time: at, Subject: "R1", Data: raw}
}

// The facts of the work count what people did on a run: a decision of a
// person with its wait, a decision of the front as handled, a message of a
// person after the end as rework, and the first thumb as the verdict. A
// message that the front relays from another agent and a note of the
// kernel do not count.
func TestWorkApply(t *testing.T) {
	t0 := time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC)
	at := func(m int) time.Time { return t0.Add(time.Duration(m) * time.Minute) }
	events := []fw.Event{
		ev(1, fw.EvRunQueued, at(0), fw.RunQueuedData{Agent: "triage"}),
		ev(2, fw.EvRunStarted, at(0), map[string]any{}),
		ev(3, fw.EvApprovalRequested, at(1), fw.ApprovalRequestedData{ApprovalID: "a1", Tool: "deploy"}),
		ev(4, fw.EvApprovalDecided, at(31), fw.ApprovalDecidedData{ApprovalID: "a1", Decision: "allow", Decider: "alice", DeciderKind: fw.ActorPerson}),
		ev(5, fw.EvApprovalRequested, at(32), fw.ApprovalRequestedData{ApprovalID: "a2", Tool: "deploy"}),
		ev(6, fw.EvApprovalDecided, at(40), fw.ApprovalDecidedData{ApprovalID: "a2", Decision: "deny", Decider: "token", DeciderKind: fw.KindToken}),
		ev(7, fw.EvMessageReceived, at(41), fw.MessageReceivedData{Role: fw.RoleUser, Origin: "http", By: "token", From: &fw.MessageFrom{Kind: fw.FromAgent, Name: "reviewer"}}),
		ev(8, fw.EvMessageReceived, at(42), fw.MessageReceivedData{Role: fw.RoleSystem, Origin: "kernel", Note: "budget"}),
		ev(9, fw.EvMessageReceived, at(43), fw.MessageReceivedData{Role: fw.RoleUser, Origin: "http", By: "alice"}),
		ev(10, fw.EvRunFinished, at(50), fw.RunFinishedData{Outcome: "completed"}),
		ev(11, fw.EvMessageReceived, at(60), fw.MessageReceivedData{Role: fw.RoleUser, Origin: "http", By: "token", From: &fw.MessageFrom{Kind: fw.FromPerson, Name: "alice"}}),
		ev(12, fw.EvRunFinished, at(65), fw.RunFinishedData{Outcome: "completed"}),
		ev(13, fw.EvRunReacted, at(70), fw.ReactionData{Seq: 12, Reaction: fw.ReactionDown, By: "alice"}),
		ev(14, fw.EvRunReacted, at(71), fw.ReactionData{Seq: 12, Reaction: fw.ReactionUp, By: "bob"}),
	}
	w := WorkOf(events)
	if !w.Facts || !w.Queued.Equal(at(0)) || !w.FirstEnd.Equal(at(50)) || w.FirstOutcome != "completed" {
		t.Fatalf("the times = %+v", w)
	}
	if w.Acts != 3 || w.Rework != 1 || w.Handled != 1 {
		t.Fatalf("acts %d, rework %d, handled %d; want 3, 1, 1", w.Acts, w.Rework, w.Handled)
	}
	if w.Waits != 1 || w.WaitMS != (30*time.Minute).Milliseconds() || !w.Asked.IsZero() {
		t.Fatalf("waits %d of %d ms, asked %v; want 1 of 30 min", w.Waits, w.WaitMS, w.Asked)
	}
	if w.Done != "" {
		t.Fatalf("a run without a definition of done has done %q", w.Done)
	}
	if w.Verdict != VerdictRejected {
		t.Fatalf("the verdict = %q, want the first thumb, %s", w.Verdict, VerdictRejected)
	}

	// A run that waits keeps the time of its request, and a receipt
	// gives the verdict when no thumb came first.
	w = WorkOf([]fw.Event{
		ev(1, fw.EvRunQueued, at(0), fw.RunQueuedData{Agent: "triage"}),
		ev(2, fw.EvRunFinished, at(5), fw.RunFinishedData{Outcome: "completed"}),
		ev(3, fw.EvRunAnnotated, at(6), fw.QualityAnnotatedData{Quality: &fw.QualityObservation{ID: "ci:1", Completion: 2, Verification: "satisfied"}}),
		ev(4, fw.EvApprovalRequested, at(7), fw.ApprovalRequestedData{ApprovalID: "a3"}),
	})
	if w.Verdict != VerdictAccepted || !w.Asked.Equal(at(7)) || w.Acts != 0 {
		t.Fatalf("a receipt and an open request = %+v", w)
	}

	// The newest end tells whether the definition of done passed.
	w = WorkOf([]fw.Event{
		ev(1, fw.EvRunQueued, at(0), fw.RunQueuedData{Agent: "fixer"}),
		ev(2, fw.EvRunFinished, at(5), fw.RunFinishedData{Outcome: "failed", Done: fw.DoneFailed}),
		ev(3, fw.EvMessageReceived, at(6), fw.MessageReceivedData{Role: fw.RoleUser, Origin: "http", By: "alice"}),
		ev(4, fw.EvRunFinished, at(9), fw.RunFinishedData{Outcome: "completed", Done: fw.DoneChecked}),
	})
	if w.Done != fw.DoneChecked || w.FirstOutcome != "failed" || w.Rework != 1 {
		t.Fatalf("a run that its checks ended and a follow-up fixed = %+v", w)
	}
	if !Checked(nil, w) {
		t.Fatalf("a run whose checks passed is not checked: %+v", w)
	}

	// A weak pass of done shows nothing about the work of the run, so the
	// run is not checked, though its done says checked.
	w = WorkOf([]fw.Event{
		ev(1, fw.EvRunQueued, at(0), fw.RunQueuedData{Agent: "fixer"}),
		ev(2, fw.EvRunFinished, at(5), fw.RunFinishedData{Outcome: "completed", Done: fw.DoneChecked, DoneWeak: true}),
	})
	if w.Done != fw.DoneChecked || !w.DoneWeak || Checked(nil, w) {
		t.Fatalf("a weak pass = %+v, checked %v", w, Checked(nil, w))
	}
	weak := &fw.Run{Done: &fw.RunDone{State: fw.DoneChecked, Weak: true}}
	if Checked(weak, Work{}) || !Checked(&fw.Run{Done: &fw.RunDone{State: fw.DoneChecked}}, Work{}) {
		t.Fatal("Checked of the run object does not read Weak")
	}

	// A heart keeps a result on the wiki. It is no verdict, so the result
	// still waits for a check, and a thumb after it gives the verdict.
	w = WorkOf([]fw.Event{
		ev(1, fw.EvRunQueued, at(0), fw.RunQueuedData{Agent: "writer"}),
		ev(2, fw.EvRunFinished, at(5), fw.RunFinishedData{Outcome: "completed"}),
		ev(3, fw.EvRunReacted, at(6), fw.ReactionData{Seq: 2, Reaction: fw.ReactionHeart, By: "alice"}),
	})
	if w.Verdict != "" || w.LastVerdict != "" || w.Reviewed != 0 {
		t.Fatalf("a heart gave a verdict: %+v", w)
	}
	hearted := &fw.Run{State: fw.StateCompleted, Metadata: map[string]string{MetaReview: "1"}, Reactions: map[string]int{fw.ReactionHeart: 1}}
	if !ToCheck(hearted, w) || !InReview(hearted) {
		t.Fatal("a result with a heart alone no longer waits for a check")
	}

	// A record that never saw the start has no facts.
	if w := WorkOf([]fw.Event{ev(9, fw.EvRunFinished, at(9), fw.RunFinishedData{Outcome: "failed"})}); w.Facts {
		t.Fatalf("a record without the start has facts: %+v", w)
	}
}

// The backfill gives a summary that an older server wrote the facts of its
// events, once. A summary whose run lost its start in the stream keeps a
// mark, so the next start does not read it again, and a summary that
// already has its facts stays as it is.
func TestArchiveBackfill(t *testing.T) {
	ctx := context.Background()
	defer func(p time.Duration) { backfillPause = p }(backfillPause)
	backfillPause = 0
	a := newArchive(bucket.NewMem(ArchiveBucket), nil, quietLogger())
	start := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
	old := conversation(t, "01OLD", "ax:team/manager-0", start)
	old = append(old[:5], archiveEvent(t, "01OLD", "ax:team/manager-0", fw.EvApprovalRequested, 6, start.Add(5*time.Second), fw.ApprovalRequestedData{ApprovalID: "a1", Tool: "bash"}),
		archiveEvent(t, "01OLD", "ax:team/manager-0", fw.EvApprovalDecided, 7, start.Add(65*time.Second), fw.ApprovalDecidedData{ApprovalID: "a1", Decision: "allow", Decider: "alice", DeciderKind: fw.ActorPerson}),
		archiveEvent(t, "01OLD", "ax:team/manager-0", fw.EvRunFinished, 8, start.Add(70*time.Second), fw.RunFinishedData{Outcome: fw.OutcomeCompleted, Output: "Done.", Done: fw.DoneChecked}))
	fresh := conversation(t, "01NEW", "ax:team/manager-0", start)
	for _, e := range append(append([]fw.Event{}, old...), fresh...) {
		if err := a.add(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	want := a.summaries(ctx)["01OLD"].Work
	// A server before the facts wrote no work into the summaries.
	for _, id := range []string{"01OLD", "01TRIMMED"} {
		if id == "01TRIMMED" {
			if err := a.add(ctx, archiveEvent(t, id, "ax:team/manager-0", fw.EvRunFinished, 9, start, fw.RunFinishedData{Outcome: fw.OutcomeCompleted})); err != nil {
				t.Fatal(err)
			}
		}
		if err := a.modify(ctx, id, func(r *archivedRun) bool {
			r.Work = Work{}
			r.Queued = true
			return true
		}); err != nil {
			t.Fatal(err)
		}
	}
	if got := a.backfill(ctx); got != 1 {
		t.Fatalf("the backfill gave %d runs their facts, want 1", got)
	}
	sums := a.summaries(ctx)
	got := sums["01OLD"].Work
	if !got.Backfilled || !got.Facts || got.Acts != 1 || got.Waits != 1 || got.WaitMS != 60000 || got.Done != fw.DoneChecked {
		t.Fatalf("the backfilled facts = %+v", got)
	}
	got.Backfilled = false
	if got != want {
		t.Fatalf("the backfilled facts = %+v, want the facts of the stream %+v", got, want)
	}
	if w := sums["01TRIMMED"].Work; !w.Backfilled || w.Facts {
		t.Fatalf("a run without its start = %+v, want the mark and no facts", w)
	}
	if w := sums["01NEW"].Work; w.Backfilled || !w.Facts {
		t.Fatalf("a run with its facts = %+v", w)
	}
	if got := a.backfill(ctx); got != 0 {
		t.Fatalf("a second backfill gave %d runs their facts", got)
	}
}

// A replica that finds the facts of a backfill of another replica in the
// bucket takes them into its copy. refresh never reads a finished run
// again, so without that the replica answered the value of the work as if
// the run had no facts, until its next start.
func TestArchiveBackfillOfAnotherReplica(t *testing.T) {
	ctx := context.Background()
	defer func(p time.Duration) { backfillPause = p }(backfillPause)
	backfillPause = 0
	b := bucket.NewMem(ArchiveBucket)
	one := newArchive(b, nil, quietLogger())
	start := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
	for _, e := range conversation(t, "01OLD", "ax:team/manager-0", start) {
		if err := one.add(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	if err := one.modify(ctx, "01OLD", func(r *archivedRun) bool {
		r.Work = Work{}
		r.Queued = true
		return true
	}); err != nil {
		t.Fatal(err)
	}
	two := newArchive(b, nil, quietLogger())
	two.refresh(ctx)
	if w := two.summaries(ctx)["01OLD"].Work; w.Facts {
		t.Fatalf("the second replica starts with facts: %+v", w)
	}
	if got := one.backfill(ctx); got != 1 {
		t.Fatalf("the first replica gave %d runs their facts, want 1", got)
	}
	if got := two.backfill(ctx); got != 0 {
		t.Fatalf("the second replica wrote %d runs that the first had filled", got)
	}
	if w := two.summaries(ctx)["01OLD"].Work; !w.Facts || !w.Backfilled {
		t.Fatalf("the copy of the second replica = %+v, want the facts of the first", w)
	}
}

// A replica reads again a finished run whose summary another replica
// changed, such as a thumb after the end that the shared consumer gave to
// the other replica. Without that, the value of the work named the
// verdict on one replica only.
func TestArchiveRefreshReadsAChangedRun(t *testing.T) {
	ctx := context.Background()
	b := bucket.NewMem(ArchiveBucket)
	one := newArchive(b, nil, quietLogger())
	start := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
	for _, e := range conversation(t, "01DONE", "ax:team/manager-0", start) {
		if err := one.add(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	two := newArchive(b, nil, quietLogger())
	two.refresh(ctx)
	if r := two.summaries(ctx)["01DONE"]; r.Run == nil || !r.Run.State.Terminal() || r.Work.Verdict != "" {
		t.Fatalf("the copy of the second replica = %+v", r)
	}
	if err := one.modify(ctx, "01DONE", func(r *archivedRun) bool {
		r.Work.Verdict = "accepted"
		return true
	}); err != nil {
		t.Fatal(err)
	}
	two.refresh(ctx)
	if v := two.summaries(ctx)["01DONE"].Work.Verdict; v != "accepted" {
		t.Fatalf("the second replica reads the verdict %q, want accepted", v)
	}
}

// A result that a person sends back comes back to be checked: the follow-up
// ends the run again, and the new answer waits for a check until a person
// reacts to it. The first verdict stays for first-pass yield, and the
// newest verdict is the one of the board. A reaction taken back opens the
// check again.
func TestWorkRework(t *testing.T) {
	t0 := time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC)
	at := func(m int) time.Time { return t0.Add(time.Duration(m) * time.Minute) }
	run := &fw.Run{ID: "R1", State: fw.StateCompleted, Metadata: map[string]string{MetaReview: "1"}}
	events := []fw.Event{
		ev(1, fw.EvRunQueued, at(0), fw.RunQueuedData{Agent: "writer"}),
		ev(2, fw.EvRunFinished, at(5), fw.RunFinishedData{Outcome: "completed"}),
	}
	if w := WorkOf(events); !ToCheck(run, w) || Again(w) {
		t.Fatalf("the first answer: to check %v, again %v (%+v)", ToCheck(run, w), Again(w), w)
	}
	events = append(events,
		ev(3, fw.EvRunReacted, at(10), fw.ReactionData{Seq: 2, Reaction: fw.ReactionDown, Note: "Name the competitor on each line.", By: "alice"}),
		ev(4, fw.EvMessageReceived, at(10), fw.MessageReceivedData{Role: fw.RoleUser, Origin: "http", By: "alice"}),
	)
	run.State = fw.StateRunning
	if w := WorkOf(events); ToCheck(run, w) || !w.Reworking {
		t.Fatalf("a run that works again is no result to check, and it reworks: %+v", w)
	}
	run.State = fw.StateCompleted
	events = append(events, ev(5, fw.EvRunFinished, at(12), fw.RunFinishedData{Outcome: "completed"}))
	w := WorkOf(events)
	if !ToCheck(run, w) || !Again(w) || w.Note != "Name the competitor on each line." || w.Verdict != VerdictRejected || w.Rework != 1 || w.Reworking {
		t.Fatalf("the new answer: to check %v, again %v, %+v", ToCheck(run, w), Again(w), w)
	}
	events = append(events, ev(6, fw.EvRunReacted, at(15), fw.ReactionData{Seq: 5, Reaction: fw.ReactionUp, By: "alice"}))
	w = WorkOf(events)
	if ToCheck(run, w) || w.LastVerdict != VerdictAccepted || w.Verdict != VerdictRejected {
		t.Fatalf("after Looks right: to check %v, %+v", ToCheck(run, w), w)
	}
	events = append(events, ev(7, fw.EvRunReacted, at(16), fw.ReactionData{Seq: 5, Reaction: fw.ReactionUp, By: "alice", Removed: true}))
	if w = WorkOf(events); !ToCheck(run, w) || w.LastVerdict != "" {
		t.Fatalf("after the undo: to check %v, %+v", ToCheck(run, w), w)
	}
	// Facts from before the archive counted ends fall back to the counts.
	run.Reactions = map[string]int{fw.ReactionDown: 1}
	if ToCheck(run, Work{Facts: true}) {
		t.Fatal("a run with a thumb and old facts is no result to check")
	}
	if !strings.Contains(SendBackText("Shorter."), "Shorter.") || !strings.Contains(SendBackText("x"), "whole result") {
		t.Fatalf("the follow-up = %q", SendBackText("Shorter."))
	}
}

// A run of a duty that tells only of what it finds waits for a check when
// its answer found something, with the words of what it found, or the
// first line of an answer without the line. An answer that found nothing
// needs no check, a run without the metadata is no found result, and a
// thumb ends the check. The relay puts a thumb on such a run on its end.
func TestFound(t *testing.T) {
	mk := func(out string, md map[string]string) *fw.Run {
		return &fw.Run{ID: "r", State: fw.StateCompleted, Output: out, Metadata: md}
	}
	found := map[string]string{MetaFound: "1"}
	for _, x := range []struct {
		run   *fw.Run
		what  string
		check bool
	}{
		{mk("Found: 2 hosts drift\nweb-3 and web-4.", found), "2 hosts drift", true},
		{mk("**Found:** nothing.\nAll match.", found), "", false},
		{mk("## Two hosts drift\nweb-3 and web-4.", found), "Two hosts drift", true},
		{mk("Found: 2 hosts drift", nil), "", false},
		{&fw.Run{State: fw.StateFailed, Output: "Found: 2", Metadata: found}, "", false},
	} {
		what, ok := Found(x.run)
		if what != x.what || ok != x.check || ToCheck(x.run, Work{}) != x.check {
			t.Errorf("Found(%q, %v) = %q %v, ToCheck %v; want %q %v", x.run.Output, x.run.Metadata, what, ok, ToCheck(x.run, Work{}), x.what, x.check)
		}
	}
	r := mk("Found: 2 hosts drift", found)
	if ToCheck(r, Work{Ends: 1, Reviewed: 1, LastVerdict: VerdictAccepted}) || !ToCheck(r, Work{Ends: 2, Reviewed: 1, LastVerdict: VerdictRejected}) {
		t.Fatal("a thumb does not end the check of a found result, or a new answer does not open it")
	}
	if !toCheck(mk("x", found)) || toCheck(mk("x", nil)) {
		t.Fatal("the relay does not put the thumb of a found result on its end")
	}
}
