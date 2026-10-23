package routines

// This file serves GET /v1/routines/summary, the week of an agent on the
// details pane of the page (docs/ARCHITECTURE.md section 21.6): how many
// runs a routine handled, how many the agent handled, how many needed a
// person, what a run cost on average, and how many came back. It follows
// the day of an agent of bossmode (section 17.8): the front keeps no state
// for it, and each request reads the view and the ledger of this process.

import (
	"context"
	"encoding/json"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/fleet"
)

// Bounds of the window of a summary, in days.
const (
	DefaultSummaryDays = 7
	MaxSummaryDays     = 90
)

// Summary is the answer of GET /v1/routines/summary. The runs are the runs
// of the profile Agent that the caller can see, of this process and of
// the other members, child runs included, that started at Since or later:
// midnight, Days-1 days before today, in Zone, the time zone of the front.
// A run is by a routine when a routine completed it, with a person when a
// person decided an approval or answered a question in it, and by the
// agent otherwise. CostUSD holds the cost of a run on average, by a
// routine and by the agent, at the prices of the workers. SentBack counts
// the runs that a quality receipt rejected. Partial is true when a count
// or a cost can be short: a run without final usage, tokens without a
// price, a run whose start the view did not see, or a ledger that did not
// read.
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

// runFacts is what the summary needs to know of one run beside its usage.
type runFacts struct {
	byRoutine, sentBack, withPerson, whole bool
}

// SummaryOf returns the summary of the profile agent over the last days
// for the caller in ctx, as Summary says. It decides no role on the agent;
// the route does.
func SummaryOf(ctx context.Context, a *fw.Agent, agent string, days int) (Summary, error) {
	if days < 1 {
		days = DefaultSummaryDays
	}
	days = min(days, MaxSummaryDays)
	loc, zone, err := fw.LoadZone(a.Config().Time.Zone)
	if err != nil {
		loc, zone = time.UTC, "UTC"
	}
	now := a.Clock().Now().In(loc)
	since := time.Date(now.Year(), now.Month(), now.Day()-(days-1), 0, 0, 0, 0, loc)
	out := Summary{Agent: agent, Days: days, Zone: zone, Since: since}
	local, err := a.List(ctx)
	if err != nil {
		return Summary{}, err
	}
	var runs []*fw.Run
	for _, r := range local {
		if r.Agent == agent && !r.CreatedAt.Before(since) {
			runs = append(runs, r)
		}
	}
	f, _ := fleet.Of(a).(*fleet.Fleet)
	if f != nil {
		remote, err := f.AgentRuns(ctx, agent, since)
		if err != nil {
			return Summary{}, err
		}
		runs = append(runs, remote...)
	}
	var byRoutine, byAgent, all fleet.UsageSum
	for _, r := range runs {
		var rf runFacts
		if r.Member == "" {
			rf = localFacts(ctx, a, r.ID)
		} else if f != nil {
			n, whole := f.Decisions(r.ID)
			rf.byRoutine, rf.sentBack, rf.whole = f.Outcome(r.ID)
			rf.withPerson, rf.whole = n > 0, rf.whole && whole
		}
		all.Add(r)
		switch {
		case rf.byRoutine:
			out.ByRoutine++
			byRoutine.Add(r)
		case rf.withPerson:
			out.WithPerson++
		default:
			out.ByAgent++
			byAgent.Add(r)
		}
		if rf.sentBack {
			out.SentBack++
		}
		out.Partial = out.Partial || !rf.whole
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

// localFacts reads the ledger of a run of this process, after the events
// that a branch kept from its source: whether a routine completed it,
// whether a person decided in it, and whether a quality receipt rejected
// it. whole is false when the ledger did not read.
func localFacts(ctx context.Context, a *fw.Agent, id string) runFacts {
	var kept uint64
	type seen struct {
		seq uint64
		typ string
		ok  bool
	}
	var list []seen
	rf := runFacts{whole: true}
	for e, err := range a.Ledger().Read(ctx, id, 0) {
		if err != nil {
			rf.whole = false
			break
		}
		switch e.Type {
		case fw.EvRunQueued:
			var d struct {
				Kept uint64 `json:"kept"`
			}
			if json.Unmarshal(e.Data, &d) == nil && d.Kept > kept {
				kept = d.Kept
			}
		case fw.EvApprovalDecided:
			list = append(list, seen{seq: e.Seq, typ: e.Type, ok: true})
		case fw.EvRoutineApplied:
			ok, _ := appliedOK(e)
			list = append(list, seen{seq: e.Seq, typ: e.Type, ok: ok})
		case fw.EvRunAnnotated:
			var d fw.QualityAnnotatedData
			if e.Decode(&d) == nil && d.Quality != nil && d.Quality.Verification == "rejected" {
				list = append(list, seen{seq: e.Seq, typ: e.Type, ok: true})
			}
		}
	}
	for _, s := range list {
		if s.seq <= kept || !s.ok {
			continue
		}
		switch s.typ {
		case fw.EvApprovalDecided:
			rf.withPerson = true
		case fw.EvRoutineApplied:
			rf.byRoutine = true
		case fw.EvRunAnnotated:
			rf.sentBack = true
		}
	}
	return rf
}
