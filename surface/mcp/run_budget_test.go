package mcp

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/surface/native"
	"github.com/bdubs00/featherweight-workbench/fleet"
)

// lookTool is a read-only tool that a run calls without an approval.
func lookTool() fw.Tool {
	return fw.NewTool(fw.ToolSpec{Name: "look", ReadOnly: true}, func(context.Context, json.RawMessage) (fw.ToolResult, error) {
		return fw.TextResult("seen"), nil
	})
}

// TestSendBudgetAndRunBudget continues a run that failed at its limit:
// run_budget changes the budget of the run, and send with a budget changes
// it with a follow-up. Only an operator of the run changes its budget.
func TestSendBudgetAndRunBudget(t *testing.T) {
	ctx := testCtx(t)
	a := manageAgent(t, newScript(toolUse("c1", "look", `{}`), endTurn("the report"), endTurn("done")).model(), fw.WithTools(lookTool()))
	srv := serveAgent(t, a, nil)
	cs := as(t, ctx, srv.URL, "alice-token")
	res := callTool(t, ctx, cs, toolRun, map[string]any{"agent": "reviewer", "input": "work", "budget": map[string]any{"maxTurns": 1}})
	r := runOf(t, res)
	if !res.IsError || r.State != fw.StateFailed || r.Output != "the report" || r.Error != "budget exceeded: maxTurns" {
		t.Fatalf("run = %+v", r)
	}

	if s := toolSchema(t, ctx, cs, toolSend); !strings.Contains(s, `"budget"`) || !strings.Contains(s, `"maxTurns"`) {
		t.Errorf("send schema lacks the budget: %s", s)
	}
	bob := as(t, ctx, srv.URL, "bob-token")
	if res := callTool(t, ctx, bob, toolRunBudget, map[string]any{"runId": r.ID, "budget": map[string]any{"maxTurns": 5}}); !res.IsError {
		t.Fatalf("a viewer changed the budget: %s", textOf(res))
	}
	for _, args := range []map[string]any{
		{"runId": r.ID},
		{"runId": r.ID, "budget": map[string]any{}},
		{"runId": r.ID, "budget": map[string]any{"deadline": "soon"}},
		{"runId": r.ID, "budget": map[string]any{"maxTurns": -1}},
		{"runId": "nope", "budget": map[string]any{"maxTurns": 5}},
	} {
		if res := callTool(t, ctx, cs, toolRunBudget, args); !res.IsError {
			t.Errorf("run_budget %v passed: %s", args, textOf(res))
		}
	}

	res = callTool(t, ctx, cs, toolRunBudget, map[string]any{"runId": r.ID, "budget": map[string]any{"maxTurns": 3, "deadline": "1h"}})
	var out budgetAnswer
	structured(t, res, &out)
	if res.IsError || out.Budget.MaxTurns != 3 || out.Budget.Deadline != time.Hour || out.Run == nil || out.Run.State != fw.StateFailed ||
		!strings.Contains(textOf(res), "maxTurns=3") || !strings.Contains(textOf(res), "send a follow-up") {
		t.Fatalf("run_budget = %+v: %s", out, textOf(res))
	}

	if res := callTool(t, ctx, cs, toolSend, map[string]any{"runId": r.ID, "content": "x", "budget": map[string]any{"maxTurns": -1}}); !res.IsError {
		t.Fatalf("send with a negative limit passed: %s", textOf(res))
	}
	if res := callTool(t, ctx, cs, toolSend, map[string]any{"runId": r.ID, "content": "go on", "budget": map[string]any{"maxTurns": 4}}); res.IsError {
		t.Fatalf("send = %s", textOf(res))
	}
	done, err := a.Wait(ctx, r.ID)
	if err != nil || done.State != fw.StateCompleted || done.Output != "done" {
		t.Fatalf("run after send = %+v, %v", done, err)
	}
	b, err := a.RunBudget(ctx, r.ID)
	if err != nil || b.MaxTurns != 4 || b.Deadline != time.Hour {
		t.Fatalf("budget = %+v, %v", b, err)
	}
	var origins []string
	for e, err := range a.Ledger().Read(ctx, r.ID, 0) {
		if err != nil {
			t.Fatal(err)
		}
		if e.Type == fw.EvBudgetChanged {
			var d fw.BudgetChangedData
			_ = e.Decode(&d)
			origins = append(origins, d.Origin+":"+d.By)
		}
	}
	if strings.Join(origins, ",") != "mcp:alice,mcp:alice" {
		t.Fatalf("changes = %v", origins)
	}
}

