package knowhow

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/fleet"
	"github.com/bdubs00/featherweight-workbench/routines"
)

// labels is the label list of the fake helpdesk.
const labels = `[{"id":11,"name":"type/bug"},{"id":12,"name":"type/outage"},{"id":13,"name":"type/access"},{"id":21,"name":"status/new"},{"id":22,"name":"status/triaged"}]`

var labelIDs = map[string]int{"bug": 11, "outage": 12, "access": 13}

// ticket is one run of the triage agent: it labels the ticket with its
// kind and status/triaged, comments, and answers TRIAGE.
type ticket struct {
	number  int
	kind    string
	comment string // replaces the comment of the agent when set
}

func (k ticket) input() string {
	return fmt.Sprintf("Ticket #%d: something about %s", k.number, k.kind)
}

func (k ticket) script() []*fw.Response {
	use := func(id, tool string, input any) fw.Block {
		b, _ := json.Marshal(input)
		return fw.Block{Type: fw.BlockToolUse, ID: id, Name: tool, Input: b}
	}
	calls := func(b ...fw.Block) *fw.Response { return &fw.Response{Content: b, StopReason: fw.StopToolUse} }
	comment := k.comment
	if comment == "" {
		comment = "Thanks. This looks like a " + k.kind + "."
	}
	return []*fw.Response{
		calls(use("c1", "gitea.label_read", map[string]any{"method": "list", "owner": "lab", "repo": "helpdesk"})),
		calls(use("c2", "gitea.issue_write", map[string]any{"method": "add_labels", "owner": "lab", "repo": "helpdesk", "issue_number": k.number, "labels": []int{labelIDs[k.kind], 22}})),
		calls(use("c3", "gitea.issue_write", map[string]any{"method": "add_comment", "owner": "lab", "repo": "helpdesk", "issue_number": k.number, "body": comment})),
		{Content: []fw.Block{fw.Text(fmt.Sprintf(`TRIAGE {"number": %d, "type": "type/%s", "status": "status/triaged"}`, k.number, k.kind))}, StopReason: fw.StopEndTurn},
	}
}

// recordRuns runs a kernel agent triage over the tickets as runs of the
// Duty nightly, and returns the events of each run that the egress stream
// carries, in order.
func recordRuns(t *testing.T, tickets []ticket) [][]fw.Event {
	t.Helper()
	scripts := map[string][]*fw.Response{}
	for _, k := range tickets {
		scripts[k.input()] = k.script()
	}
	model := fw.ModelFunc{GenerateFn: func(_ context.Context, req *fw.Request) (*fw.Response, error) {
		steps := 0
		for _, m := range req.Messages {
			if m.Role == fw.RoleAssistant {
				steps++
			}
		}
		s := scripts[fw.TextOf(req.Messages[0].Content)]
		if steps >= len(s) {
			return nil, fmt.Errorf("no response left")
		}
		return s[steps], nil
	}}
	tool := func(name, result string) fw.Tool {
		return fw.NewTool(fw.ToolSpec{Name: name}, func(context.Context, json.RawMessage) (fw.ToolResult, error) {
			return fw.TextResult(result), nil
		})
	}
	ledger := fw.NewMemLedger()
	a, err := fw.New(fw.WithoutDefaults(), fw.WithAuthn(fw.NoAuthn{}), fw.WithModelName("anthropic/test"), fw.WithModel(model),
		fw.WithLedger(ledger), fw.WithBus(fw.NewMemBus()), fw.WithTools(tool("gitea.label_read", labels), tool("gitea.issue_write", `{"ok": true}`)),
		fw.WithProfile(fw.AgentProfile{Name: "triage"}), fw.WithLogger(quiet()))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	defer a.Close(ctx)
	var out [][]fw.Event
	for _, k := range tickets {
		r, err := a.Run(ctx, k.input(), fw.AsAgent("triage"), fw.WithPrincipalName("duty:nightly"), fw.WithMetadata(map[string]string{fw.MetaDuty: "nightly"}))
		if err != nil || r.State != fw.StateCompleted {
			t.Fatalf("run %d: %+v, %v", k.number, r, err)
		}
		var evs []fw.Event
		for e, err := range ledger.Read(ctx, r.ID, 0) {
			if err != nil {
				t.Fatal(err)
			}
			if slices.Contains(fleet.EgressTypes, e.Type) {
				evs = append(evs, e)
			}
		}
		out = append(out, evs)
	}
	return out
}

