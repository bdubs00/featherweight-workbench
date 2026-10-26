package bridges

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/fleet"
)

// memberNamed reads one member of a bridge as the route answers it.
func memberNamed(t *testing.T, url, id, handle string) (Item, Member) {
	t.Helper()
	resp, body := do(t, http.MethodGet, url+"/v1/bridges/"+id, "alice", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("read the bridge = %d %s", resp.StatusCode, body)
	}
	var it Item
	decodeJSON(t, body, &it)
	for _, m := range it.Members {
		if m.Handle == handle {
			return it, m
		}
	}
	t.Fatalf("no member %s in %+v", handle, it.Members)
	return it, Member{}
}

// A seat asks the operator with bridge_post and ask: the entry is a
// question with its choices that goes to the operator alone, and the seat
// needs you, on its presence and in the count of the bridge, until a
// person answers. The final answer of the turn that asked, a one-line
// status, leaves the question waiting. A key of a choice is a reply of a
// person to the question, which reaches the seat on the usual path and
// says what it answers, and the seat no longer needs you.
func TestSeatAsksTheOperator(t *testing.T) {
	h := hooked(t)
	a := newAgent(t, nil)
	s := newStore(t, a, nil)
	v, fm := withFleet(t, a)
	v.state("default/crew-0", fleet.MemberLive)
	url := serve(t, a)
	it := open(t, url)
	mig := workerRun("r-mig", it.ID, "migrator-1", "migrator", fw.StateRunning)
	v.put(mig)
	v.put(workerRun("r-rev", it.ID, "reviewer", "reviewer", fw.StateCompleted))
	s.poll(context.Background())
	cs := connectMCP(t, a, "fleet")

	res := callTool(t, cs, ToolPost, map[string]any{"text": "Restart sshd on both hosts now?", "ask": true, "choices": []string{"Yes", "No", "Later"}}, metaOf("r-mig", "migrator", ""))
	if res.IsError || !strings.Contains(textOf(res), "end this turn now") {
		t.Fatalf("ask = %s", textOf(res))
	}
	var q Message
	structured(t, res, &q)
	if q.Ask == nil || strings.Join(q.Ask.Choices, ",") != "Yes,No,Later" || strings.Join(q.To, ",") != HandleOperator {
		t.Fatalf("question = %+v", q)
	}
	it2, m := memberNamed(t, url, it.ID, "migrator-1")
	if m.Question != q.Seq || m.Presence != PresenceNeedsYou || it2.NeedsYou != 1 {
		t.Fatalf("while it waits: member %+v, needsYou %d", m, it2.NeedsYou)
	}
	if ms := h.member("migrator-1"); len(ms) == 0 || ms[len(ms)-1].Question != q.Seq || ms[len(ms)-1].Presence != PresenceNeedsYou {
		t.Fatalf("the hook got %+v", ms)
	}
	if n := len(fm.find(http.MethodPost, "/v1/runs/r-rev/messages")); n != 0 {
		t.Fatalf("the question reached a teammate %d times", n)
	}

	// The turn that asked ends with a status, which addresses the operator
	// too: the question still waits, and the status is no second question.
	fm.setEvents("r-mig", ev(fw.EvRunQueued, nil), ev(fw.EvRunFinished, fw.RunFinishedData{Outcome: fw.OutcomeCompleted, Output: "@operator Waiting for your answer: restart sshd?"}))
	mig.State, mig.Seq = fw.StateCompleted, 2
	v.put(mig)
	s.poll(context.Background())
	msgs := channel(t, s, it.ID)
	status := msgs[len(msgs)-1]
	if status.From.Handle != "migrator-1" || status.Ask != nil {
		t.Fatalf("status = %+v", status)
	}
	if _, m := memberNamed(t, url, it.ID, "migrator-1"); m.Question != q.Seq || m.Presence != PresenceNeedsYou {
		t.Fatalf("after the status: %+v", m)
	}
	if ms := h.member("migrator-1"); ms[len(ms)-1].Question != q.Seq || ms[len(ms)-1].Presence != PresenceNeedsYou {
		t.Fatalf("the hook after the status got %+v", ms[len(ms)-1])
	}

	// A message of a person to another seat answers nothing.
	if resp, body := do(t, http.MethodPost, url+"/v1/bridges/"+it.ID+"/messages", "alice", `{"text":"Thanks.","to":["reviewer"]}`); resp.StatusCode != http.StatusCreated {
		t.Fatalf("post = %d %s", resp.StatusCode, body)
	}
	if _, m := memberNamed(t, url, it.ID, "migrator-1"); m.Question != q.Seq {
		t.Fatalf("a message to another seat answered the question: %+v", m)
	}
	// A key: a reply to the question with the choice.
	before := len(h.member("migrator-1"))
	if resp, body := do(t, http.MethodPost, url+"/v1/bridges/"+it.ID+"/messages", "alice", fmt.Sprintf(`{"text":"Yes","to":["migrator-1"],"replyTo":%d}`, q.Seq)); resp.StatusCode != http.StatusCreated {
		t.Fatalf("answer = %d %s", resp.StatusCode, body)
	}
	_, m = memberNamed(t, url, it.ID, "migrator-1")
	if m.Question != 0 || m.Presence == PresenceNeedsYou {
		t.Fatalf("after the answer: %+v", m)
	}
	if ms := h.member("migrator-1"); len(ms) <= before || ms[len(ms)-1].Question != 0 {
		t.Fatalf("the hook after the answer got %+v", ms)
	}
	eventually(t, "the seat gets the answer", func() bool { return len(fm.find(http.MethodPost, "/v1/runs/r-mig/messages")) == 1 })
	got := fm.find(http.MethodPost, "/v1/runs/r-mig/messages")[0]
	if content, _ := got.Body["content"].(string); !strings.Contains(content, fmt.Sprintf("from operator: Answer to your question in entry %d: Yes", q.Seq)) {
		t.Fatalf("delivery = %q", content)
	}
}

