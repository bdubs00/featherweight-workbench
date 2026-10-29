package knowhow

// This file is the store of the notes (docs/KNOWHOW.md section 5.1). Each
// note is one entry of the bucket under note.<agent>.<scope>.<name>, and
// the bucket keeps its last 16 values, so an undo puts back the text
// before a change. The uses of a note live in an entry of their own,
// uses.<agent>.<scope>.<name>, so a use never pushes a text out of the
// history. A write reads the entry and writes at the revision that it
// read, so two replicas of the front never lose a write.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/memory"
	"github.com/bdubs00/featherweight-workbench/internal/bucket"
)

// The kinds of a scope at the front.
const (
	ScopeAgent     = "agent"
	ScopePrincipal = "principal"
)

// The states of a note.
const (
	StateChecked   = "checked"
	StateUnchecked = "unchecked"
	StateOutOfDate = "out-of-date"
	StateResting   = "resting"
	StatePutAway   = "put-away"
	StateWaiting   = "waiting"
)

// Writers of a note that are not a person.
const (
	ByAgent   = "agent"   // the memory tool of a run, through the kernel
	ByReview  = "review"  // the after-work review, through knowhow_keep
	ByKnowhow = "knowhow" // the passes of this package
	// PersonPrefix starts the writer of a person: person:<subject>.
	PersonPrefix = "person:"
)

// Bounds of a note.
const (
	MaxNoteBytes   = memory.MaxNoteBytes
	MaxNameBytes   = 512
	MaxSources     = 32
	MaxRefBytes    = 512
	MaxVersion     = 128
	MaxPrincipal   = 256
	IfAbsent       = "absent"
	IfExists       = "*"
	descriptionCut = 200
)

// Errors of the store.
var (
	// ErrInvalid means that the agent, the scope, the name, the text, the
	// sources, or a condition does not fit.
	ErrInvalid = errors.New("knowhow: the request is not valid")
	// ErrNotFound means that no note, entry, or candidate has the name.
	ErrNotFound = errors.New("knowhow: not found")
	// ErrConflict means that the condition ifHash of a write did not hold.
	ErrConflict = errors.New("knowhow: the note changed")
	// ErrLater means that a later change touched the note or the routine
	// of an undo or of a waiting change.
	ErrLater = errors.New("knowhow: a later change touched it")
	// ErrPerson means that automation tried to change a note that a person
	// wrote. Only a person changes such a note.
	ErrPerson = errors.New("knowhow: a person wrote this note, and only a person changes it")
	// ErrBusy means that other writers changed an entry each time a write
	// tried.
	ErrBusy = errors.New("knowhow: other writers changed the entry")
	// ErrOutOfDate means that an undo would make the agent trust a note
	// that went out of date. A person edits the note or puts it away.
	ErrOutOfDate = errors.New("knowhow: the note went out of date")
)

// ConflictError is a write whose condition did not hold. Hash is the hash
// of the note as it is, or empty when no note exists.
type ConflictError struct{ Hash string }

func (e *ConflictError) Error() string {
	if e.Hash == "" {
		return "This note does not exist. View the notes again before you change it."
	}
	return "This note changed in another run. View it again before you change it."
}

func (e *ConflictError) Is(target error) bool { return target == ErrConflict }

// Scope is the scope of a note at the front: the notes of the agent, or
// the notes of one principal of the agent.
type Scope struct {
	Kind      string `json:"kind"`
	Principal string `json:"principal,omitempty"`
}

// Source is where a note came from: run:<id>, page:<path>, or
// item:<source>/<key>, with the version of a page or an item.
type Source struct {
	Ref     string `json:"ref"`
	Version string `json:"version,omitempty"`
}

// OutOfDate says since when a note is out of date, which source changed,
// and the version of that source now: the hash of the page, the version
// of the item, or empty for a page that was removed.
type OutOfDate struct {
	Since   time.Time `json:"since"`
	Source  string    `json:"source"`
	Version string    `json:"version,omitempty"`
}

