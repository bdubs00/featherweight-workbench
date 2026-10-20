package mcp

// This file holds the tools of the work view that a host that shows only
// text needs to do the work of the page: run_result reads what a run
// produced, agent_builder starts the conversation that creates an agent,
// and capabilities lists the tools of the runtime. Each answer comes from
// the view models of surface/dashboard, so the page and the text say the
// same thing.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/surface/view"
	"github.com/bdubs00/featherweight-workbench/dashboard"
	"github.com/bdubs00/featherweight-workbench/fleet"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Names of the tools of the work view.
const (
	toolRunResult    = "run_result"
	toolAgentBuilder = "agent_builder"
	toolCapabilities = "capabilities"
)

// Views of the results of the work view.
const (
	viewResult       = "result"
	viewCapabilities = "capabilities"
)

// Input schemas of the tools of the work view.
const (
	runResultSchema = `{"type":"object","properties":{"runId":` + runIDSchema + `},"required":["runId"]}`

	agentBuilderSchema = `{"type":"object","properties":{
		"input":{"type":"string","minLength":1,"description":"What the new agent handles and what it must never do, in your words. It is the first message to the builder."},
		"fromRun":{"type":"string","minLength":1,"description":"The id of a run to turn into an agent: a completed top-level run that you started, when you can create agents. The builder starts from what the run asked, its last answer, and the tools that it used."}
	}}`
)

// builderDescription tells a host that shows only text how to go on after
// agent_builder.
const builderDescription = "Start a conversation with the agent builder, which helps a person create an agent. " +
	"Pass exactly one of input (what the agent handles) and fromRun (a run to turn into an agent). " +
	"The call returns the run of the builder at once, like run with wait false. The builder never saves anything. " +
	"To go on without the page: answer the questions of the builder with send on that run, " +
	"read its latest proposal with run_result or run_events, where the item of the tool propose_agent carries profile, " +
	"then create the agent with agent_put from that profile, or check it first with agent_put and dryRun true."

type (
	builderArgs struct {
		Input   string `json:"input,omitempty"`
		FromRun string `json:"fromRun,omitempty"`
	}
	resultView struct {
		View   string           `json:"view"`
		Result dashboard.Result `json:"result"`
	}
	capabilitiesResult struct {
		View  string            `json:"view"`
		Tools []view.Capability `json:"tools"`
	}
)

// addWorkViewTools adds run_result, agent_builder, and capabilities.
// readOnly gives the annotations of a tool that reads.
func (s *server) addWorkViewTools(readOnly func() *sdk.ToolAnnotations) {
	sdk.AddTool(s.srv, &sdk.Tool{
		Name:  toolRunResult,
		Title: "Run result",
		Description: "Return what a run produced and what it needs next, in the order that a person reads it: the title and the status, " +
			"the answer of the agent, the files that the run changed, the steps that ran with their outcomes, the decision that waits, " +
			"the limits that affect the run, and the next actions. A run of another fleet member is read from that member and built here.",
		InputSchema: json.RawMessage(runResultSchema),
		Annotations: readOnly(),
	}, s.runResultTool)
	sdk.AddTool(s.srv, &sdk.Tool{
		Name:        toolAgentBuilder,
		Title:       "Build an agent",
		Description: builderDescription,
		InputSchema: json.RawMessage(agentBuilderSchema),
		Annotations: &sdk.ToolAnnotations{DestructiveHint: ptr(false), OpenWorldHint: ptr(false)},
	}, s.agentBuilder)
	sdk.AddTool(s.srv, &sdk.Tool{
		Name:  toolCapabilities,
		Title: "Capabilities",
		Description: "List the tools of the runtime that you can learn about, each with what it does in plain words, its source " +
			"(built-in, or the MCP server or peer that serves it), and its annotations. An agent can use the tools that its profile allows; " +
			"agent_get says what one agent can do.",
		InputSchema: json.RawMessage(emptySchema),
		Annotations: readOnly(),
	}, s.capabilities)
}

func (s *server) runResultTool(ctx context.Context, req *sdk.CallToolRequest, in runIDArgs) (*sdk.CallToolResult, any, error) {
	ctx = withCredential(ctx, req)
	res, err := dashboard.ResultOf(ctx, s.a, in.RunID)
	if errors.Is(err, fw.ErrRunNotFound) {
		member, ferr := s.ownerOf(ctx, in.RunID)
		switch {
		case ferr != nil:
			err = ferr
		case member != "":
			res, err = s.memberResult(ctx, member, in.RunID)
		}
	}
	if err != nil {
		return failResult(err), nil, nil
	}
	return result(res.String(), resultView{View: viewResult, Result: res}), nil, nil
}

