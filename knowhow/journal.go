package knowhow

// This file keeps the journal "What it learned" (docs/KNOWHOW.md section
// 5.4), the switch "Ask me before it keeps what it learns", and the
// changes that wait for a person under the switch. Each journal entry is
// one entry of the bucket under journal.<agent>.<id>, where the id of
// fw.NewID starts with its time, so the keys of an agent sort by time.
// The public id of an entry is <id>-<agent in base64url>, so a route finds
// the entry without the agent.

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/internal/bucket"
	"github.com/bdubs00/featherweight-workbench/routines"
)

// What a journal entry says happened.
const (
	WhatLearned     = "learned"
	WhatUpdated     = "updated"
	WhatOutOfDate   = "out-of-date"
	WhatResting     = "resting"
	WhatPutAway     = "put-away"
	WhatProposed    = "proposed"
	WhatRehearsing  = "rehearsing"
	WhatLive        = "live"
	WhatSteppedBack = "stepped-back"
	WhatRetired     = "retired"
	WhatUndone      = "undone"
	// WhatPaused is a move of a routine to paused, which the list of the
	// contract does not name.
	WhatPaused = "paused"
	// WhatStaysOut is a proposal of a routine that stayed out, with the
	// reason in why.
	WhatStaysOut = "stays-out"
)

// Bounds of the journal.
const (
	JournalDays     = 90
	JournalMax      = 5000
	DefaultLimit    = 50
	MaxLimit        = 500
	MaxWhyBytes     = 300
	pruneEvery      = 64
	MaxWaiting      = 200
	waitingKeepDays = 30
)

// Subject names the note or the routine of an entry.
type Subject struct {
	Note      string `json:"note,omitempty"`
	Scope     string `json:"scope,omitempty"`
	Principal string `json:"principal,omitempty"`
	Routine   string `json:"routine,omitempty"`
}

func noteSubject(sc Scope, name string) Subject {
	return Subject{Note: name, Scope: sc.Kind, Principal: sc.Principal}
}

// scope returns the scope of a note subject.
func (s Subject) scope() Scope { return Scope{Kind: s.Scope, Principal: s.Principal} }

// Undo says how to take an entry back. For a note, Revision is the
// revision of the note before the change, 0 for a new note, and After the
// revision that the change wrote. For a routine, From and To are the
// standings of the move, and Version the version of its body.
type Undo struct {
	Revision uint64 `json:"revision"`
	After    uint64 `json:"after,omitempty"`
	From     string `json:"from,omitempty"`
	To       string `json:"to,omitempty"`
	Version  int    `json:"version,omitempty"`
}

// Entry is one entry of the journal. Corrects is true for a change that
// replaced or put away a note that was out of date. UndoneBy names the
// entry that took this one back.
type Entry struct {
	ID       string    `json:"id"`
	At       time.Time `json:"at"`
	Agent    string    `json:"agent"`
	What     string    `json:"what"`
	Subject  Subject   `json:"subject"`
	Why      string    `json:"why"`
	Runs     []string  `json:"runs,omitempty"`
	By       string    `json:"by"`
	Undo     *Undo     `json:"undo,omitempty"`
	Corrects bool      `json:"corrects,omitempty"`
	UndoneBy string    `json:"undoneBy,omitempty"`
}

// publicID returns the id that a route takes for an entry of the key id.
func publicID(agent, id string) string { return id + "-" + agentKey(agent) }

// splitID returns the agent and the id of a public id.
func splitID(public string) (agent, id string, err error) {
	id, enc, ok := strings.Cut(public, "-")
	if !ok || len(id) != 26 {
		return "", "", fmt.Errorf("%w: %q is not the id of an entry", ErrNotFound, public)
	}
	raw, err := base64.RawURLEncoding.DecodeString(enc)
	if err != nil || CheckAgent(string(raw)) != nil {
		return "", "", fmt.Errorf("%w: %q is not the id of an entry", ErrNotFound, public)
	}
	if _, ok := fw.IDTime(id); !ok {
		return "", "", fmt.Errorf("%w: %q is not the id of an entry", ErrNotFound, public)
	}
	return string(raw), id, nil
}

func journalKey(agent, id string) string { return "journal." + agentKey(agent) + "." + id }

