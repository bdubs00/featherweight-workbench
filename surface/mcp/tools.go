package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/connections"
	"github.com/bdubs00/featherweight-workbench/fleet"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Names of the work tools.
const (
	toolRun       = "run"
	toolRunStatus = "run_status"
	toolApprove   = "approve"
	toolCancel    = "cancel"
	toolFork      = "fork"
	toolSend      = "send"
)

// workTools are the names that an exposed tool never takes: the work tools
// and the management tools.
var workTools = map[string]bool{
	toolRun: true, toolRunStatus: true, toolApprove: true, toolCancel: true, toolSend: true, toolFork: true,
	toolWhoami: true, toolAgentsList: true, toolAgentGet: true, toolAgentPut: true, toolAgentDelete: true,
	toolRunsList: true, toolRunEvents: true, toolRunFile: true, toolUsage: true, toolDashboard: true,
	toolRunResult: true, toolAgentBuilder: true, toolCapabilities: true,
}

// reserved reports whether an exposed tool cannot take the name: a work
// tool, a management tool, the fleet tool when the fleet is on, or a
// connection tool when a tool source acts for a person.
func (s *server) reserved(name string) bool {
	return workTools[name] || (name == toolFleet && fleet.Of(s.a) != nil) ||
		((name == toolConnections || name == toolDisconnect) && connections.Of(s.a) != nil) ||
		((name == toolEnqueue || name == toolBatch) && s.intakeOn())
}

// Input schemas of the work tools.
const (
	runIDSchema = `{"type":"string","minLength":1,"description":"The id of the run."}`

	runStatusSchema = `{"type":"object","properties":{"runId":` + runIDSchema + `},"required":["runId"]}`

	approveSchema = `{"type":"object","properties":{
		"runId":` + runIDSchema + `,
		"approvalId":{"type":"string","minLength":1,"description":"The id of the pending approval, from pending.id of the run object."},
		"decision":{"type":"string","enum":["allow","deny"],"description":"allow lets the tool call run. deny refuses it."},
		"note":{"type":"string","description":"A note for the record. The model reads it after a deny. For a free-text question, the note is the answer."},
		"content":{"type":"object","description":"The form answers when the pending approval is a question with a schema."}
	},"required":["runId","approvalId","decision"]}`

	cancelSchema = `{"type":"object","properties":{"runId":` + runIDSchema + `},"required":["runId"]}`

	sendSchema = `{"type":"object","properties":{
		"runId":` + runIDSchema + `,
		"content":{"type":"string","description":"The text of the follow-up message. It can be empty when the message has files."},
		"files":` + filesSchema + `
	},"required":["runId"]}`

	// filesSchema is the schema of the attachments of a message.
	filesSchema = `{"type":"array","maxItems":20,"description":"Files that a person attaches, each with a name and its bytes in base64. The model gets an image or a PDF when it reads the type, the text of a small text file, and otherwise a note with the path of the file in the workspace.","items":{"type":"object","properties":{
		"name":{"type":"string","minLength":1,"description":"The file name, such as screenshot.png."},
		"data":{"type":"string","contentEncoding":"base64","description":"The bytes of the file in base64."}
	},"required":["name","data"]}}`
)

// timeZoneDesc describes the time zone of an inline profile.
const timeZoneDesc = "The IANA time zone of the run, for example America/Chicago. Empty uses the zone of the profile, else the zone of the configuration."

// runSchema builds the input schema of run. Profile names come from the
// caller-scoped agents_list, not from the shared tool schema.
func runSchema(a *fw.Agent) json.RawMessage {
	agentDesc := "The agent profile to run. Empty selects the default profile. Use agents_list to discover the profiles you can view."
	globList := func(what string) map[string]any {
		return map[string]any{"type": "array", "items": map[string]any{"type": "string", "minLength": 1}, "maxItems": 64, "description": what}
	}
	ruleList := func(what string) map[string]any {
		return map[string]any{"type": "array", "items": map[string]any{"type": "string", "minLength": 1}, "description": what}
	}
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"input": map[string]any{"type": "string", "minLength": 1, "description": "The task for the agent. It is the first user message of the run."},
			"title": map[string]any{"type": "string", "maxLength": maxTitleRunes,
				"description": "A short name of the work, such as Fix checkout retries. Without it, the run takes a title from its input."},
			"files": json.RawMessage(filesSchema),
			"agent": map[string]any{"type": "string", "description": agentDesc},
			"instructions": map[string]any{"type": "string",
				"description": "Text for this run alone; it follows the system prompt of the profile under the heading \"Instructions for this run\"."},
			"profile": map[string]any{
				"type":        "object",
				"description": "An inline profile that overlays the named profile for this run: a set field replaces the same field of the profile, instructions are appended, and the policy can only get stricter.",
				"properties": map[string]any{
					"system":       map[string]any{"type": "string", "description": "The system prompt; it replaces the system prompt of the profile."},
					"instructions": map[string]any{"type": "array", "items": map[string]any{"type": "string", "minLength": 1}, "description": "Texts that follow the system prompt, after the instructions of the profile."},
					"skills":       globList("Globs over skill names that the run can use; an empty list allows none, and an absent list keeps the skills of the profile."),
					"tools":        globList("Globs over tool names that the run can call, such as files or git.*; an absent list keeps the tools of the profile."),
					"model":        map[string]any{"type": "string", "description": "A provider/model selector, such as anthropic/claude-haiku-5-5, or a name from the models map of the configuration."},
					"effort":       map[string]any{"type": "string", "enum": fw.Efforts, "description": "The reasoning effort of the model."},
					"timeZone":     map[string]any{"type": "string", "description": timeZoneDesc},
					"policy": map[string]any{
						"type":        "object",
						"description": "Rules that tighten the policy of the profile: a default verdict and lists of flags, tool names, or tool: command globs.",
						"properties": map[string]any{
							"default": map[string]any{"type": "string", "enum": []string{"allow", "ask", "deny"}, "description": "The verdict when no rule matches."},
							"allow":   ruleList("Rules that allow a call, such as readonly or files."),
							"ask":     ruleList("Rules that hold a call for an approval, such as destructive or bash: git push*."),
							"deny":    ruleList("Rules that refuse a call, such as bash: rm -rf*."),
						},
					},
					"budget": budgetSchema("Limits of this run from the profile side; the request budget wins over them."),
				},
			},
			"wait": map[string]any{"type": "boolean", "default": true,
				"description": "true waits until the run finishes or needs input. false returns the run object at once."},
			"budget": budgetSchema("Limits of this run. A field that is set replaces the same limit of the profile."),
			"requestId": map[string]any{"type": "string",
				"description": "An idempotency key. A retried call with the same requestId returns the same run instead of a second one."},
			"metadata": map[string]any{"type": "object", "additionalProperties": map[string]any{"type": "string"},
				"description": "Labels of the run for the caller, such as the screen that started it. The run object returns them."},
		},
		"required": []string{"input"},
	}
	if f := fleet.Of(a); f != nil {
		props := schema["properties"].(map[string]any)
		props["member"] = map[string]any{"type": "string",
			"description": "The fleet member that runs the agent; the fleet tool lists the members and their agents. Empty, or " + f.Name() + ", runs it here. A run on another member never waits: the call returns the run object at once."}
	}
	b, _ := json.Marshal(schema)
	return b
}

