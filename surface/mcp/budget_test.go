package mcp

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
)

func TestBudgetDeadlineRoundTrip(t *testing.T) {
	ctx := testCtx(t)
	a := manageAgent(t, newScript().model())
	cs := as(t, ctx, serveAgent(t, a, nil).URL, "alice-token")
	res := callTool(t, ctx, cs, toolAgentPut, map[string]any{
		"name": "timed", "budget": map[string]any{
			"deadline": "10m", "maxTurns": 7, "maxTokens": 1234, "maxCostUSD": 0.5,
			"maxToolCalls": 9, "maxCallsPerTurn": 2, "maxRepeats": 3,
		},
	})
	if res.IsError {
		t.Fatal(textOf(res))
	}
	want := fw.Budget{
		Deadline: 10 * time.Minute, MaxTurns: 7, MaxTokens: 1234, MaxCostUSD: 0.5,
		MaxToolCalls: 9, MaxCallsPerTurn: 2, MaxRepeats: 3,
	}
	res = callTool(t, ctx, cs, toolAgentGet, map[string]any{"name": "timed"})
	var got struct {
		Agent struct {
			Budget json.RawMessage `json:"budget"`
		} `json:"agent"`
	}
	structured(t, res, &got)
	var stored fw.Budget
	if err := json.Unmarshal(got.Agent.Budget, &stored); err != nil || stored != want {
		t.Fatalf("returned budget = %s, %v", got.Agent.Budget, err)
	}
	// The dashboard copies the returned budget into agent_put unchanged.
	res = callTool(t, ctx, cs, toolAgentPut, map[string]any{"name": "timed", "budget": got.Agent.Budget})
	if res.IsError {
		t.Fatal(textOf(res))
	}
	p, ok := a.ProfileNamed("timed")
	if !ok || p.Budget == nil || *p.Budget != want {
		t.Fatalf("saved budget = %+v", p.Budget)
	}
	for _, deadline := range []any{"later", "0s", "-1s", -1, 0, 1.5, true, nil, json.RawMessage(`9223372036854775808`)} {
		res = callTool(t, ctx, cs, toolAgentPut, map[string]any{
			"name": "timed", "budget": map[string]any{"deadline": deadline},
		})
		if !res.IsError {
			t.Errorf("invalid deadline %v was accepted", deadline)
		}
		p, _ := a.ProfileNamed("timed")
		if p.Budget == nil || *p.Budget != want {
			t.Fatalf("invalid deadline %v changed the budget to %+v", deadline, p.Budget)
		}
	}
}

func TestRunBudgetNumericDeadline(t *testing.T) {
	for _, inline := range []bool{false, true} {
		t.Run(fmt.Sprintf("inline=%v", inline), func(t *testing.T) {
			ctx := testCtx(t)
			a := newAgent(t, newScript(endTurn("ok")).model())
			cs := mustConnect(t, ctx, serveAgent(t, a, nil).URL, clientSetup{})
			budget := map[string]any{"deadline": int64(time.Minute)}
			args := map[string]any{"input": "hello", "budget": budget}
			if inline {
				delete(args, "budget")
				args["profile"] = map[string]any{"budget": budget}
			}
			res := callTool(t, ctx, cs, toolRun, args)
			if res.IsError {
				t.Fatal(textOf(res))
			}
			r := runOf(t, res)
			var q fw.RunQueuedData
			for e, err := range a.Ledger().Read(ctx, r.ID, 0) {
				if err != nil {
					t.Fatal(err)
				}
				if e.Type == fw.EvRunQueued {
					if err := e.Decode(&q); err != nil {
						t.Fatal(err)
					}
				}
			}
			got := q.Budget
			if inline && q.Profile != nil {
				got = q.Profile.Budget
			}
			if got == nil || got.Deadline != time.Minute {
				t.Fatalf("queued budget = %+v", got)
			}
		})
	}
}

func TestDurationArgumentPrecision(t *testing.T) {
	for _, tc := range []struct {
		json string
		want time.Duration
	}{
		{`{}`, 0},
		{`{"deadline":""}`, 0},
		{`{"deadline":"10m"}`, 10 * time.Minute},
		{`{"deadline":1}`, time.Nanosecond},
		{`{"deadline":9007199254740993}`, 9007199254740993},
		{`{"deadline":9223372036854775807}`, time.Duration(math.MaxInt64)},
	} {
		var args budgetArgs
		if err := json.Unmarshal([]byte(tc.json), &args); err != nil {
			t.Fatalf("decode %s: %v", tc.json, err)
		}
		budget, err := args.budget("budget")
		if err != nil || budget.Deadline != tc.want {
			t.Errorf("%s = %v, %v; want %v", tc.json, budget.Deadline, err, tc.want)
		}
	}
}

// TestCompactionRoundTrip checks that agent_put keeps the compaction block
// of a profile, and that the profile that agent_get returns goes back
// through agent_put unchanged.
func TestCompactionRoundTrip(t *testing.T) {
	ctx := testCtx(t)
	a := manageAgent(t, newScript().model())
	cs := as(t, ctx, serveAgent(t, a, nil).URL, "alice-token")
	res := callTool(t, ctx, cs, toolAgentPut, map[string]any{"name": "short", "compaction": map[string]any{"at": 4096, "model": "fake/small"}})
	if res.IsError {
		t.Fatal(textOf(res))
	}
	want := fw.CompactionConfig{At: 4096, Model: "fake/small"}
	if p, ok := a.ProfileNamed("short"); !ok || p.Compaction == nil || *p.Compaction != want {
		t.Fatalf("saved compaction = %+v", p.Compaction)
	}
	res = callTool(t, ctx, cs, toolAgentGet, map[string]any{"name": "short"})
	var got struct {
		Agent struct {
			Compaction json.RawMessage `json:"compaction"`
		} `json:"agent"`
	}
	structured(t, res, &got)
	res = callTool(t, ctx, cs, toolAgentPut, map[string]any{"name": "short", "compaction": got.Agent.Compaction})
	if res.IsError {
		t.Fatal(textOf(res))
	}
	if p, _ := a.ProfileNamed("short"); p.Compaction == nil || *p.Compaction != want {
		t.Fatalf("compaction after the round trip = %+v from %s", p.Compaction, got.Agent.Compaction)
	}
	res = callTool(t, ctx, cs, toolAgentPut, map[string]any{"name": "short", "compaction": map[string]any{"at": 100, "off": true}})
	if !res.IsError || !strings.Contains(textOf(res), "compaction.off") {
		t.Fatalf("a bad block = %s", textOf(res))
	}
}
