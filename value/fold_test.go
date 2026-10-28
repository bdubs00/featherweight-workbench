package value

import (
	"strings"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/duties"
	"github.com/bdubs00/featherweight-workbench/fleet"
)

var now = time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)

func run(id, agent string, state fw.RunState, ago time.Duration, cost float64, meta map[string]string) *fw.Run {
	return &fw.Run{ID: id, Agent: agent, State: state, CreatedAt: now.Add(-ago), Metadata: meta,
		Usage: fw.RunUsage{Usage: fw.Usage{InputTokens: 100, OutputTokens: 10}, Turns: 1, CostUSD: cost}}
}

func facts(ago time.Duration, lead time.Duration, acts int, verdict string) fleet.Work {
	q := now.Add(-ago)
	return fleet.Work{Facts: true, Queued: q, FirstEnd: q.Add(lead), FirstOutcome: "completed", Acts: acts, Verdict: verdict}
}

// The fold counts pieces of work, not runs: a child run and a branch add
// their cost to their piece, the runs of the manager are coordination, and
// a check run is no piece. A done piece is accepted, sent back, or
// unchecked. A piece without facts makes the answer partial, and a run
// with tokens without a price makes the spend a lower bound.
func TestFold(t *testing.T) {
	h := time.Hour
	duty := map[string]string{duties.MetaDuty: "nightly-report"}
	nightly1 := run("D1", "scribe", fw.StateCompleted, 26*h, 0.10, duty)
	nightly2 := run("D2", "scribe", fw.StateCompleted, 2*h, 0.10, duty)
	nightly3 := run("D3", "scribe", fw.StateFailed, 50*h, 0.05, duty)
	ask := run("A1", "triage", fw.StateCompleted, 3*h, 0.20, nil)
	child := run("C1", "triage", fw.StateCompleted, 3*h, 0.30, nil)
	child.Parent = "A1"
	branch := run("B1", "triage", fw.StateCompleted, 3*h, 0.40, map[string]string{"fw.forkOf": "A1"})
	waiting := run("W1", "triage", fw.StateInputRequired, 5*h, 0.01, nil)
	free := run("F1", "triage", fw.StateCompleted, 4*h, 0, nil) // tokens, no price
	mgr := run("M1", "manager", fw.StateCompleted, 1*h, 0.50, nil)
	old := run("O1", "triage", fw.StateCompleted, 9*h, 0.02, nil) // no facts
	check := run("K1", "triage", fw.StateCompleted, 1*h, 0.01, map[string]string{fw.MetaKind: "check"})
	lost := run("L1", "triage", fw.StateInputRequired, 6*h, 0, nil)
	lost.Usage = fw.RunUsage{} // it waited from its first turn
	runs := []fleet.WorkRun{
		{Run: nightly1, Work: facts(26*h, 10*time.Minute, 0, fleet.VerdictAccepted)},
		{Run: nightly2, Work: facts(2*h, 20*time.Minute, 0, "")},
		{Run: nightly3, Work: fleet.Work{Facts: true, Queued: now.Add(-50 * h)}},
		{Run: ask, Work: func() fleet.Work {
			w := facts(3*h, 60*time.Minute, 2, fleet.VerdictRejected)
			w.WaitMS, w.Waits, w.Rework = (30 * time.Minute).Milliseconds(), 1, 1
			return w
		}()},
		{Run: child}, {Run: branch},
		{Run: waiting, Work: fleet.Work{Facts: true, Queued: now.Add(-5 * h), Asked: now.Add(-2 * h)}},
		{Run: free, Work: facts(4*h, 5*time.Minute, 0, "")},
		{Run: mgr}, {Run: old}, {Run: check},
		{Run: lost, Work: fleet.Work{Facts: true, Queued: now.Add(-6 * h), Asked: now.Add(-6 * h)}, Lost: true},
	}
	a := Fold(runs, Options{Now: now, Days: 7, Coordination: map[string]bool{"manager": true},
		Estimates: map[string]Estimate{"duty:nightly-report": {Minutes: 45, By: "alice", At: now}}})

	if a.Work != (Counts{Pieces: 8, Done: 5, Failed: 1, Open: 1, Lost: 1}) {
		t.Fatalf("the counts = %+v", a.Work)
	}
	if a.Checks != (Checks{Accepted: 1, SentBack: 1, Unchecked: 3}) {
		t.Fatalf("the checks = %+v", a.Checks)
	}
	if !a.Partial || a.Unknown != 1 {
		t.Fatalf("partial %v, unknown %d; want true, 1", a.Partial, a.Unknown)
	}
	if a.People.Actions != 2 || a.People.NoPerson != 3 || a.People.Reworked != 1 || a.People.WaitingNow != 1 {
		t.Fatalf("the people = %+v", a.People)
	}
	if want := int64(30*60 + 2*3600); a.People.WaitSeconds != want {
		t.Fatalf("the wait = %d s, want %d", a.People.WaitSeconds, want)
	}
	// The lead times are 10, 20, 60, and 5 minutes: the median is 15.
	if a.Lead.Measured != 4 || a.Lead.MedianSeconds != 15*60 {
		t.Fatalf("the lead = %+v", a.Lead)
	}
	if a.Lead.WaitShare != 0.316 { // 30 of 95 minutes
		t.Fatalf("the wait share = %v", a.Lead.WaitShare)
	}
	if a.Spend.USD != 1.69 || a.Spend.CoordinationUSD != 0.5 || a.Spend.FailedUSD != 0.05 || !a.Spend.AtLeast || a.Spend.Unpriced != 1 {
		t.Fatalf("the spend = %+v", a.Spend)
	}
	if a.Spend.PerDone != 0.338 {
		t.Fatalf("the spend per done piece = %v", a.Spend.PerDone)
	}
	if len(a.Lines) != 2 || a.Lines[0].Key != "agent:triage" || a.Lines[1].Key != "duty:nightly-report" {
		t.Fatalf("the lines = %+v", a.Lines)
	}
	tri, rep := a.Lines[0], a.Lines[1]
	if tri.USD != 0.93 || tri.Counts.Done != 3 || tri.Counts.Open != 1 || tri.Unpriced != 1 {
		t.Fatalf("the line of triage = %+v", tri)
	}
	if rep.Estimate == nil || rep.EstimatedMinutes != 90 || a.Estimated == nil || a.Estimated.Minutes != 90 || a.Estimated.Pieces != 2 {
		t.Fatalf("the estimate = %+v, %+v", rep, a.Estimated)
	}
	if len(a.Periods) != 7 || a.Periods[6].Done != 4 || a.Periods[5].Done != 1 || a.Periods[4].Failed != 1 {
		t.Fatalf("the periods = %+v", a.Periods)
	}
	for _, want := range []string{
		"In the last 7 days, the agents finished 5 pieces of work, and 1 failed or stopped. 1 piece was lost when a pool replaced the worker.",
		"People accepted 1 and sent back 1; 3 are unchecked.",
		"3 of the 5 finished without an action of a person.",
		"Work waited 2 h 30 min for people.",
		"The work cost at least $1.69, $0.34 for each finished piece.",
		"1 run has tokens without a price.",
		"would take a person 1 h 30 min.",
		"1 piece has no record of the actions of people.",
	} {
		if !strings.Contains(a.Sentence, want) {
			t.Errorf("the sentence lacks %q:\n%s", want, a.Sentence)
		}
	}

	empty := Fold(nil, Options{Now: now, Days: 30})
	if empty.Sentence != "In the last 30 days, the agents finished no work that you can view." || len(empty.Periods) != 5 || empty.Lines == nil {
		t.Fatalf("an empty fold = %+v", empty)
	}
}

func TestDuration(t *testing.T) {
	for s, want := range map[int64]string{40: "40 s", 720: "12 min", 3600: "1 h", 11400: "3 h 10 min"} {
		if got := Duration(s); got != want {
			t.Errorf("Duration(%d) = %q, want %q", s, got, want)
		}
	}
}
