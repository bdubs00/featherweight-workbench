package duties

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/duties/schedule"
	"github.com/bdubs00/featherweight-workbench/internal/bucket"
)

// nine is Saturday, October 24, 2026, at 9:00 in New York.
var nine = time.Date(2026, 10, 24, 13, 0, 0, 0, time.UTC)

// A time of the schedule starts once: the run request has its id, the
// message with its placeholders, the budget, the principal of the duty,
// the metadata, and the idempotency key of the contract. The history
// follows the run to its end with its cost.
func TestScheduleStartsOnce(t *testing.T) {
	f := newFront(t, "", nil)
	f.put(t, "alice", daily("report"))
	f.pass(t)
	if n := len(f.queue.list()); n != 0 {
		t.Fatalf("a time before the duty existed started %d runs", n)
	}
	f.clock.Set(nine.Add(2 * time.Second))
	f.pass(t)
	f.pass(t)
	reqs := f.queue.list()
	if len(reqs) != 1 {
		t.Fatalf("the time started %d runs", len(reqs))
	}
	req := reqs[0]
	want := map[string]string{MetaDuty: "report", MetaSlot: "2026-10-24T13:00:00Z", fw.MetaBatch: "duty:report", fw.MetaTitle: "Write the report of 2026-10-24 at 09:00 for report"}
	for k, v := range want {
		if req.Metadata[k] != v {
			t.Errorf("metadata %s = %q, want %q", k, req.Metadata[k], v)
		}
	}
	if req.ID == "" || req.Agent != "triage" || req.Input != "Write the report of 2026-10-24 at 09:00 for report." ||
		req.IdempotencyKey != "duty/report/2026-10-24T13:00:00Z" || req.Principal != "duty:report" || req.PrincipalKind != fw.KindWorkload ||
		req.Budget == nil || req.Budget.MaxCostUSD != 0.5 || req.Budget.MaxTurns != 40 {
		t.Fatalf("the request = %+v", req)
	}
	h := f.history(t, "report")
	if len(h) != 1 || h[0].State != StateQueued || h[0].Run != req.ID || h[0].Start != StartSchedule || !h[0].Slot.Equal(nine) {
		t.Fatalf("history = %+v", h)
	}
	f.view.set(req.ID, fw.StateRunning, 0.05, func(r *fw.Run) { r.Title = "The report of Saturday" })
	f.clock.Add(time.Minute)
	f.pass(t)
	if h := f.history(t, "report"); h[0].State != "running" || h[0].CostUSD != 0.05 || h[0].Title != "The report of Saturday" {
		t.Fatalf("history while it runs = %+v", h[0])
	}
	f.view.set(req.ID, fw.StateCompleted, 0.12, func(r *fw.Run) { r.TreeUsage = &fw.RunUsage{CostUSD: 0.2} })
	f.pass(t)
	v, err := f.s.Get(as("carol"), "report")
	if err != nil {
		t.Fatal(err)
	}
	if v.State.LastRun == nil || v.State.LastRun.State != StateCompleted || v.State.LastRun.CostUSD != 0.2 || v.State.TodayUSD != 0.2 {
		t.Fatalf("the state = %+v", v.State)
	}
	if len(v.Next) != 5 || !v.Next[0].Equal(nine.AddDate(0, 0, 1)) {
		t.Fatalf("next = %v", v.Next)
	}
}

// Two replicas on one store meet the same time: only one starts it.
func TestTwoReplicasOneStart(t *testing.T) {
	for name, open := range map[string]func(*clock) (*Store, *Store){
		"mem": func(c *clock) (*Store, *Store) {
			st := memStore(c)
			return st, NewStore(st.duties, st.runs, st.seen, quiet())
		},
		"nats": func(c *clock) (*Store, *Store) {
			js := natsJS(t)
			return natsStore(js(), c), natsStore(js(), c)
		},
	} {
		t.Run(name, func(t *testing.T) {
			c := &clock{t: t0}
			st1, st2 := open(c)
			st2.now = c.Now
			f := newFront(t, "", nil)
			replicas := []*Service{}
			queues := []*queue{}
			for i, st := range []*Store{st1, st2} {
				s := newService(f.a, st, Config{}, quiet())
				q := &queue{}
				s.now, s.enqueue, s.runs, s.replica = c.Now, q.enqueue, f.view.get, "front-"+string(rune('0'+i))
				replicas, queues = append(replicas, s), append(queues, q)
			}
			d := daily("report")
			d.Limits.Overlap = OverlapAllow
			if _, err := replicas[0].Put(as("alice"), "report", DutyBody{Duty: d}, PutOptions{}); err != nil {
				t.Fatal(err)
			}
			for day := range 5 {
				c.Set(nine.AddDate(0, 0, day).Add(time.Second))
				var wg sync.WaitGroup
				for _, s := range replicas {
					wg.Add(1)
					go func() {
						defer wg.Done()
						s.pass(context.Background())
					}()
				}
				wg.Wait()
			}
			n := len(queues[0].list()) + len(queues[1].list())
			if n != 5 {
				t.Fatalf("5 times started %d runs on two replicas", n)
			}
			h, _ := st1.history(context.Background(), "report", 10)
			if len(h) != 5 {
				t.Fatalf("history = %+v", h)
			}
		})
	}
}

