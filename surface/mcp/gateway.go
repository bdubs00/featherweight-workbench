package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"reflect"
	"strings"
	"sync"

	"github.com/bdubs00/featherweight-kernel/fw"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// gateway re-exports the tools that mcp.expose names under their fw names.
// A call passes through the agent policy first. The tool list follows the
// tool sources: a tools/list, and a tools/call of a name that is not a work
// tool, adds new tools and removes gone ones before the SDK answers.
type gateway struct {
	s     *server
	globs []string

	mu    sync.Mutex
	specs map[string]fw.ToolSpec // exposed tools by name
}

func newGateway(s *server, globs []string) *gateway {
	return &gateway{s: s, globs: globs, specs: map[string]fw.ToolSpec{}}
}

// middleware syncs the exposed tools before the SDK reads its tool list.
func (g *gateway) middleware(next sdk.MethodHandler) sdk.MethodHandler {
	return func(ctx context.Context, method string, req sdk.Request) (sdk.Result, error) {
		switch method {
		case "tools/list":
			g.sync(ctx)
		case "tools/call":
			if p, ok := req.GetParams().(*sdk.CallToolParamsRaw); ok && !g.s.reserved(p.Name) {
				g.sync(ctx)
			}
		}
		return next(ctx, method, req)
	}
}

func (g *gateway) matches(name string) bool {
	for _, glob := range g.globs {
		glob = strings.TrimSpace(glob)
		if glob == "*" || glob == name {
			return true
		}
		if ok, err := path.Match(glob, name); err == nil && ok {
			return true
		}
	}
	return false
}

// sync makes the exposed tools of the server match the tools of the agent.
func (g *gateway) sync(ctx context.Context) {
	tools, err := g.s.a.Tools(ctx)
	if err != nil {
		g.s.log.Warn("mcp gateway cannot list tools", "error", err)
		return
	}
	want := map[string]fw.ToolSpec{}
	for _, t := range tools {
		spec := t.Spec()
		if !g.matches(spec.Name) {
			continue
		}
		if g.s.reserved(spec.Name) {
			g.s.log.Warn("mcp gateway skips a tool whose name is a work tool", "tool", spec.Name)
			continue
		}
		if _, dup := want[spec.Name]; !dup {
			want[spec.Name] = spec
		}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	for name, spec := range want {
		if old, ok := g.specs[name]; ok && reflect.DeepEqual(old, spec) {
			continue
		}
		if !g.add(spec) {
			delete(want, name)
		}
	}
	var gone []string
	for name := range g.specs {
		if _, ok := want[name]; !ok {
			gone = append(gone, name)
		}
	}
	if len(gone) > 0 {
		g.s.srv.RemoveTools(gone...)
	}
	g.specs = want
}

// add adds one exposed tool. The SDK panics on a schema that is not an
// object, so add reports false for a tool it cannot add.
func (g *gateway) add(spec fw.ToolSpec) (ok bool) {
	defer func() {
		if r := recover(); r != nil {
			g.s.log.Warn("mcp gateway cannot expose a tool", "tool", spec.Name, "error", fmt.Sprint(r))
			ok = false
		}
	}()
	schema := spec.InputSchema
	var probe struct {
		Type any `json:"type"`
	}
	if len(schema) == 0 || json.Unmarshal(schema, &probe) != nil || probe.Type != "object" {
		schema = json.RawMessage(`{"type":"object"}`)
	}
	g.s.srv.AddTool(&sdk.Tool{
		Name:        spec.Name,
		Description: spec.Description,
		InputSchema: schema,
		Annotations: &sdk.ToolAnnotations{
			ReadOnlyHint:    spec.ReadOnly,
			DestructiveHint: ptr(spec.Destructive),
			IdempotentHint:  spec.Idempotent,
		},
	}, g.call)
	return true
}

// lookup finds an exposed tool by name among the current tools of the agent.
func (g *gateway) lookup(ctx context.Context, name string) (fw.Tool, error) {
	if !g.matches(name) || g.s.reserved(name) {
		return nil, nil
	}
	tools, err := g.s.a.Tools(ctx)
	if err != nil {
		return nil, err
	}
	for _, t := range tools {
		if t.Spec().Name == name {
			return t, nil
		}
	}
	return nil, nil
}

// call runs an exposed tool after the policy allows it. The gateway has no
// run to hold, so a verdict of ask refuses the call.
func (g *gateway) call(ctx context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
	name := req.Params.Name
	t, err := g.lookup(ctx, name)
	if err != nil {
		return errorResult(fmt.Sprintf("The tool %s is not available: %v", name, err)), nil
	}
	if t == nil {
		return errorResult(fmt.Sprintf("The tool %s is not available through the gateway.", name)), nil
	}
	input := json.RawMessage(req.Params.Arguments)
	if len(input) == 0 || string(input) == "null" {
		input = json.RawMessage(`{}`)
	}
	principal := principalOf(ctx, req)
	d := g.s.a.Policy().Decide(ctx, fw.ResolveCall(ctx, t, fw.ToolCall{CallID: fw.NewID(), Spec: t.Spec(), Input: input}))
	log := g.s.log.With("tool", name, "principal", principal, "verdict", d.Verdict.String())
	switch d.Verdict {
	case fw.VerdictDeny:
		log.Info("mcp gateway call denied", "reason", d.Reason)
		return errorResult(fmt.Sprintf("The policy denies the tool %s (%s).", name, d.Reason)), nil
	case fw.VerdictAsk:
		log.Info("mcp gateway call needs an approval", "reason", d.Reason)
		return errorResult(fmt.Sprintf("The tool %s needs an approval (%s), so it is not available through the gateway. "+
			"Start a run with the run tool instead, and approve the call there.", name, d.Reason)), nil
	}
	tctx := fw.WithAgent(ctx, g.s.a)
	if tok := req.Params.GetProgressToken(); tok != nil {
		tctx = fw.WithProgress(tctx, func(progress, total float64, message string) {
			_ = req.Session.NotifyProgress(ctx, &sdk.ProgressNotificationParams{
				ProgressToken: tok, Progress: progress, Total: total, Message: message,
			})
		})
	}
	res, err := t.Call(tctx, input)
	if err != nil {
		log.Warn("mcp gateway call failed", "error", err)
		return errorResult(fmt.Sprintf("The tool %s failed: %v", name, err)), nil
	}
	log.Info("mcp gateway call", "isError", res.IsError)
	return toolResult(res), nil
}

// toolResult maps an fw tool result to an MCP tool result.
func toolResult(res fw.ToolResult) *sdk.CallToolResult {
	out := &sdk.CallToolResult{IsError: res.IsError, Content: []sdk.Content{}}
	for _, b := range res.Content {
		switch {
		case b.Type == fw.BlockText:
			out.Content = append(out.Content, &sdk.TextContent{Text: b.Text})
		case b.Type == fw.BlockImage && len(b.Data) > 0:
			out.Content = append(out.Content, &sdk.ImageContent{Data: b.Data, MIMEType: b.MediaType})
		case b.URL != "":
			out.Content = append(out.Content, &sdk.TextContent{Text: fmt.Sprintf("[%s %s]", b.Type, b.URL)})
		default:
			out.Content = append(out.Content, &sdk.TextContent{Text: fmt.Sprintf("[%s %s, %d bytes]", b.Type, b.MediaType, len(b.Data))})
		}
	}
	if len(res.Structured) > 0 {
		out.StructuredContent = res.Structured
	}
	return out
}
