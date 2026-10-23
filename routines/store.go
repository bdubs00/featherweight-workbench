package routines

// This file is the store of the routines (docs/ARCHITECTURE.md section
// 21.1). Each routine is one entry of a bucket (package internal/bucket)
// under the key <agent>/<name>, and its evidence is the entry
// <agent>/<name>/evidence. A write reads the entry and writes at the
// revision that it read, so two replicas of the front never lose a write:
// the loser reads again and decides again.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/memory"
	"github.com/bdubs00/featherweight-workbench/internal/bucket"
)

// Bounds of the store.
const (
	// MaxPerAgent is the most routines of one agent that are not
	// retired. The kernel bounds the routines of a profile by the same
	// number.
	MaxPerAgent = fw.MaxRoutines
	// MaxHistory is how many moves the history of a routine keeps, the
	// newest ones.
	MaxHistory = 256
	// History is how many values of each entry the bucket keeps.
	History = 10
	// MaxNoteBytes bounds the note of a move.
	MaxNoteBytes = 512
	// writeAttempts bounds the tries of a write that other writers race.
	writeAttempts = 8
	// maxValue bounds one entry of the bucket.
	maxValue = 768 << 10
)

// Errors of the store.
var (
	// ErrInvalid means that the agent, the name, the body, the standing,
	// or the note does not fit.
	ErrInvalid = errors.New("routines: the request is not valid")
	// ErrNotFound means that the agent has no routine of the name.
	ErrNotFound = errors.New("routines: no routine has the name")
	// ErrLimit means that the agent has MaxPerAgent routines already.
	ErrLimit = errors.New("routines: the agent has the most routines")
	// ErrConflict means that other writers changed the routine each time
	// a write tried.
	ErrConflict = errors.New("routines: the routine changed")
	// ErrRetired means that a write named a retired routine, which stays
	// retired.
	ErrRetired = errors.New("routines: the routine is retired")
)

// MoveError is a move that the table of the standings forbids. Allowed
// names the standings that the routine can move to.
type MoveError struct {
	From, To string
	Allowed  []string
}

func (e *MoveError) Error() string {
	return fmt.Sprintf("a %s routine cannot move to %s. It can move to %s", e.From, e.To, movesText(e.From))
}

// VersionError is a move that names a version of the body other than the
// version of the record, so the person decided on a body that changed
// since. Version and Standing are those of the record.
type VersionError struct {
	Want, Version int
	Standing      string
}

func (e *VersionError) Error() string {
	return fmt.Sprintf("the move names version %d, and the routine is at version %d now. Read it again", e.Want, e.Version)
}

// SignOff says who signed a routine off, and when.
type SignOff struct {
	By string    `json:"by"`
	At time.Time `json:"at"`
}

// Move is one entry of the history of a routine: a standing that the
// routine took, who moved it, when, why, and the version of its body.
type Move struct {
	At       time.Time `json:"at"`
	By       string    `json:"by"`
	Standing string    `json:"standing"`
	Note     string    `json:"note"`
	Version  int       `json:"version,omitempty"`
}

// Source says where a routine comes from: how many runs it was made from,
// the first and the last day of those runs, and what proposed it.
type Source struct {
	Runs       int    `json:"runs,omitempty"`
	From       string `json:"from,omitempty"`
	To         string `json:"to,omitempty"`
	ProposedBy string `json:"proposedBy,omitempty"`
}

// Record is a routine as the store keeps it. Routine is the body in the
// shape of the kernel, whose Version is the version of the record; the
// store keeps no standing of the kernel in it, and as JSON the body has
// none. Standing is the standing of the workbench. Ready is true for a
// rehearsing routine whose evidence passes the rule of the configuration.
// SignedOff is set by the last move to live. History holds the moves,
// oldest first. Evidence is set only where a route asks for it.
type Record struct {
	Agent     string     `json:"agent"`
	Routine   fw.Routine `json:"routine"`
	Standing  string     `json:"standing"`
	Ready     bool       `json:"ready"`
	SignedOff *SignOff   `json:"signedOff,omitempty"`
	History   []Move     `json:"history"`
	Source    *Source    `json:"source,omitempty"`
	CreatedAt time.Time  `json:"createdAt"`
	UpdatedAt time.Time  `json:"updatedAt"`
	Evidence  *Evidence  `json:"evidence,omitempty"`
}

