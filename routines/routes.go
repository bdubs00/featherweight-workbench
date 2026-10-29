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

// mount is the extension of the native surface. It mounts every route,
// and each request reads whether the process keeps routines at that
// moment, as the settings of bossmode read it (routines: true), because
// the surface can mount before the service of the routines opens its
// store. While the routines are off, GET /v1/routines answers 200 with
// off true and the reason, so a page reads the state and logs no failed
// request, and the other routes answer 404 with the reason.
func mount(k *native.Kit) error {
	rt := &routes{k: k, a: k.Agent()}
	k.Handle("GET /v1/routines", rt.list)
	k.Handle("GET /v1/routines/summary", rt.on(rt.summary))
	k.Handle("GET /v1/routines/now", rt.now)
	k.Handle("GET /v1/routines/{agent}/{name}", rt.on(rt.get))
	k.Handle("PUT /v1/routines/{agent}/{name}", rt.on(rt.put))
	k.Handle("POST /v1/routines/{agent}/{name}/standing", rt.on(rt.standing))
	return nil
}

// on answers a request with h while the process keeps routines, and with
// 404 and the reason while they are off.
func (rt *routes) on(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if Of(rt.a) == nil {
			native.WriteProblem(w, r, http.StatusNotFound, offReason(rt.a.Config()))
			return
		}
		h(w, r)
	}
}

// moveProblem is the problem document of a move that the table forbids,
// with the standings that the routine can move to.
type moveProblem struct {
	native.Problem
	Standing string   `json:"standing"`
	Allowed  []string `json:"allowed"`
}

// versionProblem is the problem document of a move of another version,
// with the version and the standing of the record.
type versionProblem struct {
	native.Problem
	Standing string `json:"standing"`
	Version  int    `json:"version"`
}

// fail maps an error of the routines to a problem document.
func (rt *routes) fail(w http.ResponseWriter, r *http.Request, err error) {
	var me *MoveError
	var ve *VersionError
	switch {
	case errors.As(err, &ve):
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(versionProblem{
			Problem:  native.Problem{Type: "about:blank", Title: http.StatusText(http.StatusConflict), Status: http.StatusConflict, Detail: ve.Error(), Instance: r.URL.Path},
			Standing: ve.Standing,
			Version:  ve.Version,
		})
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

// offReason says in words why the routines of the configuration cfg are
// off, for a person who reads the place Routines of the page.
func offReason(cfg *fw.Config) string {
	if c, err := Load(cfg); err == nil && !c.On() {
		return "Routines are off on this server. An admin turns them on with the setting workbench.routines.enabled."
	}
	return "Routines are off on this server, because its bus keeps no store for them. An admin runs the server with the bus nats or mem."
}

// list answers GET /v1/routines with the routines of every agent that the
// caller can view, or with ?agent= the routines of that agent, without
// evidence, the newest change first, and off false. While the process
// keeps no routines, it answers no routine, off true, and the reason.
func (rt *routes) list(w http.ResponseWriter, r *http.Request) {
	if Of(rt.a) == nil {
		native.WriteJSON(w, http.StatusOK, map[string]any{"routines": []Record{}, "off": true, "reason": offReason(rt.a.Config())})
		return
	}
	ctx, _, _ := manager.OnBehalf(r.Context())
	list, err := visible(ctx, rt.a, Of(rt.a), strings.TrimSpace(r.URL.Query().Get("agent")))
	if err != nil {
		rt.fail(w, r, err)
		return
	}
	native.WriteJSON(w, http.StatusOK, map[string]any{"routines": list, "off": false})
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
// of the agent over the last days, 7 by default and 30 at most.
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
			native.WriteProblem(w, r, http.StatusBadRequest, "days is a whole number from 1 to 30")
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
