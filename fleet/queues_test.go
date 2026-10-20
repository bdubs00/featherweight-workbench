package fleet

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/surface/view"
)

// fakeQueues stands for the server of a NATS bus: the consumers of the
// stream, its filters, and the messages that it stores. pending, when set,
// gives the jobs that wait in each queue at each read, from the number of
// the read.
type fakeQueues struct {
	mu      sync.Mutex
	filters []string
	stored  map[string]uint64
	pending func(read int) map[string]uint64
	acked   map[string]uint64
	last    map[string]time.Time
	reads   int
	lasts   int
}

func (q *fakeQueues) snapshot(context.Context, string) (queueSnapshot, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.reads++
	snap := queueSnapshot{filters: q.filters, stored: map[string]uint64{}}
	for k, v := range q.stored {
		snap.stored[k] = v
	}
	if q.pending != nil {
		for queue, n := range q.pending(q.reads) {
			snap.consumers = append(snap.consumers, consumerState{name: "fw-" + strings.ReplaceAll(queue, ".", "-"), filters: []string{queue}, pending: n, ackPending: q.acked[queue]})
		}
	}
	return snap, nil
}

func (q *fakeQueues) lastTime(_ context.Context, subject string) (time.Time, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.lasts++
	return q.last[subject], nil
}

func (q *fakeQueues) set(f func(q *fakeQueues)) {
	q.mu.Lock()
	defer q.mu.Unlock()
	f(q)
}

func (q *fakeQueues) readCount() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.reads
}

// withBusServer makes the fronts of a test read the server of the bus
// through src.
func withBusServer(t *testing.T, src queueSource) {
	t.Helper()
	old := openBusServer
	openBusServer = func(context.Context, *fw.Config, *slog.Logger) (queueSource, func(), error) {
		return src, nil, nil
	}
	t.Cleanup(func() { openBusServer = old })
}

// emit applies one event of a run on a member to the view, as the egress
// stream delivers it.
func emit(t *testing.T, f *Fleet, node, id string, seq uint64, typ string, data any) {
	t.Helper()
	e := fw.MustEvent(id, typ, data)
	e.Seq, e.Node, e.Source = seq, node, "fw://team/reviewer/"+id
	if err := f.platform.onEvent(e); err != nil {
		t.Fatal(err)
	}
}

// queueFront is a front that reads the Tasks alpha, which runs, and beta,
// which sleeps, and the queues of the intake prefix fw.jobs. Both Tasks
// serve the profile reviewer: alpha says so when the front reads it, and
// the Workspace of beta names it.
func queueFront(t *testing.T, wake bool) (*member, *fakeAX, *fakeTasks) {
	t.Helper()
	t.Setenv("FW_ATESPACE", "team")
	t.Setenv("FW_API_TOKEN", "fleet-secret")
	ax := newFakeAX(t, map[string]map[string]string{"team": {"alpha": "Running", "beta": "Suspended"}})
	ax.workspaces = map[string]string{"team/beta": "agents:\n  reviewer: {}\n"}
	tasks := newFakeTasks(t)
	long := time.Now().Add(-time.Hour)
	tasks.set("team/alpha", view.Status{Idle: true, IdleSince: &long})
	tasks.set("team/beta", view.Status{Idle: true, IdleSince: &long})
	off := time.Duration(0)
	front := newMember(t, fw.NewMemBus(), "front", func(cfg *fw.Config, fc *Config) {
		fc.Atespaces = []string{"team"}
		fc.Intake = "fw.jobs"
		fc.Interval = 40 * time.Millisecond
		fc.SuspendAfter = &off
		fc.WakeOnQueue = &wake
		cfg.AX.Server = ax.addr
		cfg.AX.Router = strings.TrimPrefix(tasks.srv.URL, "http://")
	})
	eventually(t, "the front knows what both Tasks serve", func() bool {
		p := front.f.platform
		p.mu.Lock()
		defer p.mu.Unlock()
		a, b := p.members["team/alpha"], p.members["team/beta"]
		return a != nil && b != nil && a.agents["reviewer"] && b.agents["reviewer"]
	})
	return front, ax, tasks
}

