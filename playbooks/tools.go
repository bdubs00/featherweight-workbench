package playbooks

// This file holds the MCP tools of the playbooks (docs/ARCHITECTURE.md
// section 24.2), which the package adds to the MCP server of the
// workbench through mcp.RegisterTools. Each tool calls the Go function of
// its route, with the same checks of access and the same errors. For a
// call of a run of the fleet that names its person, the tools act as that
// person (manager.OnBehalf).

import (
	"bytes"
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
	ToolList   = "playbooks_list"
	ToolGet    = "playbook_get"
	ToolPut    = "playbook_put"
	ToolDelete = "playbook_delete"
)

const (
	nameSchema = `{"type":"string","minLength":1,"maxLength":63,"pattern":"^[a-z0-9_-]+$","description":"The name of the playbook: 1 to 63 of a-z, 0-9, '-', and '_', such as weekly-patching."}`

	emptySchema = `{"type":"object","properties":{}}`

	nameOnlySchema = `{"type":"object","properties":{"name":` + nameSchema + `},"required":["name"]}`

	putSchema = `{"type":"object","properties":{
		"name":` + nameSchema + `,
		"playbook":{"type":"object","description":"The playbook: the plan of a bridge without a title. goal is the goal of the whole job. placement is queue or spawn. seats lists the seats, each with handle, agent, task, and optionally pool and budget {maxTurns, deadline}. caps {maxMessages, maxFanoutPerMinute, maxPairMessages, maxTurns} and delegation {approve, budget, members, report} are partial: a missing key takes the default. A read of playbook_get is a valid body."},
		"revision":{"type":"integer","minimum":1,"description":"The revision of the playbook that you read. The write fails when the playbook changed since."}
	},"required":["name","playbook"]}`
)

type (
	nameArgs struct {
		Name string `json:"name"`
	}
	putArgs struct {
		Name     string          `json:"name"`
		Playbook json.RawMessage `json:"playbook"`
		Revision uint64          `json:"revision,omitempty"`
	}
	listResult struct {
		Playbooks []Summary `json:"playbooks"`
	}
	putResult struct {
		Playbook View `json:"playbook"`
		Created  bool `json:"created,omitempty"`
	}
	deleteResult struct {
		Name    string `json:"name"`
		Deleted bool   `json:"deleted"`
	}
)

func ptr[T any](v T) *T { return &v }

// tools builds the tools of the playbooks for one MCP server. A process
// without playbooks gets none.
func tools(srv *mcp.Server) []mcp.Tool {
	a := srv.Agent()
	if Of(a) == nil {
		return nil
	}
	readOnly := &sdk.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true, OpenWorldHint: ptr(false)}
	return []mcp.Tool{
		mcp.NewTool(&sdk.Tool{
			Name:        ToolList,
			Title:       "List playbooks",
			Description: "List the playbooks that you can read: the saved plans of a bridge, each with its goal, placement, seats, revision, owner, and the time of its last change. A duty opens a bridge from a playbook with do.open.",
			InputSchema: json.RawMessage(emptySchema),
			Annotations: readOnly,
		}, func(ctx context.Context, _ *sdk.CallToolRequest, _ struct{}) (*sdk.CallToolResult, any, error) {
			return listTool(ctx, a), nil, nil
		}),
		mcp.NewTool(&sdk.Tool{
			Name:        ToolGet,
			Title:       "Read a playbook",
			Description: "Read one playbook with its goal, placement, seats, caps, delegation, revision, and owner.",
			InputSchema: json.RawMessage(nameOnlySchema),
			Annotations: readOnly,
		}, func(ctx context.Context, _ *sdk.CallToolRequest, in nameArgs) (*sdk.CallToolResult, any, error) {
			return getTool(ctx, a, in), nil, nil
		}),
		mcp.NewTool(&sdk.Tool{
			Name:        ToolPut,
			Title:       "Create or change a playbook",
			Description: "Save the plan of a bridge under a name, so a person or a duty opens a bridge from it later. It needs the operator role on the agent of each seat, as opening the bridge does. Pass revision to change only the playbook that you read.",
			InputSchema: json.RawMessage(putSchema),
			Annotations: &sdk.ToolAnnotations{DestructiveHint: ptr(true), IdempotentHint: true, OpenWorldHint: ptr(false)},
		}, func(ctx context.Context, _ *sdk.CallToolRequest, in putArgs) (*sdk.CallToolResult, any, error) {
			return putTool(ctx, a, in), nil, nil
		}),
		mcp.NewTool(&sdk.Tool{
			Name:        ToolDelete,
			Title:       "Delete a playbook",
			Description: "Delete a playbook that you can read. A duty that opens it fails at its next start and says why.",
			InputSchema: json.RawMessage(nameOnlySchema),
			Annotations: &sdk.ToolAnnotations{DestructiveHint: ptr(true), IdempotentHint: true, OpenWorldHint: ptr(false)},
		}, func(ctx context.Context, _ *sdk.CallToolRequest, in nameArgs) (*sdk.CallToolResult, any, error) {
			return deleteTool(ctx, a, in), nil, nil
		}),
	}
}

