package mcp

// This file holds the management tools and the dashboard resource. The
// tools let a person or a model see who they are, list and edit the agent
// profiles that they own, list runs, and read a run's events. The tool
// usage stays for old clients and is deprecated: operators read the
// Prometheus metrics.
// Only the tool dashboard points at the dashboard resource through
// _meta.ui.resourceUri, so a host that supports MCP Apps (SEP-1865) shows
// the page once, when the model opens it. The other tools return a text
// for the model and a structured value for the page, which calls them
// through the host. A host without MCP Apps reads the text.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/surface/view"
	"github.com/bdubs00/featherweight-workbench/agents"
	"github.com/bdubs00/featherweight-workbench/dashboard"
	"github.com/bdubs00/featherweight-workbench/fleet"
	"github.com/bdubs00/featherweight-workbench/manager"
	"github.com/bdubs00/featherweight-workbench/pools"
	"github.com/bdubs00/featherweight-workbench/skills"
	"github.com/bdubs00/featherweight-workbench/teams"
	"github.com/bdubs00/featherweight-workbench/titles"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Names of the management tools.
const (
	toolWhoami      = "whoami"
	toolAgentsList  = "agents_list"
	toolAgentGet    = "agent_get"
	toolAgentPut    = "agent_put"
	toolAgentDelete = "agent_delete"
	toolRunsList    = "runs_list"
	toolRunEvents   = "run_events"
	toolUsage       = "usage"
	toolDashboard   = "dashboard"
)

// Limits of a list.
const (
	defaultListLimit = 50
	maxListLimit     = 500
)

// Views of the dashboard that a tool result opens. home is the view of
// the dashboard tool by default: the work of the caller and of the teams of
// the caller (dashboard.Home) and the agents. The page knows the other
// names from the results of the management tools. usage is deprecated.
const (
	viewHome      = "home"
	viewWhoami    = "whoami"
	viewAgents    = "agents"
	viewAgent     = "agent"
	viewDraft     = "draft"
	viewRuns      = "runs"
	viewRun       = "run"
	viewApprovals = "approvals"
	viewUsage     = "usage"
)

// Input schemas of the management tools.
const (
	emptySchema = `{"type":"object","properties":{}}`

	agentNameSchema = `{"type":"string","minLength":1,"maxLength":64,"description":"The name of the agent profile."}`

	poolArgSchema = `{"type":"string","minLength":1,"description":"With pools only: the pool whose workers run the profile, when several pools run it, or the pool that gets a new profile. The fleet tool lists the pools."}`

	agentGetSchema = `{"type":"object","properties":{"name":` + agentNameSchema + `,"pool":` + poolArgSchema + `},"required":["name"]}`

	runsListProps = `
		"agent":{"type":"string","description":"Only the runs of this agent profile."},
		"state":{"type":"string","enum":["active","queued","running","input-required","completed","failed","canceled","suspended"],"description":"Only the runs in this state; active means every run that is not finished."},
		"principal":{"type":"string","description":"Only the runs that this principal started, by subject."},
		"by":{"type":"string","enum":["me","team","others"],"description":"Only a part of the runs: me keeps the runs that you started, team adds the runs of people on your teams, and others keeps every other run. The glance counts the same runs."},
		"parent":{"type":"string","description":"Only the child runs of this run."},
		"batch":{"type":"string","description":"Only the runs of this job, by the metadata fw.batch that enqueue sets."},
		"limit":{"type":"integer","minimum":1,"maximum":500,"description":"The most runs to return, newest first; the default is 50."}`

	runsListSchema = `{"type":"object","properties":{` + runsListProps + `}}`

	// runsListFleetSchema is runsListSchema with the arguments of the fleet.
	runsListFleetSchema = `{"type":"object","properties":{` + runsListProps + `,
		"scope":{"type":"string","enum":["fleet","local"],"description":"fleet, the default, lists the runs of every member of the fleet; local lists the runs of this process only."},
		"member":{"type":"string","description":"Only the runs of this fleet member; the fleet tool names the members."}
	}}`

	runEventsSchema = `{"type":"object","properties":{
		"runId":` + runIDSchema + `,
		"limit":{"type":"integer","minimum":1,"maximum":500,"description":"The most events to return; the default is 50."},
		"from":{"type":"integer","minimum":1,"description":"The sequence number to start from, to read forward. Without from and before the result is the last events of the run. The result names the next sequence number when more follow."},
		"before":{"type":"integer","minimum":1,"description":"Read the page of events that ends before this sequence number, to read back. The result names prev, the value of before for the page in front of it, until the page starts at the first event. Do not pass it with from."},
		"compact":{"type":"boolean","description":"true keeps the first 200 characters of each tool output and its size in bytes, so a poll of a busy run costs little. Read the page again without compact for the full outputs."}
	},"required":["runId"]}`

	dashboardViews = `"view":{"type":"string","enum":["home","run","agent","usage","agents","runs","approvals","fleet"],"description":"The view to open. home, the default, shows the work that needs you, the work in progress, the recent results, and your agents. run shows one run as a chat, and agent one agent profile. agents, runs, approvals, and fleet are older names of home. usage is deprecated: operators read the Prometheus metrics."},
		"agent":{"type":"string","description":"The agent profile that the agent view shows."},`

	dashboardSchema = `{"type":"object","properties":{
		` + dashboardViews + `
		"runId":{"type":"string","description":"The run that the run view shows."}
	}}`

	// dashboardFleetSchema is dashboardSchema for a run of any member.
	dashboardFleetSchema = `{"type":"object","properties":{
		` + dashboardViews + `
		"runId":{"type":"string","description":"The run that the run view shows. It can be a run of any member of the fleet."}
	}}`
)

// schemaOf picks the schema with the fleet arguments when the fleet is on.
func schemaOf(a *fw.Agent, plain, withFleet string) json.RawMessage {
	if fleet.Of(a) != nil {
		return json.RawMessage(withFleet)
	}
	return json.RawMessage(plain)
}

