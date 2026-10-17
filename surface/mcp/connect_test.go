package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// connectPeople authenticates alice as a person and the agent token as the
// process token, which cannot connect.
var connectPeople = tokenMap{
	"alice-token": {Subject: "alice", Name: "alice@example.com", Kind: fw.KindUser},
	"agent-token": {Subject: "token", Kind: fw.KindToken},
}

var alice = &fw.Principal{Subject: "alice", Kind: fw.KindUser}

// ticketsSource is a tool source with one tool that acts for the person of
// the run. It connects anybody whose code is "good".
type ticketsSource struct{}

func (ticketsSource) ConnectName() string { return "tickets" }

func ticketsKey(subject string) fw.GrantKey {
	return fw.GrantKey{Subject: subject, Issuer: "https://id.example", ClientID: "fw"}
}

func (ticketsSource) Connection(ctx context.Context, subject string) (fw.Connection, error) {
	c := fw.Connection{Server: "tickets", Title: "Tickets"}
	if g, _, err := fw.AgentFrom(ctx).Grants().Get(ctx, ticketsKey(subject)); err == nil {
		c.Connected, c.Account, c.Since = true, g.Account, g.Created
	}
	return c, nil
}

func (ticketsSource) BeginConnect(_ context.Context, _, redirect string) (fw.ConnectStart, error) {
	return fw.ConnectStart{Server: "tickets", URL: "https://id.example/authorize?state=s1", State: "s1", Verifier: "v", Redirect: redirect}, nil
}

func (t ticketsSource) FinishConnect(ctx context.Context, subject string, _ fw.ConnectStart, code string) (fw.Connection, error) {
	if code != "good" {
		return fw.Connection{}, errors.New("bad code")
	}
	g := fw.Grant{Subject: subject, Issuer: "https://id.example", ClientID: "fw", RefreshToken: "rt", Account: subject + "@tickets", Created: time.Now()}
	if _, err := fw.AgentFrom(ctx).Grants().Put(ctx, g, 0); err != nil {
		return fw.Connection{}, err
	}
	return t.Connection(ctx, subject)
}

func (ticketsSource) Disconnect(ctx context.Context, subject string) error {
	return fw.AgentFrom(ctx).Grants().Delete(ctx, ticketsKey(subject))
}

func (ticketsSource) Tools(context.Context) ([]fw.Tool, error) {
	return []fw.Tool{fw.NewTool(fw.ToolSpec{Name: "tickets.list", Source: "tickets", ReadOnly: true}, func(ctx context.Context, _ json.RawMessage) (fw.ToolResult, error) {
		info := fw.RunInfoFrom(ctx)
		if _, _, err := fw.AgentFrom(ctx).Grants().Get(ctx, ticketsKey(info.Principal)); err != nil {
			if err := fw.RequestConnect(ctx, fw.ConnectRequest{Server: "tickets", Title: "Tickets", Reason: fw.ConnectMissing}); err != nil {
				return fw.ErrorResult("not run: " + err.Error()), nil
			}
		}
		return fw.TextResult("tickets of " + info.Principal), nil
	})}, nil
}

// connectAgent is an agent with the tickets source, whose model calls
// tickets.list once and then ends with "done".
func connectAgent(t *testing.T) (*fw.Agent, *script) {
	t.Helper()
	s := newScript(toolUse("c1", "tickets.list", `{}`), endTurn("done"))
	return newAgent(t, s.model(),
		fw.WithAuthn(connectPeople),
		fw.WithToolSources(ticketsSource{}),
		fw.WithGrantStore(fw.NewMemGrantStore())), s
}

// connectNow connects tickets for alice, as the callback of the browser
// does.
func connectNow(t *testing.T, a *fw.Agent) {
	t.Helper()
	ctx := context.Background()
	start, err := a.BeginConnect(ctx, alice, "tickets", "https://fw.example/ui/connect/callback")
	if err != nil {
		t.Errorf("BeginConnect: %v", err)
		return
	}
	if _, err := a.FinishConnect(ctx, alice, start, "good"); err != nil {
		t.Errorf("FinishConnect: %v", err)
	}
}

func bearer(token string) http.Header { return http.Header{"Authorization": {"Bearer " + token}} }

