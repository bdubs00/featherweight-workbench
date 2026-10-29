package work

// This file is the check of a result by a person: Looks right and Needs
// work on a card in review, in Activity, or from an MCP client. For a run,
// Looks right is the reaction up on the run. Needs work is the reaction
// down with a note, and the note goes to the agent as a follow-up message,
// so the run works again. Both go through the routes of the kernel on the
// member that holds the run, with the access rules of those routes. The
// thumb goes on the fw.run.finished of the run, so the kernel also records
// a quality receipt. For a bridge, a team, Looks right closes the bridge
// with the verdict accepted, and Needs work posts the note in its channel
// to every seat and keeps it open (bridges.Store.Check).

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"unicode/utf8"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/bridges"
	"github.com/bdubs00/featherweight-workbench/fleet"
)

// Verdicts of a check of a result by a person. up and down are the words
// of the reactions, and right and work the words of the first build; each
// pair means the same.
const (
	ReviewRight = "right" // Looks right
	ReviewWork  = "work"  // Needs work
	ReviewUp    = "up"    // Looks right
	ReviewDown  = "down"  // Needs work
)

// MaxNote bounds the note of Needs work, in bytes.
const MaxNote = 4000

// ReviewRequest is the check of the result of a run or of a bridge: one
// of run and bridge, verdict up (or right) or down (or work), and for down
// the note that says what must change.
type ReviewRequest struct {
	Run     string `json:"run,omitempty"`
	Bridge  string `json:"bridge,omitempty"`
	Verdict string `json:"verdict"`
	Note    string `json:"note,omitempty"`
}

// ReviewAnswer says what the check did: the reaction on the run, and
// whether the note went to the agent as a follow-up. For a bridge, Bridge
// names it, Closed says that Looks right closed it, and FollowUp that the
// note went in its channel. Kept is true for a run whose worker the pool
// replaced: the front keeps the verdict, and the note of Needs work stays
// with the result, because no agent holds the run any more.
type ReviewAnswer struct {
	Run      string `json:"run,omitempty"`
	Bridge   string `json:"bridge,omitempty"`
	Reaction string `json:"reaction"`
	FollowUp bool   `json:"followUp"`
	Closed   bool   `json:"closed,omitempty"`
	Kept     bool   `json:"kept,omitempty"`
}

// ErrNotPerson refuses a check that a run of an agent asks for: only a
// person checks a result.
var ErrNotPerson = errors.New("work: only a person checks a result")

// Review checks the result of a run for the caller in ctx. The caller
// needs the operator role on the run, as for a reaction and a message. A
// verdict on a run that only the archive holds lands in the archive
// (fleet.ReactArchived): it is a verdict that the front keeps, and no
// follow-up goes out, because no agent holds the run. A run that no
// process holds is fw.ErrRunNotFound.
func Review(ctx context.Context, a *fw.Agent, req ReviewRequest) (ReviewAnswer, error) {
	id := strings.TrimSpace(req.Run)
	bridge := strings.TrimSpace(req.Bridge)
	note := strings.TrimSpace(req.Note)
	up := req.Verdict == ReviewRight || req.Verdict == ReviewUp
	switch {
	case id == "" && bridge == "":
		return ReviewAnswer{}, fmt.Errorf("%w: name the run or the bridge to check", fw.ErrInvalidRequest)
	case id != "" && bridge != "":
		return ReviewAnswer{}, fmt.Errorf("%w: name the run or the bridge to check, not both", fw.ErrInvalidRequest)
	case !up && req.Verdict != ReviewWork && req.Verdict != ReviewDown:
		return ReviewAnswer{}, fmt.Errorf("%w: verdict is %q, not %s or %s", fw.ErrInvalidRequest, req.Verdict, ReviewUp, ReviewDown)
	case !up && note == "":
		return ReviewAnswer{}, fmt.Errorf("%w: Needs work needs a note that says what must change", fw.ErrInvalidRequest)
	case len(note) > MaxNote || !utf8.ValidString(note):
		return ReviewAnswer{}, fmt.Errorf("%w: the note is at most %d bytes of text", fw.ErrInvalidRequest, MaxNote)
	}
	if bridge != "" {
		return reviewBridge(ctx, a, bridge, up, note)
	}
	ans := ReviewAnswer{Run: id, Reaction: fw.ReactionUp}
	if !up {
		ans.Reaction = fw.ReactionDown
	}
	// The thumb goes on the end of the run, so the kernel also records a
	// quality receipt (fleet.AtEnd).
	reaction := fw.ReactionData{Reaction: ans.Reaction, Note: note, Seq: fleet.FinishedSeq(ctx, a, "", id)}
	err := a.React(ctx, id, reaction)
	if err == nil {
		if !up {
			if err := a.Send(ctx, id, fw.Message{Role: fw.RoleUser, Content: []fw.Block{fw.Text(fleet.SendBackText(note))}}, fw.OriginHTTP); err != nil {
				return ans, err
			}
			ans.FollowUp = true
		}
		return ans, nil
	}
	if !errors.Is(err, fw.ErrRunNotFound) {
		return ReviewAnswer{}, err
	}
	v := fleet.Of(a)
	if v == nil {
		return ReviewAnswer{}, err
	}
	if fleet.IsArchived(ctx, a, id) {
		if _, err := fleet.ReactArchived(ctx, a, id, fw.ReactionData{Reaction: ans.Reaction, Note: note}); err != nil {
			return ReviewAnswer{}, err
		}
		ans.Kept = true
		return ans, nil
	}
	mctx, err := fleet.ForRun(ctx, a, id, fw.ActionRunOperate)
	if err != nil {
		return ReviewAnswer{}, err
	}
	run, err := v.Run(ctx, id)
	if err != nil {
		return ReviewAnswer{}, err
	}
	path := "/v1/runs/" + url.PathEscape(id)
	body := map[string]any{"reaction": ans.Reaction, "note": note}
	if seq := fleet.FinishedSeq(mctx, a, run.Member, id); seq > 0 {
		body["seq"] = seq
	}
	if err := fleet.PostJSON(mctx, v, run.Member, path+"/reactions", body, nil); err != nil {
		return ReviewAnswer{}, err
	}
	if !up {
		if err := fleet.PostJSON(mctx, v, run.Member, path+"/messages", map[string]string{"content": fleet.SendBackText(note)}, nil); err != nil {
			return ans, err
		}
		ans.FollowUp = true
	}
	return ans, nil
}

// ErrNoBridges answers a check of a bridge in a process that keeps no
// bridges.
var ErrNoBridges = errors.New("work: the bridges are off in this process")

// reviewBridge checks the result of a bridge for the caller in ctx with
// the rule of the routes of the bridges.
func reviewBridge(ctx context.Context, a *fw.Agent, id string, up bool, note string) (ReviewAnswer, error) {
	st := bridges.Of(a)
	if st == nil {
		return ReviewAnswer{}, ErrNoBridges
	}
	it, err := st.Check(ctx, id, up, note)
	if err != nil {
		return ReviewAnswer{}, err
	}
	ans := ReviewAnswer{Bridge: it.ID, Reaction: fw.ReactionUp, Closed: up && it.State == bridges.StateClosed}
	if !up {
		ans.Reaction, ans.FollowUp = fw.ReactionDown, true
	}
	return ans, nil
}
