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
// the suspended Tasks that serve it.

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

// suspendIdle suspends every running Task of the atespaces that has been
// idle for the grace period. The view decides who is a candidate: a Task
// whose phase is Running, with no run queued or running, whose newest run
// update, or whose last start, is older than the grace period. The status
// body of the Task confirms it, so a view that missed an event suspends
// nobody who works.
func (p *platform) suspendIdle(ctx context.Context, now time.Time) {
	after := p.f.suspendAfter
	if after <= 0 {
		return
	}
	p.mu.Lock()
	client := p.client
	var idle []*platformMember
	for _, m := range p.members {
		if m.atespace == "" || m.state != MemberLive || m.liveSince.IsZero() {
			continue
		}
		since, working := m.liveSince, false
		for _, r := range p.runs {
			if r.member != m.name {
				continue
			}
			if r.run.State == fw.StateQueued || r.run.State == fw.StateRunning {
				working = true
				break
			}
			if r.updated.After(since) {
				since = r.updated
			}
		}
		if !working && now.Sub(since) >= after {
			idle = append(idle, m)
		}
	}
	p.mu.Unlock()
	if client == nil {
		return
	}
	for _, m := range idle {
		var st view.Status
		if err := p.read(ctx, m, "/v1/status", &st); err != nil {
			p.f.log.Warn("fleet: read the status of an idle Task before a suspend", "member", m.name, "error", err)
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
		m.state, m.seen = MemberQuiet, now
		p.mu.Unlock()
		p.f.log.Info("fleet: suspended an idle Task", "member", m.name, "idle", idleFor.Round(time.Second).String())
	}
}

// taskOf returns the Task name of a member "<atespace>/<task>".
func taskOf(m *platformMember) string {
	return strings.TrimPrefix(m.name, m.atespace+"/")
}

// WakeFor wakes the suspended Tasks that serve a profile, after a job went
// on the queue of that profile. A Task that sleeps reads no queue, and the
// router wakes a Task on a request only, so the front sends each one a
// read of GET /v1/status. The reads run in the background; the caller does
// not wait for the wakes.
func (f *Fleet) WakeFor(agent string) {
	p := f.platform
	agent = strings.TrimSpace(agent)
	if p == nil || agent == "" {
		return
	}
	p.mu.Lock()
	var sleeping []*platformMember
	for _, m := range p.members {
		if m.atespace != "" && m.state == MemberQuiet && m.agents[agent] {
			sleeping = append(sleeping, m)
		}
	}
	p.mu.Unlock()
	for _, m := range sleeping {
		go p.wake(f.ctx, m, agent)
	}
}

// wake reads the status of a sleeping Task through the router, which
// resumes it, and makes the phase of the Task follow (resumed).
func (p *platform) wake(ctx context.Context, m *platformMember, agent string) {
	var st view.Status
	if err := p.read(ctx, m, "/v1/status", &st); err != nil {
		p.f.log.Warn("fleet: wake a Task for a job", "member", m.name, "agent", agent, "error", err)
		return
	}
	p.f.log.Info("fleet: woke a Task for a job", "member", m.name, "agent", agent)
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