// budgetSchema is the schema of a budget object.
func budgetSchema(desc string) map[string]any {
	return map[string]any{
		"type":        "object",
		"description": desc,
		"properties": map[string]any{
			"maxTurns":        map[string]any{"type": "integer", "minimum": 0, "description": "The most model turns."},
			"maxTokens":       map[string]any{"type": "integer", "minimum": 0, "description": "The most input and output tokens in total."},
			"maxCostUSD":      map[string]any{"type": "number", "minimum": 0, "description": "The most cost in US dollars."},
			"deadline":        map[string]any{"type": []string{"string", "integer"}, "minimum": 1, "description": "The limit of working time as a duration string, for example 90s, 30m, or 2h, or as positive integer nanoseconds from a saved profile. The time that the run waits for a person or a follow-up does not count."},
			"maxToolCalls":    map[string]any{"type": "integer", "minimum": 0, "description": "The most tool calls in the whole run."},
			"maxCallsPerTurn": map[string]any{"type": "integer", "minimum": 0, "description": "The most tool calls in one model response."},
			"maxRepeats":      map[string]any{"type": "integer", "minimum": 0, "description": "The most times in a row that the model can repeat the same tool call; the run fails at this count."},
		},
	}
}

// maxTitleRunes bounds the title argument of run.
const maxTitleRunes = 200

type runArgs struct {
	Input        string            `json:"input"`
	Title        string            `json:"title,omitempty"`
	Agent        string            `json:"agent,omitempty"`
	Instructions string            `json:"instructions,omitempty"`
	Profile      *profileArgs      `json:"profile,omitempty"`
	Wait         *bool             `json:"wait,omitempty"`
	Budget       *budgetArgs       `json:"budget,omitempty"`
	RequestID    string            `json:"requestId,omitempty"`
	Member       string            `json:"member,omitempty"`
	Files        []fileArg         `json:"files,omitempty"`
	Metadata     map[string]string `json:"metadata,omitempty"`
}

// fileArg is one attachment of a run or a message. JSON holds Data in
// base64, and the kernel sniffs the media type from the bytes.
type fileArg struct {
	Name string `json:"name"`
	Data []byte `json:"data"`
}

// attachments converts the files of a call to file blocks.
func attachments(files []fileArg) []fw.Block {
	out := make([]fw.Block, 0, len(files))
	for _, f := range files {
		out = append(out, fw.Attachment(f.Name, f.Data))
	}
	return out
}

// profileArgs is the inline profile of a run call.
type profileArgs struct {
	System       string      `json:"system,omitempty"`
	Instructions []string    `json:"instructions,omitempty"`
	Skills       []string    `json:"skills,omitempty"`
	Tools        []string    `json:"tools,omitempty"`
	Model        string      `json:"model,omitempty"`
	Effort       string      `json:"effort,omitempty"`
	TimeZone     string      `json:"timeZone,omitempty"`
	Policy       *fw.Rules   `json:"policy,omitempty"`
	Budget       *budgetArgs `json:"budget,omitempty"`
}

// profile converts the arguments. The prefix names the fields in an
// error: "profile." for the inline profile of run, and "" for agent_put.
// The kernel validates the sizes, the effort, and the model.
func (p *profileArgs) profile(prefix string) (fw.AgentProfile, error) {
	out := fw.AgentProfile{
		System:       p.System,
		Instructions: p.Instructions,
		Skills:       p.Skills,
		Tools:        p.Tools,
		Model:        p.Model,
		Effort:       strings.ToLower(p.Effort),
		TimeZone:     strings.TrimSpace(p.TimeZone),
	}
	if out.TimeZone != "" {
		if _, _, err := fw.LoadZone(out.TimeZone); err != nil {
			return out, fmt.Errorf("%stimeZone %q is not an IANA time zone such as America/Chicago, or UTC", prefix, p.TimeZone)
		}
	}
	if p.Policy != nil {
		r := *p.Policy
		r.Default = strings.ToLower(strings.TrimSpace(r.Default))
		switch r.Default {
		case "", "allow", "ask", "deny":
		default:
			return out, fmt.Errorf("%spolicy.default %q is not allow, ask, or deny", prefix, p.Policy.Default)
		}
		out.Policy = &r
	}
	if p.Budget != nil {
		b, err := p.Budget.budget(prefix + "budget")
		if err != nil {
			return out, err
		}
		out.Budget = &b
	}
	return out, nil
}

