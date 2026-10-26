package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
	"weak"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/teams"
)

// scripted is a fake model that answers with the responses in order and
// then ends every turn with "ok".
func scripted(responses ...*fw.Response) fw.Model {
	var mu sync.Mutex
	next := 0
	return fw.ModelFunc{GenerateFn: func(context.Context, *fw.Request) (*fw.Response, error) {
		mu.Lock()
		defer mu.Unlock()
		if next < len(responses) {
			r := responses[next]
			next++
			return r, nil
		}
		return answer("ok"), nil
	}}
}

// calls is a model response with an optional text and tool calls.
func calls(text string, uses ...fw.Block) *fw.Response {
	var content []fw.Block
	if text != "" {
		content = append(content, fw.Text(text))
	}
	content = append(content, uses...)
	return &fw.Response{Content: content, StopReason: fw.StopToolUse, Usage: fw.Usage{InputTokens: 10, OutputTokens: 5}}
}

// answer is a model response that ends the turn with text.
func answer(text string) *fw.Response {
	return &fw.Response{Content: []fw.Block{fw.Text(text)}, StopReason: fw.StopEndTurn, Usage: fw.Usage{InputTokens: 10, OutputTokens: 5}}
}

// use is a tool call block.
func use(id, name, input string) fw.Block {
	return fw.Block{Type: fw.BlockToolUse, ID: id, Name: name, Input: json.RawMessage(input)}
}

// okTool is a tool that answers "done".
func okTool(spec fw.ToolSpec) fw.Tool {
	return fw.NewTool(spec, func(context.Context, json.RawMessage) (fw.ToolResult, error) {
		return fw.TextResult("done"), nil
	})
}

func TestStatusOf(t *testing.T) {
	for _, tc := range []struct {
		state  fw.RunState
		output string
		want   string
	}{
		{fw.StateInputRequired, "", StatusNeedsYou},
		{fw.StateQueued, "", StatusWorking},
		{fw.StateRunning, "", StatusWorking},
		{fw.StateSuspended, "", StatusWorking},
		{fw.StateCompleted, "Done.", StatusReady},
		{fw.StateCompleted, "", StatusEnded},
		{fw.StateCompleted, " \n", StatusEnded},
		{fw.StateFailed, "partial", StatusFailed},
		{fw.StateCanceled, "", StatusCanceled},
	} {
		if got := StatusOf(&fw.Run{State: tc.state, Output: tc.output}); got != tc.want {
			t.Errorf("StatusOf(%s, %q) = %q, want %q", tc.state, tc.output, got, tc.want)
		}
	}
	if got := StatusOf(nil); got != "" {
		t.Errorf("StatusOf(nil) = %q", got)
	}
}