// A missed time younger than catchUp runs once, after the front comes
// back; an older one does not run, and the history says why.
func TestCatchUp(t *testing.T) {
	f := newFront(t, "", nil)
	f.put(t, "alice", daily("late"))
	never := daily("never")
	never.Limits.CatchUp = "0"
	f.put(t, "alice", never)
	often := daily("often")
	often.When.Schedule = &schedule.Schedule{Repeat: schedule.RepeatEvery, Every: "15m", TimeZone: "America/New_York"}
	f.put(t, "alice", often)
	// The front was down from noon to 9:30 the next day.
	f.clock.Set(nine.Add(30 * time.Minute))
	f.pass(t)
	var late, others int
	for _, r := range f.queue.list() {
		switch r.Metadata[MetaDuty] {
		case "late":
			late++
			if r.Metadata[MetaSlot] != "2026-10-24T13:00:00Z" {
				t.Errorf("late ran the time %s", r.Metadata[MetaSlot])
			}
		case "often":
			others++
			if r.Metadata[MetaSlot] != "2026-10-24T13:30:00Z" {
				t.Errorf("often ran the time %s, not the newest", r.Metadata[MetaSlot])
			}
		default:
			t.Errorf("%s ran", r.Metadata[MetaDuty])
		}
	}
	if late != 1 || others != 1 {
		t.Fatalf("late ran %d times and often %d", late, others)
	}
	h := f.history(t, "never")
	if len(h) != 1 || h[0].State != StateMissed || !strings.Contains(h[0].Reason, "older than the catch-up of 0") {
		t.Fatalf("history of never = %+v", h)
	}
	// Two hours late is older than the catch-up of 1h.
	f.clock.Set(nine.AddDate(0, 0, 1).Add(2 * time.Hour))
	f.pass(t)
	if h := f.history(t, "late"); h[0].State != StateMissed || !strings.Contains(h[0].Reason, "catch-up of 1h") {
		t.Fatalf("history of late = %+v", h)
	}
	// A time that the pass meets within a minute runs, also with 0.
	f.clock.Set(nine.AddDate(0, 0, 2).Add(30 * time.Second))
	f.pass(t)
	if h := f.history(t, "never"); h[0].State != StateQueued {
		t.Fatalf("a time met at once did not run: %+v", h[0])
	}
}

// overlap skip skips a time while the last run works, queue waits for it
// with one start at most, and allow runs beside it.
func TestOverlap(t *testing.T) {
	f := newFront(t, "", nil)
	for _, o := range []string{OverlapSkip, OverlapQueue, OverlapAllow} {
		d := daily(o)
		d.When.Schedule = &schedule.Schedule{Repeat: schedule.RepeatHourly, TimeZone: "UTC"}
		d.Limits.Overlap = o
		f.put(t, "alice", d)
	}
	f.clock.Set(t0.Add(time.Hour))
	f.pass(t)
	first := map[string]string{}
	for _, r := range f.queue.list() {
		first[r.Metadata[MetaDuty]] = r.ID
		f.view.set(r.ID, fw.StateRunning, 0)
	}
	f.clock.Add(time.Hour)
	f.pass(t)
	f.clock.Add(time.Hour)
	f.pass(t)
	if s := f.states(t, OverlapSkip); !slices.Equal(s, []string{StateSkipped, StateSkipped, "running"}) {
		t.Fatalf("skip = %v", s)
	}
	if h := f.history(t, OverlapSkip); !strings.Contains(h[0].Reason, "had not ended, and the overlap of the duty is skip") {
		t.Fatalf("the reason of a skip = %q", h[0].Reason)
	}
	if s := f.states(t, OverlapQueue); !slices.Equal(s, []string{StateSkipped, StateWaiting, "running"}) {
		t.Fatalf("queue = %v", s)
	}
	if s := f.states(t, OverlapAllow); !slices.Equal(s, []string{StateQueued, StateQueued, "running"}) {
		t.Fatalf("allow = %v", s)
	}
	// The run of queue ends: the start that waited goes on.
	f.view.set(first[OverlapQueue], fw.StateCompleted, 0.1)
	f.clock.Add(time.Minute)
	f.pass(t)
	if s := f.states(t, OverlapQueue); !slices.Equal(s, []string{StateSkipped, StateQueued, StateCompleted}) {
		t.Fatalf("queue after the end = %v", s)
	}
	n := 0
	for _, r := range f.queue.list() {
		if r.Metadata[MetaDuty] == OverlapQueue {
			n++
		}
	}
	if n != 2 {
		t.Fatalf("queue started %d runs", n)
	}
}

