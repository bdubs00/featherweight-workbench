package duties

// This file holds the Go functions of the duties that the routes and the
// MCP tools share, with the same checks of access and the same errors
// (docs/ARCHITECTURE.md section 22.7). The access rules of a duty are
// those of its agent: the viewers of the agent see the duty, and the
// owners of the agent and the admins change it.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/duties/schedule"
	"github.com/bdubs00/featherweight-workbench/faces"
	"github.com/bdubs00/featherweight-workbench/fleet"
)

// Errors of the functions of the duties.
var (
	// ErrHidden means that the caller cannot view the agent of the duty,
	// or that no duty that the caller can view has the name. A route
	// answers 404, so a caller learns nothing of a duty that it cannot
	// view.
	ErrHidden = errors.New("duties: hidden")
	// ErrReadOnly means that the duty is an entry of workbench.sources,
	// which only fw.yaml changes.
	ErrReadOnly = errors.New("duties: read-only")
	// ErrUpstream means that an outside system failed, such as the
	// address of a check in a test. A route answers 502.
	ErrUpstream = errors.New("duties: an outside system failed")
)

// ProblemsError is a duty that does not fit, with each problem in words.
type ProblemsError struct{ Problems []string }

func (e *ProblemsError) Error() string { return strings.Join(e.Problems, " ") }

// Sources of a duty in a read.
const (
	SourceStore = "store" // a duty of the bucket
	SourceFile  = "file"  // an entry of workbench.sources, which the routes never change
)

// View is a duty as a read gives it: the record, where it comes from, its
// revision, its readback, its next five times, and its state. History
// holds the last 50 starts, newest first, in a read of one duty.
type View struct {
	Duty
	Source   string      `json:"source"`
	Revision uint64      `json:"revision"`
	Readback string      `json:"readback"`
	Next     []time.Time `json:"next"`
	State    State       `json:"state"`
	History  []RunEntry  `json:"history,omitempty"`
}

// State is the state of a duty in a read: its last start, what it spent
// today in its time zone, the failed runs in a row, and why it paused
// itself.
type State struct {
	LastRun       *RunEntry `json:"lastRun"`
	TodayUSD      float64   `json:"todayUSD"`
	FailuresInRow int       `json:"failuresInRow"`
	PausedReason  string    `json:"pausedReason"`
}

// ListAnswer is the answer of a list: the duties that the caller can view
// and the switch that pauses every duty.
type ListAnswer struct {
	Duties []View `json:"duties"`
	Paused bool   `json:"paused"`
	Reason string `json:"reason,omitempty"`
}

// DutyBody is the body of a write: a duty, as a person writes it or as a
// read gives it. Paused is optional, so a write that leaves it out keeps
// the pause of the duty. Revision makes the write a compare-and-swap. The
// fields that only a read has are taken and left out.
type DutyBody struct {
	Duty
	Paused   *bool           `json:"paused,omitempty"`
	Revision uint64          `json:"revision,omitempty"`
	Source   string          `json:"source,omitempty"`
	Readback string          `json:"readback,omitempty"`
	Next     json.RawMessage `json:"next,omitempty"`
	State    json.RawMessage `json:"state,omitempty"`
	History  json.RawMessage `json:"history,omitempty"`
}

// PutOptions are the options of a write. DryRun saves nothing and
// answers the readback, the next five times, and every problem. Test runs
// the check once in a dry run. Zone is the time zone of a new schedule
// that names none: the header X-Time-Zone of the page, or the argument
// timeZone of the tool.
type PutOptions struct {
	DryRun bool
	Test   bool
	Zone   string
}

// PutAnswer is the answer of a write: the duty after a write, with Created
// for a new duty, or the answer of a dry run.
type PutAnswer struct {
	Created bool
	View    *View
	Dry     *DryAnswer
}

// DryAnswer is the answer of a dry run: the duty as the write would store
// it, its readback, its next five times, every problem, and with a test
// what the check finds now.
type DryAnswer struct {
	Duty     Duty        `json:"duty"`
	Readback string      `json:"readback"`
	Next     []time.Time `json:"next"`
	Problems []string    `json:"problems"`
	Check    *TestAnswer `json:"check,omitempty"`
}

