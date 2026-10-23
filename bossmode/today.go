package bossmode

// This file serves GET /v1/bossmode/agents/{agent}/today, the numbers of
// the profile pane of the page (docs/ARCHITECTURE.md section 17.8): the
// runs of an agent that started today, what they cost, and the decisions
// that people made on them.

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/surface/native"
	"github.com/bdubs00/featherweight-workbench/faces"
	"github.com/bdubs00/featherweight-workbench/fleet"
)

// WindowToday names the window of Today: the calendar day of the front.
const WindowToday = "today"

// Today is the answer of GET /v1/bossmode/agents/{agent}/today. The runs
// are the runs of the profile Agent that the caller can see, of this
// process and of the other members of the fleet, child runs included,
// that started at Since or later: midnight of the current day in Zone,
// the time zone of the front (time.zone). Runs counts them. CostUSD sums
// their usage as the tool batch sums a job (fleet.UsageSum), at the prices
// of the workers, and Partial is true when a run has no usage that is
// final or holds tokens without a price, whose cost is unknown. Decisions
// counts the fw.approval.decided events of the runs: the approvals and the
// questions that a person decided. DecisionsPartial is true when the front
// cannot know every decision of a run: the view did not see the start of a
// run of another member on the egress stream, or the ledger of a run of
// this process did not read.
type Today struct {
	Agent            string    `json:"agent"`
	Window           string    `json:"window"`
	Zone             string    `json:"zone"`
	Since            time.Time `json:"since"`
	Runs             int       `json:"runs"`
	CostUSD          float64   `json:"costUSD"`
	Partial          bool      `json:"partial"`
	Decisions        int       `json:"decisions"`
	DecisionsPartial bool      `json:"decisionsPartial"`
}

// todayRoute answers the numbers of the day of an agent, or 404 for an
// agent that the caller cannot view (faces.Check with agent.view).
func todayRoute(k *native.Kit) http.HandlerFunc {
	a := k.Agent()
	return func(w http.ResponseWriter, r *http.Request) {
		agent := r.PathValue("agent")
		ctx := r.Context()
		if faces.CheckAgent(agent) != nil {
			native.WriteProblem(w, r, http.StatusNotFound, "bossmode: no agent has the name "+agent)
			return
		}
		if err := faces.Check(ctx, a, agent, fw.ActionAgentView); err != nil {
			if errors.Is(err, fw.ErrProfileNotFound) || errors.Is(err, fw.ErrForbidden) {
				native.WriteProblem(w, r, http.StatusNotFound, "bossmode: no agent "+agent+" that you can view")
				return
			}
			k.Fail(w, r, err)
			return
		}
		out, err := TodayOf(ctx, a, agent)
		if err != nil {
			k.Fail(w, r, err)
			return
		}
		native.WriteJSON(w, http.StatusOK, out)
	}
}

// TodayOf returns the numbers of the day of the profile agent for the
// caller in ctx, as Today says. It decides no role on the agent; the route
// does.
func TodayOf(ctx context.Context, a *fw.Agent, agent string) (Today, error) {
	loc, zone, err := fw.LoadZone(a.Config().Time.Zone)
	if err != nil {
		loc, zone = time.UTC, "UTC"
	}
	now := a.Clock().Now().In(loc)
	since := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	out := Today{Agent: agent, Window: WindowToday, Zone: zone, Since: since}
	runs, err := fleet.RunsSince(ctx, a, agent, since)
	if err != nil {
		return Today{}, err
	}
	var sum fleet.UsageSum
	for _, r := range runs {
		sum.Add(r)
		facts := fleet.FactsOf(ctx, a, r)
		out.Decisions += facts.Decisions
		out.DecisionsPartial = out.DecisionsPartial || !facts.Whole
	}
	out.Runs, out.CostUSD = len(runs), sum.CostUSD
	out.Partial = sum.Missing > 0 || sum.Final < sum.Reported || sum.UnpricedTokens > 0
	return out, nil
}