// clipWhy cuts a sentence of the journal.
func clipWhy(why string) string {
	why = strings.Join(strings.Fields(why), " ")
	if len(why) <= MaxWhyBytes {
		return why
	}
	r := []rune(why)
	for len(string(r)) > MaxWhyBytes-3 {
		r = r[:len(r)-1]
	}
	return string(r) + "…"
}

// appendJournal writes one entry. Every pruneEvery entries of an agent in
// this process, it prunes the journal of the agent to its bounds.
func (s *Service) appendJournal(ctx context.Context, e Entry) (Entry, error) {
	e.At = s.now().UTC()
	id := s.newID(e.At)
	e.ID = publicID(e.Agent, id)
	e.Why = clipWhy(e.Why)
	raw, err := encode(e)
	if err != nil {
		return Entry{}, err
	}
	if _, err := s.b.Create(ctx, journalKey(e.Agent, id), raw); err != nil {
		return Entry{}, fmt.Errorf("knowhow: write the journal of %s: %w", e.Agent, err)
	}
	s.mu.Lock()
	s.appends[e.Agent]++
	prune := s.appends[e.Agent] >= pruneEvery
	if prune {
		s.appends[e.Agent] = 0
	}
	s.mu.Unlock()
	if prune {
		if err := s.pruneJournal(ctx, e.Agent); err != nil {
			s.log.Warn("knowhow: the journal of an agent was not pruned", "agent", e.Agent, "error", err)
		}
	}
	emitChange(e.Agent)
	return e, nil
}

// pruneJournal removes the entries of an agent that are older than
// JournalDays and those past the newest JournalMax.
func (s *Service) pruneJournal(ctx context.Context, agent string) error {
	keys, err := bucket.EntriesUnder(ctx, s.b, "journal."+agentKey(agent))
	if err != nil {
		return err
	}
	cut := s.now().UTC().AddDate(0, 0, -JournalDays)
	for i, k := range keys {
		id := k.Key[strings.LastIndexByte(k.Key, '.')+1:]
		t, ok := fw.IDTime(id)
		if len(keys)-i <= JournalMax && ok && !t.Before(cut) {
			break
		}
		if err := s.b.Delete(ctx, k.Key, k.Revision); err != nil && !errors.Is(err, bucket.ErrConflict) {
			return err
		}
	}
	return nil
}

// entry reads one entry of the journal by its public id, with its
// revision.
func (s *Service) entry(ctx context.Context, public string) (Entry, uint64, error) {
	agent, id, err := splitID(public)
	if err != nil {
		return Entry{}, 0, err
	}
	got, err := s.b.Get(ctx, journalKey(agent, id))
	if errors.Is(err, bucket.ErrNotFound) {
		return Entry{}, 0, fmt.Errorf("%w: no entry of the journal has the id %s", ErrNotFound, public)
	}
	if err != nil {
		return Entry{}, 0, err
	}
	var e Entry
	if err := json.Unmarshal(got.Value, &e); err != nil {
		return Entry{}, 0, fmt.Errorf("knowhow: decode an entry of the journal: %w", err)
	}
	return e, got.Revision, nil
}

// Journal returns the entries of an agent, the newest first, at most
// limit, before the entry of the public id before when it is set. A nil
// keep keeps every entry.
func (s *Service) Journal(ctx context.Context, agent string, limit int, before string, keep func(Entry) bool) ([]Entry, error) {
	if err := CheckAgent(agent); err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = DefaultLimit
	}
	limit = min(limit, MaxLimit)
	stop := ""
	if before != "" {
		ag, id, err := splitID(before)
		if err != nil || ag != agent {
			return nil, fmt.Errorf("%w: before names no entry of the agent %s", ErrInvalid, agent)
		}
		stop = journalKey(agent, id)
	}
	keys, err := bucket.EntriesUnder(ctx, s.b, "journal."+agentKey(agent))
	if err != nil {
		return nil, fmt.Errorf("knowhow: list the journal: %w", err)
	}
	out := []Entry{}
	for i := len(keys) - 1; i >= 0 && len(out) < limit; i-- {
		if stop != "" && keys[i].Key >= stop {
			continue
		}
		got, err := s.b.Get(ctx, keys[i].Key)
		if err != nil {
			continue
		}
		var e Entry
		if json.Unmarshal(got.Value, &e) != nil {
			continue
		}
		if keep == nil || keep(e) {
			out = append(out, e)
		}
	}
	return out, nil
}

