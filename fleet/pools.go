package fleet

// This file runs the pools of the front (docs/ARCHITECTURE.md section 13).
// A pool keeps a number of ax Tasks that run one agent profile. At every
// interval, after the Task list, the front reconciles each pool with the
// package pools: it creates the missing Tasks, replaces the Tasks of
// an older configuration, and deletes the Tasks above the replica count,
// never a Task with an active run. The view tells the reconciler which
// runs of a Task are active, and the status of the Task through the
// router confirms it. The state of each pool goes to the fleet surfaces.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/surface/view"
	"github.com/bdubs00/featherweight-workbench/pools"
	v1alpha1 "github.com/google/ax/pkg/apis/v1alpha1"
)

// The fleet keeps the pools of its configuration.
var _ PoolView = (*Fleet)(nil)

// LoadPools reads the pools of the configuration for the fleet fc, which
// Load filled (pools.Load). The front gives the pools its atespaces, its
// egress stream, its intake prefix, its bus, its ax API, and the variable
// of its static token.
func LoadPools(cfg *fw.Config, fc Config) ([]*pools.Pool, error) {
	if cfg == nil {
		return nil, nil
	}
	tokenEnv := strings.TrimSpace(cfg.Auth.TokenEnv)
	if tokenEnv == "" {
		tokenEnv = pools.DefaultTokenEnv
	}
	return pools.Load(cfg, pools.Front{
		Atespaces:   fc.Atespaces,
		Egress:      fc.Egress,
		Intake:      fc.Intake,
		EgressTypes: EgressTypes,
		Bus:         cfg.Bus,
		AX:          cfg.AX,
		TokenEnv:    tokenEnv,
	})
}

// startPools makes the reconciler of the pools. The caller holds no lock
// and the loop does not run yet.
func (p *platform) startPools(list []*pools.Pool, vals *pools.Values) {
	if len(list) == 0 || p.client == nil {
		return
	}
	p.pools = list
	p.reconciler = &pools.Reconciler{AX: p.client, Check: poolCheck{p}, Values: vals, Log: p.f.log}
	p.poolState = map[string]pools.Status{}
	p.merged = map[string]*pools.Pool{}
	var names []string
	for _, pool := range list {
		names = append(names, pool.Name)
		p.poolState[pool.Name] = pools.Initial(pool, "the front has not read the Tasks of the atespace yet")
		for _, w := range pool.Warnings() {
			p.f.log.Warn("fleet: a pool may not reach a host", "pool", pool.Name, "warning", w)
		}
	}
	p.f.log.Info("fleet keeps the pools", "pools", strings.Join(names, ","))
}

// reconcilePools runs one pass of every pool over the Tasks of its
// atespace. A pool whose atespace the front could not list keeps its last
// state, with the reason.
func (p *platform) reconcilePools(ctx context.Context, listed map[string][]*v1alpha1.Task, failed map[string]error, now time.Time) {
	if p.reconciler == nil {
		return
	}
	for _, pool := range p.pools {
		var st pools.Status
		wait := func(why string) {
			p.mu.Lock()
			st = p.poolState[pool.Name]
			p.mu.Unlock()
			st.Gaps = []string{p.reconciler.Values.Redact(why)}
		}
		if err := failed[pool.Atespace]; err != nil {
			wait(fmt.Sprintf("the front could not list the Tasks of the atespace %s, so the pool waits: %v", pool.Atespace, err))
		} else if merged, err := p.f.mergedPool(ctx, pool); err != nil {
			// A pass without the overlay would take the profiles and the
			// rules of the overlay away from every Task, so the pool waits.
			wait(fmt.Sprintf("the pool waits: %v", err))
		} else {
			st = p.reconciler.Reconcile(ctx, merged, listed[pool.Atespace], now)
			p.mu.Lock()
			p.merged[pool.Name] = merged
			p.mu.Unlock()
		}
		p.mu.Lock()
		prev := p.poolState[pool.Name]
		p.poolState[pool.Name] = st
		p.mu.Unlock()
		// The log names each new reason once, not at every interval.
		for _, g := range st.Gaps {
			if !slices.Contains(prev.Gaps, g) {
				p.f.log.Info("fleet: a pool differs from its configuration", "pool", pool.Name, "wanted", st.Wanted, "ready", st.Ready, "reason", g)
			}
		}
	}
}

