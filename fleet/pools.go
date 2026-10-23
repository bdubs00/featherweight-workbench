package fleet

// This file runs the pools of the front (docs/ARCHITECTURE.md section 14).
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
// Load filled (pools.Load).
func LoadPools(cfg *fw.Config, fc Config) ([]*pools.Pool, error) {
	list, _, err := LoadAll(cfg, fc)
	return list, err
}

// LoadAll reads the pools and the worker templates of the configuration
// for the fleet fc, which Load filled (pools.LoadAll). The front gives the
// pools its atespaces, its egress stream, its intake prefix, its bus, its
// ax API, and the variable of its static token.
func LoadAll(cfg *fw.Config, fc Config) ([]*pools.Pool, []*pools.Template, error) {
	if cfg == nil {
		return nil, nil, nil
	}
	tokenEnv := strings.TrimSpace(cfg.Auth.TokenEnv)
	if tokenEnv == "" {
		tokenEnv = pools.DefaultTokenEnv
	}
	return pools.LoadAll(cfg, pools.Front{
		Atespaces:   fc.Atespaces,
		Egress:      fc.Egress,
		Intake:      fc.Intake,
		EgressTypes: EgressTypes,
		MaxBytes:    EgressMaxBytes,
		Bus:         cfg.Bus,
		AX:          cfg.AX,
		TokenEnv:    tokenEnv,
		SkillDirs:   cfg.SkillDirs(),
	})
}

// startPools makes the reconciler of the pools. The caller holds no lock
// and the loop does not run yet.
func (p *platform) startPools(list []*pools.Pool, vals *pools.Values) {
	if (len(list) == 0 && len(p.f.templates) == 0) || p.client == nil {
		return
	}
	p.pools = list
	p.reconciler = &pools.Reconciler{AX: p.client, Check: poolCheck{p}, Values: vals, Log: p.f.log}
	p.poolState = map[string]pools.Status{}
	p.merged = map[string]*pools.Pool{}
	p.rt = map[string]*runtimePool{}
	var names []string
	for _, pool := range list {
		names = append(names, pool.Name)
		p.poolState[pool.Name] = pools.Initial(pool, "the front has not read the Tasks of the atespace yet")
		for _, w := range pool.Warnings() {
			p.f.log.Warn("fleet: a pool may not reach a host", "pool", pool.Name, "warning", w)
		}
	}
	for _, t := range p.f.templates {
		for _, w := range pools.Probes([]*pools.Template{t})[0].Warnings() {
			p.f.log.Warn("fleet: no egress rule of a worker template allows a host", "template", t.Name, "warning", w)
		}
	}
	p.f.log.Info("fleet keeps the pools", "pools", strings.Join(names, ","), "templates", pools.TemplateNames(p.f.templates))
}

