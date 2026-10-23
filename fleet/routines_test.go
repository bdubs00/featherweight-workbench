package fleet

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/surface/view"
	"github.com/bdubs00/featherweight-workbench/pools"
)

// testRoutines is a source of routines that a test changes.
type testRoutines struct {
	mu sync.Mutex
	m  map[string][]fw.Routine
}

func (s *testRoutines) set(m map[string][]fw.Routine) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m = m
}

func (s *testRoutines) get(context.Context, *fw.Agent) (map[string][]fw.Routine, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.m, nil
}

// useRoutines makes src the source of the routines for a test.
func useRoutines(t *testing.T, src *testRoutines) {
	t.Helper()
	UseRoutines(src.get, func(context.Context, *fw.Config) (map[string][]fw.Routine, error) { return src.get(nil, nil) })
	t.Cleanup(func() { UseRoutines(nil, nil) })
}

// plainRoutine is a routine without questions, at a version.
func plainRoutine(version int, standing string) fw.Routine {
	return fw.Routine{Name: "close-duplicate", Version: version, Standing: standing,
		Steps: []fw.RoutineStep{{Tool: "tickets.close", Input: `{"number": {{ find .input "#(\\d+)" }}}`}}}
}

// The routines of the source reach every Task of the pool that runs their
// agent: the Workspace holds them, and a change of a routine rolls the
// Tasks, as a change of the overlay does. CheckRoutines asks the merge of
// the pool, which refuses a routine that the workers cannot run.
func TestRoutinesReachThePool(t *testing.T) {
	t.Setenv("FW_ATESPACE", "team")
	t.Setenv("FW_API_TOKEN", "fleet-secret")
	src := &testRoutines{}
	src.set(map[string][]fw.Routine{"triage": {plainRoutine(1, fw.RoutineShadow)}})
	useRoutines(t, src)
	ax := newPoolAX(t)
	tasks := newFakeTasks(t)
	front := newMember(t, fw.NewMemBus(), "front", poolFront(t, ax, strings.TrimPrefix(tasks.srv.URL, "http://"), ownedBlock))
	ctx := context.Background()
	hashOf := func() string { return envOfTask(ax.get("team", "triage-0"), pools.EnvSpec) }
	eventually(t, "the pool is ready", func() bool {
		ps, err := front.f.Pools(ctx)
		return err == nil && len(ps) == 1 && ps[0].Ready == 1
	})
	content := func() string {
		ax.mu.Lock()
		defer ax.mu.Unlock()
		ws := ax.workspaces["team/triage-0-ws"]
		if ws == nil {
			return ""
		}
		return ws.Spec.Files[0].Content
	}
	if c := content(); !strings.Contains(c, "close-duplicate") || !strings.Contains(c, "standing: shadow") {
		t.Fatalf("the Workspace lacks the routine:\n%s", c)
	}
	old := hashOf()
	idle := time.Now().Add(-time.Hour)
	tasks.set("team/triage-0", view.Status{Idle: true, IdleSince: &idle})
	src.set(map[string][]fw.Routine{"triage": {plainRoutine(1, fw.RoutineLive)}})
	eventually(t, "the Task runs the live routine", func() bool {
		return hashOf() != "" && hashOf() != old && strings.Contains(content(), "standing: live")
	})

	if err := CheckRoutines(ctx, front.a, "triage", []fw.Routine{plainRoutine(2, fw.RoutineShadow)}); err != nil {
		t.Fatalf("a plain routine = %v", err)
	}
	asks := plainRoutine(2, fw.RoutineShadow)
	asks.Decider = "clef"
	asks.Questions = map[string]fw.RoutineQuestion{"kind": {Question: fw.Question{Type: fw.QuestionNoul}, Act: 0.9}}
	if err := CheckRoutines(ctx, front.a, "triage", []fw.Routine{asks}); err == nil || !strings.Contains(err.Error(), "the pool triage") || !strings.Contains(err.Error(), "decider") {
		t.Fatalf("a routine with a decider that the workers lack = %v", err)
	}
	if err := CheckRoutines(ctx, front.a, "nobody", []fw.Routine{asks}); err != nil {
		t.Fatalf("an agent of no pool = %v", err)
	}
}