// TestRunBudgetReachesTheOwner forwards run_budget and send with a budget
// to the fleet member that owns the run.
func TestRunBudgetReachesTheOwner(t *testing.T) {
	ctx := testCtx(t)
	owner := manageAgent(t, newScript(toolUse("c1", "look", `{}`), endTurn("the report"), endTurn("done")).model(),
		fw.WithTools(lookTool()), fw.WithSurfaces(native.New()))
	h, err := owner.Handler()
	if err != nil {
		t.Fatal(err)
	}
	ownerSrv := httptest.NewServer(h)
	t.Cleanup(ownerSrv.Close)
	ownerCS := as(t, ctx, serveAgent(t, owner, nil).URL, "alice-token")
	remote := runOf(t, callTool(t, ctx, ownerCS, toolRun, map[string]any{"agent": "reviewer", "input": "work", "budget": map[string]any{"maxTurns": 1}}))
	if remote.State != fw.StateFailed {
		t.Fatalf("owner run = %+v", remote)
	}

	fake := &fakeFleet{name: "view", owner: ownerSrv.URL, members: []fleet.Member{
		{Name: "view", Self: true, State: fleet.MemberLive, AccessMode: fw.AccessModeRules},
		{Name: "owner", State: fleet.MemberLive, CanForward: true, AccessMode: fw.AccessModeRules, Agents: []fleet.RecordAgent{{Name: "reviewer", Role: "owner"}}},
	}}
	fake.set(&remote)
	view := attach(manageAgent(t, newScript().model()), fake)
	cs := as(t, ctx, serveAgent(t, view, nil).URL, "alice-token")

	res := callTool(t, ctx, cs, toolRunBudget, map[string]any{"runId": remote.ID, "budget": map[string]any{"maxTurns": 3}})
	var out budgetAnswer
	structured(t, res, &out)
	if res.IsError || out.Budget.MaxTurns != 3 || out.Run == nil || out.Run.Member != "owner" {
		t.Fatalf("remote run_budget = %+v: %s", out, textOf(res))
	}
	if b, err := owner.RunBudget(ctx, remote.ID); err != nil || b.MaxTurns != 3 {
		t.Fatalf("owner budget = %+v, %v", b, err)
	}
	if res := callTool(t, ctx, cs, toolSend, map[string]any{"runId": remote.ID, "content": "go on", "budget": map[string]any{"maxTurns": 2, "deadline": "30m"}}); res.IsError || runOf(t, res).Member != "owner" {
		t.Fatalf("remote send = %s", textOf(res))
	}
	done, err := owner.WaitFor(ctx, remote.ID, func(r *fw.Run) bool { return r.State.Terminal() })
	if err != nil || done.State != fw.StateCompleted || done.Output != "done" {
		t.Fatalf("owner run after send = %+v, %v", done, err)
	}
	if b, err := owner.RunBudget(ctx, remote.ID); err != nil || b.MaxTurns != 2 || b.Deadline != 30*time.Minute {
		t.Fatalf("owner budget = %+v, %v", b, err)
	}
	bob := as(t, ctx, serveAgent(t, view, nil).URL, "bob-token")
	if res := callTool(t, ctx, bob, toolRunBudget, map[string]any{"runId": remote.ID, "budget": map[string]any{"maxTurns": 9}}); !res.IsError || !strings.Contains(textOf(res), "refused") {
		t.Fatalf("bob run_budget = %s", textOf(res))
	}
}
