package bridges

// This file tracks the questions that a seat asks the people of its bridge
// (docs/ARCHITECTURE.md section 18.5). A seat asks with bridge_post and
// ask true, with up to four choices, and as a safety net for a model that
// does not follow its instructions, any message of a seat whose to names
// operator, or whose text holds the word @operator anywhere, asks too. The
// entry carries ask, so the page draws a question card, and the record
// marks the question on the seat. While it waits, the seat needs you: its
// presence is needs-you, so the bridge counts it in needsYou, and the
// member carries the seq of the question in ask. A person answers with a
// message that reaches the seat: a message to its handle, a message to
// everyone, or a reply to the question, such as a key of a choice, which
// the page sends as a reply. The answer reaches the seat as a message of a
// person on the usual path and starts its next turn. The question ends
// too when the seat posts again, other than the final answer of the turn
// that asked, which the instructions make a one-line status.

import (
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

// Ask marks an entry of a seat that asks the people of the bridge. Choices
// holds the answers that a person picks from, two to four short strings,
// or none for a question that a person answers in words.
type Ask struct {
	Choices []string `json:"choices,omitempty"`
}

// Bounds of the choices of a question.
const (
	minChoices     = 2
	maxChoices     = 4
	maxChoiceRunes = 60
)

// operatorWord finds the word @operator anywhere in a text, in any case,
// and not inside a longer handle or an address such as a@operator.org.
var operatorWord = regexp.MustCompile(`(?i)(^|[^a-z0-9._@-])@operator([^a-z0-9_-]|$)`)

// asksOperator reports whether a message of a seat asks the people of the
// bridge: its to names operator, or its text holds the word @operator.
func asksOperator(to []string, text string) bool {
	for _, h := range to {
		if h == HandleOperator {
			return true
		}
	}
	return operatorWord.MatchString(text)
}

// checkChoices checks the choices of a question: none, or two to four
// short strings that differ, each trimmed.
func checkChoices(choices []string) ([]string, error) {
	if len(choices) == 0 {
		return nil, nil
	}
	if len(choices) < minChoices || len(choices) > maxChoices {
		return nil, invalid("a question has %d to %d choices", minChoices, maxChoices)
	}
	out := make([]string, 0, len(choices))
	seen := map[string]bool{}
	for _, c := range choices {
		c = strings.TrimSpace(c)
		switch {
		case c == "":
			return nil, invalid("a choice is empty")
		case utf8.RuneCountInString(c) > maxChoiceRunes || strings.ContainsAny(c, "\r\n"):
			return nil, invalid("a choice is one line of at most %d characters", maxChoiceRunes)
		case seen[strings.ToLower(c)]:
			return nil, invalid("the choice %q comes twice", c)
		}
		seen[strings.ToLower(c)] = true
		out = append(out, c)
	}
	return out, nil
}

// askOf returns the seq of the question of a seat that waits, or 0.
func (r *record) askOf(handle string) uint64 {
	if sm := r.Seats[handle]; sm != nil {
		return sm.Ask
	}
	return 0
}

// seatPosts marks a message of the seat handle with the seq. A question
// waits from now on; open is true for a question that the seat posted
// during a turn, whose final answer then ends that turn and leaves the
// question waiting. Any other message ends the question that waits. It
// reports whether the mark changed. It runs inside the change of the
// record.
func (r *record) seatPosts(handle string, seq uint64, question, open bool) bool {
	if r.member(handle) == nil {
		return false
	}
	sm := r.seat(handle)
	if question {
		sm.Ask, sm.AskOpen = seq, open
		return true
	}
	if sm.Ask == 0 {
		return false
	}
	sm.Ask, sm.AskOpen = 0, false
	return true
}

// seatEnds marks the final answer of the seat handle with the seq. The end
// of the turn that asked leaves its question waiting, whatever the answer
// says. Else the answer asks when it addresses operator, and any other
// answer ends the question that waits. It reports whether the answer is a
// question and whether the mark changed. It runs inside the change of the
// record.
func (r *record) seatEnds(handle string, seq uint64, question bool) (asks, changed bool) {
	if r.member(handle) == nil {
		return false, false
	}
	if sm := r.Seats[handle]; sm != nil && sm.Ask > 0 && sm.AskOpen {
		sm.AskOpen = false
		return false, true
	}
	return question, r.seatPosts(handle, seq, question, false)
}

// closeQuestions ends the question of each seat that waits, because the
// bridge closes and nobody answers in a closed bridge. It returns the
// handles whose question ended, sorted. It runs inside the change of the
// record.
func (r *record) closeQuestions() []string {
	var out []string
	for h, sm := range r.Seats {
		if sm == nil || sm.Ask == 0 {
			continue
		}
		sm.Ask, sm.AskOpen = 0, false
		if m := r.member(h); m != nil && !m.Fired {
			out = append(out, h)
		}
	}
	sort.Strings(out)
	return out
}

// personAnswers ends the questions that a message of a person answers: the
// question of each seat that the message reaches, by its to, or by an
// empty to, which reaches every member, and the question that it replies
// to. It returns the handles whose question ended. It runs inside the
// change of the record.
func (r *record) personAnswers(to []string, replyTo *uint64) []string {
	var out []string
	for h, sm := range r.Seats {
		if sm == nil || sm.Ask == 0 {
			continue
		}
		reaches := len(to) == 0 || (replyTo != nil && *replyTo == sm.Ask)
		for _, x := range to {
			reaches = reaches || x == h
		}
		if reaches {
			sm.Ask, sm.AskOpen = 0, false
			out = append(out, h)
		}
	}
	return out
}
