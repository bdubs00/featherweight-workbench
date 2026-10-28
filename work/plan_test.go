package work

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/bdubs00/featherweight-workbench/duties"
	"github.com/bdubs00/featherweight-workbench/duties/schedule"
	"github.com/bdubs00/featherweight-workbench/fleet"
)

// lab is a set of agents that a person can run, as the plan sees them.
func lab() []Candidate {
	return []Candidate{
		{Agent: "ops", Name: "Ops", Description: "Keeps the lab hosts in line with their Ansible roles and reports drift.", Tools: []string{"bash"},
			Ask: []string{"bash: git push*", "destructive"}, Hosts: []string{"*.github.com", "api.anthropic.com:443"}, Egress: true, Member: "lab/ops-0", Costs: []float64{0.02, 0.15, 0.07}},
		{Agent: "writer", Name: "Writer", Description: "Writes summaries, reports, and email drafts about competitors and launches.", Tools: []string{"pages_write"}, Member: "lab/writer-0"},
		{Agent: "reviewer", Name: "Reviewer", Description: "Reviews drafts and code against their sources.", Member: "lab/reviewer-0"},
		{Agent: "migrator", Name: "Migrator", Description: "Migrates Puppet modules to Ansible roles.", Member: "lab/migrator-0", Costs: []float64{0.3, 1.8}},
		{Agent: "chat", Name: "Chat", Description: "Answers questions.", Member: "lab/manager-0"},
	}
}

func planOf(t *testing.T, words string, change func(*PlanInput)) Plan {
	t.Helper()
	in := PlanInput{PlanRequest: PlanRequest{Words: words}, Zone: "America/New_York", Now: t0, Agents: lab(), Chat: "chat", ChatMember: "lab/manager-1", Taken: map[string]bool{}}
	if change != nil {
		change(&in)
	}
	return MakePlan(in)
}

// Words with a schedule make a duty: the grammar of the duties reads the
// schedule, the description of a profile picks the agent, the policy of the
// agent says when it asks, and the runs of the agent give the cost basis.
// The duty asks the person while away, never the manager, and it names no
// cost.
func TestPlanRepeat(t *testing.T) {
	p := planOf(t, "Check the web hosts for drift from their Ansible roles every night", nil)
	if p.Kind != PlanRepeat || len(p.Who) != 1 || p.Who[0].Agent != "ops" || p.Title != "Check the web hosts for drift from their Ansible roles" {
		t.Fatalf("the plan = %+v", p)
	}
	if p.When.Said != "every night" || p.When.Readback != "Every day at 2:00 AM, New York time." || len(p.When.Next) != 3 {
		t.Fatalf("when = %+v", p.When)
	}
	if p.Cost.Basis != "Runs of Ops cost $0.02 to $0.15 in the last 30 days." || p.Cost.Per != "run" || p.Cost.RunsPerMonth != 30 || p.Cost.Runs != 3 {
		t.Fatalf("the cost = %+v", p.Cost)
	}
	if strings.Join(p.Asks, "|") != "Before it runs git push|Before a step that changes or deletes something|When it is not sure what you mean. The question waits for you" {
		t.Fatalf("the asks = %q", p.Asks)
	}
	if p.Tell != TellFound || !strings.Contains(strings.Join(p.Quiet, "|"), "When it finds nothing") {
		t.Fatalf("tell %q, quiet %q", p.Tell, p.Quiet)
	}
	d := p.Start.Duty
	if d == nil || p.Start.Bridge != nil || p.Start.Run != nil || d["name"] != "check-the-web-hosts-for-drift-from-their" || d["agent"] != "ops" || p.Open != "#duties/check-the-web-hosts-for-drift-from-their" {
		t.Fatalf("the start = %+v", p.Start)
	}
	if s := d["when"].(map[string]any)["schedule"].(schedule.Schedule); s.Repeat != "daily" || s.Times[0] != "02:00" || s.TimeZone != "America/New_York" {
		t.Fatalf("the schedule = %+v", s)
	}
	if wa := d["whileAway"].(map[string]any); wa["decider"] != "nobody" {
		t.Fatalf("the decider while away = %v", wa)
	}
	if n := d["notify"].(map[string]any); n["resultWhen"] != TellFound || strings.Join(n["result"].([]string), ",") != "owner" {
		t.Fatalf("notify = %v", n)
	}
	if b := d["limits"].(map[string]any)["budget"].(map[string]any); len(b) != 0 {
		t.Fatalf("the plan names a cost: %v", b)
	}
	if _, ok := d["review"]; ok || len(p.Gaps) != 0 || len(p.Problems) != 0 {
		t.Fatalf("review %v, gaps %q, problems %q", d["review"], p.Gaps, p.Problems)
	}
	// A name that a duty holds gets a number.
	taken := planOf(t, "Check the web hosts for drift from their Ansible roles every night", func(in *PlanInput) { in.Taken["check-the-web-hosts-for-drift-from-their"] = true })
	if taken.Start.Duty["name"] != "check-the-web-hosts-for-drift-from-t-2" {
		t.Fatalf("the free name = %v", taken.Start.Duty["name"])
	}
}

