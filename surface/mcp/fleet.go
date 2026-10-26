package mcp

// This file serves the fleet (docs/ARCHITECTURE.md section 8.6). The tool
// fleet lists the members. A work tool that names a run of another member
// sends the request to the native API of that member. For a person, the
// front decides first, and the request carries the static token and names
// the person (onMember, fleet.ForStart). The static token and a workload
// send their own credential. The resources fw://runs/{id}, its ledger, and
// its files stay local. Without the fleet, nothing here adds a tool or
// changes one.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/dashboard"
	"github.com/bdubs00/featherweight-workbench/fleet"
	"github.com/bdubs00/featherweight-workbench/manager"
	"github.com/bdubs00/featherweight-workbench/pools"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// toolFleet is the name of the tool that lists the members of the fleet.
const toolFleet = "fleet"

// viewFleet is the dashboard view of the members.
const viewFleet = "fleet"

// maxMemberBody bounds an answer of a member.
const maxMemberBody = 64 << 20

// fleetResult is the structured result of the fleet tool. Pools holds the
// pools whose agent the caller can view.
type fleetResult struct {
	View    string         `json:"view"`
	Name    string         `json:"name"`
	Members []fleet.Member `json:"members"`
	Pools   []pools.Status `json:"pools,omitempty"`
}

// addFleetTool adds the fleet tool when the fleet is on.
func (s *server) addFleetTool() {
	if fleet.Of(s.a) == nil {
		return
	}
	sdk.AddTool(s.srv, &sdk.Tool{
		Name:        toolFleet,
		Title:       "Fleet",
		Description: "List the members of the fleet that you can see: the fw processes that share this bus. Each member has its state (live, quiet, or stopped), when it was last heard, its authorization mode, the agents that you can see, and its counts of active runs and of runs that wait for input. The pools of the server follow, each with the Tasks that it wants, the Tasks that exist and are ready, and the reason for a gap. runs_list lists the runs of every member, and the work tools act on a run of any member by its id.",
		InputSchema: json.RawMessage(emptySchema),
		Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true, OpenWorldHint: ptr(false)},
	}, s.fleetTool)
}

func (s *server) fleetTool(ctx context.Context, _ *sdk.CallToolRequest, _ struct{}) (*sdk.CallToolResult, any, error) {
	ctx, _, _ = manager.OnBehalf(ctx)
	res, err := s.members(ctx)
	if err != nil {
		return failResult(err), nil, nil
	}
	text := dashboard.MembersText(res.Name, res.Members)
	if len(res.Pools) > 0 {
		text += "\n\n" + dashboard.PoolsText(res.Pools)
	}
	return result(withPageURL(s.a, text), res), nil, nil
}

// withPageURL ends a text with the address of the dashboard page when the
// configuration names a public URL, for a host that shows no MCP App.
func withPageURL(a *fw.Agent, text string) string {
	if u := dashboard.PageURL(a.Config()); u != "" {
		return strings.TrimRight(text, "\n") + "\n\nOpen it in a browser: " + u
	}
	return text
}

// members lists the members that the caller can see.
func (s *server) members(ctx context.Context) (fleetResult, error) {
	f := fleet.Of(s.a)
	if f == nil {
		return fleetResult{}, errFleetOff
	}
	members, err := f.Members(ctx)
	if err != nil {
		return fleetResult{}, err
	}
	if members == nil {
		members = []fleet.Member{}
	}
	res := fleetResult{View: viewFleet, Name: f.Name(), Members: members}
	if pv, ok := f.(fleet.PoolView); ok {
		if res.Pools, err = pv.Pools(ctx); err != nil {
			return fleetResult{}, err
		}
	}
	return res, nil
}

// errFleetOff answers a request that needs the fleet when the fleet is off.
var errFleetOff = fmt.Errorf("%w: the fleet is off; set workbench.fleet.subject (FW_WORKBENCH_FLEET_SUBJECT) to turn it on", fw.ErrInvalidRequest)

