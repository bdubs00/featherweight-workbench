package routines

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// The table of the standings allows exactly the moves of the contract,
// and a retired routine moves nowhere.
func TestMovesTable(t *testing.T) {
	want := map[string][]string{
		Proposed:   {Rehearsing, Retired},
		Rehearsing: {Live, Paused, Retired},
		Live:       {Paused, Retired},
		Paused:     {Rehearsing, Live},
		Retired:    {},
	}
	all := []string{Proposed, Rehearsing, Live, Paused, Retired}
	for _, from := range all {
		if !slices.Equal(Moves(from), want[from]) {
			t.Errorf("Moves(%s) = %v, want %v", from, Moves(from), want[from])
		}
		for _, to := range all {
			if got := CanMove(from, to); got != slices.Contains(want[from], to) {
				t.Errorf("CanMove(%s, %s) = %v", from, to, got)
			}
		}
	}
	if ValidStanding("shadow") || !ValidStanding(Rehearsing) {
		t.Error("shadow is a standing of the kernel, not of the workbench")
	}
	for s, ks := range map[string]string{Rehearsing: fw.RoutineShadow, Live: fw.RoutineLive} {
		if got, ok := kernelStanding(s); !ok || got != ks {
			t.Errorf("kernelStanding(%s) = %s, %v", s, got, ok)
		}
	}
	for _, s := range []string{Proposed, Paused, Retired} {
		if _, ok := kernelStanding(s); ok {
			t.Errorf("%s reaches a profile", s)
		}
	}
}

