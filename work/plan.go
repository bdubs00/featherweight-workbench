package work

// This file is the plan of a task: the words of a person, read back
// before anything starts (docs/ARCHITECTURE.md section 27). MakePlan is a
// pure function with fixed rules and no model, so the same words always
// give the same plan. The schedule comes from the grammar of the duties.
// The agent comes from the names and the descriptions of the profiles that
// the person can run. The asks come from the policy of the agent, the gaps
// from the egress of its pool, and the cost basis from its runs of the
// last 30 days. The plan names no cost: the person types the most that a
// task can spend before Start (no silent cost defaults). Start sends the
// record of the plan to a route that exists: PUT /v1/duties/{name}, POST
// /v1/bridges, or POST /v1/runs?member=.

import (
	"cmp"
	"context"
	"fmt"
	"math"
	"path"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/surface/view"
	"github.com/bdubs00/featherweight-workbench/bossmode"
	"github.com/bdubs00/featherweight-workbench/duties"
	"github.com/bdubs00/featherweight-workbench/duties/schedule"
	"github.com/bdubs00/featherweight-workbench/faces"
	"github.com/bdubs00/featherweight-workbench/fleet"
)

// Kinds of a plan.
const (
	PlanOnce   = "once"   // one run now
	PlanRepeat = "repeat" // a duty on a schedule
	PlanTeam   = "team"   // a bridge with seats
)

// When a person hears of a task that repeats (PlanRequest.Tell).
const (
	TellEach     = "each"     // each result
	TellFound    = "found"    // a result only when the run found something
	TellFailures = "failures" // failures and questions only
)

// MaxWords bounds the words of a plan, in bytes.
const MaxWords = 4000

// PlanRequest is what a person asks for in words. Agent names the agent
// that the person picked, and Tell when the person hears of a task that
// repeats.
type PlanRequest struct {
	Words string `json:"words"`
	Agent string `json:"agent,omitempty"`
	Tell  string `json:"tell,omitempty"`
}

// Plan is the readback of a task. Who names the agents that do it, and
// Others the agents that the person can pick instead. When says the
// schedule in words. Cost says what runs of the agent cost, never a
// limit. Asks lists when the task waits for the person, and Quiet when it
// stays quiet. Away says, for a task that repeats, who decides a question
// while the person is away, the row "While you are away" of the readback.
// Gaps say what no agent here can do. Problems stop a start.
// Start holds the record that Start sends: one of duty, bridge, or run.
// Open is the place of the page that shows the task after the start. Same
// names a duty of the person that does the same work already, or is nil.
type Plan struct {
	Words    string      `json:"words"`
	Kind     string      `json:"kind"`
	Title    string      `json:"title"`
	Who      []PlanWho   `json:"who"`
	Others   []PlanAgent `json:"others"`
	When     PlanWhen    `json:"when"`
	Cost     PlanCost    `json:"cost"`
	Asks     []string    `json:"asks"`
	Quiet    []string    `json:"quiet"`
	Tell     string      `json:"tell"`
	Away     string      `json:"away,omitempty"`
	Result   string      `json:"result"`
	Gaps     []string    `json:"gaps"`
	Problems []string    `json:"problems"`
	Start    PlanStart   `json:"start"`
	Open     string      `json:"open"`
	Same     *PlanSame   `json:"same,omitempty"`
}

// PlanSame is a duty of the person that does the work of a plan already:
// its name and its title. The page offers to open it beside Start.
type PlanSame struct {
	Duty  string `json:"duty"`
	Title string `json:"title"`
}

// PlanWho is an agent of a plan and what it does in it. Count is the
// number of its seats in a team.
type PlanWho struct {
	Agent string `json:"agent"`
	Name  string `json:"name"`
	Does  string `json:"does"`
	Count int    `json:"count,omitempty"`
}

