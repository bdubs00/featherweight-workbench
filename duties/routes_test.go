package duties

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

// The routes read with the right to view the agent and write with the
// right to own it, as the routines decide; a caller who cannot view the
// agent gets 404, and an admin can do everything.
func TestRoutesAccess(t *testing.T) {
	f := newFront(t, "", nil)
	base := f.url + "/v1/duties"
	one := base + "/report"
	body := mustJSON(t, daily("report"))
	if resp, _ := send(t, http.MethodGet, base, "", ""); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a list without a token = %d", resp.StatusCode)
	}
	for who, want := range map[string]int{"bob": http.StatusForbidden, "carol": http.StatusForbidden, "eve": http.StatusNotFound} {
		if resp, b := send(t, http.MethodPut, one, who, body); resp.StatusCode != want {
			t.Fatalf("PUT by %s = %d %s", who, resp.StatusCode, b)
		}
	}
	resp, b := send(t, http.MethodPut, one, "alice", body)
	if resp.StatusCode != http.StatusCreated || resp.Header.Get("Location") != "/v1/duties/report" {
		t.Fatalf("PUT by the owner = %d %s", resp.StatusCode, b)
	}
	var v View
	decodeJSON(t, b, &v)
	if v.Owner != "alice" || v.Source != SourceStore || v.Revision == 0 || v.Readback != "Every day at 9:00 AM, New York time." || len(v.Next) != 5 {
		t.Fatalf("the new duty = %s", b)
	}
	resp, b = send(t, http.MethodGet, base, "carol", "")
	var list ListAnswer
	decodeJSON(t, b, &list)
	if resp.StatusCode != http.StatusOK || len(list.Duties) != 1 || list.Paused {
		t.Fatalf("the list of a viewer = %d %s", resp.StatusCode, b)
	}
	if resp, b := send(t, http.MethodGet, one, "carol", ""); resp.StatusCode != http.StatusOK || !strings.Contains(b, `"state":{"lastRun":null`) {
		t.Fatalf("GET by a viewer = %d %s", resp.StatusCode, b)
	}
	if resp, _ := send(t, http.MethodGet, one, "dave", ""); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET by a person who cannot view the agent = %d", resp.StatusCode)
	}
	resp, b = send(t, http.MethodGet, base, "dave", "")
	decodeJSON(t, b, &list)
	if len(list.Duties) != 0 {
		t.Fatalf("the list of dave = %s", b)
	}
	// An admin changes it, the owner stays, and a stale revision is 409.
	changed := daily("report")
	changed.Do.Ask.Message = "Another message."
	resp, b = send(t, http.MethodPut, one, "root", mustJSON(t, changed))
	if resp.StatusCode != http.StatusOK || !strings.Contains(b, `"owner":"alice"`) || !strings.Contains(b, "Another message.") {
		t.Fatalf("PUT by an admin = %d %s", resp.StatusCode, b)
	}
	stale := DutyBody{Duty: daily("report"), Revision: v.Revision}
	if resp, b := send(t, http.MethodPut, one, "alice", mustJSON(t, stale)); resp.StatusCode != http.StatusConflict || !strings.Contains(b, "read it again") {
		t.Fatalf("PUT at a stale revision = %d %s", resp.StatusCode, b)
	}
	// A read is a body: the fields that only a read has are taken.
	_, b = send(t, http.MethodGet, one, "alice", "")
	if resp, b2 := send(t, http.MethodPut, one, "alice", b); resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT of a read = %d %s", resp.StatusCode, b2)
	}
	if resp, b := send(t, http.MethodPut, base+"/other", "alice", body); resp.StatusCode != http.StatusBadRequest || !strings.Contains(b, "named report in the body and other in the path") {
		t.Fatalf("PUT with another name = %d %s", resp.StatusCode, b)
	}
}