// A question goes to the quick chat now. Words that ask to see a result
// before it goes out mark the run for a check, and a send of an email that
// no tool can make is a gap. A host outside the egress of the pool is a
// gap too.
func TestPlanOnce(t *testing.T) {
	q := planOf(t, "What is the state of the backups?", nil)
	if q.Kind != PlanOnce || q.Who[0].Agent != "chat" || q.Start.Run == nil || q.Start.Run.Member != "lab/manager-1" || q.Result != "The answer, in Chat" {
		t.Fatalf("a question = %+v", q)
	}
	if md := q.Start.Run.Body["metadata"].(map[string]string); md["fw.view"] != "quickchat" {
		t.Fatalf("the metadata of a question = %v", md)
	}
	e := planOf(t, "Draft the launch email for the competitors summary, and show me before it goes out", nil)
	if e.Kind != PlanOnce || e.Who[0].Agent != "writer" || e.Start.Run.Member != "lab/writer-0" {
		t.Fatalf("an email = %+v", e)
	}
	if md := e.Start.Run.Body["metadata"].(map[string]string); md[fleet.MetaReview] != "1" || md["fw.view"] != "task" {
		t.Fatalf("the metadata of a run to check = %v", md)
	}
	if !strings.Contains(strings.Join(e.Asks, "|"), "Before it goes out") || len(e.Gaps) != 1 || !strings.HasPrefix(e.Gaps[0], "No agent here can send email.") {
		t.Fatalf("asks %q, gaps %q", e.Asks, e.Gaps)
	}
	h := planOf(t, "Ask ops to read https://status.example.com/hosts and the api.github.com status", func(in *PlanInput) { in.Agent = "ops" })
	if len(h.Gaps) != 1 || !strings.HasPrefix(h.Gaps[0], "Ops cannot reach status.example.com:") {
		t.Fatalf("the gaps of hosts = %q", h.Gaps)
	}
}

// Words that split the work make a bridge with a seat per share and a
// reviewer. A request to review a draft makes a team of the writer and the
// reviewer. The cost of a team bounds the whole bridge.
func TestPlanTeam(t *testing.T) {
	p := planOf(t, "Migrate the remaining Puppet modules, split across three agents", nil)
	if p.Kind != PlanTeam || p.Who[0].Agent != "migrator" || p.Who[0].Count != 3 || p.Who[1].Agent != "reviewer" || p.Cost.Per != "bridge" {
		t.Fatalf("a team = %+v", p)
	}
	b := p.Start.Bridge
	seats := b["members"].([]map[string]any)
	if len(seats) != 4 || seats[0]["handle"] != "migrator-1" || seats[3]["agent"] != "reviewer" || b["placement"] != "queue" || !strings.HasSuffix(p.Cost.Basis, "The team stops at your limit.") {
		t.Fatalf("the bridge = %+v", b)
	}
	r := planOf(t, "Write a summary of the launch and have it reviewed", nil)
	if r.Kind != PlanTeam || len(r.Start.Bridge["members"].([]map[string]any)) != 2 {
		t.Fatalf("a reviewed draft = %+v", r)
	}
}