// explain words an error of the playbooks for a tool result.
func explain(err error) string {
	var pe *ProblemsError
	switch {
	case errors.As(err, &pe):
		return "Invalid request: the playbook does not fit.\n- " + strings.Join(pe.Problems, "\n- ")
	case errors.Is(err, ErrNotFound):
		return "Not found: " + reason(err) + ". playbooks_list names the playbooks that you can read."
	case errors.Is(err, ErrConflict):
		return "Conflict: " + reason(err) + "."
	case errors.Is(err, ErrOff):
		return "Unavailable: " + reason(err) + "."
	}
	return mcp.Explain(err)
}

// off is the result of a tool in a process without playbooks.
func off() *sdk.CallToolResult {
	return mcp.ErrorResult("The playbooks are off in this process. They live in a process of fw-workbench serve, with the bus nats or mem.")
}

// summaryLine says a playbook in one line.
func summaryLine(name, goal string, seats int, owner string) string {
	s := fmt.Sprintf("%s: %d %s, owner %s", name, seats, plural(seats, "seat", "seats"), owner)
	if goal != "" {
		s += ". Goal: " + goal
	}
	return s
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// listTool is the tool playbooks_list.
func listTool(ctx context.Context, a *fw.Agent) *sdk.CallToolResult {
	ctx, _, _ = manager.OnBehalf(ctx)
	s := Of(a)
	if s == nil {
		return off()
	}
	list, err := s.List(ctx)
	if err != nil {
		return mcp.ErrorResult(explain(err))
	}
	var b strings.Builder
	if len(list) == 0 {
		b.WriteString("You can read no playbook.")
	} else {
		fmt.Fprintf(&b, "%d %s:", len(list), plural(len(list), "playbook", "playbooks"))
	}
	for _, p := range list {
		b.WriteString("\n- " + summaryLine(p.Name, p.Goal, len(p.Seats), p.Owner))
	}
	return mcp.Result(b.String(), listResult{Playbooks: list})
}

// getTool is the tool playbook_get.
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
	b.WriteString("The playbook " + summaryLine(v.Name, v.Goal, len(v.Seats), v.Owner) + fmt.Sprintf(" (revision %d).", v.Revision))
	for _, m := range v.Seats {
		fmt.Fprintf(&b, "\n- %s (%s): %s", m.Handle, m.Agent, m.Task)
	}
	return mcp.Result(b.String(), v)
}

// putTool is the tool playbook_put.
func putTool(ctx context.Context, a *fw.Agent, in putArgs) *sdk.CallToolResult {
	ctx, _, _ = manager.OnBehalf(ctx)
	s := Of(a)
	if s == nil {
		return off()
	}
	var body Body
	dec := json.NewDecoder(bytes.NewReader(in.Playbook))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		return mcp.ErrorResult("Invalid request: the playbook does not read: " + err.Error() + ".")
	}
	if in.Revision != 0 {
		body.Revision = in.Revision
	}
	v, created, err := s.Put(ctx, in.Name, body)
	if err != nil {
		return mcp.ErrorResult(explain(err))
	}
	verb := "Changed"
	if created {
		verb = "Created"
	}
	return mcp.Result(fmt.Sprintf("%s the playbook %s (revision %d).", verb, summaryLine(v.Name, v.Goal, len(v.Seats), v.Owner), v.Revision), putResult{Playbook: v, Created: created})
}

// deleteTool is the tool playbook_delete.
func deleteTool(ctx context.Context, a *fw.Agent, in nameArgs) *sdk.CallToolResult {
	ctx, _, _ = manager.OnBehalf(ctx)
	s := Of(a)
	if s == nil {
		return off()
	}
	if err := s.Delete(ctx, in.Name); err != nil {
		return mcp.ErrorResult(explain(err))
	}
	return mcp.Result("Deleted the playbook "+in.Name+".", deleteResult{Name: in.Name, Deleted: true})
}
