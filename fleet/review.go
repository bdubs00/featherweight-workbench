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
// leaves review on the first reaction, a thumb up, a thumb down, or a
// heart.
func InReview(r *fw.Run) bool {
	if r == nil || r.State != fw.StateCompleted || r.Metadata[MetaReview] != "1" {
		return false
	}
	return r.Reactions[fw.ReactionUp]+r.Reactions[fw.ReactionDown]+r.Reactions[fw.ReactionHeart] == 0
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
