package bridges

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/fleet"
)

// budgetBridge opens a bridge whose reviewer has the budget of the manager
// of the lab, maxTurns 6, and whose two seats have a finished run each on
// the live member default/crew-0. The reviewer used 2 turns.
func budgetBridge(t *testing.T, caps string) (*Store, *fakeView, *fakeMember, string, string) {
	t.Helper()
	a := newAgent(t, nil)
	s := newStore(t, a, nil)
	v, fm := withFleet(t, a)
	v.state("default/crew-0", fleet.MemberLive, "migrator", "reviewer")
	url := serve(t, a)
	body := `{"title":"Migrate logrotate","members":[
		{"handle":"migrator-1","agent":"migrator","task":"Migrate"},
		{"handle":"reviewer","agent":"reviewer","task":"Review","budget":{"maxTurns":6}}]` + caps + `}`
	resp, out := do(t, http.MethodPost, url+"/v1/bridges", "alice", body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("open = %d %s", resp.StatusCode, out)
	}
	var it Item
	decodeJSON(t, out, &it)
	mig := workerRun("r-mig", it.ID, "migrator-1", "migrator", fw.StateCompleted)
	rev := workerRun("r-rev", it.ID, "reviewer", "reviewer", fw.StateCompleted)
	rev.Usage.Turns = 2
	v.put(mig)
	v.put(rev)
	s.poll(context.Background())
	return s, v, fm, url, it.ID
}

// budgetOfBody reads the budget of a follow-up as the owner got it, or nil.
func budgetOfBody(t *testing.T, sn seen) map[string]any {
	t.Helper()
	b, _ := sn.Body["budget"].(map[string]any)
	return b
}

// say posts a message of alice and waits until the path got n follow-ups.
func say(t *testing.T, fm *fakeMember, url, id, body, path string, n int) seen {
	t.Helper()
	if resp, out := do(t, http.MethodPost, url+"/v1/bridges/"+id+"/messages", "alice", body); resp.StatusCode != http.StatusCreated {
		t.Fatalf("post = %d %s", resp.StatusCode, out)
	}
	eventually(t, "the follow-up lands", func() bool { return len(fm.find(http.MethodPost, path)) == n })
	return fm.find(http.MethodPost, path)[n-1]
}

// Each follow-up to a seat run gives the run the allowance of its seat
// over what it used, so a seat that took several messages never dies of
// the arithmetic of a budget that counts the whole run. A seat whose
// worker gives no budget gets no change of its limits.
func TestFollowUpGivesTheAllowanceOfTheSeat(t *testing.T) {
	s, v, fm, url, id := budgetBridge(t, "")

	// The run finished its start after 2 of its 6 turns: the follow-up
	// gives it 6 more.
	got := say(t, fm, url, id, `{"text":"Review the cron role.","to":["reviewer"]}`, pathRev, 1)
	if b := budgetOfBody(t, got); b == nil || b["maxTurns"] != float64(8) {
		t.Fatalf("the first follow-up gives %v, want maxTurns 8", got.Body["budget"])
	}
	// The run works on it, and the view knows its usage only at its last
	// end: a second message raises the limits that the run has.
	rev := workerRun("r-rev", id, "reviewer", "reviewer", fw.StateRunning)
	rev.Usage.Turns = 2
	v.put(rev)
	got = say(t, fm, url, id, `{"text":"And the logrotate role.","to":["reviewer"]}`, pathRev, 2)
	if b := budgetOfBody(t, got); b == nil || b["maxTurns"] != float64(14) {
		t.Fatalf("a message that the run folds into its work gives %v, want maxTurns 14", got.Body["budget"])
	}
	// The run finished both after 9 turns: the next one gets 6 over them.
	rev.State, rev.Usage.Turns = fw.StateCompleted, 9
	v.put(rev)
	got = say(t, fm, url, id, `{"text":"Check the result.","to":["reviewer"]}`, pathRev, 3)
	if b := budgetOfBody(t, got); b == nil || b["maxTurns"] != float64(15) {
		t.Fatalf("the third follow-up gives %v, want maxTurns 15", got.Body["budget"])
	}
	rec, _, err := s.b.get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if mk := rec.Marks["r-rev"]; mk == nil || mk.Grant == nil || mk.Grant.MaxTurns != 15 {
		t.Fatalf("the mark of the run keeps %+v", mk)
	}

	// The migrator has no budget of its seat, and its worker gave none:
	// its follow-up changes no limit.
	got = say(t, fm, url, id, `{"text":"Status?","to":["migrator-1"]}`, pathMig, 1)
	if _, ok := got.Body["budget"]; ok {
		t.Fatalf("a seat without a budget got %v", got.Body["budget"])
	}
}