// Name returns the name of the routine.
func (r Record) Name() string { return r.Routine.Name }

// Version returns the version of the body.
func (r Record) Version() int { return r.Routine.Version }

// MarshalJSON writes the record with the body without its standing, which
// the store does not keep.
func (r Record) MarshalJSON() ([]byte, error) {
	type plain Record
	b, err := json.Marshal(r.Routine)
	if err != nil {
		return nil, err
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(b, &body); err != nil {
		return nil, err
	}
	delete(body, "standing")
	out := struct {
		plain
		Routine map[string]json.RawMessage `json:"routine"`
	}{plain: plain(r), Routine: body}
	return json.Marshal(out)
}

// Store keeps the routines of the agents and their evidence. Build it
// with New.
type Store struct {
	b   bucket.Bucket
	cfg Config
	log *slog.Logger
	now func() time.Time

	mu    sync.Mutex
	cache map[string]cached // by key: the record at a revision

	// proj is the projection of the evidence, which Start runs, and
	// summaries the summaries that hold for a minute (summary.go).
	proj      *projector
	summaries summaries
}

// cached is a record that the store read at a revision.
type cached struct {
	rev uint64
	rec Record
}

// New returns the store of the routines on the bucket b with the
// thresholds of c.
func New(b bucket.Bucket, c Config, log *slog.Logger) *Store {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Store{b: b, cfg: c, log: log, now: time.Now, cache: map[string]cached{}}
}

// Describe names the place of the store for a person.
func (s *Store) Describe() string { return s.b.Describe() }

// CheckAgent checks the name of an agent as the kernel checks the name of
// a profile for its memory, and that it fits a key of the bucket.
func CheckAgent(agent string) error {
	if !memory.ValidAgentName(agent) || strings.Contains(agent, "..") {
		return fmt.Errorf("%w: %q is not the name of an agent profile; a name holds 1 to 64 letters, digits, '-', '_', or '.', and no two dots in a row", ErrInvalid, agent)
	}
	return nil
}

// CheckName checks the name of a routine as the kernel does
// (fw.CheckRoutineName): 1 to 64 of a-z, 0-9, and -.
func CheckName(name string) error {
	if err := fw.CheckRoutineName(name); err != nil {
		return fmt.Errorf("%w: %s", ErrInvalid, strings.TrimPrefix(err.Error(), fw.ErrInvalidRequest.Error()+": "))
	}
	return nil
}

func keyOf(agent, name string) string         { return agent + "/" + name }
func evidenceKeyOf(agent, name string) string { return agent + "/" + name + "/evidence" }

// splitKey returns the agent and the name of a key of a record, and false
// for a key of evidence or a key that the store does not write.
func splitKey(key string) (agent, name string, ok bool) {
	parts := strings.Split(key, "/")
	if len(parts) != 2 || CheckAgent(parts[0]) != nil || CheckName(parts[1]) != nil {
		return "", "", false
	}
	return parts[0], parts[1], true
}

// decodeRecord reads an entry of a record.
func decodeRecord(value []byte) (Record, error) {
	var rec Record
	if err := json.Unmarshal(value, &rec); err != nil {
		return Record{}, fmt.Errorf("routines: decode a record: %w", err)
	}
	if !ValidStanding(rec.Standing) || rec.Routine.Name == "" {
		return Record{}, errors.New("routines: a record has no standing or no name")
	}
	return rec, nil
}

// encode writes a value of the bucket without the evidence of a record.
func encode(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, fmt.Errorf("routines: encode: %w", err)
	}
	return buf.Bytes(), nil
}

// get returns the record of a routine and its revision.
func (s *Store) get(ctx context.Context, agent, name string) (Record, uint64, error) {
	e, err := s.b.Get(ctx, keyOf(agent, name))
	if errors.Is(err, bucket.ErrNotFound) {
		return Record{}, 0, fmt.Errorf("%w: the agent %s has no routine %s", ErrNotFound, agent, name)
	}
	if err != nil {
		return Record{}, 0, fmt.Errorf("routines: read the routine %s of %s: %w", name, agent, err)
	}
	rec, err := decodeRecord(e.Value)
	if err != nil {
		return Record{}, 0, err
	}
	return rec, e.Revision, nil
}

