package pools

// This file makes the Tasks of a pool match its configuration
// (docs/ARCHITECTURE.md section 13). At every interval the fleet lists
// the Tasks of the atespaces and calls Reconcile once per pool. A pass
// sends a delete again for a Task that ax did not finish deleting,
// deletes the idle Tasks above the replica count, resumes a Task of the
// pool that did not start, creates the missing Tasks, and replaces the
// Tasks that run an older configuration: at once a Task that serves
// nobody, and one Task that serves at a time, when every Task is up.
//
// A Task belongs to the pool when its name is <pool>-<index> and its
// variable FW_POOL names the pool. The front never touches another Task.
// No replica of the front leads: every action is safe to repeat. A name
// comes from the pool and the index, so two creates of one Task make one
// Task. A delete reads the Task again first and deletes it only when it is
// the Task that the pass judged, after the Task said that it holds no
// active run. The generation of a pool orders its configurations: a front
// that sees a Task of a higher generation than its own changes nothing in
// the pool, so two replicas with two configurations do not undo each
// other.

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	v1alpha1 "github.com/google/ax/pkg/apis/v1alpha1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Phases of an ax Task.
const (
	phaseRunning   = "Running"
	phaseSuspended = "Suspended"
	phasePending   = "Pending"
	phaseFailed    = "Failed"
)

// Timeouts of the calls of a pass. A delete waits for Substrate to remove
// the actor, so it gets longer.
const (
	callTimeout   = 30 * time.Second
	deleteTimeout = 2 * time.Minute
)

// FailedRetry is how long a failed Task of the current configuration that
// never ran stays before the pool creates it again. ax sets the phase
// Failed for an error that does not go away by itself, so the pool tries
// again seldom, and a person can read the reason in between.
const FailedRetry = 5 * time.Minute

// conditionWorkspaceReady is the condition that ax sets on a Task the first
// time it resumes the Task. A Task without it never ran through ax.
const conditionWorkspaceReady = "WorkspaceReady"

// Checker tells the reconciler whether a Task holds work. The fleet
// implements it with its view and the router. A member is
// "<atespace>/<task>".
type Checker interface {
	// Active returns the runs of the member that the view holds and that
	// are not finished, sorted.
	Active(member string) []string
	// Busy reads the status of the member through the router and returns
	// the number of its runs that are not finished. It also reads each run
	// of runs, the runs that the view holds, so a run that the token cannot
	// see is an error and a run that the Task ended is no work. It sends
	// token, the static token that the Task holds, or the token of the
	// front when token is empty. A read of a sleeping Task wakes it.
	Busy(ctx context.Context, member, token string, runs []string) (int, error)
}

// Reconciler makes the Tasks of the pools match their configuration. AX is
// the ax API, Check tells whether a Task works, and Values holds the
// values of the variables of the front.
type Reconciler struct {
	AX     v1alpha1.AXClient
	Check  Checker
	Values *Values
	Log    *slog.Logger

	mu    sync.Mutex
	newer map[string]int    // the highest generation seen on a Task or a marker, by atespace/pool
	held  map[string]string // why each held pool changes nothing, as last logged
}

// Status is the state of a pool after a pass. Wanted is the replica
// count. Actual counts the Tasks of the pool that exist, are not being
// deleted, and did not fail after a run. Ready counts those of them that
// run the current configuration and are running or asleep after a first
// run. Gaps says in words why the pool is not as the configuration wants
// it, one reason each.
type Status struct {
	Name       string       `json:"name"`
	Atespace   string       `json:"atespace"`
	Agent      string       `json:"agent"`
	Intake     string       `json:"intake"`
	Wanted     int          `json:"wanted"`
	Actual     int          `json:"actual"`
	Ready      int          `json:"ready"`
	Hash       string       `json:"hash"`
	Generation int          `json:"generation,omitempty"`
	Tasks      []TaskStatus `json:"tasks"`
	Gaps       []string     `json:"gaps,omitempty"`
	CheckedAt  time.Time    `json:"checkedAt"`
}

// TaskStatus is one Task of a pool. Current is true when it runs the
// current configuration. Note says what the pass did with it, or why it
// waits.
type TaskStatus struct {
	Name    string `json:"name"`
	Phase   string `json:"phase"`
	Current bool   `json:"current"`
	Note    string `json:"note,omitempty"`
}

