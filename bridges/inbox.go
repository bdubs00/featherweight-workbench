package bridges

// This file lists what waits for a person in the open bridges, for the
// inbox of the front (package inbox): each seat whose presence is
// needs-you, as the count needsYou of the routes counts it, with the
// question of the seat when it asked one (ask.go).

import (
	"context"
	"time"
)

// Waiting is a seat of an open bridge that needs a person: the bridge and
// its title, the seat, its agent, and its newest run. Question is the seq
// of the question of the seat, or 0 when its run waits for an approval
// instead, with the id of the approval in Approval. Text is the text of the question, and At when it came, or when
// the run began to wait.
type Waiting struct {
	Bridge   string    `json:"bridge"`
	Title    string    `json:"title"`
	Handle   string    `json:"handle"`
	Agent    string    `json:"agent"`
	Run      string    `json:"run,omitempty"`
	Approval string    `json:"approval,omitempty"`
	Question uint64    `json:"question,omitempty"`
	Text     string    `json:"text,omitempty"`
	At       time.Time `json:"at"`
}

// Waiting lists the seats of the open bridges that the caller in ctx can
// list whose presence is needs-you, bridge by bridge.
func (s *Store) Waiting(ctx context.Context) ([]Waiting, error) {
	fctx := s.asFront(ctx)
	recs, err := s.b.list(fctx)
	if err != nil {
		return nil, err
	}
	admin := s.isAdmin(ctx)
	out := []Waiting{}
	for _, rec := range recs {
		if rec.State == StateClosed {
			continue
		}
		ok, err := s.listed(ctx, rec, admin)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		runs, err := s.runsOf(fctx, rec.ID)
		if err != nil {
			return nil, err
		}
		for _, m := range decorate(rec, runs, s.taskStates(fctx)) {
			if m.Presence != PresenceNeedsYou {
				continue
			}
			w := Waiting{Bridge: rec.ID, Title: rec.Title, Handle: m.Handle, Agent: m.Agent, Run: m.newest(), Question: m.Question}
			if m.Question > 0 {
				if msg, _, err := s.b.entry(fctx, rec.ID, m.Question); err == nil {
					w.Text, w.At = msg.Text, msg.At
				}
			} else if r := runs[w.Run]; r != nil {
				w.At = r.UpdatedAt
				if r.Pending != nil {
					w.Approval = r.Pending.ID
					if !r.Pending.RequestedAt.IsZero() {
						w.At = r.Pending.RequestedAt
					}
				}
			}
			out = append(out, w)
		}
	}
	return out, nil
}
