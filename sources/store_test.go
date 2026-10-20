package sources

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// clock is a time that a test moves.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// claimCycle runs the life of the claims of one store: a claim, a second
// claim while the first is pending, the end of the first, a duplicate, a
// release, and a claim again.
func claimCycle(t *testing.T, st store) {
	t.Helper()
	ctx := context.Background()
	t1, err := st.Take(ctx, "tickets", "T-1")
	if err != nil {
		t.Fatalf("Take = %v", err)
	}
	if _, err := st.Take(ctx, "tickets", "T-1"); !errors.Is(err, ErrInFlight) {
		t.Fatalf("Take of a pending key = %v, want ErrInFlight", err)
	}
	// Another source has keys of its own.
	if _, err := st.Take(ctx, "alerts", "T-1"); err != nil {
		t.Fatalf("Take of another source = %v", err)
	}
	if err := st.Finish(ctx, t1); err != nil {
		t.Fatalf("Finish = %v", err)
	}
	if _, err := st.Take(ctx, "tickets", "T-1"); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("Take of a done key = %v, want ErrDuplicate", err)
	}
	// A failed publish releases its claim, and the next delivery takes
	// the key.
	t2, err := st.Take(ctx, "tickets", "T-2")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Release(ctx, t2); err != nil {
		t.Fatalf("Release = %v", err)
	}
	t3, err := st.Take(ctx, "tickets", "T-2")
	if err != nil {
		t.Fatalf("Take after a release = %v", err)
	}
	// A release with an old ticket removes nothing.
	if err := st.Release(ctx, t2); err == nil {
		if _, err := st.Take(ctx, "tickets", "T-2"); !errors.Is(err, ErrInFlight) {
			t.Fatalf("an old ticket released the new claim: %v", err)
		}
	}
	if err := st.Finish(ctx, t3); err != nil {
		t.Fatal(err)
	}
}

func TestMemStore(t *testing.T) {
	c := &clock{t: time.Now()}
	st := newMemStore(map[string]time.Duration{"tickets": time.Hour})
	st.now = c.now
	claimCycle(t, st)
	ctx := context.Background()

	// A pending claim that stays longer than staleClaim goes to the next
	// delivery.
	if _, err := st.Take(ctx, "tickets", "T-3"); err != nil {
		t.Fatal(err)
	}
	c.add(staleClaim + time.Second)
	t3, err := st.Take(ctx, "tickets", "T-3")
	if err != nil {
		t.Fatalf("Take of a stale claim = %v", err)
	}
	if err := st.Finish(ctx, t3); err != nil {
		t.Fatal(err)
	}
	// The window ends, and the key makes a run again.
	c.add(time.Hour + time.Minute)
	if _, err := st.Take(ctx, "tickets", "T-1"); err != nil {
		t.Fatalf("Take after the window = %v", err)
	}
	if len(st.claims) > 3 {
		t.Fatalf("the sweep left %d claims", len(st.claims))
	}
}

func TestNATSStore(t *testing.T) {
	srv := runNATS(t)
	windows := map[string]time.Duration{"tickets": time.Hour, "alerts": time.Hour}
	open := func() *natsStore {
		st, err := dialNATS(srv.ClientURL(), "FW", windows, func() string { return "front-a" }, quiet())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = st.Close() })
		return st
	}
	st := open()
	claimCycle(t, st)
	ctx := context.Background()

	// The bucket carries the window as its TTL.
	kv, err := st.js.KeyValue(ctx, "FW_SOURCE_tickets")
	if err != nil {
		t.Fatal(err)
	}
	status, err := kv.Status(ctx)
	if err != nil || status.TTL() != time.Hour {
		t.Fatalf("bucket status = %+v, %v", status, err)
	}

	// A restart keeps the claims: a new connection, as a new replica or
	// a restarted one, sees the duplicate.
	other := open()
	if _, err := other.Take(ctx, "tickets", "T-1"); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("Take on a second connection = %v, want ErrDuplicate", err)
	}

	// A pending claim that stays longer than staleClaim goes to the next
	// delivery, on any replica.
	if _, err := st.Take(ctx, "tickets", "T-9"); err != nil {
		t.Fatal(err)
	}
	other.now = func() time.Time { return time.Now().Add(staleClaim + time.Second) }
	tk, err := other.Take(ctx, "tickets", "T-9")
	if err != nil {
		t.Fatalf("Take of a stale claim = %v", err)
	}
	if err := other.Finish(ctx, tk); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Take(ctx, "tickets", "T-9"); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("after the takeover: %v", err)
	}

	// A bucket that the server lost is made again.
	if err := st.js.DeleteKeyValue(ctx, "FW_SOURCE_alerts"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Take(ctx, "alerts", "A-1"); err != nil {
		t.Fatalf("Take after the bucket went = %v", err)
	}
}

// TestNATSStoreRace claims one key from two connections, as two replicas
// of the front that get the same event, many times at once. Exactly one
// claim wins.
func TestNATSStoreRace(t *testing.T) {
	srv := runNATS(t)
	windows := map[string]time.Duration{"tickets": time.Hour}
	var stores []*natsStore
	for _, name := range []string{"front-a", "front-b"} {
		st, err := dialNATS(srv.ClientURL(), "FW", windows, func() string { return name }, quiet())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = st.Close() })
		// Make the bucket first, so the race is on the key.
		if _, err := st.bucket(context.Background(), "tickets"); err != nil {
			t.Fatal(err)
		}
		stores = append(stores, st)
	}
	var won, lost atomic.Int32
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := stores[i%2].Take(context.Background(), "tickets", "T-race")
			switch {
			case err == nil:
				won.Add(1)
			case errors.Is(err, ErrInFlight):
				lost.Add(1)
			default:
				t.Errorf("Take = %v", err)
			}
		}()
	}
	wg.Wait()
	if won.Load() != 1 || lost.Load() != 19 {
		t.Fatalf("won %d, lost %d", won.Load(), lost.Load())
	}
}

// TestNATSStoreWindow lets the window of a key end on the server.
func TestNATSStoreWindow(t *testing.T) {
	srv := runNATS(t)
	st, err := dialNATS(srv.ClientURL(), "FW", map[string]time.Duration{"tickets": time.Second}, nil, quiet())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	tk, err := st.Take(ctx, "tickets", "T-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Finish(ctx, tk); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Take(ctx, "tickets", "T-1"); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("Take in the window = %v", err)
	}
	eventually(t, "the key leaves the bucket after its window", func() bool {
		_, err := st.Take(ctx, "tickets", "T-1")
		return err == nil
	})
}

// TestNATSStoreDown refuses every claim while the server is down, at
// once, and claims again when it is back.
func TestNATSStoreDown(t *testing.T) {
	srv := runNATS(t)
	st, err := dialNATS(srv.ClientURL(), "FW", map[string]time.Duration{"tickets": time.Hour}, nil, quiet())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	if _, err := st.Take(ctx, "tickets", "T-1"); err != nil {
		t.Fatal(err)
	}
	srv.Shutdown()
	srv.WaitForShutdown()
	eventually(t, "the client sees the server go", func() bool { return !st.nc.IsConnected() })
	start := time.Now()
	if _, err := st.Take(ctx, "tickets", "T-2"); !errors.Is(err, ErrStore) {
		t.Fatalf("Take while the server is down = %v, want ErrStore", err)
	}
	if time.Since(start) > time.Second {
		t.Fatalf("a claim waited %s for a server that is down", time.Since(start))
	}
}
