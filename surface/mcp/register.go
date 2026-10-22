package mcp

// This file holds the two seams through which another package of the
// workbench adds to the MCP server without an edit of this package.
//
// RegisterTools adds tools. A package calls it in init with a function
// that builds its tools for one server, so a program that links the
// package serves them, as native.Extend does for routes. The function
// gets the server, and through it the agent, and returns the tools. A
// tool keeps the rules of the tools of this package: readOnlyHint and
// destructiveHint set right, an error result that says what to do, a text
// for the model, and structuredContent for a page.
//
// CallRun reads the run behind a tools/call: the keys that the MCP client
// of the kernel sends in _meta (the kernel architecture, section 7.3).
// The server trusts the keys only when the caller is the static token of
// the fleet, so a person cannot pose as a run.

import (
	"context"
	"log/slog"
	"slices"
	"sync"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/internal/behalf"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Server is the MCP server that a package adds its tools to through
// RegisterTools.
type Server struct{ s *server }

// Agent returns the agent of the server.
func (x *Server) Agent() *fw.Agent { return x.s.a }

// Logger returns the logger of the server.
func (x *Server) Logger() *slog.Logger { return x.s.log }

// Tool is one tool that a package adds to the server. Build it with
// NewTool.
type Tool struct {
	name string
	add  func(*sdk.Server)
}

// Name returns the name of the tool.
func (t Tool) Name() string { return t.name }

// NewTool makes a Tool from its declaration and its handler, as
// sdk.AddTool takes them. The SDK checks the arguments against the input
// schema of t and decodes them into In before it calls h.
func NewTool[In, Out any](t *sdk.Tool, h sdk.ToolHandlerFor[In, Out]) Tool {
	return Tool{name: t.Name, add: func(s *sdk.Server) { sdk.AddTool(s, t, h) }}
}

// Tools builds the tools of a package for one server. It returns no tool
// when the package has nothing to offer on that server, for example when
// its store is off.
type Tools func(s *Server) []Tool

var registry struct {
	mu    sync.Mutex
	tools []Tools
}

// RegisterTools registers a function that builds tools for each MCP
// server. A package calls it in init. NewServer calls each function after
// it adds its own tools, in the order of registration. A tool whose name
// another tool already has is left out, and the log says so.
func RegisterTools(f Tools) {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	registry.tools = append(registry.tools, f)
}

// registeredTools returns the registered functions.
func registeredTools() []Tools {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	return slices.Clone(registry.tools)
}

// addRegisteredTools adds the tools of the registered functions. An
// exposed tool of mcp.expose never takes the name of one of them.
func (s *server) addRegisteredTools() {
	x := &Server{s: s}
	for _, build := range registeredTools() {
		for _, t := range build(x) {
			if t.add == nil || t.name == "" {
				continue
			}
			if s.reserved(t.name) {
				s.log.Warn("mcp leaves out a registered tool whose name another tool has", "tool", t.name)
				continue
			}
			t.add(s.srv)
			s.registered[t.name] = true
		}
	}
}

// Result is the result of a tool: a text for the model and a structured
// value for a page.
func Result(text string, structured any) *sdk.CallToolResult { return result(text, structured) }

// ErrorResult is the error result of a tool. The text says what went
// wrong and what to do.
func ErrorResult(msg string) *sdk.CallToolResult { return errorResult(msg) }

// Explain words an error of the kernel for a tool result, as the tools of
// this package word it: a refusal names the role to ask for, and an
// authorization service that does not answer says to try again.
func Explain(err error) string { return explain(err) }

// Keys of _meta of a tools/call that the MCP client of the kernel sends
// (the kernel architecture, section 7.3). The principal keys go only to a
// server that the tools.mcp entry of the worker marks trusted.
const (
	MetaRun           = "io.featherweight/run"
	MetaAgent         = "io.featherweight/agent"
	MetaNode          = "io.featherweight/node"
	MetaPrincipal     = "io.featherweight/principal"
	MetaPrincipalKind = "io.featherweight/principalKind"
)

// RunMeta is the run behind a tools/call, as CallRun reads it: the id of
// the run, its profile, the node name of the process that holds it, and
// the person of the run with the kind of that principal. Principal and
// PrincipalKind are empty for a run without a person and for a server that
// the worker does not trust. The package internal/behalf holds the type,
// so the manager and the fleet read the same keys.
type RunMeta = behalf.Run

// withCallMeta puts the run keys of the _meta of a tools/call into the
// context of the call, for CallRun.
func withCallMeta(next sdk.MethodHandler) sdk.MethodHandler {
	return func(ctx context.Context, method string, req sdk.Request) (sdk.Result, error) {
		if method == "tools/call" {
			if p, ok := req.GetParams().(*sdk.CallToolParamsRaw); ok && len(p.Meta) > 0 {
				if m := runMetaOf(p.Meta); m.Run != "" {
					ctx = behalf.WithRun(ctx, m)
				}
			}
		}
		return next(ctx, method, req)
	}
}

// runMetaOf reads the run keys of _meta. A key whose value is not a string
// is left out.
func runMetaOf(meta map[string]any) RunMeta {
	str := func(key string) string {
		v, _ := meta[key].(string)
		return v
	}
	return RunMeta{
		Run:           str(MetaRun),
		Agent:         str(MetaAgent),
		Node:          str(MetaNode),
		Principal:     str(MetaPrincipal),
		PrincipalKind: str(MetaPrincipalKind),
	}
}

// CallRun returns the run behind the tools/call of ctx and reports whether
// the call names one. It reports false unless the caller of the MCP
// request is the static token of the fleet (principal kind token), because
// only a worker of the fleet holds that token: for a person, a workload,
// or a call over stdio, the keys of _meta say nothing that the server can
// believe. It also reports false when _meta names no run. A context in
// which the server acts for the person of the run (section 19 of
// docs/ARCHITECTURE.md) still names the run, because its caller is the
// token.
func CallRun(ctx context.Context) (RunMeta, bool) { return behalf.Trusted(ctx) }
