package work

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/bdubs00/featherweight-workbench/duties"
	_ "github.com/bdubs00/featherweight-workbench/duties/actions"
)

// releaseChannel is a channel with two agents, as the plan sees it.
func releaseChannel() *PlanChannel {
	return &PlanChannel{ID: "br-notes", Title: "Release notes for 2.4", Open: true, Members: []PlanMember{
		{Handle: "scribe", Agent: "writer", Name: "Writer", Task: "Draft the notes."},
		{Handle: "reviewer-2", Agent: "reviewer", Name: "Reviewer", Task: "Check each note."},
	}}
}

// Make this a job: the plan of a channel lists its agents, runs the one
// that the person picks, else the first, and makes a job that posts the
// words without the schedule in the channel to that agent. The job spends
// nothing of its own, and its result is a post in the channel.
func TestPlanOfAChannel(t *testing.T) {
	p := planOf(t, "Draft the release notes from the merged changes. Every Friday at 4pm", func(in *PlanInput) {
		in.Channel = releaseChannel()
		in.Handle = "reviewer-2"
	})
	if p.Kind != PlanRepeat || len(p.Problems) != 0 || len(p.Members) != 2 || len(p.Who) != 1 || p.Who[0] != (PlanWho{Agent: "reviewer", Name: "Reviewer", Does: "does it in the channel", Handle: "reviewer-2"}) {
		t.Fatalf("the plan = %+v", p)
	}
	if p.Result != "A post in the channel Release notes for 2.4 for each run" || p.Cost.Per != "channel" || !strings.Contains(p.Cost.Basis, "reviewer-2 spends from its budget in the channel") {
		t.Fatalf("result %q, cost %+v", p.Result, p.Cost)
	}
	d := p.Start.Duty
	post, _ := d["do"].(map[string]any)["post"].(map[string]any)
	if d["agent"] != "reviewer" || post["bridge"] != "br-notes" || strings.Join(post["to"].([]string), ",") != "reviewer-2" || post["message"] != "Draft the release notes from the merged changes." {
		t.Fatalf("the job = %+v", d)
	}
	// No pick: the first agent runs it.
	if p := planOf(t, "Draft the notes every Monday at 9am", func(in *PlanInput) { in.Channel = releaseChannel() }); len(p.Who) != 1 || p.Who[0].Handle != "scribe" {
		t.Fatalf("the plan without a pick = %+v", p.Who)
	}
	// What stops a start.
	for _, c := range []struct {
		words, handle string
		closed        bool
		want          string
	}{
		{"Draft the notes", "", false, "when: Say how often"},
		{"Draft the notes every Monday at 9am", "nobody", false, "handle: nobody is no agent of the channel"},
		{"Draft the notes every Monday at 9am", "", true, "is closed. A job posts in a channel that stays open"},
	} {
		p := planOf(t, c.words, func(in *PlanInput) {
			in.Channel = releaseChannel()
			in.Channel.Open = !c.closed
			in.Handle = c.handle
		})
		if !strings.Contains(strings.Join(p.Problems, "|"), c.want) {
			t.Fatalf("%q, %q: problems %q", c.words, c.handle, p.Problems)
		}
	}
}

// The route reads the channel as the caller sees it, and the job of its
// plan saves as it is, with the cost that the page adds, which a job that
// posts drops: it posts in the channel to the agent that the person
// picked. A channel that the caller cannot see is 400.
func TestPlanRouteOfAChannel(t *testing.T) {
	a, srv := newTeamFront(t)
	id := openTeam(t, srv)
	code, b := post(t, srv, "alice", "/v1/work/plan", `{"words":"Write the launch email. Every Monday at 9am","bridge":"`+id+`","handle":"reviewer"}`)
	var p Plan
	if err := json.Unmarshal(b, &p); code != http.StatusOK || err != nil || len(p.Problems) != 0 || len(p.Members) != 2 || p.Who[0].Handle != "reviewer" {
		t.Fatalf("the plan = %d %s", code, b)
	}
	raw, _ := json.Marshal(p.Start.Duty)
	var d duties.Duty
	if err := json.Unmarshal(raw, &d); err != nil {
		t.Fatal(err)
	}
	d.Limits.Budget = &duties.Budget{MaxCostUSD: 0.5}
	if _, err := duties.Of(a).Put(as("alice"), d.Name, duties.DutyBody{Duty: d}, duties.PutOptions{}); err != nil {
		t.Fatalf("the job does not save: %v", err)
	}
	v, err := duties.Of(a).Get(as("alice"), d.Name)
	if err != nil || v.Duty.Do.Post == nil || v.Duty.Do.Post.Bridge != id || strings.Join(v.Duty.Do.Post.To, ",") != "reviewer" || v.Duty.Limits.Budget != nil {
		t.Fatalf("the job = %+v, %v", v.Duty, err)
	}
	if code, b := post(t, srv, "alice", "/v1/work/plan", `{"words":"Write it every Monday","bridge":"nope"}`); code != http.StatusBadRequest || !strings.Contains(string(b), "No channel that you can see") {
		t.Fatalf("a channel that is not there = %d %s", code, b)
	}
}

// The message of a job of a channel starts with a capital, also when the
// schedule came first in the words (tester finding 4).
func TestJobMessage(t *testing.T) {
	for words, want := range map[string]string{
		"every day at 9, post one new tip about backups": "Post one new tip about backups",
		"Draft the notes. Every Friday at 4pm":           "Draft the notes.",
	} {
		said := "every day at 9"
		if strings.HasPrefix(want, "Draft") {
			said = "every friday at 4pm"
		}
		if got := jobMessage(words, said); got != want {
			t.Errorf("jobMessage(%q) = %q, want %q", words, got, want)
		}
	}
}
