package dashboard

// This file holds the helpers of the work view in work.go: the lines of
// the home, the projection of the ledger into a result, the plain phrases
// of the tool calls, and the first message of "Save as agent".

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/surface/view"
)

// builderTitle is the title of a run of the agent builder.
const builderTitle = "New agent"

// opEdited is the operation of a file that the run changed in place. The
// ledger calls it fw.FileChanged.
const opEdited = "edited"

// Labels of the next actions that have a fixed label.
const (
	labelAnswer    = "Answer"
	labelReview    = "Review changes"
	labelReply     = "Reply"
	labelSaveAgent = "Save as agent"
	labelRetry     = "Try again"
)

// isBuilder reports whether a run is a conversation of the agent builder.
func isBuilder(r *fw.Run) bool {
	return r != nil && r.Metadata[ViewKey] == ViewBuilder
}

// workItem copies a run for a home list, so the list never changes a run
// that the kernel or the fleet view owns.
func workItem(r *fw.Run) WorkItem {
	c := *r
	c.Title = TitleOf(r)
	return WorkItem{Run: &c, Status: StatusOf(r)}
}

// sortByChange sorts work items by their last change, newest first.
func sortByChange(items []WorkItem) {
	sort.SliceStable(items, func(i, j int) bool {
		a, b := items[i].Run, items[j].Run
		if !a.UpdatedAt.Equal(b.UpdatedAt) {
			return a.UpdatedAt.After(b.UpdatedAt)
		}
		return a.ID > b.ID
	})
}

// statusWords is a status word as a person reads it.
func statusWords(status string) string {
	switch status {
	case StatusNeedsYou:
		return "needs you"
	case StatusReady:
		return "ready to review"
	}
	return status
}

// workLine is one run of a home list on one line: the title, the agent,
// the status, the age of the last change, and the run id. A run that waits
// for a connection says which service it waits for.
func workLine(it WorkItem) string {
	r := it.Run
	line := fmt.Sprintf("- %s (%s, %s, %s ago, run %s)", TitleOf(r), r.Agent, statusWords(it.Status), ago(r.UpdatedAt), r.ID)
	if p := r.Pending; p != nil && p.Connect != nil {
		line += ": " + connectReason(p)
	}
	return line
}

// connectLabel is the label of the action that connects the service.
func connectLabel(c fw.ConnectRequest) string { return "Connect " + view.ConnectTitle(c) }

// connectReason is the sentence of a pending connect: the reason that the
// kernel recorded, else the Message of the request.
func connectReason(p *fw.Approval) string {
	if s := strings.TrimSpace(p.Reason); s != "" {
		return s
	}
	return p.Connect.Message()
}

// eachEvent calls fn for each event of a run from the sequence number
// from on, while the ledger streams them. fn keeps what it needs, so a long
// ledger never sits in memory whole.
func eachEvent(ctx context.Context, a *fw.Agent, id string, from uint64, fn func(fw.Event)) error {
	for e, err := range a.Ledger().Read(ctx, id, from) {
		if err != nil {
			return fmt.Errorf("dashboard: read the ledger of run %s: %w", id, err)
		}
		fn(e)
	}
	return nil
}

// readAll reads the events of a run from the sequence number from on into
// memory. Only a short read, such as in a test, uses it.
func readAll(ctx context.Context, a *fw.Agent, id string, from uint64) ([]fw.Event, error) {
	var events []fw.Event
	if err := eachEvent(ctx, a, id, from, func(e fw.Event) { events = append(events, e) }); err != nil {
		return nil, err
	}
	return events, nil
}

// specsByName indexes tool specs by name. The first spec of a name wins,
// as the kernel picks it.
func specsByName(specs []fw.ToolSpec) map[string]fw.ToolSpec {
	out := make(map[string]fw.ToolSpec, len(specs))
	for _, s := range specs {
		if _, ok := out[s.Name]; !ok {
			out[s.Name] = s
		}
	}
	return out
}

