package duties

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/fleet"
)

// The options name the secrets of the rules of the operator with their
// hosts and never a value, and what the front serves now.
func TestOptions(t *testing.T) {
	f := newFront(t, checkBlock, nil)
	f.sec["SNOW_KEY"] = "s3cret"
	resp, b := send(t, http.MethodGet, f.url+"/v1/duties/options", "carol", "")
	var o OptionsAnswer
	decodeJSON(t, b, &o)
	if resp.StatusCode != http.StatusOK || len(o.Secrets) != 2 || o.Secrets[0].Name != "SNOW_KEY" || o.Secrets[0].Hosts[0] != "127.0.0.1" ||
		o.Secrets[1].Name != "HOOK_TOKEN" || o.Secrets[1].Hosts == nil || strings.Join(o.Actions, ",") != "ask" || strings.Join(o.Looks, ",") != "http" || o.Events == nil {
		t.Fatalf("options = %d %s", resp.StatusCode, b)
	}
	if strings.Contains(b, "s3cret") {
		t.Fatalf("the options hold a secret: %s", b)
	}
	if resp, _ := send(t, http.MethodGet, f.url+"/v1/duties/options", "", ""); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("options without a credential = %d", resp.StatusCode)
	}
	if err := CheckName("options"); err == nil {
		t.Fatal("a duty can take the name of the route of the options")
	}
}

// withBudget gives the agent triage a budget.
func withBudget(cfg *fw.Config) {
	p := cfg.Agents["triage"]
	p.Budget = &fw.Budget{MaxCostUSD: 0.2, MaxTurns: 8, Deadline: 10 * time.Minute}
	p.Tools = []string{"servicenow.query_incidents"}
	cfg.Agents["triage"] = p
}

// answering makes the agent of each run that the front waits for answer
// with the text, or end in the state.
func (f *front) answering(state fw.RunState, output string) {
	f.s.await = func(_ context.Context, id string) (*fw.Run, error) {
		return &fw.Run{ID: id, State: state, Output: output, Error: "the model failed"}, nil
	}
}

// The agent of a new duty writes its check from words: one run of the
// agent with its budget, as the caller, with the instructions of the
// record, hidden from the lists. The front reads the answer, tests the
// check, and answers the record, its readback, and what it finds now.
func TestWriteCheck(t *testing.T) {
	f := newFront(t, checkBlock, withBudget)
	f.sec["SNOW_KEY"] = "s3cret"
	q := &incidents{}
	q.set(0, inc("INC1", 1), inc("INC2", 4))
	srv := httptest.NewServer(q)
	t.Cleanup(srv.Close)
	answer := "```json\n" + `{"look": {"http": {"url": "` + srv.URL + `/api/queue", "secret": "SNOW_KEY", "header": "X-Key"}}, "items": "result.records", "key": "number", "where": [{"field": "priority", "oneOf": [1, 2]}]}` + "\n```"
	f.answering(fw.StateCompleted, answer)
	resp, b := send(t, http.MethodPost, f.url+"/v1/duties/incidents/write-check", "alice", `{"words": "a new P1 or P2 in the queue", "agent": "triage"}`)
	var ans WriteCheckAnswer
	decodeJSON(t, b, &ans)
	if resp.StatusCode != http.StatusOK || ans.CheckFirst == nil || ans.CheckFirst.Wake.When != WakeNew || ans.Check == nil || ans.Check.Count != 1 || !ans.Check.Wakes ||
		!strings.HasPrefix(ans.Readback, "First it looks at 127.0.0.1:") || !strings.Contains(ans.Readback, "Only items that it did not see before wake triage, by their number.") || ans.Run == "" {
		t.Fatalf("write-check = %d %s", resp.StatusCode, b)
	}
	req := f.queue.last(t)
	if req.ID != ans.Run || req.Agent != "triage" || !strings.Contains(req.Input, "a new P1 or P2 in the queue") || req.Principal != "alice" ||
		req.Budget == nil || req.Budget.MaxCostUSD != 0.2 || req.Budget.Deadline != WriteCheckTimeout || req.Metadata[fleet.MetaInternal] != "duty-write-check:incidents" ||
		req.Metadata[fw.MetaBatch] != "" || req.Metadata[MetaDuty] != "" {
		t.Fatalf("the request = %+v", req)
	}
	for _, want := range []string{"The looks that this front offers now: http.", "- SNOW_KEY: 127.0.0.1", "Your profile names these tools: servicenow.query_incidents.", `"wake": {"when": "new"}`} {
		if !strings.Contains(req.Instructions, want) {
			t.Errorf("the instructions lack %q:\n%s", want, req.Instructions)
		}
	}
	if strings.Contains(req.Instructions, "s3cret") {
		t.Fatal("the instructions hold a secret")
	}
	if resp, _ := send(t, http.MethodGet, f.url+"/v1/duties/incidents", "alice", ""); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("write-check saved the duty: %d", resp.StatusCode)
	}
}