// reconcilePools runs one pass of every pool over the Tasks of its
// atespace: the pools of the file, then the runtime pools of the store
// (runtime.go). A pool whose atespace the front could not list keeps its
// last state, with the reason. A runtime pool whose name a pool of the
// file holds is left alone, and the state of the pool of the file says
// so: the file wins.
func (p *platform) reconcilePools(ctx context.Context, listed map[string][]*v1alpha1.Task, failed map[string]error, now time.Time) {
	if p.reconciler == nil {
		return
	}
	entries, rerr := p.f.runtimeList(ctx)
	// A pass without the routines takes them away from every Task, so a
	// store of the routines that does not answer, or that is not open
	// yet, holds every pool, as an overlay does.
	routines, rterr := p.f.readRoutines(ctx)
	p.passRoutines, p.passRoutinesErr = routines, rterr
	shadow := map[string]pools.Stored{}
	for _, e := range entries {
		shadow[e.Name] = e
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
		} else if rterr != nil {
			wait(fmt.Sprintf("the pool waits: %v", rterr))
		} else if merged, err := p.f.mergedPoolWith(ctx, pool, routines); err != nil {
			// A pass without the overlay takes the profiles and the
			// rules of the overlay away from every Task, so the pool waits.
			wait(fmt.Sprintf("the pool waits: %v", err))
		} else {
			st = p.reconciler.Reconcile(ctx, merged, listed[pool.Atespace], now)
			p.mu.Lock()
			p.merged[pool.Name] = merged
			p.mu.Unlock()
		}
		if e, ok := shadow[pool.Name]; ok {
			st.Gaps = append(st.Gaps, fmt.Sprintf("the store of the runtime pools holds a pool of this name too, which %s created; the file wins, so the front runs this pool and leaves that entry alone; remove the entry with pool_delete or DELETE /v1/pools/%s", orWho(e.By), pool.Name))
		}
		p.mu.Lock()
		prev := p.poolState[pool.Name]
		p.poolState[pool.Name] = st
		p.mu.Unlock()
		// The log names each new reason once, not at every interval.
		p.logGaps(pool.Name, prev, st)
	}
	if p.f.runtime == nil {
		return
	}
	if rerr != nil {
		// Without the store, a runtime pool keeps its last state and its
		// Tasks, with the reason.
		why := p.reconciler.Values.Redact(fmt.Sprintf("the front could not read the runtime pools %s, so the pool waits: %v", p.f.runtime.Describe(), rerr))
		p.mu.Lock()
		var changed []string
		for name := range p.rt {
			st := p.poolState[name]
			if !slices.Equal(st.Gaps, []string{why}) {
				changed = append(changed, name)
			}
			st.Gaps = []string{why}
			p.poolState[name] = st
		}
		p.mu.Unlock()
		if len(changed) > 0 {
			p.f.log.Warn("fleet: the runtime pools wait", "pools", strings.Join(changed, ","), "error", rerr)
		}
		return
	}
	p.reconcileRuntime(ctx, entries, listed, failed, now)
}

// orWho names a person, or says that nobody is known.
func orWho(by string) string {
	if strings.TrimSpace(by) == "" {
		return "an unknown person"
	}
	return by
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

// Pools implements PoolView: the pools of the file in their order, then
// the runtime pools by name, each with its state. A caller sees a pool
// when the caller can view its agent, decided as for the agent of a member
// (allow), with the access block of the profile of this process, else the
// one of the workers of the pool. The person who created a runtime pool
// sees it too.
func (f *Fleet) Pools(ctx context.Context) ([]pools.Status, error) {
	p := f.platform
	if p == nil || !f.keepsPools() {
		return nil, nil
	}
	type item struct {
		st     pools.Status
		access *fw.Access
		by     string
	}
	var items []item
	p.mu.Lock()
	for _, pool := range p.pools {
		cur := pool
		if m := p.merged[pool.Name]; m != nil {
			cur = m
		}
		items = append(items, item{st: p.poolState[pool.Name], access: cur.AgentAccess()})
	}
	p.mu.Unlock()
	entries, err := f.runtimeList(ctx)
	if err != nil {
		f.log.Warn("fleet: the runtime pools come from the last pass", "error", err)
		entries = nil
		p.mu.Lock()
		for _, rt := range p.rt {
			entries = append(entries, rt.stored)
		}
		p.mu.Unlock()
		sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	}
	files := f.fileNames()
	for _, e := range entries {
		if files[e.Name] {
			continue
		}
		st := f.runtimeState(ctx, e, false)
		var access *fw.Access
		p.mu.Lock()
		if m := p.merged[e.Name]; m != nil {
			access = m.AgentAccess()
		}
		p.mu.Unlock()
		items = append(items, item{st: st, access: access, by: e.By})
	}
	cfg := f.a.Config()
	caller := fw.PrincipalFrom(ctx)
	out := []pools.Status{}
	for _, it := range items {
		if it.by != "" && caller != nil && caller.Subject == it.by {
			out = append(out, it.st)
			continue
		}
		access := it.access
		if prof, ok := f.a.ProfileNamed(it.st.Agent); ok {
			access = prof.Access
		}
		rec := &Record{
			Name:   it.st.Atespace + "/" + it.st.Name,
			Access: Access{Mode: f.a.AccessMode(), Default: cfg.Access.Default, Admins: cfg.Access.Admins},
			Agents: []RecordAgent{{Name: it.st.Agent, Access: access}},
		}
		_, roles, err := f.allow(ctx, rec, nil, true)
		if err != nil {
			return nil, err
		}
		if roles[0] != "" {
			out = append(out, it.st)
		}
	}
	return out, nil
}