// The allowance is the budget that the kernel resolved at the start of
// the run: the budget of the seat over the budget of the configuration of
// the worker. So the tool calls and the working time of the worker grow
// with each message too, and the front reads the first event once.
func TestAllowanceComesFromTheStartOfTheRun(t *testing.T) {
	_, v, fm, url, id := budgetBridge(t, "")
	fm.setEvents("r-rev", ev(fw.EvRunQueued, fw.RunQueuedData{Agent: "reviewer", Budget: &fw.Budget{MaxTurns: 6},
		Resolved: &fw.ResolvedProfile{Budget: &fw.Budget{MaxTurns: 16, MaxToolCalls: 24, Deadline: 10 * time.Minute, MaxCallsPerTurn: 4}}}))
	rev := workerRun("r-rev", id, "reviewer", "reviewer", fw.StateCompleted)
	rev.Usage.Turns, rev.Usage.ToolCalls = 2, 5
	v.put(rev)
	got := say(t, fm, url, id, `{"text":"Review it.","to":["reviewer"]}`, pathRev, 1)
	b := budgetOfBody(t, got)
	if b == nil || b["maxTurns"] != float64(8) || b["maxToolCalls"] != float64(29) || b["deadline"] != float64(20*time.Minute) {
		t.Fatalf("the follow-up gives %v, want maxTurns 8, maxToolCalls 29, deadline 20m", got.Body["budget"])
	}
	if _, ok := b["maxCallsPerTurn"]; ok {
		t.Fatalf("a limit of one turn grew: %v", b)
	}
	reads := len(fm.find(http.MethodGet, "/v1/runs/r-rev/events"))
	got = say(t, fm, url, id, `{"text":"Again.","to":["reviewer"]}`, pathRev, 2)
	if b := budgetOfBody(t, got); b == nil || b["deadline"] != float64(30*time.Minute) {
		t.Fatalf("the second follow-up gives %v, want deadline 30m", got.Body["budget"])
	}
	if n := len(fm.find(http.MethodGet, "/v1/runs/r-rev/events")); n != reads {
		t.Fatalf("the server read the events of the run again: %d reads, then %d", reads, n)
	}
}

// The cap maxTurns bounds the turns of every run of a bridge together.
// Past it nothing fans out, the channel says so once, and a raised cap
// delivers again.
func TestTurnCapBoundsTheBridge(t *testing.T) {
	_, v, fm, url, id := budgetBridge(t, `,"caps":{"maxTurns":10}`)
	mig := workerRun("r-mig", id, "migrator-1", "migrator", fw.StateCompleted)
	mig.Usage.Turns = 8
	v.put(mig)
	for range 2 {
		if resp, out := do(t, http.MethodPost, url+"/v1/bridges/"+id+"/messages", "alice", `{"text":"Go on."}`); resp.StatusCode != http.StatusCreated {
			t.Fatalf("post = %d %s", resp.StatusCode, out)
		}
	}
	time.Sleep(50 * time.Millisecond)
	if n := len(fm.find(http.MethodPost, pathRev)) + len(fm.find(http.MethodPost, pathMig)); n != 0 {
		t.Fatalf("%d follow-ups past the cap of turns", n)
	}
	resp, body := do(t, http.MethodGet, url+"/v1/bridges/"+id+"/messages", "alice", "")
	if resp.StatusCode != http.StatusOK || strings.Count(body, "The channel reached its cap of 10 turns") != 1 {
		t.Fatalf("the channel = %s", body)
	}
	if resp, out := do(t, http.MethodPatch, url+"/v1/bridges/"+id, "alice", `{"caps":{"maxTurns":100}}`); resp.StatusCode != http.StatusOK || !strings.Contains(out, `"maxTurns":100`) {
		t.Fatalf("raise the cap = %d %s", resp.StatusCode, out)
	}
	say(t, fm, url, id, `{"text":"Now go on.","to":["reviewer"]}`, pathRev, 1)
}

