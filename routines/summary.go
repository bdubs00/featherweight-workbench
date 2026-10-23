package routines

// This file serves GET /v1/routines/summary, the week of an agent on the
// details pane of the page (docs/ARCHITECTURE.md section 21.6): how many
// runs a routine handled, how many the agent handled, how many needed a
// person, what a run cost on average, and how many came back. It reads the
// runs as the day of an agent of bossmode does (section 17.8): the view,
// and the ledgers of the runs of this process in the window. A summary
// holds for a minute per agent, window, and caller, so a page that reads
// it often reads no ledger again.

import (
	"context"
	"strconv"
	"sync"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/fleet"
)

// Bounds of the window of a summary, in days, and how long a summary
// holds.
const (
	DefaultSummaryDays = 7
	MaxSummaryDays     = 30
	summaryFor         = time.Minute
	maxSummaries       = 256
)

// Summary is the answer of GET /v1/routines/summary. The runs are the runs
// of the profile Agent that the caller can see, of this process and of
// the other members, child runs included, that started at Since or later:
// midnight, Days-1 days before today, in Zone, the time zone of the front.
// A run is by a routine when a routine completed it, with a person when a
// person decided an approval or answered a question in it, and by the
// agent otherwise. CostUSD holds the cost of a run on average, by a
// routine and by the agent, at the prices of the workers. SentBack counts
// the runs that a quality observation rejected. Partial is true when a
// count or a cost can be short: a run without final usage, tokens without
// a price, a run whose start the view did not see, or a ledger that did
// not read.
type Summary struct {
	Agent      string      `json:"agent"`
	Days       int         `json:"days"`
	Zone       string      `json:"zone"`
	Since      time.Time   `json:"since"`
	Runs       int         `json:"runs"`
	ByRoutine  int         `json:"byRoutine"`
	ByAgent    int         `json:"byAgent"`
	WithPerson int         `json:"withPerson"`
	CostUSD    SummaryCost `json:"costUSD"`
	SentBack   int         `json:"sentBack"`
	Partial    bool        `json:"partial"`
}

// SummaryCost is the cost of a run on average: Routine over the runs by a
// routine, and Agent over the runs by the agent. Each is 0 without runs.
type SummaryCost struct {
	Routine float64 `json:"routine"`
	Agent   float64 `json:"agent"`
}

// summaries keeps the summaries of a store for a minute.
type summaries struct {
	mu sync.Mutex
	m  map[string]heldSummary
}

type heldSummary struct {
	s     Summary
	until time.Time
}

// SummaryOf returns the summary of the profile agent over the last days
// for the caller in ctx, as Summary says. It decides no role on the agent;
// the route does. A summary of the same agent, window, and caller holds
// for a minute.
func SummaryOf(ctx context.Context, a *fw.Agent, agent string, days int) (Summary, error) {
	if days < 1 {
		days = DefaultSummaryDays
	}
	days = min(days, MaxSummaryDays)
	now := a.Clock().Now()
	var held *summaries
	key := agent + "\x00" + strconv.Itoa(days)
	if p := fw.PrincipalFrom(ctx); p != nil {
		key += "\x00" + p.Kind + "\x00" + p.Subject
	}
	if s := Of(a); s != nil {
		held = &s.summaries
		held.mu.Lock()
		h, ok := held.m[key]
		held.mu.Unlock()
		if ok && now.Before(h.until) {
			return h.s, nil
		}
	}
	out, err := summaryOf(ctx, a, agent, days, now)
	if err != nil || held == nil {
		return out, err
	}
	held.mu.Lock()
	defer held.mu.Unlock()
	if held.m == nil || len(held.m) >= maxSummaries {
		held.m = map[string]heldSummary{}
	}
	held.m[key] = heldSummary{s: out, until: now.Add(summaryFor)}
	return out, nil
}

// summaryOf reads the summary.
func summaryOf(ctx context.Context, a *fw.Agent, agent string, days int, now time.Time) (Summary, error) {
	loc, zone, err := fw.LoadZone(a.Config().Time.Zone)
	if err != nil {
		loc, zone = time.UTC, "UTC"
	}
	now = now.In(loc)
	since := time.Date(now.Year(), now.Month(), now.Day()-(days-1), 0, 0, 0, 0, loc)
	out := Summary{Agent: agent, Days: days, Zone: zone, Since: since}
	runs, err := fleet.RunsSince(ctx, a, agent, since)
	if err != nil {
		return Summary{}, err
	}
	var byRoutine, byAgent, all fleet.UsageSum
	for _, r := range runs {
		facts := fleet.FactsOf(ctx, a, r)
		all.Add(r)
		switch {
		case facts.ByRoutine:
			out.ByRoutine++
			byRoutine.Add(r)
		case facts.Decisions > 0:
			out.WithPerson++
		default:
			out.ByAgent++
			byAgent.Add(r)
		}
		if facts.SentBack {
			out.SentBack++
		}
		out.Partial = out.Partial || !facts.Whole
	}
	out.Runs = len(runs)
	out.CostUSD = SummaryCost{Routine: average(byRoutine), Agent: average(byAgent)}
	out.Partial = out.Partial || all.Missing > 0 || all.Final < all.Reported || all.UnpricedTokens > 0
	return out, nil
}

// average is the cost of a run on average over the runs of the sum that
// report a usage, or 0.
func average(u fleet.UsageSum) float64 {
	if u.Reported == 0 {
		return 0
	}
	return u.CostUSD / float64(u.Reported)
}
