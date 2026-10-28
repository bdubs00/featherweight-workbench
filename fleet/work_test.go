package fleet

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
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
