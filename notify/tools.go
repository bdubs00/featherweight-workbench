package notify

// This file holds the MCP tools of the notifications, which the package
// adds to the MCP server of the workbench through mcp.RegisterTools. Each
// tool calls the Go function of its route, for the caller, or for the
// person of a run of the fleet (manager.OnBehalf).

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
	ToolGet  = "notifications_get"
	ToolPut  = "notifications_put"
	ToolTest = "notification_test"
)

const (
	emptySchema = `{"type":"object","properties":{}}`

	putSchema = `{"type":"object","properties":{
		"ntfy":{"type":"object","description":"Where the notifications go. server is the address of the ntfy server, or empty for the server of the front (serverDefault in notifications_get); the front never picks a public server by itself. topic is the topic that the phone of the person subscribes to: 1 to 64 of A-Z, a-z, 0-9, '-', and '_'. token is the access token of the server: \"set\" keeps the token that the front holds, an empty text clears it, and any other text replaces it.","properties":{"server":{"type":"string","maxLength":512},"topic":{"type":"string","maxLength":64},"token":{"type":"string","maxLength":512}}},
		"send":{"type":"object","description":"Which kinds go. questions: a run of a duty that waits for the person, and a decision while the person was away. failures: a failed run, a duty that paused itself, and a daily cap. digest: once a day, what the duties did. A field left out takes its default: questions and failures on, digest off.","properties":{"questions":{"type":"boolean"},"failures":{"type":"boolean"},"digest":{"type":"boolean"}}},
		"quietHours":{"type":"object","description":"A notification from from to to waits until to. from and to are HH:MM in 24 hours, such as 22:00 and 07:00, or both empty for no quiet hours. timeZone is the IANA time zone of the person, for the quiet hours and the digest, which goes at the end of the quiet hours or at 08:00.","properties":{"from":{"type":"string"},"to":{"type":"string"},"timeZone":{"type":"string"}}},
		"timeZone":{"type":"string","maxLength":64,"description":"The IANA time zone of the person, such as America/New_York, when quietHours names none. Without it, UTC."}
	}}`
)

type putArgs struct {
	Ntfy       Ntfy        `json:"ntfy"`
	Send       *SendBody   `json:"send,omitempty"`
	QuietHours *QuietHours `json:"quietHours,omitempty"`
	TimeZone   string      `json:"timeZone,omitempty"`
}

func ptr[T any](v T) *T { return &v }

// tools builds the tools of the notifications for one MCP server. A
// process without the notifications gets none.
func tools(srv *mcp.Server) []mcp.Tool {
	a := srv.Agent()
	if Of(a) == nil {
		return nil
	}
	return []mcp.Tool{
		mcp.NewTool(&sdk.Tool{
			Name:        ToolGet,
			Title:       "Read my notifications",
			Description: "Read the notifications of the person: the ntfy server and topic, whether a token is set, which kinds go (questions, failures, digest), the quiet hours, the server of the front, and off, which says why no notification reaches the person.",
			InputSchema: json.RawMessage(emptySchema),
			Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true, OpenWorldHint: ptr(false)},
		}, func(ctx context.Context, _ *sdk.CallToolRequest, _ struct{}) (*sdk.CallToolResult, any, error) {
			return getTool(ctx, a), nil, nil
		}),
		mcp.NewTool(&sdk.Tool{
			Name:        ToolPut,
			Title:       "Change my notifications",
			Description: "Replace the notifications of the person: the ntfy server and topic, the token, which kinds go, and the quiet hours. A read of notifications_get is a valid input. Then notification_test sends one test notification.",
			InputSchema: json.RawMessage(putSchema),
			Annotations: &sdk.ToolAnnotations{DestructiveHint: ptr(false), IdempotentHint: true, OpenWorldHint: ptr(false)},
		}, func(ctx context.Context, _ *sdk.CallToolRequest, in putArgs) (*sdk.CallToolResult, any, error) {
			return putTool(ctx, a, in), nil, nil
		}),
		mcp.NewTool(&sdk.Tool{
			Name:        ToolTest,
			Title:       "Send a test notification",
			Description: "Send one test notification to the person now, also during the quiet hours, and say whether it went out or why not.",
			InputSchema: json.RawMessage(emptySchema),
			Annotations: &sdk.ToolAnnotations{DestructiveHint: ptr(false), IdempotentHint: false, OpenWorldHint: ptr(true)},
		}, func(ctx context.Context, _ *sdk.CallToolRequest, _ struct{}) (*sdk.CallToolResult, any, error) {
			return testTool(ctx, a), nil, nil
		}),
	}
}

