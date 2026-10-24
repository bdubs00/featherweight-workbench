package notify

// This file serves the notifications of the caller on the native surface
// (docs/ARCHITECTURE.md section 23.2). Each route calls the Go function
// of settings.go that its MCP tool calls.

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
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
	case errors.Is(err, ErrUnknownChannel):
		native.WriteProblem(w, r, http.StatusBadRequest, capitalize(err.Error())+".")
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

// testBody is the body of a test: the id of one channel, or nothing for
// every channel.
type testBody struct {
	Channel string `json:"channel"`
}

// maxTestBody bounds the body of a test.
const maxTestBody = 4 << 10

// test answers POST /v1/notifications/test with 200 {"results": [...]},
// one result per channel, or 400 for a channel that the caller does not
// have. A send that failed is a result with sent false and the reason.
func (rt *routes) test(w http.ResponseWriter, r *http.Request) {
	ctx, _, _ := manager.OnBehalf(r.Context())
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxTestBody))
	if err != nil {
		native.WriteProblem(w, r, http.StatusBadRequest, "The body does not read, or it is longer than 4 KB.")
		return
	}
	var body testBody
	if len(bytes.TrimSpace(raw)) > 0 {
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&body); err != nil {
			native.WriteProblem(w, r, http.StatusBadRequest, `The body is {"channel": "<id>"} for one channel, or empty for every channel.`)
			return
		}
	}
	ans, err := rt.s.Test(ctx, strings.TrimSpace(body.Channel))
	if err != nil {
		rt.fail(w, r, err)
		return
	}
	native.WriteJSON(w, http.StatusOK, ans)
}
