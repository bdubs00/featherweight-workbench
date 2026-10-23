package looks

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/surface/native"
	"github.com/bdubs00/featherweight-workbench/duties"
	"github.com/bdubs00/featherweight-workbench/fleet"
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

// queueTool is the tool queue of the agent triage: it answers its text and
// records the input of each call.
type queueTool struct {
	mu     sync.Mutex
	answer fw.ToolResult
	inputs []string
}

func (q *queueTool) tool() fw.Tool {
	return fw.NewTool(fw.ToolSpec{Name: "queue", ReadOnly: true}, func(_ context.Context, in json.RawMessage) (fw.ToolResult, error) {
		q.mu.Lock()
		defer q.mu.Unlock()
		q.inputs = append(q.inputs, string(in))
		return q.answer, nil
	})
}

// front is one process that serves the duties and takes the queue of the
// agent triage itself, so a check run of a duty goes through the queue to
// its own kernel.
type front struct {
	a     *fw.Agent
	url   string
	queue *queueTool
}

func newFront(t *testing.T) *front {
	t.Helper()
	t.Setenv(fleet.EnvIntake, "fw.jobs")
	t.Setenv("FW_ATESPACE", "")
	cfg := fw.DefaultConfig()
	cfg.Home = t.TempDir()
	cfg.Access.Default = "closed"
	cfg.Bus.Intake = "fw.jobs.triage"
	cfg.Agents = map[string]fw.AgentProfile{
		"triage": {Access: &fw.Access{Owners: []string{"alice"}}, Policy: &fw.Rules{Ask: []string{"danger"}}},
	}
	q := &queueTool{answer: fw.TextResult(`{"result": {"records": [{"number": "INC1", "priority": 1}, {"number": "INC2", "priority": 3}]}}`)}
	danger := fw.NewTool(fw.ToolSpec{Name: "danger"}, func(context.Context, json.RawMessage) (fw.ToolResult, error) {
		return fw.TextResult(`{"items": []}`), nil
	})
	a, err := fw.New(fw.WithConfig(cfg), fw.WithoutDefaults(), fw.WithAuthn(people{}),
		fw.WithModel(fw.ModelFunc{GenerateFn: func(context.Context, *fw.Request) (*fw.Response, error) {
			return nil, errors.New("a check calls no model")
		}}),
		fw.WithTools(q.tool(), danger),
		fw.WithLedger(fw.NewMemLedger()), fw.WithBus(fw.NewMemBus()), fw.WithSurfaces(native.New()),
		fw.WithServices(duties.Start), fw.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
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
	if duties.Of(a) == nil {
		t.Fatal("the duties are off")
	}
	return &front{a: a, url: srv.URL, queue: q}
}

// dryTest sends a dry run with a test of the check of a duty as alice.
func (f *front) dryTest(t *testing.T, name, check string) (int, map[string]any) {
	t.Helper()
	body := `{"agent": "triage", "when": {"say": "every 15 minutes"}, "checkFirst": ` + check + `,
		"do": {"ask": {"message": "Triage {found}"}}, "limits": {"budget": {"maxTurns": 5}}}`
	req, _ := http.NewRequest(http.MethodPut, f.url+"/v1/duties/"+name+"?dryRun=true&test=true", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer alice")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	b, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("decode %s: %v", b, err)
	}
	return resp.StatusCode, out
}

// A test of a check with a tool of the agent puts a check run on the
// queue of the agent: its kernel calls the tool with the input of the
// check as the principal of the duty, the run carries the mark of the
// front and no batch, and the check finds its items in the answer.
func TestLookThroughTheToolOfTheAgent(t *testing.T) {
	f := newFront(t)
	status, out := f.dryTest(t, "incidents", `{"look": {"tool": "queue", "input": {"query": "priority<=2"}}, "items": "result.records", "key": "number", "where": [{"field": "priority", "oneOf": [1, 2]}]}`)
	check, _ := out["check"].(map[string]any)
	if status != http.StatusOK || check == nil || check["count"] != float64(1) || check["wakes"] != true {
		t.Fatalf("the dry run = %d %v", status, out)
	}
	if found := check["found"].([]any); len(found) != 1 || found[0].(map[string]any)["number"] != "INC1" {
		t.Fatalf("found = %v", check["found"])
	}
	f.queue.mu.Lock()
	inputs := append([]string(nil), f.queue.inputs...)
	f.queue.mu.Unlock()
	var in map[string]string
	if len(inputs) != 1 || json.Unmarshal([]byte(inputs[0]), &in) != nil || in["query"] != "priority<=2" {
		t.Fatalf("the tool got %q", inputs)
	}
	runs, err := f.a.List(context.Background())
	if err != nil || len(runs) != 1 {
		t.Fatalf("runs = %v, %v", runs, err)
	}
	r := runs[0]
	if r.Kind() != fw.RunKindCheck || r.Principal != "duty:incidents" || r.Metadata[fleet.MetaInternal] != "duty-check:incidents" ||
		r.Metadata[fw.MetaBatch] != "" || r.Metadata[duties.MetaDuty] != "" || fleet.Listed(r) {
		t.Fatalf("the check run = %+v", r)
	}
}

// A tool that the policy asks for fails the check with the reason, and so
// does an error result of the tool.
func TestLookFailsWithTheReason(t *testing.T) {
	f := newFront(t)
	status, out := f.dryTest(t, "danger", `{"look": {"tool": "danger"}, "items": "items", "wake": "any"}`)
	if status != http.StatusBadGateway || !strings.Contains(out["detail"].(string), "the check run") {
		t.Fatalf("a tool that asks first = %d %v", status, out)
	}
	f.queue.mu.Lock()
	f.queue.answer = fw.ErrorResult("the queue is down")
	f.queue.mu.Unlock()
	status, out = f.dryTest(t, "down", `{"look": {"tool": "queue"}, "items": "result.records", "wake": "any"}`)
	if status != http.StatusBadGateway || !strings.Contains(out["detail"].(string), "the tool queue answered an error: the queue is down") {
		t.Fatalf("an error result = %d %v", status, out)
	}
}

// The document of a check is the output of its step. A JSON text in a
// string reads as its JSON once, an error result and a cut output are
// errors, and an output that is not a check output is an error.
func TestDocument(t *testing.T) {
	for _, c := range []struct{ output, want, err string }{
		{`{"results":[{"tool":"q","output":{"a":[1]},"isError":false}]}`, `{"a":[1]}`, ""},
		{`{"results":[{"tool":"q","output":"{\"a\": [2]}","isError":false}]}`, `{"a": [2]}`, ""},
		{`{"results":[{"tool":"q","output":"plain words","isError":false}]}`, `"plain words"`, ""},
		{`{"results":[{"tool":"q","output":"no access","isError":true}]}`, "", "the tool q answered an error: no access"},
		{`{"results":[{"tool":"q","output":"{\"a\":","isError":false,"cut":true}]}`, "", "longer than 1024 KB"},
		{`{"results":[]}`, "", "no result"},
		{`done`, "", "not the output of a check"},
	} {
		doc, err := Document("q", c.output)
		switch {
		case c.err != "" && (err == nil || !strings.Contains(err.Error(), c.err)):
			t.Errorf("Document(%s) = %v, want %q", c.output, err, c.err)
		case c.err == "" && (err != nil || string(doc) != c.want):
			t.Errorf("Document(%s) = %s, %v, want %s", c.output, doc, err, c.want)
		}
	}
}

// A run that does not end in time, or that waits for a person, is
// canceled, and a failed run gives its error. The queue that does not
// take the run fails the look.
func TestLookOfARunThatDoesNotEnd(t *testing.T) {
	d := &duties.Duty{Name: "q", Agent: "triage", CheckFirst: &duties.CheckFirst{Look: duties.Look{Tool: "queue"}}}
	var canceled []string
	run := &fw.Run{State: fw.StateRunning}
	l := &looker{
		enqueue: func(context.Context, *fw.Agent, string, string, []fw.RunRequest) (string, int, error) {
			return "fw.jobs.triage", 1, nil
		},
		await: func(ctx context.Context, _ *fw.Agent, id string) (*fw.Run, error) {
			if run.State == fw.StateRunning {
				<-ctx.Done()
				return nil, ctx.Err()
			}
			r := *run
			r.ID = id
			return &r, nil
		},
		cancel: func(_ context.Context, _ *fw.Agent, id string) error { canceled = append(canceled, id); return nil },
		wait:   50 * time.Millisecond,
	}
	if _, err := l.look(context.Background(), nil, d); err == nil || !strings.Contains(err.Error(), "did not end within 50ms") || len(canceled) != 1 {
		t.Fatalf("a run that does not end = %v, canceled %v", err, canceled)
	}
	run.State = fw.StateInputRequired
	if _, err := l.look(context.Background(), nil, d); err == nil || !strings.Contains(err.Error(), "waits for a person") || len(canceled) != 2 {
		t.Fatalf("a run that asks = %v, canceled %v", err, canceled)
	}
	run.State, run.Error = fw.StateFailed, "the agent has no tool of this name"
	if _, err := l.look(context.Background(), nil, d); err == nil || !strings.Contains(err.Error(), "failed: the agent has no tool of this name") {
		t.Fatalf("a failed run = %v", err)
	}
	l.enqueue = func(context.Context, *fw.Agent, string, string, []fw.RunRequest) (string, int, error) {
		return "", 0, errors.New("fleet: workbench.fleet.intake is empty")
	}
	if _, err := l.look(context.Background(), nil, d); err == nil || !strings.Contains(err.Error(), "did not take the check: workbench.fleet.intake is empty") {
		t.Fatalf("a queue that fails = %v", err)
	}
}

// A duty that runs as its owner checks as its owner too.
func TestRequestActsAsTheOwner(t *testing.T) {
	d := &duties.Duty{Name: "q", Agent: "triage", Owner: "alice", RunAs: duties.RunAsOwner,
		CheckFirst: &duties.CheckFirst{Look: duties.Look{Tool: "queue", Input: map[string]any{"n": 1}}}}
	req, err := Request(d)
	if err != nil || req.Principal != "alice" || req.PrincipalKind != fw.KindUser || string(req.Check.Steps[0].Input) != `{"n":1}` ||
		req.Check.Timeout != duties.CheckTimeout || req.Check.MaxBytes != duties.MaxCheckBytes || req.Input != "" {
		t.Fatalf("the request = %+v, %v", req, err)
	}
}
