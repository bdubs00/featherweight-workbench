package fleet

// This file serves the packages of the pools (docs/ARCHITECTURE.md section
// 14.11). The front reads workbench.packages, the package mirror of the
// deployment. It subscribes to the subject fw.environment of the bus,
// where the kernel of each worker publishes the state of its environment
// (the kernel architecture, section 18.6), and keeps the last event of
// each Task of a pool in a bucket, so a restart of the front keeps the
// states. When a person approves an add or a remove of the tool env, the
// worker installs the change and publishes fw.env.changed, and the front
// writes the change into the overlay of the pool, so every worker of the
// pool gets it. The functions PackagesOf and PutPackages serve the routes
// /v1/pools/{name}/packages and the MCP tools packages_get and
// packages_put.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/bdubs00/featherweight-kernel/env/pkglist"
	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/internal/bucket"
	"github.com/bdubs00/featherweight-workbench/pools"
)

// EnvSubject is the subject of the bus on which the kernel publishes the
// events of the environment of a process.
const EnvSubject = "fw.environment"

// The events of the environment of the kernel.
const (
	EvEnvReady   = "fw.env.ready"
	EvEnvFailed  = "fw.env.failed"
	EvEnvChanged = "fw.env.changed"
)

// The states of a worker in the answer of PackagesOf.
const (
	EnvPending       = "pending"
	EnvMaterializing = "materializing"
	EnvReady         = "ready"
	EnvFailed        = "failed"
	EnvUnknown       = "unknown"
)

// ErrPackagesOff means that the front has no package mirror
// (workbench.packages) or keeps no pool, so the packages are off.
var ErrPackagesOff = errors.New("fleet: the packages are off, because the server has no package mirror (workbench.packages)")

// EnvBucket is the bucket of the last event of the environment of each
// Task of a pool.
var EnvBucket = bucket.Config{
	Name:        "ENVIRONMENTS",
	Description: "The last event of the package environment of each worker of a pool of a featherweight front.",
	History:     1,
	MaxValue:    256 << 10,
}

// envAttempts bounds the tries of a write that the other replica races.
const envAttempts = 5

// OpenEnvironments opens the bucket of the states of the environments on
// the bus of cfg: a bucket of the NATS server for the bus nats, and the
// memory of the process for the bus mem and for a bus that keeps no
// store, so the states then last until a restart. A test replaces it.
var OpenEnvironments = func(ctx context.Context, cfg *fw.Config, log *slog.Logger) (bucket.Bucket, func(), error) {
	b, closeFn, err := bucket.Open(ctx, cfg, EnvBucket, log)
	if errors.Is(err, bucket.ErrNoStore) {
		return bucket.NewMem(EnvBucket), nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("fleet: the states of the environments: %w", err)
	}
	return b, closeFn, nil
}

// startPackages reads the package mirror of the configuration, opens the
// bucket of the states, and subscribes to the events of the
// environments. A front without a mirror, or without pools, does nothing.
// The caller holds no lock, and the platform runs.
func (f *Fleet) startPackages(ctx context.Context, cfg *fw.Config) error {
	mirror, err := pools.LoadMirror(cfg)
	if err != nil {
		return fmt.Errorf("fleet: %w", err)
	}
	if mirror == nil || !f.keepsPools() {
		return nil
	}
	b, closeFn, err := OpenEnvironments(ctx, cfg, f.log)
	if err != nil {
		return err
	}
	f.mirror, f.envStates, f.closeEnv = mirror, b, closeFn
	if f.bus == nil {
		f.log.Warn("fleet: the front has no bus, so it hears no event of the environments of the workers")
		return nil
	}
	stop, err := f.bus.Subscribe(f.ctx, EnvSubject, f.onEnvironment)
	if err != nil {
		return fmt.Errorf("fleet: subscribe to %s: %w", EnvSubject, err)
	}
	f.viewMu.Lock()
	f.stops = append(f.stops, stop)
	f.viewMu.Unlock()
	f.log.Info("fleet keeps the packages of the pools", "subject", EnvSubject, "states", b.Describe())
	return nil
}

// PackagesOn reports whether the fleet of a serves the packages of its
// pools: the front has a package mirror and keeps pools.
func PackagesOn(a *fw.Agent) bool {
	f := fleetOf(a)
	return f != nil && f.mirror != nil
}