func TestTitleOf(t *testing.T) {
	builder := map[string]string{ViewKey: ViewBuilder}
	for _, tc := range []struct {
		name string
		run  fw.Run
		want string
	}{
		{"title", fw.Run{Agent: "reviewer", Title: "Fix checkout retries", Input: "something else"}, "Fix checkout retries"},
		{"fleet run without title", fw.Run{Agent: "reviewer", Member: "m2", Input: "## Fix checkout retries. Then look at the logs."}, "Fix checkout retries"},
		{"no text", fw.Run{Agent: "reviewer", Input: "  \n"}, "reviewer"},
		{"builder from the page", fw.Run{Agent: "default", Title: "Make an agent from this conversation", Input: "Make an agent from this conversation", Metadata: builder}, "New agent"},
		{"builder without input", fw.Run{Agent: "default", Metadata: builder}, "New agent"},
		{"builder with a title", fw.Run{Agent: "default", Title: "PR reviewer", Metadata: map[string]string{ViewKey: ViewBuilder, fw.MetaTitle: " PR reviewer "}}, "PR reviewer"},
	} {
		if got := TitleOf(&tc.run); got != tc.want {
			t.Errorf("%s: TitleOf = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestHomeOf(t *testing.T) {
	a := newAgent(t)
	ctx := fw.WithPrincipal(context.Background(), &fw.Principal{Subject: "alice", Kind: fw.KindUser, Groups: []string{"pay"}})
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	run := func(id, who string, st fw.RunState, ago time.Duration) *fw.Run {
		return &fw.Run{ID: id, Agent: "reviewer", Principal: who, State: st, Input: "Task " + id, CreatedAt: now.Add(-ago - time.Minute), UpdatedAt: now.Add(-ago)}
	}
	waitOld := run("wait-old", "alice", fw.StateInputRequired, time.Hour)
	waitNew := run("wait-new", "alice", fw.StateInputRequired, time.Minute)
	// A run of a teammate on another member of the fleet: no title, and
	// the teams of the person.
	fleet := &fw.Run{ID: "fleet", Agent: "fixer", Principal: "bob", Metadata: map[string]string{teams.MetaKey: "pay"}, Member: "m2", State: fw.StateRunning, Input: "Fix checkout retries. Then more.", UpdatedAt: now.Add(-2 * time.Minute)}
	paused := run("paused", "alice", fw.StateSuspended, 3*time.Hour)
	ready := run("ready", "alice", fw.StateCompleted, time.Hour)
	ready.Output = "All good."
	failed := run("failed", "alice", fw.StateFailed, 2*time.Hour)
	canceled := run("canceled", "alice", fw.StateCanceled, 30*time.Minute)
	old := run("old", "alice", fw.StateCompleted, 8*24*time.Hour)
	child := run("child", "alice", fw.StateRunning, 0)
	child.Parent = "ready"
	builder := run("builder", "alice", fw.StateCompleted, 10*time.Minute)
	builder.Metadata = map[string]string{ViewKey: ViewBuilder}
	builder.Title = "Make an agent from this conversation"
	other := run("other", "carol", fw.StateRunning, 0)
	otherChild := run("other-child", "carol", fw.StateCompleted, 0)
	otherChild.Parent = "other"
	runs := []*fw.Run{waitOld, waitNew, fleet, paused, ready, failed, canceled, old, child, builder, other, otherChild, nil}

	h, err := HomeOf(ctx, a, runs, now)
	if err != nil {
		t.Fatal(err)
	}
	ids := func(items []WorkItem) string {
		var s []string
		for _, it := range items {
			s = append(s, it.ID+":"+it.Status)
		}
		return strings.Join(s, " ")
	}
	for _, tc := range []struct{ name, got, want string }{
		{"needs you", ids(h.NeedsYou), "wait-new:needs-you wait-old:needs-you"},
		{"in progress", ids(h.InProgress), "fleet:working paused:working"},
		{"recent", ids(h.Recent), "builder:ended canceled:canceled ready:ready failed:failed"},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %s, want %s", tc.name, tc.got, tc.want)
		}
	}
	if h.Others != 1 {
		t.Errorf("others = %d, want 1", h.Others)
	}
	if h.InProgress[0].Title != "Fix checkout retries" || h.Recent[0].Title != "New agent" || h.NeedsYou[0].Title != "Task wait-new" {
		t.Errorf("titles = %q, %q, %q", h.InProgress[0].Title, h.Recent[0].Title, h.NeedsYou[0].Title)
	}
	// The home copies the runs. The runs of the kernel and of the fleet
	// view stay as they were.
	if fleet.Title != "" || builder.Title != "Make an agent from this conversation" || h.InProgress[0].Run == fleet {
		t.Errorf("HomeOf changed a run: %q, %q", fleet.Title, builder.Title)
	}
	if h.Glance.Working != 2 || h.Glance.Waiting != 2 || h.Glance.Agents != 2 || h.Sentence != h.Glance.Sentence || h.Sentence != "Two agents are working. Two runs need you." {
		t.Errorf("glance = %+v, sentence %q", h.Glance, h.Sentence)
	}

	b, _ := json.Marshal(h.InProgress[0])
	for _, want := range []string{`"id":"fleet"`, `"title":"Fix checkout retries"`, `"status":"working"`, `"member":"m2"`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("work item JSON lacks %s: %s", want, b)
		}
	}
}

func TestHomeOfEmptyAndBounded(t *testing.T) {
	a := newAgent(t)
	ctx := fw.WithPrincipal(context.Background(), &fw.Principal{Subject: "alice", Kind: fw.KindUser})
	now := time.Now()
	h, err := HomeOf(ctx, a, nil, now)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(h)
	for _, want := range []string{`"needsYou":[]`, `"inProgress":[]`, `"recent":[]`, `"others":0`, `"sentence":"Nothing is running right now."`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("empty home JSON lacks %s: %s", want, b)
		}
	}
	var runs []*fw.Run
	for i := range HomeRecent + 5 {
		runs = append(runs, &fw.Run{ID: fmt.Sprintf("r%02d", i), Principal: "alice", State: fw.StateCompleted, UpdatedAt: now.Add(-time.Duration(i) * time.Minute)})
	}
	h, _ = HomeOf(ctx, a, runs, now)
	if len(h.Recent) != HomeRecent || h.Recent[0].ID != "r00" || h.Recent[HomeRecent-1].ID != fmt.Sprintf("r%02d", HomeRecent-1) {
		t.Errorf("recent = %d runs, first %s", len(h.Recent), h.Recent[0].ID)
	}
}

