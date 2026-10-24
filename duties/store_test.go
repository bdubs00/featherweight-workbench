package duties

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/bdubs00/featherweight-workbench/internal/bucket"
)

// memStore returns a store in memory on the clock.
func memStore(c *clock) *Store {
	st := NewStore(bucket.NewMem(dutiesBucket), bucket.NewMem(runsBucket), bucket.NewMem(seenBucket), quiet())
	st.now = c.Now
	return st
}

// natsJS starts a JetStream server for one test and returns a function
// that opens a new connection to it, as a replica of the front does.
func natsJS(t *testing.T) func() jetstream.JetStream {
	t.Helper()
	s, err := server.NewServer(&server.Options{Host: "127.0.0.1", Port: -1, JetStream: true, StoreDir: t.TempDir(), NoLog: true, NoSigs: true})
	if err != nil {
		t.Fatal(err)
	}
	go s.Start()
	t.Cleanup(s.Shutdown)
	if !s.ReadyForConnections(5 * time.Second) {
		t.Fatal("the NATS server did not start")
	}
	return func() jetstream.JetStream {
		nc, err := nats.Connect(s.ClientURL())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(nc.Close)
		js, err := jetstream.New(nc)
		if err != nil {
			t.Fatal(err)
		}
		return js
	}
}

// natsStore returns a store on the buckets of a NATS server, with its own
// connection.
func natsStore(js jetstream.JetStream, c *clock) *Store {
	st := NewStore(bucket.NewNATS(js, "FW_DUTIES", dutiesBucket), bucket.NewNATS(js, "FW_DUTY_RUNS", runsBucket), bucket.NewNATS(js, "FW_DUTY_SEEN", seenBucket), quiet())
	st.now = c.Now
	return st
}

// A write names the revision that it read, so two writers never both win;
// a remove keeps the history and drops the counters and the seen keys.
func TestStoreDuties(t *testing.T) {
	ctx := context.Background()
	st := memStore(&clock{t: t0})
	d := daily("report")
	d.normalize("")
	rec := &stored{Duty: d, Since: t0}
	if err := st.create(ctx, rec); err != nil {
		t.Fatal(err)
	}
	if err := st.create(ctx, &stored{Duty: d}); !errors.Is(err, ErrExists) {
		t.Fatalf("a second create = %v", err)
	}
	got, err := st.get(ctx, "report")
	if err != nil || got.rev != rec.rev || got.Duty.Agent != "triage" {
		t.Fatalf("get = %+v, %v", got, err)
	}
	if _, err := st.update(ctx, "report", rec.rev, func(x *stored) error { x.Duty.Paused = true; return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := st.update(ctx, "report", rec.rev, func(x *stored) error { return nil }); !errors.Is(err, ErrConflict) {
		t.Fatalf("a write at an old revision = %v", err)
	}
	if _, err := st.get(ctx, "nothing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get of no duty = %v", err)
	}
	if _, err := st.claim(ctx, "report", "2026-10-23", entry{ID: "a", RunEntry: RunEntry{State: StateSkipped}}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.modifyCounters(ctx, "report", func(c *counters) bool { c.FailuresInRow = 2; return true }); err != nil {
		t.Fatal(err)
	}
	if err := st.remember(ctx, "report", []string{"INC1"}, ""); err != nil {
		t.Fatal(err)
	}
	cur, _ := st.get(ctx, "report")
	if err := st.remove(ctx, "report", cur.rev); err != nil {
		t.Fatal(err)
	}
	if c, _ := st.counters(ctx, "report"); c.FailuresInRow != 0 {
		t.Fatalf("the counters stayed: %+v", c)
	}
	if seen, _ := st.seenOf(ctx, "report"); len(seen) != 0 {
		t.Fatalf("the seen keys stayed: %v", seen)
	}
	if h, _ := st.history(ctx, "report", 10); len(h) != 1 {
		t.Fatalf("the history went: %+v", h)
	}
	if list, _ := st.list(ctx); len(list) != 0 {
		t.Fatalf("the list = %+v", list)
	}
}

// The history comes back newest first across days, and a day keeps at
// most maxDayEntries starts, dropping the ended starts without a run first.
func TestStoreHistory(t *testing.T) {
	ctx := context.Background()
	st := memStore(&clock{t: t0})
	at := func(h int) time.Time { return t0.Add(time.Duration(h) * time.Hour) }
	for i, c := range []struct {
		date string
		slot time.Time
	}{{"2026-10-22", at(-24)}, {"2026-10-23", at(1)}, {"2026-10-23", at(0)}} {
		e := entry{ID: c.slot.Format(time.RFC3339Nano), RunEntry: RunEntry{Slot: c.slot, State: StateCompleted, Run: string(rune('a' + i))}}
		if ok, err := st.claim(ctx, "report", c.date, e); !ok || err != nil {
			t.Fatalf("claim = %v, %v", ok, err)
		}
	}
	if ok, _ := st.claim(ctx, "report", "2026-10-23", entry{ID: at(0).Format(time.RFC3339Nano)}); ok {
		t.Fatal("a second claim of one time won")
	}
	h, err := st.history(ctx, "report", 10)
	if err != nil || len(h) != 3 || h[0].Run != "b" || h[1].Run != "c" || h[2].Run != "a" {
		t.Fatalf("history = %+v, %v", h, err)
	}
	if h, _ := st.history(ctx, "report", 2); len(h) != 2 {
		t.Fatalf("a short history = %+v", h)
	}
	var d day
	d.add(entry{ID: "run", RunEntry: RunEntry{State: StateCompleted, Run: "r"}})
	for i := range maxDayEntries {
		d.add(entry{ID: time.Duration(i).String(), RunEntry: RunEntry{State: StateQuiet}})
	}
	if len(d.Entries) != maxDayEntries || d.Dropped != 1 || d.Entries[0].ID != "run" {
		t.Fatalf("the day keeps %d starts, dropped %d, first %q", len(d.Entries), d.Dropped, d.Entries[0].ID)
	}
}

// The claim of a time is a compare-and-swap of the day on the server of
// the bus: of two replicas on two connections that claim one time at the
// same moment, one wins.
func TestStoreClaimNATS(t *testing.T) {
	open := natsJS(t)
	c := &clock{t: t0}
	a, b := natsStore(open(), c), natsStore(open(), c)
	ctx := context.Background()
	var wins atomic.Int32
	for round := range 20 {
		id := t0.Add(time.Duration(round) * time.Minute).Format(time.RFC3339Nano)
		var wg sync.WaitGroup
		for _, st := range []*Store{a, b} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				ok, err := st.claim(ctx, "report", "2026-10-23", entry{ID: id, RunEntry: RunEntry{State: StateStarting}})
				if err != nil {
					t.Error(err)
				}
				if ok {
					wins.Add(1)
				}
			}()
		}
		wg.Wait()
	}
	if wins.Load() != 20 {
		t.Fatalf("%d claims won in 20 rounds of two replicas", wins.Load())
	}
	d, _ := a.day(ctx, "report", "2026-10-23")
	if len(d.Entries) != 20 {
		t.Fatalf("the day holds %d starts", len(d.Entries))
	}
}