// resultFold projects the events of a run into a result, one event at a
// time, so the result of a long ledger never holds the ledger. It keeps the
// last answer, the net change to each file, the limits, and the newest
// MaxResultSteps steps.
type resultFold struct {
	specs    map[string]fw.ToolSpec
	last     string
	files    netFiles
	limits   []string
	steps    []ResultStep   // the steps at the positions base and up
	base     int            // the position of steps[0]
	callAt   map[string]int // the position of the step of each call id
	loopStop bool
}

// newResultFold starts the fold of a result with the tool specs by name.
func newResultFold(specs map[string]fw.ToolSpec) *resultFold {
	return &resultFold{specs: specs, callAt: map[string]int{}}
}

// add folds one event.
func (f *resultFold) add(e fw.Event) {
	// A loop stop records fw.loop.detected and then fw.budget.exceeded for
	// maxRepeats. The first one says it.
	afterLoop := f.loopStop
	f.loopStop = false
	switch e.Type {
	case fw.EvModelResponded:
		var d fw.ModelRespondedData
		if e.Decode(&d) == nil {
			if t := strings.TrimSpace(fw.TextOf(d.Content)); t != "" {
				f.last = t
			}
		}
	case fw.EvToolRequested:
		var d fw.ToolRequestedData
		if e.Decode(&d) != nil {
			return
		}
		f.step(d.CallID, ResultStep{Seq: e.Seq, Tool: d.Tool, Text: stepText(d.Tool, d.Input, f.specs[d.Tool])})
	case fw.EvToolResponded:
		var d fw.ToolRespondedData
		if e.Decode(&d) != nil {
			return
		}
		// A call that did not run, such as a denied call, has no step.
		if i, ok := f.callAt[d.CallID]; ok && d.CallID != "" && i >= f.base {
			f.steps[i-f.base].Done, f.steps[i-f.base].IsError = true, d.IsError
		}
	case fw.EvFileChanged:
		var d fw.FileChangedData
		if e.Decode(&d) == nil {
			f.files.add(e.Seq, d)
		}
	case fw.EvLoopDetected:
		var d fw.LoopDetectedData
		if e.Decode(&d) == nil {
			f.limits = append(f.limits, loopText(d))
			f.loopStop = d.Action == "failed"
		}
	case fw.EvBudgetExceeded:
		var d fw.BudgetExceededData
		if e.Decode(&d) == nil && !(afterLoop && d.Limit == "maxRepeats") {
			f.limits = append(f.limits, limitText(d))
		}
	}
}

// step records the request of a call. A resumed run can run a call again,
// and it stays one step at the position of its first request. A step that
// falls out of the newest MaxResultSteps never comes back, because the
// positions only grow, so the fold drops it and keeps its call id only.
func (f *resultFold) step(callID string, st ResultStep) {
	if i, ok := f.callAt[callID]; ok && callID != "" {
		if i >= f.base {
			f.steps[i-f.base] = st
		}
		return
	}
	f.callAt[callID] = f.base + len(f.steps)
	f.steps = append(f.steps, st)
	if len(f.steps) > MaxResultSteps {
		copy(f.steps, f.steps[1:])
		f.steps = f.steps[:MaxResultSteps]
		f.base++
	}
}

