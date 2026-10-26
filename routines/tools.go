package routines

// This file holds the MCP tools of the routines (docs/ARCHITECTURE.md
// section 21.6), which the package adds to the MCP server of the
// workbench through mcp.RegisterTools: routines_list reads the routines
// of an agent, routine_put proposes or changes one, and routine_standing
// moves one. They call the Go functions of the routes and decide as the
// routes do. Whether a call of routine_put or routine_standing waits for
// a person is the policy of the profile that calls it, such as ask in the
// profile of the manager.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/manager"
	"github.com/bdubs00/featherweight-workbench/surface/mcp"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Names of the tools.
const (
	ToolList     = "routines_list"
	ToolStanding = "routine_standing"
	ToolPut      = "routine_put"
)

const (
	listSchema = `{"type":"object","properties":{
		"agent":{"type":"string","maxLength":64,"description":"The name of the agent profile, such as triage. Without it, the routines of every agent that you can view."},
		"name":{"type":"string","maxLength":64,"description":"One routine of the agent, with what its runs showed. It needs agent."}
	}}`

	standingSchema = `{"type":"object","properties":{
		"agent":{"type":"string","minLength":1,"maxLength":64,"description":"The name of the agent profile, such as triage."},
		"name":{"type":"string","minLength":1,"maxLength":64,"description":"The name of the routine, such as acknowledge-a-new-ticket."},
		"standing":{"type":"string","enum":["rehearsing","live","paused","retired"],"description":"The new standing. rehearsing runs the routine beside the agent without acting. live signs it off, so it acts on its own. paused stops it. retired ends it for good."},
		"note":{"type":"string","maxLength":512,"description":"One line that says why, for the history of the routine, at most 512 bytes."},
		"version":{"type":"integer","minimum":1,"description":"The version of the routine that you read. The move fails when the routine changed since."}
	},"required":["agent","name","standing"]}`

	putSchema = `{"type":"object","properties":{
		"agent":{"type":"string","minLength":1,"maxLength":64,"description":"The name of the agent profile, such as triage."},
		"name":{"type":"string","minLength":1,"maxLength":64,"description":"The name of the routine, such as acknowledge-a-new-ticket."},
		"routine":{"type":"object","description":"The body of the routine in the shape of the kernel: name, decider, questions, steps, after, and answer. routines_list with name shows the body of a routine."},
		"standing":{"type":"string","enum":["proposed","rehearsing","live","paused","retired"],"description":"The standing of a new routine, proposed by default. An operator of the agent proposes a new routine; any other write needs the owner role."},
		"source":{"type":"object","properties":{"runs":{"type":"integer"},"from":{"type":"string"},"to":{"type":"string"},"proposedBy":{"type":"string"}},"description":"Where the routine comes from: the runs that it learned from, their dates, and who proposed it."},
		"replay":{"type":"object","properties":{"matched":{"type":"integer"},"total":{"type":"integer"},"at":{"type":"string"}},"description":"The result of a replay of the routine over past runs."},
		"note":{"type":"string","maxLength":512,"description":"One line that says why, for the history of the routine."}
	},"required":["agent","name","routine"]}`
)

type (
	listArgs struct {
		Agent string `json:"agent"`
		Name  string `json:"name,omitempty"`
	}
	standingArgs struct {
		Agent    string `json:"agent"`
		Name     string `json:"name"`
		Standing string `json:"standing"`
		Note     string `json:"note,omitempty"`
		Version  int    `json:"version,omitempty"`
	}
	listResult struct {
		Routines []Record `json:"routines"`
	}
	standingResult struct {
		Routine Record `json:"routine"`
		Moved   bool   `json:"moved"`
	}
	putArgs struct {
		Agent string `json:"agent"`
		Name  string `json:"name"`
		putBody
	}
	putResult struct {
		Routine Record `json:"routine"`
		Created bool   `json:"created,omitempty"`
		Changed bool   `json:"changed"`
	}
)

func ptr[T any](v T) *T { return &v }

