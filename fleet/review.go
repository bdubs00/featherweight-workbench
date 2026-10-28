package fleet

// This file holds what the front needs to know about the results that a
// person checks (docs/ARCHITECTURE.md section 26): the results that a
// person asked to check, and the results of a duty that reach people only
// when its run finds something. It also holds the egress of a pool for
// the plan of a task (section 27).

import (
	"context"
	"slices"
	"strings"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// MetaReview is the metadata key of a run whose owner asked to check its
// result. Its value is "1". A duty with review sets it on each start; a
// plan of the door sets it on a run that the words ask to check.
const MetaReview = "workbench.review"

// MetaFound is the metadata key of a run of a duty whose result reaches
// people only when the run finds something (notify.resultWhen found). Its
// value is "1". A result that found something waits for a check, as a
// result with MetaReview does (Found).
const MetaFound = "workbench.found"

// checks reports whether a completed run waits for the check of a person
// once it ends: its owner asked for it, or it found something.
func checks(r *fw.Run) bool {
	if r.Metadata[MetaReview] == "1" {
		return true
	}
	_, found := Found(r)
	return found
}

// Found reports whether a completed run with MetaFound found something,
// with the words of what it found: the value of the line "Found: ..." of
// its answer, else the first line of the answer. An answer without the
// line found something, so a run that broke the contract never hides a
// result (FoundLine).
func Found(r *fw.Run) (string, bool) {
	if r == nil || r.State != fw.StateCompleted || r.Metadata[MetaFound] != "1" {
		return "", false
	}
	what, something := FoundLine(r.Output)
	if !something {
		return "", false
	}
	if what == "" {
		for l := range strings.SplitSeq(r.Output, "\n") {
			if l = strings.TrimSpace(strings.Trim(strings.TrimSpace(l), "*_`#> ")); l != "" {
				what = l
				break
			}
		}
	}
	return what, true
}

// foundPrefix starts the line of the contract of found, in any case.
const foundPrefix = "found:"

// nothingWords are the values of the line of found that mean that the run
// found nothing.
var nothingWords = map[string]bool{"nothing": true, "none": true, "no": true, "0": true, "nothing new": true, "nothing found": true, "no items": true, "n/a": true}

// FoundLine reads the line of the contract of found from an answer: what
// the run says that it found, and whether it found something. The answer
// of such a run starts with one line "Found: <a few words>", or "Found:
// nothing". FoundLine reads the first line, and the last line when the
// answer is whole: the view of the fleet cuts a long answer at its end
// (cut). An answer without the line found something, with no words.
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

// InReview reports whether a run waits for a person to check its result:
// it completed, it carries MetaReview or found something (Found), and no
// person reacted to it yet. It reads the counts of the run object alone.
// ToCheck reads the facts of the run too, and is right after Needs work.
func InReview(r *fw.Run) bool {
	if r == nil || r.State != fw.StateCompleted || !checks(r) {
		return false
	}
	return r.Reactions[fw.ReactionUp]+r.Reactions[fw.ReactionDown]+r.Reactions[fw.ReactionHeart] == 0
}

// ToCheck reports whether a run waits for a person to check its newest
// result. It completed, it carries MetaReview or found something (Found),
// and no person reacted after its newest end. So the new answer after
// Needs work waits for a check again. Facts without ends, from before the
// archive counted them, fall back to InReview and to the verdict of the
// facts, such as a receipt.
func ToCheck(r *fw.Run, w Work) bool {
	if r == nil || r.State != fw.StateCompleted || !checks(r) {
		return false
	}
	if w.Ends > 0 {
		return w.Reviewed < w.Ends
	}
	return InReview(r) && w.Verdict == ""
}

// Again reports whether the result to check is a new answer after a person
// sent an earlier one back.
func Again(w Work) bool {
	return w.Reviewed > 0 && w.Ends > w.Reviewed && w.LastVerdict == VerdictRejected
}

// SendBackText is the follow-up message of Needs work: the note of the
// person, and the ask for the whole result again. Without that ask, a model
// answers with the change alone, and the first answer is lost to the
// person who checks.
func SendBackText(note string) string {
	return "A person checked your result and sent it back with this note:\n\n" + note +
		"\n\nAnswer again with the whole result, changed as the note says."
}

// PoolEgress returns the egress hosts of the pool name, the hosts that its
// workers reach, and reports false for a pool that the fleet does not
// keep.
func PoolEgress(ctx context.Context, a *fw.Agent, pool string) ([]string, bool) {
	f := fleetOf(a)
	if f == nil || pool == "" {
		return nil, false
	}
	for _, p := range f.poolsNow(ctx) {
		if p.Name != pool {
			continue
		}
		if p.Egress == nil {
			return nil, true
		}
		return slices.Clone(p.Egress.Hosts), true
	}
	return nil, false
}
