package routines

// This file serves GET /v1/routines/now, the routines that a run of an
// agent applies now. A worker whose pool reaches the front reads them at
// the start of each run (routines.front, docs/ROUTINES.md section 3.1), so
// a move of a routine reaches the next run and replaces no Task of the
// pool. The answer has a version, the hash of the list, and the kernel
// sends the version that it holds: the route answers 304 when that list is
// current.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/surface/native"
	"github.com/bdubs00/featherweight-workbench/manager"
)

// Now returns the routines that a run of the agent applies now, in the
// shape that the kernel reads (fw.RoutineList): with Held, the routines of
// the store that reach a profile, each with the standing of the kernel,
// oldest first, also none, in place of the routines of the profile; without
// it, the store keeps no routine of the agent, or the routines are off, and
// the run takes the routines of its profile. Version is the hash of the
// answer. While the bus keeps a store and the service has not opened it
// yet, it answers errNotOpen, and the run applies no routine.
func Now(ctx context.Context, a *fw.Agent, agent string) (fw.RoutineList, error) {
	out := fw.RoutineList{List: []fw.Routine{}}
	if s := Of(a); s != nil {
		recs, err := s.List(ctx, agent)
		if err != nil {
			return fw.RoutineList{}, err
		}
		if len(recs) > 0 {
			out.Held, out.List = true, reachingOf(recs)
		}
	} else if _, err := reaching(ctx, a, nil); err != nil {
		return fw.RoutineList{}, err
	}
	b, err := json.Marshal(struct {
		Held bool         `json:"held"`
		List []fw.Routine `json:"routines"`
	}{out.Held, out.List})
	if err != nil {
		return fw.RoutineList{}, err
	}
	sum := sha256.Sum256(b)
	out.Version = hex.EncodeToString(sum[:8])
	return out, nil
}

// now answers GET /v1/routines/now?agent= with Now and the version as the
// ETag, or 304 when If-None-Match names the version. The static token of
// the fleet, which the workers hold, reads every agent; a person needs the
// right to view the agent.
func (rt *routes) now(w http.ResponseWriter, r *http.Request) {
	agent, ok := agentParam(w, r)
	if !ok {
		return
	}
	if err := CheckAgent(agent); err != nil {
		native.WriteProblem(w, r, http.StatusBadRequest, reason(err))
		return
	}
	ctx, _, _ := manager.OnBehalf(r.Context())
	if p := fw.PrincipalFrom(ctx); p != nil && p.Kind != fw.KindToken {
		if err := allowed(ctx, rt.a, agent, fw.ActionAgentView); err != nil {
			rt.fail(w, r, err)
			return
		}
	}
	out, err := Now(ctx, rt.a, agent)
	if errors.Is(err, errNotOpen) {
		native.WriteProblem(w, r, http.StatusServiceUnavailable, "The store of the routines is not open yet. Try again in a moment.")
		return
	}
	if err != nil {
		rt.fail(w, r, err)
		return
	}
	tag := strconv.Quote(out.Version)
	w.Header().Set("ETag", tag)
	for _, m := range strings.Split(r.Header.Get("If-None-Match"), ",") {
		if strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(m), "W/")) == tag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
	}
	native.WriteJSON(w, http.StatusOK, out)
}