// waitReads waits until the front read the queues n more times.
func waitReads(t *testing.T, q *fakeQueues, n int) {
	t.Helper()
	want := q.readCount() + n
	eventually(t, "the front reads the queues", func() bool { return q.readCount() >= want })
}

// Jobs that wait in a queue wake the sleeping Tasks that serve the profile
// once no awake Task of the profile has room, with no enqueue of the front.
// While an awake Task has room and takes the jobs, the sleeping Task
// sleeps on.
func TestWakeOnQueueDepth(t *testing.T) {
	q := &fakeQueues{}
	withBusServer(t, q)
	front, ax, tasks := queueFront(t, true)

	// Nothing waits: nobody wakes.
	q.set(func(q *fakeQueues) {
		q.pending = func(int) map[string]uint64 { return map[string]uint64{"fw.jobs.reviewer": 0} }
	})
	waitReads(t, q, 3)
	if n := tasks.count("team/beta"); n != 0 {
		t.Fatalf("an empty queue woke beta: %d reads", n)
	}
	// Jobs wait, and alpha is awake with no run that works: alpha takes
	// one job per read, so beta sleeps on.
	q.set(func(q *fakeQueues) {
		start := q.reads
		q.pending = func(read int) map[string]uint64 {
			return map[string]uint64{"fw.jobs.reviewer": uint64(max(100-(read-start), 1))}
		}
	})
	waitReads(t, q, 6)
	if n := tasks.count("team/beta"); n != 0 {
		t.Fatalf("beta woke while alpha had room: %d reads", n)
	}
	// alpha works on a run, so it has no room, and the jobs that wait wake
	// beta: one status read through the router, and the phase follows.
	emit(t, front.f, "ax:team/alpha", "01ALPHA", 1, fw.EvRunQueued, fw.RunQueuedData{Agent: "reviewer", Input: "job", Origin: fw.OriginBus, Principal: "ada"})
	emit(t, front.f, "ax:team/alpha", "01ALPHA", 2, fw.EvRunStarted, fw.RunStartedData{Model: "fake/m"})
	q.set(func(q *fakeQueues) {
		q.pending = func(int) map[string]uint64 { return map[string]uint64{"fw.jobs.reviewer": 3} }
	})
	eventually(t, "the jobs that wait wake beta", func() bool { return tasks.count("team/beta") == 1 })
	eventually(t, "the phase of beta follows", func() bool { return ax.phase("team", "beta") == "Running" })
	// beta is awake now and takes the jobs; no other wake follows.
	waitReads(t, q, 8)
	if n := tasks.count("team/beta"); n != 1 {
		t.Fatalf("beta was woken %d times, want once", n)
	}
	if n := tasks.count("team/alpha"); n != 0 {
		t.Fatalf("the awake alpha got %d status reads", n)
	}
}

// When no job leaves the queue for stallIntervals reads, the front wakes
// the sleeping Tasks even though an awake Task seems to have room, because
// that Task does not take the jobs.
func TestWakeOnStalledQueue(t *testing.T) {
	q := &fakeQueues{}
	withBusServer(t, q)
	_, _, tasks := queueFront(t, true)
	q.set(func(q *fakeQueues) {
		q.pending = func(int) map[string]uint64 { return map[string]uint64{"fw.jobs.reviewer": 5} }
	})
	waitReads(t, q, 1)
	if n := tasks.count("team/beta"); n != 0 {
		t.Fatalf("beta woke at the first read, while alpha had room: %d reads", n)
	}
	eventually(t, "a stalled queue wakes beta", func() bool { return tasks.count("team/beta") == 1 })
}

