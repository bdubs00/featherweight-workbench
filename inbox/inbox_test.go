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
	r1, err := a.Start(as("alice"), "", fw.WithRequest(fw.RunRequest{Agent: "triage", Input: "deploy the site", Metadata: map[string]string{duties.MetaDuty: "report", fw.MetaBatch: "bridge:B1"}}))
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
	if strings.Join(kinds, ",") != "approval,question,failure,failure,failure,paused" {
		t.Fatalf("the kinds = %v\n%+v", kinds, ans.Items)
	}
	ap, q, fail, paused := ans.Items[0], ans.Items[1], ans.Items[2], ans.Items[5]
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
	var routed Answer
	if err := json.NewDecoder(resp.Body).Decode(&routed); err != nil || resp.StatusCode != http.StatusOK || len(routed.Items) != 6 {
		t.Fatalf("GET /v1/inbox = %d %+v %v", resp.StatusCode, routed, err)
	}
	resp.Body.Close()

	cs := connect(t, a, "alice")
	res, err := cs.CallTool(context.Background(), &sdk.CallToolParams{Name: ToolInbox, Arguments: map[string]any{}})
	if err != nil || res.IsError {
		t.Fatalf("the tool = %+v, %v", res, err)
	}
	if txt := res.Content[0].(*sdk.TextContent).Text; !strings.HasPrefix(txt, "6 items need you:\n- approval: ") || !strings.Contains(txt, "(run "+r1.ID+")") {
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
