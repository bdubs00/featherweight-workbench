package web

// This file serves GET /v1/fleet, the members and the pools of the fleet, and
// GET /v1/fleet/runs, the runs of the whole fleet (docs/ARCHITECTURE.md,
// section 4.7). The relay of the fleet answers a request about a run that
// another member owns through the native surface of the kernel, and starts
// a run on another member with POST /v1/runs?member=<name>.

import (
	"net/http"
	"strconv"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/surface/view"
	"github.com/bdubs00/featherweight-workbench/dashboard"
	"github.com/bdubs00/featherweight-workbench/fleet"
)

// fleetOff is the problem detail of a fleet route without a fleet.
const fleetOff = "the fleet is off; set workbench.fleet.atespaces (FW_WORKBENCH_FLEET_ATESPACES) or workbench.fleet.egress to turn it on"

// fleet answers GET /v1/fleet with the name of this member, the members
// that the caller can see, and the pools whose agent the caller can view,
// each with its wanted and actual Tasks and the reasons for a gap.
func (s *server) fleet(w http.ResponseWriter, r *http.Request) {
	f := fleet.Of(s.a)
	if f == nil {
		problem(w, r, http.StatusNotFound, fleetOff)
		return
	}
	members, err := f.Members(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if members == nil {
		members = []fleet.Member{}
	}
	body := map[string]any{"name": f.Name(), "members": members}
	if pv, ok := f.(fleet.PoolView); ok {
		ps, err := pv.Pools(r.Context())
		if err != nil {
			s.fail(w, r, err)
			return
		}
		if len(ps) > 0 {
			body["pools"] = ps
		}
	}
	writeJSON(w, http.StatusOK, body)
}

// fleetRuns answers GET /v1/fleet/runs with the runs of the whole fleet that
// the caller can see, in the shape of GET /v1/runs of the kernel. The query
// parameter scope is fleet, the default, or local. The parameter member
// narrows the list to one member, and parent lists the children of a run of
// any member. The parameters agent, state, principal, batch, and limit
// narrow the list as on GET /v1/runs.
func (s *server) fleetRuns(w http.ResponseWriter, r *http.Request) {
	if fleet.Of(s.a) == nil {
		problem(w, r, http.StatusNotFound, fleetOff)
		return
	}
	q := r.URL.Query()
	limit := 0
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			problem(w, r, http.StatusBadRequest, "the query parameter limit must be a positive number")
			return
		}
		limit = n
	}
	var runs []*fw.Run
	var err error
	if parent := q.Get("parent"); parent != "" {
		runs, err = dashboard.Children(r.Context(), s.a, parent)
	} else {
		runs, err = dashboard.Runs(r.Context(), s.a, q.Get("scope"), q.Get("member"))
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	matched := view.FilterBatch(view.Filter(runs, q.Get("agent"), q.Get("state"), q.Get("principal"), 0), q.Get("batch"))
	glance, total := view.GlanceOf(matched, time.Now()), len(matched)
	if limit > 0 && len(matched) > limit {
		matched = matched[:limit]
	}
	writeJSON(w, http.StatusOK, map[string]any{"runs": matched, "total": total, "glance": glance})
}
