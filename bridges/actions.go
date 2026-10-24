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
// process itself, the operator of the bridge, the owner of the duty that
// opened it, and anyone with the operator role on every agent of its
// members, as the page at /ui decides for a run. It returns nil to allow, and an error that wraps
// fw.ErrForbidden, or the error of a decision point that did not answer.
func (s *Store) allows(ctx context.Context, rec *record) error {
	p := fw.PrincipalFrom(ctx)
	if p == nil || rec.operatedBy(p.Subject) {
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

// operatedBy reports whether subject is the operator of the bridge, or
// the owner of the duty that opened it.
func (rec *record) operatedBy(subject string) bool {
	return subject != "" && (subject == rec.Operator || subject == rec.Owner)
}

// listed reports whether the list of the caller in ctx holds a bridge:
// a bridge of the caller, of a team of the caller that the caller can
// see, or, when admin is true, any bridge that the caller can see. The
// caller of a list decides admin once, with isAdmin.
func (s *Store) listed(ctx context.Context, rec *record, admin bool) (bool, error) {
	p := fw.PrincipalFrom(ctx)
	if p == nil || rec.operatedBy(p.Subject) {
		return true, nil
	}
	if !admin && !teams.Shares(rec.Teams, teams.Of(teams.Patterns(s.a.Config()), p)) {
		return false, nil
	}
	err := s.allows(ctx, rec)
	if errors.Is(err, fw.ErrForbidden) {
		return false, nil
	}
	return err == nil, err
}

// isAdmin reports whether the caller in ctx is an admin of the process
// (fw.ActionAdmin): the admins of the access configuration, or whoever
// the control plane names. A decision point that does not answer makes
// the caller no admin, and the rule of the teams decides alone.
func (s *Store) isAdmin(ctx context.Context) bool {
	return s.a.Authorize(ctx, fw.ActionAdmin, fw.Target{}) == nil
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
	unread, more, err := s.b.unread(fctx, rec, subject, read)
	if err != nil {
		return Item{}, err
	}
	runs, err := s.runsOf(fctx, rec.ID)
	if err != nil {
		return Item{}, err
	}
	ms := decorate(rec, runs, s.taskStates(fctx))
	it := Item{Bridge: rec.public(), Read: read, Unread: unread, UnreadMore: more, MemberCount: len(rec.Members), NeedsYou: needsYou(ms)}
	it.Caps.MaxTurns = rec.Caps.turnCap()
	if at, ok := s.b.lastAt(fctx, rec); ok {
		it.LastAt = &at
	}
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
		// The seating of the new seat counts for the guard of a message
		// that comes before its run (seatingOut).
		seat.Seatings, seat.Budget, seat.AgainAt = 1, spec.Budget, &now
		line = system(EventJoined, fmt.Sprintf("%s added %s: %s", s.nameOf(ctx, by), spec.Handle, oneLine(spec.Task, 300)))
		line.Seq, line.At = rec.alloc(), now
		return nil
	})
	if err != nil {
		return Member{}, err
	}
	s.land(ctx, id, line)
	m := *rec.member(spec.Handle)
	if err := s.place(ctx, rec, []Member{m}, nil); err != nil {
		_ = s.postLines(ctx, id, system(EventFailed, fmt.Sprintf("The front failed to seat %s: %s", m.Handle, oneLine(strings.TrimPrefix(err.Error(), "bridges: "), 300))))
	}
	out := s.memberOf(ctx, rec, m.Handle)
	emitMember(id, out)
	return out, nil
}

