package fleet

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/surface/view"
	v1alpha1 "github.com/google/ax/pkg/apis/v1alpha1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// repairTask serves snapshots independently of the stream, including a
// failed read or an event arriving while a snapshot is in flight.
type repairTask struct {
	mu          sync.Mutex
	srv         *httptest.Server
	run         fw.Run
	reads       int
	fail        bool
	none        bool // the Task holds no run, as a new Task of the same name
	beforeReply func()
}

func newRepairTask(t *testing.T) *repairTask {
	t.Helper()
	x := &repairTask{run: fw.Run{ID: "run", Agent: "default", State: fw.StateRunning, Seq: 2}}
	x.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/runs":
			x.mu.Lock()
			x.reads++
			run, fail, none, before := x.run, x.fail, x.none, x.beforeReply
			x.beforeReply = nil
			x.mu.Unlock()
			if before != nil {
				before()
			}
			if fail {
				http.Error(w, "retry", http.StatusServiceUnavailable)
				return
			}
			if none {
				_ = json.NewEncoder(w).Encode(map[string]any{"runs": []*fw.Run{}})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"runs": []*fw.Run{&run}})
		case "/v1/agents":
			_ = json.NewEncoder(w).Encode(map[string]any{"agents": []map[string]string{{"name": "default"}}})
		case "/v1/status":
			long := time.Now().Add(-time.Hour)
			_ = json.NewEncoder(w).Encode(view.Status{Idle: true, IdleSince: &long})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(x.srv.Close)
	return x
}