// agentPutSchema is the input schema of agent_put. It mirrors fw.AgentProfile.
func agentPutSchema() json.RawMessage {
	globList := func(what string) map[string]any {
		return map[string]any{"type": "array", "items": map[string]any{"type": "string", "minLength": 1}, "maxItems": 64, "description": what}
	}
	ruleList := func(what string) map[string]any {
		return map[string]any{"type": "array", "items": map[string]any{"type": "string", "minLength": 1}, "description": what}
	}
	entryList := func(what string) map[string]any {
		return map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": what}
	}
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"name":         map[string]any{"type": "string", "minLength": 1, "maxLength": 64, "description": "The name of the profile: letters, digits, '-', '_', and '.'. An existing name replaces that profile."},
			"description":  map[string]any{"type": "string", "description": "One line that says what the agent does."},
			"system":       map[string]any{"type": "string", "description": "The system prompt."},
			"instructions": map[string]any{"type": "array", "items": map[string]any{"type": "string", "minLength": 1}, "description": "Texts that follow the system prompt, in order."},
			// A profile from agent_get or a proposal carries "skills": null
			// for every skill, so the schema takes null as absent.
			"skills": map[string]any{"type": []string{"array", "null"}, "items": map[string]any{"type": "string", "minLength": 1}, "maxItems": 64,
				"description": "Globs over skill names that the agent can use; an absent or null list allows every skill, and an empty list allows none."},
			"tools":    globList("Globs over tool names that the agent can call, such as files or git.*; an absent or empty list allows every tool."),
			"model":    map[string]any{"type": "string", "description": "A provider/model selector, such as anthropic/claude-haiku-5-5, or a name from the models map of the configuration."},
			"effort":   map[string]any{"type": "string", "enum": append([]string{""}, fw.Efforts...), "description": "The reasoning effort of the model."},
			"timeZone": map[string]any{"type": "string", "description": "The IANA time zone of the runs of the agent, for example America/Chicago. Empty uses the zone of the configuration."},
			"policy": map[string]any{
				"type":        "object",
				"description": "The tool policy: a default verdict and lists of flags, tool names, or tool: command globs.",
				"properties": map[string]any{
					"default": map[string]any{"type": "string", "enum": []string{"allow", "ask", "deny"}, "description": "The verdict when no rule matches."},
					"allow":   ruleList("Rules that allow a call, such as readonly or files."),
					"ask":     ruleList("Rules that hold a call for an approval, such as destructive or bash: git push*."),
					"deny":    ruleList("Rules that refuse a call, such as bash: rm -rf*."),
				},
			},
			"budget":     budgetSchema("The limits of every run of the agent."),
			"outcome":    outcomeSchema("A grade of the answer of every run of the agent: a rubric, an optional grader model, and the number of passes."),
			"compaction": compactionSchema("When the runs of the agent compact their history: at, model, and off over the compaction block of the configuration."),
			"access": map[string]any{
				"type":        "object",
				"description": "Who owns and uses the agent. Each entry is a subject, a glob over subjects, group:<name>, kind:<kind>, issuer:<url>, or *. Without access, a new profile gets you as its owner, and a replaced profile keeps the access block it has.",
				"properties": map[string]any{
					"owners":    entryList("Owners manage the profile and do everything an operator does."),
					"operators": entryList("Operators start runs, send follow-ups, approve, and cancel."),
					"viewers":   entryList("Viewers read the runs and their ledgers."),
				},
			},
		},
		"required": []string{"name"},
	}
	schema["properties"].(map[string]any)["dryRun"] = map[string]any{"type": "boolean",
		"description": "true checks the profile and says in plain words what the agent can do, and saves nothing."}
	schema["properties"].(map[string]any)["pool"] = json.RawMessage(poolArgSchema)
	schema["properties"].(map[string]any)["workers"] = json.RawMessage(workersArgSchema)
	b, _ := json.Marshal(schema)
	return b
}

// workersArgSchema is the argument workers of agent_put.
const workersArgSchema = `{"type":"object","description":"With worker templates only: the workers of a new agent that no pool runs. The front creates a runtime pool named after the agent from template, else the default template, with replicas workers, 2 when it is left out, and writes the profile into the overlay of that pool. Without workers, a new agent gets such a pool when a default template exists and pool is left out. templates_list names the templates.","properties":{
	"template":{"type":"string","minLength":1,"description":"The worker template of the pool; the default template when it is left out."},
	"replicas":{"type":"integer","minimum":0,"maximum":64,"description":"The number of workers; 2 when it is left out."}}}`

// Arguments of the management tools.
type (
	agentNameArgs struct {
		Name string `json:"name"`
		Pool string `json:"pool,omitempty"`
	}
	agentPutArgs struct {
		profileArgs
		Name        string         `json:"name"`
		Description string         `json:"description,omitempty"`
		Access      *fw.Access     `json:"access,omitempty"`
		DryRun      bool           `json:"dryRun,omitempty"`
		Pool        string         `json:"pool,omitempty"`
		Workers     *fleet.Workers `json:"workers,omitempty"`
	}
	runsListArgs struct {
		Agent     string `json:"agent,omitempty"`
		State     string `json:"state,omitempty"`
		Principal string `json:"principal,omitempty"`
		By        string `json:"by,omitempty"`
		Limit     int    `json:"limit,omitempty"`
		Scope     string `json:"scope,omitempty"`
		Member    string `json:"member,omitempty"`
		Parent    string `json:"parent,omitempty"`
		Batch     string `json:"batch,omitempty"`
	}
	runEventsArgs struct {
		RunID   string `json:"runId"`
		Limit   int    `json:"limit,omitempty"`
		From    uint64 `json:"from,omitempty"`
		Before  uint64 `json:"before,omitempty"`
		Compact bool   `json:"compact,omitempty"`
	}
	dashboardArgs struct {
		View  string `json:"view,omitempty"`
		Agent string `json:"agent,omitempty"`
		RunID string `json:"runId,omitempty"`
	}
)

