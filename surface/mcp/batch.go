package mcp

// This file is the way a person sends a job to the fleet: the tool enqueue
// puts one run request per piece of work on the intake queue of a profile,
// and the tool batch tells how the job goes (docs/ARCHITECTURE.md section
// 4.9). Every run of a job carries the batch in its metadata, and the
// worker that takes a request runs it as the person who enqueued it.

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/surface/view"
	"github.com/bdubs00/featherweight-workbench/fleet"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Names of the job tools.
const (
	toolEnqueue = "enqueue"
	toolBatch   = "batch"
)

// batchListLimit bounds the runs that the batch tool names one by one.
const batchListLimit = 10

// maxEnqueueItems is fleet.MaxEnqueue for the schema.
const maxEnqueueItems = "500"

// enqueueBudgetSchema is the budget of every run of a job.
const enqueueBudgetSchema = `{"type":"object","properties":{
	"maxTurns":{"type":"integer","minimum":1},
	"maxTokens":{"type":"integer","minimum":1},
	"maxCostUSD":{"type":"number","minimum":0},
	"deadline":{"type":"string","description":"A duration such as 30m or 2h."},
	"maxToolCalls":{"type":"integer","minimum":1}
},"description":"The budget of every run of the job."}`

const enqueueSchema = `{"type":"object","properties":{
	"agent":{"type":"string","minLength":1,"description":"The agent profile that the workers of the queue run."},
	"items":{"type":"array","minItems":1,"maxItems":` + maxEnqueueItems + `,"items":{"type":"object","properties":{
		"input":{"type":"string","minLength":1,"description":"The task of one run, complete and self-contained."},
		"title":{"type":"string","description":"A title for the run."},
		"metadata":{"type":"object","additionalProperties":{"type":"string"},"description":"Metadata of the run."}
	},"required":["input"]},"description":"One entry per run."},
	"batch":{"type":"string","description":"The name of the job. Every run gets it in the metadata fw.batch. Without it, the tool makes one."},
	"instructions":{"type":"string","description":"Text that follows the system prompt of every run."},
	"budget":` + enqueueBudgetSchema + `
},"required":["agent","items"]}`

const batchSchema = `{"type":"object","properties":{
	"batch":{"type":"string","minLength":1,"description":"The name of the job, from enqueue or from the metadata fw.batch of a run."}
},"required":["batch"]}`

type enqueueArgs struct {
	Agent        string        `json:"agent"`
	Items        []enqueueItem `json:"items"`
	Batch        string        `json:"batch,omitempty"`
	Instructions string        `json:"instructions,omitempty"`
	Budget       *budgetArgs   `json:"budget,omitempty"`
}

type enqueueItem struct {
	Input    string            `json:"input"`
	Title    string            `json:"title,omitempty"`
	Metadata map[string]string `json:"metadata,omitempty"`
}

type batchArgs struct {
	Batch string `json:"batch"`
}

// enqueueResult is the structured result of enqueue.
type enqueueResult struct {
	View  string `json:"view"`
	Batch string `json:"batch"`
	Agent string `json:"agent"`
	Queue string `json:"queue"`
	Count int    `json:"count"`
}

// batchResult is the structured result of batch.
type batchResult struct {
	View     string         `json:"view"`
	Batch    string         `json:"batch"`
	Total    int            `json:"total"`
	States   map[string]int `json:"states"`
	Members  []string       `json:"members"`
	Waiting  []batchRun     `json:"waiting,omitempty"`
	Failed   []batchRun     `json:"failed,omitempty"`
	Finished bool           `json:"finished"`
	Usage    batchUsage     `json:"usage"`
	// Forgotten counts the runs of the job that the view of the fleet
	// dropped (fleet.BatchView). Total, States, and Usage leave them out.
	Forgotten int `json:"forgotten,omitempty"`
	// Unknown counts the runs of the view that have no identity yet, so
	// that their job is unknown (fleet.BatchRuns). Any job can hold them.
	Unknown int `json:"unknown,omitempty"`
	// Jobs counts the jobs that the caller put on the intake queues under
	// the batch, and Queued those of them that no Task took yet, so no run
	// stands for them (fleet.BatchRuns). Queued is absent when the front
	// knows no record of the jobs. A run in the state queued is a job that
	// a Task took and has not started; Queued never counts it.
	// JobsShared is true when every replica of the front counts the same
	// jobs.
	Jobs       int  `json:"jobs,omitempty"`
	Queued     *int `json:"queued,omitempty"`
	JobsShared bool `json:"jobsShared,omitempty"`
}