// A record of an older front has no cap of turns: the default holds, the
// list shows it, and a change of another cap keeps it valid.
func TestTurnCapOfAnOlderRecord(t *testing.T) {
	s, _, _, url, id := budgetBridge(t, "")
	if _, err := s.b.update(context.Background(), id, func(rec *record) error {
		rec.Caps.MaxTurns = 0
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	resp, body := do(t, http.MethodGet, url+"/v1/bridges", "alice", "")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `"maxTurns":1000`) {
		t.Fatalf("list = %d %s", resp.StatusCode, body)
	}
	if resp, out := do(t, http.MethodPatch, url+"/v1/bridges/"+id, "alice", `{"caps":{"maxMessages":50}}`); resp.StatusCode != http.StatusOK || !strings.Contains(out, `"maxTurns":1000`) {
		t.Fatalf("patch = %d %s", resp.StatusCode, out)
	}
}

// A seat whose newest run stopped at a limit is limit, not error. Continue
// goes on with the same run: one follow-up of the person with the
// allowance over what the run used, and a line in the channel. A message
// of a person continues it too, and a message of an agent does not.
func TestLimitStopContinuesTheSameRun(t *testing.T) {
	s, v, fm, url, id := budgetBridge(t, "")
	rev := workerRun("r-rev", id, "reviewer", "reviewer", fw.StateFailed)
	rev.Error, rev.Usage.Turns = "budget exceeded: maxTurns: the run used 6 turns, and its limit is 6 (budget.maxTurns)", 6
	v.put(rev)
	s.poll(context.Background())
	resp, body := do(t, http.MethodGet, url+"/v1/bridges/"+id, "alice", "")
	var it Item
	decodeJSON(t, body, &it)
	if resp.StatusCode != http.StatusOK || it.Members[1].Presence != PresenceLimit {
		t.Fatalf("the reviewer = %+v", it.Members)
	}

	// Continue on a seat whose run did not stop at a limit has nothing to
	// go on with.
	if resp, out := do(t, http.MethodPatch, url+"/v1/bridges/"+id+"/members/migrator-1", "alice", `{"continue":true}`); resp.StatusCode != http.StatusConflict {
		t.Fatalf("continue a finished seat = %d %s", resp.StatusCode, out)
	}
	if resp, out := do(t, http.MethodPatch, url+"/v1/bridges/"+id+"/members/reviewer", "alice", `{"continue":true,"task":"x"}`); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("continue with a task = %d %s", resp.StatusCode, out)
	}
	if resp, out := do(t, http.MethodPatch, url+"/v1/bridges/"+id+"/members/reviewer", "alice", `{"continue":true}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("continue = %d %s", resp.StatusCode, out)
	}
	eventually(t, "the run gets the follow-up", func() bool { return len(fm.find(http.MethodPost, pathRev)) == 1 })
	got := fm.find(http.MethodPost, pathRev)[0]
	content, _ := got.Body["content"].(string)
	from, _ := got.Body["from"].(map[string]any)
	if b := budgetOfBody(t, got); b == nil || b["maxTurns"] != float64(12) || !strings.Contains(content, "gave you more room") || from["kind"] != fw.FromPerson {
		t.Fatalf("continue sent %v", got.Body)
	}
	if lines := withEvent(channel(t, s, id), EventTask); len(lines) != 1 || !strings.HasSuffix(lines[0].Text, " continued reviewer") {
		t.Fatalf("task lines = %+v", lines)
	}
	if n := len(fm.find(http.MethodPost, "/v1/runs")); n != 0 {
		t.Fatalf("continue started %d new runs", n)
	}
	// A double click continues once.
	if resp, out := do(t, http.MethodPatch, url+"/v1/bridges/"+id+"/members/reviewer", "alice", `{"continue":true}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("a second continue = %d %s", resp.StatusCode, out)
	}
	time.Sleep(50 * time.Millisecond)
	if n, lines := len(fm.find(http.MethodPost, pathRev)), withEvent(channel(t, s, id), EventTask); n != 1 || len(lines) != 1 {
		t.Fatalf("a second continue: %d follow-ups, task lines %+v", n, lines)
	}

	// An agent cannot continue it; a person can.
	if _, err := s.post(context.Background(), id, sender{handle: "migrator-1", run: "r-mig"}, PostSpec{Text: "Review again.", To: []string{"reviewer"}}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if n := len(fm.find(http.MethodPost, pathRev)); n != 1 {
		t.Fatalf("a message of an agent continued the run: %d follow-ups", n)
	}
	got = say(t, fm, url, id, `{"text":"Go on and check the result.","to":["reviewer"]}`, pathRev, 2)
	if b := budgetOfBody(t, got); b == nil || b["maxTurns"] != float64(12) {
		t.Fatalf("a message of a person gives %v", got.Body["budget"])
	}
}

// A message that an agent addresses to itself drops the address, and one
// that addresses only itself reaches nobody. A final answer that repeats a
// post of the same run word for word does not reach the channel again.
func TestSelfAddressAndRepeatedAnswer(t *testing.T) {
	s, v, fm, _, id := budgetBridge(t, "")
	m, err := s.post(context.Background(), id, sender{handle: "migrator-1", run: "r-mig"}, PostSpec{Text: "Ready.", To: []string{"migrator-1", "reviewer"}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(m.To, ",") != "reviewer" {
		t.Fatalf("to = %v", m.To)
	}
	eventually(t, "the reviewer gets it", func() bool { return len(fm.find(http.MethodPost, pathRev)) == 1 })
	m, err = s.post(context.Background(), id, sender{handle: "migrator-1", run: "r-mig"}, PostSpec{Text: "@migrator-1 is waiting for @reviewer.", To: []string{"@Migrator-1"}})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if len(m.To) != 0 || len(fm.find(http.MethodPost, pathRev)) != 1 || len(fm.find(http.MethodPost, pathMig)) != 0 {
		t.Fatalf("a message to itself: to %v, %d and %d follow-ups", m.To, len(fm.find(http.MethodPost, pathRev)), len(fm.find(http.MethodPost, pathMig)))
	}

	// The reviewer posts its result and then ends its turn with the same
	// text.
	result := "@migrator-1 PASS: the cron role is fine.\n\nAll checks are green."
	if _, err := s.post(context.Background(), id, sender{handle: "reviewer", run: "r-rev"}, PostSpec{Text: result, To: []string{"migrator-1"}}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the migrator gets the post", func() bool { return len(fm.find(http.MethodPost, pathMig)) == 1 })
	rev := workerRun("r-rev", id, "reviewer", "reviewer", fw.StateRunning)
	v.put(rev)
	finish(t, s, v, fm, rev, "  @migrator-1 PASS: the cron role is fine.\nAll checks are green.\n")
	n := 0
	for _, e := range channel(t, s, id) {
		if e.From.Handle == "reviewer" && strings.Contains(e.Text, "PASS") {
			n++
		}
	}
	time.Sleep(50 * time.Millisecond)
	if n != 1 || len(fm.find(http.MethodPost, pathMig)) != 1 {
		t.Fatalf("the result reached the channel %d times and the migrator %d times", n, len(fm.find(http.MethodPost, pathMig)))
	}
	// Another answer of the run still reaches the channel.
	finish(t, s, v, fm, rev, "Done.")
	if got := channel(t, s, id); got[len(got)-1].Text != "Done." {
		t.Fatalf("the last entry = %+v", got[len(got)-1])
	}
}

// The list says when the newest post of a person or an agent of each
// channel came. A channel with no post yet moved when it opened.
func TestListCarriesTheTimeOfTheNewestEntry(t *testing.T) {
	s, _, _, url, id := budgetBridge(t, "")
	listed := func() *time.Time {
		t.Helper()
		resp, body := do(t, http.MethodGet, url+"/v1/bridges", "alice", "")
		var list struct {
			Bridges []Item `json:"bridges"`
		}
		decodeJSON(t, body, &list)
		if resp.StatusCode != http.StatusOK || len(list.Bridges) != 1 {
			t.Fatalf("list = %d %s", resp.StatusCode, body)
		}
		return list.Bridges[0].LastAt
	}
	rec, _, _ := s.b.get(context.Background(), id)
	if at := listed(); at == nil || !at.Equal(rec.CreatedAt) {
		t.Fatalf("a channel without a post = %v, opened %v", at, rec.CreatedAt)
	}
	alice := fw.WithPrincipal(context.Background(), callers["alice"])
	m, err := s.postAs(alice, id, PostSpec{Text: "Go on."})
	if err != nil {
		t.Fatal(err)
	}
	if at := listed(); at == nil || !at.Equal(m.At) {
		t.Fatalf("the newest post = %v, want %v", at, m.At)
	}
}

// A system line does not make a bridge fresh: a line of sleep or wake,
// which every bridge of a worker gets, and a line that a seat lost its
// run, which a front that starts again posts days after the last post
// (live walk N1).
func TestListTimeSkipsTheLinesOfSleepAndWake(t *testing.T) {
	s, _, _, url, id := budgetBridge(t, "")
	alice := fw.WithPrincipal(context.Background(), callers["alice"])
	m, err := s.postAs(alice, id, PostSpec{Text: "Go on."})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	if err := s.postLines(context.Background(), id, system(EventAsleep, "reviewer went to sleep"), system(EventStarted, "reviewer woke up"),
		system(EventLost, "reviewer stopped: the agent that held its work was replaced.")); err != nil {
		t.Fatal(err)
	}
	resp, body := do(t, http.MethodGet, url+"/v1/bridges", "alice", "")
	var list struct {
		Bridges []Item `json:"bridges"`
	}
	decodeJSON(t, body, &list)
	if resp.StatusCode != http.StatusOK || len(list.Bridges) != 1 || list.Bridges[0].LastAt == nil || !list.Bridges[0].LastAt.Equal(m.At) {
		t.Fatalf("list = %d %s, newest post %v", resp.StatusCode, body, m.At)
	}
	if !quietLine(system(EventStarted, "reviewer woke up")) || quietLine(system(EventStarted, "reviewer started: Review the roles")) {
		t.Fatal("quietLine tells a wake line from the start of a seat wrongly")
	}
}

// The instructions of a seat say that its sandbox is private, so a file
// for a teammate goes on the wiki, and they keep the channel quiet: no
// answer to a message that asks nothing, and no address of itself.
func TestInstructionsKeepTheSandboxAndTheQuiet(t *testing.T) {
	a := newAgent(t, nil)
	s := newStore(t, a, nil)
	url := serve(t, a)
	it := open(t, url)
	rec, _, err := s.b.get(context.Background(), it.ID)
	if err != nil {
		t.Fatal(err)
	}
	in := s.instructionsOf(context.Background(), rec, *rec.member("reviewer"))
	for _, want := range []string{
		"You work in your own sandbox on your own worker. No teammate and no person can read its files",
		"in a page under bridges/" + it.ID + "/ with pages_write: each file as a fenced code block under a heading that names its path. Then name that page in your message.",
		"A message that asks nothing of you gets no answer",
		"Never answer a thanks, an acknowledgment, or a PASS that needs nothing from you.",
		"Never address yourself: do not write @reviewer.",
		"ask with ask true and the choice [\"Done\"]",
		"never start a new page such as name-v2 for a new version",
		"A file that the person attaches to a message for you is in your sandbox under attachments/",
	} {
		if !strings.Contains(in, want) {
			t.Errorf("the instructions lack %q", want)
		}
	}
}

// The cap of turns has a default, a variable, and bounds.
func TestTurnCapConfiguration(t *testing.T) {
	a := newAgent(t, nil)
	c, err := Load(a.Config())
	if err != nil || c.MaxTurns != DefaultMaxTurns || c.caps().MaxTurns != DefaultMaxTurns {
		t.Fatalf("default = %+v %v", c, err)
	}
	t.Setenv(EnvMaxTurns, "250")
	if c, err := Load(a.Config()); err != nil || c.MaxTurns != 250 {
		t.Fatalf("variable = %+v %v", c, err)
	}
	t.Setenv(EnvMaxTurns, "-1")
	if _, err := Load(a.Config()); err == nil || !strings.Contains(err.Error(), "maxTurns") {
		t.Fatalf("a bad value = %v", err)
	}
}

// nextGrant builds each limit of the allowance on what the run used, and
// on the limits that a run that works has.
func TestNextGrant(t *testing.T) {
	allow := fw.Budget{MaxTurns: 6, MaxToolCalls: 10, MaxTokens: 1000, MaxCostUSD: 0.5, Deadline: 5 * time.Minute}
	done := &fw.Run{State: fw.StateCompleted, Usage: fw.RunUsage{Turns: 3, ToolCalls: 4, CostUSD: 0.1, Usage: fw.Usage{InputTokens: 300, OutputTokens: 50}}}
	g := nextGrant(nil, allow, done)
	if g.MaxTurns != 9 || g.MaxToolCalls != 14 || g.MaxTokens != 1350 || g.MaxCostUSD < 0.599 || g.MaxCostUSD > 0.601 || g.Deadline != 10*time.Minute {
		t.Fatalf("a finished run = %+v", g)
	}
	// The tree counts the tool calls, the tokens, and the cost.
	tree := *done
	tree.TreeUsage = &fw.RunUsage{Turns: 9, ToolCalls: 20, CostUSD: 0.3}
	if g := nextGrant(nil, allow, &tree); g.MaxTurns != 9 || g.MaxToolCalls != 30 {
		t.Fatalf("a run with helpers = %+v", g)
	}
	// A run that works builds on its limits.
	working := &fw.Run{State: fw.StateRunning, Usage: fw.RunUsage{Turns: 1}}
	if g := nextGrant(nil, allow, working); g.MaxTurns != 12 || g.Deadline != 10*time.Minute {
		t.Fatalf("a run that works on its start = %+v", g)
	}
	prev := fw.Budget{MaxTurns: 20, Deadline: 15 * time.Minute}
	if g := nextGrant(&prev, allow, working); g.MaxTurns != 26 || g.Deadline != 20*time.Minute {
		t.Fatalf("a run that works on a follow-up = %+v", g)
	}
	// A limit that the allowance lacks stays as the run has it.
	if g := nextGrant(nil, fw.Budget{MaxTurns: 6}, done); g != (fw.Budget{MaxTurns: 9}) {
		t.Fatalf("turns alone = %+v", g)
	}
}

// A run that stopped at a limit of its budget is limit, not error, unless
// its Task is blocked or stopped; any other failure is error.
func TestPresenceOfALimitStop(t *testing.T) {
	m := Member{Handle: "reviewer"}
	stop := &fw.Run{ID: "r1", State: fw.StateFailed, Error: "budget exceeded: maxTurns: 6 turns (budget.maxTurns)"}
	other := &fw.Run{ID: "r1", State: fw.StateFailed, Error: "model: 400 Bad Request"}
	for _, c := range []struct {
		run  *fw.Run
		task string
		want string
	}{
		{stop, fleet.MemberLive, PresenceLimit},
		{stop, fleet.MemberQuiet, PresenceLimit},
		{stop, fleet.MemberStopped, PresenceError},
		{stop, TaskBlocked, PresenceError},
		{other, fleet.MemberLive, PresenceError},
	} {
		if got := Presence(m, []*fw.Run{c.run}, c.task); got != c.want {
			t.Errorf("Presence(%q on %q) = %s, want %s", c.run.Error, c.task, got, c.want)
		}
	}
}
