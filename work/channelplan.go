package work

// This file is the plan of Make this a job: the work of a channel that
// runs again on a schedule. The job posts the words in the channel to one
// agent of the channel, which the person picks, so the agent does the work
// where it did it before, and its answer, the result, is a post in that
// channel. A job that posts spends nothing of its own: the agent spends
// from its budget in the channel (duties, do.post). Like MakePlan, the plan
// is a pure function of its input.

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/bridges"
)

// PlanChannel is the channel of a plan with a bridge: its id, its title,
// whether it is open, and its agents that are not taken off.
type PlanChannel struct {
	ID      string
	Title   string
	Open    bool
	Members []PlanMember
}

// PlanMember is an agent of a channel: its handle in the channel, its
// agent, its name in words, and its task.
type PlanMember struct {
	Handle string `json:"handle"`
	Agent  string `json:"agent"`
	Name   string `json:"name"`
	Task   string `json:"task,omitempty"`
}

// planChannelOf reads the channel id as the caller in ctx sees it, with its
// agents named as the candidates name them. A channel that the caller
// cannot see is an invalid request.
func planChannelOf(ctx context.Context, a *fw.Agent, id string, agents []Candidate) (*PlanChannel, error) {
	s := bridges.Of(a)
	if s == nil {
		return nil, fmt.Errorf("%w: bridge: The channels are off on this server", fw.ErrInvalidRequest)
	}
	items, err := s.List(ctx, true)
	if err != nil {
		return nil, err
	}
	names := map[string]string{}
	for _, c := range agents {
		names[c.Agent] = c.Name
	}
	for _, it := range items {
		if it.ID != id {
			continue
		}
		ch := &PlanChannel{ID: it.ID, Title: it.Title, Open: it.State == bridges.StateOpen, Members: []PlanMember{}}
		for _, m := range it.Members {
			if m.Fired {
				continue
			}
			name := names[m.Agent]
			if name == "" {
				name = m.Agent
			}
			ch.Members = append(ch.Members, PlanMember{Handle: m.Handle, Agent: m.Agent, Name: name, Task: firstLine(m.Task)})
		}
		return ch, nil
	}
	return nil, fmt.Errorf("%w: bridge: No channel that you can see has the id %s", fw.ErrInvalidRequest, id)
}

// firstLine returns the first line of a text that holds words.
func firstLine(s string) string {
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			return l
		}
	}
	return ""
}

// noChannelAgent is the problem of a channel without an agent to run a
// job.
const noChannelAgent = "handle: This channel has no agent to run the job. Add an agent to the channel first."

// channelPlan reads the words into the plan of a job that posts in the
// channel of the input. The words say how often, and the rest of the words
// is the message that each run posts. The person picks the agent by its
// handle, and the first agent of the channel runs it when the person
// picks none.
func channelPlan(in PlanInput) Plan {
	ch := in.Channel
	words := strings.Join(strings.Fields(in.Words), " ")
	w := strings.ToLower(words)
	p := Plan{Words: words, Kind: PlanRepeat, Who: []PlanWho{}, Others: []PlanAgent{}, Asks: []string{}, Quiet: []string{}, Gaps: []string{}, Problems: []string{},
		Members: slices.Clone(ch.Members), Tell: TellEach}
	byName := map[string]Candidate{}
	for _, c := range in.Agents {
		byName[c.Agent] = c
	}
	when, said := whenOf(words, in.Zone, in.Now)
	p.Title = titleWords(words, said)
	if when == nil {
		p.Kind = PlanOnce
		p.When = PlanWhen{Readback: "Now, once.", Next: []time.Time{}}
		p.Problems = append(p.Problems, "when: Say how often the job runs, such as every Monday at 9:00.")
	} else {
		p.When = PlanWhen{Said: said, Readback: when.Readback(), Next: []time.Time{}}
		if plan, err := when.Plan(); err == nil {
			p.When.Next = plan.Next(in.Now, 3)
		}
	}
	if !ch.Open {
		p.Problems = append(p.Problems, "bridge: The channel "+ch.Title+" is closed. A job posts in a channel that stays open.")
	}

	// Who runs it: the agent that the person picked, else the first one.
	var m *PlanMember
	handle := strings.TrimPrefix(strings.TrimSpace(in.Handle), "@")
	for i := range ch.Members {
		if handle == "" || ch.Members[i].Handle == handle {
			m = &ch.Members[i]
			break
		}
	}
	switch {
	case len(ch.Members) == 0:
		p.Problems = append(p.Problems, noChannelAgent)
	case m == nil:
		p.Problems = append(p.Problems, fmt.Sprintf("handle: %s is no agent of the channel %s. Pick one of the list.", handle, ch.Title))
	}

	p.Cost = PlanCost{Per: "channel", RunsPerMonth: 1}
	if when != nil {
		p.Cost.RunsPerMonth = perMonth(*when)
	}
	p.Result = "A post in the channel " + ch.Title + " for each run"
	if m == nil {
		return p
	}
	p.Who = append(p.Who, PlanWho{Agent: m.Agent, Name: m.Name, Does: "does it in the channel", Handle: m.Handle})
	c := byName[m.Agent]
	p.Cost.Low, p.Cost.High, p.Cost.Runs = taskCost(p.Who, byName)
	if len(c.Costs) > 0 {
		p.Cost.Basis = spanWords("Runs of "+m.Name+" in the last 30 days", p.Cost.Low, p.Cost.High) + " "
	}
	p.Cost.Basis += "The job spends nothing of its own: " + m.Handle + " spends from its budget in the channel."
	p.Asks = append(p.Asks, asksOf(c.Ask, c.Tools)...)
	p.Asks = append(p.Asks, "When it is not sure what you mean. The question waits for you in the channel")
	p.Quiet = append(p.Quiet, "While it works", "Each result: a post in the channel, and no message")
	p.Gaps = gapsOf(w, m.Agent, byName)

	if when == nil {
		return p
	}
	name := slugOf(p.Title)
	for i := 2; in.Taken[name]; i++ {
		name = strings.TrimRight(trimSlug(slugOf(p.Title), 36), "-") + fmt.Sprintf("-%d", i)
	}
	p.Start.Duty = map[string]any{
		"name":   name,
		"title":  p.Title,
		"agent":  m.Agent,
		"when":   map[string]any{"schedule": *when},
		"do":     map[string]any{"post": map[string]any{"bridge": ch.ID, "to": []string{m.Handle}, "message": jobMessage(words, said)}},
		"limits": map[string]any{"overlap": "skip", "catchUp": "6h"},
		"notify": map[string]any{"failure": []string{"owner"}, "needsYou": []string{"owner"}, "result": []string{}, "resultWhen": "always"},
	}
	p.Open = "#duties/" + name
	return p
}

// jobMessage is the message that each run of a job posts: the words
// without the words of the schedule.
func jobMessage(words, said string) string {
	t := words
	if said != "" {
		if i := strings.Index(strings.ToLower(t), said); i >= 0 {
			t = t[:i] + t[i+len(said):]
		}
	}
	t = strings.Join(strings.Fields(t), " ")
	t = strings.Trim(t, " ,;:-")
	t = strings.TrimSuffix(t, " .")
	if t == "" || t == "." {
		return words
	}
	return t
}