// journalSince returns the entries of an agent at since or later.
func (s *Service) journalSince(ctx context.Context, agent string, since time.Time) ([]Entry, error) {
	keys, err := bucket.EntriesUnder(ctx, s.b, "journal."+agentKey(agent))
	if err != nil {
		return nil, err
	}
	var out []Entry
	for i := len(keys) - 1; i >= 0; i-- {
		id := keys[i].Key[strings.LastIndexByte(keys[i].Key, '.')+1:]
		if t, ok := fw.IDTime(id); ok && t.Before(since.Add(-time.Second)) {
			break
		}
		got, err := s.b.Get(ctx, keys[i].Key)
		if err != nil {
			continue
		}
		var e Entry
		if json.Unmarshal(got.Value, &e) == nil && !e.At.Before(since) {
			out = append(out, e)
		}
	}
	return out, nil
}

// UndoResult is the entry that an undo wrote, and the entry that it took
// back.
type UndoResult struct {
	Entry  Entry `json:"entry"`
	Undone Entry `json:"undone"`
}

// TakeBack takes back one entry of the journal for the caller by. A note
// gets the revision before the change; a note that the change created is
// put away. A routine makes the opposite move when docs/ROUTINES.md allows
// it. A later change of the same note or routine answers ErrLater. person
// says whether a person takes it back, which a move to live needs.
func (s *Service) TakeBack(ctx context.Context, public, by string, person bool) (UndoResult, error) {
	e, rev, err := s.entry(ctx, public)
	if err != nil {
		return UndoResult{}, err
	}
	if e.UndoneBy != "" {
		return UndoResult{}, fmt.Errorf("%w: this change was taken back already", ErrLater)
	}
	if e.Undo == nil {
		return UndoResult{}, fmt.Errorf("%w: this entry cannot be taken back", ErrInvalid)
	}
	var out Entry
	switch {
	case e.Subject.Routine != "":
		out, err = s.undoRoutine(ctx, e, by, person)
	case e.Subject.Note != "":
		out, err = s.undoNote(ctx, e, by)
	default:
		err = fmt.Errorf("%w: this entry names no note and no routine", ErrInvalid)
	}
	if err != nil {
		return UndoResult{}, err
	}
	// The entry names the entry that took it back. Another replica that
	// took it back at the same time made the note conflict already.
	e.UndoneBy = out.ID
	if raw, err := encode(e); err == nil {
		_, id, _ := splitID(e.ID)
		if _, err := s.b.Update(ctx, journalKey(e.Agent, id), raw, rev); err != nil {
			s.log.Warn("knowhow: an entry of the journal does not name its undo", "entry", e.ID, "error", err)
		}
	}
	emitChange(e.Agent)
	return UndoResult{Entry: out, Undone: e}, nil
}

// undoNote puts back the revision of a note before the change of e.
func (s *Service) undoNote(ctx context.Context, e Entry, by string) (Entry, error) {
	sc := e.Subject.scope()
	if err := checkNote(e.Agent, sc, e.Subject.Note); err != nil {
		return Entry{}, err
	}
	key := noteKey(e.Agent, sc, e.Subject.Note)
	var old Note
	if e.Undo.Revision != 0 {
		got, err := s.b.At(ctx, key, e.Undo.Revision)
		if errors.Is(err, bucket.ErrNotFound) {
			return Entry{}, fmt.Errorf("%w: the text before this change is no longer kept", ErrLater)
		}
		if err != nil {
			return Entry{}, err
		}
		if old, err = decodeNote(got.Value, got.Revision); err != nil {
			return Entry{}, err
		}
	}
	wrote, before, after, err := s.modify(ctx, key, func(value []byte, exists bool, rev uint64) ([]byte, error) {
		if !exists || rev != e.Undo.After {
			return nil, fmt.Errorf("%w: a later change touched the note %s; take back the later change first", ErrLater, e.Subject.Note)
		}
		if e.Undo.Revision == 0 {
			cur, err := decodeNote(value, rev)
			if err != nil {
				return nil, err
			}
			cur.State = StatePutAway
			return encodeNote(cur)
		}
		return encodeNote(old)
	})
	if err != nil {
		return Entry{}, err
	}
	if !wrote {
		return Entry{}, fmt.Errorf("%w: nothing to take back", ErrLater)
	}
	return s.appendJournal(ctx, Entry{Agent: e.Agent, What: WhatUndone, Subject: e.Subject, Why: undoWhy(e), By: by,
		Undo: &Undo{Revision: before, After: after}})
}

