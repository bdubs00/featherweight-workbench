package work

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
	"github.com/bdubs00/featherweight-workbench/duties"
	"github.com/bdubs00/featherweight-workbench/fleet"
	"github.com/bdubs00/featherweight-workbench/surface/mcp"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// people takes the bearer token as the subject of a person.
type people struct{}

func (people) Authenticate(r *http.Request) (*fw.Principal, error) {
	tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if tok == "" || tok == r.Header.Get("Authorization") {
		return nil, fw.ErrUnauthorized
	}
	return &fw.Principal{Subject: tok, Kind: fw.KindUser}, nil
}

func (people) Challenge(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusUnauthorized)
}

// model calls deploy for an input with deploy, and answers every other
// input at once.
func model() fw.Model {
	return fw.ModelFunc{StreamFn: func(_ context.Context, req *fw.Request, _ func(fw.Delta)) (*fw.Response, error) {
		first := ""
		for _, m := range req.Messages {
			for _, b := range m.Content {
				if b.Type == fw.BlockToolResult {
					return &fw.Response{Content: []fw.Block{fw.Text("done")}, StopReason: fw.StopEndTurn}, nil
				}
				if first == "" && b.Type == fw.BlockText {
					first = b.Text
				}
			}
		}
		if !strings.Contains(first, "deploy") {
			return &fw.Response{Content: []fw.Block{fw.Text("The answer.")}, StopReason: fw.StopEndTurn}, nil
		}
		return &fw.Response{Content: []fw.Block{{Type: fw.BlockToolUse, ID: "c1", Name: "deploy", Input: json.RawMessage(`{}`)}}, StopReason: fw.StopToolUse}, nil
	}}
}

func as(who string) context.Context {
	return fw.WithPrincipal(context.Background(), &fw.Principal{Subject: who, Kind: fw.KindUser})
}