func TestHomeString(t *testing.T) {
	now := time.Now()
	item := func(id, title, status string) WorkItem {
		return WorkItem{Run: &fw.Run{ID: id, Agent: "reviewer", Title: title, UpdatedAt: now.Add(-5 * time.Minute)}, Status: status}
	}
	h := Home{
		Sentence:   "One agent is working. One run needs you.",
		NeedsYou:   []WorkItem{item("r1", "Fix checkout retries", StatusNeedsYou)},
		InProgress: []WorkItem{item("r2", "Write the notes", StatusWorking)},
		Recent:     []WorkItem{item("r3", "Review PR 42", StatusReady)},
		Others:     3,
	}
	want := "One agent is working. One run needs you.\n\n" +
		"Needs you:\n- Fix checkout retries (reviewer, needs you, 5 minutes ago, run r1)\n\n" +
		"In progress:\n- Write the notes (reviewer, working, 5 minutes ago, run r2)\n\n" +
		"Recent results:\n- Review PR 42 (reviewer, ready to review, 5 minutes ago, run r3)\n\n" +
		"3 runs by others."
	if got := h.String(); got != want {
		t.Errorf("text =\n%s\nwant\n%s", got, want)
	}
	if got := (Home{Sentence: "Nothing is running right now.", Others: 1}).String(); got != "Nothing is running right now.\n\n1 run by others." {
		t.Errorf("short text = %q", got)
	}
}

func TestActionLabel(t *testing.T) {
	for _, tc := range []struct {
		spec     fw.ToolSpec
		question bool
		want     string
	}{
		{fw.ToolSpec{Name: "bash", Action: "Run command"}, false, "Run command"},
		{fw.ToolSpec{Name: "gh", Action: " Open pull request "}, false, "Open pull request"},
		{fw.ToolSpec{Name: "deploy"}, false, "Allow"},
		{fw.ToolSpec{}, false, "Allow"},
		{fw.ToolSpec{Name: "ask_user"}, true, "Answer"},
		{fw.ToolSpec{Name: "bash", Action: "Run command"}, true, "Answer"},
	} {
		if got := ActionLabel(tc.spec, tc.question); got != tc.want {
			t.Errorf("ActionLabel(%+v, %v) = %q, want %q", tc.spec, tc.question, got, tc.want)
		}
	}
}

