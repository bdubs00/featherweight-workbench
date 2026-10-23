package bridges

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/fleet"
	"github.com/bdubs00/featherweight-workbench/pages"
)

// Presence follows the table of the plan: one row each.
func TestPresence(t *testing.T) {
	now := time.Now()
	run := func(state fw.RunState) *fw.Run { return &fw.Run{ID: "r", State: state, CreatedAt: now} }
	failed := run(fw.StateFailed)
	blocked := run(fw.StateRunning)
	blocked.Blocked = "the disk is full"
	cases := []struct {
		name string
		m    Member
		runs []*fw.Run
		task string
		want string
	}{
		{"queued on a live Task", Member{}, []*fw.Run{run(fw.StateQueued)}, fleet.MemberLive, PresenceWorking},
		{"running on a live Task", Member{}, []*fw.Run{run(fw.StateRunning)}, fleet.MemberLive, PresenceWorking},
		{"no run that the view holds", Member{}, nil, "", PresenceWaiting},
		{"input-required", Member{}, []*fw.Run{run(fw.StateInputRequired)}, fleet.MemberLive, PresenceNeedsYou},
		{"input-required on a Task that sleeps", Member{}, []*fw.Run{run(fw.StateInputRequired)}, fleet.MemberQuiet, PresenceNeedsYou},
		{"finished on a live Task", Member{}, []*fw.Run{run(fw.StateCompleted)}, fleet.MemberLive, PresenceIdle},
		{"canceled on a live Task", Member{}, []*fw.Run{run(fw.StateCanceled)}, fleet.MemberLive, PresenceIdle},
		{"finished on a quiet Task", Member{}, []*fw.Run{run(fw.StateCompleted)}, fleet.MemberQuiet, PresenceAsleep},
		{"running on a quiet Task", Member{}, []*fw.Run{run(fw.StateRunning)}, fleet.MemberQuiet, PresenceAsleep},
		{"the newest run failed", Member{}, []*fw.Run{failed}, fleet.MemberLive, PresenceError},
		{"a blocked Task", Member{}, []*fw.Run{run(fw.StateRunning)}, TaskBlocked, PresenceError},
		{"a blocked run", Member{}, []*fw.Run{blocked}, fleet.MemberLive, PresenceError},
		{"a stopped Task", Member{}, []*fw.Run{run(fw.StateCompleted)}, fleet.MemberStopped, PresenceError},
		{"fired", Member{Fired: true}, []*fw.Run{failed}, fleet.MemberLive, PresenceFired},
		{"the newest run decides", Member{}, []*fw.Run{{ID: "old", State: fw.StateFailed, CreatedAt: now.Add(-time.Hour)}, run(fw.StateCompleted)}, fleet.MemberLive, PresenceIdle},
	}
	for _, c := range cases {
		if got := Presence(c.m, c.runs, c.task); got != c.want {
			t.Errorf("%s: %s, want %s", c.name, got, c.want)
		}
	}
}