// Initial returns the state of a pool before its first pass.
func Initial(p *Pool, why string) Status {
	return Status{Name: p.Name, Atespace: p.Atespace, Agent: p.Agent, Intake: p.Intake, Wanted: p.replicas(), Hash: p.hash,
		Generation: p.Generation, Tasks: []TaskStatus{}, Gaps: []string{why}}
}

// member is one Task of the pool in a pass.
type member struct {
	name     string
	atespace string
	index    int
	phase    string
	hash     string
	gen      int // FW_POOL_GENERATION of the Task, 0 without one
	current  bool
	older    string // what is old when the Task is not current
	started  bool   // ax resumed the Task once: it has the condition WorkspaceReady
	ready    bool   // the condition WorkspaceReady is True: the kernel answered at a resume
	failedAt time.Time
	created  *timestamppb.Timestamp
	token    string   // the static token that the Task holds; never logged
	message  string   // the message of the condition Ready, for a Task that fails or waits
	seen     []string // the active runs that the view knew when the pass judged the Task
	note     string
	gone     bool // deleted in this pass
	resumed  bool // resumed in this pass, so it still starts
}

// up reports whether the Task can take work: it runs, or it sleeps after a
// first run, and the router wakes it on a request.
func (m *member) up() bool {
	return m.phase == phaseRunning || (m.phase == phaseSuspended && m.started)
}

// broken reports whether the Task failed after a first run. It can hold
// runs that nobody can read, so the pool never deletes it, and it does not
// count toward the replicas.
func (m *member) broken() bool { return m.phase == phaseFailed && m.started }

// id returns the member name of a Task in the view of the front.
func (m *member) id() string { return m.atespace + "/" + m.name }

// indexOf returns the index of a Task name of the pool, <pool>-<index>.
func (p *Pool) indexOf(name string) (int, bool) {
	rest, ok := strings.CutPrefix(name, p.Name+"-")
	if !ok || rest == "" || (len(rest) > 1 && rest[0] == '0') {
		return 0, false
	}
	for _, c := range rest {
		if c < '0' || c > '9' {
			return 0, false
		}
	}
	n, err := strconv.Atoi(rest)
	return n, err == nil
}

