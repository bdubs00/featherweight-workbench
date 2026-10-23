package fleet

// This file reads a fleet from the platforms instead of from records
// (docs/ARCHITECTURE.md section 4.8). The Task list of the ax
// API gives the members, their addresses, and their phases, and the egress
// stream of the workers gives the state of every run. A member that
// publishes no record, a bare kernel in a Task, is visible this way. The
// view reads the runs of a Task each time it becomes live, and again once a
// minute while the view holds one of its runs as queued or running. Between
// those reads it lives on the stream. It never reads a Task that sleeps,
// because a request wakes it.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/pools"
	v1alpha1 "github.com/google/ax/pkg/apis/v1alpha1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"gopkg.in/yaml.v3"
)

// Phases of an ax Task, as the view reads them.
const (
	phaseRunning   = "Running"
	phaseSuspended = "Suspended"
	phasePending   = "Pending"
)

// listPage is how many Tasks one ListTasks call asks for.
const listPage = 500

// fillPauseMax bounds the pause before the front reads the runs of a Task
// again after a read failed (fillPause).
const fillPauseMax = 5 * time.Minute

// fillRepairInterval is how often the front reads the runs of a live Task
// again while the view holds one of its runs as queued or running. A lost
// final event keeps such a run open, so a read repairs it within this time
// and one interval of the Task list.
const fillRepairInterval = time.Minute

// EgressTypes are the ledger event types that a worker publishes for the
// view of the front: enough for the state, the title, the pending
// approval, the turns, the activity, and the reactions of a run. The
// usage of a run comes from the totals that fw.run.finished and
// fw.run.suspended carry, so fw.model.responded, which holds the content
// of the model, stays out. fw.tool.requested and fw.tool.responded carry
// the input and the output of each tool call for the activity of the run
// (watch.go), each string cut to EgressMaxBytes. A worker sets
// bus.egressTypes to this list.
var EgressTypes = []string{
	fw.EvRunQueued, fw.EvRunStarted, fw.EvRunResumed, fw.EvRunSuspended, fw.EvRunFinished,
	fw.EvApprovalRequested, fw.EvApprovalDecided, fw.EvRunAnnotated, fw.EvTurnStarted, fw.EvMessageReceived,
	fw.EvRunChild, fw.EvOutcomeEvaluated,
	fw.EvToolRequested, fw.EvToolResponded, fw.EvTurnFinished, fw.EvRunReacted,
	fw.EvRoutineDecided, fw.EvRoutineApplied, fw.EvRoutineCompared,
}

// EgressMaxBytes is bus.egressMaxBytes of a worker of a pool, unless the
// pool sets it: the egress of the worker cuts each string value in the
// data of a published event to this many bytes, with a note of the cut,
// so the stream carries no whole tool input or output. The ledger of the
// worker stays whole.
const EgressMaxBytes = 4096

// platform is the view of the members that the platforms know.
type platform struct {
	f         *Fleet
	atespaces []string
	egress    string
	server    string
	prices    fw.Prices
	tokenEnv  string // the Secrets name of the static token for the first read of a running Task

	mu       sync.Mutex
	members  map[string]*platformMember // by member name
	runs     map[string]*platformRun    // by run id
	children map[string]childLink       // by the id of the child
	gone     map[string]forgotRun       // by run id: the runs that the view dropped
	conn     *grpc.ClientConn
	client   v1alpha1.AXClient
	// ends counts the ends of runs that the stream carried, for the
	// metrics (metrics.go).
	ends runEnds

	// The pools that the front keeps (pools.go), and the state of each
	// after its last pass, by name, which mu guards.
	pools      []*pools.Pool
	reconciler *pools.Reconciler
	poolState  map[string]pools.Status
	// merged holds each pool with the overlay that its last pass merged
	// (profiles.go), by name, and rt each runtime pool as the last pass
	// read it (runtime.go).
	merged map[string]*pools.Pool
	rt     map[string]*runtimePool
	// shown holds the state of each member that the last member change
	// named, by name, so a change goes out once, and quiet is the time
	// without an event after which a member that only the stream knows is
	// quiet (watch.go).
	shown map[string]string
	quiet time.Duration
	// index is the index of the runs in the bucket of the bus, nil
	// without one, and indexLoaded is true once the view loaded it
	// (index.go). listedOnce holds each atespace whose Task list the view
	// read whole once, with the runs of its live Tasks, so the view knows
	// that a Task outside the list is gone.
	index       *runIndex
	indexLoaded bool
	listedOnce  map[string]bool
}

// childLink is where a child of a run lives, from the fw.run.child event
// of its parent: a child of spawn_task or a branch in a Task of its own
// (the kernel architecture, sections 4.7 and 11.9). The view relates the
// child to its parent through it, and answers for a child whose own events
// have not arrived yet.
type childLink struct {
	parent string    // the run that started the child
	member string    // the member that runs the child
	at     time.Time // when the parent recorded it
}