// Structured results of the management tools. Each carries the view that
// the dashboard opens for it.
type (
	whoamiResult struct {
		View string `json:"view"`
		dashboard.Whoami
	}
	// With pools, each agent of agentsResult and agentResult names its
	// pool, or carries front for a profile of this process (section 14.9).
	agentsResult struct {
		View   string            `json:"view"`
		Agents []fleet.PoolAgent `json:"agents"`
	}
	// Pool is the runtime pool that agent_put created for a new agent, or
	// the pool that a save creates, in a dry run.
	agentResult struct {
		View    string        `json:"view"`
		Agent   agentDoc      `json:"agent"`
		Created bool          `json:"created,omitempty"`
		Pool    *pools.Status `json:"pool,omitempty"`
	}
	// agentDoc is one agent with ResolvedSkills, the skills of the catalog
	// of the front that the allowlist of the profile allows (section
	// 20.2). The field skills stays the allowlist of the profile.
	agentDoc struct {
		fleet.PoolAgent
		ResolvedSkills []skills.Ref `json:"resolvedSkills"`
	}
	draftResult struct {
		View  string        `json:"view"`
		Draft agents.Draft  `json:"draft"`
		Pool  *pools.Status `json:"pool,omitempty"`
	}
	deletedResult struct {
		View    string `json:"view"`
		Deleted string `json:"deleted"`
	}
	runsResult struct {
		View   string      `json:"view"`
		Runs   []*fw.Run   `json:"runs"`
		Total  int         `json:"total"`
		Glance view.Glance `json:"glance"`
	}
	eventsResult struct {
		View   string           `json:"view"`
		Run    *fw.Run          `json:"run"`
		Events []view.EventLine `json:"events"`
		Items  []dashboard.Item `json:"items"`
		Next   uint64           `json:"next,omitempty"`
		Prev   uint64           `json:"prev,omitempty"`
	}
	usageResult struct {
		View string `json:"view"`
		dashboard.Report
	}
	dashboardResult struct {
		View    string           `json:"view"`
		Whoami  dashboard.Whoami `json:"whoami"`
		Glance  view.Glance      `json:"glance"`
		Agents  []view.AgentView `json:"agents"`
		Runs    []*fw.Run        `json:"runs"`
		Home    *dashboard.Home  `json:"home,omitempty"` // the view home only
		Agent   *view.AgentView  `json:"agent,omitempty"`
		Run     *fw.Run          `json:"run,omitempty"`
		Events  []view.EventLine `json:"events,omitempty"`
		Items   []dashboard.Item `json:"items,omitempty"`
		Name    string           `json:"name,omitempty"`
		Members []fleet.Member   `json:"members,omitempty"`
		PageURL string           `json:"pageUrl,omitempty"`
	}
)

// addManageTools adds the management tools and the dashboard resource.
func (s *server) addManageTools() {
	readOnly := func() *sdk.ToolAnnotations {
		return &sdk.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true, OpenWorldHint: ptr(false)}
	}
	sdk.AddTool(s.srv, &sdk.Tool{
		Name:        toolWhoami,
		Title:       "Who am I",
		Description: "Return the principal of the caller (subject, name, kind, groups) and its role on every agent it can see: owner, operator, or viewer.",
		InputSchema: json.RawMessage(emptySchema),
		Annotations: readOnly(),
	}, s.whoami)
	sdk.AddTool(s.srv, &sdk.Tool{
		Name:        toolAgentsList,
		Title:       "List agents",
		Description: "List the agent profiles that the caller can view, with the description, the model, the tools, the skills, the access lists, and the role of the caller. With pools, the list holds the profiles that the workers of each pool run, each with its pool, and the profiles of this process, marked front.",
		InputSchema: json.RawMessage(emptySchema),
		Annotations: readOnly(),
	}, s.agentsList)
	sdk.AddTool(s.srv, &sdk.Tool{
		Name:        toolAgentGet,
		Title:       "Get an agent",
		Description: "Return one agent profile in full, with the role of the caller and what the agent can do in plain words. With pools, it returns the profile that the workers of the pool run, with the policy of the workers; pass pool when several pools run it.",
		InputSchema: json.RawMessage(agentGetSchema),
		Annotations: readOnly(),
	}, s.agentGet)
	sdk.AddTool(s.srv, &sdk.Tool{
		Name:        toolAgentPut,
		Title:       "Create or replace an agent",
		Description: "Create an agent profile, or replace the one with that name. The arguments mirror the profile: name, description, system, instructions, skills, tools, model, effort, timeZone, policy, budget, and access. Creating needs the creator role, and replacing needs the owner role on the agent. Without access, a new profile gets the caller as its owner, and a replaced profile keeps the access block it has. With pools, the profile goes to the pool whose workers run it, or to the pool that pool names, and every worker of the pool takes it. With worker templates, a new agent that no pool runs gets workers in one call: with workers, or when a default template exists and pool is left out, the front creates a runtime pool named after the agent from the template, with 2 workers unless workers names replicas, writes the profile into that pool, and returns the pool. Without a default template and without workers, the error names the templates.",
		InputSchema: agentPutSchema(),
		Annotations: &sdk.ToolAnnotations{DestructiveHint: ptr(true), IdempotentHint: true, OpenWorldHint: ptr(false)},
	}, s.agentPut)
	sdk.AddTool(s.srv, &sdk.Tool{
		Name:        toolAgentDelete,
		Title:       "Delete an agent",
		Description: "Remove an agent profile. It needs the owner role on the agent. The runs of the agent stay in the ledger. With pools, it removes what the front added to the pool; a profile of the configuration of the pool stays.",
		InputSchema: json.RawMessage(agentGetSchema),
		Annotations: &sdk.ToolAnnotations{DestructiveHint: ptr(true), IdempotentHint: true, OpenWorldHint: ptr(false)},
	}, s.agentDelete)
	sdk.AddTool(s.srv, &sdk.Tool{
		Name:        toolRunsList,
		Title:       "List runs",
		Description: "List the runs that the caller can view, newest first, with optional filters by agent, state, and principal. Each run carries its state, agent, principal, parent, depth, pending approval, and usage. With the fleet on, the list holds the runs of every member, and each run of another member names its member.",
		InputSchema: schemaOf(s.a, runsListSchema, runsListFleetSchema),
		Annotations: readOnly(),
	}, s.runsList)
	sdk.AddTool(s.srv, &sdk.Tool{
		Name:        toolRunEvents,
		Title:       "Run events",
		Description: "Return the last events of a run as short lines, newest last, with the run object. Pass from to page through the ledger from a sequence number, and compact to cut each tool output to its first 200 characters.",
		InputSchema: json.RawMessage(runEventsSchema),
		Annotations: readOnly(),
	}, s.runEvents)
	s.addFileTool(readOnly)
	s.addWorkViewTools(readOnly)
	sdk.AddTool(s.srv, &sdk.Tool{
		Name:        toolUsage,
		Title:       "Usage (deprecated)",
		Description: "Deprecated: operators read the Prometheus metrics of fw, at /metrics by default, for tokens, cost, runs, and latency. This tool stays for old clients: it sums the runs, turns, tokens, and cost of the runs that the caller can view, in total, by agent, and by principal.",
		InputSchema: json.RawMessage(emptySchema),
		Annotations: readOnly(),
	}, s.usage)
	sdk.AddTool(s.srv, &sdk.Tool{
		Name:        toolDashboard,
		Title:       "Dashboard",
		Description: "Open the fw dashboard, an MCP App that starts work, shows the work that needs you, the work in progress, and the recent results, decides pending approvals, and edits and builds agents. A host without MCP Apps gets the home as text; the management tools do the same work one call at a time.",
		InputSchema: schemaOf(s.a, dashboardSchema, dashboardFleetSchema),
		Annotations: readOnly(),
		Meta:        sdk.Meta(dashboard.ToolMeta()),
	}, s.dashboard)
	s.addFleetTool()
	s.srv.AddResource(&sdk.Resource{
		URI:         dashboard.URI,
		Name:        "dashboard",
		Title:       "fw dashboard",
		Description: "The management dashboard of fw as an MCP App: one HTML page that calls the management tools through the host.",
		MIMEType:    dashboard.MIMEType,
		Meta:        sdk.Meta(dashboard.ResourceMeta()),
	}, s.readDashboard)
}

