package fleet

// This file brings the routines of the agents to the runs
// (docs/ARCHITECTURE.md section 21.3). The package routines keeps them in a
// store of its own and sets the source here. A pass of a pool merges them
// into the overlay of the pool (mergedPool), so every Task of the pool
// runs the routines that the store holds, and the hash of the pool covers
// them. A run request that the front puts on a queue carries them in its
// inline profile too (Enqueue), so a Task that the pool did not replace
// yet runs them as well.

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/pools"
)

// RoutineSource returns the routines of the agents of a process that
// reach a profile, by agent, each with the standing of the kernel. An
// agent with an entry, also an empty one, runs those routines in place of
// the routines of its profile. An error means that the store does not
// answer, and a pass of a pool then waits, as it waits for its overlay.
type RoutineSource func(ctx context.Context, a *fw.Agent) (map[string][]fw.Routine, error)

// RoutineOpener reads the routines of a configuration once, for the dry
// run of the pools, which runs without an agent.
type RoutineOpener func(ctx context.Context, cfg *fw.Config) (map[string][]fw.Routine, error)

var routineHooks struct {
	mu   sync.Mutex
	src  RoutineSource
	open RoutineOpener
}

// UseRoutines sets the source of the routines and the reader of the dry
// run. The package routines calls it in init.
func UseRoutines(src RoutineSource, open RoutineOpener) {
	routineHooks.mu.Lock()
	defer routineHooks.mu.Unlock()
	routineHooks.src, routineHooks.open = src, open
}

// routinesOf returns the routines of the agents of a, or nil without a
// source.
func routinesOf(ctx context.Context, a *fw.Agent) (map[string][]fw.Routine, error) {
	routineHooks.mu.Lock()
	src := routineHooks.src
	routineHooks.mu.Unlock()
	if src == nil || a == nil {
		return nil, nil
	}
	m, err := src(ctx, a)
	if err != nil {
		return nil, fmt.Errorf("fleet: read the routines: %w", err)
	}
	return m, nil
}

// OpenRoutines reads the routines of a configuration for the dry run, with
// the reader that UseRoutines set, or none without one. A test replaces
// it.
var OpenRoutines = func(ctx context.Context, cfg *fw.Config) (map[string][]fw.Routine, error) {
	routineHooks.mu.Lock()
	open := routineHooks.open
	routineHooks.mu.Unlock()
	if open == nil {
		return nil, nil
	}
	m, err := open(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("fleet: read the routines: %w", err)
	}
	return m, nil
}

// CheckRoutines checks that the pools of the fleet of a that run the
// profile agent accept the routines list for it: the merge of the overlay
// of each such pool with the list, as the next pass merges it, must fit.
// It returns an error that wraps fw.ErrInvalidRequest and names the pool
// when one does not. A process without pools checks the profile of the
// agent with the routines, as the kernel checks a profile.
func CheckRoutines(ctx context.Context, a *fw.Agent, agent string, list []fw.Routine) error {
	f := fleetOf(a)
	if f == nil {
		if p, ok := a.ProfileNamed(agent); ok {
			p.Routines = list
			if err := a.CheckProfile(p); err != nil {
				return fmt.Errorf("%w: %s", fw.ErrInvalidRequest, strings.TrimPrefix(err.Error(), "fw: "))
			}
		}
		return nil
	}
	for _, base := range f.poolsNow(ctx) {
		merged, err := f.mergedPool(ctx, base)
		if err != nil {
			continue // the pool waits already, and says why
		}
		if _, runs := merged.Profiles()[agent]; !runs {
			continue
		}
		o := merged.Overlay()
		next := make(map[string][]fw.Routine, len(o.Routines)+1)
		for k, v := range o.Routines {
			next[k] = v
		}
		next[agent] = list
		o.Routines = next
		if _, err := base.WithOverlay(o); err != nil {
			return fmt.Errorf("%w: the pool %s does not accept the routines of %s: %s", fw.ErrInvalidRequest, base.Name, agent, strings.TrimPrefix(err.Error(), "pools: "))
		}
	}
	return nil
}

// withRoutines returns the overlay with the routines of the source.
func withRoutines(o pools.Overlay, routines map[string][]fw.Routine) pools.Overlay {
	if routines != nil {
		o.Routines = routines
	}
	return o
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
