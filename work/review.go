package work

// This file is the check of a result by a person: Looks right and Needs
// work on a card in review, in Activity, or from an MCP client. Looks right
// is the reaction up on the run. Needs work is the reaction down with a
// note, and the note goes to the agent as a follow-up message, so the run
// works again. Both go through the routes of the kernel on the member that
// holds the run, with the access rules of those routes.

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"unicode/utf8"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/fleet"
)

// Verdicts of a check of a result by a person.
const (
	ReviewRight = "right" // Looks right
	ReviewWork  = "work"  // Needs work
)

// MaxNote bounds the note of Needs work, in bytes.
const MaxNote = 4000

// ReviewRequest is the check of the result of a run: verdict right or
// work, and for work the note that says what must change.
type ReviewRequest struct {
	Run     string `json:"run"`
	Verdict string `json:"verdict"`
	Note    string `json:"note,omitempty"`
}

// ReviewAnswer says what the check did: the reaction on the run, and
// whether the note went to the agent as a follow-up.
type ReviewAnswer struct {
	Run      string `json:"run"`
	Reaction string `json:"reaction"`
	FollowUp bool   `json:"followUp"`
}

// ErrNotPerson refuses a check that a run of an agent asks for: only a
// person checks a result.
var ErrNotPerson = errors.New("work: only a person checks a result")

// Review checks the result of a run for the caller in ctx. The caller
// needs the operator role on the run, as for a reaction and a message. A
// run that only the archive holds is read-only, and a run that no process
// holds is fw.ErrRunNotFound.
func Review(ctx context.Context, a *fw.Agent, req ReviewRequest) (ReviewAnswer, error) {
	id := strings.TrimSpace(req.Run)
	note := strings.TrimSpace(req.Note)
	switch {
	case id == "":
		return ReviewAnswer{}, fmt.Errorf("%w: run names no run", fw.ErrInvalidRequest)
	case req.Verdict != ReviewRight && req.Verdict != ReviewWork:
		return ReviewAnswer{}, fmt.Errorf("%w: verdict is %q, not %s or %s", fw.ErrInvalidRequest, req.Verdict, ReviewRight, ReviewWork)
	case req.Verdict == ReviewWork && note == "":
		return ReviewAnswer{}, fmt.Errorf("%w: Needs work needs a note that says what must change", fw.ErrInvalidRequest)
	case len(note) > MaxNote || !utf8.ValidString(note):
		return ReviewAnswer{}, fmt.Errorf("%w: the note is at most %d bytes of text", fw.ErrInvalidRequest, MaxNote)
	}
	ans := ReviewAnswer{Run: id, Reaction: fw.ReactionUp}
	if req.Verdict == ReviewWork {
		ans.Reaction = fw.ReactionDown
	}
	reaction := fw.ReactionData{Reaction: ans.Reaction, Note: note}
	err := a.React(ctx, id, reaction)
	if err == nil {
		if req.Verdict == ReviewWork {
			if err := a.Send(ctx, id, fw.Message{Role: fw.RoleUser, Content: []fw.Block{fw.Text(note)}}, fw.OriginHTTP); err != nil {
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
		return ReviewAnswer{}, fmt.Errorf("%w: %s", fw.ErrInvalidRequest, fleet.ReadOnlyRun)
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
	if err := fleet.PostJSON(mctx, v, run.Member, path+"/reactions", map[string]string{"reaction": ans.Reaction, "note": note}, nil); err != nil {
		return ReviewAnswer{}, err
	}
	if req.Verdict == ReviewWork {
		if err := fleet.PostJSON(mctx, v, run.Member, path+"/messages", map[string]string{"content": note}, nil); err != nil {
			return ans, err
		}
		ans.FollowUp = true
	}
	return ans, nil
}
