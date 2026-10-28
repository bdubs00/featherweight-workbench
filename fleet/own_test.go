package fleet

import (
	"context"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// A front that takes its own intake and publishes its own egress, as one
// process on a workstation does, hears its runs on the stream. The view
// lists them once, as the runs of this process, and shows no second member
// for its node. The watch still sends the changes of those runs.
func TestViewLeavesOutTheMemberOfThisProcess(t *testing.T) {
	ctx := context.Background()
	bus := fw.NewMemBus()
	front := newMember(t, bus, "drive", func(c *fw.Config, _ *Config) {
		c.Node = "127.0.0.1:8770"
		c.Bus.Egress = "fw.events"
		c.Bus.EgressTypes = EgressTypes
	})
	wctx, cancel := context.WithCancel(ctx)
	defer cancel()
	changes := front.f.Watch(wctx)

	run, err := front.a.Run(as("ada"), "a fruit")
	if err != nil || run.State != fw.StateCompleted {
		t.Fatalf("run = %+v, %v", run, err)
	}
	deadline := time.After(5 * time.Second)
	for got := false; !got; {
		select {
		case c := <-changes:
			got = c.Kind == ChangeRun && c.Run != nil && c.Run.ID == run.ID && c.Run.State == fw.StateCompleted
		case <-deadline:
			t.Fatal("the watch sent no change of the run of this process")
		}
	}
	runs, err := front.f.Runs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 0 {
		t.Fatalf("the view lists %d runs of other members, want 0: %+v", len(runs), runs)
	}
	members, err := front.f.Members(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(members) != 1 || members[0].Name != "drive" || !members[0].Self {
		t.Fatalf("members = %+v, want only this process", members)
	}
}
