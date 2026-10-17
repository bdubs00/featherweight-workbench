package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// kinds lists the kinds of next actions.
func kinds(next []NextAction) string {
	var s []string
	for _, n := range next {
		s = append(s, n.Kind+"="+n.Label)
	}
	return strings.Join(s, " ")
}

// mustResult builds the result of a run or fails the test.
func mustResult(t *testing.T, ctx context.Context, a *fw.Agent, id string) Result {
	t.Helper()
	res, err := ResultOf(ctx, a, id)
	if err != nil {
		t.Fatalf("ResultOf: %v", err)
	}
	return res
}

// A run that waits for the approval of bash, a tool with an action label,
// and then finishes.
func TestResultApproval(t *testing.T) {
	ctx := context.Background()
	bash := okTool(fw.ToolSpec{Name: "bash", Title: "Run shell commands", Action: "Run command"})
	a := newAgent(t,
		fw.WithModel(scripted(calls("Let me push.", use("c1", "bash", `{"command":"git push origin main"}`)), answer("Pushed the branch."))),
		fw.WithTools(bash),
		fw.WithProfile(fw.AgentProfile{Name: "ops", Policy: &fw.Rules{Ask: []string{"bash"}}}),
	)
	r, err := a.Run(ctx, "Push the release branch", fw.AsAgent("ops"))
	if err != nil || r.State != fw.StateInputRequired {
		t.Fatalf("run = %+v, %v", r, err)
	}
	res := mustResult(t, ctx, a, r.ID)
	p := res.Pending
	if res.Status != StatusNeedsYou || res.Title != "Push the release branch" || res.Agent != "ops" || res.Summary != "Let me push." || p == nil {
		t.Fatalf("result = %+v", res)
	}
	if p.ID != r.Pending.ID || p.Tool != "bash" || p.Label != "Run command" || p.Question || p.Effect != `Runs "git push origin main".` ||
		string(p.Input) != `{"command":"git push origin main"}` || p.Schema != nil {
		t.Errorf("pending = %+v", p)
	}
	if &p.Input[0] == &r.Pending.Input[0] {
		t.Error("the decision shares its input with the run")
	}
	if got := kinds(res.Next); got != "decide=Run command" {
		t.Errorf("next = %s", got)
	}
	if len(res.Steps) != 0 {
		t.Errorf("a call that did not run is a step: %+v", res.Steps)
	}
	text := res.String()
	for _, want := range []string{"Push the release branch\nRun " + r.ID + " of the agent ops: needs you.", "Waiting for your decision (approval " + p.ID + "): Run command, the tool bash.", `Runs "git push origin main".`, "Next: Run command."} {
		if !strings.Contains(text, want) {
			t.Errorf("text lacks %q:\n%s", want, text)
		}
	}

	if err := a.Decide(ctx, r.ID, fw.ApprovalDecision{ApprovalID: p.ID, Decision: "allow"}); err != nil {
		t.Fatal(err)
	}
	if r, err = a.Wait(ctx, r.ID); err != nil || r.State != fw.StateCompleted {
		t.Fatalf("run = %+v, %v", r, err)
	}
	res = mustResult(t, ctx, a, r.ID)
	if res.Status != StatusReady || res.Summary != "Pushed the branch." || res.Pending != nil || res.Usage == nil || res.Usage.Turns != r.Usage.Turns || res.Usage.ToolCalls != 1 {
		t.Fatalf("result = %+v", res)
	}
	if want := []ResultStep{{Seq: res.Steps[0].Seq, Tool: "bash", Text: `Ran "git push origin main"`, Done: true}}; !reflect.DeepEqual(res.Steps, want) || res.Steps[0].Seq == 0 {
		t.Errorf("steps = %+v", res.Steps)
	}
	if got := kinds(res.Next); got != "reply=Reply save-agent=Save as agent" {
		t.Errorf("next = %s", got)
	}
}

