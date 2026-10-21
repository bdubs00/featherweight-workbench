package sources

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	busnats "github.com/bdubs00/featherweight-kernel/bus/nats"
	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/nats-io/nats.go/jetstream"
)

func redriveStore(t *testing.T, addr string, c *clock) *natsStore {
	t.Helper()
	n, err := dialNATS(addr, "FW", map[string]time.Duration{"tickets": time.Hour}, nil, quiet())
	if err != nil {
		t.Fatal(err)
	}
	n.redrives = map[string]Redrive{"tickets": {MaxAttempts: 3, Cooldown: time.Minute}}
	n.now = c.now
	t.Cleanup(func() { _ = n.Close() })
	return n
}

func redriveSource() *source {
	return &source{cfg: withDefaults(Source{Name: "tickets", Agent: "triage"}), queue: "fw.jobs.triage"}
}

func deadRecord(t *testing.T, src *source, tk ticket, mutate func(*fw.RunRequest)) []byte {
	t.Helper()
	req := fw.RunRequest{Agent: src.cfg.Agent, Principal: src.cfg.RunAs, PrincipalKind: src.cfg.RunAsKind,
		Metadata: map[string]string{MetaSource: src.cfg.Name, MetaKey: tk.key, MetaGeneration: tk.generation}}
	if mutate != nil {
		mutate(&req)
	}
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	ev, err := fw.NewEvent("", fw.EvIntakeDead, fw.IntakeDeadData{Queue: src.queue, Request: raw, Outcome: fw.DeadFailed})
	if err != nil {
		t.Fatal(err)
	}
	raw, err = json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func readClaim(t *testing.T, n *natsStore, key string) claim {
	t.Helper()
	kv, err := n.bucket(context.Background(), "tickets")
	if err != nil {
		t.Fatal(err)
	}
	e, err := kv.Get(context.Background(), keyID(key))
	if err != nil {
		t.Fatal(err)
	}
	var c claim
	if err := json.Unmarshal(e.Value(), &c); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestRedriveClaims(t *testing.T) {
	srv := runNATS(t)
	clock := &clock{t: time.Now()}
	n := redriveStore(t, srv.ClientURL(), clock)
	ctx := context.Background()
	src := redriveSource()
	first, err := n.Take(ctx, "tickets", "42")
	if err != nil {
		t.Fatal(err)
	}
	// A record of another principal, agent, source, or generation leaves
	// the claim alone.
	for _, mutate := range []func(*fw.RunRequest){
		func(r *fw.RunRequest) { r.Principal = "other" },
		func(r *fw.RunRequest) { r.PrincipalKind = fw.KindUser },
		func(r *fw.RunRequest) { r.Agent = "other" },
		func(r *fw.RunRequest) { r.Metadata[MetaSource] = "other" },
		func(r *fw.RunRequest) { r.Metadata[MetaGeneration] = "old" },
	} {
		if err := n.recordFailure(ctx, src, deadRecord(t, src, first, mutate)); err != nil {
			t.Fatal(err)
		}
		if got := readClaim(t, n, "42"); got.State != statePending {
			t.Fatalf("mismatch changed claim: %+v", got)
		}
	}
	// The dead letter can arrive before Finish, and Finish keeps the failure.
	dead := deadRecord(t, src, first, nil)
	if err := n.recordFailure(ctx, src, dead); err != nil {
		t.Fatal(err)
	}
	failed := readClaim(t, n, "42")
	if err := n.Finish(ctx, first); err != nil {
		t.Fatal(err)
	}
	if got := readClaim(t, n, "42"); got.State != stateFailed {
		t.Fatalf("Finish overwrote failure: %+v", got)
	}
	if _, err := n.Take(ctx, "tickets", "42"); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("cooldown = %v", err)
	}
	clock.add(30 * time.Second)
	if err := n.recordFailure(ctx, src, dead); err != nil {
		t.Fatal(err)
	}
	if got := readClaim(t, n, "42"); !got.FailedAt.Equal(failed.FailedAt) {
		t.Fatal("duplicate reset cooldown")
	}
	clock.add(31 * time.Second)
	// Of many takes by two replicas, exactly one gets the new generation.
	other := redriveStore(t, srv.ClientURL(), clock)
	var won atomic.Int32
	var next ticket
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			st := []*natsStore{n, other}[i%2]
			tk, err := st.Take(ctx, "tickets", "42")
			if err == nil {
				won.Add(1)
				mu.Lock()
				next = tk
				mu.Unlock()
			} else if !errors.Is(err, ErrInFlight) {
				t.Errorf("Take = %v", err)
			}
		}()
	}
	wg.Wait()
	if won.Load() != 1 || next.attempt != 2 || next.generation == first.generation {
		t.Fatalf("wins=%d next=%+v", won.Load(), next)
	}
	// A record, a Finish, or a Release of the old generation leaves the new
	// claim alone.
	if err := n.recordFailure(ctx, src, dead); err != nil {
		t.Fatal(err)
	}
	_ = n.Finish(ctx, first)
	_ = n.Release(ctx, first)
	if got := readClaim(t, n, "42"); got.Generation != next.generation || got.State != statePending {
		t.Fatalf("stale outcome changed claim: %+v", got)
	}
	// A failed publish and a stale pending claim keep the generation and
	// the attempt.
	if err := n.Release(ctx, next); err != nil {
		t.Fatal(err)
	}
	again, err := other.Take(ctx, "tickets", "42")
	if err != nil {
		t.Fatal(err)
	}
	if again.generation != next.generation || again.attempt != 2 {
		t.Fatalf("release lost execution: %+v", again)
	}
	clock.add(staleClaim + time.Second)
	takeover, err := n.Take(ctx, "tickets", "42")
	if err != nil {
		t.Fatal(err)
	}
	if takeover.generation != again.generation || takeover.attempt != 2 {
		t.Fatalf("takeover lost execution: %+v", takeover)
	}
	_ = other.Release(ctx, again)
	if got := readClaim(t, n, "42"); got.State != statePending {
		t.Fatal("stale release changed takeover")
	}
	if err := n.Finish(ctx, takeover); err != nil {
		t.Fatal(err)
	}
	if _, err := n.Take(ctx, "tickets", "42"); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("success duplicate = %v", err)
	}
	// Only a dead letter gives attempt three, and a job that always fails
	// stops there.
	if err := n.recordFailure(ctx, src, deadRecord(t, src, takeover, nil)); err != nil {
		t.Fatal(err)
	}
	clock.add(time.Minute)
	last, err := other.Take(ctx, "tickets", "42")
	if err != nil {
		t.Fatal(err)
	}
	if last.attempt != 3 {
		t.Fatalf("last=%+v", last)
	}
	if err := other.Finish(ctx, last); err != nil {
		t.Fatal(err)
	}
	if err := n.recordFailure(ctx, src, deadRecord(t, src, last, nil)); err != nil {
		t.Fatal(err)
	}
	clock.add(10 * time.Minute)
	for range 5 {
		if _, err := n.Take(ctx, "tickets", "42"); !errors.Is(err, ErrDuplicate) {
			t.Fatalf("poison = %v", err)
		}
	}
	// A restart keeps the used attempts.
	restart := redriveStore(t, srv.ClientURL(), clock)
	if _, err := restart.Take(ctx, "tickets", "42"); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("restart = %v", err)
	}
}

