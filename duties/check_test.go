package duties

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/duties/schedule"
)

// incidents is a queue of a ticket system that answers its records with
// the key in X-Key, or fails with a status.
type incidents struct {
	mu      sync.Mutex
	records []map[string]any
	status  int
	hits    int
}

func (q *incidents) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.hits++
	if r.Header.Get("X-Key") != "s3cret" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if q.status != 0 {
		w.WriteHeader(q.status)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"result": map[string]any{"records": q.records}})
}

func (q *incidents) set(status int, records ...map[string]any) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.status, q.records = status, records
}

func inc(number string, priority any) map[string]any {
	return map[string]any{"number": number, "priority": priority, "short": "Disk full on " + number}
}

// checkBlock lets the checks reach the test server, with the secret of the
// queue.
const checkBlock = `
duties:
  checkHosts: ["127.0.0.1"]
  secrets:
    - {name: SNOW_KEY, hosts: ["127.0.0.1"]}
    - {name: HOOK_TOKEN}
`

// triageDuty looks at the queue every 15 minutes and wakes triage for new
// incidents of priority 1 or 2.
func triageDuty(url string) Duty {
	return Duty{
		Name:  "incidents",
		Agent: "triage",
		When:  When{Schedule: &schedule.Schedule{Repeat: schedule.RepeatEvery, Every: "15m", TimeZone: "UTC"}},
		CheckFirst: &CheckFirst{
			Look:  Look{HTTP: &LookHTTP{URL: url + "/api/queue?key=x", Secret: "SNOW_KEY", Header: "X-Key"}},
			Items: "result.records",
			Key:   "number",
			Where: []Where{{Field: "priority", OneOf: []json.RawMessage{json.RawMessage("1"), json.RawMessage("2")}}},
		},
		Do:     Do{Ask: &Ask{Message: "Triage these incidents:\n{found}"}},
		Limits: Limits{Budget: &Budget{MaxCostUSD: 0.2}, Overlap: OverlapAllow},
	}
}

// A check wakes the agent only for new items that pass the filters, and
// gives the run the items; a check that finds nothing new writes only the
// history.
func TestCheckWakesForNewItems(t *testing.T) {
	q := &incidents{}
	srv := httptest.NewServer(q)
	defer srv.Close()
	f := newFront(t, checkBlock, nil)
	f.sec["SNOW_KEY"] = "s3cret"
	q.set(0, inc("INC1", "1"), inc("INC2", 3), inc("INC3", 2))
	v := f.put(t, "alice", triageDuty(srv.URL))
	if !strings.Contains(v.Readback, "First it looks at 127.0.0.1") || strings.Contains(v.Readback, "key=x") ||
		!strings.Contains(v.Readback, "where priority is 1 or 2. Only items that it did not see before wake triage, by their number. The run gets the list.") {
		t.Fatalf("readback = %q", v.Readback)
	}
	slot := t0.Add(15 * time.Minute)
	f.clock.Set(slot.Add(time.Second))
	f.pass(t)
	req := f.queue.last(t)
	if !strings.Contains(req.Input, `"number":"INC1"`) || !strings.Contains(req.Input, `"number":"INC3"`) || strings.Contains(req.Input, "INC2") {
		t.Fatalf("the input = %q", req.Input)
	}
	h := f.history(t, "incidents")
	if h[0].Found != 2 || !h[0].Woke || h[0].State != StateQueued {
		t.Fatalf("history = %+v", h[0])
	}
	// The same items again: nothing new, so no run.
	f.clock.Set(slot.Add(15*time.Minute + time.Second))
	f.pass(t)
	if n := len(f.queue.list()); n != 1 {
		t.Fatalf("a check without new items started %d runs", n)
	}
	h = f.history(t, "incidents")
	if h[0].State != StateQuiet || h[0].Found != 2 || h[0].Woke || h[0].Reason != "The check found 2 items, and none is new, so the agent did not wake." {
		t.Fatalf("history = %+v", h[0])
	}
	// One new item wakes the agent with that item alone.
	q.set(0, inc("INC1", "1"), inc("INC3", 2), inc("INC4", 1))
	f.clock.Set(slot.Add(30*time.Minute + time.Second))
	f.pass(t)
	req = f.queue.last(t)
	if !strings.Contains(req.Input, "INC4") || strings.Contains(req.Input, "INC1") {
		t.Fatalf("the input = %q", req.Input)
	}
}

