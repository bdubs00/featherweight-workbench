package dashboard

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/surface/view"
)

// connectTool is a tool that waits for the person to connect Tickets on
// every call.
func connectTool() fw.Tool {
	return fw.NewTool(fw.ToolSpec{Name: "tickets.list", Title: "List tickets", Source: "tickets"}, func(ctx context.Context, _ json.RawMessage) (fw.ToolResult, error) {
		if err := fw.RequestConnect(ctx, fw.ConnectRequest{Server: "tickets", Title: "Tickets", Reason: fw.ConnectMissing}); err != nil {
			return fw.ErrorResult("The person chose not to connect Tickets, so this call did not run."), nil
		}
		return fw.TextResult("3 tickets"), nil
	})
}

// A run that waits for a connection shows Connect, never Allow, and a skip
// ends the call.
func TestResultConnect(t *testing.T) {
	ctx := fw.WithPrincipal(context.Background(), &fw.Principal{Subject: "ada", Kind: fw.KindUser})
	a := newAgent(t, fw.WithModel(scripted(calls("Let me look.", use("c1", "tickets.list", `{}`)), answer("No access."))), fw.WithTools(connectTool()))
	r, err := a.Run(ctx, "List my tickets")
	if err != nil || r.State != fw.StateInputRequired || r.Pending.Connect == nil {
		t.Fatalf("run = %+v, %v", r, err)
	}
	res := mustResult(t, ctx, a, r.ID)
	p := res.Pending
	msg := fw.ConnectRequest{Title: "Tickets"}.Message()
	if p == nil || p.Connect == nil || p.Connect.Server != "tickets" || p.Label != "Connect Tickets" || p.Question || p.Effect != "" || p.Reason != msg || p.Schema != nil {
		t.Fatalf("pending = %+v", p)
	}
	if got := kinds(res.Next); got != "connect=Connect Tickets" {
		t.Errorf("next = %s", got)
	}
	text := res.String()
	for _, want := range []string{"Waiting for you to connect Tickets (approval " + p.ID + "): " + msg, "Next: Connect Tickets."} {
		if !strings.Contains(text, want) {
			t.Errorf("text lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "Waiting for your decision") {
		t.Errorf("a connect reads as a permission:\n%s", text)
	}

	// The home names the service that the run waits for.
	h, err := HomeOf(ctx, a, []*fw.Run{r}, time.Now())
	if err != nil || len(h.NeedsYou) != 1 || h.Glance.Waiting != 1 {
		t.Fatalf("home = %+v, %v", h, err)
	}
	if s := h.String(); !strings.Contains(s, "run "+r.ID+"): "+msg) {
		t.Errorf("home text lacks the connect:\n%s", s)
	}

	// A skip is a deny.
	if err := a.Decide(ctx, r.ID, fw.ApprovalDecision{ApprovalID: p.ID, Decision: "deny"}); err != nil {
		t.Fatal(err)
	}
	if r, err = a.Wait(ctx, r.ID); err != nil || r.State != fw.StateCompleted {
		t.Fatalf("run = %+v, %v", r, err)
	}
	events, err := readAll(ctx, a, r.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	var ask, decided *Item
	items := Transcript(events)
	for i := range items {
		switch items[i].Kind {
		case ItemAsk:
			ask = &items[i]
		case ItemDecided:
			decided = &items[i]
		}
	}
	if ask == nil || ask.Connect == nil || ask.Connect.Title != "Tickets" || ask.CallID != "c1" || ask.Detail != msg {
		t.Fatalf("ask = %+v", ask)
	}
	if decided == nil || decided.Connect == nil || decided.Text != "deny" || decided.Who != "ada" {
		t.Fatalf("decided = %+v", decided)
	}
}

// A decision without its ask in the same page has no Connect, and an
// expired grant says so in the event line.
func TestTranscriptConnectLines(t *testing.T) {
	ask := fw.MustEvent("r1", fw.EvApprovalRequested, fw.ApprovalRequestedData{ApprovalID: "ap1", Tool: "tickets.list", CallID: "c1",
		Connect: &fw.ConnectRequest{Server: "tickets", Reason: fw.ConnectExpired}})
	dec := fw.MustEvent("r1", fw.EvApprovalDecided, fw.ApprovalDecidedData{ApprovalID: "ap1", Decision: "allow", Decider: "ada", Note: "connected"})
	items := Transcript([]fw.Event{dec})
	if len(items) != 1 || items[0].Connect != nil {
		t.Fatalf("items = %+v", items)
	}
	items = Transcript([]fw.Event{ask, dec})
	if len(items) != 2 || items[0].Detail != (fw.ConnectRequest{Server: "tickets", Reason: fw.ConnectExpired}).Message() || items[1].Connect == nil {
		t.Fatalf("items = %+v", items)
	}
	if got := view.Line(ask).Line; got != "waiting for the person to sign in to tickets again" {
		t.Errorf("line = %q", got)
	}
	ask = fw.MustEvent("r1", fw.EvApprovalRequested, fw.ApprovalRequestedData{ApprovalID: "ap2", Connect: &fw.ConnectRequest{Server: "tickets", Title: "Tickets"}})
	if got := view.Line(ask).Line; got != "waiting for the person to connect Tickets" {
		t.Errorf("line = %q", got)
	}
}
