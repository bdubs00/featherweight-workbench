package notify

import (
	"context"
	"strings"
	"testing"
)

// The tools call the functions of the routes: a read, a write with the
// token kept out of every answer, and a test.
func TestTools(t *testing.T) {
	n := newNtfy(t)
	f := newFront(t, "notifications:\n  ntfy:\n    server: "+n.URL+"\n", true)
	cs := connectMCP(t, f.a, "alice")
	res := call(t, cs, ToolGet, map[string]any{})
	if res.IsError || !strings.HasPrefix(textOf(res), "Notifications are off: No topic is set.") {
		t.Fatalf("a first read = %s", textOf(res))
	}
	res = call(t, cs, ToolPut, map[string]any{"ntfy": map[string]any{"topic": "alice-fw", "token": "tk_9"}, "quietHours": map[string]any{"from": "22:00", "to": "07:00"}, "timeZone": "America/New_York"})
	var ans Answer
	structured(t, res, &ans)
	if res.IsError || strings.Contains(textOf(res), "tk_9") || ans.Ntfy.Token != TokenSet || ans.QuietHours.TimeZone != "America/New_York" ||
		!strings.Contains(textOf(res), "Quiet hours: 22:00 to 07:00, America/New_York.") {
		t.Fatalf("a write = %s", textOf(res))
	}
	if res := call(t, cs, ToolPut, map[string]any{"ntfy": map[string]any{"topic": "a b"}}); !res.IsError || !strings.Contains(textOf(res), "- ntfy.topic: ") {
		t.Fatalf("a bad write = %s", textOf(res))
	}
	if res := call(t, cs, ToolTest, map[string]any{}); res.IsError || textOf(res) != "The test notification went out." {
		t.Fatalf("a test = %s", textOf(res))
	}
	if got, tokens := n.published(); len(got) != 1 || tokens[0] != "Bearer tk_9" {
		t.Fatalf("the publish = %+v", got)
	}
}

// Each route has its tool, and each tool of the notifications has its
// route.
func TestParity(t *testing.T) {
	f := newFront(t, "", false)
	cs := connectMCP(t, f.a, "alice")
	listed, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, tool := range listed.Tools {
		if strings.HasPrefix(tool.Name, "notification") {
			names[tool.Name] = true
		}
	}
	if len(names) != 3 {
		t.Fatalf("the server has %d tools of the notifications: %v", len(names), names)
	}
	seen := map[string]bool{}
	for _, r := range table {
		if !names[r.tool] {
			t.Errorf("the route %s names the tool %s, which the server does not have", r.pattern, r.tool)
		}
		seen[r.tool] = true
	}
	for name := range names {
		if !seen[name] {
			t.Errorf("the tool %s has no route", name)
		}
	}
}