// explain words an error of the notifications for a tool result.
func explain(err error) string {
	var pe *ProblemsError
	switch {
	case errors.As(err, &pe):
		return "Invalid request: the settings do not fit.\n- " + strings.Join(pe.Problems, "\n- ")
	case errors.Is(err, ErrUpstream):
		return "Bad gateway: " + strings.TrimPrefix(err.Error(), ErrUpstream.Error()+": ")
	case errors.Is(err, ErrNobody):
		return "Forbidden: notifications belong to a person, and this call names nobody."
	}
	return mcp.Explain(err)
}

// off is the result of a tool in a process without the notifications.
func off() *sdk.CallToolResult {
	return mcp.ErrorResult("The notifications are off in this process. They run on a front that serves, with the bus nats or mem.")
}

// answerText words the settings of a person.
func answerText(ans Answer) string {
	var b strings.Builder
	if ans.Off != "" {
		b.WriteString("Notifications are off: " + ans.Off)
	} else {
		server := ans.Ntfy.Server
		if server == "" {
			server = ans.ServerDefault + " (the server of the front)"
		}
		fmt.Fprintf(&b, "Notifications go to the topic %s on %s.", ans.Ntfy.Topic, server)
	}
	var kinds []string
	for _, k := range []struct {
		on   bool
		name string
	}{{ans.Send.Questions, "questions"}, {ans.Send.Failures, "failures"}, {ans.Send.Digest, "a daily digest"}} {
		if k.on {
			kinds = append(kinds, k.name)
		}
	}
	if len(kinds) > 0 {
		b.WriteString("\nThey tell of " + strings.Join(kinds, ", ") + ".")
	}
	if q := ans.QuietHours; q != nil && q.From != "" {
		fmt.Fprintf(&b, "\nQuiet hours: %s to %s, %s.", q.From, q.To, q.TimeZone)
	}
	if ans.Ntfy.Token == TokenSet {
		b.WriteString("\nA token is set.")
	}
	return b.String()
}

// getTool is the tool notifications_get.
func getTool(ctx context.Context, a *fw.Agent) *sdk.CallToolResult {
	ctx, _, _ = manager.OnBehalf(ctx)
	s := Of(a)
	if s == nil {
		return off()
	}
	ans, err := s.Get(ctx)
	if err != nil {
		return mcp.ErrorResult(explain(err))
	}
	return mcp.Result(answerText(ans), ans)
}

// putTool is the tool notifications_put.
func putTool(ctx context.Context, a *fw.Agent, in putArgs) *sdk.CallToolResult {
	ctx, _, _ = manager.OnBehalf(ctx)
	s := Of(a)
	if s == nil {
		return off()
	}
	ans, err := s.Put(ctx, Body{Ntfy: in.Ntfy, Send: in.Send, QuietHours: in.QuietHours}, in.TimeZone)
	if err != nil {
		return mcp.ErrorResult(explain(err))
	}
	return mcp.Result("Saved. "+answerText(ans), ans)
}

// testTool is the tool notification_test.
func testTool(ctx context.Context, a *fw.Agent) *sdk.CallToolResult {
	ctx, _, _ = manager.OnBehalf(ctx)
	s := Of(a)
	if s == nil {
		return off()
	}
	if err := s.Test(ctx); err != nil {
		return mcp.ErrorResult(explain(err))
	}
	return mcp.Result("The test notification went out.", map[string]bool{"sent": true})
}
