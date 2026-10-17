package dashboard

// This file is the work view of fw: the home lists, the result of a run,
// the approval labels, the agent builder, and the capability catalog. The
// page, the native API, and the MCP tools build their answers from these
// functions, so a browser, an MCP App, and a host that shows only text say
// the same thing (PRODUCT_DIRECTION.md, "UI direction" and "MCP parity").
//
// CONTRACT: the signatures and the JSON shapes in this file are fixed for
// the surfaces. The helpers of these functions are in result.go.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/surface/view"
)

// Status words of a work item. They come from the run alone, so a list
// needs no ledger read.
const (
	StatusNeedsYou = "needs-you" // the run waits for an approval or an answer
	StatusWorking  = "working"   // queued, running, or suspended
	StatusReady    = "ready"     // completed with an answer or a changed file: a person can review it
	StatusEnded    = "ended"     // completed with nothing to review
	StatusFailed   = "failed"    // failed, for example at a limit
	StatusCanceled = "canceled"  // canceled by a person
)

// StatusOf is the status word of a run. StatusReady needs a non-empty
// output. A model that ends a run does not prove that the work passed its
// checks, so no status claims that.
func StatusOf(r *fw.Run) string {
	if r == nil {
		return ""
	}
	switch r.State {
	case fw.StateInputRequired:
		return StatusNeedsYou
	case fw.StateCompleted:
		if strings.TrimSpace(r.Output) != "" {
			return StatusReady
		}
		return StatusEnded
	case fw.StateFailed:
		return StatusFailed
	case fw.StateCanceled:
		return StatusCanceled
	}
	// Queued, running, suspended, and a state that a newer fleet member
	// names: the run is not finished.
	return StatusWorking
}

// TitleOf is the title of a run: r.Title, else fw.TitleFrom of the input,
// else the agent name. A run of the agent builder is "New agent", unless
// the metadata fw.title names another title. A run of an older fleet
// member carries no Title, so the view makes one.
func TitleOf(r *fw.Run) string {
	if r == nil {
		return ""
	}
	if isBuilder(r) {
		// The projection makes a title from the first message, which for
		// a builder run is the request to the builder, not the work.
		if t := strings.TrimSpace(r.Metadata[fw.MetaTitle]); t != "" {
			return t
		}
		return builderTitle
	}
	if t := strings.TrimSpace(r.Title); t != "" {
		return t
	}
	if t := fw.TitleFrom(r.Input); t != "" {
		return t
	}
	return r.Agent
}

// WorkItem is one run in a home list: the run object, with Title filled
// by TitleOf, and its status word.
type WorkItem struct {
	*fw.Run
	Status string `json:"status"`
}

// HomeRecent is the number of finished runs that Home lists, and
// HomeWindow how far back they reach.
const (
	HomeRecent = 20
	HomeWindow = 7 * 24 * time.Hour
)

// Home is the first screen of every surface. Its lists hold the top-level
// runs of the caller and of the teams of the caller (By ByTeam), newest
// first: NeedsYou the runs that wait for a person, InProgress the runs
// that work, and Recent the finished runs of the last HomeWindow, at most
// HomeRecent. Others counts the top-level runs of other people that the
// caller can see, which the page puts behind one row. Glance counts the
// same runs as the lists, and Sentence is the glance sentence.
type Home struct {
	Sentence   string      `json:"sentence"`
	Glance     view.Glance `json:"glance"`
	NeedsYou   []WorkItem  `json:"needsYou"`
	InProgress []WorkItem  `json:"inProgress"`
	Recent     []WorkItem  `json:"recent"`
	Others     int         `json:"others"`
}