// result returns the result of the run from the folded events, without the
// next actions.
func (f *resultFold) result(run *fw.Run) Result {
	res := Result{
		RunID:   run.ID,
		Title:   TitleOf(run),
		Agent:   run.Agent,
		Status:  StatusOf(run),
		Error:   run.Error,
		Limits:  f.limits,
		Started: run.CreatedAt,
		Changed: run.UpdatedAt,
	}
	usage := run.Usage
	res.Usage = &usage
	summary := run.Output
	if strings.TrimSpace(summary) == "" {
		summary = f.last
	}
	res.Summary, _ = cut(summary, false)
	res.Files = f.files.result()
	res.Steps = f.steps
	if p := run.Pending; p != nil && p.Connect != nil {
		c := *p.Connect
		res.Pending = &Decision{
			ID:      p.ID,
			Tool:    p.Tool,
			Label:   connectLabel(c),
			Reason:  connectReason(p),
			Input:   clone(p.Input),
			Connect: &c,
		}
	} else if p != nil {
		q := len(p.Schema) > 0
		d := &Decision{
			ID:       p.ID,
			Tool:     p.Tool,
			Label:    ActionLabel(f.specs[p.Tool], q),
			Question: q,
			Reason:   p.Reason,
			Input:    clone(p.Input),
			Schema:   clone(p.Schema),
		}
		// An answer to a question is not a permission to act, so a
		// question has no effect.
		if !q {
			d.Effect = effectOf(p.Tool, p.Input)
		}
		res.Pending = d
	}
	return res
}

// clone copies raw JSON, so a result never shares memory with a run.
func clone(raw json.RawMessage) json.RawMessage {
	if raw == nil {
		return nil
	}
	return append(json.RawMessage(nil), raw...)
}

// nextActions lists the useful actions of a result, the most useful first.
// save tells whether the caller can turn the run into an agent.
func nextActions(run *fw.Run, res Result, save bool) []NextAction {
	var out []NextAction
	if p := res.Pending; p != nil {
		if p.Connect != nil {
			out = append(out, NextAction{Kind: NextConnect, Label: p.Label})
		} else if p.Question {
			out = append(out, NextAction{Kind: NextAnswer, Label: labelAnswer})
		} else {
			out = append(out, NextAction{Kind: NextDecide, Label: p.Label})
		}
	}
	if len(res.Files) > 0 {
		out = append(out, NextAction{Kind: NextReview, Label: labelReview})
	}
	if run.State.Terminal() {
		out = append(out, NextAction{Kind: NextReply, Label: labelReply})
	}
	if save {
		out = append(out, NextAction{Kind: NextSaveAgent, Label: labelSaveAgent})
	}
	if run.State == fw.StateFailed {
		out = append(out, NextAction{Kind: NextRetry, Label: labelRetry})
	}
	return out
}

// canSave reports whether the caller can turn a run into an agent. The
// page showed "Save as agent" on the same terms.
func canSave(ctx context.Context, a *fw.Agent, run *fw.Run) bool {
	return saveCheck(ctx, a, run) == nil
}

// saveCheck returns nil when the caller can turn a run into an agent: a
// completed top-level run of this process that is not a builder run, that
// the caller started, for a caller that can create agents. Otherwise it
// returns fw.ErrForbidden with the reason in plain words, or the error of
// the authorization. A builder run snapshots the teams of the caller, so a
// run of another person never feeds it: its content would reach the teams
// of the caller.
// SaveRefusal is the reason why Save as agent refuses a run. It wraps
// fw.ErrForbidden, so a surface answers it as any refusal, but its reason
// needs no role: no owner can grant it.
type SaveRefusal struct{ Reason string }

func (e *SaveRefusal) Error() string { return fw.ErrForbidden.Error() + ": " + e.Reason }

// Unwrap returns fw.ErrForbidden.
func (e *SaveRefusal) Unwrap() error { return fw.ErrForbidden }

// refuse builds a SaveRefusal from a format and its arguments.
func refuse(format string, args ...any) error {
	return &SaveRefusal{Reason: fmt.Sprintf(format, args...)}
}

func saveCheck(ctx context.Context, a *fw.Agent, run *fw.Run) error {
	return saveTerms(ctx, a, run, true)
}