// withReceipt appends a receipt to the events of a run.
func withReceipt(events []fw.Event, verification string) []fw.Event {
	last := events[len(events)-1]
	e := fw.MustEvent(last.Subject, fw.EvRunAnnotated, fw.QualityAnnotatedData{Quality: &fw.QualityObservation{ID: "r-" + last.Subject, Completion: last.Seq, Verification: verification}})
	e.Seq, e.Time = last.Seq+1, last.Time.Add(time.Second)
	return append(slices.Clone(events), e)
}

// tickets returns n tickets over three kinds.
func tickets(n int) []ticket {
	kinds := []string{"bug", "outage", "access"}
	var out []ticket
	for i := 0; i < n; i++ {
		out = append(out, ticket{number: 30 + i, kind: kinds[i%3]})
	}
	return out
}

// proposeFront is a front with the decider clef for the questions of a
// proposal, and the runs in its archive.
func proposeFront(t *testing.T, runs [][]fw.Event) *front {
	t.Helper()
	fr := newFront(t, func(c *KnowhowConfig) { c.Propose.Decider = "clef" })
	for _, evs := range runs {
		fr.arch.put(evs, day0)
	}
	return fr
}

// Twelve completed runs of a Duty, two of them verified, give a routine
// bound to the Duty that rehearses at once; the journal says proposed and
// rehearsing with the verified runs. A second pass proposes nothing new.
func TestProposeFromRecordedRuns(t *testing.T) {
	runs := recordRuns(t, tickets(12))
	runs[0] = withReceipt(runs[0], "accepted")
	runs[1] = withReceipt(runs[1], "satisfied")
	fr := proposeFront(t, runs)
	fr.clk.add(24 * time.Hour)
	ctx := context.Background()
	res, err := fr.s.Propose(ctx)
	if err != nil || len(res.Proposed) != 1 || res.Proposed[0] != "triage/duty-nightly" {
		t.Fatalf("Propose = %+v, %v", res, err)
	}
	rec, err := routines.Of(fr.a).Get(ctx, "triage", "duty-nightly")
	if err != nil || rec.Standing != routines.Rehearsing || rec.Routine.Duty != "nightly" || rec.Source == nil || rec.Source.Runs != 12 || rec.Source.ProposedBy != ProposedBy {
		t.Fatalf("the routine = %+v, %v", rec, err)
	}
	j := fr.journal(t)
	if len(j) != 2 || j[0].What != WhatRehearsing || j[1].What != WhatProposed || j[1].Subject.Routine != "duty-nightly" || len(j[1].Runs) != 2 || !strings.Contains(j[1].Why, "12 runs of the Duty nightly") {
		t.Fatalf("journal = %+v", j)
	}
	if res, _ := fr.s.Propose(ctx); len(res.Proposed) != 0 || len(fr.journal(t)) != 2 {
		t.Fatalf("a second pass = %+v", res)
	}
	// The move to rehearsing cannot go back to proposed, so its undo
	// answers 409.
	if _, err := fr.s.TakeBack(ctx, j[0].ID, "person:alice", true); err == nil {
		t.Fatal("an undo of a move that the table forbids passed")
	}
}

// One verified run is too few, and a rejected run is never used: with it
// left out, too few runs remain.
func TestProposeNeedsVerifiedRuns(t *testing.T) {
	runs := recordRuns(t, tickets(10))
	runs[0] = withReceipt(runs[0], "accepted")
	fr := proposeFront(t, runs)
	ctx := context.Background()
	if res, err := fr.s.Propose(ctx); err != nil || len(res.Proposed) != 0 || len(res.StaysOut) != 0 || len(fr.journal(t)) != 0 {
		t.Fatalf("one verified run = %+v, %v", res, err)
	}
	runs[1] = withReceipt(runs[1], "accepted")
	runs[2] = withReceipt(runs[2], "rejected")
	fr = proposeFront(t, runs)
	if res, err := fr.s.Propose(ctx); err != nil || len(res.Proposed) != 0 {
		t.Fatalf("nine usable runs = %+v, %v", res, err)
	}
}

// withDone sets the definition of done on the newest end of a run: a pass,
// weak or not.
func withDone(events []fw.Event, weak bool) []fw.Event {
	out := slices.Clone(events)
	for i := len(out) - 1; i >= 0; i-- {
		if out[i].Type != fw.EvRunFinished {
			continue
		}
		var d fw.RunFinishedData
		if err := out[i].Decode(&d); err != nil {
			panic(err)
		}
		d.Done, d.DoneWeak, d.Stop = fw.DoneChecked, weak, fw.RunStopChecked
		if weak {
			d.Stop = fw.RunStopAnswered
		}
		raw, err := json.Marshal(d)
		if err != nil {
			panic(err)
		}
		out[i].Data = raw
		break
	}
	return out
}

