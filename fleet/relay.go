package fleet

// This file is the relay of the native surface (native.Relay): it answers a
// request about a run that another member owns by sending it to that
// member, and it relays the answer as the member sent it. A read of the run
// object of a member that sleeps is the exception: the view answers it,
// because a request through the router of ax wakes the Task (ViewRoutes).
// POST /v1/runs?member=<name> starts a run on another member. A request
// that a member forwarded is never forwarded again. The relay also answers
// GET /v1/agents/{name} for a profile that the workers of a pool run
// (native.AgentRelay).
//
// The front decides each action of a person itself, with the rules of the
// view, before any request goes to the member. The request then carries
// the static token of the fleet and names the person in HeaderOnBehalfOf
// (person.go), so a worker trusts the credential of the fleet alone. The
// static token, a workload, and the process send their own credential, and
// the member decides.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/surface/native"
	"github.com/bdubs00/featherweight-kernel/surface/view"
)

// maxRelayBytes bounds a JSON answer of a member that the relay rewrites.
const maxRelayBytes = 64 << 20

// relayRequestHeaders are the request headers that go to the member. No
// other header of the caller does, HeaderOnBehalfOf included.
var relayRequestHeaders = []string{"Accept", "Content-Type", native.HeaderIdempotencyKey, "Last-Event-ID"}

// relayHeaders are the response headers that come back from the member.
var relayHeaders = []string{"Content-Type", "Cache-Control", "Retry-After", "Location", "X-Accel-Buffering",
	"Content-Disposition", "Content-Security-Policy", "X-Content-Type-Options"}

// Relay is the native.Relay of one agent. It reads the View of the agent at
// each request, so it works when the fleet starts after the surface.
type Relay struct{ a *fw.Agent }

// NewRelay returns the relay of an agent.
func NewRelay(a *fw.Agent) *Relay { return &Relay{a: a} }

// The routes of the kernel whose body names a party, which the relay
// checks for a person (personBody), and the route of a branch, which needs
// a second decision.
const (
	routeStart     = "POST /v1/runs"
	routeMessages  = "POST /v1/runs/{id}/messages"
	routeReactions = "POST /v1/runs/{id}/reactions"
	routeFork      = "POST /v1/runs/{id}/fork"
	routeQuality   = "POST /v1/runs/{id}/quality"
)

// Where the front answers a run route of the kernel for a run of another
// member (docs/ARCHITECTURE.md section 4.5). A request to a member goes
// through the router of ax, and the router wakes a Task that sleeps for
// any request. So the view answers the reads whose answer it holds, and
// only the routes whose answer lives on the worker reach the member.
var (
	// ViewRoutes are the reads that the view answers itself while the
	// member of the run does not run: its state is quiet or stopped. The
	// answer has the shape of the answer of the member, with the field
	// member, and the view fills it as it holds the run (RestView). A
	// member that runs answers these reads itself.
	ViewRoutes = []string{"GET /v1/runs/{id}"}

	// WorkerRoutes always go to the member that holds the run, because
	// their answer lives on the worker: the ledger of the run, its live
	// text, its files, and every write. Each of them wakes a Task that
	// sleeps. A run route that neither list names goes to the member too,
	// because only the member knows it.
	WorkerRoutes = []string{
		"GET /v1/runs/{id}/events",
		"GET /v1/runs/{id}/live",
		"GET /v1/runs/{id}/files",
		"GET /v1/runs/{id}/files/{path...}",
		routeMessages,
		routeReactions,
		"PATCH /v1/runs/{id}/budget",
		"POST /v1/runs/{id}/approvals/{approval}",
		"POST /v1/runs/{id}/cancel",
		routeFork,
		routeQuality,
	}
)

// personDoes words what a person does through a run route, for a refusal
// of the front. Each value takes the id of the run.
var personDoes = map[string]string{
	"GET /v1/runs/{id}":                       "read the run %s",
	"GET /v1/runs/{id}/events":                "read the events of the run %s",
	"GET /v1/runs/{id}/live":                  "follow the run %s",
	"GET /v1/runs/{id}/files":                 "read the files of the run %s",
	"GET /v1/runs/{id}/files/{path...}":       "read the files of the run %s",
	routeMessages:                             "send a message to the run %s",
	routeReactions:                            "react to the run %s",
	"PATCH /v1/runs/{id}/budget":              "change the budget of the run %s",
	"POST /v1/runs/{id}/approvals/{approval}": "answer an approval of the run %s",
	"POST /v1/runs/{id}/cancel":               "cancel the run %s",
	routeFork:                                 "branch the run %s",
	routeQuality:                              "record a receipt of the quality of the run %s",
}

// Can is the field can of the run object that the front answers to a
// person for GET /v1/runs/{id} of a run of another member: what the person
// can do with the run, as the front decides it (AllowRun), so a page draws
// only the buttons that work. View is always true, because the front
// answers only a run that the person can view. Operate is
// fw.ActionRunOperate: to send a message, react, answer an approval,
// change the budget, cancel, and fork, which also needs fw.ActionAgentRun
// on the agent of the run.
type Can struct {
	View    bool `json:"view"`
	Operate bool `json:"operate"`
}