// durationArg accepts the duration strings used by MCP calls and the integer
// nanoseconds returned in profile budgets. It never converts through float64.
type durationArg string

func (d *durationArg) UnmarshalJSON(data []byte) error {
	var text string
	if len(data) > 0 && data[0] == '"' {
		if err := json.Unmarshal(data, &text); err != nil {
			return fmt.Errorf("mcp: decode deadline: %w", err)
		}
		*d = durationArg(text)
		return nil
	}
	var nanos int64
	if err := json.Unmarshal(data, &nanos); err != nil {
		return fmt.Errorf("mcp: deadline needs a duration string or integer nanoseconds: %w", err)
	}
	if nanos <= 0 {
		return fmt.Errorf("mcp: deadline nanoseconds must be positive")
	}
	*d = durationArg(time.Duration(nanos).String())
	return nil
}

type budgetArgs struct {
	MaxTurns        int         `json:"maxTurns,omitempty"`
	MaxTokens       int         `json:"maxTokens,omitempty"`
	MaxCostUSD      float64     `json:"maxCostUSD,omitempty"`
	Deadline        durationArg `json:"deadline,omitempty"`
	MaxToolCalls    int         `json:"maxToolCalls,omitempty"`
	MaxCallsPerTurn int         `json:"maxCallsPerTurn,omitempty"`
	MaxRepeats      int         `json:"maxRepeats,omitempty"`
}

type runIDArgs struct {
	RunID string `json:"runId"`
}

type forkArgs struct {
	RunID    string            `json:"runId"`
	Seq      uint64            `json:"seq,omitempty"`
	Input    string            `json:"input,omitempty"`
	Model    string            `json:"model,omitempty"`
	Metadata map[string]string `json:"metadata,omitempty"`
	Spawn    bool              `json:"spawn,omitempty"`
}

// forkSchema is the input of the tool fork.
const forkSchema = `{"type":"object","properties":{
	"runId":` + runIDSchema + `,
	"seq":{"type":"integer","minimum":0,"description":"The sequence number of the last event of the run that the branch keeps. 0 or absent keeps every event."},
	"input":{"type":"string","description":"A message that follows the kept events, so the branch goes on in another direction."},
	"model":{"type":"string","description":"A model selector or a name from models for the branch."},
	"metadata":{"type":"object","additionalProperties":{"type":"string"},"description":"Metadata of the branch."},
	"spawn":{"type":"boolean","description":"Run the branch in a Task of its own, with a workspace of its own, when the member has a spawner."}
},"required":["runId"],"additionalProperties":false}`

type approveArgs struct {
	RunID      string          `json:"runId"`
	ApprovalID string          `json:"approvalId"`
	Decision   string          `json:"decision"`
	Note       string          `json:"note,omitempty"`
	Content    json.RawMessage `json:"content,omitempty"`
}

type sendArgs struct {
	RunID   string    `json:"runId"`
	Content string    `json:"content"`
	Files   []fileArg `json:"files,omitempty"`
}

func ptr[T any](v T) *T { return &v }

// addWorkTools adds run, run_status, approve, cancel, and send.
func (s *server) addWorkTools() {
	sdk.AddTool(s.srv, &sdk.Tool{
		Name:        toolRun,
		Title:       "Run an agent",
		Description: "Start a run of an agent profile with a task. With wait true (the default), wait until the run finishes and return its output. Shape the run with instructions (text for this run) and profile (an inline profile with a system prompt, tools, skills, model, effort, time zone, policy, and budget). Approvals arrive as elicitation forms when the client supports them.",
		InputSchema: runSchema(s.a),
		Annotations: &sdk.ToolAnnotations{DestructiveHint: ptr(true), OpenWorldHint: ptr(true)},
	}, s.run)
	sdk.AddTool(s.srv, &sdk.Tool{
		Name:        toolRunStatus,
		Title:       "Run status",
		Description: "Return the run object: state, output, pending approval, and usage.",
		InputSchema: json.RawMessage(runStatusSchema),
		Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true, OpenWorldHint: ptr(false)},
	}, s.runStatus)
	sdk.AddTool(s.srv, &sdk.Tool{
		Name:        toolApprove,
		Title:       "Decide an approval",
		Description: "Allow or deny the pending approval of a run. The run continues after the decision.",
		InputSchema: json.RawMessage(approveSchema),
		Annotations: &sdk.ToolAnnotations{DestructiveHint: ptr(true), OpenWorldHint: ptr(true)},
	}, s.approve)
	sdk.AddTool(s.srv, &sdk.Tool{
		Name:        toolCancel,
		Title:       "Cancel a run",
		Description: "Cancel a run. It stops the model call and the tools in flight. Cancel on a finished run does nothing.",
		InputSchema: json.RawMessage(cancelSchema),
		Annotations: &sdk.ToolAnnotations{DestructiveHint: ptr(true), IdempotentHint: true, OpenWorldHint: ptr(false)},
	}, s.cancel)
	sdk.AddTool(s.srv, &sdk.Tool{
		Name:        toolFork,
		Title:       "Branch a run",
		Description: "Branch a run: a new run that keeps the events of the run up to seq, with the conversation to that point, and goes on with the message input, with another model, or the same way again. With spawn, the branch runs in a Task of its own. The source does not change. Read the branch with run_status.",
		InputSchema: json.RawMessage(forkSchema),
		Annotations: &sdk.ToolAnnotations{DestructiveHint: ptr(false), OpenWorldHint: ptr(true)},
	}, s.fork)
	sdk.AddTool(s.srv, &sdk.Tool{
		Name:        toolSend,
		Title:       "Send a follow-up",
		Description: "Add a follow-up user message to a run. A finished run starts a new turn. Read the result with run_status.",
		InputSchema: json.RawMessage(sendSchema),
		Annotations: &sdk.ToolAnnotations{DestructiveHint: ptr(true), OpenWorldHint: ptr(true)},
	}, s.send)
}

