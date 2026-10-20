package fleet

// This file keeps the profiles of the pools (docs/ARCHITECTURE.md section
// 14.9). The front owns the profiles that the workers of its pools run: the
// block fw of each pool in its configuration, and the overlay of each pool,
// which people change through agent_put, agent_delete, the builder, and
// the approval card. The overlay lives in a store that every replica of
// the front shares, the key-value bucket <stream>_POOL_<pool> on a NATS
// bus. A pass merges the overlay into the fw.yaml of the workers
// (pools.Pool.WithOverlay), so a change reaches every Task of the pool,
// and no worker diverges from its siblings.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/surface/view"
	"github.com/bdubs00/featherweight-workbench/agents"
	"github.com/bdubs00/featherweight-workbench/pools"
)

// overlayTimeout bounds one request to the store of the overlays.
const overlayTimeout = 5 * time.Second

// ErrNoPool means that a request needs a pool of the fleet, such as an
// approval rule, and no pool runs its target.
var ErrNoPool = errors.New("fleet: no pool runs this")

// OpenOverlays opens the store of the overlays of the pools: the key-value
// buckets of the NATS server of the bus for the bus nats, the memory of
// the process for the bus mem, and no store for another bus, so the pools
// run their configuration alone. The function closes the store. A test
// replaces it.
var OpenOverlays = func(ctx context.Context, cfg *fw.Config, log *slog.Logger) (pools.OverlayStore, func(), error) {
	switch strings.TrimSpace(cfg.Bus.Driver) {
	case "nats":
		n, err := openNATS(ctx, cfg, 0, log)
		if err != nil {
			return nil, nil, err
		}
		return newNATSOverlays(n), n.close, nil
	case "mem":
		return pools.NewMemOverlays(), nil, nil
	}
	return nil, nil, nil
}

// MergeOverlays returns the pools of the configuration cfg with the
// overlays that the front keeps for them, for the dry run (fw-workbench
// pools render). A pool whose overlay does not answer or does not fit
// stays as the configuration says, and the error names it.
func MergeOverlays(ctx context.Context, cfg *fw.Config, list []*pools.Pool) ([]*pools.Pool, error) {
	if len(list) == 0 {
		return list, nil
	}
	store, closeStore, err := OpenOverlays(ctx, cfg, slog.New(slog.DiscardHandler))
	if err != nil || store == nil {
		return list, err
	}
	if closeStore != nil {
		defer closeStore()
	}
	ctx, cancel := context.WithTimeout(ctx, overlayTimeout)
	defer cancel()
	out := slices.Clone(list)
	var problems []string
	for i, p := range list {
		o, err := store.Read(ctx, p.Name)
		if err == nil {
			out[i], err = p.WithOverlay(o)
		}
		if err != nil {
			out[i] = p
			problems = append(problems, fmt.Sprintf("the pool %s: %v", p.Name, err))
		}
	}
	if len(problems) > 0 {
		return out, fmt.Errorf("fleet: the overlays %s: %s", store.Describe(), strings.Join(problems, "; "))
	}
	return out, nil
}

// mergedPool reads the overlay of a pool and merges it into the pool. A
// front without a store of the overlays runs the pool as it is.
func (f *Fleet) mergedPool(ctx context.Context, base *pools.Pool) (*pools.Pool, error) {
	if f.overlays == nil {
		return base, nil
	}
	ctx, cancel := context.WithTimeout(ctx, overlayTimeout)
	defer cancel()
	o, err := f.overlays.Read(ctx, base.Name)
	if err != nil {
		return nil, fmt.Errorf("fleet: read the overlay of the pool %s %s: %w", base.Name, f.overlays.Describe(), err)
	}
	return base.WithOverlay(o)
}

// current returns the pool that the last pass merged, else the pool of the
// configuration.
func (p *platform) current(base *pools.Pool) *pools.Pool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if m := p.merged[base.Name]; m != nil {
		return m
	}
	return base
}

// fleetOf returns the fleet of an agent when it keeps pools.
func fleetOf(a *fw.Agent) *Fleet {
	f, _ := Of(a).(*Fleet)
	if f == nil || f.platform == nil || len(f.platform.pools) == 0 {
		return nil
	}
	return f
}

