package pools

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	v1alpha1 "github.com/google/ax/pkg/apis/v1alpha1"
)

// withGeneration returns the fixture with a generation and, when rev is not
// empty, a revision, so two replicas of the front can hold two
// configurations of the pool.
func withGeneration(gen int, rev string) string {
	extra := fmt.Sprintf("replicas: 3\n    generation: %d", gen)
	if rev != "" {
		extra += "\n    revision: " + rev
	}
	return strings.Replace(fixture, "replicas: 3", extra, 1)
}

// replica is one replica of the front with its own configuration of the
// pool, over the ax API and the view that every replica shares.
type replica struct {
	t   *testing.T
	p   *Pool
	ax  *fakeAX
	rec *Reconciler
	log *bytes.Buffer
}

func newReplica(t *testing.T, text string, ax *fakeAX, check *fakeCheck) *replica {
	t.Helper()
	p := loadFixture(t, text)[0]
	vals, err := Resolve([]*Pool{p}, testLookup)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	return &replica{t: t, p: p, ax: ax, rec: &Reconciler{AX: ax, Check: check, Values: vals, Log: slog.New(slog.NewTextHandler(&buf, nil))}, log: &buf}
}

// pass runs one pass over the Tasks that the ax API lists now.
func (r *replica) pass() Status {
	r.t.Helper()
	return r.rec.Reconcile(context.Background(), r.p, r.ax.list(r.p.Atespace), time.Now())
}

// worker stores a Task of the pool of this replica at index, as that
// replica renders it.
func (r *replica) worker(index int, phase string, started bool) {
	r.t.Helper()
	task, ws, err := r.p.Render(index, testLookup)
	if err != nil {
		r.t.Fatal(err)
	}
	r.ax.put(task, phase, started)
	r.ax.workspaces["team/"+ws.Metadata.Name] = ws
}

// Two replicas with generations 6 and 7 share a pool. The replica of
// generation 7 records its generation, and the replica of generation 6
// stops before the first gap opens, so the pool converges on generation 7
// and stays there. Without the generation, the two replicas deleted and
// created each other's Tasks every interval (finding 6 of the ticket soak).
func TestGenerationEndsTheFight(t *testing.T) {
	ax, check := newFakeAX(), newFakeCheck()
	old := newReplica(t, withGeneration(6, ""), ax, check)
	cur := newReplica(t, withGeneration(7, ""), ax, check)
	for i := range 3 {
		old.worker(i, phaseRunning, true)
	}
	var total []string
	quiet := 0
	for range 40 {
		old.pass()
		cur.pass()
		calls := ax.took()
		if len(calls) == 0 {
			quiet++
		} else {
			quiet = 0
		}
		total = append(total, calls...)
	}
	for _, task := range ax.list("team") {
		if genOf(task) != 7 || envOf(task, EnvSpec) != cur.p.Hash() {
			t.Fatalf("%s carries generation %d and hash %s", task.Metadata.Name, genOf(task), envOf(task, EnvSpec))
		}
	}
	if n := len(ax.list("team")); n != 3 {
		t.Fatalf("%d Tasks, want 3", n)
	}
	// Two marker writes, then each Task goes once and comes back once with
	// its Workspace, its create, and its resume.
	if len(total) != 2+3*4 || quiet < 30 {
		t.Fatalf("%d calls, %d quiet rounds at the end: %v", len(total), quiet, total)
	}
	for _, c := range total {
		if strings.HasPrefix(c, "create ") && !slices.Contains([]string{"create team/triage-0", "create team/triage-1", "create team/triage-2"}, c) {
			t.Fatalf("a Task outside the replica count: %v", total)
		}
	}
	if gen, _ := cur.p.markerOf(ax.workspaces["team/triage-generation"]); gen != 7 {
		t.Fatalf("the marker records generation %d", gen)
	}
	st := old.pass()
	if !hasGap(st, "superseded: generation 7 of another front runs this pool, and this front holds generation 6") {
		t.Fatalf("gaps = %v", st.Gaps)
	}
	if strings.Count(old.log.String(), "another front runs a newer generation") != 1 {
		t.Fatalf("the superseded replica logged its state more than once:\n%s", old.log.String())
	}
}