// principalOf returns the subject of the caller: the per-request token info
// over HTTP, else the principal in ctx.
func principalOf(ctx context.Context, req sdk.Request) string {
	if ex := req.GetExtra(); ex != nil && ex.TokenInfo != nil && ex.TokenInfo.UserID != "" {
		return ex.TokenInfo.UserID
	}
	if p := fw.PrincipalFrom(ctx); p != nil {
		return p.Subject
	}
	return ""
}

func (s *server) run(ctx context.Context, req *sdk.CallToolRequest, in runArgs) (*sdk.CallToolResult, any, error) {
	principal := principalOf(ctx, req)
	if st := req.Params.RequestState; st != "" {
		// A retry after an input_required result: record the answer and
		// continue the same run. The retry never starts a second run.
		state, err := s.openState(st, principal)
		if err != nil {
			return nil, nil, &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: err.Error()}
		}
		if err := s.answer(ctx, state, req.Params.InputResponses, principal); err != nil {
			return workError("", err), nil, nil
		}
		// A run that waits for a connection is answered by the end of the
		// connection, so answer may have waited for it.
		res, err := s.await(ctx, req, state.Run, principal)
		return res, nil, err
	}
	md, err := runMetadata(in)
	if err != nil {
		return errorResult("Invalid request: " + strings.TrimPrefix(err.Error(), "fw: invalid request: ") + "."), nil, nil
	}
	in.Metadata = md
	if member := strings.TrimSpace(in.Member); member != "" {
		f := fleet.Of(s.a)
		if f == nil {
			return failResult(errFleetOff), nil, nil
		}
		if member != f.Name() {
			if strings.TrimSpace(in.Input) == "" {
				return errorResult("Invalid request: the argument input is required and must not be empty."), nil, nil
			}
			return s.startOn(withCredential(ctx, req), member, in), nil, nil
		}
	}

	opts := []fw.RunOption{fw.WithOrigin(fw.OriginMCP)}
	if in.Agent != "" {
		opts = append(opts, fw.AsAgent(in.Agent))
	}
	if principal != "" {
		opts = append(opts, fw.WithPrincipalName(principal))
	}
	if in.RequestID != "" {
		opts = append(opts, fw.WithIdempotencyKey(in.RequestID))
	}
	if len(in.Metadata) > 0 {
		opts = append(opts, fw.WithMetadata(in.Metadata))
	}
	if len(in.Files) > 0 {
		opts = append(opts, fw.WithAttachments(attachments(in.Files)...))
	}
	if in.Instructions != "" {
		opts = append(opts, fw.WithInstructions(in.Instructions))
	}
	if in.Profile != nil {
		p, err := in.Profile.profile("profile.")
		if err != nil {
			return errorResult(err.Error()), nil, nil
		}
		opts = append(opts, fw.WithRunProfile(p))
	}
	if in.Budget != nil {
		b, err := in.Budget.budget("budget")
		if err != nil {
			return errorResult(err.Error()), nil, nil
		}
		opts = append(opts, fw.WithRunBudget(b))
	}
	r, err := s.a.Start(ctx, in.Input, opts...)
	if err != nil {
		return workError("The run did not start: ", err), nil, nil
	}
	s.log.Info("mcp run started", "run", r.ID, "agent", r.Agent, "principal", principal)
	if in.Wait != nil && !*in.Wait {
		return runResult(r), nil, nil
	}
	res, err := s.await(ctx, req, r.ID, principal)
	return res, nil, err
}

// runMetadata is the metadata of a run call: the metadata argument, with
// the title argument under fw.MetaTitle. The title wins over a title in the
// metadata. The result passes fw.CheckMetadata.
func runMetadata(in runArgs) (map[string]string, error) {
	md := in.Metadata
	if title := strings.TrimSpace(in.Title); title != "" {
		if n := utf8.RuneCountInString(title); n > maxTitleRunes {
			return nil, fmt.Errorf("%w: the title has %d characters; the limit is %d", fw.ErrInvalidRequest, n, maxTitleRunes)
		}
		md = maps.Clone(md)
		if md == nil {
			md = map[string]string{}
		}
		md[fw.MetaTitle] = title
	}
	if len(md) == 0 {
		return nil, nil
	}
	if err := fw.CheckMetadata(md); err != nil {
		return nil, err
	}
	return md, nil
}

