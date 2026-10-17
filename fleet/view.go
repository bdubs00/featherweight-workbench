package fleet

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/teams"
)

// settleMax bounds the wait for the retained records when the view starts.
const settleMax = 2 * time.Second

// maxLineage bounds a walk up the parents of a run inside a record.
const maxLineage = 64

// startView watches the records and the heartbeats of the other members,
// at the first call that needs them. After a failure, the next call tries
// again.
func (f *Fleet) startView(ctx context.Context) error {
	f.viewMu.Lock()
	defer f.viewMu.Unlock()
	if f.viewOn {
		return nil
	}
	if f.closed.Load() {
		return errors.New("fleet: the fleet is closed")
	}
	watch := f.bus.Subscribe
	if f.ret != nil {
		watch = f.ret.Watch
	}
	// One watch covers both subjects, so one marker shows the end of the
	// kept events (see settle).
	stop, err := watch(f.ctx, f.subject+".>", f.onEvent)
	if err != nil {
		return fmt.Errorf("fleet: watch %s.>: %w", f.subject, err)
	}
	f.stops = []func(){stop}
	f.viewOn = true
	if f.ret != nil {
		f.settle(ctx)
	}
	f.log.Info("fleet view started", "subject", f.subject)
	return nil
}

// settle waits until the watch delivered the kept records. Watch delivers
// the kept events first and each new event after them, so the view sends a
// heartbeat of its own and waits until the watch delivers it.
func (f *Fleet) settle(ctx context.Context) {
	at := time.Now().UTC()
	done := make(chan struct{})
	f.markMu.Lock()
	f.mark, f.markDone = at, done
	f.markMu.Unlock()
	defer func() {
		f.markMu.Lock()
		f.markDone = nil
		f.markMu.Unlock()
	}()
	if err := f.send(ctx, fw.FleetAliveSubject(f.subject, f.name), fw.EvFleetAlive, fw.FleetAlive{Name: f.name, At: at}); err != nil {
		f.log.Warn("the view starts without the retained records", "error", err)
		return
	}
	timer := time.NewTimer(settleMax)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
		f.log.Warn("the retained records did not arrive in time", "wait", settleMax)
	case <-ctx.Done():
	}
}

// onEvent sends each event of the fleet subject to its handler.
func (f *Fleet) onEvent(e fw.Event) error {
	switch e.Type {
	case fw.EvFleetRecord:
		f.onRecord(e)
	case fw.EvFleetAlive:
		f.onAlive(e)
	}
	return nil
}

// onRecord keeps the newest record of each other member.
func (f *Fleet) onRecord(e fw.Event) {
	var rec fw.FleetRecord
	if err := e.Decode(&rec); err != nil {
		f.log.Warn("a record does not decode", "id", e.ID, "error", err)
		return
	}
	if rec.Name == "" || rec.Name == f.name {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if old, ok := f.records[rec.Name]; ok && old.UpdatedAt.After(rec.UpdatedAt) {
		return
	}
	f.records[rec.Name] = &rec
}

// onAlive keeps the newest heartbeat of each other member. Its own
// heartbeat ends the wait of settle.
func (f *Fleet) onAlive(e fw.Event) {
	var hb fw.FleetAlive
	if err := e.Decode(&hb); err != nil {
		f.log.Warn("a heartbeat does not decode", "id", e.ID, "error", err)
		return
	}
	if hb.Name == f.name {
		f.markMu.Lock()
		if f.markDone != nil && hb.At.Equal(f.mark) {
			close(f.markDone)
			f.markDone = nil
		}
		f.markMu.Unlock()
		return
	}
	if hb.Name == "" {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if hb.At.After(f.alive[hb.Name]) {
		f.alive[hb.Name] = hb.At
	}
}

// remote is one other member at one moment. The record is never changed
// after it arrives, so a remote can be read without a lock.
type remote struct {
	rec  *fw.FleetRecord
	seen time.Time
}

// remotes returns the other members whose last sign of life is newer than
// fleet.expire, sorted by name.
func (f *Fleet) remotes(now time.Time) []remote {
	f.mu.RLock()
	out := make([]remote, 0, len(f.records))
	for name, rec := range f.records {
		seen := rec.UpdatedAt
		if a := f.alive[name]; a.After(seen) {
			seen = a
		}
		if now.Sub(seen) > f.expire {
			continue
		}
		out = append(out, remote{rec: rec, seen: seen})
	}
	f.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].rec.Name < out[j].rec.Name })
	return out
}