func (s *server) readDashboard(_ context.Context, req *sdk.ReadResourceRequest) (*sdk.ReadResourceResult, error) {
	if req.Params.URI != dashboard.URI {
		return nil, sdk.ResourceNotFoundError(req.Params.URI)
	}
	return &sdk.ReadResourceResult{Contents: []*sdk.ResourceContents{{
		URI:      dashboard.URI,
		MIMEType: dashboard.MIMEType,
		Text:     string(dashboard.Page()),
		Meta:     sdk.Meta(dashboard.ResourceMeta()),
	}}}, nil
}

// result builds a tool result with a text for the model and a structured
// value for the dashboard.
func result(text string, structured any) *sdk.CallToolResult {
	return &sdk.CallToolResult{
		Content:           []sdk.Content{&sdk.TextContent{Text: text}},
		StructuredContent: structured,
	}
}

// failResult maps a kernel error to an error result that a person and a
// model can read.
func failResult(err error) *sdk.CallToolResult {
	return errorResult(explain(err))
}

// explain is the text of a kernel error for a tool result.
func explain(err error) string {
	var refusal *dashboard.SaveRefusal
	switch {
	case errors.Is(err, fw.ErrAuthzUnavailable):
		cause := err.Error()
		if _, after, ok := strings.Cut(cause, fw.ErrAuthzUnavailable.Error()); ok {
			cause = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(after), ":"))
		}
		msg := "Unavailable: the authorization service did not answer, so the request is refused. Try again in a few seconds."
		if cause != "" {
			msg += " Cause: " + cause + "."
		}
		return msg
	case errors.As(err, &refusal):
		return "Forbidden: " + refusal.Reason + "."
	case errors.Is(err, fw.ErrForbidden):
		detail := strings.TrimSpace(strings.TrimPrefix(err.Error(), fw.ErrForbidden.Error()))
		detail = strings.TrimSpace(strings.TrimPrefix(detail, ":"))
		if detail == "" {
			return "Forbidden: the caller is not allowed to do this. Ask an owner of the agent for a role."
		}
		return "Forbidden: " + detail + ". Ask an owner of the agent for that role."
	case errors.Is(err, fw.ErrProfileNotFound):
		name := err.Error()
		if i := strings.LastIndex(name, ": "); i >= 0 {
			name = name[i+2:]
		}
		return fmt.Sprintf("No agent profile is named %q. The agents_list tool names the profiles you can see.", name)
	case errors.Is(err, fw.ErrRunNotFound):
		return "No run has that id, or you cannot view it. The runs_list tool names the runs you can see."
	case errors.Is(err, fw.ErrInvalidRequest):
		return "Invalid request: " + strings.TrimPrefix(err.Error(), fw.ErrInvalidRequest.Error()+": ") + "."
	case errors.Is(err, fw.ErrAgentNotFound):
		return "No agent profile is named " + strings.TrimPrefix(err.Error(), fw.ErrAgentNotFound.Error()+": ") + ". The agents_list tool names the profiles you can see."
	case errors.Is(err, fw.ErrClosed):
		return "The agent is shutting down."
	case errors.Is(err, fleet.ErrNoCredential):
		return "Forbidden: this request carries no bearer credential, so it cannot act on a run of another fleet member. Connect with a bearer token."
	case errors.Is(err, fleet.ErrNoRoute):
		return "No route: " + strings.TrimPrefix(err.Error(), "fw: ") + "."
	}
	var me *memberError
	if errors.As(err, &me) {
		return me.text()
	}
	return err.Error()
}

func (s *server) whoami(ctx context.Context, _ *sdk.CallToolRequest, _ struct{}) (*sdk.CallToolResult, any, error) {
	ctx, _, _ = manager.OnBehalf(ctx)
	w, err := dashboard.WhoamiOf(ctx, s.a)
	if err != nil {
		return failResult(err), nil, nil
	}
	return result(w.String(), whoamiResult{View: viewWhoami, Whoami: w}), nil, nil
}

func (s *server) agentsList(ctx context.Context, _ *sdk.CallToolRequest, _ struct{}) (*sdk.CallToolResult, any, error) {
	ctx, _, _ = manager.OnBehalf(ctx)
	own, err := view.Agents(ctx, s.a)
	if err != nil {
		return failResult(err), nil, nil
	}
	list, pooled, err := fleet.PoolAgents(ctx, s.a)
	if err != nil {
		return failResult(err), nil, nil
	}
	// With pools, the profiles of this process come after the profiles
	// that the workers run, marked as the profiles of the front.
	agents := make([]fleet.PoolAgent, 0, len(list)+len(own))
	agents = append(agents, list...)
	for _, v := range own {
		agents = append(agents, fleet.PoolAgent{AgentView: v, Front: pooled})
	}
	var b strings.Builder
	if len(agents) == 0 {
		b.WriteString("You can see no agent profile.")
	} else {
		fmt.Fprintf(&b, "%d agent profiles you can see:\n", len(agents))
	}
	for _, v := range agents {
		b.WriteString(agentLine(v))
	}
	return result(strings.TrimRight(b.String(), "\n"), agentsResult{View: viewAgents, Agents: agents}), nil, nil
}