// budget converts the arguments. The field names the budget in an error,
// so a profile budget reads "profile.budget".
func (b *budgetArgs) budget(field string) (fw.Budget, error) {
	out := fw.Budget{
		MaxTurns:        b.MaxTurns,
		MaxTokens:       b.MaxTokens,
		MaxCostUSD:      b.MaxCostUSD,
		MaxToolCalls:    b.MaxToolCalls,
		MaxCallsPerTurn: b.MaxCallsPerTurn,
		MaxRepeats:      b.MaxRepeats,
	}
	if out.MaxTurns < 0 || out.MaxTokens < 0 || out.MaxCostUSD < 0 || out.MaxToolCalls < 0 || out.MaxCallsPerTurn < 0 || out.MaxRepeats < 0 {
		return fw.Budget{}, fmt.Errorf("%s: a limit is negative", field)
	}
	if b.Deadline != "" {
		d, err := time.ParseDuration(string(b.Deadline))
		if err != nil || d <= 0 {
			return fw.Budget{}, fmt.Errorf("%s.deadline %q is not a positive duration such as 30m", field, b.Deadline)
		}
		out.Deadline = d
	}
	return out, nil
}

// approvalShape is how a waiting run call asks for an approval.
type approvalShape int

const (
	shapeNative        approvalShape = iota // return the run in input-required
	shapeInputRequests                      // return an input_required result (SEP-2322)
	shapeElicit                             // send elicitation/create on a legacy session
)

func shapeOf(req *sdk.CallToolRequest) approvalShape {
	caps := req.ClientCapabilities()
	if caps == nil || caps.Elicitation == nil {
		return shapeNative
	}
	if e := caps.Elicitation; e.Form == nil && e.URL != nil {
		return shapeNative // URL mode only: fw asks with a form
	}
	if req.ProtocolVersion() >= modernProtocol {
		return shapeInputRequests
	}
	return shapeElicit
}

// await waits for a run on behalf of a run call. It sends progress per turn,
// asks for approvals in the shape that the client supports, and returns the
// run object when the run finishes. When the request context ends first, the
// client is gone: await returns and the run continues.
func (s *server) await(ctx context.Context, req *sdk.CallToolRequest, id, principal string) (*sdk.CallToolResult, error) {
	cur, err := s.a.Get(ctx, id)
	if err != nil {
		return workError("", err), nil
	}
	base := s.base(req)
	var f *follower
	if !cur.State.Terminal() {
		f = s.follow(ctx, req, id, cur.Seq+1)
	}
	for {
		r, err := s.a.Wait(ctx, id)
		if err != nil {
			f.stop(ctx, false)
			if ctx.Err() != nil {
				s.log.Info("mcp client left before the run ended; the run continues", "run", id)
			}
			if r == nil {
				return workError("", err), nil
			}
			return runResultAt(r, base), nil
		}
		if r.State.Terminal() {
			f.stop(ctx, true)
			return runResultAt(r, base), nil
		}
		if r.Pending == nil {
			f.stop(ctx, false)
			return runResultAt(r, base), nil
		}
		if r.Pending.Connect != nil {
			// A connection ends in the browser, so the client only shows
			// the link and waits.
			res, err := s.askConnect(ctx, req, r, principal)
			if res != nil || err != nil {
				f.stop(ctx, false)
				return res, err
			}
			continue
		}
		switch shapeOf(req) {
		case shapeInputRequests:
			f.stop(ctx, false)
			return s.inputRequired(r, principal), nil
		case shapeElicit:
			if err := s.elicit(ctx, req, r, principal); err != nil {
				f.stop(ctx, false)
				if ctx.Err() == nil {
					s.log.Warn("mcp elicitation failed; returning the run in input-required", "run", id, "error", err)
				}
				return runResultAt(r, base), nil
			}
		default:
			f.stop(ctx, false)
			return runResultAt(r, base), nil
		}
	}
}

// decisionSchema is the form of an approval.
const decisionSchema = `{"type":"object","properties":{` +
	`"decision":{"type":"string","title":"Decision","description":"allow lets the tool call run. deny refuses it.","enum":["allow","deny"]},` +
	`"note":{"type":"string","title":"Note","description":"A note for the record. The model reads it after a deny."}` +
	`},"required":["decision"]}`

// elicitParams is the form that asks for a pending approval. A question from
// a tool carries its own schema.
func elicitParams(r *fw.Run) *sdk.ElicitParams {
	p := r.Pending
	if len(p.Schema) > 0 {
		return &sdk.ElicitParams{Mode: "form", Message: p.Reason, RequestedSchema: json.RawMessage(p.Schema)}
	}
	msg := fmt.Sprintf("The %s agent wants to call the tool %s with this input:\n%s", r.Agent, p.Tool, inputText(p.Input))
	if p.Reason != "" {
		msg += "\n\nThe policy asks because of " + p.Reason + "."
	}
	msg += "\n\nAllow the call?"
	return &sdk.ElicitParams{Mode: "form", Message: msg, RequestedSchema: json.RawMessage(decisionSchema)}
}

// decision turns an elicitation answer into an approval decision. A
// declined or canceled form denies.
func decision(p *fw.Approval, res *sdk.ElicitResult, principal string) fw.ApprovalDecision {
	d := fw.ApprovalDecision{ApprovalID: p.ID, Decider: principal}
	switch res.Action {
	case "accept":
		if len(p.Schema) > 0 {
			d.Decision = "allow"
			d.Content, _ = json.Marshal(res.Content)
			return d
		}
		v, _ := res.Content["decision"].(string)
		d.Note, _ = res.Content["note"].(string)
		d.Decision = "deny"
		if strings.EqualFold(strings.TrimSpace(v), "allow") {
			d.Decision = "allow"
		}
	case "decline":
		d.Decision, d.Note = "deny", "declined by the MCP client"
	default:
		d.Decision, d.Note = "deny", "canceled by the MCP client"
	}
	return d
}

