package notify

// This file serves the notifications of the caller on the native surface
// (docs/ARCHITECTURE.md section 23.2). Each route calls the Go function
// of settings.go that its MCP tool calls.

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/bdubs00/featherweight-kernel/surface/native"
	"github.com/bdubs00/featherweight-workbench/manager"
)

// HeaderTimeZone names the time zone of the browser of the caller, the
// default time zone of the quiet hours.
const HeaderTimeZone = "X-Time-Zone"

type routes struct {
	k *native.Kit
	s *Service
}

// route is one route of the notifications: its pattern, its handler, and
// the MCP tool that calls the same Go function.
type route struct {
	pattern string
	handle  func(rt *routes, w http.ResponseWriter, r *http.Request)
	tool    string
}

// table lists the routes of the notifications. A test of parity reads it.
var table = []route{
	{pattern: "GET /v1/notifications", handle: (*routes).get, tool: ToolGet},
	{pattern: "PUT /v1/notifications", handle: (*routes).put, tool: ToolPut},
	{pattern: "POST /v1/notifications/test", handle: (*routes).test, tool: ToolTest},
}

// mount is the extension of the native surface. A process without the
// notifications mounts no route.
func mount(k *native.Kit) error {
	s := Of(k.Agent())
	if s == nil {
		return nil
	}
	rt := &routes{k: k, s: s}
	for _, x := range table {
		h := func(w http.ResponseWriter, r *http.Request) { x.handle(rt, w, r) }
		k.Handle(x.pattern, h)
	}
	return nil
}

// problemsDoc is the problem document of a body that does not fit, with
// each problem in words.
type problemsDoc struct {
	native.Problem
	Problems []string `json:"problems"`
}

// fail maps an error of the notifications to an answer.
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
	case errors.Is(err, ErrUpstream):
		native.WriteJSON(w, http.StatusBadGateway, map[string]string{"error": strings.TrimPrefix(err.Error(), ErrUpstream.Error()+": ")})
	default:
		rt.k.Fail(w, r, err)
	}
}

// get answers GET /v1/notifications with the settings of the caller.
func (rt *routes) get(w http.ResponseWriter, r *http.Request) {
	ctx, _, _ := manager.OnBehalf(r.Context())
	ans, err := rt.s.Get(ctx)
	if err != nil {
		rt.fail(w, r, err)
		return
	}
	native.WriteJSON(w, http.StatusOK, ans)
}

// put answers PUT /v1/notifications with the settings after the write.
func (rt *routes) put(w http.ResponseWriter, r *http.Request) {
	ctx, _, _ := manager.OnBehalf(r.Context())
	var body Body
	if !rt.k.Decode(w, r, &body) {
		return
	}
	ans, err := rt.s.Put(ctx, body, strings.TrimSpace(r.Header.Get(HeaderTimeZone)))
	if err != nil {
		rt.fail(w, r, err)
		return
	}
	native.WriteJSON(w, http.StatusOK, ans)
}

// test answers POST /v1/notifications/test with 200 {"sent": true}, or
// 502 {"error": "..."} with the reason in words.
func (rt *routes) test(w http.ResponseWriter, r *http.Request) {
	ctx, _, _ := manager.OnBehalf(r.Context())
	if err := rt.s.Test(ctx); err != nil {
		rt.fail(w, r, err)
		return
	}
	native.WriteJSON(w, http.StatusOK, map[string]bool{"sent": true})
}