func waitFor(t *testing.T, a *fw.Agent, id string, state fw.RunState) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		r, err := a.Get(context.Background(), id)
		if err == nil && r.State == state {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the run %s did not reach %s: %+v, %v", id, state, r, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// The route and the tool answer the same board. The board of a person
// holds the work of the person: a run that waits for an approval, with
// the id of the approval, and a conversation that the agent answered.
// Another person sees none of it, and a window out of bounds is an error.
func TestRouteAndTool(t *testing.T) {
	t.Setenv(fleet.EnvIntake, "fw.jobs")
	t.Setenv("FW_ATESPACE", "")
	cfg := fw.DefaultConfig()
	cfg.Home = t.TempDir()
	cfg.Access.Default = "closed"
	cfg.Agents = map[string]fw.AgentProfile{"triage": {Access: &fw.Access{Owners: []string{"alice"}}}}
	deploy := fw.NewTool(fw.ToolSpec{Name: "deploy", Description: "Deploy", InputSchema: json.RawMessage(`{"type":"object"}`), Destructive: true},
		func(context.Context, json.RawMessage) (fw.ToolResult, error) { return fw.TextResult("deployed"), nil })
	a, err := fw.New(fw.WithConfig(cfg), fw.WithoutDefaults(), fw.WithAuthn(people{}), fw.WithModel(model()), fw.WithTools(deploy),
		fw.WithPolicy(fw.NewRulesPolicy(fw.DefaultRules())), fw.WithLedger(fw.NewMemLedger()), fw.WithBus(fw.NewMemBus()),
		fw.WithSurfaces(native.New()), fw.WithServices(duties.Start), fw.WithLogger(quiet()))
	if err != nil {
		t.Fatal(err)
	}
	h, err := a.Handler()
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(func() {
		srv.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		a.Close(ctx)
	})
	r1, err := a.Start(as("alice"), "", fw.WithRequest(fw.RunRequest{Agent: "triage", Input: "deploy the site"}))
	if err != nil {
		t.Fatal(err)
	}
	r2, err := a.Start(as("alice"), "", fw.WithRequest(fw.RunRequest{Agent: "triage", Input: "What is the plan?"}))
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, a, r1.ID, fw.StateInputRequired)
	waitFor(t, a, r2.ID, fw.StateCompleted)

	now := time.Now()
	ans, err := For(as("alice"), a, now, Query{})
	if err != nil {
		t.Fatal(err)
	}
	if len(ans.Cards) != 2 {
		t.Fatalf("the cards = %+v", ans.Cards)
	}
	// The board of a caller stays for CacheFor: a new run shows after it.
	r3, err := a.Start(as("alice"), "", fw.WithRequest(fw.RunRequest{Agent: "triage", Input: "What is next?"}))
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, a, r3.ID, fw.StateCompleted)
	if again, err := For(as("alice"), a, now.Add(time.Second), Query{}); err != nil || len(again.Cards) != 2 {
		t.Fatalf("the board within the cache = %d cards, %v", len(again.Cards), err)
	}
	if later, err := For(as("alice"), a, now.Add(CacheFor), Query{}); err != nil || len(later.Cards) != 3 {
		t.Fatalf("the board after the cache = %d cards, %v", len(later.Cards), err)
	}
	ask, done := ans.Cards[0], ans.Cards[1]
	pending, _ := a.Get(context.Background(), r1.ID)
	if ask.ID != "run:"+r1.ID || ask.Stage != StageNeedsYou || ask.Next != NextYou || pending.Pending == nil || ask.Links.Approval != pending.Pending.ID || ask.Reason != "Asks to run deploy." {
		t.Fatalf("the card that needs alice = %+v", ask)
	}
	if done.ID != "run:"+r2.ID || done.Stage != StageDone || done.Verdict != VerdictUnchecked || done.Ended == nil {
		t.Fatalf("the answered card = %+v", done)
	}

	get := func(token, query string) (int, Answer) {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/work"+query, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out Answer
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}
	code, routed := get("alice", "?days=30")
	if code != http.StatusOK || len(routed.Cards) != 3 || routed.Cards[0].Links.Approval != ask.Links.Approval || routed.Days != 30 || !strings.HasSuffix(routed.Sentence, "2 done in the last 30 days.") {
		t.Fatalf("GET /v1/work = %d %+v", code, routed)
	}
	if code, other := get("bob", ""); code != http.StatusOK || len(other.Cards) != 0 {
		t.Fatalf("the board of bob = %d %+v", code, other)
	}
	if code, other := get("bob", "?all=true"); code != http.StatusOK || len(other.Cards) != 0 {
		t.Fatalf("every card that bob can view = %d %+v", code, other)
	}
	for _, q := range []string{"?days=0x", "?days=91", "?days=3", "?all=maybe"} {
		if code, _ := get("alice", q); code != http.StatusBadRequest {
			t.Fatalf("GET /v1/work%s = %d, want 400", q, code)
		}
	}

	cs := connect(t, a, "alice")
	res, err := cs.CallTool(context.Background(), &sdk.CallToolParams{Name: ToolWork, Arguments: map[string]any{"days": 30}})
	if err != nil || res.IsError {
		t.Fatalf("the tool = %+v, %v", res, err)
	}
	txt := res.Content[0].(*sdk.TextContent).Text
	if !strings.HasPrefix(txt, "1 task is open. 1 needs you") || !strings.Contains(txt, "\n\nNeeds you (1):\n- deploy the site, a conversation from alice: Asks to run deploy.") || !strings.Contains(txt, "Approval "+ask.Links.Approval+".") {
		t.Fatalf("the text of the tool = %s", txt)
	}
	b, _ := json.Marshal(res.StructuredContent)
	var tooled Answer
	if err := json.Unmarshal(b, &tooled); err != nil || len(tooled.Cards) != 3 || tooled.Cards[0].ID != ask.ID || tooled.Days != 30 {
		t.Fatalf("the structured content of the tool = %s, %v", b, err)
	}
	bad, err := cs.CallTool(context.Background(), &sdk.CallToolParams{Name: ToolWork, Arguments: map[string]any{"days": 12}})
	if err != nil || !bad.IsError {
		t.Fatalf("a window of 120 days = %+v, %v", bad, err)
	}
}

// connect serves the MCP surface of the agent and connects a client with
// the token.
func connect(t *testing.T, a *fw.Agent, token string) *sdk.ClientSession {
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
