package sources

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// queueEndpoint is the endpoint of a ticket system that lists the new
// tickets under result.tickets. It answers status when status is set.
type queueEndpoint struct {
	srv     *httptest.Server
	mu      sync.Mutex
	tickets []map[string]any
	headers []http.Header
	status  int
	hits    atomic.Int32
}

func newQueueEndpoint(t *testing.T) *queueEndpoint {
	t.Helper()
	q := &queueEndpoint{}
	q.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q.hits.Add(1)
		q.mu.Lock()
		q.headers = append(q.headers, r.Header.Clone())
		status, tickets := q.status, append([]map[string]any(nil), q.tickets...)
		q.mu.Unlock()
		if status != 0 {
			http.Error(w, "the ticket system is down", status)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"result": map[string]any{"tickets": tickets}})
	}))
	t.Cleanup(q.srv.Close)
	return q
}

func (q *queueEndpoint) add(ts ...map[string]any) {
	q.mu.Lock()
	q.tickets = append(q.tickets, ts...)
	q.mu.Unlock()
}

func (q *queueEndpoint) header(i int) http.Header {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.headers[i]
}

// startPolls starts the sources of the list on the agent of a front with
// the store, without the limits of Load, so a test polls fast.
func startPolls(t *testing.T, a *fw.Agent, st store, list ...Source) *Sources {
	t.Helper()
	for i := range list {
		list[i] = withDefaults(list[i])
	}
	s, err := newSources(context.Background(), a, list, st)
	if err != nil {
		t.Fatal(err)
	}
	for _, src := range s.list {
		if src.poll != nil {
			src.poll.first = time.Millisecond
		}
	}
	attach(a, s)
	s.start()
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func pollSource(url string, every time.Duration) Source {
	return Source{Name: "queue", Agent: "default", Items: "result.tickets",
		Poll:  &Poll{URL: url, Every: every, Timeout: time.Second, Secret: "QUEUE_TOKEN"},
		Input: "Triage ticket {{.id}}", Title: "Ticket {{.id}}", Key: "{{.id}}/{{.updated}}"}
}

// TestPollMakesRunsOnce polls one endpoint from two replicas of the front
// that share the claims. Each new item makes one run, whichever replica
// sees it first, and the polls that follow make none.
func TestPollMakesRunsOnce(t *testing.T) {
	bus := &countBus{Bus: fw.NewMemBus()}
	worker := newWorker(t, bus, "fw.jobs.default")
	q := newQueueEndpoint(t)
	q.add(map[string]any{"id": "T-1", "updated": "10:00"}, map[string]any{"id": "T-2", "updated": "10:00"})
	shared := newMemStore(map[string]time.Duration{"queue": time.Hour})
	sec := secrets("QUEUE_TOKEN", "q-token-9")
	var fronts []*fw.Agent
	for range 2 {
		f := newFront(t, bus, `teams: []`, sec, nil)
		startPolls(t, f.a, shared, pollSource(q.srv.URL+"/api/tickets?state=new", 20*time.Millisecond))
		fronts = append(fronts, f.a)
	}
	eventually(t, "the worker runs both tickets", func() bool { return len(runsOf(t, worker)) == 2 })
	before := q.hits.Load()
	eventually(t, "both replicas poll again", func() bool { return q.hits.Load() > before+6 })
	if n, sent := len(runsOf(t, worker)), bus.sent.Load(); n != 2 || sent != 2 {
		t.Fatalf("after more polls the worker has %d runs from %d requests, want 2", n, sent)
	}
	// A new ticket, and a new version of an old one.
	q.add(map[string]any{"id": "T-3", "updated": "10:05"}, map[string]any{"id": "T-1", "updated": "10:05"})
	eventually(t, "the worker runs the new items", func() bool { return len(runsOf(t, worker)) == 4 })
	time.Sleep(100 * time.Millisecond)
	if n := len(runsOf(t, worker)); n != 4 {
		t.Fatalf("the worker has %d runs, want 4", n)
	}
	for _, r := range runsOf(t, worker) {
		if r.Principal != "source:queue" || r.Metadata[fw.MetaBatch] != "queue" || !strings.HasPrefix(r.Title, "Ticket T-") {
			t.Fatalf("run = %+v", r)
		}
	}
	if got := q.header(0).Get("Authorization"); got != "Bearer q-token-9" {
		t.Fatalf("Authorization = %q", got)
	}
	var accepted, duplicate uint64
	for _, a := range fronts {
		st := statOf(t, a, "queue")
		accepted, duplicate = accepted+st.Accepted, duplicate+st.Duplicate
		if st.Kind != "poll" || st.LastPollAt.IsZero() || st.Refused != 0 {
			t.Fatalf("stat = %+v", st)
		}
	}
	if accepted != 4 || duplicate == 0 {
		t.Fatalf("accepted %d, duplicate %d", accepted, duplicate)
	}
}

// TestPollBacksOff waits longer after each failed poll, and keeps the
// credential and the query of the endpoint out of the logs and the
// counters.
func TestPollBacksOff(t *testing.T) {
	q := newQueueEndpoint(t)
	q.status = http.StatusServiceUnavailable
	f := newFront(t, fw.NewMemBus(), `teams: []`, secrets("QUEUE_TOKEN", "q-token-9"), nil)
	src := pollSource(q.srv.URL+"/api/tickets?apikey=q-query-secret", 20*time.Millisecond)
	src.Poll.Header = "X-Api-Key"
	startPolls(t, f.a, newMemStore(nil), src)
	time.Sleep(500 * time.Millisecond)
	// Without a backoff, 500ms hold 25 polls of 20ms; with it, the pauses
	// are 40, 80, 160, and 320ms.
	if n := q.hits.Load(); n < 2 || n > 6 {
		t.Fatalf("%d polls in 500ms", n)
	}
	if got := q.header(0).Get("X-Api-Key"); got != "q-token-9" || q.header(0).Get("Authorization") != "" {
		t.Fatalf("headers = %v", q.header(0))
	}
	st := statOf(t, f.a, "queue")
	if !strings.Contains(st.LastError, "answered 503") || st.LastErrorAt.IsZero() || st.Accepted != 0 {
		t.Fatalf("stat = %+v", st)
	}
	logs := f.logs.String()
	for _, secret := range []string{"q-token-9", "q-query-secret"} {
		if strings.Contains(logs, secret) || strings.Contains(st.LastError, secret) {
			t.Fatalf("%s leaked: %q %q", secret, st.LastError, logs)
		}
	}
	if !strings.Contains(logs, "a poll failed") {
		t.Fatalf("logs = %s", logs)
	}
}

func TestBackoff(t *testing.T) {
	for _, c := range []struct {
		every    time.Duration
		failures int
		want     time.Duration
	}{
		{time.Minute, 0, time.Minute},
		{time.Minute, 1, 2 * time.Minute},
		{time.Minute, 4, 16 * time.Minute},
		{time.Minute, 5, maxBackoff},
		{time.Minute, 50, maxBackoff},
		{time.Hour, 3, time.Hour},
	} {
		if got := backoff(c.every, c.failures); got != c.want {
			t.Errorf("backoff(%s, %d) = %s, want %s", c.every, c.failures, got, c.want)
		}
	}
}

// TestPollRedirect follows no redirect to another host, so the credential
// of the poll stays with its endpoint.
func TestPollRedirect(t *testing.T) {
	var leaked atomic.Bool
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Api-Key") != "" || r.Header.Get("Authorization") != "" {
			leaked.Store(true)
		}
		_, _ = w.Write([]byte(`{"result": {"tickets": []}}`))
	}))
	defer other.Close()
	moved := httptest.NewServer(http.RedirectHandler(other.URL+"/api", http.StatusFound))
	defer moved.Close()
	f := newFront(t, fw.NewMemBus(), `teams: []`, secrets("QUEUE_TOKEN", "q-token-9"), nil)
	src := pollSource(moved.URL, 20*time.Millisecond)
	src.Poll.Header = "X-Api-Key"
	startPolls(t, f.a, newMemStore(nil), src)
	eventually(t, "the poll fails", func() bool { return strings.Contains(statOf(t, f.a, "queue").LastError, "another host") })
	if leaked.Load() {
		t.Fatal("the credential reached another host")
	}
}

// TestPollRefusesAnItem counts an item that does not fit the templates as
// refused, and the other items still run.
func TestPollRefusesAnItem(t *testing.T) {
	bus := fw.NewMemBus()
	worker := newWorker(t, bus, "fw.jobs.default")
	q := newQueueEndpoint(t)
	q.add(map[string]any{"id": "T-1", "updated": "10:00"}, map[string]any{"id": "T-2"})
	f := newFront(t, bus, `teams: []`, secrets("QUEUE_TOKEN", "q-token-9"), nil)
	startPolls(t, f.a, newMemStore(nil), pollSource(q.srv.URL, time.Hour))
	eventually(t, "the worker runs the item that fits", func() bool { return len(runsOf(t, worker)) == 1 })
	st := statOf(t, f.a, "queue")
	if st.Accepted != 1 || st.Refused != 1 || !strings.Contains(st.LastError, "item 2") {
		t.Fatalf("stat = %+v", st)
	}
}