// Get returns the record of a routine with its evidence.
func (s *Store) Get(ctx context.Context, agent, name string) (Record, error) {
	if err := checkKey(agent, name); err != nil {
		return Record{}, err
	}
	rec, _, err := s.get(ctx, agent, name)
	if err != nil {
		return Record{}, err
	}
	ev, _, err := s.evidence(ctx, agent, name)
	if err != nil {
		return Record{}, err
	}
	if ev == nil || ev.Version != rec.Version() {
		ev = newEvidence(rec.Version())
	}
	rec.Evidence = ev.public()
	return rec, nil
}

// checkKey checks the agent and the name of a routine.
func checkKey(agent, name string) error {
	if err := CheckAgent(agent); err != nil {
		return err
	}
	return CheckName(name)
}

// records returns every record of the store, by key, reading only the
// entries whose revision changed since the last read of this process.
func (s *Store) records(ctx context.Context) (map[string]Record, error) {
	keys, err := s.b.Keys(ctx)
	if err != nil {
		return nil, fmt.Errorf("routines: list the routines: %w", err)
	}
	out := map[string]Record{}
	live := map[string]bool{}
	for _, k := range keys {
		if _, _, ok := splitKey(k.Key); !ok {
			continue
		}
		live[k.Key] = true
		s.mu.Lock()
		c, hit := s.cache[k.Key]
		s.mu.Unlock()
		if hit && c.rev == k.Revision {
			out[k.Key] = c.rec
			continue
		}
		e, err := s.b.Get(ctx, k.Key)
		if errors.Is(err, bucket.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("routines: read %s: %w", k.Key, err)
		}
		rec, err := decodeRecord(e.Value)
		if err != nil {
			s.log.Warn("routines: an entry does not decode; the routine is left out", "key", k.Key, "error", err)
			continue
		}
		s.mu.Lock()
		s.cache[k.Key] = cached{rev: e.Revision, rec: rec}
		s.mu.Unlock()
		out[k.Key] = rec
	}
	s.mu.Lock()
	for key := range s.cache {
		if !live[key] {
			delete(s.cache, key)
		}
	}
	s.mu.Unlock()
	return out, nil
}

// List returns the records of the routines of an agent, without evidence,
// the newest change first.
func (s *Store) List(ctx context.Context, agent string) ([]Record, error) {
	if err := CheckAgent(agent); err != nil {
		return nil, err
	}
	all, err := s.records(ctx)
	if err != nil {
		return nil, err
	}
	out := []Record{}
	for _, rec := range all {
		if rec.Agent == agent {
			out = append(out, rec)
		}
	}
	newestFirst(out)
	return out, nil
}

// All returns the records of the routines of every agent, without
// evidence, the newest change first.
func (s *Store) All(ctx context.Context) ([]Record, error) {
	all, err := s.records(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Record, 0, len(all))
	for _, rec := range all {
		out = append(out, rec)
	}
	newestFirst(out)
	return out, nil
}

// newestFirst sorts records by their last change, newest first, then by
// agent and name.
func newestFirst(list []Record) {
	sort.Slice(list, func(i, j int) bool {
		if !list[i].UpdatedAt.Equal(list[j].UpdatedAt) {
			return list[i].UpdatedAt.After(list[j].UpdatedAt)
		}
		if list[i].Agent != list[j].Agent {
			return list[i].Agent < list[j].Agent
		}
		return list[i].Name() < list[j].Name()
	})
}

// Reaching returns the routines that reach the profiles of the agents, by
// agent: each rehearsing routine with the standing shadow of the kernel,
// and each live one with live, oldest first. An agent with a record in
// the store has an entry, also when none of its routines reaches a
// profile, so the store governs its routines.
func (s *Store) Reaching(ctx context.Context) (map[string][]fw.Routine, error) {
	all, err := s.records(ctx)
	if err != nil {
		return nil, err
	}
	byAgent := map[string][]Record{}
	for _, rec := range all {
		byAgent[rec.Agent] = append(byAgent[rec.Agent], rec)
	}
	out := make(map[string][]fw.Routine, len(byAgent))
	for agent, list := range byAgent {
		out[agent] = reachingOf(list)
	}
	return out, nil
}

