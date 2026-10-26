package fleet

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/internal/bucket"
)

// useIndex makes the fronts of a test keep the index of the runs in b, so
// two fronts over one bucket act as a front before and after a restart.
func useIndex(t *testing.T, b bucket.Bucket) {
	t.Helper()
	old := OpenIndex
	OpenIndex = func(context.Context, *fw.Config, *slog.Logger) (bucket.Bucket, func(), error) { return b, nil, nil }
	t.Cleanup(func() { OpenIndex = old })
}

// countingBucket counts the writes of a bucket. While hang is set, every
// call waits until release closes or its context ends, as a bucket that
// does not answer.
type countingBucket struct {
	bucket.Bucket
	writes atomic.Int64

	mu      sync.Mutex
	hang    bool
	release chan struct{}
}

func (c *countingBucket) wait(ctx context.Context) error {
	c.mu.Lock()
	hang, release := c.hang, c.release
	c.mu.Unlock()
	if !hang {
		return nil
	}
	select {
	case <-release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *countingBucket) Get(ctx context.Context, key string) (bucket.Entry, error) {
	if err := c.wait(ctx); err != nil {
		return bucket.Entry{}, err
	}
	return c.Bucket.Get(ctx, key)
}

func (c *countingBucket) Keys(ctx context.Context) ([]bucket.Entry, error) {
	if err := c.wait(ctx); err != nil {
		return nil, err
	}
	return c.Bucket.Keys(ctx)
}

func (c *countingBucket) Create(ctx context.Context, key string, value []byte) (uint64, error) {
	if err := c.wait(ctx); err != nil {
		return 0, err
	}
	c.writes.Add(1)
	return c.Bucket.Create(ctx, key, value)
}

func (c *countingBucket) Update(ctx context.Context, key string, value []byte, last uint64) (uint64, error) {
	if err := c.wait(ctx); err != nil {
		return 0, err
	}
	c.writes.Add(1)
	return c.Bucket.Update(ctx, key, value, last)
}

func (c *countingBucket) Delete(ctx context.Context, key string, last uint64) error {
	if err := c.wait(ctx); err != nil {
		return err
	}
	c.writes.Add(1)
	return c.Bucket.Delete(ctx, key, last)
}

// entry reads the entry of a run from the index, or false.
func (c *countingBucket) entry(t *testing.T, id string) (indexEntry, bool) {
	t.Helper()
	e, err := c.Bucket.Get(context.Background(), id)
	if errors.Is(err, bucket.ErrNotFound) {
		return indexEntry{}, false
	}
	if err != nil {
		t.Fatal(err)
	}
	var out indexEntry
	if err := json.Unmarshal(e.Value, &out); err != nil {
		t.Fatal(err)
	}
	return out, true
}

// put writes an entry into the index, as an earlier front did.
func (c *countingBucket) put(t *testing.T, id string, e indexEntry) {
	t.Helper()
	b, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Bucket.Create(context.Background(), id, b); err != nil {
		t.Fatal(err)
	}
}

// taskRouter plays the atenet router for Tasks: each actor lists its runs,
// takes a decision, and every request is recorded.
type taskRouter struct {
	srv  *httptest.Server
	mu   sync.Mutex
	runs map[string][]*fw.Run // by actor
	reqs []string             // "<actor> <method> <path>"
}

func newTaskRouter(t *testing.T) *taskRouter {
	t.Helper()
	tr := &taskRouter{runs: map[string][]*fw.Run{}}
	tr.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		actor := r.Header.Get("ate-target-actor")
		tr.mu.Lock()
		tr.reqs = append(tr.reqs, actor+" "+r.Method+" "+r.URL.Path)
		runs := tr.runs[actor]
		tr.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/runs":
			_ = json.NewEncoder(w).Encode(map[string]any{"runs": runs})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/agents":
			_ = json.NewEncoder(w).Encode(map[string]any{"agents": []map[string]string{{"name": "default"}}})
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/approvals/"):
			id := strings.Split(strings.TrimPrefix(r.URL.Path, "/v1/runs/"), "/")[0]
			_ = json.NewEncoder(w).Encode(fw.Run{ID: id, State: fw.StateRunning})
		default:
			http.Error(w, `{"detail":"unexpected"}`, http.StatusNotFound)
		}
	}))
	t.Cleanup(tr.srv.Close)
	return tr
}

func (tr *taskRouter) requests(actor string) []string {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	var out []string
	for _, r := range tr.reqs {
		if strings.HasPrefix(r, actor+" ") {
			out = append(out, strings.TrimPrefix(r, actor+" "))
		}
	}
	return out
}

