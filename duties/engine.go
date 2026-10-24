package duties

// This file is the engine of the duties (docs/ARCHITECTURE.md section
// 22.5). Every replica of the front runs it. At each pass it follows the
// runs of each duty to their end, and it computes the newest time of each
// schedule. A time younger than the catch-up of its duty starts once: the
// replica claims the time in the history (Store.claim), and only the
// winner starts it. A start checks the overlap and the daily cap, runs the
// check of the duty, and puts the run on the queue of the pool of the
// agent with the idempotency key duty/<name>/<time>. A run that fails goes
// again up to the retries of the duty, and three failed runs in a row
// pause the duty.

import (
	"context"
	"errors"
	"fmt"
	"math"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/duties/schedule"
	"github.com/bdubs00/featherweight-workbench/fleet"
)

// Times of the engine.
const (
	// defaultEvery is the longest pause between two passes. A pass also
	// comes at the next time of a schedule and after each write.
	defaultEvery = 15 * time.Second
	// grace is how late a pass can meet a time and still start it, also
	// with a catch-up of 0.
	grace = time.Minute
	// staleStart is how long a start can stay starting before another
	// replica takes it over: its replica stopped in the middle.
	staleStart = 2 * time.Minute
	// lostAfter is how long a start waits for a sign of its run before the
	// front stops waiting for it.
	lostAfter = 2 * time.Hour
	// retryPause is the pause before a failed run goes again.
	retryPause = time.Minute
	// breakerAfter is the number of failures in a row that pause a duty.
	breakerAfter = 3
	// seedDays is how many days of the history a replica reads when it
	// first meets a duty.
	seedDays = 7
	// maxEventBytes bounds the text of {event} that a start keeps.
	maxEventBytes = 4 << 10
	// maxWorking is the most starts of one duty that work at one time, so
	// a burst of calls stays bounded, and so does the entry of its day.
	maxWorking = 40
)

// Kinds of a start.
const (
	StartSchedule = "schedule" // a time of the schedule
	StartNow      = "now"      // a person ran the duty now
	StartCall     = "call"     // another app called the webhook
	StartEvent    = "event"    // an event of the fleet
)

// Trigger is what starts a duty once: its kind, one of the Start
// constants, and Slot, the time of the schedule, else the moment of the
// start. Event is the body of a call or of an event, for {event}. Person
// names who ran the duty now. Chain is the depth of a start in a chain of
// duties that events started, from 1, and 0 for any other start.
type Trigger struct {
	Kind   string
	Slot   time.Time
	Event  string
	Person string
	Chain  int
}

// Outcome is what a start did: the run that it put on the queue, the
// reason why it waits, skipped, or failed, or a duplicate when another
// replica holds the time.
type Outcome struct {
	Run       string `json:"run,omitempty"`
	Waiting   string `json:"waiting,omitempty"`
	Skipped   string `json:"skipped,omitempty"`
	Failed    string `json:"failed,omitempty"`
	Duplicate bool   `json:"duplicate,omitempty"`

	retrying bool // the start goes again later
}

// errUnchanged stops a write that has nothing to change.
var errUnchanged = errors.New("duties: nothing to change")

// loop runs the passes until Close.
func (s *Service) loop(ctx context.Context) {
	defer close(s.done)
	for {
		next := s.pass(ctx)
		wait := min(max(next.Sub(s.now()), 50*time.Millisecond), s.every)
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-s.poke:
			t.Stop()
		case <-t.C:
		}
	}
}

// wake asks for a pass now, after a write.
func (s *Service) wake() {
	select {
	case s.poke <- struct{}{}:
	default:
	}
}

// pass follows the runs of every duty and starts the times that are due.
// It returns the time of the next pass: the next time of a schedule, at
// most s.every away.
func (s *Service) pass(ctx context.Context) time.Time {
	now := s.now()
	next := now.Add(s.every)
	list, err := s.st.list(ctx)
	if err != nil {
		s.log.Warn("duties: the engine did not read the duties", "error", err)
		return next
	}
	sw, err := s.st.killSwitch(ctx)
	if err != nil {
		s.log.Warn("duties: the engine did not read the switch", "error", err)
		return next
	}
	for _, st := range list {
		if ctx.Err() != nil {
			return next
		}
		s.follow(ctx, st, sw, now)
		if st.Duty.When.Schedule == nil {
			continue
		}
		plan, err := st.Duty.When.Schedule.Plan()
		if err != nil {
			s.warnOnce(st.Duty.Name, err.Error())
			continue
		}
		if t, ok := s.schedule(ctx, st, plan, sw, now); ok && t.Before(next) {
			next = t
		}
	}
	return next
}

// warnOnce logs a problem of a duty once, until it changes.
func (s *Service) warnOnce(name, problem string) {
	s.mu.Lock()
	seen := s.warned[name] == problem
	s.warned[name] = problem
	s.mu.Unlock()
	if !seen {
		s.log.Warn("duties: a duty does not run", "duty", name, "problem", problem)
	}
}

