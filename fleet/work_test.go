package fleet

import (
	"context"
	"encoding/json"
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
