package fleet

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/surface/native"
)

func TestWithMemberJSON(t *testing.T) {
	if got := string(withMemberJSON([]byte(`{"id":"r1","state":"running"}`), "m")); !strings.Contains(got, `"member":"m"`) {
		t.Errorf("run = %s", got)
	}
	var page struct {
		Run    map[string]any `json:"run"`
		Events []any          `json:"events"`
	}
	if err := json.Unmarshal(withMemberJSON([]byte(`{"run":{"id":"r1","state":"running"},"events":[]}`), "m"), &page); err != nil || page.Run["member"] != "m" || page.Events == nil {
		t.Errorf("page = %v, %v", page, err)
	}
	for _, in := range []string{`{"runs":[]}`, `[1,2]`, `not json`} {
		if got := string(withMemberJSON([]byte(in), "m")); got != in {
			t.Errorf("%s became %s", in, got)
		}
	}
}

// A read of the run object of a member that sleeps comes from the view, so
// the read wakes no Task. The answer carries the member and the texts of
// the stream, not the texts of a record. The ledger, the writes, and every
// read of a member that runs still go to the member through the router.
func TestRelayAnswersFromTheViewWhileTheMemberSleeps(t *testing.T) {
	t.Setenv("FW_ATESPACE", "")
	var mu sync.Mutex
	var reached []string
	router := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		reached = append(reached, r.Method+" "+r.URL.Path)
		mu.Unlock()
		if r.Header.Get(headerTargetActor) != "team/rev" || r.Header.Get("Authorization") != "Bearer tok" {
			http.Error(w, "unexpected request", http.StatusBadRequest)
			return
		}
		run := fw.Run{ID: "r1", Agent: "default", State: fw.StateInputRequired, Output: "from the worker"}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/runs/r1":
			_ = json.NewEncoder(w).Encode(run)
		case "/v1/runs/r1/events":
			_ = json.NewEncoder(w).Encode(map[string]any{"run": run, "events": []fw.Event{}})
		case "/v1/runs/r1/cancel":
			run.State = fw.StateCanceled
			_ = json.NewEncoder(w).Encode(run)
		default:
			http.Error(w, "unexpected path", http.StatusNotFound)
		}
	}))
	t.Cleanup(router.Close)
	front := newMember(t, fw.NewMemBus(), "front", func(c *fw.Config, _ *Config) {
		c.AX.Router = strings.TrimPrefix(router.URL, "http://")
	}, fw.WithSurfaces(native.New()))
	long := strings.Repeat("a", 3*TextLimit)
	big := `{"command":"` + strings.Repeat("b", 2*PendingInputLimit) + `"}`
	now := time.Now()
	run := &fw.Run{ID: "r1", Agent: "default", State: fw.StateInputRequired, Input: long, Seq: 7,
		Pending:   &fw.Approval{ID: "a1", Tool: "bash", Input: json.RawMessage(big), RequestedAt: now},
		CreatedAt: now.Add(-time.Minute), UpdatedAt: now}
	inject(front.f, "team/rev", "ax:team/rev", "team", MemberQuiet, now, run)
	h, err := front.a.Handler()
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	do := func(method, path string) (int, []byte) {
		t.Helper()
		req, _ := http.NewRequest(method, srv.URL+path, nil)
		req.Header.Set("Authorization", "Bearer tok")
		req.Header.Set("Accept", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, b
	}
	sent := func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), reached...)
	}

	// The member sleeps: the view answers the read, and nothing reaches it.
	code, b := do(http.MethodGet, "/v1/runs/r1")
	var got fw.Run
	if err := json.Unmarshal(b, &got); code != http.StatusOK || err != nil {
		t.Fatalf("read = %d %s, %v", code, b, err)
	}
	if got.ID != "r1" || got.Member != "team/rev" || got.State != fw.StateInputRequired || got.Input != long ||
		got.Pending == nil || string(got.Pending.Input) != big || got.Seq != 7 {
		t.Fatalf("the view answered %+v", got)
	}
	if s := sent(); len(s) != 0 {
		t.Fatalf("a read of a sleeping member reached it: %v", s)
	}
	// A stopped member cannot answer, so the view answers for it too.
	inject(front.f, "team/rev", "ax:team/rev", "team", MemberStopped, now)
	if code, b := do(http.MethodGet, "/v1/runs/r1"); code != http.StatusOK || len(sent()) != 0 {
		t.Fatalf("read of a stopped member = %d %s, sent %v", code, b, sent())
	}
	inject(front.f, "team/rev", "ax:team/rev", "team", MemberQuiet, now)

	// The ledger and a write live on the worker, so they wake it.
	if code, b := do(http.MethodGet, "/v1/runs/r1/events?limit=5"); code != http.StatusOK || !strings.Contains(string(b), `"member":"team/rev"`) {
		t.Fatalf("events = %d %s", code, b)
	}
	if code, b := do(http.MethodPost, "/v1/runs/r1/cancel"); code != http.StatusOK || !strings.Contains(string(b), `"canceled"`) {
		t.Fatalf("cancel = %d %s", code, b)
	}
	if s := sent(); len(s) != 2 || s[0] != "GET /v1/runs/r1/events" || s[1] != "POST /v1/runs/r1/cancel" {
		t.Fatalf("sent = %v", s)
	}

	// A member that runs answers the read itself, as before.
	inject(front.f, "team/rev", "ax:team/rev", "team", MemberLive, time.Now())
	code, b = do(http.MethodGet, "/v1/runs/r1")
	if code != http.StatusOK || !strings.Contains(string(b), `"from the worker"`) || !strings.Contains(string(b), `"member":"team/rev"`) {
		t.Fatalf("read of a live member = %d %s", code, b)
	}
	if s := sent(); len(s) != 3 || s[2] != "GET /v1/runs/r1" {
		t.Fatalf("sent = %v", s)
	}
}

