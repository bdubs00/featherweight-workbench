package routines

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/pages"
)

// body is the JSON of a PUT with the routine of the helpdesk.
func body(t *testing.T, standing string, mut func(*fw.Routine)) string {
	t.Helper()
	r := ack()
	if mut != nil {
		mut(&r)
	}
	b, err := json.Marshal(map[string]any{"routine": r, "standing": standing, "source": Source{Runs: 212, From: "2026-09-14", To: "2026-10-21", ProposedBy: "fw routine propose"}})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// The routes read with the right to view the agent and write with the
// right to edit it, as agent_put decides; an operator proposes a new
// routine. A caller who cannot view the agent gets 404. A move that the
// table forbids answers 409 with the allowed moves, and a sign-off writes
// the runbook to the wiki as the front.
func TestRoutes(t *testing.T) {
	fr := newFront(t, withClef)
	base := fr.url + "/v1/routines"
	one := base + "/triage/acknowledge"

	if resp, _ := send(t, http.MethodGet, base+"?agent=triage", "", ""); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a list without a token = %d", resp.StatusCode)
	}
	// bob operates triage: he proposes, and he cannot start a rehearsal.
	if resp, b := send(t, http.MethodPut, one, "bob", body(t, Rehearsing, nil)); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("an operator starts a rehearsal = %d %s", resp.StatusCode, b)
	}
	if resp, b := send(t, http.MethodPut, one, "carol", body(t, "", nil)); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("a caller without a role = %d %s", resp.StatusCode, b)
	}
	resp, b := send(t, http.MethodPut, one, "bob", body(t, "", nil))
	if resp.StatusCode != http.StatusCreated || resp.Header.Get("Location") != "/v1/routines/triage/acknowledge" {
		t.Fatalf("an operator proposes = %d %s", resp.StatusCode, b)
	}
	var rec map[string]any
	decodeJSON(t, b, &rec)
	if rec["standing"] != Proposed || rec["agent"] != "triage" || rec["ready"] != false || rec["source"].(map[string]any)["runs"] != float64(212) {
		t.Fatalf("the new record = %s", b)
	}
	if _, ok := rec["routine"].(map[string]any)["standing"]; ok {
		t.Fatalf("the body holds a standing: %s", b)
	}
	// An operator changes no body; the owner does, and the version goes up.
	if resp, b := send(t, http.MethodPut, one, "bob", body(t, "", func(r *fw.Routine) { r.Answer = "x" })); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("an operator changes a body = %d %s", resp.StatusCode, b)
	}
	resp, b = send(t, http.MethodPut, one, "alice", body(t, "", func(r *fw.Routine) { r.Answer = "TRIAGE {{ .type }}" }))
	if resp.StatusCode != http.StatusOK || !strings.Contains(b, `"version":2`) {
		t.Fatalf("the owner changes the body = %d %s", resp.StatusCode, b)
	}
	for _, c := range []struct{ body, why string }{
		{`{"standing": "proposed"}`, "routine is missing"},
		{body(t, "", func(r *fw.Routine) { r.Name = "other" }), "in the path"},
		{body(t, "", func(r *fw.Routine) { r.Steps[0].Tool = "" }), "tool is empty"},
	} {
		if resp, b := send(t, http.MethodPut, one, "alice", c.body); resp.StatusCode != http.StatusBadRequest || !strings.Contains(b, c.why) {
			t.Errorf("PUT %s = %d %s", c.body, resp.StatusCode, b)
		}
	}

	// The list and the record with its evidence.
	resp, b = send(t, http.MethodGet, base+"?agent=triage", "bob", "")
	var list struct {
		Routines []map[string]any `json:"routines"`
	}
	decodeJSON(t, b, &list)
	if resp.StatusCode != http.StatusOK || len(list.Routines) != 1 || list.Routines[0]["evidence"] != nil {
		t.Fatalf("the list = %d %s", resp.StatusCode, b)
	}
	resp, b = send(t, http.MethodGet, one, "bob", "")
	if resp.StatusCode != http.StatusOK || !strings.Contains(b, `"evidence":{"version":2,"totals":{"decided":0`) || strings.Contains(b, `"marks"`) {
		t.Fatalf("one routine = %d %s", resp.StatusCode, b)
	}
	for _, url := range []string{base + "?agent=private", fr.url + "/v1/routines/private/acknowledge", base + "?agent=nobody", fr.url + "/v1/routines/triage/nothing"} {
		if resp, b := send(t, http.MethodGet, url, "alice", ""); resp.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s = %d %s", url, resp.StatusCode, b)
		}
	}

	// The moves.
	standing := one + "/standing"
	if resp, b := send(t, http.MethodPost, standing, "bob", `{"standing": "rehearsing"}`); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("an operator moves = %d %s", resp.StatusCode, b)
	}
	resp, b = send(t, http.MethodPost, standing, "alice", `{"standing": "live"}`)
	var problem struct {
		Status   int      `json:"status"`
		Standing string   `json:"standing"`
		Allowed  []string `json:"allowed"`
		Detail   string   `json:"detail"`
	}
	decodeJSON(t, b, &problem)
	if resp.StatusCode != http.StatusConflict || problem.Standing != Proposed || !slices.Equal(problem.Allowed, []string{Rehearsing, Retired}) || !strings.Contains(problem.Detail, "rehearsing or retired") {
		t.Fatalf("proposed to live = %d %s", resp.StatusCode, b)
	}
	if resp, b := send(t, http.MethodPost, standing, "alice", `{"standing": "rehearsing", "note": "looks right"}`); resp.StatusCode != http.StatusOK || !strings.Contains(b, `"standing":"rehearsing"`) {
		t.Fatalf("to rehearsing = %d %s", resp.StatusCode, b)
	}
	// A move to the standing that the routine has answers 200 and changes
	// nothing: no new move, no new time.
	_, before := send(t, http.MethodGet, one, "alice", "")
	resp, b = send(t, http.MethodPost, standing, "alice", `{"standing": "rehearsing", "note": "again"}`)
	var same, was map[string]any
	decodeJSON(t, b, &same)
	decodeJSON(t, before, &was)
	if resp.StatusCode != http.StatusOK || len(same["history"].([]any)) != 2 || same["updatedAt"] != was["updatedAt"] {
		t.Fatalf("a move to the same standing = %d %s", resp.StatusCode, b)
	}
	// A person signs off: neither the static token nor a workload that
	// owns the agent can.
	for _, who := range []string{"fleet", "robot"} {
		if resp, b := send(t, http.MethodPost, standing, who, `{"standing": "live"}`); resp.StatusCode != http.StatusForbidden {
			t.Fatalf("%s signs off = %d %s", who, resp.StatusCode, b)
		}
	}
	// A sign-off names the version that the person read; another version
	// answers 409 with the version and the standing of the routine.
	resp, b = send(t, http.MethodPost, standing, "alice", `{"standing": "live", "version": 1}`)
	var vp struct {
		Status   int    `json:"status"`
		Detail   string `json:"detail"`
		Standing string `json:"standing"`
		Version  int    `json:"version"`
	}
	decodeJSON(t, b, &vp)
	if resp.StatusCode != http.StatusConflict || vp.Version != 2 || vp.Standing != Rehearsing || vp.Detail != "the move names version 1, and the routine is at version 2 now. Read it again" {
		t.Fatalf("a sign-off of an older version = %d %s", resp.StatusCode, b)
	}
	if resp, b := send(t, http.MethodPost, standing, "alice", `{"standing": "sideways"}`); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("an unknown standing = %d %s", resp.StatusCode, b)
	}
	resp, b = send(t, http.MethodPost, standing, "alice", `{"standing": "live", "note": "the evidence is good", "version": 2}`)
	decodeJSON(t, b, &rec)
	if resp.StatusCode != http.StatusOK || rec["standing"] != Live || rec["signedOff"].(map[string]any)["by"] != "alice" {
		t.Fatalf("the sign-off = %d %s", resp.StatusCode, b)
	}
	page, err := pages.Of(fr.a).Read(context.Background(), RunbookPath("triage", "acknowledge"), 0)
	if err != nil || page.By != pages.ByFront || page.Run != "" {
		t.Fatalf("the runbook = %+v, %v", page, err)
	}
	for _, want := range []string{"# acknowledge", "Signed off by alice", "`gitea.issue_write` (add_labels)", "at least 85% sure, and only for bug, access", "at least 95% sure that the answer is no", "The result holds `status/triaged`", "Made from 212 runs of triage between 2026-09-14 and 2026-10-21"} {
		if !strings.Contains(page.Text, want) {
			t.Errorf("the runbook lacks %q:\n%s", want, page.Text)
		}
	}
	for _, never := range []string{"shadow", "decider", "procedure", "template"} {
		if strings.Contains(strings.ToLower(page.Text), never) {
			t.Errorf("the runbook says %q:\n%s", never, page.Text)
		}
	}
	h := rec["history"].([]any)
	if len(h) != 3 || h[2].(map[string]any)["note"] != "the evidence is good" {
		t.Fatalf("history = %v", h)
	}
}

