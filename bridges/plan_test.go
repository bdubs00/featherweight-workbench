package bridges

import (
	"context"
	"errors"
	"testing"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// CheckPlan names the field of each problem of a plan, and a playbook
// needs no title.
func TestCheckPlanNamesTheField(t *testing.T) {
	a := newAgent(t, nil)
	s := newStore(t, a, nil)
	spec := OpenSpec{Placement: "nowhere", Members: []SeatSpec{
		{Handle: "Bad", Agent: "migrator", Task: "x"},
		{Handle: "ok", Agent: "", Task: "", Budget: &SeatBudget{Deadline: "soon"}},
		{Handle: "ok", Agent: "migrator", Task: "y"},
	}}
	paths := map[string]bool{}
	for _, p := range s.CheckPlan(spec, false) {
		paths[p.Path] = true
	}
	for _, want := range []string{"placement", "members[0].handle", "members[1].agent", "members[1].task", "members[1].budget.deadline", "members[2].handle"} {
		if !paths[want] {
			t.Errorf("no problem of %s: %v", want, paths)
		}
	}
	if paths["title"] {
		t.Errorf("a plan without a title needs one: %v", paths)
	}
	if p := s.CheckPlan(OpenSpec{Members: []SeatSpec{{Handle: "a", Agent: "migrator", Task: "x"}}}, true); len(p) != 1 || p[0].Path != "title" {
		t.Fatalf("a bridge without a title = %+v", p)
	}
}

// OpenFor opens a bridge as the front for its operator, whose runs act as
// the operator, and PostFor posts in it as the front for a sender with a
// name. A closed bridge takes no post, and Peek tells it.
func TestOpenForAndPostFor(t *testing.T) {
	a := newAgent(t, nil)
	s := newStore(t, a, nil)
	q := captureQueues(t, a, "fw.jobs.migrator")
	duty := &fw.Principal{Subject: "duty:weekly", Kind: fw.KindWorkload}
	ctx := context.Background()
	it, err := s.OpenFor(ctx, OpenSpec{Title: "weekly: 2026-10-23", Goal: "Patch.", Members: []SeatSpec{{Handle: "patcher", Agent: "migrator", Task: "Patch the hosts."}}}, duty)
	if err != nil || it.Operator != "duty:weekly" || len(it.Members) != 1 {
		t.Fatalf("OpenFor = %+v, %v", it, err)
	}
	eventually(t, "the seat on its queue", func() bool { _, _, ok := q.get("patcher"); return ok })
	if req, _, _ := q.get("patcher"); req.Principal != "duty:weekly" || req.PrincipalKind != fw.KindWorkload {
		t.Fatalf("the seat acts as %s (%s)", req.Principal, req.PrincipalKind)
	}
	m, err := s.PostFor(ctx, it.ID, PostSpec{Text: "The weekly run starts.", To: []string{"patcher"}}, duty, "the duty weekly")
	if err != nil || m.From.Kind != KindPerson || m.From.Name != "duty:weekly" || m.From.Display != "the duty weekly" || len(m.To) != 1 {
		t.Fatalf("PostFor = %+v, %v", m, err)
	}
	if state, handles, err := s.Peek(ctx, it.ID); err != nil || state != StateOpen || len(handles) != 1 || handles[0] != "patcher" {
		t.Fatalf("Peek = %s %v, %v", state, handles, err)
	}
	closed := StateClosed
	if _, _, err := s.patch(ctx, it.ID, BridgePatch{State: &closed}, nil); err != nil {
		t.Fatal(err)
	}
	if state, _, _ := s.Peek(ctx, it.ID); state != StateClosed {
		t.Fatalf("Peek after the close = %s", state)
	}
	if _, err := s.PostFor(ctx, it.ID, PostSpec{Text: "late"}, duty, "the duty weekly"); !errors.Is(err, ErrConflict) {
		t.Fatalf("a post to a closed bridge = %v", err)
	}
	if _, err := s.PostFor(ctx, "01nobridge", PostSpec{Text: "x"}, duty, ""); err == nil {
		t.Fatal("a post to no bridge went out")
	}
	if _, _, err := s.Peek(ctx, "01nobridge"); err == nil {
		t.Fatal("Peek of no bridge has a state")
	}
}

// A bridge that a duty opens for its owner is in the list of the owner,
// as a bridge of the operator is, and not in the list of another person.
func TestOpenForListsTheOwner(t *testing.T) {
	a := newAgent(t, nil)
	s := newStore(t, a, nil)
	captureQueues(t, a, "fw.jobs.migrator")
	duty := &fw.Principal{Subject: "duty:weekly", Kind: fw.KindWorkload}
	ctx := context.Background()
	it, err := s.OpenFor(ctx, OpenSpec{Title: "weekly: 2026-10-23", Goal: "Patch.", Owner: "alice", Members: []SeatSpec{{Handle: "patcher", Agent: "migrator", Task: "Patch the hosts."}}}, duty)
	if err != nil {
		t.Fatal(err)
	}
	rec, _, err := s.b.get(ctx, it.ID)
	if err != nil || rec.Owner != "alice" {
		t.Fatalf("the record = %+v, %v", rec, err)
	}
	for who, want := range map[string]bool{"alice": true, "duty:weekly": true, "bob": false} {
		got := rec.operatedBy(who)
		if got != want {
			t.Errorf("operatedBy(%s) = %v, want %v", who, got, want)
		}
	}
	if rec.operatedBy("") {
		t.Error("an empty subject operates the bridge")
	}
	alice := fw.WithPrincipal(ctx, &fw.Principal{Subject: "alice", Kind: fw.KindUser})
	if ok, err := s.listed(alice, rec, false); err != nil || !ok {
		t.Fatalf("the list of the owner = %v, %v", ok, err)
	}
}
