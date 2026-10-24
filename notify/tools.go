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
		"channels":{"type":"array","maxItems":10,"description":"The channels of the person. The list replaces the channels; leave it out to keep them, or send [] to remove every channel. Each channel has an id (the front gives one when it is empty), a kind of the list offered of notifications_get, a label, send, and the fields of its kind: url for slack (an incoming webhook on hooks.slack.com), discord (a webhook on discord.com under /api/webhooks/), teams (a webhook of a Teams workflow), and webhook (an https address, with secret as an option for the HMAC header X-Featherweight-Signature); ntfy {server, topic, token}; email {to}; agent {agent, how, budget}, a run of the agent as the person that delivers the message, with a budget that is required; shared, the name of a shared channel of the list shared of notifications_get. url, secret, and ntfy.token are write only: \"set\" keeps the stored value of the channel with the same id, an empty text clears it, and any other text replaces it.","items":{"type":"object","properties":{
			"id":{"type":"string","maxLength":40},
			"kind":{"type":"string","enum":["ntfy","slack","discord","teams","webhook","email","agent","shared"]},
			"label":{"type":"string","maxLength":80},
			"send":{"type":"object","description":"Which kinds the channel sends. questions: a run of a duty that waits for the person, and a decision while the person was away. failures: a failed run, a duty that paused itself, and a daily cap. digest: once a day, what the duties did. A field left out is off.","properties":{"questions":{"type":"boolean"},"failures":{"type":"boolean"},"digest":{"type":"boolean"}}},
			"url":{"type":"string","maxLength":2048},
			"secret":{"type":"string","maxLength":512},
			"ntfy":{"type":"object","description":"server is the address of the ntfy server, or empty for the server of the front; the front never picks a public server by itself. topic is 1 to 64 of A-Z, a-z, 0-9, '-', and '_'. token is the access token of the server.","properties":{"server":{"type":"string","maxLength":512},"topic":{"type":"string","maxLength":64},"token":{"type":"string","maxLength":512}}},
			"email":{"type":"object","properties":{"to":{"type":"string","maxLength":254}}},
			"agent":{"type":"object","description":"agent names the agent, which the person needs the right to run. how says in words how the agent delivers the message, such as: Send it to me on Signal. budget bounds each run and is required, such as {\"maxCostUSD\": 0.02}.","properties":{"agent":{"type":"string"},"how":{"type":"string","maxLength":2000},"budget":{"type":"object","properties":{"maxCostUSD":{"type":"number"},"maxTurns":{"type":"integer"},"maxTokens":{"type":"integer"},"maxToolCalls":{"type":"integer"}}}}},
			"shared":{"type":"string","maxLength":63}
		}}},
		"quietHours":{"type":"object","description":"Every channel holds a notification from from to to until to. from and to are HH:MM in 24 hours, such as 22:00 and 07:00, or both empty for no quiet hours. timeZone is the IANA time zone of the person, for the quiet hours and the digest, which goes at the end of the quiet hours or at 08:00. Leave quietHours out to keep the quiet hours.","properties":{"from":{"type":"string"},"to":{"type":"string"},"timeZone":{"type":"string"}}},
		"timeZone":{"type":"string","maxLength":64,"description":"The IANA time zone of the person, such as America/New_York, when quietHours names none. Without it, UTC."}
	}}`

	testSchema = `{"type":"object","properties":{
		"channel":{"type":"string","maxLength":40,"description":"The id of one channel. Leave it out to test every channel."}
	}}`
)

type putArgs struct {
	Channels   []Channel       `json:"channels"`
	QuietHours *QuietHours     `json:"quietHours,omitempty"`
	TimeZone   string          `json:"timeZone,omitempty"`
	Offered    json.RawMessage `json:"offered,omitempty"`
	Shared     json.RawMessage `json:"shared,omitempty"`
	Off        json.RawMessage `json:"off,omitempty"`
}

type testArgs struct {
	Channel string `json:"channel,omitempty"`
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
			Description: "Read the notifications of the person: the channels (ntfy, Slack, Discord, Teams, webhook, email, agent, or a shared channel), each with its switches, the quiet hours, the kinds that the front offers, the shared channels of the operator, and off, which says why no channel reaches the person. An address, a secret, or a token reads as \"set\" or an empty text.",
			InputSchema: json.RawMessage(emptySchema),
			Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true, OpenWorldHint: ptr(false)},
		}, func(ctx context.Context, _ *sdk.CallToolRequest, _ struct{}) (*sdk.CallToolResult, any, error) {
			return getTool(ctx, a), nil, nil
		}),
		mcp.NewTool(&sdk.Tool{
			Name:        ToolPut,
			Title:       "Change my notifications",
			Description: "Change the notifications of the person: the channels and the quiet hours. A read of notifications_get is a valid input. A person has at most 10 channels. Then notification_test sends one test notification to a channel or to every channel.",
			InputSchema: json.RawMessage(putSchema),
			Annotations: &sdk.ToolAnnotations{DestructiveHint: ptr(false), IdempotentHint: true, OpenWorldHint: ptr(false)},
		}, func(ctx context.Context, _ *sdk.CallToolRequest, in putArgs) (*sdk.CallToolResult, any, error) {
			return putTool(ctx, a, in), nil, nil
		}),
		mcp.NewTool(&sdk.Tool{
			Name:        ToolTest,
			Title:       "Send a test notification",
			Description: "Send one test notification now to one channel of the person, or to every channel, also during the quiet hours, and say for each channel whether it went out or why not. A test of a channel of the kind agent starts one run of the agent.",
			InputSchema: json.RawMessage(testSchema),
			Annotations: &sdk.ToolAnnotations{DestructiveHint: ptr(false), IdempotentHint: false, OpenWorldHint: ptr(true)},
		}, func(ctx context.Context, _ *sdk.CallToolRequest, in testArgs) (*sdk.CallToolResult, any, error) {
			return testTool(ctx, a, in), nil, nil
		}),
	}
}

// explain words an error of the notifications for a tool result.
func explain(err error) string {
	var pe *ProblemsError
	switch {
	case errors.As(err, &pe):
		return "Invalid request: the settings do not fit.\n- " + strings.Join(pe.Problems, "\n- ")
	case errors.Is(err, ErrUnknownChannel):
		return "Invalid request: " + capitalize(err.Error()) + "."
	case errors.Is(err, ErrNobody):
		return "Forbidden: notifications belong to a person, and this call names nobody."
	}
	return mcp.Explain(err)
}

// off is the result of a tool in a process without the notifications.
func off() *sdk.CallToolResult {
	return mcp.ErrorResult("The notifications are off in this process. They run on a front that serves, with the bus nats or mem.")
}

// answerText words the settings of a person. It never holds an address,
// a secret, or a token: the answer holds none.
func answerText(ans Answer) string {
	var b strings.Builder
	switch {
	case ans.Off != "":
		b.WriteString("Notifications are off: " + ans.Off)
	case len(ans.Channels) == 1:
		b.WriteString("Notifications go to 1 channel.")
	default:
		fmt.Fprintf(&b, "Notifications go to %d channels.", len(ans.Channels))
	}
	for _, ch := range ans.Channels {
		var kinds []string
		for _, k := range []struct {
			on   bool
			name string
		}{{ch.Send.Questions, "questions"}, {ch.Send.Failures, "failures"}, {ch.Send.Digest, "the daily digest"}} {
			if k.on {
				kinds = append(kinds, k.name)
			}
		}
		what := "nothing"
		if len(kinds) > 0 {
			what = strings.Join(kinds, ", ")
		}
		fmt.Fprintf(&b, "\n- %s (id %s, %s): %s.", ch.Label, ch.ID, kindLabel(ch.Kind), what)
	}
	if q := ans.QuietHours; q != nil && q.From != "" {
		fmt.Fprintf(&b, "\nQuiet hours: %s to %s, %s.", q.From, q.To, q.TimeZone)
	}
	var offered []string
	for _, o := range ans.Offered {
		offered = append(offered, o.Kind)
	}
	b.WriteString("\nThe front offers: " + strings.Join(offered, ", ") + ".")
	if len(ans.Shared) > 0 {
		var names []string
		for _, sc := range ans.Shared {
			names = append(names, fmt.Sprintf("%s (%s, %s)", sc.Name, sc.Label, kindLabel(sc.Kind)))
		}
		b.WriteString("\nShared channels: " + strings.Join(names, ", ") + ".")
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
	ans, err := s.Put(ctx, Body{Channels: in.Channels, QuietHours: in.QuietHours}, in.TimeZone)
	if err != nil {
		return mcp.ErrorResult(explain(err))
	}
	return mcp.Result("Saved. "+answerText(ans), ans)
}

// testTool is the tool notification_test.
func testTool(ctx context.Context, a *fw.Agent, in testArgs) *sdk.CallToolResult {
	ctx, _, _ = manager.OnBehalf(ctx)
	s := Of(a)
	if s == nil {
		return off()
	}
	ans, err := s.Test(ctx, in.Channel)
	if err != nil {
		return mcp.ErrorResult(explain(err))
	}
	if len(ans.Results) == 0 {
		return mcp.Result("You have no channel, so no test went out. Add one with notifications_put.", ans)
	}
	var b strings.Builder
	for i, r := range ans.Results {
		if i > 0 {
			b.WriteString("\n")
		}
		if r.Sent {
			fmt.Fprintf(&b, "%s: the test went out.", r.Channel)
		} else {
			fmt.Fprintf(&b, "%s: the test did not go out. %s", r.Channel, r.Error)
		}
	}
	return mcp.Result(b.String(), ans)
}
