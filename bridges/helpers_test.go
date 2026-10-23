package bridges

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/surface/native"
	"github.com/bdubs00/featherweight-workbench/fleet"
	"github.com/bdubs00/featherweight-workbench/internal/bucket"
	"github.com/bdubs00/featherweight-workbench/pages"
	"github.com/bdubs00/featherweight-workbench/surface/mcp"
	"github.com/bdubs00/featherweight-workbench/teams"
	"github.com/bdubs00/featherweight-workbench/titles"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func quiet() *slog.Logger { return slog.New(slog.DiscardHandler) }

// tokenMap is a test Authn that maps a bearer token to a principal.
type tokenMap map[string]*fw.Principal

func (m tokenMap) Authenticate(r *http.Request) (*fw.Principal, error) {
	tok := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	if p, ok := m[tok]; ok && tok != "" {
		return p, nil
	}
	return nil, fw.ErrUnauthorized
}

func (m tokenMap) Challenge(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="fw"`)
	w.WriteHeader(http.StatusUnauthorized)
}

// callers: alice owns the agents migrator and reviewer, bob operates
// both, carol has no role, dave owns private, and fleet is the static
// token of the fleet. alice, bob, and dave share the team team-a.
var callers = tokenMap{
	"alice": {Subject: "alice", Name: "Alice", Kind: fw.KindUser, Groups: []string{"team-a"}},
	"bob":   {Subject: "bob", Kind: fw.KindUser, Groups: []string{"team-a"}},
	"carol": {Subject: "carol", Kind: fw.KindUser},
	"dave":  {Subject: "dave", Kind: fw.KindUser, Groups: []string{"team-a"}},
	"fleet": {Subject: fw.KindToken, Kind: fw.KindToken},
}

// fleetToken is the static token of the fleet that the front sends.
const fleetToken = "fleet-secret"

// textModel ends every turn with the same text, for a run of the front.
func textModel(out string) fw.Model {
	return fw.ModelFunc{StreamFn: func(context.Context, *fw.Request, func(fw.Delta)) (*fw.Response, error) {
		return &fw.Response{Content: []fw.Block{fw.Text(out)}, StopReason: fw.StopEndTurn, Usage: fw.Usage{InputTokens: 3, OutputTokens: 2}}, nil
	}}
}

// newAgent returns an agent with the native surface, the wiki in memory,
// and three closed profiles: migrator and reviewer, which alice owns and
// bob operates, and private, which dave owns. It names the intake prefix
// fw.jobs, so a queue bridge reaches the queues fw.jobs.<agent>.
func newAgent(t *testing.T, mut func(*fw.Config)) *fw.Agent {
	t.Helper()
	return newAgentWith(t, mut)
}

// newAgentWith is newAgent with more options, which win over the ones of
// newAgent, such as another model or a tool.
func newAgentWith(t *testing.T, mut func(*fw.Config), opts ...fw.Option) *fw.Agent {
	t.Helper()
	t.Setenv(titles.EnvTitles, titles.ModeInput)
	t.Setenv("FW_ATESPACE", "")
	t.Setenv("FW_API_TOKEN", fleetToken)
	t.Setenv(fleet.EnvIntake, "fw.jobs")
	t.Setenv(teams.EnvTeams, "")
	for _, env := range []string{EnvPlacement, EnvMaxMembers, EnvMaxMessages, EnvMaxFanoutPerMinute, EnvMaxPairMessages, EnvManager} {
		t.Setenv(env, "")
	}
	cfg := fw.DefaultConfig()
	cfg.Home = t.TempDir()
	cfg.Access.Default = "closed"
	if mut != nil {
		mut(cfg)
	}
	a, err := fw.New(append([]fw.Option{
		fw.WithConfig(cfg),
		fw.WithoutDefaults(),
		fw.WithAuthn(callers),
		fw.WithModel(textModel("done")),
		fw.WithLedger(fw.NewMemLedger()),
		fw.WithBus(fw.NewMemBus()),
		fw.WithLogger(quiet()),
		fw.WithSurfaces(native.New()),
		fw.WithServices(pages.Start),
		fw.WithProfile(fw.AgentProfile{Name: "migrator", Access: &fw.Access{Owners: []string{"alice"}, Operators: []string{"bob"}}}),
		fw.WithProfile(fw.AgentProfile{Name: "reviewer", Access: &fw.Access{Owners: []string{"alice"}, Operators: []string{"bob"}}}),
		fw.WithProfile(fw.AgentProfile{Name: "private", Access: &fw.Access{Owners: []string{"dave"}}}),
	}, opts...)...)
	if err != nil {
		t.Fatalf("fw.New: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		a.Close(ctx)
	})
	return a
}

// newStore attaches bridges in memory to the agent, with short pauses for
// the retries and no hold before a line asleep. The test drives the watch
// with poll.
func newStore(t *testing.T, a *fw.Agent, mut func(*Config)) *Store {
	t.Helper()
	rc, cc := bucketConfigs()
	return newStoreOn(t, a, bucket.NewMem(rc), bucket.NewMem(cc), mut)
}

// newStoreOn is newStore on the buckets of the records and the channels,
// so two stores over the same buckets act as two replicas of the front.
func newStoreOn(t *testing.T, a *fw.Agent, records, channels bucket.Bucket, mut func(*Config)) *Store {
	t.Helper()
	cfg, err := Load(a.Config())
	if err != nil {
		t.Fatal(err)
	}
	if mut != nil {
		mut(&cfg)
	}
	s := New(a, records, channels, cfg, quiet())
	s.pollEvery, s.retryFirst, s.retryMax, s.retryFor = time.Hour, 10*time.Millisecond, 40*time.Millisecond, 3*time.Second
	s.sleepHold = 0
	s.runWorkers()
	Attach(a, s)
	t.Cleanup(func() {
		Attach(a, nil)
		s.Close()
	})
	return s
}

// serve serves the native surface of the agent.
func serve(t *testing.T, a *fw.Agent) string {
	t.Helper()
	h, err := a.Handler()
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv.URL
}

// do sends a request as the caller with the token and returns the
// response with its body read.
func do(t *testing.T, method, url, token, body string) (*http.Response, string) {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, url, rd)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, string(b)
}

func decodeJSON(t *testing.T, body string, v any) {
	t.Helper()
	if err := json.Unmarshal([]byte(body), v); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out: %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// fakeView is a view of the fleet that a test states: the runs of the
// other members, the members, the queues of the pools, and a fake member
// that every forward reaches.
type fakeView struct {
	mu      sync.Mutex
	runs    map[string]*fw.Run
	members []fleet.Member
	target  string
	// settled is what Settled answers: whether the view holds every run of
	// the fleet (fleet.SettledView).
	settled bool
	// queues holds the intake of a pool for the jobs of an agent, by
	// "<agent>@<pool>", and by "<agent>@" for the pool that runs the agent
	// (fleet.QueueView).
	queues map[string]string
}

// PoolQueue places a job as the queues of the view say. A pool that they
// do not name for the agent is an error, as on a fleet.
func (v *fakeView) PoolQueue(_ context.Context, agent, pool string) (string, bool, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	queue, ok := v.queues[agent+"@"+pool]
	if !ok && pool != "" {
		return "", false, fmt.Errorf("%w: the pool %s does not run the agent %s", fw.ErrInvalidRequest, pool, agent)
	}
	return queue, ok, nil
}

// pool states that the pool takes the jobs of the agent on queue; an empty
// pool states the pool that runs the agent.
func (v *fakeView) pool(agent, pool, queue string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.queues == nil {
		v.queues = map[string]string{}
	}
	v.queues[agent+"@"+pool] = queue
}

func (v *fakeView) Settled() bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.settled
}

// settle states whether the view holds every run of the fleet.
func (v *fakeView) settle(yes bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.settled = yes
}

// drop takes a run out of the view, as a fleet that lost it.
func (v *fakeView) drop(id string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	delete(v.runs, id)
}

func newView(target string) *fakeView {
	return &fakeView{runs: map[string]*fw.Run{}, target: target}
}

func (v *fakeView) Name() string { return "front" }

func (v *fakeView) Members(context.Context) ([]fleet.Member, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	return append([]fleet.Member(nil), v.members...), nil
}

func (v *fakeView) Runs(context.Context) ([]*fw.Run, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	var out []*fw.Run
	for _, r := range v.runs {
		c := *r
		out = append(out, &c)
	}
	return out, nil
}

func (v *fakeView) Run(_ context.Context, id string) (*fw.Run, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if r, ok := v.runs[id]; ok {
		c := *r
		return &c, nil
	}
	return nil, fmt.Errorf("fake: %w", fw.ErrRunNotFound)
}

func (v *fakeView) Batch(_ context.Context, batch string) (fleet.BatchRuns, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	var out fleet.BatchRuns
	for _, r := range v.runs {
		if r.Metadata[fw.MetaBatch] == batch {
			c := *r
			out.Runs = append(out.Runs, &c)
		}
	}
	return out, nil
}

func (v *fakeView) Forward(ctx context.Context, member string, req *http.Request) (*http.Response, error) {
	cred := fw.CredentialFrom(ctx)
	if cred == "" {
		return nil, fleet.ErrNoCredential
	}
	u := v.target + req.URL.Path
	if req.URL.RawQuery != "" {
		u += "?" + req.URL.RawQuery
	}
	out, err := http.NewRequestWithContext(ctx, req.Method, u, req.Body)
	if err != nil {
		return nil, err
	}
	out.Header = req.Header.Clone()
	out.Header.Set("Authorization", cred)
	out.Header.Set("X-Member", member)
	return http.DefaultClient.Do(out)
}

// put states a run of a member in the view.
func (v *fakeView) put(r *fw.Run) {
	v.mu.Lock()
	defer v.mu.Unlock()
	c := *r
	v.runs[r.ID] = &c
}

// state states a member of the view.
func (v *fakeView) state(name, state string, agents ...string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	m := fleet.Member{Name: name, State: state}
	for _, ag := range agents {
		m.Agents = append(m.Agents, fleet.RecordAgent{Name: ag, Role: "owner"})
	}
	for i := range v.members {
		if v.members[i].Name == name {
			v.members[i] = m
			return
		}
	}
	v.members = append(v.members, m)
}

// seen is one request that reached the fake member.
type seen struct {
	Method, Path, Auth, Member, Idem string
	Body                             map[string]any
}

// fakeMember is a member of the fleet that answers the forwards of the
// front and records each one.
type fakeMember struct {
	mu     sync.Mutex
	srv    *httptest.Server
	reqs   []seen
	events map[string][]fw.Event // by run id
	fail   map[string]int        // by path: the 503 answers before a 200
	status map[string]int        // by path: a fixed answer
	detail map[string]string     // by path: the detail of the fixed answer
	ids    map[string]string     // by idempotency key: the run that a start answers, else mgr-run-1
}

func newMember(t *testing.T) *fakeMember {
	fm := &fakeMember{events: map[string][]fw.Event{}, fail: map[string]int{}, status: map[string]int{}, detail: map[string]string{}, ids: map[string]string{}}
	fm.srv = httptest.NewServer(http.HandlerFunc(fm.serve))
	t.Cleanup(fm.srv.Close)
	return fm
}

func (fm *fakeMember) serve(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	var body map[string]any
	_ = json.Unmarshal(b, &body)
	fm.mu.Lock()
	fm.reqs = append(fm.reqs, seen{Method: r.Method, Path: r.URL.Path, Auth: r.Header.Get("Authorization"), Member: r.Header.Get("X-Member"), Idem: r.Header.Get("Idempotency-Key"), Body: body})
	if n := fm.fail[r.URL.Path]; n > 0 {
		fm.fail[r.URL.Path] = n - 1
		fm.mu.Unlock()
		http.Error(w, `{"detail":"the Task wakes"}`, http.StatusServiceUnavailable)
		return
	}
	if code := fm.status[r.URL.Path]; code != 0 {
		detail := fm.detail[r.URL.Path]
		fm.mu.Unlock()
		if detail == "" {
			detail = "refused"
		}
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(map[string]string{"detail": detail})
		return
	}
	events := fm.events
	id := fm.ids[r.Header.Get("Idempotency-Key")]
	fm.mu.Unlock()
	if id == "" {
		id = "mgr-run-1"
	}
	w.Header().Set("Content-Type", "application/json")
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/v1/runs/"), "/")
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/v1/runs":
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(fw.Run{ID: id, Agent: fmt.Sprint(body["agent"]), State: fw.StateQueued})
	case r.Method == http.MethodGet && len(parts) == 2 && parts[1] == "events":
		fm.mu.Lock()
		list := append([]fw.Event(nil), events[parts[0]]...)
		fm.mu.Unlock()
		q := r.URL.Query()
		limit, _ := strconv.Atoi(q.Get("limit"))
		from, _ := strconv.ParseUint(q.Get("from"), 10, 64)
		var out []fw.Event
		for _, e := range list {
			if from == 0 || e.Seq >= from {
				out = append(out, e)
			}
		}
		if from == 0 && limit > 0 && len(out) > limit {
			out = out[len(out)-limit:]
		}
		if from > 0 && limit > 0 && len(out) > limit {
			out = out[:limit]
		}
		if out == nil {
			out = []fw.Event{}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"run": fw.Run{ID: parts[0]}, "events": out})
	default:
		_ = json.NewEncoder(w).Encode(fw.Run{ID: parts[0]})
	}
}

// find returns the requests with the method and the path.
func (fm *fakeMember) find(method, path string) []seen {
	fm.mu.Lock()
	defer fm.mu.Unlock()
	var out []seen
	for _, s := range fm.reqs {
		if s.Method == method && s.Path == path {
			out = append(out, s)
		}
	}
	return out
}

// setEvents states the ledger of a run, with seqs from 1.
func (fm *fakeMember) setEvents(run string, evs ...fw.Event) {
	fm.mu.Lock()
	defer fm.mu.Unlock()
	for i := range evs {
		evs[i].Subject, evs[i].Seq = run, uint64(i+1)
		if evs[i].Time.IsZero() {
			evs[i].Time = time.Now().UTC()
		}
	}
	fm.events[run] = evs
}

// withFleet attaches a fake view with a fake member to the agent.
func withFleet(t *testing.T, a *fw.Agent) (*fakeView, *fakeMember) {
	t.Helper()
	fm := newMember(t)
	v := newView(fm.srv.URL)
	fleet.Attach(a, v)
	t.Cleanup(func() { fleet.Attach(a, nil) })
	return v, fm
}

// workerRun is a run of a seat on the member crew-0 of the view.
func workerRun(id, bridge, handle, agent string, state fw.RunState) *fw.Run {
	return &fw.Run{ID: id, Agent: agent, State: state, Member: "default/crew-0", Principal: "alice", Input: "the task of " + handle,
		Metadata: map[string]string{fw.MetaBatch: BatchOf(bridge), MetaMember: handle}, CreatedAt: time.Now().UTC(), Seq: 3}
}

// open opens a queue bridge with a migrator and a reviewer as alice.
func open(t *testing.T, url string) Item {
	t.Helper()
	resp, body := do(t, http.MethodPost, url+"/v1/bridges", "alice", `{"title":"Migrate logrotate","goal":"Migrate the module and review it.","members":[
		{"handle":"migrator-1","agent":"migrator","task":"Migrate the logrotate module"},
		{"handle":"reviewer","agent":"reviewer","task":"Review the migration"}]}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("open = %d %s", resp.StatusCode, body)
	}
	var it Item
	decodeJSON(t, body, &it)
	return it
}