// runAnswer is a run object with the field can, which only a person gets.
type runAnswer struct {
	*fw.Run
	Can      *Can       `json:"can,omitempty"`
	Archived bool       `json:"archived,omitempty"`
	EndedAt  *time.Time `json:"endedAt,omitempty"`
}

// RestView is a View that can stand in for a member that does not run, so
// a read of a run wakes no Task (ViewRoutes).
//
// AtRest takes a run that Run returned for the caller, with Member set,
// and returns a copy of the run as the view holds it when that member is
// quiet or stopped. The copy carries the texts that the egress stream
// carried, not the texts of a record, which TextLimit cuts. AtRest
// returns nil when the member runs, or when the view holds no copy that
// can stand in for the answer of the member. The read then goes to the
// member.
type RestView interface {
	AtRest(run *fw.Run) *fw.Run
}

// Take implements native.Relay. A route with the path value id goes to the
// member that owns the run, when another member owns it, unless the view
// answers it (ViewRoutes). POST /v1/runs with the query parameter member
// goes to that member. For a person, the front decides first (forPerson).
// A body of POST /v1/runs with continues gets the summary of the earlier
// conversation first, wherever the run starts (continues.go).
func (rl *Relay) Take(w http.ResponseWriter, r *http.Request) bool {
	if r.Pattern == routeReactions && r.Method == http.MethodPost {
		// A reaction can be a verdict on a result, so the views that
		// keep a copy of the board forget it (OnChange).
		defer changed(rl.a)
	}
	if r.Method == http.MethodPost && r.URL.Path == "/v1/runs" && !rl.takeContinues(w, r) {
		return true // the problem of a body with continues (continues.go)
	}
	if id := r.PathValue("id"); id != "" {
		run, ok := rl.owned(w, r, id)
		switch {
		case !ok:
			return true
		case run == nil:
			// A run of this process: the kernel answers, unless the
			// reaction checks a result (endseq.go).
			if r.Pattern == routeReactions && r.Method == http.MethodPost {
				return rl.localCheck(w, r, id)
			}
			return false
		}
		if p := fw.PrincipalFrom(r.Context()); IsPerson(p) {
			rl.forPerson(w, r, run, p)
			return true
		}
		if r.Pattern == routeReactions && r.Method == http.MethodPost && toCheck(run) {
			body, ok := readBody(w, r)
			if !ok {
				return true
			}
			rl.remoteCheck(r.Context(), w, r, run, body)
			return true
		}
		if !rl.answerAtRest(w, r, run, nil) {
			rl.Forward(w, r, run.Member)
		}
		return true
	}
	if member := strings.TrimSpace(r.URL.Query().Get("member")); member != "" && r.Method == http.MethodPost {
		return rl.startOn(w, r, member)
	}
	return false
}

// answerAtRest answers a read of ViewRoutes from the view when the member
// of the run does not run, and reports whether it answered. The answer is
// the run object with member, as GET /v1/runs/{id} of the member answers
// it and the relay sets member on it, and with can when it is not nil.
func (rl *Relay) answerAtRest(w http.ResponseWriter, r *http.Request, run *fw.Run, can *Can) bool {
	if !slices.Contains(ViewRoutes, r.Pattern) {
		return false
	}
	rv, ok := Of(rl.a).(RestView)
	if !ok {
		return false
	}
	c := rv.AtRest(run)
	if c == nil {
		return false
	}
	c.Member = run.Member
	native.WriteJSON(w, http.StatusOK, runAnswer{Run: c, Can: can, EndedAt: endedOf(r.Context(), rl.a, run.ID)})
	return true
}

// forPerson answers a run route of a run of another member for the person
// p. The front decides each action of the route for p first
// (personActions), with the rules of the view, so no request that p cannot
// make leaves the front. A read of the run object gets can, and the view
// answers it while the member does not run. The request then goes with
// the static token of the fleet and names p (sendFor), and the body names
// p where the kernel takes a person from it (personBody).
func (rl *Relay) forPerson(w http.ResponseWriter, r *http.Request, run *fw.Run, p *fw.Principal) {
	ctx := r.Context()
	var can *Can
	if slices.Contains(ViewRoutes, r.Pattern) {
		// One more decision and no call to the member. A decision point
		// that does not answer gives false, and an action decides again.
		can = &Can{View: true, Operate: AllowRun(ctx, Of(rl.a), run.ID, fw.ActionRunOperate) == nil}
		if rl.answerAtRest(w, r, run, can) {
			return
		}
	}
	if action, err := decideFor(ctx, Of(rl.a), run, personActions(r)); err != nil {
		rl.refuse(w, r, p, err, action, run.Member, run.ID, run.Agent)
		return
	}
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	if body, _, ok = personBody(w, r, body, p); !ok {
		return
	}
	if r.Pattern == routeReactions && toCheck(run) {
		// The reaction checks a result (endseq.go).
		pctx, err := ForPerson(ctx, rl.a.Config(), p)
		if err != nil {
			fail(w, r, err)
			return
		}
		rl.remoteCheck(pctx, w, r, run, body)
		return
	}
	rl.sendFor(w, r, run.Member, body, p, can)
}

