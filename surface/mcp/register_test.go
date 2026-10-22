package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/bdubs00/featherweight-kernel/fw"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// withRegistered registers f for one test and restores the registry after
// it, so the other tests see the tools of this package alone.
func withRegistered(t *testing.T, f Tools) {
	t.Helper()
	registry.mu.Lock()
	saved := slices.Clone(registry.tools)
	registry.mu.Unlock()
	RegisterTools(f)
	t.Cleanup(func() {
		registry.mu.Lock()
		registry.tools = saved
		registry.mu.Unlock()
	})
}

// callers maps a bearer token to a principal: two people, the static token
// of the fleet, and a workload.
var callers = tokenMap{
	"alice-token":    {Subject: "alice", Kind: fw.KindUser},
	"fleet-token":    {Subject: fw.KindToken, Kind: fw.KindToken},
	"workload-token": {Subject: "spiffe://td/ns/fw/sa/worker", Kind: fw.KindWorkload},
}

type echoArgs struct {
	Text string `json:"text"`
}

// echoTools builds the tool echo_run, which answers the run of its call,
// and a tool named run, which the server must leave out.
func echoTools(s *Server) []Tool {
	if s.Agent() == nil || s.Logger() == nil {
		return nil
	}
	echo := NewTool(&sdk.Tool{
		Name:        "echo_run",
		Title:       "Echo the run",
		Description: "Return the run behind the call.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"text":{"type":"string","description":"Any text."}}}`),
		Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true, OpenWorldHint: ptr(false)},
	}, func(ctx context.Context, _ *sdk.CallToolRequest, in echoArgs) (*sdk.CallToolResult, any, error) {
		m, ok := CallRun(ctx)
		if !ok {
			return ErrorResult("No run: " + in.Text), nil, nil
		}
		return Result("run "+m.Run+" of "+m.Agent, m), nil, nil
	})
	clash := NewTool(&sdk.Tool{
		Name:        toolRun,
		Description: "A tool that takes the name of a work tool.",
		InputSchema: json.RawMessage(`{"type":"object"}`),
	}, func(context.Context, *sdk.CallToolRequest, struct{}) (*sdk.CallToolResult, any, error) {
		return Result("clash", nil), nil, nil
	})
	return []Tool{echo, clash}
}

// A package adds its tools through RegisterTools, and a registered tool
// never replaces a tool of the server.
func TestRegisterTools(t *testing.T) {
	withRegistered(t, echoTools)
	ctx := testCtx(t)
	a := newAgent(t, newScript().model(), fw.WithAuthn(callers))
	url := serveAgent(t, a, nil).URL
	cs := as(t, ctx, url, "alice-token")
	res, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var echo, run *sdk.Tool
	for _, tool := range res.Tools {
		switch tool.Name {
		case "echo_run":
			echo = tool
		case toolRun:
			run = tool
		}
	}
	if echo == nil || echo.Annotations == nil || !echo.Annotations.ReadOnlyHint {
		t.Fatalf("echo_run = %+v", echo)
	}
	if run == nil || !strings.HasPrefix(run.Description, "Start a run") {
		t.Fatalf("the registered tool replaced run: %+v", run)
	}
}

// CallRun trusts the keys of _meta only from the static token of the
// fleet. A person and a workload that send them get no run.
func TestCallRun(t *testing.T) {
	withRegistered(t, echoTools)
	ctx := testCtx(t)
	a := newAgent(t, newScript().model(), fw.WithAuthn(callers))
	url := serveAgent(t, a, nil).URL
	meta := sdk.Meta{
		MetaRun:           "r-1",
		MetaAgent:         "reviewer",
		MetaNode:          "default/crew-0",
		MetaPrincipal:     "alice",
		MetaPrincipalKind: fw.KindUser,
	}
	call := func(token string, meta sdk.Meta) *sdk.CallToolResult {
		t.Helper()
		cs := as(t, ctx, url, token)
		res, err := cs.CallTool(ctx, &sdk.CallToolParams{Meta: meta, Name: "echo_run", Arguments: map[string]any{"text": token}})
		if err != nil {
			t.Fatalf("call as %s: %v", token, err)
		}
		return res
	}
	res := call("fleet-token", meta)
	if res.IsError {
		t.Fatalf("the token got no run: %s", textOf(res))
	}
	var got RunMeta
	structured(t, res, &got)
	want := RunMeta{Run: "r-1", Agent: "reviewer", Node: "default/crew-0", Principal: "alice", PrincipalKind: fw.KindUser}
	if got != want {
		t.Fatalf("CallRun = %+v, want %+v", got, want)
	}
	for _, token := range []string{"alice-token", "workload-token"} {
		if res := call(token, meta); !res.IsError {
			t.Errorf("%s posed as a run: %s", token, textOf(res))
		}
	}
	if res := call("fleet-token", nil); !res.IsError {
		t.Errorf("a call without _meta named a run: %s", textOf(res))
	}
	if res := call("fleet-token", sdk.Meta{MetaRun: 7, MetaAgent: "reviewer"}); !res.IsError {
		t.Errorf("a run key that is not a string named a run: %s", textOf(res))
	}
}

// The modern stateless shape carries _meta too.
func TestCallRunModern(t *testing.T) {
	withRegistered(t, echoTools)
	ctx := testCtx(t)
	a := newAgent(t, newScript().model(), fw.WithAuthn(callers))
	url := serveAgent(t, a, nil).URL
	cs := mustConnect(t, ctx, url, clientSetup{
		protocol: modernProtocol,
		header:   http.Header{"Authorization": {"Bearer fleet-token"}},
	})
	res, err := cs.CallTool(ctx, &sdk.CallToolParams{Meta: sdk.Meta{MetaRun: "r-2", MetaAgent: "migrator"}, Name: "echo_run"})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError || !strings.Contains(textOf(res), "run r-2 of migrator") {
		t.Fatalf("result = %s", textOf(res))
	}
}
