package fleet

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/surface/view"
	v1alpha1 "github.com/google/ax/pkg/apis/v1alpha1"
	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
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
	// runs holds the runs that each actor answers on GET /v1/runs/{id},
	// and tokens the bearer tokens that each actor takes besides the
	// token of the fleet.
	runs   map[string]map[string]*fw.Run
	tokens map[string]string
}

func newFakeTasks(t *testing.T) *fakeTasks {
	t.Helper()
	ft := &fakeTasks{status: map[string]view.Status{}, reads: map[string]int{}, runs: map[string]map[string]*fw.Run{}, tokens: map[string]string{}}
	ft.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		actor := r.Header.Get("ate-target-actor")
		ft.mu.Lock()
		own := ft.tokens[actor]
		ft.mu.Unlock()
		auth := r.Header.Get("Authorization")
		if actor == "" || (auth != "Bearer fleet-secret" && (own == "" || auth != "Bearer "+own)) {
			http.Error(w, "unexpected request", http.StatusBadRequest)
			return
		}
		if id, ok := strings.CutPrefix(r.URL.Path, "/v1/runs/"); ok {
			ft.mu.Lock()
			run := ft.runs[actor][id]
			ft.mu.Unlock()
			switch {
			case run == nil:
				http.Error(w, "no run", http.StatusNotFound)
			case run.State == "":
				http.Error(w, "forbidden", http.StatusForbidden)
			default:
				_ = json.NewEncoder(w).Encode(run)
			}
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
	// Enqueue reads the intake prefix from the configuration of the agent,
	// and the fleet of the test takes the same prefix, so a wake finds the
	// Tasks of no pool that read <prefix>.<agent>.
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
		fc.Intake = "fw.jobs"
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

// A Task that slept since before the front started has no known profiles,
// so a job for a profile that no known Task serves wakes it once, and the
// front learns what it serves.
func TestWakeForTeachesTheFrontAboutUnknownTasks(t *testing.T) {
	t.Setenv("FW_ATESPACE", "team")
	t.Setenv("FW_API_TOKEN", "fleet-secret")
	t.Setenv(EnvIntake, "fw.jobs")
	ctx := context.Background()
	bus := fw.NewMemBus()
	ax := newFakeAX(t, map[string]map[string]string{"team": {"fresh": "Suspended"}})
	tasks := newFakeTasks(t)
	tasks.set("team/fresh", view.Status{Idle: true})
	front := newMember(t, bus, "front", func(cfg *fw.Config, fc *Config) {
		fc.Atespaces = []string{"team"}
		fc.Egress = "fw.events"
		fc.Intake = "fw.jobs"
		fc.Interval = 100 * time.Millisecond
		cfg.AX.Server = ax.addr
		cfg.AX.Router = strings.TrimPrefix(tasks.srv.URL, "http://")
	})
	eventually(t, "the front sees the sleeping Task", func() bool {
		ms, err := front.f.Members(ctx)
		return err == nil && memberStates(ms)["team/fresh"] == MemberQuiet
	})
	if _, _, err := Enqueue(ctx, front.a, "reviewer", []fw.RunRequest{{Input: "review"}}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the unknown sleeper is woken", func() bool { return tasks.count("team/fresh") > 0 })
	eventually(t, "its phase follows", func() bool { return ax.phase("team", "fresh") == "Running" })
	eventually(t, "the front learns its profiles", func() bool {
		p := front.f.platform
		p.mu.Lock()
		defer p.mu.Unlock()
		m := p.members["team/fresh"]
		return m != nil && m.agents["reviewer"] && m.agents["default"]
	})
}

// The configuration turns the controller off with 0 or off, defaults to
// ten minutes with atespaces, and refuses the controller without them.
func TestSuspendAfterConfig(t *testing.T) {
	for _, e := range []string{EnvAtespaces, EnvEgress, EnvName, EnvSuspendAfter} {
		t.Setenv(e, "")
	}
	t.Setenv("FW_ATESPACE", "")
	cfg := fw.DefaultConfig()
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

// syncLog is a log sink that a test reads while the front writes to it.
type syncLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncLog) Write(b []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(b)
}

// lines returns the lines of the log that hold every one of the words.
func (s *syncLog) lines(words ...string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, line := range strings.Split(s.buf.String(), "\n") {
		ok := line != ""
		for _, w := range words {
			ok = ok && strings.Contains(line, w)
		}
		if ok {
			out = append(out, line)
		}
	}
	return out
}

// startRun puts one run of a Task into the view that started and then sent
// nothing, as the egress stream delivers it.
func startRun(t *testing.T, f *Fleet, member, id string) {
	t.Helper()
	emit(t, f, "ax:"+member, id, 1, fw.EvRunQueued, fw.RunQueuedData{Agent: "reviewer", Input: "work", Origin: fw.OriginBus, Principal: "ada"})
	emit(t, f, "ax:"+member, id, 2, fw.EvRunStarted, fw.RunStartedData{Model: "fake/m"})
}

// memberNamed returns the member of the view with the name, as GET
// /v1/fleet and the MCP tool fleet show it.
func memberNamed(t *testing.T, f *Fleet, name string) Member {
	t.Helper()
	ms, err := f.Members(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range ms {
		if m.Name == name {
			return m
		}
	}
	t.Fatalf("the view has no member %s", name)
	return Member{}
}

// A Task that holds a running run, and that sent no event for the grace
// period, is suspended when its status says that a storage failure blocked
// it. The front logs one line with the reason, and the view and the gauge
// show the Task as blocked until its next event. A Task in the same state
// whose status is not blocked works slowly: the front reads its status and
// leaves it alone, and it logs nothing about it above Debug.
func TestSuspendBlockedBusyTask(t *testing.T) {
	t.Setenv("FW_ATESPACE", "team")
	t.Setenv("FW_API_TOKEN", "fleet-secret")
	bus := fw.NewMemBus()
	ax := newFakeAX(t, map[string]map[string]string{"team": {"stuck": "Running", "slow": "Running"}})
	tasks := newFakeTasks(t)
	failed := time.Now().UTC()
	reason := "storage unavailable: write ledger.jsonl: no space left on device"
	tasks.set("team/stuck", view.Status{Idle: true, IdleSince: &failed, Blocked: true, BlockedReason: reason})
	tasks.set("team/slow", view.Status{Idle: false})
	sink := &syncLog{}
	after, interval := 300*time.Millisecond, 100*time.Millisecond
	front := newMember(t, bus, "front", func(cfg *fw.Config, fc *Config) {
		fc.Atespaces = []string{"team"}
		fc.Egress = "fw.events"
		fc.Interval = interval
		fc.SuspendAfter = &after
		cfg.AX.Server = ax.addr
		cfg.AX.Router = strings.TrimPrefix(tasks.srv.URL, "http://")
	}, fw.WithLogger(slog.New(slog.NewTextHandler(sink, &slog.HandlerOptions{Level: slog.LevelDebug}))))
	reg := prometheus.NewRegistry()
	reg.MustRegister(NewCollector(front.a))
	start := time.Now()
	startRun(t, front.f, "team/stuck", "run-stuck")
	startRun(t, front.f, "team/slow", "run-slow")

	eventually(t, "the blocked Task is suspended", func() bool { return ax.phase("team", "stuck") == "Suspended" })
	// The front marks the member once SuspendTask returns.
	var m Member
	eventually(t, "the view shows the blocked Task quiet with its reason", func() bool {
		m = memberNamed(t, front.f, "team/stuck")
		return m.State == MemberQuiet && m.BlockedReason == reason
	})
	raw, _ := json.Marshal(m)
	if !strings.Contains(string(raw), `"blockedReason":"`+reason+`"`) {
		t.Fatalf("the member as JSON = %s", raw)
	}
	if got := gather(t, reg)["fw_fleet_members_blocked{}"]; got != 1 {
		t.Fatalf("fw_fleet_members_blocked = %v, want 1", got)
	}
	eventually(t, "the front logs the suspend", func() bool { return len(sink.lines("suspended a Task that a storage failure blocked")) > 0 })
	if lines := sink.lines("suspended a Task that a storage failure blocked"); len(lines) != 1 ||
		!strings.Contains(lines[0], "member=team/stuck") || !strings.Contains(lines[0], reason) {
		t.Fatalf("the log of the suspend = %q", lines)
	}

	// The slow Task stays awake, and its status was read.
	eventually(t, "the front reads the status of the slow Task", func() bool { return tasks.count("team/slow") > 0 })
	time.Sleep(3 * interval)
	if ax.phase("team", "slow") != "Running" {
		t.Fatal("the Task that is busy and not blocked was suspended")
	}
	if reads, most := tasks.count("team/slow"), int(time.Since(start)/interval)+1; reads > most {
		t.Fatalf("the front read the status of the slow Task %d times, more than once per interval (%d)", reads, most)
	}
	for _, line := range sink.lines("team/slow") {
		if !strings.Contains(line, "level=DEBUG") {
			t.Fatalf("a line about the slow Task above Debug: %s", line)
		}
	}
	ax.mu.Lock()
	suspended := strings.Join(ax.suspended, ",")
	ax.mu.Unlock()
	if suspended != "team/stuck" {
		t.Fatalf("SuspendTask calls = %s, want team/stuck", suspended)
	}

	// The next event of the Task comes from a process that works again,
	// so the mark goes.
	emit(t, front.f, "ax:team/stuck", "run-stuck", 3, fw.EvTurnStarted, fw.TurnData{Turn: 1})
	if m := memberNamed(t, front.f, "team/stuck"); m.BlockedReason != "" {
		t.Fatalf("the mark stays after an event: %+v", m)
	}
	if got := gather(t, reg)["fw_fleet_members_blocked{}"]; got != 0 {
		t.Fatalf("fw_fleet_members_blocked after the event = %v, want 0", got)
	}
}

// controllerFront is a front with the Task list of ax and the router of
// tasks whose platform loop does not run, so a test calls the controller
// with the times that it chooses.
func controllerFront(t *testing.T, ax *fakeAX, tasks *fakeTasks, after, interval time.Duration) *platform {
	t.Helper()
	t.Setenv("FW_API_TOKEN", "fleet-secret")
	front := newMember(t, fw.NewMemBus(), "front", func(cfg *fw.Config, fc *Config) {
		cfg.AX.Router = strings.TrimPrefix(tasks.srv.URL, "http://")
		fc.Atespaces = nil
		fc.Interval = interval
		fc.SuspendAfter = &after
	})
	p := front.f.platform
	conn, err := grpc.NewClient(ax.addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	p.conn, p.client, p.atespaces = conn, v1alpha1.NewAXClient(conn), []string{"team"}
	return p
}

// The controller reads the status of a busy Task at most once per
// interval, however often it runs, and suspends the Task at the first read
// that says blocked.
func TestSuspendReadsABusyTaskOncePerInterval(t *testing.T) {
	ax := newFakeAX(t, map[string]map[string]string{"team": {"slow": phaseRunning}})
	tasks := newFakeTasks(t)
	tasks.set("team/slow", view.Status{Idle: false})
	after, interval := time.Minute, 30*time.Second
	p := controllerFront(t, ax, tasks, after, interval)
	ctx := context.Background()
	p.poll(ctx) // the Task becomes live now, so the controller waits
	startRun(t, p.f, "team/slow", "run")
	if n := tasks.count("team/slow"); n != 0 {
		t.Fatalf("status reads before the grace period = %d", n)
	}
	now := time.Now().Add(after + time.Second)
	for i, c := range []struct {
		at    time.Duration
		reads int
	}{
		{0, 1},
		{time.Second, 1},
		{interval - time.Nanosecond, 1},
		{interval, 2},
		{interval + interval/2, 2},
		{2 * interval, 3},
	} {
		p.suspendIdle(ctx, now.Add(c.at))
		if got := tasks.count("team/slow"); got != c.reads {
			t.Fatalf("pass %d at +%v: status reads = %d, want %d", i, c.at, got, c.reads)
		}
	}
	if ax.phase("team", "slow") != phaseRunning {
		t.Fatal("a busy Task that is not blocked was suspended")
	}
	failed := time.Now().UTC()
	tasks.set("team/slow", view.Status{Idle: true, IdleSince: &failed, Blocked: true, BlockedReason: "storage unavailable: disk full"})
	p.suspendIdle(ctx, now.Add(2*interval+time.Second))
	if ax.phase("team", "slow") != phaseRunning || tasks.count("team/slow") != 3 {
		t.Fatalf("a pass within the interval read the status: %d reads, phase %s", tasks.count("team/slow"), ax.phase("team", "slow"))
	}
	p.suspendIdle(ctx, now.Add(3*interval))
	if ax.phase("team", "slow") != phaseSuspended || tasks.count("team/slow") != 4 {
		t.Fatalf("after a blocked status: %d reads, phase %s", tasks.count("team/slow"), ax.phase("team", "slow"))
	}
	p.mu.Lock()
	mark := p.members["team/slow"].blocked
	p.mu.Unlock()
	if mark != "storage unavailable: disk full" {
		t.Fatalf("the mark = %q", mark)
	}
}

// A run that the read of its Task shows as blocked (fw.Run.Blocked) does no
// work, although its last recorded state is running, so its Task counts
// as idle. The status of the Task here says idle and not blocked, so only
// the path for an idle Task can suspend it. The next event of the run
// clears the mark on the run.
func TestBlockedRunIsNoWork(t *testing.T) {
	ax := newFakeAX(t, map[string]map[string]string{"team": {"alpha": phaseRunning}})
	task := newRepairTask(t)
	p := repairFront(t, ax, task, "front")
	ctx := context.Background()
	p.poll(ctx)
	task.mu.Lock()
	task.run.Blocked = "storage unavailable: write ledger.jsonl: no space left on device"
	task.mu.Unlock()
	repairDue(p)
	p.poll(ctx)
	p.mu.Lock()
	r := p.runs["run"]
	if r == nil || r.run.State != fw.StateRunning || r.run.Blocked == "" || !p.members["team/alpha"].runsBlocked {
		p.mu.Unlock()
		t.Fatalf("the run after a read of a blocked Task = %+v", r)
	}
	p.mu.Unlock()
	runs, err := p.f.Runs(ctx)
	if err != nil || len(runs) != 1 || runs[0].Blocked == "" {
		t.Fatalf("the view of the run = %+v, %v", runs, err)
	}
	p.f.suspendAfter = time.Second
	p.mu.Lock()
	p.members["team/alpha"].liveSince = time.Now().Add(-time.Hour)
	p.runs["run"].updated = time.Now().Add(-time.Hour)
	p.mu.Unlock()
	p.suspendIdle(ctx, time.Now())
	if got := ax.phase("team", "alpha"); got != phaseSuspended {
		t.Fatalf("phase = %s, want Suspended", got)
	}
	emit(t, p.f, "ax:team/alpha", "run", 3, fw.EvTurnStarted, fw.TurnData{Turn: 2})
	p.mu.Lock()
	defer p.mu.Unlock()
	if r := p.runs["run"]; r.run.Blocked != "" || p.members["team/alpha"].runsBlocked {
		t.Fatalf("the mark on the run stays after an event: %+v", r.run)
	}
}