// A dry run reads the words of a schedule in the time zone of the caller,
// answers the readback, the next five times, and every problem, and saves
// nothing. A write with a problem is 400 with each problem.
func TestRoutesDryRun(t *testing.T) {
	f := newFront(t, "", nil)
	one := f.url + "/v1/duties/morning"
	words := `{"agent": "triage", "when": {"say": "weekdays at 9am"}, "do": {"ask": {"message": "Report."}}, "limits": {"budget": {"maxTurns": 10}}}`
	resp, b := send(t, http.MethodPut, one+"?dryRun=true", "alice", words, HeaderTimeZone, "America/Chicago")
	var dry DryAnswer
	decodeJSON(t, b, &dry)
	if resp.StatusCode != http.StatusOK || dry.Readback != "Every weekday at 9:00 AM, Chicago time." || len(dry.Next) != 5 || len(dry.Problems) != 0 ||
		dry.Duty.When.Schedule == nil || dry.Duty.When.Schedule.TimeZone != "America/Chicago" || dry.Duty.When.Say != "" {
		t.Fatalf("the dry run = %d %s", resp.StatusCode, b)
	}
	if !dry.Next[0].Equal(time.Date(2026, 10, 26, 14, 0, 0, 0, time.UTC)) {
		t.Fatalf("next = %v", dry.Next)
	}
	if resp, _ := send(t, http.MethodGet, one, "alice", ""); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("a dry run saved: %d", resp.StatusCode)
	}
	for _, c := range []struct{ body, want string }{
		{strings.Replace(words, "weekdays at 9am", "every Tuesday after lunch", 1), `when.say: Every Tuesday is clear. \"after lunch\" is not a time: pick one.`},
		{strings.Replace(words, `"budget": {"maxTurns": 10}`, `"perDayUSD": 2`, 1), "limits.budget: A budget is required"},
		{strings.Replace(words, `{"say": "weekdays at 9am"}`, `{"event": {"kind": "bridge.closed"}}`, 1), "when.event: Not available yet"},
		{strings.Replace(words, `{"ask": {"message": "Report."}}`, `{"post": {"bridge": "b", "message": "x"}}`, 1), "do.post: Not available yet"},
	} {
		resp, b := send(t, http.MethodPut, one+"?dryRun=true", "alice", c.body)
		if resp.StatusCode != http.StatusOK || !strings.Contains(b, c.want) {
			t.Errorf("dry run of %s = %d %s", c.body, resp.StatusCode, b)
		}
		resp, b = send(t, http.MethodPut, one, "alice", c.body)
		if resp.StatusCode != http.StatusBadRequest || !strings.Contains(b, `"problems":[`) || !strings.Contains(b, c.want) {
			t.Errorf("write of %s = %d %s", c.body, resp.StatusCode, b)
		}
	}
	if resp, b := send(t, http.MethodPut, one+"?dryRun=true", "alice", words, HeaderTimeZone, "Mars/Base"); !strings.Contains(b, `when.schedule.timeZone: The time zone \"Mars/Base\" of the caller is not an IANA time zone`) {
		t.Fatalf("a bad zone = %d %s", resp.StatusCode, b)
	}
	if resp, _ := send(t, http.MethodPut, one+"?test=true", "alice", words); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("a test without a dry run = %d", resp.StatusCode)
	}
	// A draft without a name is checked as new: its dry run says nothing
	// of the name, and a write of the name new fails.
	draft := f.url + "/v1/duties/" + DraftName
	if resp, b := send(t, http.MethodPut, draft+"?dryRun=true", "alice", words, HeaderTimeZone, "America/Chicago"); resp.StatusCode != http.StatusOK || strings.Contains(b, "name: ") || !strings.Contains(b, "Every weekday at 9:00 AM") {
		t.Fatalf("the dry run of a draft = %d %s", resp.StatusCode, b)
	}
	if resp, b := send(t, http.MethodPut, draft, "alice", words); resp.StatusCode != http.StatusBadRequest || !strings.Contains(b, "name: The name new stands for a duty that has no name yet") {
		t.Fatalf("a write of the draft name = %d %s", resp.StatusCode, b)
	}
	if resp, b := send(t, http.MethodPut, one+"?dryRun=true&test=true", "alice", words); resp.StatusCode != http.StatusOK || !strings.Contains(b, "checkFirst: The test runs the check, and the duty has none") {
		t.Fatalf("a test without a check = %d %s", resp.StatusCode, b)
	}
	if resp, b := send(t, http.MethodPut, one, "alice", `{"agent": "triage", "surprise": 1}`); resp.StatusCode != http.StatusBadRequest || !strings.Contains(b, "surprise") {
		t.Fatalf("an unknown field = %d %s", resp.StatusCode, b)
	}
}

