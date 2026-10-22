package bridges

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/fleet"
	"github.com/bdubs00/featherweight-workbench/internal/bucket"
	"github.com/bdubs00/featherweight-workbench/manager"
)

// watchView is a view of the fleet that sends the changes that a test
// gives it, as fleet.Watcher does.
type watchView struct {
	*fakeView
	mu      sync.Mutex
	changes chan fleet.Change
	watches int
}

func newWatchView(v *fakeView) *watchView {
	return &watchView{fakeView: v, changes: make(chan fleet.Change, 16)}
}

func (w *watchView) Watch(ctx context.Context) <-chan fleet.Change {
	w.mu.Lock()
	w.watches++
	w.mu.Unlock()
	out := make(chan fleet.Change)
	go func() {
		defer close(out)
		for {
			select {
			case <-ctx.Done():
				return
			case c, ok := <-w.changes:
				if !ok {
					return
				}
				select {
				case out <- c:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return out
}

func (w *watchView) Visible(context.Context, fleet.Change) (bool, error) { return true, nil }

// runningStore is newStore with the watch and the follower running, and
// the times of a test.
func runningStore(t *testing.T, a *fw.Agent, pollEvery time.Duration) *Store {
	t.Helper()
	cfg, err := Load(a.Config())
	if err != nil {
		t.Fatal(err)
	}
	rc, cc := bucketConfigs()
	s := New(a, bucket.NewMem(rc), bucket.NewMem(cc), cfg, quiet())
	s.pollEvery, s.sweepEvery, s.pollGap, s.followRetry = pollEvery, time.Hour, 5*time.Millisecond, 10*time.Millisecond
	s.retryFirst, s.retryMax, s.retryFor = 10*time.Millisecond, 40*time.Millisecond, 3*time.Second
	s.run()
	Attach(a, s)
	t.Cleanup(func() {
		Attach(a, nil)
		s.Close()
	})
	return s
}

// steady waits until the reads of the view stop for a while and returns
// their count.
func steady(t *testing.T, s *Store) int64 {
	t.Helper()
	n := s.polls.Load()
	for range 50 {
		time.Sleep(40 * time.Millisecond)
		m := s.polls.Load()
		if m == n {
			return n
		}
		n = m
	}
	t.Fatal("the watch never stops reading the view")
	return 0
}

// With a fleet that sends its changes, the watch reads the view at a
// change of a run of a bridge, of a member, or of a Task, and not at a
// change of another run and not on a tick. When the changes end and the
// fleet sends none, the poll takes over.
func TestWatchFollowsTheChangesOfTheFleet(t *testing.T) {
	a := newAgent(t, nil)
	plain := newView("http://127.0.0.1:1")
	plain.state("default/crew-0", fleet.MemberLive)
	w := newWatchView(plain)
	fleet.Attach(a, w)
	t.Cleanup(func() { fleet.Attach(a, nil) })
	s := runningStore(t, a, 30*time.Millisecond)
	eventually(t, "the store follows the fleet", s.following.Load)
	rec, err := s.open(context.Background(), OpenSpec{Title: "Followed", Members: []SeatSpec{{Handle: "migrator-1", Agent: "migrator", Task: "Migrate"}}}, callers["alice"], nil)
	if err != nil {
		t.Fatal(err)
	}
	before := steady(t, s)
	plain.put(workerRun("r-mig", rec.ID, "migrator-1", "migrator", fw.StateRunning))
	time.Sleep(100 * time.Millisecond)
	if n := s.polls.Load(); n != before || len(withEvent(channel(t, s, rec.ID), EventStarted)) != 0 {
		t.Fatalf("the watch read the view %d times with no change", n-before)
	}
	other := &fw.Run{ID: "r-other", Agent: "migrator", State: fw.StateRunning, Member: "default/crew-0", Metadata: map[string]string{fw.MetaBatch: "nightly"}}
	w.changes <- fleet.Change{Kind: fleet.ChangeRun, Member: "default/crew-0", Run: other, Time: time.Now()}
	time.Sleep(100 * time.Millisecond)
	if n := s.polls.Load(); n != before {
		t.Fatalf("a change of a run of no bridge read the view %d times", n-before)
	}
	r, _ := plain.Run(context.Background(), "r-mig")
	w.changes <- fleet.Change{Kind: fleet.ChangeRun, Member: "default/crew-0", Run: r, Time: time.Now()}
	eventually(t, "a change of a run of the bridge posts the start", func() bool { return len(withEvent(channel(t, s, rec.ID), EventStarted)) == 1 })
	before = steady(t, s)
	w.changes <- fleet.Change{Kind: fleet.ChangeTask, Member: "default/crew-0", Phase: "Suspended", Time: time.Now()}
	eventually(t, "a change of a Task reads the view", func() bool { return s.polls.Load() > before })

	// The fleet goes away: the changes end, and the poll reads the view.
	fleet.Attach(a, plain)
	close(w.changes)
	eventually(t, "the store stops following", func() bool { return !s.following.Load() })
	plain.state("default/crew-0", fleet.MemberQuiet)
	eventually(t, "the poll posts the sleep", func() bool { return len(withEvent(channel(t, s, rec.ID), EventAsleep)) == 1 })
}

// Without a fleet that sends its changes, the watch reads the view every
// pollEvery, as before the changes of the fleet existed.
func TestWatchPollsWithoutChanges(t *testing.T) {
	a := newAgent(t, nil)
	v, _ := withFleet(t, a)
	v.state("default/crew-0", fleet.MemberLive)
	s := runningStore(t, a, 20*time.Millisecond)
	rec, err := s.open(context.Background(), OpenSpec{Title: "Polled", Members: []SeatSpec{{Handle: "migrator-1", Agent: "migrator", Task: "Migrate"}}}, callers["alice"], nil)
	if err != nil {
		t.Fatal(err)
	}
	v.put(workerRun("r-mig", rec.ID, "migrator-1", "migrator", fw.StateRunning))
	eventually(t, "the poll posts the start", func() bool { return len(withEvent(channel(t, s, rec.ID), EventStarted)) == 1 })
	if s.following.Load() {
		t.Fatal("the store follows a view that sends no changes")
	}
}

// The service gives its store to the package manager, so a manager run
// on a run of a bridge gets what the record of the bridge delegates, and
// its end takes the store back.
func TestServiceSetsTheBridgesOfTheManager(t *testing.T) {
	if manager.BridgePrefix != BatchPrefix {
		t.Fatalf("the manager reads the batch %q, the bridges write %q", manager.BridgePrefix, BatchPrefix)
	}
	a := newAgent(t, nil)
	t.Setenv(EnvManager, "manager")
	t.Cleanup(func() { manager.SetBridges(nil) })
	closer, err := Start(context.Background(), a)
	if err != nil || closer == nil {
		t.Fatalf("Start = %v, %v", closer, err)
	}
	s := Of(a)
	yes, no := true, false
	open := func(approve bool) string {
		t.Helper()
		d := &DelegationPatch{Approve: &no, Members: &yes}
		if approve {
			d.Approve = &yes
		}
		rec, err := s.open(context.Background(), OpenSpec{Title: "Delegated", Placement: PlacementQueue, Delegation: d,
			Members: []SeatSpec{{Handle: "migrator-1", Agent: "migrator", Task: "Migrate"}}}, callers["alice"], nil)
		if err != nil {
			t.Fatal(err)
		}
		return rec.ID
	}
	allowed, denied := open(true), open(false)
	ctx := context.Background()
	cfg := a.Config()
	mgr := manager.Actor{Run: "r-mgr", Agent: "manager", Person: "alice", PersonKind: fw.KindUser}
	if err := manager.Delegated(ctx, cfg, mgr, BatchOf(allowed), manager.DelegateApprove); err != nil {
		t.Fatalf("a bridge that delegates approvals: %v", err)
	}
	var refused *manager.NotDelegated
	if err := manager.Delegated(ctx, cfg, mgr, BatchOf(denied), manager.DelegateApprove); !errors.As(err, &refused) || !errors.Is(err, fw.ErrForbidden) {
		t.Fatalf("a bridge that keeps approvals: %v", err)
	}
	if err := manager.Delegated(ctx, cfg, mgr, BatchOf(denied), manager.DelegateMembers); err != nil {
		t.Fatalf("a bridge that delegates the members: %v", err)
	}
	if err := manager.Delegated(ctx, cfg, mgr, BatchOf(denied), manager.DelegateBudget); !errors.As(err, &refused) {
		t.Fatalf("a bridge that keeps the budget: %v", err)
	}
	if err := closer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := manager.Delegated(ctx, cfg, mgr, BatchOf(allowed), manager.DelegateApprove); !errors.As(err, &refused) {
		t.Fatalf("after the end of the service: %v", err)
	}
}

// The roster words a tool call with fleet.ActivityPhrase, the words of
// the run events of the stream of bossmode.
func TestRosterWordsAToolCall(t *testing.T) {
	r := &fw.Run{ID: "r-mig", State: fw.StateRunning, Activity: &fw.RunActivity{Turn: 3, Tool: "bash", CallID: "c1", Since: time.Now().UTC()}}
	act := activityOf(r)
	if act == nil || act.Phrase == "" || act.Phrase != fleet.ActivityPhrase(r) {
		t.Fatalf("activity = %+v, want the phrase %q", act, fleet.ActivityPhrase(r))
	}
}
