package bridges

// This file holds what a person does on a bridge, which the routes of the
// page and the MCP tools of a person share (docs/ARCHITECTURE.md section
// 24.5): a change of the bridge, a change of a seat, a reaction to an
// entry, and the read mark. Each function checks that the caller in ctx
// can use the bridge, as the routes do (allows).

import (
	"context"
	"strings"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// Patch changes a bridge for the caller in ctx, as PATCH
// /v1/bridges/{id} does: its title, its delegation and its caps in part,
// and its state. A close of a bridge that delegates the report asks the
// manager for it. It returns the bridge with its members.
func (s *Store) Patch(ctx context.Context, id string, p BridgePatch) (Item, error) {
	rec, err := s.load(ctx, id)
	if err != nil {
		return Item{}, err
	}
	rec, closed, err := s.patch(ctx, rec.ID, p, fw.PrincipalFrom(ctx))
	if err != nil {
		return Item{}, err
	}
	if closed {
		s.askReport(ctx, rec)
	}
	return s.item(ctx, rec, subjectOf(ctx), true)
}

// VerdictAccepted is the verdict of a bridge whose result a person
// accepted with Looks right (Bridge.Verdict).
const VerdictAccepted = "accepted"

// SendBackPrefix starts the message of Needs work in the channel.
const SendBackPrefix = "Needs work: "

// Check is the check of the result of a bridge by the caller in ctx, a
// person (docs/ARCHITECTURE.md section 26.4). With up, Looks right, the
// bridge closes with the verdict accepted, as a close does: it cancels
// nothing, ends the questions, and asks the manager for the report when
// the bridge delegates it. A bridge that was closed already takes the
// verdict alone. Without up, Needs work, the note goes in the channel to
// every seat, and the bridge stays open, so the seats work again. The
// caller needs the rule of the routes (allows). It returns the bridge with
// its members.
func (s *Store) Check(ctx context.Context, id string, up bool, note string) (Item, error) {
	rec, err := s.load(ctx, id)
	if err != nil {
		return Item{}, err
	}
	if !up {
		if strings.TrimSpace(note) == "" {
			return Item{}, invalid("Needs work needs a note that says what must change")
		}
		if _, err := s.postAs(ctx, rec.ID, PostSpec{Text: SendBackPrefix + strings.TrimSpace(note)}); err != nil {
			return Item{}, err
		}
		if rec, err = s.load(ctx, rec.ID); err != nil {
			return Item{}, err
		}
		return s.item(ctx, rec, subjectOf(ctx), true)
	}
	closed := StateClosed
	rec, report, err := s.patch(ctx, rec.ID, BridgePatch{State: &closed, verdict: VerdictAccepted}, fw.PrincipalFrom(ctx))
	if err != nil {
		return Item{}, err
	}
	if report {
		s.askReport(ctx, rec)
	}
	return s.item(ctx, rec, subjectOf(ctx), true)
}

// PatchMember changes a seat of a bridge for the caller in ctx, as PATCH
// /v1/bridges/{id}/members/{handle} does: muted, a new task, or a
// continue of a seat that stopped at a limit of its budget.
func (s *Store) PatchMember(ctx context.Context, id, handle string, p MemberPatch) (Member, error) {
	rec, err := s.load(ctx, id)
	if err != nil {
		return Member{}, err
	}
	return s.patchMember(ctx, rec.ID, handle, p, fw.PrincipalFrom(ctx))
}

// React records the reaction of the caller in ctx on the entry seq of a
// bridge, as POST /v1/bridges/{id}/messages/{seq}/reactions does, and
// returns the entry.
func (s *Store) React(ctx context.Context, id string, seq uint64, spec ReactSpec) (Message, error) {
	if seq == 0 {
		return Message{}, invalid("the seq of an entry is a positive whole number")
	}
	rec, err := s.load(ctx, id)
	if err != nil {
		return Message{}, err
	}
	return s.reactEntry(ctx, rec.ID, seq, spec, fw.PrincipalFrom(ctx))
}

// MarkRead moves the read mark of the caller in ctx in a bridge to seq,
// as PUT /v1/bridges/{id}/read does, and returns the mark. A seq past the
// newest entry marks the newest entry, and a smaller seq than the mark
// leaves the mark.
func (s *Store) MarkRead(ctx context.Context, id string, seq uint64) (uint64, error) {
	rec, err := s.load(ctx, id)
	if err != nil {
		return 0, err
	}
	seq = min(seq, rec.LastSeq)
	fctx := s.asFront(ctx)
	if err := s.b.setRead(fctx, rec.ID, subjectOf(ctx), seq); err != nil {
		return 0, err
	}
	return s.b.readOf(fctx, rec.ID, subjectOf(ctx))
}