// undoWhy is the sentence of an undo.
func undoWhy(e Entry) string {
	if e.Subject.Routine != "" {
		return fmt.Sprintf("A person took back the move of the routine %s to %s.", e.Subject.Routine, e.Undo.To)
	}
	return fmt.Sprintf("A person took back a change of the note %s (%s).", e.Subject.Note, e.What)
}

// undoRoutine makes the opposite move of a routine.
func (s *Service) undoRoutine(ctx context.Context, e Entry, by string, person bool) (Entry, error) {
	st := s.routineStore()
	if st == nil {
		return Entry{}, fmt.Errorf("%w: the routines are off in this process", ErrInvalid)
	}
	if e.Undo.From == "" || e.Undo.To == "" {
		return Entry{}, fmt.Errorf("%w: this move of a routine cannot be taken back", ErrInvalid)
	}
	rec, err := st.Get(ctx, e.Agent, e.Subject.Routine)
	if err != nil {
		return Entry{}, err
	}
	if rec.Standing != e.Undo.To || (e.Undo.Version != 0 && rec.Version() != e.Undo.Version) {
		return Entry{}, fmt.Errorf("%w: the routine %s moved or changed since; it is %s at version %d", ErrLater, e.Subject.Routine, rec.Standing, rec.Version())
	}
	if !routines.CanMove(e.Undo.To, e.Undo.From) {
		return Entry{}, fmt.Errorf("%w: a %s routine cannot move back to %s", ErrLater, e.Undo.To, e.Undo.From)
	}
	_, moved, err := st.Move(withSelf(ctx), routines.MoveRequest{Agent: e.Agent, Name: e.Subject.Routine, To: e.Undo.From, By: by, Person: person,
		Note: "taken back from the journal", Version: rec.Version(), Check: s.checkFor(e.Agent)})
	if err != nil {
		return Entry{}, err
	}
	if !moved {
		return Entry{}, fmt.Errorf("%w: nothing to take back", ErrLater)
	}
	return s.appendJournal(ctx, Entry{Agent: e.Agent, What: WhatUndone, Subject: e.Subject, Why: undoWhy(e), By: by,
		Undo: &Undo{From: e.Undo.To, To: e.Undo.From, Version: rec.Version()}})
}

// routineStore returns the store of the routines, or nil.
func (s *Service) routineStore() *routines.Store {
	if s.routines == nil {
		return nil
	}
	return s.routines()
}

// checkFor checks the routines that reach the profile of an agent.
func (s *Service) checkFor(agent string) func(context.Context, []fw.Routine) error {
	if s.check == nil {
		return nil
	}
	return func(ctx context.Context, list []fw.Routine) error { return s.check(ctx, agent, list) }
}

// Settings is the switch of an agent.
type Settings struct {
	AskFirst bool      `json:"askFirst"`
	By       string    `json:"by,omitempty"`
	At       time.Time `json:"at,omitzero"`
}

func settingsKey(agent string) string { return "settings." + agentKey(agent) }

// Settings returns the switch of an agent. An agent without an entry has
// the switch off.
func (s *Service) Settings(ctx context.Context, agent string) (Settings, error) {
	if err := CheckAgent(agent); err != nil {
		return Settings{}, err
	}
	got, err := s.b.Get(ctx, settingsKey(agent))
	if errors.Is(err, bucket.ErrNotFound) {
		return Settings{}, nil
	}
	if err != nil {
		return Settings{}, fmt.Errorf("knowhow: read the switch of %s: %w", agent, err)
	}
	var st Settings
	if err := json.Unmarshal(got.Value, &st); err != nil {
		return Settings{}, fmt.Errorf("knowhow: decode the switch of %s: %w", agent, err)
	}
	return st, nil
}

// SetAskFirst turns the switch of an agent on or off.
func (s *Service) SetAskFirst(ctx context.Context, agent string, on bool, by string) (Settings, error) {
	if err := CheckAgent(agent); err != nil {
		return Settings{}, err
	}
	var out Settings
	wrote, _, _, err := s.modify(ctx, settingsKey(agent), func(value []byte, exists bool, _ uint64) ([]byte, error) {
		if exists {
			_ = json.Unmarshal(value, &out)
		}
		if exists && out.AskFirst == on {
			return nil, nil
		}
		out = Settings{AskFirst: on, By: by, At: s.now().UTC()}
		return encode(out)
	})
	if err != nil {
		return Settings{}, err
	}
	if wrote {
		emitChange(agent)
	}
	return out, nil
}

