package fleet

// This file reads the runs of one agent over a window of time, and what
// each run tells beside its usage, for the day of an agent of bossmode
// (docs/ARCHITECTURE.md section 17.8) and the summary of the routines
// (section 21.6). Both read the same runs in the same way.

import (
	"context"
	"encoding/json"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// RunsSince lists the runs of the profile agent that the caller in ctx can
// see and that started at since or later: the runs of this process, and,
// with a fleet, the runs of the other members, as GET /v1/fleet/runs
// decides for the caller (Fleet.AgentRuns). Child runs count, each on its
// own.
func RunsSince(ctx context.Context, a *fw.Agent, agent string, since time.Time) ([]*fw.Run, error) {
	local, err := a.List(ctx)
	if err != nil {
		return nil, err
	}
	var runs []*fw.Run
	for _, r := range local {
		if r.Agent == agent && !r.CreatedAt.Before(since) {
			runs = append(runs, r)
		}
	}
	if f, ok := Of(a).(*Fleet); ok && f != nil {
		remote, err := f.AgentRuns(ctx, agent, since)
		if err != nil {
			return nil, err
		}
		runs = append(runs, remote...)
	}
	return runs, nil
}

// RunFacts is what a run tells beside its usage. Decisions counts the
// fw.approval.decided events of the run: the approvals and the questions
// that a person decided. ByRoutine is true when a routine completed the
// run, and SentBack when a quality observation of the run says rejected.
// Whole is false when the front cannot know every such event of the run:
// the view did not see the start of a run of another member on the
// stream, or the ledger of a run of this process did not read.
type RunFacts struct {
	Decisions           int
	ByRoutine, SentBack bool
	Whole               bool
}

// FactsOf returns the facts of a run of RunsSince: for a run of this
// process from its ledger, and for a run of another member from the view.
// A branch leaves out the events that it kept from its source.
func FactsOf(ctx context.Context, a *fw.Agent, r *fw.Run) RunFacts {
	if r.Member == "" {
		return ledgerFacts(ctx, a, r.ID)
	}
	f, ok := Of(a).(*Fleet)
	if !ok || f == nil {
		return RunFacts{}
	}
	n, whole := f.Decisions(r.ID)
	byRoutine, sentBack, _ := f.Outcome(r.ID)
	return RunFacts{Decisions: n, ByRoutine: byRoutine, SentBack: sentBack, Whole: whole}
}

// ledgerFacts reads the facts of a run of this process from its ledger.
func ledgerFacts(ctx context.Context, a *fw.Agent, id string) RunFacts {
	var kept uint64
	type seen struct {
		seq uint64
		typ string
	}
	var list []seen
	out := RunFacts{Whole: true}
	for e, err := range a.Ledger().Read(ctx, id, 0) {
		if err != nil {
			out.Whole = false
			break
		}
		switch {
		case e.Type == fw.EvRunQueued:
			// The kept events of a branch come first, and its own
			// fw.run.queued names how many.
			var d struct {
				Kept uint64 `json:"kept"`
			}
			if json.Unmarshal(e.Data, &d) == nil && d.Kept > kept {
				kept = d.Kept
			}
		case e.Type == fw.EvApprovalDecided, AppliedOK(e), Verification(e) == "rejected":
			list = append(list, seen{seq: e.Seq, typ: e.Type})
		}
	}
	for _, s := range list {
		if s.seq <= kept {
			continue
		}
		switch s.typ {
		case fw.EvApprovalDecided:
			out.Decisions++
		case fw.EvRoutineApplied:
			out.ByRoutine = true
		case fw.EvRunAnnotated:
			out.SentBack = true
		}
	}
	return out
}
