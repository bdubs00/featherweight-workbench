package value

import (
	"bytes"
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
	"github.com/bdubs00/featherweight-workbench/internal/bucket"
	"github.com/bdubs00/featherweight-workbench/surface/mcp"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

type people struct{}

func (people) Authenticate(r *http.Request) (*fw.Principal, error) {
	tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if tok == "" || tok == r.Header.Get("Authorization") {
		return nil, fw.ErrUnauthorized
	}
	return &fw.Principal{Subject: tok, Kind: fw.KindUser}, nil
}

func (people) Challenge(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusUnauthorized) }

// model calls deploy for an input with deploy and ends the turn otherwise.
func model() fw.Model {
	return fw.ModelFunc{StreamFn: func(_ context.Context, req *fw.Request, _ func(fw.Delta)) (*fw.Response, error) {
		first, result := "", false
		for _, m := range req.Messages {
			for _, b := range m.Content {
				if b.Type == fw.BlockToolResult {
					result = true
				}
				if first == "" && b.Type == fw.BlockText {
					first = b.Text
				}
			}
		}
		if result || !strings.Contains(first, "deploy") {
			return &fw.Response{Content: []fw.Block{fw.Text("done")}, StopReason: fw.StopEndTurn}, nil
		}
		return &fw.Response{Content: []fw.Block{{Type: fw.BlockToolUse, ID: "c1", Name: "deploy", Input: json.RawMessage(`{}`)}}, StopReason: fw.StopToolUse}, nil
	}}
}

func as(who string) context.Context {
	return fw.WithPrincipal(context.Background(), &fw.Principal{Subject: who, Kind: fw.KindUser})
}