// withCredential puts the bearer credential of the HTTP request of a call
// into ctx when ctx has none. Over a legacy session, the context of a call
// comes from the request that opened the session, and the header of the
// call itself is the right credential. A request for a person then takes
// its context from onMember or from fleet.ForStart, which put the static
// token in place of the credential of a person.
func withCredential(ctx context.Context, req sdk.Request) context.Context {
	if fw.CredentialFrom(ctx) != "" || req == nil {
		return ctx
	}
	ex := req.GetExtra()
	if ex == nil || ex.Header == nil {
		return ctx
	}
	h := strings.TrimSpace(ex.Header.Get("Authorization"))
	scheme, tok, ok := strings.Cut(h, " ")
	if !ok || !strings.EqualFold(scheme, "bearer") || strings.TrimSpace(tok) == "" {
		return ctx
	}
	return fw.WithCredential(ctx, h)
}

// ownerOf returns the member that owns a run of another member, or "" when
// the fleet is off or does not know the run.
func (s *server) ownerOf(ctx context.Context, id string) (string, error) {
	f := fleet.Of(s.a)
	if f == nil {
		return "", nil
	}
	r, err := f.Run(ctx, id)
	switch {
	case errors.Is(err, fw.ErrRunNotFound):
		return "", nil
	case err != nil:
		return "", err
	}
	return r.Member, nil
}

// memberError is an error status from the native API of a member. token
// is true when the request carried the static token for a person, and the
// detail then words a refusal of that token for an operator
// (fleet.RefusedToken).
type memberError struct {
	member string
	status int
	detail string
	token  bool
}

func (e *memberError) Error() string {
	return fmt.Sprintf("mcp: the fleet member %s answered %d: %s", e.member, e.status, e.detail)
}

// text is the error as a person reads it in a tool result.
func (e *memberError) text() string {
	if e.token {
		return "Forbidden: " + e.detail + "."
	}
	switch e.status {
	case http.StatusUnauthorized:
		return fmt.Sprintf("Forbidden: the fleet member %s did not accept your credential. Both members need the same issuer and audience, or the same token.", e.member)
	case http.StatusForbidden:
		return fmt.Sprintf("Forbidden: the fleet member %s refused the request: %s.", e.member, e.detail)
	case http.StatusNotFound:
		return fmt.Sprintf("Not found: the fleet member %s answered: %s.", e.member, e.detail)
	case http.StatusServiceUnavailable:
		return fmt.Sprintf("Unavailable: the fleet member %s answered: %s. Try again in a few seconds.", e.member, e.detail)
	}
	return fmt.Sprintf("The fleet member %s answered %d: %s.", e.member, e.status, e.detail)
}

// callMember sends a request to the native API of a member and decodes the
// JSON answer into v. An error status gives a *memberError with the detail
// of the problem document.
func (s *server) callMember(ctx context.Context, member, method, path string, body any, idemKey string, v any) error {
	f := fleet.Of(s.a)
	if f == nil {
		return errFleetOff
	}
	var rd io.Reader = http.NoBody
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("mcp: encode the request for the member %s: %w", member, err)
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, path, rd)
	if err != nil {
		return fmt.Errorf("mcp: build the request for the member %s: %w", member, err)
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if idemKey != "" {
		req.Header.Set("Idempotency-Key", idemKey)
	}
	resp, err := f.Forward(ctx, member, req)
	if err != nil {
		if errors.Is(err, fleet.ErrNoRoute) || errors.Is(err, fleet.ErrNoCredential) {
			return err
		}
		return fmt.Errorf("mcp: the fleet member %s did not answer: %w", member, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxMemberBody))
	if err != nil {
		return fmt.Errorf("mcp: read the answer of the member %s: %w", member, err)
	}
	if resp.StatusCode >= http.StatusMultipleChoices {
		return tokenRefusal(ctx, problemOf(member, resp.StatusCode, b))
	}
	if v == nil {
		return nil
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("mcp: the answer of the member %s is not JSON: %w", member, err)
	}
	return nil
}