// channel reads every entry of a bridge from the store.
func channel(t *testing.T, s *Store, id string) []Message {
	t.Helper()
	rec, _, err := s.b.get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	msgs, err := s.b.tail(context.Background(), rec, 0, MaxPageLimit)
	if err != nil {
		t.Fatal(err)
	}
	return msgs
}

// events returns the entries of a channel with the event.
func withEvent(msgs []Message, event string) []Message {
	var out []Message
	for _, m := range msgs {
		if m.Event == event {
			out = append(out, m)
		}
	}
	return out
}

// connectMCP serves the MCP surface of the agent and connects a client
// with the token.
func connectMCP(t *testing.T, a *fw.Agent, token string) *sdk.ClientSession {
	t.Helper()
	h, err := mcp.Handler(a)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	hc := &http.Client{Transport: bearer{token: token}}
	client := sdk.NewClient(&sdk.Implementation{Name: "test-client", Version: "v1"}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	cs, err := client.Connect(ctx, &sdk.StreamableClientTransport{Endpoint: srv.URL + mcp.DefaultPath, HTTPClient: hc, MaxRetries: -1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

type bearer struct{ token string }

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return http.DefaultTransport.RoundTrip(r)
}

// callTool calls a tool with optional _meta.
func callTool(t *testing.T, cs *sdk.ClientSession, name string, args any, meta sdk.Meta) *sdk.CallToolResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := cs.CallTool(ctx, &sdk.CallToolParams{Meta: meta, Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("call %s: %v", name, err)
	}
	return res
}

func textOf(res *sdk.CallToolResult) string {
	var parts []string
	for _, c := range res.Content {
		if tc, ok := c.(*sdk.TextContent); ok {
			parts = append(parts, tc.Text)
		}
	}
	return strings.Join(parts, "\n")
}

func structured(t *testing.T, res *sdk.CallToolResult, v any) {
	t.Helper()
	b, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatalf("structuredContent %s: %v", b, err)
	}
}

// errIs reports whether err wraps target, for a table.
func errIs(err, target error) bool { return errors.Is(err, target) }