// agentLine is one profile on one line, with the role and the usage, and
// with pools its place: the pool and the overlay state, or the front.
func agentLine(v fleet.PoolAgent) string {
	var b strings.Builder
	fmt.Fprintf(&b, "- %s (you are %s)", v.Name, roleWord(v.Role))
	switch {
	case v.Pool != "" && v.Overlay != "":
		fmt.Fprintf(&b, " in the pool %s, %s by the front%s", v.Pool, v.Overlay, byWhom(v))
	case v.Pool != "":
		fmt.Fprintf(&b, " in the pool %s", v.Pool)
	case v.Front:
		b.WriteString(" on the front only, which no worker of a pool runs")
	}
	if v.Description != "" {
		b.WriteString(": " + v.Description)
	}
	if v.Model != "" {
		b.WriteString("; model " + v.Model)
	}
	if v.Access != nil && len(v.Access.Owners) > 0 {
		b.WriteString("; owners " + strings.Join(v.Access.Owners, ", "))
	}
	b.WriteString("; " + v.Usage.String() + "\n")
	return b.String()
}

// roleWord is a role with its article.
func roleWord(role string) string {
	switch role {
	case "owner", "operator":
		return "an " + role
	case "viewer":
		return "a viewer"
	}
	return "not allowed"
}

func (s *server) agentGet(ctx context.Context, _ *sdk.CallToolRequest, in agentNameArgs) (*sdk.CallToolResult, any, error) {
	ctx, _, _ = manager.OnBehalf(ctx)
	v, err := s.agentNamed(ctx, in.Name, in.Pool)
	if err != nil {
		return failResult(err), nil, nil
	}
	doc := s.agentDoc(v)
	return result(agentText(v, doc.ResolvedSkills), agentResult{View: viewAgent, Agent: doc}), nil, nil
}

// agentDoc adds the resolved skills to an agent. A profile of a pool
// resolves against the skills of the front too, because the front knows
// no list of the skills of its workers (section 20.2).
func (s *server) agentDoc(v fleet.PoolAgent) agentDoc {
	return agentDoc{PoolAgent: v, ResolvedSkills: skills.Allowed(s.a, v.Skills)}
}

// agentNamed returns one profile as the caller sees it. With pools, it is
// the profile that the workers of a pool run, else the profile of this
// process, marked as the profile of the front.
func (s *server) agentNamed(ctx context.Context, name, pool string) (fleet.PoolAgent, error) {
	pa, found, err := fleet.PoolAgentNamed(ctx, s.a, name, strings.TrimSpace(pool))
	if err != nil || found {
		return pa, err
	}
	pooled := fleet.Pooled(s.a)
	switch {
	case pool != "" && pooled:
		return fleet.PoolAgent{}, fmt.Errorf("%w: %s in the pool %s", fw.ErrProfileNotFound, name, pool)
	case pool != "":
		return fleet.PoolAgent{}, fmt.Errorf("%w: this process keeps no pool, so leave out pool", fw.ErrInvalidRequest)
	}
	v, err := view.Agent(ctx, s.a, name)
	return fleet.PoolAgent{AgentView: v, Front: pooled}, err
}

// skillsText words the skills that an agent can load, from the catalog of
// the front. For a profile of a pool it says where the workers find their
// skills.
func skillsText(refs []skills.Ref, pooled bool) string {
	var b strings.Builder
	if len(refs) == 0 {
		b.WriteString("It can load no skill of the front.")
	} else {
		b.WriteString("It can load these skills of the front: ")
		for i, r := range refs {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(r.Name)
			if r.Origin != "" {
				b.WriteString(" from " + r.Origin)
			}
		}
		b.WriteString(".")
	}
	if pooled {
		b.WriteString(" Its workers load the local skills that the pool ships and the skills of the servers of their own tools.mcp entries.")
	}
	return b.String() + "\n"
}

// byWhom says who wrote the overlay entry of a profile, and when.
func byWhom(v fleet.PoolAgent) string {
	var out string
	if v.By != "" {
		out += " for " + v.By
	}
	if v.At != nil {
		out += " on " + v.At.UTC().Format(time.RFC3339)
	}
	return out
}

// agentText is a profile as text: the summary line, what the agent can do
// in plain words, the skills that it can load, the policy of the workers of
// its pool, then the profile as indented JSON.
func agentText(v fleet.PoolAgent, refs []skills.Ref) string {
	head := agentLine(v)
	if t := view.AbilityText(v.Abilities); t != "" {
		head += t + "\n"
	}
	head += skillsText(refs, v.Pool != "")
	if v.WorkerPolicy != nil {
		head += "The workers of the pool decide each call by fw.policy of the pool, which the policy of the profile can only tighten: " + rulesText(*v.WorkerPolicy) + "\n"
	}
	if v.Locked {
		head += "The configuration defines this agent, so change it there.\n"
	}
	b, err := json.MarshalIndent(v.AgentProfile, "", "  ")
	if err != nil {
		return head
	}
	return head + string(b)
}

