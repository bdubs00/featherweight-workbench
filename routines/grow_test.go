package routines

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/fleet"
)

// moveCall is one call of OnMove.
type moveCall struct{ agent, name, from, to, by, note string }

// moves records the calls of OnMove for the length of a test.
func movesOf(t *testing.T) func() []moveCall {
	t.Helper()
	var mu sync.Mutex
	var got []moveCall
	OnMove = func(_ context.Context, agent, name, from, to, by, note string) {
		mu.Lock()
		got = append(got, moveCall{agent, name, from, to, by, note})
		mu.Unlock()
	}
	t.Cleanup(func() { OnMove = nil })
	return func() []moveCall {
		mu.Lock()
		defer mu.Unlock()
		return append([]moveCall(nil), got...)
	}
}

// stream feeds a projection the events of the runs of the routine
// acknowledge, as the egress stream brings them.
type stream struct {
	t       *testing.T
	p       *projector
	version int
	at      time.Time
	n       int
}

func newStream(t *testing.T, s *Store) *stream {
	return &stream{t: t, p: newProjector(s, quiet()), version: 1, at: time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC)}
}

func (st *stream) send(events ...fleet.RunEvent) {
	st.p.apply(context.Background(), events)
}

func (st *stream) event(run, typ string, data any) fleet.RunEvent {
	e := fw.MustEvent(run, typ, data)
	st.at = st.at.Add(time.Second)
	e.Time = st.at
	return fleet.RunEvent{Agent: "triage", Line: "#7 a ticket", Event: e}
}

// rehearse sends n rehearsal runs of the risk, each that the routine
// would have acted on and that agreed with the agent.
func (st *stream) rehearse(n int, risk string) {
	for range n {
		st.n++
		run := fmt.Sprintf("01R%05d", st.n)
		st.send(
			st.event(run, fw.EvRoutineDecided, fw.RoutineDecidedData{Routine: "acknowledge", Version: st.version, Mode: fw.RoutineShadow, Applies: true, Risk: risk,
				Answers: map[string]fw.Answer{"type": {Type: fw.QuestionChoice, Choice: "bug", Probability: 0.95}}}),
			st.event(run, fw.EvRoutineCompared, fw.RoutineComparedData{Routine: "acknowledge", Version: st.version, Mode: fw.RoutineShadow, Question: "type", Said: "bug", Observed: "bug", Agree: true}),
		)
	}
}

// canary sends one canary run of a live routine that agrees or not.
func (st *stream) canary(run string, agree bool) {
	observed := "bug"
	if !agree {
		observed = "access"
	}
	st.send(
		st.event(run, fw.EvRoutineDecided, fw.RoutineDecidedData{Routine: "acknowledge", Version: st.version, Standing: fw.RoutineLive, Mode: fw.RoutineCanary, Applies: true, Risk: fw.RiskWrite,
			Answers: map[string]fw.Answer{"type": {Type: fw.QuestionChoice, Choice: "bug", Probability: 0.95}}}),
		st.event(run, fw.EvRoutineCompared, fw.RoutineComparedData{Routine: "acknowledge", Version: st.version, Mode: fw.RoutineCanary, Question: "type", Said: "bug", Observed: observed, Agree: agree}),
	)
}

// get reads the record of the routine with its evidence.
func get(t *testing.T, s *Store) Record {
	t.Helper()
	rec, err := s.Get(context.Background(), "triage", "acknowledge")
	if err != nil {
		t.Fatal(err)
	}
	return rec
}