// envRecord is the value of one entry of the bucket of the states: the
// last event of the environment of a Task, with the pool that owned the
// Task when the event came.
type envRecord struct {
	Pool   string          `json:"pool"`
	Member string          `json:"member"`
	ID     string          `json:"id"`
	Type   string          `json:"type"`
	Time   time.Time       `json:"time"`
	Data   json.RawMessage `json:"data,omitempty"`
}

// envKey is the key of the state of a member, <atespace>/<task>, in
// base64url, because a key holds letters, digits, and a few marks only.
func envKey(member string) string {
	return "task." + base64.RawURLEncoding.EncodeToString([]byte(member))
}

// envChanged is the data of fw.env.changed.
type envChanged struct {
	Digest   string `json:"digest"`
	Previous string `json:"previous"`
	Manager  string `json:"manager"`
	Command  string `json:"command"`
	Package  string `json:"package"`
	Run      string `json:"run"`
	Agent    string `json:"agent"`
}

// envPackage is a package of a marker of the kernel.
type envPackage struct {
	Name     string `json:"name"`
	Version  string `json:"version"`
	Explicit bool   `json:"is_explicit"`
}

// envMarker is the data of fw.env.ready: the marker of a ready
// environment.
type envMarker struct {
	Digest string `json:"digest"`
	Python *struct {
		Packages []envPackage `json:"packages"`
	} `json:"python"`
	Tools *struct {
		Packages []envPackage `json:"packages"`
	} `json:"tools"`
}

// envFailure is the data of fw.env.failed.
type envFailure struct {
	Digest  string    `json:"digest"`
	Step    string    `json:"step"`
	Error   string    `json:"error"`
	Output  string    `json:"output"`
	RetryAt time.Time `json:"retryAt"`
}

// onEnvironment handles an event of the subject fw.environment. An event
// of a Task that no pool owns changes nothing. The front keeps fw.env.ready
// and fw.env.failed as the state of the Task, and it writes an approved
// change of the tool env (fw.env.changed) into the overlay of the pool.
// Each replica of the front gets each event, so both steps are safe to
// repeat.
func (f *Fleet) onEnvironment(e fw.Event) error {
	switch e.Type {
	case EvEnvReady, EvEnvFailed, EvEnvChanged:
	default:
		return nil
	}
	member, _, atespace := memberOf(strings.TrimSpace(e.Node))
	if atespace == "" {
		return nil
	}
	base := f.poolOfTask(atespace, strings.TrimPrefix(member, atespace+"/"))
	if base == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(f.ctx, overlayTimeout)
	defer cancel()
	if e.Type == EvEnvChanged {
		// A change keeps the last ready or failed state of the Task, so
		// its installed versions stay. The next fw.env.ready names the
		// change.
		f.applyEnvChange(ctx, base, member, e)
		return nil
	}
	if err := f.recordEnv(ctx, base.Name, member, e); err != nil {
		f.log.Warn("fleet: keep the state of the environment of a worker", "pool", base.Name, "task", member, "type", e.Type, "error", err)
	}
	return nil
}

// poolOfTask returns the pool of the configuration that owns the Task, a
// pool of the file or a runtime pool of the last pass that a person did
// not delete, or nil. A child Task of a worker belongs to no pool.
func (f *Fleet) poolOfTask(atespace, task string) *pools.Pool {
	p := f.platform
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, pl := range p.pools {
		if pl.Owns(atespace, task) {
			return pl
		}
	}
	for _, rt := range p.rt {
		if rt.base != nil && !rt.stored.Deleting && rt.base.Owns(atespace, task) {
			return rt.base
		}
	}
	return nil
}

// recordEnv keeps the event as the state of the member, unless the bucket
// holds a newer event of it or this event already.
func (f *Fleet) recordEnv(ctx context.Context, pool, member string, e fw.Event) error {
	if f.envStates == nil {
		return nil
	}
	value, err := json.Marshal(envRecord{Pool: pool, Member: member, ID: e.ID, Type: e.Type, Time: e.Time, Data: e.Data})
	if err != nil {
		return err
	}
	_, err = bucket.Modify(ctx, f.envStates, envKey(member), envAttempts, func(cur []byte, exists bool) ([]byte, error) {
		if exists {
			var old envRecord
			if json.Unmarshal(cur, &old) == nil && (old.ID == e.ID && e.ID != "" || old.Time.After(e.Time)) {
				return nil, nil
			}
		}
		return value, nil
	})
	return err
}

