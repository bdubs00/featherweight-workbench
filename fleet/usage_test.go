package fleet

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// echoTool returns its input.
func echoTool() fw.Tool {
	return fw.NewTool(fw.ToolSpec{Name: "echo", ReadOnly: true}, func(_ context.Context, in json.RawMessage) (fw.ToolResult, error) {
		return fw.TextResult(string(in)), nil
	})
}

// toolModel calls echo once for each message of the person and then
// answers. Every response reports a usage with cache tokens under the
// model fake/m.
func toolModel() fw.Model {
	return fw.ModelFunc{StreamFn: func(_ context.Context, req *fw.Request, _ func(fw.Delta)) (*fw.Response, error) {
		usage := fw.Usage{InputTokens: 100, OutputTokens: 20, CacheReadTokens: 50, CacheWriteTokens: 10}
		last := req.Messages[len(req.Messages)-1]
		if len(last.Content) > 0 && last.Content[0].Type == fw.BlockToolResult {
			return &fw.Response{Content: []fw.Block{fw.Text("done")}, StopReason: fw.StopEndTurn, Usage: usage, Provider: "fake", Model: "m"}, nil
		}
		call := fw.Block{Type: fw.BlockToolUse, ID: fw.NewID(), Name: "echo", Input: json.RawMessage(`{"text":"a"}`)}
		return &fw.Response{Content: []fw.Block{call}, StopReason: fw.StopToolUse, Usage: usage, Provider: "fake", Model: "m"}, nil
	}}
}

// TestPlatformUsageFromLifecycleEvents runs work on a worker whose egress
// sends only the types of EgressTypes, as a worker of a pool does: the
// lifecycle events and the tool calls, and no fw.model.responded. The front still
// shows the tokens, the turns, the tool calls, and the cost of the run,
// which come from the totals of fw.run.finished, and the usage grows with
// a follow-up. The front has no price table, so the cost is the cost of
// the worker.
func TestPlatformUsageFromLifecycleEvents(t *testing.T) {
	ctx := context.Background()
	bus := fw.NewMemBus()
	var mu sync.Mutex
	sent := map[string]int{}
	stop, err := bus.Subscribe(ctx, "fw.events.>", func(e fw.Event) error {
		mu.Lock()
		sent[e.Type]++
		mu.Unlock()
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	front := newMember(t, bus, "front", nil)
	worker := newWorker(t, bus, "box:8080", fw.WithModel(toolModel()), fw.WithTools(echoTool()),
		fw.WithPrices(fw.PriceTable{"fake/m": {Input: 2, Output: 10}}))

	r, err := worker.Run(as("ada"), "hello")
	if err != nil || r.State != fw.StateCompleted {
		t.Fatalf("worker run = %+v, %v", r, err)
	}
	if u := r.Usage; u.InputTokens != 200 || u.CacheReadTokens != 100 || u.Turns != 2 || u.ToolCalls != 1 || u.CostUSD <= 0 {
		t.Fatalf("the usage on the worker = %+v", u)
	}
	eventually(t, "the front shows the usage of the run", func() bool {
		got, err := front.f.Run(ctx, r.ID)
		return err == nil && got.State == fw.StateCompleted && got.Usage == r.Usage
	})
	// The mem bus hands each event to a goroutine of its own, so the spy
	// can lag behind the front. It waits for every event of the run that
	// the egress of the worker publishes, and then tells what they were.
	egress := map[string]bool{}
	for _, typ := range EgressTypes {
		egress[typ] = true
	}
	ledger, err := worker.Events(ctx, r.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	want := 0
	for e := range ledger {
		if egress[e.Type] {
			want++
		}
	}
	eventually(t, "the spy receives every published event", func() bool {
		mu.Lock()
		defer mu.Unlock()
		n := 0
		for _, c := range sent {
			n += c
		}
		return n == want
	})
	mu.Lock()
	if sent[fw.EvModelResponded] != 0 || sent[fw.EvToolRequested] != 1 || sent[fw.EvToolResponded] != 1 || sent[fw.EvRunFinished] != 1 {
		t.Fatalf("the stream carried %v", sent)
	}
	mu.Unlock()

	// A follow-up finishes the run again, and the front shows the usage of
	// both parts.
	if err := worker.Send(as("ada"), r.ID, fw.UserText("again"), fw.OriginCLI); err != nil {
		t.Fatal(err)
	}
	again, err := worker.Wait(ctx, r.ID)
	if err != nil || again.State != fw.StateCompleted || again.Usage.Turns != 4 || again.Usage.ToolCalls != 2 {
		t.Fatalf("the follow-up = %+v, %v", again, err)
	}
	eventually(t, "the front shows the usage of both parts", func() bool {
		got, err := front.f.Run(ctx, r.ID)
		return err == nil && got.Usage == again.Usage
	})
	runs, err := front.f.Runs(ctx)
	if err != nil || len(runs) != 1 || runs[0].Usage != again.Usage || runs[0].Member != "box:8080" {
		t.Fatalf("Runs = %+v, %v", runs, err)
	}
}