// A run that waits for an answer to a question.
func TestResultQuestion(t *testing.T) {
	ctx := context.Background()
	schema := `{"type":"object","properties":{"branch":{"type":"string","enum":["main","dev"]}},"required":["branch"]}`
	ask := fw.NewTool(fw.ToolSpec{Name: "ask_user", Title: "Ask you questions", ReadOnly: true}, func(ctx context.Context, _ json.RawMessage) (fw.ToolResult, error) {
		raw, err := fw.AskCaller(ctx, "Which branch?", []byte(schema))
		if err != nil {
			return fw.ErrorResult(err.Error()), nil
		}
		return fw.TextResult(string(raw)), nil
	})
	a := newAgent(t, fw.WithModel(scripted(calls("One question first.", use("q1", "ask_user", `{"question":"Which branch?"}`)))), fw.WithTools(ask))
	r, err := a.Run(ctx, "Deploy the app")
	if err != nil || r.State != fw.StateInputRequired {
		t.Fatalf("run = %+v, %v", r, err)
	}
	res := mustResult(t, ctx, a, r.ID)
	p := res.Pending
	if p == nil || !p.Question || p.Label != "Answer" || p.Effect != "" || p.Reason != "Which branch?" || string(p.Schema) != schema || p.Tool != "ask_user" {
		t.Fatalf("pending = %+v", p)
	}
	if got := kinds(res.Next); got != "answer=Answer" {
		t.Errorf("next = %s", got)
	}
	// The question comes while the call runs, so the step has no result.
	if len(res.Steps) != 1 || res.Steps[0].Text != "Ask you questions" || res.Steps[0].Done {
		t.Errorf("steps = %+v", res.Steps)
	}
	text := res.String()
	for _, want := range []string{"Waiting for your answer (approval " + p.ID + "):\nWhich branch?", "- Ask you questions (no result yet)", "Next: Answer."} {
		if !strings.Contains(text, want) {
			t.Errorf("text lacks %q:\n%s", want, text)
		}
	}
}

// filesTool changes the workspace as its input says: create and
// str_replace write the file, rename moves it, and delete removes it.
func filesTool() fw.Tool {
	return fw.NewTool(fw.ToolSpec{Name: "files", Title: "Read and change files"}, func(ctx context.Context, raw json.RawMessage) (fw.ToolResult, error) {
		var in struct {
			Command string `json:"command"`
			Path    string `json:"path"`
			NewPath string `json:"new_path"`
			Text    string `json:"file_text"`
		}
		if err := json.Unmarshal(raw, &in); err != nil {
			return fw.ErrorResult(err.Error()), nil
		}
		ws := fw.FilesFrom(ctx)
		var err error
		switch in.Command {
		case "create", "str_replace":
			err = ws.WriteFile(in.Path, []byte(in.Text))
		case "rename":
			err = ws.Rename(in.Path, in.NewPath)
		case "delete":
			err = ws.Remove(in.Path)
		case "view":
			_, err = fs.ReadFile(ws, in.Path)
		}
		if err != nil {
			return fw.ErrorResult(err.Error()), nil
		}
		return fw.TextResult("ok"), nil
	})
}

