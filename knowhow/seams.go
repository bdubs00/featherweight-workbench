package knowhow

// This file holds the functions that the seams of the other packages call
// (docs/KNOWHOW.md sections 5.2 and 6.4): a page that a write changed, an
// item of a source or a Duty with a new version, a routine that moved,
// and the switch of an agent. Each package function has the signature of
// its hook, so the wiring is one assignment, such as
// pages.OnWrite = knowhow.PageWritten. It acts on the Know-how of every
// agent of the process. The method of the same name acts on one service.

import (
	"context"
	"fmt"
	"strings"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/routines"
)

// PageWritten marks out of date every note that names the page at path
// with another version than hash (pages.OnWrite). An empty hash is a
// removal of the page.
func PageWritten(ctx context.Context, path, hash string) {
	for _, s := range all() {
		s.PageWritten(ctx, path, hash)
	}
}

// ItemChanged marks out of date every note that names the item key of the
// source with another version (sources.OnItem and duties.OnItem), such as
// the key lab/helpdesk/issue/42 of the source gitea, which a note names
// as item:gitea/lab/helpdesk/issue/42.
func ItemChanged(ctx context.Context, source, key, version string) {
	for _, s := range all() {
		s.ItemChanged(ctx, source, key, version)
	}
}

// RoutineMoved writes a move of a routine to the journal (routines.OnMove).
// from is empty for a new routine.
func RoutineMoved(ctx context.Context, agent, name, from, to, by, note string) {
	for _, s := range all() {
		s.RoutineMoved(ctx, agent, name, from, to, by, note)
	}
}

// AskFirst reports whether the switch "Ask me before it keeps what it
// learns" of the agent is on (routines.AskFirst). Without Know-how in the
// process, nothing waits.
func AskFirst(ctx context.Context, agent string) bool {
	for _, s := range all() {
		if s.AskFirst(ctx, agent) {
			return true
		}
	}
	return false
}

// PageWritten marks out of date every note of this service that names the
// page at path with another version than hash. A note that a person wrote
// stays as it is. Each move writes the journal with the source and opens
// a candidate recheck for the after-work review.
func (s *Service) PageWritten(ctx context.Context, path, hash string) {
	path = strings.TrimSpace(path)
	if path == "" {
		return
	}
	why := fmt.Sprintf("The page %s changed after the note was written.", path)
	if strings.TrimSpace(hash) == "" {
		why = fmt.Sprintf("The page %s was removed after the note was written.", path)
	}
	s.outOfDate(ctx, "page:"+path, strings.ToLower(strings.TrimSpace(hash)), why)
}

// ItemChanged marks out of date every note of this service that names the
// item key of the source with another version than version.
func (s *Service) ItemChanged(ctx context.Context, source, key, version string) {
	source, key = strings.TrimSpace(source), strings.Trim(strings.TrimSpace(key), "/")
	if source == "" || key == "" || version == "" {
		return
	}
	ref := "item:" + source + "/" + key
	s.outOfDate(ctx, ref, version, fmt.Sprintf("The item %s/%s changed after the note was written.", source, key))
}

// outOfDate marks out of date the notes that name ref with a version
// other than version.
func (s *Service) outOfDate(ctx context.Context, ref, version, why string) {
	notes, err := s.noteList(ctx, "")
	if err != nil {
		s.log.Warn("knowhow: the notes do not read; a change of a source marks nothing out of date", "source", ref, "error", err)
		return
	}
	for key, n := range notes {
		if n.OutOfDate != nil && n.OutOfDate.Source == ref && n.OutOfDate.Version != version && n.State != StatePutAway {
			// A note that is out of date already keeps the version of its
			// source now, so a person who corrects it vouches for that one.
			if _, err := s.setStateAny(ctx, key, func(x *Note) bool {
				if x.OutOfDate == nil || x.OutOfDate.Source != ref || x.OutOfDate.Version == version {
					return false
				}
				x.OutOfDate.Version = version
				return true
			}); err != nil {
				s.log.Warn("knowhow: a note out of date does not know the new version of its source", "agent", n.Agent, "note", n.Name, "error", err)
			}
			continue
		}
		if n.ByPerson() || n.State == StatePutAway || n.State == StateOutOfDate || n.OutOfDate != nil {
			continue
		}
		stale := false
		for _, src := range n.Sources {
			if src.Ref == ref && src.Version != "" && src.Version != version {
				stale = true
				break
			}
		}
		if !stale {
			continue
		}
		now := s.now().UTC()
		changed, err := s.setState(ctx, key, WhatOutOfDate, why, nil, func(x *Note) bool {
			if x.OutOfDate != nil || x.State == StatePutAway {
				return false
			}
			x.OutOfDate = &OutOfDate{Since: now, Source: ref, Version: version}
			switch x.State {
			case StateChecked, StateUnchecked:
				x.State = StateOutOfDate
			}
			return true
		})
		if err != nil {
			s.log.Warn("knowhow: a note did not go out of date", "agent", n.Agent, "note", n.Name, "error", err)
			continue
		}
		if changed {
			if err := s.recheck(ctx, n, ref, version); err != nil {
				s.log.Warn("knowhow: a note that went out of date has no candidate", "agent", n.Agent, "note", n.Name, "error", err)
			}
		}
	}
}

