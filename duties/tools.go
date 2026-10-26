package duties

// This file holds the MCP tools of the duties (docs/ARCHITECTURE.md
// section 22.7), which the package adds to the MCP server of the
// workbench through mcp.RegisterTools. Each tool calls the Go function of
// its route, with the same checks of access and the same errors. For a
// call of a run of the fleet that names its person, the tools act as that
// person (manager.OnBehalf). Whether a call waits for a person is the
// policy of the profile that calls it, such as ask in the profile of the
// manager for duty_put.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/duties/schedule"
	"github.com/bdubs00/featherweight-workbench/manager"
	"github.com/bdubs00/featherweight-workbench/surface/mcp"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Names of the tools.
const (
	ToolList       = "duties_list"
	ToolGet        = "duty_get"
	ToolPut        = "duty_put"
	ToolPreview    = "duty_preview"
	ToolRunNow     = "duty_run_now"
	ToolPause      = "duty_pause"
	ToolDelete     = "duty_delete"
	ToolPauseAll   = "duties_pause_all"
	ToolWriteCheck = "duty_write_check"
)

const (
	nameSchema = `{"type":"string","minLength":1,"maxLength":63,"pattern":"^[a-z0-9_-]+$","description":"The name of the duty: 1 to 63 of a-z, 0-9, '-', and '_', such as daily-homelab-report."}`

	emptySchema = `{"type":"object","properties":{}}`

	nameOnlySchema = `{"type":"object","properties":{"name":` + nameSchema + `},"required":["name"]}`

	putSchema = `{"type":"object","properties":{
		"name":` + nameSchema + `,
		"duty":` + dutySchema + `,
		"dryRun":{"type":"boolean","description":"Save nothing, and answer as duty_preview does. duty_preview needs no approval, so call it first."},
		"test":{"type":"boolean","description":"With dryRun, also run checkFirst once and answer the items that it finds now and whether they wake the agent."},
		"timeZone":` + zoneSchema + `
	},"required":["name","duty"]}`

	previewSchema = `{"type":"object","properties":{
		"name":` + nameSchema + `,
		"duty":` + dutySchema + `,
		"test":{"type":"boolean","description":"Also run checkFirst once and answer the items that it finds now and whether they wake the agent."},
		"timeZone":` + zoneSchema + `
	},"required":["name","duty"]}`

	zoneSchema = `{"type":"string","maxLength":64,"description":"Pass it only when the person names a time zone, such as America/New_York. Leave it out otherwise, also when your own clock says UTC: the server then takes the time zone that the person saved, else UTC. The readback names the zone, so show it to the person."}`

	dutySchema = `{"type":["object","string"],"description":"The duty, as an object. A JSON text of the object reads too. title is the name of the duty in the words of the person, such as Walk Duty 1, and is optional. agent names the agent that does it. when holds one start: say, the words of a schedule such as \"every weekday at 9am\", or schedule, the record {repeat, days, everyWeeks, dayOfMonth, times, every, between, timeZone, start, end}, or webhook {auth: {mode, secret, header}, key}. checkFirst {look: {http: {url, secret, header}} or {tool, input}, items, key, where, wake} looks for work first; duty_write_check writes it from words. do holds one action: ask {message} asks the agent, post {bridge, to, message} posts in a bridge that stays open, open {playbook, goal} opens a bridge from a playbook, and manager {message} asks the manager. A message takes the placeholders {date}, {time}, {found}, {event}, {last.title}, {last.run}, {last.state}, and {duty}. For ask and manager, limits.budget is required, with at least one of maxCostUSD, maxTurns, maxTokens, and maxToolCalls above zero, and limits.perDayUSD caps a day. A post spends nothing of its own and takes no budget. For open, limits.budget holds maxTurns alone, the most turns of the whole bridge. limits also take overlap (skip, queue, allow), catchUp, and retries. whileAway, notify, runAs (duty or owner), and paused are optional. notify {failure, needsYou} lists owner, the subject of a person, or channel:<name> for a shared channel of the operator. A read of duty_get is a valid body."}`

	pauseSchema = `{"type":"object","properties":{
		"name":` + nameSchema + `,
		"paused":{"type":"boolean","description":"true pauses the duty, and false resumes it. A resume ends the rows of failures, and the times of the pause do not run."}
	},"required":["name","paused"]}`

	writeCheckSchema = `{"type":"object","properties":{
		"name":` + nameSchema + `,
		"words":{"type":"string","minLength":1,"maxLength":2000,"description":"What the check looks for, in the words of the person, such as \"a new P1 or P2 in the Platform Ops queue\"."},
		"agent":{"type":"string","maxLength":64,"description":"The agent of a duty that does not exist yet. For a saved duty, the agent of the duty writes the check."},
		"budget":{"type":"object","description":"The budget of each run of the duty, as limits.budget of duty_put: maxCostUSD, maxTurns, maxTokens, maxToolCalls. The run that writes the check takes the budget of the agent, else this one, else the budget of the saved duty.","properties":{"maxCostUSD":{"type":"number","minimum":0},"maxTurns":{"type":"integer","minimum":0},"maxTokens":{"type":"integer","minimum":0},"maxToolCalls":{"type":"integer","minimum":0}}}
	},"required":["name","words"]}`

	pauseAllSchema = `{"type":"object","properties":{
		"paused":{"type":"boolean","description":"true pauses every duty of the fleet, and false lets them run again. Runs that work go on."},
		"reason":{"type":"string","maxLength":500,"description":"Why, in one line, for the people who see the duties."}
	},"required":["paused"]}`
)

