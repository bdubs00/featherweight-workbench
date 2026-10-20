package sources

// This file is the contract of the de-duplication of the sources, and its
// store in memory. A source claims the key of an event before it
// publishes the run request, so two replicas of the front that get the
// same event race on the claim, and only the winner publishes. A claim
// has two states. It is pending from the claim to the publish, and done
// after the publish. A done claim blocks the key for the window of the
// source. A pending claim tells another replica that the event is in
// flight. When the publish fails, the source releases the claim, so the
// next delivery of the event makes the run. A pending claim that stays
// longer than staleClaim, because its replica stopped between the claim
// and the end, goes to the next delivery of the event.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// staleClaim is how long a pending claim holds a key. It is far longer
// than a publish takes.
const staleClaim = 2 * time.Minute

// Errors of a store.
var (
	// ErrDuplicate means that an event with the key made a run within
	// the window of the source.
	ErrDuplicate = errors.New("sources: the key made a run within the window")
	// ErrInFlight means that another caller holds the key and has not
	// published yet. The event is neither new nor a duplicate yet, so
	// the caller tries again later.
	ErrInFlight = errors.New("sources: another replica handles the key now")
	// ErrStore means that the store did not answer. Without a claim, no
	// run starts.
	ErrStore = errors.New("sources: the de-duplication store did not answer")
)

// store keeps the claims on the keys of the sources. Take claims a key
// that nobody holds and returns the ticket of the claim, or ErrDuplicate,
// ErrInFlight, or an error that wraps ErrStore. Finish marks the claim of
// a ticket done after the publish, and Release removes it after a publish
// that failed. Both act only when the claim is still the one of the
// ticket. The window of a source is the window that the store got for
// it at its start.
type store interface {
	Take(ctx context.Context, source, key string) (ticket, error)
	Finish(ctx context.Context, t ticket) error
	Release(ctx context.Context, t ticket) error
	Close() error
	// Describe says in a few words where the claims live.
	Describe() string
}

// ticket is one claim of a key.
type ticket struct {
	source, key string
	id          string // the name of the claim in the store
	rev         uint64
}

// claimStates of a claim.
const (
	statePending = "pending"
	stateDone    = "done"
)

// memStore keeps the claims in the memory of the process. It serves a
// process whose bus is mem: such a process has no replica, and a restart
// forgets the claims.
type memStore struct {
	windows map[string]time.Duration
	now     func() time.Time
	stale   time.Duration

	mu     sync.Mutex
	claims map[string]memClaim
	rev    uint64
	swept  time.Time
}

type memClaim struct {
	rev   uint64
	state string
	at    time.Time // the time of the last write
}

func newMemStore(windows map[string]time.Duration) *memStore {
	return &memStore{windows: windows, now: time.Now, stale: staleClaim, claims: map[string]memClaim{}}
}

func (m *memStore) Describe() string {
	return "in the memory of this process, which a restart forgets and no replica shares"
}

func (m *memStore) window(source string) time.Duration {
	if w := m.windows[source]; w > 0 {
		return w
	}
	return DefaultDedupFor
}

func (m *memStore) Take(_ context.Context, source, key string) (ticket, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	m.sweepLocked(now)
	id := source + "\x00" + key
	if c, ok := m.claims[id]; ok && now.Sub(c.at) < m.window(source) {
		switch {
		case c.state == stateDone:
			return ticket{}, ErrDuplicate
		case now.Sub(c.at) < m.stale:
			return ticket{}, ErrInFlight
		}
	}
	m.rev++
	m.claims[id] = memClaim{rev: m.rev, state: statePending, at: now}
	return ticket{source: source, key: key, id: id, rev: m.rev}, nil
}

func (m *memStore) Finish(_ context.Context, t ticket) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.claims[t.id]
	if !ok || c.rev != t.rev {
		return fmt.Errorf("sources: the claim of %s changed", t.key)
	}
	m.rev++
	m.claims[t.id] = memClaim{rev: m.rev, state: stateDone, at: m.now()}
	return nil
}

func (m *memStore) Release(_ context.Context, t ticket) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if c, ok := m.claims[t.id]; ok && c.rev == t.rev {
		delete(m.claims, t.id)
	}
	return nil
}

func (m *memStore) Close() error { return nil }

// sweepLocked drops the expired claims, at most once a minute.
func (m *memStore) sweepLocked(now time.Time) {
	if now.Sub(m.swept) < time.Minute {
		return
	}
	m.swept = now
	for id, c := range m.claims {
		source, _, _ := strings.Cut(id, "\x00")
		if now.Sub(c.at) >= m.window(source) {
			delete(m.claims, id)
		}
	}
}
