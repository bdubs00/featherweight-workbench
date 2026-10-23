package routines

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/fleet"
)

// decidedData is a decision of the routine of the helpdesk in a mode: type
// is bug with the probability p, and beyond is no.
func decidedData(mode string, applies bool, p float64) fw.RoutineDecidedData {
	return fw.RoutineDecidedData{Routine: "acknowledge", Version: 1, Mode: mode, Applies: applies, Answers: map[string]fw.Answer{
		"type":   {Type: fw.QuestionChoice, Choice: "bug", Probability: p, Probabilities: map[string]float64{"bug": p}},
		"beyond": {Type: fw.QuestionNoul, Probability: 0.03},
	}}
}

// comparedData compares one answer of the routine with what the agent did.
func comparedData(question, said, observed string) fw.RoutineComparedData {
	return fw.RoutineComparedData{Routine: "acknowledge", Version: 1, Mode: fw.RoutineShadow, Question: question, Said: said, Observed: observed, Agree: said == observed}
}

// The ready rule: enough runs that the routine would have acted on, a
// share of them agreed, and no agreed run sent back.
func TestReadyRule(t *testing.T) {
	c := Config{ReadyAfter: 200, ReadyAgreement: 0.95}
	for _, tc := range []struct {
		acted, agreed, sentBack int
		want                    bool
	}{
		{200, 190, 0, true},
		{199, 199, 0, false},
		{200, 189, 0, false},
		{1000, 950, 0, true},
		{1000, 950, 1, false},
		{0, 0, 0, false},
	} {
		ev := &Evidence{Totals: Counts{Acted: tc.acted, Agreed: tc.agreed}, SentBack: SentBack{Agreed: tc.sentBack}}
		if got := ev.Ready(c); got != tc.want {
			t.Errorf("Ready(acted %d, agreed %d, sent back %d) = %v", tc.acted, tc.agreed, tc.sentBack, got)
		}
	}
	if (*Evidence)(nil).Ready(c) {
		t.Error("no evidence is ready")
	}
	if ev := (&Evidence{Totals: Counts{Acted: 3, Agreed: 3}}); !ev.Ready(Config{ReadyAfter: 3, ReadyAgreement: 1}) {
		t.Error("a full agreement at the bound is not ready")
	}
}

