package fleet

// This file keeps what the front learns of the work of each run from its
// events: when the work was asked for and first ended, what people did on
// it, how long it waited for them, and the first verdict on its result.
// The archive folds these facts into the summary of each run as it applies
// the events of the egress stream (archive.go), so a read of the value of
// the work (package value) reads no event again. The facts of a run of
// this process come from its ledger.

import (
	"cmp"
	"context"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// Verdicts of the work of a run (Work.Verdict).
const (
	VerdictAccepted = "accepted" // a person gave a thumb up, or a receipt accepted or satisfied the result
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
//   - Verdict is the first verdict on a result of the run: a thumb of a
//     person, or a quality receipt (VerdictAccepted or VerdictRejected). A
//     grade of a model is no verdict, and neither is a heart: a heart keeps
//     a result on the wiki of the agent, and it says nothing of whether the
//     result is right.
//   - Done is the field done of the newest fw.run.finished: fw.DoneChecked
//     when the definition of done of the agent passed, fw.DoneFailed when
//     its checks ended the run, and empty for a run without one or of an
//     older kernel. It is no verdict of a person. DoneWeak is the field
//     doneWeak of the same event: the pass showed nothing about the work
//     of the run, so the run is not checked (Checked).
//   - Ends counts the ends of the run that completed, and Reviewed is the
//     value of Ends at the newest reaction of a person. A follow-up after
//     Needs work ends the run again, so Ends passes Reviewed, and the run
//     waits for a check of its new answer (ToCheck). LastVerdict is the
//     newest verdict of a person, where Verdict keeps the first. Note is
//     the note of the newest thumb down.
//   - CanceledBy names the person who canceled the run, when one did.
//   - Backfilled is true once the backfill of the archive (backfill.go)
//     read the events of the run again for these facts.
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
	DoneWeak     bool      `json:"doneWeak,omitempty"`
	CanceledBy   string    `json:"canceledBy,omitempty"`
	Backfilled   bool      `json:"backfilled,omitempty"`
	Ends         int       `json:"ends,omitempty"`
	Reviewed     int       `json:"reviewed,omitempty"`
	LastVerdict  string    `json:"lastVerdict,omitempty"`
	Note         string    `json:"note,omitempty"`
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
		if d.Outcome == string(fw.StateCompleted) {
			w.Ends++
		}
		w.Done, w.DoneWeak = d.Done, d.DoneWeak
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
		if e.Decode(&d) != nil {
			return
		}
		var v string
		switch d.Reaction {
		case fw.ReactionUp:
			v = VerdictAccepted
		case fw.ReactionDown:
			v = VerdictRejected
		default:
			return
		}
		if d.Removed {
			// A person takes back the reaction on the newest answer, so
			// that answer waits for a check again.
			if w.Reviewed == w.Ends && w.Ends > 0 {
				w.Reviewed, w.LastVerdict, w.Note = w.Ends-1, "", ""
			}
			return
		}
		w.Reviewed, w.LastVerdict = w.Ends, v
		if v == VerdictRejected {
			w.Note = d.Note
		}
		if w.Verdict == "" {
			w.Verdict = v
		}
	case fw.EvRunAnnotated:
		// A receipt of a system of record checks the newest answer, as a
		// thumb of a person does.
		var v string
		switch Verification(e) {
		case "accepted", "satisfied":
			v = VerdictAccepted
		case "rejected":
			v = VerdictRejected
		default:
			return
		}
		w.Reviewed, w.LastVerdict = w.Ends, v
		if w.Verdict == "" {
			w.Verdict = v
		}
	}
}

// Judged is the newest verdict on the result of a run: the newest thumb of
// a person or receipt, else the first one. The board and Results both read
// it, so they count a result that a person sent back and then accepted the
// same way (plan A13).
func (w Work) Judged() string { return cmp.Or(w.LastVerdict, w.Verdict) }

// Checked reports whether the definition of done of a run passed in the
// kernel with a pass that shows the work of the run (fw.CheckedDone): the
// facts of its work say so, else the run object of a newer kernel does. A
// weak pass, such as a check that only reads a file that was there before
// the run, is no check. Results, the board, and Know-how count only such a
// run as checked.
func Checked(r *fw.Run, w Work) bool {
	if w.Done != "" {
		return fw.CheckedDone(w.Done, w.DoneWeak)
	}
	return r != nil && r.Done != nil && fw.CheckedDone(r.Done.State, r.Done.Weak)
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
	return WorkWhere(ctx, a, func(r *fw.Run) bool { return !r.CreatedAt.Before(since) })
}

// WorkWhere is WorkSince for the runs that keep picks, such as the runs
// that did not end, however old, beside the runs of a window. The board of
// the work (package work) and the value of the work (package value) read
// the archive through it, one read for each answer.
func WorkWhere(ctx context.Context, a *fw.Agent, keep func(*fw.Run) bool) ([]WorkRun, error) {
	local, err := a.List(ctx)
	if err != nil {
		return nil, err
	}
	var out []WorkRun
	seen := map[string]bool{}
	for _, r := range local {
		if !keep(r) {
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
			if run := s.listed(); run != nil && !seen[run.ID] && keep(run) {
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
		if !seen[r.ID] && keep(r) {
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