// genOf returns the generation of a Task. A Task without FW_POOL_GENERATION,
// or with a value that is not a number of 0 or more, has generation 0.
func genOf(t *v1alpha1.Task) int {
	n, err := strconv.Atoi(envOf(t, EnvGeneration))
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// envOf returns the value of a variable of a Task.
func envOf(t *v1alpha1.Task, name string) string {
	for _, e := range t.GetSpec().GetEnv() {
		if e.GetName() == name {
			return e.GetValue()
		}
	}
	return ""
}

// Reconcile runs one pass for a pool over the Tasks of its atespace, as the
// ax API listed them, and returns the state of the pool.
func (r *Reconciler) Reconcile(ctx context.Context, p *Pool, tasks []*v1alpha1.Task, now time.Time) Status {
	st := Status{Name: p.Name, Atespace: p.Atespace, Agent: p.Agent, Intake: p.Intake, Wanted: p.replicas(), Hash: p.hash,
		Generation: p.Generation, Tasks: []TaskStatus{}, CheckedAt: now}
	gap := func(format string, args ...any) { st.Gaps = append(st.Gaps, fmt.Sprintf(format, args...)) }

	var owned []*member
	taken := map[int]bool{}
	for _, t := range tasks {
		name := t.GetMetadata().GetName()
		idx, ok := p.indexOf(name)
		if !ok {
			continue
		}
		taken[idx] = true
		if envOf(t, EnvPool) != p.Name {
			gap("%s has the name of a Task of the pool but no %s=%s, so it belongs to no pool; the pool leaves it alone and uses another name", name, EnvPool, p.Name)
			continue
		}
		owned = append(owned, r.memberOf(t, idx, p))
	}
	sort.Slice(owned, func(i, j int) bool { return owned[i].index < owned[j].index })

	// A Task of a newer generation, or of the same generation with another
	// configuration, stops the pass before any change.
	if why := r.hold(ctx, p, owned); why != "" {
		gap("%s", why)
		return r.summary(st, p, owned, nil, nil)
	}

	// A delete that ax did not finish, for example because Substrate
	// refused it while the actor resumed, is sent again: the Task was
	// judged once already. A Task whose actor took a run in the meantime
	// stays.
	var counted []*member
	for _, m := range owned {
		switch {
		case m.phase == v1alpha1.PhaseTerminating:
			m.note = "being deleted"
			// The actor of such a Task can still run and take a job. When the
			// view holds a run of it, the Task decides; a Task that does not
			// answer has no actor left, and its runs went with it.
			m.seen = r.Check.Active(m.id())
			if len(m.seen) > 0 {
				if n, err := r.Check.Busy(ctx, m.id(), m.token, m.seen); err == nil && n > 0 {
					gap("%s is being deleted but holds the active run %s, so the pool waits until the run ends", m.name, m.seen[0])
					continue
				}
			}
			if err := r.deleteTask(ctx, p, m, false); err != nil {
				gap("%s is being deleted, and the delete did not go through again: %v", m.name, err)
			}
		case m.broken():
			m.note = "failed after it ran"
			alive := ""
			if runs := r.Check.Active(m.id()); len(runs) > 0 {
				alive = fmt.Sprintf(" Its actor can still run: the view holds the run %s.", runs[0])
			}
			gap("%s failed after it ran (%s), so the pool cannot ask it about its runs. It does not count toward the replicas.%s Delete it by hand (ax delete task %s) once its runs do not matter", m.name, orWord(m.message, "ax gives no reason"), alive, m.name)
		default:
			counted = append(counted, m)
		}
	}

	// The Tasks above the replica count go, the idle ones first; a Task
	// that holds a run stays until it is idle.
	deleted := 0
	if excess := len(counted) - p.replicas(); excess > 0 {
		var kept []string // why each Task that the pass tried stays
		var waiting []*member
		for _, m := range r.order(counted) {
			if excess == 0 {
				break
			}
			ok, why := r.idle(ctx, m)
			if !ok {
				kept = append(kept, m.name+" "+why)
				m.note, waiting = "waits to go: it "+why, append(waiting, m)
				continue
			}
			if err := r.deleteTask(ctx, p, m, true); err != nil {
				kept = append(kept, fmt.Sprintf("the pool could not delete %s (%v)", m.name, err))
				continue
			}
			m.gone, m.note = true, "deleted: above the replica count"
			deleted++
			excess--
			r.Log.Info("pools: deleted a Task above the replica count", "pool", p.Name, "task", m.id(), "replicas", p.replicas())
		}
		if excess > 0 {
			gap("%d %s above the replica count, and the pool deletes only an idle Task: %s", excess, plural(excess, "Task is", "Tasks are"), strings.Join(kept, "; "))
		} else {
			for _, m := range waiting {
				m.note = "" // another Task went in its place
			}
		}
	}

	// A Task of the current configuration that never ran is resumed: its
	// resume failed, or the front stopped between the create and the
	// resume. A pending Task is resumed too: ax leaves a Task pending after
	// a create or a resume that Substrate refused for a moment, and nothing
	// else tries again. A Task of an older configuration that never ran is
	// replaced below instead, without a start.
	for _, m := range counted {
		stalled := m.phase == phasePending || (m.phase == phaseSuspended && !m.started)
		if m.gone || !stalled || (!m.started && !m.current) {
			continue
		}
		why := r.Values.Redact(orWord(m.message, "ax gives no reason"))
		if err := r.resume(ctx, p, m.name); err != nil {
			m.note = "waits to start"
			gap("%s does not start (%s), and its resume failed: %v", m.name, why, err)
			continue
		}
		m.phase, m.started, m.resumed, m.note = phaseRunning, true, true, "resumed"
		r.Log.Info("pools: resumed a Task that did not run", "pool", p.Name, "task", m.id(), "reason", why)
	}

	// The missing Tasks are created at the lowest free names.
	// A Task that another replica of the front made since the list counts
	// as made, so the pool makes no Task too many.
	remaining := len(counted) - deleted
	var created []string
	existed := map[string]bool{}
	for i := 0; remaining+len(created) < p.replicas() && i <= p.replicas()+len(tasks); i++ {
		if taken[i] {
			continue
		}
		taken[i] = true
		name := p.TaskName(i)
		exists, err := r.create(ctx, p, i)
		if errors.Is(err, errForeign) {
			gap("%v; the pool uses another name", err)
			continue
		}
		if err != nil {
			gap("the pool could not create %s: %v", name, err)
			break
		}
		created = append(created, name)
		if exists {
			existed[name] = true
			continue
		}
		r.Log.Info("pools: created a Task", "pool", p.Name, "task", p.Atespace+"/"+name, "hash", p.hash)
	}

	r.replace(ctx, p, counted, len(created) == 0 && deleted == 0, now, gap)
	return r.summary(st, p, owned, created, existed)
}

// summary adds the Tasks of the pool and their counts to the state of a
// pass, and redacts the values of the front from it.
func (r *Reconciler) summary(st Status, p *Pool, owned []*member, created []string, existed map[string]bool) Status {
	gap := func(format string, args ...any) { st.Gaps = append(st.Gaps, fmt.Sprintf(format, args...)) }
	for _, m := range owned {
		st.Tasks = append(st.Tasks, TaskStatus{Name: m.name, Phase: m.phase, Current: m.current, Note: m.note})
		if m.gone || m.phase == v1alpha1.PhaseTerminating || m.broken() {
			continue
		}
		st.Actual++
		if m.current && m.up() && !m.resumed {
			st.Ready++
		}
	}
	for _, name := range created {
		note := "created"
		if existed[name] {
			note = "made by another replica of the front"
		}
		st.Tasks = append(st.Tasks, TaskStatus{Name: name, Phase: phaseRunning, Current: true, Note: note})
		st.Actual++
	}
	if len(created) > 0 {
		gap("the pool created %s; %s", strings.Join(created, ", "), plural(len(created), "it starts", "they start"))
	}
	sort.Slice(st.Tasks, func(i, j int) bool {
		a, _ := p.indexOf(st.Tasks[i].Name)
		b, _ := p.indexOf(st.Tasks[j].Name)
		return a < b
	})
	for i := range st.Gaps {
		st.Gaps[i] = r.Values.Redact(st.Gaps[i])
	}
	for i := range st.Tasks {
		st.Tasks[i].Note = r.Values.Redact(st.Tasks[i].Note)
	}
	return st
}

// replace deletes the Tasks that run an older configuration, and the next
// pass creates them again. A Task that never ran and that the view knows
// no run of serves nobody, so it goes at once, and so does a failed Task
// of the current configuration that never ran, after FailedRetry. Of the
// Tasks that serve, one goes in each pass, and only when settled is true
// and every Task is up, so the pool keeps its workers while it rolls.
func (r *Reconciler) replace(ctx context.Context, p *Pool, counted []*member, settled bool, now time.Time, gap func(string, ...any)) {
	var cands []*member
	up := 0
	for _, m := range counted {
		if m.gone {
			continue
		}
		if m.up() && !m.resumed {
			up++
		}
		if !m.current {
			cands = append(cands, m)
			continue
		}
		if m.phase == phaseFailed && !m.started {
			if at := m.failedAt.Add(FailedRetry); now.Before(at) {
				gap("%s failed before it ran: %s. The pool creates it again after %s, or fix the pool", m.name, orWord(m.message, "ax gives no reason"), at.UTC().Format(time.RFC3339))
				continue
			}
			cands = append(cands, m)
		}
	}
	// While Tasks above the count remain, they go first, and their gaps
	// tell why; no Task is replaced then.
	if len(cands) == 0 || len(counted)-countGone(counted) > p.replicas() {
		return
	}
	allUp := settled && up >= p.replicas()
	replacedUp, held := false, 0
	for _, m := range r.order(cands) {
		what := "runs " + m.older
		if m.current {
			what = "failed before it ran"
		}
		switch {
		case m.up() && replacedUp:
			m.note = what
			gap("%s %s; the pool replaces one Task at a time", m.name, what)
			continue
		case m.up() && !allUp:
			m.note = what
			held++
			continue
		}
		ok, why := r.idle(ctx, m)
		if !ok {
			m.note = what + " and waits: it " + why
			gap("%s %s and %s, so the pool replaces it once it is idle", m.name, what, why)
			continue
		}
		if err := r.deleteTask(ctx, p, m, false); err != nil {
			gap("the pool could not replace %s: %v", m.name, err)
			continue
		}
		replacedUp = replacedUp || m.up()
		m.gone, m.note = true, "deleted to be created again with the current configuration"
		gap("%s %s; the pool deleted it and creates it again", m.name, what)
		r.Log.Info("pools: deleted a Task to replace it", "pool", p.Name, "task", m.id(), "reason", what, "from", m.hash, "to", p.hash)
	}
	if held > 0 {
		gap("%d %s not current; the pool replaces %s once every Task is up", held, plural(held, "Task is", "Tasks are"), plural(held, "it", "them one at a time"))
	}
}

func countGone(ms []*member) int {
	n := 0
	for _, m := range ms {
		if m.gone {
			n++
		}
	}
	return n
}

// memberOf reads one Task of the pool. A Task is current when it carries
// the hash of the pool and the values of the variables of the front that
// the front holds now, so a new value of the front, such as a new static
// token, replaces the Task too. The values are compared in constant time
// and never logged. A Task of a higher generation, or of the same
// generation with another hash, is not older: hold stops the pass first.
func (r *Reconciler) memberOf(t *v1alpha1.Task, idx int, p *Pool) *member {
	m := &member{
		name:     t.GetMetadata().GetName(),
		atespace: p.Atespace,
		index:    idx,
		phase:    t.GetStatus().GetPhase(),
		hash:     envOf(t, EnvSpec),
		gen:      genOf(t),
		created:  t.GetMetadata().GetCreateTime(),
		token:    envOf(t, p.tokenEnv),
	}
	switch {
	case m.gen < p.Generation:
		m.older = fmt.Sprintf("an older configuration of generation %d", m.gen)
	case m.hash != p.hash:
		m.older = "an older configuration"
	case !r.sameValues(t, p):
		m.older = "an older value of a variable of the front"
	default:
		m.current = true
	}
	m.failedAt = m.created.AsTime()
	for _, c := range t.GetStatus().GetConditions() {
		switch c.GetType() {
		case conditionWorkspaceReady:
			m.started, m.ready = true, strings.EqualFold(c.GetStatus(), "True")
		case "Ready":
			m.message = strings.TrimSpace(c.GetMessage())
			if c.GetUpdateTime() != nil {
				// ax sets the condition Ready when the Task fails, so the
				// retry counts from the failure.
				m.failedAt = c.GetUpdateTime().AsTime()
			}
		}
	}
	return m
}

// hold returns why a pass must change nothing in the pool, or "". The
// generation orders the configurations of a pool. A front is superseded
// when the marker of the pool or a Task of the pool carries a higher
// generation than the pool of the front: another front runs a newer
// configuration, and this one leaves the pool to it. A Task of the same
// generation with another hash is a conflict: two configurations claim one
// generation, and no front can tell which one is newer. A generation of 0
// on both sides orders nothing, as before pools had a generation.
//
// A front of a generation above 0 records it in the marker before its
// first change. A newer front deletes a Task before it creates the next
// one, so an older front learns of the newer one from the marker before
// that gap opens, and does not fill it. A front also keeps the highest
// generation that it saw until it stops. Without the marker, a pool of a
// generation above 0 changes nothing. The log names each new reason once.
func (r *Reconciler) hold(ctx context.Context, p *Pool, owned []*member) string {
	top := 0
	for _, m := range owned {
		top = max(top, m.gen)
	}
	mark, foreign, err := r.readMarker(ctx, p)
	key := p.Atespace + "/" + p.Name
	top = r.saw(key, max(top, mark))
	why, conflict := "", (*member)(nil)
	switch {
	case top > p.Generation:
		why = fmt.Sprintf("superseded: generation %d of another front runs this pool, and this front holds generation %d, so it changes nothing in the pool", top, p.Generation)
	case p.Generation == 0:
	case err != nil:
		why = fmt.Sprintf("the front could not read the generation of the pool (%v), so it changes nothing in the pool", err)
	case foreign:
		why = fmt.Sprintf("the Workspace %s exists and belongs to no pool, so the front cannot record the generation of the pool and changes nothing in it; delete that Workspace or rename the pool", p.MarkerName())
	default:
		for _, m := range owned {
			if m.gen == p.Generation && m.hash != p.hash {
				conflict = m
				why = fmt.Sprintf("conflict: %s carries generation %d with another configuration (%s, and this front has %s), so the pool changes nothing; give each configuration of the pool its own generation", m.name, m.gen, orWord(m.hash, "no hash"), p.hash)
				break
			}
		}
		if why == "" && mark < p.Generation {
			if err := r.writeMarker(ctx, p); err != nil {
				why = fmt.Sprintf("the front could not record generation %d of the pool (%v), so it changes nothing in the pool", p.Generation, err)
			} else {
				r.Log.Info("pools: recorded the generation of the pool", "pool", p.Name, "workspace", p.Atespace+"/"+p.MarkerName(), "generation", p.Generation, "from", mark)
			}
		}
	}
	r.mu.Lock()
	if r.held == nil {
		r.held = map[string]string{}
	}
	prev := r.held[key]
	if why == "" {
		delete(r.held, key)
	} else {
		r.held[key] = why
	}
	r.mu.Unlock()
	switch {
	case why == prev:
	case conflict != nil:
		r.Log.Warn("pools: two configurations claim one generation of the pool; the pool changes nothing", "pool", p.Name, "generation", p.Generation, "task", conflict.id(), "from", conflict.hash, "to", p.hash)
	case top > p.Generation:
		r.Log.Info("pools: another front runs a newer generation of the pool; this front changes nothing in it", "pool", p.Name, "generation", p.Generation, "newer", top)
	case why != "":
		r.Log.Warn("pools: the pool changes nothing", "pool", p.Name, "generation", p.Generation, "reason", r.Values.Redact(why))
	default:
		r.Log.Info("pools: the pool no longer waits", "pool", p.Name, "generation", p.Generation)
	}
	return why
}

// readMarker reads the generation that the marker of the pool records. A
// pool without a marker has generation 0, and foreign is true for a
// Workspace of that name that the pool did not write.
func (r *Reconciler) readMarker(ctx context.Context, p *Pool) (gen int, foreign bool, err error) {
	cctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	ws, err := r.AX.GetWorkspace(cctx, &v1alpha1.GetWorkspaceRequest{Atespace: p.Atespace, Name: p.MarkerName()})
	switch {
	case status.Code(err) == codes.NotFound:
		return 0, false, nil
	case err != nil:
		return 0, false, fmt.Errorf("pools: read the Workspace %s: %w", p.MarkerName(), err)
	}
	gen, ok := p.markerOf(ws)
	return gen, !ok, nil
}

// writeMarker records the generation of the pool in its marker.
func (r *Reconciler) writeMarker(ctx context.Context, p *Pool) error {
	cctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	if _, err := r.AX.UpdateWorkspace(cctx, &v1alpha1.UpdateWorkspaceRequest{Workspace: p.Marker()}); err != nil {
		return fmt.Errorf("pools: write the Workspace %s: %w", p.MarkerName(), err)
	}
	return nil
}

// saw records the generation of a Task of a pool and returns the highest
// generation that the front saw on a Task of it.
func (r *Reconciler) saw(key string, gen int) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.newer == nil {
		r.newer = map[string]int{}
	}
	if gen > r.newer[key] {
		r.newer[key] = gen
	}
	return r.newer[key]
}