// saveTerms applies the terms of saveCheck. With here, the run must be a
// run of this process; without it, the run is a run of another member that
// the caller read through the fleet (SaveAsAgentInputOn).
func saveTerms(ctx context.Context, a *fw.Agent, run *fw.Run, here bool) error {
	switch {
	case run.State != fw.StateCompleted:
		return refuse("only a completed run becomes an agent, and run %s is %s", run.ID, run.State)
	case run.Parent != "":
		return refuse("run %s is a child run, so only its top-level run becomes an agent", run.ID)
	case here && run.Member != "":
		return refuse("run %s belongs to the fleet member %s, so only that member makes an agent from it", run.ID, run.Member)
	case isBuilder(run):
		return refuse("run %s is a conversation of the agent builder, not work to hand off", run.ID)
	}
	if p := fw.PrincipalFrom(ctx); p != nil && p.Subject != "" && run.Principal != "" && run.Principal != p.Subject {
		return refuse("only the person who started run %s can turn it into an agent", run.ID)
	}
	return a.Authorize(ctx, fw.ActionAgentCreate, fw.Target{})
}

// netFiles collects the net change of a run to each path, in the order of
// the first change.
type netFiles struct {
	list []*ResultFile
	at   map[string]*ResultFile
}

// add applies one fw.file.changed event. A change without a tool call is
// an attachment of the person, not a change of the run.
func (n *netFiles) add(seq uint64, d fw.FileChangedData) {
	if d.Path == "" || (d.CallID == "" && d.Tool == "") {
		return
	}
	if n.at == nil {
		n.at = map[string]*ResultFile{}
	}
	op := d.Op
	if op == fw.FileChanged {
		op = opEdited
	}
	f := ResultFile{Path: d.Path, Op: op, Size: d.Size, SHA256: d.SHA256, Seq: seq}
	if d.Op == fw.FileRenamed && d.OldPath != "" {
		f.OldPath = d.OldPath
		if old, ok := n.at[d.OldPath]; ok {
			// The rename moves the entry to the new path. A file that the
			// run created stays created, and a renamed file keeps the path
			// that it had before the run.
			delete(n.at, d.OldPath)
			switch old.Op {
			case fw.FileCreated:
				f.Op, f.OldPath = fw.FileCreated, ""
			case fw.FileRenamed:
				f.OldPath = old.OldPath
			}
			if f.OldPath == f.Path {
				f.Op, f.OldPath = opEdited, ""
			}
			if cur, ok := n.at[d.Path]; ok && cur != old {
				cur.Path = "" // replaced: result drops it
			}
			*old = f
			n.at[d.Path] = old
			return
		}
	}
	if cur, ok := n.at[d.Path]; ok {
		// A later change replaces an earlier one. An edit keeps a create
		// or a rename, because the net change is still that.
		if f.Op == opEdited && (cur.Op == fw.FileCreated || cur.Op == fw.FileRenamed) {
			f.Op, f.OldPath = cur.Op, cur.OldPath
		}
		*cur = f
		return
	}
	p := &f
	n.at[d.Path] = p
	n.list = append(n.list, p)
}

// result returns the entries in the order of the first change.
func (n *netFiles) result() []ResultFile {
	var out []ResultFile
	for _, f := range n.list {
		if f.Path != "" {
			out = append(out, *f)
		}
	}
	return out
}

// Inputs of the built-in tools that effectOf reads.
type (
	bashInput struct {
		Command string `json:"command"`
	}
	filesInput struct {
		Command string `json:"command"`
		Path    string `json:"path"`
	}
)

// stepText names a tool call for a person with the step phrase of the
// kernel (view.StepPhrase), which also words the activity of a run.
func stepText(tool string, input json.RawMessage, spec fw.ToolSpec) string {
	return view.StepPhrase(tool, input, spec)
}