type (
	nameArgs struct {
		Name string `json:"name"`
	}
	putArgs struct {
		Name     string          `json:"name"`
		Duty     json.RawMessage `json:"duty"`
		DryRun   bool            `json:"dryRun,omitempty"`
		Test     bool            `json:"test,omitempty"`
		TimeZone string          `json:"timeZone,omitempty"`
	}
	previewArgs struct {
		Name     string          `json:"name"`
		Duty     json.RawMessage `json:"duty"`
		Test     bool            `json:"test,omitempty"`
		TimeZone string          `json:"timeZone,omitempty"`
	}
	pauseArgs struct {
		Name   string `json:"name"`
		Paused bool   `json:"paused"`
	}
	pauseAllArgs struct {
		Paused bool   `json:"paused"`
		Reason string `json:"reason,omitempty"`
	}
	writeCheckArgs struct {
		Name   string  `json:"name"`
		Words  string  `json:"words"`
		Agent  string  `json:"agent,omitempty"`
		Budget *Budget `json:"budget,omitempty"`
	}
	// putResult is the structured result of duty_put: the duty after a
	// write, with created, or the answer of a dry run.
	putResult struct {
		Duty    *View      `json:"duty,omitempty"`
		Created bool       `json:"created,omitempty"`
		DryRun  *DryAnswer `json:"dryRun,omitempty"`
	}
	nameResult struct {
		Name    string `json:"name"`
		Deleted bool   `json:"deleted"`
	}
)

func ptr[T any](v T) *T { return &v }

