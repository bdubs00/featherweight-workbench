package duties

// This file serves the duties on the native surface (docs/ARCHITECTURE.md
// section 22.7), behind the authentication of the native API, but for the
// call of a webhook, which checks the credential of its duty (call.go).
// Each route calls the Go function of ops.go that its MCP tool calls.

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/surface/native"
	"github.com/bdubs00/featherweight-workbench/manager"
)

// HeaderTimeZone names the time zone of the browser of the caller, the
// default time zone of a new schedule.
const HeaderTimeZone = "X-Time-Zone"

type routes struct {
	k *native.Kit
	s *Service
}

// route is one route of the duties: its pattern, its handler, and the MCP
// tool that calls the same Go function, or why no tool does.
type route struct {
	pattern string
	handle  func(rt *routes, w http.ResponseWriter, r *http.Request)
	tool    string
	public  bool
	why     string
}

// table lists the routes of the duties. A test of parity reads it: each
// route has a tool or a reason.
var table = []route{
	{pattern: "GET /v1/duties", handle: (*routes).list, tool: ToolList},
	{pattern: "PATCH /v1/duties", handle: (*routes).pauseAll, tool: ToolPauseAll},
	{pattern: "GET /v1/duties/options", handle: (*routes).options,
		why: "the page reads it to offer only what the front serves; duty_put with dryRun names each choice that the front does not serve, and duty_write_check names the looks and the secrets to the agent"},
	{pattern: "GET /v1/duties/{name}", handle: (*routes).get, tool: ToolGet},
	{pattern: "PUT /v1/duties/{name}", handle: (*routes).put, tool: ToolPut},
	{pattern: "PATCH /v1/duties/{name}", handle: (*routes).patch, tool: ToolPause},
	{pattern: "DELETE /v1/duties/{name}", handle: (*routes).remove, tool: ToolDelete},
	{pattern: "POST /v1/duties/{name}/run", handle: (*routes).runNow, tool: ToolRunNow},
	{pattern: "POST /v1/duties/{name}/write-check", handle: (*routes).writeCheck, tool: ToolWriteCheck},
	{pattern: "POST /v1/duties/{name}/call", handle: (*routes).call, public: true,
		why: "another app calls it with the credential of the webhook of the duty; a person or an agent uses duty_run_now"},
}

// mount is the extension of the native surface. A process without duties
// mounts no route.
func mount(k *native.Kit) error {
	s := Of(k.Agent())
	if s == nil {
		return nil
	}
	rt := &routes{k: k, s: s}
	for _, x := range table {
		h := func(w http.ResponseWriter, r *http.Request) { x.handle(rt, w, r) }
		if x.public {
			k.HandlePublic(x.pattern, h)
		} else {
			k.Handle(x.pattern, h)
		}
	}
	return nil
}

// call answers the call of the webhook of a duty (call.go).
func (rt *routes) call(w http.ResponseWriter, r *http.Request) { rt.s.serveCall(w, r) }

// problemsDoc is the problem document of a body that does not fit, with
// each problem in words.
type problemsDoc struct {
	native.Problem
	Problems []string `json:"problems"`
}

// fail maps an error of the duties to a problem document.
func (rt *routes) fail(w http.ResponseWriter, r *http.Request, err error) {
	var pe *ProblemsError
	switch {
	case errors.As(err, &pe):
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(problemsDoc{
			Problem:  native.Problem{Type: "about:blank", Title: http.StatusText(http.StatusBadRequest), Status: http.StatusBadRequest, Detail: pe.Error(), Instance: r.URL.Path},
			Problems: pe.Problems,
		})
	case errors.Is(err, ErrHidden), errors.Is(err, ErrNotFound):
		native.WriteProblem(w, r, http.StatusNotFound, reason(err))
	case errors.Is(err, ErrConflict), errors.Is(err, ErrExists), errors.Is(err, ErrReadOnly):
		native.WriteProblem(w, r, http.StatusConflict, reason(err))
	case errors.Is(err, ErrUpstream):
		native.WriteProblem(w, r, http.StatusBadGateway, reason(err))
	case errors.Is(err, ErrNoStarts):
		native.WriteProblem(w, r, http.StatusServiceUnavailable, reason(err))
	case errors.Is(err, ErrNoAnswer):
		writeCheckFailure(w, r, http.StatusGatewayTimeout, reason(err), nil)
	default:
		rt.k.Fail(w, r, err)
	}
}