// platformMember is one member as a platform reports it. State is empty
// for a member that only the stream knows; the view then judges it by the
// time of its last event.
type platformMember struct {
	name     string
	url      string
	atespace string
	state    string
	phase    string // the phase of the Task as the ax API last gave it
	seen     time.Time
	filled   bool      // the runs were read since the Task last became live
	filling  bool      // a read of the runs is in flight
	fillRead time.Time // when the last read of the runs succeeded
	// fillFails counts the reads of the runs of the member that failed in a
	// row, and fillAt is the time of the next read after a failure (fillPause).
	fillFails int
	fillAt    time.Time
	// resuming is true while a ResumeTask call runs for the member
	// (suspend.go), so a burst of requests makes one call.
	resuming bool
	// waking is true while a wake for a job runs for the member, and
	// wokeAt is when the last one started (suspend.go), so the front
	// sends one wake at a time and gives a woken Task time to take a job.
	waking bool
	wokeAt time.Time
	// liveSince is when the phase of the Task last became Running; the
	// suspend controller counts idle time from it when the member has
	// no run (suspend.go).
	liveSince time.Time
	// agents names the profiles that the member serves, from its own
	// list read once and from its runs, so a job for one of them can wake
	// the member (suspend.go). For a Task that sleeps, the configuration
	// file in its Workspace names them (learn), so a front that starts
	// while the Task sleeps still shows it to the people who can use it.
	agents  map[string]bool
	learned bool // the Workspaces of the Task were read once
	// pool is FW_POOL of the Task, the pool whose Workspace and intake it
	// has. A child Task of spawn_task keeps the variables and the
	// Workspace of its parent, so it names the pool of its parent. A wake
	// for a queue reaches a Task of a pool for the intake of its pool alone
	// (suspend.go).
	pool string
	// blocked is the reason from the status of the Task when the suspend
	// controller found it blocked by a storage failure, and empty
	// otherwise. statusAt is when the controller last read the status of
	// the Task while the view held a queued or running run of it, so it
	// reads a busy Task at most once per interval (suspend.go).
	// runsBlocked is true while a run of the member in the view carries
	// fw.Run.Blocked from a read (backfill). The next event of the member
	// clears all three marks (unblock), because a process that a storage
	// failure blocked sends no event.
	blocked     string
	statusAt    time.Time
	runsBlocked bool
}

// platformRun is one run of a member, projected from the stream.
type platformRun struct {
	run     *fw.Run
	member  string
	updated time.Time
	// queued is true once the view applied the fw.run.queued of the run,
	// or read the run whole from its member (backfill). kept is the largest
	// RunQueuedData.Kept that the view applied (onEvent).
	queued bool
	kept   uint64
	// stateSeq is the sequence number of the latest event that changed
	// the state of the run, and added holds the usage that each event
	// after the latest totals added, so a late end of the run can apply
	// (lateEnd).
	stateSeq uint64
	added    []addedUsage
	// callID and input are the call id and the input of the latest
	// fw.tool.requested of the run, cut to ActivityInputLimit, for the
	// activity of the run (watch.go). The view forgets them when the call
	// returns.
	callID string
	input  json.RawMessage
	// decided holds the seq of each fw.approval.decided of the run that the
	// view applied, and streamed is true once the view applied the
	// fw.run.queued of the run from the stream, so it saw every decision
	// after it (Decisions).
	decided  []uint64
	streamed bool
	// byRoutine is true once a routine completed the run, and sentBack once
	// a quality receipt sent it back (tap.go).
	byRoutine bool
	sentBack  bool
	// idx is what the index of the runs holds of the run (index.go).
	// fromIndex is true while the view knows the run from the index alone:
	// no event and no read of its member confirmed it since the view
	// loaded the index. partial is true while the run has the facts of the
	// index and no title or input, until a read of its member gives them.
	idx       indexMark
	fromIndex bool
	partial   bool
}

// addedUsage is the usage that one event added to a run.
type addedUsage struct {
	seq uint64
	u   fw.RunUsage
}

// forgotRun is a run that the view dropped (drop). The tool batch tells
// that its counts leave out such a run of the job, and an event of the run
// that arrives later brings it back with its identity, such as the
// fw.run.child of a branch of a finished run. run is a copy without texts
// (forgetful), which the view authorizes as it authorizes the runs, so a
// caller learns only of the runs that the caller could see. at is the time
// of the drop; the view forgets the run for good after the expire time
// since then.
type forgotRun struct {
	run    *fw.Run
	member string
	at     time.Time
}

// startPlatform starts the sources that the configuration names: the Task
// list for the atespaces, and the egress stream. The pools reconcile with
// the Task list, with the values of the variables of the front in vals.
func (f *Fleet) startPlatform(atespaces []string, egress, server, tokenEnv string, list []*pools.Pool, vals *pools.Values) error {
	p := &platform{
		f:          f,
		atespaces:  atespaces,
		egress:     strings.TrimSpace(egress),
		server:     strings.TrimSpace(server),
		prices:     f.a.Prices(),
		tokenEnv:   tokenEnv,
		members:    map[string]*platformMember{},
		runs:       map[string]*platformRun{},
		children:   map[string]childLink{},
		gone:       map[string]forgotRun{},
		ends:       newRunEnds(),
		shown:      map[string]string{},
		quiet:      QuietAfter * f.interval,
		listedOnce: map[string]bool{},
	}
	if f.indexBucket != nil {
		p.index = newRunIndex(f.indexBucket, f.log)
	}
	if len(p.atespaces) > 0 {
		if p.server == "" {
			return errors.New("fleet: workbench.fleet.atespaces needs ax.server (FW_AX_SERVER), the ax API that lists the Tasks")
		}
		conn, err := grpc.NewClient(p.server, grpc.WithTransportCredentials(insecure.NewCredentials()))
		if err != nil {
			return fmt.Errorf("fleet: connect to the ax server %s: %w", p.server, err)
		}
		p.conn, p.client = conn, v1alpha1.NewAXClient(conn)
	}
	if p.egress != "" {
		stop, err := f.bus.Subscribe(f.ctx, p.egress+".>", p.onEvent)
		if err != nil {
			if p.conn != nil {
				_ = p.conn.Close()
			}
			return fmt.Errorf("fleet: subscribe to the egress stream %s: %w", p.egress, err)
		}
		f.viewMu.Lock()
		f.stops = append(f.stops, stop)
		f.viewMu.Unlock()
		go p.watchLoop(f.ctx, f.interval)
	}
	p.startPools(list, vals)
	f.platform = p
	if p.index != nil {
		// The view subscribed first, so a run that the stream brings while
		// the index loads keeps the newer state of the stream.
		go p.index.writeLoop(f.ctx)
		go p.loadIndex(f.ctx)
	}
	if p.client != nil {
		go p.loop(f.ctx)
	}
	f.log.Info("fleet reads the platforms", "atespaces", strings.Join(atespaces, ","), "egress", p.egress, "server", p.server)
	return nil
}