// As a safety net for a model that does not follow its instructions, a
// final answer that addresses the operator anywhere in its text asks, as
// on the homelab, where the reviewer wrote "@net-auditor ..." first and
// "@operator Two decisions are open ..." later. The answer still reaches
// the teammate that it names first. The seat needs you until it posts
// again, and a message to everyone answers it too.
func TestOperatorAnywhereAsks(t *testing.T) {
	a := newAgent(t, nil)
	s := newStore(t, a, nil)
	v, fm := withFleet(t, a)
	v.state("default/crew-0", fleet.MemberLive)
	url := serve(t, a)
	it := open(t, url)
	rev := workerRun("r-rev", it.ID, "reviewer", "reviewer", fw.StateCompleted)
	mig := workerRun("r-mig", it.ID, "migrator-1", "migrator", fw.StateRunning)
	v.put(mig)
	answer := "@migrator-1 The roles pass review.\n\n@operator Two decisions are open. Do you consent to restarting sshd on both hosts? I am waiting for your answer."
	fm.setEvents("r-rev", ev(fw.EvRunQueued, nil), ev(fw.EvRunFinished, fw.RunFinishedData{Outcome: fw.OutcomeCompleted, Output: answer}))
	rev.Seq = 2
	v.put(rev)
	s.poll(context.Background())
	var q Message
	for _, m := range channel(t, s, it.ID) {
		if m.From.Handle == "reviewer" {
			q = m
		}
	}
	if q.Ask == nil || len(q.Ask.Choices) != 0 || strings.Join(q.To, ",") != "migrator-1" {
		t.Fatalf("answer = %+v", q)
	}
	eventually(t, "the teammate gets the answer", func() bool { return len(fm.find(http.MethodPost, "/v1/runs/r-mig/messages")) == 1 })
	if _, m := memberNamed(t, url, it.ID, "reviewer"); m.Question != q.Seq || m.Presence != PresenceNeedsYou {
		t.Fatalf("reviewer = %+v", m)
	}
	// The seat posts again: its question ends.
	cs := connectMCP(t, a, "fleet")
	if res := callTool(t, cs, ToolPost, map[string]any{"text": "Never mind, the hosts restarted on their own.", "to": []string{"migrator-1"}}, metaOf("r-rev", "reviewer", "")); res.IsError {
		t.Fatalf("post = %s", textOf(res))
	}
	if _, m := memberNamed(t, url, it.ID, "reviewer"); m.Question != 0 || m.Presence == PresenceNeedsYou {
		t.Fatalf("after a new post: %+v", m)
	}
	// A bridge_post to the operator asks too, and a message of a person to
	// everyone answers it.
	res := callTool(t, cs, ToolPost, map[string]any{"text": "Which host first?", "to": []string{"operator"}}, metaOf("r-rev", "reviewer", ""))
	var q2 Message
	structured(t, res, &q2)
	if res.IsError || q2.Ask == nil {
		t.Fatalf("to operator = %s %+v", textOf(res), q2)
	}
	if resp, body := do(t, http.MethodPost, url+"/v1/bridges/"+it.ID+"/messages", "alice", `{"text":"lab-1 first."}`); resp.StatusCode != http.StatusCreated {
		t.Fatalf("post = %d %s", resp.StatusCode, body)
	}
	if _, m := memberNamed(t, url, it.ID, "reviewer"); m.Question != 0 {
		t.Fatalf("after a message to everyone: %+v", m)
	}
}