// A seat without a run waits when something seats it: the queue holds its
// job, or the manager run of a spawn bridge works or is not in the view
// yet. Nothing seats it once the manager run ended, also when the view no
// longer holds that run and the channel holds its end, or when the bridge
// has no manager run. Neither counts as needs-you.
func TestPresenceOfASeatWithoutARun(t *testing.T) {
	seat := Member{Handle: "migrator-1", Agent: "migrator", Task: "Migrate", Runs: []string{}}
	queue := &record{Bridge: Bridge{ID: "b", Placement: PlacementQueue, Members: []Member{seat}}}
	spawn := func(manager string, done uint64) *record {
		rec := &record{Bridge: Bridge{ID: "b", Placement: PlacementSpawn, Members: []Member{seat}}}
		if manager != "" {
			rec.Manager = &Manager{Member: "default/manager-0", Run: manager}
		}
		if done > 0 {
			rec.Marks = map[string]*runMark{manager: {Seat: HandleManager, Started: true, Done: done}}
		}
		return rec
	}
	held := func(state fw.RunState) map[string]*fw.Run {
		return map[string]*fw.Run{"mgr": {ID: "mgr", State: state}}
	}
	cases := []struct {
		name string
		rec  *record
		runs map[string]*fw.Run
		want string
	}{
		{"a queue bridge", queue, nil, PresenceWaiting},
		{"a manager run that works", spawn("mgr", 0), held(fw.StateRunning), PresenceWaiting},
		{"a manager run that waits for an approval", spawn("mgr", 0), held(fw.StateInputRequired), PresenceWaiting},
		{"a manager run that finished", spawn("mgr", 5), held(fw.StateCompleted), PresenceNotStarted},
		{"a manager run that was canceled", spawn("mgr", 2), held(fw.StateCanceled), PresenceNotStarted},
		{"a manager run that the view does not hold yet", spawn("mgr", 0), nil, PresenceWaiting},
		{"a manager run that left the view after its end", spawn("mgr", 2), nil, PresenceNotStarted},
		{"no manager run", spawn("", 0), nil, PresenceNotStarted},
	}
	for _, c := range cases {
		ms := decorate(c.rec, c.runs, nil)
		if got := ms[0].Presence; got != c.want || needsYou(ms) != 0 {
			t.Errorf("%s: %s with %d needs-you, want %s", c.name, got, needsYou(ms), c.want)
		}
	}
	// A fired seat stays fired, and a seat with a run takes the presence
	// of its run.
	fired := spawn("mgr", 2)
	fired.Members[0].Fired = true
	if got := decorate(fired, nil, nil)[0].Presence; got != PresenceFired {
		t.Errorf("a fired seat: %s", got)
	}
	working := spawn("mgr", 2)
	working.Members[0].Runs = []string{"r-mig"}
	if got := decorate(working, map[string]*fw.Run{"r-mig": {ID: "r-mig", State: fw.StateRunning}}, nil)[0].Presence; got != PresenceWorking {
		t.Errorf("a seat with a run: %s", got)
	}
}