// closePlatform ends the sources.
func (f *Fleet) closePlatform() {
	p := f.platform
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.conn != nil {
		_ = p.conn.Close()
		p.conn, p.client = nil, nil
	}
}

// loop reads the Task list at every interval.
func (p *platform) loop(ctx context.Context) {
	p.poll(ctx)
	t := time.NewTicker(p.f.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.poll(ctx)
		}
	}
}

// poll reads the Tasks of every atespace and updates the members. A Task
// that disappears from the list leaves the view. A Task that becomes live
// gets its runs read, and so does a live Task with a queued or running run
// in the view once per fillRepairInterval. Then the suspend controller and
// the pools act on the list.
func (p *platform) poll(ctx context.Context) {
	p.mu.Lock()
	client := p.client
	p.mu.Unlock()
	if client == nil {
		return
	}
	now := time.Now()
	seen := map[string]bool{}
	var fill []*platformMember
	var learn []*v1alpha1.Task
	listed, failed := map[string][]*v1alpha1.Task{}, map[string]error{}
	for _, atespace := range p.atespaces {
		tasks, err := p.listTasks(ctx, client, atespace)
		listed[atespace] = tasks
		if err != nil {
			failed[atespace] = err
			p.f.log.Warn("fleet: list the Tasks", "atespace", atespace, "error", err)
			// The members of this atespace keep their last state.
			p.mu.Lock()
			for name, m := range p.members {
				if m.atespace == atespace {
					seen[name] = true
				}
			}
			p.mu.Unlock()
			continue
		}
		p.mu.Lock()
		for _, t := range tasks {
			taskName := t.GetMetadata().GetName()
			if taskName == "" {
				continue
			}
			name := atespace + "/" + taskName
			seen[name] = true
			m := p.members[name]
			if m == nil {
				m = &platformMember{name: name, url: "ax:" + name, atespace: atespace}
				p.members[name] = m
			}
			was, wasPhase := m.state, m.phase
			m.phase = t.GetStatus().GetPhase()
			m.pool = taskVar(t, pools.EnvPool)
			m.state, m.seen = stateOfPhase(m.phase), now
			if m.state == MemberLive && was != MemberLive {
				m.liveSince, m.filled = now, false
			}
			if m.phase != wasPhase {
				p.noteTask(m, now)
			}
			p.noteMember(m, now)
			if p.needsFill(m, now) {
				// The read takes the request now, so a wake during the read
				// sets filled to false again and the next poll reads again.
				m.filled, m.filling = true, true
				fill = append(fill, m)
			}
			if !m.learned && len(m.agents) == 0 {
				m.learned = true
				learn = append(learn, t)
			}
		}
		p.mu.Unlock()
	}
	p.mu.Lock()
	for name, m := range p.members {
		if m.atespace != "" && !seen[name] {
			delete(p.members, name)
			p.noteGone(m, now)
			for id, r := range p.runs {
				if r.member == name {
					p.drop(id, r, now)
				}
			}
		}
	}
	p.mu.Unlock()
	for _, m := range fill {
		p.mu.Lock()
		live := p.members[m.name] == m && m.state == MemberLive
		if !live {
			m.filling, m.filled = false, false
		}
		p.mu.Unlock()
		if live {
			p.backfill(ctx, m)
		}
	}
	p.mu.Lock()
	for _, atespace := range p.atespaces {
		if failed[atespace] == nil {
			p.listedOnce[atespace] = true
		}
	}
	p.mu.Unlock()
	for _, t := range learn {
		p.learn(ctx, client, t)
	}
	p.suspendIdle(ctx, now)
	p.reconcilePools(ctx, listed, failed, now)
}

// taskVar returns the value of a variable of a Task, or "".
func taskVar(t *v1alpha1.Task, name string) string {
	for _, e := range t.GetSpec().GetEnv() {
		if e.GetName() == name {
			return e.GetValue()
		}
	}
	return ""
}