// Note is one note as the store keeps it. Uses and LastUsed come from the
// entry of its uses. Revision is the revision of its entry. Origin is
// untrusted for a note that the review kept from a candidate of that
// origin.
type Note struct {
	Agent       string     `json:"agent"`
	Scope       Scope      `json:"scope"`
	Name        string     `json:"name"`
	Text        string     `json:"text,omitempty"`
	Hash        string     `json:"hash"`
	Description string     `json:"description"`
	By          string     `json:"by"`
	Run         string     `json:"run,omitempty"`
	At          time.Time  `json:"at"`
	Sources     []Source   `json:"sources"`
	State       string     `json:"state"`
	Uses        int        `json:"uses"`
	LastUsed    *time.Time `json:"lastUsed,omitempty"`
	OutOfDate   *OutOfDate `json:"outOfDate,omitempty"`
	Origin      string     `json:"origin,omitempty"`
	Revision    uint64     `json:"revision"`
}

// Brief returns the note without its text, as a list shows it.
func (n Note) Brief() Note {
	n.Text = ""
	return n
}

// ByPerson reports whether a person wrote the note.
func (n Note) ByPerson() bool { return isPerson(n.By) }

func isPerson(by string) bool { return strings.HasPrefix(by, PersonPrefix) }

// Hash returns the hash of a text: the SHA-256 in hex.
func Hash(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// principalKey hides the subject of a principal in a key, as the kernel
// names the directory of a principal.
func principalKey(subject string) string {
	sum := sha256.Sum256([]byte(subject))
	return hex.EncodeToString(sum[:16])
}

func token(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }

// agentKey is the part of every key of an agent.
func agentKey(agent string) string { return token(agent) }

// scopeKey is the part of a key of a scope.
func scopeKey(sc Scope) string {
	if sc.Kind == ScopeAgent {
		return ScopeAgent + "._"
	}
	return ScopePrincipal + "." + principalKey(sc.Principal)
}

func noteKey(agent string, sc Scope, name string) string {
	return "note." + agentKey(agent) + "." + scopeKey(sc) + "." + token(name)
}

func usesKey(agent string, sc Scope, name string) string {
	return "uses." + agentKey(agent) + "." + scopeKey(sc) + "." + token(name)
}

// CheckAgent checks the name of an agent as the kernel checks the name of
// a profile for its memory.
func CheckAgent(agent string) error {
	if !memory.ValidAgentName(agent) {
		return fmt.Errorf("%w: %q is not the name of an agent profile; a name holds 1 to 64 letters, digits, '-', '_', or '.'", ErrInvalid, agent)
	}
	return nil
}

// CheckScope checks a scope: agent without a principal, or principal with
// one line of at most MaxPrincipal bytes.
func CheckScope(sc Scope) error {
	switch sc.Kind {
	case ScopeAgent:
		if sc.Principal != "" {
			return fmt.Errorf("%w: the scope agent names no principal", ErrInvalid)
		}
	case ScopePrincipal:
		if sc.Principal == "" {
			return fmt.Errorf("%w: the scope principal needs ?principal=", ErrInvalid)
		}
		if len(sc.Principal) > MaxPrincipal || !oneLine(sc.Principal) {
			return fmt.Errorf("%w: a principal is one line of at most %d bytes", ErrInvalid, MaxPrincipal)
		}
	default:
		return fmt.Errorf("%w: the scope %q is not agent or principal", ErrInvalid, sc.Kind)
	}
	return nil
}

// CheckName checks the name of a note as the kernel does (memory.CheckName),
// at most MaxNameBytes.
func CheckName(name string) error {
	if len(name) > MaxNameBytes {
		return fmt.Errorf("%w: a note name holds at most %d bytes", ErrInvalid, MaxNameBytes)
	}
	if err := memory.CheckName(name); err != nil {
		return fmt.Errorf("%w: %s", ErrInvalid, err.Error())
	}
	return nil
}

func oneLine(v string) bool {
	return utf8.ValidString(v) && !strings.ContainsFunc(v, unicode.IsControl)
}

// checkSources checks the sources of a note.
func checkSources(list []Source) ([]Source, error) {
	if len(list) > MaxSources {
		return nil, fmt.Errorf("%w: a note has at most %d sources", ErrInvalid, MaxSources)
	}
	out := make([]Source, 0, len(list))
	seen := map[Source]bool{}
	for _, src := range list {
		src.Ref, src.Version = strings.TrimSpace(src.Ref), strings.TrimSpace(src.Version)
		if src.Ref == "" || len(src.Ref) > MaxRefBytes || !oneLine(src.Ref) {
			return nil, fmt.Errorf("%w: a source is one line of at most %d bytes, such as run:<id>, page:<path>, or item:<source>/<key>", ErrInvalid, MaxRefBytes)
		}
		if len(src.Version) > MaxVersion || !oneLine(src.Version) {
			return nil, fmt.Errorf("%w: the version of a source is one line of at most %d bytes", ErrInvalid, MaxVersion)
		}
		if !seen[src] {
			seen[src] = true
			out = append(out, src)
		}
	}
	return out, nil
}

// checkable reports whether a source can go out of date: a page or an
// item with a version.
func checkable(src Source) bool {
	return src.Version != "" && (strings.HasPrefix(src.Ref, "page:") || strings.HasPrefix(src.Ref, "item:"))
}

// stateOf returns the state that the sources of a note give it: out of
// date, checked, or unchecked.
func stateOf(n Note) string {
	if n.OutOfDate != nil {
		return StateOutOfDate
	}
	for _, src := range n.Sources {
		if checkable(src) {
			return StateChecked
		}
	}
	return StateUnchecked
}

// describe returns the line that says what a note holds: the description
// of a front matter block, or else the first line with text, without its
// leading '#' marks, cut as the kernel cuts it.
func describe(text string) string {
	lines := strings.Split(text, "\n")
	i := 0
	if len(lines) > 0 && strings.TrimSpace(lines[0]) == "---" {
		for i = 1; i < len(lines); i++ {
			l := strings.TrimSpace(lines[i])
			if l == "---" {
				i++
				break
			}
			if v, ok := strings.CutPrefix(l, "description:"); ok {
				if v = strings.Trim(strings.TrimSpace(v), `"'`); v != "" {
					return clip(v)
				}
			}
		}
	}
	for ; i < len(lines); i++ {
		if l := strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(lines[i]), "#")); l != "" {
			return clip(l)
		}
	}
	return ""
}