// AskFirst reports whether the switch of the agent is on. A switch that
// does not read counts as on, so nothing goes ahead without a person by
// mistake.
func (s *Service) AskFirst(ctx context.Context, agent string) bool {
	st, err := s.Settings(ctx, agent)
	if err != nil {
		s.log.Warn("knowhow: the switch of an agent does not read; the change waits for a person", "agent", agent, "error", err)
		return true
	}
	return st.AskFirst
}

// Waiting is one change that waits for a person under the switch. What is
// learned, updated, or put-away for a note, and rehearsing for a routine.
// Pin is what the target must still be for a keep: the hash of the note,
// waiting for a note that waits, or the version of a routine as v<n>.
type Waiting struct {
	ID        string    `json:"id"`
	At        time.Time `json:"at"`
	Agent     string    `json:"agent"`
	What      string    `json:"what"`
	Subject   Subject   `json:"subject"`
	Text      string    `json:"text,omitempty"`
	Sources   []Source  `json:"sources,omitempty"`
	Why       string    `json:"why,omitempty"`
	Runs      []string  `json:"runs,omitempty"`
	By        string    `json:"by"`
	Run       string    `json:"run,omitempty"`
	Pin       string    `json:"pin"`
	Origin    string    `json:"origin,omitempty"`
	Candidate string    `json:"candidate,omitempty"`
}

func waitingKey(agent, id string) string { return "waiting." + agentKey(agent) + "." + id }

// addWaiting puts a change on the waiting list of its agent. The list
// keeps at most MaxWaiting changes; past it, the oldest one goes.
func (s *Service) addWaiting(ctx context.Context, w Waiting) (Waiting, error) {
	w.At = s.now().UTC()
	id := s.newID(w.At)
	w.ID, w.Why = publicID(w.Agent, id), clipWhy(w.Why)
	raw, err := encode(w)
	if err != nil {
		return Waiting{}, err
	}
	if _, err := s.b.Create(ctx, waitingKey(w.Agent, id), raw); err != nil {
		return Waiting{}, fmt.Errorf("knowhow: write the waiting list of %s: %w", w.Agent, err)
	}
	if keys, err := bucket.EntriesUnder(ctx, s.b, "waiting."+agentKey(w.Agent)); err == nil && len(keys) > MaxWaiting {
		for _, k := range keys[:len(keys)-MaxWaiting] {
			_ = s.b.Delete(ctx, k.Key, k.Revision)
		}
	}
	emitChange(w.Agent)
	return w, nil
}

// WaitingList returns the changes that wait for a person, the oldest
// first. A nil keep keeps every change.
func (s *Service) WaitingList(ctx context.Context, agent string, keep func(Waiting) bool) ([]Waiting, error) {
	if err := CheckAgent(agent); err != nil {
		return nil, err
	}
	keys, err := bucket.EntriesUnder(ctx, s.b, "waiting."+agentKey(agent))
	if err != nil {
		return nil, fmt.Errorf("knowhow: list the waiting changes: %w", err)
	}
	out := []Waiting{}
	for _, k := range keys {
		got, err := s.b.Get(ctx, k.Key)
		if err != nil {
			continue
		}
		var w Waiting
		if json.Unmarshal(got.Value, &w) != nil {
			continue
		}
		if keep == nil || keep(w) {
			out = append(out, w)
		}
	}
	return out, nil
}

// waitingEntry reads one waiting change by its public id.
func (s *Service) waitingEntry(ctx context.Context, public string) (Waiting, uint64, error) {
	agent, id, err := splitID(public)
	if err != nil {
		return Waiting{}, 0, err
	}
	got, err := s.b.Get(ctx, waitingKey(agent, id))
	if errors.Is(err, bucket.ErrNotFound) {
		return Waiting{}, 0, fmt.Errorf("%w: no change waits with the id %s", ErrNotFound, public)
	}
	if err != nil {
		return Waiting{}, 0, err
	}
	var w Waiting
	if err := json.Unmarshal(got.Value, &w); err != nil {
		return Waiting{}, 0, err
	}
	return w, got.Revision, nil
}

