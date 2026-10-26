package conversations

// This file serves PATCH /v1/conversations/{run} on the native surface
// (docs/ARCHITECTURE.md section 4.20), behind the authentication of the
// native API. The route calls Store.Patch, which the tool
// conversation_patch calls too.

import (
	"errors"
	"net/http"
	"strings"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/surface/native"
)

type routes struct {
	k *native.Kit
	a *fw.Agent
}

// mount is the extension of the native surface. It mounts the route in
// every process, and each request reads whether the process keeps a
// store, because the surface can mount before the service opens it.
func mount(k *native.Kit) error {
	rt := &routes{k: k, a: k.Agent()}
	k.Handle("PATCH /v1/conversations/{run}", rt.patch)
	return nil
}

// patch answers PATCH /v1/conversations/{run} with 200 and the answer of
// the patch: the run, the title of the caller, and hidden.
func (rt *routes) patch(w http.ResponseWriter, r *http.Request) {
	var p Patch
	if !rt.k.Decode(w, r, &p) {
		return
	}
	ans, err := Of(rt.a).Patch(r.Context(), r.PathValue("run"), p)
	if err != nil {
		fail(w, r, rt.k, err)
		return
	}
	native.WriteJSON(w, http.StatusOK, ans)
}

// fail maps an error of the package to a problem document.
func fail(w http.ResponseWriter, r *http.Request, k *native.Kit, err error) {
	switch {
	case errors.Is(err, ErrOff):
		native.WriteProblem(w, r, http.StatusServiceUnavailable, reason(err))
	case errors.Is(err, ErrNoPerson):
		native.WriteProblem(w, r, http.StatusForbidden, reason(err))
	case errors.Is(err, fw.ErrInvalidRequest):
		native.WriteProblem(w, r, http.StatusBadRequest, reason(err))
	case errors.Is(err, fw.ErrRunNotFound):
		native.WriteProblem(w, r, http.StatusNotFound, reason(err))
	default:
		k.Fail(w, r, err)
	}
}

// reason returns the words of an error without the names of its kinds.
func reason(err error) string {
	s := err.Error()
	for _, p := range []string{ErrOff.Error() + ": ", ErrNoPerson.Error() + ": ", fw.ErrInvalidRequest.Error() + ": ", fw.ErrRunNotFound.Error() + ": ", "conversations: "} {
		s = strings.ReplaceAll(s, p, "")
	}
	return s
}
