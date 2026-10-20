package fleet

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/nats-io/nats.go/jetstream"
)

// memJobs is a jobStore in memory, for tests that stand for a shared
// store.
type memJobs struct {
	mu   sync.Mutex
	recs map[jobKey]jobRecord
}

func newMemJobs() *memJobs { return &memJobs{recs: map[jobKey]jobRecord{}} }

func (m *memJobs) addJobs(_ context.Context, k jobKey, queue string, ids []string, now time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	rec := m.recs[k]
	c := jobRecord{Queues: map[string][]string{}, UpdatedAt: rec.UpdatedAt}
	for q, have := range rec.Queues {
		c.Queues[q] = append([]string(nil), have...)
	}
	c.add(queue, ids, now)
	m.recs[k] = c
	return nil
}

func (m *memJobs) jobs(_ context.Context, k jobKey) (jobRecord, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	rec, ok := m.recs[k]
	return rec, ok, nil
}

// jobRequests are the run requests of n items of a job as a person, with
// the idempotency keys of the tool enqueue.
func jobRequests(batch, principal string, n int) []fw.RunRequest {
	out := make([]fw.RunRequest, n)
	for i := range out {
		out[i] = fw.RunRequest{Input: fmt.Sprint("item ", i+1), Principal: principal, PrincipalKind: fw.KindUser,
			Metadata: map[string]string{fw.MetaBatch: batch}, IdempotencyKey: fmt.Sprint(batch, "/", i+1)}
	}
	return out
}

// jobRun is the start of a run of a job that a worker took from the queue:
// a top-level run as the person, or, with a parent, a child of such a run.
func jobRun(batch, principal, parent string) []fw.Event {
	d := fw.RunQueuedData{Agent: "default", Input: "item", Origin: fw.OriginBus, Principal: principal, Metadata: map[string]string{fw.MetaBatch: batch}}
	if parent != "" {
		d.Parent, d.Depth, d.Origin = parent, 1, fw.OriginTool
	}
	return []fw.Event{fw.MustEvent("", fw.EvRunQueued, d), fw.MustEvent("", fw.EvRunStarted, fw.RunStartedData{Model: "fake/m"})}
}

// The front counts the jobs of a batch that no Task took yet: the jobs
// that it published less the top-level runs of the person that a queue
// started. With a shared job record, every replica counts the same jobs.
func TestBatchQueuedJobs(t *testing.T) {
	t.Setenv("FW_ATESPACE", "")
	t.Setenv(EnvIntake, "fw.jobs")
	withBusServer(t, nil, newMemJobs())
	bus := fw.NewMemBus()
	intake := func(_ *fw.Config, fc *Config) { fc.Intake = "fw.jobs" }
	a := newMember(t, bus, "front-a", intake)
	b := newMember(t, bus, "front-b", intake)
	if _, sent, err := Enqueue(as("alice"), a.a, "default", jobRequests("job", "alice", 3)); err != nil || sent != 3 {
		t.Fatalf("Enqueue = %d, %v", sent, err)
	}
	for _, m := range []*member{a, b} {
		br, err := m.f.Batch(as("alice"), "job")
		if err != nil || br.Jobs != 3 || br.Queued != 3 || !br.JobsShared {
			t.Fatalf("batch on %s = %+v, %v", m.f.Name(), br, err)
		}
	}
	// Another person learns nothing of the jobs of alice under the name.
	if br, _ := a.f.Batch(as("bob"), "job"); br.Jobs != 0 || br.Queued != 0 {
		t.Fatalf("batch as bob = %+v", br)
	}
	// The same job sent again counts once.
	if _, _, err := Enqueue(as("alice"), b.a, "default", jobRequests("job", "alice", 3)); err != nil {
		t.Fatal(err)
	}
	// A worker takes one job; its child run, a run of bob under the same
	// name, and a run outside the job take no job of alice.
	stream(t, a.f, "01TAKEN", []uint64{1, 2}, jobRun("job", "alice", "")...)
	stream(t, a.f, "01CHILD", []uint64{1, 2}, jobRun("job", "alice", "01TAKEN")...)
	stream(t, a.f, "01BOB", []uint64{1, 2}, jobRun("job", "bob", "")...)
	stream(t, a.f, "01ELSE", []uint64{1, 2}, jobRun("else", "alice", "")...)
	br, err := a.f.Batch(as("alice"), "job")
	if err != nil || br.Jobs != 3 || br.Queued != 2 {
		t.Fatalf("batch after one job = %+v, %v", br, err)
	}
}

// Without a shared record, each replica counts the jobs that it sent.
func TestBatchQueuedJobsPerReplica(t *testing.T) {
	t.Setenv("FW_ATESPACE", "")
	t.Setenv(EnvIntake, "fw.jobs")
	withBusServer(t, nil, nil)
	bus := fw.NewMemBus()
	intake := func(_ *fw.Config, fc *Config) { fc.Intake = "fw.jobs" }
	a := newMember(t, bus, "front-a", intake)
	b := newMember(t, bus, "front-b", intake)
	if _, _, err := Enqueue(as("alice"), a.a, "default", jobRequests("job", "alice", 2)); err != nil {
		t.Fatal(err)
	}
	if br, _ := a.f.Batch(as("alice"), "job"); br.Jobs != 2 || br.Queued != 2 || br.JobsShared {
		t.Fatalf("batch on the replica that sent = %+v", br)
	}
	if br, _ := b.f.Batch(as("alice"), "job"); br.Jobs != 0 || br.Queued != 0 {
		t.Fatalf("batch on the other replica = %+v", br)
	}
}

