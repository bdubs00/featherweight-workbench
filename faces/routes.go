package faces

// This file serves the faces on the native surface (docs/ARCHITECTURE.md
// section 16.3).

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/surface/native"
)

// svgPolicy is the content security policy of a face that the front
// serves: the face loads nothing and runs nothing, also when a browser
// opens it on its own.
const svgPolicy = "default-src 'none'; style-src 'unsafe-inline'; sandbox"

type routes struct {
	k *native.Kit
	a *fw.Agent
}

// mount is the extension of the native surface. A process without faces
// mounts no route.
func mount(k *native.Kit) error {
	if Of(k.Agent()) == nil {
		return nil
	}
	rt := &routes{k: k, a: k.Agent()}
	k.Handle("GET /v1/faces/{agent}", rt.get)
	k.Handle("PUT /v1/faces/{agent}", rt.put)
	k.Handle("DELETE /v1/faces/{agent}", rt.remove)
	k.Handle("PATCH /v1/faces/{agent}", rt.patch)
	return nil
}

// store returns the faces and the name of the agent of the request, or
// writes the problem.
func (rt *routes) store(w http.ResponseWriter, r *http.Request) (*Store, string, bool) {
	agent := r.PathValue("agent")
	if err := CheckAgent(agent); err != nil {
		native.WriteProblem(w, r, http.StatusBadRequest, strings.TrimPrefix(err.Error(), ErrInvalid.Error()+": "))
		return nil, "", false
	}
	s := Of(rt.a)
	if s == nil {
		native.WriteProblem(w, r, http.StatusServiceUnavailable, "the faces are off in this process")
		return nil, "", false
	}
	return s, agent, true
}

// fail maps an error of the faces to a problem document.
func (rt *routes) fail(w http.ResponseWriter, r *http.Request, err error) {
	var ref *Refusal
	switch {
	case errors.As(err, &ref):
		native.WriteProblem(w, r, http.StatusUnprocessableEntity, ref.Reason)
	case errors.Is(err, ErrInvalid):
		native.WriteProblem(w, r, http.StatusBadRequest, strings.TrimPrefix(err.Error(), ErrInvalid.Error()+": "))
	case errors.Is(err, ErrConflict):
		native.WriteProblem(w, r, http.StatusConflict, strings.TrimPrefix(err.Error(), ErrConflict.Error()+": "))
	default:
		rt.k.Fail(w, r, err)
	}
}

// get answers GET /v1/faces/{agent} with the face as image/svg+xml: the
// stored face, else the identicon, with its hash in ETag. A caller that
// cannot view the agent, and an agent that the fleet does not run, get the
// identicon, which tells nothing about the agent. A client that accepts
// application/json and not image/svg+xml gets the face without the SVG:
// the hash, whether it is stored, the display name, and the tagline.
func (rt *routes) get(w http.ResponseWriter, r *http.Request) {
	s, agent, ok := rt.store(w, r)
	if !ok {
		return
	}
	f := faceOf(agent, record{}, nil, 0)
	if err := Check(r.Context(), rt.a, agent, fw.ActionAgentView); err == nil {
		got, err := s.Get(r.Context(), agent)
		if err != nil {
			rt.fail(w, r, err)
			return
		}
		f = got
	}
	w.Header().Set("ETag", strconv.Quote(f.Hash))
	w.Header().Set("Cache-Control", "private, no-cache")
	w.Header().Set("Vary", "Accept")
	if matchesETag(r.Header.Get("If-None-Match"), f.Hash) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	if native.Accepts(r, "application/json") && !native.Accepts(r, "image/svg+xml") {
		native.WriteJSON(w, http.StatusOK, f)
		return
	}
	w.Header().Set("Content-Type", "image/svg+xml")
	w.Header().Set("Content-Security-Policy", svgPolicy)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(f.SVG)
}

// put answers PUT /v1/faces/{agent}: the body is an SVG of at most 32 KB.
// It needs the owner role on the agent (fw.ActionAgentEdit). A face that
// the sanitizer refuses answers 422 with the reason, and the agent keeps
// the face that it had. The answer is the face without the SVG.
func (rt *routes) put(w http.ResponseWriter, r *http.Request) {
	s, agent, ok := rt.store(w, r)
	if !ok {
		return
	}
	if err := Check(r.Context(), rt.a, agent, fw.ActionAgentEdit); err != nil {
		rt.fail(w, r, err)
		return
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxSVGBytes+1))
	var tooBig *http.MaxBytesError
	switch {
	case errors.As(err, &tooBig) || len(data) > MaxSVGBytes:
		native.WriteProblem(w, r, http.StatusRequestEntityTooLarge, fmt.Sprintf("a face holds at most %d KB", MaxSVGBytes>>10))
		return
	case err != nil:
		native.WriteProblem(w, r, http.StatusBadRequest, "the body cannot be read: "+err.Error())
		return
	}
	by := writer(r.Context())
	f, err := s.Put(r.Context(), agent, data, by)
	if err != nil {
		rt.fail(w, r, err)
		return
	}
	rt.a.Logger().Info("faces: face set", "agent", agent, "by", by, "sha256", f.Hash, "size", len(f.SVG))
	w.Header().Set("ETag", strconv.Quote(f.Hash))
	native.WriteJSON(w, http.StatusOK, f)
}

// remove answers DELETE /v1/faces/{agent} with 204: the agent has its
// identicon again.
func (rt *routes) remove(w http.ResponseWriter, r *http.Request) {
	s, agent, ok := rt.store(w, r)
	if !ok {
		return
	}
	if err := Check(r.Context(), rt.a, agent, fw.ActionAgentEdit); err != nil {
		rt.fail(w, r, err)
		return
	}
	by := writer(r.Context())
	if err := s.Delete(r.Context(), agent, by); err != nil {
		rt.fail(w, r, err)
		return
	}
	rt.a.Logger().Info("faces: face removed", "agent", agent, "by", by)
	w.WriteHeader(http.StatusNoContent)
}

// patchBody is the body of PATCH /v1/faces/{agent}. A field that is absent
// stays as it is, and an empty one clears it.
type patchBody struct {
	DisplayName *string `json:"displayName"`
	Tagline     *string `json:"tagline"`
}

// patch answers PATCH /v1/faces/{agent} with the face after the change of
// its display name, at most 64 characters, and its tagline, at most 140.
func (rt *routes) patch(w http.ResponseWriter, r *http.Request) {
	s, agent, ok := rt.store(w, r)
	if !ok {
		return
	}
	if err := Check(r.Context(), rt.a, agent, fw.ActionAgentEdit); err != nil {
		rt.fail(w, r, err)
		return
	}
	var b patchBody
	if !rt.k.Decode(w, r, &b) {
		return
	}
	f, err := s.Patch(r.Context(), agent, b.DisplayName, b.Tagline, writer(r.Context()))
	if err != nil {
		rt.fail(w, r, err)
		return
	}
	native.WriteJSON(w, http.StatusOK, f)
}

// matchesETag reports whether an If-None-Match header names the hash.
func matchesETag(header, hash string) bool {
	for v := range strings.SplitSeq(header, ",") {
		v = strings.TrimSpace(v)
		if v == "*" || strings.Trim(strings.TrimPrefix(v, "W/"), `"`) == hash {
			return true
		}
	}
	return false
}
