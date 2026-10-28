package duties

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-workbench/sources"
)

// itemCalls records the calls of OnItem for the length of a test.
func itemCalls(t *testing.T) func() []sources.Item {
	t.Helper()
	var mu sync.Mutex
	var got []sources.Item
	OnItem = func(_ context.Context, source, key, version string) {
		mu.Lock()
		got = append(got, sources.Item{Source: source, Key: key, Version: version})
		mu.Unlock()
	}
	t.Cleanup(func() { OnItem = nil })
	return func() []sources.Item {
		mu.Lock()
		defer mu.Unlock()
		return append([]sources.Item(nil), got...)
	}
}

// issue is an issue of the API of Gitea in the queue of the test.
func issue(number int, updated string, priority any) map[string]any {
	return map[string]any{"number": number, "updated_at": updated, "priority": priority, "repository": map[string]any{"full_name": "lab/helpdesk"}}
}

// A check hands OnItem every item of its answer that names an issue, also
// an item that its filters leave out, and a call of the webhook hands it
// the issue of its body, also for a duplicate.
func TestDutiesCallOnItem(t *testing.T) {
	got := itemCalls(t)
	q := &incidents{}
	srv := httptest.NewServer(q)
	defer srv.Close()
	f := newFront(t, checkBlock, nil)
	f.sec["SNOW_KEY"] = "s3cret"
	f.sec["HOOK_TOKEN"] = "tok"
	q.set(0, issue(42, "2026-10-26T10:00:00Z", 1), issue(43, "2026-10-26T11:00:00Z", 3))
	f.put(t, "alice", triageDuty(srv.URL))
	f.clock.Set(t0.Add(15*time.Minute + time.Second))
	f.pass(t)
	want := []sources.Item{
		{Source: "gitea", Key: "lab/helpdesk/issue/42", Version: "2026-10-26T10:00:00Z"},
		{Source: "gitea", Key: "lab/helpdesk/issue/43", Version: "2026-10-26T11:00:00Z"},
	}
	if list := got(); len(list) != 2 || list[0] != want[0] || list[1] != want[1] {
		t.Fatalf("OnItem after a check got %+v, want %+v", list, want)
	}

	f.put(t, "alice", hookDuty("hook"))
	body := `{"action": "edited", "issue": {"number": 42, "updated_at": "2026-10-27T09:00:00Z"}, "repository": {"full_name": "lab/helpdesk"}}`
	for _, status := range []int{http.StatusAccepted, http.StatusOK} {
		if resp, b := send(t, http.MethodPost, f.url+"/v1/duties/hook/call", "tok", body); resp.StatusCode != status {
			t.Fatalf("a call = %d %s", resp.StatusCode, b)
		}
	}
	call := sources.Item{Source: "gitea", Key: "lab/helpdesk/issue/42", Version: "2026-10-27T09:00:00Z"}
	if list := got(); len(list) != 4 || list[2] != call || list[3] != call {
		t.Fatalf("OnItem after two calls got %+v", list)
	}
}
