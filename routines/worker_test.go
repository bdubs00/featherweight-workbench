package routines

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/fleet"
	"github.com/bdubs00/featherweight-workbench/pools"
)

// sureDecider answers every question of a routine with one answer: bug
// at 0.92 for a choice, and no at 0.02 for a yes or no question.
type sureDecider struct{}

func (sureDecider) Decide(_ context.Context, _ string, qs map[string]fw.Question) (fw.DecideResult, error) {
	out := fw.DecideResult{Model: "clef", Answers: map[string]fw.Answer{}}
	for id, q := range qs {
		if q.Type == fw.QuestionNoul {
			out.Answers[id] = fw.Answer{Type: q.Type, Probability: 0.02}
			continue
		}
		out.Answers[id] = fw.Answer{Type: q.Type, Choice: "bug", Probability: 0.92, Probabilities: map[string]float64{"bug": 0.92, "access": 0.05, "other": 0.03}}
	}
	return out, nil
}

// A worker kernel runs the routines that the store gives its profile: the
// rehearsing routine decides beside the agent, the kernel compares its
// answer and its calls with what the model did, and the events reach the
// evidence of the front over the egress stream.
func TestWorkerRehearsalReachesTheEvidence(t *testing.T) {
	fr := newFront(t, withClef)
	ctx := context.Background()
	if _, _, err := fr.s.Put(ctx, PutRequest{Agent: "triage", Routine: ack(), Standing: Rehearsing, By: "alice"}); err != nil {
		t.Fatal(err)
	}
	reach, err := fr.s.Reaching(ctx)
	if err != nil || len(reach["triage"]) != 1 || reach["triage"][0].Standing != fw.RoutineShadow {
		t.Fatalf("Reaching = %+v, %v", reach, err)
	}
	cfg := fw.DefaultConfig()
	cfg.Home = t.TempDir()
	cfg.Node = alpha
	cfg.Bus.Egress = "fw.events"
	cfg.Bus.EgressTypes = append(slices.Clone(fleet.EgressTypes), pools.RoutineEventTypes...)
	withClef(cfg)
	cfg.Agents = map[string]fw.AgentProfile{"triage": {Routines: reach["triage"]}}
	// The model makes the calls of the routine, then answers.
	model := fw.ModelFunc{GenerateFn: func(_ context.Context, req *fw.Request) (*fw.Response, error) {
		call := func(id, tool, input string) (*fw.Response, error) {
			return &fw.Response{Content: []fw.Block{{Type: fw.BlockToolUse, ID: id, Name: tool, Input: json.RawMessage(input)}}, StopReason: fw.StopToolUse}, nil
		}
		switch len(req.Messages) {
		case 1:
			return call("c1", "gitea.label_read", `{"method": "list", "owner": "lab", "repo": "helpdesk"}`)
		case 3:
			return call("c2", "gitea.issue_write", `{"method": "add_labels", "issue_number": 42}`)
		}
		return &fw.Response{Content: []fw.Block{fw.Text(`TRIAGE {"type": "type/bug"}`)}, StopReason: fw.StopEndTurn}, nil
	}}
	tool := func(name, text string) fw.Tool {
		return fw.NewTool(fw.ToolSpec{Name: name}, func(context.Context, json.RawMessage) (fw.ToolResult, error) { return fw.TextResult(text), nil })
	}
	tools := []fw.Tool{tool("gitea.label_read", `[{"id":11,"name":"type/bug"}]`), tool("gitea.issue_write", "ok"), tool("gitea.issue_read", "status/triaged")}
	worker, err := fw.New(fw.WithConfig(cfg), fw.WithoutDefaults(), fw.WithAuthn(fw.NoAuthn{}), fw.WithModel(model), fw.WithTools(tools...),
		fw.WithLedger(fw.NewMemLedger()), fw.WithBus(fr.bus), fw.WithLogger(quiet()), fw.WithDecider("clef", sureDecider{}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		worker.Close(cctx)
	})
	r, err := worker.Start(ctx, "#42 The printer on floor 3 shows an error\nIt started this morning.", fw.AsAgent("triage"))
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		got, err := worker.Get(ctx, r.ID)
		if err == nil && got.State.Terminal() {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the run did not finish: %+v, %v", got, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	var types []string
	for e, err := range worker.Ledger().Read(ctx, r.ID, 0) {
		if err == nil && strings.HasPrefix(e.Type, "fw.routine.") {
			types = append(types, e.Type+" "+string(e.Data))
		}
	}
	if len(types) != 3 || !strings.HasPrefix(types[0], fw.EvRoutineDecided) || !strings.HasPrefix(types[1], fw.EvRoutineCompared) ||
		!strings.Contains(types[2], `"calls":true`) || !strings.Contains(types[2], `"agree":true`) {
		t.Fatalf("the worker recorded %v", types)
	}
	ev := waitEvidence(t, fr.s, "triage", "acknowledge", func(ev *Evidence) bool { return ev.Totals.Agreed == 1 })
	if ev.Totals != (Counts{Decided: 1, Acted: 1, Agreed: 1}) {
		t.Fatalf("evidence = %+v, the worker recorded %v", ev.Totals, types)
	}
	if b := ev.Questions["type"]; len(b) != 5 || b[3].Runs != 1 || b[3].Agreed != 1 {
		t.Fatalf("the reliability of type = %+v", b)
	}
}
