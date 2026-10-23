package duties

// This file holds what the packages of the front read and write for the
// attention of the duties (docs/ARCHITECTURE.md section 23): the decisions
// while away (package duties/away), the starts on events of the fleet
// (package duties/events), the notifications (package notify), and the
// inbox (package inbox). Each function acts for the front itself, with no
// check of the caller. The package that calls it decides who sees what.

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"sync/atomic"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// MetaChain is the metadata key of a run that an event of the fleet
// started: the depth of the start in a chain of duties that events
// started, from 1. A run of the chain that starts the next duty gives the
// next start its depth plus one.
const MetaChain = "workbench.dutyChain"

// Bounds of the loop guard (the guardrails of the design).
const (
	// MaxChain is the deepest start of a chain of duties that events
	// started. A deeper start does not run, and the history says why.
	MaxChain = 3
	// MaxEventStartsPerHour is the most runs that events start for one
	// duty in an hour.
	MaxEventStartsPerHour = 60
)

// NoticeDecided is the kind of a notice that tells the people of
// notify.needsYou how the front decided a question of a run while nobody
// answered it, such as a deny after whileAway.timeout.
const NoticeDecided = "decided"

// away is true once a package of the front handles the runs that wait for
// a person (SetAwayHandler).
var away atomic.Bool

// SetAwayHandler tells the engine whether a package of the front handles
// the runs of the duties that wait for a person (whileAway). While it
// does, the engine sends no notice of kind needs-you itself, because that
// package tells of each question with its tool and its reason (Tell).
func SetAwayHandler(on bool) { away.Store(on) }

func awayHandled() bool { return away.Load() }

// Record is a duty of the store as the front reads it for itself: the
// duty, the kind of the principal of its owner, why it paused itself,
// the moment from which it counts its starts, and its last change.
type Record struct {
	Duty         Duty      `json:"duty"`
	OwnerKind    string    `json:"ownerKind,omitempty"`
	PausedReason string    `json:"pausedReason,omitempty"`
	Since        time.Time `json:"since"`
	Changed      time.Time `json:"changed"`
}

// Records returns every duty of the store, sorted by name, for the front
// itself.
func (s *Service) Records(ctx context.Context) ([]Record, error) {
	list, err := s.st.list(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Record, 0, len(list))
	for _, st := range list {
		out = append(out, Record{Duty: st.Duty, OwnerKind: st.OwnerKind, PausedReason: st.PausedReason, Since: st.Since, Changed: st.Changed})
	}
	return out, nil
}

// Starts returns the starts of a duty that began or changed since a
// moment, newest first, for the front itself.
func (s *Service) Starts(ctx context.Context, name string, since time.Time) ([]RunEntry, error) {
	st, err := s.st.get(ctx, name)
	if err != nil {
		return nil, err
	}
	dates, err := s.st.dates(ctx, name)
	if err != nil {
		return nil, err
	}
	// A start keeps the date of its slot, which can be a day before the
	// moment of a late change.
	first := dateOf(since.Add(-24*time.Hour), st.Duty.zone())
	var list []entry
	for _, date := range dates {
		if date < first {
			continue
		}
		x, err := s.st.day(ctx, name, date)
		if err != nil {
			return nil, err
		}
		for _, e := range x.Entries {
			if !e.At.Before(since) || !e.Updated.Before(since) {
				list = append(list, e)
			}
		}
	}
	sort.SliceStable(list, func(i, j int) bool { return list[i].Slot.After(list[j].Slot) })
	out := make([]RunEntry, 0, len(list))
	for _, e := range list {
		out = append(out, e.RunEntry)
	}
	return out, nil
}

// RecordDecision adds a decision to the history of a duty, on the start
// whose run is run, among the starts of the last eight days. It returns
// an error that wraps ErrNotFound when no such start exists.
func (s *Service) RecordDecision(ctx context.Context, name, run string, d Decided) error {
	if d.At.IsZero() {
		d.At = s.now().UTC()
	}
	d.Reason = oneLineOrEmpty(d.Reason, 500)
	dates, err := s.st.dates(ctx, name)
	if err != nil {
		return err
	}
	if len(dates) > 8 {
		dates = dates[len(dates)-8:]
	}
	ok, err := s.st.addDecided(ctx, name, run, dates, d)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: no start of the duty %s has the run %s", ErrNotFound, name, run)
	}
	changed(name)
	return nil
}

