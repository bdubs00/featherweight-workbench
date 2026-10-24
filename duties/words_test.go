package duties

import (
	"strings"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// A person reads the title of a run, so the title names the event and
// counts the items. The message still gives the agent the JSON.
func TestTitleNamesTheEventAndCountsTheItems(t *testing.T) {
	slot := time.Date(2026, 10, 24, 18, 36, 28, 0, time.UTC)
	page := `{"by":"token","deleted":false,"kind":"page.changed","path":"walk/event-test.md","title":"Event test of the walk"}`
	cases := []struct {
		message, event, found string
		count                 int
		title                 string
	}{
		{"A page of the wiki changed: {event}. Read that page.", page, "", 0, "A page of the wiki changed: Event test of the walk"},
		{"A page changed: {event}", `{"kind":"page.changed","path":"walk/a.md"}`, "", 0, "A page changed: walk/a.md"},
		{"Something happened: {event}", `{"kind":"run.failed"}`, "", 0, "Something happened: run.failed"},
		{"Something happened: {event}", `not json`, "", 0, "Something happened: an event"},
		{"Triage these tickets: {found}", "", `{"number":30}` + "\n" + `{"number":31}`, 2, "Triage these tickets: 2 items"},
		{"Triage this ticket: {found}", "", `{"number":30}`, 1, "Triage this ticket: 1 item"},
		{"Nothing found: {found} and {event}", "", "", 0, "Nothing found: none and none"},
	}
	for _, c := range cases {
		d := &Duty{Name: "watch", Do: Do{Ask: &Ask{Message: c.message}}}
		e := entry{Event: c.event, FoundText: c.found}
		e.Found, e.Slot = c.count, slot
		if got := fw.TitleFrom(fill(d.Do.Message(), titleValues(d, e, nil))); got != c.title {
			t.Errorf("title of %q = %q, want %q", c.message, got, c.title)
		}
		input := fill(d.Do.Message(), placeholders(d, e, nil))
		if c.event != "" && !strings.Contains(input, c.event) || c.found != "" && !strings.Contains(input, c.found) {
			t.Errorf("the message lost the JSON: %q", input)
		}
	}
}

// A run that a limit of its budget stopped says the limit in words. Any
// other error keeps its own words.
func TestFailReasonSaysTheLimit(t *testing.T) {
	cases := map[string]string{
		"budget exceeded: maxCostUSD": "The run reached its limit of cost, so it stopped. Raise the limit under Limits if the work needs more.",
		"budget exceeded: deadline":   "The run reached its limit of working time, so it stopped. Raise the limit under Limits if the work needs more.",
		"budget exceeded: maxCostUSD of the run tree (this run and the runs under it used $0.20 of $0.20)": "The run failed: budget exceeded: maxCostUSD of the run tree (this run and the runs under it used $0.20 of $0.20).",
		"the model refused": "The run failed: the model refused.",
	}
	for err, want := range cases {
		if got := failReason(err); got != want {
			t.Errorf("failReason(%q) = %q, want %q", err, got, want)
		}
	}
}