// indexFront is a front that reads the Tasks of the atespace team from ax
// and forwards through the router tr.
func indexFront(t *testing.T, bus fw.Bus, ax *fakeAX, tr *taskRouter, mut func(*Config)) *member {
	t.Helper()
	return newMember(t, bus, "front", func(cfg *fw.Config, fc *Config) {
		fc.Atespaces = []string{"team"}
		fc.Egress = "fw.events"
		cfg.AX.Server = ax.addr
		cfg.AX.Router = strings.TrimPrefix(tr.srv.URL, "http://")
		if mut != nil {
			mut(fc)
		}
	})
}

// A front writes one entry per change of the state of a run of a Task,
// not one per event. After a restart, while the Task sleeps, the new front
// finds the run in the index with its last known state, a decision routes
// to the Task and wakes it, an event of the Task confirms the run, and
// the entry leaves the index once the finished run expires.
func TestIndexKeepsTheRunsOfASleepingTask(t *testing.T) {
	t.Setenv("FW_ATESPACE", "front")
	t.Setenv("FW_API_TOKEN", "fleet-secret")
	idx := &countingBucket{Bucket: bucket.NewMem(IndexBucket)}
	useIndex(t, idx)
	bus := fw.NewMemBus()
	ax := newFakeAX(t, map[string]map[string]string{"team": {"alpha": "Running"}})
	tr := newTaskRouter(t)
	front := indexFront(t, bus, ax, tr, nil)
	eventually(t, "the first front settles", front.f.Settled)

	const node, id = "ax:team/alpha", "01M5HE6R06YW28JTWG5M7W6H86"
	emit(t, front.f, node, id, 1, fw.EvRunQueued, fw.RunQueuedData{Agent: "default", Input: "Migrate cron", Principal: "ada", Metadata: map[string]string{fw.MetaBatch: "bridge:b1"}})
	emit(t, front.f, node, id, 2, fw.EvRunStarted, fw.RunStartedData{Model: "fake/m"})
	emit(t, front.f, node, id, 3, fw.EvTurnStarted, fw.TurnData{Turn: 1})
	emit(t, front.f, node, id, 4, fw.EvApprovalRequested, fw.ApprovalRequestedData{ApprovalID: "ap1", Tool: "bash", Reason: "rule: ask bash: git push*"})
	eventually(t, "the index holds the run as it waits", func() bool {
		e, ok := idx.entry(t, id)
		return ok && e.State == fw.StateInputRequired && e.Pending != nil && e.Pending.ID == "ap1" && e.Seq == 4
	})
	e, _ := idx.entry(t, id)
	if e.Member != "team/alpha" || e.Agent != "default" || e.Principal != "ada" || e.Batch != "bridge:b1" || e.Pending.Tool != "bash" {
		t.Fatalf("entry = %+v", e)
	}
	if raw, _ := idx.Bucket.Get(context.Background(), id); strings.Contains(string(raw.Value), "Migrate cron") {
		t.Fatalf("the index holds the input of the run: %s", raw.Value)
	}
	// Events that change no state write nothing.
	writes := idx.writes.Load()
	if writes > 3 {
		t.Fatalf("%d writes for three states", writes)
	}
	for seq := uint64(5); seq <= 9; seq++ {
		emit(t, front.f, node, id, seq, fw.EvRunAnnotated, map[string]string{"title": "Migrate cron"})
	}
	time.Sleep(100 * time.Millisecond)
	if idx.writes.Load() != writes {
		t.Fatalf("annotations wrote %d entries", idx.writes.Load()-writes)
	}

	// The Task sleeps, and the front starts again.
	ax.set("team", "alpha", "Suspended")
	eventually(t, "the first front sees the Task sleep", func() bool {
		ms, _ := front.f.Members(context.Background())
		return memberStates(ms)["team/alpha"] == MemberQuiet
	})
	Attach(front.a, nil)
	_ = front.f.Close()
	readsBefore := len(tr.requests("team/alpha"))
	again := indexFront(t, bus, ax, tr, func(fc *Config) { fc.Expire = 2 * time.Second })
	eventually(t, "the second front settles", again.f.Settled)
	run, err := again.f.Run(as("ada"), id)
	if err != nil || run.Member != "team/alpha" || run.State != fw.StateInputRequired || run.Pending == nil || run.Pending.ID != "ap1" || run.Principal != "ada" || run.Metadata[fw.MetaBatch] != "bridge:b1" {
		t.Fatalf("the run after the restart = %+v, %v", run, err)
	}
	if got := tr.requests("team/alpha")[readsBefore:]; len(got) != 0 {
		t.Fatalf("the load read the sleeping Task: %q", got)
	}
	ms, err := again.f.Members(as("ada"))
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range ms {
		if m.Name == "team/alpha" && m.InputRequired != 1 {
			t.Fatalf("alpha = %+v", m)
		}
	}

	// A decision routes to the Task through the router, which wakes it.
	// The Task holds the run, so its read after the wake confirms it.
	tr.mu.Lock()
	tr.runs["team/alpha"] = []*fw.Run{{ID: id, Agent: "default", State: fw.StateInputRequired, Seq: 9, Principal: "ada", Input: "Migrate cron",
		Pending: &fw.Approval{ID: "ap1", Tool: "bash"}, Metadata: map[string]string{fw.MetaBatch: "bridge:b1"}}}
	tr.mu.Unlock()
	req := httptest.NewRequest(http.MethodPost, "/v1/runs/"+id+"/approvals/ap1", strings.NewReader(`{"decision":"allow"}`))
	req.SetPathValue("id", id)
	req = req.WithContext(fw.WithCredential(as("ada"), "Bearer ada-token"))
	w := httptest.NewRecorder()
	if !NewRelay(again.a).Take(w, req) || w.Code != http.StatusOK {
		t.Fatalf("the decision = %d %s", w.Code, w.Body)
	}
	if got := tr.requests("team/alpha"); got[len(got)-1] != "POST /v1/runs/"+id+"/approvals/ap1" {
		t.Fatalf("the router got %q", got)
	}
	eventually(t, "the phase of the Task follows the wake", func() bool { return ax.phase("team", "alpha") == "Running" })

	// The Task confirms the run with its next event, and the entry follows
	// the change of state.
	emit(t, again.f, node, id, 10, fw.EvApprovalDecided, fw.ApprovalDecidedData{ApprovalID: "ap1", Decision: "allow", Decider: "ada"})
	eventually(t, "the index holds the run as it runs", func() bool {
		e, ok := idx.entry(t, id)
		return ok && e.State == fw.StateRunning && e.Pending == nil && e.Seq == 10
	})
	// The run finishes: the entry stays until the view forgets the run.
	emit(t, again.f, node, id, 11, fw.EvRunFinished, fw.RunFinishedData{Outcome: fw.OutcomeCompleted, Output: "done"})
	eventually(t, "the index holds the finished run", func() bool {
		e, ok := idx.entry(t, id)
		return ok && e.State == fw.StateCompleted
	})
	eventually(t, "the entry leaves the index after the expire time", func() bool {
		_, _ = again.f.Runs(context.Background())
		_, ok := idx.entry(t, id)
		return !ok
	})
}

