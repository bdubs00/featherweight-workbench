package pools

import (
	"context"
	"io"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	v1alpha1 "github.com/google/ax/pkg/apis/v1alpha1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// rig is one pool, the ax API, and the checks of the front.
type rig struct {
	t     *testing.T
	p     *Pool
	ax    *fakeAX
	check *fakeCheck
	rec   *Reconciler
}

func newRig(t *testing.T, text string) *rig {
	t.Helper()
	r := &rig{t: t, p: loadFixture(t, text)[0], ax: newFakeAX(), check: newFakeCheck()}
	r.rec = r.reconciler()
	return r
}

// reconciler is a reconciler over the ax API and the checks of the rig, as
// one replica of the front has it.
func (r *rig) reconciler() *Reconciler {
	r.t.Helper()
	vals, err := Resolve([]*Pool{r.p}, testLookup)
	if err != nil {
		r.t.Fatal(err)
	}
	return &Reconciler{AX: r.ax, Check: r.check, Values: vals, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

// pass runs one pass over the Tasks that the ax API lists now.
func (r *rig) pass() Status {
	r.t.Helper()
	return r.rec.Reconcile(context.Background(), r.p, r.ax.list(r.p.Atespace), time.Now())
}

// worker stores a Task of the pool at index, with a hash, a phase, and
// whether it ran once.
func (r *rig) worker(index int, hash, phase string, started bool) {
	r.t.Helper()
	task, ws, err := r.p.Render(index, testLookup)
	if err != nil {
		r.t.Fatal(err)
	}
	for _, e := range task.Spec.Env {
		if e.Name == EnvSpec {
			e.Value = hash
		}
	}
	r.ax.put(task, phase, started)
	r.ax.workspaces["team/"+ws.Metadata.Name] = ws
}

func names(ts []*v1alpha1.Task) []string {
	var out []string
	for _, t := range ts {
		out = append(out, t.Metadata.Name)
	}
	return out
}

func hasGap(st Status, part string) bool {
	return slices.ContainsFunc(st.Gaps, func(g string) bool { return strings.Contains(g, part) })
}

// A missing Task is created with its Workspace first and resumed, with the
// marks of the pool and the token of the front. A pass over a pool that
// matches its configuration changes nothing.
func TestReconcileCreatesTheMissingTasks(t *testing.T) {
	r := newRig(t, fixture)
	st := r.pass()
	want := []string{
		"workspace team/triage-0-ws", "create team/triage-0", "resume team/triage-0",
		"workspace team/triage-1-ws", "create team/triage-1", "resume team/triage-1",
		"workspace team/triage-2-ws", "create team/triage-2", "resume team/triage-2",
	}
	if got := r.ax.took(); !slices.Equal(got, want) {
		t.Fatalf("calls = %v", got)
	}
	if st.Wanted != 3 || st.Actual != 3 || st.Ready != 0 || !hasGap(st, "the pool created triage-0, triage-1, triage-2; they start") {
		t.Fatalf("status = %+v", st)
	}
	task := r.ax.task("team", "triage-1")
	if envOf(task, EnvPool) != "triage" || envOf(task, EnvSpec) != r.p.Hash() || envOf(task, "FW_API_TOKEN") != "front-secret-token" || task.Status.Phase != phaseRunning {
		t.Fatalf("Task = %+v", task)
	}
	if ws := r.ax.workspaces["team/triage-1-ws"]; ws == nil || !r.p.ownsWorkspace(ws) {
		t.Fatalf("Workspace = %+v", ws)
	}

	st = r.pass()
	if got := r.ax.took(); len(got) != 0 {
		t.Fatalf("a pass over a pool as configured made calls: %v", got)
	}
	if st.Actual != 3 || st.Ready != 3 || len(st.Gaps) != 0 || len(r.check.reads) != 0 {
		t.Fatalf("status = %+v, reads %v", st, r.check.reads)
	}
}

// A Task that the pool does not own stays as it is: a Task with the name of
// the pool and no FW_POOL, and a child Task of a worker, which carries
// FW_POOL under another name. A Workspace that the pool did not render
// keeps its name too. The pool takes the next free names.
func TestReconcileLeavesForeignTasksAlone(t *testing.T) {
	r := newRig(t, fixture)
	r.ax.put(&v1alpha1.Task{Metadata: &v1alpha1.ObjectMeta{Name: "triage-1", Atespace: "team"}, Spec: &v1alpha1.TaskSpec{Image: testImage}}, phaseRunning, true)
	child, _, _ := r.p.Render(0, testLookup)
	child.Metadata.Name = "triage-0-01k8zj4x7ad9c0n3d5r6s7t8v9"
	r.ax.put(child, phaseRunning, true)
	r.ax.workspaces["team/triage-2-ws"] = &v1alpha1.Workspace{Metadata: &v1alpha1.ObjectMeta{Name: "triage-2-ws", Atespace: "team"},
		Spec: &v1alpha1.WorkspaceSpec{Files: []*v1alpha1.File{{Path: ConfigFile, Content: "model: x\n"}}}}

	st := r.pass()
	got := r.ax.took()
	for _, c := range got {
		if strings.HasSuffix(c, "/triage-1") || strings.Contains(c, "triage-2") || strings.Contains(c, "01k8zj") {
			t.Fatalf("the pool touched a Task that it does not own: %v", got)
		}
	}
	if all := names(r.ax.list("team")); !slices.Equal(all, []string{"triage-0", "triage-0-01k8zj4x7ad9c0n3d5r6s7t8v9", "triage-1", "triage-3", "triage-4"}) {
		t.Fatalf("Tasks = %v", all)
	}
	if !hasGap(st, "triage-1 has the name of a Task of the pool but no FW_POOL=triage") || !hasGap(st, "the Workspace triage-2-ws exists and belongs to no pool") || st.Actual != 3 {
		t.Fatalf("status = %+v", st)
	}
}

// Two replicas of the front act on one pool without a leader. Both create
// the same names, and ax makes each Task once. A replica that judged a Task
// before the other replaced it does not delete the new Task.
func TestReconcileRepeatsSafely(t *testing.T) {
	r := newRig(t, fixture)
	other := r.reconciler()
	seen := r.ax.list("team") // both replicas listed the atespace empty
	r.rec.Reconcile(context.Background(), r.p, seen, time.Now())
	st := other.Reconcile(context.Background(), r.p, seen, time.Now())
	if all := names(r.ax.list("team")); !slices.Equal(all, []string{"triage-0", "triage-1", "triage-2"}) {
		t.Fatalf("Tasks = %v", all)
	}
	if hasGap(st, "could not create") {
		t.Fatalf("the second replica failed: %v", st.Gaps)
	}

	// A Task of an older configuration: one replica lists it, the other
	// replaces it, and the first one deletes it after a status read.
	r2 := newRig(t, fixture)
	r2.worker(0, "oldhash", phaseRunning, true)
	r2.worker(1, r2.p.Hash(), phaseRunning, true)
	r2.worker(2, r2.p.Hash(), phaseRunning, true)
	stale := r2.ax.list("team")
	ctx := context.Background()
	r2.rec.Reconcile(ctx, r2.p, r2.ax.list("team"), time.Now()) // deletes triage-0
	r2.rec.Reconcile(ctx, r2.p, r2.ax.list("team"), time.Now()) // creates it again
	before := r2.ax.task("team", "triage-0")
	r2.ax.took()
	r2.reconciler().Reconcile(ctx, r2.p, stale, time.Now())
	if got := r2.ax.took(); slices.Contains(got, "delete team/triage-0") {
		t.Fatalf("a replica with an older list deleted the new Task: %v", got)
	}
	if after := r2.ax.task("team", "triage-0"); after == nil || !sameTime(after.Metadata.CreateTime, before.Metadata.CreateTime) {
		t.Fatal("the new Task is gone")
	}
}

// The pool replaces one Task of an older configuration at a time, when
// every Task is up, and never one that holds an active run, whether the
// view knows the run or only the Task does.
func TestReconcileReplacesOneStaleTaskAtATime(t *testing.T) {
	r := newRig(t, fixture)
	for i := range 3 {
		r.worker(i, "oldhash", phaseRunning, true)
	}
	r.check.active["team/triage-1"] = []string{"01RUN"}
	r.check.busy["team/triage-1"] = 1

	st := r.pass()
	if got := r.ax.took(); !slices.Equal(got, []string{"delete team/triage-2"}) {
		t.Fatalf("pass 1 calls = %v", got)
	}
	if !hasGap(st, "triage-2 runs an older configuration; the pool deleted it") || !hasGap(st, "triage-1 runs an older configuration; the pool replaces one Task at a time") {
		t.Fatalf("pass 1 gaps = %v", st.Gaps)
	}
	if r.check.reads[0] != "team/triage-2" {
		t.Fatalf("reads = %v", r.check.reads)
	}

	r.pass() // creates triage-2 with the current configuration
	if got := r.ax.took(); !slices.Equal(got, []string{"workspace team/triage-2-ws", "create team/triage-2", "resume team/triage-2"}) {
		t.Fatalf("pass 2 calls = %v", got)
	}
	r.pass() // replaces triage-0, the idle one
	if got := r.ax.took(); !slices.Equal(got, []string{"delete team/triage-0"}) {
		t.Fatalf("pass 3 calls = %v", got)
	}
	r.pass()
	r.ax.took()

	// triage-1 holds a run that the view knows: it waits.
	st = r.pass()
	if got := r.ax.took(); len(got) != 0 || !hasGap(st, "triage-1 runs an older configuration and holds the active run 01RUN") {
		t.Fatalf("calls %v, gaps %v", got, st.Gaps)
	}
	// The view missed the run, and the Task says that it works: it waits.
	delete(r.check.active, "team/triage-1")
	st = r.pass()
	if got := r.ax.took(); len(got) != 0 || !hasGap(st, "holds 1 active run") {
		t.Fatalf("calls %v, gaps %v", got, st.Gaps)
	}
	// A Task that does not answer is not idle either.
	r.check.busy["team/triage-1"] = 0
	r.check.failing["team/triage-1"] = true
	st = r.pass()
	if got := r.ax.took(); len(got) != 0 || !hasGap(st, "did not say whether it works") {
		t.Fatalf("calls %v, gaps %v", got, st.Gaps)
	}
	// The view still holds the run after a lost event, and the Task says
	// that it holds none: the ledger of the Task decides.
	r.check.failing["team/triage-1"] = false
	r.check.active["team/triage-1"] = []string{"01RUN"}
	r.pass()
	if got := r.ax.took(); !slices.Equal(got, []string{"delete team/triage-1"}) {
		t.Fatalf("calls = %v", got)
	}

	// While a Task is missing, no Task is replaced.
	r3 := newRig(t, fixture)
	r3.worker(0, "oldhash", phaseRunning, true)
	r3.worker(1, "oldhash", phaseRunning, true)
	st = r3.pass()
	if got := r3.ax.took(); slices.Contains(got, "delete team/triage-0") || slices.Contains(got, "delete team/triage-1") || !hasGap(st, "once every Task is up") {
		t.Fatalf("calls %v, gaps %v", got, st.Gaps)
	}
}

// A run that the stream reports between the status read and the delete
// keeps the Task.
func TestReconcileKeepsATaskThatTookARun(t *testing.T) {
	r := newRig(t, strings.Replace(fixture, "replicas: 3", "replicas: 1", 1))
	r.worker(0, r.p.Hash(), phaseRunning, true)
	r.worker(1, r.p.Hash(), phaseRunning, true)
	r.check.late = map[string][]string{"team/triage-1": {"01LATE"}, "team/triage-0": {"01LATE0"}}
	st := r.pass()
	if got := r.ax.took(); len(got) != 0 {
		t.Fatalf("calls = %v", got)
	}
	if !hasGap(st, "took the run 01LATE since the check") {
		t.Fatalf("gaps = %v", st.Gaps)
	}
}

// Above the replica count, the idle Tasks go first, with their Workspaces,
// and a Task that holds a run stays.
func TestReconcileScalesDownIdleFirst(t *testing.T) {
	r := newRig(t, fixture)
	for i := range 5 {
		r.worker(i, r.p.Hash(), phaseRunning, true)
	}
	r.check.busy["team/triage-4"] = 1
	r.check.active["team/triage-3"] = []string{"01RUN"}
	st := r.pass()
	if all := names(r.ax.list("team")); !slices.Equal(all, []string{"triage-0", "triage-3", "triage-4"}) {
		t.Fatalf("Tasks = %v", all)
	}
	if _, ok := r.ax.workspaces["team/triage-2-ws"]; ok {
		t.Fatal("the Workspace of a deleted Task stays")
	}
	if st.Actual != 3 || len(st.Gaps) != 0 {
		t.Fatalf("status = %+v", st)
	}

	// At zero, every idle Task goes, and the busy ones wait with a reason.
	zero := strings.Replace(fixture, "replicas: 3", "replicas: 0", 1)
	r.p = loadFixture(t, zero)[0]
	r.check.active = map[string][]string{"team/triage-3": {"01RUN"}}
	r.check.busy = map[string]int{"team/triage-3": 1}
	for _, name := range []string{"triage-0", "triage-4"} {
		task := r.ax.task("team", name)
		for _, e := range task.Spec.Env {
			if e.Name == EnvSpec {
				e.Value = r.p.Hash()
			}
		}
		r.ax.put(task, phaseSuspended, true)
	}
	st = r.pass()
	if all := names(r.ax.list("team")); !slices.Equal(all, []string{"triage-3"}) {
		t.Fatalf("Tasks = %v", all)
	}
	if !hasGap(st, "1 Task is above the replica count, and the pool deletes only an idle Task: triage-3 holds the active run 01RUN") {
		t.Fatalf("gaps = %v", st.Gaps)
	}
}

// A delete that ax did not finish is sent again, a Task of the pool that
// never ran is resumed, a failed Task stays with its reason, and a failed
// Task of an older configuration that never ran is replaced without a
// status read.
func TestReconcileRecovers(t *testing.T) {
	r := newRig(t, fixture)
	r.worker(0, r.p.Hash(), v1alpha1.PhaseTerminating, true)
	r.worker(1, r.p.Hash(), phaseSuspended, false)
	r.worker(2, r.p.Hash(), phaseFailed, true)
	r.ax.tasks["team/triage-2"].Status.Conditions = append(r.ax.tasks["team/triage-2"].Status.Conditions,
		&v1alpha1.Condition{Type: "Ready", Status: "False", Reason: "ActorResumeFailed", Message: "the image cannot be pulled"})
	st := r.pass()
	got := r.ax.took()
	for _, want := range []string{"delete team/triage-0", "resume team/triage-1"} {
		if !slices.Contains(got, want) {
			t.Fatalf("calls %v, want %q", got, want)
		}
	}
	// triage-2 failed after it ran: it stays, it does not count, and the
	// pool makes up for it and for the Task that is being deleted.
	if slices.Contains(got, "delete team/triage-2") || !hasGap(st, "triage-2 failed after it ran (the image cannot be pulled)") ||
		!slices.Contains(got, "create team/triage-3") || !slices.Contains(got, "create team/triage-4") || st.Actual != 3 {
		t.Fatalf("calls %v, gaps %v, actual %d", got, st.Gaps, st.Actual)
	}

	r2 := newRig(t, fixture)
	r2.worker(0, r2.p.Hash(), phaseRunning, true)
	r2.worker(1, r2.p.Hash(), phaseRunning, true)
	r2.worker(2, "oldhash", phaseFailed, false)
	r2.pass()
	if got := r2.ax.took(); !slices.Equal(got, []string{"delete team/triage-2"}) || len(r2.check.reads) != 0 {
		t.Fatalf("calls %v, reads %v", got, r2.check.reads)
	}

	// A create that ax refuses is a gap, and the pass makes no other Task.
	// An error that echoes the Task carries no secret into the gap.
	r3 := newRig(t, fixture)
	r3.ax.createErr["triage-0"] = status.Error(codes.Unavailable, "substrate refused the template with FW_API_TOKEN=front-secret-token")
	st = r3.pass()
	if got := r3.ax.took(); !slices.Equal(got, []string{"workspace team/triage-0-ws", "create team/triage-0"}) || !hasGap(st, "could not create triage-0") {
		t.Fatalf("calls %v, gaps %v", got, st.Gaps)
	}
	if hasGap(st, "front-secret-token") || !hasGap(st, "FW_API_TOKEN=<secret>") {
		t.Fatalf("gaps = %v", st.Gaps)
	}
}

// A Task that ax says never ran but that the view knows a run of, because
// a request through the router woke it, is asked before it goes.
func TestReconcileAsksAWokenTask(t *testing.T) {
	r := newRig(t, strings.Replace(fixture, "replicas: 3", "replicas: 0", 1))
	r.worker(1, r.p.Hash(), phaseSuspended, false)
	r.ax.resumeErr["triage-1"] = status.Error(codes.Unavailable, "substrate is busy")
	r.check.active["team/triage-1"] = []string{"01RUN"}
	r.check.busy["team/triage-1"] = 1
	st := r.pass()
	if got := r.ax.took(); slices.Contains(got, "delete team/triage-1") {
		t.Fatalf("calls %v", got)
	}
	if !hasGap(st, "triage-1 holds the active run 01RUN") {
		t.Fatalf("gaps = %v", st.Gaps)
	}
}

// A delete that ax did not finish waits while the actor holds a run, and it
// goes again once the run ends.
func TestReconcileRedeletesOnlyAnIdleTask(t *testing.T) {
	r := newRig(t, fixture)
	for i := range 3 {
		r.worker(i, r.p.Hash(), phaseRunning, true)
	}
	r.worker(3, r.p.Hash(), v1alpha1.PhaseTerminating, true)
	r.check.active["team/triage-3"] = []string{"01RUN"}
	r.check.busy["team/triage-3"] = 1
	st := r.pass()
	if got := r.ax.took(); len(got) != 0 || !hasGap(st, "triage-3 is being deleted but holds the active run 01RUN") {
		t.Fatalf("calls %v, gaps %v", got, st.Gaps)
	}
	// The view still holds the run, and the Task says that it ended.
	r.check.busy["team/triage-3"] = 0
	r.pass()
	if got := r.ax.took(); !slices.Equal(got, []string{"delete team/triage-3"}) {
		t.Fatalf("calls = %v", got)
	}
}

// A running Task of an older configuration whose kernel never answered, of
// which the view knows no run, is replaced like a Task that never ran.
func TestReconcileReplacesAKernelThatNeverAnswered(t *testing.T) {
	r := newRig(t, strings.Replace(fixture, "replicas: 3", "replicas: 1", 1))
	r.worker(0, "oldhash", phaseRunning, true)
	r.ax.tasks["team/triage-0"].Status.Conditions[0].Status = "False"
	r.check.failing["team/triage-0"] = true
	r.pass()
	if got := r.ax.took(); !slices.Equal(got, []string{"delete team/triage-0"}) {
		t.Fatalf("calls = %v", got)
	}
	// With a run in the view, the Task stays.
	r.worker(0, "oldhash", phaseRunning, true)
	r.ax.tasks["team/triage-0"].Status.Conditions[0].Status = "False"
	r.check.active["team/triage-0"] = []string{"01RUN"}
	st := r.pass()
	if got := r.ax.took(); len(got) != 0 || !hasGap(st, "did not say whether it works") {
		t.Fatalf("calls %v, gaps %v", got, st.Gaps)
	}
}

// The retry of a failed Task counts from the failure, not from the create.
func TestReconcileRetriesFromTheFailure(t *testing.T) {
	r := newRig(t, strings.Replace(fixture, "replicas: 3", "replicas: 1", 1))
	r.worker(0, r.p.Hash(), phaseFailed, false)
	failed := time.Now()
	r.ax.tasks["team/triage-0"].Status.Conditions = append(r.ax.tasks["team/triage-0"].Status.Conditions,
		&v1alpha1.Condition{Type: "Ready", Status: "False", Message: "the actor did not start", UpdateTime: timestamppb.New(failed)})
	st := r.rec.Reconcile(context.Background(), r.p, r.ax.list("team"), failed.Add(time.Minute))
	if got := r.ax.took(); len(got) != 0 || !hasGap(st, "the actor did not start") {
		t.Fatalf("calls %v, gaps %v", got, st.Gaps)
	}
	r.rec.Reconcile(context.Background(), r.p, r.ax.list("team"), failed.Add(FailedRetry+time.Second))
	if got := r.ax.took(); !slices.Equal(got, []string{"delete team/triage-0"}) {
		t.Fatalf("calls = %v", got)
	}
}

// A Task that failed after a run never blocks the roll of the others, and a
// failed Task of the current configuration that never ran is created again
// after FailedRetry.
func TestReconcileRollsPastAFailedTask(t *testing.T) {
	r := newRig(t, fixture)
	r.worker(0, "oldhash", phaseRunning, true)
	r.worker(1, r.p.Hash(), phaseRunning, true)
	r.worker(2, r.p.Hash(), phaseFailed, true)
	r.worker(3, r.p.Hash(), phaseRunning, true)
	st := r.pass()
	if got := r.ax.took(); !slices.Equal(got, []string{"delete team/triage-0"}) {
		t.Fatalf("calls %v, gaps %v", got, st.Gaps)
	}

	r2 := newRig(t, strings.Replace(fixture, "replicas: 3", "replicas: 1", 1))
	r2.worker(0, r2.p.Hash(), phaseFailed, false)
	created := r2.ax.task("team", "triage-0").Metadata.CreateTime.AsTime()
	st = r2.rec.Reconcile(context.Background(), r2.p, r2.ax.list("team"), created.Add(time.Minute))
	if got := r2.ax.took(); len(got) != 0 || !hasGap(st, "triage-0 failed before it ran") {
		t.Fatalf("calls %v, gaps %v", got, st.Gaps)
	}
	r2.rec.Reconcile(context.Background(), r2.p, r2.ax.list("team"), created.Add(FailedRetry+time.Minute))
	if got := r2.ax.took(); !slices.Equal(got, []string{"delete team/triage-0"}) || len(r2.check.reads) != 0 {
		t.Fatalf("calls %v, reads %v", got, r2.check.reads)
	}
}

// A Task that holds an older value of a variable of the front, such as a
// static token before a rotation, is replaced, and the front reads it with
// the token that it holds.
func TestReconcileReplacesAnOlderValue(t *testing.T) {
	r := newRig(t, strings.Replace(fixture, "replicas: 3", "replicas: 1", 1))
	r.worker(0, r.p.Hash(), phaseRunning, true)
	for _, e := range r.ax.tasks["team/triage-0"].Spec.Env {
		if e.Name == "FW_API_TOKEN" {
			e.Value = "the-token-before-the-rotation"
		}
	}
	st := r.pass()
	if got := r.ax.took(); !slices.Equal(got, []string{"delete team/triage-0"}) {
		t.Fatalf("calls %v, gaps %v", got, st.Gaps)
	}
	if !slices.Equal(r.check.tokens, []string{"the-token-before-the-rotation"}) {
		t.Fatalf("tokens = %v", r.check.tokens)
	}
	if !hasGap(st, "triage-0 runs an older value of a variable of the front") {
		t.Fatalf("gaps = %v", st.Gaps)
	}
	for _, g := range st.Gaps {
		if strings.Contains(g, "the-token-before") || strings.Contains(g, "front-secret-token") {
			t.Fatalf("a gap holds a token: %q", g)
		}
	}
}

// A replica with an older list does not write the Workspace of a Task that
// exists.
func TestReconcileLeavesTheWorkspaceOfALiveTask(t *testing.T) {
	r := newRig(t, strings.Replace(fixture, "replicas: 3", "replicas: 1", 1))
	r.worker(0, r.p.Hash(), phaseRunning, true)
	r.ax.took()
	r.rec.Reconcile(context.Background(), r.p, nil, time.Now()) // a list from before the Task
	if got := r.ax.took(); len(got) != 0 {
		t.Fatalf("calls = %v", got)
	}
}