// The choices of a question are two to four short lines; choices need
// ask, only a seat asks, and a person sends no ask on the route.
func TestAskIsChecked(t *testing.T) {
	for _, c := range [][]string{{"Yes"}, {"a", "b", "c", "d", "e"}, {"Yes", strings.Repeat("x", 61)}, {"Yes", "yes"}, {"Yes", ""}} {
		if _, err := checkChoices(c); err == nil {
			t.Errorf("checkChoices(%q) takes them", c)
		}
	}
	if got, err := checkChoices([]string{" Yes ", "No"}); err != nil || strings.Join(got, ",") != "Yes,No" {
		t.Errorf("checkChoices = %q, %v", got, err)
	}
	a := newAgent(t, nil)
	s := newStore(t, a, nil)
	v, _ := withFleet(t, a)
	v.state("default/crew-0", fleet.MemberLive)
	url := serve(t, a)
	it := open(t, url)
	v.put(workerRun("r-mig", it.ID, "migrator-1", "migrator", fw.StateRunning))
	s.poll(context.Background())
	cs := connectMCP(t, a, "fleet")
	for _, c := range []struct {
		name string
		args map[string]any
		want string
	}{
		// The schema of the tool refuses these before the front reads them.
		{"one choice", map[string]any{"text": "Go?", "ask": true, "choices": []string{"Yes"}}, "choices"},
		{"five choices", map[string]any{"text": "Go?", "ask": true, "choices": []string{"a", "b", "c", "d", "e"}}, "choices"},
		{"a long choice", map[string]any{"text": "Go?", "ask": true, "choices": []string{"Yes", strings.Repeat("x", 61)}}, "choices"},
		{"a choice twice", map[string]any{"text": "Go?", "ask": true, "choices": []string{"Yes", "yes"}}, "comes twice"},
		{"choices without ask", map[string]any{"text": "Go?", "choices": []string{"Yes", "No"}}, "with ask true"},
	} {
		res := callTool(t, cs, ToolPost, c.args, metaOf("r-mig", "migrator", ""))
		if !res.IsError || !strings.Contains(textOf(res), c.want) {
			t.Errorf("%s = %s", c.name, textOf(res))
		}
	}
	if resp, body := do(t, http.MethodPost, url+"/v1/bridges/"+it.ID+"/messages", "alice", `{"text":"Go?","ask":true}`); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("a person with ask = %d %s", resp.StatusCode, body)
	}
}