func TestBuilderRequest(t *testing.T) {
	req := BuilderRequest("Review pull requests")
	want := fw.RunRequest{
		Input:    "Review pull requests",
		Profile:  &fw.AgentProfile{System: BuilderPrompt, Tools: []string{"propose_agent", "ask_user"}},
		Metadata: map[string]string{ViewKey: ViewBuilder, fw.MetaTitle: "New agent"},
	}
	if !reflect.DeepEqual(req, want) {
		t.Fatalf("request = %+v\nwant      %+v", req, want)
	}
	if err := fw.CheckMetadata(req.Metadata); err != nil {
		t.Fatal(err)
	}
}

func TestProposalProfile(t *testing.T) {
	for _, tc := range []struct {
		name  string
		input string
		want  fw.AgentProfile
		err   bool
	}{
		{
			name:  "full",
			input: `{"name":"pr-reviewer","description":"Reviews pull requests.","instructions":"You review.","tools":["bash","files"],"asks_first":["files"],"never_runs":["git push*","rm -rf*"],"minutes":30}`,
			want: fw.AgentProfile{
				Name: "pr-reviewer", Description: "Reviews pull requests.", System: "You review.",
				Tools:  []string{"bash", "files"},
				Policy: &fw.Rules{Ask: []string{"files"}, Deny: []string{"bash: git push*", "bash: rm -rf*"}},
				Budget: &fw.Budget{Deadline: 30 * time.Minute},
			},
		},
		{
			name:  "deny only",
			input: `{"name":"x","instructions":"Go.","tools":["bash"],"never_runs":["git push*"]}`,
			want:  fw.AgentProfile{Name: "x", System: "Go.", Tools: []string{"bash"}, Policy: &fw.Rules{Deny: []string{"bash: git push*"}}},
		},
		{
			name:  "no tools and no limits",
			input: `{"name":"x","tools":"bash","asks_first":[],"minutes":0}`,
			want:  fw.AgentProfile{Name: "x", Tools: []string{}},
		},
		{name: "no name", input: `{"description":"d","tools":[]}`, err: true},
		{name: "blank name", input: `{"name":"  ","tools":[]}`, err: true},
		{name: "a cut input", input: `"{\"name\":\"x\",\"tools\":[\"ba"`, err: true},
		{name: "null", input: `null`, err: true},
		{name: "not JSON", input: `{"name":`, err: true},
	} {
		p, err := ProposalProfile(json.RawMessage(tc.input))
		if tc.err {
			if !errors.Is(err, fw.ErrInvalidRequest) || !strings.HasPrefix(err.Error(), "dashboard: ") {
				t.Errorf("%s: err = %v, want a wrapped fw.ErrInvalidRequest", tc.name, err)
			}
			continue
		}
		if err != nil || !reflect.DeepEqual(p, tc.want) {
			t.Errorf("%s: profile = %+v, %v\nwant %+v", tc.name, p, err, tc.want)
		}
	}
}

