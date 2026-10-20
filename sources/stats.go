package sources

// This file counts what each source did since this process started: the
// events that made a run, the duplicates, the events that another replica
// held at that moment, and the events that made no run for another
// reason, with the last error. Stats reads the counts, so a collector of
// metrics can export them. Each replica counts its own events, so a sum
// over the replicas gives the counts of the fleet.

import (
	"sync"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// Stat is the count of one source in this process since its start.
//
// Accepted counts the events that made a run request. Duplicate counts
// the events whose key made a run within the window. Busy counts the
// events that another replica held at that moment; the sender of a
// webhook got a 503 and sends them again, and a poll meets them again.
// Refused counts the events that made no run for any other reason: a
// wrong credential, a body or a template that failed, a store that did
// not answer, or a publish that failed. A request that the source refuses
// before it reads its events counts as one event.
//
// LastError is the last reason of a refusal or of a failed poll, in words
// that hold no secret, and LastErrorAt its time. LastAcceptedAt is the
// time of the last run request. LastPollAt is the time of the last poll
// that read the endpoint, for a poll source.
type Stat struct {
	Source         string    `json:"source"`
	Kind           string    `json:"kind"`
	Agent          string    `json:"agent"`
	Accepted       uint64    `json:"accepted"`
	Duplicate      uint64    `json:"duplicate"`
	Busy           uint64    `json:"busy"`
	Refused        uint64    `json:"refused"`
	LastError      string    `json:"lastError,omitempty"`
	LastErrorAt    time.Time `json:"lastErrorAt,omitzero"`
	LastAcceptedAt time.Time `json:"lastAcceptedAt,omitzero"`
	LastPollAt     time.Time `json:"lastPollAt,omitzero"`
}

// Stats returns the counts of the sources of an agent, in the order of the
// configuration, or nil when the agent runs no source.
func Stats(a *fw.Agent) []Stat {
	s := of(a)
	if s == nil {
		return nil
	}
	return s.Stats()
}

// counter is the count of one source.
type counter struct {
	mu sync.Mutex
	st Stat
}

func (c *counter) snapshot() Stat {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.st
}

func (c *counter) accepted(n int, at time.Time) {
	if n == 0 {
		return
	}
	c.mu.Lock()
	c.st.Accepted += uint64(n)
	c.st.LastAcceptedAt = at
	c.mu.Unlock()
}

func (c *counter) duplicate(n int) {
	c.mu.Lock()
	c.st.Duplicate += uint64(n)
	c.mu.Unlock()
}

func (c *counter) busy(n int) {
	c.mu.Lock()
	c.st.Busy += uint64(n)
	c.mu.Unlock()
}

// refused counts n refused events. A reason other than "" becomes the last
// error.
func (c *counter) refused(n int, reason string, at time.Time) {
	c.mu.Lock()
	c.st.Refused += uint64(n)
	if reason != "" {
		c.st.LastError, c.st.LastErrorAt = reason, at
	}
	c.mu.Unlock()
}

// failed records the error of a poll that read no event.
func (c *counter) failed(reason string, at time.Time) { c.refused(0, reason, at) }

func (c *counter) polled(at time.Time) {
	c.mu.Lock()
	c.st.LastPollAt = at
	c.mu.Unlock()
}
