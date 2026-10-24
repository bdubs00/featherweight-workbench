package playbooks

// This file serves the playbooks on the native surface
// (docs/ARCHITECTURE.md section 24.2), behind the authentication of the
// native API. Each route calls the Go function of playbooks.go that its
// MCP tool calls.

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/surface/native"
	"github.com/bdubs00/featherweight-workbench/manager"
)

type routes struct {
	k *native.Kit
	s *Service
}

// mount is the extension of the native surface. A process without
// playbooks mounts no route.
func mount(k *native.Kit) error {
	s := Of(k.Agent())
	if s == nil {
		return nil
	}
	rt := &routes{k: k, s: s}
	k.Handle("GET /v1/playbooks", rt.list)
	k.Handle("GET /v1/playbooks/{name}", rt.get)
	k.Handle("PUT /v1/playbooks/{name}", rt.put)
	k.Handle("DELETE /v1/playbooks/{name}", rt.remove)
	return nil
}

// problemsDoc is the problem document of a playbook that does not fit,
// with each problem in words.
type problemsDoc struct {
	native.Problem
	Problems []string `json:"problems"`
}

// fail maps an error of the playbooks to a problem document.
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
	case errors.Is(err, ErrNotFound):
		native.WriteProblem(w, r, http.StatusNotFound, reason(err))
	case errors.Is(err, ErrConflict):
		native.WriteProblem(w, r, http.StatusConflict, reason(err))
	case errors.Is(err, ErrOff):
		native.WriteProblem(w, r, http.StatusServiceUnavailable, reason(err))
	default:
		rt.k.Fail(w, r, err)
	}
}

// reason returns the words of an error without the names of the package
// and the kinds of error in front.
func reason(err error) string {
	s := err.Error()
	for _, e := range []error{ErrNotFound, ErrConflict, ErrOff, fw.ErrForbidden, fw.ErrInvalidRequest} {
		s = strings.TrimPrefix(s, e.Error()+": ")
	}
	s = strings.TrimPrefix(s, "playbooks: ")
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// list answers GET /v1/playbooks with {"playbooks": [...]}.
func (rt *routes) list(w http.ResponseWriter, r *http.Request) {
	ctx, _, _ := manager.OnBehalf(r.Context())
	list, err := rt.s.List(ctx)
	if err != nil {
		rt.fail(w, r, err)
		return
	}
	native.WriteJSON(w, http.StatusOK, map[string]any{"playbooks": list})
}

// get answers GET /v1/playbooks/{name} with the playbook.
func (rt *routes) get(w http.ResponseWriter, r *http.Request) {
	ctx, _, _ := manager.OnBehalf(r.Context())
	v, err := rt.s.Get(ctx, r.PathValue("name"))
	if err != nil {
		rt.fail(w, r, err)
		return
	}
	native.WriteJSON(w, http.StatusOK, v)
}

// put answers PUT /v1/playbooks/{name}: 201 with the playbook and its
// Location for a new playbook, else 200 with the playbook.
func (rt *routes) put(w http.ResponseWriter, r *http.Request) {
	ctx, _, _ := manager.OnBehalf(r.Context())
	var body Body
	if !rt.k.Decode(w, r, &body) {
		return
	}
	name := r.PathValue("name")
	v, created, err := rt.s.Put(ctx, name, body)
	if err != nil {
		rt.fail(w, r, err)
		return
	}
	if created {
		w.Header().Set("Location", "/v1/playbooks/"+name)
		native.WriteJSON(w, http.StatusCreated, v)
		return
	}
	native.WriteJSON(w, http.StatusOK, v)
}

// remove answers DELETE /v1/playbooks/{name} with 204.
func (rt *routes) remove(w http.ResponseWriter, r *http.Request) {
	ctx, _, _ := manager.OnBehalf(r.Context())
	if err := rt.s.Delete(ctx, r.PathValue("name")); err != nil {
		rt.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
