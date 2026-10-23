package bridges

// This file serves the bridges on the native surface (docs/ARCHITECTURE.md
// section 18.10), in the shapes of the contract of the page of bossmode.
// Every route needs the authentication of the surface. A bridge is for its
// operator and for anyone with the operator role on every agent of its
// members (allows).

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/surface/native"
	"github.com/bdubs00/featherweight-workbench/fleet"
)

// routes serves the bridges of one agent.
type routes struct {
	k *native.Kit
	a *fw.Agent
}

// mount is the extension of the native surface. A process without the
// bridges mounts no route.
func mount(k *native.Kit) error {
	if Of(k.Agent()) == nil {
		return nil
	}
	rt := &routes{k: k, a: k.Agent()}
	k.Handle("GET /v1/bridges", rt.list)
	k.Handle("POST /v1/bridges", rt.open)
	k.Handle("GET /v1/bridges/{id}", rt.get)
	k.Handle("PATCH /v1/bridges/{id}", rt.patch)
	k.Handle("PUT /v1/bridges/{id}/read", rt.read)
	k.Handle("GET /v1/bridges/{id}/messages", rt.messages)
	k.Handle("POST /v1/bridges/{id}/messages", rt.post)
	k.Handle("GET /v1/bridges/{id}/messages/{seq}", rt.entry)
	k.Handle("GET /v1/bridges/{id}/pages", rt.pages)
	k.Handle("POST /v1/bridges/{id}/messages/{seq}/reactions", rt.react)
	k.Handle("POST /v1/bridges/{id}/members", rt.addMember)
	k.Handle("DELETE /v1/bridges/{id}/members/{handle}", rt.fire)
	k.Handle("PATCH /v1/bridges/{id}/members/{handle}", rt.patchMember)
	return nil
}

// store returns the bridges, or writes the problem.
func (rt *routes) store(w http.ResponseWriter, r *http.Request) (*Store, bool) {
	s := Of(rt.a)
	if s == nil {
		native.WriteProblem(w, r, http.StatusServiceUnavailable, "the bridges are off in this process")
		return nil, false
	}
	s.learn(fw.PrincipalFrom(r.Context()))
	return s, true
}

// fail answers an error of the package with its status.
func (rt *routes) fail(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		native.WriteProblem(w, r, http.StatusNotFound, detail(err))
	case errors.Is(err, ErrConflict):
		native.WriteProblem(w, r, http.StatusConflict, detail(err))
	case errors.Is(err, ErrTooLarge):
		native.WriteProblem(w, r, http.StatusRequestEntityTooLarge, detail(err))
	case errors.Is(err, ErrUnavailable):
		native.WriteProblem(w, r, http.StatusBadGateway, detail(err))
	case errors.Is(err, fw.ErrInvalidRequest):
		native.WriteProblem(w, r, http.StatusBadRequest, detail(err))
	case errors.Is(err, fleet.ErrNoRoute):
		native.WriteProblem(w, r, http.StatusBadGateway, detail(err))
	default:
		rt.k.Fail(w, r, err)
	}
}

// detail is the words of an error for a problem document, without the
// names of the packages and the kinds of error in front.
func detail(err error) string {
	s := err.Error()
	for changed := true; changed; {
		changed = false
		for _, prefix := range []string{"bridges: not found: ", "bridges: conflict: ", "bridges: too large: ", "bridges: unavailable: ", "fw: invalid request: ", "skills: ", "bridges: ", "fleet: "} {
			if strings.HasPrefix(s, prefix) {
				s, changed = strings.TrimPrefix(s, prefix), true
			}
		}
	}
	return s
}

// bridge reads the bridge of the path and checks the caller.
func (rt *routes) bridge(w http.ResponseWriter, r *http.Request) (*Store, *record, bool) {
	s, ok := rt.store(w, r)
	if !ok {
		return nil, nil, false
	}
	rec, err := s.load(r.Context(), r.PathValue("id"))
	if err != nil {
		rt.fail(w, r, err)
		return nil, nil, false
	}
	return s, rec, true
}