// PlanAgent is an agent that the person can pick.
type PlanAgent struct {
	Agent       string `json:"agent"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

// PlanWhen is the schedule of a plan in words, and its next three times.
type PlanWhen struct {
	Said     string      `json:"said,omitempty"`
	Readback string      `json:"readback"`
	Next     []time.Time `json:"next"`
}

// PlanCost is the basis of the cost of a plan: what runs of the agent
// cost in the last 30 days, what the limit of the person bounds (a run,
// or a bridge for a team), and about how many runs a month the schedule
// starts. Low and High are the least and the most that the whole task
// cost in those days, in US dollars: one run of the agent, or for a team
// one run of each seat together. Both are 0, and absent, when no run of
// the agents reports a cost. Runs counts the runs with a cost.
type PlanCost struct {
	Basis        string  `json:"basis"`
	Per          string  `json:"per"`
	RunsPerMonth int     `json:"runsPerMonth"`
	Low          float64 `json:"low,omitempty"`
	High         float64 `json:"high,omitempty"`
	Runs         int     `json:"runs,omitempty"`
}

// PlanStart is the record that Start sends to a route that exists. The
// page adds the limit of the person: limits.budget.maxCostUSD of a duty,
// caps.maxCostUSD of a bridge, or budget.maxCostUSD of the body of a run.
type PlanStart struct {
	Duty   map[string]any `json:"duty,omitempty"`
	Bridge map[string]any `json:"bridge,omitempty"`
	Run    *PlanRun       `json:"run,omitempty"`
}

// PlanRun is the start of one run: the member that the page names in
// POST /v1/runs?member=, and the body.
type PlanRun struct {
	Member string         `json:"member"`
	Body   map[string]any `json:"body"`
}

// Candidate is an agent that the person can run, as the plan sees it: its
// name in words, its description, its tools, the patterns of its policy
// that ask a person, the egress hosts of its pool (Egress true when the
// pool says them), the member that a run of it starts on, and the costs
// of its runs of the last 30 days that report one.
type Candidate struct {
	Agent       string
	Name        string
	Description string
	Tools       []string
	Ask         []string
	Hosts       []string
	Egress      bool
	Member      string
	Costs       []float64
}

// PlanInput is everything that MakePlan reads: the request, the time zone
// of the person, the moment, the agents that the person can run, the
// agent of the quick chat with its member, the names of the duties that
// exist, and the duties that the person owns.
type PlanInput struct {
	PlanRequest
	Zone       string
	Now        time.Time
	Agents     []Candidate
	Chat       string
	ChatMember string
	Taken      map[string]bool
	Own        []duties.Duty
}

// The patterns of the words. They read plain English and no syntax.
var (
	reQuestion = regexp.MustCompile(`^(what|how|why|who|which|when|where|explain|is|are|can|does|do)\b|\?$`)
	reReview   = regexp.MustCompile(`\b(review\w*|proofread\w*|check (?:my|the) draft)\b`)
	reSignoff  = regexp.MustCompile(`\b(before it goes out|before it is sent|show me|let me (?:see|check)|my ok|my approval|check (?:it|each|every) (?:first|result))\b`)
	reTeam     = regexp.MustCompile(`\b(split|across|in parallel|several agents|\d+ agents|two agents|three agents|four agents|five agents|team)\b`)
	reFound    = regexp.MustCompile(`\b(drift\w*|only when|watch|alert|anything (?:new|changed)|something (?:changed|drifted))\b`)
	reCount    = regexp.MustCompile(`\b(\d+|two|three|four|five|several) agents\b`)
	reEmail    = regexp.MustCompile(`\be-?mail\w*\b`)
	reSend     = regexp.MustCompile(`\b(send|sends|sent|goes out|go out|mail it)\b`)
	reURL      = regexp.MustCompile(`https?://([^/\s?#]+)`)
	reHost     = regexp.MustCompile(`\b((?:[a-z0-9-]+\.)+(?:com|org|net|io|dev|ai|co|uk|de|gov|edu|app))\b`)
	reStart    = regexp.MustCompile(`\b(every|on the|weekdays|hourly)\b`)
	reSlug     = regexp.MustCompile(`[^a-z0-9]+`)
	reDayPart  = regexp.MustCompile(`^every (night|morning|evening)\b`)
)

// stopWords never pick an agent.
var stopWords = map[string]bool{
	"a": true, "an": true, "the": true, "and": true, "or": true, "of": true, "to": true, "in": true, "on": true, "for": true,
	"with": true, "from": true, "at": true, "by": true, "it": true, "its": true, "is": true, "are": true, "be": true, "me": true,
	"my": true, "our": true, "we": true, "you": true, "your": true, "this": true, "that": true, "these": true, "each": true,
	"every": true, "all": true, "any": true, "what": true, "how": true, "please": true, "can": true, "do": true, "does": true,
	"agent": true, "agents": true, "about": true, "into": true, "then": true, "when": true, "new": true, "one": true,
	"day": true, "week": true, "month": true, "morning": true, "night": true, "evening": true, "am": true, "pm": true,
	"tell": true, "show": true, "make": true, "get": true, "let": true, "them": true, "they": true, "so": true, "as": true,
}

