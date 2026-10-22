package workers

// This file serves the pools and the templates on the native surface
// (docs/ARCHITECTURE.md section 14.10), behind the authentication of the
// native API.

import (
	"errors"
	"net/http"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/surface/native"
	"github.com/bdubs00/featherweight-workbench/fleet"
	"github.com/bdubs00/featherweight-workbench/manager"
	"github.com/bdubs00/featherweight-workbench/pools"
)

type routes struct {
	k *native.Kit
	a *fw.Agent
}

// mount is the extension of the native surface. A process whose fleet
// keeps no pool and no template mounts no route.
func mount(k *native.Kit) error {
	if !fleet.Pooled(k.Agent()) {
		return nil
	}
	rt := &routes{k: k, a: k.Agent()}
	k.Handle("GET /v1/pools", rt.list)
	k.Handle("POST /v1/pools", rt.create)
	k.Handle("GET /v1/pools/{name}", rt.get)
	k.Handle("PATCH /v1/pools/{name}", rt.patch)
	k.Handle("DELETE /v1/pools/{name}", rt.remove)
	k.Handle("GET /v1/templates", rt.templates)
	return nil
}

// fail maps an error of the fleet to a problem document.
func (rt *routes) fail(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, fleet.ErrPoolNotFound):
		native.WriteProblem(w, r, http.StatusNotFound, cause(err))
	case errors.Is(err, fleet.ErrPoolExists), errors.Is(err, fleet.ErrPoolConflict):
		native.WriteProblem(w, r, http.StatusConflict, cause(err))
	default:
		rt.k.Fail(w, r, err)
	}
}

// list answers GET /v1/pools with {"pools": [...]}: the pools of the file
// and of the store that the caller can view, each with its state, its
// source, and its template.
func (rt *routes) list(w http.ResponseWriter, r *http.Request) {
	ctx, _, _ := manager.OnBehalf(r.Context())
	list, _, err := fleet.ListPools(ctx, rt.a)
	if err != nil {
		rt.fail(w, r, err)
		return
	}
	native.WriteJSON(w, http.StatusOK, map[string]any{"pools": list})
}

// get answers GET /v1/pools/{name} with one pool of the list.
func (rt *routes) get(w http.ResponseWriter, r *http.Request) {
	ctx, _, _ := manager.OnBehalf(r.Context())
	list, _, err := fleet.ListPools(ctx, rt.a)
	if err != nil {
		rt.fail(w, r, err)
		return
	}
	for _, st := range list {
		if st.Name == r.PathValue("name") {
			native.WriteJSON(w, http.StatusOK, st)
			return
		}
	}
	native.WriteProblem(w, r, http.StatusNotFound, "no pool that you can view is named "+r.PathValue("name"))
}

// create answers POST /v1/pools: the body is {name, template, agent,
// replicas, env, resources} (fleet.PoolRequest), and the answer is 201
// with the pool and its Location.
func (rt *routes) create(w http.ResponseWriter, r *http.Request) {
	var req fleet.PoolRequest
	if !rt.k.Decode(w, r, &req) {
		return
	}
	ctx, _, _ := manager.OnBehalf(r.Context())
	st, err := fleet.CreatePool(ctx, rt.a, req)
	if err != nil {
		rt.fail(w, r, err)
		return
	}
	w.Header().Set("Location", "/v1/pools/"+st.Name)
	native.WriteJSON(w, http.StatusCreated, st)
}

// patch answers PATCH /v1/pools/{name}: the body holds replicas, env,
// resources, and template, each optional (fleet.PoolPatch), and the answer
// is 200 with the pool.
func (rt *routes) patch(w http.ResponseWriter, r *http.Request) {
	var c fleet.PoolPatch
	if !rt.k.Decode(w, r, &c) {
		return
	}
	ctx, _, _ := manager.OnBehalf(r.Context())
	st, err := fleet.PatchPool(ctx, rt.a, r.PathValue("name"), c)
	if err != nil {
		rt.fail(w, r, err)
		return
	}
	native.WriteJSON(w, http.StatusOK, st)
}

// remove answers DELETE /v1/pools/{name} with 202 and the pool: the front
// deletes its Tasks once they are idle, and then the pool.
func (rt *routes) remove(w http.ResponseWriter, r *http.Request) {
	ctx, _, _ := manager.OnBehalf(r.Context())
	st, err := fleet.DeletePool(ctx, rt.a, r.PathValue("name"))
	if err != nil {
		rt.fail(w, r, err)
		return
	}
	native.WriteJSON(w, http.StatusAccepted, st)
}

// templates answers GET /v1/templates with {"templates": [...]}: the name,
// the default mark, the atespace, the model, and the generation of each
// template, and no image, variable, or egress rule.
func (rt *routes) templates(w http.ResponseWriter, r *http.Request) {
	list := fleet.TemplatesOf(rt.a)
	if list == nil {
		list = []pools.TemplateInfo{}
	}
	native.WriteJSON(w, http.StatusOK, map[string]any{"templates": list})
}
