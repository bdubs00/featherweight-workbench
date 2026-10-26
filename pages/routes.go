package pages

// This file serves the wiki on the native surface (docs/ARCHITECTURE.md
// section 15.3). The two routes that are not pages never collide with one:
// no page has the path search, so /v1/pages/search is the search, and no
// page path ends with history, so a path that ends with /history names the
// history of the page before it (CheckPath).

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/surface/native"
)

// historySuffix ends the path of the history route of a page.
const historySuffix = "/history"

// routes serves the wiki of one agent.
type routes struct {
	k *native.Kit
	a *fw.Agent
}

// mount is the extension of the native surface. A process without a wiki
// mounts no route.
func mount(k *native.Kit) error {
	if Of(k.Agent()) == nil {
		return nil
	}
	rt := &routes{k: k, a: k.Agent()}
	k.Handle("GET /v1/pages", rt.list)
	k.Handle("GET /v1/pages/search", rt.search)
	k.Handle("GET /v1/pages/removed", rt.removed)
	k.Handle("GET /v1/pages/{path...}", rt.get)
	k.Handle("PUT /v1/pages/{path...}", rt.put)
	k.Handle("DELETE /v1/pages/{path...}", rt.remove)
	return nil
}

// store returns the wiki and checks the caller, or writes the problem.
func (rt *routes) store(w http.ResponseWriter, r *http.Request, write bool) (*Store, bool) {
	s := Of(rt.a)
	if s == nil {
		native.WriteProblem(w, r, http.StatusServiceUnavailable, "the wiki is off in this process")
		return nil, false
	}
	if err := Allowed(r.Context(), rt.a, write); err != nil {
		rt.k.Fail(w, r, err)
		return nil, false
	}
	return s, true
}

// failStore maps an error of the store to a problem document. A
// condition that does not hold answers 412, and when the page exists the
// problem carries its hash in the member hash and in the header ETag, so a
// client reads the page again and merges.
func (rt *routes) failStore(w http.ResponseWriter, r *http.Request, err error) {
	var e *Error
	switch {
	case errors.Is(err, ErrConflict):
		hash := ""
		if errors.As(err, &e) {
			hash = e.Hash
		}
		if hash != "" {
			w.Header().Set("ETag", strconv.Quote(hash))
		}
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusPreconditionFailed)
		_ = json.NewEncoder(w).Encode(conflictProblem{
			Problem: native.Problem{Type: "about:blank", Title: http.StatusText(http.StatusPreconditionFailed), Status: http.StatusPreconditionFailed, Detail: Reason(err), Instance: r.URL.Path},
			Hash:    hash,
		})
	case errors.Is(err, ErrNotFound):
		native.WriteProblem(w, r, http.StatusNotFound, Reason(err))
	case errors.Is(err, ErrLimit):
		native.WriteProblem(w, r, http.StatusRequestEntityTooLarge, Reason(err))
	case errors.Is(err, ErrInvalid):
		native.WriteProblem(w, r, http.StatusBadRequest, Reason(err))
	default:
		rt.k.Fail(w, r, err)
	}
}

// conflictProblem is the problem document of a 412, with the hash of the
// page as it is, when it exists.
type conflictProblem struct {
	native.Problem
	Hash string `json:"hash,omitempty"`
}

// list answers GET /v1/pages?prefix= with every page without its text,
// sorted by path, as {"pages": [...]}. With prefix, only the pages whose
// path starts with that text, as the tool pages_list takes it.
func (rt *routes) list(w http.ResponseWriter, r *http.Request) {
	s, ok := rt.store(w, r, false)
	if !ok {
		return
	}
	infos, err := s.List(r.Context())
	if err != nil {
		rt.failStore(w, r, err)
		return
	}
	if prefix := r.URL.Query().Get("prefix"); prefix != "" {
		got := []PageInfo{}
		for _, info := range infos {
			if strings.HasPrefix(info.Path, prefix) {
				got = append(got, info)
			}
		}
		infos = got
	}
	native.WriteJSON(w, http.StatusOK, map[string]any{"pages": infos})
}