// tools builds the tools of the routines for one MCP server. A process
// without routines gets none.
func tools(s *mcp.Server) []mcp.Tool {
	a := s.Agent()
	if Of(a) == nil {
		return nil
	}
	return []mcp.Tool{
		mcp.NewTool(&sdk.Tool{
			Name:        ToolList,
			Title:       "List routines",
			Description: "List the routines of an agent, or of every agent that you can view: each with its standing (proposed, rehearsing, live, paused, or retired), whether it is ready for a sign-off, its version, and its history. With name, one routine with what its runs showed: per day how many it would have acted on, agreed with the agent on, and handed to the agent, and the last runs where it disagreed.",
			InputSchema: json.RawMessage(listSchema),
			Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true, OpenWorldHint: ptr(false)},
		}, func(ctx context.Context, _ *sdk.CallToolRequest, in listArgs) (*sdk.CallToolResult, any, error) {
			return listTool(ctx, a, in), nil, nil
		}),
		mcp.NewTool(&sdk.Tool{
			Name:        ToolStanding,
			Title:       "Move a routine",
			Description: "Move a routine of an agent to another standing. A proposed routine moves to rehearsing or retired. A rehearsing one moves to live (a sign-off, so it acts on its own), paused, or retired. A live one moves to paused or retired, and a paused one to rehearsing or live. A retired routine stays retired, so a retirement cannot be undone. A sign-off needs a person behind the call, and it writes the runbook page agents/<agent>/routines/<name>.md of the wiki.",
			InputSchema: json.RawMessage(standingSchema),
			Annotations: &sdk.ToolAnnotations{DestructiveHint: ptr(true), IdempotentHint: true, OpenWorldHint: ptr(false)},
		}, func(ctx context.Context, _ *sdk.CallToolRequest, in standingArgs) (*sdk.CallToolResult, any, error) {
			return standingTool(ctx, a, in), nil, nil
		}),
		mcp.NewTool(&sdk.Tool{
			Name:        ToolPut,
			Title:       "Propose or change a routine",
			Description: "Propose a routine of an agent, or change its body, as PUT /v1/routines/{agent}/{name} does. An operator of the agent proposes a new routine, which starts in the standing proposed. A change of the body needs the owner role, and it gives the routine the next version, so a live routine rehearses again. Move a routine with routine_standing.",
			InputSchema: json.RawMessage(putSchema),
			Annotations: &sdk.ToolAnnotations{DestructiveHint: ptr(true), IdempotentHint: true, OpenWorldHint: ptr(false)},
		}, func(ctx context.Context, _ *sdk.CallToolRequest, in putArgs) (*sdk.CallToolResult, any, error) {
			return putTool(ctx, a, in), nil, nil
		}),
	}
}

// putTool is the tool routine_put.
func putTool(ctx context.Context, a *fw.Agent, in putArgs) *sdk.CallToolResult {
	ctx, _, _ = manager.OnBehalf(ctx)
	s := Of(a)
	if s == nil {
		return mcp.ErrorResult("The routines are off in this process. They run on a server that serves, with the bus nats or mem.")
	}
	rec, res, err := put(ctx, a, s, in.Agent, in.Name, in.putBody)
	if err != nil {
		return mcp.ErrorResult(explain(err))
	}
	var text string
	switch {
	case res.Created:
		text = fmt.Sprintf("Created the routine %s of %s.", in.Name, in.Agent)
	case res.Changed:
		text = fmt.Sprintf("Changed the routine %s of %s.", in.Name, in.Agent)
	default:
		text = fmt.Sprintf("The routine %s of %s has this body already. Nothing changed.", in.Name, in.Agent)
	}
	if res.Changed {
		a.Logger().Info("routines: a routine was written", "agent", in.Agent, "routine", in.Name, "version", rec.Version(), "standing", rec.Standing, "created", res.Created)
	}
	return mcp.Result(text+" "+recordText(rec), putResult{Routine: rec, Created: res.Created, Changed: res.Changed})
}

// explain words an error of the routines for a tool result.
func explain(err error) string {
	var me *MoveError
	var ve *VersionError
	switch {
	case errors.As(err, &ve):
		return "Conflict: " + ve.Error() + " with routines_list."
	case errors.As(err, &me):
		return "Conflict: " + me.Error() + "."
	case errors.Is(err, ErrHidden):
		return "Not found: " + reason(err) + ". agents_list names the agents that you can see."
	case errors.Is(err, ErrNotFound):
		return "Not found: " + reason(err) + ". routines_list names the routines of the agent."
	case errors.Is(err, ErrInvalid):
		return "Invalid request: " + reason(err) + "."
	case errors.Is(err, ErrConflict), errors.Is(err, ErrLimit), errors.Is(err, ErrRetired):
		return "Conflict: " + reason(err) + "."
	}
	return mcp.Explain(err)
}

