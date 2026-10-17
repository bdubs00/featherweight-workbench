package mcp

import (
	"strings"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/fleet"
)

// TestEnqueueAndBatch sends a job from the front to a worker on the same
// bus, and follows it with batch and runs_list.
func TestEnqueueAndBatch(t *testing.T) {
	ctx := testCtx(t)
	t.Setenv(fleet.EnvIntake, "fw.jobs")
	bus := fw.NewMemBus()
	// The worker runs the profile default and takes its queue.
	wcfg := testConfig(t)
	wcfg.Bus.Intake = "fw.jobs.default"
	worker := newAgent(t, newScript(endTurn("done 1"), endTurn("done 2"), endTurn("done 3")).model(), fw.WithConfig(wcfg), fw.WithBus(bus))
	// The front has no agents of its own in a fleet; here it keeps the
	// default profile so the authorization of the job has an agent.
	fake := &fakeFleet{name: "front"}
	front := attach(newAgent(t, newScript().model(), fw.WithBus(bus), fw.WithAuthn(people)), fake)
	cs := as(t, ctx, serveAgent(t, front, nil).URL, "alice-token")

	res := callTool(t, ctx, cs, toolEnqueue, map[string]any{"agent": "default", "batch": "job-1",
		"items": []map[string]any{{"input": "piece one", "title": "One"}, {"input": "piece two"}}})
	if res.IsError {
		t.Fatalf("enqueue = %s", textOf(res))
	}
	var out enqueueResult
	structured(t, res, &out)
	if out.Batch != "job-1" || out.Queue != "fw.jobs.default" || out.Count != 2 || !strings.Contains(textOf(res), "Queued 2 runs") {
		t.Fatalf("enqueue = %+v %q", out, textOf(res))
	}
	// The worker takes both, as alice, with the batch in the metadata.
	var runs []*fw.Run
	deadline := time.Now().Add(5 * time.Second)
	for {
		runs, _ = worker.List(ctx)
		done := 0
		for _, r := range runs {
			if r.State.Terminal() {
				done++
			}
		}
		if done == 2 || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(runs) != 2 {
		t.Fatalf("worker runs = %d", len(runs))
	}
	titled := false
	for _, r := range runs {
		if r.Principal != "alice" || r.Metadata[fw.MetaBatch] != "job-1" || r.State != fw.StateCompleted || r.Agent != "default" {
			t.Fatalf("worker run = %+v", r)
		}
		if r.Title == "One" {
			titled = true
		}
	}
	if !titled {
		t.Fatal("the title of an item did not reach its run")
	}
	// A repeated enqueue with the same batch starts no second run.
	callTool(t, ctx, cs, toolEnqueue, map[string]any{"agent": "default", "batch": "job-1", "items": []map[string]any{{"input": "piece one"}, {"input": "piece two"}}})
	time.Sleep(50 * time.Millisecond)
	if again, _ := worker.List(ctx); len(again) != 2 {
		t.Fatalf("a repeated job started runs: %d", len(again))
	}

	// The front follows the job through its view of the fleet.
	other := &fw.Run{ID: "01OTHER", Agent: "default", State: fw.StateRunning, Principal: "alice", Member: "alpha",
		Metadata: map[string]string{fw.MetaBatch: "job-1"}, CreatedAt: time.Now()}
	failed := &fw.Run{ID: "01FAILED", Agent: "default", State: fw.StateFailed, Error: "boom", Principal: "alice", Member: "alpha",
		Metadata: map[string]string{fw.MetaBatch: "job-1"}, CreatedAt: time.Now()}
	for _, r := range runs {
		r.Member = "alpha"
	}
	fake.set(append(runs, other, failed)...)
	res = callTool(t, ctx, cs, toolBatch, map[string]any{"batch": "job-1"})
	var b batchResult
	structured(t, res, &b)
	if b.Total != 4 || b.States["completed"] != 2 || b.States["failed"] != 1 || b.States["running"] != 1 || len(b.Failed) != 1 || b.Failed[0].Detail != "boom" || b.Finished || len(b.Members) != 1 || b.Members[0] != "owner" {
		t.Fatalf("batch = %+v %q", b, textOf(res))
	}
	if txt := textOf(res); !strings.Contains(txt, "Batch job-1: 4 runs on 1 members") || strings.Contains(txt, "finished") || !strings.Contains(txt, "01FAILED failed on owner: boom") {
		t.Fatalf("batch text = %q", txt)
	}
	res = callTool(t, ctx, cs, toolRunsList, map[string]any{"batch": "job-1"})
	var list runsResult
	structured(t, res, &list)
	if len(list.Runs) != 4 || list.Total != 4 {
		t.Fatalf("runs_list with batch = %d of %d", len(list.Runs), list.Total)
	}
	res = callTool(t, ctx, cs, toolBatch, map[string]any{"batch": "job-9"})
	if res.IsError || !strings.Contains(textOf(res), "No run of batch job-9") {
		t.Fatalf("unknown batch = %q", textOf(res))
	}
	// Bad input.
	if res := callTool(t, ctx, cs, toolEnqueue, map[string]any{"agent": "default", "items": []map[string]any{{"input": "  "}}}); !res.IsError || !strings.Contains(textOf(res), "item 1 is empty") {
		t.Fatalf("empty item = %q", textOf(res))
	}
	if res := callTool(t, ctx, cs, toolEnqueue, map[string]any{"agent": "no.dots", "items": []map[string]any{{"input": "x"}}}); !res.IsError {
		t.Fatalf("bad agent = %q", textOf(res))
	}
}

func TestEnqueueNeedsTheIntake(t *testing.T) {
	ctx := testCtx(t)
	t.Setenv(fleet.EnvIntake, "")
	a := newAgent(t, newScript().model(), fw.WithAuthn(people))
	cs := as(t, ctx, serveAgent(t, a, nil).URL, "alice-token")
	tools, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tl := range tools.Tools {
		if tl.Name == toolEnqueue || tl.Name == toolBatch {
			t.Fatalf("the tool %s is listed without an intake prefix", tl.Name)
		}
	}
	if _, err := fleet.Queue(a.Config(), "default"); err == nil {
		t.Fatal("Queue without an intake prefix")
	}
}