// schedule starts the newest time of a duty that is due, or writes that
// it was missed. It returns the next time of the schedule after now.
func (s *Service) schedule(ctx context.Context, st *stored, plan *schedule.Plan, sw Switch, now time.Time) (time.Time, bool) {
	d := &st.Duty
	var next time.Time
	if t := plan.Next(now, 1); len(t) > 0 {
		next = t[0]
	}
	if d.Paused || sw.Paused {
		// The times of a pause pass, and the history holds none of them.
		s.setCursor(d.Name, now)
		return next, !next.IsZero()
	}
	cur, err := s.cursor(ctx, st, sw)
	if err != nil {
		s.log.Warn("duties: the engine did not read the history of a duty", "duty", d.Name, "error", err)
		return next, !next.IsZero()
	}
	last, ok := plan.Last(now)
	if !ok || !last.After(cur) {
		return next, !next.IsZero()
	}
	if now.Sub(last) > max(d.catchUp(), grace) {
		e := s.newEntry(Trigger{Kind: StartSchedule, Slot: last}, now)
		e.State = StateMissed
		e.Reason = fmt.Sprintf("No replica of the front ran this time, and it is older than the catch-up of %s, so it did not run.", durText(d.catchUp()))
		if _, err := s.st.claim(ctx, d.Name, dateOf(last, d.zone()), e); err != nil {
			s.log.Warn("duties: the engine did not write a missed time", "duty", d.Name, "error", err)
			return next, !next.IsZero()
		}
		s.setCursor(d.Name, last)
		changed(d.Name)
		return next, !next.IsZero()
	}
	if _, err := s.begin(ctx, st, Trigger{Kind: StartSchedule, Slot: last}); err != nil {
		s.log.Warn("duties: a time of a duty did not start", "duty", d.Name, "slot", last, "error", err)
		return next, !next.IsZero()
	}
	s.setCursor(d.Name, last)
	return next, !next.IsZero()
}

// cursor returns the newest time of the schedule of a duty that the
// engine handled: from the memory of this replica, else the newest time
// in the history, and never before the moment from which the duty counts
// its times or the end of the last pause of every duty.
func (s *Service) cursor(ctx context.Context, st *stored, sw Switch) (time.Time, error) {
	if err := s.ensureSeeded(ctx, st); err != nil {
		return time.Time{}, err
	}
	s.mu.Lock()
	c := s.cursors[st.Duty.Name]
	s.mu.Unlock()
	floor := st.Since
	if !sw.Paused && sw.At.After(floor) {
		floor = sw.At
	}
	if floor.After(c) {
		c = floor
	}
	return c, nil
}

// setCursor moves the cursor of a duty forward to t.
func (s *Service) setCursor(name string, t time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if t.After(s.cursors[name]) {
		s.cursors[name] = t
	}
}