func startOutcomeTestConsumer(t *testing.T, n *natsStore, src *source) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	s := &Sources{store: n, log: quiet(), ctx: ctx}
	s.wg.Add(1)
	go s.consumeOutcomes(src)
	return func() { cancel(); s.wg.Wait() }
}

func TestRedriveDurableConsumerRestart(t *testing.T) {
	srv := runNATS(t)
	c := &clock{t: time.Now()}
	n := redriveStore(t, srv.ClientURL(), c)
	ctx := context.Background()
	_, err := n.js.CreateStream(ctx, jetstream.StreamConfig{Name: "FW", Subjects: []string{"fw.>"}, Storage: jetstream.FileStorage})
	if err != nil {
		t.Fatal(err)
	}
	src := redriveSource()
	tk, err := n.Take(ctx, "tickets", "7")
	if err != nil {
		t.Fatal(err)
	}
	if err := n.Finish(ctx, tk); err != nil {
		t.Fatal(err)
	}
	stop := startOutcomeTestConsumer(t, n, src)
	// The shared consumer exists, and the process stops before the dead
	// letter gets its records.
	durable := redriveDurable(src)
	eventually(t, "consumer exists", func() bool { _, err := n.js.Consumer(ctx, "FW", durable); return err == nil })
	co, err := n.js.Consumer(ctx, "FW", durable)
	if err != nil {
		t.Fatal(err)
	}
	if f := co.CachedInfo().Config.FilterSubject; f != "fw.jobs.triage.dead" {
		t.Fatalf("filter = %q", f)
	}
	stop()
	for _, data := range [][]byte{[]byte("not JSON"), deadRecord(t, src, tk, func(r *fw.RunRequest) { r.Principal = "other" }), deadRecord(t, src, tk, nil)} {
		if _, err := n.js.Publish(ctx, fw.DeadLetterOf(src.queue), data); err != nil {
			t.Fatal(err)
		}
	}
	other := redriveStore(t, srv.ClientURL(), c)
	stopA := startOutcomeTestConsumer(t, n, src)
	defer stopA()
	stopB := startOutcomeTestConsumer(t, other, src)
	defer stopB()
	eventually(t, "failure recorded after restart", func() bool { return readClaim(t, other, "7").State == stateFailed })
	eventually(t, "poison and matched records acknowledged", func() bool {
		co, err := n.js.Consumer(ctx, "FW", durable)
		if err != nil {
			return false
		}
		info, err := co.Info(ctx)
		return err == nil && info.NumAckPending == 0 && info.NumPending == 0
	})
	c.add(time.Minute)
	next, err := other.Take(ctx, "tickets", "7")
	if err != nil {
		t.Fatal(err)
	}
	if next.attempt != 2 || next.generation == tk.generation {
		t.Fatalf("next=%+v", next)
	}
}

