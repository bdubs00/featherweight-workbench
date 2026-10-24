package actions

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/surface/native"
	"github.com/bdubs00/featherweight-workbench/bridges"
	"github.com/bdubs00/featherweight-workbench/duties"
	"github.com/bdubs00/featherweight-workbench/fleet"
	"github.com/bdubs00/featherweight-workbench/manager"
	"github.com/bdubs00/featherweight-workbench/playbooks"
)

// people takes the bearer token as the subject of a person.
type people struct{}

func (people) Authenticate(r *http.Request) (*fw.Principal, error) {
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || tok == "" {
		return nil, fw.ErrUnauthorized
	}
	return &fw.Principal{Subject: tok, Kind: fw.KindUser}, nil
}

func (people) Challenge(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusUnauthorized)
}

// front is a process with the duties, the bridges, and the playbooks in
// memory, served on the native surface. alice owns the agents triage and
// migrator and operates the manager, and dave owns private. The requests
// that go to the queues of migrator and the manager land in requests.
type front struct {
	a   *fw.Agent
	url string

	mu       sync.Mutex
	requests map[string][]fw.RunRequest // by queue
}

func newFront(t *testing.T, withManager bool) *front {
	t.Helper()
	t.Setenv("FW_ATESPACE", "")
	t.Setenv(fleet.EnvIntake, "fw.jobs")
	for _, env := range []string{bridges.EnvPlacement, bridges.EnvMaxMembers, bridges.EnvMaxMessages, bridges.EnvMaxFanoutPerMinute, bridges.EnvMaxPairMessages, bridges.EnvMaxTurns} {
		t.Setenv(env, "")
	}
	t.Setenv(manager.EnvManager, "")
	if withManager {
		t.Setenv(manager.EnvManager, "manager")
	}
	cfg := fw.DefaultConfig()
	cfg.Home = t.TempDir()
	cfg.Access.Default = "closed"
	cfg.Agents = map[string]fw.AgentProfile{
		"triage":   {Access: &fw.Access{Owners: []string{"alice"}}},
		"migrator": {Access: &fw.Access{Owners: []string{"alice"}}},
		"manager":  {Access: &fw.Access{Operators: []string{"alice"}}},
		"private":  {Access: &fw.Access{Owners: []string{"dave"}}},
	}
	bus := fw.NewMemBus()
	a, err := fw.New(fw.WithConfig(cfg), fw.WithoutDefaults(), fw.WithAuthn(people{}),
		fw.WithModel(fw.ModelFunc{GenerateFn: func(context.Context, *fw.Request) (*fw.Response, error) {
			return &fw.Response{Content: []fw.Block{fw.Text("done")}, StopReason: fw.StopEndTurn}, nil
		}}),
		fw.WithLedger(fw.NewMemLedger()), fw.WithBus(bus), fw.WithSurfaces(native.New()),
		fw.WithServices(bridges.Start, playbooks.Start, duties.Start), fw.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	if err != nil {
		t.Fatal(err)
	}
	h, err := a.Handler()
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(func() {
		srv.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		a.Close(ctx)
	})
	f := &front{a: a, url: srv.URL, requests: map[string][]fw.RunRequest{}}
	stop, err := bus.Subscribe(context.Background(), "fw.jobs.>", func(e fw.Event) error {
		var req fw.RunRequest
		if e.Type == fleet.EvRunRequest && e.Decode(&req) == nil {
			f.mu.Lock()
			f.requests[req.Agent] = append(f.requests[req.Agent], req)
			f.mu.Unlock()
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stop)
	return f
}

// queued returns the requests that went to the queue of an agent, once
// n of them arrived, or after five seconds.
func (f *front) queued(agent string, n int) []fw.RunRequest {
	deadline := time.Now().Add(5 * time.Second)
	for {
		f.mu.Lock()
		reqs := append([]fw.RunRequest(nil), f.requests[agent]...)
		f.mu.Unlock()
		if len(reqs) >= n || time.Now().After(deadline) {
			return reqs
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (f *front) send(t *testing.T, method, path, who, body string) (int, map[string]any) {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, _ := http.NewRequest(method, f.url+path, rd)
	req.Header.Set("Authorization", "Bearer "+who)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	var out map[string]any
	if len(b) > 0 {
		if err := json.Unmarshal(b, &out); err != nil {
			t.Fatalf("decode %s: %v", b, err)
		}
	}
	return resp.StatusCode, out
}

// duty is a duty of triage every day at 9:00 in UTC with the action do.
func duty(do string) string {
	return `{"agent": "triage", "when": {"say": "every day at 9am UTC"}, "do": ` + do + `, "limits": {"budget": {"maxCostUSD": 0.25, "maxTurns": 10}}}`
}

// problemsOf returns the problems of a dry run, joined.
func problemsOf(t *testing.T, out map[string]any) string {
	t.Helper()
	var list []string
	for _, p := range out["problems"].([]any) {
		list = append(list, p.(string))
	}
	return strings.Join(list, " | ")
}

// lastStart returns the newest start of a duty.
func (f *front) lastStart(t *testing.T, name string) map[string]any {
	t.Helper()
	status, v := f.send(t, http.MethodGet, "/v1/duties/"+name, "alice", "")
	hist, _ := v["history"].([]any)
	if status != http.StatusOK || len(hist) == 0 {
		t.Fatalf("the duty %s = %d %v", name, status, v)
	}
	return hist[0].(map[string]any)
}

// A duty that posts in a bridge posts its message through the post of the
// bridge, as the duty, to the members of to. Its start names no run. A
// bridge that the writer cannot use, a closed bridge, and a handle that
// the bridge does not have are problems of the write.
func TestPostInABridge(t *testing.T) {
	f := newFront(t, false)
	status, b := f.send(t, http.MethodPost, "/v1/bridges", "alice", `{"title": "Ops", "members": [{"handle": "ops", "agent": "migrator", "task": "Run the lab."}]}`)
	if status != http.StatusCreated {
		t.Fatalf("the bridge = %d %v", status, b)
	}
	id := b["id"].(string)
	status, v := f.send(t, http.MethodPut, "/v1/duties/status", "alice", duty(`{"post": {"bridge": "`+id+`", "to": ["ops"], "message": "Report the status of {date}."}}`))
	if status != http.StatusCreated {
		t.Fatalf("the duty = %d %v", status, v)
	}
	status, v = f.send(t, http.MethodPost, "/v1/duties/status/run", "alice", "")
	if status != http.StatusAccepted || v["run"] != "" {
		t.Fatalf("run now = %d %v", status, v)
	}
	_, page := f.send(t, http.MethodGet, "/v1/bridges/"+id+"/messages", "alice", "")
	msgs := page["messages"].([]any)
	last := msgs[len(msgs)-1].(map[string]any)
	from := last["from"].(map[string]any)
	today := time.Now().UTC().Format(time.DateOnly)
	if last["text"] != "Report the status of "+today+"." || from["name"] != "duty:status" || from["display"] != "the duty status" || len(last["to"].([]any)) != 1 {
		t.Fatalf("the post = %v", last)
	}
	start := f.lastStart(t, "status")
	if start["state"] != duties.StateCompleted || start["run"] != nil && start["run"] != "" || !strings.HasPrefix(start["reason"].(string), "Posted message ") {
		t.Fatalf("the start = %v", start)
	}
	for _, c := range []struct{ do, want string }{
		{`{"post": {"bridge": "01nobridge", "message": "x"}}`, "do.post.bridge: No bridge that you can use has the id 01nobridge."},
		{`{"post": {"bridge": "` + id + `", "to": ["nobody"], "message": "x"}}`, "do.post.to: nobody is no member of the bridge"},
		{`{"post": {"bridge": "", "message": " "}}`, "do.post.message: The message is empty."},
	} {
		_, out := f.send(t, http.MethodPut, "/v1/duties/x?dryRun=true", "alice", duty(c.do))
		if got := problemsOf(t, out); !strings.Contains(got, c.want) {
			t.Errorf("problems of %s = %s, want %s", c.do, got, c.want)
		}
	}
	if status, _ := f.send(t, http.MethodPatch, "/v1/bridges/"+id, "alice", `{"state": "closed"}`); status != http.StatusOK {
		t.Fatalf("the close = %d", status)
	}
	_, out := f.send(t, http.MethodPut, "/v1/duties/x?dryRun=true", "alice", duty(`{"post": {"bridge": "`+id+`", "message": "x"}}`))
	if got := problemsOf(t, out); !strings.Contains(got, "do.post.bridge: The bridge "+id+" is closed.") {
		t.Fatalf("problems of a closed bridge = %s", got)
	}
	status, v = f.send(t, http.MethodPost, "/v1/duties/status/run", "alice", "")
	if status != http.StatusBadGateway || !strings.Contains(v["detail"].(string), "is closed") {
		t.Fatalf("a post in a closed bridge = %d %v", status, v)
	}
}

// openedPattern finds the bridge in the note of a start of open.
var openedPattern = regexp.MustCompile(`Opened the bridge (\S+) "([^"]+)" from the playbook (\S+)\.`)

// A duty that opens a bridge from a playbook opens it through the open of
// a bridge, titled with the duty and the date of the start, as the duty,
// with the goal of the duty. A playbook that the writer cannot read is a
// problem of the write.
func TestOpenFromAPlaybook(t *testing.T) {
	f := newFront(t, false)
	if status, v := f.send(t, http.MethodPut, "/v1/playbooks/weekly", "alice", `{"goal": "Patch the lab.", "seats": [{"handle": "patcher", "agent": "migrator", "task": "Patch the hosts."}]}`); status != http.StatusCreated {
		t.Fatalf("the playbook = %d %v", status, v)
	}
	if status, v := f.send(t, http.MethodPut, "/v1/playbooks/secret", "dave", `{"goal": "x", "seats": [{"handle": "p", "agent": "private", "task": "x"}]}`); status != http.StatusCreated {
		t.Fatalf("the playbook of dave = %d %v", status, v)
	}
	if status, v := f.send(t, http.MethodPut, "/v1/duties/weekly", "alice", duty(`{"open": {"playbook": "weekly", "goal": "Patch the lab on {date}."}}`)); status != http.StatusCreated {
		t.Fatalf("the duty = %d %v", status, v)
	}
	if status, v := f.send(t, http.MethodPost, "/v1/duties/weekly/run", "alice", ""); status != http.StatusAccepted {
		t.Fatalf("run now = %d %v", status, v)
	}
	start := f.lastStart(t, "weekly")
	m := openedPattern.FindStringSubmatch(start["reason"].(string))
	today := time.Now().UTC().Format(time.DateOnly)
	if start["state"] != duties.StateCompleted || m == nil || m[2] != "weekly: "+today || m[3] != "weekly" {
		t.Fatalf("the start = %v", start)
	}
	status, b := f.send(t, http.MethodGet, "/v1/bridges/"+m[1], "alice", "")
	if status != http.StatusOK || b["title"] != "weekly: "+today || b["goal"] != "Patch the lab on "+today+"." || b["operator"] != "duty:weekly" {
		t.Fatalf("the bridge = %d %v", status, b)
	}
	if reqs := f.queued("migrator", 1); len(reqs) != 1 || reqs[0].Principal != "duty:weekly" {
		t.Fatalf("the seat requests = %+v", reqs)
	}
	for _, c := range []struct{ do, want string }{
		{`{"open": {"playbook": "none"}}`, "do.open.playbook: No playbook that you can read is named none."},
		{`{"open": {"playbook": "secret"}}`, "do.open.playbook: No playbook that you can read is named secret."},
		{`{"open": {"playbook": ""}}`, "do.open.playbook: Name the playbook"},
	} {
		_, out := f.send(t, http.MethodPut, "/v1/duties/x?dryRun=true", "alice", duty(c.do))
		if got := problemsOf(t, out); !strings.Contains(got, c.want) {
			t.Errorf("problems of %s = %s, want %s", c.do, got, c.want)
		}
	}
}

// A duty that asks the manager puts its message on the queue of the
// manager as a run of the duty: the run of the start, with the metadata,
// the budget, and the principal of the duty. Without a manager, the write
// has a problem.
func TestAskTheManager(t *testing.T) {
	f := newFront(t, true)
	if status, v := f.send(t, http.MethodPut, "/v1/duties/boss", "alice", duty(`{"manager": {"message": "Look at the fleet on {date}."}}`)); status != http.StatusCreated {
		t.Fatalf("the duty = %d %v", status, v)
	}
	status, v := f.send(t, http.MethodPost, "/v1/duties/boss/run", "alice", "")
	if status != http.StatusAccepted || v["run"] == "" {
		t.Fatalf("run now = %d %v", status, v)
	}
	reqs := f.queued("manager", 1)
	today := time.Now().UTC().Format(time.DateOnly)
	if len(reqs) != 1 {
		t.Fatalf("the manager got %d requests", len(reqs))
	}
	r := reqs[0]
	if r.ID != v["run"] || r.Input != "From the duty boss.\n\nLook at the fleet on "+today+"." || r.Principal != "duty:boss" ||
		r.Metadata[duties.MetaDuty] != "boss" || r.Metadata[fw.MetaBatch] != duties.BatchOf("boss") || r.Budget == nil || r.Budget.MaxCostUSD != 0.25 {
		t.Fatalf("the request = %+v", r)
	}
	if start := f.lastStart(t, "boss"); start["run"] != v["run"] || start["state"] != duties.StateQueued {
		t.Fatalf("the start = %v", start)
	}
	t.Setenv(manager.EnvManager, "")
	_, out := f.send(t, http.MethodPut, "/v1/duties/x?dryRun=true", "alice", duty(`{"manager": {"message": "x"}}`))
	if got := problemsOf(t, out); !strings.Contains(got, "do.manager: This front names no manager.") {
		t.Fatalf("problems without a manager = %s", got)
	}
}

// The actions are on the list of what the front offers.
func TestOffered(t *testing.T) {
	_, actions, _ := duties.Offered()
	if strings.Join(actions, ",") != "ask,manager,open,post" {
		t.Fatalf("actions = %v", actions)
	}
}