// MakePlan reads the words into a plan.
func MakePlan(in PlanInput) Plan {
	words := strings.Join(strings.Fields(in.Words), " ")
	w := strings.ToLower(words)
	p := Plan{Words: words, Who: []PlanWho{}, Others: []PlanAgent{}, Asks: []string{}, Quiet: []string{}, Gaps: []string{}, Problems: []string{}}
	byName := map[string]Candidate{}
	for _, c := range in.Agents {
		byName[c.Agent] = c
		if c.Agent != in.Chat {
			p.Others = append(p.Others, PlanAgent{Agent: c.Agent, Name: c.Name, Description: c.Description})
		}
	}
	when, said := whenOf(words, in.Zone, in.Now)
	review := reReview.MatchString(w)
	signoff := reSignoff.MatchString(w)
	split := reTeam.MatchString(w)
	question := when == nil && !split && !review && reQuestion.MatchString(w)

	agent := ""
	if a := strings.TrimSpace(in.Agent); a != "" {
		if _, ok := byName[a]; ok {
			agent = a
		} else {
			p.Problems = append(p.Problems, fmt.Sprintf("agent: You cannot run an agent named %s. Pick one of the list.", a))
		}
	}
	if agent == "" && question && in.Chat != "" {
		agent = in.Chat
	}
	if agent == "" && in.Agent == "" {
		agent = bestAgent(w, in.Agents, in.Chat)
	}
	reviewer := ""
	if review || split {
		reviewer = reviewerOf(in.Agents, agent, in.Chat)
	}
	switch {
	case when != nil:
		p.Kind = PlanRepeat
	case split || (review && agent != "" && reviewer != ""):
		p.Kind = PlanTeam
	default:
		p.Kind = PlanOnce
	}
	p.Title = titleWords(words, said)

	// Who does it.
	if agent != "" {
		c := byName[agent]
		does := "does it"
		if p.Kind == PlanTeam && split {
			does = "takes a share"
		}
		p.Who = append(p.Who, PlanWho{Agent: agent, Name: c.Name, Does: does})
	} else if len(p.Problems) == 0 {
		p.Problems = append(p.Problems, "agent: No agent fits these words. Pick one.")
	}
	var seats []map[string]any
	if p.Kind == PlanTeam && agent != "" {
		if split {
			n := seatCount(w)
			p.Who[0].Count = n
			for i := 1; i <= n; i++ {
				seats = append(seats, map[string]any{"handle": fmt.Sprintf("%s-%d", agent, i), "agent": agent,
					"task": fmt.Sprintf("Take part %d of %d of the work. Say in the channel which part you take before you start.", i, n)})
			}
		} else {
			seats = append(seats, map[string]any{"handle": agent, "agent": agent, "task": words})
		}
		if reviewer != "" {
			task := "Review each finished part against its source, and say what must change."
			if !split {
				task = "Review the draft of " + agent + ". Say what must change, and say when it is ready."
			}
			seats = append(seats, map[string]any{"handle": reviewer, "agent": reviewer, "task": task})
			p.Who = append(p.Who, PlanWho{Agent: reviewer, Name: byName[reviewer].Name, Does: "checks it"})
		}
	}

	// When.
	if when != nil {
		p.When = PlanWhen{Said: said, Readback: when.Readback(), Next: []time.Time{}}
		if plan, err := when.Plan(); err == nil {
			p.When.Next = plan.Next(in.Now, 3)
		}
	} else {
		p.When = PlanWhen{Readback: "Now, once.", Next: []time.Time{}}
	}

	// What it costs: the runs of the agent, never a limit.
	p.Cost = PlanCost{Per: "run", RunsPerMonth: 1}
	if p.Kind == PlanTeam {
		p.Cost.Per = "bridge"
	}
	if when != nil {
		p.Cost.RunsPerMonth = perMonth(*when)
	}
	if agent != "" {
		c := byName[agent]
		p.Cost.Low, p.Cost.High, p.Cost.Runs = taskCost(p.Who, byName)
		if len(c.Costs) > 0 {
			low, high := slices.Min(c.Costs), slices.Max(c.Costs)
			switch {
			case high < halfCent:
				// A cheap model costs less than a cent a run, and "$0.00
				// to $0.00" reads as no cost.
				p.Cost.Basis = fmt.Sprintf("Runs of %s cost under $0.01 each in the last 30 days.", c.Name)
			case money(low) == money(high):
				p.Cost.Basis = fmt.Sprintf("Runs of %s cost %s each in the last 30 days.", c.Name, money(high))
			default:
				p.Cost.Basis = fmt.Sprintf("Runs of %s cost %s to %s in the last 30 days.", c.Name, money(low), money(high))
			}
		} else {
			p.Cost.Basis = fmt.Sprintf("No run of %s in the last 30 days reports a cost.", c.Name)
		}
		if p.Kind == PlanTeam {
			if p.Cost.High > 0 {
				p.Cost.Basis += " " + spanWords("All its agents together", p.Cost.Low, p.Cost.High)
			}
			p.Cost.Basis += " The channel stops at your limit."
		}
	}

	// When it asks, and when it stays quiet.
	for _, who := range p.Who {
		c := byName[who.Agent]
		for _, a := range asksOf(c.Ask, c.Tools) {
			if !slices.Contains(p.Asks, a) {
				p.Asks = append(p.Asks, a)
			}
		}
	}
	if signoff {
		p.Asks = append(p.Asks, "Before it goes out: you check the result and say it is right")
	}
	p.Asks = append(p.Asks, "When it is not sure what you mean. The question waits for you")
	p.Quiet = append(p.Quiet, "While it works")
	if p.Kind == PlanTeam {
		p.Quiet = append(p.Quiet, "While the agents pass the work between them")
	}
	switch {
	case slices.Contains([]string{TellEach, TellFound, TellFailures}, in.Tell):
		p.Tell = in.Tell
	case p.Kind == PlanRepeat && reFound.MatchString(w):
		p.Tell = TellFound
	default:
		p.Tell = TellEach
	}
	switch p.Tell {
	case TellFound:
		p.Quiet = append(p.Quiet, "When it finds nothing: one line in Done, and no message")
	case TellFailures:
		p.Quiet = append(p.Quiet, "When a run works: its result waits in Done")
	}

	// Where the result lands.
	base := cmp.Or(agent, "tasks") + "/" + slugOf(p.Title)
	switch {
	case p.Kind == PlanRepeat:
		p.Result = "A page in the wiki for each run, and a card in Done"
	case p.Kind == PlanTeam:
		p.Result = "A report page in the wiki when the work ends, and a card in Done"
	case agent != "" && agent == in.Chat:
		p.Result = "The answer, in Chat"
	default:
		p.Result = "The answer, in Done"
	}
	if signoff && p.Kind != PlanTeam {
		p.Result += ", which waits for your check"
	}

	// What no agent here can do for these words, said before the start.
	p.Gaps = gapsOf(w, agent, byName)
	if p.Kind == PlanRepeat {
		p.Same = sameDuty(words, said, agent, in.Own)
	}

	// The record that Start sends.
	switch {
	case agent == "":
	case p.Kind == PlanRepeat:
		name := slugOf(p.Title)
		for i := 2; in.Taken[name]; i++ {
			name = strings.TrimRight(trimSlug(slugOf(p.Title), 36), "-") + fmt.Sprintf("-%d", i)
		}
		result := []string{"owner"}
		if p.Tell == TellFailures {
			result = []string{}
		}
		resultWhen := "always"
		if p.Tell == TellFound {
			resultWhen = TellFound
		}
		// The default of every duty while the person is away: nobody
		// decides, and after the wait the answer is no. New duty starts
		// from the same default (plan A9).
		away := duties.WhileAway{Decider: duties.DefaultDecider, Timeout: duties.AwayFor(), Then: duties.DefaultThen}
		p.Away = duties.AwayWords(&away)
		duty := map[string]any{
			"name":      name,
			"title":     p.Title,
			"agent":     agent,
			"when":      map[string]any{"schedule": *when},
			"do":        map[string]any{"ask": map[string]any{"message": words + doorSuffix + base + "/{date}.md. When you find nothing, say so in one line."}},
			"limits":    map[string]any{"budget": map[string]any{}, "overlap": "skip", "catchUp": "6h"},
			"whileAway": map[string]any{"decider": away.Decider, "timeout": away.Timeout, "then": away.Then},
			"notify":    map[string]any{"failure": []string{"owner"}, "needsYou": []string{"owner"}, "result": result, "resultWhen": resultWhen},
		}
		if signoff {
			duty["review"] = true
		}
		p.Start.Duty = duty
		p.Open = "#duties/" + name
	case p.Kind == PlanTeam:
		p.Start.Bridge = map[string]any{"title": p.Title, "goal": words, "placement": "queue", "members": seats,
			"caps": map[string]any{"maxTurns": 400}, "delegation": map[string]any{"report": true}}
	default:
		md := map[string]string{"fw.view": "task"}
		member := byName[agent].Member
		if agent == in.Chat {
			md["fw.view"] = "quickchat"
			member = cmp.Or(in.ChatMember, member)
		}
		if signoff {
			md[fleet.MetaReview] = "1"
		}
		p.Start.Run = &PlanRun{Member: member, Body: map[string]any{"agent": agent, "input": words, "title": p.Title, "metadata": md}}
		if member == "" {
			p.Problems = append(p.Problems, fmt.Sprintf("agent: Nothing in the fleet runs %s now.", byName[agent].Name))
		}
	}
	return p
}

