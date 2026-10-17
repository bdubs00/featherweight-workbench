package builder

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/bdubs00/featherweight-kernel/fw"
)

func TestProposeAgent(t *testing.T) {
	cfg := fw.DefaultConfig()
	cfg.Tools.Builtin = []string{"bash", "files", "await_task", "propose_agent"}
	tool, err := fw.NewBuiltinTool("propose_agent", cfg)
	if err != nil {
		t.Fatal(err)
	}
	spec := tool.Spec()
	if !spec.OptIn || !spec.ReadOnly || spec.Title != "Propose new agents" {
		t.Fatalf("spec = %+v", spec)
	}
	// The description names the tools of the configuration, without itself
	// and await_task.
	if !strings.Contains(spec.Description, "- bash: Run shell commands") || strings.Contains(spec.Description, "- propose_agent") || strings.Contains(spec.Description, "- await_task") {
		t.Fatalf("description = %s", spec.Description)
	}

	fake := func(name, title string) fw.Tool {
		return fw.NewTool(fw.ToolSpec{Name: name, Title: title}, func(context.Context, json.RawMessage) (fw.ToolResult, error) { return fw.TextResult("ok"), nil })
	}
	a, err := fw.New(fw.WithoutDefaults(), fw.WithLedger(fw.NewMemLedger()), fw.WithBus(fw.NewMemBus()), fw.WithAuthn(fw.NoAuthn{}),
		fw.WithModel(fw.ModelFunc{GenerateFn: func(context.Context, *fw.Request) (*fw.Response, error) { return &fw.Response{}, nil }}),
		fw.WithTools(fake("ask_user", "Ask you questions"), fake("bash", "Run shell commands"), tool))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close(context.Background()) })
	ctx := fw.WithAgent(context.Background(), a)
	good := map[string]any{
		"name": "pr-reviewer", "description": "Reviews pull requests.", "instructions": "You review pull requests.",
		"tools": []string{"ask_user"}, "asks_first": []string{"ask_user"}, "minutes": 30,
	}
	in, _ := json.Marshal(good)
	if got := mustOK(t, call(t, ctx, tool, string(in))); !strings.Contains(got, "Create agent") {
		t.Errorf("result = %q", got)
	}
	for want, change := range map[string]map[string]any{
		`no tool is named "github"`:  {"tools": []string{"github"}},
		"tools is empty":             {"tools": []string{}},
		`asks_first names "bash"`:    {"asks_first": []string{"bash"}},
		"never_runs needs bash":      {"never_runs": []string{"git push*"}},
		`the name "PR Reviewer"`:     {"name": "PR Reviewer"},
		"the instructions are empty": {"instructions": " "},
		"The tools that exist:\n- ask_user: Ask you questions\n- bash: Run shell commands": {"tools": []string{"propose_agent"}},
	} {
		p := map[string]any{}
		for k, v := range good {
			p[k] = v
		}
		for k, v := range change {
			p[k] = v
		}
		in, _ := json.Marshal(p)
		mustFail(t, call(t, ctx, tool, string(in)), want)
	}
}

// mustOK fails unless the result is not an error, and returns its text.
func mustOK(t *testing.T, res fw.ToolResult) string {
	t.Helper()
	if res.IsError {
		t.Fatalf("unexpected error result: %s", text(res))
	}
	return text(res)
}

// mustFail fails unless the result is an error that contains want.
func mustFail(t *testing.T, res fw.ToolResult, want string) {
	t.Helper()
	if !res.IsError {
		t.Fatalf("want an error result, got: %s", text(res))
	}
	if !strings.Contains(text(res), want) {
		t.Fatalf("error result %q does not contain %q", text(res), want)
	}
}

// call runs a tool with a JSON input and fails the test on a Go error.
func call(t *testing.T, ctx context.Context, tool fw.Tool, input string) fw.ToolResult {
	t.Helper()
	res, err := tool.Call(ctx, json.RawMessage(input))
	if err != nil {
		t.Fatalf("%s %s: unexpected Go error: %v", tool.Spec().Name, input, err)
	}
	return res
}

// text is the text of a tool result.
func text(res fw.ToolResult) string { return fw.TextOf(res.Content) }
