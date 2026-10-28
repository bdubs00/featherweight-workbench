package fleet

// This file holds what the front needs to know about the results that a
// person asked to check (docs/ARCHITECTURE.md section 26), and the egress
// of a pool for the plan of a task (section 27).

import (
	"context"
	"slices"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// MetaReview is the metadata key of a run whose owner asked to check its
// result. Its value is "1". A duty with review sets it on each start; a
// plan of the door sets it on a run that the words ask to check.
const MetaReview = "workbench.review"

// InReview reports whether a run waits for a person to check its result:
// it completed, it carries MetaReview, and no person reacted to it yet. It
// reads the counts of the run object alone. ToCheck reads the facts of the
// run too, and is right after Needs work.
func InReview(r *fw.Run) bool {
	if r == nil || r.State != fw.StateCompleted || r.Metadata[MetaReview] != "1" {
		return false
	}
	return r.Reactions[fw.ReactionUp]+r.Reactions[fw.ReactionDown]+r.Reactions[fw.ReactionHeart] == 0
}

// ToCheck reports whether a run waits for a person to check its newest
// result. It completed, it carries MetaReview, and no person reacted after
// its newest end. So the new answer after Needs work waits for a check
// again. Facts without ends, from before the archive counted them, fall
// back to InReview and to the verdict of the facts, such as a receipt.
func ToCheck(r *fw.Run, w Work) bool {
	if r == nil || r.State != fw.StateCompleted || r.Metadata[MetaReview] != "1" {
		return false
	}
	if w.Ends > 0 {
		return w.Reviewed < w.Ends
	}
	return InReview(r) && w.Verdict == ""
}

// Again reports whether the result to check is a new answer after a person
// sent an earlier one back.
func Again(w Work) bool {
	return w.Reviewed > 0 && w.Ends > w.Reviewed && w.LastVerdict == VerdictRejected
}

// SendBackText is the follow-up message of Needs work: the note of the
// person, and the ask for the whole result again. Without that ask, a model
// answers with the change alone, and the first answer is lost to the
// person who checks.
func SendBackText(note string) string {
	return "A person checked your result and sent it back with this note:\n\n" + note +
		"\n\nAnswer again with the whole result, changed as the note says."
}

// PoolEgress returns the egress hosts of the pool name, the hosts that its
// workers reach, and reports false for a pool that the fleet does not
// keep.
func PoolEgress(ctx context.Context, a *fw.Agent, pool string) ([]string, bool) {
	f := fleetOf(a)
	if f == nil || pool == "" {
		return nil, false
	}
	for _, p := range f.poolsNow(ctx) {
		if p.Name != pool {
			continue
		}
		if p.Egress == nil {
			return nil, true
		}
		return slices.Clone(p.Egress.Hosts), true
	}
	return nil, false
}
