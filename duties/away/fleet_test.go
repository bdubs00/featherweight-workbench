package away

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/duties"
	"github.com/bdubs00/featherweight-workbench/internal/bucket"
)

// script is a model that answers with its responses in order.
type script struct {
	mu    sync.Mutex
	resps []*fw.Response
}

func (s *script) model() fw.Model {
	return fw.ModelFunc{StreamFn: func(context.Context, *fw.Request, func(fw.Delta)) (*fw.Response, error) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if len(s.resps) == 0 {
			return &fw.Response{Content: []fw.Block{fw.Text("done")}, StopReason: fw.StopEndTurn}, nil
		}
		r := s.resps[0]
		s.resps = s.resps[1:]
		return r, nil
	}}
}

func toolUse(id, name, input string) *fw.Response {
	return &fw.Response{Content: []fw.Block{{Type: fw.BlockToolUse, ID: id, Name: name, Input: json.RawMessage(input)}}, StopReason: fw.StopToolUse}
}

// waitFor waits until the run is in the state.
func waitFor(t *testing.T, a *fw.Agent, id string, state fw.RunState) *fw.Run {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		r, err := a.Get(context.Background(), id)
		if err == nil && r.State == state {
			return r
		}
		if time.Now().After(deadline) {
			t.Fatalf("the run %s did not reach %s: %+v, %v", id, state, r, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// The seams of the fleet work on a run of this process: the watch finds
// the run of a duty that waits, sees that the policy marks its tool
// destructive, decides it with the kernel, and reads the person who
// decided another one from the ledger.
func TestSeamsOnALocalRun(t *testing.T) {
	deploy := fw.NewTool(fw.ToolSpec{Name: "deploy", Description: "Deploy", InputSchema: json.RawMessage(`{"type":"object"}`), Destructive: true},
		func(context.Context, json.RawMessage) (fw.ToolResult, error) { return fw.TextResult("deployed"), nil })
	sc := &script{resps: []*fw.Response{toolUse("c1", "deploy", `{}`), toolUse("c2", "deploy", `{}`), toolUse("c3", "deploy", `{}`)}}
	cfg := fw.DefaultConfig()
	cfg.Home = t.TempDir()
	cfg.Auth.Mode = "none"
	a, err := fw.New(fw.WithConfig(cfg), fw.WithoutDefaults(), fw.WithModel(sc.model()), fw.WithTools(deploy),
		fw.WithPolicy(fw.NewRulesPolicy(fw.DefaultRules())), fw.WithLedger(fw.NewMemLedger()), fw.WithBus(fw.NewMemBus()), fw.WithLogger(quiet()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		a.Close(ctx)
	})
	ctx := context.Background()
	run, err := a.Start(ctx, "Deploy it.", fw.WithMetadata(map[string]string{duties.MetaDuty: "report"}))
	if err != nil {
		t.Fatal(err)
	}
	r := waitFor(t, a, run.ID, fw.StateInputRequired)
	s := newService(a, bucket.NewMem(awayBucket), quiet())
	runs, err := s.waiting(ctx)
	if err != nil || len(runs) != 1 || runs[0].ID != run.ID || runs[0].Pending == nil {
		t.Fatalf("waiting = %+v, %v", runs, err)
	}
	if !s.destructive(ctx, r) {
		t.Fatalf("the destructive tool %+v is not destructive", r.Pending)
	}
	if _, _, ok := s.decider(ctx, r, r.Pending.ID); ok {
		t.Fatal("an approval that waits has a decider")
	}
	if err := s.decide(ctx, r, r.Pending.ID, "deny", "Nobody decided."); err != nil {
		t.Fatal(err)
	}
	if err := s.decide(ctx, r, r.Pending.ID, "deny", "Again."); !errors.Is(err, errGone) {
		t.Fatalf("a second decision = %v", err)
	}
	r2 := waitFor(t, a, run.ID, fw.StateInputRequired)
	if r2.Pending == nil || r2.Pending.ID == r.Pending.ID {
		t.Fatalf("the second approval = %+v", r2.Pending)
	}
	if err := a.Decide(ctx, run.ID, fw.ApprovalDecision{ApprovalID: r2.Pending.ID, Decision: "allow", Decider: "alice", DeciderKind: fw.ActorPerson}); err != nil {
		t.Fatal(err)
	}
	if by, verdict, ok := s.decider(ctx, r2, r2.Pending.ID); !ok || by != "alice" || verdict != "allow" {
		t.Fatalf("decider = %q %q %v", by, verdict, ok)
	}
	// A person with the static token is a person too. The kernel is not.
	r3 := waitFor(t, a, run.ID, fw.StateInputRequired)
	if r3.Pending == nil || r3.Pending.ID == r2.Pending.ID {
		t.Fatalf("the third approval = %+v", r3.Pending)
	}
	if err := a.Decide(ctx, run.ID, fw.ApprovalDecision{ApprovalID: r3.Pending.ID, Decision: "deny", Decider: "token", DeciderKind: fw.KindToken}); err != nil {
		t.Fatal(err)
	}
	if by, verdict, ok := s.decider(ctx, r3, r3.Pending.ID); !ok || by != "token" || verdict != "deny" {
		t.Fatalf("the decider with the static token = %q %q %v", by, verdict, ok)
	}
	got, err := s.run(ctx, run.ID)
	if err != nil || got.ID != run.ID {
		t.Fatalf("run = %+v, %v", got, err)
	}
	if _, err := s.run(ctx, "NORUN"); !errors.Is(err, fw.ErrRunNotFound) {
		t.Fatalf("an unknown run = %v", err)
	}
	if err := s.enqueue(ctx, "manager", fw.RunRequest{ID: fw.NewID(), Input: "x"}); err == nil {
		t.Fatal("a question to a manager went out without a queue")
	}
}
