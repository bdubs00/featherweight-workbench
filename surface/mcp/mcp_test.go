package mcp

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// script is a fake model that returns canned responses in order.
type script struct {
	mu    sync.Mutex
	resps []*fw.Response
	reqs  []*fw.Request
}

func newScript(resps ...*fw.Response) *script { return &script{resps: resps} }

func (s *script) model() fw.Model {
	return fw.ModelFunc{StreamFn: func(_ context.Context, req *fw.Request, _ func(fw.Delta)) (*fw.Response, error) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.reqs = append(s.reqs, req)
		if len(s.resps) == 0 {
			return nil, errors.New("script: no response left")
		}
		r := s.resps[0]
		s.resps = s.resps[1:]
		return r, nil
	}}
}

func (s *script) requests() []*fw.Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*fw.Request(nil), s.reqs...)
}

func endTurn(text string) *fw.Response {
	return &fw.Response{Content: []fw.Block{fw.Text(text)}, StopReason: fw.StopEndTurn, Usage: fw.Usage{InputTokens: 10, OutputTokens: 5}}
}

func toolUse(id, name, input string) *fw.Response {
	return &fw.Response{
		Content:    []fw.Block{fw.Text("calling"), {Type: fw.BlockToolUse, ID: id, Name: name, Input: json.RawMessage(input)}},
		StopReason: fw.StopToolUse,
		Usage:      fw.Usage{InputTokens: 10, OutputTokens: 5},
	}
}

// gate is a fake model that blocks until the test releases it.
type gate struct {
	started chan struct{}
	release chan struct{}
}

func newGate() *gate { return &gate{started: make(chan struct{}, 8), release: make(chan struct{})} }