// memberFile reads the bytes of a file of a run of another member through
// its native API, which applies the reader rules of the member. It reads
// at most maxRawBytes+1 bytes. size is the size that the member names, or
// -1 when the answer does not name it.
func (s *server) memberFile(ctx context.Context, member, id, path string) (data []byte, size int64, err error) {
	f := fleet.Of(s.a)
	if f == nil {
		return nil, -1, errFleetOff
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "/v1/runs/"+url.PathEscape(id)+"/files/"+escapePath(path), http.NoBody)
	if err != nil {
		return nil, -1, fmt.Errorf("mcp: build the request for the member %s: %w", member, err)
	}
	req.Header.Set("Accept", "application/octet-stream")
	resp, err := f.Forward(ctx, member, req)
	if err != nil {
		if errors.Is(err, fleet.ErrNoRoute) || errors.Is(err, fleet.ErrNoCredential) {
			return nil, -1, err
		}
		return nil, -1, fmt.Errorf("mcp: the fleet member %s did not answer: %w", member, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= http.StatusMultipleChoices {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		return nil, -1, tokenRefusal(ctx, problemOf(member, resp.StatusCode, b))
	}
	if resp.ContentLength > maxRawBytes {
		return nil, resp.ContentLength, nil
	}
	data, err = io.ReadAll(io.LimitReader(resp.Body, maxRawBytes+1))
	if err != nil {
		return nil, -1, fmt.Errorf("mcp: read the answer of the member %s: %w", member, err)
	}
	if len(data) > maxRawBytes {
		return data, resp.ContentLength, nil
	}
	return data, int64(len(data)), nil
}

// problemOf is the *memberError of an error status and its body, with the
// detail of a problem document.
func problemOf(member string, status int, b []byte) *memberError {
	var p struct {
		Detail string `json:"detail"`
	}
	detail := strings.TrimSpace(string(b))
	if json.Unmarshal(b, &p) == nil && p.Detail != "" {
		detail = p.Detail
	}
	if detail == "" {
		detail = http.StatusText(status)
	}
	return &memberError{member: member, status: status, detail: truncate(detail, 500)}
}

// tokenRefusal is e, or a refusal of the static token when the request of
// ctx carried that token for a person: a 401 or a 403 of the member is
// then the problem of an operator (fleet.RefusedToken).
func tokenRefusal(ctx context.Context, e *memberError) *memberError {
	env := fleet.TokenEnvOf(ctx)
	if env == "" || (e.status != http.StatusUnauthorized && e.status != http.StatusForbidden) {
		return e
	}
	return &memberError{member: e.member, status: http.StatusForbidden, detail: fleet.RefusedToken(e.member, env, e.status, e.detail), token: true}
}

// memberErr gives an error answer of a read through fleet.ReadJSON the
// text of the other requests to a member (memberError).
func memberErr(err error) error {
	var me *fleet.MemberError
	if errors.As(err, &me) {
		return &memberError{member: me.Member, status: me.Status, detail: me.Detail, token: me.Token}
	}
	return err
}

// memberRun calls a run route of a member and returns the run object with
// its member.
func (s *server) memberRun(ctx context.Context, member, method, path string, body any, idemKey string) (*fw.Run, error) {
	var r fw.Run
	if err := s.callMember(ctx, member, method, path, body, idemKey, &r); err != nil {
		return nil, err
	}
	// A member names another member for a run that it only knows about,
	// such as a branch in a child Task; the name of the view applies.
	if r.Member != "" {
		r.Member = fleet.MemberName(r.Member)
	} else {
		r.Member = member
	}
	return &r, nil
}

// remoteResult is the result of a work tool that a member answered.
func remoteResult(r *fw.Run, err error) *sdk.CallToolResult {
	if err != nil {
		return failResult(err)
	}
	return runResult(r)
}

// remoteEvents reads a page of the events of a run of another member, in
// the shape of run_events. A page before a sequence number reads forward
// from the start of that page, because the sequence numbers of a run have
// no gaps. Then a member of any version answers it.
func (s *server) remoteEvents(ctx context.Context, member, id string, p page) (eventsResult, error) {
	from, limit := p.from, p.limit
	if p.before > 0 {
		if p.before == 1 {
			return s.remoteEmpty(ctx, member, id)
		}
		from = 1
		if p.before > uint64(limit) {
			from = p.before - uint64(limit)
		}
		limit = int(p.before - from)
	}
	q := url.Values{}
	ask := limit
	if from > 0 {
		q.Set("from", strconv.FormatUint(from, 10))
		ask++ // one past the page marks where the next page starts
	}
	q.Set("limit", strconv.Itoa(ask))
	var ans struct {
		Run    *fw.Run    `json:"run"`
		Events []fw.Event `json:"events"`
	}
	if err := s.callMember(ctx, member, http.MethodGet, "/v1/runs/"+url.PathEscape(id)+"/events?"+q.Encode(), nil, "", &ans); err != nil {
		return eventsResult{}, err
	}
	if ans.Run == nil {
		return eventsResult{}, fmt.Errorf("mcp: the answer of the member %s holds no run", member)
	}
	ans.Run.Member = member
	events, next := ans.Events, uint64(0)
	if from > 0 && len(events) > limit {
		next = events[limit].Seq
		events = events[:limit]
	}
	if p.before > 0 {
		next = 0 // a page read back names the page in front of it only
	}
	return pageOf(ans.Run, events, next), nil
}

// remoteEmpty is the empty page in front of the first event of a run of
// another member: the run object with no event.
func (s *server) remoteEmpty(ctx context.Context, member, id string) (eventsResult, error) {
	r, err := s.memberRun(ctx, member, http.MethodGet, "/v1/runs/"+url.PathEscape(id), nil, "")
	if err != nil {
		return eventsResult{}, err
	}
	return pageOf(r, nil, 0), nil
}

// startOn starts a run on another member through its native API, in the
// context of fleet.ForStart. The call never waits: the client polls
// run_status, which reaches the member too. For a person, the person of an
// actor or a person who calls with her own credential, the run is for that
// person: the member takes principal and principalKind from the static
// token, which the request carries (the kernel architecture, section 11.9).
func (s *server) startOn(ctx context.Context, member string, in runArgs, act manager.Actor, acting bool) *sdk.CallToolResult {
	body := map[string]any{"input": in.Input, "wait": false}
	if p := fw.PrincipalFrom(ctx); fleet.IsPerson(p) {
		body["principal"], body["principalKind"] = p.Subject, p.Kind
	}
	if in.Agent != "" {
		body["agent"] = in.Agent
	}
	if in.Instructions != "" {
		body["instructions"] = in.Instructions
	}
	if in.Profile != nil {
		body["profile"] = in.Profile
	}
	if in.Outcome != nil {
		body["outcome"] = in.Outcome
	}
	if in.Budget != nil {
		body["budget"] = in.Budget
	}
	if len(in.Files) > 0 {
		body["files"] = in.Files
	}
	if len(in.Metadata) > 0 {
		// The metadata carries the title, so a member of any version keeps it.
		body["metadata"] = in.Metadata
	}
	r, err := s.memberRun(ctx, member, http.MethodPost, "/v1/runs", body, in.RequestID)
	if err != nil {
		return failResult(err)
	}
	if acting {
		s.log.Info("mcp run started on a fleet member for an actor", "run", r.ID, "agent", r.Agent, "member", member, "actor", act.String())
	} else {
		s.log.Info("mcp run started on a fleet member", "run", r.ID, "agent", r.Agent, "member", member)
	}
	res := runResult(r)
	res.Content = []sdk.Content{&sdk.TextContent{Text: fmt.Sprintf(
		"Started run %s of the agent %s on the fleet member %s. The run is %s. A run on another member never waits here: poll it with run_status, which reaches the member.",
		r.ID, r.Agent, member, r.State)}}
	return res
}

// fleetInstructions is the part of the server instructions about the
// fleet, or "" when the fleet is off.
func fleetInstructions(a *fw.Agent) string {
	f := fleet.Of(a)
	if f == nil {
		return ""
	}
	return fmt.Sprintf("\n\nFleet: this process is the member %s of a fleet of fw processes that share one bus. The fleet tool lists the members. runs_list lists the runs of every member; scope local keeps the runs of this process, and member keeps the runs of one member. run_status, run_events, run_result, run_file, approve, cancel, and send act on a run of any member by its id: for a person, the server decides with the rules of what the person sees, and the request goes to the member that owns the run with the token of the fleet; the static token and a workload send their own credential, and that member decides. agent_builder with fromRun reads the run from the member that owns it and starts the builder in this process. run with member starts a run on another member and returns at once. The resources fw://runs/{id} read the runs of this process only.", f.Name())
}