// A failed run goes again up to the retries, with an idempotency key of
// its own; a run that fails after them counts for the breaker, and three
// in a row pause the duty with the reason. A resume ends the row.
func TestRetriesAndBreaker(t *testing.T) {
	f := newFront(t, "", nil)
	d := daily("flaky")
	f.put(t, "alice", d)
	fail := func(day int) {
		t.Helper()
		f.clock.Set(nine.AddDate(0, 0, day).Add(time.Second))
		f.pass(t)
		req := f.queue.last(t)
		f.view.set(req.ID, fw.StateFailed, 0.1, func(r *fw.Run) { r.Error = "the model refused" })
		f.pass(t)
		h := f.history(t, "flaky")
		if h[0].State != StateRetrying || !strings.Contains(h[0].Reason, "The run failed: the model refused. It goes again.") {
			t.Fatalf("after the first failure = %+v", h[0])
		}
		f.clock.Add(retryPause)
		f.pass(t)
		retry := f.queue.last(t)
		if retry.ID == req.ID || retry.Metadata[MetaAttempt] != "2" || retry.IdempotencyKey != req.IdempotencyKey+"/2" {
			t.Fatalf("the retry = %+v", retry)
		}
		f.view.set(retry.ID, fw.StateFailed, 0.1, func(r *fw.Run) { r.Error = "the model refused again" })
		f.pass(t)
	}
	fail(0)
	h := f.history(t, "flaky")
	if h[0].State != StateFailed || h[0].Attempt != 2 || h[0].CostUSD != 0.2 {
		t.Fatalf("after the retries = %+v", h[0])
	}
	fail(1)
	fail(2)
	v, err := f.s.Get(as("alice"), "flaky")
	if err != nil {
		t.Fatal(err)
	}
	if !v.Paused || v.State.FailuresInRow != 3 || !strings.Contains(v.State.PausedReason, "3 runs failed in a row, so the duty paused itself. The last one: The run failed: the model refused again") {
		t.Fatalf("after three failures = paused %v, %+v", v.Paused, v.State)
	}
	if k := allNotices.kinds(f.a, "flaky"); !slices.Equal(k, []string{NoticeFailed, NoticeFailed, NoticeFailed, NoticePaused}) {
		t.Fatalf("notices = %v", k)
	}
	before := len(f.queue.list())
	f.clock.Set(nine.AddDate(0, 0, 3).Add(time.Second))
	f.pass(t)
	if len(f.queue.list()) != before {
		t.Fatal("a paused duty started")
	}
	v, err = f.s.SetPaused(as("alice"), "flaky", false)
	if err != nil || v.Paused || v.State.FailuresInRow != 0 || v.State.PausedReason != "" {
		t.Fatalf("resume = %+v, %v", v.State, err)
	}
	// The time of the pause does not run after the resume.
	f.pass(t)
	if len(f.queue.list()) != before {
		t.Fatal("a time of the pause ran after the resume")
	}
	// A completed run ends a row of failures.
	f.clock.Set(nine.AddDate(0, 0, 4).Add(time.Second))
	f.pass(t)
	req := f.queue.last(t)
	f.view.set(req.ID, fw.StateFailed, 0)
	f.pass(t)
	f.clock.Add(retryPause)
	f.pass(t)
	f.view.set(f.queue.last(t).ID, fw.StateCompleted, 0)
	f.pass(t)
	if c, _ := f.s.st.counters(context.Background(), "flaky"); c.FailuresInRow != 0 {
		t.Fatalf("a completed run left the row at %d", c.FailuresInRow)
	}
}