// elicit asks a legacy session with elicitation/create and records the
// answer.
func (s *server) elicit(ctx context.Context, req *sdk.CallToolRequest, r *fw.Run, principal string) error {
	res, err := req.Session.Elicit(ctx, elicitParams(r))
	if err != nil {
		return fmt.Errorf("mcp: elicitation/create: %w", err)
	}
	return s.decide(ctx, r.ID, decision(r.Pending, res, principal))
}

// decide records a decision. A decision that another surface made first is
// not an error.
func (s *server) decide(ctx context.Context, id string, d fw.ApprovalDecision) error {
	err := s.a.Decide(ctx, id, d)
	if errors.Is(err, fw.ErrNoPendingApproval) {
		return nil
	}
	if err == nil {
		s.log.Info("mcp approval decided", "run", id, "approval", d.ApprovalID, "decision", d.Decision, "decider", d.Decider)
	}
	return err
}

// inputRequired is the SEP-2322 result that asks a modern client for the
// pending approval. The client retries run with the answer and the
// unchanged requestState.
func (s *server) inputRequired(r *fw.Run, principal string) *sdk.CallToolResult {
	return &sdk.CallToolResult{
		InputRequests: sdk.InputRequestMap{r.Pending.ID: elicitParams(r)},
		RequestState:  s.sealState(r.ID, r.Pending.ID, principal),
	}
}

// answer records the answer of a retry. A retry without an answer for the
// approval records nothing, and await asks again.
func (s *server) answer(ctx context.Context, st approvalState, responses sdk.InputResponseMap, principal string) error {
	res, ok := responses[st.Approval].(*sdk.ElicitResult)
	if !ok || res == nil {
		return nil
	}
	r, err := s.a.Get(ctx, st.Run)
	if err != nil {
		return err
	}
	if r.Pending == nil || r.Pending.ID != st.Approval {
		return nil // decided elsewhere
	}
	if r.Pending.Connect != nil {
		// The form of a client without URL mode carries a choice.
		_, form := res.Content["choice"]
		_, err := s.connectAnswer(ctx, r, res, principal, !form)
		return err
	}
	return s.decide(ctx, st.Run, decision(r.Pending, res, principal))
}

// follower sends one progress notification per turn of a run on the request
// that carries a progress token.
type follower struct {
	cancel context.CancelFunc
	done   chan struct{}
}

// follow starts a follower from a sequence number. It returns nil when the
// request carries no progress token.
func (s *server) follow(ctx context.Context, req *sdk.CallToolRequest, id string, from uint64) *follower {
	tok := req.Params.GetProgressToken()
	if tok == nil {
		return nil
	}
	fctx, cancel := context.WithCancel(ctx)
	evs, err := s.a.Events(fctx, id, from)
	if err != nil {
		cancel()
		return nil
	}
	f := &follower{cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(f.done)
		var last string
		for e := range evs {
			if e.Type != fw.EvTurnFinished {
				if m := describe(e); m != "" {
					last = m
				}
				continue
			}
			var d fw.TurnData
			_ = e.Decode(&d)
			msg := fmt.Sprintf("turn %d finished", d.Turn)
			if last != "" {
				msg += ": " + last
			}
			// The handler context ties the notification to the request stream.
			if err := req.Session.NotifyProgress(ctx, &sdk.ProgressNotificationParams{
				ProgressToken: tok, Progress: float64(d.Turn), Message: msg,
			}); err != nil {
				s.log.Debug("mcp progress notification failed", "run", id, "error", err)
			}
		}
	}()
	return f
}

// stop ends the follower. With drain true the run reached its terminal
// event, so the follower sends the last notifications and ends by itself.
func (f *follower) stop(ctx context.Context, drain bool) {
	if f == nil {
		return
	}
	if drain {
		t := time.NewTimer(5 * time.Second)
		defer t.Stop()
		select {
		case <-f.done:
			f.cancel()
			return
		case <-ctx.Done():
		case <-t.C:
		}
	}
	f.cancel()
	<-f.done
}

// describe is the progress message of an event, or "" when the event says
// nothing new.
func describe(e fw.Event) string {
	switch e.Type {
	case fw.EvModelResponded:
		var d fw.ModelRespondedData
		if e.Decode(&d) != nil {
			return ""
		}
		var names []string
		for _, b := range d.Content {
			if b.Type == fw.BlockToolUse {
				names = append(names, b.Name)
			}
		}
		if len(names) > 0 {
			return "the model called " + strings.Join(names, ", ")
		}
		if t := strings.Join(strings.Fields(fw.TextOf(d.Content)), " "); t != "" {
			return "the model answered: " + truncate(t, 120)
		}
	case fw.EvToolRequested:
		var d fw.ToolRequestedData
		if e.Decode(&d) == nil {
			return "called the tool " + d.Tool
		}
	case fw.EvToolResponded:
		var d fw.ToolRespondedData
		if e.Decode(&d) == nil && d.IsError {
			return "a tool call failed"
		}
	case fw.EvApprovalRequested:
		var d fw.ApprovalRequestedData
		if e.Decode(&d) == nil {
			if d.Connect != nil {
				return "waiting for the person to connect " + connectTitle(d.Connect)
			}
			return "waiting for an approval of " + d.Tool
		}
	case fw.EvApprovalDecided:
		var d fw.ApprovalDecidedData
		if e.Decode(&d) == nil {
			return "approval decided: " + d.Decision
		}
	case fw.EvContextCompacted:
		return "compacted the context"
	case fw.EvBudgetExceeded:
		var d fw.BudgetExceededData
		if e.Decode(&d) == nil {
			return "budget exceeded: " + d.Limit
		}
	case fw.EvError:
		var d fw.ErrorData
		if e.Decode(&d) == nil {
			return "error: " + truncate(d.Message, 120)
		}
	}
	return ""
}