// personActions are the actions that the front decides for a person
// before a run route goes to the member, as the kernel decides them there:
// fw.ActionRunView for a read and fw.ActionRunOperate for a write. A fork
// also needs fw.ActionAgentRun on the agent of the run. A run route that
// the front does not know is a read for GET and HEAD, and else a write.
func personActions(r *http.Request) []string {
	switch {
	case r.Method == http.MethodGet || r.Method == http.MethodHead:
		return []string{fw.ActionRunView}
	case r.Pattern == routeFork:
		return []string{fw.ActionRunOperate, fw.ActionAgentRun}
	}
	return []string{fw.ActionRunOperate}
}

// refuse answers a person whom the front refused an action on another
// member. A refusal of the rules is a 403 that says what the person wanted
// and what that needs on which agent (refusal). The reason of the
// decision point goes to the log only, because it speaks of rules and not
// to the person. Any other error gets the answer of fail, such as a 503
// for a decision point that did not answer.
func (rl *Relay) refuse(w http.ResponseWriter, r *http.Request, p *fw.Principal, err error, action, member, id, agent string) {
	if errors.Is(err, fw.ErrAuthzUnavailable) || !errors.Is(err, fw.ErrForbidden) {
		fail(w, r, err)
		return
	}
	rl.a.Logger().Info("fleet: the front refused an action of a person on another member",
		"member", member, "run", id, "agent", agent, "action", action, "principal", p.Subject, "reason", err)
	native.WriteProblem(w, r, http.StatusForbidden, refusal(r.Pattern, action, member, id, agent))
}

// refusal words a refusal for a person: what the person wanted, and the
// role that it needs on which agent.
func refusal(pattern, action, member, id, agent string) string {
	switch {
	case pattern == routeStart:
		return fmt.Sprintf("you cannot start a run of the agent %s on %s: that needs the operator role on the agent %s", agent, member, agent)
	case action == fw.ActionAgentRun:
		return fmt.Sprintf("you cannot branch the run %s: a branch is a new run of the agent %s, and that needs the operator role on the agent %s", id, agent, agent)
	}
	what, role := "act on the run "+id, "operator"
	if action == fw.ActionRunView {
		what, role = "read the run "+id, "viewer"
	}
	if f, ok := personDoes[pattern]; ok {
		what = fmt.Sprintf(f, id)
	}
	return fmt.Sprintf("you cannot %s: that needs the %s role on the agent %s, or to be the one who started the run", what, role, agent)
}

// The relay answers the read of a profile of a pool.
var _ native.AgentRelay = (*Relay)(nil)

// TakeAgent implements native.AgentRelay. It answers GET /v1/agents/{name}
// with the profile that the workers of a pool run, as the MCP tool
// agent_get reads it (PoolAgentNamed): the profile with pool, the role of
// the caller, the policy of the workers, and the tools that the workers
// give a profile that names none, and with no abilities, because the
// tools of the workers live in their Tasks. The query
// parameter pool names the pool when several pools run the profile. The
// kernel calls it after the authentication and checks no right, so the
// read decides with the rules of the pool for the caller in the context.
// A profile that a pool runs and the caller cannot view answers 404, as
// the kernel answers a profile that the caller cannot view. A profile that
// no pool runs, and every profile of a front without pools, stay with the
// kernel.
func (rl *Relay) TakeAgent(w http.ResponseWriter, r *http.Request, name string) bool {
	if !Pooled(rl.a) {
		return false
	}
	ctx := r.Context()
	pool := strings.TrimSpace(r.URL.Query().Get("pool"))
	pa, found, err := PoolAgentNamed(ctx, rl.a, name, pool)
	switch {
	case err != nil:
		fail(w, r, err)
		return true
	case found:
		native.WriteJSON(w, http.StatusOK, pa)
		return true
	}
	// A context without a principal sees every profile of the pools. A
	// profile that a pool runs stays hidden from a caller who cannot view
	// it, also when this process holds a profile of the same name.
	_, held, err := PoolAgentNamed(fw.WithPrincipal(ctx, nil), rl.a, name, pool)
	if held || pool != "" || errors.Is(err, fw.ErrInvalidRequest) {
		detail := "no agent profile has the name " + strconv.Quote(name)
		if pool != "" {
			detail += " in the pool " + pool
		}
		native.WriteProblem(w, r, http.StatusNotFound, detail)
		return true
	}
	return false
}

// Owner names the member that owns a run when another member owns it, and
// "" when the run is local, unknown, or the fleet is off. A request that a
// member forwarded never goes out again. On failure it writes the problem
// and returns false.
func (rl *Relay) Owner(w http.ResponseWriter, r *http.Request, id string) (string, bool) {
	run, ok := rl.owned(w, r, id)
	if run == nil {
		return "", ok
	}
	return run.Member, ok
}

