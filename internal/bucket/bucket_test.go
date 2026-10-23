package bucket

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// natsJS starts a JetStream server for one test and returns a client.
func natsJS(t *testing.T) jetstream.JetStream {
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

var testBucket = Config{Name: "TEST", Description: "a test", History: 3, MaxValue: 1 << 10}

// contract checks the rules that every bucket keeps.
func contract(t *testing.T, b Bucket) {
	ctx := context.Background()
	if _, err := b.Get(ctx, "a"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get of an empty bucket = %v", err)
	}
	if keys, err := b.Keys(ctx); err != nil || len(keys) != 0 {
		t.Fatalf("Keys of an empty bucket = %v, %v", keys, err)
	}
	if h, err := b.History(ctx, "a"); err != nil || len(h) != 0 {
		t.Fatalf("History of an empty bucket = %v, %v", h, err)
	}
	r1, err := b.Create(ctx, "a", []byte("one"))
	if err != nil || r1 == 0 {
		t.Fatalf("Create = %d, %v", r1, err)
	}
	if _, err := b.Create(ctx, "a", []byte("again")); !errors.Is(err, ErrConflict) {
		t.Fatalf("a second Create = %v", err)
	}
	r2, err := b.Update(ctx, "a", []byte("two"), r1)
	if err != nil || r2 <= r1 {
		t.Fatalf("Update = %d, %v", r2, err)
	}
	if _, err := b.Update(ctx, "a", []byte("stale"), r1); !errors.Is(err, ErrConflict) {
		t.Fatalf("an Update at an old revision = %v", err)
	}
	if e, err := b.Get(ctx, "a"); err != nil || string(e.Value) != "two" || e.Revision != r2 {
		t.Fatalf("Get = %+v, %v", e, err)
	}
	if e, err := b.At(ctx, "a", r1); err != nil || string(e.Value) != "one" {
		t.Fatalf("At the first revision = %+v, %v", e, err)
	}
	if _, err := b.Create(ctx, "b", []byte("bee")); err != nil {
		t.Fatal(err)
	}
	keys, err := b.Keys(ctx)
	if err != nil || len(keys) != 2 || keys[0].Key != "a" || keys[0].Revision != r2 || keys[1].Key != "b" {
		t.Fatalf("Keys = %+v, %v", keys, err)
	}
	if err := b.Delete(ctx, "a", r1); !errors.Is(err, ErrConflict) {
		t.Fatalf("a Delete at an old revision = %v", err)
	}
	if err := b.Delete(ctx, "a", r2); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Get(ctx, "a"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get after Delete = %v", err)
	}
	if keys, _ := b.Keys(ctx); len(keys) != 1 || keys[0].Key != "b" {
		t.Fatalf("Keys after Delete = %+v", keys)
	}
	h, err := b.History(ctx, "a")
	if err != nil || len(h) != 3 || !h[2].Deleted || string(h[1].Value) != "two" {
		t.Fatalf("History = %+v, %v", h, err)
	}
	// A key with a delete marker takes a new value through Create.
	r4, err := b.Create(ctx, "a", []byte("four"))
	if err != nil {
		t.Fatalf("Create after Delete = %v", err)
	}
	// The history keeps three entries: the oldest falls off.
	h, _ = b.History(ctx, "a")
	if len(h) != 3 || string(h[0].Value) != "two" || h[2].Revision != r4 {
		t.Fatalf("History after four writes = %+v", h)
	}
	if _, err := b.At(ctx, "a", r1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("At a revision that fell off = %v", err)
	}
	if _, err := b.Create(ctx, "big", make([]byte, 2<<10)); err == nil {
		t.Fatal("a value above MaxValue was written")
	}
}

func TestMem(t *testing.T) { contract(t, NewMem(testBucket)) }

func TestNATS(t *testing.T) {
	js := natsJS(t)
	b := NewNATS(js, "FW_TEST", testBucket)
	contract(t, b)
	kv, err := js.KeyValue(context.Background(), "FW_TEST")
	if err != nil {
		t.Fatal(err)
	}
	st, err := kv.Status(context.Background())
	if err != nil || st.History() != 3 {
		t.Fatalf("the bucket keeps %d values of each key: %v", st.History(), err)
	}
}

// Two replicas on two connections never both win a write of one revision.
func TestNATSTwoWriters(t *testing.T) {
	js := natsJS(t)
	a := NewNATS(js, "FW_TEST", testBucket)
	b := NewNATS(js, "FW_TEST", testBucket)
	ctx := context.Background()
	rev, err := a.Create(ctx, "k", []byte("base"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Update(ctx, "k", []byte("from a"), rev); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Update(ctx, "k", []byte("from b"), rev); !errors.Is(err, ErrConflict) {
		t.Fatalf("the second writer won too: %v", err)
	}
	if e, _ := b.Get(ctx, "k"); string(e.Value) != "from a" {
		t.Fatalf("the value = %q", e.Value)
	}
}

func TestOpen(t *testing.T) {
	ctx := context.Background()
	cfg := fw.DefaultConfig()
	b, closeFn, err := Open(ctx, cfg, testBucket, nil)
	if err != nil || b == nil || closeFn != nil {
		t.Fatalf("Open on mem = %v, %v", b, err)
	}
	cfg.Bus.Driver = "redis"
	if _, _, err := Open(ctx, cfg, testBucket, nil); !errors.Is(err, ErrNoStore) {
		t.Fatalf("Open on redis = %v", err)
	}
	if where, ok := Where(cfg, testBucket); ok || where == "" {
		t.Fatalf("Where on redis = %q, %v", where, ok)
	}
	cfg.Bus.Driver = "nats"
	cfg.Bus.Options = map[string]string{"stream": "AGENTS"}
	if where, ok := Where(cfg, testBucket); !ok || where != "the key-value bucket AGENTS_TEST of the NATS bus" {
		t.Fatalf("Where on nats = %q, %v", where, ok)
	}
	bad := testBucket
	bad.History = 65
	if _, _, err := Open(ctx, fw.DefaultConfig(), bad, nil); err == nil {
		t.Fatal("a history of 65 opened")
	}
}

// Modify writes at the revision that it read: two writers that race add
// both of their changes, and a function that returns nothing writes
// nothing.
func TestModify(t *testing.T) {
	ctx := context.Background()
	b := NewMem(Config{History: 1})
	add := func(s string) error {
		_, err := Modify(ctx, b, "k", 8, func(v []byte, _ bool) ([]byte, error) { return append(slices.Clone(v), s...), nil })
		return err
	}
	var wg sync.WaitGroup
	for _, s := range []string{"a", "b", "c", "d"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := add(s); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	e, _ := b.Get(ctx, "k")
	if len(e.Value) != 4 {
		t.Fatalf("the value = %q", e.Value)
	}
	wrote, err := Modify(ctx, b, "k", 8, func([]byte, bool) ([]byte, error) { return nil, nil })
	if wrote || err != nil {
		t.Fatalf("a function that returns nothing wrote: %v, %v", wrote, err)
	}
}

// A bucket with a TTL forgets each value once it is older than the TTL,
// and a key without values is gone from the keys.
func TestMemTTL(t *testing.T) {
	ctx := context.Background()
	b := NewMem(Config{History: 2, TTL: time.Hour}).(*memBucket)
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	b.now = func() time.Time { return now }
	r1, _ := b.Create(ctx, "old", []byte("1"))
	now = now.Add(40 * time.Minute)
	if _, err := b.Update(ctx, "old", []byte("2"), r1); err != nil {
		t.Fatal(err)
	}
	now = now.Add(30 * time.Minute)
	if h, _ := b.History(ctx, "old"); len(h) != 1 || string(h[0].Value) != "2" {
		t.Fatalf("History after the first value expired = %+v", h)
	}
	if _, err := b.Create(ctx, "young", []byte("y")); err != nil {
		t.Fatal(err)
	}
	now = now.Add(40 * time.Minute)
	if _, err := b.Get(ctx, "old"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get of an expired key = %v", err)
	}
	if keys, _ := b.Keys(ctx); len(keys) != 1 || keys[0].Key != "young" {
		t.Fatalf("Keys = %+v", keys)
	}
	if _, err := b.Create(ctx, "old", []byte("again")); err != nil {
		t.Fatalf("Create of an expired key = %v", err)
	}
}

// KeysUnder lists the keys under a first token, on every bucket, and a
// bucket that does not exist yet has none.
func TestKeysUnder(t *testing.T) {
	js := natsJS(t)
	for name, b := range map[string]Bucket{"mem": NewMem(testBucket), "nats": NewNATS(js, "FW_TEST", testBucket), "other": wrapped{NewMem(testBucket)}} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			if keys, err := KeysUnder(ctx, b, "a"); err != nil || len(keys) != 0 {
				t.Fatalf("KeysUnder of an empty bucket = %v, %v", keys, err)
			}
			for _, k := range []string{"a.2", "a.1", "ab.1", "b.1", "a"} {
				if _, err := b.Create(ctx, k, []byte("x")); err != nil {
					t.Fatal(err)
				}
			}
			if keys, err := KeysUnder(ctx, b, "a"); err != nil || !slices.Equal(keys, []string{"a.1", "a.2"}) {
				t.Fatalf("KeysUnder = %v, %v", keys, err)
			}
		})
	}
}

// wrapped is a bucket of another package, which lists its keys with Keys.
type wrapped struct{ Bucket }

func TestNATSTTL(t *testing.T) {
	js := natsJS(t)
	c := testBucket
	c.TTL = time.Hour
	b := NewNATS(js, "FW_TTL", c)
	if _, err := b.Create(context.Background(), "k", []byte("v")); err != nil {
		t.Fatal(err)
	}
	kv, err := js.KeyValue(context.Background(), "FW_TTL")
	if err != nil {
		t.Fatal(err)
	}
	if st, err := kv.Status(context.Background()); err != nil || st.TTL() != time.Hour {
		t.Fatalf("the TTL of the bucket = %v, %v", st.TTL(), err)
	}
}