// memberResult builds the result of a run of another member from the run
// and the events of its owner, read with the credential of the caller. A
// worker serves the native API of the kernel alone and no result route,
// so the result is built here, as GET /v1/runs/{id}/result builds it.
func (s *server) memberResult(ctx context.Context, member, id string) (dashboard.Result, error) {
	run, events, err := fleet.MemberLedger(ctx, fleet.Of(s.a), member, id)
	if err != nil {
		return dashboard.Result{}, memberErr(err)
	}
	return dashboard.ResultFrom(ctx, s.a, run, events)
}

func (s *server) agentBuilder(ctx context.Context, req *sdk.CallToolRequest, in builderArgs) (*sdk.CallToolResult, any, error) {
	input, from := strings.TrimSpace(in.Input), strings.TrimSpace(in.FromRun)
	if (input == "") == (from == "") {
		return errorResult("Invalid request: pass exactly one of input and fromRun."), nil, nil
	}
	ctx = withCredential(ctx, req)
	if from != "" {
		text, err := dashboard.SaveAsAgentInput(ctx, s.a, from)
		if errors.Is(err, fw.ErrRunNotFound) {
			member, ferr := s.ownerOf(ctx, from)
			switch {
			case ferr != nil:
				err = ferr
			case member != "":
				// A worker serves no builder route, so the first message
				// comes from the ledger of the owner, and the builder runs
				// here.
				text, err = dashboard.SaveAsAgentInputOn(ctx, s.a, fleet.Of(s.a), member, from)
				err = memberErr(err)
			}
		}
		if err != nil {
			return failResult(err), nil, nil
		}
		input = text
	}
	principal := principalOf(ctx, req)
	opts := []fw.RunOption{fw.WithRequest(dashboard.BuilderRequest(input)), fw.WithOrigin(fw.OriginMCP)}
	if principal != "" {
		opts = append(opts, fw.WithPrincipalName(principal))
	}
	r, err := s.a.Start(ctx, input, opts...)
	if err != nil {
		return workError("The agent builder did not start: ", err), nil, nil
	}
	s.log.Info("mcp agent builder started", "run", r.ID, "from_run", from, "principal", principal)
	return builderResult(r), nil, nil
}

// builderResult is the result of agent_builder: the run object, and a text
// that says how to go on.
func builderResult(r *fw.Run) *sdk.CallToolResult {
	res := runResult(r)
	where := ""
	if r.Member != "" {
		where = " on the fleet member " + r.Member
	}
	res.Content = []sdk.Content{&sdk.TextContent{Text: fmt.Sprintf(
		"Started the agent builder in run %s%s. The run is %s.\n"+
			"Next: read the questions and the proposal of the builder with run_result or run_events (runId %s). "+
			"Answer the builder with send (runId %s). "+
			"When the item of the tool propose_agent carries profile, create the agent with agent_put from that profile, "+
			"or check it first with agent_put and dryRun true. The builder never saves anything.",
		r.ID, where, r.State, r.ID, r.ID)}}
	return res
}

func (s *server) capabilities(ctx context.Context, _ *sdk.CallToolRequest, _ struct{}) (*sdk.CallToolResult, any, error) {
	tools, err := view.Catalog(ctx, s.a)
	if err != nil {
		return failResult(err), nil, nil
	}
	if tools == nil {
		tools = []view.Capability{}
	}
	return result(capabilitiesText(tools), capabilitiesResult{View: viewCapabilities, Tools: tools}), nil, nil
}

// builtin reports whether a tool source names the built-in tools.
func builtin(source string) bool { return source == "" || source == "builtin" }

// capabilitiesText is the catalog as text: the built-in tools first, then
// the tools of each other source in the order of their names, one line per
// tool with its plain phrase and its name.
func capabilitiesText(tools []view.Capability) string {
	if len(tools) == 0 {
		return "The runtime has no tool that you can learn about."
	}
	groups := map[string][]view.Capability{}
	var sources []string
	for _, t := range tools {
		src := t.Source
		if builtin(src) {
			src = ""
		}
		if _, ok := groups[src]; !ok {
			sources = append(sources, src)
		}
		groups[src] = append(groups[src], t)
	}
	slices.Sort(sources) // "" sorts first: the built-in tools lead
	var b strings.Builder
	fmt.Fprintf(&b, "%d tools in the runtime.", len(tools))
	for _, src := range sources {
		if src == "" {
			b.WriteString("\n\nBuilt-in:")
		} else {
			fmt.Fprintf(&b, "\n\nFrom %s:", src)
		}
		for _, t := range groups[src] {
			text := t.Text
			if text == "" {
				text = t.Name
			}
			fmt.Fprintf(&b, "\n- %s (%s)", text, t.Name)
		}
	}
	return b.String()
}