// owned returns the run of another member as the view gives it to the
// caller, with Member set, and nil when the run is local, unknown, or the
// fleet is off. On failure it writes the problem and returns false.
func (rl *Relay) owned(w http.ResponseWriter, r *http.Request, id string) (*fw.Run, bool) {
	f := Of(rl.a)
	if f == nil || r.Header.Get(HeaderVia) != "" {
		return nil, true
	}
	// A context without a principal asks the kernel whether the run exists
	// at all, without an authorization check. The handler authorizes a
	// local run as before.
	if _, err := rl.a.Get(context.Background(), id); !errors.Is(err, fw.ErrRunNotFound) {
		return nil, true
	}
	run, err := f.Run(r.Context(), id)
	switch {
	case err == nil && run.Member != "":
		return run, true
	case errors.Is(err, fw.ErrRunNotFound):
		// No member holds the run: the archive answers for it, when it
		// holds it (archive.go).
		if rl.answerArchived(w, r, id) {
			return nil, false
		}
		return nil, true
	case err == nil:
		return nil, true
	default:
		fail(w, r, err)
		return nil, false
	}
}

// answerArchived answers a run route for a run that only the archive holds
// (archive.go), and reports whether it answered. GET /v1/runs/{id} answers
// the run with "archived": true, and GET /v1/runs/{id}/events its events
// from the archive. POST /v1/runs/{id}/reactions keeps the reaction in the
// archive (archiveverdict.go), because a verdict on a result must land
// after its worker went. Every other write answers 409 with ReadOnlyRun,
// because the worker of the run is gone. Another read, such as the live
// text or the files, answers 404.
func (rl *Relay) answerArchived(w http.ResponseWriter, r *http.Request, id string) bool {
	run, ok, err := ArchivedRun(r.Context(), rl.a, id)
	switch {
	case err != nil:
		fail(w, r, err)
		return true
	case !ok:
		return false
	}
	switch {
	case r.Pattern == "GET /v1/runs/{id}":
		var can *Can
		if IsPerson(fw.PrincipalFrom(r.Context())) {
			can = &Can{View: true}
		}
		native.WriteJSON(w, http.StatusOK, runAnswer{Run: run, Can: can, Archived: true, EndedAt: rl.archivedEnd(r.Context(), run)})
	case r.Pattern == "GET /v1/runs/{id}/events":
		rl.archivedEvents(w, r, run)
	case r.Pattern == routeReactions && r.Method == http.MethodPost:
		rl.archivedReaction(w, r, run)
	case r.Method != http.MethodGet && r.Method != http.MethodHead:
		native.WriteProblem(w, r, http.StatusConflict, ReadOnlyRun)
	default:
		native.WriteProblem(w, r, http.StatusNotFound, GoneRun)
	}
	return true
}

// archivedReaction answers POST /v1/runs/{id}/reactions for a run of the
// archive, with the body of the kernel route: the reaction, an optional
// note, seq, by, and remove. It keeps the reaction in the archive and
// answers the run with "archived": true, as the kernel answers the run
// after a reaction.
func (rl *Relay) archivedReaction(w http.ResponseWriter, r *http.Request, run *fw.Run) {
	var body thumb
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxReactionBody))
	if err != nil || json.Unmarshal(raw, &body) != nil {
		native.WriteProblem(w, r, http.StatusBadRequest, "the body of a reaction is a JSON object with reaction, and optionally note, seq, and remove")
		return
	}
	d := fw.ReactionData{Reaction: body.Reaction, Note: body.Note, By: strings.TrimSpace(body.By), Removed: body.Remove}
	if body.Seq != nil {
		d.Seq = *body.Seq
	}
	got, err := ReactArchived(r.Context(), rl.a, run.ID, d)
	if err != nil {
		fail(w, r, err)
		return
	}
	var can *Can
	if IsPerson(fw.PrincipalFrom(r.Context())) {
		can = &Can{View: true}
	}
	native.WriteJSON(w, http.StatusOK, runAnswer{Run: got, Can: can, Archived: true, EndedAt: rl.archivedEnd(r.Context(), got)})
}

// maxReactionBody bounds the body of a reaction on a run of the archive.
const maxReactionBody = 16 << 10

// archivedEvents answers GET /v1/runs/{id}/events for a run of the
// archive, as the kernel answers it: a page of {"run", "events", "lines"}
// for JSON, from the query parameter from or the last limit events, and
// else the events as server-sent events, after which the stream ends,
// because the run takes no more events.
func (rl *Relay) archivedEvents(w http.ResponseWriter, r *http.Request, run *fw.Run) {
	var from uint64
	if v := r.URL.Query().Get("from"); v != "" {
		n, err := strconv.ParseUint(v, 10, 64)
		if err != nil {
			native.WriteProblem(w, r, http.StatusBadRequest, "the query parameter from must be a sequence number")
			return
		}
		from = n
	}
	if v := strings.TrimSpace(r.Header.Get("Last-Event-ID")); v != "" {
		if n, err := strconv.ParseUint(v, 10, 64); err == nil {
			from = n + 1
		}
	}
	limit := 200
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			native.WriteProblem(w, r, http.StatusBadRequest, "the query parameter limit must be a positive number")
			return
		}
		limit = min(n, 1000)
	}
	f := Of(rl.a).(*Fleet)
	all, err := f.arch().events(r.Context(), run.ID)
	if err != nil {
		fail(w, r, err)
		return
	}
	events := []fw.Event{}
	for _, e := range all {
		if e.Seq >= from {
			events = append(events, e)
		}
	}
	if from > 0 && len(events) > limit {
		events = events[:limit]
	}
	if from == 0 && len(events) > limit {
		events = events[len(events)-limit:]
	}
	accept := r.Header.Get("Accept")
	if strings.Contains(accept, "text/event-stream") || !strings.Contains(accept, "application/json") {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		for _, e := range events {
			data, _ := json.Marshal(e)
			fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", e.Seq, e.Type, data)
		}
		return
	}
	lines := make([]view.EventLine, 0, len(events))
	for _, e := range events {
		lines = append(lines, view.Line(e))
	}
	native.WriteJSON(w, http.StatusOK, map[string]any{"run": runAnswer{Run: run, Archived: true}, "events": events, "lines": lines})
}

