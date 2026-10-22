package workers

// This file holds the MCP tools of the pools and the templates
// (docs/ARCHITECTURE.md section 14.10), which the package adds to the MCP
// server of the workbench through mcp.RegisterTools. For a call of a run of
// the fleet that names its person, the tools act as that person
// (manager.OnBehalf, section 19).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/bdubs00/featherweight-kernel/ax"
	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/dashboard"
	"github.com/bdubs00/featherweight-workbench/fleet"
	"github.com/bdubs00/featherweight-workbench/manager"
	"github.com/bdubs00/featherweight-workbench/pools"
	"github.com/bdubs00/featherweight-workbench/surface/mcp"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Names of the tools.
const (
	ToolPoolsList     = "pools_list"
	ToolPoolPut       = "pool_put"
	ToolPoolDelete    = "pool_delete"
	ToolTemplatesList = "templates_list"
)

const (
	emptySchema = `{"type":"object","properties":{}}`

	poolNameSchema = `{"type":"string","minLength":1,"maxLength":63,"description":"The name of the pool: lowercase letters, digits, and hyphens. Its Tasks are <name>-<index>."}`

	resourcesSchema = `{"type":"object","description":"The resource values that the pool sets over its template, each a quantity such as 500m, 2, or 1Gi.","properties":{
		"requests":{"type":"object","properties":{"cpu":{"type":"string"},"memory":{"type":"string"}}},
		"limits":{"type":"object","properties":{"cpu":{"type":"string"},"memory":{"type":"string"}}}}}`

	putSchema = `{"type":"object","properties":{
		"name":` + poolNameSchema + `,
		"agent":{"type":"string","minLength":1,"maxLength":64,"description":"With agent, the call creates a runtime pool whose workers take the queue of this profile. Leave it out to change the pool of the name."},
		"template":{"type":"string","minLength":1,"description":"The worker template of the pool; templates_list names them. A create without it takes the default template."},
		"replicas":{"type":"integer","minimum":0,"maximum":64,"description":"The number of workers. A create needs it; 0 makes none."},
		"env":{"type":"array","maxItems":32,"description":"The variables that the pool sets over its template, each a name and a value. A change replaces the whole list, and an empty list clears it.","items":{"type":"object","properties":{"name":{"type":"string","minLength":1},"value":{"type":"string","maxLength":4096}},"required":["name","value"]}},
		"resources":` + resourcesSchema + `
	},"required":["name"]}`

	deleteSchema = `{"type":"object","properties":{"name":` + poolNameSchema + `},"required":["name"]}`
)

type (
	putArgs struct {
		Name      string             `json:"name"`
		Agent     string             `json:"agent,omitempty"`
		Template  string             `json:"template,omitempty"`
		Replicas  *int               `json:"replicas,omitempty"`
		Env       []pools.RuntimeEnv `json:"env,omitempty"`
		Resources *ax.ResourcesSpec  `json:"resources,omitempty"`
	}
	deleteArgs struct {
		Name string `json:"name"`
	}
	// poolsResult, poolResult, and templatesResult are the structured
	// results of the tools.
	poolsResult struct {
		View  string         `json:"view"`
		Pools []pools.Status `json:"pools"`
	}
	poolResult struct {
		View    string       `json:"view"`
		Pool    pools.Status `json:"pool"`
		Created bool         `json:"created,omitempty"`
	}
	templatesResult struct {
		View      string               `json:"view"`
		Templates []pools.TemplateInfo `json:"templates"`
	}
)

// tools builds the tools of the pools for one MCP server. A process whose
// fleet keeps no pool and no template gets none.
func tools(s *mcp.Server) []mcp.Tool {
	a := s.Agent()
	if !fleet.Pooled(a) {
		return nil
	}
	readOnly := &sdk.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true, OpenWorldHint: ptr(false)}
	return []mcp.Tool{
		mcp.NewTool(&sdk.Tool{
			Name:        ToolPoolsList,
			Title:       "List pools",
			Description: "List the pools of workers that you can see: the pools of the configuration file and the runtime pools that people created from a template. Each pool names its source, its template, its agent, the workers that it wants, the workers that exist and are ready, and the reason for each gap.",
			InputSchema: json.RawMessage(emptySchema),
			Annotations: readOnly,
		}, func(ctx context.Context, _ *sdk.CallToolRequest, _ struct{}) (*sdk.CallToolResult, any, error) {
			return list(ctx, a), nil, nil
		}),
		mcp.NewTool(&sdk.Tool{
			Name:        ToolTemplatesList,
			Title:       "List worker templates",
			Description: "List the worker templates of the front: what a worker is, without its agent. Each has a name, its atespace, its model when it names one, and a mark for the default, which a new agent gets when its request names no template.",
			InputSchema: json.RawMessage(emptySchema),
			Annotations: readOnly,
		}, func(context.Context, *sdk.CallToolRequest, struct{}) (*sdk.CallToolResult, any, error) {
			ts := fleet.TemplatesOf(a)
			return mcp.Result(templatesText(ts), templatesResult{View: "templates", Templates: ts}), nil, nil
		}),
		mcp.NewTool(&sdk.Tool{
			Name:        ToolPoolPut,
			Title:       "Create or change a pool",
			Description: "Create a runtime pool of workers for an agent, or change one. With agent, the call creates the pool from template, else from the default template, with replicas workers; it needs the creator role. The profile of the agent comes from the overlay of the pool, so the pool makes no worker until agent_put with pool writes the profile; agent_put with workers creates a new agent and its pool in one call instead. Without agent, the call changes the replicas, the variables, the resources, or the template of the pool of the name; it needs the owner role on the agent of the pool. The front replaces each worker of the pool once it is idle.",
			InputSchema: json.RawMessage(putSchema),
			Annotations: &sdk.ToolAnnotations{DestructiveHint: ptr(true), IdempotentHint: false, OpenWorldHint: ptr(false)},
		}, func(ctx context.Context, _ *sdk.CallToolRequest, in putArgs) (*sdk.CallToolResult, any, error) {
			return put(ctx, a, in), nil, nil
		}),
		mcp.NewTool(&sdk.Tool{
			Name:        ToolPoolDelete,
			Title:       "Delete a pool",
			Description: "Delete a runtime pool. It needs the owner role on the agent of the pool. The front deletes each worker of the pool once it is idle, and then the pool. The profile of the agent stays in the overlay of the pool, and its runs stay in the ledgers. A pool of the configuration file changes in fw.yaml only.",
			InputSchema: json.RawMessage(deleteSchema),
			Annotations: &sdk.ToolAnnotations{DestructiveHint: ptr(true), IdempotentHint: true, OpenWorldHint: ptr(false)},
		}, func(ctx context.Context, _ *sdk.CallToolRequest, in deleteArgs) (*sdk.CallToolResult, any, error) {
			return remove(ctx, a, in), nil, nil
		}),
	}
}

