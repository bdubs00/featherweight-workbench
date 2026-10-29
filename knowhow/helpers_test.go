package knowhow

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/surface/native"
	"github.com/bdubs00/featherweight-workbench/fleet"
	"github.com/bdubs00/featherweight-workbench/internal/bucket"
	"github.com/bdubs00/featherweight-workbench/routines"
	"github.com/bdubs00/featherweight-workbench/surface/mcp"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// people takes the bearer token as the subject of a person, fleet as the
// static token of the fleet, and robot as a workload.
type people struct{}

func (people) Authenticate(r *http.Request) (*fw.Principal, error) {
	tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if tok == "" || tok == r.Header.Get("Authorization") {
		return nil, fw.ErrUnauthorized
	}
	switch tok {
	case "fleet":
		return &fw.Principal{Subject: fw.KindToken, Kind: fw.KindToken}, nil
	case "robot":
		return &fw.Principal{Subject: "spiffe://lab/robot", Kind: fw.KindWorkload}, nil
	}
	return &fw.Principal{Subject: tok, Kind: fw.KindUser}, nil
}

func (people) Challenge(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="fw"`)
	w.WriteHeader(http.StatusUnauthorized)
}

// clock is a clock that a test moves.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// fakeArchive is an archive of runs that a test fills. A run that a test
// puts later (putLater) is a change that the other replica wrote: All
// lists it only after Refresh.
type fakeArchive struct {
	mu      sync.Mutex
	runs    map[string]fleet.ArchiveEntry
	events  map[string][]fw.Event
	later   map[string]fleet.ArchiveEntry
	refresh int
}

// Refresh reads the changes of the other replica (knowhow.refresher).
func (f *fakeArchive) Refresh(context.Context) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.refresh++
	for id, e := range f.later {
		f.runs[id] = e
	}
	f.later = nil
}

// putLater puts a run as put does, as a change of the other replica.
func (f *fakeArchive) putLater(events []fw.Event, updated time.Time) {
	f.put(events, updated)
	f.mu.Lock()
	defer f.mu.Unlock()
	id := events[0].Subject
	if f.later == nil {
		f.later = map[string]fleet.ArchiveEntry{}
	}
	f.later[id] = f.runs[id]
	delete(f.runs, id)
}

func newFakeArchive() *fakeArchive {
	return &fakeArchive{runs: map[string]fleet.ArchiveEntry{}, events: map[string][]fw.Event{}}
}

func (f *fakeArchive) All(context.Context) ([]fleet.ArchiveEntry, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]fleet.ArchiveEntry, 0, len(f.runs))
	for _, e := range f.runs {
		c := e
		run := *e.Run
		c.Run = &run
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Updated.Equal(out[j].Updated) {
			return out[i].Updated.Before(out[j].Updated)
		}
		return out[i].Run.ID < out[j].Run.ID
	})
	return out, true
}

func (f *fakeArchive) Events(_ context.Context, id string) ([]fw.Event, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fw.Event(nil), f.events[id]...), nil
}

// put keeps the events of one run, with the time of the newest change of
// its summary.
func (f *fakeArchive) put(events []fw.Event, updated time.Time) *fw.Run {
	f.mu.Lock()
	defer f.mu.Unlock()
	run := &fw.Run{}
	var ended time.Time
	for _, e := range events {
		run.Apply(e, nil)
		if e.Type == fw.EvRunFinished {
			ended = e.Time
		}
	}
	id := events[0].Subject
	f.runs[id] = fleet.ArchiveEntry{Run: run, Member: "team/worker-0", Ended: ended, Updated: updated}
	f.events[id] = events
	return run
}

// recorder builds the events of runs, as the egress stream carries them.
type recorder struct {
	run string
	seq uint64
	at  time.Time
	out []fw.Event
}

func record(run string, at time.Time) *recorder { return &recorder{run: run, at: at} }

func (r *recorder) add(typ string, data any) *recorder {
	raw, err := json.Marshal(data)
	if err != nil {
		panic(err)
	}
	r.seq++
	r.at = r.at.Add(time.Second)
	r.out = append(r.out, fw.Event{SpecVersion: fw.SpecVersion, ID: fmt.Sprintf("%s-%d", r.run, r.seq), Source: "ax:team/worker-0", Type: typ,
		Time: r.at, Subject: r.run, Seq: r.seq, Node: "ax:team/worker-0", Data: raw})
	return r
}

func (r *recorder) queued(agent, input, principal string, meta map[string]string) *recorder {
	return r.add(fw.EvRunQueued, fw.RunQueuedData{Agent: agent, Input: input, Metadata: meta, Origin: "native", Principal: principal})
}

func (r *recorder) started(context string) *recorder {
	return r.add(fw.EvRunStarted, fw.RunStartedData{Model: "anthropic/test", Context: context})
}

func (r *recorder) call(id, tool, source, input, result string, isError bool) *recorder {
	r.add(fw.EvToolRequested, fw.ToolRequestedData{Tool: tool, CallID: id, Input: json.RawMessage(input), Source: source})
	return r.add(fw.EvToolResponded, fw.ToolRespondedData{Tool: tool, CallID: id, Content: []fw.Block{fw.Text(result)}, IsError: isError})
}

func (r *recorder) finished(output string, cost float64) *recorder {
	return r.add(fw.EvRunFinished, fw.RunFinishedData{Outcome: fw.OutcomeCompleted, Output: output, Totals: &fw.RunUsage{CostUSD: cost}})
}

// thumb records the thumb of a person, up or down, with a note, on the
// last event of the run.
func (r *recorder) thumb(reaction, by, note string) *recorder {
	return r.add(fw.EvRunReacted, fw.ReactionData{Seq: r.seq, Reaction: reaction, By: by, Note: note})
}

func (r *recorder) receipt(verification string) *recorder {
	return r.add(fw.EvRunAnnotated, fw.QualityAnnotatedData{Quality: &fw.QualityObservation{ID: "r-" + r.run, Completion: r.seq, Verification: verification}})
}

// front is a process with Know-how in memory on its own bucket, the
// routines and the wiki, a fleet on the mem bus, and the native surface
// on a test server. alice owns triage, bob operates it, and carol only
// views it. dave owns private, which the others cannot see. The profile
// review is the after-work review.
type front struct {
	a    *fw.Agent
	bus  *fw.MemBus
	f    *fleet.Fleet
	s    *Service
	arch *fakeArchive
	clk  *clock
	url  string
}

var day0 = time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)

func newFront(t *testing.T, mut func(*KnowhowConfig)) *front {
	t.Helper()
	t.Setenv("FW_ATESPACE", "")
	for _, name := range []string{EnvReviewAgent, EnvReviewEvery, EnvProposeEnabled, EnvProposeDecider, EnvAgingRestDays, EnvAgingPutAwayDay, routines.EnvEnabled, routines.EnvReadyAfter, routines.EnvReadyAgreement} {
		t.Setenv(name, "")
	}
	cfg := fw.DefaultConfig()
	cfg.Home = t.TempDir()
	cfg.Access.Default = "closed"
	cfg.Time.Zone = "UTC"
	cfg.Deciders = map[string]fw.DeciderConfig{"clef": {BaseURL: "http://127.0.0.1:9", Model: "clef"}}
	cfg.Agents = map[string]fw.AgentProfile{
		"triage":  {Access: &fw.Access{Owners: []string{"alice", "spiffe://lab/robot"}, Operators: []string{"bob"}, Viewers: []string{"carol"}}},
		"private": {Access: &fw.Access{Owners: []string{"dave"}}},
		"review":  {Access: &fw.Access{Owners: []string{"alice"}}},
	}
	bus := fw.NewMemBus()
	a, err := fw.New(fw.WithConfig(cfg), fw.WithoutDefaults(), fw.WithAuthn(people{}),
		fw.WithModel(fw.ModelFunc{GenerateFn: func(context.Context, *fw.Request) (*fw.Response, error) {
			return &fw.Response{Content: []fw.Block{fw.Text("done")}, StopReason: fw.StopEndTurn}, nil
		}}),
		fw.WithLedger(fw.NewMemLedger()), fw.WithBus(bus), fw.WithSurfaces(native.New()),
		fw.WithServices(routines.Start), fw.WithLogger(quiet()))
	if err != nil {
		t.Fatalf("fw.New: %v", err)
	}
	f, err := fleet.Open(context.Background(), a, fleet.Config{Name: "front", Egress: "fw.events", Interval: time.Hour, Expire: fleet.DefaultExpire})
	if err != nil {
		t.Fatalf("fleet.Open: %v", err)
	}
	fleet.Attach(a, f)
	c, err := Load(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if mut != nil {
		mut(&c)
	}
	s := New(a, bucket.NewMem(bucketConfig), c, quiet())
	clk := &clock{t: day0}
	arch := newFakeArchive()
	s.now, s.arch = clk.now, arch
	Attach(a, s)
	h, err := a.Handler()
	if err != nil {
		t.Fatalf("Handler: %v", err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(func() {
		srv.Close()
		Attach(a, nil)
		fleet.Attach(a, nil)
		_ = f.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		a.Close(ctx)
	})
	return &front{a: a, bus: bus, f: f, s: s, arch: arch, clk: clk, url: srv.URL}
}

// send makes a request with the bearer token of who, none for "".
func send(t *testing.T, method, url, who, body string) (*http.Response, string) {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, url, rd)
	if err != nil {
		t.Fatal(err)
	}
	if who != "" {
		req.Header.Set("Authorization", "Bearer "+who)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

func decodeJSON(t *testing.T, body string, v any) {
	t.Helper()
	if err := json.Unmarshal([]byte(body), v); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
}

// mustStatus checks the status of an answer.
func mustStatus(t *testing.T, resp *http.Response, body string, want int) {
	t.Helper()
	if resp.StatusCode != want {
		t.Fatalf("%s %s = %d %s, want %d", resp.Request.Method, resp.Request.URL.Path, resp.StatusCode, body, want)
	}
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

// callAs calls a tool with the run keys of _meta for the profile agent.
func callAs(t *testing.T, cs *sdk.ClientSession, agent, name string, args any) *sdk.CallToolResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	params := &sdk.CallToolParams{Name: name, Arguments: args}
	if agent != "" {
		params.Meta = sdk.Meta{mcp.MetaRun: fw.NewID(), mcp.MetaAgent: agent}
	}
	res, err := cs.CallTool(ctx, params)
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

// duty is a Duty principal of the agent triage.
var duty = Scope{Kind: ScopePrincipal, Principal: "duty:nightly"}

// write writes a note as the agent of a worker and fails the test on an
// error.
func (fr *front) write(t *testing.T, sc Scope, name, text string, sources ...Source) WriteResult {
	t.Helper()
	res, err := fr.s.Write(context.Background(), WriteRequest{Agent: "triage", Scope: sc, Name: name, Text: text, Sources: sources, By: ByAgent, Hold: true})
	if err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return res
}

// note reads a note of triage.
func (fr *front) note(t *testing.T, sc Scope, name string) Note {
	t.Helper()
	n, err := fr.s.Get(context.Background(), "triage", sc, name)
	if err != nil {
		t.Fatalf("get %s: %v", name, err)
	}
	return n
}

// journal returns the entries of triage, newest first.
func (fr *front) journal(t *testing.T) []Entry {
	t.Helper()
	list, err := fr.s.Journal(context.Background(), "triage", MaxLimit, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	return list
}