func (s *server) runStatus(ctx context.Context, req *sdk.CallToolRequest, in runIDArgs) (*sdk.CallToolResult, any, error) {
	r, err := s.a.Get(ctx, in.RunID)
	if err != nil {
		if res := s.elsewhere(withCredential(ctx, req), in.RunID, err, func(ctx context.Context, member string) *sdk.CallToolResult {
			return remoteResult(s.memberRun(ctx, member, http.MethodGet, "/v1/runs/"+url.PathEscape(in.RunID), nil, ""))
		}); res != nil {
			return res, nil, nil
		}
		return workError("", err), nil, nil
	}
	return runResultAt(r, s.base(req)), nil, nil
}

// elsewhere tries the fleet when this process does not have the run: call
// gets the member that owns it. It returns nil when err is not
// fw.ErrRunNotFound or no other member has the run either.
func (s *server) elsewhere(ctx context.Context, id string, err error, call func(ctx context.Context, member string) *sdk.CallToolResult) *sdk.CallToolResult {
	if !errors.Is(err, fw.ErrRunNotFound) {
		return nil
	}
	member, ferr := s.ownerOf(ctx, id)
	if ferr != nil {
		return failResult(ferr)
	}
	if member == "" {
		return nil
	}
	return call(ctx, member)
}

func (s *server) approve(ctx context.Context, req *sdk.CallToolRequest, in approveArgs) (*sdk.CallToolResult, any, error) {
	d := fw.ApprovalDecision{
		ApprovalID: in.ApprovalID,
		Decision:   in.Decision,
		Note:       in.Note,
		Content:    in.Content,
		Decider:    principalOf(ctx, req),
	}
	if err := s.a.Decide(ctx, in.RunID, d); err != nil {
		if res := s.elsewhere(withCredential(ctx, req), in.RunID, err, func(ctx context.Context, member string) *sdk.CallToolResult {
			body := map[string]any{"decision": in.Decision}
			if in.Note != "" {
				body["note"] = in.Note
			}
			if len(in.Content) > 0 {
				body["content"] = in.Content
			}
			r, err := s.memberRun(ctx, member, http.MethodPost, "/v1/runs/"+url.PathEscape(in.RunID)+"/approvals/"+url.PathEscape(in.ApprovalID), body, "")
			if err == nil {
				s.log.Info("mcp approval decided on a fleet member", "run", in.RunID, "approval", in.ApprovalID, "decision", in.Decision, "member", member)
			}
			return remoteResult(r, err)
		}); res != nil {
			return res, nil, nil
		}
		if errors.Is(err, fw.ErrInvalidRequest) {
			// Only the end of a connection allows a connect.
			if r, gerr := s.a.Get(ctx, in.RunID); gerr == nil && r.Pending != nil && r.Pending.Connect != nil {
				return errorResult(connectRefusal(r, s.base(req), err)), nil, nil
			}
		}
		return workError("", err), nil, nil
	}
	s.log.Info("mcp approval decided", "run", in.RunID, "approval", in.ApprovalID, "decision", in.Decision, "decider", d.Decider)
	return s.status(ctx, req, in.RunID), nil, nil
}

func (s *server) cancel(ctx context.Context, req *sdk.CallToolRequest, in runIDArgs) (*sdk.CallToolResult, any, error) {
	if err := s.a.Cancel(ctx, in.RunID); err != nil {
		if res := s.elsewhere(withCredential(ctx, req), in.RunID, err, func(ctx context.Context, member string) *sdk.CallToolResult {
			return remoteResult(s.memberRun(ctx, member, http.MethodPost, "/v1/runs/"+url.PathEscape(in.RunID)+"/cancel", map[string]any{}, ""))
		}); res != nil {
			return res, nil, nil
		}
		return workError("", err), nil, nil
	}
	s.log.Info("mcp run canceled", "run", in.RunID, "principal", principalOf(ctx, req))
	return s.status(ctx, req, in.RunID), nil, nil
}

// fork branches a run of this process or of another member.
func (s *server) fork(ctx context.Context, req *sdk.CallToolRequest, in forkArgs) (*sdk.CallToolResult, any, error) {
	if strings.TrimSpace(in.RunID) == "" {
		return errorResult("Invalid request: the argument runId is required."), nil, nil
	}
	branch, err := s.a.Fork(ctx, in.RunID, fw.ForkRequest{Seq: in.Seq, Input: in.Input, Model: in.Model, Metadata: in.Metadata, Spawn: in.Spawn, Origin: fw.OriginMCP})
	if err != nil {
		if res := s.elsewhere(withCredential(ctx, req), in.RunID, err, func(ctx context.Context, member string) *sdk.CallToolResult {
			body := map[string]any{"seq": in.Seq, "input": in.Input, "model": in.Model, "metadata": in.Metadata, "spawn": in.Spawn}
			return remoteResult(s.memberRun(ctx, member, http.MethodPost, "/v1/runs/"+url.PathEscape(in.RunID)+"/fork", body, ""))
		}); res != nil {
			return res, nil, nil
		}
		return workError("", err), nil, nil
	}
	s.log.Info("mcp run branched", "run", in.RunID, "branch", branch.ID, "spawn", in.Spawn, "principal", principalOf(ctx, req))
	return s.status(ctx, req, branch.ID), nil, nil
}