// taskCost is the least and the most that the whole task cost in the last
// 30 days: the sum over the agents of the plan, each as many times as its
// seats, of the least and the most of its runs. An agent without a run
// with a cost adds nothing, and with none, both are 0. runs counts the
// runs with a cost of the agents.
func taskCost(who []PlanWho, byName map[string]Candidate) (low, high float64, runs int) {
	for _, w := range who {
		costs := byName[w.Agent].Costs
		if len(costs) == 0 {
			continue
		}
		n := float64(max(w.Count, 1))
		low += n * slices.Min(costs)
		high += n * slices.Max(costs)
		runs += len(costs)
	}
	return math.Round(low*1e4) / 1e4, math.Round(high*1e4) / 1e4, runs
}

// spanWords says what something costs, from low to high, in words.
func spanWords(what string, low, high float64) string {
	switch {
	case high < halfCent:
		return what + " cost under $0.01."
	case money(low) == money(high):
		return what + " cost about " + money(high) + "."
	}
	return what + " cost " + money(low) + " to " + money(high) + "."
}

// doorSuffix starts the part of the message of a duty that the door adds
// to the words of the person (MakePlan).
const doorSuffix = "\n\nWrite the result to the wiki page "

// sameDuty returns a duty of the person that does the work of the words
// already, or nil: a duty of the same agent whose title and message share
// most of the words of the task, without the words of a schedule. At
// least two words must match, and at least three in five of the words of
// the task, or of the duty when it has fewer. The duty with the most of
// them wins, then the name that comes first.
func sameDuty(words, said, agent string, own []duties.Duty) *PlanSame {
	want := workStems(strings.Replace(strings.ToLower(words), said, " ", 1))
	if len(want) < 2 {
		return nil
	}
	var best *PlanSame
	bestScore := 0.0
	list := slices.Clone(own)
	sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
	for _, d := range list {
		if agent != "" && d.Agent != agent {
			continue
		}
		msg, _, _ := strings.Cut(d.Do.Message(), doorSuffix)
		have := workStems(d.Title + " " + strings.ReplaceAll(d.Name, "-", " ") + " " + msg)
		shared := 0
		for s := range want {
			if have[s] {
				shared++
			}
		}
		if len(have) == 0 {
			continue
		}
		score := float64(shared) / float64(min(len(want), len(have)))
		if shared >= 2 && score >= 0.6 && score > bestScore {
			best, bestScore = &PlanSame{Duty: d.Name, Title: cmp.Or(strings.TrimSpace(d.Title), d.Name)}, score
		}
	}
	return best
}

