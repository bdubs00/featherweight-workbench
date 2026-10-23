package fleet

// This file brings the routines of the agents to the runs
// (docs/ARCHITECTURE.md section 21.3). The package routines keeps them in a
// store of its own and sets Routines here. Each pass of the pools reads
// them once and merges them into the overlay of each pool (mergedPool),
// so every Task of a pool runs the routines that the store holds, and the
// hash of the pool covers them. The other readers of a pool, such as
// agent_put, the views, and the approval rules, take the routines of the
// last pass, so a store that does not answer for a moment breaks none of
// them. A run request that the front puts on a queue carries the routines
// in its inline profile too (Enqueue), so a Task that the pool did not
// replace yet runs them as well.

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/pools"
)

// Routines reads the routines that reach the profiles, by agent, each with
// the standing of the kernel. An agent with an entry, also an empty one,
// runs those routines in place of the routines of its profile. a is the
// agent of the front; the dry run passes nil and the configuration cfg,
// which it reads once. An error means that the store does not answer, or
// that it is not open yet, and a pass of the pools then waits. The package
// routines sets it in init, and nil means no routines. A test replaces it.
var Routines func(ctx context.Context, a *fw.Agent, cfg *fw.Config) (map[string][]fw.Routine, error)

// routineCache keeps the routines of the last pass that read them.
type routineCache struct {
	mu      sync.Mutex
	m       map[string][]fw.Routine
	read    bool // a pass or a reader read them once
	failing bool // the last read failed, and the log said so
}

// readRoutines reads the routines from the store and keeps them for the
// readers between two passes. A pass of the pools reads them this way.
func (f *Fleet) readRoutines(ctx context.Context) (map[string][]fw.Routine, error) {
	if Routines == nil {
		return nil, nil
	}
	m, err := Routines(ctx, f.a, f.a.Config())
	c := &f.routines
	c.mu.Lock()
	defer c.mu.Unlock()
	if err != nil {
		if !c.failing {
			c.failing = true
			f.log.Warn("fleet: read the routines; the pools wait, and the other readers take the routines of the last pass", "error", err)
		}
		return nil, fmt.Errorf("fleet: read the routines: %w", err)
	}
	if c.failing {
		c.failing = false
		f.log.Info("fleet: the routines read again")
	}
	c.m, c.read = m, true
	return m, nil
}

// lastRoutines returns the routines of the last pass, or reads them when
// no pass did yet. A read that fails gives none.
func (f *Fleet) lastRoutines(ctx context.Context) map[string][]fw.Routine {
	c := &f.routines
	c.mu.Lock()
	m, read := c.m, c.read
	c.mu.Unlock()
	if read {
		return m
	}
	m, _ = f.readRoutines(ctx)
	return m
}

// CheckRoutines checks that the pools of the fleet of a that run the
// profile agent accept the routines list for it: the merge of the overlay
// of each such pool with the list, as the next pass merges it, must fit.
// It returns an error that wraps fw.ErrInvalidRequest and names the pool
// when one does not. It reports false when no pool runs the agent, so the
// caller checks the routines without the configuration of the workers.
func CheckRoutines(ctx context.Context, a *fw.Agent, agent string, list []fw.Routine) (bool, error) {
	f := fleetOf(a)
	if f == nil {
		return false, nil
	}
	pooled := false
	for _, base := range f.poolsNow(ctx) {
		merged, err := f.mergedPool(ctx, base)
		if err != nil {
			continue // the pool waits already, and says why
		}
		if _, runs := merged.Profiles()[agent]; !runs {
			continue
		}
		pooled = true
		o := merged.Overlay()
		next := make(map[string][]fw.Routine, len(o.Routines)+1)
		for k, v := range o.Routines {
			next[k] = v
		}
		next[agent] = list
		o.Routines = next
		if _, err := base.WithOverlay(o); err != nil {
			return true, fmt.Errorf("%w: the pool %s does not accept the routines of %s: %s", fw.ErrInvalidRequest, base.Name, agent, strings.TrimPrefix(err.Error(), "pools: "))
		}
	}
	return pooled, nil
}

// withRoutines returns the overlay with the routines.
func withRoutines(o pools.Overlay, routines map[string][]fw.Routine) pools.Overlay {
	if routines != nil {
		o.Routines = routines
	}
	return o
}

// dropRoutines removes the routines that a caller put in the inline
// profile of a request.
func dropRoutines(req *fw.RunRequest) {
	if req.Profile == nil || req.Profile.Routines == nil {
		return
	}
	p := *req.Profile
	p.Routines = nil
	req.Profile = &p
}

// inlineRoutines gives a run request of the agent the routines that the
// store holds for it, in its inline profile, when the store holds any
// record of the agent. The request keeps its inline profile otherwise.
func inlineRoutines(req *fw.RunRequest, routines map[string][]fw.Routine, agent string) {
	list, ok := routines[agent]
	if !ok {
		return
	}
	p := fw.AgentProfile{}
	if req.Profile != nil {
		p = *req.Profile
	}
	p.Routines = append([]fw.Routine{}, list...)
	req.Profile = &p
}

// enqueueRoutines reads the routines for a request that the front puts on
// a queue. A store that does not answer gives none: the run then falls to
// the agent, the safe direction, and the log says so once until a read
// works again.
func enqueueRoutines(ctx context.Context, a *fw.Agent) map[string][]fw.Routine {
	if f, ok := Of(a).(*Fleet); ok && f != nil {
		m, err := f.readRoutines(ctx)
		if err != nil {
			return nil
		}
		return m
	}
	if Routines == nil {
		return nil
	}
	m, err := Routines(ctx, a, a.Config())
	if err != nil {
		a.Logger().Warn("fleet: read the routines; the request goes without them", "error", err)
		return nil
	}
	return m
}
