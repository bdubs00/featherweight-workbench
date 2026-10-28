package inbox

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

// model calls deploy for an input with deploy, ask for an input with ask,
// and ends the turn after a tool result.
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
		name := "deploy"
		if strings.Contains(first, "ask") {
			name = "ask"
		}
		return &fw.Response{Content: []fw.Block{{Type: fw.BlockToolUse, ID: "c1", Name: name, Input: json.RawMessage(`{}`)}}, StopReason: fw.StopToolUse}, nil
	}}
}

func as(who string) context.Context {
	return fw.WithPrincipal(context.Background(), &fw.Principal{Subject: who, Kind: fw.KindUser})
}

// waitFor waits until the run is in the state.
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

// The inbox of a person holds the runs that wait for the person, an
// approval and a question, the failed runs of the duties of the person in
// the last day, and the duties of the person that paused themselves. The
// route and the tool answer the same items, and another person sees none
// of them.
func TestInbox(t *testing.T) {
	t.Setenv(fleet.EnvIntake, "fw.jobs")
	t.Setenv("FW_ATESPACE", "")
	cfg := fw.DefaultConfig()
	cfg.Home = t.TempDir()
	cfg.Access.Default = "closed"
	cfg.Agents = map[string]fw.AgentProfile{"triage": {Access: &fw.Access{Owners: []string{"alice"}}}}
	deploy := fw.NewTool(fw.ToolSpec{Name: "deploy", Description: "Deploy", InputSchema: json.RawMessage(`{"type":"object"}`), Destructive: true},
		func(context.Context, json.RawMessage) (fw.ToolResult, error) { return fw.TextResult("deployed"), nil })
	ask := fw.NewTool(fw.ToolSpec{Name: "ask", Description: "Ask", InputSchema: json.RawMessage(`{"type":"object"}`), ReadOnly: true},
		func(ctx context.Context, _ json.RawMessage) (fw.ToolResult, error) {
			b, err := fw.AskCaller(ctx, "Which branch do I use?", nil)
			return fw.TextResult(string(b)), err
		})
	a, err := fw.New(fw.WithConfig(cfg), fw.WithoutDefaults(), fw.WithAuthn(people{}), fw.WithModel(model()), fw.WithTools(deploy, ask),
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
	// A run of the duty acts as the duty, not as alice.
	r1, err := a.Start(context.Background(), "", fw.WithRequest(fw.RunRequest{Agent: "triage", Input: "deploy the site", Principal: "duty:report", PrincipalKind: fw.KindWorkload,
		Metadata: map[string]string{duties.MetaDuty: "report", fw.MetaBatch: "bridge:B1"}}))
	if err != nil {
		t.Fatal(err)
	}
	r2, err := a.Start(as("alice"), "", fw.WithRequest(fw.RunRequest{Agent: "triage", Input: "ask about the branch"}))
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, a, r1.ID, fw.StateInputRequired)
	waitFor(t, a, r2.ID, fw.StateInputRequired)

	svc := duties.Of(a)
	d := duties.Duty{
		Name: "report", Agent: "triage",
		When:   duties.When{Webhook: &duties.Webhook{Auth: duties.WebhookAuth{Mode: "none"}}},
		Do:     duties.Do{Ask: &duties.Ask{Message: "Write the report."}},
		Limits: duties.Limits{Budget: &duties.Budget{MaxCostUSD: 0.5}, Retries: new(int)},
	}
	if _, err := svc.Put(as("alice"), "report", duties.DutyBody{Duty: d}, duties.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	t.Setenv(fleet.EnvIntake, "")
	for range 3 {
		if out, err := svc.RunNow(as("alice"), "report"); err != nil || out.Failed == "" {
			t.Fatalf("RunNow = %+v, %v", out, err)
		}
	}

	ans, err := For(as("alice"), a, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	kinds := []string{}
	for _, it := range ans.Items {
		kinds = append(kinds, it.Kind)
	}
	// The approval and the question begin to wait within a few
	// microseconds, so either can come first.
	if k := strings.Join(kinds, ","); k != "approval,question,failure,failure,failure,paused" && k != "question,approval,failure,failure,failure,paused" {
		t.Fatalf("the kinds = %v\n%+v", kinds, ans.Items)
	}
	ap, q, fail, paused := ans.Items[0], ans.Items[1], ans.Items[2], ans.Items[5]
	if ap.Kind == KindQuestion {
		ap, q = q, ap
	}
	if pending, _ := a.Get(context.Background(), r1.ID); pending == nil || pending.Pending == nil || ap.Approval != pending.Pending.ID || ap.Approval == "" {
		t.Fatalf("the approval id of the item = %q, want the pending approval of %s", ap.Approval, r1.ID)
	}
	if q.Approval == "" {
		t.Fatalf("the question has no approval id: %+v", q)
	}
	if ap.Run != r1.ID || ap.Duty != "report" || ap.Bridge != "B1" || ap.Agent != "triage" || !strings.Contains(ap.Text, "the agent triage asks to run deploy. Rule: ask destructive.") || ap.Since.IsZero() {
		t.Fatalf("the approval = %+v", ap)
	}
	if q.Run != r2.ID || !strings.Contains(q.Text, "Which branch do I use?") {
		t.Fatalf("the question = %+v", q)
	}
	if fail.Duty != "report" || !strings.Contains(fail.Text, "The queue did not take the run") {
		t.Fatalf("the failure = %+v", fail)
	}
	if paused.Duty != "report" || !strings.Contains(paused.Text, "The duty report paused itself: 3 runs failed in a row") {
		t.Fatalf("the paused duty = %+v", paused)
	}

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/inbox", nil)
	req.Header.Set("Authorization", "Bearer alice")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var routed struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&routed); err != nil || resp.StatusCode != http.StatusOK || len(routed.Items) != 6 {
		t.Fatalf("GET /v1/inbox = %d %+v %v", resp.StatusCode, routed, err)
	}
	for _, key := range []string{"kind", "run", "duty", "agent", "bridge", "approval", "text", "since"} {
		if _, ok := routed.Items[5][key]; !ok {
			t.Errorf("an item of the route lacks %s: %v", key, routed.Items[5])
		}
	}
	resp.Body.Close()

	cs := connect(t, a, "alice")
	res, err := cs.CallTool(context.Background(), &sdk.CallToolParams{Name: ToolInbox, Arguments: map[string]any{}})
	if err != nil || res.IsError {
		t.Fatalf("the tool = %+v, %v", res, err)
	}
	if txt := res.Content[0].(*sdk.TextContent).Text; !strings.HasPrefix(txt, "6 items need you:\n- ") || !strings.Contains(txt, "\n- approval: ") || !strings.Contains(txt, "(run "+r1.ID+")") {
		t.Fatalf("the text of the tool = %s", txt)
	}
	other, err := For(as("bob"), a, time.Now())
	if err != nil || len(other.Items) != 0 {
		t.Fatalf("the inbox of bob = %+v, %v", other, err)
	}
	if txt := text(other); txt != "Nothing needs you now." {
		t.Fatalf("an empty inbox = %q", txt)
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

// A run that completed with the metadata of a check is a result in the
// inbox of its person until somebody reacts to it. A result of another
// person, and a result that nobody asked to check, are no item.
func TestInboxResult(t *testing.T) {
	t.Setenv("FW_ATESPACE", "")
	cfg := fw.DefaultConfig()
	cfg.Home = t.TempDir()
	cfg.Access.Default = "closed"
	cfg.Agents = map[string]fw.AgentProfile{"writer": {Access: &fw.Access{Owners: []string{"alice", "bob"}}}}
	answer := fw.ModelFunc{StreamFn: func(context.Context, *fw.Request, func(fw.Delta)) (*fw.Response, error) {
		return &fw.Response{Content: []fw.Block{fw.Text("The draft.")}, StopReason: fw.StopEndTurn}, nil
	}}
	a, err := fw.New(fw.WithConfig(cfg), fw.WithoutDefaults(), fw.WithAuthn(people{}), fw.WithModel(answer),
		fw.WithLedger(fw.NewMemLedger()), fw.WithBus(fw.NewMemBus()), fw.WithSurfaces(native.New()), fw.WithLogger(quiet()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		a.Close(ctx)
	})
	start := func(who string, md map[string]string) string {
		t.Helper()
		r, err := a.Start(as(who), "", fw.WithRequest(fw.RunRequest{Agent: "writer", Input: "Draft the notes", Metadata: md}))
		if err != nil {
			t.Fatal(err)
		}
		waitFor(t, a, r.ID, fw.StateCompleted)
		return r.ID
	}
	check := map[string]string{fleet.MetaReview: "1"}
	mine := start("alice", check)
	start("alice", nil)
	start("bob", check)
	ans, err := For(as("alice"), a, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(ans.Items) != 1 || ans.Items[0].Kind != KindResult || ans.Items[0].Run != mine || ans.Items[0].Agent != "writer" || !strings.HasSuffix(ans.Items[0].Text, ": a result to check.") {
		t.Fatalf("the inbox of alice = %+v", ans.Items)
	}
	if err := a.React(as("alice"), mine, fw.ReactionData{Reaction: fw.ReactionUp}); err != nil {
		t.Fatal(err)
	}
	if ans, err := For(as("alice"), a, time.Now()); err != nil || len(ans.Items) != 0 {
		t.Fatalf("the inbox after a thumb up = %+v, %v", ans.Items, err)
	}
}

// A result to check of a duty reaches the people that notify.result of the
// duty names, beside its owner. A person whom the duty does not name, and
// who can view the run too, sees no item.
func TestInboxResultOfDuty(t *testing.T) {
	t.Setenv(fleet.EnvIntake, "fw.jobs")
	t.Setenv("FW_ATESPACE", "")
	cfg := fw.DefaultConfig()
	cfg.Home = t.TempDir()
	cfg.Access.Default = "closed"
	cfg.Agents = map[string]fw.AgentProfile{"writer": {Access: &fw.Access{Owners: []string{"alice"}, Viewers: []string{"carol", "dave"}}}}
	answer := fw.ModelFunc{StreamFn: func(context.Context, *fw.Request, func(fw.Delta)) (*fw.Response, error) {
		return &fw.Response{Content: []fw.Block{fw.Text("The summary.")}, StopReason: fw.StopEndTurn}, nil
	}}
	a, err := fw.New(fw.WithConfig(cfg), fw.WithoutDefaults(), fw.WithAuthn(people{}), fw.WithModel(answer),
		fw.WithLedger(fw.NewMemLedger()), fw.WithBus(fw.NewMemBus()), fw.WithSurfaces(native.New()),
		fw.WithServices(duties.Start), fw.WithLogger(quiet()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		a.Close(ctx)
	})
	d := duties.Duty{
		Name: "summary", Agent: "writer",
		When:   duties.When{Webhook: &duties.Webhook{Auth: duties.WebhookAuth{Mode: "none"}}},
		Do:     duties.Do{Ask: &duties.Ask{Message: "Sum up the week."}},
		Limits: duties.Limits{Budget: &duties.Budget{MaxCostUSD: 0.5}},
		Notify: &duties.Notify{Result: []string{"carol"}},
		Review: true,
	}
	if _, err := duties.Of(a).Put(as("alice"), "summary", duties.DutyBody{Duty: d}, duties.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	t.Setenv(fleet.EnvIntake, "")
	// A start of the duty acts as the duty, as the engine starts it.
	r, err := a.Start(context.Background(), "", fw.WithRequest(fw.RunRequest{Agent: "writer", Input: "Sum up the week.", Principal: "duty:summary", PrincipalKind: fw.KindWorkload,
		Metadata: map[string]string{duties.MetaDuty: "summary", fleet.MetaReview: "1"}}))
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, a, r.ID, fw.StateCompleted)
	for who, want := range map[string]int{"alice": 1, "carol": 1, "dave": 0} {
		ans, err := For(as(who), a, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, it := range ans.Items {
			if it.Kind == KindResult && it.Run == r.ID && it.Duty == "summary" {
				n++
			}
		}
		if n != want {
			t.Fatalf("the results to check of %s = %d, want %d: %+v", who, n, want, ans.Items)
		}
	}
}

// After Needs work, the new answer of the agent is a result to check
// again, and the inbox says that it follows a note.
func TestInboxResultAgain(t *testing.T) {
	t.Setenv("FW_ATESPACE", "")
	cfg := fw.DefaultConfig()
	cfg.Home = t.TempDir()
	cfg.Access.Default = "closed"
	cfg.Agents = map[string]fw.AgentProfile{"writer": {Access: &fw.Access{Owners: []string{"alice"}}}}
	answer := fw.ModelFunc{StreamFn: func(context.Context, *fw.Request, func(fw.Delta)) (*fw.Response, error) {
		return &fw.Response{Content: []fw.Block{fw.Text("The draft.")}, StopReason: fw.StopEndTurn}, nil
	}}
	a, err := fw.New(fw.WithConfig(cfg), fw.WithoutDefaults(), fw.WithAuthn(people{}), fw.WithModel(answer),
		fw.WithLedger(fw.NewMemLedger()), fw.WithBus(fw.NewMemBus()), fw.WithSurfaces(native.New()), fw.WithLogger(quiet()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		a.Close(ctx)
	})
	r, err := a.Start(as("alice"), "", fw.WithRequest(fw.RunRequest{Agent: "writer", Input: "Draft the notes", Metadata: map[string]string{fleet.MetaReview: "1"}}))
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, a, r.ID, fw.StateCompleted)
	if err := a.React(as("alice"), r.ID, fw.ReactionData{Reaction: fw.ReactionDown, Note: "Shorter."}); err != nil {
		t.Fatal(err)
	}
	if ans, err := For(as("alice"), a, time.Now()); err != nil || len(ans.Items) != 0 {
		t.Fatalf("the inbox after Needs work = %+v, %v", ans.Items, err)
	}
	if err := a.Send(as("alice"), r.ID, fw.Message{Role: fw.RoleUser, Content: []fw.Block{fw.Text(fleet.SendBackText("Shorter."))}}, fw.OriginHTTP); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		ans, err := For(as("alice"), a, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		if len(ans.Items) == 1 && ans.Items[0].Kind == KindResult && strings.HasSuffix(ans.Items[0].Text, ": a new answer to check, after your note.") {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the inbox after the new answer = %+v", ans.Items)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A duty that tells only of what it finds and whose run found something
// gives the people of notify.result an item of kind result that says so,
// and nobody else; the owner is one of them by the word owner. A run that
// found nothing gives no item, and Looks right ends the item.
func TestInboxFound(t *testing.T) {
	t.Setenv(fleet.EnvIntake, "fw.jobs")
	t.Setenv("FW_ATESPACE", "")
	cfg := fw.DefaultConfig()
	cfg.Home = t.TempDir()
	cfg.Access.Default = "closed"
	cfg.Agents = map[string]fw.AgentProfile{"ops": {Access: &fw.Access{Owners: []string{"alice"}, Viewers: []string{"carol", "dave"}}}}
	answer := fw.ModelFunc{StreamFn: func(_ context.Context, req *fw.Request, _ func(fw.Delta)) (*fw.Response, error) {
		text := "Found: web-3 drifts from its role\nThe diff is in the wiki."
		if strings.Contains(req.Messages[0].Content[0].Text, "quiet") {
			text = "Found: nothing\nEvery host matches."
		}
		return &fw.Response{Content: []fw.Block{fw.Text(text)}, StopReason: fw.StopEndTurn}, nil
	}}
	a, err := fw.New(fw.WithConfig(cfg), fw.WithoutDefaults(), fw.WithAuthn(people{}), fw.WithModel(answer),
		fw.WithLedger(fw.NewMemLedger()), fw.WithBus(fw.NewMemBus()), fw.WithSurfaces(native.New()),
		fw.WithServices(duties.Start), fw.WithLogger(quiet()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		a.Close(ctx)
	})
	d := duties.Duty{
		Name: "drift", Title: "Drift check", Agent: "ops",
		When:   duties.When{Webhook: &duties.Webhook{Auth: duties.WebhookAuth{Mode: "none"}}},
		Do:     duties.Do{Ask: &duties.Ask{Message: "Compare the hosts."}},
		Limits: duties.Limits{Budget: &duties.Budget{MaxCostUSD: 0.5}},
		Notify: &duties.Notify{Result: []string{"owner", "carol"}, ResultWhen: duties.ResultFound},
	}
	if _, err := duties.Of(a).Put(as("alice"), "drift", duties.DutyBody{Duty: d}, duties.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	t.Setenv(fleet.EnvIntake, "")
	start := func(input string) string {
		t.Helper()
		r, err := a.Start(context.Background(), "", fw.WithRequest(fw.RunRequest{Agent: "ops", Input: input, Principal: "duty:drift", PrincipalKind: fw.KindWorkload,
			Metadata: map[string]string{duties.MetaDuty: "drift", fleet.MetaFound: "1", fw.MetaTitle: "Drift check"}}))
		if err != nil {
			t.Fatal(err)
		}
		waitFor(t, a, r.ID, fw.StateCompleted)
		return r.ID
	}
	found, nothing := start("Compare the hosts."), start("Compare the hosts, quiet.")
	items := func(who string) []Item {
		t.Helper()
		ans, err := For(as(who), a, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		var out []Item
		for _, it := range ans.Items {
			if it.Kind == KindResult {
				out = append(out, it)
			}
		}
		return out
	}
	for who, want := range map[string]int{"alice": 1, "carol": 1, "dave": 0} {
		got := items(who)
		if len(got) != want {
			t.Fatalf("the found results of %s = %+v, want %d", who, got, want)
		}
		if want == 1 && (got[0].Run != found || got[0].Text != "Drift check: it found something." || got[0].Duty != "drift") {
			t.Fatalf("the item of %s = %+v, not of %s", who, got[0], nothing)
		}
	}
	if err := a.React(as("alice"), found, fw.ReactionData{Reaction: fw.ReactionUp}); err != nil {
		t.Fatal(err)
	}
	if got := items("carol"); len(got) != 0 {
		t.Fatalf("the found results after Looks right = %+v", got)
	}
}
