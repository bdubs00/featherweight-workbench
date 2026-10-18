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
)

// SuspendTask records the call and moves the Task to the phase Suspended.
func (f *fakeAX) SuspendTask(_ context.Context, req *v1alpha1.SuspendTaskRequest) (*v1alpha1.Task, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.tasks[req.GetAtespace()][req.GetName()]; !ok {
		return nil, context.Canceled
	}
	f.tasks[req.GetAtespace()][req.GetName()] = "Suspended"
	f.suspended = append(f.suspended, req.GetAtespace()+"/"+req.GetName())
	return &v1alpha1.Task{}, nil
}

// ResumeTask records the call and moves the Task to the phase Running.
func (f *fakeAX) ResumeTask(_ context.Context, req *v1alpha1.ResumeTaskRequest) (*v1alpha1.Task, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.tasks[req.GetAtespace()][req.GetName()]; !ok {
		return nil, context.Canceled
	}
	f.tasks[req.GetAtespace()][req.GetName()] = "Running"
	f.resumed = append(f.resumed, req.GetAtespace()+"/"+req.GetName())
	return &v1alpha1.Task{}, nil
}

func (f *fakeAX) phase(atespace, name string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.tasks[atespace][name]
}

// fakeTasks plays the atenet router for Tasks that answer the reads of the
// front: their runs, their agents, and their status. It counts the status
// reads of each actor, because a read of a sleeping Task is a wake.
type fakeTasks struct {
	srv    *httptest.Server
	mu     sync.Mutex
	status map[string]view.Status
	reads  map[string]int
}

func newFakeTasks(t *testing.T) *fakeTasks {
	t.Helper()
	ft := &fakeTasks{status: map[string]view.Status{}, reads: map[string]int{}}
	ft.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		actor := r.Header.Get("ate-target-actor")
		if actor == "" || r.Header.Get("Authorization") != "Bearer fleet-secret" {
			http.Error(w, "unexpected request", http.StatusBadRequest)
			return
		}
		switch r.URL.Path {
		case "/v1/runs":
			_ = json.NewEncoder(w).Encode(map[string]any{"runs": []*fw.Run{}})
		case "/v1/agents":
			_ = json.NewEncoder(w).Encode(map[string]any{"agents": []map[string]string{{"name": "default"}, {"name": "reviewer"}}})
		case "/v1/status":
			ft.mu.Lock()
			ft.reads[actor]++
			st := ft.status[actor]
			ft.mu.Unlock()
			_ = json.NewEncoder(w).Encode(st)
		default:
			http.Error(w, "unexpected path", http.StatusNotFound)
		}
	}))
	t.Cleanup(ft.srv.Close)
	return ft
}

func (ft *fakeTasks) set(actor string, st view.Status) {
	ft.mu.Lock()
	defer ft.mu.Unlock()
	ft.status[actor] = st
}

func (ft *fakeTasks) count(actor string) int {
	ft.mu.Lock()
	defer ft.mu.Unlock()
	return ft.reads[actor]
}