// batchUsage is the usage of a job: the sum of the usage of its runs that
// report one, field by field. The sum counts each run once. A child run
// carries the batch of its parent, so it is a run of the job of its own,
// and its usage is not also in the usage of its parent; a branch counts
// only its own work, not the events that it kept from its source. The sum
// adds Run.Usage and never Run.TreeUsage, which holds the child runs too.
//
// Reported counts the runs in the sum, and Missing the runs whose usage
// the view does not know (usageOf). Final counts the reported runs whose
// usage is final. Partial is true when Missing is above zero, when a
// reported run is not final, or when the view forgot runs of the job.
// CostUSD is the cost at the prices of the workers, and it leaves out
// UnpricedTokens, whose cost is unknown.
type batchUsage struct {
	fw.RunUsage
	Reported int  `json:"reported"`
	Final    int  `json:"final"`
	Missing  int  `json:"missing"`
	Partial  bool `json:"partial"`
}

// add counts one run of the job: its usage, or one more missing run.
func (u *batchUsage) add(r *fw.Run) {
	known, final := usageOf(r)
	if !known {
		u.Missing++
		return
	}
	u.Reported++
	if final {
		u.Final++
	}
	u.Usage = u.Usage.Add(r.Usage.Usage)
	u.Turns += r.Usage.Turns
	u.ToolCalls += r.Usage.ToolCalls
	u.CostUSD += r.Usage.CostUSD
	u.UnpricedTokens += r.Usage.UnpricedTokens
}

// usageOf reports whether the view knows the usage of a run, and whether
// that usage is final.
//
// A run of this process, without Member, has the usage of its whole
// ledger, so the view knows it, and it is final once the run is finished.
// A run of another member has a known usage when its kernel recorded the
// totals of the run on fw.run.finished or fw.run.suspended
// (fw.Run.UsageSeq), and the usage is final when the run is finished: the
// totals of its end and the annotations that the stream carried after
// them. Tokens without totals do not count, because on a stream of
// lifecycle events they come from fw.run.annotated alone, such as a title
// call, and not from the model calls of the run. A run that works or waits
// again after its totals, after a follow-up, a resume, or a decision, is
// known up to its totals, so its usage is not final.
func usageOf(r *fw.Run) (known, final bool) {
	if r.Member == "" {
		return true, r.State.Terminal()
	}
	known = r.UsageSeq > 0
	return known, known && r.State.Terminal()
}

