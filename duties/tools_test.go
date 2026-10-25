package duties

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/surface/mcp"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// duty_preview is the dry run of duty_put as a tool that only reads, so a
// policy that allows read-only tools lets it run without an approval. A
// duty sent as a JSON text reads as the object, in both tools.
func TestPreview(t *testing.T) {
	f := newFront(t, "", nil)
	cs := connectMCP(t, f.a, "alice")
	listed, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range listed.Tools {
		switch tool.Name {
		case ToolPreview:
			if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint {
				t.Errorf("duty_preview is not read-only: %+v", tool.Annotations)
			}
		case ToolPut:
			if tool.Annotations == nil || tool.Annotations.ReadOnlyHint {
				t.Errorf("duty_put reads only: %+v", tool.Annotations)
			}
		}
	}
	duty := `{"agent": "triage", "when": {"say": "every weekday at 9am"}, "do": {"ask": {"message": "Write the report of {date}."}}, "limits": {"budget": {"maxCostUSD": 0.5}}}`
	var object map[string]any
	if err := json.Unmarshal([]byte(duty), &object); err != nil {
		t.Fatal(err)
	}
	for _, body := range []any{object, duty} {
		res := call(t, cs, ToolPreview, map[string]any{"name": "report", "duty": body, "timeZone": "America/New_York"})
		var dry putResult
		structured(t, res, &dry)
		if res.IsError || !strings.Contains(textOf(res), "nothing is saved") || dry.DryRun == nil || len(dry.DryRun.Next) != 5 {
			t.Fatalf("a preview of %T = %s", body, textOf(res))
		}
	}
	if res := call(t, cs, ToolList, map[string]any{}); !strings.Contains(textOf(res), "You can view no duty.") {
		t.Fatalf("a preview saved: %s", textOf(res))
	}
	if res := call(t, cs, ToolPut, map[string]any{"name": "report", "duty": duty, "timeZone": "America/New_York"}); res.IsError || !strings.HasPrefix(textOf(res), "Created the duty report") {
		t.Fatalf("a write of a JSON text = %s", textOf(res))
	}
	if res := call(t, cs, ToolPreview, map[string]any{"name": "report", "duty": "not a duty"}); !res.IsError || !strings.Contains(textOf(res), "the duty does not read") {
		t.Fatalf("a preview of a text that holds no object = %s", textOf(res))
	}
}