// The front suspends a Task that stays idle past the grace period, leaves
// a Task that works alone, and wakes a sleeping Task when a job arrives for
// a profile that it serves.
func TestSuspendIdleTasksAndWakeForJobs(t *testing.T) {
	t.Setenv("FW_ATESPACE", "team")
	t.Setenv("FW_API_TOKEN", "fleet-secret")
	// Enqueue reads the intake prefix from the configuration of the agent.
	t.Setenv(EnvIntake, "fw.jobs")
	ctx := context.Background()
	bus := fw.NewMemBus()
	ax := newFakeAX(t, map[string]map[string]string{"team": {"alpha": "Running", "busy": "Running"}})
	tasks := newFakeTasks(t)
	long := time.Now().Add(-time.Hour)
	tasks.set("team/alpha", view.Status{Idle: true, IdleSince: &long})
	tasks.set("team/busy", view.Status{Idle: false})
	after := 300 * time.Millisecond
	front := newMember(t, bus, "front", func(cfg *fw.Config, fc *Config) {
		fc.Atespaces = []string{"team"}
		fc.Egress = "fw.events"
		fc.Interval = 100 * time.Millisecond
		fc.SuspendAfter = &after
		cfg.AX.Server = ax.addr
		cfg.AX.Router = strings.TrimPrefix(tasks.srv.URL, "http://")
	})

	eventually(t, "the idle Task is suspended", func() bool { return ax.phase("team", "alpha") == "Suspended" })
	eventually(t, "the view shows it quiet", func() bool {
		ms, err := front.f.Members(ctx)
		return err == nil && memberStates(ms)["team/alpha"] == MemberQuiet
	})
	time.Sleep(3 * after)
	if ax.phase("team", "busy") != "Running" {
		t.Fatal("the Task that works was suspended")
	}

	// A job for a profile that the sleeping Task serves wakes it: one more
	// status read through the router. A job for a profile that nobody
	// serves wakes nobody.
	before := tasks.count("team/alpha")
	if _, sent, err := Enqueue(ctx, front.a, "reviewer", []fw.RunRequest{{Input: "review this"}}); err != nil || sent != 1 {
		t.Fatalf("Enqueue = %d, %v", sent, err)
	}
	eventually(t, "the sleeping Task is woken for the job", func() bool { return tasks.count("team/alpha") > before })
	// The ax API learns of the wake, so the phase follows, and the Task is
	// suspended again once it idles: the cycle closes.
	eventually(t, "the phase of the woken Task follows", func() bool {
		ax.mu.Lock()
		defer ax.mu.Unlock()
		return len(ax.resumed) == 1 && ax.resumed[0] == "team/alpha"
	})
	eventually(t, "the woken Task is suspended again when it idles", func() bool {
		ax.mu.Lock()
		defer ax.mu.Unlock()
		return len(ax.suspended) == 2
	})
	woken := tasks.count("team/alpha")
	if _, _, err := Enqueue(ctx, front.a, "nobody", []fw.RunRequest{{Input: "x"}}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(3 * after)
	if tasks.count("team/alpha") != woken {
		t.Fatalf("a job for a profile that the Task does not serve woke it: %d reads, want %d", tasks.count("team/alpha"), woken)
	}
	ax.mu.Lock()
	n, r := len(ax.suspended), len(ax.resumed)
	ax.mu.Unlock()
	if n != 2 || r != 1 {
		t.Fatalf("SuspendTask calls = %d, ResumeTask calls = %d; want 2 and 1", n, r)
	}
}

// The configuration turns the controller off with 0 or off, defaults to
// ten minutes with atespaces, and refuses the controller without them.
func TestSuspendAfterConfig(t *testing.T) {
	for _, e := range []string{EnvSubject, EnvAtespaces, EnvEgress, EnvName, EnvURL, EnvSuspendAfter} {
		t.Setenv(e, "")
	}
	t.Setenv("FW_ATESPACE", "")
	cfg := fw.DefaultConfig()
	t.Setenv(EnvSubject, "fw.fleet")
	t.Setenv(EnvAtespaces, "team")
	c, err := Load(cfg)
	if err != nil || c.suspendAfter() != DefaultSuspendAfter {
		t.Fatalf("default = %v, %v", c.suspendAfter(), err)
	}
	t.Setenv(EnvSuspendAfter, "off")
	if c, err = Load(cfg); err != nil || c.suspendAfter() != 0 {
		t.Fatalf("off = %v, %v", c.suspendAfter(), err)
	}
	t.Setenv(EnvSuspendAfter, "2m")
	if c, err = Load(cfg); err != nil || c.suspendAfter() != 2*time.Minute {
		t.Fatalf("2m = %v, %v", c.suspendAfter(), err)
	}
	t.Setenv(EnvSuspendAfter, "soon")
	if _, err = Load(cfg); err == nil || !strings.Contains(err.Error(), EnvSuspendAfter) {
		t.Fatalf("bad duration = %v", err)
	}
	t.Setenv(EnvSuspendAfter, "2m")
	t.Setenv(EnvAtespaces, "")
	if _, err = Load(cfg); err == nil || !strings.Contains(err.Error(), "suspendAfter needs") {
		t.Fatalf("without atespaces = %v", err)
	}
	t.Setenv(EnvSuspendAfter, "")
	if c, err = Load(cfg); err != nil || c.suspendAfter() != 0 {
		t.Fatalf("no atespaces, no controller = %v, %v", c.suspendAfter(), err)
	}
}
