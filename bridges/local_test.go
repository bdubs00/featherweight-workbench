package bridges

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/fleet"
)

// A seat whose run lives in the front itself takes its follow-ups and its
// reactions through the kernel: the message names its sender, and a thumb
// on the answer is a quality receipt.
func TestLocalSeat(t *testing.T) {
	a := newAgent(t, nil)
	s := newStore(t, a, nil)
	url := serve(t, a)
	it := open(t, url)
	ctx := context.Background()
	r, err := a.Start(ctx, "Migrate the logrotate module", fw.AsAgent("migrator"), fw.WithMetadata(map[string]string{fw.MetaBatch: BatchOf(it.ID), MetaMember: "migrator-1"}))
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "the local run completes", func() bool {
		got, err := a.Get(ctx, r.ID)
		return err == nil && got.State == fw.StateCompleted
	})
	s.poll(ctx)
	var answer Message
	for _, m := range channel(t, s, it.ID) {
		if m.From.Kind == KindAgent {
			answer = m
		}
	}
	if answer.Text != "done" || answer.Mirrors == nil || answer.Mirrors.Type != fw.EvRunFinished {
		t.Fatalf("answer = %+v", answer)
	}
	if resp, body := do(t, http.MethodPost, fmt.Sprintf("%s/v1/bridges/%s/messages/%d/reactions", url, it.ID, answer.Seq), "bob", `{"reaction":"up"}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("react = %d %s", resp.StatusCode, body)
	}
	eventually(t, "the thumb reaches the run", func() bool {
		got, _ := a.Get(ctx, r.ID)
		return got != nil && got.Reactions[fw.ReactionUp] == 1
	})
	do(t, http.MethodPost, url+"/v1/bridges/"+it.ID+"/messages", "alice", `{"text":"One more thing.","to":["migrator-1"]}`)
	var from *fw.MessageFrom
	eventually(t, "the follow-up reaches the run", func() bool {
		for e, err := range a.Ledger().Read(ctx, r.ID, 0) {
			if err != nil {
				return false
			}
			var d fw.MessageReceivedData
			if e.Type == fw.EvMessageReceived && e.Decode(&d) == nil && d.From != nil {
				from = d.From
				return true
			}
		}
		return false
	})
	if from.Kind != fw.FromPerson || from.Name != "alice" || from.Member != "migrator-1" {
		t.Fatalf("from = %+v", from)
	}
	var receipt bool
	for e, err := range a.Ledger().Read(ctx, r.ID, 0) {
		if err != nil {
			t.Fatal(err)
		}
		var d fw.ReactionData
		if e.Type == fw.EvRunReacted && e.Decode(&d) == nil {
			receipt = d.Receipt && d.By == "bob"
		}
	}
	if !receipt {
		t.Fatal("the thumb on the answer recorded no quality receipt")
	}
}

// The roster words the activity of a seat whose run lives in the front
// itself with fleet.ActivityPhrase, as it words a run of a worker, in the
// route and in the hook. No view holds the input of a call of such a run,
// so the phrase of a tool call names the tool alone.
func TestLocalSeatActivity(t *testing.T) {
	h := hooked(t)
	release := make(chan struct{})
	var once sync.Once
	done := func() { once.Do(func() { close(release) }) }
	t.Cleanup(done)
	var calls atomic.Int32
	model := fw.ModelFunc{StreamFn: func(context.Context, *fw.Request, func(fw.Delta)) (*fw.Response, error) {
		if calls.Add(1) == 1 {
			use := fw.Block{Type: fw.BlockToolUse, ID: "c1", Name: "bash", Input: json.RawMessage(`{"command":"go test ./..."}`)}
			return &fw.Response{Content: []fw.Block{use}, StopReason: fw.StopToolUse}, nil
		}
		return &fw.Response{Content: []fw.Block{fw.Text("done")}, StopReason: fw.StopEndTurn}, nil
	}}
	bash := fw.NewTool(fw.ToolSpec{Name: "bash", InputSchema: json.RawMessage(`{"type":"object"}`)}, func(ctx context.Context, _ json.RawMessage) (fw.ToolResult, error) {
		select {
		case <-release:
		case <-ctx.Done():
		}
		return fw.TextResult("ok"), nil
	})
	a := newAgentWith(t, nil, fw.WithModel(model), fw.WithTools(bash))
	s := newStore(t, a, nil)
	url := serve(t, a)
	it := open(t, url)
	ctx := context.Background()
	r, err := a.Start(ctx, "Migrate the logrotate module", fw.AsAgent("migrator"), fw.WithMetadata(map[string]string{fw.MetaBatch: BatchOf(it.ID), MetaMember: "migrator-1"}))
	if err != nil {
		t.Fatal(err)
	}
	var run *fw.Run
	eventually(t, "the local run waits for its tool call", func() bool {
		run, err = a.Get(ctx, r.ID)
		return err == nil && run.Activity != nil && run.Activity.CallID == "c1"
	})
	want := fleet.ActivityPhrase(run)
	if want != "bash" {
		t.Fatalf("fleet.ActivityPhrase of a run of the front = %q, want the tool alone", want)
	}
	s.poll(ctx)
	resp, body := do(t, http.MethodGet, url+"/v1/bridges/"+it.ID, "alice", "")
	var got Item
	decodeJSON(t, body, &got)
	if resp.StatusCode != http.StatusOK || len(got.Members) == 0 {
		t.Fatalf("bridge = %d %s", resp.StatusCode, body)
	}
	m := got.Members[0]
	if m.Handle != "migrator-1" || m.Presence != PresenceWorking || m.Activity == nil || m.Activity.Tool != "bash" || m.Activity.CallID != "c1" || m.Activity.Phrase != want {
		t.Fatalf("member = %+v, activity %+v, want the phrase %q", m, m.Activity, want)
	}
	if ms := h.member("migrator-1"); len(ms) == 0 || ms[len(ms)-1].Activity == nil || ms[len(ms)-1].Activity.Phrase != want {
		t.Fatalf("member hooks = %+v", ms)
	}
	done()
	eventually(t, "the local run completes", func() bool {
		got, err := a.Get(ctx, r.ID)
		return err == nil && got.State == fw.StateCompleted
	})
}
