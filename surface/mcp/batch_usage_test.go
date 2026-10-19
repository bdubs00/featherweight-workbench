package mcp

import (
	"context"
	"encoding/json"
	"fmt"
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

// egressEvent is one event of a run on the egress stream of a worker.
type egressEvent struct {
	typ  string
	data any
}

// publishInOrder sends the events of one run of a batch on the egress
// stream, as the worker box:8080 does, with the sequence numbers from 1.
// The mem bus hands each event to a goroutine of its own, so it does not
// keep their order, as a stream of NATS does; the helper waits after each
// event until the view of the front applied it.
func publishInOrder(t *testing.T, ctx context.Context, bus fw.Bus, f *fleet.Fleet, batch, id string, evs []egressEvent) {
	t.Helper()
	for i, ev := range evs {
		e := fw.MustEvent(id, ev.typ, ev.data)
		e.Seq, e.Node, e.Source = uint64(i+1), "box:8080", "fw://local/default/"+id
		if err := bus.Publish(ctx, "fw.events.local.default."+ev.typ, e); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(5 * time.Second)
		for !applied(f, batch, id, e.Seq) {
			if time.Now().After(deadline) {
				t.Fatalf("the front did not apply event %d of %s", e.Seq, id)
			}
			time.Sleep(time.Millisecond)
		}
	}
}

// applied reports whether the view of the front holds the run of a batch
// up to seq.
func applied(f *fleet.Fleet, batch, id string, seq uint64) bool {
	b, err := f.Batch(context.Background(), batch)
	if err != nil {
		return false
	}
	for _, r := range b.Runs {
		if r.ID == id {
			return r.Seq >= seq
		}
	}
	return false
}

// lifecycleFront is a front with the batch tools whose fleet reads the
// egress stream fw.events of the bus. call runs the tool batch as alice.
func lifecycleFront(t *testing.T) (context.Context, fw.Bus, *fleet.Fleet, func(string) (batchResult, string)) {
	t.Helper()
	ctx := testCtx(t)
	t.Setenv(fleet.EnvIntake, "fw.jobs")
	t.Setenv(fleet.EnvEgress, "")
	t.Setenv(fleet.EnvAtespaces, "")
	bus := fw.NewMemBus()
	front := newAgent(t, newScript().model(), fw.WithBus(bus), fw.WithAuthn(people))
	f, err := fleet.Open(ctx, front, fleet.Config{Name: "front", Egress: "fw.events", Interval: 40 * time.Millisecond, Expire: fleet.DefaultExpire})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	attach(front, f)
	cs := as(t, ctx, serveAgent(t, front, nil).URL, "alice-token")
	return ctx, bus, f, func(batch string) (batchResult, string) {
		res := callTool(t, ctx, cs, toolBatch, map[string]any{"batch": batch})
		var b batchResult
		structured(t, res, &b)
		return b, textOf(res)
	}
}

// finishedEvents is the lifecycle of one finished run of a batch, as alice,
// with totals.
func finishedEvents(batch, input string, totals fw.RunUsage) []egressEvent {
	return []egressEvent{
		{fw.EvRunQueued, fw.RunQueuedData{Agent: "default", Input: input, Origin: fw.OriginBus, Principal: "alice", Metadata: map[string]string{fw.MetaBatch: batch}}},
		{fw.EvRunStarted, fw.RunStartedData{Model: "fake/m"}},
		{fw.EvTurnStarted, fw.TurnData{Turn: 1}},
		{fw.EvRunFinished, fw.RunFinishedData{Outcome: fw.OutcomeCompleted, Output: "ok", Totals: &totals}},
	}
}

// TestBatchCountsEveryRun follows a job of 60 runs on one worker. A record
// of the view holds the newest 50 finished runs of a member, and the tool
// batch counts every run of the job all the same: the total, the states,
// and the usage.
func TestBatchCountsEveryRun(t *testing.T) {
	ctx, bus, f, call := lifecycleFront(t)
	per := fw.RunUsage{Usage: fw.Usage{InputTokens: 100, OutputTokens: 10}, Turns: 1, CostUSD: 0.01}
	var want fw.RunUsage
	for i := 0; i < 60; i++ {
		publishInOrder(t, ctx, bus, f, "big", fw.NewID(), finishedEvents("big", fmt.Sprint("item ", i), per))
		want.Usage = want.Usage.Add(per.Usage)
		want.Turns += per.Turns
		want.CostUSD += per.CostUSD
	}
	publishInOrder(t, ctx, bus, f, "other", fw.NewID(), finishedEvents("other", "else", per))
	b, text := call("big")
	if b.Total != 60 || b.States["completed"] != 60 || !b.Finished || b.Forgotten != 0 {
		t.Fatalf("batch = %+v; text %q", b, text)
	}
	if b.Usage.RunUsage != want || b.Usage.Reported != 60 || b.Usage.Missing != 0 {
		t.Fatalf("usage = %+v, want %+v of 60 runs", b.Usage, want)
	}
	if !strings.Contains(text, "Batch big: 60 runs on 1 members.") || !strings.Contains(text, "Usage of 60 runs: 60 turns, 6600 tokens, $0.6000.") || strings.Contains(text, "partial") {
		t.Fatalf("batch text = %q", text)
	}
}