// text words the usage of a job on one line.
func (u batchUsage) text() string {
	if u.Reported == 0 {
		return "No run reports usage yet."
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Usage of %s: %s, %s", plural(u.Reported, "run", "runs"), plural(u.Turns, "turn", "turns"), plural(u.InputTokens+u.OutputTokens, "token", "tokens"))
	if c := u.CacheReadTokens + u.CacheWriteTokens; c > 0 {
		fmt.Fprintf(&b, " and %s", plural(c, "cache token", "cache tokens"))
	}
	if u.ToolCalls > 0 {
		b.WriteString(", " + plural(u.ToolCalls, "tool call", "tool calls"))
	}
	if u.CostUSD > 0 {
		fmt.Fprintf(&b, ", $%.4f", u.CostUSD)
	}
	b.WriteString(".")
	switch {
	case u.UnpricedTokens > 0 && u.CostUSD > 0:
		fmt.Fprintf(&b, " The cost leaves out %s without a price, so the whole cost is unknown.", plural(u.UnpricedTokens, "token", "tokens"))
	case u.UnpricedTokens > 0:
		fmt.Fprintf(&b, " The cost is unknown: no price covers %s.", plural(u.UnpricedTokens, "token", "tokens"))
	}
	switch n := u.Reported - u.Final; {
	case n == 1:
		b.WriteString(" The usage of 1 run is not final yet, because the run is not finished.")
	case n > 1:
		fmt.Fprintf(&b, " The usage of %d runs is not final yet, because they are not finished.", n)
	}
	switch {
	case u.Missing == 1:
		b.WriteString(" 1 run reports no usage yet.")
	case u.Missing > 1:
		fmt.Fprintf(&b, " %d runs report no usage yet.", u.Missing)
	}
	if u.Partial {
		b.WriteString(" The sum is partial.")
	}
	b.WriteString(" The sum counts each run once: the usage of a child run is not also in its parent, and a branch counts only its own work.")
	return b.String()
}

// batchRuns lists every run of a batch that the caller can view: the runs
// of this process and the runs of the batch that the view of the fleet
// holds, whole and not cut to the newest finished runs of each member as
// the records are, with the counts of the view of the runs that it forgot
// and of the runs that it knows no identity of (fleet.BatchRuns).
func (s *server) batchRuns(ctx context.Context, batch string) (fleet.BatchRuns, error) {
	local, err := s.a.List(ctx)
	if err != nil {
		return fleet.BatchRuns{}, err
	}
	out := fleet.BatchRuns{Runs: view.FilterBatch(local, batch)}
	v := fleet.Of(s.a)
	if v == nil {
		return out, nil
	}
	if bv, ok := v.(fleet.BatchView); ok {
		b, err := bv.Batch(ctx, batch)
		if err != nil {
			return fleet.BatchRuns{}, err
		}
		b.Runs = append(out.Runs, b.Runs...)
		out = b
	} else {
		remote, err := v.Runs(ctx)
		if err != nil {
			return fleet.BatchRuns{}, err
		}
		out.Runs = append(out.Runs, view.FilterBatch(remote, batch)...)
	}
	view.SortRuns(out.Runs)
	return out, nil
}

// unknownText tells of the runs of the view that have no identity yet, or
// says nothing when there are none.
func unknownText(n int) string {
	if n == 0 {
		return ""
	}
	return fmt.Sprintf(" The view holds %s whose job it does not know yet, because it missed their start, so the counts and the usage can leave out runs of this job.", plural(n, "run", "runs"))
}

// queuedText tells how many of the jobs of the caller wait in the queue,
// or says nothing when the front knows no record of them.
func queuedText(b fleet.BatchRuns) string {
	if b.Jobs == 0 {
		return ""
	}
	if b.Queued == 0 {
		return fmt.Sprintf(" No job of the %d that you sent waits in the queue.", b.Jobs)
	}
	if b.Queued == 1 {
		return fmt.Sprintf(" 1 job of the %d that you sent waits in the queue: no Task took it yet.", b.Jobs)
	}
	return fmt.Sprintf(" %d jobs of the %d that you sent wait in the queue: no Task took them yet.", b.Queued, b.Jobs)
}

// plural writes a number with the singular or the plural noun.
func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

// batchRun is one run of a job that a person must look at.
type batchRun struct {
	RunID  string `json:"runId"`
	Member string `json:"member,omitempty"`
	State  string `json:"state"`
	Title  string `json:"title,omitempty"`
	Detail string `json:"detail,omitempty"`
}

// intakeOn reports whether the configuration names the intake queues.
func (s *server) intakeOn() bool {
	c, err := fleet.Load(s.a.Config())
	return err == nil && c.Intake != ""
}

// addBatchTools adds enqueue and batch when the configuration names the
// intake queues of the workers.
func (s *server) addBatchTools() {
	if !s.intakeOn() {
		return
	}
	sdk.AddTool(s.srv, &sdk.Tool{
		Name:        toolEnqueue,
		Title:       "Enqueue a job",
		Description: "Send a job to the fleet: one run per item, on the queue of an agent profile, which the workers that run that profile take. Every run acts as you and carries the batch name in its metadata. The call returns at once; follow the job with batch, or list its runs with runs_list and the batch argument.",
		InputSchema: json.RawMessage(enqueueSchema),
		Annotations: &sdk.ToolAnnotations{IdempotentHint: true, OpenWorldHint: ptr(false)},
	}, s.enqueue)
	sdk.AddTool(s.srv, &sdk.Tool{
		Name:        toolBatch,
		Title:       "Job status",
		Description: "Tell how a job goes: how many of the jobs that you sent wait in the queue because no Task took them yet, how many runs it has in each state, which members hold them, which runs wait for you, and which failed, with their errors. The job is finished only when no job waits in the queue and every run ended. It also sums the tokens, turns, tool calls, and cost of the runs, counting each run once and a branch for its own work, and says when the sum is partial: a run reports no usage yet, a run is not finished, or the view forgot runs of the job.",
		InputSchema: json.RawMessage(batchSchema),
		Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true, OpenWorldHint: ptr(false)},
	}, s.batch)
}

