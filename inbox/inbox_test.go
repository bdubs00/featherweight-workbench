package inbox

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/surface/native"
	"github.com/bdubs00/featherweight-workbench/bridges"
	"github.com/bdubs00/featherweight-workbench/duties"
	"github.com/bdubs00/featherweight-workbench/fleet"
	"github.com/bdubs00/featherweight-workbench/knowhow"
	"github.com/bdubs00/featherweight-workbench/surface/mcp"
	"github.com/bdubs00/featherweight-workbench/work"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// oldWords matches the words that a person no longer reads: the page says
// channel, agent, and job.
var oldWords = regexp.MustCompile(`(?i)\b(bridges?|seats?|seated|workers?|duty|duties|teams?)\b`)

// saysChannelAndJob fails the test for an item whose text says bridge,
// seat, worker, duty, or team.
func saysChannelAndJob(t *testing.T, items []Item) {
	t.Helper()
	for _, it := range items {
		if w := oldWords.FindString(it.Text); w != "" {
			t.Errorf("the text of an item of the kind %s says %q: %q", it.Kind, w, it.Text)
		}
	}
}

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
	// A run of the duty acts as the duty, not as alice. Its card is the
	// start of the duty, which alice owns.
	r1, err := a.Start(context.Background(), "", fw.WithRequest(fw.RunRequest{Agent: "triage", Input: "deploy the site", Principal: "duty:report", PrincipalKind: fw.KindWorkload,
		Metadata: map[string]string{duties.MetaDuty: "report", duties.MetaSlot: time.Now().UTC().Format(time.RFC3339Nano)}}))
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
	if ap.Run != r1.ID || ap.Duty != "report" || ap.Agent != "triage" || ap.Card == "" || strings.Join(ap.Actions, " ") != "allow deny" || !strings.Contains(ap.Text, "the agent triage asks to run deploy. Rule: ask destructive.") || ap.Since.IsZero() {
		t.Fatalf("the approval = %+v", ap)
	}
	if q.Run != r2.ID || !strings.Contains(q.Text, "Which branch do I use?") {
		t.Fatalf("the question = %+v", q)
	}
	if fail.Duty != "report" || !strings.Contains(fail.Text, "The queue did not take the run") {
		t.Fatalf("the failure = %+v", fail)
	}
	if paused.Duty != "report" || !strings.Contains(paused.Text, "The job report paused itself: 3 runs failed in a row") {
		t.Fatalf("the paused duty = %+v", paused)
	}
	saysChannelAndJob(t, ans.Items)

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
	// The one count comes from the lanes of the board: the result waits in
	// To check, and the badge and Activity say 1 (plan A7).
	if ans.Waiting == nil || *ans.Waiting != (work.Waiting{Count: 1, ToCheck: 1}) {
		t.Fatalf("the one count of alice = %+v", ans.Waiting)
	}
	if err := a.React(as("alice"), mine, fw.ReactionData{Reaction: fw.ReactionUp}); err != nil {
		t.Fatal(err)
	}
	if ans, err := For(as("alice"), a, time.Now()); err != nil || len(ans.Items) != 0 || ans.Waiting == nil || ans.Waiting.Count != 0 {
		t.Fatalf("the inbox after a thumb up = %+v, %+v, %v", ans.Items, ans.Waiting, err)
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
		Metadata: map[string]string{duties.MetaDuty: "summary", duties.MetaSlot: time.Now().UTC().Format(time.RFC3339Nano), fleet.MetaReview: "1"}}))
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
			Metadata: map[string]string{duties.MetaDuty: "drift", duties.MetaSlot: time.Now().UTC().Format(time.RFC3339Nano), fleet.MetaFound: "1", fw.MetaTitle: "Drift check"}}))
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

// A question of a seat is an item, also when no worker holds its run, gone
// in the channel or lost in the archive: an answer seats the member again.
// A decision of such a seat is no item, and an approval that an item of
// its run holds already is no second item.
func TestSeatItems(t *testing.T) {
	waiting := []bridges.Waiting{
		{Bridge: "b1", Title: "Exporter", Handle: "writer", Run: "r1", Question: 4, Text: "Which port?"},
		{Bridge: "b1", Title: "Exporter", Handle: "gone", Run: "r2", Question: 5, Text: "Which host?", Gone: true},
		{Bridge: "b1", Title: "Exporter", Handle: "lost", Run: "r3", Approval: "ap3"},
		{Bridge: "b1", Title: "Exporter", Handle: "asks", Run: "r4", Approval: "ap4"},
		{Bridge: "b1", Title: "Exporter", Handle: "twice", Run: "r5", Approval: "ap5"},
	}
	seen := map[string]bool{"r5": true}
	got := seatItems(waiting, seen, map[string]bool{"r3": true})
	saysChannelAndJob(t, got)
	if len(got) != 3 || got[0].Kind != KindQuestion || got[0].Text != "writer asks in Exporter: Which port?" || got[0].Question != 4 || got[0].Handle != "writer" ||
		got[1].Question != 5 || got[2].Kind != KindApproval || got[2].Approval != "ap4" || !seen["r4"] {
		t.Fatalf("the items of the seats = %+v", got)
	}
}