// Owner is Relay.Owner for the agent a.
func Owner(a *fw.Agent, w http.ResponseWriter, r *http.Request, id string) (string, bool) {
	return NewRelay(a).Owner(w, r, id)
}

// Forward is Relay.Forward for the agent a.
func Forward(a *fw.Agent, w http.ResponseWriter, r *http.Request, member string) {
	NewRelay(a).Forward(w, r, member)
}

// startOn handles POST /v1/runs?member=<name>. It reports true when it
// answered the request, and false when the run starts here, because the
// member is this process.
func (rl *Relay) startOn(w http.ResponseWriter, r *http.Request, member string) bool {
	f := Of(rl.a)
	switch {
	case f == nil:
		native.WriteProblem(w, r, http.StatusBadRequest, "the query parameter member needs the fleet, and the fleet is off; set workbench.fleet.atespaces ("+EnvAtespaces+") or workbench.fleet.egress to turn it on")
		return true
	case member == f.Name():
		return false
	case r.Header.Get(HeaderVia) != "":
		native.WriteProblem(w, r, http.StatusBadGateway, fmt.Sprintf("the member %s forwarded this request, and a member never forwards a request again", r.Header.Get(HeaderVia)))
		return true
	}
	if p := fw.PrincipalFrom(r.Context()); IsPerson(p) {
		rl.startFor(w, r, member, p)
		return true
	}
	rl.Forward(w, r, member)
	return true
}

// startFor starts a run on another member for the person p. The body
// names p in principal and principalKind, which the member takes from the
// static token, so the run belongs to p (personBody). The front first
// decides fw.ActionAgentRun for p on the agent of the body at that member,
// the agent default when the body names none, as the kernel does.
func (rl *Relay) startFor(w http.ResponseWriter, r *http.Request, member string, p *fw.Principal) {
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	body, agent, ok := personBody(w, r, body, p)
	if !ok {
		return
	}
	if agent == "" {
		agent = "default"
	}
	if err := AllowAgent(r.Context(), Of(rl.a), member, agent, fw.ActionAgentRun); err != nil {
		rl.refuse(w, r, p, err, fw.ActionAgentRun, member, "", agent)
		return
	}
	rl.sendFor(w, r, member, body, p, nil)
}

// Forward sends the request to a member with the credential of the caller
// and relays the answer. The query parameter member stays here. It is the
// path of the static token, a workload, and the process. Take sends the
// request of a person on its own path, which decides first, and another
// surface takes ForRun or ForStart for a person.
func (rl *Relay) Forward(w http.ResponseWriter, r *http.Request, member string) {
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	rl.send(r.Context(), w, r, member, body, nil)
}

// sendFor sends the request with body to a member for the person p, after
// the front decided the action for p: with the static token of the fleet
// as the credential and with HeaderOnBehalfOf (ForPerson). can goes into
// the run object of the answer.
func (rl *Relay) sendFor(w http.ResponseWriter, r *http.Request, member string, body []byte, p *fw.Principal, can *Can) {
	ctx, err := ForPerson(r.Context(), rl.a.Config(), p)
	if err != nil {
		fail(w, r, err)
		return
	}
	rl.send(ctx, w, r, member, body, can)
}

// send sends the request with body to a member, with the credential and
// the person of ctx, and relays the answer, with can in its run object.
// The query parameter member stays here.
func (rl *Relay) send(ctx context.Context, w http.ResponseWriter, r *http.Request, member string, body []byte, can *Can) {
	f := Of(rl.a)
	if f == nil {
		native.WriteProblem(w, r, http.StatusBadGateway, "the fleet is off")
		return
	}
	q := r.URL.Query()
	q.Del("member")
	target := r.URL.EscapedPath()
	if enc := q.Encode(); enc != "" {
		target += "?" + enc
	}
	var rd io.Reader = http.NoBody
	if body != nil {
		rd = bytes.NewReader(body)
	}
	out, err := http.NewRequestWithContext(ctx, r.Method, target, rd)
	if err != nil {
		fail(w, r, fmt.Errorf("fleet: build the request for the member %s: %w", member, err))
		return
	}
	for _, k := range relayRequestHeaders {
		if v := r.Header.Get(k); v != "" {
			out.Header.Set(k, v)
		}
	}
	resp, err := f.Forward(ctx, member, out)
	if err != nil {
		switch {
		case errors.Is(err, ErrNoRoute), errors.Is(err, ErrNoCredential):
			fail(w, r, err)
		case r.Context().Err() != nil:
			// The client is gone.
		default:
			rl.a.Logger().Warn("fleet: the member did not answer", "member", member, "method", r.Method, "path", r.URL.Path, "error", err)
			native.WriteProblem(w, r, http.StatusBadGateway, fmt.Sprintf("the fleet member %s did not answer; try again later", member))
		}
		return
	}
	defer resp.Body.Close()
	var ended *time.Time
	if id := r.PathValue("id"); id != "" && r.Pattern == "GET /v1/runs/{id}" {
		ended = endedOf(ctx, rl.a, id)
	}
	relay(w, r, member, resp, TokenEnvOf(ctx), can, ended)
}