// For a saved duty, the agent of the duty writes the check, whatever the
// body names. The tool duty_write_check calls the same function.
func TestWriteCheckOfASavedDuty(t *testing.T) {
	f := newFront(t, checkBlock, withBudget)
	f.put(t, "alice", daily("report"))
	f.answering(fw.StateCompleted, `{"look": {"tool": "servicenow.query_incidents", "input": {"query": "priority<=2"}}, "items": "result.records", "wake": "any"}`)
	resp, b := send(t, http.MethodPost, f.url+"/v1/duties/report/write-check", "alice", `{"words": "any open incident", "agent": "private"}`)
	var ans WriteCheckAnswer
	decodeJSON(t, b, &ans)
	if resp.StatusCode != http.StatusOK || f.queue.last(t).Agent != "triage" || ans.Check != nil || ans.CheckFirst.Look.Tool != "servicenow.query_incidents" ||
		ans.Readback != "First it calls the tool servicenow.query_incidents for the items at result.records. Any item wakes triage." {
		t.Fatalf("write-check of a saved duty = %d %s", resp.StatusCode, b)
	}
	cs := connectMCP(t, f.a, "alice")
	res := call(t, cs, ToolWriteCheck, map[string]any{"name": "report", "words": "any open incident"})
	if res.IsError || !strings.Contains(textOf(res), "First it calls the tool servicenow.query_incidents") || !strings.Contains(textOf(res), "did not test the check") {
		t.Fatalf("duty_write_check = %s", textOf(res))
	}
	f.answering(fw.StateCompleted, "I think you want a check of the queue.")
	if res := call(t, cs, ToolWriteCheck, map[string]any{"name": "report", "words": "any open incident"}); !res.IsError || !strings.Contains(textOf(res), "Unprocessable: The answer of the agent triage is not one JSON object of a check") {
		t.Fatalf("duty_write_check of words = %s", textOf(res))
	}
}

// An answer that is no check, a check that does not fit, a failed run, a
// run that asks a person, and an agent without a budget get 422 with the
// reason in error. A run that does not answer in time is canceled and
// gets 504. A new duty needs its agent, a viewer of the agent gets 403, a
// person who cannot view it gets 404, and a body without words gets 400.
func TestWriteCheckRefusals(t *testing.T) {
	f := newFront(t, checkBlock, withBudget)
	url := f.url + "/v1/duties/incidents/write-check"
	body := `{"words": "new incidents", "agent": "triage"}`
	for _, c := range []struct {
		state  fw.RunState
		output string
		want   string
	}{
		{fw.StateCompleted, `{"look": {"tool": "q"}, "items": "result.records", "key": "number", "color": "red"}`, `unknown field \"color\"`},
		{fw.StateCompleted, `{"look": {"tool": "q"}} {"more": 1}`, "holds more than the JSON object"},
		{fw.StateCompleted, `{"look": {"tool": "q"}, "wake": "new"}`, "checkFirst.key: A key is required with wake new"},
		{fw.StateCompleted, `{"look": {"http": {"url": "https://example.com/q", "secret": "OTHER"}}, "key": "id"}`, "OTHER is not in workbench.duties.secrets"},
		{fw.StateFailed, "", "ended failed and wrote no check: the model failed."},
		{fw.StateInputRequired, "", "asked a person to allow a tool"},
	} {
		f.answering(c.state, c.output)
		resp, b := send(t, http.MethodPost, url, "alice", body)
		if resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(b, `"error":`) || !strings.Contains(b, c.want) {
			t.Errorf("an answer %q in the state %s = %d %s", c.output, c.state, resp.StatusCode, b)
		}
	}
	var canceled []string
	f.s.stopRun = func(_ context.Context, id string) error { canceled = append(canceled, id); return nil }
	f.s.writeWait = 50 * time.Millisecond
	f.s.await = func(ctx context.Context, _ string) (*fw.Run, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	resp, b := send(t, http.MethodPost, url, "alice", body)
	if resp.StatusCode != http.StatusGatewayTimeout || !strings.Contains(b, "did not answer within") || len(canceled) != 1 || canceled[0] != f.queue.last(t).ID {
		t.Fatalf("a run without an answer = %d %s, canceled %v", resp.StatusCode, b, canceled)
	}
	for _, c := range []struct {
		who, body string
		status    int
		want      string
	}{
		{"alice", `{"words": "x"}`, http.StatusBadRequest, "agent: Name the agent"},
		{"alice", `{"words": " ", "agent": "triage"}`, http.StatusBadRequest, "words: Say in words"},
		{"carol", body, http.StatusForbidden, "needs an owner entry on the agent triage"},
		{"dave", body, http.StatusNotFound, "No agent that you can view is named triage"},
		{"dave", `{"words": "x", "agent": "private"}`, http.StatusUnprocessableEntity, "The agent private has no budget"},
	} {
		resp, b := send(t, http.MethodPost, url, c.who, c.body)
		if resp.StatusCode != c.status || !strings.Contains(b, c.want) {
			t.Errorf("%s %s = %d %s", c.who, c.body, resp.StatusCode, b)
		}
	}
}