// Pooled reports whether the fleet of a keeps pools.
func Pooled(a *fw.Agent) bool { return fleetOf(a) != nil }

// PlaceOf returns the place of the profile name for agent_put,
// agent_delete, and the builder: the pool of the fleet of a that runs the
// profile, else this process (agents.Local). pool names the pool, for a
// profile that several pools run or that no pool runs yet. A pool runs the
// profile that its key agent names, or a profile of its fw.agents and its
// overlay. Without pools, the place is this process, and pool must be
// empty. With pools, a name that no pool runs and that this process does
// not hold is an error that names the pools: a new agent needs a pool of
// its own, or pool.
func PlaceOf(ctx context.Context, a *fw.Agent, name, pool string) (agents.Place, error) {
	name, pool = strings.TrimSpace(name), strings.TrimSpace(pool)
	f := fleetOf(a)
	if f == nil {
		if pool != "" {
			return nil, fmt.Errorf("%w: this process keeps no pool, so leave out pool", fw.ErrInvalidRequest)
		}
		return agents.Local(a), nil
	}
	base, err := f.poolFor(name, pool)
	if err != nil {
		return nil, err
	}
	if base == nil {
		return agents.Local(a), nil
	}
	if f.overlays == nil {
		return nil, fmt.Errorf("%w: the pool %s runs its configuration alone, because the profiles of a pool live in the key-value store of a NATS bus; change the pool in workbench.pools of fw.yaml", fw.ErrInvalidRequest, base.Name)
	}
	merged, err := f.mergedPool(ctx, base)
	if err != nil {
		return nil, err
	}
	return &poolPlace{f: f, base: base, pool: merged}, nil
}

// PoolOf returns the name of the pool of a place from PlaceOf, or "" for
// this process.
func PoolOf(at agents.Place) string {
	if pl, ok := at.(*poolPlace); ok {
		return pl.base.Name
	}
	return ""
}

// poolFor picks the pool of a profile, as PlaceOf says. It returns nil for
// a profile of this process.
func (f *Fleet) poolFor(name, pool string) (*pools.Pool, error) {
	list := f.platform.pools
	if pool != "" {
		for _, p := range list {
			if p.Name == pool {
				return p, nil
			}
		}
		return nil, fmt.Errorf("%w: no pool is named %s; the pools are %s", fw.ErrInvalidRequest, pool, poolNames(list))
	}
	var by []*pools.Pool
	for _, p := range list {
		if p.Agent == name {
			by = append(by, p)
		}
	}
	if len(by) == 0 {
		for _, p := range list {
			if _, ok := f.platform.current(p).Profiles()[name]; ok {
				by = append(by, p)
			}
		}
	}
	switch len(by) {
	case 1:
		return by[0], nil
	case 0:
		if _, ok := f.a.ProfileNamed(name); ok {
			return nil, nil
		}
		return nil, fmt.Errorf("%w: no pool runs the agent %s. A pool takes the jobs of its agent from its own queue, so a new agent needs an entry in workbench.pools of fw.yaml with agent %s; to add the profile to the workers of a pool, for spawn_task or a run on a member, pass pool. The pools are %s", fw.ErrInvalidRequest, name, name, poolNames(list))
	}
	return nil, fmt.Errorf("%w: the pools %s run the agent %s; pass pool to name one", fw.ErrInvalidRequest, poolNames(by), name)
}

// poolNames names the pools and their agents.
func poolNames(list []*pools.Pool) string {
	var out []string
	for _, p := range list {
		out = append(out, p.Name+" (agent "+p.Agent+")")
	}
	return strings.Join(out, ", ")
}

// authorizePool decides an action of the kernel on a profile of a pool,
// as the front decides for the agent of a member (section 4.4): in the
// rules mode with the access configuration of the front and the access
// block of the profile in the pool, and else with the decision point of
// this process, which receives the pool as the member and the access
// block. A call without a principal is the process itself.
func (f *Fleet) authorizePool(ctx context.Context, pool *pools.Pool, action, name string) error {
	return f.authorizePoolAll(ctx, pool, action, []string{name})[0]
}