// removed answers GET /v1/pages/removed with the pages that were removed
// and whose history the wiki keeps, the newest removal first, as
// {"removed": [{"path", "title", "version", "removedAt"}]}. version is the
// newest version with a text, which GET /v1/pages/{path}?version= reads
// for a restore. A path is never removed, because a page path ends with
// .md.
func (rt *routes) removed(w http.ResponseWriter, r *http.Request) {
	s, ok := rt.store(w, r, false)
	if !ok {
		return
	}
	list, err := s.Removed(r.Context())
	if err != nil {
		rt.failStore(w, r, err)
		return
	}
	native.WriteJSON(w, http.StatusOK, map[string]any{"removed": list})
}

// search answers GET /v1/pages/search?q=&limit= with the pages that hold
// every word of q, best first.
func (rt *routes) search(w http.ResponseWriter, r *http.Request) {
	s, ok := rt.store(w, r, false)
	if !ok {
		return
	}
	q := r.URL.Query()
	limit := 0
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			native.WriteProblem(w, r, http.StatusBadRequest, "the query parameter limit must be a positive number")
			return
		}
		limit = n
	}
	found, err := s.Find(r.Context(), q.Get("q"), limit)
	if err != nil {
		rt.failStore(w, r, err)
		return
	}
	native.WriteJSON(w, http.StatusOK, found)
}

// get answers GET /v1/pages/{path}, and the history route
// GET /v1/pages/{path}/history.
//
// A page answers as JSON with its hash in the header ETag. A client that
// accepts text/markdown or text/plain and not JSON gets the text alone.
// ?version= reads a version that the history names. If-None-Match with the
// hash answers 304.
func (rt *routes) get(w http.ResponseWriter, r *http.Request) {
	p := r.PathValue("path")
	if p == "" {
		rt.list(w, r)
		return
	}
	if before, ok := strings.CutSuffix(p, historySuffix); ok && strings.HasSuffix(before, ".md") {
		rt.history(w, r, before)
		return
	}
	s, ok := rt.store(w, r, false)
	if !ok {
		return
	}
	var version uint64
	if v := r.URL.Query().Get("version"); v != "" {
		n, err := strconv.ParseUint(v, 10, 64)
		if err != nil || n == 0 {
			native.WriteProblem(w, r, http.StatusBadRequest, "the query parameter version must be a version that the history of the page names")
			return
		}
		version = n
	}
	page, err := s.Read(r.Context(), p, version)
	if err != nil {
		rt.failStore(w, r, err)
		return
	}
	etag := strconv.Quote(page.Hash)
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "private, no-cache")
	if matchesETag(r.Header.Get("If-None-Match"), page.Hash) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	if !native.Accepts(r, "application/json") && (native.Accepts(r, "text/markdown") || native.Accepts(r, "text/plain")) {
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, page.Text)
		return
	}
	native.WriteJSON(w, http.StatusOK, page)
}

// history answers GET /v1/pages/{path}/history with the versions of the
// page, newest first.
func (rt *routes) history(w http.ResponseWriter, r *http.Request, p string) {
	s, ok := rt.store(w, r, false)
	if !ok {
		return
	}
	versions, err := s.History(r.Context(), p)
	if err != nil {
		rt.failStore(w, r, err)
		return
	}
	native.WriteJSON(w, http.StatusOK, map[string]any{"path": p, "versions": versions})
}

// putBody is the JSON body of PUT /v1/pages/{path}.
type putBody struct {
	Text    *string `json:"text"`
	Summary string  `json:"summary"`
}

// put answers PUT /v1/pages/{path} by writing the page.
//
// The body is the JSON object {"text": "...", "summary": "..."}. If-Match
// with the hash of the page, quoted or weak, makes the write a
// compare-and-swap, If-Match: * writes only a page that exists, and
// If-None-Match: * only a new page. Each answers 412 when the page
// differs, with the hash of the page in the member hash of the problem and
// in the header ETag when the page exists. A new page answers 201 with
// Location, and a changed one 200, each with the page and its new hash in
// ETag.
func (rt *routes) put(w http.ResponseWriter, r *http.Request) {
	s, ok := rt.store(w, r, true)
	if !ok {
		return
	}
	p := r.PathValue("path")
	if err := checkWritable(p); err != nil {
		rt.failStore(w, r, err)
		return
	}
	ifHash, ok := condition(w, r)
	if !ok {
		return
	}
	text, summary, ok := rt.body(w, r, s)
	if !ok {
		return
	}
	by, run := writer(r.Context())
	page, did, err := s.write(r.Context(), p, text, by, run, summary, ifHash)
	if err != nil {
		rt.failStore(w, r, err)
		return
	}
	if did != unchanged {
		rt.a.Logger().Info("pages: page written", "path", p, "by", by, "version", page.Version, "size", page.Size, "sha256", page.Hash)
	}
	w.Header().Set("ETag", strconv.Quote(page.Hash))
	status := http.StatusOK
	if did == created {
		w.Header().Set("Location", "/v1/pages/"+escapePath(p))
		status = http.StatusCreated
	}
	native.WriteJSON(w, status, page)
}