// ensureSeeded reads the last days of the history of a duty once: the
// newest time of its schedule that ran, and the days that hold a start
// that works.
func (s *Service) ensureSeeded(ctx context.Context, st *stored) error {
	name := st.Duty.Name
	s.mu.Lock()
	done := s.seeded[name]
	s.mu.Unlock()
	if done {
		return nil
	}
	dates, err := s.st.dates(ctx, name)
	if err != nil {
		return err
	}
	if len(dates) > seedDays {
		dates = dates[len(dates)-seedDays:]
	}
	var newest time.Time
	open := map[string]bool{}
	for _, date := range dates {
		d, err := s.st.day(ctx, name, date)
		if err != nil {
			return err
		}
		for _, e := range d.Entries {
			if e.Start == StartSchedule && e.Slot.After(newest) {
				newest = e.Slot
			}
			if !ended(e.State) {
				open[date] = true
			}
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seeded[name] = true
	if newest.After(s.cursors[name]) {
		s.cursors[name] = newest
	}
	for date := range open {
		s.noteOpenLocked(name, date)
	}
	return nil
}

// noteOpen remembers a date of the history of a duty with a start that
// works, so the engine follows it.
func (s *Service) noteOpen(name, date string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.noteOpenLocked(name, date)
}

func (s *Service) noteOpenLocked(name, date string) {
	if s.open[name] == nil {
		s.open[name] = map[string]bool{}
	}
	s.open[name][date] = true
}

// forgetOpen drops a date whose starts all ended.
func (s *Service) forgetOpen(name, date string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.open[name], date)
}

// followDates returns the dates of the history of a duty that the engine reads:
// today and yesterday in the time zone of the duty, and the older dates
// with a start that works, oldest first.
func (s *Service) followDates(name string, loc *time.Location, now time.Time) []string {
	set := map[string]bool{dateOf(now, loc): true, dateOf(now.AddDate(0, 0, -1), loc): true}
	s.mu.Lock()
	for date := range s.open[name] {
		set[date] = true
	}
	s.mu.Unlock()
	out := make([]string, 0, len(set))
	for date := range set {
		out = append(out, date)
	}
	sort.Strings(out)
	return out
}

// recent reads the days that the engine follows.
func (s *Service) recent(ctx context.Context, name string, loc *time.Location, now time.Time) (map[string]day, []string, error) {
	dates := s.followDates(name, loc, now)
	days := make(map[string]day, len(dates))
	for _, date := range dates {
		d, err := s.st.day(ctx, name, date)
		if err != nil {
			return nil, nil, err
		}
		days[date] = d
	}
	return days, dates, nil
}

// newEntry returns a start that a replica claims. The id of a time of the
// schedule is the time, so two replicas claim the same id. Any other start
// gets a random end to its id, so two starts at one moment differ.
func (s *Service) newEntry(start Trigger, now time.Time) entry {
	now = now.UTC()
	id := start.Slot.UTC().Format(time.RFC3339Nano)
	if start.Kind != StartSchedule {
		id += "-" + strings.ToLower(fw.NewID()[20:])
	}
	return entry{
		RunEntry: RunEntry{At: now, Slot: start.Slot.UTC(), State: StateStarting, Start: start.Kind, Attempt: 1},
		ID:       id,
		Event:    cut(start.Event, maxEventBytes),
		Person:   start.Person,
		Chain:    start.Chain,
		By:       s.replica,
		Updated:  now,
	}
}

// stopped reports whether a duty does not start now, and why: it is
// paused, or the switch pauses every duty.
func (s *Service) stopped(ctx context.Context, st *stored) (bool, string) {
	if st.Duty.Paused {
		if st.PausedReason != "" {
			return true, "The duty is paused: " + st.PausedReason
		}
		return true, "The duty is paused."
	}
	return s.switchedOff(ctx)
}

// switchedOff reports whether the switch pauses every duty, and why.
func (s *Service) switchedOff(ctx context.Context) (bool, string) {
	sw, err := s.st.killSwitch(ctx)
	if err != nil || !sw.Paused {
		return false, ""
	}
	why := "Every duty is paused."
	if sw.Reason != "" {
		why = "Every duty is paused: " + sw.Reason
	}
	return true, why
}

// begin starts a duty once: it decides the overlap and the daily cap,
// claims the start in the history, and, for a start that goes on, runs
// the check and the action. An error means that the store did not answer,
// and the start did not happen.
func (s *Service) begin(ctx context.Context, st *stored, start Trigger) (Outcome, error) {
	if s.passive {
		return Outcome{}, ErrNoStarts
	}
	d := &st.Duty
	now := s.now()
	loc := d.zone()
	e := s.newEntry(start, now)
	days, _, err := s.recent(ctx, d.Name, loc, now)
	if err != nil {
		return Outcome{}, err
	}
	if n := countWorking(days); n >= maxWorking {
		e.State, e.Reason = StateSkipped, fmt.Sprintf("%d runs of the duty work now, the most at one time.", n)
	} else if open := lastWorking(days); open != nil {
		switch d.Limits.Overlap {
		case OverlapSkip:
			e.State, e.Reason = StateSkipped, fmt.Sprintf("The run of %s had not ended, and the overlap of the duty is skip.", slotWords(open.Slot, loc))
		case OverlapQueue:
			if waits(days) {
				e.State, e.Reason = StateSkipped, "A start already waits for the run that works, and the overlap of the duty is queue."
			} else {
				e.State, e.Reason = StateWaiting, fmt.Sprintf("It waits for the run of %s to end, as the overlap of the duty is queue.", slotWords(open.Slot, loc))
			}
		}
	}
	capped := ""
	if !ended(e.State) {
		why, err := s.overCaps(ctx, d, days, now)
		if err != nil {
			return Outcome{}, err
		}
		if why != "" {
			capped = why
			e.State, e.Reason = StateSkipped, capped
		}
	}
	ok, err := s.st.claim(ctx, d.Name, dateOf(start.Slot, loc), e)
	if err != nil {
		return Outcome{}, err
	}
	if !ok {
		return Outcome{Duplicate: true}, nil
	}
	if !ended(e.State) {
		s.noteOpen(d.Name, dateOf(start.Slot, loc))
	}
	changed(d.Name)
	if capped != "" {
		s.capNotice(ctx, st, dateOf(now, loc), capped)
	}
	switch e.State {
	case StateSkipped:
		return Outcome{Skipped: e.Reason}, nil
	case StateWaiting:
		return Outcome{Waiting: e.Reason}, nil
	}
	return s.carry(ctx, st, dateOf(start.Slot, loc), e), nil
}

// lastWorking returns the newest start whose run works, or nil.
func lastWorking(days map[string]day) *entry {
	var out *entry
	for _, d := range days {
		for i := range d.Entries {
			e := &d.Entries[i]
			if working(e.State) && (out == nil || e.Slot.After(out.Slot)) {
				out = e
			}
		}
	}
	return out
}

// countWorking counts the starts whose run works.
func countWorking(days map[string]day) int {
	n := 0
	for _, d := range days {
		for _, e := range d.Entries {
			if working(e.State) {
				n++
			}
		}
	}
	return n
}

// waits reports whether a start waits for a run that works.
func waits(days map[string]day) bool {
	for _, d := range days {
		for _, e := range d.Entries {
			if e.State == StateWaiting {
				return true
			}
		}
	}
	return false
}

// overCap reports whether a duty with a daily cap spent it today, in its
// time zone, and what it spent.
func overCap(d *Duty, days map[string]day, now time.Time) (float64, bool) {
	if d.Limits.PerDayUSD <= 0 {
		return 0, false
	}
	today := days[dateOf(now, d.zone())]
	spent := today.spent()
	return spent, spent >= d.Limits.PerDayUSD
}

// capWords says why a start skips at the daily cap.
func capWords(d *Duty, spent float64) string {
	return fmt.Sprintf("The duty spent $%.2f today, and its daily cap is $%.2f, so it skips until midnight, %s.", spent, d.Limits.PerDayUSD, schedule.ZoneWords(d.zone().String()))
}

// overCaps says why a duty does not start now at a daily cap, or "": the
// cap of the duty (perDayUSD), the cap of its agent across its duties
// (workbench.duties.caps.perAgentUSD), or the cap of the fleet across
// every duty (workbench.duties.caps.fleetUSD). Today starts at midnight in
// the time zone of the duty. An error means that the store did not answer.
func (s *Service) overCaps(ctx context.Context, d *Duty, days map[string]day, now time.Time) (string, error) {
	if spent, over := overCap(d, days, now); over {
		return capWords(d, spent), nil
	}
	caps := s.cfg.Caps
	if caps.PerAgentUSD <= 0 && caps.FleetUSD <= 0 {
		return "", nil
	}
	loc := d.zone()
	y, m, dd := now.In(loc).Date()
	midnight := time.Date(y, m, dd, 0, 0, 0, 0, loc)
	agent, fleet, err := s.spentSince(ctx, d.Agent, midnight, now)
	if err != nil {
		return "", err
	}
	zone := schedule.ZoneWords(loc.String())
	if caps.PerAgentUSD > 0 && agent >= caps.PerAgentUSD {
		return fmt.Sprintf("The duties of the agent %s spent $%.2f today, and the daily cap of an agent is $%.2f, so the duty skips until midnight, %s.", d.Agent, agent, caps.PerAgentUSD, zone), nil
	}
	if caps.FleetUSD > 0 && fleet >= caps.FleetUSD {
		return fmt.Sprintf("The duties of the fleet spent $%.2f today, and the daily cap of the fleet is $%.2f, so the duty skips until midnight, %s.", fleet, caps.FleetUSD, zone), nil
	}
	return "", nil
}

// spentSince returns the cost of the starts since a moment: of the duties
// of the agent, and of every duty. A start counts by its slot.
func (s *Service) spentSince(ctx context.Context, agent string, since, now time.Time) (float64, float64, error) {
	list, err := s.st.list(ctx)
	if err != nil {
		return 0, 0, err
	}
	var mine, all float64
	for _, st := range list {
		loc := st.Duty.zone()
		seen := map[string]bool{}
		for t := since; ; t = t.Add(12 * time.Hour) {
			if t.After(now) {
				t = now
			}
			date := dateOf(t, loc)
			if !seen[date] {
				seen[date] = true
				x, err := s.st.day(ctx, st.Duty.Name, date)
				if err != nil {
					return 0, 0, err
				}
				for _, e := range x.Entries {
					if e.Slot.Before(since) {
						continue
					}
					all += e.CostUSD
					if st.Duty.Agent == agent {
						mine += e.CostUSD
					}
				}
			}
			if !t.Before(now) {
				break
			}
		}
	}
	return usd(mine), usd(all), nil
}

// capNotice tells the owner once a day that a duty reached its cap.
func (s *Service) capNotice(ctx context.Context, st *stored, today, reason string) {
	first := false
	_, err := s.st.modifyCounters(ctx, st.Duty.Name, func(c *counters) bool {
		first = c.CapDay != today
		c.CapDay = today
		return first
	})
	if err != nil {
		s.log.Warn("duties: the counters of a duty did not change", "duty", st.Duty.Name, "error", err)
		return
	}
	if first {
		s.notify(ctx, st, Notice{Kind: NoticeCap, Reason: reason})
	}
}

// slotWords writes the time of a start for a person, in its zone.
func slotWords(t time.Time, loc *time.Location) string {
	return t.In(loc).Format("Mon Jan 2, 3:04 PM")
}

// carry runs the check of a start that a replica claimed, and its action
// when the check finds work or the duty has no check.
func (s *Service) carry(ctx context.Context, st *stored, date string, e entry) Outcome {
	d := &st.Duty
	var keys []string
	if d.CheckFirst != nil {
		res, err := s.check(ctx, d)
		if err != nil {
			reason := "The check failed, so the model did not wake: " + strings.TrimSuffix(err.Error(), ".") + "."
			s.endEntry(ctx, st, date, e, []string{StateStarting}, StateCheckFailed, reason)
			s.checkFailed(ctx, st, reason)
			return Outcome{Failed: reason}
		}
		s.checkPassed(ctx, d.Name)
		if !res.Wakes {
			_, wrote, err := s.st.setEntry(ctx, d.Name, date, e.ID, []string{StateStarting}, func(x *entry) bool {
				x.State, x.Found, x.Reason = StateQuiet, len(res.Matched), res.Why
				return true
			})
			if err != nil {
				s.log.Warn("duties: the history of a duty did not change", "duty", d.Name, "error", err)
			}
			if wrote {
				changed(d.Name)
			}
			return Outcome{Skipped: res.Why}
		}
		e.Found, e.Woke, e.FoundText = len(res.Wake), true, res.Text
		keys = res.New
	}
	out := s.act(ctx, st, date, e)
	if len(keys) > 0 && (out.Run != "" || out.retrying) {
		// The run, or its retry, has the items, so a later check does not
		// wake the agent for them again.
		if err := s.st.remember(ctx, d.Name, keys, e.ID); err != nil {
			s.log.Warn("duties: the seen keys of a check did not land", "duty", d.Name, "error", err)
		}
	}
	return out
}

// act carries out the action of a start: for ask, it puts the run on the
// queue of the pool of the agent. The start names its run before the
// publish, so a replica that stops in the middle leaves a start that the
// engine follows by its run.
func (s *Service) act(ctx context.Context, st *stored, date string, e entry) Outcome {
	d := &st.Duty
	req := s.request(st, e, s.lastRun(ctx, d.Name, e.Slot))
	_, wrote, err := s.st.setEntry(ctx, d.Name, date, e.ID, []string{StateStarting}, func(x *entry) bool {
		x.Run, x.Title = req.ID, req.Metadata[fw.MetaTitle]
		x.Found, x.Woke, x.FoundText = e.Found, e.Woke, e.FoundText
		return true
	})
	if err != nil {
		return Outcome{Failed: "The history of the duty did not answer, so the run did not start: " + err.Error() + "."}
	}
	if !wrote {
		return Outcome{Failed: "Another replica took the start over."}
	}
	if name := d.Do.Name(); name != ActionAsk {
		return s.actRegistered(ctx, st, date, e, name, req)
	}
	_, sent, err := s.enqueue(ctx, d.Agent, d.Pool, []fw.RunRequest{req})
	if err == nil && sent == 0 {
		err = errors.New("no request went out")
	}
	if err != nil {
		return s.failAttempt(ctx, st, date, e, []string{StateStarting}, "The queue did not take the run: "+reasonOf(err)+".", false)
	}
	s.queued(ctx, st, date, e, req.ID)
	return Outcome{Run: req.ID}
}

// actRegistered carries out an action that a later package registered.
func (s *Service) actRegistered(ctx context.Context, st *stored, date string, e entry, name string, req fw.RunRequest) Outcome {
	act, ok := actionOf(name)
	if !ok {
		reason := "The action " + name + " is not available yet."
		s.endEntry(ctx, st, date, e, []string{StateStarting}, StateFailed, reason)
		return Outcome{Failed: reason}
	}
	started, err := act.Start(ctx, s.a, &st.Duty, req)
	if err != nil {
		return s.failAttempt(ctx, st, date, e, []string{StateStarting}, "The action "+name+" failed: "+reasonOf(err)+".", false)
	}
	if started.Run == "" {
		s.endWithoutRun(ctx, st, date, e, started.Note)
		return Outcome{}
	}
	s.queued(ctx, st, date, e, started.Run)
	return Outcome{Run: started.Run}
}

// queued moves a start to queued with its run.
func (s *Service) queued(ctx context.Context, st *stored, date string, e entry, run string) {
	_, wrote, err := s.st.setEntry(ctx, st.Duty.Name, date, e.ID, []string{StateStarting}, func(x *entry) bool {
		x.State, x.Run, x.Reason = StateQueued, run, ""
		return true
	})
	if err != nil {
		// The start stays starting with its run, and the engine follows
		// the run after staleStart.
		s.log.Warn("duties: the history did not take a run that went out", "duty", st.Duty.Name, "run", run, "error", err)
	}
	if wrote {
		changed(st.Duty.Name)
	}
	s.log.Info("duties: a duty started a run", "duty", st.Duty.Name, "slot", e.Slot, "start", e.Start, "attempt", e.Attempt, "run", run)
}

// reasonOf words an error without the prefix of its package.
func reasonOf(err error) string {
	msg := err.Error()
	for _, p := range []string{"fleet: ", fw.ErrInvalidRequest.Error() + ": "} {
		msg = strings.ReplaceAll(msg, p, "")
	}
	return strings.TrimSuffix(msg, ".")
}

// request returns the run request of a start: the message with its
// placeholders filled, the budget, the principal, the metadata of the
// duty, and the idempotency key duty/<name>/<id>, which is
// duty/<name>/<time> for a time of the schedule, with the attempt after it
// for a retry. The request names its run, so the front knows the run at
// once.
func (s *Service) request(st *stored, e entry, last *RunEntry) fw.RunRequest {
	d := &st.Duty
	slot := e.Slot.UTC().Format(time.RFC3339Nano)
	input := fill(d.Do.Message(), placeholders(d, e, last))
	md := map[string]string{MetaDuty: d.Name, MetaSlot: slot, fw.MetaBatch: BatchOf(d.Name)}
	if title := fw.TitleFrom(fill(d.Do.Message(), titleValues(d, e, last))); title != "" {
		md[fw.MetaTitle] = title
	}
	key := "duty/" + d.Name + "/" + e.ID
	if e.Attempt > 1 {
		md[MetaAttempt] = strconv.Itoa(e.Attempt)
		key += "/" + strconv.Itoa(e.Attempt)
	}
	if e.Chain > 0 {
		md[MetaChain] = strconv.Itoa(e.Chain)
	}
	principal, kind := "duty:"+d.Name, fw.KindWorkload
	if d.RunAs == RunAsOwner && d.Owner != "" {
		principal, kind = d.Owner, st.OwnerKind
		if kind == "" {
			kind = fw.KindUser
		}
	}
	var budget *fw.Budget
	if b := d.Limits.Budget; b != nil {
		budget = &fw.Budget{MaxCostUSD: b.MaxCostUSD, MaxTurns: b.MaxTurns, MaxTokens: b.MaxTokens, MaxToolCalls: b.MaxToolCalls}
	}
	return fw.RunRequest{
		ID:             fw.NewID(),
		Agent:          d.Agent,
		Input:          input,
		Budget:         budget,
		Metadata:       md,
		Principal:      principal,
		PrincipalKind:  kind,
		IdempotencyKey: key,
	}
}

// lastRun returns the newest start of a duty with a run before the slot,
// for the placeholders {last.*}, or nil.
func (s *Service) lastRun(ctx context.Context, name string, slot time.Time) *RunEntry {
	list, err := s.st.history(ctx, name, 50)
	if err != nil {
		return nil
	}
	for i := range list {
		if list[i].Run != "" && list[i].Slot.Before(slot) {
			return &list[i]
		}
	}
	return nil
}

// endEntry moves a start from one of the states from to a final state,
// with the reason.
func (s *Service) endEntry(ctx context.Context, st *stored, date string, e entry, from []string, to, reason string) bool {
	_, wrote, err := s.st.setEntry(ctx, st.Duty.Name, date, e.ID, from, func(x *entry) bool {
		x.State, x.Reason = to, reason
		x.FoundText, x.Event = "", ""
		return true
	})
	if err != nil {
		s.log.Warn("duties: the history of a duty did not change", "duty", st.Duty.Name, "error", err)
	}
	if wrote {
		changed(st.Duty.Name)
	}
	return wrote
}

// failAttempt ends an attempt that did not reach a run: the start goes again after retryPause while it has retries and
// the duty runs, else it fails, which counts for the breaker.
func (s *Service) failAttempt(ctx context.Context, st *stored, date string, e entry, from []string, reason string, stopped bool) Outcome {
	d := &st.Duty
	now := s.now()
	after, wrote, err := s.st.setEntry(ctx, d.Name, date, e.ID, from, func(x *entry) bool {
		// Each caller failed before a queue took the run, so no run of the
		// attempt exists and the start names none.
		x.Run = ""
		if x.Attempt <= d.retries() && !stopped {
			x.State, x.RetryAt, x.Reason = StateRetrying, now.Add(retryPause).UTC(), reason+" It goes again."
		} else {
			x.State, x.Reason = StateFailed, reason
			x.FoundText, x.Event = "", ""
		}
		return true
	})
	if err != nil {
		s.log.Warn("duties: the history of a duty did not change", "duty", d.Name, "error", err)
		return Outcome{Failed: reason}
	}
	if !wrote {
		return Outcome{Failed: reason}
	}
	changed(d.Name)
	if after.State == StateFailed {
		s.failed(ctx, st, after, reason)
		return Outcome{Failed: reason}
	}
	return Outcome{Failed: reason, retrying: true}
}

// failed counts a failed start for the breaker, tells the people of
// notify.failure, and pauses the duty at breakerAfter failures in a row.
func (s *Service) failed(ctx context.Context, st *stored, e entry, reason string) {
	c, err := s.st.modifyCounters(ctx, st.Duty.Name, func(c *counters) bool {
		c.FailuresInRow++
		return true
	})
	if err != nil {
		s.log.Warn("duties: the counters of a duty did not change", "duty", st.Duty.Name, "error", err)
	}
	// The items of the start did not reach the agent, so the next check
	// wakes it for them again.
	if err := s.st.forget(ctx, st.Duty.Name, e.ID); err != nil {
		s.log.Warn("duties: the seen keys of a failed start stay", "duty", st.Duty.Name, "error", err)
	}
	s.notify(ctx, st, Notice{Kind: NoticeFailed, Run: e.Run, Slot: e.Slot, Reason: reason})
	if c.FailuresInRow >= breakerAfter {
		s.pause(ctx, st, fmt.Sprintf("%d runs failed in a row, so the duty paused itself. The last one: %s", c.FailuresInRow, reason))
	}
}

// checkFailed counts a failed check, and pauses the duty at breakerAfter
// failed checks in a row.
func (s *Service) checkFailed(ctx context.Context, st *stored, reason string) {
	c, err := s.st.modifyCounters(ctx, st.Duty.Name, func(c *counters) bool {
		c.ChecksFailed++
		return true
	})
	if err != nil {
		s.log.Warn("duties: the counters of a duty did not change", "duty", st.Duty.Name, "error", err)
		return
	}
	if c.ChecksFailed >= breakerAfter {
		s.pause(ctx, st, fmt.Sprintf("%d checks failed in a row, so the duty paused itself. The last one: %s", c.ChecksFailed, reason))
	}
}

// checkPassed ends a row of failed checks.
func (s *Service) checkPassed(ctx context.Context, name string) {
	if _, err := s.st.modifyCounters(ctx, name, func(c *counters) bool {
		changed := c.ChecksFailed != 0
		c.ChecksFailed = 0
		return changed
	}); err != nil {
		s.log.Warn("duties: the counters of a duty did not change", "duty", name, "error", err)
	}
}

// completed ends a row of failed runs.
func (s *Service) completed(ctx context.Context, name string) {
	if _, err := s.st.modifyCounters(ctx, name, func(c *counters) bool {
		changed := c.FailuresInRow != 0
		c.FailuresInRow = 0
		return changed
	}); err != nil {
		s.log.Warn("duties: the counters of a duty did not change", "duty", name, "error", err)
	}
}

// pause pauses a duty with the reason, and tells the people of
// notify.failure. A duty that another replica paused first stays as it
// is.
func (s *Service) pause(ctx context.Context, st *stored, reason string) {
	now := s.now().UTC()
	_, err := s.st.update(ctx, st.Duty.Name, 0, func(x *stored) error {
		if x.Duty.Paused {
			return errUnchanged
		}
		x.Duty.Paused, x.PausedReason, x.Changed, x.ChangedBy = true, reason, now, "fw-workbench"
		return nil
	})
	if errors.Is(err, errUnchanged) {
		return
	}
	if err != nil {
		s.log.Warn("duties: a duty did not pause", "duty", st.Duty.Name, "error", err)
		return
	}
	st.Duty.Paused, st.PausedReason = true, reason
	changed(st.Duty.Name)
	s.notify(ctx, st, Notice{Kind: NoticePaused, Reason: reason})
}

// follow follows the starts of a duty that work: it starts a start that
// waited once no run works, starts a retry at its time, takes over a start
// whose replica stopped, and keeps the state and the cost of each run as
// the view of the fleet shows them.
func (s *Service) follow(ctx context.Context, st *stored, sw Switch, now time.Time) {
	d := &st.Duty
	if err := s.ensureSeeded(ctx, st); err != nil {
		s.log.Warn("duties: the engine did not read the history of a duty", "duty", d.Name, "error", err)
		return
	}
	days, dates, err := s.recent(ctx, d.Name, d.zone(), now)
	if err != nil {
		s.log.Warn("duties: the engine did not read the history of a duty", "duty", d.Name, "error", err)
		return
	}
	stopped := d.Paused || sw.Paused
	var runs map[string]*fw.Run
	loaded := false
	load := func() map[string]*fw.Run {
		if !loaded {
			loaded = true
			var err error
			if runs, err = s.runs(ctx, d.Name); err != nil {
				s.log.Warn("duties: the engine did not read the runs of a duty", "duty", d.Name, "error", err)
			}
		}
		return runs
	}
	type place struct {
		date string
		e    entry
	}
	var waiting []place
	for _, date := range dates {
		open := false
		for _, e := range days[date].Entries {
			if ended(e.State) {
				continue
			}
			open = true
			switch e.State {
			case StateWaiting:
				if stopped {
					s.endEntry(ctx, st, date, e, []string{StateWaiting}, StateSkipped, "The duty paused before the run that it waited for ended.")
					continue
				}
				waiting = append(waiting, place{date, e})
			case StateRetrying:
				switch {
				case stopped:
					s.endEntry(ctx, st, date, e, []string{StateRetrying}, StateFailed, "The duty paused before its retry.")
				case !now.Before(e.RetryAt):
					s.retry(ctx, st, date, e, now)
				}
			case StateStarting:
				if now.Sub(e.Updated) < staleStart {
					continue
				}
				if e.Run == "" {
					s.failAttempt(ctx, st, date, e, []string{StateStarting}, "The front stopped before the run reached its queue.", stopped)
					continue
				}
				// The run may have gone out: the start waits for it as for
				// a run on the queue.
				if r, cost := find(load(), e.Run); r != nil {
					s.settle(ctx, st, date, e, r, cost, stopped)
				} else if now.Sub(e.Updated) > lostAfter {
					s.endEntry(ctx, st, date, e, []string{StateStarting}, StateLost, lostWords(e.Run))
				}
			default:
				r, cost := find(load(), e.Run)
				if r == nil {
					if now.Sub(e.Updated) > lostAfter {
						s.endEntry(ctx, st, date, e, []string{e.State}, StateLost, lostWords(e.Run))
					}
					continue
				}
				s.settle(ctx, st, date, e, r, cost, stopped)
			}
		}
		if !open {
			s.forgetOpen(d.Name, date)
		}
	}
	if len(waiting) == 0 {
		return
	}
	// A start that waits goes on once no run of the duty works, after the
	// runs above ended in this pass.
	days, _, err = s.recent(ctx, d.Name, d.zone(), now)
	if err != nil || lastWorking(days) != nil {
		return
	}
	sort.Slice(waiting, func(i, j int) bool { return waiting[i].e.Slot.Before(waiting[j].e.Slot) })
	s.resume(ctx, st, waiting[0].date, waiting[0].e)
}

// lostWords says why the front stopped waiting for a run.
func lostWords(run string) string {
	return fmt.Sprintf("No worker reported the run %s for %s, so the front stopped waiting for it.", run, durText(lostAfter))
}

// resume starts a start that waited, once no run of the duty works. It
// reports whether the start went on.
func (s *Service) resume(ctx context.Context, st *stored, date string, e entry) bool {
	d := &st.Duty
	now := s.now()
	days, _, err := s.recent(ctx, d.Name, d.zone(), now)
	if err != nil {
		return false
	}
	if reason, err := s.overCaps(ctx, d, days, now); err != nil {
		return false
	} else if reason != "" {
		if s.endEntry(ctx, st, date, e, []string{StateWaiting}, StateSkipped, reason) {
			s.capNotice(ctx, st, dateOf(now, d.zone()), reason)
		}
		return false
	}
	after, wrote, err := s.st.setEntry(ctx, d.Name, date, e.ID, []string{StateWaiting}, func(x *entry) bool {
		x.State, x.By, x.Reason = StateStarting, s.replica, ""
		return true
	})
	if err != nil || !wrote {
		return false
	}
	changed(d.Name)
	s.carry(ctx, st, date, after)
	return true
}

// retry starts the next attempt of a start whose run failed.
func (s *Service) retry(ctx context.Context, st *stored, date string, e entry, now time.Time) {
	d := &st.Duty
	days, _, err := s.recent(ctx, d.Name, d.zone(), now)
	if err != nil {
		return
	}
	if why, err := s.overCaps(ctx, d, days, now); err != nil {
		return
	} else if why != "" {
		reason := "The daily cap stopped the retry: " + why
		if s.endEntry(ctx, st, date, e, []string{StateRetrying}, StateFailed, reason) {
			s.capNotice(ctx, st, dateOf(now, d.zone()), why)
			after := e
			after.State = StateFailed
			s.failed(ctx, st, after, reason)
		}
		return
	}
	after, wrote, err := s.st.setEntry(ctx, d.Name, date, e.ID, []string{StateRetrying}, func(x *entry) bool {
		if now.Before(x.RetryAt) {
			return false
		}
		x.State, x.Attempt, x.By, x.Run, x.RetryAt = StateStarting, x.Attempt+1, s.replica, "", time.Time{}
		return true
	})
	if err != nil || !wrote {
		return
	}
	changed(d.Name)
	s.act(ctx, st, date, after)
}

// find returns the run of a start in runs, by its id, and its cost. When
// the intake of the kernel retried a failed run, it follows the retry,
// and the cost holds every attempt of the intake.
func find(runs map[string]*fw.Run, id string) (*fw.Run, float64) {
	r := runs[id]
	if r == nil {
		return nil, 0
	}
	cost := costOf(r)
	for range 8 {
		if r.State != fw.StateFailed || r.Metadata[fw.MetaIntakeOutcome] != fw.IntakeRetried {
			break
		}
		var next *fw.Run
		for _, x := range runs {
			if x.Metadata[fw.MetaIntakeRetryOf] == r.ID {
				next = x
				break
			}
		}
		if next == nil {
			break
		}
		r = next
		cost += costOf(r)
	}
	return r, cost
}

// usd rounds a cost to a millionth of a dollar, so a sum reads as a person
// adds it.
func usd(x float64) float64 { return math.Round(x*1e6) / 1e6 }

// costOf returns the cost of a run and of the runs under it.
func costOf(r *fw.Run) float64 {
	if r.TreeUsage != nil {
		return r.TreeUsage.CostUSD
	}
	return r.Usage.CostUSD
}

// owed reports whether the intake of the kernel still owes the next step
// of a failed run, or retried it: the run is not the end of its job yet.
func owed(r *fw.Run) bool {
	if r.State != fw.StateFailed {
		return false
	}
	outcome := r.Metadata[fw.MetaIntakeOutcome]
	return outcome == fw.IntakeRetried || (outcome == "" && r.Metadata[fw.MetaIntakeQueue] != "")
}

// settle writes the state, the cost, and the title of the run of a start
// as the view shows it. A run that ended ends the start: a completed run
// ends a row of failures, a failed run goes again while the start has
// retries, and a run that asks a person tells the people of
// notify.needsYou once.
func (s *Service) settle(ctx context.Context, st *stored, date string, e entry, r *fw.Run, cost float64, stopped bool) {
	d := &st.Duty
	now := s.now()
	reason := ""
	after, wrote, err := s.st.setEntry(ctx, d.Name, date, e.ID, []string{e.State}, func(x *entry) bool {
		if x.Run != e.Run {
			return false
		}
		before := *x
		x.Run = r.ID
		x.CostUSD = usd(x.SpentUSD + cost)
		if r.Title != "" {
			x.Title = r.Title
		}
		switch {
		case owed(r):
			x.State = string(fw.StateRunning)
		case r.State == fw.StateCompleted:
			x.State, x.Reason = StateCompleted, ""
			x.FoundText, x.Event = "", ""
		case r.State == fw.StateCanceled:
			x.State, x.Reason = StateCanceled, "Somebody canceled the run."
			x.FoundText, x.Event = "", ""
		case r.State == fw.StateFailed:
			reason = "The run failed: " + strings.TrimSuffix(errorLine(r.Error, 300), ".") + "."
			if x.Attempt <= d.retries() && !stopped {
				x.State, x.RetryAt, x.SpentUSD, x.Reason = StateRetrying, now.Add(retryPause).UTC(), x.CostUSD, reason+" It goes again."
			} else {
				x.State, x.Reason = StateFailed, reason
				x.FoundText, x.Event = "", ""
			}
		default:
			x.State = string(r.State)
		}
		if x.State == string(fw.StateInputRequired) {
			x.Asked = true
		}
		return !sameEntry(before, *x)
	})
	if err != nil {
		s.log.Warn("duties: the history of a duty did not change", "duty", d.Name, "error", err)
		return
	}
	if !wrote {
		return
	}
	changed(d.Name)
	switch {
	case after.State == StateCompleted:
		s.completed(ctx, d.Name)
	case after.State == StateFailed:
		s.failed(ctx, st, after, reason)
	case after.State == string(fw.StateInputRequired) && !e.Asked && !awayHandled():
		// A package that handles whileAway tells of each question itself,
		// with its tool and its reason (SetAwayHandler).
		s.notify(ctx, st, Notice{Kind: NoticeNeedsYou, Run: after.Run, Slot: after.Slot, Reason: "A run of the duty waits for a person."})
	}
}

// sameEntry reports whether two starts hold the same values, but for the
// time of the last change.
func sameEntry(a, b entry) bool {
	a.Updated, b.Updated = time.Time{}, time.Time{}
	return reflect.DeepEqual(a, b)
}

// htmlTag matches a tag of HTML, such as <center> or </body>, in an error
// that quotes the page of a proxy.
var htmlTag = regexp.MustCompile(`</?[a-zA-Z][a-zA-Z0-9]*(\s[^<>]*)?/?>`)

// errorLine returns the error of a run as one line of words, without the
// tags of an HTML page that a proxy answered, cut to n bytes.
func errorLine(s string, n int) string {
	return oneLine(htmlTag.ReplaceAllString(s, " "), n)
}

// oneLine cuts a text to one line of at most n bytes.
func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if s == "" {
		return "no reason was given"
	}
	return cut(s, n)
}