// Decide keeps a waiting change or drops it for the person by. A target
// that changed since the change waits answers ErrLater, and the change
// stays on the list until a person drops it.
func (s *Service) Decide(ctx context.Context, public string, keep bool, by string) (*Entry, error) {
	w, rev, err := s.waitingEntry(ctx, public)
	if err != nil {
		return nil, err
	}
	var out *Entry
	if keep {
		if out, err = s.apply(ctx, w, by); err != nil {
			return nil, err
		}
	} else if w.What == WhatLearned {
		// A new note that waited goes away, and an undo brings it back.
		key := noteKey(w.Agent, w.Subject.scope(), w.Subject.Note)
		if _, err := s.setStateAny(ctx, key, func(n *Note) bool {
			if n.State != StateWaiting {
				return false
			}
			n.State = StatePutAway
			return true
		}); err != nil {
			return nil, err
		}
	}
	_, id, _ := splitID(w.ID)
	if err := s.b.Delete(ctx, waitingKey(w.Agent, id), rev); err != nil && !errors.Is(err, bucket.ErrConflict) {
		return out, err
	}
	emitChange(w.Agent)
	return out, nil
}

// apply makes a waiting change, if its target is still what it pinned.
func (s *Service) apply(ctx context.Context, w Waiting, by string) (*Entry, error) {
	if w.Subject.Routine != "" {
		return s.applyRoutine(ctx, w, by)
	}
	sc := w.Subject.scope()
	key := noteKey(w.Agent, sc, w.Subject.Note)
	cur, err := s.get(ctx, key)
	if errors.Is(err, ErrNotFound) {
		return nil, fmt.Errorf("%w: the note %s is gone", ErrLater, w.Subject.Note)
	}
	if err != nil {
		return nil, err
	}
	switch w.What {
	case WhatLearned:
		if cur.State != StateWaiting {
			return nil, fmt.Errorf("%w: the note %s changed since it waits", ErrLater, w.Subject.Note)
		}
		var n Note
		wrote, before, after, err := s.modify(ctx, key, func(value []byte, exists bool, rev uint64) ([]byte, error) {
			if !exists {
				return nil, fmt.Errorf("%w: the note %s is gone", ErrLater, w.Subject.Note)
			}
			var err error
			if n, err = decodeNote(value, rev); err != nil {
				return nil, err
			}
			if n.State != StateWaiting {
				return nil, fmt.Errorf("%w: the note %s changed since it waits", ErrLater, w.Subject.Note)
			}
			n.State = stateOf(n)
			return encodeNote(n)
		})
		if err != nil || !wrote {
			return nil, err
		}
		e, err := s.appendJournal(ctx, Entry{Agent: w.Agent, What: WhatLearned, Subject: w.Subject, Why: keptWhy(w), Runs: w.Runs, By: w.By,
			Undo: &Undo{Revision: before, After: after}})
		return &e, err
	case WhatUpdated:
		if cur.Hash != w.Pin || cur.State == StatePutAway {
			return nil, fmt.Errorf("%w: the note %s changed since this change waits", ErrLater, w.Subject.Note)
		}
		res, err := s.Write(ctx, WriteRequest{Agent: w.Agent, Scope: sc, Name: w.Subject.Note, Text: w.Text, IfHash: w.Pin, Sources: w.Sources,
			By: w.By, Run: w.Run, Origin: w.Origin, Why: keptWhy(w), Runs: w.Runs})
		if errors.Is(err, ErrConflict) {
			return nil, fmt.Errorf("%w: the note %s changed since this change waits", ErrLater, w.Subject.Note)
		}
		if err != nil {
			return nil, err
		}
		return res.Entry, nil
	case WhatPutAway:
		if cur.Hash != w.Pin {
			return nil, fmt.Errorf("%w: the note %s changed since this change waits", ErrLater, w.Subject.Note)
		}
		res, err := s.PutAway(ctx, PutAwayRequest{Agent: w.Agent, Scope: sc, Name: w.Subject.Note, IfHash: w.Pin, By: w.By, Run: w.Run, Why: keptWhy(w)})
		if errors.Is(err, ErrConflict) {
			return nil, fmt.Errorf("%w: the note %s changed since this change waits", ErrLater, w.Subject.Note)
		}
		if err != nil {
			return nil, err
		}
		return res.Entry, nil
	}
	return nil, fmt.Errorf("%w: the change %s cannot be kept", ErrInvalid, w.What)
}

// keptWhy is the sentence of a waiting change that a person kept.
func keptWhy(w Waiting) string {
	if w.Why != "" {
		return w.Why
	}
	return "A person kept a change that waited."
}