// envState reads the last event of the member, and reports false when the
// bucket holds none.
func (f *Fleet) envState(ctx context.Context, member string) (envRecord, bool, error) {
	if f.envStates == nil {
		return envRecord{}, false, nil
	}
	e, err := f.envStates.Get(ctx, envKey(member))
	if errors.Is(err, bucket.ErrNotFound) {
		return envRecord{}, false, nil
	}
	if err != nil {
		return envRecord{}, false, err
	}
	var r envRecord
	if err := json.Unmarshal(e.Value, &r); err != nil {
		return envRecord{}, false, fmt.Errorf("fleet: decode the state of %s: %w", member, err)
	}
	return r, true, nil
}

// applyEnvChange writes an add or a remove of the tool env of a worker into
// the overlay of its pool, so every worker of the pool gets it. It writes
// only a change that a person allowed: the setting of the pool is on and
// lets the agent ask, and the view saw a person allow the call of the tool
// env of the run with the same command, list, and package. A change that
// the overlay holds already, or an event that the front wrote before,
// writes nothing, so two replicas of the front write it once.
func (f *Fleet) applyEnvChange(ctx context.Context, base *pools.Pool, member string, e fw.Event) {
	var d envChanged
	if err := json.Unmarshal(e.Data, &d); err != nil || (d.Command != "add" && d.Command != "remove") {
		return
	}
	log := f.log.With("pool", base.Name, "task", member, "command", d.Command, "manager", d.Manager, "package", d.Package, "agent", d.Agent, "run", d.Run)
	name := strings.TrimSpace(d.Package)
	if err := pkglist.CheckRequirement(d.Manager, name); err != nil {
		log.Warn("fleet: a worker changed a package that the front does not take", "reason", "the list or the name does not fit")
		return
	}
	if f.overlays == nil {
		log.Warn("fleet: a worker changed a package, and the pool runs its configuration alone, so the other workers do not get it")
		return
	}
	decider, ok := f.platform.envApproval(d.Run, d.Command, d.Manager, name)
	if !ok {
		log.Warn("fleet: a worker changed a package, and the front saw no person allow it, so the front does not give it to the other workers")
		return
	}
	now := time.Now().UTC()
	item := pools.PackageItem{Name: name, By: decider, At: &now, Agent: d.Agent, Run: d.Run}
	refused := ""
	var wrote *pools.PackageSetting
	_, err := f.overlays.UpdatePackages(ctx, base.Name, func(cur *pools.PackageSetting) (*pools.PackageSetting, error) {
		wrote, refused = nil, ""
		s := base.ConfigPackages()
		if cur != nil {
			s = *cur
		}
		switch {
		case !s.On:
			refused = "the packages of the pool are off"
			return nil, nil
		case !s.Ask:
			refused = "the agent of the pool cannot ask for a package"
			return nil, nil
		case !s.NoteApplied(e.ID):
			return nil, nil
		case !pools.ChangePackages(&s, d.Manager, d.Command, item):
			return nil, nil
		}
		s.By, s.At = decider, &now
		wrote = &s
		return &s, nil
	})
	switch {
	case err != nil:
		log.Warn("fleet: write a package that a person approved into the overlay of the pool", "error", err)
	case refused != "":
		log.Warn("fleet: a worker changed a package that the pool does not take", "reason", refused)
	case wrote != nil:
		log.Info("fleet: the overlay of a pool takes a package that a person approved", "principal", decider)
		// The other workers of the pool get the package now.
		f.pushPool(base.Name, *wrote)
	}
}

// envAsk is a call of the tool env that asked a person, as the view saw
// it on the stream: the approval, the input, and the decision.
type envAsk struct {
	approval string
	command  string
	manager  string
	pkg      string
	allowed  bool
	decider  string
}

// maxEnvAsks bounds the calls of the tool env that the view keeps for one
// run.
const maxEnvAsks = 32