func urlCaps() *sdk.ClientCapabilities {
	return &sdk.ClientCapabilities{Elicitation: &sdk.ElicitationCapabilities{Form: &sdk.FormElicitationCapabilities{}, URL: &sdk.URLElicitationCapabilities{}}}
}

// lastResult is the text of the latest tool result in the requests of the
// script.
func lastResult(t *testing.T, s *script) string {
	t.Helper()
	reqs := s.requests()
	msgs := reqs[len(reqs)-1].Messages
	for _, b := range msgs[len(msgs)-1].Content {
		if b.Type == fw.BlockToolResult {
			return fw.TextOf(b.Content)
		}
	}
	t.Fatalf("no tool result in the last request")
	return ""
}

func TestConnectByLegacyURLElicitation(t *testing.T) {
	ctx := testCtx(t)
	a, s := connectAgent(t)
	ts := serveAgent(t, a, nil)
	var mu sync.Mutex
	var got *sdk.ElicitParams
	completed := make(chan string, 1)
	cs := mustConnect(t, ctx, ts.URL, clientSetup{
		protocol: "2025-11-25",
		header:   bearer("alice-token"),
		opts: &sdk.ClientOptions{
			Capabilities: urlCaps(),
			ElicitationHandler: func(_ context.Context, req *sdk.ElicitRequest) (*sdk.ElicitResult, error) {
				mu.Lock()
				got = req.Params
				mu.Unlock()
				// The person opens the address and connects in the browser
				// while the client waits.
				go func() { time.Sleep(50 * time.Millisecond); connectNow(t, a) }()
				return &sdk.ElicitResult{Action: "accept"}, nil
			},
			ElicitationCompleteHandler: func(_ context.Context, req *sdk.ElicitationCompleteNotificationRequest) {
				completed <- req.Params.ElicitationID
			},
		},
	})
	res := callTool(t, ctx, cs, "run", map[string]any{"input": "list my tickets"})
	if res.IsError || textOf(res) != "done" {
		t.Fatalf("result = %q, isError %v", textOf(res), res.IsError)
	}
	r := runOf(t, res)
	mu.Lock()
	p := got
	mu.Unlock()
	if p == nil || p.Mode != "url" || p.URL != ts.URL+"/ui#connect/tickets/"+r.ID || p.ElicitationID == "" || !strings.Contains(p.Message, "Connect Tickets") {
		t.Fatalf("elicitation = %+v", p)
	}
	select {
	case id := <-completed:
		if id != p.ElicitationID {
			t.Errorf("complete for %q, want %q", id, p.ElicitationID)
		}
	case <-time.After(5 * time.Second):
		t.Errorf("no notifications/elicitation/complete")
	}
	if got := lastResult(t, s); got != "tickets of alice" {
		t.Fatalf("tool result = %q", got)
	}
	if d := decided(t, a, r.ID); d.Decision != "allow" || d.Decider != "alice" || d.Note != "connected" {
		t.Errorf("decision = %+v", d)
	}
}

func TestConnectDeclineSkips(t *testing.T) {
	ctx := testCtx(t)
	a, s := connectAgent(t)
	cs := mustConnect(t, ctx, serveAgent(t, a, nil).URL, clientSetup{
		protocol: "2025-11-25",
		header:   bearer("alice-token"),
		opts: &sdk.ClientOptions{
			Capabilities: urlCaps(),
			ElicitationHandler: func(context.Context, *sdk.ElicitRequest) (*sdk.ElicitResult, error) {
				return &sdk.ElicitResult{Action: "decline"}, nil
			},
		},
	})
	res := callTool(t, ctx, cs, "run", map[string]any{"input": "list my tickets"})
	if res.IsError || textOf(res) != "done" {
		t.Fatalf("result = %q, isError %v", textOf(res), res.IsError)
	}
	r := runOf(t, res)
	if d := decided(t, a, r.ID); d.Decision != "deny" || !strings.Contains(d.Note, "declined") {
		t.Errorf("decision = %+v", d)
	}
	if got := lastResult(t, s); !strings.Contains(got, "did not connect") {
		t.Errorf("tool result = %q", got)
	}
}