// A request that the front puts on a queue carries the routines of its
// agent in its inline profile, beside the inline profile that it had. An
// agent without an entry keeps its request as it is.
func TestEnqueueCarriesTheRoutines(t *testing.T) {
	t.Setenv("FW_ATESPACE", "")
	t.Setenv(EnvIntake, "fw.jobs")
	withBusServer(t, nil, newMemJobs())
	src := &testRoutines{}
	src.set(map[string][]fw.Routine{"default": {plainRoutine(3, fw.RoutineLive)}})
	useRoutines(t, src)
	bus := fw.NewMemBus()
	m := newMember(t, bus, "front", func(_ *fw.Config, fc *Config) { fc.Intake = "fw.jobs" })
	got := make(chan fw.RunRequest, 4)
	stop, err := bus.Subscribe(context.Background(), "fw.jobs.>", func(e fw.Event) error {
		var req fw.RunRequest
		if e.Decode(&req) == nil {
			got <- req
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stop)
	reqs := []fw.RunRequest{{Input: "#5 a duplicate", Profile: &fw.AgentProfile{Tools: []string{"tickets.close"}}}}
	if _, _, err := Enqueue(as("alice"), m.a, "default", reqs); err != nil {
		t.Fatal(err)
	}
	req := <-got
	if req.Profile == nil || len(req.Profile.Routines) != 1 || req.Profile.Routines[0].Version != 3 || len(req.Profile.Tools) != 1 {
		t.Fatalf("the request = %+v", req.Profile)
	}
	if reqs[0].Profile.Routines != nil {
		t.Fatal("Enqueue changed the request of the caller")
	}
	if _, _, err := Enqueue(as("alice"), m.a, "other", []fw.RunRequest{{Input: "x"}}); err != nil {
		t.Fatal(err)
	}
	if req := <-got; req.Profile != nil {
		t.Fatalf("a request of an agent without routines = %+v", req.Profile)
	}
}

// The view marks a run that a routine completed and a run that a receipt
// sent back, and a tap gets the events of its types with the agent and the
// first line of the input of their run.
func TestTapAndOutcome(t *testing.T) {
	t.Setenv("FW_ATESPACE", "")
	m := newMember(t, fw.NewMemBus(), "front", nil)
	var mu sync.Mutex
	var seen []RunEvent
	Tap(func(a *fw.Agent, ev RunEvent) {
		if a != m.a {
			return
		}
		mu.Lock()
		seen = append(seen, ev)
		mu.Unlock()
	}, fw.EvRoutineApplied)
	stream(t, m.f, "01TAP", []uint64{1, 2, 3, 4},
		fw.MustEvent("", fw.EvRunQueued, fw.RunQueuedData{Agent: "default", Input: "  #9 the first line\nthe second", Principal: "alice"}),
		fw.MustEvent("", fw.EvRoutineApplied, fw.RoutineAppliedData{Routine: "close-duplicate", Version: 1, OK: true}),
		fw.MustEvent("", fw.EvRunFinished, fw.RunFinishedData{Outcome: fw.OutcomeCompleted}),
		fw.MustEvent("", fw.EvRunAnnotated, fw.QualityAnnotatedData{Quality: &fw.QualityObservation{ID: "r", Completion: 3, Verification: "rejected"}}))
	eventually(t, "the marks", func() bool {
		byRoutine, sentBack, whole := m.f.Outcome("01TAP")
		return byRoutine && sentBack && whole
	})
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 1 || seen[0].Agent != "default" || seen[0].Input != "#9 the first line" || seen[0].Event.Type != fw.EvRoutineApplied {
		t.Fatalf("the tap = %+v", seen)
	}
	if a, b, w := m.f.Outcome("01NONE"); a || b || w {
		t.Fatal("an unknown run has marks")
	}
}