// A pause, a run now, a delete, and the switch, each with its access.
func TestRoutesLifecycle(t *testing.T) {
	f := newFront(t, "", nil)
	base := f.url + "/v1/duties"
	one := base + "/report"
	if resp, b := send(t, http.MethodPut, one, "alice", mustJSON(t, daily("report"))); resp.StatusCode != http.StatusCreated {
		t.Fatalf("create = %d %s", resp.StatusCode, b)
	}
	if resp, _ := send(t, http.MethodPatch, one, "bob", `{"paused": true}`); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("a pause by an operator = %d", resp.StatusCode)
	}
	if resp, _ := send(t, http.MethodPatch, one, "alice", `{}`); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("a pause without paused = %d", resp.StatusCode)
	}
	if resp, b := send(t, http.MethodPatch, one, "alice", `{"paused": true}`); resp.StatusCode != http.StatusOK || !strings.Contains(b, `"paused":true`) {
		t.Fatalf("a pause = %d %s", resp.StatusCode, b)
	}
	// A write that leaves out paused keeps the pause.
	if _, b := send(t, http.MethodPut, one, "alice", strings.Replace(mustJSON(t, daily("report")), `,"paused":false`, "", 1)); !strings.Contains(b, `"paused":true`) {
		t.Fatalf("a write without paused = %s", b)
	}
	resp, b := send(t, http.MethodPost, one+"/run", "alice", "")
	if resp.StatusCode != http.StatusAccepted || !strings.Contains(b, `"run":"`) {
		t.Fatalf("run now = %d %s", resp.StatusCode, b)
	}
	resp, b = send(t, http.MethodPost, one+"/run", "alice", "")
	if resp.StatusCode != http.StatusOK || !strings.Contains(b, `"skipped":"The run of`) {
		t.Fatalf("a second run now = %d %s", resp.StatusCode, b)
	}
	f.queue.setFail(errDown)
	d := daily("other")
	d.Limits.Retries = intp(0)
	send(t, http.MethodPut, base+"/other", "alice", mustJSON(t, d))
	if resp, b := send(t, http.MethodPost, base+"/other/run", "alice", ""); resp.StatusCode != http.StatusBadGateway || !strings.Contains(b, "The queue did not take the run: the bus is down.") {
		t.Fatalf("run now with the queue down = %d %s", resp.StatusCode, b)
	}
	if resp, _ := send(t, http.MethodPatch, base, "alice", `{"paused": true, "reason": "storm"}`); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("the switch by an owner = %d", resp.StatusCode)
	}
	if resp, b := send(t, http.MethodPatch, base, "root", `{"paused": true, "reason": "storm"}`); resp.StatusCode != http.StatusOK || !strings.Contains(b, `"paused":true`) || !strings.Contains(b, `"by":"root"`) {
		t.Fatalf("the switch by an admin = %d %s", resp.StatusCode, b)
	}
	if _, b := send(t, http.MethodGet, base, "carol", ""); !strings.Contains(b, `"paused":true,"reason":"storm"`) {
		t.Fatalf("the list under the switch = %s", b)
	}
	if resp, _ := send(t, http.MethodDelete, one, "bob", ""); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("a delete by an operator = %d", resp.StatusCode)
	}
	if resp, _ := send(t, http.MethodDelete, one, "alice", ""); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("a delete = %d", resp.StatusCode)
	}
	if resp, _ := send(t, http.MethodGet, one, "alice", ""); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET after a delete = %d", resp.StatusCode)
	}
}

// The entries of workbench.sources show as duties with the source file,
// for the viewers of their agent, and no route changes them.
func TestRoutesFileSources(t *testing.T) {
	block := `
sources:
  - name: tickets
    agent: triage
    auth: {mode: none}
    input: "Triage {{.id}}"
    key: "{{.id}}"
  - name: alerts
    agent: private
    poll: {url: "https://alerts.example.com/api?token=x", every: 2m}
    key: "{{.id}}"
`
	f := newFront(t, block, nil)
	base := f.url + "/v1/duties"
	resp, b := send(t, http.MethodGet, base, "carol", "")
	var list ListAnswer
	decodeJSON(t, b, &list)
	if resp.StatusCode != http.StatusOK || len(list.Duties) != 1 || list.Duties[0].Name != "tickets" || list.Duties[0].Source != SourceFile ||
		!strings.Contains(list.Duties[0].Readback, "When another app calls the address of this source") || strings.Contains(list.Duties[0].Readback, "/v1/") || strings.Contains(b, "{{") {
		t.Fatalf("the list = %d %s", resp.StatusCode, b)
	}
	_, b = send(t, http.MethodGet, base+"/alerts", "dave", "")
	if !strings.Contains(b, `"source":"file"`) || !strings.Contains(b, "Every 2m, it looks at alerts.example.com/api") || strings.Contains(b, "token=x") {
		t.Fatalf("a poll source = %s", b)
	}
	if resp, _ := send(t, http.MethodGet, base+"/alerts", "carol", ""); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("a source of another agent = %d", resp.StatusCode)
	}
	for _, c := range []struct{ method, path, body string }{
		{http.MethodPut, "/tickets", mustJSON(t, daily("tickets"))},
		{http.MethodPatch, "/tickets", `{"paused": true}`},
		{http.MethodDelete, "/tickets", ""},
		{http.MethodPost, "/tickets/run", ""},
	} {
		if resp, b := send(t, c.method, base+c.path, "alice", c.body); resp.StatusCode != http.StatusConflict || !strings.Contains(b, "workbench.sources in fw.yaml") {
			t.Errorf("%s %s = %d %s", c.method, c.path, resp.StatusCode, b)
		}
	}
}

// Each route of the duties has its MCP tool, or a reason why none.
func TestParity(t *testing.T) {
	f := newFront(t, "", nil)
	cs := connectMCP(t, f.a, "alice")
	listed, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, tool := range listed.Tools {
		if strings.HasPrefix(tool.Name, "dut") {
			names[tool.Name] = true
		}
	}
	if len(names) != 9 {
		t.Fatalf("the server has %d tools of the duties: %v", len(names), names)
	}
	seen := map[string]bool{}
	for _, r := range table {
		switch {
		case r.tool != "" && !names[r.tool]:
			t.Errorf("the route %s names the tool %s, which the server does not have", r.pattern, r.tool)
		case r.tool == "" && r.why == "":
			t.Errorf("the route %s has no tool and no reason", r.pattern)
		}
		seen[r.tool] = true
	}
	// duty_preview is the dry run of PUT /v1/duties/{name}.
	seen[ToolPreview] = seen[ToolPut]
	for name := range names {
		if !seen[name] {
			t.Errorf("the tool %s has no route", name)
		}
	}
}