// The moves of a routine follow the table: a forbidden move is a
// *MoveError with the allowed moves, a move to the same standing changes
// nothing, every move records who and when, a sign-off needs a person
// and sets signedOff, and a routine leaves ready behind when it leaves
// rehearsing.
func TestStoreMoves(t *testing.T) {
	ctx := context.Background()
	s := memStore(Config{})
	clock := time.Date(2026, 10, 23, 9, 0, 0, 0, time.UTC)
	s.now = func() time.Time { clock = clock.Add(time.Minute); return clock }
	rec, res, err := s.Put(ctx, PutRequest{Agent: "triage", Routine: ack(), By: "lab", Source: &Source{Runs: 212, From: "2026-09-14", To: "2026-10-21", ProposedBy: "fw routine propose"}})
	if err != nil || !res.Created || rec.Standing != Proposed || rec.Version() != 1 || len(rec.History) != 1 || rec.History[0].By != "lab" || rec.History[0].Standing != Proposed {
		t.Fatalf("Put = %+v, %+v, %v", rec, res, err)
	}
	// proposed cannot go live.
	_, _, err = s.Move(ctx, MoveRequest{Agent: "triage", Name: "acknowledge", To: Live, By: "ada", Person: true})
	var me *MoveError
	if !errors.As(err, &me) || me.From != Proposed || !slices.Equal(me.Allowed, []string{Rehearsing, Retired}) {
		t.Fatalf("proposed to live = %v", err)
	}
	if !strings.Contains(err.Error(), "rehearsing or retired") {
		t.Fatalf("the error does not name the moves: %v", err)
	}
	rec, moved, err := s.Move(ctx, MoveRequest{Agent: "triage", Name: "acknowledge", To: Rehearsing, By: "ada", Note: "try it", Person: true})
	if err != nil || !moved || rec.Standing != Rehearsing || len(rec.History) != 2 || rec.History[1].Note != "try it" || rec.History[1].By != "ada" || rec.History[1].At.IsZero() {
		t.Fatalf("to rehearsing = %+v, %v, %v", rec, moved, err)
	}
	if _, moved, err := s.Move(ctx, MoveRequest{Agent: "triage", Name: "acknowledge", To: Rehearsing, By: "ada"}); err != nil || moved {
		t.Fatalf("a second move to rehearsing = %v, %v", moved, err)
	}
	// The static token alone does not sign off.
	if _, _, err := s.Move(ctx, MoveRequest{Agent: "triage", Name: "acknowledge", To: Live, By: "token"}); !errors.Is(err, fw.ErrForbidden) {
		t.Fatalf("a sign-off without a person = %v", err)
	}
	rec, _, err = s.Move(ctx, MoveRequest{Agent: "triage", Name: "acknowledge", To: Live, By: "ada@example.org", Person: true})
	if err != nil || rec.Standing != Live || rec.SignedOff == nil || rec.SignedOff.By != "ada@example.org" || rec.SignedOff.At.IsZero() {
		t.Fatalf("sign-off = %+v, %v", rec, err)
	}
	for _, step := range []struct {
		to      string
		allowed bool
	}{{Rehearsing, false}, {Paused, true}, {Proposed, false}, {Live, true}, {Retired, true}, {Rehearsing, false}, {Live, false}} {
		_, _, err := s.Move(ctx, MoveRequest{Agent: "triage", Name: "acknowledge", To: step.to, By: "ada", Person: true})
		if step.allowed != (err == nil) {
			t.Fatalf("move to %s = %v, want allowed %v", step.to, err, step.allowed)
		}
	}
	rec, _ = s.Get(ctx, "triage", "acknowledge")
	if rec.Standing != Retired || len(rec.History) != 6 {
		t.Fatalf("after the walk = %s with %d moves", rec.Standing, len(rec.History))
	}
	// A retired routine takes no new body, and the bad requests are
	// ErrInvalid.
	if _, _, err := s.Put(ctx, PutRequest{Agent: "triage", Routine: ack(), By: "lab"}); !errors.Is(err, ErrRetired) {
		t.Fatalf("a body for a retired routine = %v", err)
	}
	for _, req := range []MoveRequest{{Agent: "triage", Name: "acknowledge", To: "shadow"}, {Agent: "tri age", Name: "x", To: Live}, {Agent: "triage", Name: "Bad_Name", To: Live}, {Agent: "triage", Name: "acknowledge", To: Paused, Note: "two\nlines"}} {
		if _, _, err := s.Move(ctx, req); !errors.Is(err, ErrInvalid) {
			t.Errorf("Move(%+v) = %v, want ErrInvalid", req, err)
		}
	}
	if _, _, err := s.Move(ctx, MoveRequest{Agent: "triage", Name: "nothing", To: Live}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a move of a missing routine = %v", err)
	}
}