// noteEnv records an approval of the tool env of the run and its decision
// from the stream, so the front writes into the overlay only a change that
// a person allowed (applyEnvChange).
func (r *platformRun) noteEnv(e fw.Event) {
	switch e.Type {
	case fw.EvApprovalRequested:
		var d fw.ApprovalRequestedData
		if e.Decode(&d) != nil || d.Tool != pools.EnvTool || len(r.env) >= maxEnvAsks {
			return
		}
		var in struct {
			Command string `json:"command"`
			Manager string `json:"manager"`
			Package string `json:"package"`
		}
		if json.Unmarshal(d.Input, &in) != nil {
			return
		}
		r.env = append(r.env, envAsk{approval: d.ApprovalID, command: in.Command, manager: in.Manager, pkg: strings.TrimSpace(in.Package)})
	case fw.EvApprovalDecided:
		var d fw.ApprovalDecidedData
		if e.Decode(&d) != nil {
			return
		}
		for i := range r.env {
			if r.env[i].approval == d.ApprovalID {
				r.env[i].allowed, r.env[i].decider = d.Decision == "allow", d.Decider
			}
		}
	}
}

// envApproval returns the person who allowed a call of the tool env of the
// run with the command, the list, and the package, and reports false when
// the view saw no such approval.
func (p *platform) envApproval(run, command, manager, pkg string) (string, bool) {
	if p == nil || run == "" {
		return "", false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	r := p.runs[run]
	if r == nil {
		return "", false
	}
	for i := len(r.env) - 1; i >= 0; i-- {
		a := r.env[i]
		if a.allowed && a.command == command && a.manager == manager && a.pkg == pkg {
			return a.decider, true
		}
	}
	return "", false
}

// Packages is the answer of GET and PUT /v1/pools/{name}/packages and of
// the tools packages_get and packages_put. Available is true when the
// front can change the setting of the pool. On, Ask, Python, and Tools
// are the setting that the workers get. Applying is true while a worker
// of the pool runs an older configuration. Workers holds the state of the
// environment of each worker, and no worker while the packages are off.
type Packages struct {
	Pool      string              `json:"pool"`
	Available bool                `json:"available"`
	On        bool                `json:"on"`
	Ask       bool                `json:"ask"`
	Python    []pools.PackageItem `json:"python"`
	Tools     []pools.PackageItem `json:"tools"`
	Applying  bool                `json:"applying"`
	Workers   []WorkerPackages    `json:"workers"`
}

// WorkerPackages is the state of the environment of one worker. State is
// EnvPending, EnvMaterializing, EnvReady, EnvFailed, or EnvUnknown. At is
// the time of the last event of the worker, Digest the digest of its
// environment, Installed the versions of the packages of the setting that
// it installed, and Failure the last failure of its install.
type WorkerPackages struct {
	Task      string           `json:"task"`
	State     string           `json:"state"`
	At        *time.Time       `json:"at,omitempty"`
	Digest    string           `json:"digest,omitempty"`
	Installed *InstalledLists  `json:"installed,omitempty"`
	Failure   *PackagesFailure `json:"failure,omitempty"`
}

// InstalledLists names the installed version of each package of the
// setting, by list.
type InstalledLists struct {
	Python []InstalledPackage `json:"python"`
	Tools  []InstalledPackage `json:"tools"`
}

// InstalledPackage is one installed package.
type InstalledPackage struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// PackagesFailure is the last failure of the install of a worker: the
// step, the error, the end of the output of the manager as it is, and the
// time of the next try.
type PackagesFailure struct {
	Step    string     `json:"step"`
	Error   string     `json:"error"`
	Output  string     `json:"output,omitempty"`
	RetryAt *time.Time `json:"retryAt,omitempty"`
}

// PackagesRequest is the body of PUT /v1/pools/{name}/packages and the
// arguments of packages_put: the whole setting, which replaces the one of
// the pool. On is required. Python and Tools name the packages.
type PackagesRequest struct {
	On     *bool    `json:"on"`
	Ask    bool     `json:"ask,omitempty"`
	Python []string `json:"python,omitempty"`
	Tools  []string `json:"tools,omitempty"`
}

// packagesTarget is a pool for the packages: the pool of the
// configuration, the pool with its overlay, and its state.
type packagesTarget struct {
	f      *Fleet
	base   *pools.Pool
	merged *pools.Pool
	st     pools.Status
}

// packagesPool finds the pool of the name for the caller. A pool that the
// caller cannot view answers ErrPoolNotFound. The merged pool holds the
// overlay that the store holds now, merged or raw when it does not fit.
func packagesPool(ctx context.Context, a *fw.Agent, name string) (*packagesTarget, error) {
	f := fleetOf(a)
	if f == nil || f.mirror == nil {
		return nil, ErrPackagesOff
	}
	name = strings.TrimSpace(name)
	st, ok, err := PoolNamed(ctx, a, name)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("%w: no pool that you can view is named %s", ErrPoolNotFound, name)
	}
	var base *pools.Pool
	for _, p := range f.poolsNow(ctx) {
		if p.Name == name {
			base = p
		}
	}
	if base == nil {
		return nil, fmt.Errorf("%w: the pool %s is being deleted, or its entry does not build", ErrPoolConflict, name)
	}
	merged, err := f.mergedPool(ctx, base)
	if err != nil {
		var fit *overlayFitError
		if !errors.As(err, &fit) {
			return nil, err
		}
		merged = base.WithRawOverlay(fit.overlay)
	}
	return &packagesTarget{f: f, base: base, merged: merged, st: st}, nil
}