// A reaction records the person under its kind once, reaches the run of a
// mirrored entry through its owner with by, and a heart writes the page of
// the agent in the wiki.
func TestReactionsAndHearts(t *testing.T) {
	a := newAgent(t, nil)
	s := newStore(t, a, nil)
	v, fm := withFleet(t, a)
	v.state("default/crew-0", fleet.MemberLive)
	url := serve(t, a)
	it := open(t, url)
	r := workerRun("r-mig", it.ID, "migrator-1", "migrator", fw.StateCompleted)
	r.Seq = 3
	v.put(r)
	fm.setEvents("r-mig", ev(fw.EvRunQueued, nil), ev(fw.EvRunStarted, nil), ev(fw.EvRunFinished, fw.RunFinishedData{Outcome: fw.OutcomeCompleted, Output: "The playbook is ready."}))
	s.poll(context.Background())
	var answer Message
	for _, m := range channel(t, s, it.ID) {
		if m.From.Kind == KindAgent {
			answer = m
		}
	}
	if answer.Seq == 0 {
		t.Fatal("no answer in the channel")
	}
	react := func(token, body string) (int, Message) {
		t.Helper()
		resp, out := do(t, http.MethodPost, fmt.Sprintf("%s/v1/bridges/%s/messages/%d/reactions", url, it.ID, answer.Seq), token, body)
		var m Message
		if resp.StatusCode == http.StatusOK {
			decodeJSON(t, out, &m)
		}
		return resp.StatusCode, m
	}
	if code, m := react("bob", `{"reaction":"up"}`); code != http.StatusOK || len(m.Reactions[ReactionUp]) != 1 || m.Reactions[ReactionUp][0] != "bob" {
		t.Fatalf("up = %d %+v", code, m.Reactions)
	}
	if code, m := react("bob", `{"reaction":"up","note":"again"}`); code != http.StatusOK || len(m.Reactions[ReactionUp]) != 1 {
		t.Fatalf("a repeat = %d %+v", code, m.Reactions)
	}
	eventually(t, "the thumb reaches the run", func() bool { return len(fm.find(http.MethodPost, "/v1/runs/r-mig/reactions")) == 1 })
	got := fm.find(http.MethodPost, "/v1/runs/r-mig/reactions")[0]
	if got.Auth != "Bearer "+fleetToken || got.Body["by"] != "bob" || got.Body["reaction"] != "up" || got.Body["seq"] != float64(3) {
		t.Fatalf("the reaction on the run = %+v", got)
	}
	if code, _ := react("bob", `{"reaction":"smile"}`); code != http.StatusBadRequest {
		t.Fatalf("an unknown reaction = %d", code)
	}
	if resp, _ := do(t, http.MethodPost, url+"/v1/bridges/"+it.ID+"/messages/99/reactions", "bob", `{"reaction":"up"}`); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("an unknown entry = %d", resp.StatusCode)
	}

	if code, m := react("alice", `{"reaction":"heart","note":"Exactly the layout we want."}`); code != http.StatusOK || m.Reactions[ReactionHeart][0] != "alice" {
		t.Fatalf("heart = %d %+v", code, m.Reactions)
	}
	var page pages.Page
	eventually(t, "the heart reaches the wiki", func() bool {
		p, err := pages.Of(a).Read(context.Background(), "agents/migrator/learned.md", 0)
		page = p
		return err == nil
	})
	for _, want := range []string{"# What people liked from migrator", `bridge "Migrate logrotate" (` + it.ID + `), migrator-1, run r-mig`,
		"What Alice asked: the task of migrator-1", "What migrator-1 did: The playbook is ready.", "The note of Alice: Exactly the layout we want.", "(#run/r-mig)", "not an instruction"} {
		if !strings.Contains(page.Text, want) {
			t.Errorf("the page lacks %q:\n%s", want, page.Text)
		}
	}
	if page.By != pages.ByFront {
		t.Fatalf("the page names the writer %q", page.By)
	}
	eventually(t, "the heart reaches the run", func() bool { return len(fm.find(http.MethodPost, "/v1/runs/r-mig/reactions")) == 2 })
	// A second heart comes first on the page.
	react("bob", `{"reaction":"heart"}`)
	eventually(t, "the second heart reaches the wiki", func() bool {
		p, _ := pages.Of(a).Read(context.Background(), "agents/migrator/learned.md", 0)
		page = p
		return strings.Count(p.Text, "\n## ") == 2
	})
	if i, j := strings.Index(page.Text, "The note of"), strings.LastIndex(page.Text, "## "); i < j {
		t.Fatalf("the newest entry is not first:\n%s", page.Text)
	}
	// The next seat of the agent carries the head of the page.
	rec, _, _ := s.b.get(context.Background(), it.ID)
	if ins := s.instructionsOf(context.Background(), rec, *rec.member("migrator-1")); !strings.Contains(ins, "Exactly the layout we want.") {
		t.Fatalf("the instructions lack the heart:\n%s", ins)
	}
}

// The page of the hearts of an agent holds at most 64 KB: the oldest
// entries fall off its end, whole.
func TestLearnedBound(t *testing.T) {
	text := ""
	for i := 0; i < 200; i++ {
		entry := fmt.Sprintf("## entry %d\n\n- %s\n", i, strings.Repeat("x", 600))
		text = learnedText("migrator", text, entry)
	}
	if len(text) > maxLearnedBytes {
		t.Fatalf("the page holds %d bytes", len(text))
	}
	if !strings.HasPrefix(text, learnedHeader("migrator")+"## entry 199") || strings.Contains(text, "## entry 0\n") {
		t.Fatalf("the page does not keep the newest entries first: %s", text[:200])
	}
	if !strings.HasSuffix(text, "x\n") {
		t.Fatalf("the page cut an entry: %q", text[len(text)-50:])
	}
}