// A run that creates, edits, renames, and removes files.
func TestResultFiles(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	for name, data := range map[string]string{"pre.txt": "before", "edit.txt": "old", "gone.txt": "bye"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cfg := fw.DefaultConfig()
	cfg.Home = filepath.Join(root, ".fw")
	cfg.Tools.Files.Root = root
	model := scripted(
		calls("Working on the files.",
			use("c1", "files", `{"command":"create","path":"notes/a.md","file_text":"# A"}`),
			use("c2", "files", `{"command":"str_replace","path":"notes/a.md","file_text":"# A\nmore"}`),
			use("c3", "files", `{"command":"create","path":"tmp.txt","file_text":"tmp"}`),
			use("c4", "files", `{"command":"rename","path":"tmp.txt","new_path":"moved.txt"}`),
			use("c5", "files", `{"command":"rename","path":"pre.txt","new_path":"post.txt"}`),
			use("c6", "files", `{"command":"str_replace","path":"edit.txt","file_text":"new"}`),
			use("c7", "files", `{"command":"delete","path":"gone.txt"}`),
			use("c8", "files", `{"command":"view","path":"missing.txt"}`),
		),
		answer("Done."),
	)
	a := newAgent(t, fw.WithConfig(cfg), fw.WithModel(model), fw.WithTools(filesTool()))
	r, err := a.Run(ctx, "Tidy the notes")
	if err != nil || r.State != fw.StateCompleted {
		t.Fatalf("run = %+v, %v", r, err)
	}
	res := mustResult(t, ctx, a, r.ID)
	var files []string
	for _, f := range res.Files {
		s := f.Op + " " + f.Path
		if f.OldPath != "" {
			s += " from " + f.OldPath
		}
		files = append(files, s)
		if f.Seq == 0 || (f.Op != fw.FileRemoved && f.SHA256 == "") {
			t.Errorf("file = %+v", f)
		}
	}
	want := "created notes/a.md|created moved.txt|renamed post.txt from pre.txt|edited edit.txt|removed gone.txt"
	if got := strings.Join(files, "|"); got != want {
		t.Errorf("files = %s\nwant    %s", got, want)
	}
	if res.Files[0].Size != int64(len("# A\nmore")) {
		t.Errorf("size of notes/a.md = %d", res.Files[0].Size)
	}
	var steps []string
	for _, s := range res.Steps {
		if !s.Done {
			t.Errorf("step without a result: %+v", s)
		}
		steps = append(steps, s.Text)
	}
	wantSteps := "Wrote notes/a.md|Edited notes/a.md|Wrote tmp.txt|Read and change files|Read and change files|Edited edit.txt|Removed gone.txt|Read missing.txt"
	if got := strings.Join(steps, "|"); got != wantSteps {
		t.Errorf("steps = %s\nwant    %s", got, wantSteps)
	}
	if last := res.Steps[len(res.Steps)-1]; !last.IsError {
		t.Errorf("the failed view is not an error: %+v", last)
	}
	if got := kinds(res.Next); got != "review=Review changes reply=Reply save-agent=Save as agent" {
		t.Errorf("next = %s", got)
	}
	text := res.String()
	for _, want := range []string{"Files that the run changed:\n- Created notes/a.md (8 bytes)\n- Created moved.txt (3 bytes)\n- Renamed pre.txt to post.txt\n- Edited edit.txt (3 bytes)\n- Removed gone.txt", "- Read missing.txt (failed)"} {
		if !strings.Contains(text, want) {
			t.Errorf("text lacks %q:\n%s", want, text)
		}
	}
}

// The net change of a path: a later change replaces an earlier one, a
// rename moves the entry, and an attachment of the person is no change of
// the run.
func TestNetFiles(t *testing.T) {
	type change struct {
		op, path, old string
		tool          string
	}
	for _, tc := range []struct {
		name    string
		changes []change
		want    string
	}{
		{"edit after create", []change{{fw.FileCreated, "a", "", "files"}, {fw.FileChanged, "a", "", "files"}}, "created a"},
		{"edit", []change{{fw.FileChanged, "a", "", "files"}, {fw.FileChanged, "a", "", "files"}}, "edited a"},
		{"remove after edit", []change{{fw.FileChanged, "a", "", "files"}, {fw.FileRemoved, "a", "", "files"}}, "removed a"},
		{"rename keeps the place", []change{{fw.FileChanged, "a", "", "files"}, {fw.FileCreated, "b", "", "files"}, {fw.FileRenamed, "c", "a", "files"}}, "renamed c from a|created b"},
		{"rename twice", []change{{fw.FileRenamed, "b", "a", "files"}, {fw.FileRenamed, "c", "b", "files"}}, "renamed c from a"},
		{"rename back", []change{{fw.FileRenamed, "b", "a", "files"}, {fw.FileRenamed, "a", "b", "files"}}, "edited a"},
		{"rename onto a removed path", []change{{fw.FileRemoved, "b", "", "files"}, {fw.FileChanged, "a", "", "files"}, {fw.FileRenamed, "b", "a", "files"}}, "renamed b from a"},
		{"edit after rename", []change{{fw.FileRenamed, "b", "a", "files"}, {fw.FileChanged, "b", "", "files"}}, "renamed b from a"},
		{"attachment", []change{{fw.FileCreated, "attachments/x.png", "", ""}, {fw.FileCreated, "y", "", "bash"}}, "created y"},
	} {
		var n netFiles
		for i, c := range tc.changes {
			n.add(uint64(i+1), fw.FileChangedData{Op: c.op, Path: c.path, OldPath: c.old, Tool: c.tool})
		}
		var got []string
		for _, f := range n.result() {
			s := f.Op + " " + f.Path
			if f.OldPath != "" {
				s += " from " + f.OldPath
			}
			got = append(got, s)
		}
		if s := strings.Join(got, "|"); s != tc.want {
			t.Errorf("%s: files = %s, want %s", tc.name, s, tc.want)
		}
	}
}

// loopModel calls echo with the same input forever.
func loopModel() fw.Model {
	var n atomic.Int32
	return fw.ModelFunc{GenerateFn: func(context.Context, *fw.Request) (*fw.Response, error) {
		id := "c" + string(rune('a'+n.Add(1)))
		return calls("", use(id, "echo", `{"text":"again"}`)), nil
	}}
}

// A run that stops at a limit of its budget.
func TestResultBudgetStop(t *testing.T) {
	ctx := context.Background()
	a := newAgent(t, fw.WithModel(loopModel()), fw.WithTools(okTool(fw.ToolSpec{Name: "echo"})))
	r, err := a.Run(ctx, "Loop", fw.WithRunBudget(fw.Budget{MaxTurns: 1}))
	if err != nil || r.State != fw.StateFailed {
		t.Fatalf("run = %+v, %v", r, err)
	}
	res := mustResult(t, ctx, a, r.ID)
	if res.Status != StatusFailed || res.Error != "budget exceeded: maxTurns" || !reflect.DeepEqual(res.Limits, []string{"Stopped at the limit of 1 turn."}) {
		t.Fatalf("result = %+v", res)
	}
	if got := kinds(res.Next); got != "reply=Reply retry=Try again" {
		t.Errorf("next = %s", got)
	}
	if len(res.Steps) != 1 || res.Steps[0].Text != "echo" || !res.Steps[0].Done {
		t.Errorf("steps = %+v", res.Steps)
	}
	if !strings.Contains(res.String(), "Error: budget exceeded: maxTurns\n\nSteps:\n- echo\n\nLimits:\n- Stopped at the limit of 1 turn.\n\nNext: Reply, Try again.") {
		t.Errorf("text =\n%s", res.String())
	}

	// The loop guard warns once and then stops the run. The stop is one
	// limit, not two.
	r, err = a.Run(ctx, "Loop again", fw.WithRunBudget(fw.Budget{MaxRepeats: 3}))
	if err != nil || r.State != fw.StateFailed {
		t.Fatalf("run = %+v, %v", r, err)
	}
	res = mustResult(t, ctx, a, r.ID)
	if len(res.Limits) != 2 || !strings.HasPrefix(res.Limits[0], "The agent made the same echo call ") || !strings.HasSuffix(res.Limits[0], "and fw told it so.") ||
		res.Limits[1] != "Stopped, because the agent made the same echo call 3 times in a row." {
		t.Errorf("limits = %q", res.Limits)
	}
}

func TestLimitText(t *testing.T) {
	for _, tc := range []struct {
		limit string
		value float64
		want  string
	}{
		{"maxTurns", 20, "Stopped at the limit of 20 turns."},
		{"maxTokens", 120000, "Stopped at the token limit, after 120000 tokens."},
		{"maxCostUSD", 2.5, "Stopped at the cost limit, after $2.50."},
		{"maxToolCalls", 1, "Stopped at the limit of 1 tool call."},
		{"deadline", 1805, "Stopped at the time limit, after 30 minutes of work."},
		{"deadline", 42, "Stopped at the time limit, after 42 seconds of work."},
		{"maxRepeats", 4, "Stopped, because the agent made the same call 4 times in a row."},
		{"maxWidgets", 3, "Stopped at the limit maxWidgets, at 3."},
	} {
		if got := limitText(fw.BudgetExceededData{Limit: tc.limit, Value: tc.value}); got != tc.want {
			t.Errorf("limitText(%s, %g) = %q, want %q", tc.limit, tc.value, got, tc.want)
		}
	}
}

func TestStepTextAndEffect(t *testing.T) {
	long := strings.Repeat("é", 100)
	for _, tc := range []struct {
		tool, input string
		spec        fw.ToolSpec
		step        string
		effect      string
	}{
		{"bash", `{"command":"go test ./..."}`, fw.ToolSpec{}, `Ran "go test ./..."`, `Runs "go test ./...".`},
		{"bash", `{"command":"\n  cd x\ngo build"}`, fw.ToolSpec{}, `Ran "cd x"`, "Runs \"cd x\ngo build\"."},
		{"bash", `{"command":"echo ` + long + `"}`, fw.ToolSpec{}, `Ran "echo ` + strings.Repeat("é", 74) + `…"`, `Runs "echo ` + long + `".`},
		{"bash", `{"restart":true}`, fw.ToolSpec{Title: "Run shell commands"}, "Run shell commands", ""},
		{"files", `{"command":"view","path":"docs/a.md"}`, fw.ToolSpec{}, "Read docs/a.md", ""},
		{"files", `{"command":"create","path":"a.go","file_text":"x"}`, fw.ToolSpec{}, "Wrote a.go", "Changes a.go."},
		{"files", `{"command":"str_replace","path":"a.go"}`, fw.ToolSpec{}, "Edited a.go", "Changes a.go."},
		{"files", `{"command":"insert","path":"a.go"}`, fw.ToolSpec{}, "Edited a.go", "Changes a.go."},
		{"files", `{"command":"delete","path":"a.go"}`, fw.ToolSpec{}, "Removed a.go", "Removes a.go."},
		{"files", `{"command":"chmod","path":"a.go"}`, fw.ToolSpec{Title: "Read and change files"}, "Read and change files", ""},
		{"http", `{"url":"https://example.com/a"}`, fw.ToolSpec{}, "Fetched https://example.com/a", ""},
		{"deploy", `{"env":"prod"}`, fw.ToolSpec{Title: "Deploy the app"}, "Deploy the app", ""},
		{"deploy", `{"env":"prod"}`, fw.ToolSpec{}, "deploy", ""},
		{"bash", `"{\"command\":\"go te`, fw.ToolSpec{}, "bash", ""},
	} {
		if got := stepText(tc.tool, json.RawMessage(tc.input), tc.spec); got != tc.step {
			t.Errorf("stepText(%s, %s) = %q, want %q", tc.tool, tc.input, got, tc.step)
		}
		if got := effectOf(tc.tool, json.RawMessage(tc.input)); got != tc.effect {
			t.Errorf("effectOf(%s, %s) = %q, want %q", tc.tool, tc.input, got, tc.effect)
		}
	}
}

// A builder run: its title, its result, and the profile of its proposal.
func TestBuilderRun(t *testing.T) {
	ctx := context.Background()
	proposal := `{"name":"pr-reviewer","description":"Reviews pull requests.","instructions":"You review.","tools":["bash"],"never_runs":["git push*"],"minutes":20}`
	propose := okTool(fw.ToolSpec{Name: "propose_agent", ReadOnly: true, OptIn: true})
	ask := okTool(fw.ToolSpec{Name: "ask_user", ReadOnly: true})
	a := newAgent(t, fw.WithModel(scripted(calls("", use("p1", "propose_agent", proposal)), answer("Tap Create agent."))), fw.WithTools(propose, ask))
	req := BuilderRequest("Review my pull requests")
	r, err := a.Run(ctx, req.Input, fw.WithRunProfile(*req.Profile), fw.WithMetadata(req.Metadata))
	if err != nil || r.State != fw.StateCompleted {
		t.Fatalf("run = %+v, %v", r, err)
	}
	if r.Title != "New agent" || TitleOf(r) != "New agent" {
		t.Errorf("title = %q, TitleOf = %q", r.Title, TitleOf(r))
	}
	res := mustResult(t, ctx, a, r.ID)
	if res.Title != "New agent" || res.Status != StatusReady || res.Summary != "Tap Create agent." {
		t.Errorf("result = %+v", res)
	}
	if got := kinds(res.Next); got != "reply=Reply" {
		t.Errorf("a builder run offers %s", got)
	}

	events, err := readAll(ctx, a, r.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	var tool *Item
	items := Transcript(events)
	for i := range items {
		if items[i].Kind == ItemTool {
			tool = &items[i]
		}
	}
	want := fw.AgentProfile{Name: "pr-reviewer", Description: "Reviews pull requests.", System: "You review.", Tools: []string{"bash"},
		Policy: &fw.Rules{Deny: []string{"bash: git push*"}}, Budget: &fw.Budget{Deadline: 20 * 60e9}}
	if tool == nil || tool.Profile == nil || !reflect.DeepEqual(*tool.Profile, want) {
		t.Fatalf("tool item = %+v", tool)
	}
	b, _ := json.Marshal(tool)
	if !strings.Contains(string(b), `"profile":{"name":"pr-reviewer"`) {
		t.Errorf("item JSON = %s", b)
	}
}

// The profile of a proposal stays nil when the input does not convert.
func TestTranscriptProposalWithoutProfile(t *testing.T) {
	long := `{"name":"x","instructions":"` + strings.Repeat("a", MaxItemText) + `","tools":[]}`
	items := Transcript([]fw.Event{
		fw.MustEvent("r1", fw.EvModelResponded, fw.ModelRespondedData{Content: []fw.Block{
			use("p1", "propose_agent", `{"description":"no name"}`),
			use("p2", "propose_agent", long),
			use("p3", "other", `{"name":"x","tools":[]}`),
		}}),
	})
	if len(items) != 3 {
		t.Fatalf("items = %+v", items)
	}
	for _, it := range items {
		if it.Profile != nil {
			t.Errorf("item %s has a profile: %+v", it.CallID, it.Profile)
		}
	}
	if !items[1].Cut {
		t.Error("the long proposal is not cut")
	}
}

func TestSaveAsAgentInput(t *testing.T) {
	ctx := context.Background()
	model := scripted(
		calls("", use("c1", "bash", `{"command":"gh pr diff 42"}`), use("c2", "files", `{"command":"view","path":"a.go"}`)),
		answer("Looks good."),
		calls("", use("c3", "bash", `{"command":"go test ./..."}`)),
		answer("  Tests pass.  "),
	)
	a := newAgent(t, fw.WithModel(model), fw.WithTools(okTool(fw.ToolSpec{Name: "bash"}), okTool(fw.ToolSpec{Name: "files"})),
		fw.WithProfile(fw.AgentProfile{Name: "reviewer"}))
	r, err := a.Run(ctx, "  Review PR 42  ", fw.AsAgent("reviewer"))
	if err != nil || r.State != fw.StateCompleted {
		t.Fatalf("run = %+v, %v", r, err)
	}
	if err := a.Send(ctx, r.ID, fw.Message{Role: fw.RoleUser, Content: []fw.Block{fw.Text("Also check the tests")}}, fw.OriginHTTP); err != nil {
		t.Fatal(err)
	}
	if r, err = a.Wait(ctx, r.ID); err != nil || r.State != fw.StateCompleted {
		t.Fatalf("run = %+v, %v", r, err)
	}
	got, err := SaveAsAgentInput(ctx, a, r.ID)
	want := "Make an agent from this conversation, so I can hand off this kind of work again.\n\n" +
		"What I asked:\n- Review PR 42\n- Also check the tests\n\n" +
		"What reviewer answered last:\nTests pass.\n\n" +
		"The tools that it used: bash, files."
	if err != nil || got != want {
		t.Fatalf("input = %q, %v\nwant    %q", got, err, want)
	}
	if _, err := SaveAsAgentInput(ctx, a, "nope"); !errors.Is(err, fw.ErrRunNotFound) {
		t.Errorf("an unknown run = %v", err)
	}
}

// The bounds of the first message of "Save as agent": eight messages of
// the person, 2000 characters of the answer, and no message of the kernel.
func TestSaveAsAgentBounds(t *testing.T) {
	events := []fw.Event{fw.MustEvent("r1", fw.EvRunQueued, fw.RunQueuedData{Agent: "a", Input: "m0"})}
	for i := 1; i < 10; i++ {
		events = append(events, fw.MustEvent("r1", fw.EvMessageReceived, fw.MessageReceivedData{Role: fw.RoleUser, Content: []fw.Block{fw.Text("m" + string(rune('0'+i)))}}))
		if i == 2 {
			events = append(events,
				fw.MustEvent("r1", fw.EvMessageReceived, fw.MessageReceivedData{Role: fw.RoleUser, Content: []fw.Block{fw.Text("Continue.")}, Origin: fw.OriginKernel}),
				fw.MustEvent("r1", fw.EvMessageReceived, fw.MessageReceivedData{Role: fw.RoleSystem, Content: []fw.Block{fw.Text("It is 9:00.")}, Note: fw.NoteTime}),
				fw.MustEvent("r1", fw.EvMessageReceived, fw.MessageReceivedData{Role: fw.RoleUser, Content: []fw.Block{{Type: fw.BlockFile, File: &fw.FileRef{Path: "attachments/a.png"}}}}),
			)
		}
	}
	events = append(events, fw.MustEvent("r1", fw.EvModelResponded, fw.ModelRespondedData{Content: []fw.Block{fw.Text(strings.Repeat("é", 2500))}}))
	got := saveAsAgentText("a", events)
	want := "What I asked:\n- m0\n- m1\n- m2\n- m3\n- m4\n- m5\n- m6\n- m7\n\nWhat a answered last:\n" + strings.Repeat("é", 2000) + "\n\nThe tools that it used: none."
	if !strings.HasSuffix(got, want) {
		t.Errorf("input = %q", got)
	}
}

// Save as agent is for the person who started a completed top-level run
// and who can create agents.
func TestResultSaveAgentForTheStarter(t *testing.T) {
	ctx := context.Background()
	a := newAgent(t, fw.WithProfile(fw.AgentProfile{Name: "reviewer", Access: &fw.Access{Owners: []string{"alice"}, Viewers: []string{"bob"}}}))
	alice := fw.WithPrincipal(ctx, &fw.Principal{Subject: "alice", Kind: fw.KindUser})
	bob := fw.WithPrincipal(ctx, &fw.Principal{Subject: "bob", Kind: fw.KindUser})
	carol := fw.WithPrincipal(ctx, &fw.Principal{Subject: "carol", Kind: fw.KindUser})
	r, err := a.Run(alice, "Review PR 42", fw.AsAgent("reviewer"))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		ctx  context.Context
		want string
	}{
		{"alice", alice, "reply=Reply save-agent=Save as agent"},
		{"bob", bob, "reply=Reply"},
		{"the process", ctx, "reply=Reply save-agent=Save as agent"},
	} {
		if got := kinds(mustResult(t, tc.ctx, a, r.ID).Next); got != tc.want {
			t.Errorf("%s: next = %s, want %s", tc.name, got, tc.want)
		}
	}
	if _, err := ResultOf(carol, a, r.ID); !errors.Is(err, fw.ErrForbidden) {
		t.Errorf("carol reads the result: %v", err)
	}
	if _, err := SaveAsAgentInput(carol, a, r.ID); !errors.Is(err, fw.ErrForbidden) {
		t.Error("carol reads the conversation")
	}
}

func TestNearLimits(t *testing.T) {
	u := fw.RunUsage{Usage: fw.Usage{InputTokens: 700, OutputTokens: 100}, Turns: 18, ToolCalls: 2, CostUSD: 0.5}
	got := nearLimits(u, fw.Budget{MaxTurns: 20, MaxTokens: 1000, MaxToolCalls: 40, MaxCostUSD: 4})
	want := []string{"Used 18 of 20 turns.", "Used 800 of 1000 tokens."}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("near limits = %q, want %q", got, want)
	}
	if got := nearLimits(u, fw.Budget{}); len(got) != 0 {
		t.Fatalf("no budget: %q", got)
	}
}
