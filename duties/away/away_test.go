package away

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/duties"
	"github.com/bdubs00/featherweight-workbench/fleet"
	"github.com/bdubs00/featherweight-workbench/internal/bucket"
)

// An approval of a run of a duty goes to the manager in one message that
// names the duty, the tool, the input, and the reason. The front applies
// the verdict with its own authority, and the history says that the
// manager decided. The person hears nothing while the manager decides.
func TestManagerDecides(t *testing.T) {
	b := bucket.NewMem(awayBucket)
	f := newFront(t, b, newFleet(), &clock{t: t0})
	run := f.duty(t, "report", nil)
	f.asks(run, "report", "A1", "git.push", "rule: ask git.*", nil)
	f.pass(t)
	_, _, _, asked := f.fleet.lists()
	if len(asked) != 1 {
		t.Fatalf("the manager got %d questions", len(asked))
	}
	q := asked[0]
	for _, want := range []string{"the duty report", "owner alice", "the tool git.push", "rule: ask git.*", `{"command":"git push origin main"}`, "Push the report of {date}.", `{"verdict": "allow", "reason": "one sentence"}`} {
		if !strings.Contains(q.Input, want) {
			t.Errorf("the message lacks %q:\n%s", want, q.Input)
		}
	}
	if q.Agent != "manager" || q.Principal != "alice" || q.PrincipalKind != fw.KindUser || q.Budget == nil || q.Budget.MaxCostUSD != 0.5 ||
		q.Metadata[MetaVerdict] != run+"/A1" || q.IdempotencyKey != "duty-away/"+run+"/A1" || q.Metadata[fleet.MetaInternal] != "duty-verdict:report" {
		t.Fatalf("the request of the manager = %+v", q)
	}
	if w := entryOf(t, b, run, "A1"); w.Route != RouteManager || w.Manager != q.ID || w.State != stateOpen || w.Claim != "" {
		t.Fatalf("the entry = %+v", w)
	}
	f.pass(t)
	if decided, _, _, _ := f.fleet.lists(); len(decided) != 0 {
		t.Fatalf("the front decided before the verdict: %v", decided)
	}
	f.fleet.set(&fw.Run{ID: q.ID, Agent: "manager", State: fw.StateCompleted, Output: "```json\n{\"verdict\": \"allow\", \"reason\": \"The push  serves the report.\"}\n```"})
	f.pass(t)
	decided, notes, _, _ := f.fleet.lists()
	if !slices.Equal(decided, []string{run + "/A1=allow"}) || notes[0] != "The manager decided for the duty report while its owner was away: The push serves the report." {
		t.Fatalf("decided = %v, notes = %v", decided, notes)
	}
	got := f.decided(t, "report")
	if len(got) != 1 || got[0].By != duties.DecidedByManager || got[0].Verdict != "allow" || got[0].Tool != "git.push" || got[0].Reason != "The push serves the report." {
		t.Fatalf("the history = %+v", got)
	}
	if w := entryOf(t, b, run, "A1"); w.State != stateDone || w.Outcome != "manager allow" {
		t.Fatalf("the entry after the verdict = %+v", w)
	}
	if n := allNotices.of(f.a); len(n) != 0 {
		t.Fatalf("notices = %+v", n)
	}
}

// A question goes to the person with a needs-you notice, never to the
// manager. After the timeout the front denies it, records the decision,
// and tells the people of notify.needsYou.
func TestQuestionTimesOut(t *testing.T) {
	b := bucket.NewMem(awayBucket)
	c := &clock{t: t0}
	f := newFront(t, b, newFleet(), c)
	run := f.duty(t, "report", &duties.WhileAway{Decider: "manager", Timeout: "30m", Then: "deny"})
	f.asks(run, "report", "Q1", "ask_user", "Which branch?", json.RawMessage(`{"type":"object"}`))
	f.pass(t)
	if _, _, _, asked := f.fleet.lists(); len(asked) != 0 {
		t.Fatal("a question went to the manager")
	}
	n := allNotices.of(f.a)
	if len(n) != 1 || n[0].Kind != duties.NoticeNeedsYou || !slices.Equal(n[0].Who, []string{"alice", "erin"}) || n[0].Run != run ||
		n[0].Reason != "The agent triage asks you a question. Decide in Activity, or with approve. Without an answer within 30m, the front denies it." {
		t.Fatalf("notices = %+v", n)
	}
	f.pass(t)
	if n := allNotices.of(f.a); len(n) != 1 {
		t.Fatalf("the person heard of the question %d times", len(n))
	}
	c.Add(30 * time.Minute)
	f.pass(t)
	decided, notes, _, _ := f.fleet.lists()
	if !slices.Equal(decided, []string{run + "/Q1=deny"}) || !strings.Contains(notes[0], "Nobody decided within 30m, so the front denied it") {
		t.Fatalf("decided = %v %v", decided, notes)
	}
	got := f.decided(t, "report")
	if len(got) != 1 || got[0].By != duties.DecidedByTimeout || got[0].Verdict != "deny" {
		t.Fatalf("the history = %+v", got)
	}
	n = allNotices.of(f.a)
	if len(n) != 2 || n[1].Kind != duties.NoticeDecided || n[1].Reason != "Nobody answered the question of the agent triage within 30m, so the front denied the call." {
		t.Fatalf("notices = %+v", n)
	}
}