// learn reads the profiles that a Task serves from the configuration file
// in its Workspaces, once, when the Task first appears. A Task that sleeps
// cannot be asked, and a read through the router would wake it, but its
// Workspace is a resource of the ax API and costs nothing. The file is the
// one that FW_CONFIG in the Task names, else .fw/fw.yaml in the first
// Workspace, as the kernel reads it under ax. A Task without such a file
// stays unknown until it runs.
func (p *platform) learn(ctx context.Context, client v1alpha1.AXClient, t *v1alpha1.Task) {
	atespace, taskName := t.GetMetadata().GetAtespace(), t.GetMetadata().GetName()
	name := atespace + "/" + taskName
	config := ""
	for _, e := range t.GetSpec().GetEnv() {
		if e.GetName() == "FW_CONFIG" {
			config = e.GetValue()
		}
	}
	for i, ref := range t.GetSpec().GetWorkspaces() {
		want := ".fw/fw.yaml"
		if config != "" {
			rel, ok := strings.CutPrefix(config, strings.TrimSuffix(ref.GetPath(), "/")+"/")
			if !ok {
				continue
			}
			want = rel
		} else if i > 0 {
			break
		}
		cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		ws, err := client.GetWorkspace(cctx, &v1alpha1.GetWorkspaceRequest{Atespace: atespace, Name: ref.GetName()})
		cancel()
		if err != nil {
			p.f.log.Warn("fleet: read the Workspace of a Task", "member", name, "workspace", ref.GetName(), "error", err)
			return
		}
		for _, f := range ws.GetSpec().GetFiles() {
			if strings.TrimPrefix(f.GetPath(), "./") != want {
				continue
			}
			agents, err := agentsOf([]byte(f.GetContent()))
			if err != nil {
				p.f.log.Warn("fleet: read the agents of a Task from its Workspace", "member", name, "file", f.GetPath(), "error", err)
				return
			}
			p.mu.Lock()
			if m := p.members[name]; m != nil {
				for _, ag := range agents {
					m.serves(ag)
				}
			}
			p.mu.Unlock()
			p.f.log.Info("fleet: learned the agents of a Task from its Workspace", "member", name, "agents", strings.Join(agents, ","))
			return
		}
	}
}

// agentsOf returns the names under the key agents of a configuration file,
// sorted.
func agentsOf(file []byte) ([]string, error) {
	var doc struct {
		Agents map[string]any `yaml:"agents"`
	}
	if err := yaml.Unmarshal(file, &doc); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(doc.Agents))
	for name := range doc.Agents {
		out = append(out, name)
	}
	sort.Strings(out)
	return out, nil
}

// listTasks reads every Task of an atespace, page by page.
func (p *platform) listTasks(ctx context.Context, client v1alpha1.AXClient, atespace string) ([]*v1alpha1.Task, error) {
	var out []*v1alpha1.Task
	for offset := int64(0); ; offset += listPage {
		cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		resp, err := client.ListTasks(cctx, &v1alpha1.ListTasksRequest{Atespace: atespace, Limit: listPage, Offset: offset})
		cancel()
		if err != nil {
			return nil, err
		}
		out = append(out, resp.GetTasks()...)
		if len(resp.GetTasks()) < listPage {
			return out, nil
		}
	}
}

// stateOfPhase maps the phase of a Task to the state of a member.
func stateOfPhase(phase string) string {
	switch phase {
	case phaseRunning:
		return MemberLive
	case phaseSuspended, phasePending, "":
		return MemberQuiet
	}
	return MemberStopped
}

// needsFill reports whether the front reads the runs of a member now: a
// live member that became live since its last read, or one that the view
// holds a queued or running run of, once per fillRepairInterval. A member
// that sleeps is never read. The caller holds p.mu.
func (p *platform) needsFill(m *platformMember, now time.Time) bool {
	if m.state != MemberLive || m.filling || now.Before(m.fillAt) {
		return false
	}
	if !m.filled {
		return true
	}
	if now.Sub(m.fillRead) < fillRepairInterval {
		return false
	}
	for _, r := range p.runs {
		if r.member == m.name && (r.run.State == fw.StateQueued || r.run.State == fw.StateRunning) {
			return true
		}
	}
	return false
}