// Three failed checks in a row pause the duty with the reason, and the
// model never wakes for a failed check.
func TestCheckFailuresPause(t *testing.T) {
	q := &incidents{}
	srv := httptest.NewServer(q)
	defer srv.Close()
	f := newFront(t, checkBlock, nil)
	f.sec["SNOW_KEY"] = "s3cret"
	q.set(http.StatusServiceUnavailable)
	f.put(t, "alice", triageDuty(srv.URL))
	for i := 1; i <= 3; i++ {
		f.clock.Set(t0.Add(time.Duration(15*i) * time.Minute).Add(time.Second))
		f.pass(t)
	}
	if n := len(f.queue.list()); n != 0 {
		t.Fatalf("a failed check woke the model %d times", n)
	}
	h := f.history(t, "incidents")
	if h[0].State != StateCheckFailed || !strings.Contains(h[0].Reason, "127.0.0.1: the endpoint answered 503") {
		t.Fatalf("history = %+v", h[0])
	}
	v, _ := f.s.Get(as("alice"), "incidents")
	if !v.Paused || !strings.HasPrefix(v.State.PausedReason, "3 checks failed in a row, so the duty paused itself.") {
		t.Fatalf("after three failed checks = %v %q", v.Paused, v.State.PausedReason)
	}
	// A check that works between failures ends the row.
	f2 := newFront(t, checkBlock, nil)
	f2.sec["SNOW_KEY"] = "s3cret"
	f2.put(t, "alice", triageDuty(srv.URL))
	for i, status := range []int{500, 500, 0, 500, 500} {
		q.set(status, inc("INC1", 1))
		f2.clock.Set(t0.Add(time.Duration(15*(i+1)) * time.Minute).Add(time.Second))
		f2.pass(t)
	}
	if v, _ := f2.s.Get(as("alice"), "incidents"); v.Paused {
		t.Fatal("a row of failed checks with a good check in it paused the duty")
	}
}

// The filters compare numbers as numbers, also in text, and text as text;
// wake any and atLeast count the items that pass.
func TestJudge(t *testing.T) {
	f := newFront(t, "", nil)
	raw := func(s string) json.RawMessage { return json.RawMessage(s) }
	body := []byte(`{"items": [
		{"id": 1, "p": "1", "tags": ["disk", "db"], "title": "Disk FULL", "at": "2026-10-20", "who": {"team": "ops"}},
		{"id": 2, "p": 2, "tags": ["net"], "title": "Link down", "at": "2026-10-25", "who": {"team": "net"}},
		{"id": 3, "p": 4, "tags": [], "title": "Slow", "at": "2026-10-26", "who": null}
	]}`)
	for _, c := range []struct {
		where Where
		want  []float64
	}{
		{Where{Field: "p", Is: raw("1")}, []float64{1}},
		{Where{Field: "p", IsNot: raw("1")}, []float64{2, 3}},
		{Where{Field: "p", OneOf: []json.RawMessage{raw("2"), raw(`"4"`)}}, []float64{2, 3}},
		{Where{Field: "tags", Contains: raw(`"db"`)}, []float64{1}},
		{Where{Field: "title", Contains: raw(`"full"`)}, []float64{1}},
		{Where{Field: "p", AtLeast: raw("2")}, []float64{2, 3}},
		{Where{Field: "p", AtMost: raw("2")}, []float64{1, 2}},
		{Where{Field: "at", AtLeast: raw(`"2026-10-25"`)}, []float64{2, 3}},
		{Where{Field: "who.team", Is: raw(`"ops"`)}, []float64{1}},
		{Where{Field: "missing", IsNot: raw(`"x"`)}, []float64{1, 2, 3}},
	} {
		d := &Duty{Name: "j", CheckFirst: &CheckFirst{Items: "items", Where: []Where{c.where}, Wake: Wake{When: WakeAny}}}
		res, err := f.s.judge(context.Background(), d, body)
		if err != nil {
			t.Fatal(err)
		}
		var ids []float64
		for _, it := range res.Matched {
			n, _ := number(it["id"])
			ids = append(ids, n)
		}
		if !slices.Equal(ids, c.want) {
			t.Errorf("where %+v = %v, want %v", c.where, ids, c.want)
		}
	}
	d := &Duty{Name: "j", CheckFirst: &CheckFirst{Items: "items", Wake: Wake{When: WakeAtLeast, Count: 4}}}
	res, _ := f.s.judge(context.Background(), d, body)
	if res.Wakes || res.Why != "The check found 3 items, fewer than 4, so the agent did not wake." {
		t.Fatalf("atLeast 4 = %+v", res)
	}
	d.CheckFirst.Wake.Count = 3
	if res, _ := f.s.judge(context.Background(), d, body); !res.Wakes || len(res.Wake) != 3 {
		t.Fatalf("atLeast 3 = %+v", res)
	}
	d.CheckFirst.Items = "nothing"
	if _, err := f.s.judge(context.Background(), d, body); err == nil || !strings.Contains(err.Error(), "the answer does not read") {
		t.Fatalf("a path that finds nothing = %v", err)
	}
	if text := foundText([]map[string]any{{"n": strings.Repeat("x", maxFoundBytes)}, {"n": 2}}); text != "(2 more items are not shown)" {
		t.Fatalf("found text over the bound = %q", text)
	}
}

