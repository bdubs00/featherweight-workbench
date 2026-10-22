package mcp

import (
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/surface/native"
	"github.com/bdubs00/featherweight-workbench/fleet"
	"github.com/bdubs00/featherweight-workbench/manager"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// fleetCallers maps a bearer token to a principal: three people and the
// static token of the fleet.
var fleetCallers = tokenMap{
	"alice-token": {Subject: "alice", Kind: fw.KindUser},
	"bob-token":   {Subject: "bob", Kind: fw.KindUser},
	"carol-token": {Subject: "carol", Kind: fw.KindUser},
	"fleet-token": {Subject: fw.KindToken, Kind: fw.KindToken},
}

// metaFor is the _meta of a call of the run mgr-1 of the profile manager
// for a person, as the MCP client of a worker sends it to a trusted server.
func metaFor(person string) sdk.Meta {
	return sdk.Meta{MetaRun: "mgr-1", MetaAgent: "manager", MetaNode: "default/manager-0", MetaPrincipal: person, MetaPrincipalKind: fw.KindUser}
}

// metaCall calls a tool with _meta.
func metaCall(t *testing.T, ctx context.Context, cs *sdk.ClientSession, meta sdk.Meta, name string, args any) *sdk.CallToolResult {
	t.Helper()
	res, err := cs.CallTool(ctx, &sdk.CallToolParams{Meta: meta, Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("call %s: %v", name, err)
	}
	return res
}

// deployModel asks for the tool deploy at the start of each turn and ends
// the turn after its result, so each run waits for an approval.
func deployModel() fw.Model {
	var n atomic.Int32
	return fw.ModelFunc{StreamFn: func(_ context.Context, req *fw.Request, _ func(fw.Delta)) (*fw.Response, error) {
		if len(req.Messages) > 0 {
			for _, b := range req.Messages[len(req.Messages)-1].Content {
				if b.Type == fw.BlockToolResult {
					return endTurn("done"), nil
				}
			}
		}
		return toolUse(fmt.Sprintf("c%d", n.Add(1)), "deploy", `{"target":"prod"}`), nil
	}}
}

// personFleet is a fake view of one other member, owner, that decides as
// the rules of a front with the token as an admin would: the token sees
// every run, and a person the runs that the person started. Runners are
// the people who can run an agent on the owner.
type personFleet struct {
	*fakeFleet
	runners map[string]bool

	mu     sync.Mutex
	checks []string
}

func visibleTo(ctx context.Context, r *fw.Run) bool {
	p := fw.PrincipalFrom(ctx)
	return p == nil || p.Kind == fw.KindToken || r.Principal == p.Subject
}

func (f *personFleet) Runs(ctx context.Context) ([]*fw.Run, error) {
	all, err := f.fakeFleet.Runs(ctx)
	return slices.DeleteFunc(all, func(r *fw.Run) bool { return !visibleTo(ctx, r) }), err
}

func (f *personFleet) Run(ctx context.Context, id string) (*fw.Run, error) {
	r, err := f.fakeFleet.Run(ctx, id)
	if err == nil && !visibleTo(ctx, r) {
		return nil, fw.ErrRunNotFound
	}
	return r, err
}

func (f *personFleet) AllowRun(ctx context.Context, id, action string) error {
	f.note(ctx, action+" "+id)
	r, err := f.fakeFleet.Run(ctx, id)
	if err != nil {
		return err
	}
	if !visibleTo(ctx, r) {
		return fmt.Errorf("%w: %s %s on %s", fw.ErrForbidden, fw.PrincipalFrom(ctx).Subject, action, id)
	}
	return nil
}

func (f *personFleet) AllowAgent(ctx context.Context, member, agent, action string) error {
	f.note(ctx, action+" "+member+"/"+agent)
	if p := fw.PrincipalFrom(ctx); p != nil && p.Kind != fw.KindToken && !f.runners[p.Subject] {
		return fmt.Errorf("%w: %s %s on %s", fw.ErrForbidden, p.Subject, action, agent)
	}
	return nil
}

func (f *personFleet) note(ctx context.Context, what string) {
	who := "process"
	if p := fw.PrincipalFrom(ctx); p != nil {
		who = p.Subject
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.checks = append(f.checks, who+" "+what)
}

func (f *personFleet) checked() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.checks)
}

// boss is a fleet for the tests of the on-behalf-of: a worker owner with
// the native API, whose static token is an admin there, and a front
// whose MCP server the tests call, with the guard of the manager.
type boss struct {
	owner, front *fw.Agent
	view         *personFleet
	url          string
}

// newBoss builds the fleet. admins are the admins of the front, which is
// closed: reviewer belongs to alice, bobs to bob, and private to carol, and
// alice and the token operate shared. In the rules, whoever has a role on
// an agent views all of its runs, so each person has an agent of their own.
func newBoss(t *testing.T, admins ...string) *boss {
	t.Helper()
	var calls atomic.Int32
	ocfg := testConfig(t)
	ocfg.Access.Admins = []string{"kind:token"}
	owner := newAgent(t, deployModel(), fw.WithConfig(ocfg), fw.WithAuthn(fleetCallers), fw.WithTools(deployTool(&calls)),
		fw.WithSurfaces(native.New()), fw.WithProfile(fw.AgentProfile{Name: "reviewer"}))
	h, err := owner.Handler()
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	view := &personFleet{fakeFleet: &fakeFleet{name: "front", owner: srv.URL}, runners: map[string]bool{"alice": true}}
	fcfg := testConfig(t)
	fcfg.Access.Default = "closed"
	fcfg.Access.Admins = admins
	fcfg.FileProfiles = map[string]bool{"reviewer": true, "bobs": true, "shared": true, "private": true}
	front := attach(newAgent(t, deployModel(), fw.WithConfig(fcfg), fw.WithAuthn(fleetCallers), fw.WithTools(deployTool(&calls)),
		fw.WithAuthzFrom(manager.Guard(nil)),
		fw.WithProfile(fw.AgentProfile{Name: "reviewer", Access: &fw.Access{Owners: []string{"alice"}}}),
		fw.WithProfile(fw.AgentProfile{Name: "bobs", Access: &fw.Access{Owners: []string{"bob"}}}),
		fw.WithProfile(fw.AgentProfile{Name: "shared", Access: &fw.Access{Operators: []string{"alice", "token"}}}),
		fw.WithProfile(fw.AgentProfile{Name: "private", Access: &fw.Access{Owners: []string{"carol"}}}),
	), view)
	if !manager.Guarded(front) {
		t.Fatal("the front has no guard")
	}
	return &boss{owner: owner, front: front, view: view, url: serveAgent(t, front, nil).URL}
}

// ownerRun starts a run of reviewer on the owner as a person and waits
// until it asks for the approval of deploy. The view knows the run.
func (b *boss) ownerRun(t *testing.T, ctx context.Context, person string, md map[string]string) *fw.Run {
	t.Helper()
	pctx := fw.WithPrincipal(ctx, &fw.Principal{Subject: person, Kind: fw.KindUser})
	r, err := b.owner.Start(pctx, "ship it", fw.AsAgent("reviewer"), fw.WithMetadata(md))
	if err != nil {
		t.Fatal(err)
	}
	r, err = b.owner.WaitFor(ctx, r.ID, func(r *fw.Run) bool { return r.Pending != nil || r.State.Terminal() })
	if err != nil || r.Pending == nil {
		t.Fatalf("owner run = %+v, %v", r, err)
	}
	runs, _ := b.view.fakeFleet.Runs(ctx)
	b.view.set(append(runs, r)...)
	return r
}

// frontRun starts a run on the front as a person and waits until it asks
// for the approval of deploy.
func (b *boss) frontRun(t *testing.T, ctx context.Context, person, agent string) *fw.Run {
	t.Helper()
	pctx := fw.WithPrincipal(ctx, &fw.Principal{Subject: person, Kind: fw.KindUser})
	r, err := b.front.Start(pctx, "ship it", fw.AsAgent(agent))
	if err != nil {
		t.Fatal(err)
	}
	r, err = b.front.WaitFor(ctx, r.ID, func(r *fw.Run) bool { return r.Pending != nil || r.State.Terminal() })
	if err != nil || r.Pending == nil {
		t.Fatalf("front run = %+v, %v", r, err)
	}
	return r
}

func listIDs(t *testing.T, res *sdk.CallToolResult) []string {
	t.Helper()
	if res.IsError {
		t.Fatalf("runs_list = %s", textOf(res))
	}
	var out runsResult
	structured(t, res, &out)
	var ids []string
	for _, r := range out.Runs {
		ids = append(ids, r.ID)
	}
	slices.Sort(ids)
	return ids
}

func sorted(ids ...string) []string {
	out := slices.Clone(ids)
	slices.Sort(out)
	return out
}

// The token with the person of a run in _meta reads as that person: the
// runs of this process and of the fleet narrow to the runs of the person,
// and a run of another person is not there. A person who sends the same
// _meta reads as themself.
func TestOnBehalfNarrowsTheView(t *testing.T) {
	ctx := testCtx(t)
	b := newBoss(t, "kind:token")
	ra := b.ownerRun(t, ctx, "alice", nil)
	rb := b.ownerRun(t, ctx, "bob", nil)
	la := b.frontRun(t, ctx, "alice", "reviewer")
	lb := b.frontRun(t, ctx, "bob", "bobs")
	token := as(t, ctx, b.url, "fleet-token")

	if got := listIDs(t, callTool(t, ctx, token, toolRunsList, map[string]any{})); !slices.Equal(got, sorted(ra.ID, rb.ID, la.ID, lb.ID)) {
		t.Fatalf("the token alone sees %v", got)
	}
	if got := listIDs(t, metaCall(t, ctx, token, metaFor("alice"), toolRunsList, map[string]any{})); !slices.Equal(got, sorted(ra.ID, la.ID)) {
		t.Fatalf("the token for alice sees %v", got)
	}
	if got := listIDs(t, metaCall(t, ctx, token, metaFor("bob"), toolRunsList, map[string]any{"scope": "local"})); !slices.Equal(got, []string{lb.ID}) {
		t.Fatalf("the token for bob sees here %v", got)
	}
	// The status of a run of another person is not there for alice, and
	// her own run of the owner is read with the credential of the token.
	if res := metaCall(t, ctx, token, metaFor("alice"), toolRunStatus, map[string]any{"runId": rb.ID}); !res.IsError {
		t.Fatalf("alice read the run of bob: %s", textOf(res))
	}
	if res := metaCall(t, ctx, token, metaFor("alice"), toolRunStatus, map[string]any{"runId": lb.ID}); !res.IsError {
		t.Fatalf("alice read the run of bob here: %s", textOf(res))
	}
	st := runOf(t, metaCall(t, ctx, token, metaFor("alice"), toolRunStatus, map[string]any{"runId": ra.ID}))
	if st.Member != "owner" || st.Pending == nil {
		t.Fatalf("the run of alice = %+v", st)
	}
	if h := b.view.lastForward(); h.Get("Authorization") != "Bearer fleet-token" {
		t.Fatalf("the forward carries %q", h.Get("Authorization"))
	}
	// whoami names the person, and agents_list her roles.
	var w whoamiResult
	structured(t, metaCall(t, ctx, token, metaFor("bob"), toolWhoami, map[string]any{}), &w)
	if w.Principal == nil || w.Principal.Subject != "bob" || w.Roles["bobs"] != "owner" || w.Roles["reviewer"] != "" {
		t.Fatalf("whoami for bob = %+v", w.Whoami)
	}
	res := metaCall(t, ctx, token, metaFor("bob"), toolAgentsList, map[string]any{})
	if text := textOf(res); !strings.Contains(text, "- bobs (you are an owner)") || strings.Contains(text, "- reviewer") || strings.Contains(text, "- private") {
		t.Fatalf("agents for bob = %s", text)
	}

	// A person who sends the keys of a run is no run: alice reads as
	// alice, also with the keys of bob.
	alice := as(t, ctx, b.url, "alice-token")
	if got := listIDs(t, metaCall(t, ctx, alice, metaFor("bob"), toolRunsList, map[string]any{})); !slices.Equal(got, sorted(ra.ID, la.ID)) {
		t.Fatalf("alice with the keys of bob sees %v", got)
	}
	carol := as(t, ctx, b.url, "carol-token")
	if got := listIDs(t, metaCall(t, ctx, carol, metaFor("alice"), toolRunsList, map[string]any{})); len(got) != 0 {
		t.Fatalf("carol with the keys of alice sees %v", got)
	}
	// _meta without a person keeps the caller: the token.
	noPerson := sdk.Meta{MetaRun: "mgr-1", MetaAgent: "manager"}
	if got := listIDs(t, metaCall(t, ctx, token, noPerson, toolRunsList, map[string]any{})); len(got) != 4 {
		t.Fatalf("the token with a run and no person sees %v", got)
	}
}

// A person that the token acts for never gets more than the token: alice
// is an admin of this front and the token is not, so through the token she
// reaches only what both reach.
func TestOnBehalfNeverWidensTheToken(t *testing.T) {
	ctx := testCtx(t)
	b := newBoss(t, "alice")
	lc := b.frontRun(t, ctx, "carol", "private")
	alice := as(t, ctx, b.url, "alice-token")
	token := as(t, ctx, b.url, "fleet-token")

	if res := callTool(t, ctx, alice, toolAgentGet, map[string]any{"name": "private"}); res.IsError {
		t.Fatalf("alice, an admin, reads private: %s", textOf(res))
	}
	if res := metaCall(t, ctx, token, metaFor("alice"), toolAgentGet, map[string]any{"name": "private"}); !res.IsError {
		t.Fatalf("the token for alice read private: %s", textOf(res))
	}
	if res := metaCall(t, ctx, token, metaFor("alice"), toolAgentGet, map[string]any{"name": "shared"}); res.IsError {
		t.Fatalf("the token for alice cannot read shared, which both operate: %s", textOf(res))
	}
	if got := listIDs(t, callTool(t, ctx, alice, toolRunsList, map[string]any{"scope": "local"})); !slices.Equal(got, []string{lc.ID}) {
		t.Fatalf("alice sees %v", got)
	}
	if got := listIDs(t, metaCall(t, ctx, token, metaFor("alice"), toolRunsList, map[string]any{"scope": "local"})); len(got) != 0 {
		t.Fatalf("the token for alice sees the run of carol: %v", got)
	}
	res := metaCall(t, ctx, token, metaFor("alice"), toolApprove, map[string]any{"runId": lc.ID, "approvalId": lc.Pending.ID, "decision": "allow"})
	if !res.IsError {
		t.Fatalf("the token for alice decided the run of carol: %s", textOf(res))
	}
	if r, _ := b.front.Get(ctx, lc.ID); r.Pending == nil {
		t.Fatal("the run of carol was decided")
	}
	// A run starts as the person only where the token can start it too.
	if res := metaCall(t, ctx, token, metaFor("alice"), toolRun, map[string]any{"agent": "private", "input": "x", "wait": false}); !res.IsError {
		t.Fatalf("the token for alice started private: %s", textOf(res))
	}
	r := runOf(t, metaCall(t, ctx, token, metaFor("alice"), toolRun, map[string]any{"agent": "shared", "input": "x", "wait": false}))
	if r.Principal != "alice" {
		t.Fatalf("the run of shared = %+v", r)
	}
}

// run and enqueue for a person start runs as that person, with fw.via that
// names the manager run, on this process, on another member, and on a
// queue; the member gets the token as the credential.
func TestOnBehalfStartsRunsAsThePerson(t *testing.T) {
	ctx := testCtx(t)
	b := newBoss(t, "kind:token")
	token := as(t, ctx, b.url, "fleet-token")

	local := runOf(t, metaCall(t, ctx, token, metaFor("alice"), toolRun, map[string]any{"agent": "reviewer", "input": "here", "wait": false}))
	if local.Principal != "alice" || queuedKind(t, b.front, local.ID) != fw.KindUser || local.Metadata[manager.MetaVia] != "manager:mgr-1" {
		t.Fatalf("local run = %+v", local)
	}
	remote := runOf(t, metaCall(t, ctx, token, metaFor("alice"), toolRun, map[string]any{"agent": "reviewer", "input": "there", "member": "owner"}))
	got, err := b.owner.Get(ctx, remote.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Principal != "alice" || queuedKind(t, b.owner, got.ID) != fw.KindUser || got.Metadata[manager.MetaVia] != "manager:mgr-1" {
		t.Fatalf("the run on the owner = %+v", got)
	}
	if h := b.view.lastForward(); h.Get("Authorization") != "Bearer fleet-token" {
		t.Fatalf("the forward carries %q", h.Get("Authorization"))
	}
	if !slices.Contains(b.view.checked(), "alice agent.run owner/reviewer") {
		t.Fatalf("the view decided %v", b.view.checked())
	}
	// bob cannot run reviewer on the owner, so the front starts nothing for him.
	if res := metaCall(t, ctx, token, metaFor("bob"), toolRun, map[string]any{"agent": "reviewer", "input": "there", "member": "owner"}); !res.IsError {
		t.Fatalf("the token for bob started a run on the owner: %s", textOf(res))
	}
	if res := metaCall(t, ctx, token, metaFor("alice"), toolRun, map[string]any{"input": "there", "member": "owner"}); !res.IsError || !strings.Contains(textOf(res), "name the agent") {
		t.Fatalf("a run for a person without an agent = %s", textOf(res))
	}
	// Only the front names fw.via.
	alice := as(t, ctx, b.url, "alice-token")
	res := callTool(t, ctx, alice, toolRun, map[string]any{"agent": "reviewer", "input": "x", "wait": false, "metadata": map[string]string{manager.MetaVia: "manager:forged"}})
	if !res.IsError || !strings.Contains(textOf(res), manager.MetaVia) {
		t.Fatalf("a caller set fw.via: %s", textOf(res))
	}
	// A person with the keys of a run starts the run as themself, with no fw.via.
	own := runOf(t, metaCall(t, ctx, alice, metaFor("bob"), toolRun, map[string]any{"agent": "reviewer", "input": "x", "wait": false}))
	if own.Principal != "alice" || own.Metadata[manager.MetaVia] != "" {
		t.Fatalf("alice with the keys of bob started %+v", own)
	}
}

// enqueue for a person puts runs of that person on the queue, each with
// fw.via.
func TestOnBehalfEnqueuesAsThePerson(t *testing.T) {
	ctx := testCtx(t)
	t.Setenv(fleet.EnvIntake, "fw.jobs")
	bus := fw.NewMemBus()
	wcfg := testConfig(t)
	wcfg.Bus.Intake = "fw.jobs.default"
	worker := newAgent(t, newScript(endTurn("done")).model(), fw.WithConfig(wcfg), fw.WithBus(bus))
	front := attach(newAgent(t, newScript().model(), fw.WithBus(bus), fw.WithAuthn(fleetCallers), fw.WithAuthzFrom(manager.Guard(nil))), &fakeFleet{name: "front"})
	token := as(t, ctx, serveAgent(t, front, nil).URL, "fleet-token")
	res := metaCall(t, ctx, token, metaFor("alice"), toolEnqueue, map[string]any{"agent": "default", "batch": "job-a", "items": []map[string]any{{"input": "piece"}}})
	if res.IsError {
		t.Fatalf("enqueue = %s", textOf(res))
	}
	var runs []*fw.Run
	deadline := time.Now().Add(5 * time.Second)
	for len(runs) == 0 && time.Now().Before(deadline) {
		runs, _ = worker.List(ctx)
		time.Sleep(20 * time.Millisecond)
	}
	if len(runs) != 1 || runs[0].Principal != "alice" || queuedKind(t, worker, runs[0].ID) != fw.KindUser || runs[0].Metadata[manager.MetaVia] != "manager:mgr-1" || runs[0].Metadata[fw.MetaBatch] != "job-a" {
		t.Fatalf("worker runs = %+v", runs)
	}
	res = metaCall(t, ctx, token, metaFor("alice"), toolEnqueue, map[string]any{"agent": "default", "items": []map[string]any{{"input": "piece", "metadata": map[string]string{manager.MetaVia: "x"}}}})
	if !res.IsError {
		t.Fatalf("an item set fw.via: %s", textOf(res))
	}
}

// approve for a person decides as the token, with a note that names the
// manager run and the person, on another member and on this process; the
// front refuses a run that the person does not operate.
func TestOnBehalfApproveNamesTheActor(t *testing.T) {
	ctx := testCtx(t)
	b := newBoss(t, "kind:token")
	ra := b.ownerRun(t, ctx, "alice", nil)
	rb := b.ownerRun(t, ctx, "bob", nil)
	la := b.frontRun(t, ctx, "alice", "reviewer")
	token := as(t, ctx, b.url, "fleet-token")

	res := metaCall(t, ctx, token, metaFor("alice"), toolApprove, map[string]any{"runId": ra.ID, "approvalId": ra.Pending.ID, "decision": "allow", "note": "looks right"})
	if res.IsError {
		t.Fatalf("approve = %s", textOf(res))
	}
	d := decided(t, b.owner, ra.ID)
	if d.Decider != fw.KindToken || d.Note != "by manager run mgr-1 for alice: looks right" || d.Decision != "allow" {
		t.Fatalf("the owner recorded %+v", d)
	}
	if !slices.Contains(b.view.checked(), "alice run.operate "+ra.ID) {
		t.Fatalf("the view decided %v", b.view.checked())
	}
	// alice does not operate the run of bob, though the token does.
	if res := metaCall(t, ctx, token, metaFor("alice"), toolApprove, map[string]any{"runId": rb.ID, "approvalId": rb.Pending.ID, "decision": "allow"}); !res.IsError {
		t.Fatalf("the token for alice decided the run of bob: %s", textOf(res))
	}
	if r, _ := b.owner.Get(ctx, rb.ID); r.Pending == nil {
		t.Fatal("the run of bob was decided")
	}
	// On this process, the decider and the note are the same.
	res = metaCall(t, ctx, token, metaFor("alice"), toolApprove, map[string]any{"runId": la.ID, "approvalId": la.Pending.ID, "decision": "deny"})
	if res.IsError {
		t.Fatalf("approve here = %s", textOf(res))
	}
	if d := decided(t, b.front, la.ID); d.Decider != fw.KindToken || d.Note != "by manager run mgr-1 for alice" || d.Decision != "deny" {
		t.Fatalf("the front recorded %+v", d)
	}
}

// queuedKind returns the kind of the person of a run, as fw.run.queued
// records it.
func queuedKind(t *testing.T, a *fw.Agent, id string) string {
	t.Helper()
	for e, err := range a.Ledger().Read(context.Background(), id, 0) {
		if err != nil {
			t.Fatal(err)
		}
		if e.Type == fw.EvRunQueued {
			var d fw.RunQueuedData
			if err := e.Decode(&d); err != nil {
				t.Fatal(err)
			}
			return d.PrincipalKind
		}
	}
	t.Fatalf("no %s event in %v", fw.EvRunQueued, ledgerTypes(t, a, id))
	return ""
}

// received returns the follow-up messages of a run.
func received(t *testing.T, a *fw.Agent, id string) []fw.MessageReceivedData {
	t.Helper()
	var out []fw.MessageReceivedData
	for e, err := range a.Ledger().Read(context.Background(), id, 0) {
		if err != nil {
			t.Fatal(err)
		}
		if e.Type == fw.EvMessageReceived {
			var d fw.MessageReceivedData
			if err := e.Decode(&d); err != nil {
				t.Fatal(err)
			}
			out = append(out, d)
		}
	}
	return out
}

// send for a person sends as the token, from the manager run, with a first
// line that names the person, on another member and on this process.
func TestOnBehalfSendNamesTheSender(t *testing.T) {
	ctx := testCtx(t)
	t.Setenv(manager.EnvManager, "manager")
	b := newBoss(t, "kind:token")
	ra := b.ownerRun(t, ctx, "alice", nil)
	la := b.frontRun(t, ctx, "alice", "reviewer")
	lb := b.frontRun(t, ctx, "bob", "bobs")
	// A run that waits for an approval holds a message, so each run ends
	// first, and the message starts a new turn.
	for _, x := range []struct {
		a *fw.Agent
		r *fw.Run
	}{{b.owner, ra}, {b.front, la}, {b.front, lb}} {
		if err := x.a.Decide(ctx, x.r.ID, fw.ApprovalDecision{ApprovalID: x.r.Pending.ID, Decision: "allow"}); err != nil {
			t.Fatal(err)
		}
		if _, err := x.a.WaitFor(ctx, x.r.ID, func(r *fw.Run) bool { return r.State.Terminal() }); err != nil {
			t.Fatal(err)
		}
	}
	token := as(t, ctx, b.url, "fleet-token")

	check := func(a *fw.Agent, id string) {
		t.Helper()
		msgs := received(t, a, id)
		if len(msgs) == 0 {
			t.Fatalf("no message reached %s", id)
		}
		m := msgs[len(msgs)-1]
		want := fw.MessageFrom{Kind: fw.FromAgent, Name: "manager", Run: "mgr-1"}
		if m.From == nil || *m.From != want || m.By != fw.KindToken {
			t.Fatalf("the message of %s = %+v", id, m)
		}
		if text := fw.TextOf(m.Content); !strings.HasPrefix(text, "From the manager run mgr-1 for alice.\nalso check the logs") {
			t.Fatalf("the text of %s = %q", id, text)
		}
	}
	if res := metaCall(t, ctx, token, metaFor("alice"), toolSend, map[string]any{"runId": ra.ID, "content": "also check the logs"}); res.IsError {
		t.Fatalf("send = %s", textOf(res))
	}
	check(b.owner, ra.ID)
	if res := metaCall(t, ctx, token, metaFor("alice"), toolSend, map[string]any{"runId": la.ID, "content": "also check the logs"}); res.IsError {
		t.Fatalf("send here = %s", textOf(res))
	}
	check(b.front, la.ID)
	// alice does not operate the run of bob here, though the token does.
	if res := metaCall(t, ctx, token, metaFor("alice"), toolSend, map[string]any{"runId": lb.ID, "content": "hi"}); !res.IsError {
		t.Fatalf("the token for alice sent to bob: %s", textOf(res))
	}
	if msgs := received(t, b.front, lb.ID); len(msgs) != 0 {
		t.Fatalf("bob got %+v", msgs)
	}
	// A manager run changes a budget with run_budget alone.
	res := metaCall(t, ctx, token, metaFor("alice"), toolSend, map[string]any{"runId": ra.ID, "content": "more", "budget": map[string]any{"maxTurns": 50}})
	if !res.IsError || !strings.Contains(textOf(res), "run_budget") {
		t.Fatalf("a manager run sent a budget: %s", textOf(res))
	}
}

// fakeBridges answers the delegation of every bridge with yes or no, or
// with an error, and records each question.
type fakeBridges struct {
	mu    sync.Mutex
	yes   bool
	err   error
	asked []string
}

func (f *fakeBridges) Delegates(_ context.Context, batch, action string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked = append(f.asked, batch+" "+action)
	return f.yes, f.err
}

func (f *fakeBridges) questions() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.asked)
}

