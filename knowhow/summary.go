package knowhow

// This file serves GET /v1/knowhow/summary (docs/KNOWHOW.md sections 1
// and 8.3): the four numbers of an agent over the last days, split into
// normal and held-back runs. The runs come from the archive of the fleet,
// so a run of a replaced worker counts too. A run is done well when it
// completed and nothing that a system or a person recorded says
// otherwise: no receipt sent it back, its grader did not fail it, and no
// person gave it a thumbs down. The comparison takes the normal runs of
// the Duties that had a held-back run, so both sides did the same work,
// and it gives no percentage from fewer than MinHeldBack held-back runs.

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/surface/native"
)

// Bounds of a summary.
const (
	DefaultSummaryDays = 7
	MaxSummaryDays     = 30
	// MinHeldBack is the fewest held-back runs that a comparison takes.
	MinHeldBack = 20
	// NotEnough is the line of a comparison with fewer held-back runs.
	NotEnough = "Not enough held-back runs yet."
)

// Summary is the answer of GET /v1/knowhow/summary.
type Summary struct {
	Agent          string     `json:"agent"`
	Days           int        `json:"days"`
	Since          time.Time  `json:"since"`
	CostPerJob     CostPerJob `json:"costPerJob"`
	DoneByRoutines Share      `json:"doneByRoutines"`
	NotesCorrected int        `json:"notesCorrected"`
	Normal         Split      `json:"normal"`
	HeldBack       Split      `json:"heldBack"`
	Compare        Compare    `json:"compare"`
}

// CostPerJob is the mean cost of the finished runs of the window, and of
// the window of the same length before it, for the trend: down, up, or
// flat within 5%.
type CostPerJob struct {
	USD         float64  `json:"usd"`
	Runs        int      `json:"runs"`
	PreviousUSD *float64 `json:"previousUsd,omitempty"`
	Trend       string   `json:"trend,omitempty"`
}

// Share is the share of the normal runs that a routine did with no model
// call.
type Share struct {
	Runs      int     `json:"runs"`
	ByRoutine int     `json:"byRoutine"`
	Share     float64 `json:"share"`
}

// Split holds the counts of one kind of runs: how many finished, how many
// were done well, how many a routine did, and the mean cost.
type Split struct {
	Runs       int     `json:"runs"`
	DoneWell   int     `json:"doneWell"`
	ByRoutine  int     `json:"byRoutine"`
	CostPerJob float64 `json:"costPerJobUsd"`
}

// Compare is the line "with and without what it learned". The shares are
// absent while fewer than MinHeldBack runs were held back.
type Compare struct {
	Enough   bool     `json:"enough"`
	HeldBack int      `json:"heldBack"`
	Normal   int      `json:"normal"`
	OneIn    int      `json:"oneIn,omitempty"`
	Without  *float64 `json:"doneWellWithout,omitempty"`
	With     *float64 `json:"doneWellWith,omitempty"`
	Line     string   `json:"line"`
}