// With workbench.fleet.wakeOnQueue off, the front reads the queues and
// wakes no Task for them.
func TestWakeOnQueueOff(t *testing.T) {
	q := &fakeQueues{pending: func(int) map[string]uint64 { return map[string]uint64{"fw.jobs.reviewer": 5} }}
	withBusServer(t, q)
	front, _, tasks := queueFront(t, false)
	emit(t, front.f, "ax:team/alpha", "01ALPHA", 1, fw.EvRunQueued, fw.RunQueuedData{Agent: "reviewer", Input: "job", Origin: fw.OriginBus, Principal: "ada"})
	emit(t, front.f, "ax:team/alpha", "01ALPHA", 2, fw.EvRunStarted, fw.RunStartedData{Model: "fake/m"})
	waitReads(t, q, 3*stallIntervals)
	if n := tasks.count("team/beta"); n != 0 {
		t.Fatalf("beta woke with the wake on queue depth off: %d reads", n)
	}
	states, _ := front.f.queueStates()
	if st := states["fw.jobs.reviewer"]; !st.depthKnown || st.pending != 5 {
		t.Fatalf("queue state = %+v", st)
	}
}

// depthsOf takes the depth of a queue from its consumer, or from the
// messages that the stream stores when no worker ever took the queue, and
// counts the dead letters when the stream captures their subject.
func TestDepthsOf(t *testing.T) {
	snap := queueSnapshot{
		filters: []string{"fw.>"},
		stored:  map[string]uint64{"fw.jobs.a": 9, "fw.jobs.b": 4, "fw.jobs.a.dead": 2},
		consumers: []consumerState{
			{name: "fw-fw-jobs-a", filters: []string{"fw.jobs.a"}, pending: 3, ackPending: 1},
			{name: "fw-other", filters: []string{"fw.jobs.a", "fw.jobs.b"}, pending: 50},
		},
	}
	d := depthsOf(snap, []string{"fw.jobs.a", "fw.jobs.b"})
	if a := d["fw.jobs.a"]; !a.consumer || a.pending != 3 || a.ackPending != 1 || !a.deadStored || a.dead != 2 {
		t.Fatalf("a = %+v", a)
	}
	if b := d["fw.jobs.b"]; b.consumer || b.pending != 4 || b.ackPending != 0 || !b.deadStored || b.dead != 0 {
		t.Fatalf("b = %+v", b)
	}
	// A stream that captures the queues alone holds no dead letter.
	snap.filters = []string{"fw.jobs.a", "fw.jobs.b"}
	if a := depthsOf(snap, []string{"fw.jobs.a"})["fw.jobs.a"]; a.deadStored || a.dead != 0 {
		t.Fatalf("a without a dead-letter filter = %+v", a)
	}
}

// A stream that stores the dead letters gives their count and the time of
// the last one, which the front reads again only when the count changes.
func TestDeadLettersFromTheStream(t *testing.T) {
	t.Setenv("FW_ATESPACE", "")
	first := time.Date(2026, 10, 19, 3, 4, 5, 0, time.UTC)
	q := &fakeQueues{filters: []string{"fw.>"}, stored: map[string]uint64{"fw.jobs.default.dead": 2},
		last: map[string]time.Time{"fw.jobs.default.dead": first}}
	withBusServer(t, q)
	front := newMember(t, fw.NewMemBus(), "front", func(_ *fw.Config, fc *Config) { fc.Intake = "fw.jobs" })
	state := func() queueState {
		states, _ := front.f.queueStates()
		return states["fw.jobs.default"]
	}
	eventually(t, "the front counts the stored dead letters", func() bool {
		st := state()
		return st.deadStored && st.dead == 2 && st.deadAt.Equal(first)
	})
	waitReads(t, q, 3)
	q.mu.Lock()
	lasts := q.lasts
	q.mu.Unlock()
	if lasts != 1 {
		t.Fatalf("the front read the last dead letter %d times for one count", lasts)
	}
	second := first.Add(time.Minute)
	q.set(func(q *fakeQueues) {
		q.stored["fw.jobs.default.dead"] = 3
		q.last["fw.jobs.default.dead"] = second
	})
	eventually(t, "a new dead letter moves the count and the time", func() bool {
		st := state()
		return st.dead == 3 && st.deadAt.Equal(second)
	})
}

