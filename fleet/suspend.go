package fleet

// This file is the suspend controller of the front (docs/ARCHITECTURE.md
// section 4.12). A Task that waits for a person costs compute while it
// runs, and nothing in it changes until the person answers. The controller
// reads the idle time of every running Task from the view, which lives on
// the egress stream, and when a Task stays idle past the grace period, it
// reads GET /v1/status of the Task once through the router, which wakes
// nobody because the Task runs, and calls SuspendTask on the ax API. The
// router wakes the Task on the next request: an approval, a message, or a
// run. A worker that sleeps reads no queue, so a job for a profile wakes
// the suspended Tasks that serve it: right after the enqueue of the front,
// and when the front finds jobs that wait in the queue (queues.go).
//
// A storage failure, such as a full /workspace, blocks the kernel of a
// Task: it writes nothing, keeps its open runs in their last state, and
// sends no event. The view then shows those runs as running for ever. So
// the controller also reads the status of a Task that holds a queued or
// running run but sent nothing for the grace period, at most once per
// interval, and suspends the Task when the status says blocked. A suspend
// and a resume restart the process under ax, and the fresh process
// rebuilds from its ledger.

import (
	"context"
	"strings"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/surface/view"
	v1alpha1 "github.com/google/ax/pkg/apis/v1alpha1"
)

// suspendTimeout bounds one SuspendTask call.
const suspendTimeout = 30 * time.Second

// blockedNoReason is the mark of a Task whose status says blocked without
// a reason, so the mark is never empty.
const blockedNoReason = "the Task reports a storage failure and gives no reason"

// candidate is a Task that the controller reads before it acts. busy is
// true when the view holds a queued or running run of the Task, so only a
// blocked status makes the controller suspend it.
type candidate struct {
	m    *platformMember
	busy bool
}

// suspendIdle suspends every running Task of the atespaces that has been
// idle for the grace period, and every running Task that a storage failure
// blocked. The view decides who is a candidate: a Task whose phase is
// Running and whose newest run update, or whose last start, is older than
// the grace period. A Task with no run queued or running is idle, and the
// controller reads its status at every interval. A Task with such a run is
// busy, and the controller reads its status at most once per interval. The
// status body of the Task decides. A Task that says blocked is suspended at
// once. An idle Task is suspended when the body says idle for the grace
// period, so a view that missed an event suspends nobody who works. A busy
// Task that is not blocked is left alone: its runs can work slowly.
func (p *platform) suspendIdle(ctx context.Context, now time.Time) {
	after := p.f.suspendAfter
	if after <= 0 {
		return
	}
	p.mu.Lock()
	client := p.client
	var check []candidate
	for _, m := range p.members {
		if m.atespace == "" || m.state != MemberLive || m.liveSince.IsZero() {
			continue
		}
		since, working := m.liveSince, false
		for _, r := range p.runs {
			if r.member != m.name {
				continue
			}
			// A run that a storage failure blocked does no work, whatever
			// its last recorded state (fw.Run.Blocked).
			if (r.run.State == fw.StateQueued || r.run.State == fw.StateRunning) && r.run.Blocked == "" {
				working = true
			}
			if r.updated.After(since) {
				since = r.updated
			}
		}
		// A child that the member placed in another process, and that has
		// no events yet, is work of the member: it sends the branch once
		// the child Task runs, and a sleeping member sends nothing.
		for id, l := range p.children {
			if _, ok := p.runs[id]; ok {
				continue
			}
			if src := p.runs[l.parent]; src != nil && src.member == m.name {
				working = true
				break
			}
		}
		if now.Sub(since) < after {
			continue
		}
		if working {
			// One read per interval at most, whatever calls the
			// controller. A pass that comes a moment early, by the jitter
			// of the ticker, leaves the read to the next pass.
			if now.Sub(m.statusAt) < p.f.interval {
				continue
			}
			m.statusAt = now
		}
		check = append(check, candidate{m: m, busy: working})
	}
	p.mu.Unlock()
	if client == nil {
		return
	}
	for _, c := range check {
		m := c.m
		var st view.Status
		if err := p.read(ctx, m, "/v1/status", &st); err != nil {
			if c.busy {
				p.f.log.Debug("fleet: read the status of a busy Task that sent no event for the grace period", "member", m.name, "error", err)
			} else {
				p.f.log.Warn("fleet: read the status of an idle Task before a suspend", "member", m.name, "error", err)
			}
			continue
		}
		if st.Blocked {
			p.suspendBlocked(ctx, client, m, st.BlockedReason, now)
			continue
		}
		if c.busy {
			p.f.log.Debug("fleet: a busy Task sent no event for the grace period and is not blocked; the front leaves it", "member", m.name)
			continue
		}
		if !st.Idle || (st.IdleSince != nil && now.Sub(*st.IdleSince) < after) {
			continue
		}
		cctx, cancel := context.WithTimeout(ctx, suspendTimeout)
		_, err := client.SuspendTask(cctx, &v1alpha1.SuspendTaskRequest{Atespace: m.atespace, Name: taskOf(m)})
		cancel()
		if err != nil {
			p.f.log.Warn("fleet: suspend an idle Task", "member", m.name, "error", err)
			continue
		}
		idleFor := after
		if st.IdleSince != nil {
			idleFor = now.Sub(*st.IdleSince)
		}
		p.mu.Lock()
		// The phase follows the suspend, so a wake through the router
		// before the next Task list makes the front resume the Task
		// (resumed), as after any other suspend.
		m.state, m.phase, m.seen = MemberQuiet, phaseSuspended, now
		p.mu.Unlock()
		p.f.log.Info("fleet: suspended an idle Task", "member", m.name, "idle", idleFor.Round(time.Second).String())
	}
}

