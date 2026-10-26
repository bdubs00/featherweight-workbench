package fleet

// This file keeps the time at which a run ended (docs/ARCHITECTURE.md
// section 4.21). The updatedAt of a run moves with each later event, such
// as a reaction or a title, so it is no time of the end. The view and the
// archive keep the time of the newest fw.run.finished of each run that
// ended, and the answers of the front carry it as endedAt.

import (
	"context"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// endedAfter returns the end of a run after the event e applied to run:
// the time of e for fw.run.finished, zero for a run that works again, and
// the end before for any other event.
func endedAfter(before time.Time, run *fw.Run, e fw.Event) time.Time {
	switch {
	case e.Type == fw.EvRunFinished:
		return e.Time.UTC()
	case !run.State.Terminal():
		return time.Time{}
	}
	return before
}

// noteEnd keeps the end of the run of the view after the event e.
func (r *platformRun) noteEnd(e fw.Event) { r.ended = endedAfter(r.ended, r.run, e) }

// EndedAt returns the time at which each run of the ids ended, as the
// view of the fleet of a and its archive saw its newest fw.run.finished.
// A run that works, a run of this process, and a run whose end the view
// did not see have none.
func EndedAt(ctx context.Context, a *fw.Agent, ids []string) map[string]time.Time {
	out := map[string]time.Time{}
	f, _ := Of(a).(*Fleet)
	if f == nil || len(ids) == 0 {
		return out
	}
	if p := f.platform; p != nil {
		p.mu.Lock()
		for _, id := range ids {
			if r := p.runs[id]; r != nil && !r.ended.IsZero() && r.run.State.Terminal() {
				out[id] = r.ended
			}
		}
		p.mu.Unlock()
	}
	if arch := f.arch(); arch != nil {
		var all map[string]archivedRun
		for _, id := range ids {
			if _, ok := out[id]; ok {
				continue
			}
			if all == nil {
				all = arch.summaries(ctx)
			}
			if r, ok := all[id]; ok && !r.Ended.IsZero() && r.Run != nil && r.Run.State.Terminal() {
				out[id] = r.Ended
			}
		}
	}
	return out
}

// endedOf returns the end of one run as EndedAt gives it, or nil.
func endedOf(ctx context.Context, a *fw.Agent, id string) *time.Time {
	if t, ok := EndedAt(ctx, a, []string{id})[id]; ok {
		return &t
	}
	return nil
}

// archivedEnd returns the end of a run of the archive: the end that its
// summary keeps, else the time of the newest fw.run.finished among its
// events, for a summary that a server before 2026-10-27 wrote. A run that
// did not end has none.
func (rl *Relay) archivedEnd(ctx context.Context, run *fw.Run) *time.Time {
	if !run.State.Terminal() {
		return nil
	}
	if t := endedOf(ctx, rl.a, run.ID); t != nil {
		return t
	}
	f, _ := Of(rl.a).(*Fleet)
	if f == nil || f.arch() == nil {
		return nil
	}
	events, err := f.arch().events(ctx, run.ID)
	if err != nil {
		return nil
	}
	var end *time.Time
	for _, e := range events {
		if e.Type == fw.EvRunFinished {
			t := e.Time.UTC()
			end = &t
		}
	}
	return end
}