// Without a stream that stores them, the front counts the dead letters
// that reach its subscription.
func TestDeadLettersBySubscription(t *testing.T) {
	t.Setenv("FW_ATESPACE", "")
	withBusServer(t, nil)
	bus := fw.NewMemBus()
	front := newMember(t, bus, "front", func(_ *fw.Config, fc *Config) { fc.Intake = "fw.jobs" })
	eventually(t, "the front subscribes to the dead letters", func() bool {
		states, _ := front.f.queueStates()
		st, ok := states["fw.jobs.default"]
		return ok && st.deadKnown && !st.depthKnown
	})
	at := time.Date(2026, 10, 19, 3, 4, 5, 0, time.UTC)
	for i := 0; i < 2; i++ {
		e := fw.MustEvent("01RUN", "fw.job.dead", map[string]string{"reason": "the run failed on its last attempt"})
		e.Time = at.Add(time.Duration(i) * time.Second)
		if err := bus.Publish(context.Background(), "fw.jobs.default.dead", e); err != nil {
			t.Fatal(err)
		}
	}
	eventually(t, "the front counts both", func() bool {
		states, _ := front.f.queueStates()
		st := states["fw.jobs.default"]
		return st.dead == 2 && st.deadAt.Equal(at.Add(time.Second)) && !st.deadStored
	})
}

// The wake on queue depth is on by default with atespaces and an intake
// prefix, off without them, and refused when it is on without them.
func TestWakeOnQueueConfig(t *testing.T) {
	for _, e := range []string{EnvAtespaces, EnvEgress, EnvName, EnvSuspendAfter, EnvIntake, EnvWakeOnQueue} {
		t.Setenv(e, "")
	}
	t.Setenv("FW_ATESPACE", "")
	cfg := fw.DefaultConfig()
	t.Setenv(EnvAtespaces, "team")
	t.Setenv(EnvIntake, "fw.jobs")
	if c, err := Load(cfg); err != nil || !c.wakeOnQueue() {
		t.Fatalf("default = %v, %v", c.wakeOnQueue(), err)
	}
	t.Setenv(EnvWakeOnQueue, "off")
	if c, err := Load(cfg); err != nil || c.wakeOnQueue() {
		t.Fatalf("off = %v, %v", c.wakeOnQueue(), err)
	}
	t.Setenv(EnvWakeOnQueue, "sometimes")
	if _, err := Load(cfg); err == nil || !strings.Contains(err.Error(), EnvWakeOnQueue) {
		t.Fatalf("a bad value = %v", err)
	}
	t.Setenv(EnvWakeOnQueue, "")
	t.Setenv(EnvIntake, "")
	if c, err := Load(cfg); err != nil || c.wakeOnQueue() {
		t.Fatalf("without an intake prefix = %v, %v", c.wakeOnQueue(), err)
	}
	t.Setenv(EnvWakeOnQueue, "on")
	if _, err := Load(cfg); err == nil || !strings.Contains(err.Error(), "wakeOnQueue needs") {
		t.Fatalf("on without an intake prefix = %v", err)
	}
}

// fw doctor says whether the front reads the queues and wakes the Tasks
// for the jobs that wait in them.
func TestDoctorNamesTheQueues(t *testing.T) {
	for _, e := range []string{EnvAtespaces, EnvEgress, EnvName, EnvSuspendAfter, EnvIntake, EnvWakeOnQueue} {
		t.Setenv(e, "")
	}
	t.Setenv("FW_ATESPACE", "")
	t.Setenv(EnvAtespaces, "team")
	t.Setenv(EnvIntake, "fw.jobs")
	cfg := fw.DefaultConfig()
	cfg.Bus.Driver = "nats"
	ok, line := DoctorCheck(context.Background(), cfg, fw.EnvSecrets{}, time.Second)
	if !ok || !strings.Contains(line, "reads the depth and the dead letters of each queue from the nats server") || !strings.Contains(line, "wakes the sleeping Tasks for the jobs that wait in a queue") {
		t.Fatalf("doctor on nats = %v %q", ok, line)
	}
	cfg.Bus.Driver = "redis"
	t.Setenv(EnvWakeOnQueue, "off")
	ok, line = DoctorCheck(context.Background(), cfg, fw.EnvSecrets{}, time.Second)
	if !ok || !strings.Contains(line, "reads no queue depth, because the bus is redis") || !strings.Contains(line, "after its own enqueue only") {
		t.Fatalf("doctor on redis = %v %q", ok, line)
	}
}