func (g *gate) model() fw.Model {
	return fw.ModelFunc{StreamFn: func(ctx context.Context, _ *fw.Request, _ func(fw.Delta)) (*fw.Response, error) {
		g.started <- struct{}{}
		select {
		case <-g.release:
			return endTurn("released"), nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}}
}

func (g *gate) waitStarted(t *testing.T) {
	t.Helper()
	select {
	case <-g.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the model was not called")
	}
}

// deployTool is a destructive tool, so the default policy asks before it runs.
func deployTool(calls *atomic.Int32) fw.Tool {
	return fw.NewTool(fw.ToolSpec{
		Name:        "deploy",
		Description: "Deploy to a target",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"target":{"type":"string"}}}`),
		Destructive: true,
	}, func(_ context.Context, in json.RawMessage) (fw.ToolResult, error) {
		calls.Add(1)
		return fw.TextResult("deployed " + string(in)), nil
	})
}

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func newAgent(t *testing.T, m fw.Model, opts ...fw.Option) *fw.Agent {
	t.Helper()
	t.Setenv("FW_ATESPACE", "")
	// A temporary home keeps the state key file out of the source tree. A
	// later WithConfig replaces it.
	base := []fw.Option{
		fw.WithConfig(testConfig(t)),
		fw.WithoutDefaults(),
		fw.WithAuthn(fw.NoAuthn{}),
		fw.WithModel(m),
		fw.WithLedger(fw.NewMemLedger()),
		fw.WithBus(fw.NewMemBus()),
		fw.WithLogger(quietLogger()),
	}
	a, err := fw.New(append(append(base, fw.WithTitles(fw.TitlesInput)), opts...)...)
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

// testConfig returns the default configuration with a temporary home.
func testConfig(t *testing.T) *fw.Config {
	cfg := fw.DefaultConfig()
	cfg.Home = t.TempDir()
	return cfg
}

func testCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// headerLog records the headers of every request that reaches the handler.
type headerLog struct {
	mu   sync.Mutex
	seen []http.Header
}

func (h *headerLog) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		h.seen = append(h.seen, r.Header.Clone())
		h.mu.Unlock()
		next.ServeHTTP(w, r)
	})
}

func (h *headerLog) any(name, value string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, hd := range h.seen {
		if v := hd.Get(name); v != "" && (value == "" || v == value) {
			return true
		}
	}
	return false
}

func (h *headerLog) count(name, value string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, hd := range h.seen {
		if hd.Get(name) == value {
			n++
		}
	}
	return n
}

// syncBuffer is a log sink that tests read while the server writes.
type syncBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// serveAgent serves the MCP handler of an agent on a test listener.
func serveAgent(t *testing.T, a *fw.Agent, log *headerLog) *httptest.Server {
	t.Helper()
	h, err := Handler(a)
	if err != nil {
		t.Fatalf("Handler: %v", err)
	}
	if log != nil {
		h = log.wrap(h)
	}
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	return ts
}

type clientSetup struct {
	opts     *sdk.ClientOptions
	protocol string
	header   http.Header
}

type headerTransport struct {
	base   http.RoundTripper
	header http.Header
}

func (h headerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	for k, v := range h.header {
		r.Header[k] = v
	}
	return h.base.RoundTrip(r)
}

func connect(t *testing.T, ctx context.Context, url string, c clientSetup) (*sdk.ClientSession, error) {
	t.Helper()
	hc := &http.Client{Transport: headerTransport{base: http.DefaultTransport, header: c.header}}
	client := sdk.NewClient(&sdk.Implementation{Name: "test-client", Version: "v1"}, c.opts)
	var so *sdk.ClientSessionOptions
	if c.protocol != "" {
		so = &sdk.ClientSessionOptions{ProtocolVersion: c.protocol}
	}
	cs, err := client.Connect(ctx, &sdk.StreamableClientTransport{Endpoint: url + DefaultPath, HTTPClient: hc, MaxRetries: -1}, so)
	if err != nil {
		return nil, err
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs, nil
}

func mustConnect(t *testing.T, ctx context.Context, url string, c clientSetup) *sdk.ClientSession {
	t.Helper()
	cs, err := connect(t, ctx, url, c)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	return cs
}

func callTool(t *testing.T, ctx context.Context, cs *sdk.ClientSession, name string, args any) *sdk.CallToolResult {
	t.Helper()
	res, err := cs.CallTool(ctx, &sdk.CallToolParams{Name: name, Arguments: args})
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

func runOf(t *testing.T, res *sdk.CallToolResult) fw.Run {
	t.Helper()
	b, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var r fw.Run
	if err := json.Unmarshal(b, &r); err != nil || r.ID == "" {
		t.Fatalf("structuredContent %s is not a run: %v", b, err)
	}
	return r
}

func ledgerTypes(t *testing.T, a *fw.Agent, id string) []string {
	t.Helper()
	var out []string
	for e, err := range a.Ledger().Read(context.Background(), id, 0) {
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, e.Type)
	}
	return out
}

func decided(t *testing.T, a *fw.Agent, id string) fw.ApprovalDecidedData {
	t.Helper()
	for e, err := range a.Ledger().Read(context.Background(), id, 0) {
		if err != nil {
			t.Fatal(err)
		}
		if e.Type == fw.EvApprovalDecided {
			var d fw.ApprovalDecidedData
			if err := e.Decode(&d); err != nil {
				t.Fatal(err)
			}
			return d
		}
	}
	t.Fatalf("no %s event in %v", fw.EvApprovalDecided, ledgerTypes(t, a, id))
	return fw.ApprovalDecidedData{}
}

func TestListTools(t *testing.T) {
	ctx := testCtx(t)
	a := newAgent(t, newScript().model())
	cs := mustConnect(t, ctx, serveAgent(t, a, nil).URL, clientSetup{})
	res, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]*sdk.Tool{}
	var names []string
	for _, tool := range res.Tools {
		got[tool.Name] = tool
		names = append(names, tool.Name)
	}
	slices.Sort(names)
	if strings.Join(names, ",") != "agent_builder,agent_delete,agent_get,agent_put,agents_list,approve,cancel,capabilities,dashboard,run,run_events,run_file,run_result,run_status,runs_list,send,usage,whoami" {
		t.Fatalf("tools = %v", names)
	}
	destructive := func(name string) bool {
		an := got[name].Annotations
		return an != nil && an.DestructiveHint != nil && *an.DestructiveHint && !an.ReadOnlyHint
	}
	if !destructive("run") || !destructive("send") {
		t.Errorf("run and send are not destructive: %+v %+v", got["run"].Annotations, got["send"].Annotations)
	}
	if an := got["run_status"].Annotations; an == nil || !an.ReadOnlyHint {
		t.Errorf("run_status annotations = %+v", an)
	}
	schema, _ := json.Marshal(got["run"].InputSchema)
	for _, field := range []string{`"input"`, `"title"`, `"agent"`, `"wait"`, `"budget"`, `"requestId"`, `"maxCostUSD"`, `"deadline"`,
		`"instructions"`, `"profile"`, `"system"`, `"skills"`, `"tools"`, `"model"`, `"effort"`, `"timeZone"`, `"policy"`, `"maxToolCalls"`, `"maxCallsPerTurn"`, `"maxRepeats"`} {
		if !strings.Contains(string(schema), field) {
			t.Errorf("run schema lacks %s: %s", field, schema)
		}
	}
	var props struct {
		Properties map[string]struct {
			Description string `json:"description"`
			Properties  map[string]struct {
				Description string `json:"description"`
			} `json:"properties"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(schema, &props); err != nil {
		t.Fatal(err)
	}
	for name, p := range props.Properties {
		if p.Description == "" {
			t.Errorf("run argument %s has no description", name)
		}
		for sub, sp := range p.Properties {
			if sp.Description == "" {
				t.Errorf("run argument %s.%s has no description", name, sub)
			}
		}
	}
	r := cs.InitializeResult()
	if r == nil || !strings.Contains(r.Instructions, "the run call ends but the run continues") {
		t.Errorf("instructions = %+v", r)
	}
	if r != nil && (!strings.Contains(r.Instructions, "instructions") || !strings.Contains(r.Instructions, "profile")) {
		t.Errorf("instructions do not mention instructions and profile:\n%s", r.Instructions)
	}
}

func TestRunWithInstructionsAndProfile(t *testing.T) {
	ctx := testCtx(t)
	m := newScript(endTurn("shaped"))
	a := newAgent(t, m.model(), fw.WithProfile(fw.AgentProfile{Name: "writer", System: "You write.", Instructions: []string{"House rule."}}))
	cs := mustConnect(t, ctx, serveAgent(t, a, nil).URL, clientSetup{})
	res := callTool(t, ctx, cs, "run", map[string]any{
		"input":        "go",
		"agent":        "writer",
		"instructions": "Only this run.",
		"profile": map[string]any{
			"system":       "You edit.",
			"instructions": []string{"Be brief."},
			"tools":        []string{"files"},
			"effort":       "low",
			"timeZone":     "Asia/Tokyo",
			"policy":       map[string]any{"default": "ask", "deny": []string{"bash"}},
			"budget":       map[string]any{"maxTurns": 3, "maxToolCalls": 7, "maxCallsPerTurn": 2, "maxRepeats": 1, "deadline": "1m"},
		},
		"budget": map[string]any{"maxTokens": 500, "maxToolCalls": 9},
	})
	if res.IsError || textOf(res) != "shaped" {
		t.Fatalf("result = %q, isError %v", textOf(res), res.IsError)
	}
	r := runOf(t, res)
	reqs := m.requests()
	if len(reqs) != 1 {
		t.Fatalf("model calls = %d", len(reqs))
	}
	sys := reqs[0].System
	for _, want := range []string{"You edit.", "House rule.", "Be brief.", "Only this run."} {
		if !strings.Contains(sys, want) {
			t.Errorf("system prompt lacks %q:\n%s", want, sys)
		}
	}
	if strings.Contains(sys, "You write.") || reqs[0].Effort != "low" {
		t.Errorf("the inline profile was not applied: effort %q\n%s", reqs[0].Effort, sys)
	}
	var q fw.RunQueuedData
	for e, err := range a.Ledger().Read(ctx, r.ID, 0) {
		if err != nil {
			t.Fatal(err)
		}
		if e.Type == fw.EvRunQueued {
			_ = e.Decode(&q)
		}
	}
	if q.Instructions != "Only this run." || q.Profile == nil || q.Profile.System != "You edit." || q.Profile.TimeZone != "Asia/Tokyo" || q.Profile.Policy == nil ||
		q.Profile.Policy.Default != "ask" || q.Profile.Budget == nil || q.Profile.Budget.MaxToolCalls != 7 ||
		q.Profile.Budget.MaxCallsPerTurn != 2 || q.Profile.Budget.MaxRepeats != 1 || q.Profile.Budget.Deadline != time.Minute ||
		q.Budget == nil || q.Budget.MaxTokens != 500 || q.Budget.MaxToolCalls != 9 {
		t.Fatalf("queued = %+v profile %+v budget %+v", q, q.Profile, q.Budget)
	}

	for _, tc := range []struct {
		args map[string]any
		want string
	}{
		{map[string]any{"input": "x", "profile": map[string]any{"effort": "extreme"}}, "effort"},
		{map[string]any{"input": "x", "profile": map[string]any{"timeZone": "Mars/Olympus"}}, "profile.timeZone"},
		{map[string]any{"input": "x", "profile": map[string]any{"policy": map[string]any{"default": "maybe"}}}, "allow ask deny"},
		{map[string]any{"input": "x", "profile": map[string]any{"budget": map[string]any{"maxRepeats": -1}}}, "maxRepeats"},
		{map[string]any{"input": "x", "profile": map[string]any{"budget": map[string]any{"deadline": "later"}}}, "profile.budget.deadline"},
		{map[string]any{"input": "x", "profile": map[string]any{"model": "nowhere"}}, "model"},
		{map[string]any{"input": "x", "budget": map[string]any{"maxToolCalls": -1}}, "budget"},
	} {
		res := callTool(t, ctx, cs, "run", tc.args)
		if !res.IsError || !strings.Contains(textOf(res), tc.want) {
			t.Errorf("%v: result %q, isError %v", tc.args, textOf(res), res.IsError)
		}
	}
	if list, _ := a.List(ctx); len(list) != 1 {
		t.Errorf("runs = %d, want 1", len(list))
	}
}

func TestRunWaitCompletes(t *testing.T) {
	ctx := testCtx(t)
	a := newAgent(t, newScript(endTurn("hello")).model())
	var hl headerLog
	var mu sync.Mutex
	var progress []string
	cs := mustConnect(t, ctx, serveAgent(t, a, &hl).URL, clientSetup{opts: &sdk.ClientOptions{
		ProgressNotificationHandler: func(_ context.Context, req *sdk.ProgressNotificationClientRequest) {
			mu.Lock()
			progress = append(progress, req.Params.Message)
			mu.Unlock()
		},
	}})
	params := &sdk.CallToolParams{Name: "run", Arguments: map[string]any{"input": "say hi", "budget": map[string]any{"maxTurns": 3, "deadline": "5m"}}}
	params.SetProgressToken("tok-1")
	res, err := cs.CallTool(ctx, params)
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError || textOf(res) != "hello" {
		t.Fatalf("result = %q, isError %v", textOf(res), res.IsError)
	}
	r := runOf(t, res)
	if r.State != fw.StateCompleted || r.Output != "hello" || r.Origin != fw.OriginMCP || r.Principal != "anonymous" || r.Usage.Turns != 1 {
		t.Fatalf("run = %+v", r)
	}
	if !hl.any("Mcp-Protocol-Version", modernProtocol) || hl.any("Mcp-Session-Id", "") {
		t.Errorf("a modern client was not served without a session")
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		n := len(progress)
		mu.Unlock()
		if n > 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(progress) != 1 || !strings.Contains(progress[0], "turn 1 finished") || !strings.Contains(progress[0], "hello") {
		t.Errorf("progress = %q", progress)
	}
}

func TestRunFailedIsError(t *testing.T) {
	ctx := testCtx(t)
	a := newAgent(t, newScript().model()) // the model fails at once
	cs := mustConnect(t, ctx, serveAgent(t, a, nil).URL, clientSetup{})
	res := callTool(t, ctx, cs, "run", map[string]any{"input": "x"})
	if !res.IsError || runOf(t, res).State != fw.StateFailed || !strings.Contains(textOf(res), "failed") {
		t.Fatalf("result = %q, isError %v", textOf(res), res.IsError)
	}
	res = callTool(t, ctx, cs, "run", map[string]any{"input": "x", "agent": "nobody"})
	if !res.IsError || !strings.Contains(textOf(res), "not found") {
		t.Fatalf("unknown agent result = %q", textOf(res))
	}
}

func TestRunApprovalByElicitation(t *testing.T) {
	ctx := testCtx(t)
	var calls atomic.Int32
	// Two destructive calls in a row: one run call, two input_required
	// rounds, and two retries that carry the requestState.
	s := newScript(toolUse("c1", "deploy", `{"target":"prod"}`), toolUse("c2", "deploy", `{"target":"eu"}`), endTurn("done"))
	a := newAgent(t, s.model(), fw.WithTools(deployTool(&calls)))
	var hl headerLog
	var asked atomic.Value
	var asks atomic.Int32
	cs := mustConnect(t, ctx, serveAgent(t, a, &hl).URL, clientSetup{opts: &sdk.ClientOptions{
		ElicitationHandler: func(_ context.Context, req *sdk.ElicitRequest) (*sdk.ElicitResult, error) {
			if asks.Add(1) == 1 {
				asked.Store(req.Params.Message)
			}
			return &sdk.ElicitResult{Action: "accept", Content: map[string]any{"decision": "allow", "note": "go"}}, nil
		},
	}})
	res := callTool(t, ctx, cs, "run", map[string]any{"input": "ship it"})
	if res.IsError || textOf(res) != "done" {
		t.Fatalf("result = %q, isError %v", textOf(res), res.IsError)
	}
	r := runOf(t, res)
	if r.State != fw.StateCompleted || calls.Load() != 2 || asks.Load() != 2 {
		t.Fatalf("run = %+v, calls %d, asks %d", r, calls.Load(), asks.Load())
	}
	if n := hl.count("Mcp-Method", "tools/call"); n != 3 {
		t.Errorf("tools/call requests = %d, want 3 (one call and two retries)", n)
	}
	msg, _ := asked.Load().(string)
	if !strings.Contains(msg, "deploy") || !strings.Contains(msg, `"target":"prod"`) {
		t.Errorf("elicitation message = %q", msg)
	}
	if d := decided(t, a, r.ID); d.Decision != "allow" || d.Note != "go" || d.Decider != "anonymous" {
		t.Errorf("decision = %+v", d)
	}
	if hl.any("Mcp-Session-Id", "") {
		t.Errorf("the input_required flow opened a session")
	}
	if n := len(a.Profiles()); n != 1 {
		t.Fatalf("profiles = %d", n)
	}
	runs, _ := a.List(ctx)
	if len(runs) != 1 {
		t.Errorf("the retry started a second run: %d runs", len(runs))
	}
}

func TestRunApprovalByLegacyElicitation(t *testing.T) {
	ctx := testCtx(t)
	var calls atomic.Int32
	s := newScript(toolUse("c1", "deploy", `{"target":"prod"}`), endTurn("stopped"))
	a := newAgent(t, s.model(), fw.WithTools(deployTool(&calls)))
	var hl headerLog
	cs := mustConnect(t, ctx, serveAgent(t, a, &hl).URL, clientSetup{
		protocol: "2025-11-25",
		opts: &sdk.ClientOptions{
			ElicitationHandler: func(context.Context, *sdk.ElicitRequest) (*sdk.ElicitResult, error) {
				return &sdk.ElicitResult{Action: "decline"}, nil
			},
		},
	})
	if r := cs.InitializeResult(); r == nil || r.ProtocolVersion != "2025-11-25" {
		t.Fatalf("negotiated %+v", r)
	}
	res := callTool(t, ctx, cs, "run", map[string]any{"input": "ship it"})
	if res.IsError || textOf(res) != "stopped" {
		t.Fatalf("result = %q, isError %v", textOf(res), res.IsError)
	}
	r := runOf(t, res)
	if calls.Load() != 0 {
		t.Fatalf("a declined approval ran the tool")
	}
	if d := decided(t, a, r.ID); d.Decision != "deny" || !strings.Contains(d.Note, "declined") {
		t.Errorf("decision = %+v", d)
	}
	if !hl.any("Mcp-Session-Id", "") {
		t.Errorf("a legacy client did not get a session")
	}
}

func TestRunApprovalWithoutElicitation(t *testing.T) {
	ctx := testCtx(t)
	var calls atomic.Int32
	a := newAgent(t, newScript(toolUse("c1", "deploy", `{"target":"prod"}`), endTurn("done")).model(), fw.WithTools(deployTool(&calls)))
	cs := mustConnect(t, ctx, serveAgent(t, a, nil).URL, clientSetup{})
	res := callTool(t, ctx, cs, "run", map[string]any{"input": "ship it"})
	r := runOf(t, res)
	if res.IsError || r.State != fw.StateInputRequired || r.Pending == nil {
		t.Fatalf("run = %+v, isError %v", r, res.IsError)
	}
	text := textOf(res)
	want := "POST /v1/runs/" + r.ID + "/approvals/" + r.Pending.ID
	if !strings.Contains(text, want) || !strings.Contains(text, "deploy") || !strings.Contains(text, "approve tool") {
		t.Fatalf("text = %q, want it to name %s", text, want)
	}
	res = callTool(t, ctx, cs, "approve", map[string]any{"runId": r.ID, "approvalId": r.Pending.ID, "decision": "allow"})
	if res.IsError {
		t.Fatalf("approve = %q", textOf(res))
	}
	done, err := a.Wait(ctx, r.ID)
	if err != nil || done.State != fw.StateCompleted || calls.Load() != 1 {
		t.Fatalf("run = %+v, %v, calls %d", done, err, calls.Load())
	}
	res = callTool(t, ctx, cs, "approve", map[string]any{"runId": r.ID, "approvalId": r.Pending.ID, "decision": "allow"})
	if !res.IsError {
		t.Errorf("a second approve succeeded")
	}
}

func TestRunStatusCancelAndSend(t *testing.T) {
	ctx := testCtx(t)
	g := newGate()
	a := newAgent(t, g.model())
	cs := mustConnect(t, ctx, serveAgent(t, a, nil).URL, clientSetup{})

	res := callTool(t, ctx, cs, "run", map[string]any{"input": "work", "wait": false, "requestId": "req-1"})
	r := runOf(t, res)
	if r.State.Terminal() || r.State == fw.StateInputRequired {
		t.Fatalf("run = %+v", r)
	}
	again := runOf(t, callTool(t, ctx, cs, "run", map[string]any{"input": "work", "wait": false, "requestId": "req-1"}))
	if again.ID != r.ID {
		t.Fatalf("the same requestId started a second run: %s, %s", r.ID, again.ID)
	}
	g.waitStarted(t)

	res = callTool(t, ctx, cs, "run_status", map[string]any{"runId": r.ID})
	if st := runOf(t, res); st.ID != r.ID || st.State != fw.StateRunning || !strings.Contains(textOf(res), "running") {
		t.Fatalf("run_status = %+v, %q", st, textOf(res))
	}
	res = callTool(t, ctx, cs, "cancel", map[string]any{"runId": r.ID})
	if st := runOf(t, res); res.IsError || st.State != fw.StateCanceled || !strings.Contains(textOf(res), "canceled") {
		t.Fatalf("cancel = %+v, %q", st, textOf(res))
	}

	res = callTool(t, ctx, cs, "send", map[string]any{"runId": r.ID, "content": "one more thing"})
	if res.IsError {
		t.Fatalf("send = %q", textOf(res))
	}
	g.waitStarted(t)
	close(g.release)
	done, err := a.Wait(ctx, r.ID)
	if err != nil || done.State != fw.StateCompleted || done.Output != "released" {
		t.Fatalf("run after send = %+v, %v", done, err)
	}

	res = callTool(t, ctx, cs, "run_status", map[string]any{"runId": "nope"})
	if !res.IsError {
		t.Errorf("run_status of an unknown run succeeded")
	}
}

func TestDisconnectDoesNotCancelRun(t *testing.T) {
	ctx := testCtx(t)
	g := newGate()
	var logs syncBuffer
	a := newAgent(t, g.model(), fw.WithLogger(slog.New(slog.NewTextHandler(&logs, nil))))
	cs := mustConnect(t, ctx, serveAgent(t, a, nil).URL, clientSetup{})
	cctx, cancel := context.WithCancel(ctx)
	errc := make(chan error, 1)
	go func() {
		_, err := cs.CallTool(cctx, &sdk.CallToolParams{Name: "run", Arguments: map[string]any{"input": "long job"}})
		errc <- err
	}()
	g.waitStarted(t)
	cancel()
	select {
	case err := <-errc:
		if err == nil {
			t.Fatal("the call returned without an error after the client left")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the call did not end after the client left")
	}
	runs, _ := a.List(ctx)
	if len(runs) != 1 {
		t.Fatalf("runs = %d", len(runs))
	}
	// The handler sees the disconnect and returns while the run goes on.
	for deadline := time.Now().Add(5 * time.Second); !strings.Contains(logs.String(), "the run continues"); {
		if time.Now().After(deadline) {
			t.Fatalf("the handler did not see the disconnect; logs:\n%s", logs.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if st, _ := a.Get(ctx, runs[0].ID); st.State != fw.StateRunning {
		t.Fatalf("run after the disconnect = %+v", st)
	}
	close(g.release)
	done, err := a.Wait(ctx, runs[0].ID)
	if err != nil || done.State != fw.StateCompleted {
		t.Fatalf("run = %+v, %v", done, err)
	}
}

func TestResources(t *testing.T) {
	ctx := testCtx(t)
	cfg := testConfig(t)
	dir := filepath.Join(cfg.Home, "skills", "review")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	skill := "---\nname: review\ndescription: Review a pull request\n---\n# Review\n"
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(skill), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cfg.Home, "skills", "empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A second directory from tools.skills: the kernel and the surface agree on it.
	extra := t.TempDir()
	if err := os.MkdirAll(filepath.Join(extra, "deploy"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(extra, "deploy", "SKILL.md"), []byte("# Deploy\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg.Tools.Skills = []string{filepath.Join(cfg.Home, "skills"), extra}
	a := newAgent(t, newScript(endTurn("hello")).model(), fw.WithConfig(cfg))
	r, err := a.Run(ctx, "say hi")
	if err != nil {
		t.Fatal(err)
	}
	cs := mustConnect(t, ctx, serveAgent(t, a, nil).URL, clientSetup{})

	res, err := cs.ReadResource(ctx, &sdk.ReadResourceParams{URI: "fw://runs/" + r.ID})
	if err != nil {
		t.Fatal(err)
	}
	var got fw.Run
	if err := json.Unmarshal([]byte(res.Contents[0].Text), &got); err != nil || got.ID != r.ID || got.Output != "hello" {
		t.Fatalf("run resource = %s, %v", res.Contents[0].Text, err)
	}
	if res.Contents[0].MIMEType != "application/json" {
		t.Errorf("mime = %q", res.Contents[0].MIMEType)
	}

	res, err = cs.ReadResource(ctx, &sdk.ReadResourceParams{URI: "fw://runs/" + r.ID + "/ledger"})
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(res.Contents[0].Text), "\n")
	var first fw.Event
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil || first.Type != fw.EvRunQueued {
		t.Fatalf("first ledger line = %s, %v", lines[0], err)
	}
	if want := len(ledgerTypes(t, a, r.ID)); len(lines) != want {
		t.Errorf("ledger lines = %d, want %d", len(lines), want)
	}

	if _, err := cs.ReadResource(ctx, &sdk.ReadResourceParams{URI: "fw://runs/UNKNOWN"}); err == nil {
		t.Errorf("an unknown run was readable")
	}

	list, err := cs.ListResources(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var skills []*sdk.Resource
	for _, r := range list.Resources {
		if strings.HasPrefix(r.URI, skillPrefix) {
			skills = append(skills, r)
		}
	}
	if len(list.Resources) != 3 || len(skills) != 2 || skills[0].URI != "fw://skills/deploy" || skills[0].Description != "The skill deploy." ||
		skills[1].URI != "fw://skills/review" || skills[1].Description != "Review a pull request" {
		t.Fatalf("resources = %+v", list.Resources)
	}
	res, err = cs.ReadResource(ctx, &sdk.ReadResourceParams{URI: "fw://skills/review"})
	if err != nil || res.Contents[0].Text != skill || res.Contents[0].MIMEType != "text/markdown" {
		t.Fatalf("skill = %+v, %v", res, err)
	}
	res, err = cs.ReadResource(ctx, &sdk.ReadResourceParams{URI: "fw://skills/deploy"})
	if err != nil || res.Contents[0].Text != "# Deploy\n" {
		t.Fatalf("skill from tools.skills = %+v, %v", res, err)
	}
	if got, ok := a.SkillNamed("deploy"); !ok || got.Dir != filepath.Join(extra, "deploy") {
		t.Fatalf("the kernel does not see the same skill: %+v, %v", got, ok)
	}
	for _, uri := range []string{"fw://skills/empty", "fw://skills/..", "fw://skills/%2E%2E"} {
		if _, err := cs.ReadResource(ctx, &sdk.ReadResourceParams{URI: uri}); err == nil {
			t.Errorf("%s was readable", uri)
		}
	}
	tmpl, err := cs.ListResourceTemplates(ctx, nil)
	if err != nil || len(tmpl.ResourceTemplates) != 4 {
		t.Fatalf("templates = %+v, %v", tmpl, err)
	}
	var files *sdk.ResourceTemplate
	for _, rt := range tmpl.ResourceTemplates {
		if rt.Name == "run-file" {
			files = rt
		}
	}
	if files == nil || files.URITemplate != "fw://runs/{id}/files/{+path}" || !strings.Contains(files.Description, "this process only") || !strings.Contains(files.Description, "4 MiB") {
		t.Fatalf("file template = %+v", files)
	}
}

func TestPrompts(t *testing.T) {
	ctx := testCtx(t)
	a := newAgent(t, newScript().model(),
		fw.WithProfile(fw.AgentProfile{Name: "reviewer", System: "You review code.\nBe brief."}),
		fw.WithProfile(fw.AgentProfile{Name: "writer"}),
	)
	cs := mustConnect(t, ctx, serveAgent(t, a, nil).URL, clientSetup{})
	list, err := cs.ListPrompts(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, p := range list.Prompts {
		names = append(names, p.Name)
	}
	slices.Sort(names)
	if strings.Join(names, ",") != "reviewer,writer" {
		t.Fatalf("prompts = %v", names)
	}
	res, err := cs.GetPrompt(ctx, &sdk.GetPromptParams{Name: "reviewer", Arguments: map[string]string{"input": "Review PR 42"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Messages) != 2 || res.Description != "You review code." {
		t.Fatalf("prompt = %+v", res)
	}
	for i, want := range []string{"You review code.\nBe brief.", "Review PR 42"} {
		m := res.Messages[i]
		tc, ok := m.Content.(*sdk.TextContent)
		if m.Role != "user" || !ok || tc.Text != want {
			t.Errorf("message %d = %+v", i, m)
		}
	}
	if r := cs.InitializeResult(); r == nil || !strings.Contains(r.Instructions, "Use agents_list") || strings.Contains(r.Instructions, "You review code.") {
		t.Errorf("instructions must use caller-scoped profile discovery: %+v", r)
	}
}

// post sends a raw JSON-RPC request to the endpoint.
func post(t *testing.T, url string, header map[string]string) *http.Response {
	t.Helper()
	body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"raw","version":"1"}}}`
	req, err := http.NewRequest(http.MethodPost, url+DefaultPath, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp
}

func TestOriginCheck(t *testing.T) {
	cfg := testConfig(t)
	cfg.MCP.Origins = []string{"https://ide.example.com"}
	a := newAgent(t, newScript().model(), fw.WithConfig(cfg))
	ts := serveAgent(t, a, nil)
	if resp := post(t, ts.URL, map[string]string{"Origin": "https://evil.example.com"}); resp.StatusCode != http.StatusForbidden {
		t.Errorf("wrong origin: status %d", resp.StatusCode)
	}
	if resp := post(t, ts.URL, map[string]string{"Origin": "https://IDE.example.com/"}); resp.StatusCode != http.StatusOK {
		t.Errorf("allowed origin: status %d", resp.StatusCode)
	}
	if resp := post(t, ts.URL, nil); resp.StatusCode != http.StatusOK {
		t.Errorf("no origin: status %d", resp.StatusCode)
	}
}

func TestTokenAuth(t *testing.T) {
	ctx := testCtx(t)
	a := newAgent(t, newScript(endTurn("hello")).model(), fw.WithAuthn(fw.TokenAuthn{Token: "s3cret"}))
	ts := serveAgent(t, a, nil)
	resp := post(t, ts.URL, nil)
	if resp.StatusCode != http.StatusUnauthorized || !strings.HasPrefix(resp.Header.Get("WWW-Authenticate"), "Bearer") {
		t.Fatalf("no token: status %d, challenge %q", resp.StatusCode, resp.Header.Get("WWW-Authenticate"))
	}
	if resp := post(t, ts.URL, map[string]string{"Authorization": "Bearer wrong"}); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong token: status %d", resp.StatusCode)
	}
	if _, err := connect(t, ctx, ts.URL, clientSetup{}); err == nil {
		t.Fatal("a client without a token connected")
	}
	cs := mustConnect(t, ctx, ts.URL, clientSetup{header: http.Header{"Authorization": {"Bearer s3cret"}}})
	res := callTool(t, ctx, cs, "run", map[string]any{"input": "say hi"})
	if r := runOf(t, res); r.State != fw.StateCompleted || r.Principal != "token" {
		t.Fatalf("run = %+v", r)
	}
}

func TestGateway(t *testing.T) {
	ctx := testCtx(t)
	cfg := testConfig(t)
	cfg.MCP.Expose = []string{"echo", "danger*", "run"}
	var dangerCalls atomic.Int32
	echo := fw.NewTool(fw.ToolSpec{
		Name:        "echo",
		Description: "Echo the text",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"}},"required":["text"]}`),
		ReadOnly:    true,
	}, func(ctx context.Context, in json.RawMessage) (fw.ToolResult, error) {
		if fw.AgentFrom(ctx) == nil {
			return fw.ErrorResult("no agent in context"), nil
		}
		var v struct{ Text string }
		_ = json.Unmarshal(in, &v)
		return fw.ToolResult{Content: []fw.Block{fw.Text("echo: " + v.Text)}, Structured: json.RawMessage(`{"text":"` + v.Text + `"}`)}, nil
	})
	danger := fw.NewTool(fw.ToolSpec{Name: "danger.wipe", Destructive: true}, func(context.Context, json.RawMessage) (fw.ToolResult, error) {
		dangerCalls.Add(1)
		return fw.TextResult("wiped"), nil
	})
	hidden := fw.NewTool(fw.ToolSpec{Name: "hidden", ReadOnly: true}, func(context.Context, json.RawMessage) (fw.ToolResult, error) {
		return fw.TextResult("hidden"), nil
	})
	shadow := fw.NewTool(fw.ToolSpec{Name: "run", ReadOnly: true}, func(context.Context, json.RawMessage) (fw.ToolResult, error) {
		return fw.TextResult("shadow"), nil
	})
	a := newAgent(t, newScript().model(), fw.WithConfig(cfg), fw.WithTools(echo, danger, hidden, shadow))
	cs := mustConnect(t, ctx, serveAgent(t, a, nil).URL, clientSetup{})

	list, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	tools := map[string]*sdk.Tool{}
	for _, tool := range list.Tools {
		tools[tool.Name] = tool
	}
	if tools["echo"] == nil || tools["danger.wipe"] == nil || tools["hidden"] != nil || len(tools) != len(workTools)+2 {
		t.Fatalf("tools = %v", tools)
	}
	if an := tools["echo"].Annotations; an == nil || !an.ReadOnlyHint {
		t.Errorf("echo annotations = %+v", an)
	}
	if an := tools["danger.wipe"].Annotations; an == nil || an.DestructiveHint == nil || !*an.DestructiveHint {
		t.Errorf("danger annotations = %+v", an)
	}
	if tools["run"].Description == "" || strings.Contains(tools["run"].Description, "shadow") {
		t.Errorf("an exposed tool replaced run")
	}

	res := callTool(t, ctx, cs, "echo", map[string]any{"text": "hi"})
	if res.IsError || textOf(res) != "echo: hi" {
		t.Fatalf("echo = %q, isError %v", textOf(res), res.IsError)
	}
	if b, _ := json.Marshal(res.StructuredContent); string(b) != `{"text":"hi"}` {
		t.Errorf("echo structured = %s", b)
	}
	res = callTool(t, ctx, cs, "danger.wipe", map[string]any{})
	if !res.IsError || !strings.Contains(textOf(res), "needs an approval") || dangerCalls.Load() != 0 {
		t.Fatalf("danger = %q, isError %v, calls %d", textOf(res), res.IsError, dangerCalls.Load())
	}
	if _, err := cs.CallTool(ctx, &sdk.CallToolParams{Name: "hidden", Arguments: map[string]any{}}); err == nil {
		t.Errorf("a tool outside mcp.expose was callable")
	}
}

func TestGatewayPolicyDeny(t *testing.T) {
	ctx := testCtx(t)
	cfg := testConfig(t)
	cfg.MCP.Expose = []string{"*"}
	echo := fw.NewTool(fw.ToolSpec{Name: "echo", ReadOnly: true}, func(context.Context, json.RawMessage) (fw.ToolResult, error) {
		return fw.TextResult("echo"), nil
	})
	deny := fw.PolicyFunc(func(context.Context, fw.ToolCall) fw.Decision {
		return fw.Decision{Verdict: fw.VerdictDeny, Reason: "rule: deny everything"}
	})
	a := newAgent(t, newScript().model(), fw.WithConfig(cfg), fw.WithTools(echo), fw.WithPolicy(deny))
	cs := mustConnect(t, ctx, serveAgent(t, a, nil).URL, clientSetup{})
	res := callTool(t, ctx, cs, "echo", map[string]any{})
	if !res.IsError || !strings.Contains(textOf(res), "denies") {
		t.Fatalf("echo = %q, isError %v", textOf(res), res.IsError)
	}
}

func TestStdio(t *testing.T) {
	ctx := testCtx(t)
	var calls atomic.Int32
	a := newAgent(t, newScript(toolUse("c1", "deploy", `{"target":"prod"}`), endTurn("done")).model(), fw.WithTools(deployTool(&calls)))
	st, ct := sdk.NewInMemoryTransports()
	sctx, stop := context.WithCancel(fw.WithPrincipal(ctx, &fw.Principal{Subject: "local-user"}))
	errc := make(chan error, 1)
	go func() { errc <- serve(sctx, a, st) }()
	client := sdk.NewClient(&sdk.Implementation{Name: "stdio-client", Version: "v1"}, &sdk.ClientOptions{
		ElicitationHandler: func(context.Context, *sdk.ElicitRequest) (*sdk.ElicitResult, error) {
			return &sdk.ElicitResult{Action: "accept", Content: map[string]any{"decision": "allow"}}, nil
		},
	})
	cs, err := client.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	res := callTool(t, ctx, cs, "run", map[string]any{"input": "ship it"})
	if res.IsError || textOf(res) != "done" || calls.Load() != 1 {
		t.Fatalf("result = %q, isError %v, calls %d", textOf(res), res.IsError, calls.Load())
	}
	if r := runOf(t, res); r.Principal != "local-user" || decided(t, a, r.ID).Decider != "local-user" {
		t.Errorf("run = %+v", r)
	}
	_ = cs.Close()
	stop()
	select {
	case err := <-errc:
		if err != nil {
			t.Errorf("serve: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Error("serve did not return")
	}
}

func TestRequestState(t *testing.T) {
	a := newAgent(t, newScript().model())
	s := &server{a: a, key: []byte("k"), stateTTL: time.Hour}
	state := s.sealState("run1", "ap1", "alice")
	got, err := s.openState(state, "alice")
	if err != nil || got.Run != "run1" || got.Approval != "ap1" {
		t.Fatalf("open = %+v, %v", got, err)
	}
	if _, err := s.openState(state, "bob"); err == nil {
		t.Error("another principal opened the state")
	}
	body, sig, _ := strings.Cut(state, ".")
	if _, err := s.openState(body+"x."+sig, "alice"); err == nil {
		t.Error("a changed state opened")
	}
	other := &server{a: a, key: []byte("other"), stateTTL: time.Hour}
	if _, err := other.openState(state, "alice"); err == nil {
		t.Error("a state opened with another key")
	}
	old := &server{a: a, key: []byte("k"), stateTTL: -time.Minute}
	if _, err := s.openState(old.sealState("run1", "ap1", "alice"), "alice"); err == nil {
		t.Error("an expired state opened")
	}
}

func TestSurfaceMountsAtPath(t *testing.T) {
	cfg := testConfig(t)
	cfg.MCP.Path = "/custom-mcp"
	a := newAgent(t, newScript().model(), fw.WithConfig(cfg), fw.WithSurfaces(New()))
	h, err := a.Handler()
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/custom-mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		t.Fatalf("no MCP endpoint at %s", cfg.MCP.Path)
	}
	if !slices.Contains(fw.RegisteredSurfaces(), "mcp") {
		t.Errorf("surfaces = %v", fw.RegisteredSurfaces())
	}
}

func TestSendFiles(t *testing.T) {
	ctx := testCtx(t)
	root := t.TempDir()
	cfg := testConfig(t)
	cfg.Tools.Files.Root = root
	a := newAgent(t, newScript(endTurn("first"), endTurn("second")).model(), fw.WithConfig(cfg))
	cs := mustConnect(t, ctx, serveAgent(t, a, nil).URL, clientSetup{})
	r := runOf(t, callTool(t, ctx, cs, "run", map[string]any{"input": "hi"}))

	// A file of 5 MB passes the 4 MiB default of the SDK.
	big := bytes.Repeat([]byte("0123456789"), 500_000)
	res := callTool(t, ctx, cs, "send", map[string]any{"runId": r.ID, "files": []map[string]any{{"name": "big.txt", "data": base64.StdEncoding.EncodeToString(big)}}})
	if res.IsError {
		t.Fatalf("send = %q", textOf(res))
	}
	if done, err := a.Wait(ctx, r.ID); err != nil || done.Output != "second" {
		t.Fatalf("run = %+v, %v", done, err)
	}
	if info, err := os.Stat(filepath.Join(root, "attachments", "big.txt")); err != nil || info.Size() != int64(len(big)) {
		t.Fatalf("file = %v, %v", info, err)
	}
	if res := callTool(t, ctx, cs, "send", map[string]any{"runId": r.ID}); !res.IsError || !strings.Contains(textOf(res), "content or files") {
		t.Errorf("an empty send = %q", textOf(res))
	}
}

func TestRunNamesExtraTools(t *testing.T) {
	ctx := testCtx(t)
	a := newAgent(t, newScript(endTurn("ok")).model(), fw.WithProfile(fw.AgentProfile{Name: "narrow", Tools: []string{"none-such"}}),
		fw.WithTools(fw.NewTool(fw.ToolSpec{Name: "probe"}, func(context.Context, json.RawMessage) (fw.ToolResult, error) { return fw.TextResult("ok"), nil })))
	cs := mustConnect(t, ctx, serveAgent(t, a, nil).URL, clientSetup{})
	res := callTool(t, ctx, cs, toolRun, map[string]any{"input": "hi", "agent": "narrow", "profile": map[string]any{"tools": []string{"probe"}}})
	if res.IsError || !strings.Contains(textOf(res), "For this run only, the inline profile gives narrow tools that its profile does not list: probe.") {
		t.Fatalf("run = %s", textOf(res))
	}
}