func TestRedriveWebhookKernelRetries(t *testing.T) {
	srv := runNATS(t)
	ctx := context.Background()
	cfg := fw.DefaultConfig()
	cfg.Bus.Driver = "nats"
	cfg.Bus.URL = srv.ClientURL()
	bus, err := busnats.Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer bus.Close()
	f := newFront(t, bus, `sources:
 - name: tickets
   agent: triage
   auth: {mode: bearer, secret: KEY}
   key: "{{.id}}"
   redrive: {maxAttempts: 2, cooldown: 1m}
`, secrets("KEY", "secret"), func(c *fw.Config) { c.Bus.Driver = "nats"; c.Bus.URL = srv.ClientURL() })
	clock := &clock{t: time.Now()}
	f.s.store.(*natsStore).now = clock.now
	wc := fw.DefaultConfig()
	wc.Home = t.TempDir()
	wc.Bus.Driver = "nats"
	wc.Bus.URL = srv.ClientURL()
	wc.Bus.Intake = "fw.jobs.triage"
	wc.Bus.IntakeRetries = 1
	wc.Agents = map[string]fw.AgentProfile{"triage": {}}
	wb, err := busnats.Open(ctx, wc)
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	worker, err := fw.New(fw.WithConfig(wc), fw.WithoutDefaults(), fw.WithAuthn(fw.NoAuthn{}), fw.WithModel(fw.ModelFunc{StreamFn: func(context.Context, *fw.Request, func(fw.Delta)) (*fw.Response, error) {
		if calls.Add(1) <= 2 {
			return nil, errors.New("terminal test failure")
		}
		return &fw.Response{Content: []fw.Block{fw.Text("done")}, StopReason: fw.StopEndTurn}, nil
	}}), fw.WithLedger(fw.NewMemLedger()), fw.WithBus(wb), fw.WithLogger(quiet()))
	if err != nil {
		t.Fatal(err)
	}
	defer worker.Close(ctx)
	url := f.url + "/v1/sources/tickets"
	resp, body := post(t, url, `{"id":42}`, "Authorization", "Bearer secret")
	if resp.StatusCode != 202 {
		t.Fatalf("first: %d %s", resp.StatusCode, body)
	}
	n := f.s.store.(*natsStore)
	eventually(t, "kernel retry exhausted and durable failure consumed", func() bool { return readClaim(t, n, "42").State == stateFailed })
	runs := runsOf(t, worker)
	if len(runs) != 2 {
		t.Fatalf("retry runs=%d", len(runs))
	}
	generation := runs[0].Metadata[MetaGeneration]
	if generation == "" || runs[1].Metadata[MetaGeneration] != generation {
		t.Fatal("kernel retry lost generation")
	}
	clock.add(time.Minute)
	// The credential is checked before the claim, also for a failed ticket.
	resp, _ = post(t, url, `{"id":42}`, "Authorization", "Bearer wrong")
	if resp.StatusCode != 401 {
		t.Fatalf("unauthorized redrive=%d", resp.StatusCode)
	}
	resp, body = post(t, url, `{"id":42}`, "Authorization", "Bearer secret")
	if resp.StatusCode != 202 {
		t.Fatalf("redrive: %d %s", resp.StatusCode, body)
	}
	eventually(t, "redelivery actually executes rather than kernel dedup", func() bool { return calls.Load() == 3 })
	eventually(t, "redrive finished", func() bool {
		for _, r := range runsOf(t, worker) {
			if r.State == fw.StateCompleted {
				return true
			}
		}
		return false
	})
	current := readClaim(t, n, "42")
	if current.Attempt != 2 || current.Generation == generation {
		t.Fatalf("redrive=%+v", current)
	}
	for range 3 {
		resp, _ = post(t, url, `{"id":42}`, "Authorization", "Bearer secret")
		if resp.StatusCode != 200 {
			t.Fatal(resp.StatusCode)
		}
	}
	if calls.Load() != 3 {
		t.Fatalf("success duplicate executed: %d", calls.Load())
	}
	for _, r := range runsOf(t, worker) {
		if r.Metadata[MetaSource] != "tickets" || r.Metadata[MetaKey] != "42" || r.Principal != "source:tickets" {
			t.Fatalf("identity changed: %+v", r)
		}
	}
}