// Without agent, the list holds the routines of every agent that the
// caller can view, the newest change first. An agent that the caller
// cannot view, and an agent of the store that the fleet no longer runs,
// stay out. A named agent that the caller cannot view answers 404.
func TestListEveryAgent(t *testing.T) {
	fr := newFront(t, nil)
	ctx := context.Background()
	r := fw.Routine{Steps: []fw.RoutineStep{{Tool: "files", Input: "{}"}}}
	for _, c := range []struct{ agent, name string }{{"triage", "first"}, {"private", "secret"}, {"gone", "old"}, {"triage", "second"}} {
		r.Name = c.name
		if _, _, err := fr.s.Put(ctx, PutRequest{Agent: c.agent, Routine: r}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(2 * time.Millisecond)
	}
	names := func(who, query string) (int, []string) {
		resp, b := send(t, http.MethodGet, fr.url+"/v1/routines"+query, who, "")
		var list struct {
			Routines []Record `json:"routines"`
		}
		if resp.StatusCode == http.StatusOK {
			decodeJSON(t, b, &list)
		}
		var out []string
		for _, rec := range list.Routines {
			out = append(out, rec.Agent+"/"+rec.Name())
		}
		return resp.StatusCode, out
	}
	if code, got := names("alice", ""); code != http.StatusOK || !slices.Equal(got, []string{"triage/second", "triage/first"}) {
		t.Fatalf("alice = %d %v", code, got)
	}
	if code, got := names("dave", ""); code != http.StatusOK || !slices.Equal(got, []string{"private/secret"}) {
		t.Fatalf("dave = %d %v", code, got)
	}
	if code, got := names("carol", ""); code != http.StatusOK || len(got) != 0 {
		t.Fatalf("carol = %d %v", code, got)
	}
	if code, got := names("fleet", ""); code != http.StatusOK || len(got) != 3 {
		t.Fatalf("the static token = %d %v", code, got)
	}
	if code, got := names("bob", "?agent=triage"); code != http.StatusOK || len(got) != 2 {
		t.Fatalf("bob for triage = %d %v", code, got)
	}
	if code, _ := names("alice", "?agent=private"); code != http.StatusNotFound {
		t.Fatalf("alice for private = %d", code)
	}
	res := call(t, connectMCP(t, fr.a, "dave"), ToolList, map[string]any{})
	if res.IsError || !strings.Contains(textOf(res), "private: secret") || strings.Contains(textOf(res), "triage") {
		t.Fatalf("routines_list without agent = %s", textOf(res))
	}
}

// The tools offer the same reads and moves, with the same rights.
func TestTools(t *testing.T) {
	fr := newFront(t, withClef)
	if _, _, err := fr.s.Put(context.Background(), PutRequest{Agent: "triage", Routine: ack(), Standing: Rehearsing, By: "alice"}); err != nil {
		t.Fatal(err)
	}
	bob := connectMCP(t, fr.a, "bob")
	res := call(t, bob, ToolList, map[string]any{"agent": "triage"})
	var got listResult
	structured(t, res, &got)
	if res.IsError || len(got.Routines) != 1 || !strings.Contains(textOf(res), "acknowledge, version 1: rehearsing") {
		t.Fatalf("routines_list = %s", textOf(res))
	}
	res = call(t, bob, ToolList, map[string]any{"agent": "triage", "name": "acknowledge"})
	if res.IsError || !strings.Contains(textOf(res), "would have acted on 0 runs") {
		t.Fatalf("routines_list with a name = %s", textOf(res))
	}
	if res := call(t, bob, ToolStanding, map[string]any{"agent": "triage", "name": "acknowledge", "standing": "paused"}); !res.IsError {
		t.Fatalf("an operator moves with the tool: %s", textOf(res))
	}
	if res := call(t, connectMCP(t, fr.a, "carol"), ToolList, map[string]any{"agent": "triage"}); !res.IsError || !strings.Contains(textOf(res), "Not found") {
		t.Fatalf("a caller without a role = %s", textOf(res))
	}
	alice := connectMCP(t, fr.a, "alice")
	res = call(t, alice, ToolStanding, map[string]any{"agent": "triage", "name": "acknowledge", "standing": "paused", "note": "holiday"})
	var moved standingResult
	structured(t, res, &moved)
	if res.IsError || !moved.Moved || moved.Routine.Standing != Paused {
		t.Fatalf("routine_standing = %s", textOf(res))
	}
	res = call(t, alice, ToolStanding, map[string]any{"agent": "triage", "name": "acknowledge", "standing": "retired"})
	if !res.IsError || !strings.Contains(textOf(res), "rehearsing or live") {
		t.Fatalf("a forbidden move = %s", textOf(res))
	}
}

// The summary counts the runs of an agent in the window: by a routine
// when a routine completed it, with a person when a person decided in it,
// by the agent otherwise, the cost of a run on average for a routine and
// for the agent, and the runs that came back.
func TestSummary(t *testing.T) {
	fr := newFront(t, nil)
	now := time.Now().UTC()
	queued := fw.RunQueuedData{Agent: "triage", Input: "#1 a ticket", Principal: "bob"}
	done := func(cost float64) fw.RunFinishedData {
		return fw.RunFinishedData{Outcome: fw.OutcomeCompleted, Totals: &fw.RunUsage{Turns: 1, CostUSD: cost}}
	}
	// Two runs by a routine, one by the agent, one with a person, and one
	// of another agent.
	for i, id := range []string{"01R1", "01R2"} {
		fr.event(t, now, id, 1, fw.EvRunQueued, queued)
		fr.event(t, now, id, 2, fw.EvRoutineDecided, fw.RoutineDecidedData{Routine: "acknowledge", Version: 1, Mode: fw.RoutineLive, Applies: true})
		fr.event(t, now, id, 3, fw.EvRoutineApplied, fw.RoutineAppliedData{Routine: "acknowledge", Version: 1, OK: true, Steps: 4})
		fr.event(t, now, id, 4, fw.EvRunFinished, done(0.001*float64(i+1)))
	}
	fr.event(t, now, "01AG", 1, fw.EvRunQueued, queued)
	fr.event(t, now, "01AG", 2, fw.EvRoutineApplied, fw.RoutineAppliedData{Routine: "acknowledge", Version: 1, OK: false, Reason: "check failed"})
	fr.event(t, now, "01AG", 3, fw.EvRunFinished, done(0.02))
	fr.event(t, now, "01AG", 4, fw.EvRunAnnotated, fw.QualityAnnotatedData{Quality: &fw.QualityObservation{ID: "r", Completion: 3, Verification: "rejected"}})
	fr.event(t, now, "01PE", 1, fw.EvRunQueued, queued)
	fr.event(t, now, "01PE", 2, fw.EvApprovalRequested, fw.ApprovalRequestedData{ApprovalID: "ap", Tool: "bash"})
	fr.event(t, now, "01PE", 3, fw.EvApprovalDecided, fw.ApprovalDecidedData{ApprovalID: "ap", Decision: "allow", Decider: "bob"})
	fr.event(t, now, "01PE", 4, fw.EvRunFinished, done(0.5))
	fr.event(t, now, "01OT", 1, fw.EvRunQueued, fw.RunQueuedData{Agent: "private", Input: "x", Principal: "dave"})

	resp, b := send(t, http.MethodGet, fr.url+"/v1/routines/summary?agent=triage&days=7", "bob", "")
	var got Summary
	decodeJSON(t, b, &got)
	if resp.StatusCode != http.StatusOK || got.Agent != "triage" || got.Days != 7 || got.Zone != "UTC" {
		t.Fatalf("summary = %d %s", resp.StatusCode, b)
	}
	if got.Runs != 4 || got.ByRoutine != 2 || got.ByAgent != 1 || got.WithPerson != 1 || got.SentBack != 1 || got.Partial {
		t.Fatalf("summary = %s", b)
	}
	if diff := got.CostUSD.Routine - 0.0015; diff > 1e-9 || diff < -1e-9 || got.CostUSD.Agent != 0.02 {
		t.Fatalf("cost = %+v", got.CostUSD)
	}
	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	if !got.Since.Equal(midnight.AddDate(0, 0, -6)) {
		t.Fatalf("since = %s", got.Since)
	}
	for _, q := range []string{"days=0", "days=91", "days=x"} {
		if resp, _ := send(t, http.MethodGet, fr.url+"/v1/routines/summary?agent=triage&"+q, "bob", ""); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s = %d", q, resp.StatusCode)
		}
	}
	if resp, _ := send(t, http.MethodGet, fr.url+"/v1/routines/summary?agent=private", "bob", ""); resp.StatusCode != http.StatusNotFound {
		t.Errorf("the summary of an agent that bob cannot view = %d", resp.StatusCode)
	}
}