// superseded returns an error when the front saw a Task of the pool with a
// higher generation than its own. Each call that changes the pool checks
// it first, so a newer generation that a read learned in a pass stops the
// rest of that pass too.
func (r *Reconciler) superseded(p *Pool) error {
	if top := r.saw(p.Atespace+"/"+p.Name, 0); top > p.Generation {
		return fmt.Errorf("pools: generation %d of another front runs the pool %s, and this front holds generation %d, so it changes nothing", top, p.Name, p.Generation)
	}
	return nil
}

// sameValues reports whether a Task holds the value of every variable that
// the pool copies from the front.
func (r *Reconciler) sameValues(t *v1alpha1.Task, p *Pool) bool {
	if r.Values == nil {
		return true
	}
	for _, e := range p.env {
		if e.FromEnv == "" {
			continue
		}
		want, ok := r.Values.Lookup(e.FromEnv)
		if ok && subtle.ConstantTimeCompare([]byte(envOf(t, e.Name)), []byte(want)) != 1 {
			return false
		}
	}
	return true
}

// order sorts Tasks for a delete, the best candidate first: a Task that
// never ran, then one of which the view knows no active run, then one
// with an older configuration, then a running one, whose status read
// wakes nobody, then the highest index.
func (r *Reconciler) order(ms []*member) []*member {
	out := make([]*member, 0, len(ms))
	known := map[*member]bool{}
	for _, m := range ms {
		if m.gone {
			continue
		}
		out = append(out, m)
		known[m] = len(r.Check.Active(m.id())) > 0
	}
	rank := func(m *member) [4]bool {
		return [4]bool{m.started, known[m], m.current, m.phase != phaseRunning}
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := rank(out[i]), rank(out[j])
		for k := range a {
			if a[k] != b[k] {
				return !a[k]
			}
		}
		return out[i].index > out[j].index
	})
	return out
}