// tools builds the tools of the duties for one MCP server. A process
// without duties gets none.
func tools(srv *mcp.Server) []mcp.Tool {
	a := srv.Agent()
	if Of(a) == nil {
		return nil
	}
	readOnly := &sdk.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true, OpenWorldHint: ptr(false)}
	return []mcp.Tool{
		mcp.NewTool(&sdk.Tool{
			Name:        ToolList,
			Title:       "List duties",
			Description: "List the duties that you can view: what each agent does on its own, when in words, its next time, its last start, and what it spent today. A duty whose source is file is an entry of workbench.sources in fw.yaml, which the tools never change. paused tells whether the switch pauses every duty.",
			InputSchema: json.RawMessage(emptySchema),
			Annotations: readOnly,
		}, func(ctx context.Context, _ *sdk.CallToolRequest, _ struct{}) (*sdk.CallToolResult, any, error) {
			return listTool(ctx, a), nil, nil
		}),
		mcp.NewTool(&sdk.Tool{
			Name:        ToolGet,
			Title:       "Read a duty",
			Description: "Read one duty with its readback in words, its next five times, its state, and its history: the last 50 starts, newest first, each with its run, its state, its cost, what its check found, and why a start did not run.",
			InputSchema: json.RawMessage(nameOnlySchema),
			Annotations: readOnly,
		}, func(ctx context.Context, _ *sdk.CallToolRequest, in nameArgs) (*sdk.CallToolResult, any, error) {
			return getTool(ctx, a, in), nil, nil
		}),
		mcp.NewTool(&sdk.Tool{
			Name:        ToolPut,
			Title:       "Create or change a duty",
			Description: "Create or change a duty: what an agent does on its own, when, and within which limits. It needs the owner role on the agent. The schedule is words, such as {\"when\": {\"say\": \"every weekday at 9am\"}}, and the server reads them with a fixed grammar. Call duty_preview first, show the readback and the next times to the person, and save with duty_put only after the person agrees.",
			InputSchema: json.RawMessage(putSchema),
			Annotations: &sdk.ToolAnnotations{DestructiveHint: ptr(true), IdempotentHint: true, OpenWorldHint: ptr(false)},
		}, func(ctx context.Context, _ *sdk.CallToolRequest, in putArgs) (*sdk.CallToolResult, any, error) {
			return putTool(ctx, a, in), nil, nil
		}),
		mcp.NewTool(&sdk.Tool{
			Name:        ToolPreview,
			Title:       "Preview a duty",
			Description: "Show a duty as duty_put would save it, and save nothing: the duty as it would be stored, its readback in words, its next five times, and every problem. It reads only, so it needs no approval. Show the readback and the next times to the person, then save with duty_put after the person agrees. With test, it also runs the check once.",
			InputSchema: json.RawMessage(previewSchema),
			Annotations: readOnly,
		}, func(ctx context.Context, _ *sdk.CallToolRequest, in previewArgs) (*sdk.CallToolResult, any, error) {
			return putTool(ctx, a, putArgs{Name: in.Name, Duty: in.Duty, DryRun: true, Test: in.Test, TimeZone: in.TimeZone}), nil, nil
		}),
		mcp.NewTool(&sdk.Tool{
			Name:        ToolRunNow,
			Title:       "Run a duty now",
			Description: "Start a duty now, out of its schedule, for a test. It needs the owner role on the agent. It runs the check of the duty and keeps its overlap and its daily cap. The answer names the run, or says why the duty did not start.",
			InputSchema: json.RawMessage(nameOnlySchema),
			Annotations: &sdk.ToolAnnotations{DestructiveHint: ptr(false), IdempotentHint: false, OpenWorldHint: ptr(false)},
		}, func(ctx context.Context, _ *sdk.CallToolRequest, in nameArgs) (*sdk.CallToolResult, any, error) {
			return runNowTool(ctx, a, in), nil, nil
		}),
		mcp.NewTool(&sdk.Tool{
			Name:        ToolPause,
			Title:       "Pause or resume a duty",
			Description: "Pause a duty, or resume it. It needs the owner role on the agent. Runs that work go on. A resume ends the rows of failures, and the times of the pause do not run.",
			InputSchema: json.RawMessage(pauseSchema),
			Annotations: &sdk.ToolAnnotations{DestructiveHint: ptr(false), IdempotentHint: true, OpenWorldHint: ptr(false)},
		}, func(ctx context.Context, _ *sdk.CallToolRequest, in pauseArgs) (*sdk.CallToolResult, any, error) {
			return pauseTool(ctx, a, in), nil, nil
		}),
		mcp.NewTool(&sdk.Tool{
			Name:        ToolDelete,
			Title:       "Delete a duty",
			Description: "Delete a duty. It needs the owner role on the agent. Runs that work go on, and the history stays for 30 days.",
			InputSchema: json.RawMessage(nameOnlySchema),
			Annotations: &sdk.ToolAnnotations{DestructiveHint: ptr(true), IdempotentHint: true, OpenWorldHint: ptr(false)},
		}, func(ctx context.Context, _ *sdk.CallToolRequest, in nameArgs) (*sdk.CallToolResult, any, error) {
			return deleteTool(ctx, a, in), nil, nil
		}),
		mcp.NewTool(&sdk.Tool{
			Name:        ToolWriteCheck,
			Title:       "Write the check of a duty",
			Description: "Turn the words of a person into the check of a duty, the record checkFirst, which looks for work before the duty wakes the agent. The agent of the duty writes it with its own tools, in one run within its budget, and the server tests it once. The answer is the record, its readback in words, and what the check finds now. It saves nothing: put the record in checkFirst with duty_put. It needs the owner role on the agent, and an agent without a budget does not run.",
			InputSchema: json.RawMessage(writeCheckSchema),
			Annotations: &sdk.ToolAnnotations{DestructiveHint: ptr(false), IdempotentHint: false, OpenWorldHint: ptr(true)},
		}, func(ctx context.Context, _ *sdk.CallToolRequest, in writeCheckArgs) (*sdk.CallToolResult, any, error) {
			return writeCheckTool(ctx, a, in), nil, nil
		}),
		mcp.NewTool(&sdk.Tool{
			Name:        ToolPauseAll,
			Title:       "Pause every duty",
			Description: "The kill switch: pause every duty of the fleet, or let them run again. It needs the admin role of the process. Runs that work go on unless somebody also cancels them.",
			InputSchema: json.RawMessage(pauseAllSchema),
			Annotations: &sdk.ToolAnnotations{DestructiveHint: ptr(false), IdempotentHint: true, OpenWorldHint: ptr(false)},
		}, func(ctx context.Context, _ *sdk.CallToolRequest, in pauseAllArgs) (*sdk.CallToolResult, any, error) {
			return pauseAllTool(ctx, a, in), nil, nil
		}),
	}
}

