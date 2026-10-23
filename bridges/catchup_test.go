package bridges

import (
	"context"
	"sync"
	"testing"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// caughtUp records what the OnCatchUp hooks get, by agent.
type caughtUp struct {
	mu  sync.Mutex
	got map[*fw.Agent][]Message
}

func catchUps(t *testing.T) *caughtUp {
	c := &caughtUp{got: map[*fw.Agent][]Message{}}
	OnCatchUp(func(a *fw.Agent, _ string, m Message) {
		c.mu.Lock()
		c.got[a] = append(c.got[a], m)
		c.mu.Unlock()
	})
	t.Cleanup(func() {
		hooks.mu.Lock()
		hooks.catchUp = nil
		hooks.mu.Unlock()
	})
	return c
}

func (c *caughtUp) of(a *fw.Agent) []Message {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]Message(nil), c.got[a]...)
}

// An entry that one replica writes reaches the streams of the other at the
// next read of its watch, also when the relay of the bus misses it, as the
// needs-you cards 39 and 51 of the homelab never reached the open channel
// of a page on the other replica. The replica that wrote an entry gives it
// to its streams itself and catches up nothing. A replica starts at the
// end of a channel, and an entry whose write did not land yet waits for
// the next read, up to catchUpHold.
func TestCatchUpGivesTheEntriesOfTheOtherReplica(t *testing.T) {
	c := catchUps(t)
	r := newReplicas(t)
	one, two := r.stores[0], r.stores[1]
	ctx := context.Background()
	r.at(0, "live")
	if len(c.of(one.a)) != 0 || len(c.of(two.a)) != 0 {
		t.Fatalf("a start gave history: %d and %d", len(c.of(one.a)), len(c.of(two.a)))
	}
	// The run of the seat waits: the first replica posts the card.
	mig := workerRun("r-mig", r.bridge, "migrator-1", "migrator", fw.StateInputRequired)
	mig.Pending = &fw.Approval{ID: "ap1", Tool: "bash", Input: []byte(`{"command":"systemctl restart sshd"}`), Reason: "rule: ask bash"}
	r.views[0].put(mig)
	one.poll(ctx)
	r.views[1].put(mig)
	two.poll(ctx)
	got := c.of(two.a)
	if len(got) != 1 || got[0].Event != EventNeedsYou || got[0].Decision == nil || got[0].Decision.ID != "ap1" {
		t.Fatalf("the other replica caught up %+v", got)
	}
	if mine := c.of(one.a); len(mine) != 0 {
		t.Fatalf("the replica that wrote the card caught up %+v", mine)
	}
	// A message of a person on the second replica reaches the first.
	if _, err := two.post(ctx, r.bridge, sender{subject: "alice"}, PostSpec{Text: "Go ahead."}); err != nil {
		t.Fatal(err)
	}
	one.poll(ctx)
	two.poll(ctx)
	if mine := c.of(one.a); len(mine) != 1 || mine[0].Text != "Go ahead." {
		t.Fatalf("the first replica caught up %+v", mine)
	}
	if again := c.of(two.a); len(again) != 1 {
		t.Fatalf("the second replica caught up its own message: %+v", again)
	}
	// A seq whose write did not land yet waits; past the hold it is a gap.
	if _, err := one.b.update(ctx, r.bridge, func(rec *record) error { rec.alloc(); return nil }); err != nil {
		t.Fatal(err)
	}
	if err := one.postLines(ctx, r.bridge, system(EventChanged, "after the gap")); err != nil {
		t.Fatal(err)
	}
	two.poll(ctx)
	if got := c.of(two.a); len(got) != 1 {
		t.Fatalf("the catch-up went past a seq that waits: %+v", got)
	}
	r.clock.set(r.clock.now().Add(catchUpHold))
	two.poll(ctx)
	if got := c.of(two.a); len(got) != 2 || got[1].Text != "after the gap" {
		t.Fatalf("after the hold: %+v", got)
	}
}