// readBody reads the body of a request that goes to a member, and returns
// nil for a request without one. It reports false when it answered.
func readBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	if r.Method == http.MethodGet || r.Method == http.MethodHead || r.Body == nil {
		return nil, true
	}
	// The member applies its own limit. A run or a message with files is
	// the largest body that a route takes. A body of a known length takes
	// one buffer of that length.
	var buf bytes.Buffer
	if r.ContentLength > 0 {
		buf.Grow(int(min(r.ContentLength, native.MaxFilesBodyBytes)) + 1)
	}
	_, err := buf.ReadFrom(http.MaxBytesReader(w, r.Body, native.MaxFilesBodyBytes))
	b := buf.Bytes()
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			native.WriteProblem(w, r, http.StatusRequestEntityTooLarge, fmt.Sprintf("the body is larger than %d bytes", native.MaxFilesBodyBytes))
			return nil, false
		}
		native.WriteProblem(w, r, http.StatusBadRequest, "the body is not readable")
		return nil, false
	}
	return b, true
}

// personBody checks the body of a request of the person p before it goes
// to a member with the static token. The kernel takes some fields of a
// body from a trusted caller alone, the static token or a workload: the
// sender of a message, the person of a reaction, and the id and the person
// of a run. From a person it refuses them or puts the person in their
// place, and the token is a trusted caller. So the front does it here,
// with the words of the kernel: it refuses a sender, another person of a
// reaction, and the id of a run, and it names p as the person of a
// reaction and of a run that starts. It returns the body to send and the
// agent that a start names, and it reports false when it answered. A key
// names a field without regard to case, as the kernel decodes a body, and
// the body goes on as the front read it.
func personBody(w http.ResponseWriter, r *http.Request, body []byte, p *fw.Principal) ([]byte, string, bool) {
	switch r.Pattern {
	case routeStart, routeMessages, routeReactions:
	default:
		return body, "", true
	}
	if len(bytes.TrimSpace(body)) == 0 {
		// The member refuses an empty body, and it names no party.
		return body, "", true
	}
	o, err := parseObject(body)
	if err != nil {
		native.WriteProblem(w, r, http.StatusBadRequest, "the body is not valid: "+err.Error())
		return nil, "", false
	}
	agent, status, detail := o.forPerson(r.Pattern, p)
	if status != 0 {
		native.WriteProblem(w, r, status, detail)
		return nil, "", false
	}
	return o.bytes(), agent, true
}

// object is a body that holds one JSON object: its members in the order of
// the body, each a part of the body, so a large value, such as the files of
// a message, is never copied. take and set change the members, and bytes
// writes the body again only when they did.
type object struct {
	body    []byte
	members []objectMember
	added   []objectMember
	changed bool
}

// objectMember is one member of an object: its key, the part of the body
// from its key to the end of its value, and its value. A member that
// take removed is gone.
type objectMember struct {
	key   string
	raw   []byte
	value json.RawMessage
	gone  bool
}

// parseObject reads a body that holds one JSON object. null is an empty
// object, as the kernel reads it.
func parseObject(b []byte) (*object, error) {
	if !json.Valid(b) {
		var probe map[string]json.RawMessage
		if err := json.Unmarshal(b, &probe); err != nil {
			return nil, err
		}
		return nil, errors.New("the body is not JSON")
	}
	o := &object{body: b}
	i := skipSpace(b, 0)
	switch {
	case bytes.HasPrefix(b[i:], []byte("null")):
		o.changed = true // the body becomes {}
		return o, nil
	case b[i] != '{':
		return nil, errors.New("the body is not a JSON object")
	}
	i++
	for {
		i = skipSpace(b, i)
		switch b[i] {
		case '}':
			return o, nil
		case ',':
			i = skipSpace(b, i+1)
		}
		start := i
		i = skipString(b, i)
		var key string
		if err := json.Unmarshal(b[start:i], &key); err != nil {
			return nil, err
		}
		i = skipSpace(b, skipSpace(b, i)+1) // the colon
		from := i
		i = skipValue(b, i)
		o.members = append(o.members, objectMember{key: key, raw: b[start:i], value: b[from:i]})
	}
}

// skipSpace returns the index of the first byte at or after i that is no
// white space of JSON.
func skipSpace(b []byte, i int) int {
	for i < len(b) && (b[i] == ' ' || b[i] == '\t' || b[i] == '\r' || b[i] == '\n') {
		i++
	}
	return i
}

// skipString returns the index after the JSON string that starts at i, in
// a body that json.Valid accepted.
func skipString(b []byte, i int) int {
	for i++; i < len(b); i++ {
		switch b[i] {
		case '\\':
			i++
		case '"':
			return i + 1
		}
	}
	return i
}