// cut cuts a text to at most n bytes, at a boundary of a character.
func cut(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && n < len(s) && (s[n]&0xC0) == 0x80 {
		n--
	}
	return s[:n]
}

// runsOfFleet returns the runs of the batch of a duty, by id: the runs of
// this process and of the view of the fleet. The context carries no
// principal, so the list holds every run of the batch.
func (s *Service) runsOfFleet(ctx context.Context, name string) (map[string]*fw.Run, error) {
	ctx = fw.WithPrincipal(ctx, nil)
	batch := BatchOf(name)
	out := map[string]*fw.Run{}
	local, err := s.a.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("duties: list the runs: %w", err)
	}
	for _, r := range local {
		if r.Metadata[fw.MetaBatch] == batch {
			out[r.ID] = r
		}
	}
	v := fleet.Of(s.a)
	if v == nil {
		return out, nil
	}
	var remote []*fw.Run
	if bv, ok := v.(fleet.BatchView); ok {
		b, err := bv.Batch(ctx, batch)
		if err != nil {
			return nil, fmt.Errorf("duties: list the runs of %s: %w", batch, err)
		}
		remote = b.Runs
	} else {
		all, err := v.Runs(ctx)
		if err != nil {
			return nil, fmt.Errorf("duties: list the runs of the fleet: %w", err)
		}
		for _, r := range all {
			if r.Metadata[fw.MetaBatch] == batch {
				remote = append(remote, r)
			}
		}
	}
	for _, r := range remote {
		if _, ok := out[r.ID]; !ok {
			out[r.ID] = r
		}
	}
	return out, nil
}
