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

// newFront serves an agent writer that answers at once, for alice and bob,
// and returns the agent and the address of its native surface.
func newFront(t *testing.T) (*fw.Agent, *httptest.Server) {
	t.Helper()
	t.Setenv("FW_ATESPACE", "")
	cfg := fw.DefaultConfig()
	cfg.Home = t.TempDir()
	cfg.Access.Default = "closed"
	cfg.Agents = map[string]fw.AgentProfile{
		"writer": {Description: "Writes release notes and summaries of the changes.", Access: &fw.Access{Owners: []string{"alice"}, Viewers: []string{"bob"}},
			Policy: &fw.Rules{Default: "allow", Ask: []string{"bash: git push*"}}},
		"ops": {Description: "Keeps the hosts in line with their roles.", Access: &fw.Access{Owners: []string{"carol"}}},
	}
	a, err := fw.New(fw.WithConfig(cfg), fw.WithoutDefaults(), fw.WithAuthn(people{}), fw.WithModel(model()),
		fw.WithLedger(fw.NewMemLedger()), fw.WithBus(fw.NewMemBus()), fw.WithSurfaces(native.New()), fw.WithServices(duties.Start, Service), fw.WithLogger(quiet()))
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
	return a, srv
}

// post sends a JSON body as a person and returns the status and the body.
func post(t *testing.T, srv *httptest.Server, token, path, body string, header ...string) (int, []byte) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, srv.URL+path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

// A result that its person asked to check is a card in review. Looks
// right is a thumb up, and the card is done and accepted at once. Needs
// work needs a note: the thumb goes down, the note goes to the agent, and
// the run works again. A person without the operator role cannot check,
// and the tool does the same as the route.
func TestReviewRoute(t *testing.T) {
	a, srv := newFront(t)
	start := func() string {
		t.Helper()
		r, err := a.Start(as("alice"), "", fw.WithRequest(fw.RunRequest{Agent: "writer", Input: "Draft the notes", Metadata: map[string]string{fleet.MetaReview: "1"}}))
		if err != nil {
			t.Fatal(err)
		}
		waitFor(t, a, r.ID, fw.StateCompleted)
		return r.ID
	}
	right, work := start(), start()
	ans, err := For(as("alice"), a, time.Now(), Query{})
	if err != nil {
		t.Fatal(err)
	}
	if ans.Lanes[4].Stage != StageReview || ans.Lanes[4].Count != 2 || ans.Lanes[4].Yours != 2 {
		t.Fatalf("the lane review = %+v", ans.Lanes[4])
	}
	if code, b := post(t, srv, "alice", "/v1/work/review", `{"run":"`+right+`","verdict":"right"}`); code != http.StatusOK || !strings.Contains(string(b), `"reaction":"up"`) {
		t.Fatalf("Looks right = %d %s", code, b)
	}
	// The thumb is on the end of the run, so the kernel recorded a receipt.
	var end uint64
	var thumb fw.ReactionData
	for e, err := range a.Ledger().Read(context.Background(), right, 0) {
		if err != nil {
			t.Fatal(err)
		}
		switch e.Type {
		case fw.EvRunFinished:
			end = e.Seq
		case fw.EvRunReacted:
			_ = e.Decode(&thumb)
		}
	}
	if end == 0 || thumb.Seq != end || !thumb.Receipt || thumb.By != "alice" {
		t.Fatalf("the thumb = %+v, the end %d", thumb, end)
	}
	// The check clears the cache, so the board shows it at once.
	if c := cardOf(t, mustFor(t, a, "alice"), "run:"+right); c.Stage != StageDone || c.Verdict != VerdictAccepted {
		t.Fatalf("the card after Looks right = %+v", c)
	}
	if code, b := post(t, srv, "alice", "/v1/work/review", `{"run":"`+work+`","verdict":"work"}`); code != http.StatusBadRequest || !strings.Contains(string(b), "needs a note") {
		t.Fatalf("Needs work without a note = %d %s", code, b)
	}
	if code, b := post(t, srv, "bob", "/v1/work/review", `{"run":"`+work+`","verdict":"right"}`); code != http.StatusForbidden && code != http.StatusNotFound {
		t.Fatalf("a check by a viewer = %d %s", code, b)
	}
	cs := connect(t, a, "alice")
	res, err := cs.CallTool(context.Background(), &sdk.CallToolParams{Name: ToolReview, Arguments: map[string]any{"run": work, "verdict": "work", "note": "Name the release in the title."}})
	if err != nil || res.IsError {
		t.Fatalf("Needs work = %+v, %v", res, err)
	}
	r, err := a.Get(context.Background(), work)
	if err != nil || r.Reactions[fw.ReactionDown] != 1 {
		t.Fatalf("the run after Needs work = %+v, %v", r, err)
	}
	// The follow-up made the run work again, and the model answered it.
	waitFor(t, a, work, fw.StateCompleted)
	events := 0
	for e, err := range a.Ledger().Read(context.Background(), work, 0) {
		if err != nil {
			t.Fatal(err)
		}
		if e.Type == fw.EvMessageReceived && strings.Contains(string(e.Data), "Name the release in the title.") {
			events++
		}
	}
	if events != 1 {
		t.Fatalf("the follow-up reached the run %d times", events)
	}
}

func mustFor(t *testing.T, a *fw.Agent, who string) Answer {
	t.Helper()
	ans, err := For(as(who), a, time.Now(), Query{})
	if err != nil {
		t.Fatal(err)
	}
	return ans
}