// The load cleans the index: a run that its live Task no longer holds, a
// run of a Task that left the Task list, and a finished run older than the
// expire time leave it. The run of a sleeping Task stays until a pool
// deletes the Task.
func TestIndexForgetsTheRunsThatAreGone(t *testing.T) {
	t.Setenv("FW_ATESPACE", "front")
	t.Setenv("FW_API_TOKEN", "fleet-secret")
	idx := &countingBucket{Bucket: bucket.NewMem(IndexBucket)}
	useIndex(t, idx)
	old := time.Now().Add(-time.Hour).UTC()
	idx.put(t, "RLIVE", indexEntry{Member: "team/beta", Agent: "default", State: fw.StateRunning, Seq: 5, At: old, CreatedAt: old})
	idx.put(t, "RHELD", indexEntry{Member: "team/beta", Agent: "default", State: fw.StateRunning, Seq: 7, At: old, CreatedAt: old})
	idx.put(t, "RGONE", indexEntry{Member: "team/gone", Agent: "default", State: fw.StateInputRequired, Seq: 3, At: old, CreatedAt: old})
	idx.put(t, "ROLD", indexEntry{Member: "team/alpha", Agent: "default", State: fw.StateCompleted, Seq: 9, At: old, CreatedAt: old})
	idx.put(t, "RWAIT", indexEntry{Member: "team/alpha", Agent: "default", State: fw.StateInputRequired, Seq: 4, At: old, CreatedAt: old,
		Pending: &indexPending{ID: "ap9", Tool: "bash"}})
	ax := newFakeAX(t, map[string]map[string]string{"team": {"alpha": "Suspended", "beta": "Running"}})
	tr := newTaskRouter(t)
	tr.runs["team/beta"] = []*fw.Run{{ID: "RHELD", Agent: "default", State: fw.StateCompleted, Seq: 8, Input: "the task", CreatedAt: old, UpdatedAt: old}}
	front := indexFront(t, fw.NewMemBus(), ax, tr, func(fc *Config) { fc.Expire = 30 * time.Minute })
	eventually(t, "the server settles and cleans the index", func() bool {
		if !front.f.Settled() {
			return false
		}
		for _, gone := range []string{"RLIVE", "RGONE", "ROLD"} {
			if _, ok := idx.entry(t, gone); ok {
				return false
			}
		}
		_, err := front.f.Run(context.Background(), "RLIVE")
		return errors.Is(err, fw.ErrRunNotFound)
	})
	if r, err := front.f.Run(context.Background(), "RWAIT"); err != nil || r.Member != "team/alpha" || r.State != fw.StateInputRequired {
		t.Fatalf("the run of the sleeping Task = %+v, %v", r, err)
	}
	if _, ok := idx.entry(t, "RWAIT"); !ok {
		t.Fatal("the run of the sleeping Task left the index")
	}
	// The read of the live Task gives the run its whole state and its text.
	eventually(t, "the read confirms the run that beta holds", func() bool {
		r, err := front.f.Run(context.Background(), "RHELD")
		return err == nil && r.State == fw.StateCompleted && r.Input == "the task"
	})
	eventually(t, "the entry follows the read", func() bool {
		e, ok := idx.entry(t, "RHELD")
		return ok && e.State == fw.StateCompleted && e.Seq == 8
	})
	// A pool deletes the sleeping Task alpha to replace it: its run leaves
	// the view and the index, and the view stays settled, so the bridges
	// can tell that the run is gone.
	ax.set("team", "alpha", "")
	eventually(t, "the run of a Task that left the list leaves the index", func() bool {
		_, ok := idx.entry(t, "RWAIT")
		_, err := front.f.Run(context.Background(), "RWAIT")
		return !ok && errors.Is(err, fw.ErrRunNotFound)
	})
	if !front.f.Settled() {
		t.Fatal("the view no longer counts as settled")
	}
}