// caller names who acts in ctx: the subject and the kind of the
// principal, else fw-workbench for the process itself.
func caller(ctx context.Context) (string, string) {
	if p := fw.PrincipalFrom(ctx); p != nil && p.Subject != "" {
		return p.Subject, p.Kind
	}
	return "fw-workbench", ""
}

// admin reports whether the caller in ctx is an admin of the process
// (fw.ActionAdmin), or the process itself.
func (s *Service) admin(ctx context.Context) bool {
	return s.a.Authorize(ctx, fw.ActionAdmin, fw.Target{}) == nil
}

// allowed decides an action of the kernel on the agent of a duty for the
// caller in ctx, as the faces and the routines decide (faces.Check). An
// admin can do every action. A caller that cannot view the agent gets
// ErrHidden; one that can view it but not do the action gets the refusal
// of the kernel.
func (s *Service) allowed(ctx context.Context, agent, action string) error {
	if s.admin(ctx) {
		return nil
	}
	err := faces.Check(ctx, s.a, agent, fw.ActionAgentView)
	switch {
	case errors.Is(err, fw.ErrProfileNotFound), errors.Is(err, fw.ErrForbidden), errors.Is(err, fw.ErrInvalidRequest):
		return ErrHidden
	case err != nil:
		return err
	case action == fw.ActionAgentView:
		return nil
	}
	return faces.Check(ctx, s.a, agent, action)
}

// hidden returns ErrHidden with the name of the duty.
func hidden(name string) error {
	return fmt.Errorf("%w: no duty that you can view is named %s", ErrHidden, name)
}

// readOnly returns ErrReadOnly with the name of the duty.
func readOnly(name string) error {
	return fmt.Errorf("%w: the duty %s is an entry of workbench.sources in fw.yaml, so it changes there", ErrReadOnly, name)
}

// load reads a duty of the store for the caller in ctx, who needs the
// action on its agent.
func (s *Service) load(ctx context.Context, name, action string) (*stored, error) {
	if s.fileOf(name) != nil {
		if action != fw.ActionAgentView {
			return nil, readOnly(name)
		}
	}
	st, err := s.st.get(ctx, name)
	if errors.Is(err, ErrNotFound) {
		return nil, hidden(name)
	}
	if err != nil {
		return nil, err
	}
	if err := s.allowed(ctx, st.Duty.Agent, action); err != nil {
		if errors.Is(err, ErrHidden) {
			return nil, hidden(name)
		}
		return nil, err
	}
	return st, nil
}