// poolCheck answers the reconciler from the view and the router.
type poolCheck struct{ p *platform }

// childWait is how long a child that a member placed in another Task, and
// that sent no event yet, counts as work of the member. The spawner gives
// a child Task some minutes to run and its run some minutes to start, so a
// child that stays silent longer failed to start.
const childWait = 30 * time.Minute

// Active implements pools.Checker: the runs of the member that the view
// holds and that are not finished, and the children that it placed in
// another Task a short time ago whose events have not arrived yet, because
// the member waits for them. A child that the view dropped is no such
// child.
func (c poolCheck) Active(member string) []string {
	p := c.p
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []string
	for id, r := range p.runs {
		if r.member == member && !r.run.State.Terminal() {
			out = append(out, id)
		}
	}
	now := time.Now()
	for id, l := range p.children {
		if _, ok := p.runs[id]; ok {
			continue
		}
		if _, gone := p.gone[id]; gone || now.Sub(l.at) > childWait {
			continue
		}
		if src := p.runs[l.parent]; src != nil && src.member == member {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

// Busy implements pools.Checker: it reads GET /v1/status of the member
// through the router with token, else with the static token of the front.
// A process that is not idle with no active run, because it sends a branch
// or follows a child, counts as one. Then it reads each run of runs, the
// runs that the view holds, with GET /v1/runs/{id}. A run that the Task
// does not have (404), or that is finished, is a run that the view still
// holds after a lost event. A run that the Task does not show to the token
// (403), or any other answer, is an error, so the Task stays.
func (c poolCheck) Busy(ctx context.Context, member, token string, runs []string) (int, error) {
	p := c.p
	p.mu.Lock()
	m := p.members[member]
	p.mu.Unlock()
	if m == nil {
		return 0, fmt.Errorf("fleet: %s is not in the Task list", member)
	}
	var st view.Status
	if err := p.readAs(ctx, m, "/v1/status", token, &st); err != nil {
		return 0, err
	}
	n := st.Runs.Active
	if n == 0 && !st.Idle {
		n = 1
	}
	active := 0
	for _, id := range runs {
		var r fw.Run
		err := p.readAs(ctx, m, "/v1/runs/"+url.PathEscape(id), token, &r)
		var ae *answerError
		switch {
		case errors.As(err, &ae) && ae.code == http.StatusNotFound:
		case err != nil:
			return 0, fmt.Errorf("fleet: read the run %s of %s: %w", id, member, err)
		case !r.State.Terminal():
			active++
		}
	}
	return max(n, active), nil
}

// Pools implements PoolView. A caller sees a pool when the caller can view
// its agent, decided as for the agent of a member (allow), with the access
// block of the profile of this process, else the one of the configuration
// of the workers.
func (f *Fleet) Pools(ctx context.Context) ([]pools.Status, error) {
	p := f.platform
	if p == nil || len(p.pools) == 0 {
		return nil, nil
	}
	p.mu.Lock()
	states := make([]pools.Status, len(p.pools))
	current := make([]*pools.Pool, len(p.pools))
	for i, pool := range p.pools {
		states[i] = p.poolState[pool.Name]
		current[i] = pool
		if m := p.merged[pool.Name]; m != nil {
			current[i] = m
		}
	}
	p.mu.Unlock()
	cfg := f.a.Config()
	out := []pools.Status{}
	for i, pool := range p.pools {
		access := current[i].AgentAccess()
		if prof, ok := f.a.ProfileNamed(pool.Agent); ok {
			access = prof.Access
		}
		rec := &Record{
			Name:   pool.Atespace + "/" + pool.Name,
			Access: Access{Mode: f.a.AccessMode(), Default: cfg.Access.Default, Admins: cfg.Access.Admins},
			Agents: []RecordAgent{{Name: pool.Agent, Access: access}},
		}
		_, roles, err := f.allow(ctx, rec, nil, true)
		if err != nil {
			return nil, err
		}
		if roles[0] != "" {
			out = append(out, states[i])
		}
	}
	return out, nil
}