// body reads the text and the summary of a write from the JSON body.
func (rt *routes) body(w http.ResponseWriter, r *http.Request, s *Store) (string, string, bool) {
	// JSON escapes a byte in at most six.
	limit := 6*int64(s.cfg.MaxPageBytes) + 4096
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limit+1))
	var tooBig *http.MaxBytesError
	switch {
	case errors.As(err, &tooBig) || int64(len(data)) > limit:
		native.WriteProblem(w, r, http.StatusRequestEntityTooLarge, fmt.Sprintf("a page holds at most %d KB", s.cfg.MaxPageBytes>>10))
		return "", "", false
	case err != nil:
		native.WriteProblem(w, r, http.StatusBadRequest, "the body cannot be read: "+err.Error())
		return "", "", false
	}
	var b putBody
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&b); err != nil {
		native.WriteProblem(w, r, http.StatusBadRequest, "the body is not valid: "+err.Error()+`; send the JSON object {"text": "...", "summary": "..."}`)
		return "", "", false
	}
	if dec.More() {
		native.WriteProblem(w, r, http.StatusBadRequest, `the body holds data after the JSON object; send {"text": "...", "summary": "..."}`)
		return "", "", false
	}
	if b.Text == nil {
		native.WriteProblem(w, r, http.StatusBadRequest, `the body has no text; send the JSON object {"text": "...", "summary": "..."}`)
		return "", "", false
	}
	return *b.Text, b.Summary, true
}

// condition reads If-Match and If-None-Match into the condition of a write.
func condition(w http.ResponseWriter, r *http.Request) (string, bool) {
	match := strings.TrimSpace(r.Header.Get("If-Match"))
	none := strings.TrimSpace(r.Header.Get("If-None-Match"))
	switch {
	case match != "" && none != "":
		native.WriteProblem(w, r, http.StatusBadRequest, "send If-Match or If-None-Match, not both")
		return "", false
	case none == "*":
		return IfAbsent, true
	case none != "":
		native.WriteProblem(w, r, http.StatusBadRequest, "a write takes If-None-Match: * alone, which writes only a new page")
		return "", false
	case match == "":
		return IfAny, true
	case match == "*":
		return IfExists, true
	}
	if strings.Contains(match, ",") {
		native.WriteProblem(w, r, http.StatusBadRequest, "If-Match takes one hash")
		return "", false
	}
	// An entity tag is quoted, also a weak one. The store checks that it
	// holds a hash, so an empty tag never turns into no condition.
	if !strings.HasPrefix(strings.TrimPrefix(match, "W/"), `"`) {
		native.WriteProblem(w, r, http.StatusBadRequest, `If-Match takes the hash of the page in quotes, such as "<hash>", or *`)
		return "", false
	}
	return match, true
}

// escapePath escapes each part of a page path for a URL.
func escapePath(p string) string {
	parts := strings.Split(p, "/")
	for i, part := range parts {
		parts[i] = url.PathEscape(part)
	}
	return strings.Join(parts, "/")
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

// remove answers DELETE /v1/pages/{path} with 204. If-Match makes the
// removal a compare-and-swap.
func (rt *routes) remove(w http.ResponseWriter, r *http.Request) {
	s, ok := rt.store(w, r, true)
	if !ok {
		return
	}
	p := r.PathValue("path")
	ifHash, ok := condition(w, r)
	if !ok {
		return
	}
	by, _ := writer(r.Context())
	if err := s.Delete(r.Context(), p, by, ifHash); err != nil {
		rt.failStore(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