// A body that changes gets the next version and starts its evidence
// again; the same body changes nothing. A live routine whose body changes
// goes back to rehearsing, and its sign-off goes. A new routine starts
// proposed or rehearsing, never live, and the kernel checks the body.
func TestStorePutVersions(t *testing.T) {
	ctx := context.Background()
	s := memStore(Config{})
	if _, _, err := s.Put(ctx, PutRequest{Agent: "triage", Routine: ack(), Standing: Live}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a new live routine = %v", err)
	}
	bad := ack()
	bad.Steps = nil
	bad.Answer = "{{ .type"
	if _, _, err := s.Put(ctx, PutRequest{Agent: "triage", Routine: bad}); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "answer") {
		t.Fatalf("a body that the kernel refuses = %v", err)
	}
	rec, _, err := s.Put(ctx, PutRequest{Agent: "triage", Routine: ack(), Standing: Rehearsing, By: "alice"})
	if err != nil || rec.Standing != Rehearsing || rec.Version() != 1 || rec.Routine.Standing != "" {
		t.Fatalf("a new rehearsing routine = %+v, %v", rec, err)
	}
	_, _ = s.updateEvidence(ctx, "triage", "acknowledge", func(ev *Evidence) bool {
		ev.Version = 1
		ev.decided("01A", time.Now(), fw.RoutineDecidedData{Mode: fw.RoutineShadow, Applies: true}, ack())
		return true
	})
	if _, res, err := s.Put(ctx, PutRequest{Agent: "triage", Routine: ack(), By: "alice"}); err != nil || res.Changed {
		t.Fatalf("the same body = %+v, %v", res, err)
	}
	if got, _ := s.Get(ctx, "triage", "acknowledge"); got.Evidence.Totals.Acted != 1 || got.Version() != 1 {
		t.Fatalf("the same body touched the evidence: %+v", got.Evidence.Totals)
	}
	changed := ack()
	changed.Answer = "TRIAGE {{ .type }}"
	rec, res, err := s.Put(ctx, PutRequest{Agent: "triage", Routine: changed, By: "alice"})
	if err != nil || !res.Changed || rec.Version() != 2 || rec.Standing != Rehearsing || len(rec.History) != 1 {
		t.Fatalf("a new body = %+v, %+v, %v", rec, res, err)
	}
	if got, _ := s.Get(ctx, "triage", "acknowledge"); got.Evidence.Version != 2 || got.Evidence.Totals.Acted != 0 {
		t.Fatalf("the evidence of version 2 = %+v", got.Evidence)
	}
	if _, _, err := s.Move(ctx, MoveRequest{Agent: "triage", Name: "acknowledge", To: Live, By: "ada", Person: true}); err != nil {
		t.Fatal(err)
	}
	changed.Answer = "TRIAGE type/{{ .type }}"
	rec, res, err = s.Put(ctx, PutRequest{Agent: "triage", Routine: changed, By: "alice", Note: "a clearer answer"})
	if err != nil || !res.Changed || rec.Standing != Rehearsing || rec.Version() != 3 || rec.SignedOff != nil || rec.Ready {
		t.Fatalf("a new body of a live routine = %+v, %+v, %v", rec, res, err)
	}
	last := rec.History[len(rec.History)-1]
	if last.Standing != Rehearsing || last.Version != 3 || !strings.Contains(last.Note, "rehearses again") || !strings.Contains(last.Note, "a clearer answer") {
		t.Fatalf("the move back = %+v", last)
	}
	// A paused routine can go live with one move, so its new body
	// rehearses first too.
	if _, _, err := s.Move(ctx, MoveRequest{Agent: "triage", Name: "acknowledge", To: Live, By: "ada", Person: true}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Move(ctx, MoveRequest{Agent: "triage", Name: "acknowledge", To: Paused, By: "ada"}); err != nil {
		t.Fatal(err)
	}
	changed.Answer = "TRIAGE type {{ .type }}"
	rec, res, err = s.Put(ctx, PutRequest{Agent: "triage", Routine: changed, By: "alice"})
	if err != nil || !res.Changed || rec.Standing != Rehearsing || rec.Version() != 4 || rec.SignedOff != nil {
		t.Fatalf("a new body of a paused routine = %+v, %+v, %v", rec, res, err)
	}
	// A write of a routine that exists names no other standing.
	if _, _, err := s.Put(ctx, PutRequest{Agent: "triage", Routine: changed, Standing: Live}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a standing in a write of a routine that exists = %v", err)
	}
	// The JSON of a record has no standing in its body.
	b, _ := json.Marshal(rec)
	var raw map[string]map[string]any
	_ = json.Unmarshal(b, &raw)
	if _, ok := raw["routine"]["standing"]; ok || raw["routine"]["version"] != float64(4) || raw["routine"]["name"] != "acknowledge" {
		t.Fatalf("the JSON of the record = %s", b)
	}
}

// A write that only creates refuses a routine that exists.
func TestStoreCreateOnly(t *testing.T) {
	ctx := context.Background()
	s := memStore(Config{})
	if _, _, err := s.Put(ctx, PutRequest{Agent: "triage", Routine: ack(), CreateOnly: true}); err != nil {
		t.Fatal(err)
	}
	changed := ack()
	changed.Answer = "other"
	if _, _, err := s.Put(ctx, PutRequest{Agent: "triage", Routine: changed, CreateOnly: true}); !errors.Is(err, fw.ErrForbidden) {
		t.Fatalf("a second create = %v", err)
	}
	if rec, _ := s.Get(ctx, "triage", "acknowledge"); rec.Version() != 1 {
		t.Fatalf("the routine changed: version %d", rec.Version())
	}
}

