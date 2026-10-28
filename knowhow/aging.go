package knowhow

// This file holds the daily passes (docs/KNOWHOW.md section 5.5): the
// notes that no run used for restDays rest, and after putAwayDays they
// are put away. The same day the passes prune the journals, the waiting
// lists, and the candidates, and propose routines. One replica runs them
// each day: it claims the day in the entry mark.daily at the revision
// that it read, and every other replica sees the claim.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/bdubs00/featherweight-workbench/internal/bucket"
)

// mark is the entry of a pass that one replica claims.
type mark struct {
	Day string    `json:"day,omitempty"`
	At  time.Time `json:"at,omitzero"`
}

// claim claims the day of the pass name for this replica, and reports
// false when another replica claimed it already.
func (s *Service) claim(ctx context.Context, name, day string) bool {
	wrote, _, _, err := s.modify(ctx, "mark."+name, func(value []byte, exists bool, _ uint64) ([]byte, error) {
		var m mark
		if exists && json.Unmarshal(value, &m) == nil && m.Day == day {
			return nil, nil
		}
		return encode(mark{Day: day, At: s.now().UTC()})
	})
	if err != nil {
		s.log.Warn("knowhow: a daily pass did not claim its day", "pass", name, "error", err)
		return false
	}
	return wrote
}

// Daily runs the daily passes once per day across the replicas.
func (s *Service) Daily(ctx context.Context) {
	day := s.now().UTC().Format(time.DateOnly)
	if !s.claim(ctx, "daily", day) {
		return
	}
	rested, away, err := s.Age(ctx)
	if err != nil {
		s.log.Warn("knowhow: the pass of resting notes failed", "error", err)
	}
	s.prune(ctx)
	if dropped, err := s.dropCandidates(ctx); err != nil {
		s.log.Warn("knowhow: old candidates were not dropped", "error", err)
	} else if dropped > 0 {
		s.log.Info("knowhow: old candidates dropped", "count", dropped)
	}
	proposed := 0
	if s.cfg.Propose.On() {
		res, err := s.Propose(ctx)
		if err != nil {
			s.log.Warn("knowhow: the proposals of routines failed", "error", err)
		}
		proposed = len(res.Proposed)
	}
	s.log.Info("knowhow: the daily passes ran", "day", day, "resting", rested, "putAway", away, "proposed", proposed)
}

// Age rests the notes that no run used for restDays and puts away those
// that no run used for putAwayDays. A note without a use counts from the
// time it was written. A note that a person wrote never rests, and a note
// that waits for a person stays as it is. Each move writes the journal.
func (s *Service) Age(ctx context.Context) (rested, putAway int, err error) {
	notes, err := s.noteList(ctx, "")
	if err != nil {
		return 0, 0, err
	}
	now := s.now().UTC()
	rest := time.Duration(s.cfg.Aging.RestDays) * 24 * time.Hour
	away := time.Duration(s.cfg.Aging.PutAwayDays) * 24 * time.Hour
	for key, n := range notes {
		if n.ByPerson() || n.State == StatePutAway || n.State == StateWaiting {
			continue
		}
		last := n.At
		if n.LastUsed != nil && n.LastUsed.After(last) {
			last = *n.LastUsed
		}
		idle := now.Sub(last)
		switch {
		case idle >= away:
			why := fmt.Sprintf("No run used this note for %d days.", s.cfg.Aging.PutAwayDays)
			ok, err := s.setState(ctx, key, WhatPutAway, why, nil, func(x *Note) bool {
				if x.State == StatePutAway || x.State == StateWaiting || x.At.After(last) {
					return false
				}
				x.State = StatePutAway
				return true
			})
			if err != nil {
				return rested, putAway, err
			}
			if ok {
				putAway++
			}
		case idle >= rest && n.State != StateResting:
			why := fmt.Sprintf("No run used this note for %d days.", s.cfg.Aging.RestDays)
			ok, err := s.setState(ctx, key, WhatResting, why, nil, func(x *Note) bool {
				if x.State == StateResting || x.State == StatePutAway || x.State == StateWaiting || x.At.After(last) {
					return false
				}
				x.State = StateResting
				return true
			})
			if err != nil {
				return rested, putAway, err
			}
			if ok {
				rested++
			}
		}
	}
	return rested, putAway, nil
}

// agents returns the agents that have entries under the first token.
func (s *Service) agents(ctx context.Context, first string) []string {
	keys, err := bucket.EntriesUnder(ctx, s.b, first)
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, k := range keys {
		parts := strings.SplitN(k.Key, ".", 3)
		if len(parts) < 2 || seen[parts[1]] {
			continue
		}
		seen[parts[1]] = true
		if raw, err := decodeToken(parts[1]); err == nil && CheckAgent(raw) == nil {
			out = append(out, raw)
		}
	}
	return out
}

// prune keeps the journals and the waiting lists of every agent within
// their bounds.
func (s *Service) prune(ctx context.Context) {
	for _, agent := range s.agents(ctx, "journal") {
		if err := s.pruneJournal(ctx, agent); err != nil {
			s.log.Warn("knowhow: the journal of an agent was not pruned", "agent", agent, "error", err)
		}
	}
	for _, agent := range s.agents(ctx, "waiting") {
		s.pruneWaiting(ctx, agent)
	}
}