// backfill reads the runs and the agents of a live Task with the static
// token of the fleet, so the view knows the runs that started before the
// front listened, the runs whose final event the stream lost, and the
// profiles that the Task serves. A read never replaces a newer state of the
// stream. The poller sets filling under p.mu before the read.
func (p *platform) backfill(ctx context.Context, m *platformMember) {
	start := time.Now()
	var runs struct {
		Runs []*fw.Run `json:"runs"`
	}
	runsErr := p.read(ctx, m, "/v1/runs", &runs)
	var agents struct {
		Agents []struct {
			Name string `json:"name"`
		} `json:"agents"`
	}
	if err := p.read(ctx, m, "/v1/agents", &agents); err != nil {
		p.f.log.Warn("fleet: read the agents of a Task; its runs name them later", "member", m.name, "error", err)
	}
	now := time.Now()
	p.mu.Lock()
	defer p.mu.Unlock()
	m.filling = false
	if p.members[m.name] != m {
		return // the Task left the view while the read was in flight
	}
	if runsErr != nil {
		// The runs that the stream reaches until then have no identity, so
		// the front reads the Task again after a pause.
		m.filled = false
		m.fillFails++
		m.fillAt = now.Add(fillPause(p.f.interval, m.fillFails))
		p.f.log.Warn("fleet: read the runs of a Task; the view reads it again later", "member", m.name, "retryAt", m.fillAt, "error", runsErr)
	} else {
		m.fillFails, m.fillAt, m.fillRead = 0, time.Time{}, now
	}
	for _, r := range runs.Runs {
		if r == nil || r.ID == "" {
			continue
		}
		m.serves(r.Agent)
		delete(p.gone, r.ID) // the run is in view again, not forgotten
		if r.Blocked != "" {
			// The kernel gives the storage failure of a run as it is, and
			// redacts only the reason of its status, so the front takes
			// out its own secrets, which a worker can share.
			r.Blocked = cut(p.f.a.Redact(r.Blocked), TextLimit)
		}
		cur, ok := p.runs[r.ID]
		changed := true
		switch {
		case !ok || cur.run.Seq < r.Seq || ((!cur.queued || cur.partial) && cur.run.Seq == r.Seq):
			// The read is newer than the view, or as new and the view has
			// no identity for the run, or only the facts of the index: the
			// read is the whole run. The input of the latest tool call
			// stays, because a read has none, and so does what the index
			// holds of the run.
			next := &platformRun{run: r, member: m.name, updated: now, queued: true, stateSeq: r.Seq}
			if ok {
				next.callID, next.input, next.idx = cur.callID, cur.input, cur.idx
			}
			p.runs[r.ID] = next
		case !cur.queued || cur.partial:
			cur.identify(r)
			cur.fromIndex, cur.partial = false, false
		case cur.run.Seq == r.Seq:
			// The read is as new as the view. fw.Run.Blocked is no event, so
			// only a read brings it: a storage failure stopped the process,
			// and the run keeps its last recorded state. The time of the
			// last update stays, so the suspend controller still sees how
			// long the run sent nothing.
			changed = cur.run.Blocked != r.Blocked
			cur.run.Blocked = r.Blocked
		default:
			changed = false
		}
		if changed {
			p.noteRun(r.ID, p.runs[r.ID], now)
		}
		if p.runs[r.ID].run.Blocked != "" {
			m.runsBlocked = true
		}
	}
	for _, ag := range agents.Agents {
		m.serves(ag.Name)
	}
	if runsErr == nil {
		// A run that only the index names, and that the Task does not hold,
		// is gone: a new Task of the same name took the place of the one
		// that held it. A run that the index gained after the read began
		// can be younger than the read, so it stays.
		held := make(map[string]bool, len(runs.Runs))
		for _, r := range runs.Runs {
			if r != nil {
				held[r.ID] = true
			}
		}
		for id, r := range p.runs {
			if r.member == m.name && r.fromIndex && !held[id] && r.updated.Before(start) {
				delete(p.runs, id)
				p.forgetIndex(id, r)
			}
		}
	}
}

// serves records that the member serves a profile. The caller holds p.mu.
func (m *platformMember) serves(agent string) {
	agent = strings.TrimSpace(agent)
	if agent == "" {
		return
	}
	if m.agents == nil {
		m.agents = map[string]bool{}
	}
	m.agents[agent] = true
}

// read makes one GET through the router to a member, with the static token
// of the fleet, and decodes the JSON answer into out. The caller makes sure
// that the Task is awake, or means to wake it.
func (p *platform) read(ctx context.Context, m *platformMember, path string, out any) error {
	return p.readAs(ctx, m, path, "", out)
}

// readAs is read with token instead of the static token of the fleet, when
// token is not empty: a pool reads a Task with the token that the Task
// holds, which is older than the token of the front after a rotation.
func (p *platform) readAs(ctx context.Context, m *platformMember, path, token string, out any) error {
	if strings.TrimSpace(token) == "" {
		t, err := fw.EnvSecrets{}.Get(ctx, p.tokenEnv)
		if err != nil || strings.TrimSpace(t) == "" {
			return fmt.Errorf("no static token in the secret %s", p.tokenEnv)
		}
		token = t
	}
	cctx, cancel := context.WithTimeout(fw.WithCredential(ctx, "Bearer "+strings.TrimSpace(token)), readTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := p.f.Forward(cctx, m.name, req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return &answerError{path: path, code: resp.StatusCode}
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, RecordLimit*4)).Decode(out); err != nil {
		return fmt.Errorf("decode %s: %w", path, err)
	}
	return nil
}

// answerError is an answer of a member other than 200 to a read.
type answerError struct {
	path string
	code int
}

func (e *answerError) Error() string { return fmt.Sprintf("%s answered %d", e.path, e.code) }

// readTimeout bounds one read of a Task through the router. A wake of a
// sleeping Task takes a few seconds, so it is longer than one request.
const readTimeout = 60 * time.Second

