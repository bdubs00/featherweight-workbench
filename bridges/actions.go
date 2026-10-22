package bridges

// This file holds the actions on a bridge that the routes and the tools
// share: a seat added, fired, muted, or given a new task, and the bridge
// changed, closed, or opened again (docs/ARCHITECTURE.md section 18.9).
// Each action changes the record with a compare-and-swap and posts its
// line in the same change.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/teams"
)

// allows decides whether the caller in ctx can see and change a bridge: the
// process itself, the operator of the bridge, and anyone with the
// operator role on every agent of its members, as the page at /ui decides
// for a run. It returns nil to allow, and an error that wraps
// fw.ErrForbidden, or the error of a decision point that did not answer.
func (s *Store) allows(ctx context.Context, rec *record) error {
	p := fw.PrincipalFrom(ctx)
	if p == nil || (p.Subject != "" && p.Subject == rec.Operator) {
		return nil
	}
	seen := map[string]bool{}
	for _, m := range rec.Members {
		if seen[m.Agent] {
			continue
		}
		seen[m.Agent] = true
		ok, err := s.operates(ctx, m.Agent, m.Pool)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("%w: the bridge %s needs its operator or the operator role on every agent of its members, and the agent %s is not yours to operate", fw.ErrForbidden, rec.ID, m.Agent)
		}
	}
	return nil
}

// listed reports whether the list of the caller in ctx holds a bridge:
// a bridge of the caller, or of a team of the caller that the caller can
// see.
func (s *Store) listed(ctx context.Context, rec *record) (bool, error) {
	p := fw.PrincipalFrom(ctx)
	if p == nil || (p.Subject != "" && p.Subject == rec.Operator) {
		return true, nil
	}
	if !teams.Shares(rec.Teams, teams.Of(teams.Patterns(s.a.Config()), p)) {
		return false, nil
	}
	err := s.allows(ctx, rec)
	if errors.Is(err, fw.ErrForbidden) {
		return false, nil
	}
	return err == nil, err
}

// Visible reports whether the caller in ctx can see the bridge id, by the
// rule of the routes (allows). The live stream of bossmode asks it before
// it sends an event of the bridge to a caller. An unknown bridge is not
// visible; an error means that the store or a decision point did not
// answer.
func (s *Store) Visible(ctx context.Context, id string) (bool, error) {
	rec, _, err := s.b.get(s.asFront(ctx), id)
	if errors.Is(err, ErrNotFound) || errors.Is(err, fw.ErrInvalidRequest) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	err = s.allows(ctx, rec)
	if errors.Is(err, fw.ErrForbidden) {
		return false, nil
	}
	return err == nil, err
}