// asksOperator finds the word @operator anywhere, in any case and in the
// marks of Markdown, and not inside another word or address.
func TestAsksOperator(t *testing.T) {
	for text, want := range map[string]bool{
		"@operator please decide":            true,
		"Done. **@Operator:** restart sshd?": true,
		"Restart it (@operator)?":            true,
		"@net-auditor go ahead":              false,
		"mail ops@operator.example":          false,
		"@operators read this":               false,
		"the operator decides":               false,
	} {
		if got := asksOperator(nil, text); got != want {
			t.Errorf("asksOperator(%q) = %v", text, got)
		}
	}
	if !asksOperator([]string{"reviewer", HandleOperator}, "hi") {
		t.Error("to operator")
	}
}

// A close ends the questions that wait, because nobody answers in a
// closed bridge: the seat no longer needs you, the bridge counts no
// question, and the line of the close says so. A bridge that a front
// closed before this rule shows no question either.
func TestCloseEndsTheQuestions(t *testing.T) {
	a := newAgent(t, nil)
	s := newStore(t, a, nil)
	v, _ := withFleet(t, a)
	v.state("default/crew-0", fleet.MemberLive)
	url := serve(t, a)
	it := open(t, url)
	rev := workerRun("r-rev", it.ID, "reviewer", "reviewer", fw.StateCompleted)
	v.put(rev)
	s.poll(context.Background())
	cs := connectMCP(t, a, "fleet")
	res := callTool(t, cs, ToolPost, map[string]any{"text": "Which host first?", "to": []string{"operator"}}, metaOf("r-rev", "reviewer", ""))
	var q Message
	structured(t, res, &q)
	if res.IsError || q.Ask == nil {
		t.Fatalf("to operator = %s %+v", textOf(res), q)
	}
	if _, m := memberNamed(t, url, it.ID, "reviewer"); m.Question != q.Seq || m.Presence != PresenceNeedsYou {
		t.Fatalf("reviewer before the close = %+v", m)
	}
	if waits, err := s.Waiting(fw.WithPrincipal(context.Background(), &fw.Principal{Subject: "alice", Kind: fw.KindUser})); err != nil || len(waits) != 1 {
		t.Fatalf("Waiting before the close = %+v, %v", waits, err)
	}
	if resp, body := do(t, http.MethodPatch, url+"/v1/bridges/"+it.ID, "alice", `{"state":"closed","delegation":{"report":false}}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("close = %d %s", resp.StatusCode, body)
	}
	item, m := memberNamed(t, url, it.ID, "reviewer")
	if m.Question != 0 || m.Presence == PresenceNeedsYou || item.NeedsYou != 0 {
		t.Fatalf("reviewer after the close = %+v, needs you %d", m, item.NeedsYou)
	}
	if waits, err := s.Waiting(fw.WithPrincipal(context.Background(), &fw.Principal{Subject: "alice", Kind: fw.KindUser})); err != nil || len(waits) != 0 {
		t.Fatalf("Waiting after the close = %+v, %v", waits, err)
	}
	var line Message
	for _, e := range channel(t, s, it.ID) {
		if e.Event == EventClosed {
			line = e
		}
	}
	if !strings.HasSuffix(line.Text, "closed the bridge; the question of reviewer ends without an answer") {
		t.Fatalf("the line of the close = %q", line.Text)
	}
	rec, _, err := s.b.get(context.Background(), it.ID)
	if err != nil || rec.askOf("reviewer") != 0 {
		t.Fatalf("the record keeps the question: %v", err)
	}
	// A record that an older front closed still holds the mark.
	old := &record{Bridge: Bridge{State: StateClosed, Members: []Member{{Handle: "reviewer"}}}, Seats: map[string]*seatMark{"reviewer": {Ask: 7}}}
	if got := old.public().Members[0].Question; got != 0 {
		t.Fatalf("an old closed bridge shows the question %d", got)
	}
}
