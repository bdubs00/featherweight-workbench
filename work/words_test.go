package work

import (
	"regexp"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/bridges"
)

// oldWords matches the words that a person no longer reads: the page says
// channel, agent, and job.
var oldWords = regexp.MustCompile(`(?i)\b(bridges?|seats?|seated|workers?|duty|duties|teams?)\b`)

// The words that the board and the door write for a person say channel,
// agent, and job: no reason of a card, no line of the readback of a plan,
// and no words of a tool say bridge, seat, worker, duty, or team.
func TestWordsSayChannelAndJob(t *testing.T) {
	texts := []string{agentWorks, teamDone, agentGone}
	for _, tool := range []string{"front.bridge_open", "front.bridge_post", "front.bridge_read", "front.bridge_close", "front.bridge_add_member",
		"front.duty_put", "front.duty_run_now", "front.duties_list", "front.duties_pause_all"} {
		texts = append(texts, toolWords(tool))
	}

	// The readback of each kind of plan, also when no member runs its
	// agent, and with the gaps of email and of hosts.
	noMember := func(in *PlanInput) {
		for i := range in.Agents {
			in.Agents[i].Member = ""
		}
		in.ChatMember = ""
	}
	for _, words := range []string{
		"Migrate the remaining Puppet modules, split across three agents",
		"Check the web hosts for drift from their Ansible roles every night",
		"Draft the launch email for the competitors summary, and show me before it goes out",
		"Ask ops to read https://status.example.com/hosts and the api.github.com status",
	} {
		for _, p := range []Plan{planOf(t, words, nil), planOf(t, words, noMember)} {
			texts = append(texts, p.When.Readback, p.Cost.Basis, p.Away, p.Result)
			for _, list := range [][]string{p.Asks, p.Quiet, p.Gaps, p.Problems} {
				texts = append(texts, list...)
			}
			for _, w := range p.Who {
				texts = append(texts, w.Does)
			}
		}
	}

	// The reasons of the cards of channels whose agents are in each
	// presence, of an empty channel, and of a closed one.
	open := func(id string, members ...bridges.Member) bridges.Item {
		return bridges.Item{Bridge: bridges.Bridge{ID: id, Title: id, Operator: "alice", State: bridges.StateOpen, CreatedAt: ago(time.Hour), Members: members}}
	}
	var items []bridges.Item
	for i, p := range []string{bridges.PresenceNeedsYou, bridges.PresenceWorking, bridges.PresenceWaiting, bridges.PresenceError, bridges.PresenceLimit, bridges.PresenceNotStarted, bridges.PresenceIdle} {
		items = append(items, open(p, bridges.Member{Handle: "writer", Agent: "writer", Presence: p, Gone: i == 3}))
	}
	items = append(items, open("asked-and-gone", bridges.Member{Handle: "ops", Agent: "ops", Presence: bridges.PresenceNeedsYou, Gone: true}), open("empty"))
	closed := open("closed")
	closed.State = bridges.StateClosed
	items = append(items, closed)
	failed := run("R-X", "writer", fw.StateFailed, ago(time.Hour), ago(time.Minute), nil)
	failed.Error = "budget exceeded: maxCostUSD"
	in := Input{Me: "alice", Now: t0, Bridges: items, Runs: []Run{
		{Run: run("R-Q", "writer", fw.StateQueued, ago(time.Hour), ago(time.Minute), nil)},
		{Run: run("R-W", "writer", fw.StateRunning, ago(time.Hour), ago(time.Minute), nil)},
		{Run: failed},
	}}
	in.Mine = mineAll(in.Runs...)
	ans := Assemble(in)
	if len(ans.Cards) < len(items) {
		t.Fatalf("the board has %d cards for %d channels", len(ans.Cards), len(items))
	}
	for _, c := range ans.Cards {
		texts = append(texts, c.Reason)
	}

	for _, text := range texts {
		if w := oldWords.FindString(text); w != "" {
			t.Errorf("a text for a person says %q: %q", w, text)
		}
	}
}