func TestTranscriptBefore(t *testing.T) {
	ctx := context.Background()
	a := newAgent(t, fw.WithModel(scripted(calls("", use("c1", "echo", `{}`)), answer("done"))), fw.WithTools(okTool(fw.ToolSpec{Name: "echo"})))
	r, err := a.Run(ctx, "go")
	if err != nil {
		t.Fatal(err)
	}
	all, err := readAll(ctx, a, r.ID, 0)
	if err != nil || len(all) < 8 {
		t.Fatalf("ledger = %d events, %v", len(all), err)
	}
	n := len(all)
	seqs := func(events []fw.Event) []uint64 {
		out := []uint64{}
		for _, e := range events {
			out = append(out, e.Seq)
		}
		return out
	}

	// Page back from the end to the first event.
	var pages [][]fw.Event
	before := uint64(0)
	for range n {
		page, prev, err := TranscriptBefore(ctx, a, r.ID, before, 3)
		if err != nil {
			t.Fatal(err)
		}
		pages = append([][]fw.Event{page}, pages...)
		if prev == 0 {
			break
		}
		if prev != page[0].Seq {
			t.Fatalf("prev = %d, first of the page = %d", prev, page[0].Seq)
		}
		before = prev
	}
	var joined []fw.Event
	for _, p := range pages {
		joined = append(joined, p...)
	}
	if want := (n + 2) / 3; len(pages) != want || !reflect.DeepEqual(seqs(joined), seqs(all)) {
		t.Fatalf("%d pages hold %v, want %d pages with %v", len(pages), seqs(joined), want, seqs(all))
	}

	for _, tc := range []struct {
		name     string
		before   uint64
		limit    int
		from, to int // the page is all[from:to]
		prev     uint64
	}{
		{"the last page", 0, 3, n - 3, n, all[n-3].Seq},
		{"a before past the end", all[n-1].Seq + 10, 3, n - 3, n, all[n-3].Seq},
		{"the default limit", 0, 0, 0, n, 0},
		{"a page that ends at the second event", all[1].Seq, 3, 0, 1, 0},
		{"a page of exactly the rest", all[3].Seq, 3, 0, 3, 0},
		{"one more than the page", all[4].Seq, 3, 1, 4, all[1].Seq},
		{"the whole run less one", 0, n - 1, 1, n, all[1].Seq},
		{"the whole run", 0, n, 0, n, 0},
		{"before the first event", all[0].Seq, 3, 0, 0, 0},
	} {
		page, prev, err := TranscriptBefore(ctx, a, r.ID, tc.before, tc.limit)
		if err != nil || page == nil || !reflect.DeepEqual(seqs(page), seqs(all[tc.from:tc.to])) || prev != tc.prev {
			t.Errorf("%s: page %v, prev %d, %v; want %v, prev %d", tc.name, seqs(page), prev, err, seqs(all[tc.from:tc.to]), tc.prev)
		}
	}
}

// paddedLedger appends filler events to a read of one run, after the
// events of the run. Each filler has a payload of its own. Before the last
// filler, it counts how many earlier payloads are still in memory. A reader
// that holds the whole ledger keeps them all.
type paddedLedger struct {
	fw.Ledger
	mu    sync.Mutex
	run   string
	n     int
	alive int // the payloads in memory before the last filler, -1 before a padded read
}

// pad pads the next reads of run with n fillers.
func (l *paddedLedger) pad(run string, n int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.run, l.n, l.alive = run, n, -1
}

func (l *paddedLedger) kept() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.alive
}

func (l *paddedLedger) Read(ctx context.Context, runID string, from uint64) iter.Seq2[fw.Event, error] {
	l.mu.Lock()
	n := l.n
	if runID != l.run {
		n = 0
	}
	l.mu.Unlock()
	inner := l.Ledger.Read(ctx, runID, from)
	if n == 0 {
		return inner
	}
	return func(yield func(fw.Event, error) bool) {
		var seq uint64
		for e, err := range inner {
			if !yield(e, err) || err != nil {
				return
			}
			seq = e.Seq
		}
		payloads := make([]weak.Pointer[byte], 0, n)
		for i := range n {
			if i == n-1 {
				runtime.GC()
				alive := 0
				for _, p := range payloads {
					if p.Value() != nil {
						alive++
					}
				}
				l.mu.Lock()
				l.alive = alive
				l.mu.Unlock()
			}
			data := []byte(`"` + strings.Repeat("x", 4<<10) + `"`)
			payloads = append(payloads, weak.Make(&data[0]))
			seq++
			if !yield(fw.Event{SpecVersion: "1.0", ID: fmt.Sprint("filler-", seq), Type: "test.filler", Subject: runID, Seq: seq, Data: data}, nil) {
				return
			}
		}
	}
}

