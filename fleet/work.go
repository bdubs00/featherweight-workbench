package fleet

// This file keeps what the front learns of the work of each run from its
// events: when the work was asked for and first ended, what people did on
// it, how long it waited for them, and the first verdict on its result.
// The archive folds these facts into the summary of each run as it applies
// the events of the egress stream (archive.go), so a read of the value of
// the work (package value) reads no event again. The facts of a run of
// this process come from its ledger.

import (
	"context"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// Verdicts of the work of a run (Work.Verdict).
const (
	VerdictAccepted = "accepted" // a person gave a thumb up or a heart, or a receipt accepted or satisfied the result
	VerdictRejected = "rejected" // a person gave a thumb down, or a receipt rejected the result
)

// Work is what the events of one run tell about its work.
//
//   - Queued is the time of the first fw.run.queued: the time that the
//     work was asked for. Facts is true once the record applied that
//     event, so it saw every later event of the run. A record without it,
//     such as a summary of the archive that a server before this file
//     wrote, holds only a part of the facts.
//   - FirstEnd is the time of the first fw.run.finished, and FirstOutcome
//     its outcome. A follow-up can end the run again later; the state of
//     the run tells the last end.
//   - Acts counts the actions of people on the run: each approval or
//     question that a person decided, and each message of a person after
//     the start. Rework counts the messages of people after the first end.
//     Handled counts the decisions that no person made: the front, the
//     manager, a timeout, or a parent run.
//   - WaitMS sums the time from each approval request to its decision by a
//     person, and Waits counts those decisions. Asked is the time of the
//     request that waits now, or zero.
//   - Verdict is the first verdict on a result of the run: a thumb or a
//     heart of a person, or a quality receipt (VerdictAccepted or
//     VerdictRejected). A grade of a model is no verdict.
//   - Done is the field done of the newest fw.run.finished: fw.DoneChecked
//     when the definition of done of the agent passed, fw.DoneFailed when
//     its checks ended the run, and empty for a run without one or of an
//     older kernel. It is no verdict of a person.
//   - CanceledBy names the person who canceled the run, when one did.
//
// A branch counts the events that it kept from its source too, so a
// reader that counts pieces of work takes no branch as one.
type Work struct {
	Queued       time.Time `json:"queued,omitzero"`
	Facts        bool      `json:"facts,omitempty"`
	FirstEnd     time.Time `json:"firstEnd,omitzero"`
	FirstOutcome string    `json:"firstOutcome,omitempty"`
	Acts         int       `json:"acts,omitempty"`
	Rework       int       `json:"rework,omitempty"`
	Handled      int       `json:"handled,omitempty"`
	WaitMS       int64     `json:"waitMs,omitempty"`
	Waits        int       `json:"waits,omitempty"`
	Asked        time.Time `json:"asked,omitzero"`
	Verdict      string    `json:"verdict,omitempty"`
	Done         string    `json:"done,omitempty"`
	CanceledBy   string    `json:"canceledBy,omitempty"`
}

// Apply folds one event of the run into the facts. The events of a run
// come in the order of its ledger; an event that comes late still counts,
// and a second delivery of an event is the duty of the caller, as the
// archive drops it with the keys that it saw.
func (w *Work) Apply(e fw.Event) {
	switch e.Type {
	case fw.EvRunQueued:
		if w.Queued.IsZero() || e.Time.Before(w.Queued) {
			w.Queued = e.Time
		}
		w.Facts = true
	case fw.EvRunFinished:
		var d fw.RunFinishedData
		_ = e.Decode(&d)
		if w.FirstEnd.IsZero() {
			w.FirstEnd, w.FirstOutcome = e.Time, d.Outcome
		}
		if d.Outcome == string(fw.StateCanceled) && d.By != "" {
			w.CanceledBy = d.By
		}
		w.Done = d.Done
		w.Asked = time.Time{}
	case fw.EvApprovalRequested:
		if w.Asked.IsZero() {
			w.Asked = e.Time
		}
	case fw.EvApprovalDecided:
		var d fw.ApprovalDecidedData
		if e.Decode(&d) != nil {
			return
		}
		if DecidedByPerson(d) {
			w.Acts++
			if !w.Asked.IsZero() && !e.Time.Before(w.Asked) {
				w.WaitMS += e.Time.Sub(w.Asked).Milliseconds()
				w.Waits++
			}
		} else {
			w.Handled++
		}
		w.Asked = time.Time{}
	case fw.EvMessageReceived:
		var d fw.MessageReceivedData
		if e.Decode(&d) != nil || !FromPerson(d) {
			return
		}
		w.Acts++
		if !w.FirstEnd.IsZero() {
			w.Rework++
		}
	case fw.EvRunReacted:
		var d fw.ReactionData
		if w.Verdict != "" || e.Decode(&d) != nil || d.Removed {
			return
		}
		switch d.Reaction {
		case fw.ReactionUp, fw.ReactionHeart:
			w.Verdict = VerdictAccepted
		case fw.ReactionDown:
			w.Verdict = VerdictRejected
		}
	case fw.EvRunAnnotated:
		if w.Verdict != "" {
			return
		}
		switch Verification(e) {
		case "accepted", "satisfied":
			w.Verdict = VerdictAccepted
		case "rejected":
			w.Verdict = VerdictRejected
		}
	}
}

// DecidedByPerson reports whether a person decided an approval: the kind
// of the decider is a person, or an older kernel named a decider without
// a kind. The front, the manager, and a timeout decide with the static
// token of the fleet, whose kind is token, and a parent run decides as a
// parent, so none of them counts.
func DecidedByPerson(d fw.ApprovalDecidedData) bool {
	switch d.DeciderKind {
	case fw.ActorPerson:
		return true
	case "":
		return d.Decider != ""
	}
	return false
}

// FromPerson reports whether a message of a run came from a person after
// the start: a message of the user that the kernel did not write, from a
// person that a trusted caller relays, or from a caller that names a
// subject and relays nobody. A message that the front relays from another
// agent does not count.
func FromPerson(d fw.MessageReceivedData) bool {
	if d.Role != fw.RoleUser || d.Note != "" || d.Origin == "kernel" {
		return false
	}
	if d.From != nil {
		return d.From.Kind == fw.FromPerson
	}
	return d.By != ""
}

// WorkOf folds the events of one run into its facts.
func WorkOf(events []fw.Event) Work {
	var w Work
	for _, e := range events {
		w.Apply(e)
	}
	return w
}

// WorkRun is a run that the caller can view, with the facts of its work.
// Lost is true for a run of the archive that never ended and that no
// member holds now: a pool replaced its Task while it worked or waited, so
// it can never end, and no person can answer it.
type WorkRun struct {
	Run  *fw.Run
	Work Work
	Lost bool
}

// WorkSince returns the runs that were asked for at since or later and
// that the caller in ctx can view, each with the facts of its work: the
// runs of this process from their ledgers, and the runs of the workers
// from the archive, which holds every event of the egress stream and keeps
// the runs of the replaced Tasks. Without an archive, the runs of the
// workers come from the view and carry no facts. Child runs and the runs
// that the lists leave out are in it; the caller picks.
func WorkSince(ctx context.Context, a *fw.Agent, since time.Time) ([]WorkRun, error) {
	local, err := a.List(ctx)
	if err != nil {
		return nil, err
	}
	var out []WorkRun
	seen := map[string]bool{}
	for _, r := range local {
		if r.CreatedAt.Before(since) {
			continue
		}
		seen[r.ID] = true
		out = append(out, WorkRun{Run: r, Work: ledgerWork(ctx, a, r.ID)})
	}
	f, _ := Of(a).(*Fleet)
	if f == nil {
		return out, nil
	}
	if arch := f.arch(); arch != nil {
		sums := arch.summaries(ctx)
		var runs []*fw.Run
		for _, s := range sums {
			if run := s.listed(); run != nil && !seen[run.ID] && !run.CreatedAt.Before(since) {
				runs = append(runs, run)
			}
		}
		visible, err := f.visibleArchived(ctx, runs)
		if err != nil {
			return nil, err
		}
		for _, run := range visible {
			seen[run.ID] = true
			lost := !run.State.Terminal() && !f.held(run.ID)
			out = append(out, WorkRun{Run: run, Work: sums[run.ID].Work, Lost: lost})
		}
		return out, nil
	}
	remote, err := f.Runs(ctx)
	if err != nil {
		return nil, err
	}
	for _, r := range remote {
		if !seen[r.ID] && !r.CreatedAt.Before(since) {
			out = append(out, WorkRun{Run: r})
		}
	}
	return out, nil
}

// ledgerWork reads the facts of the work of a run of this process from
// its ledger. A ledger that does not read gives the facts so far, without
// Facts.
func ledgerWork(ctx context.Context, a *fw.Agent, id string) Work {
	var w Work
	for e, err := range a.Ledger().Read(ctx, id, 0) {
		if err != nil {
			w.Facts = false
			return w
		}
		w.Apply(e)
	}
	return w
}