// The items of the work that needs the caller are the cards that the count
// counts, one item per card: a team whose agents finished and whose next
// move is the caller's is an item to check, a result of a run whose
// worker is gone is an item from its card, and a decision keeps the item
// of its record. A team of another person, a team that works, and an item
// of a record whose card does not wait for the caller are no item, so the
// items and the count always agree. A note that waits under ask first is
// an item that asks the person to keep it (plan A17).
func TestTeamAndNoteItems(t *testing.T) {
	at := time.Date(2026, 10, 28, 9, 0, 0, 0, time.UTC)
	board := work.Answer{Cards: []work.Card{
		{ID: "run:R0", Kind: work.KindConversation, Title: "Deploy", Stage: work.StageNeedsYou, Next: work.NextYou, Since: at, Summary: "Asks before it runs a shell command.", Links: work.Links{Run: "R0", Approval: "ap0", Runs: []string{"R0"}}},
		{ID: "bridge:B1", Kind: work.KindBridge, Title: "Release notes", Stage: work.StageReview, Next: work.NextYou, Since: at, Agents: []string{"writer", "reviewer"}, Summary: "The notes are in the wiki.", CostUSD: 0.12, Links: work.Links{Bridge: "B1"}},
		{ID: "bridge:B2", Kind: work.KindBridge, Title: "Theirs", Stage: work.StageReview, Next: work.NextPerson, Links: work.Links{Bridge: "B2"}},
		{ID: "bridge:B3", Kind: work.KindBridge, Title: "Busy", Stage: work.StageWorking, Next: work.NextAgent, Links: work.Links{Bridge: "B3"}},
		{ID: "duty:found@x", Kind: work.KindDuty, Title: "Found check", Stage: work.StageReview, Next: work.NextYou, Since: at, Reason: "Found: zebra", Summary: "zebra", Agents: []string{"pooled"}, Links: work.Links{Run: "R1", Duty: "found", Runs: []string{"R1"}}},
	}, Waiting: work.Waiting{NeedsYou: 1, ToCheck: 2}}
	records := []Item{
		{Kind: KindApproval, Run: "R0", Approval: "ap0", Agent: "ops", Text: "Deploy: the agent ops asks to run bash."},
		{Kind: KindQuestion, Bridge: "B2", Run: "R9", Question: 3, Text: "writer asks in Theirs: Which port?"},
		{Kind: KindFailure, Duty: "nightly", Text: "A run of the job nightly failed."},
	}
	got := fromBoard(board, records)
	saysChannelAndJob(t, got)
	if len(got) != 4 || got[0].Card != "run:R0" || got[0].Approval != "ap0" || got[0].Agent != "ops" || strings.Join(got[0].Actions, " ") != "allow deny" {
		t.Fatalf("the decision = %+v", got)
	}
	if got[1].Kind != KindResult || got[1].Bridge != "B1" || got[1].Agent != "writer" || got[1].Channel != "Release notes" || got[1].Summary != "The notes are in the wiki." || got[1].CostUSD != 0.12 ||
		strings.Join(got[1].Actions, " ") != "looks-right needs-work" {
		t.Fatalf("the team = %+v", got[1])
	}
	if got[2].Kind != KindResult || got[2].Run != "R1" || got[2].Duty != "found" || got[2].Card != "duty:found@x" || got[2].Text != "Found check: found: zebra" || got[2].Summary != "zebra" {
		t.Fatalf("the result of a duty = %+v", got[2])
	}
	if got[3].Kind != KindFailure {
		t.Fatalf("the failure = %+v", got[3])
	}
	if n := len(got) - 1; n != board.Waiting.NeedsYou+board.Waiting.ToCheck {
		t.Fatalf("%d items, and the count is %+v", n, board.Waiting)
	}
	for w, want := range map[string]string{
		knowhow.WhatLearned: "triage wrote a note, ports.md. Keep it?",
		knowhow.WhatUpdated: "triage changed its note ports.md. Keep the change?",
		knowhow.WhatPutAway: "triage wants to put its note ports.md away. Put it away?",
	} {
		it := noteItem(knowhow.Waiting{ID: "triage.01", At: at, Agent: "triage", What: w, Run: "R9", Subject: knowhow.Subject{Note: "ports.md"}})
		if it.Kind != KindNote || it.Note != "triage.01" || it.Run != "R9" || it.Agent != "triage" || it.Text != want || !it.Since.Equal(at) {
			t.Errorf("the item of a note that waits (%s) = %+v", w, it)
		}
	}
}