// List returns the duties whose agent the caller in ctx can view, with
// the entries of workbench.sources, by name, and the switch.
func (s *Service) List(ctx context.Context) (ListAnswer, error) {
	sw, err := s.st.killSwitch(ctx)
	if err != nil {
		return ListAnswer{}, err
	}
	list, err := s.st.list(ctx)
	if err != nil {
		return ListAnswer{}, err
	}
	seen := map[string]bool{}
	visible := func(agent string) (bool, error) {
		if ok, done := seen[agent]; done {
			return ok, nil
		}
		err := s.allowed(ctx, agent, fw.ActionAgentView)
		if err != nil && !errors.Is(err, ErrHidden) {
			return false, err
		}
		seen[agent] = err == nil
		return err == nil, nil
	}
	out := []View{}
	for _, st := range list {
		ok, err := visible(st.Duty.Agent)
		if err != nil {
			return ListAnswer{}, err
		}
		if !ok {
			continue
		}
		v, err := s.view(ctx, st, false)
		if err != nil {
			return ListAnswer{}, err
		}
		out = append(out, v)
	}
	for _, v := range s.files() {
		ok, err := visible(v.Agent)
		if err != nil {
			return ListAnswer{}, err
		}
		if ok {
			out = append(out, v)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return ListAnswer{Duties: out, Paused: sw.Paused, Reason: sw.Reason}, nil
}

// Get returns one duty with its history for the caller in ctx.
func (s *Service) Get(ctx context.Context, name string) (View, error) {
	if v := s.fileOf(name); v != nil {
		if err := s.allowed(ctx, v.Agent, fw.ActionAgentView); err != nil {
			if errors.Is(err, ErrHidden) {
				return View{}, hidden(name)
			}
			return View{}, err
		}
		return *v, nil
	}
	st, err := s.load(ctx, name, fw.ActionAgentView)
	if err != nil {
		return View{}, err
	}
	return s.view(ctx, st, true)
}

// view builds the read of a duty of the store.
func (s *Service) view(ctx context.Context, st *stored, withHistory bool) (View, error) {
	d := st.Duty
	now := s.now()
	v := View{Duty: d, Source: SourceStore, Revision: st.rev, Readback: readback(&d), Next: next(&d, now, 5)}
	v.State.PausedReason = st.PausedReason
	c, err := s.st.counters(ctx, d.Name)
	if err != nil {
		return View{}, err
	}
	v.State.FailuresInRow = c.FailuresInRow
	n := 1
	if withHistory {
		n = 50
	}
	hist, err := s.st.history(ctx, d.Name, n)
	if err != nil {
		return View{}, err
	}
	if len(hist) > 0 {
		last := hist[0]
		v.State.LastRun = &last
	}
	if withHistory {
		v.History = hist
	}
	today, err := s.st.day(ctx, d.Name, dateOf(now, d.zone()))
	if err != nil {
		return View{}, err
	}
	v.State.TodayUSD = today.spent()
	return v, nil
}

// Put creates or changes a duty for the caller in ctx, who needs the
// owner role on the agent of the duty, on the old agent too for a change
// of agent. The owner of a new duty is the caller, and a write never takes
// the owner from the body. runAs owner holds only for a write of the
// owner: a write of another person turns it back to duty. A dry run saves
// nothing and answers every problem; a write with a problem fails with a
// *ProblemsError.
func (s *Service) Put(ctx context.Context, name string, body DutyBody, opt PutOptions) (PutAnswer, error) {
	d := body.Duty
	switch strings.TrimSpace(d.Name) {
	case "", name:
		d.Name = name
	default:
		return PutAnswer{}, &ProblemsError{Problems: []string{problem("name", "The duty is named %s in the body and %s in the path.", d.Name, name)}}
	}
	if err := CheckName(name); err != nil {
		return PutAnswer{}, &ProblemsError{Problems: []string{problem("name", "%s.", capital(err.Error()))}}
	}
	if s.fileOf(name) != nil {
		return PutAnswer{}, readOnly(name)
	}
	old, err := s.st.get(ctx, name)
	switch {
	case errors.Is(err, ErrNotFound):
		old = nil
	case err != nil:
		return PutAnswer{}, err
	}
	if old != nil {
		if err := s.allowed(ctx, old.Duty.Agent, fw.ActionAgentEdit); err != nil {
			if errors.Is(err, ErrHidden) {
				return PutAnswer{}, hidden(name)
			}
			return PutAnswer{}, err
		}
	}
	if agent := strings.TrimSpace(d.Agent); agent != "" && (old == nil || agent != old.Duty.Agent) {
		if err := s.allowed(ctx, agent, fw.ActionAgentEdit); err != nil {
			if errors.Is(err, ErrHidden) {
				return PutAnswer{}, fmt.Errorf("%w: no agent that you can view is named %s", ErrHidden, agent)
			}
			return PutAnswer{}, err
		}
	}
	by, kind := caller(ctx)
	d.Owner = by
	if old != nil {
		d.Owner = old.Duty.Owner
	}
	switch {
	case body.Paused != nil:
		d.Paused = *body.Paused
	case old != nil:
		d.Paused = old.Duty.Paused
	default:
		d.Paused = false
	}
	now := s.now()
	var problems []string
	zone := strings.TrimSpace(opt.Zone)
	if zone != "" {
		if loc, err := time.LoadLocation(zone); err != nil || loc == nil || zone == "Local" {
			problems = append(problems, problem("when.schedule.timeZone", "The time zone %q of the caller is not an IANA time zone, such as America/New_York.", zone))
			zone = ""
		}
	}
	if zone == "" && old != nil && old.Duty.When.Schedule != nil {
		zone = old.Duty.When.Schedule.TimeZone
	}
	d.normalize(zone)
	if say := strings.TrimSpace(d.When.Say); say != "" && d.When.starts() == 1 {
		sch, err := schedule.Parse(say, schedule.Options{Zone: zone, Now: now})
		if err != nil {
			problems = append(problems, problem("when.say", "%s", err.Error()))
		} else {
			d.When.Schedule, d.When.Say = &sch, ""
		}
	}
	if sch := d.When.Schedule; sch != nil && sch.Repeat == schedule.RepeatWeekly && sch.EveryWeeks > 1 && sch.Start == "" {
		sch.Start = s.anchor(old, *sch, now)
	}
	if d.RunAs == RunAsOwner && by != d.Owner {
		d.RunAs = RunAsDuty
	}
	problems = append(problems, d.problems()...)
	problems = append(problems, s.contextProblems(ctx, &d)...)
	if opt.DryRun {
		return s.dryRun(ctx, d, problems, opt.Test, now)
	}
	if len(problems) > 0 {
		return PutAnswer{}, &ProblemsError{Problems: problems}
	}
	if old == nil {
		if body.Revision != 0 {
			return PutAnswer{}, fmt.Errorf("%w: no duty is named %s; leave out revision to create it", ErrConflict, name)
		}
		st := &stored{Duty: d, OwnerKind: kind, Since: now.UTC(), Created: now.UTC(), Changed: now.UTC(), ChangedBy: by}
		if err := s.st.create(ctx, st); err != nil {
			return PutAnswer{}, err
		}
		s.log.Info("duties: a duty was created", "duty", name, "agent", d.Agent, "by", by)
		changed(name)
		s.wake()
		v, err := s.view(ctx, st, false)
		return PutAnswer{Created: true, View: &v}, err
	}
	resumed := false
	st, err := s.st.update(ctx, name, body.Revision, func(x *stored) error {
		prev := x.Duty
		nd := d
		nd.Owner = prev.Owner
		if nd.RunAs == RunAsOwner && by != prev.Owner {
			nd.RunAs = RunAsDuty
		}
		resumed = prev.Paused && !nd.Paused
		if resumed || !reflect.DeepEqual(prev.When, nd.When) {
			x.Since = now.UTC()
		}
		if prev.Paused != nd.Paused {
			x.PausedReason = ""
		}
		x.Duty, x.Changed, x.ChangedBy = nd, now.UTC(), by
		return nil
	})
	if err != nil {
		return PutAnswer{}, err
	}
	if resumed {
		s.resetCounters(ctx, name)
	}
	s.log.Info("duties: a duty was changed", "duty", name, "agent", d.Agent, "by", by, "revision", st.rev)
	changed(name)
	s.wake()
	v, err := s.view(ctx, st, false)
	return PutAnswer{View: &v}, err
}

// anchor returns the start of a schedule of several weeks that names
// none: the start of the duty when only the start differs, so a change of
// the message keeps the weeks, else today in the zone of the schedule.
func (s *Service) anchor(old *stored, sch schedule.Schedule, now time.Time) string {
	if old != nil && old.Duty.When.Schedule != nil && old.Duty.When.Schedule.Start != "" {
		prev := *old.Duty.When.Schedule
		start := prev.Start
		prev.Start = ""
		if reflect.DeepEqual(prev, sch) {
			return start
		}
	}
	loc, err := time.LoadLocation(sch.TimeZone)
	if err != nil {
		loc = time.UTC
	}
	return dateOf(now, loc)
}

// dryRun answers a dry run: the duty, its readback, its next five times,
// and every problem, and with test what the check finds now.
func (s *Service) dryRun(ctx context.Context, d Duty, problems []string, test bool, now time.Time) (PutAnswer, error) {
	ans := &DryAnswer{Duty: d, Next: next(&d, now, 5), Problems: problems}
	if ans.Problems == nil {
		ans.Problems = []string{}
	}
	if d.When.Schedule != nil && d.When.Schedule.Check() != nil {
		ans.Readback = ""
	} else {
		ans.Readback = readback(&d)
	}
	if !test {
		return PutAnswer{Dry: ans}, nil
	}
	if d.CheckFirst == nil {
		ans.Problems = append(ans.Problems, problem("checkFirst", "The test runs the check, and the duty has none."))
		return PutAnswer{Dry: ans}, nil
	}
	for _, p := range problems {
		if strings.HasPrefix(p, "checkFirst") {
			// The check does not fit, so the test does not run it.
			return PutAnswer{Dry: ans}, nil
		}
	}
	t, err := s.testCheck(ctx, &d)
	switch {
	case errors.Is(err, ErrNotAvailable):
		ans.Problems = append(ans.Problems, problem("checkFirst.look.tool", "Not available yet: %s.", strings.TrimPrefix(err.Error(), ErrNotAvailable.Error()+": ")))
	case err != nil:
		return PutAnswer{}, fmt.Errorf("%w: the check failed: %s", ErrUpstream, strings.TrimSuffix(err.Error(), "."))
	default:
		ans.Check = t
	}
	return PutAnswer{Dry: ans}, nil
}

// contextProblems lists what does not fit in a duty beside the record: an
// agent that the fleet does not know or cannot queue for, a part that is
// not available yet, and a secret or a host that the rules of the
// operator refuse.
func (s *Service) contextProblems(ctx context.Context, d *Duty) []string {
	var p []string
	front := fw.WithPrincipal(ctx, nil)
	if d.Agent != "" {
		err := faces.Check(front, s.a, d.Agent, fw.ActionAgentView)
		switch {
		case errors.Is(err, fw.ErrProfileNotFound), errors.Is(err, fw.ErrInvalidRequest):
			p = append(p, problem("agent", "%s is not an agent of this front or of a pool of the fleet.", d.Agent))
		case err == nil && d.Do.Ask != nil:
			if _, err := fleet.QueueFor(front, s.a, d.Agent, d.Pool); err != nil {
				p = append(p, problem("agent", "%s has no queue: %s.", d.Agent, reasonOf(err)))
			}
		}
	}
	if e := d.When.Event; e != nil {
		if check := eventsCheck(); check == nil {
			p = append(p, problem("when.event", "Not available yet: this front starts a duty on no event of the fleet. Use a schedule or a webhook for now."))
		} else {
			p = append(p, check(e)...)
		}
	}
	if name := d.Do.Name(); name != "" && name != ActionAsk {
		act, ok := actionOf(name)
		switch {
		case !ok:
			_, served, _ := Offered()
			p = append(p, problem("do."+name, "Not available yet: this front serves %s.", strings.Join(served, ", ")))
		case act.Check != nil:
			p = append(p, act.Check(ctx, s.a, d)...)
		}
	}
	p = append(p, s.checkContextProblems(ctx, d.CheckFirst)...)
	if w := d.When.Webhook; w != nil && w.Auth.Secret != "" && envPattern.MatchString(w.Auth.Secret) {
		p = append(p, s.cfg.webhookProblems(w)...)
		if s.cfg.secretRule(w.Auth.Secret) != nil {
			if _, err := s.secret(ctx, w.Auth.Secret); err != nil {
				p = append(p, problem("when.webhook.auth.secret", "%s holds no secret in this front.", w.Auth.Secret))
			}
		}
	}
	return p
}

// checkContextProblems lists what the rules of the operator refuse in a
// check: a secret or a host of an address that the rules refuse. A write
// keeps a look that this front does not serve yet, and its test says so.
func (s *Service) checkContextProblems(ctx context.Context, c *CheckFirst) []string {
	var p []string
	if c == nil {
		return nil
	}
	if c.Look.HTTP != nil && c.Look.Tool == "" && hostOf(c.Look.HTTP.URL) != "" {
		h := c.Look.HTTP
		p = append(p, s.cfg.lookProblems(h)...)
		if h.Secret != "" && s.cfg.secretRule(h.Secret) != nil {
			if _, err := s.secret(ctx, h.Secret); err != nil {
				p = append(p, problem("checkFirst.look.http.secret", "%s holds no secret in this front.", h.Secret))
			}
		}
	}
	return p
}

// SetPaused pauses or resumes a duty for the caller in ctx, who needs the
// owner role on its agent. A resume ends the rows of failures, and the
// times of the pause do not run.
func (s *Service) SetPaused(ctx context.Context, name string, paused bool) (View, error) {
	if _, err := s.load(ctx, name, fw.ActionAgentEdit); err != nil {
		return View{}, err
	}
	by, _ := caller(ctx)
	now := s.now().UTC()
	resumed := false
	next, err := s.st.update(ctx, name, 0, func(x *stored) error {
		if x.Duty.Paused == paused {
			return errUnchanged
		}
		resumed = x.Duty.Paused && !paused
		x.Duty.Paused, x.PausedReason, x.Changed, x.ChangedBy = paused, "", now, by
		if resumed {
			x.Since = now
		}
		return nil
	})
	switch {
	case errors.Is(err, errUnchanged):
		next, err = s.st.get(ctx, name)
		if err != nil {
			return View{}, err
		}
	case err != nil:
		return View{}, err
	default:
		if resumed {
			s.resetCounters(ctx, name)
		}
		s.log.Info("duties: a duty was paused or resumed", "duty", name, "paused", paused, "by", by)
		changed(name)
		s.wake()
	}
	return s.view(ctx, next, false)
}

// resetCounters ends the rows of failed runs and of failed checks.
func (s *Service) resetCounters(ctx context.Context, name string) {
	if _, err := s.st.modifyCounters(ctx, name, func(c *counters) bool {
		changed := c.FailuresInRow != 0 || c.ChecksFailed != 0
		c.FailuresInRow, c.ChecksFailed = 0, 0
		return changed
	}); err != nil {
		s.log.Warn("duties: the counters of a duty did not change", "duty", name, "error", err)
	}
}

// Delete removes a duty for the caller in ctx, who needs the owner role
// on its agent. Runs that work go on, and the history stays for 30 days.
func (s *Service) Delete(ctx context.Context, name string) error {
	st, err := s.load(ctx, name, fw.ActionAgentEdit)
	if err != nil {
		return err
	}
	if err := s.st.remove(ctx, name, st.rev); err != nil {
		return err
	}
	s.mu.Lock()
	delete(s.cursors, name)
	delete(s.open, name)
	delete(s.seeded, name)
	s.mu.Unlock()
	by, _ := caller(ctx)
	s.log.Info("duties: a duty was removed", "duty", name, "by", by)
	changed(name)
	return nil
}

// RunNow starts a duty now, out of its schedule, for the caller in ctx,
// who needs the owner role on its agent. It runs the check of the duty
// and keeps its overlap and its daily cap. It also runs a paused duty, for
// a test, but not while the switch pauses every duty.
func (s *Service) RunNow(ctx context.Context, name string) (Outcome, error) {
	st, err := s.load(ctx, name, fw.ActionAgentEdit)
	if err != nil {
		return Outcome{}, err
	}
	if off, why := s.switchedOff(ctx); off {
		return Outcome{Skipped: why}, nil
	}
	by, _ := caller(ctx)
	s.log.Info("duties: a person ran a duty now", "duty", name, "by", by)
	return s.begin(ctx, st, Trigger{Kind: StartNow, Slot: s.now().UTC(), Person: by})
}

// PauseAll turns the switch that pauses every duty on or off, for an admin
// of the process. Runs that work go on. The times of the pause do not run.
func (s *Service) PauseAll(ctx context.Context, paused bool, reason string) (Switch, error) {
	if !s.admin(ctx) {
		return Switch{}, fmt.Errorf("%w: the switch of every duty needs the admin role of the process (access.admins)", fw.ErrForbidden)
	}
	by, _ := caller(ctx)
	sw := Switch{Paused: paused, By: by, At: s.now().UTC()}
	if paused {
		sw.Reason = cut(strings.TrimSpace(reason), 500)
	}
	if err := s.st.setSwitch(ctx, sw); err != nil {
		return Switch{}, err
	}
	s.log.Info("duties: the switch of every duty changed", "paused", paused, "reason", sw.Reason, "by", by)
	changed("")
	s.wake()
	return sw, nil
}
