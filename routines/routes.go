package routines

// This file serves the routines on the native surface
// (docs/ARCHITECTURE.md section 21.6), behind the authentication of the
// native API.

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/surface/native"
	"github.com/bdubs00/featherweight-workbench/manager"
)

type routes struct {
	k *native.Kit
	a *fw.Agent
}

// mount is the extension of the native surface. A process without
// routines mounts no route.
func mount(k *native.Kit) error {
	if Of(k.Agent()) == nil {
		return nil
	}
	rt := &routes{k: k, a: k.Agent()}
	k.Handle("GET /v1/routines", rt.list)
	k.Handle("GET /v1/routines/summary", rt.summary)
	k.Handle("GET /v1/routines/{agent}/{name}", rt.get)
	k.Handle("PUT /v1/routines/{agent}/{name}", rt.put)
	k.Handle("POST /v1/routines/{agent}/{name}/standing", rt.standing)
	return nil
}

// moveProblem is the problem document of a move that the table forbids,
// with the standings that the routine can move to.
type moveProblem struct {
	native.Problem
	Standing string   `json:"standing"`
	Allowed  []string `json:"allowed"`
}

// fail maps an error of the routines to a problem document.
func (rt *routes) fail(w http.ResponseWriter, r *http.Request, err error) {
	var me *MoveError
	switch {
	case errors.As(err, &me):
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(moveProblem{
			Problem:  native.Problem{Type: "about:blank", Title: http.StatusText(http.StatusConflict), Status: http.StatusConflict, Detail: me.Error(), Instance: r.URL.Path},
			Standing: me.From,
			Allowed:  me.Allowed,
		})
	case errors.Is(err, ErrHidden), errors.Is(err, ErrNotFound):
		native.WriteProblem(w, r, http.StatusNotFound, reason(err))
	case errors.Is(err, ErrInvalid):
		native.WriteProblem(w, r, http.StatusBadRequest, reason(err))
	case errors.Is(err, ErrLimit), errors.Is(err, ErrConflict), errors.Is(err, ErrRetired):
		native.WriteProblem(w, r, http.StatusConflict, reason(err))
	default:
		rt.k.Fail(w, r, err)
	}
}

// agentParam returns the agent of the query, or writes the problem.
func agentParam(w http.ResponseWriter, r *http.Request) (string, bool) {
	agent := strings.TrimSpace(r.URL.Query().Get("agent"))
	if agent == "" {
		native.WriteProblem(w, r, http.StatusBadRequest, "name the agent with ?agent=")
		return "", false
	}
	return agent, true
}

// list answers GET /v1/routines?agent= with the routines of the agent,
// without evidence, the newest change first.
func (rt *routes) list(w http.ResponseWriter, r *http.Request) {
	ctx, _, _ := manager.OnBehalf(r.Context())
	agent, ok := agentParam(w, r)
	if !ok {
		return
	}
	if err := allowed(ctx, rt.a, agent, fw.ActionAgentView); err != nil {
		rt.fail(w, r, err)
		return
	}
	list, err := Of(rt.a).List(ctx, agent)
	if err != nil {
		rt.fail(w, r, err)
		return
	}
	native.WriteJSON(w, http.StatusOK, map[string]any{"routines": list})
}

// get answers GET /v1/routines/{agent}/{name} with the record and its
// evidence.
func (rt *routes) get(w http.ResponseWriter, r *http.Request) {
	ctx, _, _ := manager.OnBehalf(r.Context())
	agent, name := r.PathValue("agent"), r.PathValue("name")
	if err := allowed(ctx, rt.a, agent, fw.ActionAgentView); err != nil {
		rt.fail(w, r, err)
		return
	}
	rec, err := Of(rt.a).Get(ctx, agent, name)
	if err != nil {
		rt.fail(w, r, err)
		return
	}
	native.WriteJSON(w, http.StatusOK, rec)
}

// put answers PUT /v1/routines/{agent}/{name}: 201 with the record for a
// new routine, else 200 with the record.
func (rt *routes) put(w http.ResponseWriter, r *http.Request) {
	ctx, _, _ := manager.OnBehalf(r.Context())
	var in putBody
	if !rt.k.Decode(w, r, &in) {
		return
	}
	agent, name := r.PathValue("agent"), r.PathValue("name")
	rec, res, err := put(ctx, rt.a, Of(rt.a), agent, name, in)
	if err != nil {
		rt.fail(w, r, err)
		return
	}
	if res.Changed {
		rt.a.Logger().Info("routines: a routine was written", "agent", agent, "routine", name, "version", rec.Version(), "standing", rec.Standing, "created", res.Created)
	}
	status := http.StatusOK
	if res.Created {
		status = http.StatusCreated
		w.Header().Set("Location", "/v1/routines/"+agent+"/"+name)
	}
	native.WriteJSON(w, status, rec)
}

// standing answers POST /v1/routines/{agent}/{name}/standing with the
// record after the move, or 409 with the standings that the routine can
// move to.
func (rt *routes) standing(w http.ResponseWriter, r *http.Request) {
	ctx, _, _ := manager.OnBehalf(r.Context())
	var in moveBody
	if !rt.k.Decode(w, r, &in) {
		return
	}
	rec, _, err := move(ctx, rt.a, Of(rt.a), r.PathValue("agent"), r.PathValue("name"), in)
	if err != nil {
		rt.fail(w, r, err)
		return
	}
	native.WriteJSON(w, http.StatusOK, rec)
}

// summary answers GET /v1/routines/summary?agent=&days= with the summary
// of the agent over the last days, 7 by default and 90 at most.
func (rt *routes) summary(w http.ResponseWriter, r *http.Request) {
	ctx, _, _ := manager.OnBehalf(r.Context())
	agent, ok := agentParam(w, r)
	if !ok {
		return
	}
	days := DefaultSummaryDays
	if v := strings.TrimSpace(r.URL.Query().Get("days")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > MaxSummaryDays {
			native.WriteProblem(w, r, http.StatusBadRequest, "days is a whole number from 1 to 90")
			return
		}
		days = n
	}
	if err := allowed(ctx, rt.a, agent, fw.ActionAgentView); err != nil {
		rt.fail(w, r, err)
		return
	}
	out, err := SummaryOf(ctx, rt.a, agent, days)
	if err != nil {
		rt.fail(w, r, err)
		return
	}
	native.WriteJSON(w, http.StatusOK, out)
}