// With no agent that fits, the plan says so, lists the agents, and holds
// no start. An agent that the person cannot run is a problem too.
func TestPlanNoAgent(t *testing.T) {
	p := planOf(t, "Juggle flaming torches", nil)
	if p.Kind != PlanOnce || len(p.Who) != 0 || strings.Join(p.Problems, "|") != "agent: No agent fits these words. Pick one." || p.Start.Run != nil || p.Start.Duty != nil {
		t.Fatalf("no agent = %+v", p)
	}
	if len(p.Others) != 4 || p.Others[0].Agent != "ops" {
		t.Fatalf("the agents to pick = %+v", p.Others)
	}
	x := planOf(t, "Check the hosts", func(in *PlanInput) { in.Agent = "nobody" })
	if len(x.Problems) != 1 || !strings.HasPrefix(x.Problems[0], "agent: You cannot run an agent named nobody.") || len(x.Who) != 0 {
		t.Fatalf("an unknown agent = %+v", x)
	}
}

// The readback names only the asks of tools that the agent can call, and
// names the tool of an ask with an argument.
func TestPlanAsksOfItsTools(t *testing.T) {
	patterns := []string{"bash: git push*", "ansible.playbook: check=false", "env: command=add", "destructive", "front.*"}
	got := strings.Join(asksOf(patterns, []string{"bash", "files"}), "|")
	if got != "Before it runs git push|Before a step that changes or deletes something" {
		t.Errorf("the asks of an agent with bash and files = %q", got)
	}
	got = strings.Join(asksOf(patterns, []string{"bash", "ansible.*", "front.*"}), "|")
	if got != "Before it runs git push|Before it calls playbook in ansible, with check=false|Before a step that changes or deletes something|Before it uses a tool of this server" {
		t.Errorf("the asks of an agent with ansible = %q", got)
	}
	if n := len(asksOf(patterns, nil)); n != len(patterns) {
		t.Errorf("an agent without a list of tools has %d asks, want %d", n, len(patterns))
	}
}

// A model that costs less than a cent a run never reads as no cost in the
// basis of a plan: "$0.00 to $0.00" became "under $0.01 each".
func TestPlanCostBelowACent(t *testing.T) {
	setCosts := func(costs ...float64) func(*PlanInput) {
		return func(in *PlanInput) {
			for i := range in.Agents {
				if in.Agents[i].Agent == "ops" {
					in.Agents[i].Costs = costs
				}
			}
		}
	}
	words := "Check the web hosts for drift from their Ansible roles every night"
	cases := []struct {
		costs []float64
		want  string
	}{
		{[]float64{0.0007, 0.0024}, "Runs of Ops cost under $0.01 each in the last 30 days."},
		{[]float64{0.0007, 0.03}, "Runs of Ops cost under $0.01 to $0.03 in the last 30 days."},
		{[]float64{0.021, 0.024}, "Runs of Ops cost $0.02 each in the last 30 days."},
	}
	for _, c := range cases {
		if p := planOf(t, words, setCosts(c.costs...)); p.Cost.Basis != c.want {
			t.Fatalf("costs %v: the basis = %q, want %q", c.costs, p.Cost.Basis, c.want)
		}
	}
}

// The action of the person picks the agent over a shared word: "Draft an
// email" goes to the agent that writes, though the reviewer reviews drafts.
// A title keeps a period inside a word, such as the 3.0 of a version.
func TestPlanActionAndTitle(t *testing.T) {
	quill := func(in *PlanInput) {
		in.Agents = []Candidate{
			{Agent: "ops", Name: "Ops", Description: "Compares the hosts with the baseline and reports drift.", Member: "q/ops-0"},
			{Agent: "researcher", Name: "Researcher", Description: "Finds and sums up facts from the shared notes and the wiki.", Member: "q/researcher-0"},
			{Agent: "reviewer", Name: "Reviewer", Description: "Reviews drafts and flags claims without a source.", Member: "q/reviewer-0"},
			{Agent: "writer", Name: "Writer", Description: "Writes marketing copy for the launch.", Member: "q/writer-0"},
		}
	}
	for words, want := range map[string]string{
		"Draft a short thank-you email to our beta users":  "writer",
		"Write a short thank-you email to our beta users":  "writer",
		"Review the draft of the launch email":             "reviewer",
		"Summarize the competitor notes":                   "researcher",
		"Every night, compare the hosts with the baseline": "ops",
	} {
		if p := planOf(t, words, quill); len(p.Who) == 0 || p.Who[0].Agent != want {
			t.Errorf("%q picks %+v, want %s", words, p.Who, want)
		}
	}
	p := planOf(t, "Write an invite to the 3.0 webinar. Keep it short.", quill)
	if p.Title != "Write an invite to the 3.0 webinar" {
		t.Fatalf("the title = %q", p.Title)
	}
	if sentenceEnd("See example.com/docs") != -1 || sentenceEnd("Done. Next") != 4 || sentenceEnd("Why? Because") != 3 {
		t.Fatal("sentenceEnd")
	}
}