func (s *server) agentPut(ctx context.Context, _ *sdk.CallToolRequest, in agentPutArgs) (*sdk.CallToolResult, any, error) {
	ctx, act, acting := manager.OnBehalf(ctx)
	p, err := in.profile("")
	if err != nil {
		return errorResult("Invalid request: " + err.Error() + "."), nil, nil
	}
	p.Name, p.Description, p.Access = in.Name, in.Description, in.Access
	at, err := fleet.PlaceFor(ctx, s.a, p.Name, in.Pool, in.Workers)
	if err != nil {
		return failResult(err), nil, nil
	}
	pool := fleet.PoolOf(at)
	if in.DryRun {
		d, err := agents.PreviewIn(ctx, at, p)
		if err != nil {
			return failResult(err), nil, nil
		}
		verb := "create"
		if d.Exists {
			verb = "replace"
		}
		where := ""
		if pool != "" {
			where = " in the pool " + pool
		}
		text := fmt.Sprintf("Checked the agent profile %s%s. Saving it would %s the agent. Nothing is saved.\n", d.Name, where, verb)
		res := draftResult{View: viewDraft, Draft: d}
		if planned, ok := fleet.PlannedPool(at); ok {
			text = fmt.Sprintf("Checked the agent profile %s. A save creates the agent and its runtime pool %s from the template %s, with %d %s. Nothing is saved.\n", d.Name, planned.Name, planned.Template, planned.Wanted, workerWord(planned.Wanted))
			res.Pool = &planned
		}
		text += agentText(fleet.PoolAgent{AgentView: d.AgentView, Pool: pool}, skills.Allowed(s.a, d.Skills))
		return result(text, res), nil, nil
	}
	saved, created, err := agents.PutIn(ctx, s.a, at, p)
	if err != nil {
		return failResult(err), nil, nil
	}
	v := fleet.PoolAgent{AgentView: saved, Pool: pool}
	if pool != "" {
		if pa, ok, err := fleet.PoolAgentNamed(ctx, s.a, saved.Name, pool); err == nil && ok {
			v = pa
		}
	}
	verb := "Replaced"
	if created {
		verb = "Created"
	}
	made := fleet.CreatedPool(at)
	if acting {
		s.log.Info("mcp profile saved for an actor", "profile", v.Name, "created", created, "pool", pool, "createdPool", made, "principal", subjectOf(ctx), "actor", act.String())
	} else {
		s.log.Info("mcp profile saved", "profile", v.Name, "created", created, "pool", pool, "createdPool", made, "principal", subjectOf(ctx))
	}
	doc := s.agentDoc(v)
	res := agentResult{View: viewAgent, Agent: doc, Created: created}
	text := fmt.Sprintf("%s the agent profile %s.\n", verb, v.Name)
	switch {
	case made != "":
		st, ok, err := fleet.PoolNamed(ctx, s.a, made)
		if err == nil && ok {
			res.Pool = &st
			text = fmt.Sprintf("Created the agent profile %s and its runtime pool %s from the template %s, with %d %s. The front creates the workers at its next pass, and each takes the profile at its start.\n", v.Name, made, st.Template, st.Wanted, workerWord(st.Wanted))
		} else {
			text = fmt.Sprintf("Created the agent profile %s and its runtime pool %s. The front creates the workers at its next pass.\n", v.Name, made)
		}
	case pool != "":
		text = fmt.Sprintf("%s the agent profile %s in the pool %s. The front replaces each Task of the pool once it is idle, so each worker takes the profile at its next start.\n", verb, v.Name, pool)
		if in.Workers != nil {
			text += fmt.Sprintf("The pool %s runs the agent already, so workers changes nothing; pool_put changes the workers of a runtime pool.\n", pool)
		}
	}
	return result(text+agentText(v, doc.ResolvedSkills), res), nil, nil
}

// workerWord is worker or workers for n.
func workerWord(n int) string {
	if n == 1 {
		return "worker"
	}
	return "workers"
}

func (s *server) agentDelete(ctx context.Context, _ *sdk.CallToolRequest, in agentNameArgs) (*sdk.CallToolResult, any, error) {
	ctx, act, acting := manager.OnBehalf(ctx)
	at, err := fleet.PlaceOf(ctx, s.a, in.Name, in.Pool)
	if err != nil {
		return failResult(err), nil, nil
	}
	if err := agents.DeleteIn(ctx, s.a, at, in.Name); err != nil {
		return failResult(err), nil, nil
	}
	pool := fleet.PoolOf(at)
	if acting {
		s.log.Info("mcp profile deleted for an actor", "profile", in.Name, "pool", pool, "principal", subjectOf(ctx), "actor", act.String())
	} else {
		s.log.Info("mcp profile deleted", "profile", in.Name, "pool", pool, "principal", subjectOf(ctx))
	}
	text := fmt.Sprintf("Deleted the agent profile %s. Its runs stay in the ledger.", in.Name)
	switch {
	case fleet.DroppedPool(at) != "":
		text = fmt.Sprintf("Deleted the agent profile %s and its runtime pool %s. The front deletes each worker of the pool once it is idle, and then the pool. Its runs stay in the ledgers.", in.Name, pool)
	case pool != "":
		text = fmt.Sprintf("Removed the agent profile %s from what the front adds to the pool %s. Each worker of the pool runs the configuration of the pool again after the front replaces its Task. Its runs stay in the ledgers.", in.Name, pool)
	}
	return result(text, deletedResult{View: viewAgents, Deleted: in.Name}), nil, nil
}

// rulesText words a policy in one line: its default and its lists.
func rulesText(r fw.Rules) string {
	def := r.Default
	if def == "" {
		def = "allow"
	}
	parts := []string{"by default " + def}
	for _, l := range []struct {
		lead string
		list []string
	}{{"asks first for ", r.Ask}, {"never runs ", r.Deny}, {"allows ", r.Allow}} {
		if len(l.list) > 0 {
			parts = append(parts, l.lead+strings.Join(l.list, ", "))
		}
	}
	if len(r.All) > 0 {
		parts = append(parts, fmt.Sprintf("and %d more clauses, all of which must allow a call", len(r.All)))
	}
	return strings.Join(parts, "; ") + "."
}

func (s *server) runsList(ctx context.Context, req *sdk.CallToolRequest, in runsListArgs) (*sdk.CallToolResult, any, error) {
	ctx, _, _ = manager.OnBehalf(ctx)
	var runs runsResult
	var err error
	if in.Parent != "" {
		runs, err = s.childRuns(withCredential(ctx, req), in.Parent, in.Agent, in.State, in.Principal, in.By, in.Limit)
	} else {
		runs, err = s.visibleRuns(ctx, in.Agent, in.State, in.Principal, in.By, in.Scope, in.Member, in.Batch, in.Limit)
	}
	if err != nil {
		return failResult(err), nil, nil
	}
	var b strings.Builder
	if len(runs.Runs) == 0 {
		b.WriteString("No run matches.")
	} else {
		fmt.Fprintf(&b, "%d of %d runs, newest first:\n", len(runs.Runs), runs.Total)
	}
	for _, r := range runs.Runs {
		b.WriteString(runLine(r))
	}
	return result(strings.TrimRight(b.String(), "\n"), runs), nil, nil
}

