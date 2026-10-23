package fleet

// This file runs the runtime pools of the front (docs/ARCHITECTURE.md
// section 14.10): the pools that people create from a worker template
// while the front runs. Their entries live in the store of the front, the
// key-value bucket <stream>_POOLS on a NATS bus (pools.Store). At each
// pass the front reads the store, builds each pool from its template and
// its entry, merges the overlay of the pool, and reconciles it as a pool
// of the file. A runtime pool whose name a pool of the file holds is left
// alone, and the state of the pool of the file says so. Deleting a runtime
// pool sets its replicas to 0, and the pass removes the entry once its
// Tasks are gone. The functions CreatePool, PatchPool, and DeletePool
// serve the routes /v1/pools and the MCP tools, with the checks of who
// can.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/bdubs00/featherweight-kernel/ax"
	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/internal/bucket"
	"github.com/bdubs00/featherweight-workbench/pools"
	v1alpha1 "github.com/google/ax/pkg/apis/v1alpha1"
)

// Errors of the runtime pools beside those of the kernel.
var (
	// ErrPoolNotFound means that no pool has the name.
	ErrPoolNotFound = errors.New("fleet: no pool has the name")
	// ErrPoolExists means that a pool of the file or of the store has the
	// name already.
	ErrPoolExists = errors.New("fleet: a pool has the name")
	// ErrPoolConflict means that the pool cannot change now, because a
	// person deleted it or other writers changed it each time.
	ErrPoolConflict = errors.New("fleet: the pool cannot change now")
)

// DefaultWorkers is the number of workers of the runtime pool that
// agent_put and PUT /v1/agents/{name} make for a new agent when the
// request names no replicas.
const DefaultWorkers = 2

// OpenRuntime opens the store of the runtime pools on the bus of cfg: the
// bucket <stream>_POOLS of the NATS server for the bus nats, the memory of
// the process for the bus mem, and no store for another bus. With once, a
// NATS server that does not answer fails at once, for a command that runs
// once. The function that it returns closes the store. A test replaces it.
var OpenRuntime = func(ctx context.Context, cfg *fw.Config, log *slog.Logger, once bool) (*pools.Store, func(), error) {
	open := bucket.Open
	if once {
		open = bucket.OpenOnce
	}
	b, closeFn, err := open(ctx, cfg, pools.StoreBucket, log)
	if errors.Is(err, bucket.ErrNoStore) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("fleet: the store of the runtime pools: %w", err)
	}
	return pools.NewStore(b, log), closeFn, nil
}

// runtimePool is a runtime pool as the last pass read it: its entry, and
// the pool that the entry builds, nil when it does not build.
type runtimePool struct {
	stored pools.Stored
	base   *pools.Pool
}

// keepsPools reports whether the front keeps pools: pools of the file, or
// templates from which people create runtime pools.
func (f *Fleet) keepsPools() bool {
	return f.platform != nil && (len(f.platform.pools) > 0 || len(f.templates) > 0)
}