// workStems are the stems of a text without the words of a schedule and
// the words with a digit, such as 9am, with the verbs of sameAction
// folded, so "sum up" and "summarize" meet.
func workStems(text string) map[string]bool {
	out := map[string]bool{}
	for s := range stems(text) {
		if scheduleWords[s] || dayWords[s] || strings.ContainsAny(s, "0123456789") {
			continue
		}
		out[actionStem(s)] = true
	}
	return out
}

// dayWords are the words of a schedule that stems keeps: the days as
// stems drops their plural, and the words around a time.
var dayWords = map[string]bool{"monday": true, "tuesday": true, "wednesday": true, "thursday": true, "friday": true, "saturday": true, "sunday": true,
	"weekday": true, "hour": true, "minute": true, "first": true, "last": true, "weekly": true, "daily": true, "nightly": true}

// bestAgent picks the agent whose name and description share the most
// words with the words of the person, or "" when none shares one. A word
// of the name counts three times. The action of the person, the first word
// that is no stop word, counts four times more when it is the action of
// the agent: the first word of its description, or its name. So "Draft an
// email" picks the agent that writes, not the agent that reviews drafts. A
// tie goes to the name that comes first.
func bestAgent(w string, agents []Candidate, chat string) string {
	want := stems(w)
	var act string
	if a := actionsOf(w); len(a) > 0 {
		act = a[0]
	}
	best, bestScore := "", 0
	list := slices.Clone(agents)
	sort.Slice(list, func(i, j int) bool { return list[i].Agent < list[j].Agent })
	for _, c := range list {
		if c.Agent == chat {
			continue
		}
		name := stems(strings.NewReplacer("-", " ", "_", " ").Replace(c.Agent) + " " + c.Name)
		desc := stems(c.Description)
		score := 0
		if act != "" && (slices.Contains(actionsOf(c.Description), act) || name[act]) {
			score += 4
		}
		for s := range want {
			switch {
			case name[s]:
				score += 3
			case desc[s]:
				score++
			}
		}
		if score > bestScore {
			best, bestScore = c.Agent, score
		}
	}
	return best
}

// reviewerOf names an agent that reviews, other than the agent of the
// task: its name or its description speaks of a review.
func reviewerOf(agents []Candidate, agent, chat string) string {
	list := slices.Clone(agents)
	sort.Slice(list, func(i, j int) bool { return list[i].Agent < list[j].Agent })
	for _, c := range list {
		if c.Agent == agent || c.Agent == chat {
			continue
		}
		if strings.Contains(strings.ToLower(c.Agent), "review") {
			return c.Agent
		}
	}
	for _, c := range list {
		if c.Agent != agent && c.Agent != chat && reReview.MatchString(strings.ToLower(c.Description)) {
			return c.Agent
		}
	}
	return ""
}

// sameAction folds the verbs that name one action into one stem.
var sameAction = map[string]string{
	"draft": "write", "compose": "write", "author": "write", "writer": "write",
	"summarize": "sum", "summarise": "sum", "recap": "sum",
	"proofread": "review", "reviewer": "review", "summary": "sum",
}

// actionsOf are the actions of a text: the stem of its first word that is
// no stop word and no word of a schedule, and the word after an "and" that
// follows it, as in "Finds and sums up", with the verbs of sameAction
// folded.
func actionsOf(text string) []string {
	var out []string
	fields := strings.FieldsFunc(strings.ToLower(text), func(r rune) bool { return !unicode.IsLetter(r) })
	for i, f := range fields {
		if len(f) < 3 || stopWords[f] || scheduleWords[f] {
			continue
		}
		out = append(out, actionStem(f))
		if i+2 < len(fields) && fields[i+1] == "and" {
			out = append(out, actionStem(fields[i+2]))
		}
		break
	}
	return out
}

func actionStem(f string) string {
	if v, ok := sameAction[f]; ok {
		return v
	}
	s := stem(f)
	if v, ok := sameAction[s]; ok {
		return v
	}
	return s
}

// scheduleWords open words that name a schedule, not an action.
var scheduleWords = map[string]bool{"nightly": true, "daily": true, "weekly": true, "monthly": true, "hourly": true,
	"monday": true, "tuesday": true, "wednesday": true, "thursday": true, "friday": true, "saturday": true, "sunday": true,
	"weekday": true, "weekdays": true, "tonight": true, "today": true, "tomorrow": true, "now": true, "once": true}

// stems are the words of a text in lower case, without stop words and
// without the ends s, es, ing, and ed of a long word.
func stems(text string) map[string]bool {
	out := map[string]bool{}
	for _, f := range strings.FieldsFunc(strings.ToLower(text), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }) {
		if len(f) < 3 || stopWords[f] {
			continue
		}
		out[stem(f)] = true
	}
	return out
}

