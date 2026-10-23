package routines

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/surface/native"
	"github.com/bdubs00/featherweight-workbench/fleet"
	"github.com/bdubs00/featherweight-workbench/internal/bucket"
	"github.com/bdubs00/featherweight-workbench/pages"
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

// front is a process with the routines and the wiki in memory, a fleet
// that reads the egress stream of the mem bus, and the native surface on
// a test server. alice and the workload robot own the agent triage, and bob
// operates it. dave owns private, which the others cannot see.
type front struct {
	a   *fw.Agent
	bus *fw.MemBus
	f   *fleet.Fleet
	s   *Store
	url string
}

const alpha = "ax:team/alpha"

func newFront(t *testing.T, mut func(*fw.Config)) *front {
	t.Helper()
	t.Setenv("FW_ATESPACE", "")
	t.Setenv(EnvReadyAfter, "")
	t.Setenv(EnvReadyAgreement, "")
	cfg := fw.DefaultConfig()
	cfg.Home = t.TempDir()
	cfg.Access.Default = "closed"
	cfg.Time.Zone = "UTC"
	cfg.Agents = map[string]fw.AgentProfile{
		"triage":  {Access: &fw.Access{Owners: []string{"alice", "spiffe://lab/robot"}, Operators: []string{"bob"}}},
		"private": {Access: &fw.Access{Owners: []string{"dave"}}},
	}
	if mut != nil {
		mut(cfg)
	}
	bus := fw.NewMemBus()
	a, err := fw.New(fw.WithConfig(cfg), fw.WithoutDefaults(), fw.WithAuthn(people{}),
		fw.WithModel(fw.ModelFunc{GenerateFn: func(context.Context, *fw.Request) (*fw.Response, error) {
			return &fw.Response{Content: []fw.Block{fw.Text("done")}, StopReason: fw.StopEndTurn}, nil
		}}),
		fw.WithLedger(fw.NewMemLedger()), fw.WithBus(bus), fw.WithSurfaces(native.New()),
		fw.WithServices(pages.Start, Start), fw.WithLogger(quiet()))
	if err != nil {
		t.Fatalf("fw.New: %v", err)
	}
	f, err := fleet.Open(context.Background(), a, fleet.Config{Name: "front", Egress: "fw.events", Interval: time.Hour, Expire: fleet.DefaultExpire})
	if err != nil {
		t.Fatalf("fleet.Open: %v", err)
	}
	fleet.Attach(a, f)
	h, err := a.Handler()
	if err != nil {
		t.Fatalf("Handler: %v", err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(func() {
		srv.Close()
		fleet.Attach(a, nil)
		_ = f.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		a.Close(ctx)
	})
	s := Of(a)
	if s == nil {
		t.Fatal("the routines are off")
	}
	return &front{a: a, bus: bus, f: f, s: s, url: srv.URL}
}

// memStore returns a store in memory with the thresholds of c.
func memStore(c Config) *Store {
	if c.ReadyAfter == 0 {
		c.ReadyAfter = DefaultReadyAfter
	}
	if c.ReadyAgreement == 0 {
		c.ReadyAgreement = DefaultReadyAgreement
	}
	return New(bucket.NewMem(bucketConfig), c, quiet())
}

// event publishes one event of a worker on the egress stream at the time
// at and waits until the view applied it.
func (fr *front) event(t *testing.T, at time.Time, id string, seq uint64, typ string, data any) {
	t.Helper()
	e := fw.MustEvent(id, typ, data)
	e.Seq, e.Node, e.Time = seq, alpha, at
	if err := fr.bus.Publish(context.Background(), "fw.events.team.default."+typ, e); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		r, err := fr.f.Run(context.Background(), id)
		if err == nil && r.Seq >= seq {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the view did not apply %s %d of %s", typ, seq, id)
		}
		time.Sleep(5 * time.Millisecond)
	}
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

// call calls a tool.
func call(t *testing.T, cs *sdk.ClientSession, name string, args any) *sdk.CallToolResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
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

// ack is a routine of the helpdesk, after section 2 of the contract,
// with one question that the kernel compares.
func ack() fw.Routine {
	return fw.Routine{
		Name:    "acknowledge",
		Decider: "clef",
		Questions: map[string]fw.RoutineQuestion{
			"type": {Question: fw.Question{Type: fw.QuestionChoice, Instructions: "What kind of ticket is this?",
				Criteria: map[string]any{"bug": "Something broke", "access": "An account", "other": "None of these"}},
				Act: 0.85, Expect: fw.Expect{"bug", "access"}, Observe: &fw.Observe{Answer: `"type": "type/([a-z-]+)"`}},
			"beyond": {Question: fw.Question{Type: fw.QuestionNoul, Instructions: "Does it ask for more than a label?"}, Act: 0.95, Expect: fw.Expect{"no"}},
		},
		Steps: []fw.RoutineStep{
			{Tool: "gitea.label_read", Input: `{"method": "list", "owner": "lab", "repo": "helpdesk"}`, As: "labels"},
			{Tool: "gitea.issue_write", Input: `{"method": "add_labels", "issue_number": {{ find .input "#(\\d+)" }}}`},
		},
		After:  []fw.RoutineCheck{{Tool: "gitea.issue_read", Input: `{"method": "get"}`, Contains: []string{"status/triaged"}, Lacks: []string{"status/new"}}},
		Answer: `TRIAGE {"type": "type/{{ .type }}"}`,
	}
}

// withClef gives the front a decider, so a local profile takes a routine
// with questions.
func withClef(c *fw.Config) {
	c.Deciders = map[string]fw.DeciderConfig{"clef": {BaseURL: "http://127.0.0.1:9", Model: "clef"}}
}