// explain words an error of the duties for a tool result.
func explain(err error) string {
	var pe *ProblemsError
	switch {
	case errors.As(err, &pe):
		return "Invalid request: the duty does not fit.\n- " + strings.Join(pe.Problems, "\n- ")
	case errors.Is(err, ErrHidden), errors.Is(err, ErrNotFound):
		return "Not found: " + reason(err) + ". duties_list names the duties that you can view."
	case errors.Is(err, ErrConflict), errors.Is(err, ErrExists):
		return "Conflict: " + reason(err) + "."
	case errors.Is(err, ErrReadOnly):
		return "Conflict: " + strings.TrimSuffix(reason(err), ".") + "."
	case errors.Is(err, ErrUpstream):
		return "Bad gateway: " + reason(err) + "."
	case errors.Is(err, ErrNoAnswer):
		return "Timeout: " + reason(err) + "."
	}
	var ce *CheckError
	if errors.As(err, &ce) {
		return "Unprocessable: " + ce.Reason
	}
	return mcp.Explain(err)
}

// off is the result of a tool in a process without duties.
func off() *sdk.CallToolResult {
	return mcp.ErrorResult("The duties are off in this process. They run on a server that serves, with the bus nats or mem.")
}

// listTool is the tool duties_list.
func listTool(ctx context.Context, a *fw.Agent) *sdk.CallToolResult {
	ctx, _, _ = manager.OnBehalf(ctx)
	s := Of(a)
	if s == nil {
		return off()
	}
	ans, err := s.List(ctx)
	if err != nil {
		return mcp.ErrorResult(explain(err))
	}
	var b strings.Builder
	switch n := len(ans.Duties); n {
	case 0:
		b.WriteString("You can view no duty.")
	case 1:
		b.WriteString("1 duty:")
	default:
		fmt.Fprintf(&b, "%d duties:", n)
	}
	for _, v := range ans.Duties {
		b.WriteString("\n- " + viewLine(v))
	}
	if ans.Paused {
		b.WriteString("\nThe switch pauses every duty")
		if ans.Reason != "" {
			b.WriteString(": " + ans.Reason)
		}
		b.WriteString(".")
	}
	return mcp.Result(b.String(), ans)
}

// viewLine says a duty in one line.
func viewLine(v View) string {
	name := v.Name
	if v.Title != "" {
		name = fmt.Sprintf("%q, %s,", v.Title, v.Name)
	}
	s := fmt.Sprintf("%s (%s): %s", name, v.Agent, v.Readback)
	if v.Origin != nil {
		s += " " + OriginText
	}
	if v.Locked {
		s += " " + LockedText
	}
	switch {
	case v.Paused && v.State.PausedReason != "":
		s += " Paused: " + v.State.PausedReason
	case v.Paused:
		s += " Paused."
	case len(v.Next) > 0:
		s += " Next: " + timeWords(&v.Duty, v.Next[0]) + "."
	}
	if r := v.State.LastRun; r != nil {
		s += fmt.Sprintf(" Last start: %s, %s", timeWords(&v.Duty, r.Slot), r.State)
		if r.Reason != "" {
			s += " (" + r.Reason + ")"
		}
		s += "."
	}
	if v.State.TodayUSD > 0 {
		s += fmt.Sprintf(" Spent today: $%.2f.", v.State.TodayUSD)
	}
	return s
}