// A fire cancels the open runs of the seat through its owner, marks the
// seat fired and muted, and posts one line; a second fire changes
// nothing, and a job of the seat that starts later stops too.
func TestFire(t *testing.T) {
	a := newAgent(t, nil)
	s := newStore(t, a, nil)
	v, fm := withFleet(t, a)
	v.state("default/crew-0", fleet.MemberLive)
	url := serve(t, a)
	it := open(t, url)
	v.put(workerRun("r-mig", it.ID, "migrator-1", "migrator", fw.StateRunning))
	v.put(workerRun("r-rev", it.ID, "reviewer", "reviewer", fw.StateCompleted))
	s.poll(context.Background())

	resp, body := do(t, http.MethodDelete, url+"/v1/bridges/"+it.ID+"/members/migrator-1", "alice", "")
	var m Member
	decodeJSON(t, body, &m)
	if resp.StatusCode != http.StatusOK || !m.Fired || !m.Muted || m.Presence != PresenceFired {
		t.Fatalf("fire = %d %+v", resp.StatusCode, m)
	}
	eventually(t, "the cancel reaches the run", func() bool { return len(fm.find(http.MethodPost, "/v1/runs/r-mig/cancel")) == 1 })
	if c := fm.find(http.MethodPost, "/v1/runs/r-mig/cancel")[0]; c.Auth != "Bearer "+fleetToken {
		t.Fatalf("cancel = %+v", c)
	}
	if resp, _ := do(t, http.MethodDelete, url+"/v1/bridges/"+it.ID+"/members/migrator-1", "alice", ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("a second fire = %d", resp.StatusCode)
	}
	fired := withEvent(channel(t, s, it.ID), EventFired)
	if len(fired) != 1 || fired[0].Text != "Alice fired migrator-1" {
		t.Fatalf("fired lines = %+v", fired)
	}
	// A broadcast reaches the others only.
	do(t, http.MethodPost, url+"/v1/bridges/"+it.ID+"/messages", "alice", `{"text":"Who is left?"}`)
	eventually(t, "the reviewer gets it", func() bool { return len(fm.find(http.MethodPost, "/v1/runs/r-rev/messages")) == 1 })
	if n := len(fm.find(http.MethodPost, "/v1/runs/r-mig/messages")); n != 0 {
		t.Fatalf("the fired seat got %d messages", n)
	}
	// A job of the fired seat that waited in the queue starts and stops.
	v.put(workerRun("r-mig-late", it.ID, "migrator-1", "migrator", fw.StateRunning))
	s.poll(context.Background())
	eventually(t, "the late run stops", func() bool { return len(fm.find(http.MethodPost, "/v1/runs/r-mig-late/cancel")) == 1 })
	if resp, _ := do(t, http.MethodPatch, url+"/v1/bridges/"+it.ID+"/members/migrator-1", "alice", `{"muted":false}`); resp.StatusCode != http.StatusConflict {
		t.Fatalf("a change of a fired seat = %d", resp.StatusCode)
	}
}