// runtimeList reads the runtime pools of the store, or none when the front
// keeps no store.
func (f *Fleet) runtimeList(ctx context.Context) ([]pools.Stored, error) {
	if f.runtime == nil {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(ctx, overlayTimeout)
	defer cancel()
	return f.runtime.List(ctx)
}

// fileNames returns the names of the pools of the file.
func (f *Fleet) fileNames() map[string]bool {
	out := map[string]bool{}
	for _, p := range f.platform.pools {
		out[p.Name] = true
	}
	return out
}

// poolsNow returns the pools of the fleet that hold profiles: the pools of
// the file, then the runtime pools of the store that build, by name,
// without a runtime pool whose name a pool of the file holds and without a
// runtime pool that a person deleted, whose agent is gone. When the store
// does not answer, the runtime pools of the last pass stand in, and the
// log says so.
func (f *Fleet) poolsNow(ctx context.Context) []*pools.Pool {
	out := slices.Clone(f.platform.pools)
	if f.runtime == nil {
		return out
	}
	entries, err := f.runtimeList(ctx)
	if err != nil {
		f.log.Warn("fleet: the runtime pools come from the last pass", "error", err)
		p := f.platform
		p.mu.Lock()
		defer p.mu.Unlock()
		names := make([]string, 0, len(p.rt))
		for name := range p.rt {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			if rt := p.rt[name]; rt.base != nil && !rt.stored.Deleting {
				out = append(out, rt.base)
			}
		}
		return out
	}
	files := f.fileNames()
	for _, e := range entries {
		if files[e.Name] || e.Deleting {
			continue
		}
		if base, err := pools.FromRuntime(e.Runtime, f.templates); err == nil {
			out = append(out, base)
		}
	}
	return out
}

// reconcileRuntime runs one pass of each runtime pool of the store, after
// the pools of the file. shadowed holds the runtime pools whose name a
// pool of the file holds; the pass leaves them alone. A runtime pool that
// left the store leaves the view.
func (p *platform) reconcileRuntime(ctx context.Context, entries []pools.Stored, listed map[string][]*v1alpha1.Task, failed map[string]error, now time.Time) {
	files := p.f.fileNames()
	seen := map[string]bool{}
	for _, e := range entries {
		if files[e.Name] {
			continue
		}
		seen[e.Name] = true
		if gone := p.passRuntime(ctx, e, listed, failed, now); gone {
			seen[e.Name] = false
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for name := range p.rt {
		if !seen[name] {
			delete(p.rt, name)
			delete(p.poolState, name)
			delete(p.merged, name)
		}
	}
}

// passRuntime runs one pass of a runtime pool and records its state. It
// reports whether the pool is gone: a deleted pool whose Tasks are gone,
// whose entry the pass removed.
func (p *platform) passRuntime(ctx context.Context, e pools.Stored, listed map[string][]*v1alpha1.Task, failed map[string]error, now time.Time) bool {
	base, err := pools.FromRuntime(e.Runtime, p.f.templates)
	p.mu.Lock()
	prev := p.poolState[e.Name]
	p.rt[e.Name] = &runtimePool{stored: e, base: base}
	p.mu.Unlock()
	var st pools.Status
	switch {
	case err != nil:
		// A pool that does not build, for example because its template
		// left the configuration, changes nothing; its Tasks stay.
		st = pools.RuntimeStatus(e.Runtime, p.reconciler.Values.Redact(strings.TrimPrefix(err.Error(), "pools: ")+"; the pool changes nothing until it builds again"))
	case failed[base.Atespace] != nil:
		st = waiting(prev, base, p.reconciler.Values.Redact(fmt.Sprintf("the front could not list the Tasks of the atespace %s, so the pool waits: %v", base.Atespace, failed[base.Atespace])))
	default:
		use := base
		if !e.Deleting {
			// A deleted pool wants no Task, so it needs no profile: it
			// runs without its overlay until its Tasks are gone.
			if p.passRoutinesErr != nil {
				st = waiting(prev, base, p.reconciler.Values.Redact(fmt.Sprintf("the pool waits: %v", p.passRoutinesErr)))
				break
			}
			merged, err := p.f.mergedPoolWith(ctx, base, p.passRoutines)
			if err != nil {
				st = waiting(prev, base, p.reconciler.Values.Redact(fmt.Sprintf("the pool waits: %v", strings.TrimPrefix(err.Error(), "pools: "))))
				break
			}
			use = merged
		}
		st = p.reconciler.Reconcile(ctx, use, listed[base.Atespace], now)
		p.mu.Lock()
		p.merged[e.Name] = use
		p.mu.Unlock()
		if e.Deleting && p.finishDelete(ctx, e, base, &st) {
			p.mu.Lock()
			delete(p.rt, e.Name)
			delete(p.poolState, e.Name)
			delete(p.merged, e.Name)
			p.mu.Unlock()
			return true
		}
	}
	p.mu.Lock()
	p.poolState[e.Name] = st
	p.mu.Unlock()
	p.logGaps(e.Name, prev, st)
	return false
}

// waiting returns the state of a pool that waits for a reason: the Tasks
// of its last state, with the reason as its one gap.
func waiting(prev pools.Status, base *pools.Pool, why string) pools.Status {
	st := pools.Initial(base, why)
	if prev.Name == base.Name {
		st.Tasks, st.Actual, st.Ready, st.CheckedAt = prev.Tasks, prev.Actual, prev.Ready, prev.CheckedAt
	}
	return st
}

// finishDelete removes the entry of a deleted runtime pool once the Task
// list shows no Task of it, after the marker of the pool. A Task that
// failed after it ran keeps the entry until a person deletes the Task
// (section 14.4). It reports whether the entry is gone.
func (p *platform) finishDelete(ctx context.Context, e pools.Stored, base *pools.Pool, st *pools.Status) bool {
	if len(st.Tasks) > 0 {
		st.Gaps = append(st.Gaps, "a person deleted the pool, so the front deletes each Task of it once the Task is idle, and then the pool")
		return false
	}
	if err := p.reconciler.Forget(ctx, base); err != nil {
		st.Gaps = append(st.Gaps, p.reconciler.Values.Redact(fmt.Sprintf("the Tasks of the deleted pool are gone, and the front could not delete its marker (%v), so the pool stays until the next pass", err)))
		return false
	}
	rctx, cancel := context.WithTimeout(ctx, overlayTimeout)
	defer cancel()
	if err := p.f.runtime.Remove(rctx, e.Name, e.Rev); err != nil {
		st.Gaps = append(st.Gaps, fmt.Sprintf("the Tasks of the deleted pool are gone, and the front could not remove its entry (%v), so it tries again at the next pass", err))
		return false
	}
	p.f.log.Info("fleet: removed a deleted runtime pool whose Tasks are gone", "pool", e.Name, "agent", e.Agent, "generation", e.Generation)
	return true
}

// logGaps logs each reason of a state that the last state did not hold.
func (p *platform) logGaps(name string, prev, st pools.Status) {
	for _, g := range st.Gaps {
		if !slices.Contains(prev.Gaps, g) {
			p.f.log.Info("fleet: a pool differs from its configuration", "pool", name, "source", st.Source, "wanted", st.Wanted, "ready", st.Ready, "reason", g)
		}
	}
}

// runtimeState returns the state of a runtime pool for its entry e: the
// state of the last pass when that pass read this entry, else the state
// before the next pass, with the Tasks of the last one. With profile, a
// pool whose overlay holds no profile of its agent says that it waits for
// one.
func (f *Fleet) runtimeState(ctx context.Context, e pools.Stored, profile bool) pools.Status {
	base, err := pools.FromRuntime(e.Runtime, f.templates)
	if err != nil {
		return pools.RuntimeStatus(e.Runtime, strings.TrimPrefix(err.Error(), "pools: "))
	}
	p := f.platform
	p.mu.Lock()
	prev, ok := p.poolState[e.Name]
	rt := p.rt[e.Name]
	p.mu.Unlock()
	if ok && rt != nil && rt.stored.Rev == e.Rev {
		return prev
	}
	why := fmt.Sprintf("the front applies generation %d of the pool at its next pass", base.Generation)
	if profile && f.overlays != nil && !e.Deleting {
		octx, cancel := context.WithTimeout(ctx, overlayTimeout)
		o, err := f.overlays.Read(octx, e.Name)
		cancel()
		if _, has := o.Profile(e.Agent); err == nil && !has {
			why = fmt.Sprintf("the pool waits for the profile %s, which agent_put with pool %s writes, and then creates its Tasks", e.Agent, e.Name)
		}
	}
	return waiting(prev, base, why)
}

// PoolRequest asks for a runtime pool: the body of POST /v1/pools and the
// arguments of the MCP tool pool_put. Template defaults to the default
// template. Replicas is required, so no request makes a worker that it did
// not ask for. Env and Resources set values over the template.
type PoolRequest struct {
	Name      string             `json:"name"`
	Template  string             `json:"template,omitempty"`
	Agent     string             `json:"agent"`
	Replicas  *int               `json:"replicas"`
	Env       []pools.RuntimeEnv `json:"env,omitempty"`
	Resources *ax.ResourcesSpec  `json:"resources,omitempty"`
}

// PoolPatch changes a runtime pool: the body of PATCH /v1/pools/{name}. A
// field that is nil stays as it is. Env replaces the variables of the pool
// as a whole, and an empty list clears them; Resources replaces the
// resource values of the pool, and an empty object clears them.
type PoolPatch struct {
	Replicas  *int                `json:"replicas,omitempty"`
	Env       *[]pools.RuntimeEnv `json:"env,omitempty"`
	Resources *ax.ResourcesSpec   `json:"resources,omitempty"`
	Template  *string             `json:"template,omitempty"`
}

// Empty reports whether the patch changes nothing.
func (c PoolPatch) Empty() bool {
	return c.Replicas == nil && c.Env == nil && c.Resources == nil && c.Template == nil
}

// runtimeFleet returns the fleet of a when it keeps runtime pools, else an
// error that says why not.
func runtimeFleet(a *fw.Agent) (*Fleet, error) {
	f, _ := Of(a).(*Fleet)
	switch {
	case f == nil || f.platform == nil:
		return nil, fmt.Errorf("%w: the fleet is off, so this process keeps no pool; set workbench.fleet.atespaces", fw.ErrInvalidRequest)
	case len(f.templates) == 0:
		return nil, fmt.Errorf("%w: the configuration names no worker template, and a runtime pool renders from one; the deployer adds workbench.templates to fw.yaml", fw.ErrInvalidRequest)
	case f.runtime == nil || f.overlays == nil:
		return nil, fmt.Errorf("%w: the runtime pools are off, because they and the profiles of a pool live in the key-value store of a NATS bus; set bus.driver to nats", fw.ErrInvalidRequest)
	}
	return f, nil
}

// TemplatesOf returns the templates of the fleet of a, as the front shows
// them, and an empty list when it keeps none.
func TemplatesOf(a *fw.Agent) []pools.TemplateInfo {
	f, _ := Of(a).(*Fleet)
	if f == nil {
		return []pools.TemplateInfo{}
	}
	return pools.Infos(f.templates)
}

// ListPools returns the pools of the fleet of a that the caller can view,
// of the file and of the store, with their state. It reports false when
// the fleet keeps no pool and no template.
func ListPools(ctx context.Context, a *fw.Agent) ([]pools.Status, bool, error) {
	f, _ := Of(a).(*Fleet)
	if f == nil || !f.keepsPools() {
		return nil, false, nil
	}
	list, err := f.Pools(ctx)
	if list == nil {
		list = []pools.Status{}
	}
	return list, true, err
}

// CreatePool creates a runtime pool for the caller. It needs agent.create
// on the process (the creators of access). The profile of the agent comes
// from the overlay of the pool: until agent_put with the pool writes it,
// the pool creates no Task, and its state says so. A name that a pool of
// the file or of the store holds is an error that wraps ErrPoolExists.
func CreatePool(ctx context.Context, a *fw.Agent, req PoolRequest) (pools.Status, error) {
	f, err := runtimeFleet(a)
	if err != nil {
		return pools.Status{}, err
	}
	req.Agent = strings.TrimSpace(req.Agent)
	if err := f.a.Authorize(ctx, fw.ActionAgentCreate, fw.Target{Agent: req.Agent}); err != nil {
		return pools.Status{}, err
	}
	e, err := f.newRuntime(ctx, req)
	if err != nil {
		return pools.Status{}, err
	}
	if f.fileNames()[e.Name] {
		return pools.Status{}, fmt.Errorf("%w: the configuration file of the front holds a pool named %s; choose another name", ErrPoolExists, e.Name)
	}
	stored, err := f.createRuntime(ctx, e, false)
	if err != nil {
		return pools.Status{}, err
	}
	return f.runtimeState(ctx, stored, true), nil
}

// newRuntime checks a request for a runtime pool and returns the entry.
func (f *Fleet) newRuntime(ctx context.Context, req PoolRequest) (pools.Runtime, error) {
	name := strings.TrimSpace(req.Name)
	switch {
	case name == "":
		return pools.Runtime{}, fmt.Errorf("%w: set name, the name of the pool and the prefix of its Tasks, such as triage", fw.ErrInvalidRequest)
	case req.Agent == "":
		return pools.Runtime{}, fmt.Errorf("%w: set agent, the profile whose queue the workers take", fw.ErrInvalidRequest)
	case req.Replicas == nil:
		return pools.Runtime{}, fmt.Errorf("%w: set replicas, the number of workers of the pool; 0 makes none", fw.ErrInvalidRequest)
	}
	t, err := f.templateFor(req.Template)
	if err != nil {
		return pools.Runtime{}, err
	}
	e := pools.Runtime{Name: name, Template: t.Name, Agent: req.Agent, Replicas: *req.Replicas, Generation: 1, Env: req.Env,
		Resources: req.Resources, By: subjectOf(ctx), At: time.Now().UTC()}
	if _, err := pools.FromRuntime(e, f.templates); err != nil {
		return pools.Runtime{}, fmt.Errorf("%w: %s", fw.ErrInvalidRequest, strings.TrimPrefix(err.Error(), "pools: "))
	}
	return e, nil
}

// templateFor returns the template of the name, or the default template
// for an empty name, else an error that names the templates.
func (f *Fleet) templateFor(name string) (*pools.Template, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		if t := pools.DefaultTemplate(f.templates); t != nil {
			return t, nil
		}
		return nil, fmt.Errorf("%w: no template is the default, so name one in template; the templates are %s", fw.ErrInvalidRequest, pools.TemplateNames(f.templates))
	}
	if t := pools.TemplateNamed(f.templates, name); t != nil {
		return t, nil
	}
	return nil, fmt.Errorf("%w: no template is named %s; the templates are %s", fw.ErrInvalidRequest, name, pools.TemplateNames(f.templates))
}

// createRuntime writes a new runtime pool. With reuse, a pool of the name
// that runs the same agent and that nobody deleted counts as made, so a
// request that a client sends again makes one pool.
func (f *Fleet) createRuntime(ctx context.Context, e pools.Runtime, reuse bool) (pools.Stored, error) {
	ctx, cancel := context.WithTimeout(ctx, overlayTimeout)
	defer cancel()
	stored, err := f.runtime.Create(ctx, e)
	if errors.Is(err, pools.ErrRuntimeExists) {
		cur, gerr := f.runtime.Get(ctx, e.Name)
		switch {
		case gerr == nil && cur.Deleting:
			return pools.Stored{}, fmt.Errorf("%w: the pool %s is being deleted; create it again once it is gone", ErrPoolConflict, e.Name)
		case gerr == nil && reuse && cur.Agent == e.Agent:
			return cur, nil
		}
		return pools.Stored{}, fmt.Errorf("%w: the store holds a runtime pool named %s; choose another name", ErrPoolExists, e.Name)
	}
	if err != nil {
		return pools.Stored{}, err
	}
	f.log.Info("fleet: created a runtime pool", "pool", e.Name, "template", e.Template, "agent", e.Agent, "replicas", e.Replicas, "generation", stored.Generation, "principal", e.By)
	return stored, nil
}

// PatchPool changes a runtime pool for the caller. It needs the owner role
// on the agent of the pool, as a change of the profile does. A pool of the
// file changes in fw.yaml alone, and a deleted pool changes no more.
func PatchPool(ctx context.Context, a *fw.Agent, name string, c PoolPatch) (pools.Status, error) {
	f, cur, err := runtimeTarget(ctx, a, name)
	if err != nil {
		return pools.Status{}, err
	}
	if cur.Deleting {
		return pools.Status{}, fmt.Errorf("%w: a person deleted the pool %s, so it changes no more", ErrPoolConflict, cur.Name)
	}
	if c.Empty() {
		return f.runtimeState(ctx, cur, true), nil
	}
	ctx2, cancel := context.WithTimeout(ctx, overlayTimeout)
	defer cancel()
	stored, err := f.runtime.Update(ctx2, cur.Name, func(e *pools.Runtime) error {
		if e.Deleting {
			return fmt.Errorf("%w: a person deleted the pool %s, so it changes no more", ErrPoolConflict, e.Name)
		}
		if c.Replicas != nil {
			e.Replicas = *c.Replicas
		}
		if c.Env != nil {
			e.Env = append([]pools.RuntimeEnv(nil), (*c.Env)...)
		}
		if c.Resources != nil {
			r := *c.Resources
			e.Resources = &r
			if r == (ax.ResourcesSpec{}) {
				e.Resources = nil
			}
		}
		if c.Template != nil {
			t, err := f.templateFor(*c.Template)
			if err != nil {
				return err
			}
			e.Template = t.Name
		}
		if _, err := pools.FromRuntime(*e, f.templates); err != nil {
			return fmt.Errorf("%w: %s", fw.ErrInvalidRequest, strings.TrimPrefix(err.Error(), "pools: "))
		}
		return nil
	})
	if err != nil {
		return pools.Status{}, poolStoreError(err)
	}
	f.log.Info("fleet: changed a runtime pool", "pool", stored.Name, "template", stored.Template, "replicas", stored.Replicas, "generation", stored.Generation, "principal", subjectOf(ctx))
	return f.runtimeState(ctx, stored, true), nil
}

// DeletePool deletes a runtime pool for the caller. It needs the owner
// role on the agent of the pool. The pool then wants no Task, the front
// deletes each Task once it is idle, and the pass removes the entry once
// the Tasks are gone. An entry whose name a pool of the file holds owns no
// Task, so it goes at once. The answer is the state of the pool after the
// change.
func DeletePool(ctx context.Context, a *fw.Agent, name string) (pools.Status, error) {
	f, cur, err := runtimeTarget(ctx, a, name)
	if err != nil {
		return pools.Status{}, err
	}
	if f.fileNames()[cur.Name] {
		rctx, cancel := context.WithTimeout(ctx, overlayTimeout)
		defer cancel()
		if err := f.runtime.Remove(rctx, cur.Name, cur.Rev); err != nil {
			return pools.Status{}, poolStoreError(err)
		}
		f.log.Info("fleet: removed a runtime pool that a pool of the file shadows", "pool", cur.Name, "principal", subjectOf(ctx))
		st := pools.RuntimeStatus(cur.Runtime, "the front removed the entry at once, because the configuration file holds a pool of this name, which runs instead")
		st.Deleting = true
		return st, nil
	}
	stored, err := f.dropRuntime(ctx, cur.Name)
	if err != nil {
		return pools.Status{}, err
	}
	return f.runtimeState(ctx, stored, false), nil
}

// dropRuntime marks a runtime pool deleted: it wants no Task, and the pass
// removes it once its Tasks are gone. A pool that a person deleted already
// stays as it is.
func (f *Fleet) dropRuntime(ctx context.Context, name string) (pools.Stored, error) {
	ctx2, cancel := context.WithTimeout(ctx, overlayTimeout)
	defer cancel()
	cur, err := f.runtime.Get(ctx2, name)
	if err != nil {
		return pools.Stored{}, poolStoreError(err)
	}
	if cur.Deleting {
		return cur, nil
	}
	stored, err := f.runtime.Update(ctx2, name, func(e *pools.Runtime) error {
		e.Deleting, e.Replicas = true, 0
		return nil
	})
	if err != nil {
		return pools.Stored{}, poolStoreError(err)
	}
	f.log.Info("fleet: deleted a runtime pool; the front deletes its Tasks once they are idle", "pool", name, "generation", stored.Generation, "principal", subjectOf(ctx))
	return stored, nil
}

// runtimeTarget reads the runtime pool of the name for a change and checks
// that the caller has the owner role on its agent.
func runtimeTarget(ctx context.Context, a *fw.Agent, name string) (*Fleet, pools.Stored, error) {
	f, err := runtimeFleet(a)
	if err != nil {
		return nil, pools.Stored{}, err
	}
	name = strings.TrimSpace(name)
	ctx2, cancel := context.WithTimeout(ctx, overlayTimeout)
	cur, err := f.runtime.Get(ctx2, name)
	cancel()
	if errors.Is(err, pools.ErrRuntimeNotFound) && f.fileNames()[name] {
		return nil, pools.Stored{}, fmt.Errorf("%w: the pool %s comes from the configuration file of the front, so change it in workbench.pools of fw.yaml", fw.ErrInvalidRequest, name)
	}
	if err != nil {
		return nil, pools.Stored{}, poolStoreError(err)
	}
	if err := f.authorizeRuntime(ctx, cur.Runtime, fw.ActionAgentEdit); err != nil {
		return nil, pools.Stored{}, err
	}
	return f, cur, nil
}

// poolStoreError maps an error of the store to an error of the fleet.
func poolStoreError(err error) error {
	switch {
	case errors.Is(err, pools.ErrRuntimeNotFound):
		return fmt.Errorf("%w: %s", ErrPoolNotFound, strings.TrimPrefix(err.Error(), pools.ErrRuntimeNotFound.Error()+": "))
	case errors.Is(err, pools.ErrRuntimeConflict):
		return fmt.Errorf("%w: %s", ErrPoolConflict, strings.TrimPrefix(err.Error(), "pools: "))
	}
	return err
}

// authorizeRuntime decides an action on a runtime pool with the role of
// the caller on the agent of the pool, as the front decides for a profile
// of a pool: with the profile in the overlay of the pool. Until the
// overlay holds the profile, the person who wrote the entry last owns the
// pool.
func (f *Fleet) authorizeRuntime(ctx context.Context, e pools.Runtime, action string) error {
	if fw.PrincipalFrom(ctx) == nil {
		return nil
	}
	member := "/" + e.Name
	profiles := map[string]fw.AgentProfile{}
	if base, err := pools.FromRuntime(e, f.templates); err == nil {
		at, err := f.placeIn(ctx, base)
		if err != nil {
			return err
		}
		pool := at.(*poolPlace).pool
		profiles, member = profilesOf(pool), pool.Atespace+"/"+pool.Name
	} else if e.By != "" {
		profiles[e.Agent] = fw.AgentProfile{Name: e.Agent, Access: &fw.Access{Owners: []string{e.By}}}
	}
	return f.authorizeProfiles(ctx, profiles, member, action, []string{e.Agent})[0]
}

// profilesOf returns the profiles of a pool for a decision: the profiles
// that its workers run, and for a runtime pool whose overlay holds no
// profile of its agent yet, that agent with the person who created the
// pool as its owner.
func profilesOf(pool *pools.Pool) map[string]fw.AgentProfile {
	out := pool.Profiles()
	if r := pool.Runtime(); r != nil && r.By != "" {
		if _, ok := out[pool.Agent]; !ok {
			out[pool.Agent] = fw.AgentProfile{Name: pool.Agent, Access: &fw.Access{Owners: []string{r.By}}}
		}
	}
	return out
}

// RuntimePools reads the runtime pools of the store of cfg once, for the
// dry run (fw-workbench pools render): it builds each pool from its
// template and merges its overlay. A pool whose name a pool of the file
// holds is left out, and so is a pool that does not build; the error names
// each. A pool whose overlay does not answer or does not fit stays without
// it, as MergeOverlays says.
func RuntimePools(ctx context.Context, cfg *fw.Config, files []*pools.Pool, templates []*pools.Template) ([]*pools.Pool, error) {
	if len(templates) == 0 {
		return nil, nil
	}
	store, closeStore, err := OpenRuntime(ctx, cfg, slog.New(slog.DiscardHandler), true)
	if err != nil || store == nil {
		return nil, err
	}
	if closeStore != nil {
		defer closeStore()
	}
	lctx, cancel := context.WithTimeout(ctx, overlayTimeout)
	entries, err := store.List(lctx)
	cancel()
	if err != nil {
		return nil, fmt.Errorf("fleet: the runtime pools %s: %w", store.Describe(), err)
	}
	names := map[string]bool{}
	for _, p := range files {
		names[p.Name] = true
	}
	var built []*pools.Pool
	var problems []string
	for _, e := range entries {
		if names[e.Name] {
			problems = append(problems, fmt.Sprintf("the runtime pool %s has the name of a pool of the file, which runs instead", e.Name))
			continue
		}
		p, err := pools.FromRuntime(e.Runtime, templates)
		if err != nil {
			problems = append(problems, strings.TrimPrefix(err.Error(), "pools: "))
			continue
		}
		built = append(built, p)
	}
	merged, err := MergeOverlays(ctx, cfg, built)
	if err != nil {
		problems = append(problems, strings.TrimPrefix(err.Error(), "fleet: "))
	}
	if len(problems) > 0 {
		return merged, fmt.Errorf("fleet: %s", strings.Join(problems, "; "))
	}
	return merged, nil
}
