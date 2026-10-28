package routines

// This file is the projection of the evidence (docs/ARCHITECTURE.md
// section 21.4). It hangs on the egress stream that the fleet reads
// already (fleet.Tap): the view hands it each event of a routine and each
// annotation of a run, and a goroutine of its own applies them to the
// evidence in the store, one write per routine for the events that wait.
// No second subscription, and no scan of a ledger.

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/fleet"
)

// Bounds of the projection.
const (
	// projectQueue is how many events wait for the projection. Past it,
	// the projection drops an event, and the log says so once until the
	// queue has room again.
	projectQueue = 4096
	// projectBatch is how many waiting events one round applies.
	projectBatch = 256
	// projectTimeout bounds the writes of one routine in a round.
	projectTimeout = 10 * time.Second
	// maxTouched is how many runs the projection remembers with the
	// routines that decided in them, for the receipts that come later.
	maxTouched = 16384
	// maxWaiting is how many runs the projection holds events of while it
	// does not know their agent yet, and maxWaitingEvents how many events
	// of one run it holds.
	maxWaiting       = 1024
	maxWaitingEvents = 64
)

// projector applies the events of the stream to the evidence of a store.
type projector struct {
	s   *Store
	log *slog.Logger
	in  chan fleet.RunEvent

	mu      sync.Mutex
	full    bool
	touched map[string][]string // run id to the keys of the routines decided in it
	order   []string            // the run ids of touched, oldest first

	// waiting holds the events of the runs whose agent the view did not
	// know yet, because the bus brought them before the start of the run.
	// The goroutine of the projection alone uses it.
	waiting      map[string][]fleet.RunEvent
	waitingOrder []string
}

func newProjector(s *Store, log *slog.Logger) *projector {
	return &projector{s: s, log: log, in: make(chan fleet.RunEvent, projectQueue), touched: map[string][]string{}, waiting: map[string][]fleet.RunEvent{}}
}

// known returns the events of a round with an agent: an event of a run
// whose agent is not known yet waits, and the first event of the run with
// its agent brings the waiting events along, before itself. The view
// knows the agent from the start of the run, which the tap carries too.
func (p *projector) known(batch []fleet.RunEvent) []fleet.RunEvent {
	var out []fleet.RunEvent
	for _, ev := range batch {
		run := ev.Event.Subject
		if ev.Agent == "" {
			if _, ok := p.waiting[run]; !ok {
				p.waitingOrder = append(p.waitingOrder, run)
				if len(p.waitingOrder) > maxWaiting {
					delete(p.waiting, p.waitingOrder[0])
					p.waitingOrder = slices.Clone(p.waitingOrder[1:])
				}
			}
			if len(p.waiting[run]) < maxWaitingEvents {
				p.waiting[run] = append(p.waiting[run], ev)
			}
			continue
		}
		if held, ok := p.waiting[run]; ok {
			delete(p.waiting, run)
			p.waitingOrder = slices.DeleteFunc(p.waitingOrder, func(id string) bool { return id == run })
			for _, h := range held {
				h.Agent = ev.Agent
				if h.Line == "" {
					h.Line = ev.Line
				}
				out = append(out, h)
			}
		}
		if ev.Event.Type != fw.EvRunQueued {
			out = append(out, ev)
		}
	}
	return out
}

// tapped is the function of fleet.Tap: it hands the event to the
// projection of the agent, without blocking the view.
func tapped(a *fw.Agent, ev fleet.RunEvent) {
	if s := Of(a); s != nil && s.proj != nil {
		s.proj.offer(ev)
	}
}

// offer queues an event, or drops it when the queue is full.
func (p *projector) offer(ev fleet.RunEvent) {
	select {
	case p.in <- ev:
		p.mu.Lock()
		if p.full {
			p.full = false
			p.log.Info("routines: the projection of the evidence takes events again")
		}
		p.mu.Unlock()
	default:
		p.mu.Lock()
		if !p.full {
			p.full = true
			p.log.Warn("routines: the projection of the evidence is behind and drops events; the evidence misses them", "queue", projectQueue)
		}
		p.mu.Unlock()
	}
}

// run applies the events until ctx ends.
func (p *projector) run(ctx context.Context) {
	for {
		var batch []fleet.RunEvent
		select {
		case <-ctx.Done():
			return
		case ev := <-p.in:
			batch = append(batch, ev)
		}
	more:
		for len(batch) < projectBatch {
			select {
			case ev := <-p.in:
				batch = append(batch, ev)
			default:
				break more
			}
		}
		p.apply(ctx, batch)
	}
}

// group is the events of one routine in a round, in order.
type group struct {
	agent, name string
	events      []fleet.RunEvent
}

// apply applies one round of events: it groups them by routine and writes
// the evidence of each routine once.
func (p *projector) apply(ctx context.Context, batch []fleet.RunEvent) {
	var groups []*group
	byKey := map[string]*group{}
	add := func(agent, name string, ev fleet.RunEvent) {
		key := keyOf(agent, name)
		g := byKey[key]
		if g == nil {
			g = &group{agent: agent, name: name}
			byKey[key] = g
			groups = append(groups, g)
		}
		g.events = append(g.events, ev)
	}
	for _, ev := range p.known(batch) {
		if CheckAgent(ev.Agent) != nil {
			continue
		}
		if ev.Event.Type == fw.EvRunAnnotated {
			if fleet.Verification(ev.Event) == "" {
				continue
			}
			for _, key := range p.routinesOfRun(ev.Event.Subject) {
				agent, name, _ := splitKey(key)
				add(agent, name, ev)
			}
			continue
		}
		name, _, ok := routineOf(ev.Event)
		if !ok || CheckName(name) != nil {
			continue
		}
		if ev.Event.Type == fw.EvRoutineDecided {
			p.touch(ev.Event.Subject, keyOf(ev.Agent, name))
		}
		add(ev.Agent, name, ev)
	}
	for _, g := range groups {
		cctx, cancel := context.WithTimeout(ctx, projectTimeout)
		if err := p.applyGroup(cctx, g); err != nil {
			p.log.Warn("routines: the evidence of a routine did not take its events", "agent", g.agent, "routine", g.name, "events", len(g.events), "error", err)
		}
		cancel()
	}
}

