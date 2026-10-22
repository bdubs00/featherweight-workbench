package fleet

// This file sums the usage of a set of runs, such as the runs of a job
// (the MCP tool batch, docs/ARCHITECTURE.md section 4.9) or of a bridge
// (package bridges, section 18). Both sum the same way, so a job and a
// bridge of the same runs show the same usage.

import "github.com/bdubs00/featherweight-kernel/fw"

// UsageSum is the usage of a set of runs: the sum of the usage of the runs
// that report one, field by field. The sum counts each run once. A child
// run carries the batch of its parent, so it is a run of the set of its
// own, and its usage is not also in the usage of its parent; a branch
// counts only its own work, not the events that it kept from its source.
// The sum adds Run.Usage and never Run.TreeUsage, which holds the child
// runs too.
//
// Reported counts the runs in the sum, and Missing the runs whose usage
// the view does not know (UsageOf). Final counts the reported runs whose
// usage is final. Partial is true when Missing is above zero, when a
// reported run is not final, or when the view forgot runs of the set; the
// caller sets it, because only the caller knows what the view forgot.
// CostUSD is the cost at the prices of the workers, and it leaves out
// UnpricedTokens, whose cost is unknown.
type UsageSum struct {
	fw.RunUsage
	Reported int  `json:"reported"`
	Final    int  `json:"final"`
	Missing  int  `json:"missing"`
	Partial  bool `json:"partial"`
}

// Add counts one run: its usage, or one more missing run.
func (u *UsageSum) Add(r *fw.Run) {
	known, final := UsageOf(r)
	if !known {
		u.Missing++
		return
	}
	u.Reported++
	if final {
		u.Final++
	}
	u.Usage = u.Usage.Add(r.Usage.Usage)
	u.Turns += r.Usage.Turns
	u.ToolCalls += r.Usage.ToolCalls
	u.CostUSD += r.Usage.CostUSD
	u.UnpricedTokens += r.Usage.UnpricedTokens
}

// UsageOf reports whether the view knows the usage of a run, and whether
// that usage is final.
//
// A run of this process, without Member, has the usage of its whole
// ledger, so the view knows it, and it is final once the run is finished.
// A run of another member has a known usage when its kernel recorded the
// totals of the run on fw.run.finished or fw.run.suspended
// (fw.Run.UsageSeq), and the usage is final when the run is finished: the
// totals of its end and the annotations that the stream carried after
// them. Tokens without totals do not count, because on a stream of
// lifecycle events they come from fw.run.annotated alone, such as a title
// call, and not from the model calls of the run. A run that works or waits
// again after its totals, after a follow-up, a resume, or a decision, is
// known up to its totals, so its usage is not final.
func UsageOf(r *fw.Run) (known, final bool) {
	if r.Member == "" {
		return true, r.State.Terminal()
	}
	known = r.UsageSeq > 0
	return known, known && r.State.Terminal()
}