// The route and the tool of the plan answer the same plan for the profiles
// that the person can run: the description picks the agent, the policy
// says when it asks, and the header names the time zone. An agent that the
// person cannot run is no choice, and empty words or a time zone that does
// not exist answer 400.
func TestPlanRoute(t *testing.T) {
	t.Setenv(fleet.EnvIntake, "fw.jobs")
	a, srv := newFront(t)
	code, b := post(t, srv, "alice", "/v1/work/plan", `{"words":"Summarize the changes of the week every Friday at 4pm"}`, "X-Time-Zone", "Europe/Berlin")
	var p Plan
	if err := json.Unmarshal(b, &p); code != http.StatusOK || err != nil {
		t.Fatalf("the plan = %d %s", code, b)
	}
	if p.Kind != PlanRepeat || len(p.Who) != 1 || p.Who[0].Agent != "writer" || p.When.Readback != "Every Friday at 4:00 PM, Berlin time." || p.Start.Duty == nil {
		t.Fatalf("the plan = %s", b)
	}
	if strings.Join(p.Asks, "|") != "Before it runs git push|When it is not sure what you mean. The question waits for you" || len(p.Others) != 1 {
		t.Fatalf("asks %q, others %+v", p.Asks, p.Others)
	}
	cs := connect(t, a, "alice")
	res, err := cs.CallTool(context.Background(), &sdk.CallToolParams{Name: ToolPlan, Arguments: map[string]any{"words": "Summarize the changes of the week every Friday at 4pm", "timeZone": "Europe/Berlin"}})
	if err != nil || res.IsError {
		t.Fatalf("the tool = %+v, %v", res, err)
	}
	raw, _ := json.Marshal(res.StructuredContent)
	var tooled Plan
	if err := json.Unmarshal(raw, &tooled); err != nil || tooled.Title != p.Title || tooled.When.Readback != p.When.Readback || tooled.Start.Duty["name"] != p.Start.Duty["name"] {
		t.Fatalf("the plan of the tool = %s, %v", raw, err)
	}
	if txt := res.Content[0].(*sdk.TextContent).Text; !strings.Contains(txt, "Who: writer does it.") || !strings.Contains(txt, "Set the most it can spend before the start.") {
		t.Fatalf("the text of the plan = %s", txt)
	}
	if code, b := post(t, srv, "alice", "/v1/work/plan", `{"words":"Check the hosts", "agent":"ops"}`); code != http.StatusOK || !strings.Contains(string(b), "You cannot run an agent named ops") {
		t.Fatalf("an agent of another person = %d %s", code, b)
	}
	// A duty of alice that does the same work is the same of the plan.
	d := duties.Duty{Name: "week-notes", Title: "Notes of the week", Agent: "writer", When: duties.When{Say: "every friday at 4pm"},
		Do: duties.Do{Ask: &duties.Ask{Message: "Summarize the changes of the week."}}, Limits: duties.Limits{Budget: &duties.Budget{MaxCostUSD: 0.5}}}
	if _, err := duties.Of(a).Put(as("alice"), d.Name, duties.DutyBody{Duty: d}, duties.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	if code, b := post(t, srv, "alice", "/v1/work/plan", `{"words":"Summarize the changes of the week every Friday at 4pm"}`); code != http.StatusOK || !strings.Contains(string(b), `"same":{"duty":"week-notes","title":"Notes of the week"}`) {
		t.Fatalf("the plan with the same work = %d %s", code, b)
	}
	for _, x := range []struct{ body, zone string }{{`{"words":"  "}`, ""}, {`{"words":"Check the hosts"}`, "Mars/Olympus"}, {`{"words":"x","tell":"never"}`, ""}} {
		if code, b := post(t, srv, "alice", "/v1/work/plan", x.body, "X-Time-Zone", x.zone); code != http.StatusBadRequest {
			t.Fatalf("POST %s in %q = %d %s", x.body, x.zone, code, b)
		}
	}
}

// A duty on a schedule shows its next start in the lane Coming up of its
// owner, with since at that start. A paused duty shows none.
func TestNextStartRoute(t *testing.T) {
	t.Setenv(fleet.EnvIntake, "fw.jobs")
	a, _ := newFront(t)
	svc := duties.Of(a)
	d := duties.Duty{Name: "notes", Agent: "writer", When: duties.When{Say: "every day at 9am"},
		Do: duties.Do{Ask: &duties.Ask{Message: "Write the notes."}}, Limits: duties.Limits{Budget: &duties.Budget{MaxCostUSD: 0.5}}}
	if _, err := svc.Put(as("alice"), "notes", duties.DutyBody{Duty: d}, duties.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	v, err := svc.Get(as("alice"), "notes")
	if err != nil || len(v.Next) == 0 {
		t.Fatalf("the duty = %+v, %v", v, err)
	}
	c := cardOf(t, mustFor(t, a, "alice"), dutyKey("notes", v.Next[0]))
	if c.Stage != StageWaiting || !c.Since.Equal(v.Next[0]) || c.Kind != KindDuty || c.Links.Duty != "notes" {
		t.Fatalf("the next start = %+v", c)
	}
	if _, err := svc.SetPaused(as("alice"), "notes", true); err != nil {
		t.Fatal(err)
	}
	CacheFor = 0
	t.Cleanup(func() { CacheFor = 10 * time.Second })
	for _, c := range mustFor(t, a, "alice").Cards {
		if c.Links.Duty == "notes" {
			t.Fatalf("a paused duty shows %+v", c)
		}
	}
}