// liveRoutine puts the routine and has alice sign it off.
func liveRoutine(t *testing.T, s *Store) {
	t.Helper()
	ctx := context.Background()
	if _, _, err := s.Put(ctx, PutRequest{Agent: "triage", Routine: ack(), Standing: Rehearsing, By: "alice"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Move(ctx, MoveRequest{Agent: "triage", Name: "acknowledge", To: Live, By: "alice", Person: true}); err != nil {
		t.Fatal(err)
	}
}

// Every move of the store calls OnMove with the standing before and after
// it and its writer: a new routine, a move of a person, and a new body of
// a live routine. A person still cannot move a live routine to rehearsing.
func TestMovesCallOnMove(t *testing.T) {
	got := movesOf(t)
	ctx := context.Background()
	s := memStore(Config{})
	if _, _, err := s.Put(ctx, PutRequest{Agent: "triage", Routine: ack(), By: "bob", Note: "from 30 runs"}); err != nil {
		t.Fatal(err)
	}
	for _, to := range []string{Rehearsing, Live} {
		if _, _, err := s.Move(ctx, MoveRequest{Agent: "triage", Name: "acknowledge", To: to, By: "alice", Person: true}); err != nil {
			t.Fatal(err)
		}
	}
	// A move to the standing that the routine has changes nothing.
	if _, moved, _ := s.Move(ctx, MoveRequest{Agent: "triage", Name: "acknowledge", To: Live, By: "alice", Person: true}); moved {
		t.Fatal("a move to the same standing moved")
	}
	var me *MoveError
	if _, _, err := s.Move(ctx, MoveRequest{Agent: "triage", Name: "acknowledge", To: Rehearsing, By: "alice", Person: true}); !errors.As(err, &me) {
		t.Fatalf("a person moved a live routine to rehearsing: %v", err)
	}
	body := ack()
	body.Answer = `TRIAGE {"type": "type/{{ .type }}", "v": 2}`
	if _, _, err := s.Put(ctx, PutRequest{Agent: "triage", Routine: body, By: "carol"}); err != nil {
		t.Fatal(err)
	}
	want := []moveCall{
		{"triage", "acknowledge", "", Proposed, "bob", "from 30 runs"},
		{"triage", "acknowledge", Proposed, Rehearsing, "alice", ""},
		{"triage", "acknowledge", Rehearsing, Live, "alice", ""},
		{"triage", "acknowledge", Live, Rehearsing, "carol", "a new version rehearses again"},
	}
	list := got()
	if len(list) != len(want) {
		t.Fatalf("OnMove got %+v, want %+v", list, want)
	}
	for i := range want {
		if list[i] != want[i] {
			t.Errorf("move %d = %+v, want %+v", i, list[i], want[i])
		}
	}
	if rec := get(t, s); rec.SignedOff != nil {
		t.Errorf("a new version kept the sign-off %+v", rec.SignedOff)
	}
}

// One disagreement in a canary run steps a live routine back to
// rehearsing, by Know-how, with the run in the note. The step back keeps
// the sign-off, and the ready rule starts to count again.
func TestStepBackOnACanaryDisagreement(t *testing.T) {
	got := movesOf(t)
	s := memStore(Config{})
	liveRoutine(t, s)
	st := newStream(t, s)
	st.canary("01CANARY1", true)
	if rec := get(t, s); rec.Standing != Live {
		t.Fatalf("an agreed canary moved the routine: %s", rec.Standing)
	}
	st.canary("01CANARY2", false)
	rec := get(t, s)
	last := rec.History[len(rec.History)-1]
	if rec.Standing != Rehearsing || !rec.SteppedBack || rec.Ready || rec.SignedOff == nil || rec.SignedOff.By != "alice" ||
		last.By != ByKnowhow || !strings.Contains(last.Note, "run 01CANARY2") || !strings.Contains(last.Note, "canary") {
		t.Fatalf("after a canary disagreement: %+v, last move %+v", rec, last)
	}
	if ev := rec.Evidence; ev.Since == nil || ev.Since.Totals.Acted != 2 || ev.Risk != fw.RiskWrite {
		t.Fatalf("the evidence after the step back = since %+v, risk %q", ev.Since, ev.Risk)
	}
	list := got()
	if m := list[len(list)-1]; m.from != Live || m.to != Rehearsing || m.by != ByKnowhow || !strings.Contains(m.note, "01CANARY2") {
		t.Fatalf("OnMove of the step back = %+v", m)
	}
}

// A run that a live routine completed and that came back with a rejected
// receipt steps the routine back.
func TestStepBackOnASentBackRun(t *testing.T) {
	movesOf(t)
	s := memStore(Config{})
	liveRoutine(t, s)
	st := newStream(t, s)
	st.send(
		st.event("01LIVE", fw.EvRoutineDecided, fw.RoutineDecidedData{Routine: "acknowledge", Version: 1, Standing: fw.RoutineLive, Mode: fw.RoutineLive, Applies: true, Risk: fw.RiskWrite}),
		st.event("01LIVE", fw.EvRoutineApplied, fw.RoutineAppliedData{Routine: "acknowledge", Version: 1, OK: true, Steps: 3}),
	)
	if rec := get(t, s); rec.Standing != Live {
		t.Fatalf("a run that the routine completed moved it: %s", rec.Standing)
	}
	st.send(st.event("01LIVE", fw.EvRunAnnotated, fw.QualityAnnotatedData{Quality: &fw.QualityObservation{ID: "q1", Completion: 5, Verification: "rejected"}}))
	rec := get(t, s)
	if last := rec.History[len(rec.History)-1]; rec.Standing != Rehearsing || last.By != ByKnowhow || !strings.Contains(last.Note, "run 01LIVE") || !strings.Contains(last.Note, "sent back") {
		t.Fatalf("after a sent-back run: %s, last move %+v", rec.Standing, last)
	}
}

// A ready routine that only reads goes live by itself after 20 runs, and
// one that writes waits for a person after 50 runs, the defaults of
// readyAfter.
func TestReadyByRisk(t *testing.T) {
	got := movesOf(t)
	read, write := memStore(Config{}), memStore(Config{})
	ctx := context.Background()
	for _, s := range []*Store{read, write} {
		if _, _, err := s.Put(ctx, PutRequest{Agent: "triage", Routine: ack(), Standing: Rehearsing, By: "alice"}); err != nil {
			t.Fatal(err)
		}
	}
	r := newStream(t, read)
	r.rehearse(19, fw.RiskRead)
	if rec := get(t, read); rec.Standing != Rehearsing || rec.Ready {
		t.Fatalf("a read routine after 19 runs: %s, ready %v", rec.Standing, rec.Ready)
	}
	r.rehearse(1, fw.RiskRead)
	rec := get(t, read)
	if last := rec.History[len(rec.History)-1]; rec.Standing != Live || rec.SignedOff != nil || last.By != ByKnowhow || !strings.Contains(last.Note, "only reads") {
		t.Fatalf("a read routine after 20 runs: %s, signed off %+v, last move %+v", rec.Standing, rec.SignedOff, last)
	}
	if m := got()[len(got())-1]; m.from != Rehearsing || m.to != Live || m.by != ByKnowhow {
		t.Fatalf("OnMove of the move to live = %+v", m)
	}

	w := newStream(t, write)
	w.rehearse(49, fw.RiskWrite)
	if rec := get(t, write); rec.Standing != Rehearsing || rec.Ready {
		t.Fatalf("a write routine after 49 runs: %s, ready %v", rec.Standing, rec.Ready)
	}
	w.rehearse(1, fw.RiskWrite)
	if rec := get(t, write); rec.Standing != Rehearsing || !rec.Ready {
		t.Fatalf("a write routine after 50 runs: %s, ready %v", rec.Standing, rec.Ready)
	}
	// A person signs it off.
	if _, _, err := write.Move(ctx, MoveRequest{Agent: "triage", Name: "acknowledge", To: Live, By: "alice", Person: true, Version: 1}); err != nil {
		t.Fatal(err)
	}
	if rec := get(t, write); rec.SignedOff == nil || rec.SignedOff.Version != 1 {
		t.Fatalf("the sign-off = %+v", rec.SignedOff)
	}
}

// With AskFirst on for the agent, a ready routine that only reads waits
// for a person.
func TestReadyReadAsksFirst(t *testing.T) {
	movesOf(t)
	AskFirst = func(_ context.Context, agent string) bool { return agent == "triage" }
	t.Cleanup(func() { AskFirst = nil })
	s := memStore(Config{})
	if _, _, err := s.Put(context.Background(), PutRequest{Agent: "triage", Routine: ack(), Standing: Rehearsing, By: "alice"}); err != nil {
		t.Fatal(err)
	}
	newStream(t, s).rehearse(25, fw.RiskRead)
	if rec := get(t, s); rec.Standing != Rehearsing || !rec.Ready {
		t.Fatalf("with ask first, a ready read routine: %s, ready %v", rec.Standing, rec.Ready)
	}
}

// A version that a person signed off and that stepped back goes live
// again by itself once it is ready again, counted from the step back. A
// routine that a person moved back by hand does not.
func TestSignedOffReturnsToLive(t *testing.T) {
	got := movesOf(t)
	s := memStore(Config{})
	liveRoutine(t, s)
	st := newStream(t, s)
	st.rehearse(60, fw.RiskWrite) // counted while live, before the step back
	st.canary("01CANARY", false)
	if rec := get(t, s); rec.Standing != Rehearsing || rec.Ready {
		t.Fatalf("after the step back: %s, ready %v", rec.Standing, rec.Ready)
	}
	st.rehearse(49, fw.RiskWrite)
	if rec := get(t, s); rec.Standing != Rehearsing {
		t.Fatalf("after 49 runs since the step back: %s", rec.Standing)
	}
	st.rehearse(1, fw.RiskWrite)
	rec := get(t, s)
	last := rec.History[len(rec.History)-1]
	if rec.Standing != Live || rec.SteppedBack || rec.SignedOff == nil || rec.SignedOff.By != "alice" || last.By != ByKnowhow || !strings.Contains(last.Note, "alice signed off version 1") {
		t.Fatalf("after 50 runs since the step back: %+v, last move %+v", rec, last)
	}
	if m := got()[len(got())-1]; m.from != Rehearsing || m.to != Live || m.by != ByKnowhow {
		t.Fatalf("OnMove = %+v", m)
	}

	// A person pauses it and moves it back to rehearsing: it waits for a
	// person once it is ready.
	ctx := context.Background()
	for _, to := range []string{Paused, Rehearsing} {
		if _, _, err := s.Move(ctx, MoveRequest{Agent: "triage", Name: "acknowledge", To: to, By: "alice", Person: true}); err != nil {
			t.Fatal(err)
		}
	}
	st.rehearse(1, fw.RiskWrite)
	if rec := get(t, s); rec.Standing != Rehearsing || !rec.Ready {
		t.Fatalf("a routine that a person moved back: %s, ready %v", rec.Standing, rec.Ready)
	}
}

// calls sends n rehearsal runs of a routine without questions, each with
// the comparison of its calls, that agree or not.
func (st *stream) calls(n int, risk string, agree bool) {
	for range n {
		st.n++
		run := fmt.Sprintf("01R%05d", st.n)
		c := fw.RoutineComparedData{Routine: "acknowledge", Version: st.version, Mode: fw.RoutineShadow, Calls: true, Agree: agree}
		if !agree {
			c.Step, c.Said, c.Observed = 2, `{"text":"STOCK pens=90"}`, `{"text":"STOCK pens=120"}`
			c.Reason = "step 2 files: the key text differs"
		}
		st.send(
			st.event(run, fw.EvRoutineDecided, fw.RoutineDecidedData{Routine: "acknowledge", Version: st.version, Mode: fw.RoutineShadow, Applies: true, Risk: risk}),
			st.event(run, fw.EvRoutineCompared, c),
		)
	}
}

// plain is the routine of the tests without its questions, as Know-how
// proposes it without a decider.
func plain() fw.Routine {
	r := ack()
	r.Questions, r.Decider = nil, ""
	r.Answer = "TRIAGE"
	return r
}

// A routine without questions gains agreement from the comparison of its
// calls. Ready, one that only reads goes live by itself, and one that
// writes waits for a sign-off (docs/KNOWHOW.md section 6.5). A
// disagreement of the calls shows with its reason.
func TestReadyWithoutQuestions(t *testing.T) {
	movesOf(t)
	ctx := context.Background()
	read, write := memStore(Config{}), memStore(Config{})
	for _, s := range []*Store{read, write} {
		if _, _, err := s.Put(ctx, PutRequest{Agent: "triage", Routine: plain(), Standing: Rehearsing, By: "alice"}); err != nil {
			t.Fatal(err)
		}
	}
	r := newStream(t, read)
	r.calls(19, fw.RiskRead, true)
	if rec := get(t, read); rec.Standing != Rehearsing || rec.Evidence.Totals.Agreed != 19 {
		t.Fatalf("a read routine after 19 runs: %s, totals %+v", rec.Standing, rec.Evidence.Totals)
	}
	r.calls(1, fw.RiskRead, true)
	if rec := get(t, read); rec.Standing != Live || rec.SignedOff != nil {
		t.Fatalf("a read routine after 20 agreed runs: %s, signed off %+v", rec.Standing, rec.SignedOff)
	}

	w := newStream(t, write)
	w.calls(1, fw.RiskWrite, false)
	w.calls(50, fw.RiskWrite, true)
	rec := get(t, write)
	if rec.Standing != Rehearsing || !rec.Ready || rec.Evidence.Totals != (Counts{Decided: 51, Acted: 51, Agreed: 50, Disagreed: 1}) {
		t.Fatalf("a write routine after 51 runs: %s, ready %v, totals %+v", rec.Standing, rec.Ready, rec.Evidence.Totals)
	}
	d := rec.Evidence.Disagreements
	if len(d) != 1 || len(d[0].Answers) != 1 || !d[0].Answers[0].Calls || d[0].Answers[0].Reason != "step 2 files: the key text differs" ||
		d[0].Answers[0].Said != `{"text":"STOCK pens=90"}` || d[0].Input != "#7 a ticket" {
		t.Fatalf("disagreements = %+v", d)
	}
}

// A live routine whose work failed the checks of done of a run steps back
// to rehearsing, as for a run that came back.
func TestStepBackOnFailedChecksOfDone(t *testing.T) {
	movesOf(t)
	s := memStore(Config{})
	liveRoutine(t, s)
	st := newStream(t, s)
	st.send(
		st.event("01FAIL", fw.EvRoutineDecided, fw.RoutineDecidedData{Routine: "acknowledge", Version: 1, Standing: fw.RoutineLive, Mode: fw.RoutineLive, Applies: true, Risk: fw.RiskWrite}),
		st.event("01FAIL", fw.EvRoutineApplied, fw.RoutineAppliedData{Routine: "acknowledge", Version: 1, Steps: 2, FailedStep: 2}),
	)
	if rec := get(t, s); rec.Standing != Live {
		t.Fatalf("a failed step moved the routine: %s", rec.Standing)
	}
	st.send(
		st.event("01DONE", fw.EvRoutineDecided, fw.RoutineDecidedData{Routine: "acknowledge", Version: 1, Standing: fw.RoutineLive, Mode: fw.RoutineLive, Applies: true, Risk: fw.RiskWrite}),
		st.event("01DONE", fw.EvRoutineApplied, fw.RoutineAppliedData{Routine: "acknowledge", Version: 1, Steps: 4, FailedStep: 4, Done: fw.DoneFailed,
			Reason: `done: check 1 "the report is saved" (files) failed: the result lacks "STOCK"`}),
	)
	rec := get(t, s)
	if last := rec.History[len(rec.History)-1]; rec.Standing != Rehearsing || last.By != ByKnowhow || !strings.Contains(last.Note, "run 01DONE") ||
		!strings.Contains(last.Note, "checks of done") || rec.Evidence.Totals.Failed != 2 {
		t.Fatalf("after failed checks of done: %s, last move %+v, totals %+v", rec.Standing, last, rec.Evidence.Totals)
	}
}