// ResultOf, TranscriptBefore, and SaveAsAgentInput read a long ledger
// while it streams: none of them holds the whole ledger in memory.
func TestLongLedgerStaysOutOfMemory(t *testing.T) {
	ctx := context.Background()
	l := &paddedLedger{Ledger: fw.NewMemLedger()}
	a := newAgent(t, fw.WithLedger(l), fw.WithModel(scripted(calls("", use("c1", "echo", `{}`)), answer("done"))), fw.WithTools(okTool(fw.ToolSpec{Name: "echo"})))
	r, err := a.Run(ctx, "go")
	if err != nil || r.State != fw.StateCompleted {
		t.Fatalf("run = %+v, %v", r, err)
	}
	const n, limit = 200, 10
	check := func(name string, most int, read func() error) {
		t.Helper()
		l.pad(r.ID, n)
		err := read()
		kept := l.kept()
		l.pad("", 0)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if kept < 0 || kept > most {
			t.Errorf("%s keeps %d of %d earlier events in memory, want at most %d", name, kept, n-1, most)
		}
	}
	check("ResultOf", 2, func() error {
		res, err := ResultOf(ctx, a, r.ID)
		if err == nil && (res.Summary != "done" || len(res.Steps) != 1) {
			t.Errorf("result = %+v", res)
		}
		return err
	})
	check("TranscriptBefore", limit+2, func() error {
		events, prev, err := TranscriptBefore(ctx, a, r.ID, 0, limit)
		if err == nil && (len(events) != limit || prev != events[0].Seq || events[limit-1].Type != "test.filler") {
			t.Errorf("page of %d events, prev %d", len(events), prev)
		}
		return err
	})
	check("SaveAsAgentInput", 2, func() error {
		text, err := SaveAsAgentInput(ctx, a, r.ID)
		if err == nil && !strings.Contains(text, "- go\n") {
			t.Errorf("input = %q", text)
		}
		return err
	})
}

// Save as agent takes a completed top-level run that the caller started,
// for a caller who can create agents. A view of the run is not enough,
// because the builder run belongs to the caller and the teams of the
// caller.
func TestSaveAsAgentTerms(t *testing.T) {
	ctx := context.Background()
	a := newAgent(t, fw.WithProfile(fw.AgentProfile{Name: "reviewer", Access: &fw.Access{Owners: []string{"alice"}, Viewers: []string{"bob"}}}))
	alice := fw.WithPrincipal(ctx, &fw.Principal{Subject: "alice", Kind: fw.KindUser})
	bob := fw.WithPrincipal(ctx, &fw.Principal{Subject: "bob", Kind: fw.KindUser})
	r, err := a.Run(alice, "Review PR 42", fw.AsAgent("reviewer"))
	if err != nil || r.State != fw.StateCompleted {
		t.Fatalf("run = %+v, %v", r, err)
	}
	if _, err := a.Get(bob, r.ID); err != nil {
		t.Fatalf("bob does not view the run: %v", err)
	}
	if _, err := SaveAsAgentInput(bob, a, r.ID); !errors.Is(err, fw.ErrForbidden) || !strings.Contains(err.Error(), "only the person who started run "+r.ID) {
		t.Errorf("bob saves the run of alice: %v", err)
	}
	if text, err := SaveAsAgentInput(alice, a, r.ID); err != nil || !strings.Contains(text, "Review PR 42") {
		t.Errorf("alice saves her run: %q, %v", text, err)
	}
	req := BuilderRequest("An agent for triage")
	b, err := a.Run(alice, req.Input, fw.WithRunProfile(*req.Profile), fw.WithMetadata(req.Metadata))
	if err != nil || b.State != fw.StateCompleted {
		t.Fatalf("builder run = %+v, %v", b, err)
	}
	if _, err := SaveAsAgentInput(alice, a, b.ID); !errors.Is(err, fw.ErrForbidden) || !strings.Contains(err.Error(), "agent builder") {
		t.Errorf("a builder run becomes an agent: %v", err)
	}
}