// A replica that sees a Task of a newer generation changes nothing in the
// pool: it neither creates a missing Task, nor resumes a pending one, nor
// sends a delete again. It keeps that knowledge when the newer Task is
// gone from its list, and a delete that it judged before stops too.
func TestGenerationSupersededChangesNothing(t *testing.T) {
	ax, check := newFakeAX(), newFakeCheck()
	old := newReplica(t, withGeneration(6, ""), ax, check)
	cur := newReplica(t, withGeneration(7, ""), ax, check)
	cur.worker(0, phaseRunning, true)
	old.worker(1, v1alpha1.PhaseTerminating, true)
	old.worker(2, phasePending, false)
	st := old.pass()
	if got := ax.took(); len(got) != 0 {
		t.Fatalf("calls = %v", got)
	}
	if !hasGap(st, "superseded: generation 7") || st.Generation != 6 {
		t.Fatalf("status = %+v", st)
	}

	// The newer Task leaves the list, and the pool misses every Task.
	delete(ax.tasks, "team/triage-0")
	delete(ax.tasks, "team/triage-1")
	delete(ax.tasks, "team/triage-2")
	old.pass()
	if got := ax.took(); len(got) != 0 {
		t.Fatalf("a superseded replica filled the pool: %v", got)
	}
	if err := old.rec.superseded(old.p); err == nil {
		t.Fatal("the replica forgot the newer generation")
	}

	// A new replica of generation 6 learns it from the marker alone.
	cur.pass() // records generation 7 and creates the Tasks
	ax.took()
	fresh := newReplica(t, withGeneration(6, ""), ax, check)
	delete(ax.tasks, "team/triage-2")
	fresh.pass()
	if got := ax.took(); len(got) != 0 {
		t.Fatalf("a replica of an older generation filled a gap: %v", got)
	}
}

// Two configurations under one generation are a conflict. Neither replica
// changes the pool, and each says why, once in its log.
func TestGenerationConflictChangesNothing(t *testing.T) {
	ax, check := newFakeAX(), newFakeCheck()
	a := newReplica(t, withGeneration(7, "a"), ax, check)
	b := newReplica(t, withGeneration(7, "b"), ax, check)
	a.worker(0, phaseRunning, true)
	b.worker(1, phaseRunning, true)
	for range 3 {
		for _, r := range []*replica{a, b} {
			st := r.pass()
			if !hasGap(st, "conflict:") || !hasGap(st, "carries generation 7 with another configuration") {
				t.Fatalf("gaps = %v", st.Gaps)
			}
		}
	}
	// The conflict shows before the marker, so neither replica writes it.
	if got := ax.took(); len(got) != 0 {
		t.Fatalf("calls = %v", got)
	}
	for _, r := range []*replica{a, b} {
		if n := strings.Count(r.log.String(), "two configurations claim one generation"); n != 1 {
			t.Fatalf("the conflict was logged %d times:\n%s", n, r.log.String())
		}
	}
}

// Generation 0 on both sides keeps the order of the hash alone: a Task
// with another hash and no generation runs an older configuration, and
// the pool replaces it, without a marker.
func TestGenerationZeroKeepsTheHashOrder(t *testing.T) {
	r := newRig(t, strings.Replace(fixture, "replicas: 3", "replicas: 1", 1))
	r.worker(0, "oldhash", phaseRunning, true)
	if genOf(r.ax.task("team", "triage-0")) != 0 {
		t.Fatal("a Task of generation 0 carries a generation")
	}
	st := r.pass()
	if got := r.ax.took(); !slices.Equal(got, []string{"delete team/triage-0"}) {
		t.Fatalf("calls = %v", got)
	}
	if !hasGap(st, "triage-0 runs an older configuration; the pool deleted it") {
		t.Fatalf("gaps = %v", st.Gaps)
	}
	if _, ok := r.ax.workspaces["team/triage-generation"]; ok {
		t.Fatal("a pool of generation 0 wrote a marker")
	}
}

// A new generation replaces the Tasks one at a time, each once it is idle,
// as a new configuration does.
func TestGenerationBumpRollsOneAtATime(t *testing.T) {
	ax, check := newFakeAX(), newFakeCheck()
	old := newReplica(t, withGeneration(1, ""), ax, check)
	cur := newReplica(t, withGeneration(2, ""), ax, check)
	for i := range 3 {
		old.worker(i, phaseRunning, true)
	}
	check.active["team/triage-1"] = []string{"01RUN"}
	check.busy["team/triage-1"] = 1

	st := cur.pass()
	if got := ax.took(); !slices.Equal(got, []string{"workspace team/triage-generation", "delete team/triage-2"}) {
		t.Fatalf("pass 1 calls = %v", got)
	}
	if !hasGap(st, "triage-2 runs an older configuration of generation 1; the pool deleted it") || !hasGap(st, "the pool replaces one Task at a time") {
		t.Fatalf("pass 1 gaps = %v", st.Gaps)
	}
	cur.pass()
	if got := ax.took(); !slices.Equal(got, []string{"workspace team/triage-2-ws", "create team/triage-2", "resume team/triage-2"}) {
		t.Fatalf("pass 2 calls = %v", got)
	}
	if task := ax.task("team", "triage-2"); envOf(task, EnvGeneration) != "2" {
		t.Fatalf("the new Task carries %s=%q", EnvGeneration, envOf(task, EnvGeneration))
	}
	cur.pass()
	if got := ax.took(); !slices.Equal(got, []string{"delete team/triage-0"}) {
		t.Fatalf("pass 3 calls = %v", got)
	}
	cur.pass()
	ax.took()
	st = cur.pass()
	if got := ax.took(); len(got) != 0 || !hasGap(st, "holds the active run 01RUN") {
		t.Fatalf("calls %v, gaps %v", got, st.Gaps)
	}
}