// Mute, unmute, and a new task each post a line; a new task reaches the
// newest run of the seat as a follow-up, and a seat whose run was canceled
// gets a new run.
func TestPatchMember(t *testing.T) {
	a := newAgent(t, nil)
	s := newStore(t, a, nil)
	v, fm := withFleet(t, a)
	v.state("default/crew-0", fleet.MemberLive)
	url := serve(t, a)
	q := captureQueues(t, a, "fw.jobs.reviewer")
	it := open(t, url)
	v.put(workerRun("r-mig", it.ID, "migrator-1", "migrator", fw.StateCompleted))
	v.put(workerRun("r-rev", it.ID, "reviewer", "reviewer", fw.StateCanceled))
	s.poll(context.Background())
	resp, body := do(t, http.MethodPatch, url+"/v1/bridges/"+it.ID+"/members/migrator-1", "alice", `{"muted":true,"task":"Migrate the cron module too"}`)
	var m Member
	decodeJSON(t, body, &m)
	if resp.StatusCode != http.StatusOK || !m.Muted || m.Task != "Migrate the cron module too" {
		t.Fatalf("patch = %d %+v", resp.StatusCode, m)
	}
	msgs := channel(t, s, it.ID)
	if len(withEvent(msgs, EventMuted)) != 1 || withEvent(msgs, EventTask)[0].Text != "migrator-1 has a new task: Migrate the cron module too" {
		t.Fatalf("lines = %+v", msgs)
	}
	eventually(t, "the new task reaches the run", func() bool { return len(fm.find(http.MethodPost, "/v1/runs/r-mig/messages")) == 1 })
	if c, _ := fm.find(http.MethodPost, "/v1/runs/r-mig/messages")[0].Body["content"].(string); !strings.Contains(c, "] @migrator-1 from operator: Your new task: Migrate the cron module too") {
		t.Fatalf("content = %q", c)
	}
	// The canceled reviewer gets a new run, with the next idempotency key.
	do(t, http.MethodPatch, url+"/v1/bridges/"+it.ID+"/members/reviewer", "alice", `{"task":"Review the cron module"}`)
	eventually(t, "the reviewer is seated again", func() bool {
		r, _, ok := q.get("reviewer")
		return ok && r.Input == "Review the cron module"
	})
	if r, _, _ := q.get("reviewer"); r.IdempotencyKey != "bridge:"+it.ID+":reviewer:2" {
		t.Fatalf("key = %s", r.IdempotencyKey)
	}
}