// skipValue returns the index after the JSON value that starts at i, in a
// body that json.Valid accepted.
func skipValue(b []byte, i int) int {
	switch b[i] {
	case '"':
		return skipString(b, i)
	case '{', '[':
		depth := 0
		for i < len(b) {
			switch b[i] {
			case '"':
				i = skipString(b, i)
				continue
			case '{', '[':
				depth++
			case '}', ']':
				if depth--; depth == 0 {
					return i + 1
				}
			}
			i++
		}
		return i
	}
	for i < len(b) && !strings.ContainsRune(",}] \t\r\n", rune(b[i])) {
		i++
	}
	return i
}

// take removes the field name from o under every key that names it
// without regard to case, and returns its value, or nil when o holds no
// such field. A field under two keys is an error: the kernel takes the
// later key, and the front refuses to guess which one the kernel matches.
func (o *object) take(name string) (json.RawMessage, error) {
	var v json.RawMessage
	n := 0
	for i := range o.members {
		if m := &o.members[i]; !m.gone && strings.EqualFold(m.key, name) {
			v, n = m.value, n+1
			m.gone, o.changed = true, true
		}
	}
	for i := 0; i < len(o.added); i++ {
		if strings.EqualFold(o.added[i].key, name) {
			v, n = o.added[i].value, n+1
			o.added = append(o.added[:i], o.added[i+1:]...)
			i--
		}
	}
	if n > 1 {
		return nil, fmt.Errorf("the body names the field %s more than once", name)
	}
	return v, nil
}

// text takes the field name from o as a string, "" when o holds no such
// field or holds null.
func (o *object) text(name string) (string, error) {
	raw, err := o.take(name)
	if err != nil || raw == nil {
		return "", err
	}
	var s *string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", fmt.Errorf("the field %s is not a string", name)
	}
	if s == nil {
		return "", nil
	}
	return *s, nil
}

// set sets the field name of o to a string. The field must not be in o:
// take it first.
func (o *object) set(name, value string) {
	key, _ := json.Marshal(name)
	raw, _ := json.Marshal(value)
	o.added = append(o.added, objectMember{key: name, raw: append(append(key, ':'), raw...), value: raw})
	o.changed = true
}

// bytes returns the body: the body as the front read it when nothing
// changed, else the members that stay and the members that set added.
func (o *object) bytes() []byte {
	if !o.changed {
		return o.body
	}
	out := make([]byte, 0, len(o.body)+64)
	out = append(out, '{')
	for _, m := range append(slices.Clone(o.members), o.added...) {
		if m.gone {
			continue
		}
		if len(out) > 1 {
			out = append(out, ',')
		}
		out = append(out, m.raw...)
	}
	return append(out, '}')
}

// forPerson applies the rules of personBody to the body of the route
// pattern. It returns the agent that a start names, and the status and the
// detail of a problem, or a status of 0.
func (o *object) forPerson(pattern string, p *fw.Principal) (agent string, status int, detail string) {
	switch pattern {
	case routeMessages:
		from, err := o.take("from")
		switch {
		case err != nil:
			return "", http.StatusBadRequest, err.Error()
		case from != nil && !bytes.Equal(bytes.TrimSpace(from), []byte("null")):
			return "", http.StatusForbidden, "only the static token or a workload can name the sender of a message"
		}
	case routeReactions:
		by, err := o.text("by")
		switch {
		case err != nil:
			return "", http.StatusBadRequest, err.Error()
		case by != "" && by != p.Subject:
			return "", http.StatusForbidden, "only the static token or a workload can react for another person"
		}
		o.set("by", p.Subject)
	case routeStart:
		id, err := o.text("id")
		switch {
		case err != nil:
			return "", http.StatusBadRequest, err.Error()
		case id != "":
			return "", http.StatusForbidden, "only the static token or a workload can name the id of a run"
		}
		if agent, err = o.text("agent"); err != nil {
			return "", http.StatusBadRequest, err.Error()
		}
		if agent != "" {
			o.set("agent", agent)
		}
		for _, name := range []string{"principal", "principalKind"} {
			if _, err := o.take(name); err != nil {
				return "", http.StatusBadRequest, err.Error()
			}
		}
		o.set("principal", p.Subject)
		if p.Kind != "" {
			o.set("principalKind", p.Kind)
		}
	}
	return agent, 0, ""
}