func stem(s string) string {
	switch {
	case len(s) > 6 && strings.HasSuffix(s, "ies"):
		return strings.TrimSuffix(s, "ies") + "y"
	case len(s) > 6 && strings.HasSuffix(s, "ing"):
		return strings.TrimSuffix(s, "ing")
	case len(s) > 5 && strings.HasSuffix(s, "ed"):
		return strings.TrimSuffix(s, "ed")
	case len(s) > 5 && strings.HasSuffix(s, "es") && sibilant(strings.TrimSuffix(s, "es")):
		// passes, boxes, matches: the e belongs to the end, so writes
		// stays write.
		return strings.TrimSuffix(s, "es")
	case len(s) > 3 && strings.HasSuffix(s, "s") && !strings.HasSuffix(s, "ss"):
		return strings.TrimSuffix(s, "s")
	}
	return s
}

// sibilant reports whether a word ends in a sound that takes es for its
// plural: s, x, z, ch, or sh.
func sibilant(s string) bool {
	for _, end := range []string{"s", "x", "z", "ch", "sh"} {
		if strings.HasSuffix(s, end) {
			return true
		}
	}
	return false
}

// whenOf finds a schedule in the words with the grammar of the duties, or
// nil. Each, nightly, weekly, monthly, and daily read as every. Night is 2
// AM, morning 9 AM, and evening 6 PM, unless the words name a time. A week
// is Monday, and a month is its first day. said is the part of the words
// that the schedule reads.
func whenOf(words, zone string, now time.Time) (*schedule.Schedule, string) {
	t := " " + strings.ToLower(words) + " "
	for _, r := range [][2]string{{" each ", " every "}, {" nightly ", " every night "}, {" weekly ", " every week "}, {" monthly ", " every month "}, {" daily ", " every day "}} {
		t = strings.ReplaceAll(t, r[0], r[1])
	}
	for _, at := range reStart.FindAllStringIndex(t, -1) {
		rest := t[at[0]:]
		if i := strings.IndexAny(rest, ",.;:!?"); i >= 0 {
			rest = rest[:i]
		}
		fields := strings.Fields(rest)
		for n := len(fields); n >= 1; n-- {
			said := strings.Join(fields[:n], " ")
			phrase := said
			if m := reDayPart.FindStringSubmatch(phrase); m != nil {
				phrase = strings.Replace(phrase, m[0], "every day", 1)
				if !strings.Contains(phrase, " at ") {
					phrase += map[string]string{"night": " at 2am", "morning": " at 9am", "evening": " at 6pm"}[m[1]]
				}
			}
			if strings.HasPrefix(phrase, "every week") {
				phrase = "every monday" + strings.TrimPrefix(phrase, "every week")
			}
			if strings.HasPrefix(phrase, "every month") {
				phrase = "on the first of the month" + strings.TrimPrefix(phrase, "every month")
			}
			s, err := schedule.Parse(phrase, schedule.Options{Zone: zone, Now: now})
			if err == nil {
				return &s, said
			}
		}
	}
	return nil, ""
}

// perMonth is about how many runs a schedule starts in 30 days.
func perMonth(s schedule.Schedule) int {
	times := max(len(s.Times), 1)
	days := len(s.Days)
	if days == 0 {
		days = 7
	}
	span := 24.0
	if len(s.Between) == 2 {
		a, errA := time.Parse("15:04", s.Between[0])
		b, errB := time.Parse("15:04", s.Between[1])
		if errA == nil && errB == nil && b.After(a) {
			span = b.Sub(a).Hours()
		}
	}
	switch s.Repeat {
	case "daily":
		return 30 * times
	case "weekdays":
		return 22 * times
	case "weekly":
		return int(math.Round(4.3 * float64(days*times) / float64(max(s.EveryWeeks, 1))))
	case "monthly":
		return times
	case "hourly":
		return int(math.Round(span * 4.3 * float64(days)))
	}
	every, err := time.ParseDuration(s.Every)
	if err != nil || every <= 0 {
		every = time.Hour
	}
	return int(math.Round(span * 60 / every.Minutes() * 4.3 * float64(days)))
}

// asksOf words the patterns of a policy that ask a person. With tools,
// the tools that the agent can call, it leaves out a pattern of a tool
// that the agent lacks, so the readback names no ask that cannot happen.
// A tool reads as what its call does (toolWords), as in "Before it writes
// a page of the wiki" for front.pages_write, and never by its name. A
// pattern with an argument of bash names the command, as in "Before it
// runs git push", and of another tool adds the argument, as in "Before it
// calls playbook in ansible, with check=false".
func asksOf(patterns, tools []string) []string {
	var out []string
	for _, p := range patterns {
		p = strings.TrimSpace(p)
		tool, arg, hasArg := strings.Cut(p, ":")
		tool = strings.TrimSpace(tool)
		switch {
		case p == "":
		case p == "destructive":
			out = append(out, "Before a step that changes or deletes something")
		case p == "*":
			out = append(out, "Before each step")
		case !mayCall(tools, tool):
		case hasArg:
			arg = strings.TrimSpace(strings.TrimRight(strings.TrimSpace(arg), "*"))
			switch {
			case arg == "":
				out = append(out, "Before it "+toolWords(tool))
			case tool == "bash":
				out = append(out, "Before it runs "+arg)
			default:
				out = append(out, "Before it "+toolWords(tool)+", with "+arg)
			}
		case strings.HasSuffix(p, ".*"):
			out = append(out, "Before it uses a tool of "+serverWords(strings.TrimSuffix(p, ".*")))
		default:
			out = append(out, "Before it "+toolWords(strings.TrimRight(p, "*")))
		}
	}
	return out
}

