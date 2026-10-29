package duties

import (
	"context"
	"log/slog"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/faces"
	"github.com/bdubs00/featherweight-workbench/internal/bucket"
	"github.com/bdubs00/featherweight-workbench/sources"
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
		if c.event != "" && !strings.Contains(input, sources.Frame("the event of the duty watch", c.event)) ||
			c.found != "" && !strings.Contains(input, sources.Frame("the check of the duty watch", c.found)) {
			t.Errorf("the message lost the JSON or its frame: %q", input)
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

// A webhook reads in plain words, with the app that signs it when its
// header names one, and the noun of its key. The address stays in the
// part for developers.
func TestWebhookWords(t *testing.T) {
	for _, c := range []struct {
		w    Webhook
		want string
	}{
		{Webhook{Auth: WebhookAuth{Mode: "hmac", Header: "X-Gitea-Signature"}, Key: "issue.number"}, "When Gitea sends an event, signed. Repeats of one issue within a week are skipped."},
		{Webhook{Auth: WebhookAuth{Mode: "hmac", Header: "X-Hub-Signature-256"}, Key: "pull_request.id"}, "When GitHub sends an event, signed. Repeats of one pull request within a week are skipped."},
		{Webhook{Auth: WebhookAuth{Mode: "hmac", Header: "X-Signature-256"}, Key: "ticket_id"}, "When another app sends an event, signed. Repeats of one ticket within a week are skipped."},
		{Webhook{Auth: WebhookAuth{Mode: "bearer"}, Key: "id"}, "When another app sends an event with its key. Repeats of one event within a week are skipped."},
		{Webhook{Auth: WebhookAuth{Mode: "bearer"}, Key: "data.workOrderId"}, "When another app sends an event with its key. Repeats of one work order within a week are skipped."},
		{Webhook{Auth: WebhookAuth{Mode: "none"}}, "When another app sends an event. Nothing checks who sends it. Repeats of the same event within ten minutes are skipped."},
	} {
		if got := webhookWords(&c.w); got != c.want {
			t.Errorf("webhookWords(%+v) = %q, want %q", c.w, got, c.want)
		}
	}
	d := Duty{Name: "tickets", When: When{Webhook: &Webhook{Auth: WebhookAuth{Mode: "hmac", Header: "X-Gitea-Signature"}, Key: "issue.number"}}}
	if got := details(&d); got != "Another app calls POST /v1/duties/tickets/call with the HMAC-SHA256 signature of the body in the header X-Gitea-Signature. A second call with the same issue.number within a week is a duplicate." {
		t.Fatalf("details = %q", got)
	}
	if got := readback(&d, ""); strings.Contains(got, "/v1/") || strings.Contains(got, "HMAC") || strings.Contains(got, "issue.number") {
		t.Fatalf("the readback holds a word of the API: %q", got)
	}
}

// An event with a wait says it in words.
func TestEventWords(t *testing.T) {
	for _, c := range []struct {
		e    Event
		want string
	}{
		{Event{Kind: EventQuestionWaiting, After: "1h"}, "When a question waits longer than 1 hour."},
		{Event{Kind: EventQuestionWaiting, After: "90m"}, "When a question waits longer than 1 hour and 30 minutes."},
		{Event{Kind: EventBridgeClosed, After: "30m"}, "When a channel closes. The job starts 30 minutes later."},
		{Event{Kind: EventCapReached}, "When a job reaches its daily cost cap."},
		{Event{Kind: EventPageChanged, Path: "walk/", After: "0"}, "When a page of the wiki under walk/ changes."},
		{Event{Kind: EventRunFailed, Agent: "triage"}, "When a run of triage fails after its retries."},
		{Event{Kind: "custom.kind", Agent: "triage"}, "When the event custom.kind comes, for the agent triage."},
	} {
		if got := eventWords(&c.e); got != c.want {
			t.Errorf("eventWords(%+v) = %q, want %q", c.e, got, c.want)
		}
	}
}

// A readback names an agent by its display name, and the record keeps the
// name of the agent.
func TestReadbackNamesTheAgentInWords(t *testing.T) {
	f := newFront(t, checkBlock, nil)
	st := faces.New(bucket.NewMem(bucket.Config{Name: "FACES"}), slog.New(slog.DiscardHandler))
	faces.Attach(f.a, st)
	t.Cleanup(func() { faces.Attach(f.a, nil) })
	if _, err := st.Meta(context.Background(), "triage", "Triage Desk", "", "alice"); err != nil {
		t.Fatal(err)
	}
	d := Duty{Name: "q", Agent: "triage", When: When{Event: &Event{Kind: EventQuestionWaiting, Agent: "triage", After: "1h"}},
		CheckFirst: &CheckFirst{Look: Look{HTTP: &LookHTTP{URL: "https://example.com/q"}}, Wake: Wake{When: WakeAny}}}
	w := f.s.worded(context.Background(), d)
	if got := readback(&w, ""); got != "When a question of Triage Desk waits longer than 1 hour. First it looks at example.com/q. Any item wakes Triage Desk." {
		t.Fatalf("readback = %q", got)
	}
	if d.Agent != "triage" || d.When.Event.Agent != "triage" {
		t.Fatalf("the record changed: %+v", d)
	}
	if got := f.s.agentWords(context.Background(), "reviewer"); got != "reviewer" {
		t.Fatalf("an agent without a display name = %q", got)
	}
}

// oldWords matches the words that a person no longer reads: the page says
// job and channel. paths matches a path or an address, such as
// /v1/duties/x/call, which is no word.
var (
	oldWords = regexp.MustCompile(`(?i)\b(bridges?|seats?|seated|workers?|duty|duties|teams?)\b`)
	paths    = regexp.MustCompile(`\S*/\S*`)
)

// The readback of a job, its part for developers, its row While you are
// away, and the reasons of its history say job and channel, never duty,
// bridge, seat, worker, or team.
func TestWordsSayJob(t *testing.T) {
	check := &CheckFirst{Look: Look{HTTP: &LookHTTP{URL: "https://example.com/q?key=1"}}, Where: []Where{{Field: "state", Is: []byte(`"open"`)}}, Key: "id", Wake: Wake{When: WakeNew}}
	notify := &Notify{Result: []string{"owner", "bob@example.com"}, ResultWhen: ResultFound}
	duties := []Duty{
		{Name: "daily", Agent: "triage", When: When{Schedule: scheduleOf("daily", "09:00")}, CheckFirst: check, Notify: notify, Review: true},
		{Name: "bearer", Agent: "triage", When: When{Webhook: &Webhook{Auth: WebhookAuth{Mode: "bearer"}, Key: "ticket_id"}}, Notify: &Notify{Result: []string{"owner"}}},
		{Name: "hmac", Agent: "triage", When: When{Webhook: &Webhook{Auth: WebhookAuth{Mode: "hmac", Header: "X-Gitea-Signature"}}}, CheckFirst: &CheckFirst{Look: Look{Tool: "github.list_issues"}, Wake: Wake{When: WakeAtLeast, Count: 3}}},
		{Name: "open", Agent: "triage", When: When{Webhook: &Webhook{Auth: WebhookAuth{Mode: "none"}}}, Review: true},
	}
	for _, kind := range []string{EventBridgeClosed, EventRunFailed, EventQuestionWaiting, EventPageChanged, EventRoutineReady, EventCapReached, "custom.kind"} {
		duties = append(duties, Duty{Name: kind, Agent: "triage", When: When{Event: &Event{Kind: kind, Agent: "triage", Path: "walk/", After: "30m"}}})
	}
	var texts []string
	for _, d := range duties {
		texts = append(texts, readback(&d, "alice"), readback(&d, ""), details(&d), capWords(&d, 2))
	}
	texts = append(texts,
		AwayWords(nil),
		AwayWords(&WhileAway{Decider: DeciderManager, Timeout: "90m", Then: "cancel"}),
		lostWords("R-1"),
		failReason("budget exceeded: maxCostUSD"),
		failReason("the model refused"),
		reviewWords, youReview,
	)
	for _, text := range texts {
		if w := oldWords.FindString(paths.ReplaceAllString(text, "")); w != "" {
			t.Errorf("a text for a person says %q: %q", w, text)
		}
	}
}
