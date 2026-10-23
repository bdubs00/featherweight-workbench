package bridges

// This file gives a seat run the allowance of its seat for each message
// that it handles (docs/ARCHITECTURE.md section 18.5). The kernel counts
// every limit of a budget over the whole run, and a seat run takes every
// follow-up of the channel, so a budget that fits the start of the work
// runs out after a few messages, and the seat fails at a limit. So before
// each follow-up to a seat run, the front raises the limits of the run by
// its allowance over what the run used: the follow-up carries the new
// limits (fw.WithSendBudget, or budget in the body of POST
// /v1/runs/{id}/messages), which the kernel applies before the message.
// The start of a run keeps the budget of its seat. The allowance is the
// budget that the kernel resolved at the start of the run: the budget of
// the seat over the budget of the profile and of the configuration of its
// worker. The front reads it once from the first event of the run and
// keeps it in the mark of the run. The cap maxTurns bounds the turns of
// the whole bridge (fanPlan), so agents that keep each other busy cannot
// spend without end.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"hash/fnv"
	"strings"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// allowanceOf keeps the limits of a budget that count the whole run: the
// turns, the tool calls, the tokens, the cost, and the working time. The
// limits of one turn, such as maxCallsPerTurn, and the final report stay
// as the run has them.
func allowanceOf(b fw.Budget) fw.Budget {
	return fw.Budget{MaxTurns: b.MaxTurns, MaxToolCalls: b.MaxToolCalls, MaxTokens: b.MaxTokens, MaxCostUSD: b.MaxCostUSD, Deadline: b.Deadline}
}

// startBudget is the budget of a run as the kernel resolved it when it
// queued the run: the budget of the request, with each zero field taken
// from the budget of the resolved profile, which holds the budget of the
// configuration. An event of an older kernel resolved nothing, so it gives
// the budget of the request alone, the budget of the seat.
func startBudget(q fw.RunQueuedData) fw.Budget {
	var b fw.Budget
	if q.Budget != nil {
		b = *q.Budget
	}
	if q.Resolved != nil && q.Resolved.Budget != nil {
		b = b.Merge(*q.Resolved.Budget)
	}
	return b
}

// readAllowance reads the allowance of a run from its first event,
// fw.run.queued, at the owner of the run. It reports false when the read
// fails. The follow-up that needs it goes to the same worker, so the read
// wakes nothing that the follow-up does not.
func (s *Store) readAllowance(ctx context.Context, r *fw.Run) (fw.Budget, bool) {
	events, err := s.events(ctx, r, 1, 1)
	if err != nil {
		s.log.Debug("bridges: read the budget of a run", "run", r.ID, "error", err)
		return fw.Budget{}, false
	}
	for _, e := range events {
		if e.Type != fw.EvRunQueued {
			continue
		}
		var q fw.RunQueuedData
		if e.Decode(&q) != nil {
			return fw.Budget{}, false
		}
		return allowanceOf(startBudget(q)), true
	}
	return fw.Budget{}, false
}

// usedOf is what a run used of each limit, as the view knows it: the
// turns of the run, and the tool calls, the tokens, and the cost of the
// run and its helpers, which the kernel counts together against the
// budget of the run.
func usedOf(r *fw.Run) (turns, calls, tokens int, cost float64) {
	tree := r.Usage
	if r.TreeUsage != nil {
		tree = *r.TreeUsage
	}
	return r.Usage.Turns, tree.ToolCalls, tree.InputTokens + tree.OutputTokens, tree.CostUSD
}

// nextGrant is the limits that a follow-up gives a seat run: each limit of
// the allowance on top of what the run used. prev is the limits that the
// front gave the run last, or nil before the first follow-up, when the
// run has its start limits, the allowance itself.
//
// The view knows the usage of a run only at its last end. A run that
// works can be in the middle of a turn, and it folds the message into
// that work, so for a run that works the follow-up raises the limits that
// the run has, prev or the allowance, when they are above what the view
// knows. Every message that a seat handles thus gives it one allowance.
// The deadline counts working time, which the view does not know, so each
// follow-up adds the allowance of working time to the deadline that the
// run has.
func nextGrant(prev *fw.Budget, allow fw.Budget, r *fw.Run) fw.Budget {
	base := allow
	if prev != nil {
		base = *prev
	}
	working := !r.State.Terminal()
	turns, calls, tokens, cost := usedOf(r)
	from := func(used, limit int) int {
		if working && limit > used {
			return limit
		}
		return used
	}
	var out fw.Budget
	if allow.MaxTurns > 0 {
		out.MaxTurns = from(turns, base.MaxTurns) + allow.MaxTurns
	}
	if allow.MaxToolCalls > 0 {
		out.MaxToolCalls = from(calls, base.MaxToolCalls) + allow.MaxToolCalls
	}
	if allow.MaxTokens > 0 {
		out.MaxTokens = from(tokens, base.MaxTokens) + allow.MaxTokens
	}
	if allow.MaxCostUSD > 0 {
		used := cost
		if working && base.MaxCostUSD > used {
			used = base.MaxCostUSD
		}
		out.MaxCostUSD = used + allow.MaxCostUSD
	}
	if allow.Deadline > 0 {
		last := base.Deadline
		if last <= 0 {
			last = allow.Deadline
		}
		out.Deadline = last + allow.Deadline
	}
	return out
}