// An agent holds at most 64 routines that are not retired.
func TestStoreLimit(t *testing.T) {
	ctx := context.Background()
	s := memStore(Config{})
	r := fw.Routine{Steps: []fw.RoutineStep{{Tool: "files", Input: "{}"}}}
	for i := range MaxPerAgent {
		r.Name = fmt.Sprintf("r%d", i)
		if _, _, err := s.Put(ctx, PutRequest{Agent: "triage", Routine: r}); err != nil {
			t.Fatal(err)
		}
	}
	r.Name = "one-more"
	if _, _, err := s.Put(ctx, PutRequest{Agent: "triage", Routine: r}); !errors.Is(err, ErrLimit) {
		t.Fatalf("routine 65 = %v", err)
	}
	if _, _, err := s.Put(ctx, PutRequest{Agent: "other", Routine: r}); err != nil {
		t.Fatalf("another agent = %v", err)
	}
	if _, _, err := s.Move(ctx, MoveRequest{Agent: "triage", Name: "r0", To: Retired}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Put(ctx, PutRequest{Agent: "triage", Routine: r}); err != nil {
		t.Fatalf("after a retirement = %v", err)
	}
}

// Only a rehearsing and a live routine reach a profile, as shadow and
// live, oldest first. An agent whose routines reach none has an empty
// entry, so its profile runs no routine.
func TestStoreReaching(t *testing.T) {
	ctx := context.Background()
	s := memStore(Config{})
	clock := time.Date(2026, 10, 23, 9, 0, 0, 0, time.UTC)
	s.now = func() time.Time { clock = clock.Add(time.Minute); return clock }
	r := fw.Routine{Steps: []fw.RoutineStep{{Tool: "files", Input: "{}"}}}
	for _, c := range []struct{ agent, name, to string }{
		{"triage", "first", Rehearsing}, {"triage", "second", Live}, {"triage", "third", Proposed},
		{"triage", "fourth", Paused}, {"desk", "only", Proposed},
	} {
		r.Name = c.name
		if _, _, err := s.Put(ctx, PutRequest{Agent: c.agent, Routine: r, Standing: Rehearsing}); err != nil {
			t.Fatal(err)
		}
		switch c.to {
		case Live, Paused:
			if _, _, err := s.Move(ctx, MoveRequest{Agent: c.agent, Name: c.name, To: c.to, By: "ada", Person: true}); err != nil {
				t.Fatal(err)
			}
		case Proposed:
			_, _ = s.b.Get(ctx, keyOf(c.agent, c.name))
			rec, rev, _ := s.get(ctx, c.agent, c.name)
			rec.Standing = Proposed
			v, _ := encode(rec)
			if _, err := s.b.Update(ctx, keyOf(c.agent, c.name), v, rev); err != nil {
				t.Fatal(err)
			}
		}
	}
	got, err := s.Reaching(ctx)
	if err != nil {
		t.Fatal(err)
	}
	tr := got["triage"]
	if len(tr) != 2 || tr[0].Name != "first" || tr[0].Standing != fw.RoutineShadow || tr[1].Name != "second" || tr[1].Standing != fw.RoutineLive || tr[0].Version != 1 {
		t.Fatalf("triage = %+v", tr)
	}
	if d, ok := got["desk"]; !ok || d == nil || len(d) != 0 {
		t.Fatalf("desk = %+v, %v", d, ok)
	}
	if _, ok := got["nobody"]; ok {
		t.Fatal("an agent without records has an entry")
	}
}