// suspendBlocked suspends a Task whose status says that a storage failure
// blocked it, and marks the member as blocked with the reason until its
// next event (unblock), so the view tells a person why the Task went to
// sleep. The mark stays when the suspend fails, because the Task is still
// blocked, and the controller tries again at a later interval. The worker
// redacts the reason, and the front redacts it again with its own secrets
// and cuts it to TextLimit before the reason reaches the view or the log.
func (p *platform) suspendBlocked(ctx context.Context, client v1alpha1.AXClient, m *platformMember, reason string, now time.Time) {
	reason = cut(strings.TrimSpace(p.f.a.Redact(reason)), TextLimit)
	if reason == "" {
		reason = blockedNoReason
	}
	cctx, cancel := context.WithTimeout(ctx, suspendTimeout)
	_, err := client.SuspendTask(cctx, &v1alpha1.SuspendTaskRequest{Atespace: m.atespace, Name: taskOf(m)})
	cancel()
	p.mu.Lock()
	m.blocked = reason
	if err == nil {
		// The phase follows the suspend, as for an idle Task.
		m.state, m.phase, m.seen = MemberQuiet, phaseSuspended, now
	}
	p.mu.Unlock()
	if err != nil {
		p.f.log.Warn("fleet: suspend a Task that a storage failure blocked", "member", m.name, "reason", reason, "error", err)
		return
	}
	p.f.log.Warn("fleet: suspended a Task that a storage failure blocked; the next request restarts it from its ledger", "member", m.name, "reason", reason)
}

// taskOf returns the Task name of a member "<atespace>/<task>".
func taskOf(m *platformMember) string {
	return strings.TrimPrefix(m.name, m.atespace+"/")
}

// Causes of a wake, as the log names them.
const (
	wakeEnqueue = "enqueue" // a job of the front went on the queue (Enqueue)
	wakeQueue   = "queue"   // jobs wait in the queue (queues.go)
)

// WakeFor wakes the suspended Tasks that take the jobs of the queue of a
// profile, <intake>.<agent>, after a job went on that queue. A Task that
// sleeps reads no queue, and the router wakes a Task on a request only, so
// the front sends each one a read of GET /v1/status. The reads run in the
// background; the caller does not wait for the wakes. A Task of a pool
// takes the jobs of the intake of its pool alone, and a Task of no pool
// takes the jobs of every profile that it serves (servesQueue). When no
// sleeping Task is known to take the jobs, the front wakes the sleeping
// Tasks of no pool whose profiles it does not know yet, such as the Tasks
// that slept since before the front started: one of them can serve the
// profile, and the first wake teaches the front what each one serves.
func (f *Fleet) WakeFor(agent string) {
	agent = strings.TrimSpace(agent)
	f.wakeFor(f.intake+"."+agent, agent, wakeEnqueue, 0)
}