// checkProblem is the problem document of a check that the agent did not
// write, with the reason in error too, and the record and its readback
// when the agent wrote one.
type checkProblem struct {
	native.Problem
	Error      string      `json:"error"`
	CheckFirst *CheckFirst `json:"checkFirst,omitempty"`
	Readback   string      `json:"readback,omitempty"`
}

// writeCheckFailure writes the problem document of a write of a check
// that failed.
func writeCheckFailure(w http.ResponseWriter, r *http.Request, status int, reason string, ce *CheckError) {
	doc := checkProblem{
		Problem: native.Problem{Type: "about:blank", Title: http.StatusText(status), Status: status, Detail: reason, Instance: r.URL.Path},
		Error:   reason,
	}
	if ce != nil {
		doc.CheckFirst, doc.Readback = ce.CheckFirst, ce.Readback
	}
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(doc)
}

// reason returns the words of an error of the package without its prefix.
func reason(err error) string {
	s := err.Error()
	for _, e := range []error{ErrHidden, ErrNotFound, ErrConflict, ErrExists, ErrReadOnly, ErrUpstream, ErrNoStarts, ErrNoAnswer, fw.ErrInvalidRequest, fw.ErrForbidden} {
		s = strings.TrimPrefix(s, e.Error()+": ")
	}
	return capital(strings.TrimPrefix(s, "duties: "))
}

// list answers GET /v1/duties with {"duties": [...], "paused": false}.
func (rt *routes) list(w http.ResponseWriter, r *http.Request) {
	ctx, _, _ := manager.OnBehalf(r.Context())
	ans, err := rt.s.List(ctx)
	if err != nil {
		rt.fail(w, r, err)
		return
	}
	native.WriteJSON(w, http.StatusOK, ans)
}

// get answers GET /v1/duties/{name} with the duty and its history.
func (rt *routes) get(w http.ResponseWriter, r *http.Request) {
	ctx, _, _ := manager.OnBehalf(r.Context())
	v, err := rt.s.Get(ctx, r.PathValue("name"))
	if err != nil {
		rt.fail(w, r, err)
		return
	}
	native.WriteJSON(w, http.StatusOK, v)
}

// put answers PUT /v1/duties/{name}: 201 with the duty for a new duty,
// else 200. ?dryRun=true saves nothing and answers {"duty", "readback",
// "next", "problems"}, and &test=true adds "check".
func (rt *routes) put(w http.ResponseWriter, r *http.Request) {
	ctx, _, _ := manager.OnBehalf(r.Context())
	var body DutyBody
	if !rt.k.Decode(w, r, &body) {
		return
	}
	q := r.URL.Query()
	opt := PutOptions{DryRun: isTrue(q.Get("dryRun")), Test: isTrue(q.Get("test")), Zone: strings.TrimSpace(r.Header.Get(HeaderTimeZone))}
	if opt.Test && !opt.DryRun {
		native.WriteProblem(w, r, http.StatusBadRequest, "test runs with dryRun=true only")
		return
	}
	name := r.PathValue("name")
	ans, err := rt.s.Put(ctx, name, body, opt)
	if err != nil {
		rt.fail(w, r, err)
		return
	}
	switch {
	case ans.Dry != nil:
		native.WriteJSON(w, http.StatusOK, ans.Dry)
	case ans.Created:
		w.Header().Set("Location", "/v1/duties/"+name)
		native.WriteJSON(w, http.StatusCreated, ans.View)
	default:
		native.WriteJSON(w, http.StatusOK, ans.View)
	}
}

// isTrue reads a switch of a query.
func isTrue(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "true", "1", "yes", "on":
		return true
	}
	return false
}

// pausedBody is the body of PATCH /v1/duties/{name}.
type pausedBody struct {
	Paused *bool `json:"paused"`
}

