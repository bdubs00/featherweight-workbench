package routines

// This file is the projection of the evidence (docs/ARCHITECTURE.md
// section 21.4). It hangs on the egress stream that the fleet reads
// already (fleet.Tap): the view hands it each event of a routine and each
// annotation of a run, and a goroutine of its own applies them to the
// evidence in the store, one write per routine for the events that wait.
// No second subscription, and no scan of a ledger.

import (
	"context"
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
}

func newProjector(s *Store, log *slog.Logger) *projector {
	return &projector{s: s, log: log, in: make(chan fleet.RunEvent, projectQueue), touched: map[string][]string{}}
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
	for _, ev := range batch {
		if ev.Agent == "" || CheckAgent(ev.Agent) != nil {
			continue
		}
		if ev.Event.Type == fw.EvRunAnnotated {
			if verificationOf(ev.Event) == "" {
				continue
			}
			for _, key := range p.routinesOfRun(ctx, ev) {
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
// of a receipt. A run that this process did not see decide, such as one
// from before its start, gives every routine of its agent, whose evidence
// changes only when it holds a mark of the run.
func (p *projector) routinesOfRun(ctx context.Context, ev fleet.RunEvent) []string {
	p.mu.Lock()
	keys, ok := p.touched[ev.Event.Subject]
	p.mu.Unlock()
	if ok {
		return slices.Clone(keys)
	}
	list, err := p.s.List(ctx, ev.Agent)
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(list))
	for _, rec := range list {
		out = append(out, keyOf(rec.Agent, rec.Name()))
	}
	return out
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
	version, sh := rec.Version(), shapeOf(rec.Routine)
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
				changed = ev.verified(e.Subject, verificationOf(e)) || changed
				continue
			}
			if _, v, _ := routineOf(e); v != version {
				continue
			}
			switch e.Type {
			case fw.EvRoutineDecided:
				if d, ok := decisionOf(e, g.name); ok {
					changed = ev.decided(e.Subject, e.Time, d) || changed
				}
			case fw.EvRoutineApplied:
				if ok, read := appliedOK(e); read {
					changed = ev.applied(e.Subject, e.Time, ok) || changed
				}
			case fw.EvRoutineCompared:
				if c, ok := comparisonOf(e, g.name); ok {
					changed = ev.compared(e.Subject, re.Input, c, sh) || changed
				}
			}
		}
		return changed
	})
	if err != nil || ev == nil {
		return err
	}
	ready := rec.Standing == Rehearsing && ev.Ready(p.s.cfg)
	if ready != rec.Ready {
		if err := p.s.setReady(ctx, g.agent, g.name, version, ready); err != nil {
			return err
		}
	}
	emitChange(g.agent, g.name)
	return nil
}

// shapeOf returns what the evidence needs of the body of a routine.
func shapeOf(r fw.Routine) shape {
	sh := shape{act: map[string]float64{}}
	for _, name := range r.QuestionNames() {
		q := r.Questions[name]
		sh.act[name] = q.Act
		if q.Observe != nil {
			sh.observed = append(sh.observed, name)
		}
	}
	return sh
}
