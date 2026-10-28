package web

// This file serves GET /v1/fleet, the members and the pools of the fleet, and
// GET /v1/fleet/runs, the runs of the whole fleet (docs/ARCHITECTURE.md,
// section 4.7). The relay of the fleet answers a request about a run that
// another member owns through the native surface of the kernel, and starts
// a run on another member with POST /v1/runs?member=<name>.

import (
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/surface/view"
	"github.com/bdubs00/featherweight-workbench/conversations"
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
// narrow the list as on GET /v1/runs. A run that the caller renamed
// carries myTitle, and a run that the caller hid stays out, unless hidden
// is true; then it carries hidden: true (package conversations).
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
	parent := q.Get("parent")
	if parent != "" {
		runs, err = dashboard.Children(r.Context(), s.a, parent)
		if errors.Is(err, fw.ErrRunNotFound) && fleet.IsArchived(r.Context(), s.a, parent) {
			// The parent lives in the archive alone, so its children do
			// too.
			runs, err = nil, nil
		}
	} else {
		runs, err = dashboard.Runs(r.Context(), s.a, q.Get("scope"), q.Get("member"))
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	// The runs that no member holds come from the archive (section 4.18),
	// where the lists leave out the same runs.
	archived := map[string]bool{}
	if q.Get("scope") != dashboard.ScopeLocal {
		old, err := fleet.ArchivedRuns(r.Context(), s.a, q.Get("member"), parent)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		old = fleet.ListedOnly(old)
		have := make(map[string]bool, len(runs))
		for _, run := range runs {
			have[run.ID] = true
		}
		for _, run := range old {
			if !have[run.ID] {
				archived[run.ID] = true
				runs = append(runs, run)
			}
		}
		if len(archived) > 0 {
			view.SortRuns(runs)
		}
	}
	// A run of the archive that never ended is lost: the pool replaced its
	// worker, so it neither works nor waits. A filter by state leaves it
	// out, and the glance does not count it.
	lost := map[string]bool{}
	for _, run := range runs {
		if archived[run.ID] && !run.State.Terminal() {
			lost[run.ID] = true
		}
	}
	matched := view.FilterBatch(view.Filter(runs, q.Get("agent"), q.Get("state"), q.Get("principal"), 0), q.Get("batch"))
	if len(lost) > 0 && strings.TrimSpace(q.Get("state")) != "" {
		matched = slices.DeleteFunc(matched, func(run *fw.Run) bool { return lost[run.ID] })
	}
	// The titles of the caller, and the runs that the caller hid, which
	// the list leaves out unless the query asks for them (section 4.20).
	matched, marks := conversations.Mark(r.Context(), s.a, matched, q.Get("hidden") == "true")
	counted := matched
	if len(lost) > 0 {
		counted = slices.DeleteFunc(slices.Clone(matched), func(run *fw.Run) bool { return lost[run.ID] })
	}
	glance, total := view.GlanceOf(counted, time.Now()), len(matched)
	if limit > 0 && len(matched) > limit {
		matched = matched[:limit]
	}
	var ids []string
	for _, run := range matched {
		if run.State.Terminal() {
			ids = append(ids, run.ID)
		}
	}
	ends := fleet.EndedAt(r.Context(), s.a, ids)
	list := make([]any, 0, len(matched))
	for _, run := range matched {
		mark, marked := marks[run.ID]
		end, ended := ends[run.ID]
		switch {
		case marked || ended:
			l := listedRun{Run: run, Archived: archived[run.ID], Lost: lost[run.ID], MyTitle: mark.MyTitle, Hidden: mark.Hidden}
			if ended {
				l.EndedAt = &end
			}
			list = append(list, l)
		case archived[run.ID]:
			list = append(list, archivedRun{Run: run, Archived: true, Lost: lost[run.ID]})
		default:
			list = append(list, run)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"runs": list, "total": total, "glance": glance})
}

// archivedRun is a run of the archive in an answer: the run object with
// "archived": true, and "lost": true for a run that never ended.
type archivedRun struct {
	*fw.Run
	Archived bool `json:"archived"`
	Lost     bool `json:"lost,omitempty"`
}

// listedRun is a run of a list with what the front adds to the run
// object: archived, the title of the caller in myTitle, hidden, and
// endedAt, the time of the newest fw.run.finished of a run that ended.
type listedRun struct {
	*fw.Run
	Archived bool       `json:"archived,omitempty"`
	Lost     bool       `json:"lost,omitempty"`
	MyTitle  string     `json:"myTitle,omitempty"`
	Hidden   bool       `json:"hidden,omitempty"`
	EndedAt  *time.Time `json:"endedAt,omitempty"`
}
