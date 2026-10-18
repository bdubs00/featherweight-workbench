package fleet

import (
	"context"
	"testing"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// branchSpawner runs a branch in another worker, as the ax driver runs it
// in a child Task: Place names the worker at once, and Branch waits for
// the release before it sends the kept events, as a Task that boots would.
type branchSpawner struct {
	child   *fw.Agent
	member  string
	release chan struct{}
}

func (s *branchSpawner) Spawn(context.Context, *fw.Run, fw.RunRequest) (*fw.Run, string, error) {
	return nil, "", fw.ErrInvalidRequest
}
func (s *branchSpawner) Get(ctx context.Context, _, id string) (*fw.Run, error) {
	return s.child.Get(ctx, id)
}
func (s *branchSpawner) WaitFor(ctx context.Context, _, id string, done func(*fw.Run) bool) (*fw.Run, error) {
	return s.child.WaitFor(ctx, id, done)
}
func (s *branchSpawner) Decide(ctx context.Context, _, id string, d fw.ApprovalDecision) error {
	return s.child.Decide(ctx, id, d)
}
func (s *branchSpawner) Cancel(ctx context.Context, _, id string) error {
	return s.child.Cancel(ctx, id)
}
func (s *branchSpawner) Place(context.Context, *fw.Run) (string, error) { return s.member, nil }
func (s *branchSpawner) Branch(ctx context.Context, _ string, events []fw.Event, req fw.ForkRequest) (*fw.Run, error) {
	select {
	case <-s.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return s.child.ForkEvents(ctx, events, req)
}

// A branch in a Task of its own is known to the view from the fw.run.child
// event of its source before the Task has it: as a queued run of the
// source's member, under the source, so the source answers for it. Once
// the Task publishes the events of the branch, the view shows the branch
// on that member, still under the source, and once only.
func TestViewFollowsABranchIntoItsOwnTask(t *testing.T) {
	ctx := context.Background()
	bus := fw.NewMemBus()
	front := newMember(t, bus, "front", nil)
	child := newWorkerWith(t, bus, "ax:team/alpha-1a2b", textModel("quince"))
	sp := &branchSpawner{child: child, member: "ax:team/alpha-1a2b", release: make(chan struct{})}
	source := newWorkerWith(t, bus, "ax:team/alpha", textModel("pear"), fw.WithSpawner(sp))

	src, err := source.Run(as("ada"), "a fruit")
	if err != nil || src.State != fw.StateCompleted {
		t.Fatalf("source run = %+v, %v", src, err)
	}
	b, err := source.Fork(as("ada"), src.ID, fw.ForkRequest{Input: "another", Spawn: true})
	if err != nil {
		t.Fatalf("Fork: %v", err)
	}
	if b.State != fw.StateQueued || b.Member != "ax:team/alpha-1a2b" {
		t.Fatalf("the branch as the source answers it = %+v", b)
	}
	eventually(t, "the view knows the starting branch from the source", func() bool {
		got, err := front.f.Run(ctx, b.ID)
		return err == nil && got.Member == "team/alpha" && got.State == fw.StateQueued && got.Parent == src.ID && got.Principal == "ada"
	})
	kids := childrenOf(t, front.f, src.ID)
	if len(kids) != 1 || kids[0].ID != b.ID {
		t.Fatalf("children of the source while the branch starts = %+v", kids)
	}

	close(sp.release)
	eventually(t, "the view follows the branch to its own Task", func() bool {
		got, err := front.f.Run(ctx, b.ID)
		return err == nil && got.Member == "team/alpha-1a2b" && got.State == fw.StateCompleted && got.Output == "quince" && got.Parent == src.ID && got.Depth == 1
	})
	runs, err := front.f.Runs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, r := range runs {
		if r.ID == b.ID {
			seen++
		}
	}
	if seen != 1 || len(runs) != 2 {
		t.Fatalf("the branch appears %d times among %d runs", seen, len(runs))
	}
	if got := MemberName("ax:team/alpha-1a2b"); got != "team/alpha-1a2b" {
		t.Fatalf("MemberName = %q", got)
	}
}

// childrenOf lists the runs of the view whose parent is id.
func childrenOf(t *testing.T, f *Fleet, id string) []*fw.Run {
	t.Helper()
	runs, err := f.Runs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var out []*fw.Run
	for _, r := range runs {
		if r.Parent == id {
			out = append(out, r)
		}
	}
	return out
}