// The switch, the counters, and the seen keys keep their values, and the
// seen keys forget a key after HistoryFor.
func TestStoreState(t *testing.T) {
	ctx := context.Background()
	c := &clock{t: t0}
	st := memStore(c)
	if sw, err := st.killSwitch(ctx); err != nil || sw.Paused {
		t.Fatalf("the switch of a new store = %+v, %v", sw, err)
	}
	if err := st.setSwitch(ctx, Switch{Paused: true, Reason: "storm", By: "root", At: t0}); err != nil {
		t.Fatal(err)
	}
	if sw, _ := st.killSwitch(ctx); !sw.Paused || sw.Reason != "storm" {
		t.Fatalf("the switch = %+v", sw)
	}
	if err := st.remember(ctx, "report", []string{"A", "B"}, "s1"); err != nil {
		t.Fatal(err)
	}
	c.Add(HistoryFor + time.Hour)
	if err := st.remember(ctx, "report", []string{"C"}, "s2"); err != nil {
		t.Fatal(err)
	}
	seen, _ := st.seenOf(ctx, "report")
	if _, ok := seen["A"]; ok || len(seen) != 1 {
		t.Fatalf("the seen keys = %v", seen)
	}
}

// A call claims its key for its window: a second call within the window
// is a duplicate, a call after it is new, and a release frees the key.
func TestStoreCalls(t *testing.T) {
	ctx := context.Background()
	c := &clock{t: t0}
	st := memStore(c)
	ok, release, err := st.takeCall(ctx, "hook", "key:A", time.Hour)
	if !ok || err != nil {
		t.Fatalf("the first call = %v, %v", ok, err)
	}
	if ok, _, _ := st.takeCall(ctx, "hook", "key:A", time.Hour); ok {
		t.Fatal("a second call within the window was new")
	}
	if ok, _, _ := st.takeCall(ctx, "other", "key:A", time.Hour); !ok {
		t.Fatal("a call of another duty was a duplicate")
	}
	release()
	if ok, _, _ := st.takeCall(ctx, "hook", "key:A", time.Hour); !ok {
		t.Fatal("a released key was a duplicate")
	}
	c.Add(2 * time.Hour)
	if ok, _, _ := st.takeCall(ctx, "hook", "key:A", time.Hour); !ok {
		t.Fatal("a call after the window was a duplicate")
	}
}