// Seat again: a patch with the same task seats a seat whose newest run
// failed again, with the next key, and a second such patch before the new
// run shows seats nothing more. A patch with a task on a seat that works
// starts no run: a new text changes the task of the record and reaches the
// run as a follow-up, and the same text changes nothing.
func TestSeatAgainWithTheSameTask(t *testing.T) {
	a := newAgent(t, nil)
	s := newStore(t, a, nil)
	v, fm := withFleet(t, a)
	v.state("default/crew-0", fleet.MemberLive)
	url := serve(t, a)
	var mu sync.Mutex
	keys := map[string]int{}
	inputs := map[string]string{}
	for _, queue := range []string{"fw.jobs.migrator", "fw.jobs.reviewer"} {
		stop, err := a.Bus().Subscribe(context.Background(), queue, func(e fw.Event) error {
			var req fw.RunRequest
			if e.Type == fleet.EvRunRequest && e.Decode(&req) == nil {
				mu.Lock()
				keys[req.IdempotencyKey]++
				inputs[req.IdempotencyKey] = req.Input
				mu.Unlock()
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(stop)
	}
	count := func(key string) int {
		mu.Lock()
		defer mu.Unlock()
		return keys[key]
	}
	it := open(t, url)
	failed := workerRun("r-mig", it.ID, "migrator-1", "migrator", fw.StateFailed)
	failed.Error = "model: the provider answered 500"
	v.put(failed)
	v.put(workerRun("r-rev", it.ID, "reviewer", "reviewer", fw.StateRunning))
	s.poll(context.Background())

	same := `{"task":"Migrate the logrotate module"}`
	resp, body := do(t, http.MethodPatch, url+"/v1/bridges/"+it.ID+"/members/migrator-1", "alice", same)
	var m Member
	decodeJSON(t, body, &m)
	if resp.StatusCode != http.StatusOK || m.Task != "Migrate the logrotate module" {
		t.Fatalf("seat again = %d %s", resp.StatusCode, body)
	}
	again := "bridge:" + it.ID + ":migrator-1:2"
	eventually(t, "the seat runs again", func() bool { return count(again) == 1 })
	mu.Lock()
	input := inputs[again]
	mu.Unlock()
	if input != "Migrate the logrotate module" {
		t.Fatalf("the new run has the input %q", input)
	}
	lines := withEvent(channel(t, s, it.ID), EventTask)
	if len(lines) != 1 || lines[0].Text != "Alice seated migrator-1 again on its task: Migrate the logrotate module" {
		t.Fatalf("task lines = %+v", lines)
	}
	// A double click: the new run has not shown yet, so nothing more.
	do(t, http.MethodPatch, url+"/v1/bridges/"+it.ID+"/members/migrator-1", "alice", same)
	time.Sleep(100 * time.Millisecond)
	if n := count("bridge:" + it.ID + ":migrator-1:3"); n != 0 || count(again) != 1 {
		t.Fatalf("a second patch seated %d more runs", n)
	}
	// Once the new run is canceled, the same patch seats the seat once more.
	next := workerRun("r-mig-2", it.ID, "migrator-1", "migrator", fw.StateCanceled)
	next.CreatedAt = failed.CreatedAt.Add(time.Second)
	v.put(next)
	s.poll(context.Background())
	do(t, http.MethodPatch, url+"/v1/bridges/"+it.ID+"/members/migrator-1", "alice", same)
	eventually(t, "the canceled seat runs again", func() bool { return count("bridge:"+it.ID+":migrator-1:3") == 1 })

	// The reviewer works: a new text changes the record and reaches the run,
	// and no run starts.
	resp, body = do(t, http.MethodPatch, url+"/v1/bridges/"+it.ID+"/members/reviewer", "alice", `{"task":"Review the cron module"}`)
	decodeJSON(t, body, &m)
	if resp.StatusCode != http.StatusOK || m.Task != "Review the cron module" {
		t.Fatalf("a new task of a seat that works = %d %s", resp.StatusCode, body)
	}
	eventually(t, "the new task reaches the run", func() bool { return len(fm.find(http.MethodPost, "/v1/runs/r-rev/messages")) == 1 })
	do(t, http.MethodPatch, url+"/v1/bridges/"+it.ID+"/members/reviewer", "alice", `{"task":"Review the cron module"}`)
	time.Sleep(100 * time.Millisecond)
	if n := count("bridge:" + it.ID + ":reviewer:2"); n != 0 {
		t.Fatalf("a seat that works was seated again %d times", n)
	}
	if n := len(fm.find(http.MethodPost, "/v1/runs/r-rev/messages")); n != 1 {
		t.Fatalf("the same task reached the run again: %d messages", n)
	}
	if lines := withEvent(channel(t, s, it.ID), EventTask); len(lines) != 3 {
		t.Fatalf("task lines = %+v", lines)
	}
}

// A close cancels nothing, posts one line, and asks the manager run of the
// bridge for the report, whose link the channel gets once the page exists.
// An open after it posts a line too.
func TestCloseAsksForTheReport(t *testing.T) {
	a := newAgent(t, nil)
	t.Setenv(EnvManager, "manager")
	s := newStore(t, a, nil)
	v, fm := withFleet(t, a)
	v.state("default/manager-0", fleet.MemberLive, "manager")
	url := serve(t, a)
	resp, body := do(t, http.MethodPost, url+"/v1/bridges", "alice", `{"title":"Spawned","placement":"spawn","members":[{"handle":"migrator-1","agent":"migrator","task":"Migrate"}]}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("open = %d %s", resp.StatusCode, body)
	}
	var it Item
	decodeJSON(t, body, &it)
	v.put(&fw.Run{ID: "mgr-run-1", Agent: "manager", Member: "default/manager-0", State: fw.StateCompleted, Metadata: map[string]string{fw.MetaBatch: BatchOf(it.ID)}})

	resp, body = do(t, http.MethodPatch, url+"/v1/bridges/"+it.ID, "alice", `{"state":"closed"}`)
	decodeJSON(t, body, &it)
	if resp.StatusCode != http.StatusOK || it.State != StateClosed || it.ClosedAt == nil {
		t.Fatalf("close = %d %s", resp.StatusCode, body)
	}
	eventually(t, "the manager gets the ask", func() bool { return len(fm.find(http.MethodPost, "/v1/runs/mgr-run-1/messages")) == 1 })
	ask := fm.find(http.MethodPost, "/v1/runs/mgr-run-1/messages")[0]
	content, _ := ask.Body["content"].(string)
	from, _ := ask.Body["from"].(map[string]any)
	if !strings.Contains(content, "bridges/"+it.ID+"/report.md") || !strings.Contains(content, "pages_write") || from["member"] != HandleManager || from["name"] != "alice" {
		t.Fatalf("ask = %+v", ask.Body)
	}
	do(t, http.MethodPatch, url+"/v1/bridges/"+it.ID, "alice", `{"state":"closed"}`)
	if l := withEvent(channel(t, s, it.ID), EventClosed); len(l) != 1 || l[0].Text != "Alice closed the bridge" {
		t.Fatalf("closed lines = %+v", l)
	}
	if len(fm.find(http.MethodPost, "/v1/runs/mgr-run-1/cancel")) != 0 {
		t.Fatal("the close canceled a run")
	}
	// The report appears; the channel links it once.
	if _, err := pages.Of(a).Write(context.Background(), "bridges/"+it.ID+"/report.md", "# Report\n\nAll done.\n", "agent:manager", "report", pages.IfAny); err != nil {
		t.Fatal(err)
	}
	s.poll(context.Background())
	s.poll(context.Background())
	var links []Message
	for _, m := range channel(t, s, it.ID) {
		if m.Page != "" {
			links = append(links, m)
		}
	}
	report := "bridges/" + it.ID + "/report.md"
	if len(links) != 1 || links[0].Page != report || links[0].Event != EventChanged || links[0].Text != "The report is in the wiki: "+report || strings.Contains(links[0].Text, "#page/") {
		t.Fatalf("the lines of the report = %+v", links)
	}
	// The page field reaches the routes and the hooks with the entry.
	_, body = do(t, http.MethodGet, url+"/v1/bridges/"+it.ID+"/messages", "alice", "")
	if !strings.Contains(body, `"page":"`+report+`"`) {
		t.Fatalf("the channel holds no page field: %s", body)
	}
	resp, body = do(t, http.MethodPatch, url+"/v1/bridges/"+it.ID, "alice", `{"state":"open"}`)
	decodeJSON(t, body, &it)
	if resp.StatusCode != http.StatusOK || it.State != StateOpen || it.ClosedAt != nil || len(withEvent(channel(t, s, it.ID), EventReopened)) != 1 {
		t.Fatalf("open again = %d %s", resp.StatusCode, body)
	}

	// A queue bridge has no manager run, so the close starts one for the
	// report.
	q := open(t, url)
	do(t, http.MethodPatch, url+"/v1/bridges/"+q.ID, "alice", `{"state":"closed"}`)
	starts := fm.find(http.MethodPost, "/v1/runs")
	last := starts[len(starts)-1]
	if input, _ := last.Body["input"].(string); len(starts) != 2 || !strings.Contains(input, "bridges/"+q.ID+"/report.md") || last.Idem == "" {
		t.Fatalf("the report run = %+v", last)
	}
	rec, _, _ := s.b.get(context.Background(), q.ID)
	if rec.Manager == nil || rec.Manager.Run != "mgr-run-1" {
		t.Fatalf("manager = %+v", rec.Manager)
	}

	// A manager run that stopped at a limit fails a follow-up at once, so
	// the close starts a new manager run for the report.
	v.put(&fw.Run{ID: "mgr-run-1", Agent: "manager", Member: "default/manager-0", State: fw.StateFailed, Error: "budget exceeded: maxTurns 12", Metadata: map[string]string{fw.MetaBatch: BatchOf(it.ID)}})
	before := len(fm.find(http.MethodPost, "/v1/runs/mgr-run-1/messages"))
	do(t, http.MethodPatch, url+"/v1/bridges/"+it.ID, "alice", `{"state":"closed"}`)
	starts = fm.find(http.MethodPost, "/v1/runs")
	if input, _ := starts[len(starts)-1].Body["input"].(string); len(starts) != 3 || !strings.Contains(input, "bridges/"+it.ID+"/report.md") {
		t.Fatalf("the report run after a limit = %+v", starts[len(starts)-1])
	}
	time.Sleep(50 * time.Millisecond)
	if n := len(fm.find(http.MethodPost, "/v1/runs/mgr-run-1/messages")); n != before {
		t.Fatalf("the stopped manager run got %d asks", n-before)
	}
}