// relay copies the answer of a member. An event stream flows through with
// a flush after each read. A JSON run object, and the run of a page of
// events, gets the field member, and a run object gets can when it is not
// nil. A 401 of the member becomes a 403, because the caller is signed in
// here and only the member refused the credential. When the request
// carried the static token for a person, tokenEnv names the secret of the
// token, and a 401 or a 403 of the member says that the member refused the
// token of the fleet (RefusedToken).
func relay(w http.ResponseWriter, r *http.Request, member string, resp *http.Response, tokenEnv string, can *Can, ended *time.Time) {
	if tokenEnv != "" && (resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden) {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		native.WriteProblem(w, r, http.StatusForbidden, RefusedToken(member, tokenEnv, resp.StatusCode, errorOf(member, resp.StatusCode, b).Detail))
		return
	}
	if resp.StatusCode == http.StatusUnauthorized {
		native.WriteProblem(w, r, http.StatusForbidden, fmt.Sprintf("the fleet member %s did not accept your credential; both members need the same issuer and audience, or the same token", member))
		return
	}
	h := w.Header()
	for _, k := range relayHeaders {
		if v := resp.Header.Get(k); v != "" {
			h.Set(k, v)
		}
	}
	mt, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	switch {
	case mt == "text/event-stream":
		rc := http.NewResponseController(w)
		// A write timeout of the server cuts a long stream, so it goes.
		_ = rc.SetWriteDeadline(time.Time{})
		w.WriteHeader(resp.StatusCode)
		_ = rc.Flush()
		buf := make([]byte, 32<<10)
		for {
			n, err := resp.Body.Read(buf)
			if n > 0 {
				if _, werr := w.Write(buf[:n]); werr != nil {
					return
				}
				_ = rc.Flush()
			}
			if err != nil {
				return
			}
		}
	case mt == "application/json" && resp.StatusCode < http.StatusMultipleChoices:
		b, err := io.ReadAll(io.LimitReader(resp.Body, maxRelayBytes+1))
		if err != nil || len(b) > maxRelayBytes {
			h.Del("Content-Type")
			native.WriteProblem(w, r, http.StatusBadGateway, fmt.Sprintf("this process cannot read the answer of the fleet member %s", member))
			return
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(withMemberJSON(b, member, can, ended))
	default:
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	}
}

// withMemberJSON sets the field member on a run object, or on the run of a
// page of events, the field can on a run object when can is not nil, and
// the field endedAt on a run object when ended is not nil. Any other JSON
// stays as it is.
func withMemberJSON(b []byte, member string, can *Can, ended *time.Time) []byte {
	var m map[string]json.RawMessage
	if json.Unmarshal(b, &m) != nil || m == nil {
		return b
	}
	name, _ := json.Marshal(member)
	switch {
	case m["id"] != nil && m["state"] != nil:
		m["member"] = name
		if can != nil {
			m["can"], _ = json.Marshal(can)
		}
		if ended != nil {
			m["endedAt"], _ = json.Marshal(ended)
		}
	case m["run"] != nil:
		var run map[string]json.RawMessage
		if json.Unmarshal(m["run"], &run) != nil || run == nil {
			return b
		}
		run["member"] = name
		raw, err := json.Marshal(run)
		if err != nil {
			return b
		}
		m["run"] = raw
	default:
		return b
	}
	out, err := json.Marshal(m)
	if err != nil {
		return b
	}
	return append(out, '\n')
}

// Fail writes the problem document of an error of a request that a
// surface sends to another member (ForRun, ForStart, ReadJSON), as the
// relay answers it: ErrNoRoute is 502, ErrNoToken 503, ErrNoCredential and
// a refusal 403, a run that the view does not give 404, and an error of
// the kernel the status that the run routes give it.
func Fail(w http.ResponseWriter, r *http.Request, err error) { fail(w, r, err) }

// fail writes the problem document of an error of the fleet or of the
// kernel, with the status that the run routes give it.
func fail(w http.ResponseWriter, r *http.Request, err error) {
	detail := strings.TrimPrefix(strings.TrimPrefix(err.Error(), "fleet: "), "fw: ")
	var me *MemberError
	switch {
	case errors.As(err, &me) && me.Status != http.StatusServiceUnavailable:
		// The answer of a member, with its words, as the relay copies it. The
		// caller is signed in here, so a 401 is a 403.
		status := me.Status
		switch {
		case status == http.StatusUnauthorized:
			status = http.StatusForbidden
		case status >= http.StatusInternalServerError:
			status = http.StatusBadGateway
		}
		native.WriteProblem(w, r, status, me.Detail)
	case errors.Is(err, fw.ErrAuthzUnavailable):
		w.Header().Set("Retry-After", strconv.Itoa(5))
		native.WriteProblem(w, r, http.StatusServiceUnavailable, "the authorization service did not answer: "+detail)
	case errors.Is(err, fw.ErrForbidden):
		native.WriteProblem(w, r, http.StatusForbidden, detail)
	case errors.Is(err, fw.ErrInvalidRequest):
		native.WriteProblem(w, r, http.StatusBadRequest, strings.TrimPrefix(detail, strings.TrimPrefix(fw.ErrInvalidRequest.Error(), "fw: ")+": "))
	case errors.Is(err, ErrNoRoute):
		native.WriteProblem(w, r, http.StatusBadGateway, detail)
	case errors.Is(err, ErrNoCredential):
		// The caller is signed in here, so a 401 would be wrong: only the
		// owner of the run cannot check this caller.
		native.WriteProblem(w, r, http.StatusForbidden, detail+"; sign in with a bearer token to act on a run of another member")
	case errors.Is(err, ErrNoToken):
		// An operator sets the token; the person can do nothing about it.
		native.WriteProblem(w, r, http.StatusServiceUnavailable, detail+"; until an operator sets it, the server acts for no person on another member")
	case errors.Is(err, fw.ErrRunNotFound):
		native.WriteProblem(w, r, http.StatusNotFound, "no run has the id "+strconv.Quote(r.PathValue("id")))
	default:
		native.WriteProblem(w, r, http.StatusInternalServerError, detail)
	}
}