// state names the state of a member: stopped after a last record, live
// within fw.FleetQuietAfter of its intervals, else quiet. A record without
// an interval is judged by the interval of this process.
func (f *Fleet) state(r remote, now time.Time) string {
	interval := r.rec.Interval
	if interval <= 0 {
		interval = f.interval
	}
	switch {
	case r.rec.Stopped:
		return fw.MemberStopped
	case now.Sub(r.seen) <= fw.FleetQuietAfter*interval:
		return fw.MemberLive
	}
	return fw.MemberQuiet
}

// Members implements fw.Fleet.
func (f *Fleet) Members(ctx context.Context) ([]fw.FleetMember, error) {
	if err := f.startView(ctx); err != nil {
		return nil, err
	}
	self, err := f.self(ctx)
	if err != nil {
		return nil, err
	}
	out := []fw.FleetMember{self}
	now := time.Now()
	for _, r := range f.remotes(now) {
		runOK, roles, err := f.allow(ctx, r.rec, r.rec.Runs, true)
		if err != nil {
			return nil, err
		}
		m := fw.FleetMember{
			Name:       r.rec.Name,
			URL:        r.rec.URL,
			State:      f.state(r, now),
			LastSeen:   r.seen,
			CanForward: f.canForward(r.rec.URL),
			AccessMode: r.rec.Access.Mode,
			Agents:     []fw.FleetAgent{},
		}
		for i, ag := range r.rec.Agents {
			if roles[i] != "" {
				ag.Role = roles[i]
				// The teams that this process knows win over the record.
				if t := teams.OfAgent(teams.Patterns(f.a.Config()), ag.Access); t != nil {
					ag.Teams = t
				}
				m.Agents = append(m.Agents, ag)
			}
		}
		seen := 0
		for i, run := range r.rec.Runs {
			if !runOK[i] {
				continue
			}
			seen++
			if !run.State.Terminal() {
				m.Active++
			}
			if run.State == fw.StateInputRequired {
				m.InputRequired++
			}
		}
		if len(m.Agents) == 0 && seen == 0 && fw.PrincipalFrom(ctx) != nil {
			continue
		}
		out = append(out, m)
	}
	return out, nil
}

// self is this process as a member: its agents and its runs as the
// principal in ctx sees them.
func (f *Fleet) self(ctx context.Context) (fw.FleetMember, error) {
	m := fw.FleetMember{
		Name:       f.name,
		URL:        f.url,
		Self:       true,
		State:      fw.MemberLive,
		LastSeen:   time.Now().UTC(),
		AccessMode: f.a.AccessMode(),
		Agents:     []fw.FleetAgent{},
	}
	profiles, err := f.profiles(ctx)
	if err != nil {
		return m, err
	}
	targets := make([]fw.Target, len(profiles))
	for i, p := range profiles {
		targets[i] = fw.Target{Agent: p.Name}
	}
	roles, err := f.roles(ctx, targets)
	if err != nil {
		return m, err
	}
	for i, p := range profiles {
		if roles[i] == "" {
			continue
		}
		m.Agents = append(m.Agents, fw.FleetAgent{Name: p.Name, Description: p.Description, Access: p.Access, Teams: teams.OfAgent(teams.Patterns(f.a.Config()), p.Access), Role: roles[i]})
	}
	runs, err := f.a.List(ctx)
	if err != nil {
		return m, fmt.Errorf("fleet: list the runs: %w", err)
	}
	for _, r := range runs {
		if !r.State.Terminal() {
			m.Active++
		}
		if r.State == fw.StateInputRequired {
			m.InputRequired++
		}
	}
	return m, nil
}

// Runs implements fw.Fleet.
func (f *Fleet) Runs(ctx context.Context) ([]*fw.Run, error) {
	if err := f.startView(ctx); err != nil {
		return nil, err
	}
	var out []*fw.Run
	for _, r := range f.remotes(time.Now()) {
		ok, _, err := f.allow(ctx, r.rec, r.rec.Runs, false)
		if err != nil {
			return nil, err
		}
		for i, run := range r.rec.Runs {
			if ok[i] {
				out = append(out, withMember(run, r.rec.Name))
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.After(out[j].CreatedAt)
		}
		return out[i].ID > out[j].ID
	})
	return out, nil
}

