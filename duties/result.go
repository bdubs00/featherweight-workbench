package duties

// This file sends the result of a start to the people of notify.result,
// and holds the contract of "found" (docs/ARCHITECTURE.md section 22.12).
//
// With notify.resultWhen always, each completed start sends its result.
// With found, only a start that found something sends it:
//
//   - A duty with a check first starts a run only when the check found
//     items, so each completed start of such a duty found something, and
//     the notice names how many items.
//   - A duty without a check first tells its agent the contract in the
//     instructions of each run (FoundInstructions): the answer starts with
//     one line "Found: <a few words>", or "Found: nothing". The front reads
//     the first line of the answer, and also its last line when the view
//     holds the whole answer (FoundLine). The first line comes first
//     because the view of the fleet cuts a long answer at its end.
//   - An answer without such a line sends its result, so a run that broke
//     the contract never hides a result.

import (
	"context"
	"strings"
	"unicode/utf8"

	"github.com/bdubs00/featherweight-workbench/fleet"
)

// FoundInstructions are the instructions of a run of a duty whose results
// reach people only when it finds something.
const FoundInstructions = "The person hears of the result of this run only when it finds something. Start your answer with one line in this form: \"Found: <what you found, in a few words>\". When you found nothing, make that first line \"Found: nothing\"."

// cutMark ends an answer that the view of the fleet cut (fleet.cut).
const cutMark = "…"

// FoundLine reads the line of the contract from an answer: what the run
// says that it found, and whether it found something (fleet.FoundLine).
func FoundLine(output string) (string, bool) { return fleet.FoundLine(output) }

// resultNotice tells the people of notify.result of a completed start, as
// notify.resultWhen says: e is the start, and output the answer of its
// run, or the note of an action without a run.
func (s *Service) resultNotice(ctx context.Context, st *stored, e entry, output string) {
	d := &st.Duty
	if d.Notify == nil || len(d.Notify.Result) == 0 {
		return
	}
	reason, send := resultReason(d, e, output)
	if !send {
		return
	}
	s.notify(ctx, st, Notice{Kind: NoticeResult, Run: e.Run, Slot: e.Slot, Reason: reason})
}

// resultReason words the result of a completed start, and reports whether
// it goes to the people of notify.result.
func resultReason(d *Duty, e entry, output string) (string, bool) {
	head := firstWords(output)
	if d.CheckFirst != nil && e.Woke {
		found := "The check found " + itemsWords(e.Found) + "."
		if head != "" {
			found += " " + head
		}
		return found, true
	}
	if d.Notify.ResultWhen == ResultFound {
		what, something := FoundLine(output)
		if !something {
			return "", false
		}
		if what != "" {
			return "Found: " + what, true
		}
	}
	if head == "" {
		head = "The run completed."
	}
	return head, true
}

// maxResultBytes bounds the words of a result in a notice.
const maxResultBytes = 300

// firstWords is the start of an answer in one line, cut to maxResultBytes
// at a rune boundary.
func firstWords(output string) string {
	s := strings.Join(strings.Fields(output), " ")
	if len(s) <= maxResultBytes {
		return s
	}
	i := maxResultBytes - len(cutMark)
	for i > 0 && !utf8.RuneStart(s[i]) {
		i--
	}
	return s[:i] + cutMark
}

// resultWords says who hears of the results of a duty, and when, for the
// readback of the person me. It is "" for a duty that sends no result.
func resultWords(d *Duty, me string) string {
	n := d.Notify
	if n == nil || len(n.Result) == 0 {
		return ""
	}
	who := whoWords(n.Result, d.Owner, me)
	switch {
	case n.ResultWhen != ResultFound:
		return "Each result goes to " + who + "."
	case d.CheckFirst != nil:
		return "Each result goes to " + who + ", with what the check found."
	}
	return "A result goes to " + who + " when the run says that it found something."
}

// whoWords names the people of a notify list in words for the person me:
// you for me, also as the owner, the owner, a person by the part of the
// subject before an @, and a shared channel by its name. A duty without an
// owner yet is the duty of me, who saves it.
func whoWords(list []string, owner, me string) string {
	var names []string
	for _, who := range list {
		switch name, ok := ChannelOf(who); {
		case ok:
			names = append(names, "the channel "+name)
		case me != "" && (who == me || who == RunAsOwner && (owner == me || owner == "")):
			names = append(names, "you")
		case who == RunAsOwner:
			names = append(names, "the owner")
		default:
			if at := strings.IndexByte(who, '@'); at > 0 {
				who = who[:at]
			}
			names = append(names, who)
		}
	}
	switch len(names) {
	case 0:
		return "nobody"
	case 1:
		return names[0]
	}
	return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
}

// reviewWords says that the owner checks each result, for the readback,
// and youReview says it to the owner.
const (
	reviewWords = "The owner checks each result before it counts as done."
	youReview   = "You check each result before it counts as done."
)