func (s *server) send(ctx context.Context, req *sdk.CallToolRequest, in sendArgs) (*sdk.CallToolResult, any, error) {
	var content []fw.Block
	if strings.TrimSpace(in.Content) != "" {
		content = append(content, fw.Text(in.Content))
	}
	content = append(content, attachments(in.Files)...)
	if len(content) == 0 {
		return errorResult("Invalid request: a message needs content or files."), nil, nil
	}
	if err := s.a.Send(ctx, in.RunID, fw.Message{Role: fw.RoleUser, Content: content}, fw.OriginMCP); err != nil {
		if res := s.elsewhere(withCredential(ctx, req), in.RunID, err, func(ctx context.Context, member string) *sdk.CallToolResult {
			body := map[string]any{"content": in.Content}
			if len(in.Files) > 0 {
				body["files"] = in.Files
			}
			return remoteResult(s.memberRun(ctx, member, http.MethodPost, "/v1/runs/"+url.PathEscape(in.RunID)+"/messages", body, ""))
		}); res != nil {
			return res, nil, nil
		}
		return workError("", err), nil, nil
	}
	return s.status(ctx, req, in.RunID), nil, nil
}

// status returns the run object as a tool result.
func (s *server) status(ctx context.Context, req sdk.Request, id string) *sdk.CallToolResult {
	r, err := s.a.Get(ctx, id)
	if err != nil {
		return workError("", err)
	}
	return runResultAt(r, s.base(req))
}

// runResult is the result of every work tool: a text and the run object.
// A run with tools beyond its agent says so, so the client can tell the
// person. A run that waits for a connection names the dashboard route.
func runResult(r *fw.Run) *sdk.CallToolResult { return runResultAt(r, "") }

// runResultAt is runResult with base, the address of the server, so a run
// that waits for a connection gives an absolute link (see server.base).
func runResultAt(r *fw.Run, base string) *sdk.CallToolResult {
	text := runText(r, base)
	if note := extraNote(r); note != "" {
		text += "\n\n" + note
	}
	return &sdk.CallToolResult{
		Content:           []sdk.Content{&sdk.TextContent{Text: text}},
		StructuredContent: r,
		IsError:           r.State == fw.StateFailed,
	}
}

// extraNote names the tools that a run has beyond the tool list of its
// agent, or "" when it has none.
func extraNote(r *fw.Run) string {
	if len(r.ExtraTools) == 0 {
		return ""
	}
	return fmt.Sprintf("For this run only, the inline profile gives %s tools that its profile does not list: %s. Tell the person.", r.Agent, strings.Join(r.ExtraTools, ", "))
}

// workError is the error result of a work tool for a kernel error. An
// authorization service that did not answer gets the text of explain,
// which tells the caller to try again. Every other error keeps its text
// after the prefix.
func workError(prefix string, err error) *sdk.CallToolResult {
	if errors.Is(err, fw.ErrAuthzUnavailable) {
		return failResult(err)
	}
	return errorResult(prefix + err.Error())
}

func errorResult(msg string) *sdk.CallToolResult {
	return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: msg}}, IsError: true}
}

// runText says what the state of a run means and what to do next. base is
// the address of the server, or "".
func runText(r *fw.Run, base string) string {
	switch r.State {
	case fw.StateCompleted:
		if r.Output != "" {
			return r.Output
		}
		return fmt.Sprintf("Run %s completed with no output.", r.ID)
	case fw.StateFailed:
		msg := fmt.Sprintf("Run %s failed: %s", r.ID, r.Error)
		if r.Output != "" {
			msg += "\n\n" + r.Output
		}
		return msg
	case fw.StateCanceled:
		return fmt.Sprintf("Run %s was canceled.", r.ID)
	case fw.StateInputRequired:
		if r.Pending != nil && r.Pending.Connect != nil {
			return connectText(r, base)
		}
		return pendingText(r)
	}
	return fmt.Sprintf("Run %s is %s. Read it with run_status or the resource fw://runs/%s. Stop it with the cancel tool.", r.ID, r.State, r.ID)
}

// pendingText names the pending approval and the two ways to decide it.
func pendingText(r *fw.Run) string {
	p := r.Pending
	if p == nil {
		return fmt.Sprintf("Run %s waits for input. Read it with run_status.", r.ID)
	}
	endpoint := fmt.Sprintf("POST /v1/runs/%s/approvals/%s", r.ID, p.ID)
	if len(p.Schema) > 0 {
		return fmt.Sprintf("Run %s asks a question: %s\nThe answer has this schema: %s\n"+
			"Answer with the approve tool (runId %s, approvalId %s, decision allow, and the answer in content, or in note for a free-text answer), "+
			"or with %s on the native API. The run waits for an answer from any surface.",
			r.ID, p.Reason, string(p.Schema), r.ID, p.ID, endpoint)
	}
	msg := fmt.Sprintf("Run %s waits for an approval. The tool %s wants to run with this input:\n%s\n", r.ID, p.Tool, inputText(p.Input))
	if p.Reason != "" {
		msg += "The policy asks because of " + p.Reason + ".\n"
	}
	msg += fmt.Sprintf("Decide with the approve tool (runId %s, approvalId %s, decision allow or deny), "+
		`or with %s and the body {"decision":"allow"} or {"decision":"deny"}. The run waits for a decision from any surface.`,
		r.ID, p.ID, endpoint)
	return msg
}

// inputText is a tool input for a person to read.
func inputText(in json.RawMessage) string {
	if len(in) == 0 {
		return "{}"
	}
	return truncate(string(in), 2000)
}