// PackagesOf returns the packages of the pool of the name for the caller,
// with the state of each worker. The caller must view the pool.
func PackagesOf(ctx context.Context, a *fw.Agent, name string) (Packages, error) {
	t, err := packagesPool(ctx, a, name)
	if err != nil {
		return Packages{}, err
	}
	return t.answer(ctx, t.merged)
}

// PutPackages replaces the packages of the pool of the name with the
// request for the caller, in the overlay of the pool. It needs the owner
// role on the agent of the pool, as PATCH /v1/pools/{name}. Each name is
// trimmed, a name that a list holds twice counts once, and a name that the
// kernel refuses is an error. An item that stays keeps its author, and a
// new item names the caller. With dry, it checks the request, answers the
// setting that it writes without dry, and writes nothing. A request
// that changes nothing writes nothing. The front sends the change to each
// running worker of the pool and replaces no worker for it.
func PutPackages(ctx context.Context, a *fw.Agent, name string, req PackagesRequest, dry bool) (Packages, error) {
	t, err := packagesPool(ctx, a, name)
	if err != nil {
		return Packages{}, err
	}
	f, base := t.f, t.base
	if err := f.authorizePool(ctx, t.merged, fw.ActionAgentEdit, base.Agent); err != nil {
		return Packages{}, err
	}
	if f.overlays == nil {
		return Packages{}, fmt.Errorf("%w: the pool %s runs its configuration alone, because the packages of a pool live in the key-value store of a NATS bus; set packages in workbench.pools of fw.yaml", fw.ErrInvalidRequest, base.Name)
	}
	if req.On == nil {
		return Packages{}, fmt.Errorf("%w: set on, true to give the workers packages and false to turn them off", fw.ErrInvalidRequest)
	}
	python, err := pools.CleanPackages(pools.ListPython, "python", req.Python)
	if err != nil {
		return Packages{}, fmt.Errorf("%w: %s", fw.ErrInvalidRequest, packageSentence(err))
	}
	tools, err := pools.CleanPackages(pools.ListTools, "tools", req.Tools)
	if err != nil {
		return Packages{}, fmt.Errorf("%w: %s", fw.ErrInvalidRequest, packageSentence(err))
	}
	cur := t.merged.PackageSetting()
	var added []string
	for _, n := range python {
		if !slices.Contains(pools.Names(cur.Python), n) {
			added = append(added, n)
		}
	}
	if err := f.checkPython(ctx, added); err != nil {
		return Packages{}, err
	}
	now, by := time.Now().UTC(), subjectOf(ctx)
	next := pools.PackageSetting{On: *req.On, Ask: req.Ask, Python: keepItems(cur.Python, python, by, now), Tools: keepItems(cur.Tools, tools, by, now), By: by, At: &now}
	if next.Same(cur) {
		return t.answer(ctx, t.merged)
	}
	proposed, err := base.WithOverlay(t.merged.Overlay().WithPackages(&next))
	switch {
	case errors.Is(err, pools.ErrNoProfile):
		// A runtime pool that waits for its profile renders nothing yet,
		// so the setting waits with it.
		proposed = base.WithRawOverlay(t.merged.Overlay().WithPackages(&next))
	case err != nil:
		return Packages{}, fmt.Errorf("%w: %s", fw.ErrInvalidRequest, strings.TrimPrefix(err.Error(), "pools: "))
	}
	if dry {
		return t.answer(ctx, proposed)
	}
	wctx, cancel := context.WithTimeout(ctx, overlayTimeout)
	defer cancel()
	if _, err := f.overlays.UpdatePackages(wctx, base.Name, func(old *pools.PackageSetting) (*pools.PackageSetting, error) {
		s := next.Clone()
		if old != nil {
			s.Applied = old.Applied
		}
		return &s, nil
	}); err != nil {
		return Packages{}, fmt.Errorf("fleet: write the packages of the pool %s %s: %w", base.Name, f.overlays.Describe(), err)
	}
	f.log.Info("fleet: the overlay of a pool sets the packages", "pool", base.Name, "on", next.On, "ask", next.Ask,
		"python", len(next.Python), "tools", len(next.Tools), "principal", by)
	// The running workers take the change in place: the hash of the pool
	// leaves the setting out, so the pool replaces no worker for it.
	f.pushPool(base.Name, next)
	return t.answer(ctx, proposed)
}

