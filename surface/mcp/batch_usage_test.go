package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/fleet"
)

// TestBatchUsage follows a job on a front whose fleet reads the egress of a
// worker that sends only the lifecycle events (fleet.EgressTypes), with no
// fw.model.responded. The batch tool sums the usage of the two runs of the
// job from the totals of fw.run.finished, with the cost of the worker,
// because the front has no price table. A run whose events carry no
// totals, as an older kernel writes them, counts as missing, so the sum
// reads as partial.
func TestBatchUsage(t *testing.T) {
	ctx := testCtx(t)
	t.Setenv(fleet.EnvIntake, "fw.jobs")
	t.Setenv(fleet.EnvEgress, "")
	t.Setenv(fleet.EnvAtespaces, "")
	bus := fw.NewMemBus()

	model := func(r *fw.Response, name string) *fw.Response {
		r.Provider, r.Model = "fake", name
		r.Usage.CacheReadTokens = 7
		return r
	}
	script := newScript(
		model(toolUse("c1", "echo", `{"text":"a"}`), "m"),
		model(endTurn("one"), "m"),
		model(endTurn("two"), "unpriced"),
	)
	echo := fw.NewTool(fw.ToolSpec{Name: "echo", ReadOnly: true}, func(_ context.Context, in json.RawMessage) (fw.ToolResult, error) {
		return fw.TextResult(string(in)), nil
	})
	wcfg := testConfig(t)
	wcfg.Node = "box:8080"
	wcfg.Bus.Egress = "fw.events"
	wcfg.Bus.EgressTypes = fleet.EgressTypes
	worker := newAgent(t, script.model(), fw.WithConfig(wcfg), fw.WithBus(bus), fw.WithTools(echo),
		fw.WithPrices(fw.PriceTable{"fake/m": {Input: 2, Output: 10}}))

	front := newAgent(t, newScript().model(), fw.WithBus(bus), fw.WithAuthn(people))
	f, err := fleet.Open(ctx, front, fleet.Config{Name: "front", Egress: "fw.events", Interval: 40 * time.Millisecond, Expire: fleet.DefaultExpire})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	attach(front, f)
	cs := as(t, ctx, serveAgent(t, front, nil).URL, "alice-token")

	// Two runs of the job on the worker, as alice.
	alice := fw.WithPrincipal(ctx, &fw.Principal{Subject: "alice", Kind: fw.KindUser})
	job := fw.WithRequest(fw.RunRequest{Metadata: map[string]string{fw.MetaBatch: "job-1"}})
	var want fw.RunUsage
	for _, in := range []string{"one", "two"} {
		r, err := worker.Run(alice, in, job)
		if err != nil || r.State != fw.StateCompleted {
			t.Fatalf("worker run = %+v, %v", r, err)
		}
		u := r.Usage
		want.Usage = want.Usage.Add(u.Usage)
		want.Turns += u.Turns
		want.ToolCalls += u.ToolCalls
		want.CostUSD += u.CostUSD
		want.UnpricedTokens += u.UnpricedTokens
	}
	if want.ToolCalls != 1 || want.Turns != 3 || want.CostUSD <= 0 || want.UnpricedTokens != 22 || want.CacheReadTokens != 21 {
		t.Fatalf("the usage on the worker = %+v", want)
	}
	// A third run of the job, as a worker on an older kernel sends it: its
	// fw.run.finished carries no totals.
	old := fw.NewID()
	for i, ev := range []struct {
		typ  string
		data any
	}{
		{fw.EvRunQueued, fw.RunQueuedData{Agent: "default", Input: "three", Origin: fw.OriginBus, Principal: "alice", Metadata: map[string]string{fw.MetaBatch: "job-1"}}},
		{fw.EvRunStarted, fw.RunStartedData{Model: "fake/m"}},
		{fw.EvTurnStarted, fw.TurnData{Turn: 1}},
		{fw.EvRunFinished, fw.RunFinishedData{Outcome: fw.OutcomeCompleted, Output: "three"}},
	} {
		e := fw.MustEvent(old, ev.typ, ev.data)
		e.Seq, e.Node, e.Source = uint64(i+1), "box:8080", "fw://local/default/"+old
		if err := bus.Publish(ctx, "fw.events.local.default."+ev.typ, e); err != nil {
			t.Fatal(err)
		}
	}

	var b batchResult
	var text string
	deadline := time.Now().Add(5 * time.Second)
	for {
		res := callTool(t, ctx, cs, toolBatch, map[string]any{"batch": "job-1"})
		b = batchResult{}
		structured(t, res, &b)
		text = textOf(res)
		if (b.Total == 3 && b.Finished && b.Usage.Reported == 2) || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if b.Total != 3 || !b.Finished || b.Usage.RunUsage != want || b.Usage.Reported != 2 || b.Usage.Missing != 1 {
		t.Fatalf("batch = %+v, want usage %+v of 2 runs and 1 missing; text %q", b, want, text)
	}
	for _, s := range []string{
		"Usage of 2 runs: 3 turns, ",
		"1 tool call, $",
		"The cost leaves out 22 tokens without a price",
		"1 run reports no usage yet, so the sum is partial.",
		"The sum counts each run once",
	} {
		if !strings.Contains(text, s) {
			t.Fatalf("batch text lacks %q: %q", s, text)
		}
	}
}