// visibleRuns lists the runs that the caller can view after the filters.
// With the fleet on, the scope fleet, the default, adds the runs of the
// other members, and member keeps the runs of one member.
func (s *server) visibleRuns(ctx context.Context, agent, state, principal, by, scope, member, batch string, limit int) (runsResult, error) {
	all, err := dashboard.Runs(ctx, s.a, scope, member)
	if err != nil {
		return runsResult{}, err
	}
	all = view.FilterBatch(all, batch)
	titles.Want(s.a, all...)
	if all, err = teams.By(ctx, s.a.Config(), all, by); err != nil {
		return runsResult{}, err
	}
	return listOf(all, agent, state, principal, limit), nil
}

// childRuns lists the child runs of a run that the caller can view, after
// the filters. The parent can belong to another member of the fleet.
func (s *server) childRuns(ctx context.Context, parent, agent, state, principal, by string, limit int) (runsResult, error) {
	kids, err := dashboard.Children(ctx, s.a, parent)
	if err != nil {
		return runsResult{}, err
	}
	if kids, err = teams.By(ctx, s.a.Config(), kids, by); err != nil {
		return runsResult{}, err
	}
	return listOf(kids, agent, state, principal, limit), nil
}

// listOf filters runs, counts the glance over every run that matches, and
// then cuts the list to the limit.
func listOf(all []*fw.Run, agent, state, principal string, limit int) runsResult {
	switch {
	case limit <= 0:
		limit = defaultListLimit
	case limit > maxListLimit:
		limit = maxListLimit
	}
	matched := view.Filter(all, agent, state, principal, 0)
	runs := view.Filter(matched, "", "", "", limit)
	return runsResult{View: viewRuns, Runs: runs, Total: len(matched), Glance: view.GlanceOf(matched, time.Now())}
}

// runLine is one run on one line. A child run is indented under its parent.
func runLine(r *fw.Run) string {
	var b strings.Builder
	b.WriteString("- ")
	b.WriteString(strings.Repeat("  ", r.Depth))
	fmt.Fprintf(&b, "%s %s on %s", r.ID, r.State, r.Agent)
	if r.Member != "" {
		b.WriteString(" of the member " + r.Member)
	}
	if r.Principal != "" {
		b.WriteString(" by " + r.Principal)
	}
	if r.Pending != nil {
		b.WriteString(", waiting for an approval of " + r.Pending.Tool)
	}
	fmt.Fprintf(&b, ", %d turns, %d tokens, %s ago", r.Usage.Turns, r.Usage.InputTokens+r.Usage.OutputTokens, age(r.UpdatedAt))
	if t := strings.Join(strings.Fields(r.Input), " "); t != "" {
		b.WriteString(": " + truncate(t, 80))
	}
	b.WriteString("\n")
	return b.String()
}

