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
)

// FoundInstructions are the instructions of a run of a duty whose results
// reach people only when it finds something.
const FoundInstructions = "The person hears of the result of this run only when it finds something. Start your answer with one line in this form: \"Found: <what you found, in a few words>\". When you found nothing, make that first line \"Found: nothing\"."

// foundPrefix starts the line of the contract, in any case.
const foundPrefix = "found:"

// nothingWords are the values of the line that mean that the run found
// nothing.
var nothingWords = map[string]bool{"nothing": true, "none": true, "no": true, "0": true, "nothing new": true, "nothing found": true, "no items": true, "n/a": true}

// cutMark ends an answer that the view of the fleet cut (fleet.cut).
const cutMark = "…"

// FoundLine reads the line of the contract from an answer: what the run
// says that it found, and whether it found something. It reads the first
// line, and the last line when the answer is whole. An answer without the
// line found something, with no words.
func FoundLine(output string) (string, bool) {
	var lines []string
	for l := range strings.SplitSeq(output, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			lines = append(lines, l)
		}
	}
	if len(lines) == 0 {
		return "", true
	}
	candidates := []string{lines[0]}
	if len(lines) > 1 && !strings.HasSuffix(strings.TrimSpace(output), cutMark) {
		candidates = append(candidates, lines[len(lines)-1])
	}
	for _, l := range candidates {
		l = strings.Trim(l, "*_`#> ")
		if len(l) < len(foundPrefix) || !strings.EqualFold(l[:len(foundPrefix)], foundPrefix) {
			continue
		}
		v := strings.Trim(strings.TrimSpace(l[len(foundPrefix):]), "*_`. ")
		return v, !nothingWords[strings.ToLower(v)]
	}
	return "", true
}

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
// readback. It is "" for a duty that sends no result.
func resultWords(d *Duty) string {
	n := d.Notify
	if n == nil || len(n.Result) == 0 {
		return ""
	}
	who := whoWords(n.Result)
	switch {
	case n.ResultWhen != ResultFound:
		return "Each result goes to " + who + "."
	case d.CheckFirst != nil:
		return "Each result goes to " + who + ", with what the check found."
	}
	return "A result goes to " + who + " when the run says that it found something."
}

// whoWords names the people of a notify list in words: the owner, a person
// by the part of the subject before an @, and a shared channel by its name.
func whoWords(list []string) string {
	var names []string
	for _, who := range list {
		switch name, ok := ChannelOf(who); {
		case ok:
			names = append(names, "the channel "+name)
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

// reviewWords says that the owner checks each result, for the readback.
const reviewWords = "The owner checks each result before it counts as done."