// timeWords writes a time in the zone of a duty.
func timeWords(d *Duty, t time.Time) string {
	loc := d.zone()
	return t.In(loc).Format("Mon Jan 2, 3:04 PM") + " " + schedule.ZoneWords(loc.String())
}

// getTool is the tool duty_get.
func getTool(ctx context.Context, a *fw.Agent, in nameArgs) *sdk.CallToolResult {
	ctx, _, _ = manager.OnBehalf(ctx)
	s := Of(a)
	if s == nil {
		return off()
	}
	v, err := s.Get(ctx, in.Name)
	if err != nil {
		return mcp.ErrorResult(explain(err))
	}
	var b strings.Builder
	b.WriteString("The duty " + viewLine(v))
	if v.ForDevelopers != "" {
		b.WriteString("\nFor developers: " + v.ForDevelopers)
	}
	if len(v.Next) > 1 {
		times := make([]string, 0, len(v.Next))
		for _, t := range v.Next {
			times = append(times, timeWords(&v.Duty, t))
		}
		b.WriteString("\nThe next times: " + strings.Join(times, "; ") + ".")
	}
	if len(v.History) > 0 {
		b.WriteString("\nThe last starts, newest first:")
		for i, r := range v.History {
			if i == 10 {
				fmt.Fprintf(&b, "\n- and %d more in the structured result", len(v.History)-10)
				break
			}
			fmt.Fprintf(&b, "\n- %s: %s", timeWords(&v.Duty, r.Slot), r.State)
			if r.Run != "" {
				b.WriteString(", run " + r.Run)
			}
			if r.CostUSD > 0 {
				fmt.Fprintf(&b, ", $%.2f", r.CostUSD)
			}
			if r.Reason != "" {
				b.WriteString(". " + r.Reason)
			}
		}
	}
	return mcp.Result(b.String(), v)
}