// listTool is the tool routines_list.
func listTool(ctx context.Context, a *fw.Agent, in listArgs) *sdk.CallToolResult {
	ctx, _, _ = manager.OnBehalf(ctx)
	s := Of(a)
	if s == nil {
		return mcp.ErrorResult("The routines are off in this process. They run on a server that serves, with the bus nats or mem.")
	}
	if in.Name != "" {
		if in.Agent == "" {
			return mcp.ErrorResult("Invalid request: name needs agent.")
		}
		if err := allowed(ctx, a, in.Agent, fw.ActionAgentView); err != nil {
			return mcp.ErrorResult(explain(err))
		}
		rec, err := s.Get(ctx, in.Agent, in.Name)
		if err != nil {
			return mcp.ErrorResult(explain(err))
		}
		return mcp.Result(recordText(rec), listResult{Routines: []Record{rec}})
	}
	list, err := visible(ctx, a, s, in.Agent)
	if err != nil {
		return mcp.ErrorResult(explain(err))
	}
	whose := "every agent that you can view"
	if in.Agent != "" {
		whose = in.Agent
	}
	if len(list) == 0 {
		return mcp.Result("No routine of "+whose+".", listResult{Routines: list})
	}
	lines := make([]string, 0, len(list))
	for _, rec := range list {
		line := recordLine(rec)
		if in.Agent == "" {
			line = rec.Agent + ": " + line
		}
		lines = append(lines, "- "+line)
	}
	text := fmt.Sprintf("The routines of %s, the newest change first:\n%s", whose, strings.Join(lines, "\n"))
	return mcp.Result(text, listResult{Routines: list})
}

// standingTool is the tool routine_standing.
func standingTool(ctx context.Context, a *fw.Agent, in standingArgs) *sdk.CallToolResult {
	ctx, _, _ = manager.OnBehalf(ctx)
	s := Of(a)
	if s == nil {
		return mcp.ErrorResult("The routines are off in this process. They run on a server that serves, with the bus nats or mem.")
	}
	rec, moved, err := move(ctx, a, s, in.Agent, in.Name, moveBody{Standing: in.Standing, Note: in.Note, Version: in.Version})
	if err != nil {
		return mcp.ErrorResult(explain(err))
	}
	text := fmt.Sprintf("The routine %s of %s is %s already. Nothing changed.", in.Name, in.Agent, rec.Standing)
	if moved {
		text = fmt.Sprintf("Moved the routine %s of %s to %s.", in.Name, in.Agent, rec.Standing)
		if rec.Standing == Live {
			text += " It acts on its own from the next run that the workers take, and the runbook is the page " + RunbookPath(in.Agent, in.Name) + "."
		}
	}
	return mcp.Result(text, standingResult{Routine: rec, Moved: moved})
}

// recordLine words a routine in one line.
func recordLine(rec Record) string {
	s := fmt.Sprintf("%s, version %d: %s", rec.Name(), rec.Version(), rec.Standing)
	if rec.Ready {
		s += ", ready for a sign-off"
	}
	if so := rec.SignedOff; so != nil && rec.Standing == Live {
		s += fmt.Sprintf(", signed off by %s on %s", so.By, so.At.Format("2006-01-02"))
	}
	return s
}

// recordText words a routine with its evidence.
func recordText(rec Record) string {
	s := "The routine " + recordLine(rec) + "."
	if ev := rec.Evidence; ev != nil {
		t := ev.Totals
		s += fmt.Sprintf("\nOver this version it would have acted on %d runs, agreed with the agent on %d, disagreed on %d, and handed %d to the agent. Live, it completed %d runs, and the agent took over %d. Sent back: %d it completed, %d where it agreed.", t.Acted, t.Agreed, t.Disagreed, t.Handed, t.Applied, t.Failed, ev.SentBack.Applied, ev.SentBack.Agreed)
	}
	return s
}
