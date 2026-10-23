package duties

import (
	"context"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/fleet"
)

// A time of a duty goes through fleet.EnqueueFor to a worker that takes
// the queue of the agent: the worker runs it under the id that the front
// named, as the principal of the duty, with its metadata, and the history
// follows the run to its end.
func TestEnqueueToWorker(t *testing.T) {
	bus := fw.NewMemBus()
	f := newFrontOn(t, bus, "", nil)
	f.s.enqueue = func(ctx context.Context, agent, pool string, reqs []fw.RunRequest) (string, int, error) {
		return fleet.EnqueueFor(ctx, f.a, agent, pool, reqs)
	}
	cfg := fw.DefaultConfig()
	cfg.Home = t.TempDir()
	cfg.Bus.Intake = "fw.jobs.triage"
	cfg.Agents = map[string]fw.AgentProfile{"triage": {}}
	worker, err := fw.New(fw.WithConfig(cfg), fw.WithoutDefaults(), fw.WithAuthn(fw.NoAuthn{}),
		fw.WithModel(fw.ModelFunc{GenerateFn: func(context.Context, *fw.Request) (*fw.Response, error) {
			return &fw.Response{Content: []fw.Block{fw.Text("done")}, StopReason: fw.StopEndTurn}, nil
		}}),
		fw.WithLedger(fw.NewMemLedger()), fw.WithBus(bus), fw.WithLogger(quiet()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		worker.Close(ctx)
	})
	f.s.runs = func(ctx context.Context, name string) (map[string]*fw.Run, error) {
		list, err := worker.List(ctx)
		out := map[string]*fw.Run{}
		for _, r := range list {
			if r.Metadata[fw.MetaBatch] == BatchOf(name) {
				out[r.ID] = r
			}
		}
		return out, err
	}
	f.put(t, "alice", daily("report"))
	f.clock.Set(nine.Add(time.Second))
	f.pass(t)
	h := f.history(t, "report")
	if len(h) != 1 || h[0].State != StateQueued || h[0].Run == "" {
		t.Fatalf("history = %+v", h)
	}
	deadline := time.Now().Add(10 * time.Second)
	var run *fw.Run
	for time.Now().Before(deadline) {
		if r, err := worker.Get(context.Background(), h[0].Run); err == nil && r.State.Terminal() {
			run = r
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if run == nil {
		t.Fatal("the worker did not run the request under its id")
	}
	if run.Principal != "duty:report" || run.Metadata[MetaDuty] != "report" || run.Metadata[MetaSlot] != "2026-10-24T13:00:00Z" || run.State != fw.StateCompleted {
		t.Fatalf("the run = %+v", run)
	}
	f.pass(t)
	if h := f.history(t, "report"); h[0].State != StateCompleted {
		t.Fatalf("history after the run = %+v", h[0])
	}
}