// patch answers PATCH /v1/duties/{name} with the duty after the pause or
// the resume.
func (rt *routes) patch(w http.ResponseWriter, r *http.Request) {
	ctx, _, _ := manager.OnBehalf(r.Context())
	var body pausedBody
	if !rt.k.Decode(w, r, &body) {
		return
	}
	if body.Paused == nil {
		native.WriteProblem(w, r, http.StatusBadRequest, "the body is {\"paused\": true} or {\"paused\": false}, and paused is missing")
		return
	}
	v, err := rt.s.SetPaused(ctx, r.PathValue("name"), *body.Paused)
	if err != nil {
		rt.fail(w, r, err)
		return
	}
	native.WriteJSON(w, http.StatusOK, v)
}

// remove answers DELETE /v1/duties/{name} with 204.
func (rt *routes) remove(w http.ResponseWriter, r *http.Request) {
	ctx, _, _ := manager.OnBehalf(r.Context())
	if err := rt.s.Delete(ctx, r.PathValue("name")); err != nil {
		rt.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// runNow answers POST /v1/duties/{name}/run: 202 with the run, or with
// the reason why the start waits; 200 with the reason why it skipped; and
// 502 when the check or the queue failed.
func (rt *routes) runNow(w http.ResponseWriter, r *http.Request) {
	ctx, _, _ := manager.OnBehalf(r.Context())
	out, err := rt.s.RunNow(ctx, r.PathValue("name"))
	if err != nil {
		rt.fail(w, r, err)
		return
	}
	writeOutcome(w, r, out)
}

// writeOutcome writes the answer of a start.
func writeOutcome(w http.ResponseWriter, r *http.Request, out Outcome) {
	switch {
	case out.Run != "":
		native.WriteJSON(w, http.StatusAccepted, map[string]string{"run": out.Run})
	case out.Waiting != "":
		native.WriteJSON(w, http.StatusAccepted, map[string]string{"waiting": out.Waiting})
	case out.Failed != "":
		native.WriteProblem(w, r, http.StatusBadGateway, out.Failed)
	case out.Duplicate:
		native.WriteJSON(w, http.StatusOK, map[string]bool{"duplicate": true})
	case out.Skipped != "":
		native.WriteJSON(w, http.StatusOK, map[string]string{"skipped": out.Skipped})
	default:
		native.WriteJSON(w, http.StatusAccepted, map[string]string{"run": ""})
	}
}

// options answers GET /v1/duties/options with what the page can offer:
// {"secrets": [{"name", "hosts"}], "events", "actions", "looks"}.
func (rt *routes) options(w http.ResponseWriter, r *http.Request) {
	native.WriteJSON(w, http.StatusOK, rt.s.Options())
}

// writeCheck answers POST /v1/duties/{name}/write-check: 200 with
// {"checkFirst", "readback", "check", "run"}, 422 with the reason in
// error when the agent wrote no check that works, and 504 when it did
// not answer in time.
func (rt *routes) writeCheck(w http.ResponseWriter, r *http.Request) {
	ctx, _, _ := manager.OnBehalf(r.Context())
	var body WriteCheckBody
	if !rt.k.Decode(w, r, &body) {
		return
	}
	ans, err := rt.s.WriteCheck(ctx, r.PathValue("name"), body)
	var ce *CheckError
	switch {
	case errors.As(err, &ce):
		writeCheckFailure(w, r, http.StatusUnprocessableEntity, ce.Reason, ce)
	case err != nil:
		rt.fail(w, r, err)
	default:
		native.WriteJSON(w, http.StatusOK, ans)
	}
}

// switchBody is the body of PATCH /v1/duties.
type switchBody struct {
	Paused *bool  `json:"paused"`
	Reason string `json:"reason,omitempty"`
}

// pauseAll answers PATCH /v1/duties, for an admin, with the switch.
func (rt *routes) pauseAll(w http.ResponseWriter, r *http.Request) {
	ctx, _, _ := manager.OnBehalf(r.Context())
	var body switchBody
	if !rt.k.Decode(w, r, &body) {
		return
	}
	if body.Paused == nil {
		native.WriteProblem(w, r, http.StatusBadRequest, "the body is {\"paused\": true, \"reason\": \"...\"} or {\"paused\": false}, and paused is missing")
		return
	}
	sw, err := rt.s.PauseAll(ctx, *body.Paused, body.Reason)
	if err != nil {
		rt.fail(w, r, err)
		return
	}
	native.WriteJSON(w, http.StatusOK, sw)
}
