package sources

import (
	"context"
	"net/http"
	"sync"
	"testing"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// ItemOf finds the issue of Gitea in a webhook and in an item of the API,
// with the number and updated_at, and nothing in other events.
func TestItemOf(t *testing.T) {
	cases := []struct {
		name, body string
		want       Item
		ok         bool
	}{
		{"webhook of an issue", `{"action": "edited", "number": 42, "issue": {"id": 7, "number": 42, "updated_at": "2026-10-26T10:00:00Z", "title": "Printer"}, "repository": {"full_name": "lab/helpdesk"}}`,
			Item{"gitea", "lab/helpdesk/issue/42", "2026-10-26T10:00:00Z"}, true},
		{"webhook of a pull request", `{"action": "opened", "pull_request": {"number": 9, "updated_at": "2026-10-26T11:00:00Z"}, "repository": {"full_name": "lab/app"}}`,
			Item{"gitea", "lab/app/issue/9", "2026-10-26T11:00:00Z"}, true},
		{"an issue of the API", `{"id": 7, "number": 42, "updated_at": "2026-10-26T10:00:00Z", "repository": {"id": 1, "name": "helpdesk", "owner": "lab", "full_name": "lab/helpdesk"}}`,
			Item{"gitea", "lab/helpdesk/issue/42", "2026-10-26T10:00:00Z"}, true},
		{"an issue without a repository", `{"number": "42", "updated_at": "2026-10-26T10:00:00Z"}`,
			Item{"gitea", "issue/42", "2026-10-26T10:00:00Z"}, true},
		{"an issue of GitHub", `{"issue": {"node_id": "I_1", "number": 42, "updated_at": "2026-10-26T10:00:00Z"}}`, Item{}, false},
		{"no version", `{"issue": {"number": 42}}`, Item{}, false},
		{"no number", `{"issue": {"updated_at": "2026-10-26T10:00:00Z"}}`, Item{}, false},
		{"a fraction", `{"number": 4.5, "updated_at": "2026-10-26T10:00:00Z"}`, Item{}, false},
		{"a ticket of another system", `{"id": "T-1042", "updated": "2026-10-19T10:05:00Z"}`, Item{}, false},
	}
	for _, c := range cases {
		events, err := decodeEvents([]byte(c.body), "")
		if err != nil || len(events) != 1 {
			t.Fatalf("%s: %v", c.name, err)
		}
		got, ok := ItemOf(events[0])
		if ok != c.ok || got != c.want {
			t.Errorf("%s: ItemOf = %+v %v, want %+v %v", c.name, got, ok, c.want, c.ok)
		}
	}
	if ref := (Item{Source: "gitea", Key: "lab/helpdesk/issue/42"}).Ref(); ref != "item:gitea/lab/helpdesk/issue/42" {
		t.Errorf("Ref = %s", ref)
	}
}

// itemCalls records the calls of OnItem for the length of a test.
func itemCalls(t *testing.T) func() []Item {
	t.Helper()
	var mu sync.Mutex
	var got []Item
	OnItem = func(_ context.Context, source, key, version string) {
		mu.Lock()
		got = append(got, Item{source, key, version})
		mu.Unlock()
	}
	t.Cleanup(func() { OnItem = nil })
	return func() []Item {
		mu.Lock()
		defer mu.Unlock()
		return append([]Item(nil), got...)
	}
}

// A webhook of Gitea calls OnItem for each event that names an issue, a
// duplicate included, and an event of another system calls nothing.
func TestWebhookCallsOnItem(t *testing.T) {
	got := itemCalls(t)
	bus := fw.NewMemBus()
	newWorker(t, bus, "fw.jobs.default")
	f := newFront(t, bus, `
sources:
  - name: gitea
    agent: default
    auth: {mode: bearer, secret: TICKETS_TOKEN}
    input: "Triage #{{.issue.number}}"
    key: "{{.issue.number}}/{{.issue.updated_at}}"
`, secrets("TICKETS_TOKEN", ticketsToken), nil)
	url := f.url + "/v1/sources/gitea"
	auth := []string{"Authorization", "Bearer " + ticketsToken}
	event := `{"action": "edited", "issue": {"number": 42, "updated_at": "2026-10-26T10:00:00Z"}, "repository": {"full_name": "lab/helpdesk"}}`
	for range 2 {
		if resp, body := post(t, url, event, auth...); resp.StatusCode != http.StatusAccepted && resp.StatusCode != http.StatusOK {
			t.Fatalf("post = %d %s", resp.StatusCode, body)
		}
	}
	want := Item{"gitea", "lab/helpdesk/issue/42", "2026-10-26T10:00:00Z"}
	list := got()
	if len(list) != 2 || list[0] != want || list[1] != want {
		t.Fatalf("OnItem got %+v, want %+v twice", list, want)
	}
}
