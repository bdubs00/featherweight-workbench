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

// maxLineage bounds a walk up the parents of a run inside a record.
const maxLineage = 64

// remote is one other member at one moment. The record is computed from
// the platforms and never changed after that, so a remote can be read
// without a lock.
type remote struct {
	rec  *Record
	seen time.Time
}

// remotes returns the other members, sorted by name: the Tasks of the
// atespaces, whatever their phase, and the workers that only the stream
// knows while their last event is newer than fleet.expire.
func (f *Fleet) remotes(now time.Time) []remote {
	if f.platform == nil {
		return nil
	}
	var out []remote
	for _, r := range f.platform.records(now, false) {
		if r.rec.State != "" || now.Sub(r.seen) <= f.expire {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].rec.Name < out[j].rec.Name })
	return out
}

// state names the state of a member: the phase of its Task when the ax API
// gives one, else live within QuietAfter intervals of its last event, else
// quiet.
func (f *Fleet) state(r remote, now time.Time) string {
	switch {
	case r.rec.State != "":
		return r.rec.State
	case now.Sub(r.seen) <= QuietAfter*f.interval:
		return MemberLive
	}
	return MemberQuiet
}

// Members implements View.
func (f *Fleet) Members(ctx context.Context) ([]Member, error) {
	self, err := f.self(ctx)
	if err != nil {
		return nil, err
	}
	out := []Member{self}
	now := time.Now()
	for _, r := range f.remotes(now) {
		runOK, roles, err := f.allow(ctx, r.rec, r.rec.Runs, true)
		if err != nil {
			return nil, err
		}
		m := Member{
			Name:       r.rec.Name,
			URL:        r.rec.URL,
			State:      f.state(r, now),
			LastSeen:   r.seen,
			CanForward: f.canForward(r.rec.URL),
			AccessMode: r.rec.Access.Mode,
			Agents:     []RecordAgent{},
			// The reason comes from the status of the Task, which the
			// worker redacts, and the front redacts it again (suspend.go).
			BlockedReason: r.rec.BlockedReason,
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
func (f *Fleet) self(ctx context.Context) (Member, error) {
	m := Member{
		Name:       f.name,
		Self:       true,
		State:      MemberLive,
		LastSeen:   time.Now().UTC(),
		AccessMode: f.a.AccessMode(),
		Agents:     []RecordAgent{},
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
		m.Agents = append(m.Agents, RecordAgent{Name: p.Name, Description: p.Description, Access: p.Access, Teams: teams.OfAgent(teams.Patterns(f.a.Config()), p.Access), Role: roles[i]})
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

// Runs implements View.
func (f *Fleet) Runs(ctx context.Context) ([]*fw.Run, error) {
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

// The fleet lists a batch whole, so the tool batch never reads the cut
// records for it.
var _ BatchView = (*Fleet)(nil)

// Batch implements BatchView.
func (f *Fleet) Batch(ctx context.Context, batch string) (BatchRuns, error) {
	var out BatchRuns
	if f.platform == nil {
		return out, nil
	}
	for _, r := range f.platform.records(time.Now(), true) {
		var runs []*fw.Run
		for _, run := range r.rec.Runs {
			if run.Metadata[fw.MetaBatch] == batch {
				runs = append(runs, run)
			}
		}
		if len(runs) == 0 {
			continue
		}
		ok, _, err := f.allow(ctx, r.rec, runs, false)
		if err != nil {
			return BatchRuns{}, err
		}
		for i, run := range runs {
			if ok[i] {
				out.Runs = append(out.Runs, withMember(run, r.rec.Name))
			}
		}
	}
	sort.Slice(out.Runs, func(i, j int) bool {
		if !out.Runs[i].CreatedAt.Equal(out.Runs[j].CreatedAt) {
			return out.Runs[i].CreatedAt.After(out.Runs[j].CreatedAt)
		}
		return out.Runs[i].ID > out.Runs[j].ID
	})
	// A run without identity can be a run of this job only on a member
	// that holds a run of the job, and it matters only while the job works:
	// once every run of the job that the caller sees is finished, the work
	// of the job is done. So a caller learns nothing of the runs of members
	// where the caller has no run of the job, and a run of someone else
	// keeps no finished job from being finished.
	members, working := map[string]bool{}, false
	for _, r := range out.Runs {
		members[r.Member] = true
		working = working || !r.State.Terminal()
	}
	if working {
		f.platform.mu.Lock()
		out.Unknown, out.UnknownActive = f.platform.unknown(members)
		f.platform.mu.Unlock()
	}
	forgotten := f.platform.forgotten(batch)
	for member, runs := range forgotten {
		ok, _, err := f.allow(ctx, f.forgotRecord(member, runs), runs, false)
		if err != nil {
			return BatchRuns{}, err
		}
		for _, yes := range ok {
			if yes {
				out.Forgotten++
			}
		}
	}
	// The jobs of the caller that no Task took yet are part of the job
	// too: the front published them, and the view holds no run of them.
	out.Jobs, out.Queued, out.JobsShared = f.queuedJobs(ctx, batch, out.Runs, forgotten)
	return out, nil
}

// forgotRecord is a record for the runs that the view dropped from a
// member, so the view authorizes them as it authorizes the runs of a
// record (allow): with the access of this process and its profiles of the
// agents of the runs. The member itself can be gone.
func (f *Fleet) forgotRecord(member string, runs []*fw.Run) *Record {
	cfg := f.a.Config()
	rec := &Record{Name: member, Access: Access{Mode: f.a.AccessMode(), Default: cfg.Access.Default, Admins: cfg.Access.Admins}, Runs: runs}
	seen := map[string]bool{}
	for _, r := range runs {
		if r.Agent == "" || seen[r.Agent] {
			continue
		}
		seen[r.Agent] = true
		ag := RecordAgent{Name: r.Agent}
		if prof, ok := f.a.ProfileNamed(r.Agent); ok {
			ag.Description, ag.Access = prof.Description, prof.Access
		}
		rec.Agents = append(rec.Agents, ag)
	}
	return rec
}

// Run implements View.
func (f *Fleet) Run(ctx context.Context, id string) (*fw.Run, error) {
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
func (f *Fleet) find(id string) (*Record, *fw.Run) {
	var owner *Record
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
func (f *Fleet) allow(ctx context.Context, rec *Record, runs []*fw.Run, agents bool) (runOK []bool, roles []string, err error) {
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
	for _, ag := range rec.Agents {
		access[ag.Name] = ag.Access
	}
	byID := make(map[string]*fw.Run, len(rec.Runs))
	for _, r := range rec.Runs {
		byID[r.ID] = r
	}
	if rec.Access.Mode == fw.AccessModeRules {
		rules := recordRules(rec)
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