// reachingOf returns the routines of the records that reach a profile,
// oldest first, each with the standing of the kernel.
func reachingOf(list []Record) []fw.Routine {
	sort.Slice(list, func(i, j int) bool {
		if !list[i].CreatedAt.Equal(list[j].CreatedAt) {
			return list[i].CreatedAt.Before(list[j].CreatedAt)
		}
		return list[i].Name() < list[j].Name()
	})
	out := []fw.Routine{}
	for _, rec := range list {
		if st, ok := kernelStanding(rec.Standing); ok {
			r := rec.Routine
			r.Standing = st
			out = append(out, r)
		}
	}
	return out
}

// reachingWith returns the routines of the agent that reach a profile
// when the record rec takes the place of the record of its name.
func (s *Store) reachingWith(ctx context.Context, rec Record) ([]fw.Routine, error) {
	all, err := s.records(ctx)
	if err != nil {
		return nil, err
	}
	list := []Record{rec}
	for _, r := range all {
		if r.Agent == rec.Agent && r.Name() != rec.Name() {
			list = append(list, r)
		}
	}
	return reachingOf(list), nil
}

// count returns how many routines of the agent are not retired.
func (s *Store) count(ctx context.Context, agent string) (int, error) {
	all, err := s.records(ctx)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, rec := range all {
		if rec.Agent == agent && rec.Standing != Retired {
			n++
		}
	}
	return n, nil
}

// PutRequest is a write of the body of a routine. Standing is the standing
// of a new routine, proposed when empty or rehearsing; for a routine that
// exists it is empty or its standing. Source and Replay are optional, and
// each replaces what the routine holds.
type PutRequest struct {
	Agent    string
	Routine  fw.Routine
	Standing string
	Source   *Source
	Replay   *Replay
	By       string
	Note     string
	// CreateOnly refuses a write of a routine that exists, so an operator
	// who proposes a new routine never changes one that appeared since.
	CreateOnly bool
	// Check checks the routines that reach the profiles of the agent with
	// this write, such as fleet.CheckRoutines. Nil checks nothing.
	Check func(ctx context.Context, list []fw.Routine) error
}

// PutResult says what a write did: Created for a new routine, and Changed
// when the body changed and the version went up.
type PutResult struct {
	Created, Changed bool
}

