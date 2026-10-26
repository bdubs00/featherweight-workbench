package dashboard

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/fleet"
)

// stubFleet is a fleet with fixed runs of another member.
type stubFleet struct {
	runs []*fw.Run
	err  error
}

func (stubFleet) Name() string { return "here" }

func (stubFleet) Members(context.Context) ([]fleet.Member, error) { return nil, nil }

func (s stubFleet) Runs(context.Context) ([]*fw.Run, error) { return s.runs, s.err }

func (s stubFleet) Run(_ context.Context, id string) (*fw.Run, error) {
	for _, r := range s.runs {
		if r.ID == id {
			return r, nil
		}
	}
	return nil, fw.ErrRunNotFound
}

func (stubFleet) Forward(context.Context, string, *http.Request) (*http.Response, error) {
	return nil, fleet.ErrNoRoute
}

func ids(runs []*fw.Run) string {
	var out []string
	for _, r := range runs {
		out = append(out, r.ID)
	}
	return strings.Join(out, ",")
}

func TestMembersText(t *testing.T) {
	text := MembersText("here", []fleet.Member{
		{Name: "here", Self: true, State: fleet.MemberLive, AccessMode: fw.AccessModeRules, Agents: []fleet.RecordAgent{{Name: "reviewer", Role: "owner"}}, Active: 1},
		{Name: "there", State: fleet.MemberQuiet, LastSeen: time.Now().Add(-3 * time.Minute), AccessMode: fw.AccessModeExternal, CanForward: true, Agents: []fleet.RecordAgent{{Name: "deployer", Role: "operator"}}, InputRequired: 2},
		{Name: "far", State: fleet.MemberStopped, LastSeen: time.Now().Add(-50 * time.Hour)},
		{Name: "full", State: fleet.MemberQuiet, LastSeen: time.Now().Add(-10 * time.Second), BlockedReason: "storage unavailable: no space left on device"},
	})
	for _, want := range []string{
		"This process is the fleet member here.",
		"- here (this process): live, access rules; agents reviewer (you are owner); 1 active run, 0 waiting for input",
		"- there: quiet, heard 3 minutes ago, access external; agents deployer (you are operator); 0 active runs, 2 waiting for input; this process can act on its runs",
		"- far: stopped, heard 2 days ago; no agent you can see",
		"- full: quiet, heard 10 seconds ago; blocked by a storage failure (storage unavailable: no space left on device), so the server suspends its Task, and the next request restarts it; no agent you can see",
		"read-only from here",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("text lacks %q:\n%s", want, text)
		}
	}
	if got := MembersText("here", nil); !strings.Contains(got, "You can see no member.") {
		t.Errorf("empty = %s", got)
	}
	// The agents come first in plain words, before the names of the
	// members.
	agents := AgentsText([]fleet.Member{
		{Name: "front", Self: true, State: fleet.MemberLive, Agents: []fleet.RecordAgent{{Name: "triage"}}},
		{Name: "default/manager-1", State: fleet.MemberLive, Agents: []fleet.RecordAgent{{Name: "manager"}}, Active: 1},
		{Name: "team/reviewer-0", State: fleet.MemberLive, Agents: []fleet.RecordAgent{{Name: "reviewer"}}, InputRequired: 1},
		{Name: "team/reviewer-1", State: fleet.MemberQuiet, Agents: []fleet.RecordAgent{{Name: "reviewer"}}},
		{Name: "team/migrator-0", State: fleet.MemberQuiet, Agents: []fleet.RecordAgent{{Name: "migrator"}}},
	})
	want := "Agents that you can see:\n- triage: on this server\n- manager: 1 worker, awake; 1 run works\n- reviewer: 2 workers: 1 awake, 1 asleep; 1 run waits for a person\n- migrator: 1 worker, asleep\n" +
		"To a person, name the agents and their workers, such as \"the manager has 1 worker awake\", and not the names of the members below."
	if agents != want {
		t.Errorf("AgentsText =\n%s\nwant\n%s", agents, want)
	}
	if i, j := strings.Index(text, "Agents that you can see:"), strings.Index(text, "members you can see"); i < 0 || j < i {
		t.Errorf("the agents do not come first:\n%s", text)
	}
	if got := AgentsText([]fleet.Member{{Name: "far"}}); got != "" {
		t.Errorf("members without an agent = %q", got)
	}
}

func TestPageHasTheFleet(t *testing.T) {
	for _, want := range []string{"/v1/fleet", `call('fleet')`, `S.me.links.metrics`, `'/v1/home'`, `data-view="others"`, "Runs by others", "?member="} {
		if !bytes.Contains(Page(), []byte(want)) {
			t.Errorf("page lacks %q", want)
		}
	}
}

// The lists of runs leave out a check run of this process, and a check
// run and a run of the front for itself of another member.
func TestRunsLeaveOutTheRunsOfTheFront(t *testing.T) {
	queue := fw.NewTool(fw.ToolSpec{Name: "queue", ReadOnly: true}, func(context.Context, json.RawMessage) (fw.ToolResult, error) {
		return fw.TextResult(`{"items": []}`), nil
	})
	a := newAgent(t, fw.WithTools(queue))
	ctx := context.Background()
	check, err := a.Run(ctx, "", fw.WithCheck(fw.Check{Steps: []fw.CheckStep{{Tool: "queue"}}}))
	if err != nil || check.Kind() != fw.RunKindCheck {
		t.Fatalf("the check run = %+v, %v", check, err)
	}
	work, err := a.Run(ctx, "Write the report")
	if err != nil {
		t.Fatal(err)
	}
	fleet.Attach(a, stubFleet{runs: []*fw.Run{
		{ID: "remote-check", Member: "team/w", Metadata: map[string]string{fw.MetaKind: fw.RunKindCheck}},
		{ID: "remote-own", Member: "team/w", Metadata: map[string]string{fleet.MetaInternal: "duty-write-check:report"}},
		{ID: "remote-work", Member: "team/w"},
	}})
	t.Cleanup(func() { fleet.Attach(a, nil) })
	runs, err := Runs(ctx, a, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(runs); got != work.ID+",remote-work" && got != "remote-work,"+work.ID {
		t.Fatalf("runs = %s", got)
	}
}