func clip(s string) string {
	s = memory.ClipLine(s)
	if utf8.RuneCountInString(s) > descriptionCut {
		r := []rune(s)
		s = string(r[:descriptionCut-1]) + "…"
	}
	return s
}

// encode writes a value of the bucket.
func encode(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, fmt.Errorf("knowhow: encode: %w", err)
	}
	return buf.Bytes(), nil
}

// decodeNote reads an entry of a note. The uses stay with their own entry.
func decodeNote(value []byte, rev uint64) (Note, error) {
	var n Note
	if err := json.Unmarshal(value, &n); err != nil {
		return Note{}, fmt.Errorf("knowhow: decode a note: %w", err)
	}
	if n.Name == "" || n.Agent == "" {
		return Note{}, errors.New("knowhow: a note has no name or no agent")
	}
	n.Revision, n.Uses, n.LastUsed = rev, 0, nil
	if n.Sources == nil {
		n.Sources = []Source{}
	}
	return n, nil
}

// encodeNote writes a note without the fields of other entries.
func encodeNote(n Note) ([]byte, error) {
	n.Uses, n.LastUsed, n.Revision = 0, nil, 0
	return encode(n)
}

// modify reads the newest value of key, gives it to f, and writes what f
// returns at the revision that it read, as bucket.Modify does. It also
// returns the revision that it read (0 for none) and the revision that it
// wrote.
func (s *Service) modify(ctx context.Context, key string, f func(value []byte, exists bool, rev uint64) ([]byte, error)) (wrote bool, before, after uint64, err error) {
	for range writeAttempts {
		cur, err := s.b.Get(ctx, key)
		exists := err == nil
		if err != nil && !errors.Is(err, bucket.ErrNotFound) {
			return false, 0, 0, err
		}
		if !exists {
			cur = bucket.Entry{}
		}
		value, err := f(cur.Value, exists, cur.Revision)
		if err != nil || value == nil {
			return false, cur.Revision, cur.Revision, err
		}
		var rev uint64
		if exists {
			rev, err = s.b.Update(ctx, key, value, cur.Revision)
		} else {
			rev, err = s.b.Create(ctx, key, value)
		}
		if errors.Is(err, bucket.ErrConflict) {
			continue
		}
		if err != nil {
			return false, cur.Revision, 0, err
		}
		return true, cur.Revision, rev, nil
	}
	return false, 0, 0, fmt.Errorf("%w %d times while a write of %s tried; try again", ErrBusy, writeAttempts, key)
}

