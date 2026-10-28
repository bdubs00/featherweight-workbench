package knowhow

// This file lists the notes that wait for the caller under ask first, for
// the one count of the work that needs a person (package work, plan A7 and
// A17). With ask first on, a note that an agent wrote waits for a person
// to keep it. The board, Activity, the badge of the page, and Results
// count such a note beside the tasks that need the person, so a person who
// never opens Know-how still sees it.

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/bdubs00/featherweight-workbench/internal/bucket"
)

// WaitingFor lists the changes of notes on the waiting lists that the
// caller in ctx can decide, the oldest first: the caller reads the note
// and can change its scope, as POST /v1/knowhow/waiting/{id} decides. A
// change of a routine waits in the place Know-how alone. A nil service
// lists none.
func (s *Service) WaitingFor(ctx context.Context) ([]Waiting, error) {
	if s == nil {
		return nil, nil
	}
	keys, err := bucket.EntriesUnder(ctx, s.b, "waiting")
	if err != nil {
		return nil, fmt.Errorf("knowhow: list the waiting changes: %w", err)
	}
	of := map[string]*rights{}
	out := []Waiting{}
	for _, k := range keys {
		got, err := s.b.Get(ctx, k.Key)
		if err != nil {
			continue
		}
		var x Waiting
		if json.Unmarshal(got.Value, &x) != nil || x.Subject.Note == "" {
			continue
		}
		rg, seen := of[x.Agent]
		if !seen {
			if g, err := s.rightsOn(ctx, x.Agent); err == nil {
				rg = &g
			}
			of[x.Agent] = rg
		}
		if rg != nil && rg.seeSubject(x.Subject) && rg.changeSubject(x.Subject) == nil {
			out = append(out, x)
		}
	}
	slices.SortStableFunc(out, func(a, b Waiting) int { return a.At.Compare(b.At) })
	return out, nil
}
