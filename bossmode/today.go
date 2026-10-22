package bossmode

// This file serves GET /v1/bossmode/agents/{agent}/today, the numbers of
// the profile pane of the page (docs/ARCHITECTURE.md section 17.8): the
// runs of an agent that started today, what they cost, and the decisions
// that people made on them.

import (
	"context"
	"encoding/json"
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
	local, err := a.List(ctx)
	if err != nil {
		return Today{}, err
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
			return Today{}, err
		}
		runs = append(runs, remote...)
	}
	var sum fleet.UsageSum
	for _, r := range runs {
		sum.Add(r)
		n, whole := 0, false
		if r.Member == "" {
			n, whole = localDecisions(ctx, a, r.ID)
		} else if f != nil {
			n, whole = f.Decisions(r.ID)
		}
		out.Decisions += n
		out.DecisionsPartial = out.DecisionsPartial || !whole
	}
	out.Runs, out.CostUSD = len(runs), sum.CostUSD
	out.Partial = sum.Missing > 0 || sum.Final < sum.Reported || sum.UnpricedTokens > 0
	return out, nil
}

// localDecisions counts the fw.approval.decided events in the ledger of a
// run of this process, after the events that a branch kept from its
// source, and reports whether the ledger read whole.
func localDecisions(ctx context.Context, a *fw.Agent, id string) (int, bool) {
	var decided []uint64
	var kept uint64
	whole := true
	for e, err := range a.Ledger().Read(ctx, id, 0) {
		if err != nil {
			whole = false
			break
		}
		switch e.Type {
		case fw.EvRunQueued:
			// The kept events of a branch come first, and its own
			// fw.run.queued names how many.
			var d struct {
				Kept uint64 `json:"kept"`
			}
			if json.Unmarshal(e.Data, &d) == nil && d.Kept > kept {
				kept = d.Kept
			}
		case fw.EvApprovalDecided:
			decided = append(decided, e.Seq)
		}
	}
	n := 0
	for _, seq := range decided {
		if seq > kept {
			n++
		}
	}
	return n, whole
}
