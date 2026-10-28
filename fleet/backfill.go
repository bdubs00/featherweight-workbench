package fleet

// This file gives the summaries of the archive the facts of the work of
// their runs (work.go) when a server before those facts wrote them. The
// archive keeps every event of each run, so the backfill reads them again
// and folds them, once per run, in the background after the start. The
// value of the work (package value) then counts the runs that the archive
// kept before this file existed, and no longer calls them unknown.

import (
	"context"
	"time"
)

// backfillPause is the pause between two runs of the backfill, so a large
// archive does not flood the server of the bus. A test sets it to zero.
var backfillPause = 20 * time.Millisecond

// backfillTries bounds the reads of one run whose summary moved on while
// the backfill read its events.
const backfillTries = 3

// backfill gives each summary without facts the facts of its events, and
// returns how many it gave. A summary without the start of its run has no
// identity, so it waits. A summary that the backfill could not give facts,
// because the stream no longer held the start of its run, keeps the mark
// Backfilled, so a later start of the front does not read it again. Every
// replica runs it: a write is a compare-and-swap, and a replica that finds
// the facts there already writes nothing.
func (a *archive) backfill(ctx context.Context) int {
	a.refresh(ctx)
	var todo []string
	for id, r := range a.summaries(ctx) {
		if r.Queued && r.Run != nil && !r.Work.Facts && !r.Work.Backfilled {
			todo = append(todo, id)
		}
	}
	filled := 0
	for _, id := range todo {
		if ctx.Err() != nil {
			break
		}
		if a.backfillOne(ctx, id) {
			filled++
		}
		if backfillPause > 0 {
			select {
			case <-ctx.Done():
			case <-time.After(backfillPause):
			}
		}
	}
	if filled > 0 {
		a.log.Info("fleet: the archive gave runs the facts of their work", "runs", filled, "of", len(todo))
	}
	return filled
}

// backfillOne reads the events of one run and writes their facts into its
// summary. An event that arrives while the read runs makes the summary
// count more events than the read found, and the read starts again.
func (a *archive) backfillOne(ctx context.Context, id string) bool {
	for range backfillTries {
		events, err := a.events(ctx, id)
		if err != nil {
			a.log.Warn("fleet: the backfill of the archive did not read a run", "run", id, "error", err)
			return false
		}
		w := WorkOf(events)
		w.Backfilled = true
		again, wrote := false, false
		err = a.modify(ctx, id, func(r *archivedRun) bool {
			again, wrote = false, false
			if r.Work.Facts || r.Work.Backfilled {
				return false
			}
			if r.Events > len(events) {
				again = true
				return false
			}
			r.Work, wrote = w, true
			return true
		})
		if err != nil {
			a.log.Warn("fleet: the backfill of the archive did not write a run", "run", id, "error", err)
			return false
		}
		if !again {
			return wrote && w.Facts
		}
	}
	return false
}