// HomeOf builds the home of the caller in ctx from every run that the
// caller can see (dashboard.Runs with the fleet scope), at the time now.
func HomeOf(ctx context.Context, a *fw.Agent, runs []*fw.Run, now time.Time) (Home, error) {
	top := make([]*fw.Run, 0, len(runs))
	for _, r := range runs {
		if r != nil && r.Parent == "" {
			top = append(top, r)
		}
	}
	team, err := view.By(ctx, a, top, view.ByTeam)
	if err != nil {
		return Home{}, err
	}
	others, err := view.By(ctx, a, top, view.ByOthers)
	if err != nil {
		return Home{}, err
	}
	h := Home{
		Glance:     view.GlanceOf(team, now),
		NeedsYou:   []WorkItem{},
		InProgress: []WorkItem{},
		Recent:     []WorkItem{},
		Others:     len(others),
	}
	h.Sentence = h.Glance.Sentence
	for _, r := range team {
		it := workItem(r)
		switch it.Status {
		case StatusNeedsYou:
			h.NeedsYou = append(h.NeedsYou, it)
		case StatusWorking:
			h.InProgress = append(h.InProgress, it)
		default:
			if now.Sub(r.UpdatedAt) <= HomeWindow {
				h.Recent = append(h.Recent, it)
			}
		}
	}
	for _, list := range [][]WorkItem{h.NeedsYou, h.InProgress, h.Recent} {
		sortByChange(list)
	}
	if len(h.Recent) > HomeRecent {
		h.Recent = h.Recent[:HomeRecent]
	}
	return h, nil
}

// String is the home as text for a host that shows only text: the
// sentence, then each list with one line per run (title, agent, status,
// age, id), then the count of other runs.
func (h Home) String() string {
	var b strings.Builder
	b.WriteString(h.Sentence)
	for _, sec := range []struct {
		title string
		items []WorkItem
	}{{"Needs you", h.NeedsYou}, {"In progress", h.InProgress}, {"Recent results", h.Recent}} {
		if len(sec.items) == 0 {
			continue
		}
		b.WriteString("\n\n" + sec.title + ":")
		for _, it := range sec.items {
			b.WriteString("\n" + workLine(it))
		}
	}
	if h.Others > 0 {
		b.WriteString("\n\n" + count(h.Others, "run", "runs") + " by others.")
	}
	return b.String()
}

// Result is what a run produced and what it needs next, in the order that
// a person reads it: the title and the status, the answer of the agent,
// the files that the run changed, the evidence of the work, the decision
// that waits, and the next actions. It is a projection of the ledger.
//
// Steps are evidence, not a verdict: each tool call that ran, with its
// outcome. Only a recorded outcome appears. Limits are the budget limits
// that affect the run, in plain words, such as "Stopped at the limit of 20
// turns." or "Used 18 of 20 turns.".
type Result struct {
	RunID   string       `json:"runId"`
	Title   string       `json:"title"`
	Agent   string       `json:"agent"`
	Status  string       `json:"status"`
	Summary string       `json:"summary,omitempty"` // the last answer of the agent, at most MaxItemText bytes
	Error   string       `json:"error,omitempty"`
	Files   []ResultFile `json:"files,omitempty"`
	Steps   []ResultStep `json:"steps,omitempty"`
	Pending *Decision    `json:"pending,omitempty"`
	Limits  []string     `json:"limits,omitempty"`
	Next    []NextAction `json:"next,omitempty"`
	Started time.Time    `json:"started"`
	Changed time.Time    `json:"changed"`
	Usage   *fw.RunUsage `json:"usage,omitempty"` // the usage of this run, for the limits beside the work
}

// ResultFile is the net change of the run to one path: the last
// fw.file.changed event of the path. Op is created, edited, removed, or
// renamed. Size and SHA256 describe the content that the run left. A
// reader compares SHA256 with the file now to see that it changed after
// the run.
type ResultFile struct {
	Path    string `json:"path"`
	OldPath string `json:"oldPath,omitempty"`
	Op      string `json:"op"`
	Size    int64  `json:"size,omitempty"`
	SHA256  string `json:"sha256,omitempty"`
	Seq     uint64 `json:"seq"`
}

// ResultStep is one tool call of the run with its recorded outcome. Text
// names the call for a person, such as `Ran "go test ./..."` or "Read
// docs/ARCHITECTURE.md". IsError is the outcome that the tool recorded.
// The newest MaxResultSteps calls appear.
type ResultStep struct {
	Seq     uint64 `json:"seq"`
	Tool    string `json:"tool"`
	Text    string `json:"text"`
	IsError bool   `json:"isError,omitempty"`
	Done    bool   `json:"done"` // a result was recorded
}

// MaxResultSteps bounds Result.Steps.
const MaxResultSteps = 12