// idle reports whether a Task holds no run that is not finished, or why
// not. A Task that never ran, and of which the view knows no active run,
// holds none. Any other Task must say so itself: its status counts the
// runs of its ledger, which the view can miss, and it answers for each
// run that the view holds, which can be a run that ended after a lost
// event. A Task that neither runs nor sleeps cannot answer. A running Task
// whose kernel never answered at a resume, and that does not answer now,
// counts as a Task that never ran, when the view knows no run of it. idle
// records the runs that the view knew, so the delete can see a run that
// starts after the status read.
func (r *Reconciler) idle(ctx context.Context, m *member) (bool, string) {
	runs := r.Check.Active(m.id())
	m.seen = runs
	if !m.started && len(runs) == 0 {
		return true, ""
	}
	switch m.phase {
	case phaseRunning:
	case phaseSuspended:
		r.Log.Info("pools: read the status of a sleeping Task before it goes; the read wakes it", "task", m.id())
	default:
		if len(runs) > 0 {
			return false, "holds the active run " + runs[0]
		}
		return false, fmt.Sprintf("is %s, so the pool cannot ask it about its runs", strings.ToLower(orWord(m.phase, "in no phase")))
	}
	n, err := r.Check.Busy(ctx, m.id(), m.token, runs)
	switch {
	case err != nil && m.phase == phaseRunning && !m.ready && len(runs) == 0:
		r.Log.Info("pools: the kernel of a Task never answered; the pool takes it for a Task that never ran", "task", m.id(), "error", r.Values.Redact(err.Error()))
		return true, ""
	case err != nil:
		return false, "did not say whether it works (" + err.Error() + ")"
	case n > 0 && len(runs) > 0:
		return false, fmt.Sprintf("holds the active run %s", runs[0])
	case n > 0:
		return false, fmt.Sprintf("holds %d active %s", n, plural(n, "run", "runs"))
	}
	if len(runs) > 0 {
		r.Log.Info("pools: the view holds runs that the Task ended; the Task decides", "task", m.id(), "runs", strings.Join(runs, ","))
	}
	return true, ""
}

