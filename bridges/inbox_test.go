package bridges

import (
	"context"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/fleet"
)

// Waiting lists the seats that need a person, as the count needsYou of a
// bridge counts them: a seat that asked a question, with its text, and a
// seat whose run waits for an approval. A caller who cannot list the
// bridge gets none.
func TestWaiting(t *testing.T) {
	a := newAgent(t, nil)
	s := newStore(t, a, nil)
	v, _ := withFleet(t, a)
	v.state("default/crew-0", fleet.MemberLive)
	url := serve(t, a)
	it := open(t, url)
	v.put(workerRun("r-mig", it.ID, "migrator-1", "migrator", fw.StateRunning))
	asked := time.Date(2026, 10, 23, 16, 0, 0, 0, time.UTC)
	rev := workerRun("r-rev", it.ID, "reviewer", "reviewer", fw.StateInputRequired)
	rev.Pending = &fw.Approval{ID: "A1", Tool: "git.push", RequestedAt: asked}
	v.put(rev)
	s.poll(context.Background())
	cs := connectMCP(t, a, "fleet")
	res := callTool(t, cs, ToolPost, map[string]any{"text": "Restart sshd now?", "ask": true}, metaOf("r-mig", "migrator", ""))
	if res.IsError {
		t.Fatalf("ask = %s", textOf(res))
	}
	alice := fw.WithPrincipal(context.Background(), &fw.Principal{Subject: "alice", Kind: fw.KindUser})
	got, err := s.Waiting(alice)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("waiting = %+v", got)
	}
	by := map[string]Waiting{}
	for _, w := range got {
		by[w.Handle] = w
	}
	if w := by["migrator-1"]; w.Bridge != it.ID || w.Title != "Migrate logrotate" || w.Question == 0 || w.Text != "Restart sshd now?" || w.Run != "r-mig" || w.At.IsZero() {
		t.Fatalf("the question = %+v", w)
	}
	if w := by["reviewer"]; w.Question != 0 || w.Run != "r-rev" || w.Agent != "reviewer" || !w.At.Equal(asked) {
		t.Fatalf("the approval = %+v", w)
	}
	dave := fw.WithPrincipal(context.Background(), &fw.Principal{Subject: "dave", Kind: fw.KindUser})
	if got, err := s.Waiting(dave); err != nil || len(got) != 0 {
		t.Fatalf("waiting for dave = %+v, %v", got, err)
	}
}
