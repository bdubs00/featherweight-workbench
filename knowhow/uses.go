package knowhow

// This file counts the uses of each note (docs/KNOWHOW.md section 4.5).
// The kernel records fw.memory.used for the block at the start of a run,
// for each view of the memory tool, and for each hit of recall that the
// run read. The fleet hands each such event of the egress stream to the
// service (fleet.Tap), with the agent and the principal of its run, and
// a goroutine of the service counts it. Every replica gets every event,
// so the entry of the uses keeps the ids of the last events that it
// counted, and a write at the revision that it read counts each event once.

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/fleet"
)

// Bounds of the count of uses.
const (
	// usesQueue is how many events wait for the count. Past it, the count
	// drops an event, and the log says so once.
	usesQueue = 4096
	// usesSeen is how many event ids the entry of a note keeps.
	usesSeen = 32
	// usesTimeout bounds the writes of one event.
	usesTimeout = 10 * time.Second
	// usesRetry is when an event whose run the view did not know yet
	// counts again.
	usesRetry = 2 * time.Second
)

// tapped is the function of fleet.Tap: it hands the event to the count of
// the agent, without blocking the view.
func tapped(a *fw.Agent, ev fleet.RunEvent) {
	if s := Of(a); s != nil {
		s.offerUse(ev)
	}
}

// offerUse queues an event, or drops it when the queue is full.
func (s *Service) offerUse(ev fleet.RunEvent) {
	select {
	case s.usesIn <- ev:
	default:
		s.mu.Lock()
		first := !s.full
		s.full = true
		s.mu.Unlock()
		if first {
			s.log.Warn("knowhow: the count of the uses of the notes is behind and drops events", "queue", usesQueue)
		}
	}
}

// countUses counts the queued events until ctx ends.
func (s *Service) countUses(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case ev := <-s.usesIn:
			s.mu.Lock()
			s.full = false
			s.mu.Unlock()
			cctx, cancel := context.WithTimeout(ctx, usesTimeout)
			s.CountUse(cctx, ev)
			cancel()
		}
	}
}

// parseRef reads a ref of a note: mem://agent/<agent>/<name> or
// mem://principal/<agent>/<name>. A ref of the shared scope, or one that
// does not read, reports false.
func parseRef(ref string) (kind, agent, name string, ok bool) {
	rest, ok := strings.CutPrefix(ref, "mem://")
	if !ok {
		return "", "", "", false
	}
	parts := strings.SplitN(rest, "/", 3)
	if len(parts) != 3 || (parts[0] != ScopeAgent && parts[0] != ScopePrincipal) || CheckAgent(parts[1]) != nil || CheckName(parts[2]) != nil {
		return "", "", "", false
	}
	return parts[0], parts[1], parts[2], true
}

// CountUse counts one use of each note that an event fw.memory.used
// names. A note of the scope principal belongs to the principal of the
// run. When the view did not know the principal, the note of that name
// whose hash the event names counts, if exactly one note has it.
func (s *Service) CountUse(ctx context.Context, ev fleet.RunEvent) {
	e := ev.Event
	if e.Type != fw.EvMemoryUsed {
		return
	}
	var d fw.MemoryUsedData
	if e.Decode(&d) != nil {
		return
	}
	id := e.ID
	if id == "" {
		id = e.Subject + "/" + e.Type + "/" + e.Time.UTC().Format(time.RFC3339Nano)
	}
	at := e.Time.UTC()
	if at.IsZero() {
		at = s.now().UTC()
	}
	done := map[string]bool{}
	for _, r := range d.Refs {
		kind, agent, name, ok := parseRef(r.Ref)
		if !ok {
			continue
		}
		sc := Scope{Kind: kind}
		if kind == ScopePrincipal {
			sc.Principal = ev.Principal
			if sc.Principal == "" {
				sc.Principal = s.principalOfRun(ctx, e.Subject)
			}
			if sc.Principal == "" {
				sc.Principal = s.principalOf(ctx, agent, name, r.Hash)
			}
			if sc.Principal == "" {
				// The bus brought the event before the start of its run.
				s.later(ev)
				continue
			}
		}
		key := usesKey(agent, sc, name)
		if done[key] {
			continue
		}
		done[key] = true
		if err := s.use(ctx, agent, sc, name, id, at); err != nil {
			s.log.Warn("knowhow: a use of a note was not counted", "agent", agent, "note", name, "error", err)
		}
	}
}

// principalOfRun returns the principal of a run that the view of the
// fleet holds, or "".
func (s *Service) principalOfRun(ctx context.Context, id string) string {
	if s.a == nil {
		return ""
	}
	f, ok := fleet.Of(s.a).(*fleet.Fleet)
	if !ok || f == nil {
		return ""
	}
	r, err := f.Run(ctx, id)
	if err != nil || r == nil {
		return ""
	}
	return r.Principal
}

// later counts an event again after usesRetry, once, for an event whose
// run the view did not know yet.
func (s *Service) later(ev fleet.RunEvent) {
	s.mu.Lock()
	if s.retried == nil || len(s.retried) > usesQueue {
		s.retried = map[string]bool{}
	}
	again := !s.retried[ev.Event.ID]
	s.retried[ev.Event.ID] = true
	s.mu.Unlock()
	if again {
		time.AfterFunc(usesRetry, func() { s.offerUse(ev) })
	}
}

// principalOf finds the principal of the only note of the agent with the
// name and the hash, or "".
func (s *Service) principalOf(ctx context.Context, agent, name, hash string) string {
	if hash == "" {
		return ""
	}
	notes, err := s.noteList(ctx, agent)
	if err != nil {
		return ""
	}
	found := ""
	for _, n := range notes {
		if n.Scope.Kind == ScopePrincipal && n.Name == name && n.Hash == hash {
			if found != "" && found != n.Scope.Principal {
				return ""
			}
			found = n.Scope.Principal
		}
	}
	return found
}

// use counts one use of a note by the event id at the time at, once. A
// note that rests goes back to its state.
func (s *Service) use(ctx context.Context, agent string, sc Scope, name, id string, at time.Time) error {
	if _, err := s.b.Get(ctx, noteKey(agent, sc, name)); err != nil {
		// A note that the front does not keep has no uses to count.
		return nil
	}
	wrote, _, _, err := s.modify(ctx, usesKey(agent, sc, name), func(value []byte, exists bool, _ uint64) ([]byte, error) {
		var u usesEntry
		if exists && json.Unmarshal(value, &u) != nil {
			u = usesEntry{}
		}
		if slices.Contains(u.Seen, id) {
			return nil, nil
		}
		u.Uses++
		if at.After(u.LastUsed) {
			u.LastUsed = at
		}
		u.Seen = append(u.Seen, id)
		if len(u.Seen) > usesSeen {
			u.Seen = slices.Clone(u.Seen[len(u.Seen)-usesSeen:])
		}
		return encode(u)
	})
	if err != nil || !wrote {
		return err
	}
	woke, err := s.setStateAny(ctx, noteKey(agent, sc, name), func(n *Note) bool {
		if n.State != StateResting {
			return false
		}
		n.State = stateOf(*n)
		return true
	})
	if woke {
		emitChange(agent)
	}
	return err
}