// place starts a run for each seat in the way of its bridge: a request on
// the queue of its pool (enqueue), or in a spawn bridge one follow-up per
// seat to the manager run that asks it to seat the member. A follow-up
// starts a new turn of a manager run that finished, also of one that a
// person canceled. A spawn bridge whose manager run the view does not
// hold, or whose manager run stopped at a limit of its budget, where a
// follow-up fails at once, gets a new manager run for the seats
// (newManager). inputs holds the input of the run of a seat by handle
// (inputOf).
func (s *Store) place(ctx context.Context, rec *record, seats []Member, inputs map[string]string) error {
	if rec.Placement == PlacementQueue {
		s.enqueue(ctx, rec, seats, inputs)
		return nil
	}
	mgr := s.runOf(s.asFront(ctx), managerRun(rec))
	if mgr == nil || limited(mgr) {
		r, err := s.newManager(ctx, rec, seats, inputs)
		if r == nil {
			return err
		}
		mgr = r
	}
	op := orOperator(rec.Operator)
	for _, m := range seats {
		budget := ""
		if sm := rec.Seats[m.Handle]; sm != nil && sm.Budget != nil && (sm.Budget.MaxTurns > 0 || sm.Budget.Deadline != "") {
			budget = fmt.Sprintf(" Give it max_turns %d and deadline %q.", sm.Budget.MaxTurns, sm.Budget.Deadline)
		}
		text := fmt.Sprintf("Seat %s with spawn_task, with wait false: the agent %s, the input \"[seat %s] %s\", and the instructions between <<< and >>> word for word.%s Then end your turn with the run id of the child.\n<<<\n%s\n>>>",
			m.Handle, m.Agent, m.Handle, oneLine(inputOf(m, inputs), maxTaskBytes), budget, s.instructionsOf(ctx, rec, m))
		content := header(rec, []string{HandleManager}, HandleOperator) + text
		handle := m.Handle
		s.submit(&job{
			name: "the seat " + handle + " of " + rec.ID + " to the manager",
			do: func(ctx context.Context) error {
				return s.send(ctx, mgr, content, fw.MessageFrom{Kind: fw.FromPerson, Name: op, Member: HandleManager}, false)
			},
			fail: func(err error) {
				_ = s.postLines(s.ctx, rec.ID, system(EventFailed, fmt.Sprintf("The manager did not get the seat %s: %s", handle, oneLine(err.Error(), 300))))
			},
		})
	}
	return nil
}

// newManager seats members of a spawn bridge whose manager run the view
// does not hold, or whose manager run stopped at a limit of its budget.
// It reads the record again first, and the starts of this
// replica go one at a time, so of two seatings at once the second one
// finds the run that the first one started. A manager run that the watch
// never saw, such as a run that started a moment ago, gets the follow-ups:
// newManager returns it. Otherwise it starts one manager run with the plan
// of these seats alone, under the idempotency key bridge:<id>:manager:<n>,
// and makes it the manager run of the record, so the watch claims its
// children by their parent and mirrors it. It returns nil then.
func (s *Store) newManager(ctx context.Context, rec *record, seats []Member, inputs map[string]string) (*fw.Run, error) {
	s.startMu.Lock()
	defer s.startMu.Unlock()
	fctx := s.asFront(ctx)
	cur, _, err := s.b.get(fctx, rec.ID)
	if err != nil {
		return nil, err
	}
	if id := managerRun(cur); id != "" {
		r := s.runOf(fctx, id)
		switch {
		case r != nil && !limited(r):
			return r, nil
		case r == nil && (cur.Marks[id] == nil || !cur.Marks[id].Started):
			return &fw.Run{ID: id, Member: cur.Manager.Member}, nil
		}
	}
	n := 0
	cur, err = s.b.update(ctx, rec.ID, func(rec *record) error {
		rec.ManagerStarts++
		n = rec.ManagerStarts
		return nil
	})
	if err != nil {
		return nil, err
	}
	mgr, err := s.startManager(ctx, cur, s.planOf(ctx, cur, seats, inputs), fmt.Sprintf("bridge:%s:manager:%d", rec.ID, n))
	if err != nil {
		return nil, err
	}
	_, err = s.b.update(ctx, rec.ID, func(rec *record) error {
		rec.Manager = mgr
		return nil
	})
	return nil, err
}

// seatAgain starts a new run for a seat whose newest run cannot go on, or
// that nothing seats. It marks the newest run of the seat and the time,
// for the guard of giveTask (seatingOut).
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
	return s.place(ctx, rec, []Member{*rec.member(handle)}, nil)
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
		if sm := rec.Seats[handle]; sm != nil {
			sm.Ask, sm.AskOpen = 0, false
		}
		line = system(EventFired, fmt.Sprintf("%s fired %s", s.nameOf(ctx, by), handle))
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

// MemberPatch changes a seat: muted, and a new task. Continue continues a
// seat whose newest run stopped at a limit of its budget (continueSeat).
type MemberPatch struct {
	Muted    *bool   `json:"muted,omitempty"`
	Task     *string `json:"task,omitempty"`
	Continue bool    `json:"continue,omitempty"`
}