// mayCall reports whether an agent with the tools may call the tool of a
// pattern of its policy. A pattern of a tool can be a glob, such as
// ansible.*, and so can an entry of the tools. No tools means every tool.
func mayCall(tools []string, tool string) bool {
	if len(tools) == 0 {
		return true
	}
	for _, t := range tools {
		if t == tool || t == "*" {
			return true
		}
		if ok, _ := path.Match(t, tool); ok {
			return true
		}
		if ok, _ := path.Match(tool, t); ok {
			return true
		}
	}
	return false
}

// gapsOf says what the agent cannot do for the words: send an email
// without a tool for it, or reach a host that the egress of its pool does
// not allow.
func gapsOf(w, agent string, byName map[string]Candidate) []string {
	gaps := []string{}
	c, ok := byName[agent]
	if reEmail.MatchString(w) && reSend.MatchString(w) {
		mail := false
		for _, t := range c.Tools {
			mail = mail || strings.Contains(strings.ToLower(t), "mail")
		}
		if !mail {
			gaps = append(gaps, "No agent here can send email. After your OK, you send it.")
		}
	}
	if !ok || !c.Egress {
		return gaps
	}
	seen := map[string]bool{}
	var hosts []string
	for _, m := range reURL.FindAllStringSubmatch(w, -1) {
		hosts = append(hosts, m[1])
	}
	for _, m := range reHost.FindAllStringSubmatch(w, -1) {
		hosts = append(hosts, m[1])
	}
	for _, h := range hosts {
		h = strings.TrimSuffix(strings.Split(h, ":")[0], ".")
		if seen[h] {
			continue
		}
		seen[h] = true
		if !reaches(c.Hosts, h) {
			gaps = append(gaps, fmt.Sprintf("%s cannot reach %s: it reaches only the hosts that the server allows. The person who runs the server adds a host.", c.Name, h))
		}
	}
	return gaps
}

// reaches reports whether an egress host of a pool matches a host: the
// same name, or a pattern such as *.example.com, without the port.
func reaches(allowed []string, host string) bool {
	for _, a := range allowed {
		a = strings.ToLower(strings.TrimSpace(a))
		if i := strings.LastIndexByte(a, ':'); i > 0 && !strings.Contains(a[i:], "*") {
			a = a[:i]
		}
		if a == host {
			return true
		}
		if ok, _ := path.Match(a, host); ok {
			return true
		}
	}
	return false
}

// seatCount is the number of seats that the words ask for, 2 to 6, and 3
// for "several" or no number.
func seatCount(w string) int {
	m := reCount.FindStringSubmatch(w)
	if m == nil {
		return 3
	}
	switch m[1] {
	case "two":
		return 2
	case "three", "several":
		return 3
	case "four":
		return 4
	case "five":
		return 5
	}
	n := 0
	fmt.Sscanf(m[1], "%d", &n)
	return min(max(n, 2), 6)
}

// titleWords makes a short title of the words: the first clause, without
// the schedule, with a capital, at most 64 bytes.
func titleWords(words, said string) string {
	t := words
	if said != "" {
		if i := strings.Index(strings.ToLower(t), said); i >= 0 {
			t = t[:i] + t[i+len(said):]
		}
	}
	for _, x := range []string{"every night", "every morning", "every evening", "every week", "every month", "each night", "each morning", "each week", "nightly", "weekly", "monthly", "daily"} {
		if i := strings.Index(strings.ToLower(t), x); i >= 0 {
			t = t[:i] + t[i+len(x):]
		}
	}
	t = strings.Join(strings.Fields(t), " ")
	t = strings.TrimLeft(t, " ,.;:-")
	if i := sentenceEnd(t); i >= 0 {
		t = t[:i]
	}
	for _, cut := range []string{", and then", ", then", ", tell", ", and tell", ", show"} {
		if i := strings.Index(strings.ToLower(t), cut); i >= 0 {
			t = t[:i]
		}
	}
	t = strings.TrimRight(t, " ,")
	if len(t) > 64 {
		t = t[:64]
		if i := strings.LastIndexByte(t, ' '); i > 0 {
			t = t[:i]
		}
	}
	if t == "" {
		return "A task"
	}
	return strings.ToUpper(t[:1]) + t[1:]
}

// sentenceEnd finds the end of the first sentence of t: a semicolon, or a
// period, a question mark, or an exclamation mark at the end or before a
// space. A period inside a word, as in "3.0" or "example.com", ends nothing.
// It returns -1 for one sentence.
func sentenceEnd(t string) int {
	for i := 0; i < len(t); i++ {
		switch t[i] {
		case ';':
			return i
		case '.', '?', '!':
			if i+1 == len(t) || t[i+1] == ' ' {
				return i
			}
		}
	}
	return -1
}

// slugOf makes the name of a duty from words: a-z, 0-9, and hyphens, at
// most 40 characters.
func slugOf(s string) string {
	out := strings.Trim(reSlug.ReplaceAllString(strings.ToLower(s), "-"), "-")
	out = strings.TrimRight(trimSlug(out, 40), "-")
	if out == "" {
		return "task"
	}
	return out
}