// errForeign marks a name that the pool cannot use, because a Workspace of
// that name belongs to no pool.
var errForeign = errors.New("belongs to no pool")

// create makes the Task of the pool at index: its Workspace, the Task, and
// the resume, because ax creates every Task suspended. A Task of that name
// that exists already is not an error: another replica of the front made
// it, its Workspace stays as that replica wrote it, and exists is true. A
// Workspace of that name that the pool did not render stays as it is, and
// the pool uses another name.
func (r *Reconciler) create(ctx context.Context, p *Pool, index int) (exists bool, err error) {
	if err := r.superseded(p); err != nil {
		return false, err
	}
	task, ws, err := p.Render(index, r.Values.Lookup)
	if err != nil {
		return false, err
	}
	cctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	name, wsName := task.GetMetadata().GetName(), ws.GetMetadata().GetName()
	if cur, err := r.AX.GetTask(cctx, &v1alpha1.GetTaskRequest{Atespace: p.Atespace, Name: name}); err == nil {
		r.saw(p.Atespace+"/"+p.Name, genOf(cur))
		r.Log.Info("pools: the Task exists already; another replica of the front made it", "pool", p.Name, "task", p.Atespace+"/"+name)
		return true, nil
	} else if status.Code(err) != codes.NotFound {
		return false, fmt.Errorf("pools: read the Task %s: %w", name, err)
	}
	// A newer front can record its generation while this pass runs. The
	// marker is read once more, right before the gap is filled. A pool of
	// a generation above 0 fills no gap without it.
	mark, _, merr := r.readMarker(cctx, p)
	if merr != nil && p.Generation > 0 {
		return false, merr
	}
	if merr == nil {
		r.saw(p.Atespace+"/"+p.Name, mark)
		if err := r.superseded(p); err != nil {
			return false, err
		}
	}
	cur, err := r.AX.GetWorkspace(cctx, &v1alpha1.GetWorkspaceRequest{Atespace: p.Atespace, Name: wsName})
	switch {
	case status.Code(err) == codes.NotFound:
	case err != nil:
		return false, fmt.Errorf("pools: read the Workspace %s: %w", wsName, err)
	case !p.ownsWorkspace(cur):
		return false, fmt.Errorf("pools: the Workspace %s exists and %w", wsName, errForeign)
	}
	if _, err := r.AX.UpdateWorkspace(cctx, &v1alpha1.UpdateWorkspaceRequest{Workspace: ws}); err != nil {
		return false, fmt.Errorf("pools: write the Workspace %s: %w", wsName, err)
	}
	if _, err := r.AX.CreateTask(cctx, &v1alpha1.CreateTaskRequest{Task: task}); err != nil {
		if c := status.Code(err); c == codes.AlreadyExists || c == codes.FailedPrecondition {
			r.Log.Info("pools: the Task exists already; another replica of the front made it", "pool", p.Name, "task", p.Atespace+"/"+name)
			return true, nil
		}
		return false, fmt.Errorf("pools: create the Task %s: %w", name, err)
	}
	if err := r.resume(cctx, p, name); err != nil {
		return false, fmt.Errorf("%w; the pool resumes it at the next interval", err)
	}
	return false, nil
}