// Put creates or replaces the body of a routine. A body that differs from
// the body that the store holds gets the next version, and its evidence
// starts again. A live routine whose body changes goes back to
// rehearsing, because a new version rehearses again. The name of the body
// names the routine. The kernel checks the body as it checks the
// routines of a profile, without the deciders, which the workers hold.
func (s *Store) Put(ctx context.Context, req PutRequest) (Record, PutResult, error) {
	body := req.Routine
	if err := CheckAgent(req.Agent); err != nil {
		return Record{}, PutResult{}, err
	}
	if err := CheckName(body.Name); err != nil {
		return Record{}, PutResult{}, err
	}
	note, err := checkNote(req.Note)
	if err != nil {
		return Record{}, PutResult{}, err
	}
	body.Standing = ""
	trial := body
	trial.Standing, trial.Version = fw.RoutineShadow, 1
	if problems := fw.RoutineProblems(nil, []fw.Routine{trial}); len(problems) > 0 {
		// The kernel keys a problem from the list of a profile; here the
		// body is the only routine.
		return Record{}, PutResult{}, fmt.Errorf("%w: %s", ErrInvalid, strings.TrimPrefix(problems[0], "routines[0] ("+body.Name+")."))
	}
	if err := checkSource(req.Source); err != nil {
		return Record{}, PutResult{}, err
	}
	var rec, cur Record
	var res PutResult
	wrote, err := bucket.Modify(ctx, s.b, keyOf(req.Agent, body.Name), writeAttempts, func(value []byte, exists bool) ([]byte, error) {
		now := s.now().UTC()
		res, cur = PutResult{}, Record{}
		if exists {
			var err error
			if cur, err = decodeRecord(value); err != nil {
				return nil, err
			}
		}
		rec = cur
		body := body
		if !exists {
			standing := req.Standing
			switch standing {
			case "":
				standing = Proposed
			case Proposed, Rehearsing:
			default:
				return nil, fmt.Errorf("%w: a new routine starts proposed or rehearsing, not %s", ErrInvalid, standing)
			}
			n, err := s.count(ctx, req.Agent)
			if err != nil {
				return nil, err
			}
			if n >= MaxPerAgent {
				return nil, fmt.Errorf("%w: the agent %s has %d routines that are not retired, the most; retire one, or change one", ErrLimit, req.Agent, n)
			}
			body.Version = 1
			rec = Record{Agent: req.Agent, Routine: body, Standing: standing, CreatedAt: now}
			rec.History = []Move{{At: now, By: req.By, Standing: standing, Note: note, Version: 1}}
			res = PutResult{Created: true, Changed: true}
		} else {
			switch {
			case req.CreateOnly:
				return nil, fmt.Errorf("%w: the routine %s of %s exists, and an owner of the agent changes it", fw.ErrForbidden, body.Name, req.Agent)
			case req.Standing != "" && req.Standing != cur.Standing:
				return nil, fmt.Errorf("%w: the routine %s is %s; change its standing with POST /v1/routines/%s/%s/standing or routine_standing", ErrInvalid, body.Name, cur.Standing, req.Agent, body.Name)
			case cur.Standing == Retired:
				return nil, fmt.Errorf("%w: the routine %s of %s is retired and stays retired; give a new routine another name", ErrRetired, body.Name, req.Agent)
			}
			body.Version = cur.Version()
			if !sameBody(body, cur.Routine) {
				body.Version = cur.Version() + 1
				rec.Routine, rec.Ready = body, false
				res.Changed = true
				if cur.Standing == Live || cur.Standing == Paused {
					// A paused routine can go live with one move, so its
					// new version rehearses first too.
					rec.Standing, rec.SignedOff = Rehearsing, nil
					rec.History = append(slices.Clone(cur.History), Move{At: now, By: req.By, Standing: Rehearsing, Note: joinNote("a new version rehearses again", note), Version: body.Version})
				}
			}
		}
		if req.Source != nil {
			src := *req.Source
			rec.Source = &src
		}
		if !res.Changed && sameSource(cur.Source, rec.Source) {
			return nil, nil
		}
		if _, reaches := kernelStanding(rec.Standing); reaches && res.Changed && req.Check != nil {
			list, err := s.reachingWith(ctx, rec)
			if err != nil {
				return nil, err
			}
			if err := req.Check(ctx, list); err != nil {
				return nil, err
			}
		}
		rec.History = trimHistory(rec.History)
		rec.UpdatedAt = now
		rec.Evidence = nil
		return encode(rec)
	})
	if errors.Is(err, bucket.ErrConflict) {
		return Record{}, PutResult{}, fmt.Errorf("%w: other writers changed the routine %s of %s %d times while this write tried; try again", ErrConflict, body.Name, req.Agent, writeAttempts)
	}
	if err != nil {
		return Record{}, PutResult{}, fmt.Errorf("routines: write the routine %s of %s: %w", body.Name, req.Agent, err)
	}
	if !wrote {
		rec = cur
	}
	if res.Changed || req.Replay != nil {
		if err := s.resetEvidence(ctx, rec, res.Changed, req.Replay); err != nil {
			s.log.Warn("routines: the evidence of a new version did not start; the projection starts it at the next event", "agent", req.Agent, "routine", body.Name, "error", err)
		}
	}
	if wrote || req.Replay != nil {
		emitChange(req.Agent, body.Name)
	}
	return rec, res, nil
}

// MoveRequest is a move of a routine to another standing. Version, when
// it is not zero, is the version of the body that the caller read: a
// record of another version refuses the move with a *VersionError.
type MoveRequest struct {
	Agent, Name, To, Note, By string
	Version                   int
	// Person is true when a person makes the move, not the process or the
	// static token alone. A move to live needs a person.
	Person bool
	// Check is as for PutRequest.
	Check func(ctx context.Context, list []fw.Routine) error
}