// onEvent applies one event of the egress stream to the run that it
// belongs to. An event without a node cannot be placed and is dropped.
func (p *platform) onEvent(e fw.Event) error {
	node := strings.TrimSpace(e.Node)
	if node == "" || e.Subject == "" {
		return nil
	}
	name, u, atespace := memberOf(node)
	now := time.Now()
	p.mu.Lock()
	defer p.mu.Unlock()
	m := p.members[name]
	if m == nil {
		m = &platformMember{name: name, url: u}
		p.members[name] = m
	}
	if m.atespace == "" {
		m.atespace = atespace
	}
	if m.state == "" || m.state == MemberQuiet {
		// An event is a sign of life, whatever the last phase said.
		m.seen = now
	}
	p.noteMember(m, now)
	r := p.runs[e.Subject]
	if r == nil {
		r = &platformRun{run: &fw.Run{}, member: name}
		if fr, ok := p.gone[e.Subject]; ok {
			// A run that the view dropped comes back with its identity.
			delete(p.gone, e.Subject)
			c := *fr.run
			r = &platformRun{run: &c, member: fr.member, queued: true, stateSeq: c.Seq}
		}
		p.runs[e.Subject] = r
	}
	// The totals of fw.run.finished and fw.run.suspended set the usage of
	// the run, with the cost at the prices of the worker. The prices of
	// this process price only the increments that the stream carries after
	// them, such as fw.run.annotated.
	//
	// An old or repeated event changes nothing, with two exceptions for an
	// event that arrives after later events, which a bus can deliver out of
	// order. The first fw.run.queued of a run gives the run its identity,
	// because only that event names the agent, the person, and the
	// metadata; the projection keeps the state of the later events. An end
	// of the run, fw.run.finished or fw.run.suspended, applies when no
	// event that changes the state came after it (lateEnd).
	//
	// Two processes can publish for one branch: its source records it as
	// failed under its id when the start failed, with Kept zero and no
	// copy, while the process of the branch took it after all, with Kept
	// the number of its copies. A later fw.run.queued with a larger Kept
	// applies too, so the copies of the source never count as the work of
	// the branch; the record of the source holds no work to lose.
	kept := queuedKept(e)
	switch late := e.Seq != 0 && e.Seq <= r.run.Seq; {
	case !late:
		before := r.run.Usage
		r.run.Apply(e, p.prices)
		r.track(e, before)
		r.noteToolInput(e)
		r.mark(e)
		if e.Type == fw.EvApprovalDecided && len(r.decided) < maxDecided {
			r.decided = append(r.decided, e.Seq)
		}
	case e.Type == fw.EvRunQueued && (!r.queued || kept > r.kept):
		r.run.Apply(e, p.prices)
	case (e.Type == fw.EvRunFinished || e.Type == fw.EvRunSuspended) && e.Seq > r.stateSeq:
		r.lateEnd(e, p.prices)
	default:
		return nil
	}
	if e.Type == fw.EvRunFinished {
		// An end that the view applied counts once: a repeated event
		// changes nothing above.
		p.ends.count(agentOf(r.run, e), name, e)
	}
	if e.Type == fw.EvRunQueued {
		r.queued, r.streamed = true, true
		r.kept = max(r.kept, kept)
	}
	// An event of the run confirms a run that the view knew from the index
	// alone.
	r.fromIndex = false
	r.member, r.updated = name, now
	if m.blocked != "" || m.runsBlocked {
		p.unblock(m)
	}
	m.serves(r.run.Agent)
	if e.Type == fw.EvRunChild {
		var d fw.RunChildData
		if e.Decode(&d) == nil && d.Child != "" {
			child, _, _ := memberOf(d.Member)
			p.children[d.Child] = childLink{parent: e.Subject, member: child, at: now}
		}
	}
	p.noteRun(e.Subject, r, now)
	p.tapped(r, e)
	return nil
}

// unblock clears the marks of a storage failure on a member and on its
// runs, after a new event of the member. A process that a storage failure
// blocked writes no event, so a new event comes from a process that works
// again, such as the fresh process after a suspend. The caller holds p.mu.
func (p *platform) unblock(m *platformMember) {
	m.blocked, m.runsBlocked = "", false
	for _, r := range p.runs {
		if r.member == m.name {
			r.run.Blocked = ""
		}
	}
}

// drop removes a run from the view and counts it for its batch, and the
// run leaves the index of the runs. The caller holds p.mu.
func (p *platform) drop(id string, r *platformRun, now time.Time) {
	delete(p.runs, id)
	p.forgetIndex(id, r)
	if r.queued {
		p.gone[id] = forgotRun{run: forgetful(r.run), member: r.member, at: now}
	}
}

// forgetful returns a copy of a run with what the authorization and the
// batch read, and no text: the id, the agent, the person, the parent and
// the depth for the lineage, the state, the origin, the usage, and the
// metadata fw.batch. A check of the control plane receives the facts of a
// run (fw.RunFacts) and no metadata, and the rules read the person, the
// agent, and the parent, so the title, the input, the output, the error,
// the pending call, and the rest of the metadata go.
func forgetful(r *fw.Run) *fw.Run {
	c := *r
	c.Title, c.Input, c.Output, c.Error, c.Pending, c.ExtraTools, c.Metadata = "", "", "", "", nil, nil, nil
	if batch := r.Metadata[fw.MetaBatch]; batch != "" {
		c.Metadata = map[string]string{fw.MetaBatch: batch}
	}
	return &c
}

// forgotten returns the runs of a batch that the view dropped, by member.
func (p *platform) forgotten(batch string) map[string][]*fw.Run {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := map[string][]*fw.Run{}
	for _, fr := range p.gone {
		if fr.run.Metadata[fw.MetaBatch] == batch {
			out[fr.member] = append(out[fr.member], fr.run)
		}
	}
	return out
}

// queuedKept returns RunQueuedData.Kept of a fw.run.queued, or zero for
// another event.
func queuedKept(e fw.Event) uint64 {
	if e.Type != fw.EvRunQueued {
		return 0
	}
	var d struct {
		Kept uint64 `json:"kept"`
	}
	_ = e.Decode(&d)
	return d.Kept
}