// A run that the queue does not take goes again, and a canceled run does
// not.
func TestQueueFailureAndCancel(t *testing.T) {
	f := newFront(t, "", nil)
	f.put(t, "alice", daily("report"))
	f.queue.setFail(errDown)
	f.clock.Set(nine.Add(time.Second))
	f.pass(t)
	if h := f.history(t, "report"); h[0].State != StateRetrying || !strings.Contains(h[0].Reason, "The queue did not take the run: the bus is down. It goes again.") {
		t.Fatalf("after a failed publish = %+v", h[0])
	}
	f.queue.setFail(nil)
	f.clock.Add(retryPause)
	f.pass(t)
	req := f.queue.last(t)
	if req.Metadata[MetaAttempt] != "2" {
		t.Fatalf("the retry = %+v", req)
	}
	f.view.set(req.ID, fw.StateCanceled, 0.01)
	f.pass(t)
	if h := f.history(t, "report"); h[0].State != StateCanceled {
		t.Fatalf("after a cancel = %+v", h[0])
	}
}

// perDayUSD counts the cost of the runs since midnight in the time zone of
// the duty: at the cap the duty skips until midnight, the history says so,
// and the owner hears of it once.
func TestDailyCap(t *testing.T) {
	f := newFront(t, "", nil)
	d := daily("capped")
	d.When.Schedule = &schedule.Schedule{Repeat: schedule.RepeatEvery, Every: "2h", TimeZone: "America/New_York"}
	d.Limits.PerDayUSD = 1
	f.put(t, "alice", d)
	start := time.Date(2026, 10, 24, 4, 0, 0, 0, time.UTC) // midnight in New York
	for i, cost := range []float64{0.6, 0.5} {
		f.clock.Set(start.Add(time.Duration(2*i) * time.Hour).Add(time.Second))
		f.pass(t)
		f.view.set(f.queue.last(t).ID, fw.StateCompleted, cost)
		f.pass(t)
	}
	f.clock.Set(start.Add(4 * time.Hour).Add(time.Second))
	f.pass(t)
	f.clock.Set(start.Add(6 * time.Hour).Add(time.Second))
	f.pass(t)
	h := f.history(t, "capped")
	if h[0].State != StateSkipped || h[1].State != StateSkipped || !strings.Contains(h[0].Reason, "The duty spent $1.10 today, and its daily cap is $1.00, so it skips until midnight, New York time.") {
		t.Fatalf("history = %+v", h[:2])
	}
	if n := len(f.queue.list()); n != 2 {
		t.Fatalf("the duty started %d runs", n)
	}
	if k := allNotices.kinds(f.a, "capped"); !slices.Equal(k, []string{NoticeCap}) {
		t.Fatalf("notices = %v", k)
	}
	// The next day in New York starts again.
	f.clock.Set(start.AddDate(0, 0, 1).Add(time.Second))
	f.pass(t)
	if n := len(f.queue.list()); n != 3 {
		t.Fatalf("the next day started %d runs in all", n)
	}
}

// The switch pauses every duty for an admin only: the times of the pause
// do not run, a person cannot run a duty now, and runs that work go on.
func TestKillSwitch(t *testing.T) {
	f := newFront(t, "", nil)
	f.put(t, "alice", daily("report"))
	if _, err := f.s.PauseAll(as("alice"), true, "storm"); err == nil || !strings.Contains(err.Error(), "admin role") {
		t.Fatalf("the switch for an owner = %v", err)
	}
	sw, err := f.s.PauseAll(as("root"), true, "storm")
	if err != nil || !sw.Paused || sw.By != "root" || sw.Reason != "storm" {
		t.Fatalf("the switch = %+v, %v", sw, err)
	}
	f.clock.Set(nine.Add(time.Second))
	f.pass(t)
	if n := len(f.queue.list()); n != 0 {
		t.Fatalf("the switch let %d runs start", n)
	}
	out, err := f.s.RunNow(as("alice"), "report")
	if err != nil || out.Skipped != "Every duty is paused: storm" {
		t.Fatalf("run now under the switch = %+v, %v", out, err)
	}
	list, _ := f.s.List(as("carol"))
	if !list.Paused || list.Reason != "storm" {
		t.Fatalf("the list = %+v", list)
	}
	f.clock.Add(10 * time.Minute)
	if _, err := f.s.PauseAll(as("root"), false, ""); err != nil {
		t.Fatal(err)
	}
	f.pass(t)
	if n := len(f.queue.list()); n != 0 {
		t.Fatal("a time of the pause ran after the switch")
	}
	f.clock.Set(nine.AddDate(0, 0, 1).Add(time.Second))
	f.pass(t)
	if n := len(f.queue.list()); n != 1 {
		t.Fatalf("the next time started %d runs", n)
	}
}