func ptr[T any](v T) *T { return &v }

// explain words an error of the fleet for a tool result.
func explain(err error) string {
	switch {
	case errors.Is(err, fleet.ErrPoolNotFound):
		return "Not found: " + cause(err) + ". pools_list names the pools that you can see."
	case errors.Is(err, fleet.ErrPoolExists), errors.Is(err, fleet.ErrPoolConflict):
		return "Conflict: " + cause(err) + "."
	}
	return mcp.Explain(err)
}

// list is the tool pools_list.
func list(ctx context.Context, a *fw.Agent) *sdk.CallToolResult {
	ctx, _, _ = manager.OnBehalf(ctx)
	ps, _, err := fleet.ListPools(ctx, a)
	if err != nil {
		return mcp.ErrorResult(explain(err))
	}
	text := "You can see no pool."
	if len(ps) > 0 {
		text = dashboard.PoolsText(ps)
	}
	return mcp.Result(text, poolsResult{View: "pools", Pools: ps})
}

// put is the tool pool_put: a create with agent, else a change.
func put(ctx context.Context, a *fw.Agent, in putArgs) *sdk.CallToolResult {
	ctx, _, _ = manager.OnBehalf(ctx)
	if strings.TrimSpace(in.Agent) != "" {
		st, err := fleet.CreatePool(ctx, a, fleet.PoolRequest{Name: in.Name, Template: in.Template, Agent: in.Agent, Replicas: in.Replicas, Env: in.Env, Resources: in.Resources})
		if errors.Is(err, fleet.ErrPoolExists) {
			return mcp.ErrorResult(explain(err) + " To change the pool, call pool_put without agent.")
		}
		if err != nil {
			return mcp.ErrorResult(explain(err))
		}
		text := fmt.Sprintf("Created the runtime pool %s of the template %s for the agent %s, with %d %s.\n%s", st.Name, st.Template, st.Agent, st.Wanted, plural(st.Wanted, "worker", "workers"), poolText(st))
		return mcp.Result(text, poolResult{View: "pool", Pool: st, Created: true})
	}
	c := fleet.PoolPatch{Replicas: in.Replicas, Resources: in.Resources}
	if in.Env != nil {
		env := in.Env
		c.Env = &env
	}
	if in.Template != "" {
		c.Template = &in.Template
	}
	st, err := fleet.PatchPool(ctx, a, in.Name, c)
	if errors.Is(err, fleet.ErrPoolNotFound) {
		return mcp.ErrorResult(explain(err) + " To create a pool, pass agent and replicas.")
	}
	if err != nil {
		return mcp.ErrorResult(explain(err))
	}
	text := fmt.Sprintf("Changed the runtime pool %s: it wants %d %s at generation %d, and the front replaces each worker once it is idle.\n%s", st.Name, st.Wanted, plural(st.Wanted, "worker", "workers"), st.Generation, poolText(st))
	return mcp.Result(text, poolResult{View: "pool", Pool: st})
}

// remove is the tool pool_delete.
func remove(ctx context.Context, a *fw.Agent, in deleteArgs) *sdk.CallToolResult {
	ctx, _, _ = manager.OnBehalf(ctx)
	st, err := fleet.DeletePool(ctx, a, in.Name)
	if err != nil {
		return mcp.ErrorResult(explain(err))
	}
	text := fmt.Sprintf("Deleted the runtime pool %s: the front deletes each worker of it once the worker is idle, and then the pool. The profile of the agent %s stays in the overlay of the pool.\n%s", st.Name, st.Agent, poolText(st))
	return mcp.Result(text, poolResult{View: "pool", Pool: st})
}