// effectOf says in one sentence what allowing a call does, when the input
// has a shape that says it reliably. It returns "" otherwise.
func effectOf(tool string, input json.RawMessage) string {
	switch tool {
	case "bash":
		var in bashInput
		if json.Unmarshal(input, &in) == nil {
			if c := strings.TrimSpace(in.Command); c != "" {
				return `Runs "` + c + `".`
			}
		}
	case "files":
		var in filesInput
		if json.Unmarshal(input, &in) == nil && in.Path != "" {
			switch in.Command {
			case "create", "write", "str_replace", "insert", "edit":
				return "Changes " + in.Path + "."
			case "delete", "remove":
				return "Removes " + in.Path + "."
			}
		}
	}
	return ""
}

// nearShare is the share of a limit from which nearLimits words it.
const nearShare = 0.75

// nearLimits words each limit of the budget that a run has used for
// nearShare or more, such as "Used 18 of 20 turns.". A limit of zero has
// no bound and makes no line. The turns are those of the run. The tokens,
// the cost, and the tool calls are those of the run and of its child runs
// (Run.TreeUsage), because the run shares those limits with them.
func nearLimits(r *fw.Run, b fw.Budget) []string {
	var out []string
	u, tree, with := r.Usage, r.Usage, ""
	if r.TreeUsage != nil {
		tree, with = *r.TreeUsage, " with the child runs"
	}
	near := func(used, limit float64) bool { return limit > 0 && used >= nearShare*limit }
	if near(float64(u.Turns), float64(b.MaxTurns)) {
		out = append(out, fmt.Sprintf("Used %d of %s.", u.Turns, count(b.MaxTurns, "turn", "turns")))
	}
	if tokens := tree.InputTokens + tree.OutputTokens; near(float64(tokens), float64(b.MaxTokens)) {
		out = append(out, fmt.Sprintf("Used %d of %s%s.", tokens, count(b.MaxTokens, "token", "tokens"), with))
	}
	if near(tree.CostUSD, b.MaxCostUSD) {
		out = append(out, fmt.Sprintf("Used $%.2f of the $%.2f cost limit%s.", tree.CostUSD, b.MaxCostUSD, with))
	}
	if near(float64(tree.ToolCalls), float64(b.MaxToolCalls)) {
		out = append(out, fmt.Sprintf("Used %d of %s%s.", tree.ToolCalls, count(b.MaxToolCalls, "tool call", "tool calls"), with))
	}
	return out
}

// limitText words a budget stop.
func limitText(d fw.BudgetExceededData) string {
	if d.Scope == fw.ScopeTree {
		return treeLimitText(d)
	}
	n := int(d.Value)
	switch d.Limit {
	case "maxTurns":
		return "Stopped at the limit of " + count(n, "turn", "turns") + "."
	case "maxTokens":
		return "Stopped at the token limit, after " + count(n, "token", "tokens") + "."
	case "maxCostUSD":
		return fmt.Sprintf("Stopped at the cost limit, after $%.2f.", d.Value)
	case "maxToolCalls":
		return "Stopped at the limit of " + count(n, "tool call", "tool calls") + "."
	case "deadline":
		return "Stopped at the time limit, after " + workTime(time.Duration(d.Value*float64(time.Second))) + " of work."
	case "maxRepeats":
		return fmt.Sprintf("Stopped, because the agent made the same call %d times in a row.", n)
	}
	return fmt.Sprintf("Stopped at the limit %s, at %g.", d.Limit, d.Value)
}

// treeLimitText words a stop at a limit that a run shares with the runs
// under it, such as its child runs.
func treeLimitText(d fw.BudgetExceededData) string {
	who := "run " + d.Run
	if d.Run == "" {
		who = "a run above this one"
	}
	n := int(d.Value)
	switch d.Limit {
	case "maxTokens":
		return fmt.Sprintf("Stopped at the token limit of %s, which it shares with its child runs, after %s in all.", who, count(n, "token", "tokens"))
	case "maxCostUSD":
		return fmt.Sprintf("Stopped at the cost limit of %s, which it shares with its child runs, after $%.2f in all.", who, d.Value)
	case "maxToolCalls":
		return fmt.Sprintf("Stopped at the tool call limit of %s, which it shares with its child runs, after %s in all.", who, count(n, "tool call", "tool calls"))
	}
	return fmt.Sprintf("Stopped at the limit %s of %s, which it shares with its child runs, at %g.", d.Limit, who, d.Value)
}