// identify gives a run that the stream reached before its fw.run.queued,
// and that is newer than the read of its member, the identity from the
// read: the agent, the person, the input, the parent, the batch, and the
// start. The state that the stream changed after the read stays, and the
// usage takes the read and the usage that the stream added after it,
// unless the stream brought newer totals.
func (r *platformRun) identify(read *fw.Run) {
	c := r.run
	c.Agent, c.Input, c.Origin, c.Principal = read.Agent, read.Input, read.Origin, read.Principal
	c.Parent, c.Depth, c.ExtraTools, c.CreatedAt = read.Parent, read.Depth, read.ExtraTools, read.CreatedAt
	md := maps.Clone(read.Metadata)
	if md == nil {
		md = map[string]string{}
	}
	maps.Copy(md, c.Metadata)
	c.Metadata = md
	if c.Title == "" {
		c.Title = read.Title
	}
	if r.stateSeq <= read.Seq {
		c.State, c.Pending, c.Output, c.Error = read.State, read.Pending, read.Output, read.Error
	}
	if c.UsageSeq <= read.Seq {
		u := read.Usage
		var after []addedUsage
		for _, a := range r.added {
			if a.seq > read.Seq {
				u = usageSum(u, a.u)
				after = append(after, a)
			}
		}
		c.Usage, c.UsageSeq, r.added = u, read.UsageSeq, after
	}
	if c.TreeUsage == nil {
		c.TreeUsage, c.TreeOpen, c.TreeWorking = read.TreeUsage, read.TreeOpen, read.TreeWorking
	}
	r.queued = true
}

// fillPause is the pause before a read of the runs of a Task after fails
// reads in a row failed: one interval, doubling up to fillPauseMax.
func fillPause(interval time.Duration, fails int) time.Duration {
	d := interval
	for i := 1; i < fails && d < fillPauseMax; i++ {
		d *= 2
	}
	return min(d, fillPauseMax)
}

// unknown counts the runs on the members that have no identity yet, and
// those of them that are not finished. The caller holds p.mu.
func (p *platform) unknown(members map[string]bool) (n, active int) {
	for _, r := range p.runs {
		if r.queued || r.run.ID == "" || !members[r.member] {
			continue
		}
		n++
		if !r.run.State.Terminal() {
			active++
		}
	}
	return n, active
}

// track records what an event in order did to the run: whether it changed
// the state, and the usage that it added after the latest totals.
func (r *platformRun) track(e fw.Event, before fw.RunUsage) {
	if changesState(e.Type) {
		r.stateSeq = e.Seq
	}
	if e.Seq != 0 && r.run.UsageSeq == e.Seq {
		r.added = nil // the totals of e cover every event before it
		return
	}
	if d := usageDiff(r.run.Usage, before); d != (fw.RunUsage{}) {
		r.added = append(r.added, addedUsage{seq: e.Seq, u: d})
	}
}

// lateEnd applies an end of the run that arrived after later events that
// changed no state, such as a title annotation, so the end is still the
// latest state of the run. Its totals cover the events before it, and the
// usage that the later events added goes on top.
func (r *platformRun) lateEnd(e fw.Event, prices fw.Prices) {
	r.run.Apply(e, prices)
	r.stateSeq = e.Seq
	if r.run.UsageSeq != e.Seq {
		return // an older kernel sends no totals
	}
	var after []addedUsage
	for _, a := range r.added {
		if a.seq > e.Seq {
			r.run.Usage = usageSum(r.run.Usage, a.u)
			after = append(after, a)
		}
	}
	r.added = after
}

// changesState reports whether an event of the type can change the state
// of a run in the projection.
func changesState(typ string) bool {
	switch typ {
	case fw.EvRunQueued, fw.EvRunStarted, fw.EvRunResumed, fw.EvTurnStarted, fw.EvMessageReceived,
		fw.EvApprovalRequested, fw.EvApprovalDecided, fw.EvRunSuspended, fw.EvRunFinished:
		return true
	}
	return false
}

// usageSum adds two usages field by field.
func usageSum(a, b fw.RunUsage) fw.RunUsage {
	a.Usage = a.Usage.Add(b.Usage)
	a.Turns += b.Turns
	a.ToolCalls += b.ToolCalls
	a.CostUSD += b.CostUSD
	a.UnpricedTokens += b.UnpricedTokens
	return a
}

// usageDiff is the usage a has beyond b, field by field.
func usageDiff(a, b fw.RunUsage) fw.RunUsage {
	a.InputTokens -= b.InputTokens
	a.OutputTokens -= b.OutputTokens
	a.CacheReadTokens -= b.CacheReadTokens
	a.CacheWriteTokens -= b.CacheWriteTokens
	a.Turns -= b.Turns
	a.ToolCalls -= b.ToolCalls
	a.CostUSD -= b.CostUSD
	a.UnpricedTokens -= b.UnpricedTokens
	return a
}

// MemberName turns the node of a kernel, "ax:<atespace>/<task>" for a Task,
// into the name of the member in the view.
func MemberName(node string) string {
	name, _, _ := memberOf(node)
	return name
}

// memberOf turns the node of an event into a member name, an address, and
// an atespace. "ax:<atespace>/<task>" is a Task; anything else is a
// process that the view cannot reach.
func memberOf(node string) (name, u, atespace string) {
	if target, ok := strings.CutPrefix(node, "ax:"); ok {
		space, task, found := strings.Cut(target, "/")
		if found && space != "" && task != "" {
			return target, node, space
		}
	}
	return node, "", ""
}