// setStateAny is setState without the journal, for a note that waits.
func (s *Service) setStateAny(ctx context.Context, key string, to func(n *Note) bool) (bool, error) {
	wrote, _, _, err := s.modify(ctx, key, func(value []byte, exists bool, rev uint64) ([]byte, error) {
		if !exists {
			return nil, nil
		}
		n, err := decodeNote(value, rev)
		if err != nil {
			return nil, err
		}
		if !to(&n) {
			return nil, nil
		}
		return encodeNote(n)
	})
	return wrote, err
}

// applyRoutine moves a proposed routine to rehearsing for a person.
func (s *Service) applyRoutine(ctx context.Context, w Waiting, by string) (*Entry, error) {
	st := s.routineStore()
	if st == nil {
		return nil, fmt.Errorf("%w: the routines are off in this process", ErrInvalid)
	}
	rec, err := st.Get(ctx, w.Agent, w.Subject.Routine)
	if err != nil {
		return nil, err
	}
	if fmt.Sprintf("v%d", rec.Version()) != w.Pin || rec.Standing != routines.Proposed {
		return nil, fmt.Errorf("%w: the routine %s moved or changed since this change waits", ErrLater, w.Subject.Routine)
	}
	_, moved, err := st.Move(withSelf(ctx), routines.MoveRequest{Agent: w.Agent, Name: w.Subject.Routine, To: routines.Rehearsing, By: by,
		Note: "kept from the waiting list of Know-how", Version: rec.Version(), Check: s.checkFor(w.Agent)})
	if err != nil || !moved {
		return nil, err
	}
	e, err := s.appendJournal(ctx, Entry{Agent: w.Agent, What: WhatRehearsing, Subject: w.Subject, Why: "A person kept the proposal, so the routine rehearses beside the agent.",
		Runs: w.Runs, By: by, Undo: &Undo{From: routines.Proposed, To: routines.Rehearsing, Version: rec.Version()}})
	return &e, err
}

// pruneWaiting drops the waiting changes older than waitingKeepDays.
func (s *Service) pruneWaiting(ctx context.Context, agent string) {
	keys, err := bucket.EntriesUnder(ctx, s.b, "waiting."+agentKey(agent))
	if err != nil {
		return
	}
	cut := s.now().UTC().AddDate(0, 0, -waitingKeepDays)
	for _, k := range keys {
		id := k.Key[strings.LastIndexByte(k.Key, '.')+1:]
		if t, ok := fw.IDTime(id); ok && t.Before(cut) {
			_ = s.b.Delete(ctx, k.Key, k.Revision)
		}
	}
}

// crockford is the alphabet of an id of fw.NewID.
const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// newID returns an id in the form of fw.NewID with the time t, so the
// keys of a journal sort by the clock of the service and fw.IDTime reads
// the time back. Two ids of one millisecond grow in the order of their
// making, as a monotonic ULID does.
func (s *Service) newID(t time.Time) string {
	ms := uint64(t.UnixMilli())
	s.mu.Lock()
	if ms <= s.idMS {
		ms = s.idMS
		for i := len(s.idRand) - 1; i >= 0; i-- {
			s.idRand[i]++
			if s.idRand[i] != 0 {
				break
			}
		}
	} else {
		_, _ = rand.Read(s.idRand[:])
		s.idRand[0] &= 0x7f // room to grow within the millisecond
	}
	s.idMS = ms
	var b [16]byte
	for i := 0; i < 6; i++ {
		b[i] = byte(ms >> (40 - 8*i))
	}
	copy(b[6:], s.idRand[:])
	s.mu.Unlock()
	hi := uint64(b[0])<<56 | uint64(b[1])<<48 | uint64(b[2])<<40 | uint64(b[3])<<32 | uint64(b[4])<<24 | uint64(b[5])<<16 | uint64(b[6])<<8 | uint64(b[7])
	lo := uint64(b[8])<<56 | uint64(b[9])<<48 | uint64(b[10])<<40 | uint64(b[11])<<32 | uint64(b[12])<<24 | uint64(b[13])<<16 | uint64(b[14])<<8 | uint64(b[15])
	var out [26]byte
	for i := 25; i >= 0; i-- {
		out[i] = crockford[lo&31]
		lo = lo>>5 | hi<<59
		hi >>= 5
	}
	return string(out[:])
}