// cachedNote is a note that the service read at a revision.
type cachedNote struct {
	rev  uint64
	note Note
}

// cachedUses is the uses of a note that the service read at a revision.
type cachedUses struct {
	rev  uint64
	uses usesEntry
}

// usesEntry counts the uses of a note. Seen holds the ids of the last
// events that it counted, so a second delivery and a second replica count
// an event once.
type usesEntry struct {
	Uses     int       `json:"uses"`
	LastUsed time.Time `json:"lastUsed"`
	Seen     []string  `json:"seen,omitempty"`
}

// noteList returns the notes under the prefix of keys, by key, with their
// uses, reading only the entries whose revision changed since the last
// read of this process.
func (s *Service) noteList(ctx context.Context, agent string) (map[string]Note, error) {
	prefix := "note"
	if agent != "" {
		prefix += "." + agentKey(agent)
	}
	entries, err := bucket.EntriesUnder(ctx, s.b, prefix)
	if err != nil {
		return nil, fmt.Errorf("knowhow: list the notes: %w", err)
	}
	out := map[string]Note{}
	for _, e := range entries {
		s.mu.Lock()
		c, hit := s.notes[e.Key]
		s.mu.Unlock()
		if hit && c.rev == e.Revision {
			out[e.Key] = c.note
			continue
		}
		got, err := s.b.Get(ctx, e.Key)
		if errors.Is(err, bucket.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("knowhow: read %s: %w", e.Key, err)
		}
		n, err := decodeNote(got.Value, got.Revision)
		if err != nil {
			s.log.Warn("knowhow: an entry of a note does not decode; it is left out", "key", e.Key, "error", err)
			continue
		}
		s.mu.Lock()
		s.notes[e.Key] = cachedNote{rev: got.Revision, note: n}
		s.mu.Unlock()
		out[e.Key] = n
	}
	uses, err := s.usesList(ctx, agent)
	if err != nil {
		return nil, err
	}
	for key, n := range out {
		if u, ok := uses["uses"+strings.TrimPrefix(key, "note")]; ok {
			n.Uses = u.Uses
			if !u.LastUsed.IsZero() {
				t := u.LastUsed
				n.LastUsed = &t
			}
			out[key] = n
		}
	}
	return out, nil
}

// usesList returns the uses entries of an agent, or of every agent, by key.
func (s *Service) usesList(ctx context.Context, agent string) (map[string]usesEntry, error) {
	prefix := "uses"
	if agent != "" {
		prefix += "." + agentKey(agent)
	}
	entries, err := bucket.EntriesUnder(ctx, s.b, prefix)
	if err != nil {
		return nil, fmt.Errorf("knowhow: list the uses: %w", err)
	}
	out := map[string]usesEntry{}
	for _, e := range entries {
		s.mu.Lock()
		c, hit := s.uses[e.Key]
		s.mu.Unlock()
		if hit && c.rev == e.Revision {
			out[e.Key] = c.uses
			continue
		}
		got, err := s.b.Get(ctx, e.Key)
		if err != nil {
			continue
		}
		var u usesEntry
		if json.Unmarshal(got.Value, &u) != nil {
			continue
		}
		s.mu.Lock()
		s.uses[e.Key] = cachedUses{rev: got.Revision, uses: u}
		s.mu.Unlock()
		out[e.Key] = u
	}
	return out, nil
}