// withBridges sets the Bridges of the manager for one test.
func withBridges(t *testing.T, b manager.Bridges) {
	t.Helper()
	manager.SetBridges(b)
	t.Cleanup(func() { manager.SetBridges(nil) })
}

// A manager run that approves, cancels, or changes the budget of a run of
// a bridge needs the delegation of the operator; another run and a run
// outside a bridge need none.
func TestManagerNeedsTheDelegationOfABridge(t *testing.T) {
	ctx := testCtx(t)
	t.Setenv(manager.EnvManager, "manager")
	b := newBoss(t, "kind:token")
	seat := b.ownerRun(t, ctx, "alice", map[string]string{fw.MetaBatch: "bridge:b1"})
	plain := b.ownerRun(t, ctx, "alice", nil)
	token := as(t, ctx, b.url, "fleet-token")
	approve := func(meta sdk.Meta, r *fw.Run) *sdk.CallToolResult {
		t.Helper()
		return metaCall(t, ctx, token, meta, toolApprove, map[string]any{"runId": r.ID, "approvalId": r.Pending.ID, "decision": "allow"})
	}

	// Without Bridges, the manager decides nothing in a bridge.
	withBridges(t, nil)
	res := approve(metaFor("alice"), seat)
	if !res.IsError || !strings.Contains(textOf(res), "the operator did not delegate approvals for this bridge") {
		t.Fatalf("approve without bridges = %s", textOf(res))
	}
	// The bridge says no.
	no := &fakeBridges{}
	withBridges(t, no)
	if res := approve(metaFor("alice"), seat); !res.IsError || !strings.Contains(textOf(res), "did not delegate approvals") {
		t.Fatalf("approve with no = %s", textOf(res))
	}
	if res := metaCall(t, ctx, token, metaFor("alice"), toolRunBudget, map[string]any{"runId": seat.ID, "budget": map[string]any{"maxTurns": 50}}); !res.IsError || !strings.Contains(textOf(res), "did not delegate budget changes") {
		t.Fatalf("run_budget with no = %s", textOf(res))
	}
	if res := metaCall(t, ctx, token, metaFor("alice"), toolCancel, map[string]any{"runId": seat.ID}); !res.IsError || !strings.Contains(textOf(res), "did not delegate changes to the members") {
		t.Fatalf("cancel with no = %s", textOf(res))
	}
	if got := no.questions(); !slices.Equal(got, []string{"bridge:b1 approve", "bridge:b1 budget", "bridge:b1 members"}) {
		t.Fatalf("the bridges were asked %v", got)
	}
	if r, _ := b.owner.Get(ctx, seat.ID); r.Pending == nil || r.State.Terminal() {
		t.Fatalf("the seat changed: %+v", r)
	}
	// Outside a bridge, and for a run of another profile, the policy of
	// the profile alone binds the call: nobody asks the bridges.
	if res := approve(metaFor("alice"), plain); res.IsError {
		t.Fatalf("approve outside a bridge = %s", textOf(res))
	}
	member := sdk.Meta{MetaRun: "mig-1", MetaAgent: "migrator", MetaPrincipal: "alice", MetaPrincipalKind: fw.KindUser}
	if res := metaCall(t, ctx, token, member, toolRunBudget, map[string]any{"runId": seat.ID, "budget": map[string]any{"maxTurns": 50}}); res.IsError {
		t.Fatalf("run_budget of a member run = %s", textOf(res))
	}
	if got := no.questions(); len(got) != 3 {
		t.Fatalf("the bridges were asked again: %v", got)
	}
	// The bridge says yes.
	yes := &fakeBridges{yes: true}
	withBridges(t, yes)
	if res := approve(metaFor("alice"), seat); res.IsError {
		t.Fatalf("approve with yes = %s", textOf(res))
	}
	if d := decided(t, b.owner, seat.ID); d.Decider != fw.KindToken || d.Note != "by manager run mgr-1 for alice" {
		t.Fatalf("the seat recorded %+v", d)
	}
	if res := metaCall(t, ctx, token, metaFor("alice"), toolCancel, map[string]any{"runId": seat.ID}); res.IsError {
		t.Fatalf("cancel with yes = %s", textOf(res))
	}
	if got := yes.questions(); !slices.Equal(got, []string{"bridge:b1 approve", "bridge:b1 members"}) {
		t.Fatalf("the bridges were asked %v", got)
	}
	// A bridge that fails refuses with its error.
	withBridges(t, &fakeBridges{err: errors.New("the bucket did not answer")})
	if res := metaCall(t, ctx, token, metaFor("alice"), toolRunBudget, map[string]any{"runId": seat.ID, "budget": map[string]any{"maxTurns": 60}}); !res.IsError || !strings.Contains(textOf(res), "the bucket did not answer") {
		t.Fatalf("run_budget with a failing bridge = %s", textOf(res))
	}
}