// The cost of a plan is for the whole task: one run of the agent, or one
// run of each seat of a team together. Without a run with a cost, both
// numbers are absent.
func TestPlanCostOfTask(t *testing.T) {
	once := planOf(t, "Check the web hosts for drift from their Ansible roles", nil)
	if once.Cost.Low != 0.02 || once.Cost.High != 0.15 {
		t.Fatalf("the cost of one run = %+v", once.Cost)
	}
	team := planOf(t, "Migrate the Puppet modules to Ansible roles, split across four agents", nil)
	if team.Kind != PlanTeam || team.Who[0].Agent != "migrator" || team.Who[0].Count != 4 {
		t.Fatalf("the team = %+v", team.Who)
	}
	if team.Cost.Low != 1.2 || team.Cost.High != 7.2 || team.Cost.Runs != 2 || !strings.Contains(team.Cost.Basis, "The whole team cost $1.20 to $7.20. The team stops at your limit.") {
		t.Fatalf("the cost of the team = %+v", team.Cost)
	}
	none := planOf(t, "Write a summary of the launch every Friday", nil)
	raw, _ := json.Marshal(none.Cost)
	if none.Who[0].Agent != "writer" || strings.Contains(string(raw), `"low"`) || strings.Contains(string(raw), `"high"`) {
		t.Fatalf("the cost without a priced run = %s", raw)
	}
}

// A plan that repeats names a duty of the person that does the same work:
// the same agent and most of the same words, whatever the schedule. A duty
// of another agent, a duty with other work, and a task once name none.
func TestPlanSame(t *testing.T) {
	own := []duties.Duty{
		{Name: "weekly-competitor-summary", Title: "Weekly competitor summary", Agent: "writer",
			Do: duties.Do{Ask: &duties.Ask{Message: "Sum up what our competitors shipped this week.\n\nWrite the result to the wiki page writer/x/{date}.md. When you find nothing, say so in one line."}}},
		{Name: "launch-notes", Title: "Launch notes", Agent: "writer", Do: duties.Do{Ask: &duties.Ask{Message: "Write the notes of the launch."}}},
		{Name: "competitor-drift", Agent: "ops", Do: duties.Do{Ask: &duties.Ask{Message: "Sum up what our competitors shipped."}}},
	}
	with := func(in *PlanInput) { in.Own = own }
	p := planOf(t, "Every Monday at 9am, sum up what our competitors shipped last week", with)
	if p.Kind != PlanRepeat || p.Same == nil || p.Same.Duty != "weekly-competitor-summary" || p.Same.Title != "Weekly competitor summary" {
		t.Fatalf("the same duty = %+v (who %+v)", p.Same, p.Who)
	}
	if !strings.Contains(PlanText(p), "The same work: your duty Weekly competitor summary (weekly-competitor-summary) does it already.") {
		t.Fatalf("the text = %s", PlanText(p))
	}
	if q := planOf(t, "Every Friday, write a poem about the office plants", with); q.Same != nil {
		t.Fatalf("other work = %+v", q.Same)
	}
	if q := planOf(t, "Every Monday, sum up what our competitors shipped", func(in *PlanInput) { in.Own = own; in.Agent = "ops" }); q.Same == nil || q.Same.Duty != "competitor-drift" {
		t.Fatalf("the duty of the picked agent = %+v", q.Same)
	}
	if q := planOf(t, "Sum up what our competitors shipped last week", with); q.Kind != PlanOnce || q.Same != nil {
		t.Fatalf("a task once = %s %+v", q.Kind, q.Same)
	}
	raw, _ := json.Marshal(planOf(t, "Every Friday, write a poem about the office plants", nil))
	if strings.Contains(string(raw), `"same"`) {
		t.Fatalf("a plan without the same work = %s", raw)
	}
}