// wakeFor wakes the sleeping Tasks for the queue of a profile as WakeFor
// says, and returns how many wakes it started. It leaves out a Task whose
// wake still runs, and, with hold above zero, a Task that a wake reached
// within hold, so a Task that just woke has time to take a job. The ax API
// records no reason for a suspend, so the front cannot tell a Task that a
// person suspended from one that the controller suspended, and wakes both.
func (f *Fleet) wakeFor(queue, agent, cause string, hold time.Duration) int {
	p := f.platform
	if p == nil || agent == "" {
		return 0
	}
	now := time.Now()
	p.mu.Lock()
	intakes := p.intakes()
	var sleeping, unknown []*platformMember
	for _, m := range p.members {
		if m.atespace == "" || m.state != MemberQuiet {
			continue
		}
		switch serves, known := m.servesQueue(queue, agent, intakes); {
		case serves:
			sleeping = append(sleeping, m)
		case !known:
			unknown = append(unknown, m)
		}
	}
	if len(sleeping) == 0 {
		sleeping = unknown
	}
	var wake []*platformMember
	for _, m := range sleeping {
		if m.waking || (hold > 0 && now.Sub(m.wokeAt) < hold) {
			continue
		}
		m.waking, m.wokeAt = true, now
		wake = append(wake, m)
	}
	p.mu.Unlock()
	for _, m := range wake {
		go p.wake(f.ctx, m, agent, cause)
	}
	return len(wake)
}

// intakes returns the intake of each pool that the front keeps, by the
// name of the pool: the pools of the file and the runtime pools of the
// last pass, as the pools render bus.intake. The caller holds p.mu.
func (p *platform) intakes() map[string]string {
	out := make(map[string]string, len(p.pools)+len(p.rt))
	for _, pl := range p.pools {
		out[pl.Name] = pl.Intake
	}
	for name, rt := range p.rt {
		if _, ok := out[name]; !ok && rt.base != nil {
			out[name] = rt.base.Intake
		}
	}
	return out
}

// servesQueue reports whether the member takes the jobs of queue, the
// queue of the profile agent, and whether the front knows that. A Task of
// a pool that the front keeps takes the jobs of the intake of its pool
// alone, whatever profiles its configuration holds: the pool of a manager
// holds the profiles of its children, and its Tasks read the queue of the
// manager. A Task of no pool takes the jobs of every profile that it
// serves. The front does not know about a Task of no pool whose profiles it
// has not learned yet.
func (m *platformMember) servesQueue(queue, agent string, intakes map[string]string) (serves, known bool) {
	if intake, ok := intakes[m.pool]; ok && m.pool != "" {
		return intake == queue, true
	}
	if len(m.agents) == 0 {
		return false, false
	}
	return m.agents[agent], true
}

// wake reads the status of a sleeping Task through the router, which
// resumes it, and makes the phase of the Task follow (resumed).
func (p *platform) wake(ctx context.Context, m *platformMember, agent, cause string) {
	var st view.Status
	err := p.read(ctx, m, "/v1/status", &st)
	p.mu.Lock()
	m.waking = false
	p.mu.Unlock()
	if err != nil {
		p.f.log.Warn("fleet: wake a Task for a job", "member", m.name, "agent", agent, "cause", cause, "error", err)
		return
	}
	p.f.log.Info("fleet: woke a Task for a job", "member", m.name, "agent", agent, "cause", cause)
	p.resumed(m.name)
}

// resumed makes the phase of a Task follow a request that reached it
// through the router. The router wakes a sleeping actor on a request, and
// the ax API does not learn of it: the phase stays Suspended, so the
// controller would never suspend the Task again, and the actor would run
// on. The front marks the member live and calls ResumeTask, which changes
// nothing for a Task that runs, once per wake. Forward calls it after every
// answer of an ax member, so an approval or a run that wakes a Task counts.
func (p *platform) resumed(member string) {
	p.mu.Lock()
	m := p.members[member]
	client := p.client
	if m == nil || m.atespace == "" || client == nil {
		p.mu.Unlock()
		return
	}
	now := time.Now()
	if m.state != MemberLive {
		m.state, m.liveSince = MemberLive, now
		// The next poll reads the runs again, because the worker can lose
		// its final events when it sleeps. An answer of a Task that is
		// live already asks for no read.
		m.filled = false
	}
	m.seen = now
	if m.phase == phaseRunning || m.resuming {
		p.mu.Unlock()
		return
	}
	m.resuming = true
	p.mu.Unlock()
	go func() {
		ctx, cancel := context.WithTimeout(p.f.ctx, suspendTimeout)
		defer cancel()
		_, err := client.ResumeTask(ctx, &v1alpha1.ResumeTaskRequest{Atespace: m.atespace, Name: taskOf(m)})
		p.mu.Lock()
		m.resuming = false
		if err == nil {
			m.phase = phaseRunning
		}
		p.mu.Unlock()
		if err != nil {
			p.f.log.Warn("fleet: the phase of a woken Task does not follow", "member", m.name, "error", err)
			return
		}
		p.f.log.Info("fleet: the phase of a woken Task follows", "member", m.name)
	}()
}