// loopText words a repeat that the loop guard found.
func loopText(d fw.LoopDetectedData) string {
	what := "the same call"
	if d.Tool != "" {
		what = "the same " + d.Tool + " call"
	}
	if d.Action == "failed" {
		return fmt.Sprintf("Stopped, because the agent made %s %d times in a row.", what, d.Count)
	}
	return fmt.Sprintf("The agent made %s %d times in a row, and fw told it so.", what, d.Count)
}

// workTime words a duration in minutes, or in seconds below a minute.
func workTime(d time.Duration) string {
	if d < time.Minute {
		return count(int(d.Round(time.Second)/time.Second), "second", "seconds")
	}
	return count(int(d.Round(time.Minute)/time.Minute), "minute", "minutes")
}

// resultText is the result as text, in the order of its fields.
func resultText(r Result) string {
	var b strings.Builder
	b.WriteString(r.Title)
	fmt.Fprintf(&b, "\nRun %s of the agent %s: %s.", r.RunID, r.Agent, statusWords(r.Status))
	if r.Summary != "" {
		b.WriteString("\n\n" + r.Summary)
	}
	if r.Error != "" {
		b.WriteString("\n\nError: " + r.Error)
	}
	if len(r.Files) > 0 {
		b.WriteString("\n\nFiles that the run changed:")
		for _, f := range r.Files {
			b.WriteString("\n- " + fileLine(f))
		}
	}
	if len(r.Steps) > 0 {
		b.WriteString("\n\nSteps:")
		for _, s := range r.Steps {
			b.WriteString("\n- " + s.Text)
			switch {
			case !s.Done:
				b.WriteString(" (no result yet)")
			case s.IsError:
				b.WriteString(" (failed)")
			}
		}
	}
	if p := r.Pending; p != nil {
		if p.Connect != nil {
			fmt.Fprintf(&b, "\n\nWaiting for you to connect %s (approval %s): %s", view.ConnectTitle(*p.Connect), p.ID, p.Reason)
		} else if p.Question {
			fmt.Fprintf(&b, "\n\nWaiting for your answer (approval %s):\n%s", p.ID, p.Reason)
		} else {
			fmt.Fprintf(&b, "\n\nWaiting for your decision (approval %s): %s, the tool %s.", p.ID, p.Label, p.Tool)
			if p.Effect != "" {
				b.WriteString("\n" + p.Effect)
			}
			if p.Reason != "" {
				b.WriteString("\nReason: " + p.Reason)
			}
		}
	}
	if len(r.Limits) > 0 {
		b.WriteString("\n\nLimits:")
		for _, l := range r.Limits {
			b.WriteString("\n- " + l)
		}
	}
	if len(r.Next) > 0 {
		labels := make([]string, len(r.Next))
		for i, n := range r.Next {
			labels[i] = n.Label
		}
		b.WriteString("\n\nNext: " + strings.Join(labels, ", ") + ".")
	}
	if !r.Started.IsZero() {
		fmt.Fprintf(&b, "\n\nStarted %s ago, last change %s ago.", ago(r.Started), ago(r.Changed))
	}
	if u := r.Usage; u != nil {
		fmt.Fprintf(&b, "\nUsage: %s, %s", count(u.Turns, "turn", "turns"), count(u.InputTokens+u.OutputTokens, "token", "tokens"))
		if u.ToolCalls > 0 {
			b.WriteString(", " + count(u.ToolCalls, "tool call", "tool calls"))
		}
		if u.CostUSD > 0 {
			fmt.Fprintf(&b, ", $%.4f", u.CostUSD)
		}
		b.WriteString(".")
	}
	return b.String()
}

