package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/fleet"
)

// orderedBus delivers each event to the subscribers of its subject before
// Publish returns, in the order of the calls, as NATS keeps the order of
// one publisher. The mem bus hands each event to a goroutine of its own,
// so the order in which a front sees the events of a run is up to the
// scheduler. Consume goes to the mem bus.
type orderedBus struct {
	*fw.MemBus
	mu   sync.Mutex
	subs []*orderedSub
}

type orderedSub struct {
	subject string
	h       func(fw.Event) error
	stopped atomic.Bool
}

func newOrderedBus() *orderedBus { return &orderedBus{MemBus: fw.NewMemBus()} }

// Subscribe implements fw.Bus.
func (b *orderedBus) Subscribe(_ context.Context, subject string, h func(fw.Event) error) (func(), error) {
	s := &orderedSub{subject: subject, h: h}
	b.mu.Lock()
	b.subs = append(b.subs, s)
	b.mu.Unlock()
	return func() { s.stopped.Store(true) }, nil
}

// Publish implements fw.Bus.
func (b *orderedBus) Publish(ctx context.Context, subject string, e fw.Event) error {
	b.mu.Lock()
	subs := append([]*orderedSub(nil), b.subs...)
	b.mu.Unlock()
	for _, s := range subs {
		if !s.stopped.Load() && fw.SubjectMatch(s.subject, subject) {
			_ = s.h(e)
		}
	}
	return b.MemBus.Publish(ctx, subject, e)
}

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
	bus := newOrderedBus()

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
		// The egress of the worker publishes in the background.
		waitApplied(t, f, "job-1", r.ID, r.Seq)
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
	publishInOrder(t, ctx, bus, f, "job-1", fw.NewID(), []egressEvent{
		{fw.EvRunQueued, fw.RunQueuedData{Agent: "default", Input: "three", Origin: fw.OriginBus, Principal: "alice", Metadata: map[string]string{fw.MetaBatch: "job-1"}}},
		{fw.EvRunStarted, fw.RunStartedData{Model: "fake/m"}},
		{fw.EvTurnStarted, fw.TurnData{Turn: 1}},
		{fw.EvRunFinished, fw.RunFinishedData{Outcome: fw.OutcomeCompleted, Output: "three"}},
	})

	res := callTool(t, ctx, cs, toolBatch, map[string]any{"batch": "job-1"})
	var b batchResult
	structured(t, res, &b)
	text := textOf(res)
	if b.Total != 3 || !b.Finished || b.Usage.RunUsage != want || b.Usage.Reported != 2 || b.Usage.Final != 2 || b.Usage.Missing != 1 || !b.Usage.Partial {
		t.Fatalf("batch = %+v, want usage %+v of 2 runs and 1 missing; text %q", b, want, text)
	}
	for _, s := range []string{
		"Usage of 2 runs: 3 turns, ",
		"1 tool call, $",
		"The cost leaves out 22 tokens without a price",
		"1 run reports no usage yet. The sum is partial.",
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
// stream, as the worker box:8080 does, with the sequence numbers from 1,
// and waits after each event until the view of the front applied it, so
// the events arrive in order on any bus.
func publishInOrder(t *testing.T, ctx context.Context, bus fw.Bus, f *fleet.Fleet, batch, id string, evs []egressEvent) {
	t.Helper()
	for i, ev := range evs {
		e := fw.MustEvent(id, ev.typ, ev.data)
		e.Seq, e.Node, e.Source = uint64(i+1), "box:8080", "fw://local/default/"+id
		if err := bus.Publish(ctx, "fw.events.local.default."+ev.typ, e); err != nil {
			t.Fatal(err)
		}
		waitApplied(t, f, batch, id, e.Seq)
	}
}

// waitApplied waits until the view of the front holds the run of a batch
// up to seq, for as long as the test may run.
func waitApplied(t *testing.T, f *fleet.Fleet, batch, id string, seq uint64) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !applied(f, batch, id, seq) {
		if time.Now().After(deadline) {
			t.Fatalf("the front did not apply event %d of %s", seq, id)
		}
		time.Sleep(time.Millisecond)
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
	bus := newOrderedBus()
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
	if b.Usage.RunUsage != want || b.Usage.Reported != 60 || b.Usage.Final != 60 || b.Usage.Missing != 0 || b.Usage.Partial {
		t.Fatalf("usage = %+v, want %+v of 60 final runs", b.Usage, want)
	}
	if !strings.Contains(text, "Batch big: 60 runs on 1 members.") || !strings.Contains(text, "Usage of 60 runs: 60 turns, 6600 tokens, $0.6000.") || strings.Contains(text, "partial") {
		t.Fatalf("batch text = %q", text)
	}
}

// TestBatchUsageIsPartial follows a job of three runs on a worker that
// sends only the lifecycle events. One run waits for an approval, and the
// stream carried only the tokens of its title call: the view does not know
// its usage. One run finished, took a follow-up, and works again: its
// totals date from its first end, so its usage is not final. One run
// finished. The sum holds the two runs with totals and reads as partial.
func TestBatchUsageIsPartial(t *testing.T) {
	ctx, bus, f, call := lifecycleFront(t)
	title := fw.Usage{InputTokens: 60, OutputTokens: 6}
	publishInOrder(t, ctx, bus, f, "j", fw.NewID(), []egressEvent{
		{fw.EvRunQueued, fw.RunQueuedData{Agent: "default", Input: "waits", Origin: fw.OriginBus, Principal: "alice", Metadata: map[string]string{fw.MetaBatch: "j"}}},
		{fw.EvRunStarted, fw.RunStartedData{Model: "fake/m"}},
		{fw.EvRunAnnotated, fw.RunAnnotatedData{Metadata: map[string]string{fw.MetaTitle: "T"}, Usage: &title, Provider: "fake", Model: "titles"}},
		{fw.EvTurnStarted, fw.TurnData{Turn: 1}},
		{fw.EvApprovalRequested, fw.ApprovalRequestedData{ApprovalID: "ap1", Tool: "shell", CallID: "c1", Reason: "needs ok"}},
	})
	first := fw.RunUsage{Usage: fw.Usage{InputTokens: 100, OutputTokens: 10}, Turns: 1}
	again := append(finishedEvents("j", "again", first),
		egressEvent{fw.EvMessageReceived, fw.MessageReceivedData{Role: fw.RoleUser, Content: []fw.Block{fw.Text("more")}, Origin: fw.OriginHTTP}},
		egressEvent{fw.EvTurnStarted, fw.TurnData{Turn: 2}},
	)
	publishInOrder(t, ctx, bus, f, "j", fw.NewID(), again)
	done := fw.RunUsage{Usage: fw.Usage{InputTokens: 300, OutputTokens: 30}, Turns: 2}
	publishInOrder(t, ctx, bus, f, "j", fw.NewID(), finishedEvents("j", "done", done))

	b, text := call("j")
	want := fw.RunUsage{Usage: first.Usage.Add(done.Usage), Turns: first.Turns + 1 + done.Turns}
	if b.Total != 3 || b.Usage.RunUsage != want || b.Usage.Reported != 2 || b.Usage.Final != 1 || b.Usage.Missing != 1 || !b.Usage.Partial {
		t.Fatalf("usage = %+v, want %+v of 2 runs, 1 final, 1 missing; text %q", b.Usage, want, text)
	}
	for _, s := range []string{
		"Usage of 2 runs: 4 turns, 440 tokens.",
		"The usage of 1 run is not final yet, because the run is not finished.",
		"1 run reports no usage yet.",
		"The sum is partial.",
	} {
		if !strings.Contains(text, s) {
			t.Fatalf("batch text lacks %q: %q", s, text)
		}
	}
}

// TestUsageOf tells for each kind of run whether the view knows its usage
// and whether that usage is final.
func TestUsageOf(t *testing.T) {
	tokens := fw.RunUsage{Usage: fw.Usage{InputTokens: 60, OutputTokens: 6}}
	for _, tc := range []struct {
		name         string
		run          fw.Run
		known, final bool
	}{
		{"a run of this process that works", fw.Run{State: fw.StateRunning, Usage: tokens}, true, false},
		{"a finished run of this process from an older ledger", fw.Run{State: fw.StateCompleted, Usage: tokens}, true, true},
		{"a run of a worker with the tokens of a title call", fw.Run{Member: "w", State: fw.StateInputRequired, Usage: tokens}, false, false},
		{"a run of a worker that works after its totals", fw.Run{Member: "w", State: fw.StateRunning, Usage: tokens, UsageSeq: 9}, true, false},
		{"a suspended run of a worker", fw.Run{Member: "w", State: fw.StateSuspended, Usage: tokens, UsageSeq: 9}, true, false},
		{"a finished run of a worker with totals", fw.Run{Member: "w", State: fw.StateFailed, UsageSeq: 9}, true, true},
		{"a finished run of an older kernel on a worker", fw.Run{Member: "w", State: fw.StateCompleted, Usage: tokens}, false, false},
	} {
		if known, final := usageOf(&tc.run); known != tc.known || final != tc.final {
			t.Errorf("%s: known %v, final %v; want %v, %v", tc.name, known, final, tc.known, tc.final)
		}
	}
}

// TestBatchBranchWithoutWork forks a finished run of a job on a worker of
// the lifecycle stream without a message. The branch stays finished and
// does no work, so the job reads as final, with the usage of the source
// alone, and not as partial for good.
func TestBatchBranchWithoutWork(t *testing.T) {
	ctx := testCtx(t)
	t.Setenv(fleet.EnvIntake, "fw.jobs")
	t.Setenv(fleet.EnvEgress, "")
	t.Setenv(fleet.EnvAtespaces, "")
	bus := newOrderedBus()
	wcfg := testConfig(t)
	wcfg.Node = "box:8080"
	wcfg.Bus.Egress = "fw.events"
	wcfg.Bus.EgressTypes = fleet.EgressTypes
	worker := newAgent(t, newScript(endTurn("one")).model(), fw.WithConfig(wcfg), fw.WithBus(bus))
	front := newAgent(t, newScript().model(), fw.WithBus(bus), fw.WithAuthn(people))
	f, err := fleet.Open(ctx, front, fleet.Config{Name: "front", Egress: "fw.events", Interval: 40 * time.Millisecond, Expire: fleet.DefaultExpire})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	attach(front, f)
	cs := as(t, ctx, serveAgent(t, front, nil).URL, "alice-token")

	alice := fw.WithPrincipal(ctx, &fw.Principal{Subject: "alice", Kind: fw.KindUser})
	src, err := worker.Run(alice, "one", fw.WithRequest(fw.RunRequest{Metadata: map[string]string{fw.MetaBatch: "fork-job"}}))
	if err != nil || src.State != fw.StateCompleted {
		t.Fatalf("src = %+v, %v", src, err)
	}
	br, err := worker.Fork(alice, src.ID, fw.ForkRequest{})
	if err != nil || br.State != fw.StateCompleted {
		t.Fatalf("branch = %+v, %v", br, err)
	}
	waitApplied(t, f, "fork-job", src.ID, src.Seq)
	waitApplied(t, f, "fork-job", br.ID, br.Seq)

	res := callTool(t, ctx, cs, toolBatch, map[string]any{"batch": "fork-job"})
	var b batchResult
	structured(t, res, &b)
	if b.Total != 2 || !b.Finished || b.Usage.Reported != 2 || b.Usage.Final != 2 || b.Usage.Missing != 0 || b.Usage.Partial || b.Usage.RunUsage != src.Usage {
		t.Fatalf("batch = %+v, want the usage %+v of the source, final; text %q", b, src.Usage, textOf(res))
	}
}