func (s *server) enqueue(ctx context.Context, req *sdk.CallToolRequest, in enqueueArgs) (*sdk.CallToolResult, any, error) {
	agent := strings.TrimSpace(in.Agent)
	if agent == "" {
		return errorResult("Invalid request: the argument agent is required."), nil, nil
	}
	if len(in.Items) == 0 {
		return errorResult("Invalid request: the argument items needs at least one item."), nil, nil
	}
	p := fw.PrincipalFrom(ctx)
	if p == nil {
		return errorResult("Forbidden: sign in to send a job; the runs act as you."), nil, nil
	}
	if err := s.a.Authorize(ctx, fw.ActionAgentRun, fw.Target{Agent: agent}); err != nil {
		return failResult(err), nil, nil
	}
	batch := strings.TrimSpace(in.Batch)
	if batch == "" {
		batch = strings.ToLower(fw.NewID())
	}
	if n := utf8.RuneCountInString(batch); n > 128 || strings.ContainsAny(batch, " \t\r\n") {
		return errorResult("Invalid request: the batch name holds a space or is longer than 128 characters."), nil, nil
	}
	var budget *fw.Budget
	if in.Budget != nil {
		b, err := in.Budget.budget("budget")
		if err != nil {
			return errorResult("Invalid request: " + strings.TrimPrefix(err.Error(), "fw: invalid request: ") + "."), nil, nil
		}
		budget = &b
	}
	reqs := make([]fw.RunRequest, 0, len(in.Items))
	for i, item := range in.Items {
		if strings.TrimSpace(item.Input) == "" {
			return errorResult(fmt.Sprintf("Invalid request: the input of item %d is empty.", i+1)), nil, nil
		}
		md := maps.Clone(item.Metadata)
		if md == nil {
			md = map[string]string{}
		}
		md[fw.MetaBatch] = batch
		if title := strings.TrimSpace(item.Title); title != "" {
			if utf8.RuneCountInString(title) > maxTitleRunes {
				return errorResult(fmt.Sprintf("Invalid request: the title of item %d is longer than %d characters.", i+1, maxTitleRunes)), nil, nil
			}
			md[fw.MetaTitle] = title
		}
		if err := fw.CheckMetadata(md); err != nil {
			return errorResult(fmt.Sprintf("Invalid request: item %d: %s.", i+1, strings.TrimPrefix(err.Error(), "fw: invalid request: "))), nil, nil
		}
		reqs = append(reqs, fw.RunRequest{
			Input:          item.Input,
			Instructions:   strings.TrimSpace(in.Instructions),
			Budget:         budget,
			Metadata:       md,
			Principal:      p.Subject,
			PrincipalKind:  p.Kind,
			IdempotencyKey: batch + "/" + fmt.Sprint(i+1),
		})
	}
	queue, sent, err := fleet.Enqueue(ctx, s.a, agent, reqs)
	if err != nil {
		if sent > 0 {
			return errorResult(fmt.Sprintf("Only %d of %d runs reached the queue %s as batch %s: %s. Send the rest again with the same batch; a run that arrived twice starts once.", sent, len(reqs), queue, batch, strings.TrimPrefix(err.Error(), "fleet: "))), nil, nil
		}
		return failResult(err), nil, nil
	}
	s.log.Info("mcp job enqueued", "batch", batch, "agent", agent, "queue", queue, "count", sent, "subject", p.Subject)
	out := enqueueResult{View: "batch", Batch: batch, Agent: agent, Queue: queue, Count: sent}
	text := fmt.Sprintf("Queued %d runs of %s as batch %s on %s. They act as you. Call batch with %q to follow the job, or runs_list with batch %q to see its runs.", sent, agent, batch, queue, batch, batch)
	return result(text, out), nil, nil
}