// The tools call the functions of the routes: a dry run with words, a
// write, a read, a run now, a pause, the switch for an admin, and a
// delete, each with the access of its route.
func TestTools(t *testing.T) {
	f := newFront(t, "", nil)
	cs := connectMCP(t, f.a, "alice")
	duty := map[string]any{
		"agent":  "triage",
		"when":   map[string]any{"say": "every weekday at 9am"},
		"do":     map[string]any{"ask": map[string]any{"message": "Write the report of {date}."}},
		"limits": map[string]any{"budget": map[string]any{"maxCostUSD": 0.5}},
	}
	res := call(t, cs, ToolPut, map[string]any{"name": "report", "duty": duty, "dryRun": true, "timeZone": "America/New_York"})
	text := textOf(res)
	if res.IsError || !strings.Contains(text, "nothing is saved") || !strings.Contains(text, "Readback: Every weekday at 9:00 AM, New York time.") || !strings.Contains(text, "Next: Mon Oct 26, 9:00 AM New York time") {
		t.Fatalf("a dry run = %s", text)
	}
	var dry putResult
	structured(t, res, &dry)
	if dry.DryRun == nil || len(dry.DryRun.Problems) != 0 || len(dry.DryRun.Next) != 5 {
		t.Fatalf("the structured dry run = %+v", dry)
	}
	if res := call(t, cs, ToolList, map[string]any{}); !strings.Contains(textOf(res), "You can view no duty.") {
		t.Fatalf("a dry run saved: %s", textOf(res))
	}
	res = call(t, cs, ToolPut, map[string]any{"name": "report", "duty": duty, "timeZone": "America/New_York"})
	if res.IsError || !strings.HasPrefix(textOf(res), "Created the duty report (triage): Every weekday at 9:00 AM, New York time.") {
		t.Fatalf("a write = %s", textOf(res))
	}
	bad := map[string]any{"agent": "triage", "when": map[string]any{"say": "every weekday at 9am"}, "do": map[string]any{"ask": map[string]any{"message": "x"}}}
	if res := call(t, cs, ToolPut, map[string]any{"name": "nobudget", "duty": bad}); !res.IsError || !strings.Contains(textOf(res), "limits.budget: A budget is required") {
		t.Fatalf("a write without a budget = %s", textOf(res))
	}
	res = call(t, cs, ToolGet, map[string]any{"name": "report"})
	var v View
	structured(t, res, &v)
	if res.IsError || v.Owner != "alice" || v.When.Schedule.TimeZone != "America/New_York" {
		t.Fatalf("a read = %s", textOf(res))
	}
	res = call(t, cs, ToolRunNow, map[string]any{"name": "report"})
	if res.IsError || !strings.Contains(textOf(res), "started the run") {
		t.Fatalf("run now = %s", textOf(res))
	}
	if res := call(t, cs, ToolGet, map[string]any{"name": "report"}); !strings.Contains(textOf(res), "The last starts, newest first:") {
		t.Fatalf("a read with history = %s", textOf(res))
	}
	if res := call(t, cs, ToolPause, map[string]any{"name": "report", "paused": true}); res.IsError || !strings.HasPrefix(textOf(res), "Paused the duty report") {
		t.Fatalf("a pause = %s", textOf(res))
	}
	if res := call(t, cs, ToolPauseAll, map[string]any{"paused": true}); !res.IsError || !strings.Contains(textOf(res), "admin role") {
		t.Fatalf("the switch for an owner = %s", textOf(res))
	}
	root := connectMCP(t, f.a, "root")
	if res := call(t, root, ToolPauseAll, map[string]any{"paused": true, "reason": "storm"}); res.IsError || textOf(res) != "Every duty is paused. Runs that work go on." {
		t.Fatalf("the switch for an admin = %s", textOf(res))
	}
	if res := call(t, cs, ToolList, map[string]any{}); !strings.Contains(textOf(res), "The switch pauses every duty: storm.") {
		t.Fatalf("the list under the switch = %s", textOf(res))
	}
	carol := connectMCP(t, f.a, "carol")
	if res := call(t, carol, ToolDelete, map[string]any{"name": "report"}); !res.IsError || !strings.Contains(textOf(res), "Forbidden") {
		t.Fatalf("a delete by a viewer = %s", textOf(res))
	}
	if res := call(t, carol, ToolGet, map[string]any{"name": "nothing"}); !res.IsError || !strings.Contains(textOf(res), "Not found") {
		t.Fatalf("a read of no duty = %s", textOf(res))
	}
	if res := call(t, cs, ToolDelete, map[string]any{"name": "report"}); res.IsError || !strings.HasPrefix(textOf(res), "Deleted the duty report.") {
		t.Fatalf("a delete = %s", textOf(res))
	}
}

// connectMCP serves the MCP surface of the agent and connects a client
// with the token.
func connectMCP(t *testing.T, a *fw.Agent, token string) *sdk.ClientSession {
	t.Helper()
	h, err := mcp.Handler(a)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	hc := &http.Client{Transport: bearer{token: token}}
	client := sdk.NewClient(&sdk.Implementation{Name: "test-client", Version: "v1"}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	cs, err := client.Connect(ctx, &sdk.StreamableClientTransport{Endpoint: srv.URL + mcp.DefaultPath, HTTPClient: hc, MaxRetries: -1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

type bearer struct{ token string }

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return http.DefaultTransport.RoundTrip(r)
}

// call calls a tool.
func call(t *testing.T, cs *sdk.ClientSession, name string, args any) *sdk.CallToolResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := cs.CallTool(ctx, &sdk.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("call %s: %v", name, err)
	}
	return res
}

func textOf(res *sdk.CallToolResult) string {
	var parts []string
	for _, c := range res.Content {
		if tc, ok := c.(*sdk.TextContent); ok {
			parts = append(parts, tc.Text)
		}
	}
	return strings.Join(parts, "\n")
}

func structured(t *testing.T, res *sdk.CallToolResult, v any) {
	t.Helper()
	b, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatalf("structuredContent %s: %v", b, err)
	}
}