func TestConnectByInputRequest(t *testing.T) {
	ctx := testCtx(t)
	a, s := connectAgent(t)
	var hl headerLog
	ts := serveAgent(t, a, &hl)
	var mu sync.Mutex
	var got *sdk.ElicitParams
	cs := mustConnect(t, ctx, ts.URL, clientSetup{
		header: bearer("alice-token"),
		opts: &sdk.ClientOptions{
			Capabilities: urlCaps(),
			ElicitationHandler: func(_ context.Context, req *sdk.ElicitRequest) (*sdk.ElicitResult, error) {
				mu.Lock()
				got = req.Params
				mu.Unlock()
				go func() { time.Sleep(50 * time.Millisecond); connectNow(t, a) }()
				return &sdk.ElicitResult{Action: "accept"}, nil
			},
		},
	})
	res := callTool(t, ctx, cs, "run", map[string]any{"input": "list my tickets"})
	if res.IsError || textOf(res) != "done" {
		t.Fatalf("result = %q, isError %v", textOf(res), res.IsError)
	}
	r := runOf(t, res)
	mu.Lock()
	p := got
	mu.Unlock()
	if p == nil || p.Mode != "url" || p.URL != ts.URL+"/ui#connect/tickets/"+r.ID {
		t.Fatalf("input request = %+v", p)
	}
	if n := hl.count("Mcp-Method", "tools/call"); n != 2 {
		t.Errorf("tools/call requests = %d, want 2 (the call and one retry that waits)", n)
	}
	if hl.any("Mcp-Session-Id", "") {
		t.Errorf("the input_required flow opened a session")
	}
	if got := lastResult(t, s); got != "tickets of alice" {
		t.Fatalf("tool result = %q", got)
	}
}

func TestConnectByForm(t *testing.T) {
	ctx := testCtx(t)
	t.Run("legacy done", func(t *testing.T) {
		a, s := connectAgent(t)
		ts := serveAgent(t, a, nil)
		var msg string
		cs := mustConnect(t, ctx, ts.URL, clientSetup{
			protocol: "2025-11-25",
			header:   bearer("alice-token"),
			opts: &sdk.ClientOptions{
				ElicitationHandler: func(_ context.Context, req *sdk.ElicitRequest) (*sdk.ElicitResult, error) {
					msg = req.Params.Message
					if req.Params.Mode == "url" || req.Params.RequestedSchema == nil {
						t.Errorf("a form-only client got %+v", req.Params)
					}
					connectNow(t, a)
					return &sdk.ElicitResult{Action: "accept", Content: map[string]any{"choice": "done"}}, nil
				},
			},
		})
		res := callTool(t, ctx, cs, "run", map[string]any{"input": "list my tickets"})
		if res.IsError || textOf(res) != "done" {
			t.Fatalf("result = %q, isError %v", textOf(res), res.IsError)
		}
		r := runOf(t, res)
		if !strings.Contains(msg, ts.URL+"/ui#connect/tickets/"+r.ID) || !strings.Contains(msg, "skip") {
			t.Errorf("form message = %q", msg)
		}
		if got := lastResult(t, s); got != "tickets of alice" {
			t.Fatalf("tool result = %q", got)
		}
	})
	t.Run("modern skip", func(t *testing.T) {
		a, s := connectAgent(t)
		cs := mustConnect(t, ctx, serveAgent(t, a, nil).URL, clientSetup{
			header: bearer("alice-token"),
			opts: &sdk.ClientOptions{
				ElicitationHandler: func(_ context.Context, req *sdk.ElicitRequest) (*sdk.ElicitResult, error) {
					return &sdk.ElicitResult{Action: "accept", Content: map[string]any{"choice": "skip"}}, nil
				},
			},
		})
		res := callTool(t, ctx, cs, "run", map[string]any{"input": "list my tickets"})
		if res.IsError || textOf(res) != "done" {
			t.Fatalf("result = %q, isError %v", textOf(res), res.IsError)
		}
		if d := decided(t, a, runOf(t, res).ID); d.Decision != "deny" || !strings.Contains(d.Note, "skipped") {
			t.Errorf("decision = %+v", d)
		}
		if got := lastResult(t, s); !strings.Contains(got, "did not connect") {
			t.Errorf("tool result = %q", got)
		}
	})
}