// fileLine words the net change to one file.
func fileLine(f ResultFile) string {
	switch f.Op {
	case fw.FileRenamed:
		return "Renamed " + f.OldPath + " to " + f.Path
	case fw.FileRemoved:
		return "Removed " + f.Path
	case fw.FileCreated:
		return fmt.Sprintf("Created %s (%s)", f.Path, count(int(f.Size), "byte", "bytes"))
	case opEdited:
		return fmt.Sprintf("Edited %s (%s)", f.Path, count(int(f.Size), "byte", "bytes"))
	}
	return f.Op + " " + f.Path
}

// Bounds of the first message of "Save as agent".
const (
	maxAsked      = 8
	maxAnswerRune = 2000
)

// saveAsAgentText is the first message of a builder conversation that
// makes an agent from the events of a run of the agent: what the person
// asked, the last answer of the agent, and the tools that the run called.
// The page said the same before the server made it.
func saveAsAgentText(agent string, events []fw.Event) string {
	f := newSaveFold()
	for _, e := range events {
		f.add(e)
	}
	return f.text(agent)
}

// saveFold collects the first message of "Save as agent" one event at a
// time, so a long ledger never sits in memory whole. It keeps at most
// maxAsked messages of the person, the last answer, and the names of the
// tools.
type saveFold struct {
	asked, used []string
	seen        map[string]bool
	last        string
}

func newSaveFold() *saveFold { return &saveFold{seen: map[string]bool{}} }

// ask keeps a message of the person while fewer than maxAsked are kept.
func (f *saveFold) ask(s string) {
	if s = strings.TrimSpace(s); s != "" && len(f.asked) < maxAsked {
		f.asked = append(f.asked, s)
	}
}

// add folds one event.
func (f *saveFold) add(e fw.Event) {
	switch e.Type {
	case fw.EvRunQueued:
		var d fw.RunQueuedData
		if e.Decode(&d) == nil {
			f.ask(d.Input)
		}
	case fw.EvMessageReceived:
		var d fw.MessageReceivedData
		// The kernel sends "Continue." itself. The person did not.
		if e.Decode(&d) == nil && d.Role == fw.RoleUser && d.Origin != fw.OriginKernel {
			f.ask(fw.TextOf(d.Content))
		}
	case fw.EvModelResponded:
		var d fw.ModelRespondedData
		if e.Decode(&d) != nil {
			return
		}
		if t := strings.TrimSpace(fw.TextOf(d.Content)); t != "" {
			f.last = t
		}
		for _, b := range d.Content {
			if b.Type == fw.BlockToolUse && b.Name != "" && !f.seen[b.Name] {
				f.seen[b.Name] = true
				f.used = append(f.used, b.Name)
			}
		}
	}
}

// text is the first message for the agent of the run.
func (f *saveFold) text(agent string) string {
	last := f.last
	if r := []rune(last); len(r) > maxAnswerRune {
		last = string(r[:maxAnswerRune])
	}
	lines := make([]string, len(f.asked))
	for i, s := range f.asked {
		lines[i] = "- " + s
	}
	tools := strings.Join(f.used, ", ")
	if tools == "" {
		tools = "none"
	}
	return "Make an agent from this conversation, so I can hand off this kind of work again.\n\nWhat I asked:\n" +
		strings.Join(lines, "\n") +
		"\n\nWhat " + agent + " answered last:\n" + last +
		"\n\nThe tools that it used: " + tools + "."
}

// jsonString decodes a JSON string. Any other value is "".
func jsonString(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return ""
	}
	return s
}

// jsonStrings decodes a JSON array of strings. Any other value is nil, as
// the page took a value that is not an array for no entry.
func jsonStrings(raw json.RawMessage) []string {
	var s []string
	if json.Unmarshal(raw, &s) != nil {
		return nil
	}
	return s
}
