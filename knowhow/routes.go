package knowhow

// This file serves Know-how on the native surface (docs/KNOWHOW.md section
// 5.3), behind the authentication of the native API. Every route is
// mounted, and each request reads whether the process keeps Know-how at
// that moment, because the surface can mount before the service opens its
// store.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/surface/native"
	"github.com/bdubs00/featherweight-workbench/internal/bucket"
	"github.com/bdubs00/featherweight-workbench/manager"
	"github.com/bdubs00/featherweight-workbench/routines"
)

type routes struct {
	k *native.Kit
	a *fw.Agent
}

// offReason says why Know-how is off, for a person.
const offReason = "Know-how is off on this server, because its bus keeps no store for it. An admin runs the server with the bus nats or mem."

// mount is the extension of the native surface.
func mount(k *native.Kit) error {
	rt := &routes{k: k, a: k.Agent()}
	k.Handle("GET /v1/knowhow/notes", rt.on(rt.list))
	k.Handle("GET /v1/knowhow/notes/{agent}/{scope}/{name...}", rt.on(rt.get))
	k.Handle("PUT /v1/knowhow/notes/{agent}/{scope}/{name...}", rt.on(rt.put))
	k.Handle("DELETE /v1/knowhow/notes/{agent}/{scope}/{name...}", rt.on(rt.remove))
	k.Handle("GET /v1/knowhow/runs", rt.on(rt.runs))
	k.Handle("GET /v1/knowhow/journal", rt.on(rt.journal))
	k.Handle("POST /v1/knowhow/journal/{id}/undo", rt.on(rt.undo))
	k.Handle("GET /v1/knowhow/settings/{agent}", rt.on(rt.settings))
	k.Handle("PUT /v1/knowhow/settings/{agent}", rt.on(rt.putSettings))
	k.Handle("GET /v1/knowhow/waiting", rt.on(rt.waiting))
	k.Handle("POST /v1/knowhow/waiting/{id}", rt.on(rt.decide))
	k.Handle("GET /v1/knowhow/summary", rt.on(rt.summary))
	return nil
}

// on answers a request with h while the process keeps Know-how, and with
// 404 and the reason while it does not.
func (rt *routes) on(h func(w http.ResponseWriter, r *http.Request, s *Service)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s := Of(rt.a)
		if s == nil {
			native.WriteProblem(w, r, http.StatusNotFound, offReason)
			return
		}
		h(w, r, s)
	}
}

// conflictProblem is the problem document of a write whose condition did
// not hold, with the hash of the note as it is.
type conflictProblem struct {
	native.Problem
	Hash string `json:"hash,omitempty"`
}

// fail maps an error of Know-how to a problem document.
func (rt *routes) fail(w http.ResponseWriter, r *http.Request, err error) {
	var ce *ConflictError
	var me *routines.MoveError
	switch {
	case errors.As(err, &ce):
		if ce.Hash != "" {
			w.Header().Set("ETag", strconv.Quote(ce.Hash))
		}
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusPreconditionFailed)
		_ = json.NewEncoder(w).Encode(conflictProblem{
			Problem: native.Problem{Type: "about:blank", Title: http.StatusText(http.StatusPreconditionFailed), Status: http.StatusPreconditionFailed, Detail: ce.Error(), Instance: r.URL.Path},
			Hash:    ce.Hash,
		})
	case errors.Is(err, ErrHidden), errors.Is(err, ErrNotFound), errors.Is(err, routines.ErrNotFound):
		native.WriteProblem(w, r, http.StatusNotFound, reason(err))
	case errors.Is(err, ErrInvalid), errors.Is(err, routines.ErrInvalid):
		native.WriteProblem(w, r, http.StatusBadRequest, reason(err))
	case errors.Is(err, ErrPerson):
		native.WriteProblem(w, r, http.StatusForbidden, reason(err))
	case errors.As(err, &me), errors.Is(err, ErrLater), errors.Is(err, ErrBusy), errors.Is(err, routines.ErrConflict), errors.Is(err, routines.ErrRetired):
		native.WriteProblem(w, r, http.StatusConflict, reason(err))
	default:
		rt.k.Fail(w, r, err)
	}
}