// The evidence counts each event once: a decision, an end of a live
// routine, a comparison, and a receipt that comes twice, as two replicas
// of the front apply it, count once. A disagreement moves an agreed run to
// disagreed, a receipt marks the disagreement and counts a sent-back run
// on the right side, and the reliability table puts each answer in the
// band of its probability.
func TestEvidenceCountsOnce(t *testing.T) {
	ev := newEvidence(1)
	r := ack()
	day := time.Date(2026, 10, 23, 10, 0, 0, 0, time.UTC)
	for range 2 {
		ev.decided("01A", day, decidedData(fw.RoutineShadow, true, 0.91), r)
		ev.decided("01B", day, decidedData(fw.RoutineShadow, true, 0.86), r)
		ev.decided("01C", day, decidedData(fw.RoutineShadow, false, 0.4), r)
		ev.decided("01L", day.Add(24*time.Hour), fw.RoutineDecidedData{Mode: fw.RoutineLive, Applies: true}, r)
		ev.applied("01L", day.Add(24*time.Hour), fw.RoutineAppliedData{OK: true})
		ev.compared("01A", "Printer on floor 3 shows an error", day, comparedData("type", "bug", "bug"), r)
		ev.compared("01B", "VPN drops every hour", day, comparedData("type", "bug", "access"), r)
		ev.compared("01C", "Hello", day, comparedData("type", "other", "bug"), r)
		ev.verified("01B", "accepted")
		ev.verified("01A", "rejected")
		ev.verified("01L", "rejected")
	}
	want := Counts{Decided: 3, Acted: 2, Agreed: 1, Disagreed: 1, Handed: 1, Applied: 1}
	if ev.Totals != want {
		t.Fatalf("totals = %+v, want %+v", ev.Totals, want)
	}
	if len(ev.Days) != 2 || ev.Days[0].Day != "2026-10-23" || ev.Days[0].Decided != 3 || ev.Days[1].Applied != 1 {
		t.Fatalf("days = %+v", ev.Days)
	}
	if ev.SentBack != (SentBack{Applied: 1, Agreed: 1}) {
		t.Fatalf("sent back = %+v", ev.SentBack)
	}
	if len(ev.Disagreements) != 1 {
		t.Fatalf("disagreements = %+v", ev.Disagreements)
	}
	d := ev.Disagreements[0]
	if d.Run != "01B" || d.Input != "VPN drops every hour" || d.Verification != "accepted" || len(d.Answers) != 1 || d.Answers[0].Said != "bug" || d.Answers[0].Observed != "access" || d.Answers[0].Probability != 0.86 {
		t.Fatalf("disagreement = %+v", d)
	}
	// Bands 0, 0.5, 0.7, 0.85, 0.95 of type, whose act is 0.85 already.
	bands := ev.Questions["type"]
	if len(bands) != 5 || bands[0].Runs != 1 || bands[0].Agreed != 0 || bands[3].From != 0.85 || bands[3].Runs != 2 || bands[3].Agreed != 1 {
		t.Fatalf("the reliability of type = %+v", bands)
	}
	if ev.Ready(Config{ReadyAfter: 2, ReadyAgreement: 0.5}) {
		t.Fatal("a routine whose agreed run came back is ready")
	}

	// A later disagreement of an agreed run moves it, also out of the
	// agreed runs that came back.
	both := ack()
	beyond := both.Questions["beyond"]
	beyond.Observe = &fw.Observe{Answer: "beyond: (yes|no)"}
	both.Questions["beyond"] = beyond
	ev.compared("01A", "Printer on floor 3 shows an error", day, comparedData("beyond", "no", "yes"), both)
	if ev.Totals.Agreed != 0 || ev.Totals.Disagreed != 2 || ev.SentBack.Agreed != 0 || len(ev.Disagreements) != 2 {
		t.Fatalf("after the late disagreement: totals %+v, sent back %+v, %d disagreements", ev.Totals, ev.SentBack, len(ev.Disagreements))
	}
	if a := ev.Disagreements[0].Answers; ev.Disagreements[0].Run != "01A" && ev.Disagreements[1].Run != "01A" || len(a) == 0 {
		t.Fatalf("the disagreement of 01A = %+v", ev.Disagreements)
	}
}

// The act of a question that is not a bound of the table becomes one.
func TestEvidenceBandOfAct(t *testing.T) {
	ev := newEvidence(1)
	ev.band("urgency", 0.82, true, 0.8)
	b := ev.Questions["urgency"]
	if len(b) != 6 || b[3].From != 0.8 || b[3].To != 0.85 || b[3].Runs != 1 {
		t.Fatalf("bands = %+v", b)
	}
	ev.band("urgency", 1, true, 0.8)
	if b := ev.Questions["urgency"]; b[5].Runs != 1 {
		t.Fatalf("a probability of 1 = %+v", b)
	}
}