func waitFor(t *testing.T, a *fw.Agent, id string, state fw.RunState) *fw.Run {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		r, err := a.Get(context.Background(), id)
		if err == nil && r.State == state {
			return r
		}
		if time.Now().After(deadline) {
			t.Fatalf("the run %s did not reach %s: %+v, %v", id, state, r, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// The value of the work of a person counts the runs that the person can
// view: a run that needed an approval of the person, a run that the
// person accepted with a thumb, and a run that needed nobody. The route
// and the tool answer the same numbers, another person sees no work, and
// only the owner of the agent sets an estimate of a line.
func TestValue(t *testing.T) {
	cfg := fw.DefaultConfig()
	cfg.Home = t.TempDir()
	cfg.Access.Default = "closed"
	cfg.Agents = map[string]fw.AgentProfile{"triage": {Access: &fw.Access{Owners: []string{"alice"}, Operators: []string{"carol"}}}}
	deploy := fw.NewTool(fw.ToolSpec{Name: "deploy", Description: "Deploy", InputSchema: json.RawMessage(`{"type":"object"}`), Destructive: true},
		func(context.Context, json.RawMessage) (fw.ToolResult, error) { return fw.TextResult("deployed"), nil })
	a, err := fw.New(fw.WithConfig(cfg), fw.WithoutDefaults(), fw.WithAuthn(people{}), fw.WithModel(model()), fw.WithTools(deploy),
		fw.WithPolicy(fw.NewRulesPolicy(fw.DefaultRules())), fw.WithLedger(fw.NewMemLedger()), fw.WithBus(fw.NewMemBus()),
		fw.WithSurfaces(native.New()), fw.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	if err != nil {
		t.Fatal(err)
	}
	Attach(a, NewStore(bucket.NewMem(bucketConfig)))
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
	pending := waitFor(t, a, r1.ID, fw.StateInputRequired)
	if err := a.Decide(as("alice"), r1.ID, fw.ApprovalDecision{ApprovalID: pending.Pending.ID, Decision: "allow"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, a, r1.ID, fw.StateCompleted)
	r2, err := a.Start(as("alice"), "", fw.WithRequest(fw.RunRequest{Agent: "triage", Input: "summarize the queue"}))
	if err != nil {
		t.Fatal(err)
	}
	done := waitFor(t, a, r2.ID, fw.StateCompleted)
	if err := a.React(as("alice"), r2.ID, fw.ReactionData{Seq: done.Seq, Reaction: fw.ReactionUp}); err != nil {
		t.Fatal(err)
	}
	r3, err := a.Start(as("alice"), "", fw.WithRequest(fw.RunRequest{Agent: "triage", Input: "list the open tickets"}))
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, a, r3.ID, fw.StateCompleted)

	ans, err := For(as("alice"), a, time.Now(), 7)
	if err != nil {
		t.Fatal(err)
	}
	if ans.Work.Done != 3 || ans.Checks.Accepted != 1 || ans.Checks.Unchecked != 2 || ans.People.Actions != 1 || ans.People.NoPerson != 2 || ans.Partial {
		t.Fatalf("the value of alice = %+v", ans)
	}
	if ans.Lead.Measured != 3 || ans.People.WaitSeconds < 0 || len(ans.Lines) != 1 || ans.Lines[0].Key != "agent:triage" || ans.Lines[0].Agent != "triage" {
		t.Fatalf("the lead and the lines = %+v %+v", ans.Lead, ans.Lines)
	}
	if other, err := For(as("bob"), a, time.Now(), 7); err != nil || other.Work.Pieces != 0 || len(other.Lines) != 0 {
		t.Fatalf("the value of bob = %+v, %v", other, err)
	}
	if _, err := For(as("alice"), a, time.Now(), 5); err == nil {
		t.Fatal("a window of 5 days answered")
	}

	get := func(who, q string) (int, Answer) {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/value"+q, nil)
		req.Header.Set("Authorization", "Bearer "+who)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out Answer
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}
	if code, routed := get("alice", "?days=7"); code != http.StatusOK || routed.Work != ans.Work || routed.Checks != ans.Checks || routed.Sentence != ans.Sentence {
		t.Fatalf("GET /v1/value = %d %+v", code, routed)
	}
	if code, _ := get("alice", "?days=5"); code != http.StatusBadRequest {
		t.Fatalf("GET /v1/value?days=5 = %d, want 400", code)
	}
	if code, routed := get("alice", ""); code != http.StatusOK || routed.Days != DefaultDays {
		t.Fatalf("GET /v1/value = %d, days %d", code, routed.Days)
	}

	put := func(who string, body string) int {
		req, _ := http.NewRequest(http.MethodPut, srv.URL+"/v1/value/estimates", bytes.NewBufferString(body))
		req.Header.Set("Authorization", "Bearer "+who)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := put("bob", `{"line":"agent:triage","minutes":20}`); code != http.StatusNotFound {
		t.Fatalf("an estimate of bob = %d, want 404", code)
	}
	if code := put("carol", `{"line":"agent:triage","minutes":20}`); code != http.StatusForbidden {
		t.Fatalf("an estimate of an operator = %d, want 403", code)
	}
	if code := put("alice", `{"line":"agent:triage","minutes":9999}`); code != http.StatusBadRequest {
		t.Fatalf("an estimate of 9999 minutes = %d, want 400", code)
	}
	if code := put("alice", `{"line":"agent:triage","minutes":20}`); code != http.StatusOK {
		t.Fatalf("an estimate of alice = %d, want 200", code)
	}
	_, routed := get("alice", "?days=7")
	if routed.Estimated == nil || routed.Estimated.Minutes != 60 || routed.Lines[0].Estimate == nil || routed.Lines[0].Estimate.By != "alice" {
		t.Fatalf("the value with the estimate = %+v %+v", routed.Estimated, routed.Lines)
	}

	cs := connect(t, a, "alice")
	res, err := cs.CallTool(context.Background(), &sdk.CallToolParams{Name: ToolValue, Arguments: map[string]any{"days": 7}})
	if err != nil || res.IsError {
		t.Fatalf("the tool value = %+v, %v", res, err)
	}
	if txt := res.Content[0].(*sdk.TextContent).Text; !strings.HasPrefix(txt, "In the last 7 days, the agents finished 3 pieces of work.") || !strings.Contains(txt, "\n- agent:triage: 3 done") || !strings.Contains(txt, "20 min a piece by alice") {
		t.Fatalf("the text of the tool = %s", txt)
	}
	res, err = cs.CallTool(context.Background(), &sdk.CallToolParams{Name: ToolEstimate, Arguments: map[string]any{"line": "agent:triage", "minutes": 0}})
	if err != nil || res.IsError || !strings.Contains(res.Content[0].(*sdk.TextContent).Text, "has no estimate now") {
		t.Fatalf("the tool value_estimate = %+v, %v", res, err)
	}
	bob := connect(t, a, "bob")
	res, err = bob.CallTool(context.Background(), &sdk.CallToolParams{Name: ToolEstimate, Arguments: map[string]any{"line": "agent:triage", "minutes": 5}})
	if err != nil || !res.IsError {
		t.Fatalf("the tool value_estimate of bob = %+v, %v", res, err)
	}
}

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