// selfKey marks a context in which this package moves a routine and
// writes the journal itself, so RoutineMoved writes nothing a second time.
type selfKey struct{}

func withSelf(ctx context.Context) context.Context { return context.WithValue(ctx, selfKey{}, true) }

func isSelf(ctx context.Context) bool {
	v, _ := ctx.Value(selfKey{}).(bool)
	return v
}

// RoutineMoved writes one move of a routine of the agent to the journal of
// this service: proposed, rehearsing, live, paused, or retired, and
// stepped-back for a live routine that went back to rehearsing without a
// person. The note of the move is the sentence of the entry. A move that
// this package made itself writes nothing here, because the package wrote
// the entry already.
func (s *Service) RoutineMoved(ctx context.Context, agent, name, from, to, by, note string) {
	if isSelf(ctx) || CheckAgent(agent) != nil || name == "" || from == to {
		return
	}
	what := to
	switch to {
	case routines.Proposed:
		what = WhatProposed
	case routines.Rehearsing:
		what = WhatRehearsing
		if from == routines.Live {
			what = WhatSteppedBack
		}
	case routines.Live:
		what = WhatLive
	case routines.Paused:
		what = WhatPaused
	case routines.Retired:
		what = WhatRetired
	default:
		return
	}
	why := strings.TrimSpace(note)
	if why == "" {
		why = routineWhy(what, name)
	}
	var runs []string
	if what == WhatSteppedBack {
		runs = runsIn(note)
	}
	e := Entry{Agent: agent, What: what, Subject: Subject{Routine: name}, Why: why, Runs: runs, By: by}
	if from != "" {
		version := 0
		if st := s.routineStore(); st != nil {
			if rec, err := st.Get(ctx, agent, name); err == nil {
				version = rec.Version()
			}
		}
		e.Undo = &Undo{From: from, To: to, Version: version}
	}
	if _, err := s.appendJournal(ctx, e); err != nil {
		s.log.Warn("knowhow: the journal did not take a move of a routine", "agent", agent, "routine", name, "error", err)
	}
}

// routineWhy is the sentence of a move of a routine without a note.
func routineWhy(what, name string) string {
	switch what {
	case WhatProposed:
		return fmt.Sprintf("The routine %s was proposed.", name)
	case WhatRehearsing:
		return fmt.Sprintf("The routine %s rehearses beside the agent.", name)
	case WhatSteppedBack:
		return fmt.Sprintf("The routine %s went back to rehearsal by itself.", name)
	case WhatLive:
		return fmt.Sprintf("The routine %s now does the work on its own.", name)
	case WhatPaused:
		return fmt.Sprintf("The routine %s is paused.", name)
	}
	return fmt.Sprintf("The routine %s is retired.", name)
}

// runsIn returns the run ids that a note names, such as the run of a step
// back.
func runsIn(note string) []string {
	var out []string
	for _, f := range strings.FieldsFunc(note, func(r rune) bool { return !(r >= '0' && r <= '9' || r >= 'A' && r <= 'Z') }) {
		if _, ok := fw.IDTime(f); ok {
			out = append(out, f)
		}
	}
	return out
}