func trimSlug(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// halfCent is the smallest cost that reads as a cent.
const halfCent = 0.005

// money words an amount in US dollars to the cent: a cost above zero and
// below a cent is "under $0.01", as on Results (value.Dollars).
func money(v float64) string {
	if v > 0 && v < halfCent {
		return "under $0.01"
	}
	return fmt.Sprintf("$%.2f", v)
}

// PlanFor makes the plan of the words of the caller in ctx, at the time
// now, in the time zone zone (the header X-Time-Zone, UTC when empty). The
// agents are the profiles that the caller can run: the profiles of the
// pools and of this process, without the manager. It starts and saves
// nothing.
func PlanFor(ctx context.Context, a *fw.Agent, now time.Time, zone string, req PlanRequest) (Plan, error) {
	req.Words = strings.TrimSpace(req.Words)
	switch {
	case req.Words == "":
		return Plan{}, fmt.Errorf("%w: words: Say what you need done", fw.ErrInvalidRequest)
	case len(req.Words) > MaxWords || !utf8.ValidString(req.Words):
		return Plan{}, fmt.Errorf("%w: words: Say it in at most %d characters", fw.ErrInvalidRequest, MaxWords)
	case req.Tell != "" && !slices.Contains([]string{TellEach, TellFound, TellFailures}, req.Tell):
		return Plan{}, fmt.Errorf("%w: tell is %q, not %s, %s, or %s", fw.ErrInvalidRequest, req.Tell, TellEach, TellFound, TellFailures)
	}
	zone = strings.TrimSpace(zone)
	if zone == "" {
		zone = "UTC"
	}
	if _, err := time.LoadLocation(zone); err != nil {
		return Plan{}, fmt.Errorf("%w: the time zone %q is not an IANA name, such as America/New_York", fw.ErrInvalidRequest, zone)
	}
	in := PlanInput{PlanRequest: req, Zone: zone, Now: now, Taken: map[string]bool{}}
	bc, err := bossmode.Load(a.Config())
	if err != nil {
		return Plan{}, err
	}
	if qc, err := bossmode.QuickChatOf(ctx, a); err != nil {
		return Plan{}, err
	} else if qc != nil {
		in.Chat, in.ChatMember = qc.Agent, qc.Member
	}
	costs := map[string][]float64{}
	recent, err := fleet.WorkSince(ctx, a, now.Add(-30*24*time.Hour))
	if err != nil {
		return Plan{}, err
	}
	for _, w := range recent {
		if r := w.Run; r != nil && r.Parent == "" && r.Usage.CostUSD > 0 {
			costs[r.Agent] = append(costs[r.Agent], r.Usage.CostUSD)
		}
	}
	pooled, _, err := fleet.PoolAgents(ctx, a)
	if err != nil {
		return Plan{}, err
	}
	own, err := view.Agents(ctx, a)
	if err != nil {
		return Plan{}, err
	}
	agents := slices.Clone(pooled)
	for _, v := range own {
		agents = append(agents, fleet.PoolAgent{AgentView: v})
	}
	fs := faces.Of(a)
	seen := map[string]bool{}
	for _, v := range agents {
		name := v.Name
		if seen[name] || name == bc.Manager || (v.Role != fw.AccessOwner.String() && v.Role != fw.AccessOperator.String()) {
			continue
		}
		seen[name] = true
		// The tools of the profile, or the tools of its workers for a
		// profile that names none: the tools that its runs can call.
		tools := v.Tools
		if len(tools) == 0 {
			tools = v.WorkerTools
		}
		c := Candidate{Agent: name, Name: name, Description: v.Description, Tools: slices.Clone(tools), Costs: costs[name]}
		if fs != nil {
			if f, err := fs.Get(ctx, name); err == nil && strings.TrimSpace(f.DisplayName) != "" {
				c.Name = f.DisplayName
			}
		}
		c.Ask = askPatterns(v.Policy, v.WorkerPolicy)
		if v.Pool != "" {
			c.Hosts, c.Egress = fleet.PoolEgress(ctx, a, v.Pool)
		}
		if c.Member, err = bossmode.MemberFor(ctx, a, name); err != nil {
			return Plan{}, err
		}
		in.Agents = append(in.Agents, c)
	}
	if svc := duties.Of(a); svc != nil {
		recs, err := svc.Records(ctx)
		if err != nil {
			return Plan{}, err
		}
		me := ""
		if p := fw.PrincipalFrom(ctx); p != nil {
			me = p.Subject
		}
		for _, r := range recs {
			in.Taken[r.Duty.Name] = true
			if me != "" && r.Duty.Owner == me {
				in.Own = append(in.Own, r.Duty)
			}
		}
	}
	return MakePlan(in), nil
}

// askPatterns lists the patterns of the policies that ask a person, with
// the clauses of a conjunction.
func askPatterns(rules ...*fw.Rules) []string {
	var out []string
	var walk func(r fw.Rules)
	walk = func(r fw.Rules) {
		for _, p := range r.Ask {
			if !slices.Contains(out, p) {
				out = append(out, p)
			}
		}
		for _, c := range r.All {
			walk(c)
		}
	}
	for _, r := range rules {
		if r != nil {
			walk(*r)
		}
	}
	return out
}