// AtRest stands in for a worker that only the stream knows once it sent no
// event for QuietAfter intervals, and for no member that runs.
func TestAtRest(t *testing.T) {
	front := newMember(t, fw.NewMemBus(), "front", nil)
	now := time.Now()
	run := &fw.Run{ID: "r2", Agent: "default", State: fw.StateCompleted, Output: "done", CreatedAt: now, UpdatedAt: now}
	inject(front.f, "box:8080", "", "", "", now, run)
	listed := &fw.Run{ID: "r2", Member: "box:8080"}
	if c := front.f.AtRest(listed); c != nil {
		t.Fatalf("a worker that just sent an event is at rest: %+v", c)
	}
	inject(front.f, "box:8080", "", "", "", now.Add(-time.Hour))
	c := front.f.AtRest(listed)
	if c == nil || c.Output != "done" || c.Member != "box:8080" {
		t.Fatalf("AtRest = %+v", c)
	}
	// A run that the member does not hold, and a member that the view does
	// not know, have no copy.
	if c := front.f.AtRest(&fw.Run{ID: "r3", Member: "box:8080"}); c != nil {
		t.Fatalf("unknown run = %+v", c)
	}
	if c := front.f.AtRest(&fw.Run{ID: "r2", Member: "elsewhere"}); c != nil {
		t.Fatalf("unknown member = %+v", c)
	}
}

// fail answers an invalid request with 400 and the words after the prefix
// of the kernel, such as the refusal of a profile that several pools run.
func TestFailInvalidRequest(t *testing.T) {
	w := httptest.NewRecorder()
	fail(w, httptest.NewRequest(http.MethodGet, "/v1/agents/migrator", nil),
		fmt.Errorf("%w: the pools manager, crew run the agent migrator; pass pool to name one", fw.ErrInvalidRequest))
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), `"detail":"the pools manager, crew run the agent migrator; pass pool to name one"`) {
		t.Fatalf("fail = %d %s", w.Code, w.Body.String())
	}
}