// records returns the members of the platforms as records, for the view.
// Each record carries the runs of the member, the newest finished runs
// first cut to FinishedRuns, and the agents that the member serves and
// that its runs name, with the access block that this process has for
// each agent, because the front decides for a worker with its own
// configuration. With all, a record holds every run of the member, as the
// tool batch needs it, and no finished run is left out.
func (p *platform) records(now time.Time, all bool) []remote {
	p.mu.Lock()
	defer p.mu.Unlock()
	for id, r := range p.runs {
		// A run without an identity, which no fw.run.queued, no read of its
		// member, and no copy of a dropped run gave one, expires after the
		// expire time without an event, whatever its state.
		if (r.run.State.Terminal() || !r.queued) && now.Sub(r.updated) > p.f.expire {
			p.drop(id, r, now)
		}
	}
	for id, fr := range p.gone {
		if now.Sub(fr.at) > p.f.expire {
			delete(p.gone, id)
		}
	}
	for id, l := range p.children {
		if _, ok := p.runs[id]; !ok && now.Sub(l.at) > p.f.expire {
			delete(p.children, id) // a child that never spoke
		}
	}
	byMember := p.runsByMember()
	out := make([]remote, 0, len(p.members))
	for _, m := range p.members {
		out = append(out, p.record(m, byMember[m.name], all))
	}
	return out
}

// memberRecord returns the record of one member as records gives it, or
// nil when the view does not hold the member.
func (p *platform) memberRecord(name string, now time.Time) *Record {
	p.mu.Lock()
	defer p.mu.Unlock()
	m := p.members[name]
	if m == nil {
		return nil
	}
	return p.record(m, p.runsByMember()[name], false).rec
}

// listed returns a run as the records list it. A child in another process
// has no parent in its own ledger; the parent recorded it, so the view
// shows it under the parent. The caller holds p.mu.
func (p *platform) listed(id string, r *platformRun) *fw.Run {
	run := r.run
	if l, ok := p.children[id]; ok && run.Parent == "" {
		c := *run
		c.Parent, c.Depth = l.parent, 1
		if src := p.runs[l.parent]; src != nil {
			c.Depth = src.run.Depth + 1
		}
		run = &c
	}
	return run
}

// runsByMember returns the runs that the records list, by member: every
// run with an identity, and a queued stub for each child that its process
// does not have yet. The caller holds p.mu.
func (p *platform) runsByMember() map[string][]*fw.Run {
	byMember := map[string][]*fw.Run{}
	for id, r := range p.runs {
		if r.run.ID == "" || !r.queued {
			continue // a run that has no identity yet is not listed
		}
		byMember[r.member] = append(byMember[r.member], p.listed(id, r))
	}
	for id, l := range p.children {
		// A child that its process does not have yet, for example a branch
		// whose Task is starting, is a queued run of its parent's member,
		// so a person finds it and the parent answers for it.
		src := p.runs[l.parent]
		if _, ok := p.runs[id]; ok || src == nil || src.run.ID == "" {
			continue
		}
		stub := &fw.Run{ID: id, Agent: src.run.Agent, State: fw.StateQueued, Principal: src.run.Principal,
			Parent: l.parent, Depth: src.run.Depth + 1, CreatedAt: l.at, UpdatedAt: l.at,
			Metadata: map[string]string{fw.MetaParent: l.parent}}
		if src.run.Metadata[fw.MetaBatch] != "" {
			stub.Metadata[fw.MetaBatch] = src.run.Metadata[fw.MetaBatch]
		}
		byMember[src.member] = append(byMember[src.member], stub)
	}
	return byMember
}

// record builds the record of one member from its runs (runsByMember).
// The caller holds p.mu.
func (p *platform) record(m *platformMember, runs []*fw.Run, all bool) remote {
	cfg := p.f.a.Config()
	sort.Slice(runs, func(i, j int) bool {
		if !runs[i].CreatedAt.Equal(runs[j].CreatedAt) {
			return runs[i].CreatedAt.After(runs[j].CreatedAt)
		}
		return runs[i].ID > runs[j].ID
	})
	kept, omitted := pickRuns(runs)
	if all {
		kept, omitted = make([]*fw.Run, 0, len(runs)), 0
		for _, r := range runs {
			kept = append(kept, shorten(r))
		}
	}
	rec := &Record{
		Name:          m.name,
		URL:           m.url,
		Atespace:      m.atespace,
		Access:        Access{Mode: p.f.a.AccessMode(), Default: cfg.Access.Default, Admins: cfg.Access.Admins},
		Agents:        []RecordAgent{},
		Runs:          kept,
		Omitted:       omitted,
		State:         m.state,
		BlockedReason: m.blocked,
		UpdatedAt:     m.seen,
	}
	// The agents of a member are the profiles that it serves, read from
	// the Task once (backfill), and the ones that its runs name, so an idle
	// Task is visible to the people who can use it.
	seenAgent := map[string]bool{}
	addAgent := func(name string) {
		if name == "" || seenAgent[name] {
			return
		}
		seenAgent[name] = true
		ag := RecordAgent{Name: name}
		if prof, ok := p.f.a.ProfileNamed(name); ok {
			ag.Description, ag.Access = prof.Description, prof.Access
		}
		rec.Agents = append(rec.Agents, ag)
	}
	for name := range m.agents {
		addAgent(name)
	}
	for _, r := range runs {
		addAgent(r.Agent)
	}
	sort.Slice(rec.Agents, func(i, j int) bool { return rec.Agents[i].Name < rec.Agents[j].Name })
	seen := m.seen
	for _, r := range runs {
		if r.UpdatedAt.After(seen) {
			seen = r.UpdatedAt
		}
	}
	return remote{rec: rec, seen: seen}
}