// packageSentence says a refusal of CleanPackages to a person: the
// sentence of a refused name, else the error as it is.
func packageSentence(err error) string {
	var pe *pools.PackageError
	if errors.As(err, &pe) {
		return pe.Sentence()
	}
	return capitalFirst(err.Error()) + "."
}

// capitalFirst writes the first letter of s as a capital.
func capitalFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// keepItems returns the items of names: an item that cur holds keeps its
// author and its time, and a new item names by and now.
func keepItems(cur []pools.PackageItem, names []string, by string, now time.Time) []pools.PackageItem {
	out := make([]pools.PackageItem, 0, len(names))
	for _, n := range names {
		i := slices.IndexFunc(cur, func(it pools.PackageItem) bool { return it.Name == n })
		if i >= 0 {
			out = append(out, cur[i])
			continue
		}
		at := now
		out = append(out, pools.PackageItem{Name: n, By: by, At: &at})
	}
	return out
}

// answer builds the answer for the pool p, the pool with the overlay of
// the store or of a request. applying is true while a Task of the pool
// runs an older configuration, or while a worker has not taken the
// setting yet: it is pending or materializing.
func (t *packagesTarget) answer(ctx context.Context, p *pools.Pool) (Packages, error) {
	s := p.PackageSetting()
	out := Packages{Pool: t.base.Name, Available: t.f.overlays != nil, On: s.On, Ask: s.Ask, Python: s.Python, Tools: s.Tools, Workers: []WorkerPackages{}}
	ctx, cancel := context.WithTimeout(ctx, overlayTimeout)
	defer cancel()
	for _, task := range t.st.Tasks {
		if task.Phase == phaseTerminating {
			continue
		}
		w, err := t.worker(ctx, p, s, task)
		if err != nil {
			return Packages{}, err
		}
		if w.State == EnvPending || w.State == EnvMaterializing {
			out.Applying = true
		}
		if s.On {
			out.Workers = append(out.Workers, w)
		}
	}
	return out, nil
}

// phaseTerminating is the phase of a Task that ax deletes.
const phaseTerminating = "Terminating"

// worker returns the state of the environment of one Task of the pool p
// with the setting s. A Task of an older configuration is pending: it gets
// the change once it is idle. An event since the creation of the Task
// decides for a Task of the current configuration: fw.env.ready says
// ready and fw.env.failed says failed, when the event names the switch
// and the lists of the setting. An event that names another switch or
// other lists says pending, because the front sends the setting to the
// worker (PUT /v1/environment). Without such an event, a Task that runs
// is materializing, because the kernel installs at its start, and a Task
// that does not run yet is pending. Without an event and without the time
// of its creation, the state is unknown.
func (t *packagesTarget) worker(ctx context.Context, p *pools.Pool, s pools.PackageSetting, task pools.TaskStatus) (WorkerPackages, error) {
	member := p.Atespace + "/" + task.Name
	w := WorkerPackages{Task: task.Name, State: EnvUnknown}
	rec, ok, err := t.f.envState(ctx, member)
	if err != nil {
		return w, fmt.Errorf("fleet: read the state of the environment of %s %s: %w", member, t.f.envStates.Describe(), err)
	}
	created := t.f.platform.createdOf(member)
	fresh := ok && (created.IsZero() || !rec.Time.Before(created))
	if fresh {
		at := rec.Time
		w.At = &at
		switch rec.Type {
		case EvEnvReady:
			var m envMarker
			if json.Unmarshal(rec.Data, &m) == nil {
				w.Digest, w.Installed = m.Digest, installed(m, s)
			}
		case EvEnvFailed:
			var fl envFailure
			if json.Unmarshal(rec.Data, &fl) == nil {
				w.Digest = fl.Digest
				w.Failure = &PackagesFailure{Step: fl.Step, Error: fl.Error, Output: fl.Output}
				if !fl.RetryAt.IsZero() {
					r := fl.RetryAt
					w.Failure.RetryAt = &r
				}
			}
		}
	}
	switch {
	case !task.Current || p.Hash() != t.st.Hash:
		w.State = EnvPending
	case fresh && !envTook(rec, s):
		w.State = EnvPending
	case fresh && rec.Type == EvEnvFailed:
		w.State = EnvFailed
	case fresh:
		w.State = EnvReady
	case created.IsZero() && !ok:
		w.State = EnvUnknown
	case task.Phase == phaseRunning:
		w.State = EnvMaterializing
	default:
		w.State = EnvPending
	}
	return w, nil
}