// No platform loop runs: tests drive polls and age the repair/retry
// deadlines explicitly instead of sleeping for the one-minute interval.
func repairFront(t *testing.T, ax *fakeAX, task *repairTask, name string) *platform {
	t.Helper()
	t.Setenv("FW_API_TOKEN", "fleet-secret")
	front := newMember(t, fw.NewMemBus(), name, func(cfg *fw.Config, fc *Config) {
		cfg.AX.Router = strings.TrimPrefix(task.srv.URL, "http://")
		fc.Atespaces = nil
	})
	p := front.f.platform
	conn, err := grpc.NewClient(ax.addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	p.conn, p.client, p.atespaces = conn, v1alpha1.NewAXClient(conn), []string{"team"}
	return p
}

func (x *repairTask) count() int {
	x.mu.Lock()
	defer x.mu.Unlock()
	return x.reads
}

func repairDue(p *platform) {
	p.mu.Lock()
	p.members["team/alpha"].fillRead = time.Now().Add(-fillRepairInterval)
	p.mu.Unlock()
}

func assertRepairState(t *testing.T, p *platform, state fw.RunState) {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	r := p.runs["run"]
	if r == nil || r.run.State != state {
		t.Fatalf("run = %+v, want %s", r, state)
	}
}

// Both replicas repair a lost final event even if a short suspend/resume
// happened entirely between polls and neither observed a phase transition.
func TestPlatformRepairConvergesWithoutPhaseTransition(t *testing.T) {
	ax := newFakeAX(t, map[string]map[string]string{"team": {"alpha": phaseRunning}})
	task := newRepairTask(t)
	a, b := repairFront(t, ax, task, "front-a"), repairFront(t, ax, task, "front-b")
	ctx := context.Background()
	a.poll(ctx)
	b.poll(ctx)
	task.mu.Lock()
	task.run.State, task.run.Seq = fw.StateFailed, 3
	task.mu.Unlock()
	for _, p := range []*platform{a, b} {
		p.poll(ctx)
		assertRepairState(t, p, fw.StateRunning)
		repairDue(p)
		p.poll(ctx)
		assertRepairState(t, p, fw.StateFailed)
	}
	// Once repaired, the stale running projection no longer excludes the
	// Task from idle suspension. The worker's own status still confirms it.
	a.f.suspendAfter = time.Second
	a.mu.Lock()
	a.members["team/alpha"].liveSince = time.Now().Add(-time.Hour)
	a.runs["run"].updated = time.Now().Add(-time.Hour)
	a.mu.Unlock()
	a.suspendIdle(ctx, time.Now())
	if got := ax.phase("team", "alpha"); got != phaseSuspended {
		t.Fatalf("phase = %s", got)
	}
}

func TestPlatformRepairAfterWake(t *testing.T) {
	for _, wake := range []string{"external", "front"} {
		t.Run(wake, func(t *testing.T) {
			ax := newFakeAX(t, map[string]map[string]string{"team": {"alpha": phaseRunning}})
			task := newRepairTask(t)
			p := repairFront(t, ax, task, "front")
			ctx := context.Background()
			p.poll(ctx)
			task.mu.Lock()
			task.run.State, task.run.Seq = fw.StateFailed, 3
			task.mu.Unlock()
			ax.set("team", "alpha", phaseSuspended)
			p.poll(ctx)
			if task.count() != 1 {
				t.Fatalf("sleeping Task read %d times", task.count())
			}
			if wake == "external" {
				ax.set("team", "alpha", phaseRunning)
			} else {
				req, _ := http.NewRequest(http.MethodGet, "/v1/status", nil)
				resp, err := p.f.Forward(fw.WithCredential(ctx, "Bearer fleet-secret"), "team/alpha", req)
				if err != nil {
					t.Fatal(err)
				}
				_ = resp.Body.Close()
				eventually(t, "the front wake updates the phase", func() bool {
					p.mu.Lock()
					defer p.mu.Unlock()
					return p.members["team/alpha"].phase == phaseRunning
				})
			}
			p.poll(ctx)
			assertRepairState(t, p, fw.StateFailed)
			if task.count() != 2 {
				t.Fatalf("wake reads = %d, want 2", task.count())
			}
			// Every backfill is itself forwarded; those ordinary live
			// answers must not perpetually invalidate the snapshot.
			p.poll(ctx)
			if task.count() != 2 {
				t.Fatalf("already-live answers caused %d reads", task.count())
			}
		})
	}
}

func TestPlatformRepairRetriesFailedRead(t *testing.T) {
	ax := newFakeAX(t, map[string]map[string]string{"team": {"alpha": phaseRunning}})
	task := newRepairTask(t)
	p := repairFront(t, ax, task, "front")
	p.f.interval = time.Minute
	ctx := context.Background()
	p.poll(ctx)
	repairDue(p)
	task.mu.Lock()
	task.fail = true
	task.mu.Unlock()
	p.poll(ctx)
	p.mu.Lock()
	m := p.members["team/alpha"]
	if m.filled || m.filling || m.fillFails != 1 || m.fillAt.IsZero() {
		t.Fatalf("failed fill = %+v", m)
	}
	if p.needsFill(m, m.fillAt.Add(-time.Nanosecond)) {
		t.Fatal("retry before backoff expired")
	}
	if !p.needsFill(m, m.fillAt) {
		t.Fatal("no retry at deadline")
	}
	m.fillAt = time.Time{}
	p.mu.Unlock()
	task.mu.Lock()
	task.fail = false
	task.run.State, task.run.Seq = fw.StateFailed, 3
	task.mu.Unlock()
	p.poll(ctx)
	assertRepairState(t, p, fw.StateFailed)
	if task.count() != 3 {
		t.Fatalf("reads = %d, want 3", task.count())
	}
}

func TestPlatformRepairKeepsNewerEventsAndWakeRequest(t *testing.T) {
	ax := newFakeAX(t, map[string]map[string]string{"team": {"alpha": phaseRunning}})
	task := newRepairTask(t)
	p := repairFront(t, ax, task, "front")
	ctx := context.Background()
	p.poll(ctx)
	repairDue(p)
	task.mu.Lock()
	task.beforeReply = func() {
		// The snapshot was taken before this newer event and wake.
		emit(t, p.f, "ax:team/alpha", "run", 5, fw.EvRunFinished, fw.RunFinishedData{Outcome: "failed"})
		p.mu.Lock()
		p.members["team/alpha"].state = MemberQuiet
		p.mu.Unlock()
		p.resumed("team/alpha")
	}
	task.mu.Unlock()
	p.poll(ctx)
	assertRepairState(t, p, fw.StateFailed)
	p.mu.Lock()
	m := p.members["team/alpha"]
	if m.filled || m.filling || !p.needsFill(m, time.Now()) {
		t.Fatalf("new wake request lost: %+v", m)
	}
	p.mu.Unlock()
	p.poll(ctx)
	assertRepairState(t, p, fw.StateFailed)
	if task.count() != 3 {
		t.Fatalf("reads = %d, want 3", task.count())
	}
}

func TestPlatformRepairEligibility(t *testing.T) {
	now := time.Now()
	m := &platformMember{name: "team/alpha", state: MemberLive, filled: true, fillRead: now}
	p := &platform{runs: map[string]*platformRun{"run": {member: m.name, run: &fw.Run{State: fw.StateRunning}}}}
	if p.needsFill(m, now.Add(fillRepairInterval-time.Nanosecond)) {
		t.Fatal("repair too early")
	}
	if !p.needsFill(m, now.Add(fillRepairInterval)) {
		t.Fatal("repair not due at bound")
	}
	p.runs["run"].run.State = fw.StateQueued
	if !p.needsFill(m, now.Add(fillRepairInterval)) {
		t.Fatal("apparently queued run not eligible for repair")
	}
	m.filling = true
	if p.needsFill(m, now.Add(fillRepairInterval)) {
		t.Fatal("concurrent read allowed")
	}
	m.filling = false
	for _, state := range []string{MemberQuiet, MemberStopped} {
		m.state, m.filled = state, false
		if p.needsFill(m, now.Add(fillRepairInterval)) {
			t.Fatalf("refresh of %s member", state)
		}
	}
	m.state, m.filled = MemberLive, true
	for _, state := range []fw.RunState{fw.StateCompleted, fw.StateFailed, fw.StateInputRequired} {
		p.runs["run"].run.State = state
		if p.needsFill(m, now.Add(fillRepairInterval)) {
			t.Fatalf("periodic repair of %s run", state)
		}
	}
}

// A pool, another replica of the front, or a person can delete a Task and
// create it again under the same name between two reads of the Task list,
// as a roll of every pool does. The view must not keep the open runs of
// the old Task on the new one: a run whose final event the old Task never
// sent stays open for good, and the page then counts the worker as busy
// and shows "waiting for a worker" while it runs idle. The new creation
// time of the Task tells the new Task: the view forgets the old Task with
// its runs and reads the new one, also when the phase never left Running.
func TestPlatformForgetsTheRunsOfAReplacedTask(t *testing.T) {
	ax := newFakeAX(t, map[string]map[string]string{"team": {"alpha": phaseRunning}})
	first := time.Now().Add(-time.Hour).UTC()
	ax.created = map[string]time.Time{"team/alpha": first}
	task := newRepairTask(t)
	task.run.State, task.run.CreatedAt = fw.StateInputRequired, first.Add(time.Minute)
	p := repairFront(t, ax, task, "front")
	ctx := context.Background()
	p.poll(ctx)
	assertRepairState(t, p, fw.StateInputRequired)
	reads := task.count()

	// The roll deletes alpha and creates it again; the list shows it as
	// Running both times, and the new Task holds no run.
	ax.mu.Lock()
	ax.created["team/alpha"] = time.Now().UTC()
	ax.mu.Unlock()
	task.mu.Lock()
	task.none = true
	task.mu.Unlock()
	p.poll(ctx)
	p.mu.Lock()
	r, m := p.runs["run"], p.members["team/alpha"]
	p.mu.Unlock()
	if r != nil {
		t.Fatalf("the view keeps the open run of the replaced Task: %+v", r.run)
	}
	if m == nil || m.state != MemberLive || task.count() != reads+1 {
		t.Fatalf("the new Task = %+v, reads %d after %d", m, task.count(), reads)
	}
	if got, err := p.f.Runs(ctx); err != nil || len(got) != 0 {
		t.Fatalf("runs = %+v, %v", got, err)
	}
	if ms, err := p.f.Members(ctx); err != nil || memberStates(ms)["team/alpha"] != MemberLive {
		t.Fatalf("members = %+v, %v", ms, err)
	}

	// A read of the new Task drops an open run of the stream that is older
	// than the Task, which the new Task does not hold.
	p.mu.Lock()
	p.runs["old"] = &platformRun{run: &fw.Run{ID: "old", Agent: "default", State: fw.StateRunning, CreatedAt: first}, member: "team/alpha", updated: time.Now().Add(-time.Minute), queued: true}
	p.runs["young"] = &platformRun{run: &fw.Run{ID: "young", Agent: "default", State: fw.StateRunning, CreatedAt: time.Now().UTC()}, member: "team/alpha", updated: time.Now().Add(-time.Minute), queued: true}
	p.members["team/alpha"].fillRead = time.Now().Add(-fillRepairInterval)
	p.mu.Unlock()
	p.poll(ctx)
	p.mu.Lock()
	_, old := p.runs["old"]
	_, young := p.runs["young"]
	p.mu.Unlock()
	if old || !young {
		t.Fatalf("after the read: old run kept %v, young run kept %v", old, young)
	}
}
