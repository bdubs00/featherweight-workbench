package bridges

// This file gives a card the whole call that its run waits for
// (docs/ARCHITECTURE.md section 18.6). The view of the fleet learns a
// pending call from the egress stream, whose worker cuts each string of an
// event to bus.egressMaxBytes, and the records of the view leave out an
// input above fleet.PendingInputLimit. A card built from the view then
// shows a playbook cut, or no input at all, and a person allows a call
// without seeing it. So when the view holds the input cut or not at all,
// the watch reads the run from its owner, as GET /v1/runs/{id} of the
// front does, and the card takes the whole call. A read that fails leaves
// the card with the input of the view and marks it thin; the watch reads
// again every wholeEvery and writes the card again in place once a read
// works.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"time"
	"unicode/utf8"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/fleet"
)

// cardInputBytes bounds the input of the call on a card. A larger input
// keeps a shorter start of each of its strings, with the note of a cut,
// so the card fits an entry of the channel and shows every field.
const cardInputBytes = 32 << 10

// wholeTimeout bounds one read of a run from its owner for its card. A
// Task that sleeps wakes for the read.
const wholeTimeout = 20 * time.Second

// cutNote is the note that the egress of a worker adds to a string that
// it cut, such as " [cut 4120 of 8216 bytes]" (the kernel, egresscut.go).
var cutNote = regexp.MustCompile(`\[cut [0-9]+ of [0-9]+ bytes\]`)

// thinInput reports whether the input of a pending call as the view holds
// it lacks part of the call: no input, or a string that the egress cut.
func thinInput(in json.RawMessage) bool {
	t := bytes.TrimSpace(in)
	return len(t) == 0 || string(t) == "null" || cutNote.Match(t)
}

// thin reports whether a run of another member waits for a call whose
// input the view holds cut or not at all. A run of this process holds its
// whole call.
func thin(r *fw.Run) bool {
	return r.Member != "" && r.Pending != nil && r.Pending.Connect == nil && thinInput(r.Pending.Input)
}

// needsWhole reports whether the watch reads the whole call of a run now:
// the run waits for a call whose input the view holds thin, and the
// channel holds no card of it yet, or holds a card marked thin. A card
// that is new reads at once; a thin card reads again once per wholeEvery.
func (s *Store) needsWhole(r *fw.Run, mk *runMark) bool {
	if r.State != fw.StateInputRequired || !thin(r) {
		return false
	}
	switch {
	case mk == nil || mk.Pending == "":
		return true
	case mk.Pending == r.Pending.ID && mk.Thin:
		return s.wholeDue(r.ID)
	}
	return false
}

// wholeDue reports whether a thin card of a run reads its call again now,
// at most once per wholeEvery.
func (s *Store) wholeDue(run string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	if at, ok := s.wholeAt[run]; ok && now.Sub(at) < wholeEvery {
		return false
	}
	s.wholeAt[run] = now
	return true
}

// readWhole reads the call that a run of another member waits for from
// the owner of the run, through the forward of the fleet with the static
// token, as GET /v1/runs/{id} of the front reads it. It returns nil when
// the read fails or the run waits for another call now.
func (s *Store) readWhole(ctx context.Context, r *fw.Run) *fw.Approval {
	s.mu.Lock()
	s.wholeAt[r.ID] = s.now()
	s.mu.Unlock()
	v := fleet.Of(s.a)
	if v == nil {
		return nil
	}
	tctx, err := s.asToken(ctx)
	if err != nil {
		s.log.Debug("bridges: read the whole call of a card", "run", r.ID, "error", err)
		return nil
	}
	tctx, cancel := context.WithTimeout(tctx, wholeTimeout)
	defer cancel()
	got, err := fleet.MemberRun(tctx, v, r.Member, r.ID)
	if err != nil {
		s.log.Debug("bridges: read the whole call of a card", "run", r.ID, "member", r.Member, "error", err)
		return nil
	}
	if got.Pending == nil || got.Pending.ID != r.Pending.ID {
		return nil
	}
	p := *got.Pending
	return &p
}

// withWhole returns the run with the whole call that it waits for, or the
// run itself without one.
func withWhole(r *fw.Run, whole *fw.Approval) *fw.Run {
	if whole == nil || r.Pending == nil || whole.ID != r.Pending.ID {
		return r
	}
	c := *r
	c.Pending = whole
	return &c
}

// repairCard writes a card that waits again with the whole call of c,
// under its seq, and gives the hook the changed entry. A card that no
// longer waits, or that waits for another call, stays as it is.
func (s *Store) repairCard(ctx context.Context, id string, c Message) {
	if c.Decision == nil {
		return
	}
	got, changed, err := s.b.changeEntry(s.asFront(ctx), id, c.Seq, func(m *Message) bool {
		if m.Decision == nil || m.Decision.State != DecisionPending || m.Decision.ID != c.Decision.ID {
			return false
		}
		d := *m.Decision
		d.Decision, d.Always = c.Decision.Decision, c.Decision.Always
		m.Decision = &d
		return true
	})
	if err != nil {
		s.log.Warn("bridges: write a card again with its whole call", "bridge", id, "seq", c.Seq, "error", err)
		return
	}
	if changed {
		emitMessage(id, got)
	}
}

// fitInput returns the input of a call that fits max bytes: the input as
// it is when it fits, else the input with each string cut to its start,
// with the note of a cut, at the largest bound that fits. An input that
// does not fit even so, or that does not decode, returns nil.
func fitInput(in json.RawMessage, max int) json.RawMessage {
	if len(in) <= max {
		return in
	}
	var v any
	dec := json.NewDecoder(bytes.NewReader(in))
	dec.UseNumber()
	if dec.Decode(&v) != nil {
		return nil
	}
	for limit := max / 2; limit >= 64; limit /= 2 {
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(false)
		if enc.Encode(cutStrings(v, limit)) != nil {
			return nil
		}
		if out := bytes.TrimRight(buf.Bytes(), "\n"); len(out) <= max {
			return json.RawMessage(out)
		}
	}
	return nil
}

// cutStrings returns a copy of a decoded JSON value with each string cut
// to at most limit bytes at a rune boundary, with the note of the egress.
func cutStrings(v any, limit int) any {
	switch x := v.(type) {
	case string:
		if len(x) <= limit {
			return x
		}
		i := limit
		for i > 0 && !utf8.RuneStart(x[i]) {
			i--
		}
		return x[:i] + fmt.Sprintf(" [cut %d of %d bytes]", len(x)-i, len(x))
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = cutStrings(e, limit)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = cutStrings(e, limit)
		}
		return out
	}
	return v
}