// Decision is the approval or the question that a run waits for. Label is
// the text of the button that allows it: ActionLabel of the tool. Question
// is true for a question to a person (the approval has a schema), where
// the answer is not a permission to act. Effect says what allowing does in
// one sentence, when the tool input says it reliably, such as `Runs "git
// push origin main".`.
//
// Connect is set when the run waits for the person to connect a service
// (fw.ConnectRequest). Label is then "Connect <Title>", and Reason is the
// Message of the request. Only the end of the connection allows it. A
// surface offers Connect, which opens the connection, and Skip, which
// denies it, and never Allow.
type Decision struct {
	ID       string             `json:"id"`
	Tool     string             `json:"tool,omitempty"`
	Label    string             `json:"label"`
	Question bool               `json:"question,omitempty"`
	Reason   string             `json:"reason,omitempty"`
	Effect   string             `json:"effect,omitempty"`
	Input    json.RawMessage    `json:"input,omitempty"`
	Schema   json.RawMessage    `json:"schema,omitempty"`
	Connect  *fw.ConnectRequest `json:"connect,omitempty"`
}

// Kinds of a next action.
const (
	NextDecide    = "decide"     // allow or deny the pending approval; Label is the allow label
	NextAnswer    = "answer"     // answer the pending question
	NextConnect   = "connect"    // connect the service that the run waits for, or skip it; Label is "Connect <Title>"
	NextReview    = "review"     // open the changed files
	NextReply     = "reply"      // send a follow-up
	NextSaveAgent = "save-agent" // turn this conversation into an agent
	NextRetry     = "retry"      // start the same request again after a failure
)

// NextAction is one action that the result offers, the most useful first.
type NextAction struct {
	Kind  string `json:"kind"`
	Label string `json:"label"`
}

// ResultOf builds the result of a run for the caller in ctx. It reads the
// run with a.Get, which authorizes the caller, and then folds the whole
// ledger of the run while it streams. It returns the error of a.Get:
// fw.ErrRunNotFound for a run that this process does not know, and
// fw.ErrForbidden for a run that the caller cannot see. A run of another
// fleet member is the job of the surface, which forwards the request to
// the owner.
func ResultOf(ctx context.Context, a *fw.Agent, id string) (Result, error) {
	run, err := a.Get(ctx, id)
	if err != nil {
		return Result{}, err
	}
	specs, err := a.ToolSpecs(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("dashboard: list the tools: %w", err)
	}
	fold := newResultFold(specsByName(specs))
	if err := eachEvent(ctx, a, id, 0, fold.add); err != nil {
		return Result{}, err
	}
	res := fold.result(run)
	if len(res.Limits) == 0 {
		// A run that no limit stopped shows the limits that it nears.
		if budget, err := a.RunBudget(ctx, id); err == nil {
			res.Limits = nearLimits(run.Usage, budget)
		}
	}
	res.Next = nextActions(run, res, canSave(ctx, a, run))
	return res, nil
}

// String is the result as text for a host that shows only text.
func (r Result) String() string {
	return resultText(r)
}

// ActionLabel is the label of the button that allows a call of a tool:
// spec.Action when the tool sets it, else "Allow". A question gets
// "Answer". It never invents a label from a tool name.
func ActionLabel(spec fw.ToolSpec, question bool) string {
	// An answer to a question is not a permission to act, so a question
	// never takes the action label of its tool.
	if question {
		return "Answer"
	}
	if s := strings.TrimSpace(spec.Action); s != "" {
		return s
	}
	return "Allow"
}

// TranscriptBefore reads the page of events that ends before the sequence
// number before: the last limit events with a smaller sequence number.
// prev is the sequence number to pass as before for the page in front of
// it, and 0 when the page starts at the first event. With before 0, it
// reads the last limit events of the run. The caller authorizes the read.
// It keeps limit events in a ring while the ledger streams, so a long
// ledger never sits in memory whole.
func TranscriptBefore(ctx context.Context, a *fw.Agent, id string, before uint64, limit int) (events []fw.Event, prev uint64, err error) {
	if limit <= 0 {
		limit = 50
	}
	var (
		ring []fw.Event // the newest events, the oldest at ring[head] when the ring is full
		head int
		seen int
	)
	for e, rerr := range a.Ledger().Read(ctx, id, 0) {
		if rerr != nil {
			return nil, 0, fmt.Errorf("dashboard: read the ledger of run %s: %w", id, rerr)
		}
		if before > 0 && e.Seq >= before {
			break
		}
		seen++
		if len(ring) < limit {
			ring = append(ring, e)
			continue
		}
		ring[head] = e
		head = (head + 1) % limit
	}
	events = make([]fw.Event, 0, len(ring))
	events = append(events, ring[head:]...)
	events = append(events, ring[:head]...)
	if seen > limit {
		prev = events[0].Seq
	}
	return events, prev, nil
}