// Move moves a routine to another standing, as the table of the standings
// allows, and records the move in the history. A move to live needs a
// person and sets SignedOff. A move to the standing that the routine has
// changes nothing. A move that the table forbids is a *MoveError. It
// reports whether the routine moved.
func (s *Store) Move(ctx context.Context, req MoveRequest) (Record, bool, error) {
	if err := checkKey(req.Agent, req.Name); err != nil {
		return Record{}, false, err
	}
	if !ValidStanding(req.To) {
		return Record{}, false, fmt.Errorf("%w: %q is not a standing; a routine is proposed, rehearsing, live, paused, or retired", ErrInvalid, req.To)
	}
	note, err := checkNote(req.Note)
	if err != nil {
		return Record{}, false, err
	}
	var rec Record
	wrote, err := bucket.Modify(ctx, s.b, keyOf(req.Agent, req.Name), writeAttempts, func(value []byte, exists bool) ([]byte, error) {
		if !exists {
			return nil, fmt.Errorf("%w: the agent %s has no routine %s", ErrNotFound, req.Agent, req.Name)
		}
		cur, err := decodeRecord(value)
		if err != nil {
			return nil, err
		}
		rec = cur
		switch {
		case req.Version != 0 && req.Version != cur.Version():
			return nil, &VersionError{Want: req.Version, Version: cur.Version(), Standing: cur.Standing}
		case cur.Standing == req.To:
			return nil, nil
		case !CanMove(cur.Standing, req.To):
			return nil, &MoveError{From: cur.Standing, To: req.To, Allowed: Moves(cur.Standing)}
		case req.To == Live && !req.Person:
			return nil, fmt.Errorf("%w: a person signs a routine off: a user of the issuer or the local user, not the process, the static token, or a workload", fw.ErrForbidden)
		}
		now := s.now().UTC()
		rec.Standing = req.To
		rec.History = trimHistory(append(slices.Clone(cur.History), Move{At: now, By: req.By, Standing: req.To, Note: note, Version: cur.Version()}))
		rec.UpdatedAt = now
		if req.To == Live {
			rec.SignedOff = &SignOff{By: req.By, At: now}
		}
		rec.Ready = false
		if req.To == Rehearsing {
			ev, _, err := s.evidence(ctx, req.Agent, req.Name)
			if err != nil {
				return nil, err
			}
			rec.Ready = ev != nil && ev.Version == rec.Version() && ev.Ready(s.cfg)
		}
		if _, reaches := kernelStanding(req.To); reaches && req.Check != nil {
			list, err := s.reachingWith(ctx, rec)
			if err != nil {
				return nil, err
			}
			if err := req.Check(ctx, list); err != nil {
				return nil, err
			}
		}
		return encode(rec)
	})
	if errors.Is(err, bucket.ErrConflict) {
		return Record{}, false, fmt.Errorf("%w: other writers changed the routine %s of %s %d times while this move tried; try again", ErrConflict, req.Name, req.Agent, writeAttempts)
	}
	if err != nil {
		return Record{}, false, err
	}
	if wrote {
		emitChange(req.Agent, req.Name)
	}
	return rec, wrote, nil
}

// evidence returns the evidence of a routine and its revision, or nil and
// 0 when it has none yet. An entry that does not decode reads as none.
func (s *Store) evidence(ctx context.Context, agent, name string) (*Evidence, uint64, error) {
	e, err := s.b.Get(ctx, evidenceKeyOf(agent, name))
	if errors.Is(err, bucket.ErrNotFound) {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, fmt.Errorf("routines: read the evidence of %s of %s: %w", name, agent, err)
	}
	ev, err := decodeEvidence(e.Value)
	if err != nil {
		s.log.Warn("routines: the evidence does not decode; it starts again", "agent", agent, "routine", name, "error", err)
		return nil, e.Revision, nil
	}
	return ev, e.Revision, nil
}

// decodeEvidence reads an entry of evidence.
func decodeEvidence(value []byte) (*Evidence, error) {
	var ev Evidence
	if err := json.Unmarshal(value, &ev); err != nil {
		return nil, err
	}
	if ev.Questions == nil {
		ev.Questions = map[string][]Band{}
	}
	return &ev, nil
}

// resetEvidence starts the evidence of the version of rec again when fresh
// is true, and sets the result of a replay when replay is not nil.
func (s *Store) resetEvidence(ctx context.Context, rec Record, fresh bool, replay *Replay) error {
	_, err := s.updateEvidence(ctx, rec.Agent, rec.Name(), func(ev *Evidence) bool {
		if fresh || ev.Version != rec.Version() {
			*ev = *newEvidence(rec.Version())
		}
		if replay != nil {
			r := *replay
			if r.At.IsZero() {
				r.At = s.now().UTC()
			}
			ev.Replay = &r
		}
		return true
	})
	return err
}