// Run implements fw.Fleet.
func (f *Fleet) Run(ctx context.Context, id string) (*fw.Run, error) {
	if err := f.startView(ctx); err != nil {
		return nil, err
	}
	owner, found := f.find(id)
	// A run that another member started a moment ago, for example through
	// this view, is in no record yet. Its id holds its start time, so the
	// view waits a little for the record that names it.
	if at, ok := fw.IDTime(id); found == nil && ok && time.Since(at) < freshRun {
		wait := time.NewTimer(freshWait)
		tick := time.NewTicker(50 * time.Millisecond)
		for found == nil {
			select {
			case <-ctx.Done():
			case <-wait.C:
			case <-tick.C:
				owner, found = f.find(id)
				continue
			}
			break
		}
		wait.Stop()
		tick.Stop()
	}
	if found == nil {
		return nil, fmt.Errorf("fleet: run %s: %w", id, fw.ErrRunNotFound)
	}
	ok, _, err := f.allow(ctx, owner, []*fw.Run{found}, false)
	if err != nil {
		return nil, err
	}
	if !ok[0] {
		return nil, fmt.Errorf("fleet: run %s: %w", id, fw.ErrRunNotFound)
	}
	return withMember(found, owner.Name), nil
}

// A run younger than freshRun can be missing from the records for a moment,
// and the view waits up to freshWait for it.
const (
	freshRun  = 10 * time.Second
	freshWait = 3 * time.Second
)

// find returns the newest record that holds the run, and the run.
func (f *Fleet) find(id string) (*fw.FleetRecord, *fw.Run) {
	var owner *fw.FleetRecord
	var found *fw.Run
	for _, r := range f.remotes(time.Now()) {
		for _, run := range r.rec.Runs {
			if run.ID == id && (owner == nil || r.rec.UpdatedAt.After(owner.UpdatedAt)) {
				owner, found = r.rec, run
			}
		}
	}
	return owner, found
}

// withMember returns a copy of a run of another member with Member set.
func withMember(r *fw.Run, member string) *fw.Run {
	c := *r
	c.Member = member
	return &c
}

// allow decides which of runs the principal in ctx can view, and with
// agents set the role of the principal on each agent of the record: "" for
// an agent that the principal cannot view. It decides as the owner
// decides: with the published rules in the rules mode, and else with the
// Authz of this process, which receives the member, the access block, and
// the lineage. A call without a principal sees everything as an owner. A
// decision point that does not answer is an error, so a listing never
// hides an outage.
func (f *Fleet) allow(ctx context.Context, rec *fw.FleetRecord, runs []*fw.Run, agents bool) (runOK []bool, roles []string, err error) {
	runOK = make([]bool, len(runs))
	if agents {
		roles = make([]string, len(rec.Agents))
	}
	p := fw.PrincipalFrom(ctx)
	if p == nil {
		for i := range runOK {
			runOK[i] = true
		}
		for i := range roles {
			roles[i] = roleOwner
		}
		return runOK, roles, nil
	}
	access := map[string]*fw.Access{}
	known := map[string]bool{}
	for _, ag := range rec.Agents {
		access[ag.Name] = ag.Access
		known[ag.Name] = true
	}
	byID := make(map[string]*fw.Run, len(rec.Runs))
	for _, r := range rec.Runs {
		byID[r.ID] = r
	}
	if rec.Access.Mode == fw.AccessModeRules {
		rules := fw.NewRulesAuthz(fw.AccessConfig{Default: rec.Access.Default, Admins: rec.Access.Admins},
			func(name string) (fw.AgentProfile, bool) {
				if !known[name] {
					return fw.AgentProfile{}, false
				}
				return fw.AgentProfile{Name: name, Access: access[name]}, true
			})
		for i, r := range runs {
			runOK[i] = viewRun(ctx, rules, p, r, byID)
		}
		for i, ag := range rec.Agents[:len(roles)] {
			var errs []error
			for _, action := range roleActions {
				errs = append(errs, rules.Allow(ctx, p, action, fw.Target{Agent: ag.Name}))
			}
			roles[i] = roleFrom(errs)
		}
		return runOK, roles, nil
	}
	// external, custom, or a mode that this version does not know: only a
	// decision point of this process can decide. One batch carries every
	// check of the member.
	checks := make([]fw.AuthzCheck, 0, len(runs)+len(roles)*len(roleActions))
	for _, r := range runs {
		checks = append(checks, fw.AuthzCheck{Action: fw.ActionRunView, Target: fw.Target{
			Agent: r.Agent, Run: r, Member: rec.Name, Access: access[r.Agent], Lineage: lineage(r, byID),
		}})
	}
	for _, ag := range rec.Agents[:len(roles)] {
		for _, action := range roleActions {
			checks = append(checks, fw.AuthzCheck{Action: action, Target: fw.Target{
				Agent: ag.Name, Member: rec.Name, Access: ag.Access,
			}})
		}
	}
	errs := f.a.AuthorizeAll(ctx, checks)
	for _, e := range errs {
		if errors.Is(e, fw.ErrAuthzUnavailable) {
			return nil, nil, fmt.Errorf("fleet: decide about the member %s: %w", rec.Name, e)
		}
	}
	for i := range runs {
		runOK[i] = errs[i] == nil
	}
	rest := errs[len(runs):]
	for i := range roles {
		roles[i] = roleFrom(rest[i*len(roleActions) : (i+1)*len(roleActions)])
	}
	return runOK, roles, nil
}