// SummaryOf returns the summary of the agent over the last days.
func (s *Service) SummaryOf(ctx context.Context, agent string, days int) (Summary, error) {
	if err := CheckAgent(agent); err != nil {
		return Summary{}, err
	}
	if days < 1 {
		days = DefaultSummaryDays
	}
	days = min(days, MaxSummaryDays)
	now := s.now().UTC()
	since := now.Add(-time.Duration(days) * 24 * time.Hour)
	before := since.Add(-time.Duration(days) * 24 * time.Hour)
	out := Summary{Agent: agent, Days: days, Since: since}
	var cost, prevCost float64
	var prevRuns int
	type item struct {
		run   *fw.Run
		facts runFacts
	}
	var inWindow []item
	heldDuties := map[string]bool{}
	for _, e := range s.archive(ctx) {
		r := e.Run
		if r.Agent != agent || !r.State.Terminal() {
			continue
		}
		switch {
		case !r.CreatedAt.Before(since):
		case !r.CreatedAt.Before(before):
			prevCost += r.Usage.CostUSD
			prevRuns++
			continue
		default:
			continue
		}
		f, err := s.factsOf(ctx, e)
		if err != nil {
			return Summary{}, err
		}
		inWindow = append(inWindow, item{run: r, facts: f})
		cost += r.Usage.CostUSD
		if heldBack(r) {
			heldDuties[dutyOf(r)] = true
		}
	}
	var normalCost, heldCost float64
	var cmpNormal, cmpNormalWell, cmpHeldWell int
	for _, it := range inWindow {
		r, f := it.run, it.facts
		split, c := &out.Normal, &normalCost
		if heldBack(r) {
			split, c = &out.HeldBack, &heldCost
		}
		split.Runs++
		*c += r.Usage.CostUSD
		well := doneWell(r, f)
		if well {
			split.DoneWell++
		}
		if f.ByRoutine {
			split.ByRoutine++
		}
		switch {
		case heldBack(r):
			if well {
				cmpHeldWell++
			}
		case heldDuties[dutyOf(r)]:
			cmpNormal++
			if well {
				cmpNormalWell++
			}
		}
	}
	out.Normal.CostPerJob = mean(normalCost, out.Normal.Runs)
	out.HeldBack.CostPerJob = mean(heldCost, out.HeldBack.Runs)
	out.CostPerJob = CostPerJob{USD: mean(cost, len(inWindow)), Runs: len(inWindow)}
	if prevRuns > 0 {
		p := mean(prevCost, prevRuns)
		out.CostPerJob.PreviousUSD = &p
		out.CostPerJob.Trend = trend(out.CostPerJob.USD, p, len(inWindow))
	}
	out.DoneByRoutines = Share{Runs: out.Normal.Runs, ByRoutine: out.Normal.ByRoutine}
	if out.Normal.Runs > 0 {
		out.DoneByRoutines.Share = float64(out.Normal.ByRoutine) / float64(out.Normal.Runs)
	}
	entries, err := s.journalSince(ctx, agent, since)
	if err != nil {
		return Summary{}, err
	}
	for _, e := range entries {
		if e.Corrects && e.UndoneBy == "" {
			out.NotesCorrected++
		}
	}
	out.Compare = compare(out.HeldBack.Runs, cmpHeldWell, cmpNormal, cmpNormalWell)
	return out, nil
}

// compare words the line "with and without what it learned".
func compare(held, heldWell, normal, normalWell int) Compare {
	c := Compare{HeldBack: held, Normal: normal, Line: NotEnough}
	if held < MinHeldBack || normal == 0 {
		return c
	}
	without := float64(heldWell) / float64(held)
	with := float64(normalWell) / float64(normal)
	c.Enough, c.Without, c.With = true, &without, &with
	c.OneIn = int(math.Round(float64(held+normal) / float64(held)))
	c.Line = fmt.Sprintf("Held back 1 in %d runs. Done well: %d%% without what it learned, %d%% with it.", c.OneIn, int(math.Round(without*100)), int(math.Round(with*100)))
	return c
}

func mean(sum float64, n int) float64 {
	if n == 0 {
		return 0
	}
	return sum / float64(n)
}

// trend compares the mean cost with the mean of the window before.
func trend(now, before float64, runs int) string {
	switch {
	case runs == 0:
		return ""
	case before == 0 && now == 0:
		return "flat"
	case before == 0:
		return "up"
	}
	switch d := (now - before) / before; {
	case d <= -0.05:
		return "down"
	case d >= 0.05:
		return "up"
	}
	return "flat"
}

// summary answers GET /v1/knowhow/summary?agent=&days=7. It needs the
// right to view the agent.
func (rt *routes) summary(w http.ResponseWriter, r *http.Request, s *Service) {
	ctx := onBehalf(r)
	agent, ok := agentParam(w, r)
	if !ok {
		return
	}
	days := DefaultSummaryDays
	if v := strings.TrimSpace(r.URL.Query().Get("days")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > MaxSummaryDays {
			native.WriteProblem(w, r, http.StatusBadRequest, "days is a whole number from 1 to 30")
			return
		}
		days = n
	}
	if _, err := s.rightsOn(ctx, agent); err != nil {
		rt.fail(w, r, err)
		return
	}
	out, err := s.SummaryOf(ctx, agent, days)
	if err != nil {
		rt.fail(w, r, err)
		return
	}
	native.WriteJSON(w, http.StatusOK, out)
}