// resume resumes a Task of the pool, which runs it.
func (r *Reconciler) resume(ctx context.Context, p *Pool, name string) error {
	if err := r.superseded(p); err != nil {
		return err
	}
	cctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	if _, err := r.AX.ResumeTask(cctx, &v1alpha1.ResumeTaskRequest{Atespace: p.Atespace, Name: name}); err != nil {
		return fmt.Errorf("pools: resume the Task %s: %w", name, err)
	}
	return nil
}

// deleteTask deletes a Task of the pool that a pass judged, and with
// workspace its Workspace too. It reads the Task first and deletes it only
// when it is the Task that the pass judged: the same pool, the same hash
// and generation, the same create time, and still in the phase Terminating
// when the pass sends a delete again. So a replica of the front with an
// older list never deletes a Task that another replica made since, and a
// Task that a front of a newer generation made in between stops the pass.
// It asks the view once more, so a run that started after the check keeps
// the Task. A Task that is gone is not an error.
func (r *Reconciler) deleteTask(ctx context.Context, p *Pool, m *member, workspace bool) error {
	if err := r.superseded(p); err != nil {
		return err
	}
	cctx, cancel := context.WithTimeout(ctx, deleteTimeout)
	defer cancel()
	cur, err := r.AX.GetTask(cctx, &v1alpha1.GetTaskRequest{Atespace: p.Atespace, Name: m.name})
	if status.Code(err) == codes.NotFound {
		return nil
	}
	if err != nil {
		return fmt.Errorf("pools: read the Task %s: %w", m.name, err)
	}
	if g := genOf(cur); g > p.Generation {
		r.saw(p.Atespace+"/"+p.Name, g)
		return fmt.Errorf("pools: the Task %s carries generation %d now, newer than generation %d of this front, so the pool leaves it alone", m.name, g, p.Generation)
	}
	if envOf(cur, EnvPool) != p.Name || envOf(cur, EnvSpec) != m.hash || genOf(cur) != m.gen || !sameTime(cur.GetMetadata().GetCreateTime(), m.created) ||
		(m.phase == v1alpha1.PhaseTerminating && cur.GetStatus().GetPhase() != v1alpha1.PhaseTerminating) {
		return fmt.Errorf("pools: the Task %s changed since the pass read it; the pool looks at it again at the next interval", m.name)
	}
	for _, run := range r.Check.Active(m.id()) {
		if !slices.Contains(m.seen, run) {
			return fmt.Errorf("pools: the Task %s took the run %s since the check; the pool looks at it again at the next interval", m.name, run)
		}
	}
	if _, err := r.AX.DeleteTask(cctx, &v1alpha1.DeleteTaskRequest{Atespace: p.Atespace, Name: m.name}); err != nil && status.Code(err) != codes.NotFound {
		return fmt.Errorf("pools: delete the Task %s: %w", m.name, err)
	}
	if !workspace {
		return nil
	}
	// Another replica can create a Task of that name right after the
	// delete, with its Workspace first, so the Workspace goes only while
	// no Task of that name exists.
	wsName := p.WorkspaceName(m.index)
	if _, err := r.AX.GetTask(cctx, &v1alpha1.GetTaskRequest{Atespace: p.Atespace, Name: m.name}); status.Code(err) != codes.NotFound {
		return nil
	}
	ws, err := r.AX.GetWorkspace(cctx, &v1alpha1.GetWorkspaceRequest{Atespace: p.Atespace, Name: wsName})
	switch {
	case status.Code(err) == codes.NotFound:
	case err != nil:
		r.Log.Warn("pools: read the Workspace of a deleted Task", "pool", p.Name, "workspace", p.Atespace+"/"+wsName, "error", r.Values.Redact(err.Error()))
	case p.ownsWorkspace(ws):
		if _, err := r.AX.DeleteWorkspace(cctx, &v1alpha1.DeleteWorkspaceRequest{Atespace: p.Atespace, Name: wsName}); err != nil && status.Code(err) != codes.NotFound {
			r.Log.Warn("pools: delete the Workspace of a deleted Task", "pool", p.Name, "workspace", p.Atespace+"/"+wsName, "error", r.Values.Redact(err.Error()))
		}
	}
	return nil
}

// sameTime reports whether two create times are equal, nil included.
func sameTime(a, b *timestamppb.Timestamp) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.AsTime().Equal(b.AsTime())
}

func orWord(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}