// Run now starts a duty out of its schedule, also a paused one, with the
// start now in the history.
func TestRunNow(t *testing.T) {
	f := newFront(t, "", nil)
	d := daily("report")
	d.Paused = true
	f.put(t, "alice", d)
	if _, err := f.s.RunNow(as("bob"), "report"); err == nil {
		t.Fatal("an operator ran a duty now")
	}
	out, err := f.s.RunNow(as("alice"), "report")
	if err != nil || out.Run == "" {
		t.Fatalf("run now = %+v, %v", out, err)
	}
	req := f.queue.last(t)
	if req.ID != out.Run || !strings.HasPrefix(req.IdempotencyKey, "duty/report/2026-10-23T16:00:00") {
		t.Fatalf("the request = %+v", req)
	}
	h := f.history(t, "report")
	if h[0].Start != StartNow || h[0].Run != out.Run {
		t.Fatalf("history = %+v", h)
	}
	// overlap skip holds for a run now too.
	f.clock.Add(time.Second)
	out, err = f.s.RunNow(as("alice"), "report")
	if err != nil || !strings.Contains(out.Skipped, "had not ended") {
		t.Fatalf("a second run now = %+v, %v", out, err)
	}
}

// runAs owner runs as the owner, and a change by another person turns it
// back to the principal of the duty.
func TestRunAsOwner(t *testing.T) {
	f := newFront(t, "", nil)
	d := daily("mine")
	d.RunAs = RunAsOwner
	v := f.put(t, "alice", d)
	if v.Owner != "alice" || v.RunAs != RunAsOwner {
		t.Fatalf("the duty = owner %q, runAs %q", v.Owner, v.RunAs)
	}
	if _, err := f.s.RunNow(as("alice"), "mine"); err != nil {
		t.Fatal(err)
	}
	if req := f.queue.last(t); req.Principal != "alice" || req.PrincipalKind != fw.KindUser {
		t.Fatalf("the principal = %q %q", req.Principal, req.PrincipalKind)
	}
	d.Do.Ask.Message = "Another message."
	ans, err := f.s.Put(as("root"), "mine", DutyBody{Duty: d}, PutOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if ans.View.RunAs != RunAsDuty || ans.View.Owner != "alice" {
		t.Fatalf("after a change by an admin = runAs %q, owner %q", ans.View.RunAs, ans.View.Owner)
	}
}

// A start whose replica stopped before its run reached the queue goes
// again; one whose run went out waits for the run; and a run that no
// worker reports for lostAfter is lost.
func TestStaleAndLost(t *testing.T) {
	f := newFront(t, "", nil)
	f.put(t, "alice", daily("report"))
	ctx := context.Background()
	date := "2026-10-23"
	stale := entry{ID: "a", RunEntry: RunEntry{Slot: t0, State: StateStarting, Start: StartNow, Attempt: 1}, Updated: t0}
	sent := entry{ID: "b", RunEntry: RunEntry{Slot: t0.Add(time.Second), State: StateStarting, Start: StartNow, Attempt: 1, Run: "R-SENT"}, Updated: t0}
	queued := entry{ID: "c", RunEntry: RunEntry{Slot: t0.Add(2 * time.Second), State: StateQueued, Start: StartNow, Attempt: 1, Run: "R-GONE"}, Updated: t0}
	for _, e := range []entry{stale, sent, queued} {
		if _, err := f.s.st.claim(ctx, "report", date, e); err != nil {
			t.Fatal(err)
		}
	}
	f.clock.Set(t0.Add(time.Minute))
	f.pass(t)
	if s := f.states(t, "report"); !slices.Equal(s, []string{StateQueued, StateStarting, StateStarting}) {
		t.Fatalf("within staleStart = %v", s)
	}
	f.clock.Set(t0.Add(3 * time.Minute))
	f.view.set("R-SENT", fw.StateRunning, 0)
	f.pass(t)
	if s := f.states(t, "report"); !slices.Equal(s, []string{StateQueued, "running", StateRetrying}) {
		t.Fatalf("after staleStart = %v", s)
	}
	f.clock.Set(t0.Add(lostAfter + time.Hour))
	f.pass(t)
	h := f.history(t, "report")
	if h[0].State != StateLost || !strings.Contains(h[0].Reason, "No worker reported the run R-GONE for 2h") {
		t.Fatalf("a run that nobody reports = %+v", h[0])
	}
}

// The intake of the kernel can retry a failed run: the start follows the
// retry, adds its cost, and decides at its end.
func TestFollowsIntakeRetry(t *testing.T) {
	f := newFront(t, "", nil)
	d := daily("report")
	d.Limits.Retries = intp(0)
	f.put(t, "alice", d)
	f.clock.Set(nine.Add(time.Second))
	f.pass(t)
	id := f.queue.last(t).ID
	f.view.set(id, fw.StateFailed, 0.1, func(r *fw.Run) { r.Metadata[fw.MetaIntakeQueue] = "fw.jobs.triage" })
	f.pass(t)
	if h := f.history(t, "report"); h[0].State != "running" {
		t.Fatalf("a failed run that the intake owes = %+v", h[0])
	}
	f.view.set(id, fw.StateFailed, 0.1, func(r *fw.Run) {
		r.Metadata[fw.MetaIntakeQueue], r.Metadata[fw.MetaIntakeOutcome] = "fw.jobs.triage", fw.IntakeRetried
	})
	f.view.set("RETRY", fw.StateCompleted, 0.2, func(r *fw.Run) { r.Metadata[fw.MetaIntakeRetryOf] = id })
	f.pass(t)
	h := f.history(t, "report")
	if h[0].State != StateCompleted || h[0].Run != "RETRY" || h[0].CostUSD < 0.3-1e-9 || h[0].CostUSD > 0.3+1e-9 {
		t.Fatalf("after the retry of the intake = %+v", h[0])
	}
}

// A run that asks a person tells the people of notify.needsYou once.
func TestNeedsYou(t *testing.T) {
	f := newFront(t, "", nil)
	d := daily("asks")
	d.Notify = &Notify{NeedsYou: []string{"owner", "erin"}}
	f.put(t, "alice", d)
	f.clock.Set(nine.Add(time.Second))
	f.pass(t)
	f.view.set(f.queue.last(t).ID, fw.StateInputRequired, 0)
	f.pass(t)
	f.clock.Add(time.Minute)
	f.pass(t)
	got := allNotices.of(f.a, "asks")
	if len(got) != 1 || got[0].Kind != NoticeNeedsYou || !slices.Equal(got[0].Who, []string{"alice", "erin"}) {
		t.Fatalf("notices = %+v", got)
	}
}

// The placeholders fill from the start, the last run, and the duty; any
// other text in braces stays.
func TestPlaceholders(t *testing.T) {
	f := newFront(t, "", nil)
	d := daily("report")
	d.Do.Ask.Message = "Date {date} {time}. Last: {last.title} ({last.run}, {last.state}). Found: {found}. Event: {event}. Other: {other} {duty}."
	f.put(t, "alice", d)
	f.clock.Set(nine.Add(time.Second))
	f.pass(t)
	first := f.queue.last(t)
	if first.Input != "Date 2026-10-24 09:00. Last: none (none, none). Found: none. Event: none. Other: {other} report." {
		t.Fatalf("the first input = %q", first.Input)
	}
	f.view.set(first.ID, fw.StateCompleted, 0, func(r *fw.Run) { r.Title = "Report of Saturday" })
	f.pass(t)
	f.clock.Set(nine.AddDate(0, 0, 1).Add(time.Second))
	f.pass(t)
	second := f.queue.last(t)
	want := "Date 2026-10-25 09:00. Last: Report of Saturday (" + first.ID + ", completed)."
	if !strings.HasPrefix(second.Input, want) {
		t.Fatalf("the second input = %q", second.Input)
	}
	if got := fill("{date}{date}", map[string]string{"{date}": "{time}"}); got != "{time}{time}" {
		t.Fatalf("a value with a placeholder = %q", got)
	}
}

// Without a fleet view the engine reads the runs of this process: a run
// of the batch of the duty counts.
func TestRunsOfThisProcess(t *testing.T) {
	f := newFront(t, "", nil)
	s := newService(f.a, memStore(&clock{t: t0}), Config{}, quiet())
	runs, err := s.runsOfFleet(context.Background(), "report")
	if err != nil || len(runs) != 0 {
		t.Fatalf("runs = %v, %v", runs, err)
	}
	r, err := f.a.Start(context.Background(), "hello", fw.AsAgent("triage"), func(req *fw.RunRequest) {
		req.Metadata = map[string]string{fw.MetaBatch: BatchOf("report")}
	})
	if err != nil {
		t.Fatal(err)
	}
	runs, err = s.runsOfFleet(context.Background(), "report")
	if err != nil || runs[r.ID] == nil {
		t.Fatalf("runs = %v, %v", runs, err)
	}
}

// The service opens on the bus mem, and a bus without a store keeps no
// duties.
func TestServiceOpens(t *testing.T) {
	f := newFront(t, "", nil)
	if Of(f.a) == nil {
		t.Fatal("no duties")
	}
	cfg := fw.DefaultConfig()
	if ok, line := Status(cfg); !ok || !strings.Contains(line, "the memory of this process") {
		t.Fatalf("Status = %v, %q", ok, line)
	}
	cfg.Bus.Driver = "redis"
	if ok, line := Status(cfg); !ok || !strings.Contains(line, "off") {
		t.Fatalf("Status on redis = %v, %q", ok, line)
	}
	if _, _, err := bucket.Open(context.Background(), cfg, dutiesBucket, nil); err == nil {
		t.Fatal("a store opened on redis")
	}
}

// The service starts on the bus mem with its engine, and its close stops
// the engine and detaches the duties.
func TestStartAndClose(t *testing.T) {
	cfg := fw.DefaultConfig()
	cfg.Home = t.TempDir()
	a, err := fw.New(fw.WithConfig(cfg), fw.WithoutDefaults(), fw.WithAuthn(fw.NoAuthn{}),
		fw.WithLedger(fw.NewMemLedger()), fw.WithBus(fw.NewMemBus()), fw.WithLogger(quiet()),
		fw.WithModel(fw.ModelFunc{GenerateFn: func(context.Context, *fw.Request) (*fw.Response, error) {
			return &fw.Response{Content: []fw.Block{fw.Text("done")}, StopReason: fw.StopEndTurn}, nil
		}}))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close(context.Background())
	c, err := Start(context.Background(), a)
	if err != nil || c == nil || Of(a) == nil {
		t.Fatalf("Start = %v, %v", c, err)
	}
	Of(a).wake()
	if err := c.Close(); err != nil || Of(a) != nil {
		t.Fatalf("Close = %v, %v", err, Of(a))
	}
	cfg.Bus.Driver = "redis"
	if c, err := Start(context.Background(), a); err != nil || c != nil {
		t.Fatalf("Start on redis = %v, %v", c, err)
	}
	ok, line := DoctorCheck(context.Background(), func() *fw.Config {
		c := fw.DefaultConfig()
		c.Extensions = workbench(t, "duties:\n  secrets: [{name: DUTY_SECRET_X, hosts: [api.example.com]}]\n")
		return c
	}(), fw.SecretsFunc(func(context.Context, string) (string, error) { return "", fw.ErrSecretNotFound }), time.Second)
	if ok || !strings.Contains(line, "a duty may name DUTY_SECRET_X for api.example.com, but it holds no secret") {
		t.Fatalf("doctor = %v, %q", ok, line)
	}
}

// A duty holds at most maxWorking starts that work at one time.
func TestMaxWorking(t *testing.T) {
	f := newFront(t, checkBlock, nil)
	f.sec["HOOK_TOKEN"] = "tok"
	f.put(t, "alice", hookDuty("burst"))
	st, _ := f.s.st.get(context.Background(), "burst")
	for i := range maxWorking + 2 {
		out, err := f.s.begin(context.Background(), st, Trigger{Kind: StartCall, Slot: f.clock.Now(), Event: "x"})
		if err != nil {
			t.Fatal(err)
		}
		if i < maxWorking && out.Run == "" {
			t.Fatalf("start %d = %+v", i, out)
		}
		if i >= maxWorking && out.Skipped != "40 runs of the duty work now, the most at one time." {
			t.Fatalf("start %d = %+v", i, out)
		}
	}
}