// authorizePoolAll is authorizePool for several profiles of one pool, in
// one request to a decision point.
func (f *Fleet) authorizePoolAll(ctx context.Context, pool *pools.Pool, action string, names []string) []error {
	out := make([]error, len(names))
	p := fw.PrincipalFrom(ctx)
	if p == nil {
		return out
	}
	profiles := pool.Profiles()
	if f.a.AccessMode() == fw.AccessModeRules {
		rules := fw.NewRulesAuthz(f.a.Config().Access, func(n string) (fw.AgentProfile, bool) {
			prof, ok := profiles[n]
			return prof, ok
		})
		for i, name := range names {
			out[i] = rules.Allow(ctx, p, action, fw.Target{Agent: name})
		}
		return out
	}
	checks := make([]fw.AuthzCheck, len(names))
	for i, name := range names {
		checks[i] = fw.AuthzCheck{Action: action, Target: fw.Target{Agent: name, Member: pool.Atespace + "/" + pool.Name, Access: profiles[name].Access}}
	}
	return f.a.AuthorizeAll(ctx, checks)
}

// poolRoles returns the role of the caller on each profile of a pool, ""
// for a profile that the caller cannot view.
func (f *Fleet) poolRoles(ctx context.Context, pool *pools.Pool, names []string) ([]string, error) {
	var answers [][]error
	for _, action := range roleActions {
		errs := f.authorizePoolAll(ctx, pool, action, names)
		for _, err := range errs {
			if errors.Is(err, fw.ErrAuthzUnavailable) {
				return nil, fmt.Errorf("fleet: decide about the profiles of the pool %s: %w", pool.Name, err)
			}
		}
		answers = append(answers, errs)
	}
	out := make([]string, len(names))
	for i := range names {
		out[i] = roleFrom([]error{answers[0][i], answers[1][i], answers[2][i]})
	}
	return out, nil
}

// poolPlace is a pool as the place of its profiles (agents.Place). base is
// the pool of the configuration, and pool the pool with the overlay that
// the place read.
type poolPlace struct {
	f    *Fleet
	base *pools.Pool
	pool *pools.Pool
}

func (pl *poolPlace) Profile(name string) (fw.AgentProfile, bool) {
	p, ok := pl.pool.Profiles()[name]
	return p, ok
}

// Check runs the checks of the kernel on the name and the limits, and then
// the checks of a pool on the merged configuration, which also checks the
// model against the models of the workers.
func (pl *poolPlace) Check(p fw.AgentProfile) error {
	c := p
	c.Model = ""
	if err := pl.f.a.CheckProfile(c); err != nil {
		return err
	}
	if _, err := pl.base.WithOverlay(pl.pool.Overlay().WithProfile(pools.OverlayProfile{Profile: p})); err != nil {
		return fmt.Errorf("%w: %s", fw.ErrInvalidRequest, strings.TrimPrefix(err.Error(), "pools: "))
	}
	return nil
}

func (pl *poolPlace) Authorize(ctx context.Context, action, name string) error {
	return pl.f.authorizePool(ctx, pl.pool, action, name)
}

// Locked is false: the overlay can replace a profile of the configuration
// of the pool. Remove refuses to remove one.
func (pl *poolPlace) Locked(string) bool { return false }

func (pl *poolPlace) Save(ctx context.Context, p fw.AgentProfile) error {
	ctx, cancel := context.WithTimeout(ctx, overlayTimeout)
	defer cancel()
	if err := pl.f.overlays.PutProfile(ctx, pl.base.Name, pools.OverlayProfile{Profile: p, By: subjectOf(ctx), At: time.Now().UTC()}); err != nil {
		return fmt.Errorf("fleet: write the profile %s to the overlay of the pool %s: %w", p.Name, pl.base.Name, err)
	}
	pl.f.log.Info("fleet: the overlay of a pool sets a profile", "pool", pl.base.Name, "profile", p.Name, "principal", subjectOf(ctx))
	return nil
}