// age is how long ago a time was, in one unit.
func age(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

func (s *server) runEvents(ctx context.Context, req *sdk.CallToolRequest, in runEventsArgs) (*sdk.CallToolResult, any, error) {
	ctx, _, _ = manager.OnBehalf(ctx)
	if in.From > 0 && in.Before > 0 {
		return errorResult("Invalid request: pass from to read forward or before to read back, not both."), nil, nil
	}
	res, err := s.events(withCredential(ctx, req), in.RunID, page{from: in.From, before: in.Before, limit: in.Limit})
	if err != nil {
		return failResult(err), nil, nil
	}
	if in.Compact {
		compact(res.Items)
	}
	return result(eventsText(res), res), nil, nil
}

// compactOutput is how many characters of each tool output a compact page
// of events keeps.
const compactOutput = 200

// compact cuts each tool output of a page to its first compactOutput
// characters and sets Size to the bytes of the output that the page held,
// so a poll of a busy run costs little.
func compact(items []dashboard.Item) {
	for i := range items {
		it := &items[i]
		if it.Kind != dashboard.ItemResult || utf8.RuneCountInString(it.Text) <= compactOutput {
			continue
		}
		it.Size = int64(len(it.Text))
		it.Text, it.Cut = string([]rune(it.Text)[:compactOutput]), true
	}
}

// page names a page of the events of a run: from reads forward from a
// sequence number, before reads back from one, and neither reads the last
// events. Only one of from and before is set.
type page struct {
	from, before uint64
	limit        int
}

// events reads a run and a page of its events. The read of the run
// authorizes the caller. A run of another member comes from that member.
func (s *server) events(ctx context.Context, id string, p page) (eventsResult, error) {
	switch {
	case p.limit <= 0:
		p.limit = defaultListLimit
	case p.limit > maxListLimit:
		p.limit = maxListLimit
	}
	r, err := s.a.Get(ctx, id)
	if errors.Is(err, fw.ErrRunNotFound) {
		member, ferr := s.ownerOf(ctx, id)
		if ferr != nil {
			return eventsResult{}, ferr
		}
		if member != "" {
			return s.remoteEvents(ctx, member, id, p)
		}
	}
	if err != nil {
		return eventsResult{}, err
	}
	if p.before > 0 {
		events, prev, err := dashboard.TranscriptBefore(ctx, s.a, id, p.before, p.limit)
		if err != nil {
			return eventsResult{}, err
		}
		res := pageOf(r, events, 0)
		res.Prev = prev
		return res, nil
	}
	events, next, err := view.EventsPage(ctx, s.a, id, p.from, p.limit)
	if err != nil {
		return eventsResult{}, err
	}
	return pageOf(r, events, next), nil
}

// pageOf is a page of events in the shape of run_events: the lines for the
// text, and the chat items for the page. prev is the first sequence number
// of the page, the before of the page in front of it, and 0 when the page
// starts at the first event.
func pageOf(r *fw.Run, events []fw.Event, next uint64) eventsResult {
	res := eventsResult{View: viewRun, Run: r, Events: make([]view.EventLine, 0, len(events)), Items: dashboard.Transcript(events), Next: next}
	for _, e := range events {
		res.Events = append(res.Events, view.Line(e))
	}
	if len(events) > 0 && events[0].Seq > 1 {
		res.Prev = events[0].Seq
	}
	return res
}

// eventsText is a page of events as text.
func eventsText(res eventsResult) string {
	var b strings.Builder
	r := res.Run
	fmt.Fprintf(&b, "Run %s is %s on %s", r.ID, r.State, r.Agent)
	if r.Pending != nil {
		b.WriteString(", waiting for an approval of " + r.Pending.Tool)
	}
	fmt.Fprintf(&b, " (%d events).", r.Seq)
	if len(res.Events) == 0 {
		b.WriteString("\nNo event on this page.")
	} else {
		fmt.Fprintf(&b, "\nEvents %d to %d:", res.Events[0].Seq, res.Events[len(res.Events)-1].Seq)
	}
	for _, e := range res.Events {
		fmt.Fprintf(&b, "\n%5d %s %s", e.Seq, e.Time.UTC().Format("15:04:05"), e.Line)
	}
	if res.Prev > 0 {
		fmt.Fprintf(&b, "\nEarlier events: call run_events with before %d.", res.Prev)
	}
	if res.Next > 0 {
		fmt.Fprintf(&b, "\nMore follow: call run_events with from %d.", res.Next)
	}
	return b.String()
}

func (s *server) usage(ctx context.Context, _ *sdk.CallToolRequest, _ struct{}) (*sdk.CallToolResult, any, error) {
	ctx, _, _ = manager.OnBehalf(ctx)
	rep, err := dashboard.Usages(ctx, s.a)
	if err != nil {
		return failResult(err), nil, nil
	}
	return result(rep.String(), usageResult{View: viewUsage, Report: rep}), nil, nil
}

// usageDeprecated is the first line of the usage view of the dashboard.
const usageDeprecated = "The usage view is deprecated. Operators read the Prometheus metrics of fw, at /metrics by default."

// dashboard answers with everything the page needs to open: the caller,
// the glance, the agents, and the runs, plus the home, the agent, or the
// run that the view names, and the members of the fleet when it is on. The
// text carries the same data for a host without MCP Apps, and it ends with
// the address of the page when one is known.
func (s *server) dashboard(ctx context.Context, req *sdk.CallToolRequest, in dashboardArgs) (*sdk.CallToolResult, any, error) {
	ctx, _, _ = manager.OnBehalf(ctx)
	ctx = withCredential(ctx, req)
	which := strings.ToLower(strings.TrimSpace(in.View))
	switch which {
	case "", viewAgents, viewRuns, viewApprovals, viewFleet:
		which = viewHome
	case viewHome, viewRun, viewAgent, viewUsage:
	default:
		return failResult(fmt.Errorf("%w: the which %q is not home, run, or agent", fw.ErrInvalidRequest, in.View)), nil, nil
	}
	who, err := dashboard.WhoamiOf(ctx, s.a)
	if err != nil {
		return failResult(err), nil, nil
	}
	out := dashboardResult{View: which, Whoami: who, PageURL: dashboard.PageURL(s.a.Config())}
	agents, err := view.Agents(ctx, s.a)
	if err != nil {
		return failResult(err), nil, nil
	}
	out.Agents = agents
	// Every run that the caller can see, of every member. The fields runs
	// and glance keep their old meaning for the page in the field.
	all, err := dashboard.Runs(ctx, s.a, "", "")
	if err != nil {
		return failResult(err), nil, nil
	}
	runs := listOf(all, "", "", "", 100)
	out.Runs, out.Glance = runs.Runs, runs.Glance
	if out.Runs == nil {
		out.Runs = []*fw.Run{}
	}
	var b strings.Builder
	switch which {
	case viewAgent:
		v, err := s.agentNamed(ctx, in.Agent, "")
		if err != nil {
			return failResult(err), nil, nil
		}
		out.Agent = &v.AgentView
		b.WriteString("At a glance: " + out.Glance.Sentence + "\n\n")
		b.WriteString(agentText(v, skills.Allowed(s.a, v.Skills)))
	case viewRun:
		ev, err := s.events(ctx, in.RunID, page{limit: defaultListLimit})
		if err != nil {
			return failResult(err), nil, nil
		}
		out.Run, out.Events, out.Items = ev.Run, ev.Events, ev.Items
		b.WriteString("At a glance: " + out.Glance.Sentence + "\n\n")
		b.WriteString(eventsText(ev))
	case viewUsage:
		rep, err := dashboard.Usages(ctx, s.a)
		if err != nil {
			return failResult(err), nil, nil
		}
		b.WriteString(usageDeprecated + "\n\n" + rep.String())
	default:
		home, err := dashboard.HomeOf(ctx, s.a, all, time.Now())
		if err != nil {
			return failResult(err), nil, nil
		}
		out.Home = &home
		if fleet.Of(s.a) != nil {
			fl, err := s.members(ctx)
			if err != nil {
				return failResult(err), nil, nil
			}
			out.Name, out.Members = fl.Name, fl.Members
		}
		b.WriteString(homeText(out))
	}
	b.WriteString("\n\nA host that supports MCP Apps shows this as the fw dashboard, " + dashboard.URI + ".")
	return result(withPageURL(s.a, b.String()), out), nil, nil
}

// homeText is the home view as text: the home of the caller, the agents
// that the caller can see, and the members of the fleet.
func homeText(out dashboardResult) string {
	var b strings.Builder
	if out.Home != nil {
		if t := strings.TrimSpace(out.Home.String()); t != "" {
			b.WriteString(t + "\n\n")
		}
	}
	if len(out.Agents) == 0 {
		b.WriteString("You can see no agent profile.")
	} else {
		b.WriteString("Your agents:\n")
		for _, v := range out.Agents {
			b.WriteString(agentBrief(v))
		}
	}
	if out.Name != "" {
		b.WriteString("\n" + dashboard.MembersText(out.Name, out.Members))
	}
	return strings.TrimRight(b.String(), "\n")
}

// agentBrief is one profile on one line, with the role and the description.
func agentBrief(v view.AgentView) string {
	line := fmt.Sprintf("- %s (you are %s)", v.Name, roleWord(v.Role))
	if v.Description != "" {
		line += ": " + v.Description
	}
	return line + "\n"
}

// subjectOf is the subject of the principal in ctx, or "".
func subjectOf(ctx context.Context) string {
	if p := fw.PrincipalFrom(ctx); p != nil {
		return p.Subject
	}
	return ""
}