// A replica judged a Task of an older generation, and a front of a newer
// generation replaced the Task before the delete. The read before the
// delete sees the newer generation, so the Task stays, and the replica
// changes nothing more.
func TestGenerationDeleteRereadLeavesANewerTask(t *testing.T) {
	ax, check := newFakeAX(), newFakeCheck()
	older := newReplica(t, strings.Replace(withGeneration(5, ""), "replicas: 3", "replicas: 1", 1), ax, check)
	mid := newReplica(t, strings.Replace(withGeneration(6, ""), "replicas: 3", "replicas: 1", 1), ax, check)
	newer := newReplica(t, strings.Replace(withGeneration(7, ""), "replicas: 3", "replicas: 1", 1), ax, check)
	older.worker(0, phaseRunning, true)
	mid.writeMarkerFor(t, 6)
	stale := ax.list("team")
	delete(ax.tasks, "team/triage-0")
	newer.worker(0, phaseRunning, true)
	before := ax.task("team", "triage-0")

	st := mid.rec.Reconcile(context.Background(), mid.p, stale, time.Now())
	if got := ax.took(); slices.Contains(got, "delete team/triage-0") {
		t.Fatalf("calls = %v", got)
	}
	if after := ax.task("team", "triage-0"); after == nil || !sameTime(after.Metadata.CreateTime, before.Metadata.CreateTime) {
		t.Fatal("the newer Task is gone")
	}
	if !hasGap(st, "carries generation 7 now, newer than generation 6 of this front") {
		t.Fatalf("gaps = %v", st.Gaps)
	}
	mid.rec.Reconcile(context.Background(), mid.p, stale, time.Now())
	if got := ax.took(); len(got) != 0 {
		t.Fatalf("calls after the reread = %v", got)
	}
}

// writeMarkerFor stores the marker of the pool with a generation, as a
// front of that generation records it.
func (r *replica) writeMarkerFor(t *testing.T, gen int) {
	t.Helper()
	p := *r.p
	p.Generation = gen
	r.ax.workspaces["team/"+p.MarkerName()] = p.Marker()
}

// A marker that the pool did not write keeps a pool of a generation above
// 0 from any change, and a pool of generation 0 ignores it.
func TestGenerationForeignMarker(t *testing.T) {
	ax, check := newFakeAX(), newFakeCheck()
	ax.workspaces["team/triage-generation"] = &v1alpha1.Workspace{Metadata: &v1alpha1.ObjectMeta{Name: "triage-generation", Atespace: "team"},
		Spec: &v1alpha1.WorkspaceSpec{Files: []*v1alpha1.File{{Path: markerFile, Content: "9\n"}}}}
	r := newReplica(t, withGeneration(3, ""), ax, check)
	st := r.pass()
	if got := ax.took(); len(got) != 0 || !hasGap(st, "the Workspace triage-generation exists and belongs to no pool") {
		t.Fatalf("calls %v, gaps %v", got, st.Gaps)
	}
	zero := newReplica(t, fixture, ax, check)
	zero.pass()
	if got := ax.took(); len(got) != 9 {
		t.Fatalf("a pool of generation 0 did not create its Tasks: %v", got)
	}
}

// A Task without FW_POOL_GENERATION, or with a value that is not a number
// of 0 or more, has generation 0. A pool of generation 0 stamps none.
func TestGenerationOfATask(t *testing.T) {
	task := func(v string) *v1alpha1.Task {
		return &v1alpha1.Task{Spec: &v1alpha1.TaskSpec{Env: []*v1alpha1.EnvVar{{Name: EnvGeneration, Value: v}}}}
	}
	for v, want := range map[string]int{"4": 4, "0": 0, "": 0, "abc": 0, "-3": 0, " 5": 0} {
		if got := genOf(task(v)); got != want {
			t.Errorf("genOf(%q) = %d, want %d", v, got, want)
		}
	}
	if got := genOf(&v1alpha1.Task{Spec: &v1alpha1.TaskSpec{}}); got != 0 {
		t.Errorf("genOf of a Task without the variable = %d", got)
	}
	p := loadFixture(t, fixture)[0]
	plain, _, err := p.Render(0, testLookup)
	if err != nil {
		t.Fatal(err)
	}
	if envOf(plain, EnvGeneration) != "" || slices.ContainsFunc(plain.Spec.Env, func(e *v1alpha1.EnvVar) bool { return e.Name == EnvGeneration }) {
		t.Fatal("a pool of generation 0 stamped a generation")
	}
	q := loadFixture(t, withGeneration(4, ""))[0]
	stamped, _, err := q.Render(0, testLookup)
	if err != nil {
		t.Fatal(err)
	}
	if genOf(stamped) != 4 {
		t.Fatalf("a pool of generation 4 stamped %q", envOf(stamped, EnvGeneration))
	}
	var out strings.Builder
	if err := Write(&out, []*Pool{q}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "records generation 4 in the Workspace triage-generation") || !strings.Contains(out.String(), "name: triage-generation") {
		t.Fatalf("the dry run misses the marker:\n%s", out.String())
	}
}