// Once the front read the queues after the last enqueue, the queued jobs
// are never more than the jobs that the queues hold: a job that left the
// queue without a run that the view knows waits nowhere.
func TestBatchQueuedJobsBoundByTheQueue(t *testing.T) {
	t.Setenv("FW_ATESPACE", "")
	q := &fakeQueues{pending: func(int) map[string]uint64 { return map[string]uint64{"fw.jobs.default": 0} },
		acked: map[string]uint64{"fw.jobs.default": 1}}
	withBusServer(t, q, nil)
	front := newMember(t, fw.NewMemBus(), "front", func(_ *fw.Config, fc *Config) { fc.Intake = "fw.jobs" })
	k := jobKey{batch: "job", subject: "alice"}
	front.f.jobs.add(context.Background(), k, "fw.jobs.default", []string{"1", "2", "3"}, time.Now().Add(-time.Minute))
	waitReads(t, q, 2)
	if br, _ := front.f.Batch(as("alice"), "job"); br.Jobs != 3 || br.Queued != 1 {
		t.Fatalf("batch = %+v, want 3 jobs and 1 in the queue", br)
	}
	// A new enqueue lifts the bound until the front reads the queues again.
	front.f.jobs.add(context.Background(), k, "fw.jobs.default", []string{"4"}, time.Now())
	if br, _ := front.f.Batch(as("alice"), "job"); br.Jobs != 4 || br.Queued != 4 {
		t.Fatalf("batch after an enqueue = %+v, want 4 and 4", br)
	}
}

// fakeEntry is one entry of fakeBucket.
type fakeEntry struct {
	key   string
	value []byte
	rev   uint64
}

func (e fakeEntry) Bucket() string                  { return JobsBucket }
func (e fakeEntry) Key() string                     { return e.key }
func (e fakeEntry) Value() []byte                   { return e.value }
func (e fakeEntry) Revision() uint64                { return e.rev }
func (e fakeEntry) Created() time.Time              { return time.Time{} }
func (e fakeEntry) Delta() uint64                   { return 0 }
func (e fakeEntry) Operation() jetstream.KeyValueOp { return jetstream.KeyValuePut }

// fakeBucket is a key-value bucket with revisions. race, when set, runs
// once before the next write, as another replica that writes first.
type fakeBucket struct {
	mu    sync.Mutex
	items map[string]fakeEntry
	rev   uint64
	race  func()
}

func (b *fakeBucket) Get(_ context.Context, key string) (jetstream.KeyValueEntry, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	e, ok := b.items[key]
	if !ok {
		return nil, jetstream.ErrKeyNotFound
	}
	return e, nil
}

func (b *fakeBucket) Create(ctx context.Context, key string, value []byte, _ ...jetstream.KVCreateOpt) (uint64, error) {
	b.racer()
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.items[key]; ok {
		return 0, jetstream.ErrKeyExists
	}
	b.rev++
	b.items[key] = fakeEntry{key: key, value: value, rev: b.rev}
	return b.rev, nil
}

func (b *fakeBucket) Update(_ context.Context, key string, value []byte, rev uint64) (uint64, error) {
	b.racer()
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.items[key].rev != rev {
		return 0, jetstream.ErrKeyRevisionMismatch
	}
	b.rev++
	b.items[key] = fakeEntry{key: key, value: value, rev: b.rev}
	return b.rev, nil
}

func (b *fakeBucket) racer() {
	b.mu.Lock()
	race := b.race
	b.race = nil
	b.mu.Unlock()
	if race != nil {
		race()
	}
}

// A write of a job record that another replica raced reads the record
// again and keeps the jobs of both.
func TestJobRecordInTheBucket(t *testing.T) {
	ctx := context.Background()
	b := &fakeBucket{items: map[string]fakeEntry{}}
	k := jobKey{batch: "job one/ü", subject: "alice@example.com"}
	now := time.Now().UTC()
	if err := kvAddJobs(ctx, b, k, "fw.jobs.default", []string{"1", "2"}, now); err != nil {
		t.Fatal(err)
	}
	// Another replica adds a job between the read and the write, twice.
	b.race = func() {
		if err := kvAddJobs(ctx, b, k, "fw.jobs.default", []string{"3"}, now); err != nil {
			t.Error(err)
		}
	}
	if err := kvAddJobs(ctx, b, k, "fw.jobs.other", []string{"1"}, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	rec, ok, err := kvJobs(ctx, b, k)
	if err != nil || !ok || rec.count() != 4 || len(rec.Queues["fw.jobs.default"]) != 3 || !rec.UpdatedAt.Equal(now.Add(time.Second)) {
		t.Fatalf("record = %+v, %v, %v", rec, ok, err)
	}
	// The same jobs again change nothing.
	before := b.rev
	if err := kvAddJobs(ctx, b, k, "fw.jobs.default", []string{"1", "3"}, now); err != nil || b.rev != before {
		t.Fatalf("a repeated write = %v, revision %d, want %d", err, b.rev, before)
	}
	if _, ok, err := kvJobs(ctx, b, jobKey{batch: "job one/ü", subject: "bob"}); ok || err != nil {
		t.Fatalf("the record of bob = %v, %v", ok, err)
	}
}