// then cancel cancels the run at the timeout.
func TestTimeoutCancels(t *testing.T) {
	b := bucket.NewMem(awayBucket)
	c := &clock{t: t0}
	f := newFront(t, b, newFleet(), c)
	run := f.duty(t, "report", &duties.WhileAway{Decider: "nobody", Timeout: "1h", Then: "cancel"})
	f.asks(run, "report", "A1", "git.push", "rule: ask git.*", nil)
	f.pass(t)
	if w := entryOf(t, b, run, "A1"); w.Route != RoutePerson || w.Why != "The duty says that nobody decides while you are away." || !w.Told {
		t.Fatalf("the entry = %+v", w)
	}
	c.Add(time.Hour)
	f.pass(t)
	_, _, canceled, _ := f.fleet.lists()
	if !slices.Equal(canceled, []string{run}) {
		t.Fatalf("canceled = %v", canceled)
	}
	if got := f.decided(t, "report"); len(got) != 1 || got[0].By != "timeout" || got[0].Verdict != "cancel" {
		t.Fatalf("the history = %+v", got)
	}
}

// A tool that the policy marks destructive, and a front without a manager,
// send the approval to the person.
func TestRoutesToThePerson(t *testing.T) {
	b := bucket.NewMem(awayBucket)
	f := newFront(t, b, newFleet(), &clock{t: t0})
	run := f.duty(t, "report", nil)
	f.asks(run, "report", "A1", "files.delete", "rule: ask destructive", nil)
	f.pass(t)
	if w := entryOf(t, b, run, "A1"); w.Route != RoutePerson || !strings.Contains(w.Why, "marks the tool files.delete destructive") {
		t.Fatalf("a destructive tool = %+v", w)
	}
	if n := allNotices.of(f.a); len(n) != 1 || !strings.Contains(n[0].Reason, "The agent triage asks to run files.delete. The policy marks the tool files.delete destructive") {
		t.Fatalf("notices = %+v", n)
	}
	f.mgr = ""
	f.asks(run, "report", "A2", "git.push", "rule: ask git.*", nil)
	f.pass(t)
	if w := entryOf(t, b, run, "A2"); w.Route != RoutePerson || !strings.Contains(w.Why, "workbench.bossmode.manager names none") {
		t.Fatalf("no manager = %+v", w)
	}
	if _, _, _, asked := f.fleet.lists(); len(asked) != 0 {
		t.Fatalf("the manager got %d questions", len(asked))
	}
}

// An answer of the manager that does not read as a verdict, and a queue
// that does not take the question, send the approval to the person.
func TestBadVerdictGoesToThePerson(t *testing.T) {
	b := bucket.NewMem(awayBucket)
	f := newFront(t, b, newFleet(), &clock{t: t0})
	run := f.duty(t, "report", nil)
	f.asks(run, "report", "A1", "git.push", "rule: ask git.*", nil)
	f.pass(t)
	_, _, _, asked := f.fleet.lists()
	f.fleet.set(&fw.Run{ID: asked[0].ID, Agent: "manager", State: fw.StateCompleted, Output: "Sure, allow it."})
	f.pass(t)
	if decided, _, _, _ := f.fleet.lists(); len(decided) != 0 {
		t.Fatalf("decided = %v", decided)
	}
	if w := entryOf(t, b, run, "A1"); w.Route != RoutePerson || !strings.Contains(w.Why, "did not read as a verdict") || !w.Told {
		t.Fatalf("the entry = %+v", w)
	}
	g := newFront(t, bucket.NewMem(awayBucket), newFleet(), &clock{t: t0})
	g.fleet.failQ = errors.New("no pool runs the manager")
	run = g.duty(t, "other", nil)
	g.asks(run, "other", "A1", "git.push", "rule: ask git.*", nil)
	g.pass(t)
	if w := entryOf(t, g.s.b, run, "A1"); w.Route != RoutePerson || w.Manager != "" || !strings.Contains(w.Why, "did not go out: no pool runs the manager") {
		t.Fatalf("the entry = %+v", w)
	}
}