// reason returns the words of an error without the prefix of its package.
func reason(err error) string {
	// A hidden thing gets one fixed sentence, which names neither the agent
	// nor the scope, so the answer says nothing about what exists.
	if errors.Is(err, ErrHidden) {
		return hiddenReason
	}
	msg := err.Error()
	for _, e := range []error{ErrInvalid, ErrNotFound, ErrLater, ErrBusy, fw.ErrInvalidRequest, fw.ErrForbidden, routines.ErrNotFound, routines.ErrInvalid} {
		msg = strings.TrimPrefix(msg, e.Error()+": ")
	}
	return strings.TrimPrefix(msg, "knowhow: ")
}

// hiddenReason is the detail of a 404 for a thing that the caller cannot
// view.
const hiddenReason = "Nothing that you can view has this name."

// agentParam returns the agent of the query, or writes the problem.
func agentParam(w http.ResponseWriter, r *http.Request) (string, bool) {
	agent := strings.TrimSpace(r.URL.Query().Get("agent"))
	if agent == "" {
		native.WriteProblem(w, r, http.StatusBadRequest, "name the agent with ?agent=")
		return "", false
	}
	return agent, true
}

// limitParam reads ?limit=, def when absent, at most max.
func limitParam(w http.ResponseWriter, r *http.Request, def, max int) (int, bool) {
	v := strings.TrimSpace(r.URL.Query().Get("limit"))
	if v == "" {
		return def, true
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 || n > max {
		native.WriteProblem(w, r, http.StatusBadRequest, "limit is a whole number from 1 to "+strconv.Itoa(max))
		return 0, false
	}
	return n, true
}

// scopeOf reads the scope of a request: the kind of the path or of
// ?scope=, and ?principal=. A person who names no principal of the scope
// principal means their own.
func scopeOf(r *http.Request, kind string, c caller) Scope {
	sc := Scope{Kind: strings.TrimSpace(kind), Principal: strings.TrimSpace(r.URL.Query().Get("principal"))}
	if sc.Kind == ScopePrincipal && sc.Principal == "" && !c.trusted {
		sc.Principal = c.subject
	}
	return sc
}

// list answers GET /v1/knowhow/notes?agent=&scope=&principal=: the notes
// that the caller can read, without their text. ?putAway=true adds the
// notes that were put away.
func (rt *routes) list(w http.ResponseWriter, r *http.Request, s *Service) {
	ctx, _, _ := manager.OnBehalf(r.Context())
	agent, ok := agentParam(w, r)
	if !ok {
		return
	}
	rg, err := s.rightsOn(ctx, agent)
	if err != nil {
		rt.fail(w, r, err)
		return
	}
	q := r.URL.Query()
	var want *Scope
	if kind := strings.TrimSpace(q.Get("scope")); kind != "" {
		sc := scopeOf(r, kind, rg.c)
		if err := CheckScope(sc); err != nil {
			rt.fail(w, r, err)
			return
		}
		if !rg.read(sc) {
			rt.fail(w, r, ErrHidden)
			return
		}
		want = &sc
	}
	notes, err := s.List(ctx, agent, q.Get("putAway") == "true", func(n Note) bool {
		if want != nil {
			return n.Scope == *want
		}
		return rg.read(n.Scope)
	})
	if err != nil {
		rt.fail(w, r, err)
		return
	}
	out := make([]ListItem, 0, len(notes))
	for _, n := range notes {
		out = append(out, listItem(n))
	}
	native.WriteJSON(w, http.StatusOK, map[string]any{"agent": agent, "notes": out})
}

// ListItem is one note of the list: the record without its text, with
// sources as a count, as the kernel and the page read it.
type ListItem struct {
	Agent       string     `json:"agent"`
	Scope       Scope      `json:"scope"`
	Name        string     `json:"name"`
	Hash        string     `json:"hash"`
	State       string     `json:"state"`
	Description string     `json:"description"`
	By          string     `json:"by"`
	Run         string     `json:"run,omitempty"`
	At          time.Time  `json:"at"`
	Uses        int        `json:"uses"`
	LastUsed    *time.Time `json:"lastUsed,omitempty"`
	Sources     int        `json:"sources"`
	OutOfDate   *OutOfDate `json:"outOfDate,omitempty"`
	Origin      string     `json:"origin,omitempty"`
	Revision    uint64     `json:"revision"`
}

func listItem(n Note) ListItem {
	return ListItem{Agent: n.Agent, Scope: n.Scope, Name: n.Name, Hash: n.Hash, State: n.State, Description: n.Description, By: n.By, Run: n.Run,
		At: n.At, Uses: n.Uses, LastUsed: n.LastUsed, Sources: len(n.Sources), OutOfDate: n.OutOfDate, Origin: n.Origin, Revision: n.Revision}
}

// noteParams reads the agent, the scope, and the name of a note route,
// with the rights of the caller on the agent.
func (rt *routes) noteParams(w http.ResponseWriter, r *http.Request, s *Service) (rights, string, Scope, string, bool) {
	ctx := r.Context()
	agent, name := r.PathValue("agent"), r.PathValue("name")
	rg, err := s.rightsOn(ctx, agent)
	if err != nil {
		rt.fail(w, r, err)
		return rights{}, "", Scope{}, "", false
	}
	sc := scopeOf(r, r.PathValue("scope"), rg.c)
	if err := checkNote(agent, sc, name); err != nil {
		rt.fail(w, r, err)
		return rights{}, "", Scope{}, "", false
	}
	return rg, agent, sc, name, true
}

// get answers GET /v1/knowhow/notes/{agent}/{scope}/{name...} with the
// note and its text, and its hash in ETag.
func (rt *routes) get(w http.ResponseWriter, r *http.Request, s *Service) {
	r = r.WithContext(onBehalf(r))
	rg, agent, sc, name, ok := rt.noteParams(w, r, s)
	if !ok {
		return
	}
	if !rg.read(sc) {
		rt.fail(w, r, ErrHidden)
		return
	}
	n, err := s.Get(r.Context(), agent, sc, name)
	if err != nil {
		rt.fail(w, r, err)
		return
	}
	w.Header().Set("ETag", strconv.Quote(n.Hash))
	native.WriteJSON(w, http.StatusOK, n)
}

// putBody is the body of PUT /v1/knowhow/notes/{agent}/{scope}/{name...}.
type putBody struct {
	Text    *string  `json:"text"`
	IfHash  string   `json:"ifHash"`
	Sources []Source `json:"sources"`
	By      string   `json:"by"`
	Run     string   `json:"run"`
}

// writeAnswer is the answer of a write: the note without its text, and
// the change that waits for a person, when one does.
type writeAnswer struct {
	Note    Note     `json:"note"`
	Waiting *Waiting `json:"waiting,omitempty"`
}

// put answers PUT /v1/knowhow/notes/{agent}/{scope}/{name...}: 201 for a
// new note, 200 for a change or no change, 202 for a change that waits for
// a person, and 412 when ifHash does not match. A worker names the writer
// in by, agent or person:<subject>, and the run; a person writes as
// themselves.
func (rt *routes) put(w http.ResponseWriter, r *http.Request, s *Service) {
	r = r.WithContext(onBehalf(r))
	rg, agent, sc, name, ok := rt.noteParams(w, r, s)
	if !ok {
		return
	}
	var in putBody
	if !rt.k.Decode(w, r, &in) {
		return
	}
	if in.Text == nil {
		native.WriteProblem(w, r, http.StatusBadRequest, `the body is {"text", "ifHash", "sources", "by", "run"}, and text is missing`)
		return
	}
	if in.IfHash == "" {
		in.IfHash = r.Header.Get("If-Match")
	}
	if err := rg.change(sc); err != nil {
		rt.fail(w, r, err)
		return
	}
	by, run, err := writer(rg.c, in.By, in.Run)
	if err != nil {
		rt.fail(w, r, err)
		return
	}
	res, err := s.Write(r.Context(), WriteRequest{Agent: agent, Scope: sc, Name: name, Text: *in.Text, IfHash: in.IfHash, Sources: in.Sources,
		By: by, Run: run, Hold: !isPerson(by)})
	if err != nil {
		rt.fail(w, r, err)
		return
	}
	status := http.StatusOK
	switch {
	case res.Waiting != nil:
		status = http.StatusAccepted
	case res.Created:
		status = http.StatusCreated
	}
	if res.Changed || res.Waiting != nil {
		s.log.Info("knowhow: a note was written", "agent", agent, "scope", sc.Kind, "note", name, "by", by, "hash", res.Note.Hash, "waiting", res.Waiting != nil)
	}
	w.Header().Set("ETag", strconv.Quote(res.Note.Hash))
	native.WriteJSON(w, status, writeAnswer{Note: res.Note.Brief(), Waiting: res.Waiting})
}

// writer returns the writer and the run of a change: a worker names them,
// agent by default; a person is always person:<subject> without a run.
func writer(c caller, by, run string) (string, string, error) {
	if !c.trusted {
		return c.by(), "", nil
	}
	by, run = strings.TrimSpace(by), strings.TrimSpace(run)
	switch {
	case by == "", by == ByAgent:
		by = ByAgent
	case by == "person":
		// The native API of a worker without a subject of the caller.
		by = PersonPrefix + "unknown"
	case strings.HasPrefix(by, PersonPrefix) && len(by) > len(PersonPrefix) && len(by) <= MaxPrincipal && oneLine(by):
	default:
		return "", "", errors.Join(ErrInvalid, errors.New("by is agent or person:<subject>"))
	}
	if run != "" {
		if _, ok := fw.IDTime(run); !ok {
			return "", "", errors.Join(ErrInvalid, errors.New("run is the id of a run"))
		}
	}
	return by, run, nil
}

// remove answers DELETE /v1/knowhow/notes/{agent}/{scope}/{name...}: it
// puts the note away, 200 with the note, or 202 for a change that waits.
func (rt *routes) remove(w http.ResponseWriter, r *http.Request, s *Service) {
	r = r.WithContext(onBehalf(r))
	rg, agent, sc, name, ok := rt.noteParams(w, r, s)
	if !ok {
		return
	}
	if err := rg.change(sc); err != nil {
		rt.fail(w, r, err)
		return
	}
	q := r.URL.Query()
	cond := q.Get("ifHash")
	if cond == "" {
		cond = r.Header.Get("If-Match")
	}
	by, run, err := writer(rg.c, q.Get("by"), q.Get("run"))
	if err != nil {
		rt.fail(w, r, err)
		return
	}
	res, err := s.PutAway(r.Context(), PutAwayRequest{Agent: agent, Scope: sc, Name: name, IfHash: cond, By: by, Run: run, Hold: !isPerson(by)})
	if err != nil {
		rt.fail(w, r, err)
		return
	}
	status := http.StatusOK
	if res.Waiting != nil {
		status = http.StatusAccepted
	}
	if res.Changed {
		s.log.Info("knowhow: a note was put away", "agent", agent, "scope", sc.Kind, "note", name, "by", by)
	}
	native.WriteJSON(w, status, writeAnswer{Note: res.Note.Brief(), Waiting: res.Waiting})
}

// onBehalf returns the context of the request for the person that the
// manager acts for, as the other routes of the front do.
func onBehalf(r *http.Request) context.Context {
	ctx, _, _ := manager.OnBehalf(r.Context())
	return ctx
}

// journal answers GET /v1/knowhow/journal?agent=&limit=&before= with the
// entries that the caller can read, newest first.
func (rt *routes) journal(w http.ResponseWriter, r *http.Request, s *Service) {
	ctx := onBehalf(r)
	agent, ok := agentParam(w, r)
	if !ok {
		return
	}
	limit, ok := limitParam(w, r, DefaultLimit, MaxLimit)
	if !ok {
		return
	}
	rg, err := s.rightsOn(ctx, agent)
	if err != nil {
		rt.fail(w, r, err)
		return
	}
	list, err := s.Journal(ctx, agent, limit, strings.TrimSpace(r.URL.Query().Get("before")), func(e Entry) bool { return rg.seeSubject(e.Subject) })
	if err != nil {
		rt.fail(w, r, err)
		return
	}
	native.WriteJSON(w, http.StatusOK, map[string]any{"agent": agent, "entries": list})
}

// undo answers POST /v1/knowhow/journal/{id}/undo with the entry of the
// undo and the entry that it took back, or 409 when a later change
// touched the same note or routine.
func (rt *routes) undo(w http.ResponseWriter, r *http.Request, s *Service) {
	ctx := onBehalf(r)
	e, _, err := s.entry(ctx, r.PathValue("id"))
	if err != nil {
		rt.fail(w, r, err)
		return
	}
	rg, err := s.rightsOn(ctx, e.Agent)
	if err == nil && !rg.seeSubject(e.Subject) {
		err = ErrHidden
	}
	if err == nil {
		err = rg.changeSubject(e.Subject)
	}
	if err != nil {
		rt.fail(w, r, err)
		return
	}
	by := rg.c.by()
	if rg.c.trusted {
		by = "fw-workbench"
		if p := fw.PrincipalFrom(ctx); p != nil {
			by = p.Subject
		}
	}
	res, err := s.TakeBack(ctx, e.ID, by, rg.c.person())
	if err != nil {
		rt.fail(w, r, err)
		return
	}
	s.log.Info("knowhow: a change was taken back", "agent", e.Agent, "entry", e.ID, "what", e.What, "by", by)
	native.WriteJSON(w, http.StatusOK, res)
}

// settingsAnswer is the switch of an agent with the state of the review.
type settingsAnswer struct {
	Agent string `json:"agent"`
	Settings
	Review reviewState `json:"review"`
}

// reviewState says whether the after-work review runs, and why not.
type reviewState struct {
	On     bool   `json:"on"`
	Agent  string `json:"agent,omitempty"`
	Every  string `json:"every,omitempty"`
	Reason string `json:"reason,omitempty"`
}

func (s *Service) reviewState() reviewState {
	if !s.cfg.Review.On() {
		return reviewState{Reason: ReviewOff}
	}
	return reviewState{On: true, Agent: s.cfg.Review.Agent, Every: s.cfg.Review.Every.String()}
}

// settings answers GET /v1/knowhow/settings/{agent}.
func (rt *routes) settings(w http.ResponseWriter, r *http.Request, s *Service) {
	ctx := onBehalf(r)
	agent := r.PathValue("agent")
	if _, err := s.rightsOn(ctx, agent); err != nil {
		rt.fail(w, r, err)
		return
	}
	st, err := s.Settings(ctx, agent)
	if err != nil {
		rt.fail(w, r, err)
		return
	}
	native.WriteJSON(w, http.StatusOK, settingsAnswer{Agent: agent, Settings: st, Review: s.reviewState()})
}

// putSettings answers PUT /v1/knowhow/settings/{agent} with the body
// {"askFirst": true}. It needs the right to edit the agent.
func (rt *routes) putSettings(w http.ResponseWriter, r *http.Request, s *Service) {
	ctx := onBehalf(r)
	agent := r.PathValue("agent")
	var in struct {
		AskFirst *bool `json:"askFirst"`
	}
	if !rt.k.Decode(w, r, &in) {
		return
	}
	if in.AskFirst == nil {
		native.WriteProblem(w, r, http.StatusBadRequest, `the body is {"askFirst": true} or {"askFirst": false}`)
		return
	}
	rg, err := s.rightsOn(ctx, agent)
	if err == nil && !rg.edit {
		err = errors.Join(fw.ErrForbidden, errors.New("the switch of an agent needs the owner role on the agent"))
	}
	if err != nil {
		rt.fail(w, r, err)
		return
	}
	st, err := s.SetAskFirst(ctx, agent, *in.AskFirst, rg.c.by())
	if err != nil {
		rt.fail(w, r, err)
		return
	}
	s.log.Info("knowhow: the switch of an agent changed", "agent", agent, "askFirst", st.AskFirst)
	native.WriteJSON(w, http.StatusOK, settingsAnswer{Agent: agent, Settings: st, Review: s.reviewState()})
}

// waiting answers GET /v1/knowhow/waiting?agent= with the changes that
// wait for a person and that the caller can read. Without an agent, it
// answers the changes of every agent that the caller can view, so the
// list of agents of the page marks each agent that waits for a person.
func (rt *routes) waiting(w http.ResponseWriter, r *http.Request, s *Service) {
	ctx := onBehalf(r)
	agent := strings.TrimSpace(r.URL.Query().Get("agent"))
	if agent == "" {
		rt.waitingAll(w, r, s)
		return
	}
	rg, err := s.rightsOn(ctx, agent)
	if err != nil {
		rt.fail(w, r, err)
		return
	}
	list, err := s.WaitingList(ctx, agent, func(x Waiting) bool { return rg.seeSubject(x.Subject) })
	if err != nil {
		rt.fail(w, r, err)
		return
	}
	native.WriteJSON(w, http.StatusOK, map[string]any{"agent": agent, "waiting": list})
}

// waitingAll answers GET /v1/knowhow/waiting without an agent. An agent
// that the caller cannot view adds nothing, as for one agent.
func (rt *routes) waitingAll(w http.ResponseWriter, r *http.Request, s *Service) {
	ctx := onBehalf(r)
	keys, err := bucket.EntriesUnder(ctx, s.b, "waiting")
	if err != nil {
		rt.fail(w, r, fmt.Errorf("knowhow: list the waiting changes: %w", err))
		return
	}
	of := map[string]*rights{}
	out := []Waiting{}
	for _, k := range keys {
		got, err := s.b.Get(ctx, k.Key)
		if err != nil {
			continue
		}
		var x Waiting
		if json.Unmarshal(got.Value, &x) != nil {
			continue
		}
		rg, seen := of[x.Agent]
		if !seen {
			if g, err := s.rightsOn(ctx, x.Agent); err == nil {
				rg = &g
			}
			of[x.Agent] = rg
		}
		if rg != nil && rg.seeSubject(x.Subject) {
			out = append(out, x)
		}
	}
	native.WriteJSON(w, http.StatusOK, map[string]any{"waiting": out})
}

// decide answers POST /v1/knowhow/waiting/{id} with {"keep": true} or
// {"keep": false}. A target that changed since gets 409.
func (rt *routes) decide(w http.ResponseWriter, r *http.Request, s *Service) {
	ctx := onBehalf(r)
	var in struct {
		Keep *bool `json:"keep"`
	}
	if !rt.k.Decode(w, r, &in) {
		return
	}
	if in.Keep == nil {
		native.WriteProblem(w, r, http.StatusBadRequest, `the body is {"keep": true} or {"keep": false}`)
		return
	}
	x, _, err := s.waitingEntry(ctx, r.PathValue("id"))
	if err != nil {
		rt.fail(w, r, err)
		return
	}
	rg, err := s.rightsOn(ctx, x.Agent)
	if err == nil && !rg.seeSubject(x.Subject) {
		err = ErrHidden
	}
	if err == nil {
		err = rg.changeSubject(x.Subject)
	}
	if err != nil {
		rt.fail(w, r, err)
		return
	}
	e, err := s.Decide(ctx, x.ID, *in.Keep, rg.c.by())
	if err != nil {
		rt.fail(w, r, err)
		return
	}
	s.log.Info("knowhow: a person decided a waiting change", "agent", x.Agent, "waiting", x.ID, "keep", *in.Keep)
	native.WriteJSON(w, http.StatusOK, map[string]any{"kept": *in.Keep, "entry": e})
}
