package pages

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/bdubs00/featherweight-workbench/surface/mcp"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Only the front writes agents/<agent>/learned.md and the pages below
// agents/<agent>/knowhow/.
func TestReserved(t *testing.T) {
	for p, want := range map[string]bool{
		"agents/triage/learned.md":          true,
		"agents/triage/knowhow/a.md":        true,
		"agents/triage/knowhow/deep/b.md":   true,
		"agents/triage/notes.md":            false,
		"agents/triage/notes/learned.md":    false,
		"agents/triage/knowhow.md":          false,
		"agents/learned.md":                 false,
		"notes/agents/triage/learned.md":    false,
		"learned.md":                        false,
		"agents/triage/routines/thing.md":   false,
		"agents/triage/sub/knowhow/note.md": false,
	} {
		if got := Reserved(p); got != want {
			t.Errorf("Reserved(%q) = %v, want %v", p, got, want)
		}
	}
}

// A run writes only below agents/<its agent>/ and outside agents/, never a
// page that the front keeps, and each refusal names the path to use. A
// person writes the folder of any agent, but not a page that the front
// keeps.
func TestAgentWriteRules(t *testing.T) {
	a := newAgent(t, nil)
	fleet := connectMCP(t, a, "fleet")
	alice := connectMCP(t, a, "alice")
	write := func(cs *sdk.ClientSession, path string, meta sdk.Meta) *sdk.CallToolResult {
		t.Helper()
		return call(t, cs, ToolWrite, map[string]any{"path": path, "text": "# " + path + "\n"}, meta)
	}
	for _, p := range []string{"notes/plan.md", "agents/migrator/plan.md", "agents/migrator/deep/a.md"} {
		if res := write(fleet, p, runMeta); res.IsError {
			t.Fatalf("the run of migrator wrote %s: %s", p, textOf(res))
		}
	}
	for p, use := range map[string]string{
		"agents/reviewer/plan.md":           "agents/migrator/plan.md",
		"agents/migrator/learned.md":        "agents/migrator/notes/learned.md",
		"agents/reviewer/learned.md":        "agents/migrator/notes/learned.md",
		"agents/migrator/knowhow/gotcha.md": "agents/migrator/notes/gotcha.md",
	} {
		res := write(fleet, p, runMeta)
		if !res.IsError || !strings.Contains(textOf(res), "Refused") || !strings.Contains(textOf(res), use) {
			t.Errorf("the run of migrator wrote %s: %s", p, textOf(res))
		}
		if _, err := Of(a).Read(context.Background(), p, 0); err == nil {
			t.Errorf("the refused page %s is in the wiki", p)
		}
	}
	// A run whose call names no agent writes no page below agents/.
	if res := write(fleet, "agents/migrator/x.md", sdk.Meta{mcp.MetaRun: "r-8"}); !res.IsError || !strings.Contains(textOf(res), "migrator/x.md") {
		t.Errorf("a run without an agent wrote below agents/: %s", textOf(res))
	}
	// A person writes the folder of an agent, and not a page that the
	// front keeps.
	if res := write(alice, "agents/reviewer/plan.md", nil); res.IsError {
		t.Fatalf("alice wrote agents/reviewer/plan.md: %s", textOf(res))
	}
	if res := write(alice, "agents/reviewer/learned.md", nil); !res.IsError || !strings.Contains(textOf(res), "agents/reviewer/notes/learned.md") {
		t.Errorf("alice wrote agents/reviewer/learned.md: %s", textOf(res))
	}
	// The front writes it through Go.
	if _, err := Of(a).Write(context.Background(), "agents/reviewer/learned.md", "# Hearts\n", ByFront, "", IfAny); err != nil {
		t.Fatalf("the front did not write learned.md: %v", err)
	}
}

// The routes refuse a write and a removal of a page that the front keeps
// with 403 and the path to use.
func TestRoutesRefuseReserved(t *testing.T) {
	a := newAgent(t, nil)
	base := serve(t, a) + "/v1/pages"
	resp, out := do(t, "PUT", base+"/agents/reviewer/knowhow/a.md", "alice", jsonText("# A\n"))
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(out, "agents/reviewer/notes/a.md") {
		t.Fatalf("PUT of a page of the front = %d %s", resp.StatusCode, out)
	}
	if _, err := Of(a).Write(context.Background(), "agents/reviewer/learned.md", "# Hearts\n", ByFront, "", IfAny); err != nil {
		t.Fatal(err)
	}
	resp, out = do(t, "DELETE", base+"/agents/reviewer/learned.md", "alice", "")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("DELETE of a page of the front = %d %s", resp.StatusCode, out)
	}
	if _, err := Of(a).Read(context.Background(), "agents/reviewer/learned.md", 0); err != nil {
		t.Fatalf("the page of the front is gone: %v", err)
	}
}

// OnWrite gets each page that a write created or changed with its hash,
// and each removed page with an empty hash. A write of the same text, a
// refused write, and the index give nothing.
func TestOnWrite(t *testing.T) {
	type got struct{ path, hash string }
	var mu sync.Mutex
	var calls []got
	OnWrite = func(_ context.Context, path, hash string) {
		mu.Lock()
		calls = append(calls, got{path, hash})
		mu.Unlock()
	}
	t.Cleanup(func() { OnWrite = nil })
	s := memStore(Config{})
	ctx := context.Background()
	first := mustWrite(t, s, "notes/plan.md", "# Plan\n\nOne.\n", IfAny)
	mustWrite(t, s, "notes/plan.md", "# Plan\n\nOne.\n", IfAny)
	if _, err := s.Write(ctx, "notes/plan.md", "# Plan\n\nTwo.\n", "alice", "", "absent"); err == nil {
		t.Fatal("a refused write landed")
	}
	second := mustWrite(t, s, "notes/plan.md", "# Plan\n\nTwo.\n", first.Hash)
	if err := s.Delete(ctx, "notes/plan.md", "bob", ""); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	want := []got{{"notes/plan.md", first.Hash}, {"notes/plan.md", second.Hash}, {"notes/plan.md", ""}}
	if len(calls) != len(want) {
		t.Fatalf("OnWrite got %+v, want %+v", calls, want)
	}
	for i := range want {
		if calls[i] != want[i] {
			t.Errorf("call %d = %+v, want %+v", i, calls[i], want[i])
		}
	}
}