// grant decides the limits of the next follow-up to a seat run, and keeps
// them and the allowance in the mark of the run, in one change of the
// record, so the next follow-up builds on them. The allowance comes from
// the mark, else from the first event of the run, else from the budget of
// the seat. It returns nil when the run has no limit that counts the
// whole run, and then the follow-up changes no limit.
func (s *Store) grant(ctx context.Context, bridge, handle string, r *fw.Run) *fw.Budget {
	fctx := s.asFront(ctx)
	rec, _, err := s.b.get(fctx, bridge)
	if err != nil {
		s.log.Warn("bridges: read the bridge for the budget of a follow-up", "bridge", bridge, "run", r.ID, "error", err)
		return nil
	}
	var allow fw.Budget
	read := false
	if mk := rec.Marks[r.ID]; mk != nil && mk.Allow != nil {
		allow = *mk.Allow
	} else if b, ok := s.readAllowance(ctx, r); ok {
		allow, read = b, true
	} else if sm := rec.Seats[handle]; sm != nil {
		if b := budgetOf(sm.Budget); b != nil {
			allow = allowanceOf(*b)
		}
	}
	if allow == (fw.Budget{}) && !read {
		return nil
	}
	var out *fw.Budget
	_, err = s.b.update(ctx, bridge, func(rec *record) error {
		out = nil
		mk := rec.mark(r.ID, handle)
		if read {
			a := allow
			mk.Allow = &a
		}
		if allow == (fw.Budget{}) {
			return nil
		}
		g := nextGrant(mk.Grant, allow, r)
		mk.Grant, out = &g, &g
		return nil
	})
	if err != nil {
		s.log.Warn("bridges: keep the budget of a follow-up", "bridge", bridge, "run", r.ID, "error", err)
		if allow == (fw.Budget{}) {
			return nil
		}
		g := nextGrant(nil, allow, r)
		return &g
	}
	return out
}

// sendSeat gives a seat run a follow-up with the allowance of its seat
// (grant). The run is read again from the view first, so the limits build
// on its newest usage. A run that stopped at a limit and has no allowance
// would fail the follow-up at once, so it gets none, and the channel says
// once that it cannot take messages. The grant and the send of one run go
// one at a time on this replica, so a follow-up that leaves later never
// gives the run the smaller limits of an earlier one. files go with the
// follow-up when they are not nil.
func (s *Store) sendSeat(ctx context.Context, bridge, handle string, r *fw.Run, content string, from fw.MessageFrom, pinned bool, files *upload) error {
	mu := &s.sendLocks[lockOf(r.ID, len(s.sendLocks))]
	mu.Lock()
	defer mu.Unlock()
	if fresh := s.runOf(s.asFront(ctx), r.ID); fresh != nil && (fresh.Member == r.Member || r.Member == "") {
		r = fresh
	}
	budget := s.grant(ctx, bridge, handle, r)
	if budget == nil && limited(r) {
		s.cannotTakeLine(ctx, bridge, handle, r)
		return nil
	}
	return s.sendWith(ctx, r, content, from, pinned, budget, files)
}

// lockOf is the index of the lock of a run among n.
func lockOf(run string, n int) int {
	h := fnv.New32a()
	_, _ = h.Write([]byte(run))
	return int(h.Sum32() % uint32(n))
}

// postDigest is the digest of a text that a run posted, with its spaces
// made one, so an answer that repeats the post word for word has the same
// digest.
func postDigest(text string) string {
	sum := sha256.Sum256([]byte(strings.Join(strings.Fields(text), " ")))
	return hex.EncodeToString(sum[:16])
}

// maxPosts bounds the digests of the posts that the mark of a run keeps.
const maxPosts = 8