// patchMember mutes, unmutes, or gives a seat a task. A task that differs
// from the task of the record changes the record and posts its line. Any
// patch with a task then acts on the newest run of the seat (giveTask): a
// seat whose newest run failed or was canceled, or that nothing seats,
// gets a new run, also when the text did not change, which is how the page
// seats a stopped member again; a changed task reaches a run that can go
// on as a follow-up of the person, and a seat that works gets no new run.
// Continue goes on with the newest run of a seat that stopped at a limit
// of its budget (continueSeat), and a seat whose newest run did not stop
// at a limit answers ErrConflict.
func (s *Store) patchMember(ctx context.Context, id, handle string, p MemberPatch, by *fw.Principal) (Member, error) {
	var task string
	if p.Task != nil {
		task = strings.TrimSpace(*p.Task)
		switch {
		case task == "":
			return Member{}, invalid("a task needs words")
		case len(task) > maxTaskBytes:
			return Member{}, invalid("a task holds at most %d bytes", maxTaskBytes)
		case p.Continue:
			return Member{}, invalid("continue goes on with the run that stopped; a task alone seats the member again on a new run")
		}
	}
	var stopped *fw.Run
	if p.Continue {
		r, err := s.limitedRun(ctx, id, handle)
		if err != nil {
			return Member{}, err
		}
		stopped = r
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
			lines = append(lines, system(EventMuted, fmt.Sprintf("%s %s %s", s.nameOf(ctx, by), verb, handle)))
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
	if stopped != nil {
		s.continueSeat(ctx, rec, *rec.member(handle), stopped, by)
	}
	out := s.memberOf(ctx, rec, handle)
	emitMember(id, out)
	return out, nil
}

// continueText is the follow-up that continues a seat run that stopped at
// a limit of its budget. Messages for the seat waited in the channel while
// the run was stopped, so the run reads the channel first.
const continueText = "You stopped at a limit of your budget, and the operator gave you more room. Go on with your task from where you stopped. Messages for you may have come while you were stopped, so read the channel with bridge_read first. Your final answer reaches the channel by itself."

// limitedRun returns the newest run of a seat when it stopped at a limit
// of its budget, and ErrConflict otherwise, because the seat then has no
// run to continue: a task seats it again on a new run.
func (s *Store) limitedRun(ctx context.Context, id, handle string) (*fw.Run, error) {
	rec, _, err := s.b.get(ctx, id)
	if err != nil {
		return nil, err
	}
	m := rec.member(handle)
	switch {
	case m == nil:
		return nil, fmt.Errorf("%w: no member %s in the bridge %s", ErrNotFound, handle, id)
	case m.Fired:
		return nil, fmt.Errorf("%w: %s is fired", ErrConflict, handle)
	case rec.State != StateOpen:
		return nil, fmt.Errorf("%w: the bridge %s is closed", ErrConflict, id)
	}
	r := s.runOf(s.asFront(ctx), m.newest())
	if r == nil || !limited(r) {
		return nil, fmt.Errorf("%w: the newest run of %s did not stop at a limit, so there is nothing to continue; a task seats it again on a new run", ErrConflict, handle)
	}
	return r, nil
}

// continueSeat continues the newest run of a seat that stopped at a limit
// of its budget: the channel says so, and one follow-up of the person
// gives the run the allowance of its seat again (grant) and asks it to go
// on. The run keeps its context, which a new run with the task would lose.
// A second continue of the same run within seatAgainWait, before the run
// goes on, does nothing, so a double click continues once.
func (s *Store) continueSeat(ctx context.Context, rec *record, m Member, r *fw.Run, by *fw.Principal) {
	now := s.now().UTC()
	var line []Message
	fresh, err := s.b.update(ctx, rec.ID, func(rec *record) error {
		line = nil
		sm := rec.seat(m.Handle)
		if sm.Continued == r.ID && sm.ContinuedAt != nil && now.Sub(*sm.ContinuedAt) < seatAgainWait {
			return errNoChange
		}
		sm.Continued, sm.ContinuedAt = r.ID, &now
		line = seqLines(rec, []Message{system(EventTask, fmt.Sprintf("%s continued %s", s.nameOf(ctx, by), m.Handle))}, now)
		return nil
	})
	if err != nil {
		s.log.Warn("bridges: post the line of a continue", "bridge", rec.ID, "member", m.Handle, "error", err)
		return
	}
	if len(line) == 0 {
		return // a continue of this run went out a moment ago
	}
	s.land(ctx, rec.ID, line...)
	seq := line[0].Seq
	snd := sender{subject: orOperator(subjectOfPrincipal(by))}
	s.submit(s.deliveryJob(delivery{bridge: rec.ID, seq: seq, handle: m.Handle, run: r,
		content: header(fresh, []string{m.Handle}, snd.name()) + continueText, from: snd.party(m.Handle)}))
}

// giveTask acts on the newest run of a seat after a patch with a task.
// changed is true when the patch changed the task of the record. A seat
// whose newest run failed or was canceled is seated again with the next
// idempotency key, bridge:<id>:<handle>:<n+1>, also when the task did not
// change. A second patch with the same task within seatAgainWait, while
// the new seating has no run in the view yet, seats nothing more, so a
// double click starts one run, and a seating whose job never ran can be
// tried again after the wait. A seat whose newest run no worker of the
// fleet holds, as the channel said with the line lost, and a seat without
// a run that nothing seats (not-started) are seated again in the same way.
// A changed task reaches any other run as a follow-up of the person, and
// an unchanged one changes nothing. A seat without a run that something
// seats gets nothing: its job waits in the queue, or the manager run seats
// it, and the channel holds the line of the task.
func (s *Store) giveTask(ctx context.Context, rec *record, m Member, by *fw.Principal, changed bool) {
	fctx := s.asFront(ctx)
	r := s.runOf(fctx, m.newest())
	gone := r == nil && m.newest() != "" && rec.Seats[m.Handle] != nil && rec.Seats[m.Handle].Gone == m.newest()
	unseated := m.newest() == "" && !willSeat(rec, s.runOf(fctx, managerRun(rec)))
	if r == nil && !gone && !unseated {
		return
	}
	if gone || unseated || r.State == fw.StateFailed || r.State == fw.StateCanceled {
		if !changed {
			if seatingOut(rec.Seats[m.Handle], m, s.now()) {
				return
			}
			_ = s.postLines(ctx, rec.ID, system(EventTask, fmt.Sprintf("%s seated %s again on its task: %s", s.nameOf(ctx, by), m.Handle, oneLine(m.Task, 300))))
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

// seatAgainWait is how long a seating of a seat waits for its run before
// the same patch, or a message, seats the seat once more.
const seatAgainWait = time.Minute

// seatingOut reports whether a seating of the seat went out within
// seatAgainWait for its newest run, or for no run when it has none, so its
// new run comes and reads the channel first.
func seatingOut(sm *seatMark, m Member, now time.Time) bool {
	return sm != nil && sm.Again == m.newest() && sm.AgainAt != nil && now.Sub(*sm.AgainAt) < seatAgainWait
}

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
			if rec.Caps.MaxTurns == 0 {
				// A record of an older front has no cap of turns.
				rec.Caps.MaxTurns = rec.Caps.turnCap()
			}
			before := rec.Caps
			p.Caps.apply(&rec.Caps)
			if problem := checkCaps(rec.Caps); problem != "" {
				return invalid("the caps of the bridge: %s", problem)
			}
			if before != rec.Caps {
				changed = true
				// A raised cap delivers again, and a cap that still holds
				// says so again.
				rec.Fan.Capped, rec.Fan.TurnsCapped, rec.Fan.PairCapped, rec.Fan.MinuteCapped = false, false, nil, time.Time{}
			}
		}
		var l Message
		switch {
		case p.State != nil && *p.State == StateClosed && rec.State == StateOpen:
			rec.State, rec.ClosedAt = StateClosed, &now
			l = system(EventClosed, fmt.Sprintf("%s closed the bridge", s.nameOf(ctx, by)))
			if rec.Delegation.Report {
				rec.Report = &reportMark{AskedAt: now}
				closed = true
			}
		case p.State != nil && *p.State == StateOpen && rec.State == StateClosed:
			rec.State, rec.ClosedAt, rec.Report = StateOpen, nil, nil
			l = system(EventReopened, fmt.Sprintf("%s opened the bridge again", s.nameOf(ctx, by)))
		case changed:
			l = system(EventChanged, fmt.Sprintf("%s changed the settings of the bridge", s.nameOf(ctx, by)))
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
	// A manager run that stopped at a limit fails a follow-up at once, so
	// a new manager run writes the report.
	if mgr := s.runOf(s.asFront(ctx), managerRun(rec)); mgr != nil && !limited(mgr) {
		content := header(rec, []string{HandleManager}, HandleOperator) + ask
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
		_ = s.postLines(ctx, rec.ID, system(EventChanged, "Nobody writes a report: the configuration names no manager"))
		return
	}
	input := header(rec, []string{HandleManager}, HandleOperator) + ask
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
