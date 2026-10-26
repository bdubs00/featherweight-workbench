package bridges

import (
	"context"
	"strings"
	"testing"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/fleet"
)

// A person does over MCP what the page does, with the functions of the
// routes and their access: post with a file and a reply, change the
// bridge and a seat, react, and mark the channel read. A person without
// the role is refused, and a run of the fleet uses the tools of a run.
func TestToolsOfAPerson(t *testing.T) {
	a := newAgent(t, nil)
	s := newStore(t, a, nil)
	v, _ := withFleet(t, a)
	v.state("default/crew-0", fleet.MemberLive)
	url := serve(t, a)
	it := open(t, url)
	v.put(workerRun("r-rev", it.ID, "reviewer", "reviewer", fw.StateRunning))
	s.poll(context.Background())
	alice := connectMCP(t, a, "alice")

	res := callTool(t, alice, ToolPost, map[string]any{"bridge": it.ID, "text": "Notes for the work.", "to": []string{"reviewer"},
		"files": []map[string]any{{"name": "notes.md", "data": "IyBOb3Rlcwo="}}}, nil)
	var m Message
	structured(t, res, &m)
	if res.IsError || m.From.Kind != KindPerson || m.From.Name != "alice" || len(m.Files) != 1 || !strings.Contains(textOf(res), "It carries 1 file.") {
		t.Fatalf("a post with a file = %s %+v", textOf(res), m)
	}
	posted := m.Seq
	res = callTool(t, alice, ToolPost, map[string]any{"bridge": it.ID, "text": "Yes.", "replyTo": posted}, nil)
	structured(t, res, &m)
	if res.IsError || m.ReplyTo == nil || *m.ReplyTo != posted {
		t.Fatalf("a reply = %s %+v", textOf(res), m)
	}
	if res := callTool(t, alice, ToolPost, map[string]any{"bridge": it.ID, "text": "?", "ask": true}, nil); !res.IsError || !strings.Contains(textOf(res), "ask and choices are for a seat") {
		t.Fatalf("a person who asks = %s", textOf(res))
	}

	res = callTool(t, alice, ToolReact, map[string]any{"bridge": it.ID, "seq": posted, "reaction": "up", "note": "good"}, nil)
	if res.IsError || !strings.Contains(textOf(res), "Reacted up to entry") {
		t.Fatalf("bridge_react = %s", textOf(res))
	}
	res = callTool(t, alice, ToolReact, map[string]any{"bridge": it.ID, "seq": posted, "reaction": "up", "remove": true}, nil)
	var took Message
	structured(t, res, &took)
	if res.IsError || !strings.Contains(textOf(res), "Took back up on entry") || len(took.Reactions[ReactionUp]) != 0 {
		t.Fatalf("bridge_react with remove = %s %+v", textOf(res), took.Reactions)
	}
	// As on the page, the same reaction twice puts it on and takes it back.
	for i, want := range []string{"Reacted heart to entry", "Took back heart on entry"} {
		res = callTool(t, alice, ToolReact, map[string]any{"bridge": it.ID, "seq": posted, "reaction": "heart"}, nil)
		var m Message
		structured(t, res, &m)
		if res.IsError || !strings.Contains(textOf(res), want) || len(m.Reactions[ReactionHeart]) != 1-i {
			t.Fatalf("bridge_react heart %d = %s %+v", i+1, textOf(res), m.Reactions)
		}
	}
	res = callTool(t, alice, ToolMarkRead, map[string]any{"bridge": it.ID, "seq": 9999}, nil)
	var mark markReadResult
	structured(t, res, &mark)
	rec, _, err := s.b.get(context.Background(), it.ID)
	if err != nil || res.IsError || mark.Read != rec.LastSeq {
		t.Fatalf("bridge_mark_read = %s %+v", textOf(res), mark)
	}
	res = callTool(t, alice, ToolMemberPatch, map[string]any{"bridge": it.ID, "handle": "reviewer", "muted": true}, nil)
	var seat Member
	structured(t, res, &seat)
	if res.IsError || !seat.Muted || !strings.Contains(textOf(res), "is ") {
		t.Fatalf("bridge_member_patch = %s %+v", textOf(res), seat)
	}
	res = callTool(t, alice, ToolPatch, map[string]any{"bridge": it.ID, "title": "Migrate logrotate, part 2", "caps": map[string]any{"maxMessages": 50}, "state": "closed"}, nil)
	var changed Item
	structured(t, res, &changed)
	if res.IsError || changed.Title != "Migrate logrotate, part 2" || changed.State != StateClosed || changed.Caps.MaxMessages != 50 {
		t.Fatalf("bridge_patch = %s %+v", textOf(res), changed)
	}

	carol := connectMCP(t, a, "carol")
	if res := callTool(t, carol, ToolPatch, map[string]any{"bridge": it.ID, "title": "mine"}, nil); !res.IsError || !strings.Contains(textOf(res), "Forbidden") {
		t.Fatalf("carol bridge_patch = %s", textOf(res))
	}
	fleetMCP := connectMCP(t, a, "fleet")
	for _, name := range []string{ToolPatch, ToolMemberPatch, ToolReact, ToolMarkRead} {
		args := map[string]any{"bridge": it.ID, "seq": 1, "handle": "reviewer", "reaction": "up"}
		if res := callTool(t, fleetMCP, name, args, metaOf("r-mig", "migrator", "alice")); !res.IsError || !strings.Contains(textOf(res), "does what a person does") {
			t.Fatalf("a run calls %s = %s", name, textOf(res))
		}
	}
}
