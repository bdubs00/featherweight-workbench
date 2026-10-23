package routines

import (
	"strings"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// The runbook names the routine as the page does, says each value that
// the routine fills in as words, never as the text of the routine, ends
// each sentence with one period, counts tickets, and states the standing.
func TestRunbook(t *testing.T) {
	r := ack()
	r.Name = "acknowledge-a-new-ticket"
	r.Version = 3
	r.Questions["priority"] = fw.RoutineQuestion{Question: fw.Question{Type: fw.QuestionScore, Instructions: "How urgent is it.", Criteria: []any{"p1", "p2", "p3"}}, Act: 0.8}
	r.After = []fw.RoutineCheck{{Tool: "gitea.issue_read", Input: `{"method": "get", "issue_number": {{ find .input "#(\\d+)" }}}`,
		Contains: []string{"status/triaged", "type/{{ .type }}", "priority/{{ .priority }}"}, Lacks: []string{"status/new", "{{ .labels }}"}}}
	at := time.Date(2026, 10, 23, 9, 12, 0, 0, time.UTC)
	rec := Record{Agent: "triage", Routine: r, Standing: Live, SignedOff: &SignOff{By: "ada@example.org", At: at},
		Source:   &Source{Runs: 1212, From: "2026-09-14", To: "2026-10-21", ProposedBy: "fw routine propose"},
		Evidence: &Evidence{Totals: Counts{Acted: 1432, Agreed: 1397}, Replay: &Replay{Matched: 212, Total: 212}}}
	text := Runbook(rec)
	for _, want := range []string{
		"title: Acknowledge a new ticket\n", "# Acknowledge a new ticket\n",
		"A routine of triage, version 3. Signed off by ada@example.org on 23 October 2026. It is live: it acts on its own, and the agent still handles 5% of the tickets, to compare.",
		"- **Priority**: How urgent is it. Answers: p1, p2, p3. It acts when it is at least 80% sure.",
		"`type/…`, with the type it decided", "`priority/…`, with the priority it decided", "The result holds no `status/new` and `…`, with the labels it read.",
		"Made from 1,212 tickets that triage handled between 14 September 2026 and 21 October 2026, proposed by fw routine propose.",
		"would have acted on 1,432 tickets, with 97.6% agreed", "212 of 212 past tickets, handled the same way.",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the runbook lacks %q:\n%s", want, text)
		}
	}
	for _, never := range []string{"{{", "}}", "..", " runs", "shadow", "decider", "template"} {
		if strings.Contains(text, never) {
			t.Errorf("the runbook says %q:\n%s", never, text)
		}
	}
	for standing, want := range map[string]string{
		Rehearsing: "It is rehearsing: it decides beside the agent and does not act.",
		Paused:     "It is paused: it does not run, and the agent handles every ticket.",
		Retired:    "It is retired: it no longer runs.",
		Proposed:   "It is proposed: it does not run yet.",
	} {
		rec.Standing = standing
		if text := Runbook(rec); !strings.Contains(text, want) || strings.Contains(text, "acts on its own") {
			t.Errorf("the runbook of a %s routine:\n%s", standing, text)
		}
	}
	if got := TitleOf("close-a-duplicate"); got != "Close a duplicate" {
		t.Errorf("TitleOf = %q", got)
	}
}