// load reads a record and checks that the caller in ctx can use it.
func (s *Store) load(ctx context.Context, id string) (*record, error) {
	rec, _, err := s.b.get(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := s.allows(ctx, rec); err != nil {
		return nil, err
	}
	return rec, nil
}

// item returns a bridge as the routes answer it to the caller subject:
// the record with the counts of the caller, and the members with their
// presence when members is true.
func (s *Store) item(ctx context.Context, rec *record, subject string, members bool) (Item, error) {
	fctx := s.asFront(ctx)
	read, err := s.b.readOf(fctx, rec.ID, subject)
	if err != nil {
		return Item{}, err
	}
	unread, err := s.b.unread(fctx, rec, subject, read)
	if err != nil {
		return Item{}, err
	}
	runs, err := s.runsOf(fctx, rec.ID)
	if err != nil {
		return Item{}, err
	}
	ms := decorate(rec, runs, s.taskStates(fctx))
	it := Item{Bridge: rec.public(), Read: read, Unread: unread, MemberCount: len(rec.Members), NeedsYou: needsYou(ms)}
	it.Members = nil
	if members {
		it.Members = ms
	}
	return it, nil
}

// memberOf returns one seat with its presence.
func (s *Store) memberOf(ctx context.Context, rec *record, handle string) Member {
	fctx := s.asFront(ctx)
	runs, err := s.runsOf(fctx, rec.ID)
	if err != nil {
		runs = map[string]*fw.Run{}
	}
	for _, m := range decorate(rec, runs, s.taskStates(fctx)) {
		if m.Handle == handle {
			return m
		}
	}
	return Member{}
}

// addMember adds a seat to an open bridge and starts its run.
func (s *Store) addMember(ctx context.Context, id string, spec SeatSpec, by *fw.Principal) (Member, error) {
	spec.Handle, spec.Agent, spec.Task, spec.Pool = strings.TrimSpace(spec.Handle), strings.TrimSpace(spec.Agent), strings.TrimSpace(spec.Task), strings.TrimSpace(spec.Pool)
	if err := checkSeat(spec); err != nil {
		return Member{}, err
	}
	ok, err := s.operates(ctx, spec.Agent, spec.Pool)
	if err != nil {
		return Member{}, err
	}
	if !ok {
		return Member{}, fmt.Errorf("%w: adding the seat %s needs the operator role on the agent %s", fw.ErrForbidden, spec.Handle, spec.Agent)
	}
	pool := s.poolOf(ctx, spec.Agent, spec.Pool)
	now := s.now().UTC()
	var line Message
	rec, err := s.b.update(ctx, id, func(rec *record) error {
		switch {
		case rec.State != StateOpen:
			return fmt.Errorf("%w: the bridge %s is closed, so it takes no member", ErrConflict, id)
		case rec.member(spec.Handle) != nil:
			return fmt.Errorf("%w: the handle %s is taken in this bridge", ErrConflict, spec.Handle)
		case len(rec.Members) >= s.cfg.MaxMembers:
			return invalid("a bridge holds at most %d members (workbench.bridges.maxMembers)", s.cfg.MaxMembers)
		}
		rec.Members = append(rec.Members, Member{Handle: spec.Handle, Agent: spec.Agent, Pool: pool, Task: spec.Task, Runs: []string{}})
		seat := rec.seat(spec.Handle)
		seat.Seatings, seat.Budget = 1, spec.Budget
		line = system(EventJoined, fmt.Sprintf("%s added %s: %s", nameOf(by), spec.Handle, oneLine(spec.Task, 300)))
		line.Seq, line.At = rec.alloc(), now
		return nil
	})
	if err != nil {
		return Member{}, err
	}
	s.land(ctx, id, line)
	m := *rec.member(spec.Handle)
	if err := s.place(ctx, rec, m); err != nil {
		_ = s.postLines(ctx, id, system(EventFailed, fmt.Sprintf("The front failed to seat %s: %s", m.Handle, oneLine(strings.TrimPrefix(err.Error(), "bridges: "), 300))))
	}
	out := s.memberOf(ctx, rec, m.Handle)
	emitMember(id, out)
	return out, nil
}

// place starts a run for a seat in the way of its bridge: a request on
// the queue of its agent, or one follow-up to the manager run of a spawn
// bridge that asks it to seat the member.
func (s *Store) place(ctx context.Context, rec *record, m Member) error {
	if rec.Placement == PlacementQueue {
		s.enqueue(ctx, rec, []Member{m})
		return nil
	}
	mgr := s.runOf(s.asFront(ctx), managerRun(rec))
	if mgr == nil {
		return fmt.Errorf("%w: the manager run of the bridge %s is gone, so nobody seats %s", ErrConflict, rec.ID, m.Handle)
	}
	b := rec.seat(m.Handle).Budget
	budget := ""
	if b != nil && (b.MaxTurns > 0 || b.Deadline != "") {
		budget = fmt.Sprintf(" Give it max_turns %d and deadline %q.", b.MaxTurns, b.Deadline)
	}
	text := fmt.Sprintf("Seat %s with spawn_task, with wait false: the agent %s, the input \"[seat %s] %s\", and the instructions between <<< and >>> word for word.%s Then end your turn with the run id of the child.\n<<<\n%s\n>>>",
		m.Handle, m.Agent, m.Handle, oneLine(m.Task, maxTaskBytes), budget, s.instructionsOf(ctx, rec, m))
	op := orOperator(rec.Operator)
	content := header(rec, []string{HandleManager}, op) + text
	s.submit(&job{
		name: "the seat " + m.Handle + " of " + rec.ID + " to the manager",
		do: func(ctx context.Context) error {
			return s.send(ctx, mgr, content, fw.MessageFrom{Kind: fw.FromPerson, Name: op, Member: HandleManager}, false)
		},
		fail: func(err error) {
			_ = s.postLines(s.ctx, rec.ID, system(EventFailed, fmt.Sprintf("The manager did not get the seat %s: %s", m.Handle, oneLine(err.Error(), 300))))
		},
	})
	return nil
}

// seatAgain starts a new run for a seat whose newest run cannot go on. It
// marks the newest run of the seat and the time, for the guard of giveTask.
func (s *Store) seatAgain(ctx context.Context, id, handle string) error {
	now := s.now().UTC()
	rec, err := s.b.update(ctx, id, func(rec *record) error {
		m := rec.member(handle)
		if m == nil {
			return fmt.Errorf("%w: no member %s in the bridge %s", ErrNotFound, handle, id)
		}
		sm := rec.seat(handle)
		sm.Seatings++
		sm.Again, sm.AgainAt = m.newest(), &now
		return nil
	})
	if err != nil {
		return err
	}
	return s.place(ctx, rec, *rec.member(handle))
}

// fire takes a member off the bridge: it cancels the open runs of the
// seat, marks the seat fired and muted, and posts the line. The Task of
// the agent stays in its pool. A fire of a fired seat changes nothing.
func (s *Store) fire(ctx context.Context, id, handle string, by *fw.Principal) (Member, error) {
	rec, _, err := s.b.get(ctx, id)
	if err != nil {
		return Member{}, err
	}
	m := rec.member(handle)
	if m == nil {
		return Member{}, fmt.Errorf("%w: no member %s in the bridge %s", ErrNotFound, handle, id)
	}
	if m.Fired {
		return s.memberOf(ctx, rec, handle), nil
	}
	now := s.now().UTC()
	var line Message
	fired := false
	rec, err = s.b.update(ctx, id, func(rec *record) error {
		m := rec.member(handle)
		if m.Fired {
			fired = false
			return errNoChange
		}
		fired = true
		m.Fired, m.Muted = true, true
		line = system(EventFired, fmt.Sprintf("%s fired %s. Its Task stays in its pool.", nameOf(by), handle))
		line.Seq, line.At = rec.alloc(), now
		return nil
	})
	if err != nil {
		return Member{}, err
	}
	if fired {
		s.land(ctx, id, line)
		s.cancelSeat(ctx, rec, *rec.member(handle))
	}
	out := s.memberOf(ctx, rec, handle)
	emitMember(id, out)
	return out, nil
}

// cancelSeat cancels the runs of a seat that are not finished, through
// the Task that holds each one; a child of a spawn bridge lives in a child
// Task, and the cancel goes there.
func (s *Store) cancelSeat(ctx context.Context, rec *record, m Member) {
	fctx := s.asFront(ctx)
	runs, err := s.runsOf(fctx, rec.ID)
	if err != nil {
		s.log.Warn("bridges: read the runs of a fired seat", "bridge", rec.ID, "member", m.Handle, "error", err)
		return
	}
	for _, r := range runs {
		owned := false
		for _, id := range m.Runs {
			owned = owned || id == r.ID
		}
		if r.Metadata[MetaMember] == m.Handle {
			owned = true
		}
		if !owned || r.State.Terminal() {
			continue
		}
		s.submitCancel(rec.ID, m.Handle, r)
	}
}

// submitCancel cancels one run in the background, with the retries of the
// fan-out.
func (s *Store) submitCancel(bridge, handle string, r *fw.Run) {
	s.mu.Lock()
	if s.canceled[r.ID] {
		s.mu.Unlock()
		return
	}
	s.canceled[r.ID] = true
	s.mu.Unlock()
	s.submit(&job{
		name: "the cancel of run " + r.ID + " of " + handle,
		do:   func(ctx context.Context) error { return s.cancelRun(ctx, r) },
		fail: func(err error) {
			s.log.Warn("bridges: a run of a fired seat did not stop", "bridge", bridge, "member", handle, "run", r.ID, "error", err)
			_ = s.postLines(s.ctx, bridge, system(EventFailed, fmt.Sprintf("The run %s of %s did not stop: %s", r.ID, handle, oneLine(err.Error(), 300))))
		},
	})
}

// MemberPatch changes a seat: muted, and a new task.
type MemberPatch struct {
	Muted *bool   `json:"muted,omitempty"`
	Task  *string `json:"task,omitempty"`
}

// patchMember mutes, unmutes, or gives a seat a task. A task that differs
// from the task of the record changes the record and posts its line. Any
// patch with a task then acts on the newest run of the seat (giveTask): a
// seat whose newest run failed or was canceled gets a new run, also when
// the text did not change, which is how the page seats a stopped member
// again; a changed task reaches a run that can go on as a follow-up of the
// person, and a seat that works gets no new run.
func (s *Store) patchMember(ctx context.Context, id, handle string, p MemberPatch, by *fw.Principal) (Member, error) {
	var task string
	if p.Task != nil {
		task = strings.TrimSpace(*p.Task)
		switch {
		case task == "":
			return Member{}, invalid("a task needs words")
		case len(task) > maxTaskBytes:
			return Member{}, invalid("a task holds at most %d bytes", maxTaskBytes)
		}
	}
	now := s.now().UTC()
	var lines []Message
	newTask := false
	rec, err := s.b.update(ctx, id, func(rec *record) error {
		lines, newTask = nil, false
		m := rec.member(handle)
		switch {
		case m == nil:
			return fmt.Errorf("%w: no member %s in the bridge %s", ErrNotFound, handle, id)
		case m.Fired:
			return fmt.Errorf("%w: %s is fired", ErrConflict, handle)
		}
		if p.Muted != nil && *p.Muted != m.Muted {
			m.Muted = *p.Muted
			verb := "unmuted"
			if m.Muted {
				verb = "muted"
			}
			lines = append(lines, system(EventMuted, fmt.Sprintf("%s %s %s.", nameOf(by), verb, handle)))
		}
		if p.Task != nil && task != m.Task {
			m.Task, newTask = task, true
			lines = append(lines, system(EventTask, fmt.Sprintf("%s has a new task: %s", handle, oneLine(task, 300))))
		}
		if len(lines) == 0 {
			return errNoChange
		}
		seqLines(rec, lines, now)
		return nil
	})
	if err != nil {
		return Member{}, err
	}
	s.land(ctx, id, lines...)
	if p.Task != nil {
		s.giveTask(ctx, rec, *rec.member(handle), by, newTask)
	}
	out := s.memberOf(ctx, rec, handle)
	emitMember(id, out)
	return out, nil
}

// giveTask acts on the newest run of a seat after a patch with a task.
// changed is true when the patch changed the task of the record. A seat
// whose newest run failed or was canceled is seated again with the next
// idempotency key, bridge:<id>:<handle>:<n+1>, also when the task did not
// change. A second patch with the same task within seatAgainWait, while
// the new seating has no run in the view yet, seats nothing more, so a
// double click starts one run, and a seating whose job never ran can be
// tried again after the wait. A changed task
// reaches any other run as a follow-up of the person, and an unchanged
// one changes nothing. A seat without a run gets nothing: its job waits in
// the queue, and the channel holds the line of the task.
func (s *Store) giveTask(ctx context.Context, rec *record, m Member, by *fw.Principal, changed bool) {
	r := s.runOf(s.asFront(ctx), m.newest())
	if r == nil {
		return
	}
	if r.State == fw.StateFailed || r.State == fw.StateCanceled {
		if !changed {
			sm := rec.seat(m.Handle)
			if sm.Again != "" && sm.Again == m.newest() && sm.AgainAt != nil && s.now().Sub(*sm.AgainAt) < seatAgainWait {
				return
			}
			_ = s.postLines(ctx, rec.ID, system(EventTask, fmt.Sprintf("%s seated %s again on its task: %s", nameOf(by), m.Handle, oneLine(m.Task, 300))))
		}
		if err := s.seatAgain(ctx, rec.ID, m.Handle); err != nil {
			_ = s.postLines(ctx, rec.ID, system(EventFailed, fmt.Sprintf("The front failed to seat %s again: %s", m.Handle, oneLine(err.Error(), 300))))
		}
		return
	}
	if !changed {
		return
	}
	snd := sender{subject: orOperator(subjectOfPrincipal(by))}
	s.submit(s.deliveryJob(delivery{bridge: rec.ID, seq: rec.LastSeq, handle: m.Handle, run: r,
		content: header(rec, []string{m.Handle}, snd.name()) + "Your new task: " + m.Task, from: snd.party(m.Handle)}))
}

// seatAgainWait is how long a seating of a seat with the same task waits
// for its run before the same patch seats the seat once more.
const seatAgainWait = time.Minute

// subjectOfPrincipal is the subject of a principal, or "".
func subjectOfPrincipal(p *fw.Principal) string {
	if p == nil {
		return ""
	}
	return p.Subject
}

// BridgePatch changes a bridge: its title, its delegation and its caps in
// part, and its state, closed or open.
type BridgePatch struct {
	Title      *string          `json:"title,omitempty"`
	Delegation *DelegationPatch `json:"delegation,omitempty"`
	Caps       *CapsPatch       `json:"caps,omitempty"`
	State      *string          `json:"state,omitempty"`
}

// patch changes a bridge. A close marks the record closed, stops the
// fan-out, and cancels nothing; with delegation.report it marks the wait
// for the report and reports true, so the caller asks the manager for it
// (askReport), unless the manager closed the bridge itself. An open after
// a close opens the bridge again.
func (s *Store) patch(ctx context.Context, id string, p BridgePatch, by *fw.Principal) (*record, bool, error) {
	var title string
	if p.Title != nil {
		title = strings.TrimSpace(*p.Title)
		switch {
		case title == "":
			return nil, false, invalid("a bridge needs a title")
		case utf8.RuneCountInString(title) > maxTitleRunes:
			return nil, false, invalid("the title of a bridge holds at most %d characters", maxTitleRunes)
		}
	}
	if p.State != nil && *p.State != StateOpen && *p.State != StateClosed {
		return nil, false, invalid("the state of a bridge is open or closed, not %q", *p.State)
	}
	now := s.now().UTC()
	var line *Message
	closed := false
	rec, err := s.b.update(ctx, id, func(rec *record) error {
		line, closed = nil, false
		changed := false
		if p.Title != nil && title != rec.Title {
			rec.Title, changed = title, true
		}
		if p.Delegation != nil {
			before := rec.Delegation
			p.Delegation.apply(&rec.Delegation)
			changed = changed || before != rec.Delegation
		}
		if p.Caps != nil {
			before := rec.Caps
			p.Caps.apply(&rec.Caps)
			if problem := checkCaps(rec.Caps); problem != "" {
				return invalid("the caps of the bridge: %s", problem)
			}
			if before != rec.Caps {
				changed = true
				// A raised cap delivers again, and a cap that still holds
				// says so again.
				rec.Fan.Capped, rec.Fan.PairCapped, rec.Fan.MinuteCapped = false, nil, time.Time{}
			}
		}
		var l Message
		switch {
		case p.State != nil && *p.State == StateClosed && rec.State == StateOpen:
			rec.State, rec.ClosedAt = StateClosed, &now
			l = system(EventClosed, fmt.Sprintf("%s closed the bridge. Nothing was canceled.", nameOf(by)))
			if rec.Delegation.Report {
				rec.Report = &reportMark{AskedAt: now}
				closed = true
			}
		case p.State != nil && *p.State == StateOpen && rec.State == StateClosed:
			rec.State, rec.ClosedAt, rec.Report = StateOpen, nil, nil
			l = system(EventReopened, fmt.Sprintf("%s opened the bridge again.", nameOf(by)))
		case changed:
			l = system(EventChanged, fmt.Sprintf("%s changed the settings of the bridge.", nameOf(by)))
		default:
			return errNoChange
		}
		l.Seq, l.At = rec.alloc(), now
		line = &l
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	if line != nil {
		s.land(ctx, id, *line)
	}
	return rec, closed, nil
}

// askReport asks the manager for the report of a closed bridge, in the
// page bridges/<id>/report.md: one follow-up to the manager run of the
// bridge, or a new run of the manager when the bridge has none. The watch
// posts the link when the page appears.
func (s *Store) askReport(ctx context.Context, rec *record) {
	op := orOperator(rec.Operator)
	ask := fmt.Sprintf("The operator closed the bridge. Write the report of the bridge in the wiki page %s with pages_write: what each member did, what is done, what is open, and the runs that hold the work. Read the channel with bridge_read (bridge %s) first. Then end your turn.", reportPath(rec.ID), rec.ID)
	if mgr := s.runOf(s.asFront(ctx), managerRun(rec)); mgr != nil {
		content := header(rec, []string{HandleManager}, op) + ask
		s.submit(&job{
			name: "the report of " + rec.ID,
			do: func(ctx context.Context) error {
				return s.send(ctx, mgr, content, fw.MessageFrom{Kind: fw.FromPerson, Name: op, Member: HandleManager}, false)
			},
			fail: func(err error) {
				_ = s.postLines(s.ctx, rec.ID, system(EventFailed, "The manager did not get the ask for the report: "+oneLine(err.Error(), 300)))
			},
		})
		return
	}
	if s.cfg.Manager == "" {
		_ = s.postLines(ctx, rec.ID, system(EventChanged, "Nobody writes a report: the configuration names no manager (workbench.bossmode.manager)."))
		return
	}
	input := header(rec, []string{HandleManager}, op) + ask
	mgr, err := s.startManager(ctx, rec, input, fmt.Sprintf("bridge:%s:report:%d", rec.ID, rec.LastSeq))
	if err != nil {
		_ = s.postLines(ctx, rec.ID, system(EventFailed, "The front failed to ask the manager for the report: "+oneLine(strings.TrimPrefix(err.Error(), "bridges: "), 300)))
		return
	}
	_, err = s.b.update(ctx, rec.ID, func(rec *record) error {
		if rec.Manager != nil && rec.Manager.Run != "" {
			return errNoChange
		}
		rec.Manager = mgr
		return nil
	})
	if err != nil {
		s.log.Warn("bridges: record the manager run of the report", "bridge", rec.ID, "error", err)
	}
}

// Delegates reports whether the bridge of a batch, bridge:<id>, delegates
// an action to the manager. action is a key of the delegation: approve,
// budget, members, or report, or the name of a tool that it covers:
// run_budget (budget), cancel, bridge_add_member, and bridge_remove_member
// (members), and bridge_close (report). A batch of no bridge, and an
// unknown action, delegate nothing. The manager branch reads it through an
// interface of its own.
func (s *Store) Delegates(ctx context.Context, batch, action string) (bool, error) {
	id, ok := strings.CutPrefix(strings.TrimSpace(batch), BatchPrefix)
	if !ok || checkID(id) != nil {
		return false, nil
	}
	rec, _, err := s.b.get(s.asFront(ctx), id)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	d := rec.Delegation
	switch strings.TrimSpace(action) {
	case "approve":
		return d.Approve, nil
	case "budget", "run_budget":
		return d.Budget, nil
	case "members", "cancel", ToolAddMember, ToolRemoveMember:
		return d.Members, nil
	case "report", ToolClose:
		return d.Report, nil
	}
	return false, nil
}