// Get returns one note with its text and its uses.
func (s *Service) Get(ctx context.Context, agent string, sc Scope, name string) (Note, error) {
	if err := checkNote(agent, sc, name); err != nil {
		return Note{}, err
	}
	n, err := s.get(ctx, noteKey(agent, sc, name))
	if err != nil {
		return Note{}, err
	}
	if e, err := s.b.Get(ctx, usesKey(agent, sc, name)); err == nil {
		var u usesEntry
		if json.Unmarshal(e.Value, &u) == nil {
			n.Uses = u.Uses
			if !u.LastUsed.IsZero() {
				n.LastUsed = &u.LastUsed
			}
		}
	}
	return n, nil
}

// get reads the note of a key.
func (s *Service) get(ctx context.Context, key string) (Note, error) {
	e, err := s.b.Get(ctx, key)
	if errors.Is(err, bucket.ErrNotFound) {
		return Note{}, fmt.Errorf("%w: no note has the name", ErrNotFound)
	}
	if err != nil {
		return Note{}, fmt.Errorf("knowhow: read a note: %w", err)
	}
	return decodeNote(e.Value, e.Revision)
}

// List returns the notes of an agent, sorted by scope and name. A nil
// keep keeps every note. Put-away notes stay out unless putAway is true.
func (s *Service) List(ctx context.Context, agent string, putAway bool, keep func(Note) bool) ([]Note, error) {
	if err := CheckAgent(agent); err != nil {
		return nil, err
	}
	m, err := s.noteList(ctx, agent)
	if err != nil {
		return nil, err
	}
	out := []Note{}
	for _, n := range m {
		if n.Agent != agent || (n.State == StatePutAway && !putAway) || (keep != nil && !keep(n)) {
			continue
		}
		out = append(out, n.Brief())
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Scope.Kind != out[j].Scope.Kind {
			return out[i].Scope.Kind < out[j].Scope.Kind
		}
		if out[i].Scope.Principal != out[j].Scope.Principal {
			return out[i].Scope.Principal < out[j].Scope.Principal
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

func checkNote(agent string, sc Scope, name string) error {
	if err := CheckAgent(agent); err != nil {
		return err
	}
	if err := CheckScope(sc); err != nil {
		return err
	}
	return CheckName(name)
}

// hexHash matches a hash of 64 hex digits.
var hexHash = regexp.MustCompile(`^[0-9a-fA-F]{64}$`)

// checkIf checks a condition of a write: "", absent, *, or a hash.
func checkIf(cond string) (string, error) {
	cond = strings.Trim(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(cond), "W/")), `"`)
	switch {
	case cond == "", cond == IfAbsent, cond == IfExists:
		return cond, nil
	case hexHash.MatchString(cond):
		return strings.ToLower(cond), nil
	}
	return "", fmt.Errorf("%w: ifHash is the hash of the note that you read, %s for a new note, or %s for a note that exists", ErrInvalid, IfAbsent, IfExists)
}

// holds reports whether the condition holds for the note n, which exists
// when ok is true. A put-away note counts as no note for absent.
func holds(cond string, n Note, ok bool) bool {
	live := ok && n.State != StatePutAway
	switch cond {
	case "":
		return true
	case IfAbsent:
		return !live
	case IfExists:
		return live
	}
	return ok && n.Hash == cond
}

// WriteRequest is a write of the text of a note. By is agent, review,
// knowhow, or person:<subject>. Run is the run that wrote it, such as the
// run of the memory tool of a kernel. Why, Runs, and Corrects go to the
// journal, and without Runs the entry names Run (runsOf). Hold makes the
// change wait for a person while the switch of the agent is on. Candidate
// names the candidate of a write of the review.
type WriteRequest struct {
	Agent   string
	Scope   Scope
	Name    string
	Text    string
	IfHash  string
	Sources []Source
	By      string
	Run     string
	Origin  string
	Why     string
	Runs    []string
	Hold    bool
	// Candidate names the candidate of a write of the review.
	Candidate string
}

// WriteResult says what a write did: Created for a new note, Changed when
// the text, the sources, or the state changed, and Waiting for a change
// that waits for a person.
type WriteResult struct {
	Note    Note
	Created bool
	Changed bool
	Waiting *Waiting
	Entry   *Entry
}

// Write writes the text of a note, as the condition IfHash allows. A note
// that a person wrote changes only by a person. With Hold and the switch
// of the agent on, a new note waits in the state waiting, and a change of
// a note that exists waits beside it, and the note stays as it is.
func (s *Service) Write(ctx context.Context, req WriteRequest) (WriteResult, error) {
	if err := checkNote(req.Agent, req.Scope, req.Name); err != nil {
		return WriteResult{}, err
	}
	if len(req.Text) > MaxNoteBytes || !utf8.ValidString(req.Text) {
		return WriteResult{}, fmt.Errorf("%w: the text of a note is UTF-8 of at most %d bytes", ErrInvalid, MaxNoteBytes)
	}
	cond, err := checkIf(req.IfHash)
	if err != nil {
		return WriteResult{}, err
	}
	sources, err := checkSources(req.Sources)
	if err != nil {
		return WriteResult{}, err
	}
	if req.By == "" {
		req.By = ByAgent
	}
	hold := req.Hold && !isPerson(req.By) && s.AskFirst(ctx, req.Agent)
	key := noteKey(req.Agent, req.Scope, req.Name)
	given := sources
	var res WriteResult
	var cur Note
	var existed bool
	wrote, before, after, err := s.modify(ctx, key, func(value []byte, exists bool, rev uint64) ([]byte, error) {
		res, cur, existed = WriteResult{}, Note{}, exists
		if exists {
			var err error
			if cur, err = decodeNote(value, rev); err != nil {
				return nil, err
			}
		}
		if !holds(cond, cur, exists) {
			hash := ""
			if exists {
				hash = cur.Hash
			}
			return nil, &ConflictError{Hash: hash}
		}
		live := exists && cur.State != StatePutAway
		if live && cur.ByPerson() && !isPerson(req.By) {
			return nil, ErrPerson
		}
		sources = given
		if live && cur.OutOfDate != nil && isPerson(req.By) {
			sources = vouched(sources, *cur.OutOfDate)
		}
		hash := Hash(req.Text)
		if live && cur.Hash == hash && slices.Equal(cur.Sources, sources) {
			return nil, nil
		}
		if hold && live && cur.State != StateWaiting {
			// The note stays as it is, and the change waits beside it.
			return nil, nil
		}
		now := s.now().UTC()
		n := Note{Agent: req.Agent, Scope: req.Scope, Name: req.Name, Text: req.Text, Hash: hash, Description: describe(req.Text),
			By: req.By, Run: req.Run, At: now, Sources: sources, Origin: req.Origin}
		n.State = stateOf(n)
		if hold {
			n.State = StateWaiting
		}
		res.Created, res.Changed = !live, true
		res.Note = n
		return encodeNote(n)
	})
	if err != nil {
		return WriteResult{}, wrapWrite(err, req.Name)
	}
	live := existed && cur.State != StatePutAway
	if !wrote {
		if hold && live && cur.State != StateWaiting && (cur.Hash != Hash(req.Text) || !slices.Equal(cur.Sources, sources)) {
			w, err := s.addWaiting(ctx, Waiting{Agent: req.Agent, What: WhatUpdated, Subject: noteSubject(req.Scope, req.Name), Text: req.Text,
				Sources: sources, Why: req.Why, Runs: req.Runs, By: req.By, Run: req.Run, Pin: cur.Hash, Origin: req.Origin, Candidate: req.Candidate})
			if err != nil {
				return WriteResult{}, err
			}
			cur.Text = ""
			return WriteResult{Note: cur, Waiting: &w}, nil
		}
		return WriteResult{Note: cur}, nil
	}
	res.Note.Revision = after
	what, why := WhatLearned, req.Why
	if live {
		what = WhatUpdated
	}
	if why == "" {
		why = writeWhy(req.By, live)
	}
	if res.Note.State == StateWaiting {
		if !live || cur.State != StateWaiting {
			w, err := s.addWaiting(ctx, Waiting{Agent: req.Agent, What: WhatLearned, Subject: noteSubject(req.Scope, req.Name),
				Why: why, Runs: req.Runs, By: req.By, Run: req.Run, Pin: StateWaiting, Origin: req.Origin, Candidate: req.Candidate})
			if err != nil {
				s.log.Warn("knowhow: a waiting note has no entry on the waiting list", "agent", req.Agent, "error", err)
			} else {
				res.Waiting = &w
			}
		}
		emitChange(req.Agent)
		return res, nil
	}
	e, err := s.appendJournal(ctx, Entry{Agent: req.Agent, What: what, Subject: noteSubject(req.Scope, req.Name), Why: why,
		Runs: runsOf(req.Runs, req.Run), By: req.By, Undo: &Undo{Revision: before, After: after}, Corrects: live && cur.State == StateOutOfDate})
	if err != nil {
		s.log.Warn("knowhow: the journal did not take a change", "agent", req.Agent, "error", err)
	} else {
		res.Entry = &e
	}
	emitChange(req.Agent)
	return res, nil
}

// runsOf returns the runs of a journal entry: runs, or else the one run
// that made the change, so the entry of a note that the memory tool of a
// kernel wrote links its run too.
func runsOf(runs []string, run string) []string {
	if len(runs) == 0 && run != "" {
		return []string{run}
	}
	return runs
}

// vouched returns the sources of a note that a person corrected after it
// went out of date. The person read the source as it is now, so the
// source that changed takes its new version, and a page that was removed
// leaves the list. Without this, the note would say checked against the
// old version of its source.
func vouched(list []Source, ood OutOfDate) []Source {
	out := make([]Source, 0, len(list))
	for _, src := range list {
		if src.Ref == ood.Source && src.Version != ood.Version {
			if ood.Version == "" {
				continue
			}
			src.Version = ood.Version
		}
		out = append(out, src)
	}
	return out
}

// writeWhy is the sentence of the journal for a write that the review did
// not explain.
func writeWhy(by string, changed bool) string {
	switch {
	case isPerson(by) && changed:
		return "A person changed this note."
	case isPerson(by):
		return "A person wrote this note."
	case changed:
		return "The agent changed this note in a run."
	}
	return "The agent wrote this note in a run."
}

// wrapWrite words an error of a write.
func wrapWrite(err error, name string) error {
	switch {
	case errors.Is(err, ErrConflict), errors.Is(err, ErrPerson), errors.Is(err, ErrInvalid), errors.Is(err, ErrBusy), errors.Is(err, ErrNotFound):
		return err
	}
	return fmt.Errorf("knowhow: write the note %s: %w", name, err)
}

// PutAwayRequest puts a note away.
type PutAwayRequest struct {
	Agent  string
	Scope  Scope
	Name   string
	IfHash string
	By     string
	Run    string
	Why    string
	Hold   bool
}

// PutAway puts a note away: it keeps its record and its history, so an
// undo brings it back. A note that a person wrote is put away only by a
// person. With Hold and the switch on, the change waits for a person.
func (s *Service) PutAway(ctx context.Context, req PutAwayRequest) (WriteResult, error) {
	if err := checkNote(req.Agent, req.Scope, req.Name); err != nil {
		return WriteResult{}, err
	}
	cond, err := checkIf(req.IfHash)
	if err != nil {
		return WriteResult{}, err
	}
	if req.By == "" {
		req.By = ByAgent
	}
	hold := req.Hold && !isPerson(req.By) && s.AskFirst(ctx, req.Agent)
	var cur Note
	wrote, before, after, err := s.modify(ctx, noteKey(req.Agent, req.Scope, req.Name), func(value []byte, exists bool, rev uint64) ([]byte, error) {
		if !exists {
			return nil, fmt.Errorf("%w: no note has the name %s", ErrNotFound, req.Name)
		}
		var err error
		if cur, err = decodeNote(value, rev); err != nil {
			return nil, err
		}
		if !holds(cond, cur, true) {
			return nil, &ConflictError{Hash: cur.Hash}
		}
		if cur.State == StatePutAway {
			return nil, nil
		}
		if cur.ByPerson() && !isPerson(req.By) {
			return nil, ErrPerson
		}
		if hold && cur.State != StateWaiting {
			return nil, nil
		}
		n := cur
		n.State = StatePutAway
		return encodeNote(n)
	})
	if err != nil {
		return WriteResult{}, wrapWrite(err, req.Name)
	}
	if !wrote {
		if hold && cur.State != StatePutAway && cur.State != StateWaiting {
			w, err := s.addWaiting(ctx, Waiting{Agent: req.Agent, What: WhatPutAway, Subject: noteSubject(req.Scope, req.Name),
				Why: req.Why, By: req.By, Run: req.Run, Pin: cur.Hash})
			if err != nil {
				return WriteResult{}, err
			}
			cur.Text = ""
			return WriteResult{Note: cur, Waiting: &w}, nil
		}
		return WriteResult{Note: cur}, nil
	}
	cur.State, cur.Revision = StatePutAway, after
	why := req.Why
	if why == "" {
		why = "The agent put this note away in a run."
		if isPerson(req.By) {
			why = "A person put this note away."
		}
	}
	res := WriteResult{Note: cur, Changed: true}
	e, err := s.appendJournal(ctx, Entry{Agent: req.Agent, What: WhatPutAway, Subject: noteSubject(req.Scope, req.Name), Why: why,
		Runs: runsOf(nil, req.Run), By: req.By, Undo: &Undo{Revision: before, After: after}, Corrects: cur.OutOfDate != nil})
	if err == nil {
		res.Entry = &e
	}
	emitChange(req.Agent)
	return res, nil
}

// setState moves a note of a key to the state that to returns, by
// automation: a note that a person wrote never changes. to gets the note
// and returns the new note, or false for no change. It writes the journal
// with what and why, and reports whether the note changed.
func (s *Service) setState(ctx context.Context, key, what, why string, runs []string, to func(n *Note) bool) (bool, error) {
	var n Note
	wrote, before, after, err := s.modify(ctx, key, func(value []byte, exists bool, rev uint64) ([]byte, error) {
		if !exists {
			return nil, nil
		}
		var err error
		if n, err = decodeNote(value, rev); err != nil {
			return nil, err
		}
		if n.ByPerson() || !to(&n) {
			return nil, nil
		}
		return encodeNote(n)
	})
	if err != nil || !wrote {
		return false, err
	}
	if what != "" {
		// A note that went out of date has no undo: an undo would make the
		// agent trust the note over its source again. A person edits the
		// note or puts it away.
		undo := &Undo{Revision: before, After: after}
		if what == WhatOutOfDate {
			undo = nil
		}
		if _, err := s.appendJournal(ctx, Entry{Agent: n.Agent, What: what, Subject: noteSubject(n.Scope, n.Name), Why: why, Runs: runs,
			By: ByKnowhow, Undo: undo}); err != nil {
			s.log.Warn("knowhow: the journal did not take a change", "agent", n.Agent, "error", err)
		}
	}
	emitChange(n.Agent)
	return true, nil
}

// timeOf returns the time of an id of fw.NewID, or zero.
func timeOf(id string) time.Time {
	t, _ := fw.IDTime(id)
	return t
}

// decodeToken reads a part of a key that token wrote.
func decodeToken(t string) (string, error) {
	b, err := base64.RawURLEncoding.DecodeString(t)
	return string(b), err
}