// answer writes a bridge with its members for the caller.
func (rt *routes) answer(w http.ResponseWriter, r *http.Request, s *Store, rec *record, status int, usage bool) {
	it, err := s.item(r.Context(), rec, subjectOf(r.Context()), true)
	if err != nil {
		rt.fail(w, r, err)
		return
	}
	if usage {
		u, err := s.usage(r.Context(), rec.ID)
		if err != nil {
			rt.fail(w, r, err)
			return
		}
		it.Usage = &u
	}
	native.WriteJSON(w, status, it)
}

// list answers GET /v1/bridges with the bridges of the caller and of the
// teams of the caller, open first. An admin of the process lists every
// bridge that it can see.
func (rt *routes) list(w http.ResponseWriter, r *http.Request) {
	s, ok := rt.store(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	recs, err := s.b.list(s.asFront(ctx))
	if err != nil {
		rt.fail(w, r, err)
		return
	}
	admin := s.isAdmin(ctx)
	items := []Item{}
	for _, rec := range recs {
		ok, err := s.listed(ctx, rec, admin)
		if err != nil {
			rt.fail(w, r, err)
			return
		}
		if !ok {
			continue
		}
		it, err := s.item(ctx, rec, subjectOf(ctx), false)
		if err != nil {
			rt.fail(w, r, err)
			return
		}
		items = append(items, it)
	}
	sortItems(items)
	native.WriteJSON(w, http.StatusOK, map[string]any{"bridges": items})
}

// open answers POST /v1/bridges: 201 with the record and its members.
func (rt *routes) open(w http.ResponseWriter, r *http.Request) {
	s, ok := rt.store(w, r)
	if !ok {
		return
	}
	var spec OpenSpec
	if !rt.k.Decode(w, r, &spec) {
		return
	}
	rec, err := s.open(r.Context(), spec, fw.PrincipalFrom(r.Context()), nil)
	if err != nil {
		rt.fail(w, r, err)
		return
	}
	w.Header().Set("Location", "/v1/bridges/"+rec.ID)
	rt.answer(w, r, s, rec, http.StatusCreated, false)
}

// get answers GET /v1/bridges/{id}: the record with the presence of each
// member and the usage of the runs of the bridge.
func (rt *routes) get(w http.ResponseWriter, r *http.Request) {
	s, rec, ok := rt.bridge(w, r)
	if !ok {
		return
	}
	rt.answer(w, r, s, rec, http.StatusOK, true)
}

// patch answers PATCH /v1/bridges/{id}: the title, the delegation and the
// caps in part, and the state.
func (rt *routes) patch(w http.ResponseWriter, r *http.Request) {
	s, rec, ok := rt.bridge(w, r)
	if !ok {
		return
	}
	var p BridgePatch
	if !rt.k.Decode(w, r, &p) {
		return
	}
	rec, closed, err := s.patch(r.Context(), rec.ID, p, fw.PrincipalFrom(r.Context()))
	if err != nil {
		rt.fail(w, r, err)
		return
	}
	if closed {
		s.askReport(r.Context(), rec)
	}
	rt.answer(w, r, s, rec, http.StatusOK, false)
}

// read answers PUT /v1/bridges/{id}/read with 204: it moves the read mark
// of the caller. A smaller seq than the mark leaves the mark.
func (rt *routes) read(w http.ResponseWriter, r *http.Request) {
	s, rec, ok := rt.bridge(w, r)
	if !ok {
		return
	}
	var body struct {
		Seq *uint64 `json:"seq"`
	}
	if !rt.k.Decode(w, r, &body) {
		return
	}
	if body.Seq == nil {
		native.WriteProblem(w, r, http.StatusBadRequest, "the body needs seq, the newest entry that the person read")
		return
	}
	seq := min(*body.Seq, rec.LastSeq)
	if err := s.b.setRead(s.asFront(r.Context()), rec.ID, subjectOf(r.Context()), seq); err != nil {
		rt.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// messages answers GET /v1/bridges/{id}/messages?before=&from=&limit= with
// one page of the channel, as the transcript route pages a run.
// entry answers GET /v1/bridges/{id}/messages/{seq} with one entry of the
// channel and its whole text (wholeEntry): text holds the whole text, up
// to 64 KB, and cut stays true only when the front holds no more than the
// start of the entry. The page reads it to show a long entry whole.
func (rt *routes) entry(w http.ResponseWriter, r *http.Request) {
	s, rec, ok := rt.bridge(w, r)
	if !ok {
		return
	}
	seq, err := strconv.ParseUint(r.PathValue("seq"), 10, 64)
	if err != nil || seq == 0 {
		native.WriteProblem(w, r, http.StatusBadRequest, "the seq of an entry is a positive whole number")
		return
	}
	m, text, whole, err := s.wholeEntry(r.Context(), rec, seq)
	if err != nil {
		rt.fail(w, r, err)
		return
	}
	m.Text, m.Cut = text, !whole
	native.WriteJSON(w, http.StatusOK, m)
}

// pages answers GET /v1/bridges/{id}/pages with the pages of the wiki that
// belong to the work of the bridge (pagesOf). The caller needs the rule of
// the bridge and the rule of the wiki.
func (rt *routes) pages(w http.ResponseWriter, r *http.Request) {
	s, rec, ok := rt.bridge(w, r)
	if !ok {
		return
	}
	if err := s.readsWiki(r.Context()); err != nil {
		rt.fail(w, r, err)
		return
	}
	list, err := s.pagesOf(r.Context(), rec)
	if err != nil {
		rt.fail(w, r, err)
		return
	}
	native.WriteJSON(w, http.StatusOK, BridgePages{Bridge: rec.ID, Pages: list, Count: len(list)})
}

func (rt *routes) messages(w http.ResponseWriter, r *http.Request) {
	s, rec, ok := rt.bridge(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	num := func(name string) (uint64, bool) {
		v := q.Get(name)
		if v == "" {
			return 0, true
		}
		n, err := strconv.ParseUint(v, 10, 64)
		if err != nil {
			native.WriteProblem(w, r, http.StatusBadRequest, "the query parameter "+name+" must be a whole number")
			return 0, false
		}
		return n, true
	}
	from, ok1 := num("from")
	before, ok2 := num("before")
	limit, ok3 := num("limit")
	if !ok1 || !ok2 || !ok3 {
		return
	}
	if q.Get("limit") != "" && limit == 0 {
		native.WriteProblem(w, r, http.StatusBadRequest, "the query parameter limit must be a positive number")
		return
	}
	page, err := s.b.page(s.asFront(r.Context()), rec, from, before, int(min(limit, MaxPageLimit)))
	if err != nil {
		rt.fail(w, r, err)
		return
	}
	native.WriteJSON(w, http.StatusOK, page)
}

// post answers POST /v1/bridges/{id}/messages: 201 with the entry of a
// message of the caller, which fans out in the background.
func (rt *routes) post(w http.ResponseWriter, r *http.Request) {
	s, rec, ok := rt.bridge(w, r)
	if !ok {
		return
	}
	var spec PostSpec
	if !rt.k.DecodeFiles(w, r, &spec, func() int { return len(spec.Files) }) {
		return
	}
	m, err := s.postAs(r.Context(), rec.ID, spec)
	if err != nil {
		rt.fail(w, r, err)
		return
	}
	native.WriteJSON(w, http.StatusCreated, m)
}

// Post posts a message of the caller in ctx to the channel of a bridge, as
// POST /v1/bridges/{id}/messages does: the caller must be able to use the
// bridge, and a spec with Skill makes the message a slash command. The
// tool skill_send of the package bossmode posts through it. It returns an
// error that wraps ErrNotFound for an unknown bridge or skill, ErrConflict
// for a closed bridge, ErrTooLarge for a slash command over its bound, and
// fw.ErrInvalidRequest or fw.ErrForbidden.
func (s *Store) Post(ctx context.Context, id string, spec PostSpec) (Message, error) {
	rec, err := s.load(ctx, id)
	if err != nil {
		return Message{}, err
	}
	return s.postAs(ctx, rec.ID, spec)
}

// Detail is the words of an error of the package without the names of the
// packages and the kinds of error in front, for a tool result.
func Detail(err error) string { return detail(err) }

// postAs posts a message of the caller in ctx, a person.
func (s *Store) postAs(ctx context.Context, id string, spec PostSpec) (Message, error) {
	p := fw.PrincipalFrom(ctx)
	snd := sender{subject: subjectOfPrincipal(p)}
	if p != nil {
		snd.display = p.Name
	}
	return s.post(ctx, id, snd, spec)
}

// react answers POST /v1/bridges/{id}/messages/{seq}/reactions with the
// entry after the reaction of the caller.
func (rt *routes) react(w http.ResponseWriter, r *http.Request) {
	s, rec, ok := rt.bridge(w, r)
	if !ok {
		return
	}
	seq, err := strconv.ParseUint(r.PathValue("seq"), 10, 64)
	if err != nil || seq == 0 {
		native.WriteProblem(w, r, http.StatusBadRequest, "the seq of an entry is a positive whole number")
		return
	}
	var spec ReactSpec
	if !rt.k.Decode(w, r, &spec) {
		return
	}
	m, err := s.reactEntry(r.Context(), rec.ID, seq, spec, fw.PrincipalFrom(r.Context()))
	if err != nil {
		rt.fail(w, r, err)
		return
	}
	native.WriteJSON(w, http.StatusOK, m)
}

// addMember answers POST /v1/bridges/{id}/members: 201 with the new seat.
func (rt *routes) addMember(w http.ResponseWriter, r *http.Request) {
	s, rec, ok := rt.bridge(w, r)
	if !ok {
		return
	}
	var spec SeatSpec
	if !rt.k.Decode(w, r, &spec) {
		return
	}
	m, err := s.addMember(r.Context(), rec.ID, spec, fw.PrincipalFrom(r.Context()))
	if err != nil {
		rt.fail(w, r, err)
		return
	}
	native.WriteJSON(w, http.StatusCreated, m)
}

// fire answers DELETE /v1/bridges/{id}/members/{handle}: 200 with the
// fired seat.
func (rt *routes) fire(w http.ResponseWriter, r *http.Request) {
	s, rec, ok := rt.bridge(w, r)
	if !ok {
		return
	}
	m, err := s.fire(r.Context(), rec.ID, r.PathValue("handle"), fw.PrincipalFrom(r.Context()))
	if err != nil {
		rt.fail(w, r, err)
		return
	}
	native.WriteJSON(w, http.StatusOK, m)
}

// patchMember answers PATCH /v1/bridges/{id}/members/{handle}: muted, a
// new task, or both.
func (rt *routes) patchMember(w http.ResponseWriter, r *http.Request) {
	s, rec, ok := rt.bridge(w, r)
	if !ok {
		return
	}
	var p MemberPatch
	if !rt.k.Decode(w, r, &p) {
		return
	}
	m, err := s.patchMember(r.Context(), rec.ID, r.PathValue("handle"), p, fw.PrincipalFrom(r.Context()))
	if err != nil {
		rt.fail(w, r, err)
		return
	}
	native.WriteJSON(w, http.StatusOK, m)
}

// usage sums the usage of the runs of a bridge as the tool batch sums a
// job (fleet.UsageSum): every run of the batch of this process and of the
// view, with the partial mark when a run reports no usage, a usage is not
// final, or the view forgot or does not know runs of the batch.
func (s *Store) usage(ctx context.Context, id string) (fleet.UsageSum, error) {
	fctx := s.asFront(ctx)
	var u fleet.UsageSum
	forgotten, unknown := 0, 0
	batch := BatchOf(id)
	local, err := s.a.List(fctx)
	if err != nil {
		return u, err
	}
	for _, r := range local {
		if r.Metadata[fw.MetaBatch] == batch {
			u.Add(r)
		}
	}
	if v := fleet.Of(s.a); v != nil {
		if bv, ok := v.(fleet.BatchView); ok {
			b, err := bv.Batch(fctx, batch)
			if err != nil {
				return u, err
			}
			for _, r := range b.Runs {
				u.Add(r)
			}
			forgotten, unknown = b.Forgotten, b.Unknown
		} else {
			runs, err := s.runsOf(fctx, id)
			if err != nil {
				return u, err
			}
			for _, r := range runs {
				if r.Member != "" {
					u.Add(r)
				}
			}
		}
	}
	u.Partial = u.Missing > 0 || u.Final < u.Reported || forgotten > 0 || unknown > 0
	return u, nil
}
