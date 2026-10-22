package fleet

// This file is the relay of the native surface (native.Relay): it answers a
// request about a run that another member owns by sending it to that member
// with the credential of the caller, and it relays the answer as the member
// sent it. A read of the run object of a member that sleeps is the
// exception: the view answers it, because a request through the router of
// ax wakes the Task (ViewRoutes). POST /v1/runs?member=<name> starts a run
// on another member. A request that a member forwarded is never forwarded
// again. The relay also answers GET /v1/agents/{name} for a profile that
// the workers of a pool run (native.AgentRelay).

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
)

// maxRelayBytes bounds a JSON answer of a member that the relay rewrites.
const maxRelayBytes = 64 << 20

// relayRequestHeaders are the request headers that go to the member.
var relayRequestHeaders = []string{"Accept", "Content-Type", native.HeaderIdempotencyKey, "Last-Event-ID"}

// relayHeaders are the response headers that come back from the member.
var relayHeaders = []string{"Content-Type", "Cache-Control", "Retry-After", "Location", "X-Accel-Buffering",
	"Content-Disposition", "Content-Security-Policy", "X-Content-Type-Options"}

// Relay is the native.Relay of one agent. It reads the View of the agent at
// each request, so it works when the fleet starts after the surface.
type Relay struct{ a *fw.Agent }

// NewRelay returns the relay of an agent.
func NewRelay(a *fw.Agent) *Relay { return &Relay{a: a} }

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
		"POST /v1/runs/{id}/messages",
		"POST /v1/runs/{id}/reactions",
		"PATCH /v1/runs/{id}/budget",
		"POST /v1/runs/{id}/approvals/{approval}",
		"POST /v1/runs/{id}/cancel",
		"POST /v1/runs/{id}/fork",
	}
)

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
// goes to that member.
func (rl *Relay) Take(w http.ResponseWriter, r *http.Request) bool {
	if id := r.PathValue("id"); id != "" {
		run, ok := rl.owned(w, r, id)
		switch {
		case !ok:
			return true
		case run == nil:
			return false
		case rl.answerAtRest(w, r, run):
			return true
		}
		rl.Forward(w, r, run.Member)
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
// it and the relay sets member on it.
func (rl *Relay) answerAtRest(w http.ResponseWriter, r *http.Request, run *fw.Run) bool {
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
	native.WriteJSON(w, http.StatusOK, c)
	return true
}

// The relay answers the read of a profile of a pool.
var _ native.AgentRelay = (*Relay)(nil)

// TakeAgent implements native.AgentRelay. It answers GET /v1/agents/{name}
// with the profile that the workers of a pool run, as the MCP tool
// agent_get reads it (PoolAgentNamed): the profile with pool, the role of
// the caller, and the policy of the workers, and with no abilities,
// because the tools of the workers live in their Tasks. The query
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
	case err == nil, errors.Is(err, fw.ErrRunNotFound):
		return nil, true
	default:
		fail(w, r, err)
		return nil, false
	}
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
	rl.Forward(w, r, member)
	return true
}

// Forward sends the request to a member and relays the answer. The query
// parameter member stays here.
func (rl *Relay) Forward(w http.ResponseWriter, r *http.Request, member string) {
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
	var body io.Reader = http.NoBody
	if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Body != nil {
		// The member applies its own limit. A run or a message with files
		// is the largest body that a route takes.
		b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, native.MaxFilesBodyBytes))
		if err != nil {
			var tooBig *http.MaxBytesError
			if errors.As(err, &tooBig) {
				native.WriteProblem(w, r, http.StatusRequestEntityTooLarge, fmt.Sprintf("the body is larger than %d bytes", native.MaxFilesBodyBytes))
				return
			}
			native.WriteProblem(w, r, http.StatusBadRequest, "the body is not readable")
			return
		}
		body = bytes.NewReader(b)
	}
	out, err := http.NewRequestWithContext(r.Context(), r.Method, target, body)
	if err != nil {
		fail(w, r, fmt.Errorf("fleet: build the request for the member %s: %w", member, err))
		return
	}
	for _, k := range relayRequestHeaders {
		if v := r.Header.Get(k); v != "" {
			out.Header.Set(k, v)
		}
	}
	resp, err := f.Forward(r.Context(), member, out)
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
	relay(w, r, member, resp)
}

// relay copies the answer of a member. An event stream flows through with
// a flush after each read. A JSON run object, and the run of a page of
// events, gets the field member. A 401 of the member becomes a 403, because
// the caller is signed in here and only the member refused the credential.
func relay(w http.ResponseWriter, r *http.Request, member string, resp *http.Response) {
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
		_, _ = w.Write(withMemberJSON(b, member))
	default:
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	}
}

// withMemberJSON sets the field member on a run object, or on the run of a
// page of events. Any other JSON stays as it is.
func withMemberJSON(b []byte, member string) []byte {
	var m map[string]json.RawMessage
	if json.Unmarshal(b, &m) != nil || m == nil {
		return b
	}
	name, _ := json.Marshal(member)
	switch {
	case m["id"] != nil && m["state"] != nil:
		m["member"] = name
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

// fail writes the problem document of an error of the fleet or of the
// kernel, with the status that the run routes give it.
func fail(w http.ResponseWriter, r *http.Request, err error) {
	detail := strings.TrimPrefix(strings.TrimPrefix(err.Error(), "fleet: "), "fw: ")
	switch {
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
	default:
		native.WriteProblem(w, r, http.StatusInternalServerError, detail)
	}
}
