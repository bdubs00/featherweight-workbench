package knowhow

import (
	"context"
	"testing"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// A note that waits under ask first counts for a person who can decide
// it: an owner of the agent for a note of a Duty, never an operator or a
// viewer, who cannot keep it. The process itself sees every waiting note.
// A change of a routine waits in Know-how alone. (plan A17)
func TestWaitingFor(t *testing.T) {
	fr := newFront(t, nil)
	ctx := context.Background()
	if _, err := fr.s.SetAskFirst(ctx, "triage", true, "person:alice"); err != nil {
		t.Fatal(err)
	}
	fr.write(t, duty, "a.md", "one")
	fr.write(t, duty, "b.md", "two")
	as := func(who string) context.Context {
		return fw.WithPrincipal(ctx, &fw.Principal{Subject: who, Kind: fw.KindUser})
	}
	for who, want := range map[string]int{"alice": 2, "bob": 0, "carol": 0, "dave": 0} {
		got, err := fr.s.WaitingFor(as(who))
		if err != nil || len(got) != want {
			t.Errorf("the notes that wait for %s = %d, %v; want %d", who, len(got), err, want)
		}
	}
	all, err := fr.s.WaitingFor(ctx)
	if err != nil || len(all) != 2 || all[0].Subject.Note != "a.md" || all[1].Subject.Note != "b.md" {
		t.Fatalf("the notes that wait = %+v, %v", all, err)
	}
	if _, err := fr.s.Decide(ctx, all[0].ID, true, "person:alice"); err != nil {
		t.Fatal(err)
	}
	if left, _ := fr.s.WaitingFor(as("alice")); len(left) != 1 {
		t.Fatalf("after a keep, the notes that wait = %+v", left)
	}
	var none *Service
	if got, err := none.WaitingFor(ctx); got != nil || err != nil {
		t.Fatalf("a nil service = %v, %v", got, err)
	}
}
