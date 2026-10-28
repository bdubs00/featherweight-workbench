package bridges

// This file holds the cap of cost of a bridge, caps.maxCostUSD
// (docs/ARCHITECTURE.md section 18.5). The cost of a bridge is the cost of
// every run of its batch together, at the prices of the workers: the
// seats, their helpers, and in a spawn bridge the manager run and its
// children. At the cap, the channel says so once, nothing fans out or
// seats, and each open run that the channel mirrors stops at a limit of
// its budget. A person who raises the cap clears the mark, and the next
// message of a person continues a stopped seat with its allowance, as for
// any run that stopped at a limit.

import (
	"context"
	"fmt"
	"math"
	"net/http"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// costOf sums the cost of the runs of a bridge. Each run counts its own
// usage, never the usage of its tree, so no call counts twice.
func costOf(runs map[string]*fw.Run) float64 {
	sum := 0.0
	for _, r := range runs {
		sum += r.Usage.CostUSD
	}
	return sum
}

// costCapLine reports whether the runs of a bridge reached its cap of
// cost. The first time, it marks the record and returns the system line
// that says so; later it returns no line. A bridge without the cap never
// reaches it.
func costCapLine(rec *record, runs map[string]*fw.Run) (*Message, bool) {
	limit := rec.Caps.MaxCostUSD
	if limit <= 0 {
		return nil, false
	}
	spent := costOf(runs)
	if spent < limit {
		return nil, false
	}
	if rec.Fan.CostCapped {
		return nil, true
	}
	rec.Fan.CostCapped = true
	cap, cost := capDollars(limit, spent)
	m := system(EventCap, fmt.Sprintf("The bridge reached its cap of %s: its runs cost %s. Its open runs stop at a limit", cap, cost))
	return &m, true
}

// capDollars words a cap of cost and the cost of the runs to the cent,
// or to a tenth of a cent when the cap is not a whole number of cents. A
// cap of $0.005 then reads "$0.005", not "$0.01", and its cost "$0.007".
func capDollars(limit, spent float64) (string, string) {
	format := "$%.2f"
	if c := limit * 100; math.Abs(c-math.Round(c)) > 1e-9 {
		format = "$%.3f"
	}
	return fmt.Sprintf(format, limit), fmt.Sprintf(format, spent)
}

// capBudget is the change of budget that stops a run at a limit: its cost
// so far, so the next model call passes the limit and the run stops at
// once. A run without a priced cost, such as a run of a model without a
// price, stops at a limit of its turns so far, at the start of its next
// turn, with its final report.
func capBudget(r *fw.Run) fw.Budget {
	if r.Usage.CostUSD > 0 {
		return fw.Budget{MaxCostUSD: r.Usage.CostUSD}
	}
	return fw.Budget{MaxTurns: max(r.Usage.Turns, 1)}
}

// stopAtCap stops each open run that the channel mirrors at a limit of its
// budget, as the tool run_budget does, once for each run and cap. A run
// that waits for a person stops when it goes on. A change that fails says
// so in the channel.
func (s *Store) stopAtCap(rec *record, runs map[string]*fw.Run) {
	limit := rec.Caps.MaxCostUSD
	for _, r := range sorted(runs) {
		seat := mirrored(rec, r)
		if seat == "" || r.State.Terminal() {
			continue
		}
		s.mu.Lock()
		done := s.capStops[r.ID] == limit
		s.capStops[r.ID] = limit
		s.mu.Unlock()
		if done {
			continue
		}
		run, change, bridge := r, capBudget(r), rec.ID
		s.submit(&job{
			name: "the stop at the cap of cost of run " + run.ID + " of " + seat,
			do:   func(ctx context.Context) error { return s.changeBudget(ctx, run, change) },
			fail: func(err error) {
				s.log.Warn("bridges: a run did not stop at the cap of cost", "bridge", bridge, "member", seat, "run", run.ID, "error", err)
				_ = s.postLines(s.ctx, bridge, system(EventFailed, fmt.Sprintf("The run %s of %s did not stop at the cap of cost: %s", run.ID, seat, oneLine(err.Error(), 300))))
			},
		})
	}
}

// changeBudget changes the budget of a run of this process or of a member
// of the fleet, as PATCH /v1/runs/{id}/budget does.
func (s *Store) changeBudget(ctx context.Context, r *fw.Run, b fw.Budget) error {
	if r.Member == "" {
		_, err := s.a.ChangeBudget(s.asFront(ctx), r.ID, b, fw.OriginTool)
		return err
	}
	return s.call(ctx, r.Member, http.MethodPatch, runPath(r.ID, "/budget"), b, "", nil)
}