// putTool is the tool duty_put.
func putTool(ctx context.Context, a *fw.Agent, in putArgs) *sdk.CallToolResult {
	ctx, _, _ = manager.OnBehalf(ctx)
	s := Of(a)
	if s == nil {
		return off()
	}
	var body DutyBody
	dec := json.NewDecoder(bytes.NewReader(objectOf(in.Duty)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		return mcp.ErrorResult("Invalid request: the duty does not read: " + err.Error() + ".")
	}
	if in.Test && !in.DryRun {
		return mcp.ErrorResult("Invalid request: test runs with dryRun only.")
	}
	ans, err := s.Put(ctx, in.Name, body, PutOptions{DryRun: in.DryRun, Test: in.Test, Zone: in.TimeZone})
	if err != nil {
		return mcp.ErrorResult(explain(err))
	}
	if d := ans.Dry; d != nil {
		return mcp.Result(dryText(in.Name, d), putResult{DryRun: d})
	}
	verb := "Changed"
	if ans.Created {
		verb = "Created"
	}
	return mcp.Result(verb+" the duty "+viewLine(*ans.View), putResult{Duty: ans.View, Created: ans.Created})
}

// objectOf returns the JSON object of the field duty. A model sometimes
// sends the object as a JSON text, so a text that holds an object reads
// as that object.
func objectOf(raw json.RawMessage) json.RawMessage {
	var text string
	if json.Unmarshal(raw, &text) != nil {
		return raw
	}
	if t := strings.TrimSpace(text); strings.HasPrefix(t, "{") {
		return json.RawMessage(t)
	}
	return raw
}

// dryText words the answer of a dry run.
func dryText(name string, d *DryAnswer) string {
	var b strings.Builder
	b.WriteString("Dry run of the duty " + name + ": nothing is saved.")
	if d.Readback != "" {
		b.WriteString("\nReadback: " + d.Readback)
	}
	if d.ForDevelopers != "" {
		b.WriteString("\nFor developers: " + d.ForDevelopers)
	}
	if len(d.Next) > 0 {
		times := make([]string, 0, len(d.Next))
		for _, t := range d.Next {
			times = append(times, timeWords(&d.Duty, t))
		}
		b.WriteString("\nNext: " + strings.Join(times, "; ") + ".")
	}
	if len(d.Problems) == 0 {
		b.WriteString("\nNo problem: show the readback to the person, and save with duty_put after the person agrees.")
	} else {
		b.WriteString("\nProblems:\n- " + strings.Join(d.Problems, "\n- "))
	}
	if c := d.Check; c != nil {
		fmt.Fprintf(&b, "\nThe check finds %s now", itemsWords(c.Count))
		if c.Wakes {
			b.WriteString(", and they wake the agent.")
		} else {
			b.WriteString(". " + c.Reason)
		}
	}
	return b.String()
}

// runNowTool is the tool duty_run_now.
func runNowTool(ctx context.Context, a *fw.Agent, in nameArgs) *sdk.CallToolResult {
	ctx, _, _ = manager.OnBehalf(ctx)
	s := Of(a)
	if s == nil {
		return off()
	}
	out, err := s.RunNow(ctx, in.Name)
	if err != nil {
		return mcp.ErrorResult(explain(err))
	}
	switch {
	case out.Run != "":
		return mcp.Result(fmt.Sprintf("The duty %s started the run %s. run_status follows it.", in.Name, out.Run), out)
	case out.Waiting != "":
		return mcp.Result("The duty "+in.Name+" waits: "+out.Waiting, out)
	case out.Failed != "":
		return mcp.ErrorResult("Bad gateway: " + out.Failed)
	case out.Skipped != "":
		return mcp.Result("The duty "+in.Name+" did not start: "+out.Skipped, out)
	}
	return mcp.Result("The duty "+in.Name+" did its action.", out)
}

// pauseTool is the tool duty_pause.
func pauseTool(ctx context.Context, a *fw.Agent, in pauseArgs) *sdk.CallToolResult {
	ctx, _, _ = manager.OnBehalf(ctx)
	s := Of(a)
	if s == nil {
		return off()
	}
	v, err := s.SetPaused(ctx, in.Name, in.Paused)
	if err != nil {
		return mcp.ErrorResult(explain(err))
	}
	verb := "Resumed"
	if in.Paused {
		verb = "Paused"
	}
	return mcp.Result(verb+" the duty "+viewLine(v), v)
}

// deleteTool is the tool duty_delete.
func deleteTool(ctx context.Context, a *fw.Agent, in nameArgs) *sdk.CallToolResult {
	ctx, _, _ = manager.OnBehalf(ctx)
	s := Of(a)
	if s == nil {
		return off()
	}
	if err := s.Delete(ctx, in.Name); err != nil {
		return mcp.ErrorResult(explain(err))
	}
	return mcp.Result("Deleted the duty "+in.Name+". Runs that work go on, and its history stays for 30 days.", nameResult{Name: in.Name, Deleted: true})
}

// pauseAllTool is the tool duties_pause_all.
func pauseAllTool(ctx context.Context, a *fw.Agent, in pauseAllArgs) *sdk.CallToolResult {
	ctx, _, _ = manager.OnBehalf(ctx)
	s := Of(a)
	if s == nil {
		return off()
	}
	sw, err := s.PauseAll(ctx, in.Paused, in.Reason)
	if err != nil {
		return mcp.ErrorResult(explain(err))
	}
	text := "Every duty runs again. The times of the pause do not run."
	if sw.Paused {
		text = "Every duty is paused. Runs that work go on."
	}
	return mcp.Result(text, sw)
}

// writeCheckTool is the tool duty_write_check.
func writeCheckTool(ctx context.Context, a *fw.Agent, in writeCheckArgs) *sdk.CallToolResult {
	ctx, _, _ = manager.OnBehalf(ctx)
	s := Of(a)
	if s == nil {
		return off()
	}
	ans, err := s.WriteCheck(ctx, in.Name, WriteCheckBody{Words: in.Words, Agent: in.Agent, Budget: in.Budget})
	if err != nil {
		return mcp.ErrorResult(explain(err))
	}
	var b strings.Builder
	b.WriteString("The check of the duty " + in.Name + ", which nothing saved yet: " + ans.Readback)
	if ans.ForDevelopers != "" {
		b.WriteString(" " + ans.ForDevelopers)
	}
	switch c := ans.Check; {
	case c == nil:
		b.WriteString("\nThe server cannot run this look yet, so it did not test the check.")
	case c.Wakes:
		fmt.Fprintf(&b, "\nNow the check finds %s, and they wake the agent.", itemsWords(c.Count))
	default:
		b.WriteString("\nNow: " + c.Reason)
	}
	b.WriteString("\nShow the readback to the person, and put checkFirst in the duty with duty_put after the person agrees.")
	return mcp.Result(b.String(), ans)
}