// Roles of a caller on an agent, as the dashboard names them.
const (
	roleOwner    = "owner"
	roleOperator = "operator"
	roleViewer   = "viewer"
)

// roleActions are the checks that give a role, the highest first.
var roleActions = []string{fw.ActionAgentEdit, fw.ActionAgentRun, fw.ActionAgentView}

// roleFrom turns the answers to roleActions into the highest allowed role.
// A principal that can neither edit, run, nor view an agent gets "". An
// agent that the principal cannot view gets "" too, whatever else the
// answers allow.
func roleFrom(errs []error) string {
	if errs[2] != nil {
		return ""
	}
	switch {
	case errs[0] == nil:
		return roleOwner
	case errs[1] == nil:
		return roleOperator
	}
	return roleViewer
}

// roles asks the Authz of this process about agents of this process in one
// batch, and returns the role of the principal in ctx on each.
func (f *Fleet) roles(ctx context.Context, targets []fw.Target) ([]string, error) {
	out := make([]string, len(targets))
	if fw.PrincipalFrom(ctx) == nil {
		for i := range out {
			out[i] = roleOwner
		}
		return out, nil
	}
	checks := make([]fw.AuthzCheck, 0, len(targets)*len(roleActions))
	for _, t := range targets {
		for _, action := range roleActions {
			checks = append(checks, fw.AuthzCheck{Action: action, Target: t})
		}
	}
	errs := f.a.AuthorizeAll(ctx, checks)
	for _, e := range errs {
		if errors.Is(e, fw.ErrAuthzUnavailable) {
			return nil, fmt.Errorf("fleet: decide about the agents: %w", e)
		}
	}
	for i := range out {
		out[i] = roleFrom(errs[i*len(roleActions) : (i+1)*len(roleActions)])
	}
	return out, nil
}

// viewRun applies the rules to a run, and then to its ancestors inside the
// record: whoever sees a run sees its children.
func viewRun(ctx context.Context, rules fw.Authz, p *fw.Principal, r *fw.Run, byID map[string]*fw.Run) bool {
	cur := r
	for depth := 0; cur != nil && depth <= maxLineage; depth++ {
		if rules.Allow(ctx, p, fw.ActionRunView, fw.Target{Agent: cur.Agent, Run: cur}) == nil {
			return true
		}
		if cur.Parent == "" {
			return false
		}
		cur = byID[cur.Parent]
	}
	return false
}

// lineage lists the ancestors of a run inside the record, the parent first.
func lineage(r *fw.Run, byID map[string]*fw.Run) []fw.RunRef {
	var out []fw.RunRef
	for cur, depth := r, 0; cur.Parent != "" && depth < maxLineage; depth++ {
		parent, ok := byID[cur.Parent]
		if !ok {
			break
		}
		out = append(out, fw.RunRef{ID: parent.ID, Agent: parent.Agent, Principal: parent.Principal})
		cur = parent
	}
	return out
}