// envBase is what fw.env.ready and fw.env.failed name of the base of the
// environment: the switch, whether the agent can ask, and the lists with
// the local changes of the tool env. An event of an older kernel names
// none of them.
type envBase struct {
	On    *bool `json:"on"`
	Ask   *bool `json:"ask"`
	Lists *struct {
		Python []string `json:"python"`
		Tools  []string `json:"tools"`
	} `json:"lists"`
}

// envTook reports whether the event of a worker names the setting s: the
// same switch, the same ask, and, while the packages are on, the same
// names in both lists in any order. An event that names no switch comes
// from a kernel that takes no setting, and it counts as taken.
func envTook(rec envRecord, s pools.PackageSetting) bool {
	var b envBase
	if json.Unmarshal(rec.Data, &b) != nil || b.On == nil {
		return true
	}
	if *b.On != s.On || (b.Ask != nil && *b.Ask != s.Ask) {
		return false
	}
	if !s.On {
		return true
	}
	var py, tools []string
	if b.Lists != nil {
		py, tools = b.Lists.Python, b.Lists.Tools
	}
	return sameNames(py, pools.Names(s.Python)) && sameNames(tools, pools.Names(s.Tools))
}

// sameNames reports whether two lists hold the same trimmed names, in any
// order.
func sameNames(a, b []string) bool {
	norm := func(l []string) []string {
		out := make([]string, 0, len(l))
		for _, n := range l {
			if n = strings.TrimSpace(n); n != "" && !slices.Contains(out, n) {
				out = append(out, n)
			}
		}
		slices.Sort(out)
		return out
	}
	return slices.Equal(norm(a), norm(b))
}

// installed returns the installed version of each package of the setting
// from a marker, by list. A Program counts when its name matches, or when
// pixi marks it as named in the manifest.
func installed(m envMarker, s pools.PackageSetting) *InstalledLists {
	out := &InstalledLists{Python: []InstalledPackage{}, Tools: []InstalledPackage{}}
	want := func(manager string, list []pools.PackageItem) map[string]bool {
		names := map[string]bool{}
		for _, it := range list {
			names[pools.PackageName(manager, it.Name)] = true
		}
		return names
	}
	if m.Python != nil {
		names := want(pools.ListPython, s.Python)
		for _, pk := range m.Python.Packages {
			if names[pools.PackageName(pools.ListPython, pk.Name)] {
				out.Python = append(out.Python, InstalledPackage{Name: pk.Name, Version: pk.Version})
			}
		}
	}
	if m.Tools != nil {
		names := want(pools.ListTools, s.Tools)
		for _, pk := range m.Tools.Packages {
			if names[pools.PackageName(pools.ListTools, pk.Name)] || pk.Explicit {
				out.Tools = append(out.Tools, InstalledPackage{Name: pk.Name, Version: pk.Version})
			}
		}
	}
	return out
}

// createdOf returns when ax created the Task of the member, as the Task
// list gave it, and the zero time when the view does not know.
func (p *platform) createdOf(member string) time.Time {
	if p == nil {
		return time.Time{}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if m := p.members[member]; m != nil {
		return m.created
	}
	return time.Time{}
}