// A person who decides first wins: the front decides nothing more, stops
// the run of the manager, and the history names the person.
func TestPersonDecidesFirst(t *testing.T) {
	b := bucket.NewMem(awayBucket)
	c := &clock{t: t0}
	f := newFront(t, b, newFleet(), c)
	run := f.duty(t, "report", nil)
	f.asks(run, "report", "A1", "git.push", "rule: ask git.*", nil)
	f.pass(t)
	_, _, _, asked := f.fleet.lists()
	f.fleet.set(&fw.Run{ID: run, Agent: "triage", State: fw.StateRunning, Metadata: map[string]string{duties.MetaDuty: "report"}})
	f.fleet.deciders["A1"] = [2]string{"alice", "deny"}
	f.pass(t)
	decided, _, canceled, _ := f.fleet.lists()
	if len(decided) != 0 || !slices.Equal(canceled, []string{asked[0].ID}) {
		t.Fatalf("decided = %v, canceled = %v", decided, canceled)
	}
	if got := f.decided(t, "report"); len(got) != 1 || got[0].By != "alice" || got[0].Verdict != "deny" {
		t.Fatalf("the history = %+v", got)
	}
	c.Add(3 * time.Hour)
	f.pass(t)
	if decided, _, _, _ := f.fleet.lists(); len(decided) != 0 {
		t.Fatalf("the timeout applied after a person decided: %v", decided)
	}
}

// Of two replicas on one bucket, one asks the manager, and one applies the
// timeout.
func TestOneReplicaPerApproval(t *testing.T) {
	b := bucket.NewMem(awayBucket)
	fl := newFleet()
	c := &clock{t: t0}
	f := newFront(t, b, fl, c)
	run := f.duty(t, "report", &duties.WhileAway{Decider: "manager", Timeout: "1h", Then: "deny"})
	f.asks(run, "report", "A1", "git.push", "rule: ask git.*", nil)
	// A second watch serves the duties of the same front, as a replica.
	gs := newService(f.a, b, quiet())
	gs.now, gs.replica = c.Now, "front-other"
	gs.waiting, gs.run, gs.decide, gs.cancelRun, gs.enqueue, gs.decider = fl.waiting, fl.run, fl.decide, fl.cancel, fl.enqueue, fl.decider
	gs.manager = func() string { return "manager" }
	f.pass(t)
	gs.pass(t.Context())
	f.pass(t)
	if _, _, _, asked := fl.lists(); len(asked) != 1 {
		t.Fatalf("the manager got %d questions", len(asked))
	}
	c.Add(time.Hour)
	gs.pass(t.Context())
	f.pass(t)
	if decided, _, _, _ := fl.lists(); !slices.Equal(decided, []string{run + "/A1=deny"}) {
		t.Fatalf("decided = %v", decided)
	}
	if got := f.decided(t, "report"); len(got) != 1 {
		t.Fatalf("the history holds %d decisions", len(got))
	}
}

// A verdict is one JSON object with allow or deny and a reason, and
// nothing else.
func TestParseVerdict(t *testing.T) {
	for _, c := range []struct {
		in, verdict, err string
	}{
		{`{"verdict": "deny", "reason": "Not needed."}`, "deny", ""},
		{"```json\n{\"verdict\": \"allow\", \"reason\": \"Fine.\"}\n```", "allow", ""},
		{"```{\"verdict\": \"allow\", \"reason\": \"Fine.\"}```", "allow", ""},
		{`{"verdict": "Allow", "reason": "x"}`, "", "not allow or deny"},
		{`{"verdict": "allow"}`, "", "no reason"},
		{`{"verdict": "allow", "reason": "x", "rule": true}`, "", "not the JSON object"},
		{`{"verdict": "allow", "reason": "x"} and more`, "", "more than"},
		{`I allow it.`, "", "not the JSON object"},
		{``, "", "empty"},
		{`{"verdict": "allow", "reason": "` + strings.Repeat("x", 600) + `"}`, "", "longer than"},
	} {
		v, err := ParseVerdict(c.in)
		switch {
		case c.err == "" && (err != nil || v.Verdict != c.verdict):
			t.Errorf("ParseVerdict(%q) = %+v, %v", c.in, v, err)
		case c.err != "" && (err == nil || !strings.Contains(err.Error(), c.err)):
			t.Errorf("ParseVerdict(%q) = %v, want an error with %q", c.in, err, c.err)
		}
	}
}