func TestConnectWithoutElicitation(t *testing.T) {
	ctx := testCtx(t)
	a, _ := connectAgent(t)
	ts := serveAgent(t, a, nil)
	cs := as(t, ctx, ts.URL, "alice-token")
	res := callTool(t, ctx, cs, "run", map[string]any{"input": "list my tickets"})
	r := runOf(t, res)
	if res.IsError || r.State != fw.StateInputRequired || r.Pending == nil || r.Pending.Connect == nil {
		t.Fatalf("run = %+v, isError %v", r, res.IsError)
	}
	link := ts.URL + "/ui#connect/tickets/" + r.ID
	if text := textOf(res); !strings.Contains(text, link) || !strings.Contains(text, "decision deny") {
		t.Fatalf("text = %q, want the link %s", text, link)
	}
	// A click is not a grant.
	res = callTool(t, ctx, cs, "approve", map[string]any{"runId": r.ID, "approvalId": r.Pending.ID, "decision": "allow"})
	if !res.IsError || !strings.Contains(textOf(res), "connect Tickets to continue") || !strings.Contains(textOf(res), link) {
		t.Fatalf("approve allow = %q", textOf(res))
	}
	res = callTool(t, ctx, cs, "run_status", map[string]any{"runId": r.ID})
	if !strings.Contains(textOf(res), link) {
		t.Errorf("run_status text = %q", textOf(res))
	}
	connectNow(t, a)
	done, err := a.Wait(ctx, r.ID)
	if err != nil || done.State != fw.StateCompleted {
		t.Fatalf("run = %+v, %v", done, err)
	}
}

func TestConnectionTools(t *testing.T) {
	ctx := testCtx(t)
	a, _ := connectAgent(t)
	ts := serveAgent(t, a, nil)
	cs := as(t, ctx, ts.URL, "alice-token")
	list := func() connectionsResult {
		t.Helper()
		res := callTool(t, ctx, cs, "connections", map[string]any{})
		if res.IsError {
			t.Fatalf("connections = %q", textOf(res))
		}
		var out connectionsResult
		structured(t, res, &out)
		return out
	}
	out := list()
	if len(out.Connections) != 1 || out.Connections[0].Connected || out.Connections[0].ConnectURL != ts.URL+"/ui#connect/tickets" || out.View != viewConnections {
		t.Fatalf("connections = %+v", out)
	}
	connectNow(t, a)
	if out = list(); !out.Connections[0].Connected || out.Connections[0].Account != "alice@tickets" {
		t.Fatalf("connections after connect = %+v", out)
	}
	res := callTool(t, ctx, cs, "disconnect", map[string]any{"server": "tickets"})
	if res.IsError || !strings.Contains(textOf(res), "Disconnected Tickets") {
		t.Fatalf("disconnect = %q", textOf(res))
	}
	if out = list(); out.Connections[0].Connected {
		t.Fatalf("connections after disconnect = %+v", out)
	}
	res = callTool(t, ctx, cs, "disconnect", map[string]any{"server": "docs"})
	if !res.IsError || !strings.Contains(textOf(res), "no service of that name") {
		t.Errorf("disconnect of an unknown service = %q", textOf(res))
	}

	tok := as(t, ctx, ts.URL, "agent-token")
	for _, name := range []string{"connections", "disconnect"} {
		res := callTool(t, ctx, tok, name, map[string]any{"server": "tickets"})
		if !res.IsError || !strings.Contains(textOf(res), "only a signed-in person") {
			t.Errorf("%s as the token = %q", name, textOf(res))
		}
	}
}

func TestConnectURLUsesThePageURL(t *testing.T) {
	ctx := testCtx(t)
	cfg := testConfig(t)
	cfg.A2A.URL = "https://fw.example.com/a2a"
	s := newScript(toolUse("c1", "tickets.list", `{}`), endTurn("done"))
	a := newAgent(t, s.model(), fw.WithConfig(cfg), fw.WithAuthn(connectPeople),
		fw.WithToolSources(ticketsSource{}), fw.WithGrantStore(fw.NewMemGrantStore()))
	cs := as(t, ctx, serveAgent(t, a, nil).URL, "alice-token")
	res := callTool(t, ctx, cs, "run", map[string]any{"input": "list my tickets"})
	r := runOf(t, res)
	if want := "https://fw.example.com/ui#connect/tickets/" + r.ID; !strings.Contains(textOf(res), want) {
		t.Fatalf("text = %q, want %s", textOf(res), want)
	}
}
