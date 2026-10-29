package bridges

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/fleet"
)

func TestOpaque(t *testing.T) {
	for subject, want := range map[string]bool{
		erinSubject:                        true,
		"google-oauth2|109876543210987654": true,
		"1098765432109876543":              true,
		"alice":                            false,
		"alice@example.com":                false,
		"token":                            false,
		"local:bdubs":                      false,
		"spiffe://td/ns/prod/sa/crew":      false,
		"deadbeef":                         false,
	} {
		if got := opaque(subject); got != want {
			t.Errorf("opaque(%q) = %v", subject, got)
		}
	}
}

// No text of the front names a person by an id that the issuer made. The
// manager opens a bridge for erin, whose subject is a UUID: before the
// front knows her name, the line, the instructions, and the result of the
// tool say a person or the operator; once erin called a route, they name
// her by her display name, the part of her email before the @ in readable
// form, and the record keeps it. Toward the agents, a person is operator.
func TestNoTextNamesAPersonByAnID(t *testing.T) {
	a := newAgentWith(t, nil, fw.WithProfile(fw.AgentProfile{Name: "helper", Access: &fw.Access{Operators: []string{"*"}}}))
	s := newStore(t, a, nil)
	v, fm := withFleet(t, a)
	v.state("default/crew-0", fleet.MemberLive)
	url := serve(t, a)
	cs := connectMCP(t, a, "fleet")
	meta := metaOf("mgr-conv", "manager", erinSubject)
	plan := map[string]any{"title": "Backup policy", "goal": "Write it.", "members": []map[string]any{
		{"handle": "writer", "agent": "helper", "task": "Write the policy"},
		{"handle": "reviewer", "agent": "helper", "task": "Review the policy"}}}
	res := callTool(t, cs, ToolOpen, plan, meta)
	if res.IsError || strings.Contains(textOf(res), erinSubject) || !strings.Contains(textOf(res), "for a person") {
		t.Fatalf("open = %s", textOf(res))
	}
	var first Item
	structured(t, res, &first)
	if l := withEvent(channel(t, s, first.ID), EventOpened); len(l) != 1 || l[0].Text != "the manager for a person opened the channel with 2 members" {
		t.Fatalf("opened = %+v", l)
	}
	rec, _, _ := s.b.get(context.Background(), first.ID)
	if in := s.instructionsOf(context.Background(), rec, rec.Members[0]); strings.Contains(in, erinSubject) || !strings.Contains(in, "This work is for the person who asked, who reads the channel.") {
		t.Fatalf("instructions = %s", in)
	}

	// erin opens the page, which lists her bridges: the front learns her
	// name.
	if resp, body := do(t, http.MethodGet, url+"/v1/bridges", "erin", ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("list = %d %s", resp.StatusCode, body)
	}
	res = callTool(t, cs, ToolOpen, plan, meta)
	var second Item
	structured(t, res, &second)
	if res.IsError || !strings.Contains(textOf(res), "for Erin") || second.OperatorName != "Erin" || second.Operator != erinSubject {
		t.Fatalf("open after the list = %s %+v", textOf(res), second.Bridge)
	}
	if l := withEvent(channel(t, s, second.ID), EventOpened); len(l) != 1 || l[0].Text != "the manager for Erin opened the channel with 2 members" {
		t.Fatalf("opened = %+v", l)
	}
	rec, _, _ = s.b.get(context.Background(), second.ID)
	if in := s.instructionsOf(context.Background(), rec, rec.Members[0]); !strings.Contains(in, "This work is for Erin, who reads the channel.") {
		t.Fatalf("instructions = %s", in)
	}
	if res := callTool(t, cs, ToolList, map[string]any{}, meta); !strings.Contains(textOf(res), "of Erin.") {
		t.Fatalf("list = %s", textOf(res))
	}

	// A message of erin reaches a seat from Erin (person), and bridge_read
	// names her operator.
	v.put(workerRun("r-wri", second.ID, "writer", "helper", fw.StateCompleted))
	s.poll(context.Background())
	do(t, http.MethodPost, url+"/v1/bridges/"+second.ID+"/messages", "erin", `{"text":"Go on.","to":["writer"]}`)
	eventually(t, "the writer gets the message", func() bool { return len(fm.find(http.MethodPost, "/v1/runs/r-wri/messages")) == 1 })
	if c, _ := fm.find(http.MethodPost, "/v1/runs/r-wri/messages")[0].Body["content"].(string); !strings.Contains(c, "] @writer from Erin (person): Go on.") {
		t.Fatalf("content = %q", c)
	}
	res = callTool(t, cs, ToolRead, map[string]any{}, metaOf("r-wri", "helper", erinSubject))
	if strings.Contains(textOf(res), erinSubject) || !strings.Contains(textOf(res), "Erin (person) to @writer: Go on.") {
		t.Fatalf("read = %s", textOf(res))
	}

	// A decider whose name the front never learned is a person.
	if got := s.deciderName("0f6a3c2e-1b7d-4e2a-9c1f-2a3b4c5d6e7f"); got != "a person" {
		t.Fatalf("decider = %q", got)
	}
	if got := s.deciderName(erinSubject); got != "Erin" {
		t.Fatalf("decider erin = %q", got)
	}
}