func (pl *poolPlace) Remove(ctx context.Context, name string) error {
	if _, ok := pl.pool.Overlay().Profile(name); !ok {
		return fmt.Errorf("%w: the configuration of the pool %s defines the agent %s, so change it in workbench.pools of fw.yaml", fw.ErrInvalidRequest, pl.base.Name, name)
	}
	ctx, cancel := context.WithTimeout(ctx, overlayTimeout)
	defer cancel()
	if _, err := pl.f.overlays.DeleteProfile(ctx, pl.base.Name, name); err != nil {
		return fmt.Errorf("fleet: remove the profile %s from the overlay of the pool %s: %w", name, pl.base.Name, err)
	}
	pl.f.log.Info("fleet: the overlay of a pool drops a profile", "pool", pl.base.Name, "profile", name, "principal", subjectOf(ctx))
	return nil
}

func (pl *poolPlace) View(ctx context.Context, name string) (view.AgentView, error) {
	merged, err := pl.f.mergedPool(ctx, pl.base)
	if err != nil {
		return view.AgentView{}, err
	}
	prof, ok := merged.Profiles()[name]
	if !ok {
		return view.AgentView{}, fmt.Errorf("%w: %s", fw.ErrProfileNotFound, name)
	}
	roles, err := pl.f.poolRoles(ctx, merged, []string{name})
	if err != nil {
		return view.AgentView{}, err
	}
	return view.AgentView{AgentProfile: prof, Role: roleOrNone(roles[0])}, nil
}

func (pl *poolPlace) Role(ctx context.Context, name string) string {
	roles, err := pl.f.poolRoles(ctx, pl.pool, []string{name})
	if err != nil {
		return fw.AccessNone.String()
	}
	return roleOrNone(roles[0])
}

// Abilities is empty: the tools of the workers live in their Tasks, so the
// front cannot word them. The view of a pool profile shows the policy of
// the workers instead.
func (pl *poolPlace) Abilities(context.Context, fw.AgentProfile) ([]view.Ability, error) {
	return nil, nil
}

func roleOrNone(role string) string {
	if role == "" {
		return fw.AccessNone.String()
	}
	return role
}

// PoolAgent is a profile that the workers of a pool run, as the caller
// sees it: the profile with the overlay on top and the role of the caller.
// Overlay is "added" when the overlay adds the profile, "replaced" when it
// replaces the profile of the configuration of the pool, and empty for the
// profile of the configuration; By and At say who wrote the overlay entry
// and when. WorkerPolicy is fw.policy of the workers with the rules of the
// overlay, which the policy of the profile can only tighten. Front marks a
// profile of this process instead, which no worker of a pool runs.
type PoolAgent struct {
	view.AgentView
	Pool         string     `json:"pool,omitempty"`
	Overlay      string     `json:"overlay,omitempty"`
	By           string     `json:"by,omitempty"`
	At           *time.Time `json:"at,omitempty"`
	WorkerPolicy *fw.Rules  `json:"workerPolicy,omitempty"`
	Front        bool       `json:"front,omitempty"`
}

// Overlay states of a PoolAgent.
const (
	OverlayAdded    = "added"
	OverlayReplaced = "replaced"
)

// PoolAgents lists the profiles of the pools of the fleet of a that the
// caller can view, in the order of the pools and by name in a pool, and
// reports whether the fleet keeps pools. It reads the overlay of each
// pool. When the store does not answer, it lists the profiles that the
// last pass merged, and the log says so.
func PoolAgents(ctx context.Context, a *fw.Agent) ([]PoolAgent, bool, error) {
	f := fleetOf(a)
	if f == nil {
		return nil, false, nil
	}
	out := []PoolAgent{}
	for _, base := range f.platform.pools {
		got, err := f.poolAgents(ctx, base, "")
		if err != nil {
			return nil, true, err
		}
		out = append(out, got...)
	}
	return out, true, nil
}