// The projection reads the events of the routines from the egress stream
// that the fleet reads: a recorded sequence of a rehearsal, written by
// hand from the data of the kernel, lands in the evidence, an event of an
// older version counts nothing, a receipt of a run marks it, the ready
// flag follows the rule, and the route of one routine serves the
// evidence.
func TestProjectionOnTheStream(t *testing.T) {
	fr := newFront(t, withClef)
	ctx := context.Background()
	if _, _, err := fr.s.Put(ctx, PutRequest{Agent: "triage", Routine: ack(), Standing: Rehearsing, By: "alice"}); err != nil {
		t.Fatal(err)
	}
	fr.s.cfg = Config{ReadyAfter: 3, ReadyAgreement: 0.6}
	at := time.Date(2026, 10, 23, 11, 0, 0, 0, time.UTC)
	answers := func(p float64) map[string]fw.Answer {
		return map[string]fw.Answer{
			"type":   {Type: fw.QuestionChoice, Choice: "bug", Probability: p, Probabilities: map[string]float64{"bug": p, "other": 1 - p}},
			"beyond": {Type: fw.QuestionNoul, Probability: 0.02},
		}
	}
	run := func(id, input string, observed string, p float64) {
		fr.event(t, at, id, 1, fw.EvRunQueued, fw.RunQueuedData{Agent: "triage", Input: input + "\nThe second line.", Principal: "gitea"})
		fr.event(t, at, id, 2, fw.EvRunStarted, fw.RunStartedData{})
		fr.event(t, at, id, 3, fw.EvRoutineDecided, fw.RoutineDecidedData{Routine: "acknowledge", Version: 1, Standing: fw.RoutineShadow, Mode: fw.RoutineShadow, Applies: true, Answers: answers(p), Decider: "clef", Model: "clef"})
		fr.event(t, at, id, 4, fw.EvRoutineCompared, fw.RoutineComparedData{Routine: "acknowledge", Version: 1, Mode: fw.RoutineShadow, Question: "type", Said: "bug", Observed: observed, Agree: observed == "bug"})
		fr.event(t, at, id, 5, fw.EvRunFinished, fw.RunFinishedData{Outcome: fw.OutcomeCompleted, Totals: &fw.RunUsage{Turns: 1, CostUSD: 0.02}})
	}
	run("01RUNA", "#41 Printer shows an error", "bug", 0.93)
	run("01RUNB", "#42 VPN drops every hour", "access", 0.88)
	run("01RUNC", "#43 Laptop does not start", "bug", 0.97)
	// An event of an older version counts nothing.
	fr.event(t, at, "01RUNC", 6, fw.EvRoutineDecided, fw.RoutineDecidedData{Routine: "acknowledge", Version: 9, Mode: fw.RoutineShadow, Applies: true})
	// A requester rejects the run where the routine disagreed.
	fr.event(t, at, "01RUNB", 6, fw.EvRunAnnotated, fw.QualityAnnotatedData{Quality: &fw.QualityObservation{ID: "r1", Completion: 5, Verification: "rejected"}})

	ev := waitEvidence(t, fr.s, "triage", "acknowledge", func(ev *Evidence) bool {
		return ev.Totals.Decided == 3 && len(ev.Disagreements) == 1 && ev.Disagreements[0].Verification == "rejected"
	})
	if ev.Totals != (Counts{Decided: 3, Acted: 3, Agreed: 2, Disagreed: 1}) || ev.SentBack != (SentBack{}) {
		t.Fatalf("evidence = %+v, sent back %+v", ev.Totals, ev.SentBack)
	}
	if d := ev.Disagreements[0]; d.Run != "01RUNB" || d.Input != "#42 VPN drops every hour" || d.Answers[0].Observed != "access" {
		t.Fatalf("the disagreement = %+v", d)
	}
	// 2 of 3 agreed passes 0.6 over 3 runs, and the run that came back
	// is one where the routine disagreed.
	if rec, _ := fr.s.Get(ctx, "triage", "acknowledge"); !rec.Ready || rec.Standing != Rehearsing {
		t.Fatalf("the routine is not ready: %+v", rec)
	}
	// An agreed run that comes back takes ready away.
	fr.event(t, at, "01RUNA", 6, fw.EvRunAnnotated, fw.QualityAnnotatedData{Quality: &fw.QualityObservation{ID: "r2", Completion: 5, Verification: "rejected"}})
	waitEvidence(t, fr.s, "triage", "acknowledge", func(ev *Evidence) bool { return ev.SentBack.Agreed == 1 })
	deadline := time.Now().Add(5 * time.Second)
	for {
		rec, _ := fr.s.Get(ctx, "triage", "acknowledge")
		if !rec.Ready {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("an agreed run came back and the routine stays ready")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// waitEvidence waits until the evidence of a routine passes ok.
func waitEvidence(t *testing.T, s *Store, agent, name string, ok func(*Evidence) bool) *Evidence {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		rec, err := s.Get(context.Background(), agent, name)
		if err == nil && rec.Evidence != nil && ok(rec.Evidence) {
			return rec.Evidence
		}
		if time.Now().After(deadline) {
			t.Fatalf("the evidence did not arrive: %+v, %v", rec.Evidence, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Two replicas of the front read the same stream and write one bucket:
// each applies every event, and the evidence counts each event once.
func TestTwoReplicasCountOnce(t *testing.T) {
	ctx := context.Background()
	one := memStore(Config{})
	two := New(one.b, one.cfg, quiet())
	if _, _, err := one.Put(ctx, PutRequest{Agent: "triage", Routine: ack(), Standing: Rehearsing}); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 10, 23, 11, 0, 0, 0, time.UTC)
	ev := func(id string, seq uint64, typ string, data any) fleet.RunEvent {
		e := fw.MustEvent(id, typ, data)
		e.Seq, e.Time = seq, at
		return fleet.RunEvent{Agent: "triage", Line: "#7 a ticket", Event: e}
	}
	batch := []fleet.RunEvent{
		ev("01X", 3, fw.EvRoutineDecided, fw.RoutineDecidedData{Routine: "acknowledge", Version: 1, Mode: fw.RoutineCanary, Applies: true,
			Answers: map[string]fw.Answer{"type": {Type: fw.QuestionChoice, Choice: "bug", Probability: 0.9}}}),
		ev("01X", 4, fw.EvRoutineCompared, fw.RoutineComparedData{Routine: "acknowledge", Version: 1, Mode: fw.RoutineCanary, Question: "type", Said: "bug", Observed: "bug", Agree: true}),
		ev("01Y", 3, fw.EvRoutineDecided, fw.RoutineDecidedData{Routine: "acknowledge", Version: 1, Mode: fw.RoutineLive, Applies: true}),
		ev("01Y", 9, fw.EvRoutineApplied, fw.RoutineAppliedData{Routine: "acknowledge", Version: 1, OK: false, Steps: 2, FailedStep: 2, Reason: "hole empty"}),
	}
	for _, s := range []*Store{one, two, one} {
		newProjector(s, quiet()).apply(ctx, batch)
	}
	rec, _ := two.Get(ctx, "triage", "acknowledge")
	if rec.Evidence.Totals != (Counts{Decided: 1, Acted: 1, Agreed: 1, Failed: 1}) {
		t.Fatalf("totals = %+v", rec.Evidence.Totals)
	}
}

// The projection never moves the evidence back to an older version: a
// group that read the record before a new version started its evidence
// leaves that evidence as it is.
func TestProjectionKeepsTheNewerVersion(t *testing.T) {
	ctx := context.Background()
	s := memStore(Config{})
	if _, _, err := s.Put(ctx, PutRequest{Agent: "triage", Routine: ack(), Standing: Rehearsing}); err != nil {
		t.Fatal(err)
	}
	// The evidence of version 2 started, as a write of a new body does,
	// after the projection read the record at version 1.
	_, _ = s.updateEvidence(ctx, "triage", "acknowledge", func(ev *Evidence) bool {
		*ev = *newEvidence(2)
		ev.Totals.Handed = 7
		return true
	})
	e := fw.MustEvent("01OLD", fw.EvRoutineDecided, fw.RoutineDecidedData{Routine: "acknowledge", Version: 1, Mode: fw.RoutineShadow, Applies: true})
	e.Time = time.Now()
	g := &group{agent: "triage", name: "acknowledge", events: []fleet.RunEvent{{Agent: "triage", Event: e}}}
	if err := newProjector(s, quiet()).applyGroup(ctx, g); err != nil {
		t.Fatal(err)
	}
	ev, _, _ := s.evidence(ctx, "triage", "acknowledge")
	if ev.Version != 2 || ev.Totals.Handed != 7 || ev.Totals.Acted != 0 {
		t.Fatalf("the evidence = version %d, %+v", ev.Version, ev.Totals)
	}
}

// The evidence stays under the bound of an entry of the bucket: a mark
// keeps its probabilities only while a comparison can still come, and the
// oldest marks go until the value fits.
func TestEvidenceFitsTheBucket(t *testing.T) {
	ctx := context.Background()
	s := memStore(Config{})
	r := ack()
	r.Questions = map[string]fw.RoutineQuestion{}
	for i := range 10 {
		r.Questions[fmt.Sprintf("question_number_%02d_of_routine", i)] = fw.RoutineQuestion{
			Question: fw.Question{Type: fw.QuestionChoice, Criteria: map[string]any{"a": "A", "b": "B"}}, Act: 0.5,
			Observe: &fw.Observe{Answer: `answer: ([a-z]+)`}}
	}
	if _, _, err := s.Put(ctx, PutRequest{Agent: "triage", Routine: r, Standing: Rehearsing}); err != nil {
		t.Fatal(err)
	}
	long := strings.Repeat("an answer of the agent that goes on ", 20)
	at := time.Date(2026, 10, 23, 9, 0, 0, 0, time.UTC)
	ev, err := s.updateEvidence(ctx, "triage", "acknowledge", func(ev *Evidence) bool {
		ev.Version = 1
		for i := range 1200 {
			run := fmt.Sprintf("01RUN%05d", i)
			d := fw.RoutineDecidedData{Mode: fw.RoutineShadow, Applies: true, Answers: map[string]fw.Answer{}}
			for q := range r.Questions {
				d.Answers[q] = fw.Answer{Type: fw.QuestionChoice, Choice: "a", Probability: 0.75}
			}
			ev.decided(run, at, d, r)
			// Nine of ten questions come, so each mark keeps its
			// probabilities and its answers.
			for _, q := range r.QuestionNames()[:9] {
				ev.compared(run, "#1 a ticket", at, comparedData(q, long, long), r)
			}
		}
		return true
	})
	if err != nil {
		t.Fatalf("the write of a large evidence = %v", err)
	}
	value, _ := encode(ev)
	if len(value) > maxValue || len(ev.Marks) == 0 || ev.Marks[len(ev.Marks)-1].Run != "01RUN01199" || ev.Totals.Agreed != 1200 {
		t.Fatalf("the evidence holds %d bytes and %d marks, the newest %s, agreed %d", len(value), len(ev.Marks), ev.Marks[len(ev.Marks)-1].Run, ev.Totals.Agreed)
	}
	// The tenth question of the newest run comes, and its mark drops what
	// it no longer needs.
	ev, _ = s.updateEvidence(ctx, "triage", "acknowledge", func(ev *Evidence) bool {
		return ev.compared("01RUN01199", "#1 a ticket", at, comparedData(r.QuestionNames()[9], "a", "a"), r)
	})
	if m := ev.markOf("01RUN01199"); m == nil || m.P != nil || m.Answers != nil || len(m.Done) != 10 {
		t.Fatalf("the complete mark = %+v", m)
	}
}

// A bus can bring the events of a run out of order: a comparison before
// its decision, and both before the start of the run, whose agent the
// view learns from that start. The projection counts the run once, as in
// order.
func TestProjectionOutOfOrder(t *testing.T) {
	ctx := context.Background()
	s := memStore(Config{})
	if _, _, err := s.Put(ctx, PutRequest{Agent: "triage", Routine: ack(), Standing: Rehearsing}); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 10, 23, 11, 0, 0, 0, time.UTC)
	ev := func(id, agent string, typ string, data any) fleet.RunEvent {
		e := fw.MustEvent(id, typ, data)
		e.Time = at
		return fleet.RunEvent{Agent: agent, Line: "#7 a ticket", Event: e}
	}
	p := newProjector(s, quiet())
	// The first run: the comparison, then the decision, then the start,
	// each in a round of its own.
	p.apply(ctx, []fleet.RunEvent{ev("01A", "", fw.EvRoutineCompared, comparedData("type", "bug", "access"))})
	p.apply(ctx, []fleet.RunEvent{ev("01A", "", fw.EvRoutineDecided, decidedData(fw.RoutineShadow, true, 0.9))})
	p.apply(ctx, []fleet.RunEvent{ev("01A", "triage", fw.EvRunQueued, fw.RunQueuedData{Agent: "triage"})})
	// The second run: the agent is known, and the comparison comes before
	// the decision.
	p.apply(ctx, []fleet.RunEvent{ev("01B", "triage", fw.EvRoutineCompared, comparedData("type", "bug", "bug"))})
	p.apply(ctx, []fleet.RunEvent{ev("01B", "triage", fw.EvRoutineDecided, decidedData(fw.RoutineShadow, true, 0.9))})
	rec, _ := s.Get(ctx, "triage", "acknowledge")
	e := rec.Evidence
	if e.Totals != (Counts{Decided: 2, Acted: 2, Agreed: 1, Disagreed: 1}) || len(e.Disagreements) != 1 || e.Disagreements[0].Run != "01A" || e.Disagreements[0].Input != "#7 a ticket" {
		t.Fatalf("evidence = %+v, disagreements %+v", e.Totals, e.Disagreements)
	}
	if b := e.Questions["type"]; b[3].Runs != 2 || b[3].Agreed != 1 {
		t.Fatalf("bands = %+v", b)
	}
}