// oneLineOrEmpty is oneLine that keeps an empty text empty.
func oneLineOrEmpty(s string, n int) string {
	if s == "" {
		return ""
	}
	return oneLine(s, n)
}

// Tell sends a notice of a duty to the OnNotice hooks, as the engine does:
// the kind, the run, and the reason in words. The people of
// notify.needsYou hear of the kinds needs-you and decided, and the people
// of notify.failure hear of the others. The caller sends each notice
// once, from one replica.
func (s *Service) Tell(ctx context.Context, kind, name, run, reason string) error {
	st, err := s.st.get(ctx, name)
	if err != nil {
		return err
	}
	d := &st.Duty
	var list []string
	if d.Notify != nil {
		list = d.Notify.Failure
		if kind == NoticeNeedsYou || kind == NoticeDecided {
			list = d.Notify.NeedsYou
		}
	}
	n := Notice{Kind: kind, Duty: d.Name, Agent: d.Agent, Owner: d.Owner, Run: run, Reason: reason, At: s.now().UTC()}
	for _, who := range list {
		if who == RunAsOwner {
			who = d.Owner
		}
		if who != "" && !slices.Contains(n.Who, who) {
			n.Who = append(n.Who, who)
		}
	}
	s.log.Info("duties: a notice", "kind", n.Kind, "duty", n.Duty, "run", n.Run, "reason", n.Reason)
	hooks.mu.Lock()
	fns := append([]func(context.Context, *fw.Agent, Notice){}, hooks.notice...)
	hooks.mu.Unlock()
	for _, fn := range fns {
		fn(ctx, s.a, n)
	}
	return nil
}

// FireChain starts a duty for an event of the fleet, as Fire does, at the
// depth chain of a chain of duties that events started (MetaChain). A
// start deeper than MaxChain does not run, and neither does a start past
// MaxEventStartsPerHour runs that events started for the duty in the last
// hour: the history says why, and the outcome is skipped.
func (s *Service) FireChain(ctx context.Context, name string, event json.RawMessage, chain int) (Outcome, error) {
	st, err := s.st.get(ctx, name)
	if err != nil {
		return Outcome{}, err
	}
	if st.Duty.When.Event == nil {
		return Outcome{}, fmt.Errorf("%w: the duty %s starts on no event", fw.ErrInvalidRequest, name)
	}
	if blocked, why := s.stopped(ctx, st); blocked {
		return Outcome{Skipped: why}, nil
	}
	chain = max(chain, 1)
	now := s.now()
	start := Trigger{Kind: StartEvent, Slot: now.UTC(), Event: string(event), Chain: chain}
	why := ""
	if chain > MaxChain {
		why = fmt.Sprintf("An event of a run that a chain of %d duties started fired it, and a chain of duties that events start stops at a depth of %d, so it did not run.", chain-1, MaxChain)
	} else {
		days, _, err := s.recent(ctx, name, st.Duty.zone(), now)
		if err != nil {
			return Outcome{}, err
		}
		if n := eventRuns(days, now); n >= MaxEventStartsPerHour {
			why = fmt.Sprintf("Events started the duty %d times in the last hour, the most, so it did not run.", n)
		}
	}
	if why == "" {
		return s.begin(ctx, st, start)
	}
	e := s.newEntry(start, now)
	e.State, e.Reason, e.Event = StateSkipped, why, ""
	ok, err := s.st.claim(ctx, name, dateOf(start.Slot, st.Duty.zone()), e)
	if err != nil {
		return Outcome{}, err
	}
	if !ok {
		return Outcome{Duplicate: true}, nil
	}
	changed(name)
	s.log.Info("duties: the loop guard stopped a start on an event", "duty", name, "chain", chain, "reason", why)
	return Outcome{Skipped: why}, nil
}

// eventRuns counts the starts that events made in the hour before now and
// that did not skip.
func eventRuns(days map[string]day, now time.Time) int {
	n := 0
	for _, d := range days {
		for _, e := range d.Entries {
			if e.Start == StartEvent && now.Sub(e.At) < time.Hour && e.State != StateSkipped && e.State != StateMissed {
				n++
			}
		}
	}
	return n
}