// PoolAgentNamed returns the profile name that a pool of the fleet of a
// runs, as the caller sees it. pool names the pool when several pools run
// the profile. It reports false when no pool that the caller can view
// runs the profile.
func PoolAgentNamed(ctx context.Context, a *fw.Agent, name, pool string) (PoolAgent, bool, error) {
	f := fleetOf(a)
	if f == nil {
		return PoolAgent{}, false, nil
	}
	var found []PoolAgent
	for _, base := range f.platform.pools {
		if pool != "" && base.Name != pool {
			continue
		}
		got, err := f.poolAgents(ctx, base, name)
		if err != nil {
			return PoolAgent{}, false, err
		}
		found = append(found, got...)
	}
	switch len(found) {
	case 0:
		return PoolAgent{}, false, nil
	case 1:
		return found[0], true, nil
	}
	var names []string
	for _, pa := range found {
		names = append(names, pa.Pool)
	}
	return PoolAgent{}, false, fmt.Errorf("%w: the pools %s run the agent %s; pass pool to name one", fw.ErrInvalidRequest, strings.Join(names, ", "), name)
}

// poolAgents returns the profiles of one pool that the caller can view,
// or only the profile only when it is set.
func (f *Fleet) poolAgents(ctx context.Context, base *pools.Pool, only string) ([]PoolAgent, error) {
	merged, err := f.mergedPool(ctx, base)
	if err != nil {
		f.log.Warn("fleet: the profiles of a pool come from the last pass", "pool", base.Name, "error", err)
		merged = f.platform.current(base)
	}
	profiles, given := merged.Profiles(), base.Profiles()
	var names []string
	for name := range profiles {
		if only == "" || name == only {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	roles, err := f.poolRoles(ctx, merged, names)
	if err != nil {
		return nil, err
	}
	policy := merged.Policy().Rules
	var out []PoolAgent
	for i, name := range names {
		if roles[i] == "" {
			continue
		}
		pa := PoolAgent{AgentView: view.AgentView{AgentProfile: profiles[name], Role: roles[i]}, Pool: base.Name, WorkerPolicy: &policy}
		if e, ok := merged.Overlay().Profile(name); ok {
			pa.Overlay = OverlayAdded
			if _, ok := given[name]; ok {
				pa.Overlay = OverlayReplaced
			}
			at := e.At
			pa.By, pa.At = e.By, &at
		}
		out = append(out, pa)
	}
	return out, nil
}

// AllowRule writes an allow rule for the tool of the approval approvalID of
// the run runID into the overlay of the pool whose Task runs the run, so
// the workers of the pool run every later call of the tool without asking
// (section 14.9). The rule is the name of the tool, as the policy names it,
// such as github.create_pull_request, and for bash the whole tool. It
// needs the owner role on the profile of the run in the pool, as
// agent_put does. note goes into the entry beside the subject of the
// caller and the time, so a person sees why the rule exists. It returns
// the pool and the rule, and it decides nothing: the caller allows the
// approval. It returns an error that wraps ErrNoPool when no pool runs the
// run, and fw.ErrInvalidRequest when a rule cannot answer the approval.
func AllowRule(ctx context.Context, a *fw.Agent, runID, approvalID, note string) (string, string, error) {
	f := fleetOf(a)
	if f == nil {
		return "", "", fmt.Errorf("%w: this process keeps no pool", ErrNoPool)
	}
	if _, err := a.Get(ctx, runID); err == nil {
		return "", "", fmt.Errorf("%w: the run %s runs on this process, not on a worker of a pool", ErrNoPool, runID)
	}
	r, err := f.Run(ctx, runID)
	if err != nil {
		return "", "", err
	}
	base := f.poolOfRun(r)
	if base == nil {
		return "", "", fmt.Errorf("%w: no pool keeps the member %s of the run %s", ErrNoPool, r.Member, runID)
	}
	if f.overlays == nil {
		return "", "", fmt.Errorf("%w: the pool %s runs its configuration alone, because the rules of a pool live in the key-value store of a NATS bus; add the rule to fw.policy of the pool in fw.yaml", fw.ErrInvalidRequest, base.Name)
	}
	p := r.Pending
	switch {
	case p == nil || p.ID != approvalID:
		return "", "", fmt.Errorf("%w: the run %s waits for no approval %s", fw.ErrInvalidRequest, runID, approvalID)
	case p.Connect != nil || len(p.Schema) > 0:
		return "", "", fmt.Errorf("%w: the approval %s is a question or a connection, not a tool call, so no rule answers it", fw.ErrInvalidRequest, approvalID)
	}
	merged, err := f.mergedPool(ctx, base)
	if err != nil {
		return "", "", err
	}
	if err := f.authorizePool(ctx, merged, fw.ActionAgentEdit, r.Agent); err != nil {
		return "", "", err
	}
	rule := strings.TrimSpace(p.Tool)
	if why := ruleRefusal(p.Reason, rule); why != "" {
		return "", "", fmt.Errorf("%w: %s", fw.ErrInvalidRequest, why)
	}
	if merged.Policy().External != nil {
		return "", "", fmt.Errorf("%w: the workers of the pool %s ask a control plane about each call (fw.policy.external), so add the rule there", fw.ErrInvalidRequest, base.Name)
	}
	entry := pools.OverlayRule{Rule: rule, Agent: r.Agent, Run: r.ID, By: subjectOf(ctx), At: time.Now().UTC(), Note: strings.TrimSpace(note)}
	next, err := base.WithOverlay(merged.Overlay().WithRule(entry))
	if err != nil {
		return "", "", fmt.Errorf("%w: %s", fw.ErrInvalidRequest, strings.TrimPrefix(err.Error(), "pools: "))
	}
	input := p.Input
	if !json.Valid(input) {
		input = nil
	}
	if d := next.Decide(r.Agent, rule, input); d.Verdict != fw.VerdictAllow {
		return "", "", fmt.Errorf("%w: an allow rule for %s does not change the decision, because %q of the profile %s or of the pool still holds; change that policy with agent_put or in fw.yaml, or allow once", fw.ErrInvalidRequest, rule, d.Reason, r.Agent)
	}
	wctx, cancel := context.WithTimeout(ctx, overlayTimeout)
	defer cancel()
	if err := f.overlays.AddRule(wctx, base.Name, entry); err != nil {
		return "", "", fmt.Errorf("fleet: write the rule %s to the overlay of the pool %s: %w", rule, base.Name, err)
	}
	f.log.Info("fleet: the overlay of a pool allows a tool", "pool", base.Name, "rule", rule, "agent", r.Agent, "run", r.ID, "principal", entry.By)
	return base.Name, rule, nil
}

// poolOfRun returns the pool whose Task runs the run: the pool of its
// member by the name of the Task, else the only pool of its agent.
func (f *Fleet) poolOfRun(r *fw.Run) *pools.Pool {
	atespace, task, _ := strings.Cut(r.Member, "/")
	var byAgent []*pools.Pool
	for _, p := range f.platform.pools {
		if p.Owns(atespace, task) {
			return p
		}
		if p.Agent == r.Agent {
			byAgent = append(byAgent, p)
		}
	}
	if len(byAgent) == 1 {
		return byAgent[0]
	}
	return nil
}

// ruleRefusal says why an allow rule for the tool cannot answer an
// approval of the reason, or "". A rule answers an ask of the default and
// an ask of a rule that names the tool, a glob, or a pattern; the merged
// policy then shows whether the rule takes the ask away. A rule by a flag
// asks for every tool with the flag, and no allow rule loosens it. Another
// reason, such as a rerun after a resume, comes from no policy rule.
func ruleRefusal(reason, tool string) string {
	switch {
	case tool == "":
		return "the approval names no tool"
	case reason == "default":
		return ""
	case strings.HasPrefix(reason, "rule: ask "):
		entry := strings.TrimSpace(strings.TrimPrefix(reason, "rule: ask "))
		switch strings.ToLower(entry) {
		case "readonly", "read-only", "idempotent", "destructive", "parallel", "parallelsafe":
			return fmt.Sprintf("the policy asks before every tool with the flag %s, and an allow rule for %s does not loosen that ask; change the policy of the pool in fw.yaml, or allow once", entry, tool)
		}
		return ""
	}
	return fmt.Sprintf("the call waits for %q, which no policy rule answers; allow once", reason)
}

// subjectOf is the subject of the principal in ctx, or "".
func subjectOf(ctx context.Context) string {
	if p := fw.PrincipalFrom(ctx); p != nil {
		return p.Subject
	}
	return ""
}