// A bucket that does not answer holds up neither the view nor its start:
// the stream and the reads work, the view does not settle, and once the
// bucket answers, the view loads the index and writes the entries that
// waited.
func TestIndexThatDoesNotAnswer(t *testing.T) {
	t.Setenv("FW_ATESPACE", "front")
	t.Setenv("FW_API_TOKEN", "fleet-secret")
	idx := &countingBucket{Bucket: bucket.NewMem(IndexBucket), hang: true, release: make(chan struct{})}
	useIndex(t, idx)
	ax := newFakeAX(t, map[string]map[string]string{"team": {"alpha": "Running"}})
	tr := newTaskRouter(t)
	front := indexFront(t, fw.NewMemBus(), ax, tr, nil)
	const node, id = "ax:team/alpha", "01M5HE6R06YW28JTWG5M7W6H87"
	emit(t, front.f, node, id, 1, fw.EvRunQueued, fw.RunQueuedData{Agent: "default", Input: "hello", Principal: "ada"})
	emit(t, front.f, node, id, 2, fw.EvRunStarted, fw.RunStartedData{Model: "fake/m"})
	if r, err := front.f.Run(as("ada"), id); err != nil || r.State != fw.StateRunning {
		t.Fatalf("the run while the bucket hangs = %+v, %v", r, err)
	}
	eventually(t, "the Task list is read", func() bool {
		ms, _ := front.f.Members(context.Background())
		return memberStates(ms)["team/alpha"] == MemberLive
	})
	if front.f.Settled() {
		t.Fatal("the view settled without the index")
	}
	idx.mu.Lock()
	idx.hang = false
	close(idx.release)
	idx.mu.Unlock()
	eventually(t, "the view settles", front.f.Settled)
	eventually(t, "the entry that waited is written", func() bool {
		e, ok := idx.entry(t, id)
		return ok && e.State == fw.StateRunning
	})
}

// A view without an index never settles.
func TestNoIndexNeverSettles(t *testing.T) {
	old := OpenIndex
	OpenIndex = func(context.Context, *fw.Config, *slog.Logger) (bucket.Bucket, func(), error) { return nil, nil, nil }
	t.Cleanup(func() { OpenIndex = old })
	front := newMember(t, fw.NewMemBus(), "front", nil)
	stream(t, front.f, fw.NewID(), []uint64{1, 2, 3, 4}, finishedRun("job", "x", fw.RunUsage{})...)
	time.Sleep(50 * time.Millisecond)
	if front.f.Settled() {
		t.Fatal("a view without an index settled")
	}
}