// A weak pass of done verifies nothing, so it does not count toward
// minVerified. Two checked runs that are not weak do.
func TestProposeWeakPassVerifiesNothing(t *testing.T) {
	runs := recordRuns(t, tickets(12))
	runs[0] = withDone(runs[0], true)
	runs[1] = withDone(runs[1], true)
	fr := proposeFront(t, runs)
	ctx := context.Background()
	if res, err := fr.s.Propose(ctx); err != nil || len(res.Proposed) != 0 {
		t.Fatalf("two weak passes = %+v, %v", res, err)
	}
	runs[0] = withDone(runs[0], false)
	runs[1] = withDone(runs[1], false)
	fr = proposeFront(t, runs)
	if res, err := fr.s.Propose(ctx); err != nil || len(res.Proposed) != 1 {
		t.Fatalf("two checked runs = %+v, %v", res, err)
	}
}

// A proposal with a value that no run explains stays out, and the journal
// says why once.
func TestProposeStaysOut(t *testing.T) {
	list := tickets(12)
	for i := range list {
		list[i].comment = fmt.Sprintf("Note %d of the day: %s", i*7, strings.Repeat("z", i))
	}
	runs := recordRuns(t, list)
	runs[0] = withReceipt(runs[0], "accepted")
	runs[1] = withReceipt(runs[1], "accepted")
	fr := proposeFront(t, runs)
	ctx := context.Background()
	res, err := fr.s.Propose(ctx)
	if err != nil || len(res.Proposed) != 0 || res.StaysOut["triage/nightly"] == "" {
		t.Fatalf("Propose = %+v, %v", res, err)
	}
	j := fr.journal(t)
	if len(j) != 1 || j[0].What != WhatStaysOut || !strings.Contains(j[0].Why, "cannot fill") {
		t.Fatalf("journal = %+v", j)
	}
	if _, err := routines.Of(fr.a).Get(ctx, "triage", "duty-nightly"); err == nil {
		t.Fatal("a proposal with a free hole reached the store")
	}
	if _, err := fr.s.Propose(ctx); err != nil || len(fr.journal(t)) != 1 {
		t.Fatalf("the same reason wrote the journal again: %d entries", len(fr.journal(t)))
	}
}

// With the switch on, a proposal stays proposed and the move to
// rehearsing waits for a person, who keeps it.
func TestProposeAsksFirst(t *testing.T) {
	runs := recordRuns(t, tickets(12))
	runs[0] = withReceipt(runs[0], "accepted")
	runs[1] = withReceipt(runs[1], "accepted")
	fr := proposeFront(t, runs)
	ctx := context.Background()
	if _, err := fr.s.SetAskFirst(ctx, "triage", true, "person:alice"); err != nil {
		t.Fatal(err)
	}
	if res, err := fr.s.Propose(ctx); err != nil || len(res.Proposed) != 1 {
		t.Fatalf("Propose = %+v, %v", res, err)
	}
	st := routines.Of(fr.a)
	if rec, _ := st.Get(ctx, "triage", "duty-nightly"); rec.Standing != routines.Proposed {
		t.Fatalf("under the switch the routine is %s", rec.Standing)
	}
	w, err := fr.s.WaitingList(ctx, "triage", nil)
	if err != nil || len(w) != 1 || w[0].What != WhatRehearsing || w[0].Pin != "v1" {
		t.Fatalf("waiting = %+v, %v", w, err)
	}
	if _, err := fr.s.Decide(ctx, w[0].ID, true, "person:alice"); err != nil {
		t.Fatal(err)
	}
	if rec, _ := st.Get(ctx, "triage", "duty-nightly"); rec.Standing != routines.Rehearsing {
		t.Fatalf("after the keep the routine is %s", rec.Standing)
	}
	if j := fr.journal(t); j[0].What != WhatRehearsing || j[0].By != "person:alice" {
		t.Fatalf("journal = %+v", j[0])
	}
}