// touch remembers that a routine decided in a run.
func (p *projector) touch(run, key string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	keys, ok := p.touched[run]
	if !ok {
		p.order = append(p.order, run)
		if len(p.order) > maxTouched {
			delete(p.touched, p.order[0])
			p.order = slices.Clone(p.order[1:])
		}
	}
	if !slices.Contains(keys, key) {
		p.touched[run] = append(keys, key)
	}
}

// routinesOfRun returns the keys of the routines that decided in the run
// of a quality observation. A run that this process did not see decide,
// such as one from before its start, gives none, so an observation of a
// run of no routine costs no read of the store. Another replica that saw
// the decision counts it.
func (p *projector) routinesOfRun(run string) []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.touched[run])
}

// applyGroup applies the events of one routine to its evidence, as one
// write, and updates the ready flag of the routine when it changes. An
// event of another version than the version of the record counts nothing,
// and so does an event of a routine that the store does not hold.
func (p *projector) applyGroup(ctx context.Context, g *group) error {
	rec, _, err := p.s.get(ctx, g.agent, g.name)
	if err != nil {
		return nil // a routine that the store does not hold, such as one of the configuration of a pool
	}
	version, body := rec.Version(), rec.Routine
	ev, err := p.s.updateEvidence(ctx, g.agent, g.name, func(ev *Evidence) bool {
		changed := false
		switch {
		case ev.Version > version:
			// A newer version started after the read of the record, so
			// these events belong to an older one.
			return false
		case ev.Version < version:
			*ev = *newEvidence(version)
			changed = true
		}
		for _, re := range g.events {
			e := re.Event
			if e.Type == fw.EvRunAnnotated {
				changed = ev.verified(e.Subject, fleet.Verification(e)) || changed
				continue
			}
			if _, v, _ := routineOf(e); v != version {
				continue
			}
			switch e.Type {
			case fw.EvRoutineDecided:
				var d fw.RoutineDecidedData
				if e.Decode(&d) == nil {
					changed = ev.decided(e.Subject, e.Time, d, body) || changed
				}
			case fw.EvRoutineApplied:
				var d fw.RoutineAppliedData
				if e.Decode(&d) == nil {
					changed = ev.applied(e.Subject, e.Time, d) || changed
				}
			case fw.EvRoutineCompared:
				var d fw.RoutineComparedData
				if e.Decode(&d) == nil && d.Question != "" {
					changed = ev.compared(e.Subject, re.Line, e.Time, d, body) || changed
				}
			}
		}
		if ev.stepBack != nil && rec.Standing == Live {
			// The ready rule counts the runs after the step back, so the
			// routine rehearses again before it is ready.
			ev.stepBackAt(e0(g))
		}
		return changed
	})
	if err != nil || ev == nil {
		return err
	}
	if sb := ev.stepBack; sb != nil && rec.Standing == Live {
		note := fmt.Sprintf("It stepped back to rehearsing by itself: %s, run %s", sb.why, sb.run)
		if _, _, err := p.s.autoMove(ctx, g.agent, g.name, version, Live, Rehearsing, note); err != nil {
			return err
		}
		return nil
	}
	ready := rec.Standing == Rehearsing && ev.Ready(p.s.cfg)
	if ready != rec.Ready {
		if err := p.s.setReady(ctx, g.agent, g.name, version, ready); err != nil {
			return err
		}
	}
	emitChange(g.agent, g.name)
	if ready {
		return p.s.grow(ctx, rec, ev)
	}
	return nil
}

// e0 is the time of the newest event of a group, or now for a group
// without one.
func e0(g *group) time.Time {
	var at time.Time
	for _, re := range g.events {
		if re.Event.Time.After(at) {
			at = re.Event.Time
		}
	}
	if at.IsZero() {
		at = time.Now()
	}
	return at
}

// grow moves a ready rehearsing routine to live by itself, as Know-how
// allows (docs/KNOWHOW.md section 6.5): a version that a person signed off
// and that stepped back goes live again, and a routine that only reads
// goes live unless the agent asks first. Any other ready routine waits for
// a person, as ready on the page.
func (s *Store) grow(ctx context.Context, rec Record, ev *Evidence) error {
	version := rec.Version()
	var note string
	switch {
	case rec.SteppedBack && rec.SignedOff.holds(version):
		note = fmt.Sprintf("It is ready again after it stepped back, and %s signed off version %d, so it went live again by itself", rec.SignedOff.By, version)
	case ev.Risk == fw.RiskRead && !askFirst(ctx, rec.Agent):
		t, _ := ev.current()
		note = fmt.Sprintf("It only reads and is ready: it would have acted on %d runs, and %d agreed, so it went live by itself", t.Acted, t.Agreed)
	default:
		return nil
	}
	_, _, err := s.autoMove(ctx, rec.Agent, rec.Name(), version, Rehearsing, Live, note)
	return err
}

// routineOf returns the routine and the version that an event of a
// routine names.
func routineOf(e fw.Event) (string, int, bool) {
	var d struct {
		Routine string `json:"routine"`
		Version int    `json:"version"`
	}
	if e.Decode(&d) != nil || d.Routine == "" {
		return "", 0, false
	}
	return d.Routine, d.Version, true
}