// The rules of the operator: a check reaches only the hosts of
// checkHosts, or without it no loopback or private address, and a secret
// goes only to its hosts.
func TestCheckRules(t *testing.T) {
	q := &incidents{}
	srv := httptest.NewServer(q)
	defer srv.Close()
	q.set(0, inc("INC1", 1))
	// Without checkHosts the dialer refuses the loopback address of the
	// test server, also on a test.
	f := newFront(t, "duties:\n  secrets: [{name: SNOW_KEY, hosts: [\"127.0.0.1\"]}]\n", nil)
	f.sec["SNOW_KEY"] = "s3cret"
	d := triageDuty(srv.URL)
	_, err := f.s.Put(as("alice"), d.Name, DutyBody{Duty: d}, PutOptions{DryRun: true, Test: true})
	if !errors.Is(err, ErrUpstream) || !strings.Contains(err.Error(), "loopback, private, or link-local") {
		t.Fatalf("a test of a loopback address = %v", err)
	}
	// A secret that the list does not name, or names for another host.
	other := newFront(t, "duties:\n  secrets: [{name: SNOW_KEY, hosts: [\"snow.example.com\"]}]\n", nil)
	other.sec["SNOW_KEY"], other.sec["OTHER"] = "s3cret", "x"
	ans, err := other.s.Put(as("alice"), d.Name, DutyBody{Duty: d}, PutOptions{DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(ans.Dry.Problems, " "); !strings.Contains(got, "checkFirst.look.http.secret: workbench.duties.secrets does not allow SNOW_KEY for the host 127.0.0.1") {
		t.Fatalf("problems = %q", got)
	}
	d.CheckFirst.Look.HTTP.Secret = "OTHER"
	ans, _ = other.s.Put(as("alice"), d.Name, DutyBody{Duty: d}, PutOptions{DryRun: true})
	if got := strings.Join(ans.Dry.Problems, " "); !strings.Contains(got, "checkFirst.look.http.secret: OTHER is not in workbench.duties.secrets") {
		t.Fatalf("problems = %q", got)
	}
	// checkHosts that do not list the host.
	listed := newFront(t, "duties:\n  checkHosts: [\"*.example.com\"]\n", nil)
	d.CheckFirst.Look.HTTP.Secret, d.CheckFirst.Look.HTTP.Header = "", ""
	ans, _ = listed.s.Put(as("alice"), d.Name, DutyBody{Duty: d}, PutOptions{DryRun: true})
	if got := strings.Join(ans.Dry.Problems, " "); !strings.Contains(got, "checkFirst.look.http.url: The address reaches the host 127.0.0.1, which workbench.duties.checkHosts does not list") {
		t.Fatalf("problems = %q", got)
	}
	// A secret that the list names but the front does not hold.
	empty := newFront(t, checkBlock, nil)
	d = triageDuty(srv.URL)
	ans, _ = empty.s.Put(as("alice"), d.Name, DutyBody{Duty: d}, PutOptions{DryRun: true})
	if got := strings.Join(ans.Dry.Problems, " "); !strings.Contains(got, "checkFirst.look.http.secret: SNOW_KEY holds no secret in this front") {
		t.Fatalf("problems = %q", got)
	}
	if _, err := LoadConfig(func() *fw.Config {
		cfg := fw.DefaultConfig()
		cfg.Extensions = workbench(t, "duties:\n  secrets: [{name: \"1bad\"}]\n  checkHosts: [\"[\"]\n")
		return cfg
	}()); err == nil || !strings.Contains(err.Error(), "secrets[0].name") || !strings.Contains(err.Error(), "checkHosts[0]") {
		t.Fatalf("a bad configuration = %v", err)
	}
}

// A test runs the check once and shows what it finds now, and writes no
// seen key.
func TestCheckTest(t *testing.T) {
	q := &incidents{}
	srv := httptest.NewServer(q)
	defer srv.Close()
	f := newFront(t, checkBlock, nil)
	f.sec["SNOW_KEY"] = "s3cret"
	q.set(0, inc("INC1", 1), inc("INC2", 3))
	d := triageDuty(srv.URL)
	for range 2 {
		ans, err := f.s.Put(as("alice"), d.Name, DutyBody{Duty: d}, PutOptions{DryRun: true, Test: true})
		if err != nil {
			t.Fatal(err)
		}
		c := ans.Dry.Check
		if c == nil || c.Count != 1 || !c.Wakes || c.Matched != 1 || c.Found[0]["number"] != "INC1" {
			t.Fatalf("the test = %+v", c)
		}
	}
	if seen, _ := f.s.st.seenOf(context.Background(), "incidents"); len(seen) != 0 {
		t.Fatalf("a test wrote seen keys: %v", seen)
	}
}

// A check with a tool of the agent saves, and until a package registers
// the look tool, a test and a start answer "not available yet".
func TestCheckTool(t *testing.T) {
	f := newFront(t, "", nil)
	d := triageDuty("https://example.com")
	d.CheckFirst.Look = Look{Tool: "servicenow.query_incidents", Input: map[string]any{"query": "priority<=2"}}
	ans, err := f.s.Put(as("alice"), d.Name, DutyBody{Duty: d}, PutOptions{DryRun: true, Test: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(ans.Dry.Problems, " "); !strings.Contains(got, "checkFirst.look.tool: Not available yet") || ans.Dry.Check != nil {
		t.Fatalf("the test of a tool = %q, %+v", got, ans.Dry.Check)
	}
	if !strings.Contains(ans.Dry.Readback, "First it calls the tool servicenow.query_incidents") {
		t.Fatalf("readback = %q", ans.Dry.Readback)
	}
	f.put(t, "alice", d)
	f.clock.Set(t0.Add(15*time.Minute + time.Second))
	f.pass(t)
	h := f.history(t, "incidents")
	if h[0].State != StateCheckFailed || !strings.Contains(h[0].Reason, "not available yet") {
		t.Fatalf("history = %+v", h[0])
	}
	// A registered look serves the check.
	RegisterLook("tool", func(context.Context, *fw.Agent, *Duty) ([]byte, error) {
		return []byte(`{"result": {"records": [{"number": "INC9", "priority": 1}]}}`), nil
	})
	defer func() {
		hooks.mu.Lock()
		delete(hooks.looks, "tool")
		hooks.mu.Unlock()
	}()
	f.clock.Set(t0.Add(30*time.Minute + time.Second))
	f.pass(t)
	if req := f.queue.last(t); !strings.Contains(req.Input, "INC9") {
		t.Fatalf("the input = %q", req.Input)
	}
}

// The items of a start that fails for good go back to the check, so the
// next check wakes the agent for them again. A start that reached its run
// keeps them.
func TestCheckGivesBackTheItemsOfAFailedStart(t *testing.T) {
	q := &incidents{}
	srv := httptest.NewServer(q)
	defer srv.Close()
	f := newFront(t, checkBlock, nil)
	f.sec["SNOW_KEY"] = "s3cret"
	q.set(0, inc("INC1", "1"))
	f.put(t, "alice", triageDuty(srv.URL))
	f.queue.setFail(errDown)
	slot := t0.Add(15 * time.Minute)
	f.clock.Set(slot.Add(time.Second))
	f.pass(t)
	f.clock.Set(slot.Add(retryPause + 2*time.Second))
	f.pass(t)
	if h := f.history(t, "incidents"); h[0].State != StateFailed || !h[0].Woke {
		t.Fatalf("history = %+v", h[0])
	}
	f.queue.setFail(nil)
	f.clock.Set(slot.Add(15*time.Minute + time.Second))
	f.pass(t)
	if req := f.queue.last(t); !strings.Contains(req.Input, "INC1") {
		t.Fatalf("the input = %q", req.Input)
	}
	f.clock.Set(slot.Add(30*time.Minute + time.Second))
	f.pass(t)
	if n := len(f.queue.list()); n != 1 {
		t.Fatalf("the items of a start that reached its run woke the agent again: %d runs", n)
	}
}