// A move of a routine through the hook writes the journal, a step back
// names its run, and a move that this package made writes once. An undo
// of a pause makes the opposite move.
func TestRoutineMoved(t *testing.T) {
	fr := newFront(t, nil)
	ctx := context.Background()
	run := fw.NewID()
	RoutineMoved(ctx, "triage", "ack", routines.Live, routines.Rehearsing, "routines", "the run "+run+" disagreed")
	RoutineMoved(withSelf(ctx), "triage", "ack", routines.Proposed, routines.Rehearsing, ByKnowhow, "")
	j := fr.journal(t)
	if len(j) != 1 || j[0].What != WhatSteppedBack || !slices.Equal(j[0].Runs, []string{run}) || j[0].Subject.Routine != "ack" {
		t.Fatalf("journal = %+v", j)
	}
	st := routines.Of(fr.a)
	body := fw.Routine{Name: "ack", Steps: []fw.RoutineStep{{Tool: "gitea.label_read", Input: `{"method": "list"}`}}, Answer: "done"}
	if _, _, err := st.Put(ctx, routines.PutRequest{Agent: "triage", Routine: body, Standing: routines.Rehearsing, By: "alice"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.Move(ctx, routines.MoveRequest{Agent: "triage", Name: "ack", To: routines.Paused, By: "alice"}); err != nil {
		t.Fatal(err)
	}
	RoutineMoved(ctx, "triage", "ack", routines.Rehearsing, routines.Paused, "person:alice", "")
	j = fr.journal(t)
	if j[0].What != WhatPaused || j[0].Undo == nil || j[0].Undo.Version != 1 {
		t.Fatalf("journal = %+v", j[0])
	}
	if _, err := fr.s.TakeBack(ctx, j[0].ID, "person:alice", true); err != nil {
		t.Fatal(err)
	}
	if rec, _ := st.Get(ctx, "triage", "ack"); rec.Standing != routines.Rehearsing {
		t.Fatalf("after the undo the routine is %s", rec.Standing)
	}
	if RoutineName("Nightly Sweep!") != "duty-nightly-sweep" {
		t.Fatalf("RoutineName = %q", RoutineName("Nightly Sweep!"))
	}
}

// An owner starts the pass at once: POST /v1/knowhow/propose for the
// agents that the owner owns, and POST /v1/knowhow/propose/{agent} for one
// agent. A viewer and an operator cannot, a person who cannot view the
// agent learns nothing, a query agent gets 400 instead of a wider pass,
// and with proposals off the route says so.
func TestProposeRoute(t *testing.T) {
	runs := recordRuns(t, tickets(12))
	runs[0] = withReceipt(runs[0], "accepted")
	runs[1] = withReceipt(runs[1], "accepted")
	fr := proposeFront(t, runs)
	url := fr.url + "/v1/knowhow/propose"
	for who, want := range map[string]int{"carol": http.StatusForbidden, "bob": http.StatusForbidden, "dave": http.StatusNotFound, "": http.StatusUnauthorized} {
		resp, body := send(t, http.MethodPost, url+"/triage", who, "")
		mustStatus(t, resp, body, want)
	}
	resp, body := send(t, http.MethodPost, url+"?agent=triage", "alice", "")
	mustStatus(t, resp, body, http.StatusBadRequest)
	if _, err := routines.Of(fr.a).Get(context.Background(), "triage", "duty-nightly"); err == nil {
		t.Fatal("a query agent ran the pass")
	}
	// dave owns only private, which has no Duty with runs.
	resp, body = send(t, http.MethodPost, url, "dave", "")
	mustStatus(t, resp, body, http.StatusOK)
	var res ProposeResult
	decodeJSON(t, body, &res)
	if len(res.Proposed) != 0 || len(res.StaysOut) != 0 {
		t.Fatalf("the pass of dave = %+v", res)
	}
	if _, err := routines.Of(fr.a).Get(context.Background(), "triage", "duty-nightly"); err == nil {
		t.Fatal("the pass of dave proposed a routine of an agent that dave does not own")
	}
	resp, body = send(t, http.MethodPost, url+"/triage", "alice", "")
	mustStatus(t, resp, body, http.StatusOK)
	decodeJSON(t, body, &res)
	if len(res.Proposed) != 1 || res.Proposed[0] != "triage/duty-nightly" || !strings.Contains(body, `"agent":"triage"`) {
		t.Fatalf("the pass of alice = %s", body)
	}
	rec, err := routines.Of(fr.a).Get(context.Background(), "triage", "duty-nightly")
	if err != nil || rec.Standing != routines.Rehearsing {
		t.Fatalf("the routine = %+v, %v", rec, err)
	}
	resp, body = send(t, http.MethodPost, url, "alice", "")
	mustStatus(t, resp, body, http.StatusOK)
	if decodeJSON(t, body, &res); len(res.Proposed) != 0 {
		t.Fatalf("a second pass = %s", body)
	}

	off := false
	fr = newFront(t, func(c *KnowhowConfig) { c.Propose.Enabled = &off })
	for _, path := range []string{"/v1/knowhow/propose", "/v1/knowhow/propose/triage"} {
		resp, body = send(t, http.MethodPost, fr.url+path, "alice", "")
		mustStatus(t, resp, body, http.StatusConflict)
		if !strings.Contains(body, "The proposals of routines are off") {
			t.Fatalf("the answer of %s with proposals off = %s", path, body)
		}
	}
}
