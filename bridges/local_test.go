package bridges

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/bdubs00/featherweight-kernel/fw"
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
