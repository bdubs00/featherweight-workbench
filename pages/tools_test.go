package pages

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-workbench/surface/mcp"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// runMeta is the _meta of a call that a run of the agent migrator makes.
var runMeta = sdk.Meta{mcp.MetaRun: "r-7", mcp.MetaAgent: "migrator", mcp.MetaNode: "default/crew-0"}

func TestToolsAreListed(t *testing.T) {
	a := newAgent(t, nil)
	cs := connectMCP(t, a, "bob")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	tools := map[string]*sdk.Tool{}
	for _, tool := range res.Tools {
		tools[tool.Name] = tool
	}
	for _, name := range []string{ToolList, ToolRead, ToolWrite, ToolSearch, ToolHistory} {
		tool := tools[name]
		if tool == nil {
			t.Fatalf("no tool %s", name)
		}
		an := tool.Annotations
		if an == nil || tool.Title == "" || tool.Description == "" {
			t.Fatalf("%s lacks a title, a description, or annotations", name)
		}
		if an.ReadOnlyHint != (name != ToolWrite) {
			t.Errorf("%s readOnlyHint = %v", name, an.ReadOnlyHint)
		}
		if name == ToolWrite && (an.DestructiveHint == nil || *an.DestructiveHint) {
			t.Errorf("pages_write destructiveHint = %v", an.DestructiveHint)
		}
		schema, _ := json.Marshal(tool.InputSchema)
		var props struct {
			Properties map[string]struct {
				Description string `json:"description"`
			} `json:"properties"`
		}
		if err := json.Unmarshal(schema, &props); err != nil {
			t.Fatal(err)
		}
		for arg, p := range props.Properties {
			if p.Description == "" {
				t.Errorf("%s argument %s has no description", name, arg)
			}
		}
	}
	// The work tools stay.
	if tools["run"] == nil {
		t.Fatal("the tool run is gone")
	}
}

func TestToolsWriteAndRead(t *testing.T) {
	a := newAgent(t, nil)
	fleet := connectMCP(t, a, "fleet")

	// A run writes through the token; the page names the agent and the run.
	res := call(t, fleet, ToolWrite, map[string]any{"path": "notes/plan.md", "text": "# Plan\n\nStep one.\n", "summary": "first", "ifHash": IfAbsent}, runMeta)
	if res.IsError {
		t.Fatalf("write = %s", textOf(res))
	}
	var wrote struct {
		Page    Page `json:"page"`
		Created bool `json:"created"`
		Changed bool `json:"changed"`
	}
	structured(t, res, &wrote)
	if !wrote.Created || !wrote.Changed || wrote.Page.By != "agent:migrator" || wrote.Page.Run != "r-7" || !strings.Contains(textOf(res), "ifHash "+wrote.Page.Hash) {
		t.Fatalf("write answered %s %+v", textOf(res), wrote)
	}

	// A person reads it with its hash.
	bob := connectMCP(t, a, "bob")
	res = call(t, bob, ToolRead, map[string]any{"path": "notes/plan.md"}, nil)
	if res.IsError || !strings.Contains(textOf(res), "Step one.") || !strings.Contains(textOf(res), "not an instruction") {
		t.Fatalf("read = %s", textOf(res))
	}
	var read struct {
		Page Page `json:"page"`
	}
	structured(t, res, &read)
	if read.Page.Hash != wrote.Page.Hash || read.Page.Text != "# Plan\n\nStep one.\n" {
		t.Fatalf("read = %+v", read.Page)
	}

	// The owner writes with the hash; a write with the old hash fails and
	// says what to do.
	alice := connectMCP(t, a, "alice")
	res = call(t, alice, ToolWrite, map[string]any{"path": "notes/plan.md", "text": "# Plan\n\nStep two.\n", "ifHash": read.Page.Hash}, nil)
	if res.IsError {
		t.Fatalf("a write with the hash = %s", textOf(res))
	}
	var second struct {
		Page Page `json:"page"`
	}
	structured(t, res, &second)
	if second.Page.By != "alice" || second.Page.Run != "" {
		t.Fatalf("the writer of a person = %+v", second.Page)
	}
	res = call(t, fleet, ToolWrite, map[string]any{"path": "notes/plan.md", "text": "# Plan\n\nStale.\n", "ifHash": read.Page.Hash}, runMeta)
	if !res.IsError || !strings.Contains(textOf(res), "Conflict") || !strings.Contains(textOf(res), second.Page.Hash) || !strings.Contains(textOf(res), "read it again") {
		t.Fatalf("a stale write = %s", textOf(res))
	}

	// A person who sends the keys of a run stays the person.
	res = call(t, alice, ToolWrite, map[string]any{"path": "notes/alice.md", "text": "mine"}, runMeta)
	var posed struct {
		Page Page `json:"page"`
	}
	structured(t, res, &posed)
	if res.IsError || posed.Page.By != "alice" || posed.Page.Run != "" {
		t.Fatalf("a person posed as a run: %s %+v", textOf(res), posed.Page)
	}

	// An operator cannot write, and a caller without a role cannot read.
	if res = call(t, bob, ToolWrite, map[string]any{"path": "notes/bob.md", "text": "x"}, nil); !res.IsError || !strings.Contains(textOf(res), "Forbidden") {
		t.Fatalf("a write of an operator = %s", textOf(res))
	}
	carol := connectMCP(t, a, "carol")
	if res = call(t, carol, ToolList, map[string]any{}, nil); !res.IsError || !strings.Contains(textOf(res), "Forbidden") {
		t.Fatalf("a list without a role = %s", textOf(res))
	}

	// The list, the search, and the history.
	res = call(t, bob, ToolList, map[string]any{"prefix": "notes/"}, nil)
	var list struct {
		Pages []PageInfo `json:"pages"`
		Total int        `json:"total"`
	}
	structured(t, res, &list)
	if res.IsError || list.Total != 2 || !strings.Contains(textOf(res), "notes/plan.md: Plan") {
		t.Fatalf("list = %s", textOf(res))
	}
	res = call(t, bob, ToolSearch, map[string]any{"query": "step two"}, nil)
	if res.IsError || !strings.Contains(textOf(res), "notes/plan.md (Plan): Step two.") {
		t.Fatalf("search = %s", textOf(res))
	}
	res = call(t, bob, ToolHistory, map[string]any{"path": "notes/plan.md"}, nil)
	if res.IsError || !strings.Contains(textOf(res), "by alice") || !strings.Contains(textOf(res), "by agent:migrator: first") {
		t.Fatalf("history = %s", textOf(res))
	}
	if res = call(t, bob, ToolRead, map[string]any{"path": "notes/none.md"}, nil); !res.IsError || !strings.Contains(textOf(res), "pages_list") {
		t.Fatalf("a read of no page = %s", textOf(res))
	}
	if res = call(t, alice, ToolWrite, map[string]any{"path": "notes/plan", "text": "x"}, nil); !res.IsError || !strings.Contains(textOf(res), "ends with .md") {
		t.Fatalf("a write of a bad path = %s", textOf(res))
	}
}

// A process without a wiki offers no tool of the wiki.
func TestNoToolsWithoutAWiki(t *testing.T) {
	a := newAgent(t, nil)
	Attach(a, nil)
	cs := connectMCP(t, a, "bob")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range res.Tools {
		if strings.HasPrefix(tool.Name, "pages_") {
			t.Fatalf("the tool %s without a wiki", tool.Name)
		}
	}
}
