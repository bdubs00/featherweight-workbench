package fleet

// This file lets the view stand in for a member that sleeps (RestView). A
// request to a Task goes through the router of ax, and the router wakes the
// Task for any request, also for a read of a run whose answer the view
// already holds. So the relay answers such a read from the view while the
// member does not run (relay.go), and a person who reads a conversation
// does not wake its worker.

import (
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// The fleet stands in for a member that sleeps.
var _ RestView = (*Fleet)(nil)

// AtRest implements RestView. The copy is the run as the projection of the
// view holds it: the state, the pending call, the usage, and the texts that
// the egress stream carried, which a worker of a pool cuts to
// EgressMaxBytes per string. A child that its process does not have yet is
// the queued run that the record of the member of its parent shows, so
// AtRest returns the run that it took. A run that the view holds without an
// identity has no copy that can stand in for the member, and AtRest
// returns nil for it.
func (f *Fleet) AtRest(run *fw.Run) *fw.Run {
	p := f.platform
	if p == nil || run == nil || run.ID == "" || run.Member == "" {
		return nil
	}
	now := time.Now()
	p.mu.Lock()
	defer p.mu.Unlock()
	m := p.members[run.Member]
	if m == nil || p.runsNow(m, now) {
		return nil
	}
	r, ok := p.runs[run.ID]
	if !ok {
		if l, child := p.children[run.ID]; child && p.runs[l.parent] != nil && p.runs[l.parent].member == run.Member {
			c := *run
			return detach(&c)
		}
		return nil
	}
	if !r.queued || r.run.ID == "" || r.member != run.Member {
		return nil
	}
	c := *p.listed(run.ID, r)
	if c.Pending != nil {
		pending := *c.Pending
		c.Pending = &pending
	}
	c.Member = run.Member
	return detach(&c)
}

// runsNow reports whether a member runs: its Task is live, or a worker that
// only the stream knows sent an event within QuietAfter intervals. The
// caller holds p.mu.
func (p *platform) runsNow(m *platformMember, now time.Time) bool {
	if m.state != "" {
		return m.state == MemberLive
	}
	return now.Sub(m.seen) <= QuietAfter*p.f.interval
}
