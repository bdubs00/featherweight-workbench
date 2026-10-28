package fleet

// This file lets a pass of the front read the whole archive of the runs
// (docs/ARCHITECTURE.md section 4.18), such as the passes of Know-how that
// look for what an agent can learn (package knowhow). The routes of runs
// read the archive for a caller and leave out the runs that a member
// holds. A pass of the front reads every run that the archive keeps, also
// the runs that a member still holds, and decides nothing for a caller:
// the route that shows what the pass found decides.

import (
	"context"
	"sort"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// ArchiveEntry is one run of the archive as a pass of the front reads it:
// the run with its member, the time of its newest fw.run.finished, and the
// time at which the archive last changed its summary. Updated moves with
// each event that the archive applies to the run, also an event after the
// end, such as a quality receipt or a reaction.
type ArchiveEntry struct {
	Run     *fw.Run
	Member  string
	Ended   time.Time
	Updated time.Time
}

// ArchiveAll returns every run of the archive of the fleet of a that has
// its identity (a fw.run.queued), held by a member or not, the oldest
// change first. It decides nothing for a caller. It reports false when the
// fleet keeps no archive. The first call of a process waits until the
// summaries of the bucket are read.
func ArchiveAll(ctx context.Context, a *fw.Agent) ([]ArchiveEntry, bool) {
	f, _ := Of(a).(*Fleet)
	if f == nil || f.arch() == nil {
		return nil, false
	}
	arch := f.arch()
	arch.mu.Lock()
	loaded := arch.loaded
	arch.mu.Unlock()
	if !loaded {
		arch.reading.Lock()
		arch.mu.Lock()
		loaded = arch.loaded
		arch.mu.Unlock()
		if !loaded {
			arch.refresh(ctx)
		}
		arch.reading.Unlock()
	}
	var out []ArchiveEntry
	for _, r := range arch.summaries(ctx) {
		run := r.listed()
		if run == nil {
			continue
		}
		out = append(out, ArchiveEntry{Run: run, Member: r.Member, Ended: r.Ended, Updated: r.Updated})
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Updated.Equal(out[j].Updated) {
			return out[i].Updated.Before(out[j].Updated)
		}
		return out[i].Run.ID < out[j].Run.ID
	})
	return out, true
}

// ArchiveEvents returns the events of the run id that the archive keeps,
// in the order of its ledger, held by a member or not. It decides nothing
// for a caller. The events are those of the egress stream, so each long
// string is cut to EgressMaxBytes and no fw.model.responded is among them.
func ArchiveEvents(ctx context.Context, a *fw.Agent, id string) ([]fw.Event, error) {
	f, _ := Of(a).(*Fleet)
	if f == nil || f.arch() == nil {
		return nil, nil
	}
	return f.arch().events(ctx, id)
}