// Agent builder (moved from the page, so a host that shows only text can
// use it).

// ViewKey and ViewBuilder label a run of the agent builder in its metadata.
const (
	ViewKey     = "fw.view"
	ViewBuilder = "builder"
)

// BuilderPrompt is the system prompt of a conversation that creates an
// agent. The tool propose_agent holds the method. The person creates the
// agent from the proposal, so the model never saves anything.
const BuilderPrompt = "You help a person create an agent in fw. Talk with the person in short, plain sentences. " +
	"Find out what the agent handles and what it must never do, then call propose_agent. Do not do the work of the agent yourself."

// BuilderRequest is the run request of a conversation that creates an
// agent: the default agent with an inline profile that holds BuilderPrompt
// and the tools propose_agent and ask_user, the metadata ViewKey =
// ViewBuilder, and the title "New agent". input is the first message.
func BuilderRequest(input string) fw.RunRequest {
	return fw.RunRequest{
		Input: input,
		Profile: &fw.AgentProfile{
			System: BuilderPrompt,
			Tools:  []string{"propose_agent", "ask_user"},
		},
		Metadata: map[string]string{ViewKey: ViewBuilder, fw.MetaTitle: builderTitle},
	}
}

// SaveAsAgentInput is the first message of a builder conversation that
// makes an agent from the run id: what the person asked (at most eight
// messages), the last answer of the agent (at most 2000 characters), and
// the tools that the run used. It authorizes the read with a.Get. Then it
// applies the terms of "Save as agent": the caller started the run, the
// run is a completed top-level run, and the caller can create agents.
// Otherwise it returns fw.ErrForbidden with the reason. A view of the run
// is not enough, because the builder run belongs to the caller and to the
// teams of the caller.
func SaveAsAgentInput(ctx context.Context, a *fw.Agent, id string) (string, error) {
	run, err := a.Get(ctx, id)
	if err != nil {
		return "", err
	}
	if err := saveCheck(ctx, a, run); err != nil {
		return "", err
	}
	fold := newSaveFold()
	if err := eachEvent(ctx, a, id, 0, fold.add); err != nil {
		return "", err
	}
	return fold.text(run.Agent), nil
}

// ProposalProfile turns the input of a propose_agent call into a profile:
// name, description, instructions as the system prompt, tools, a tool in
// asks_first as an ask rule, a command in never_runs as a deny rule for
// bash ("bash: <command>"), and minutes as the deadline of the budget.
func ProposalProfile(input json.RawMessage) (fw.AgentProfile, error) {
	var d map[string]json.RawMessage
	if err := json.Unmarshal(input, &d); err != nil || d == nil {
		return fw.AgentProfile{}, fmt.Errorf("dashboard: %w: the proposal of propose_agent is not a JSON object", fw.ErrInvalidRequest)
	}
	p := fw.AgentProfile{
		Name:        strings.TrimSpace(jsonString(d["name"])),
		Description: jsonString(d["description"]),
		System:      jsonString(d["instructions"]),
		Tools:       jsonStrings(d["tools"]),
	}
	if p.Name == "" {
		return fw.AgentProfile{}, fmt.Errorf("dashboard: %w: the proposal of propose_agent names no agent", fw.ErrInvalidRequest)
	}
	if p.Tools == nil {
		p.Tools = []string{}
	}
	ask := jsonStrings(d["asks_first"])
	var deny []string
	for _, c := range jsonStrings(d["never_runs"]) {
		deny = append(deny, "bash: "+c)
	}
	if len(ask) > 0 || len(deny) > 0 {
		p.Policy = &fw.Rules{Ask: ask, Deny: deny}
	}
	var minutes float64
	if json.Unmarshal(d["minutes"], &minutes) == nil && minutes > 0 {
		p.Budget = &fw.Budget{Deadline: time.Duration(minutes * float64(time.Minute))}
	}
	return p, nil
}