func (s *server) batch(ctx context.Context, req *sdk.CallToolRequest, in batchArgs) (*sdk.CallToolResult, any, error) {
	batch := strings.TrimSpace(in.Batch)
	if batch == "" {
		return errorResult("Invalid request: the argument batch is required."), nil, nil
	}
	found, err := s.batchRuns(ctx, batch)
	if err != nil {
		return failResult(err), nil, nil
	}
	runs, forgotten := found.Runs, found.Forgotten
	// A run whose job the view does not know can be a run of this job that
	// still works, and a job in the queue has no run yet.
	out := batchResult{View: "batch", Batch: batch, Total: len(runs), States: map[string]int{}, Members: []string{},
		Finished: len(runs) > 0 && found.UnknownActive == 0 && found.Queued == 0, Forgotten: forgotten, Unknown: found.Unknown,
		Jobs: found.Jobs, JobsShared: found.JobsShared}
	if found.Jobs > 0 {
		out.Queued = &found.Queued
	}
	members := map[string]bool{}
	for _, r := range runs {
		out.States[string(r.State)]++
		out.Usage.add(r)
		if !r.State.Terminal() {
			out.Finished = false
		}
		member := r.Member
		if member == "" {
			if f := fleet.Of(s.a); f != nil {
				member = f.Name()
			}
		}
		if member != "" {
			members[member] = true
		}
		switch {
		case r.State == fw.StateInputRequired && len(out.Waiting) < batchListLimit:
			br := batchRun{RunID: r.ID, Member: r.Member, State: string(r.State), Title: r.Title}
			if r.Pending != nil {
				br.Detail = r.Pending.Reason
				if br.Detail == "" {
					br.Detail = r.Pending.Tool
				}
			}
			out.Waiting = append(out.Waiting, br)
		case r.State == fw.StateFailed && len(out.Failed) < batchListLimit:
			out.Failed = append(out.Failed, batchRun{RunID: r.ID, Member: r.Member, State: string(r.State), Title: r.Title, Detail: r.Error})
		}
	}
	out.Usage.Partial = out.Usage.Missing > 0 || out.Usage.Final < out.Usage.Reported || forgotten > 0 || found.Unknown > 0
	for m := range members {
		out.Members = append(out.Members, m)
	}
	sort.Strings(out.Members)
	if len(runs) == 0 {
		text := fmt.Sprintf("No run of batch %s is in view. A run appears when a worker takes it from the queue, and a finished run leaves the view after workbench.fleet.expire.", batch)
		text += queuedText(found)
		if forgotten > 0 {
			text += fmt.Sprintf(" The view forgot %s of it.", plural(forgotten, "run", "runs"))
		}
		text += unknownText(found.Unknown)
		return result(text, out), nil, nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Batch %s: %d runs", batch, len(runs))
	if len(out.Members) > 0 {
		fmt.Fprintf(&b, " on %d members", len(out.Members))
	}
	b.WriteString(".")
	states := make([]string, 0, len(out.States))
	for st := range out.States {
		states = append(states, st)
	}
	sort.Strings(states)
	for _, st := range states {
		fmt.Fprintf(&b, " %s: %d.", st, out.States[st])
	}
	b.WriteString(queuedText(found))
	if out.Finished {
		b.WriteString(" The job is finished.")
	}
	if forgotten > 0 {
		fmt.Fprintf(&b, " The view forgot %s of the job, because a finished run leaves the view after workbench.fleet.expire and a run leaves with its Task; the counts and the usage leave them out.", plural(forgotten, "run", "runs"))
	}
	b.WriteString(unknownText(found.Unknown))
	b.WriteString("\n" + out.Usage.text())
	for _, w := range out.Waiting {
		fmt.Fprintf(&b, "\n- %s waits for you", w.RunID)
		if w.Member != "" {
			fmt.Fprintf(&b, " on %s", w.Member)
		}
		if w.Detail != "" {
			fmt.Fprintf(&b, ": %s", w.Detail)
		}
	}
	for _, f := range out.Failed {
		fmt.Fprintf(&b, "\n- %s failed", f.RunID)
		if f.Member != "" {
			fmt.Fprintf(&b, " on %s", f.Member)
		}
		if f.Detail != "" {
			fmt.Fprintf(&b, ": %s", f.Detail)
		}
	}
	return result(b.String(), out), nil, nil
}