// updateEvidence applies f to the evidence of a routine and writes the
// result at the revision that it read, when f reports a change. It
// returns the evidence after the change, or nil when nothing changed.
func (s *Store) updateEvidence(ctx context.Context, agent, name string, f func(ev *Evidence) bool) (*Evidence, error) {
	var out *Evidence
	_, err := bucket.Modify(ctx, s.b, evidenceKeyOf(agent, name), writeAttempts, func(value []byte, exists bool) ([]byte, error) {
		out = newEvidence(0)
		if exists {
			ev, err := decodeEvidence(value)
			if err != nil {
				s.log.Warn("routines: the evidence does not decode; it starts again", "agent", agent, "routine", name, "error", err)
			} else {
				out = ev
			}
		}
		if !f(out) {
			out = nil
			return nil, nil
		}
		out.UpdatedAt = s.now().UTC()
		return out.fit(maxValue)
	})
	if errors.Is(err, bucket.ErrConflict) {
		return nil, fmt.Errorf("%w: other writers changed the evidence of %s of %s %d times while this write tried", ErrConflict, name, agent, writeAttempts)
	}
	if err != nil {
		return nil, fmt.Errorf("routines: write the evidence of %s of %s: %w", name, agent, err)
	}
	return out, nil
}

// setReady writes the ready flag of a routine when it differs.
func (s *Store) setReady(ctx context.Context, agent, name string, version int, ready bool) error {
	_, err := bucket.Modify(ctx, s.b, keyOf(agent, name), writeAttempts, func(value []byte, exists bool) ([]byte, error) {
		if !exists {
			return nil, nil
		}
		cur, err := decodeRecord(value)
		if err != nil {
			return nil, err
		}
		want := ready && cur.Standing == Rehearsing && cur.Version() == version
		if cur.Ready == want {
			return nil, nil
		}
		cur.Ready = want
		return encode(cur)
	})
	if err != nil {
		return fmt.Errorf("routines: write the ready flag of the routine %s of %s: %w", name, agent, err)
	}
	return nil
}

// sameBody reports whether two bodies are equal, their standing and their
// version aside, as canonical JSON.
func sameBody(a, b fw.Routine) bool {
	a.Standing, a.Version, b.Standing, b.Version = "", 0, "", 0
	ja, err1 := json.Marshal(a)
	jb, err2 := json.Marshal(b)
	return err1 == nil && err2 == nil && bytes.Equal(ja, jb)
}

func sameSource(a, b *Source) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// checkSource checks the source of a routine: a count of runs of 0 or
// more, days as YYYY-MM-DD, and one line of what proposed it.
func checkSource(src *Source) error {
	if src == nil {
		return nil
	}
	if src.Runs < 0 {
		return fmt.Errorf("%w: source.runs is a count, not %d", ErrInvalid, src.Runs)
	}
	for _, d := range []struct{ key, v string }{{"source.from", src.From}, {"source.to", src.To}} {
		if d.v == "" {
			continue
		}
		if _, err := time.Parse(time.DateOnly, d.v); err != nil {
			return fmt.Errorf("%w: %s %q is not a day such as 2026-10-21", ErrInvalid, d.key, d.v)
		}
	}
	if _, err := checkLine("source.proposedBy", src.ProposedBy, 200); err != nil {
		return err
	}
	return nil
}

// checkNote checks the note of a move: one line of at most MaxNoteBytes.
func checkNote(note string) (string, error) {
	return checkLine("note", note, MaxNoteBytes)
}

// checkLine checks one line of text without control characters, of at
// most n bytes after the spaces at its ends go.
func checkLine(field, v string, n int) (string, error) {
	v = strings.TrimSpace(v)
	if !utf8.ValidString(v) || strings.ContainsFunc(v, unicode.IsControl) {
		return "", fmt.Errorf("%w: %s must be one line of text without control characters", ErrInvalid, field)
	}
	if len(v) > n {
		return "", fmt.Errorf("%w: %s holds at most %d bytes, and this one has %d", ErrInvalid, field, n, len(v))
	}
	return v, nil
}

// joinNote joins the note of the store and the note of the caller.
func joinNote(ours, theirs string) string {
	if theirs == "" {
		return ours
	}
	return ours + ": " + theirs
}

// trimHistory keeps the newest MaxHistory moves.
func trimHistory(h []Move) []Move {
	if len(h) <= MaxHistory {
		return h
	}
	return slices.Clone(h[len(h)-MaxHistory:])
}
