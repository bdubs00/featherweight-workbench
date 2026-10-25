package workers

// This file holds the MCP tools of the pools, the templates, and the
// orphan Tasks (docs/ARCHITECTURE.md section 14.10), which the package
// adds to the MCP server of the workbench through mcp.RegisterTools. Each
// tool calls the Go function of its route. For a call of a run of
// the fleet that names its person, the tools act as that person
// (manager.OnBehalf, section 19).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

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
	ToolOrphansList   = "orphans_list"
	ToolOrphansRemove = "orphans_remove"
	ToolPackagesGet   = "packages_get"
	ToolPackagesPut   = "packages_put"
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

	packagesGetSchema = `{"type":"object","properties":{"pool":` + poolNameSchema + `},"required":["pool"]}`

	packageListSchema = `{"type":"array","maxItems":100,"items":{"type":"string","minLength":1,"maxLength":200}`

	packagesPutSchema = `{"type":"object","properties":{
		"pool":` + poolNameSchema + `,
		"on":{"type":"boolean","description":"True gives the workers of the pool the packages of the lists. False turns the packages off, and the workers use only the programs of their image."},
		"ask":{"type":"boolean","description":"True lets the agent ask for a package, and a person approves each one. False lets the agent only list its packages."},
		"python":` + packageListSchema + `,"description":"The Python packages, such as ansible-lint or pyyaml>=6. The list replaces the list of the pool, and an empty list clears it."},
		"tools":` + packageListSchema + `,"description":"The Programs, such as nodejs>=22 or iproute2. The list replaces the list of the pool, and an empty list clears it."},
		"dryRun":{"type":"boolean","description":"Check the request and answer the packages that it would set, and change nothing."}
	},"required":["pool","on"]}`
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
	packagesGetArgs struct {
		Pool string `json:"pool"`
	}
	packagesPutArgs struct {
		Pool   string   `json:"pool"`
		On     *bool    `json:"on"`
		Ask    bool     `json:"ask,omitempty"`
		Python []string `json:"python,omitempty"`
		Tools  []string `json:"tools,omitempty"`
		DryRun bool     `json:"dryRun,omitempty"`
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
	// orphansResult is the structured result of orphans_list, as GET
	// /v1/fleet/orphans answers.
	orphansResult struct {
		Orphans     []fleet.Orphan `json:"orphans"`
		RemoveAfter int64          `json:"removeAfter"`
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
	out := []mcp.Tool{
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
		mcp.NewTool(&sdk.Tool{
			Name:        ToolOrphansList,
			Title:       "List orphan Tasks",
			Description: "List the orphan Tasks of the fleet: the child Tasks whose parent Task is gone, each after the Tasks under it, with its parent, its phase, its pool, and its open runs, and how long an orphan sleeps before the front removes it. It needs the admin role of the process.",
			InputSchema: json.RawMessage(emptySchema),
			Annotations: readOnly,
		}, func(ctx context.Context, _ *sdk.CallToolRequest, _ struct{}) (*sdk.CallToolResult, any, error) {
			return orphans(ctx, a), nil, nil
		}),
		mcp.NewTool(&sdk.Tool{
			Name:        ToolOrphansRemove,
			Title:       "Remove orphan Tasks",
			Description: "Remove every orphan Task of the fleet, each Task before the Task above it, with the runs that it holds. The answer names the Tasks that went and each Task whose delete failed. It needs the admin role of the process. List them with orphans_list first.",
			InputSchema: json.RawMessage(emptySchema),
			Annotations: &sdk.ToolAnnotations{DestructiveHint: ptr(true), IdempotentHint: true, OpenWorldHint: ptr(true)},
		}, func(ctx context.Context, _ *sdk.CallToolRequest, _ struct{}) (*sdk.CallToolResult, any, error) {
			return removeOrphans(ctx, a), nil, nil
		}),
	}
	if !fleet.PackagesOn(a) {
		return out
	}
	return append(out,
		mcp.NewTool(&sdk.Tool{
			Name:        ToolPackagesGet,
			Title:       "Read the packages of a pool",
			Description: "Read the packages of the workers of a pool: whether they are on, whether the agent can ask for a package, the Python packages and the Programs with who added each one and when, whether a worker still waits for a change, and the state of each worker: pending, materializing, ready, failed, or unknown, with the installed versions or the failure.",
			InputSchema: json.RawMessage(packagesGetSchema),
			Annotations: readOnly,
		}, func(ctx context.Context, _ *sdk.CallToolRequest, in packagesGetArgs) (*sdk.CallToolResult, any, error) {
			return packagesGet(ctx, a, in), nil, nil
		}),
		mcp.NewTool(&sdk.Tool{
			Name:        ToolPackagesPut,
			Title:       "Set the packages of a pool",
			Description: "Turn the packages of the workers of a pool on or off, and replace its lists of Python packages and Programs. It needs the owner role on the agent of the pool. Each worker gets the change when it is idle, and work in progress does not stop. With dryRun, it checks the request and changes nothing. Read the setting with packages_get first, because the lists replace the lists of the pool.",
			InputSchema: json.RawMessage(packagesPutSchema),
			Annotations: &sdk.ToolAnnotations{DestructiveHint: ptr(true), IdempotentHint: true, OpenWorldHint: ptr(false)},
		}, func(ctx context.Context, _ *sdk.CallToolRequest, in packagesPutArgs) (*sdk.CallToolResult, any, error) {
			return packagesPut(ctx, a, in), nil, nil
		}),
	)
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

// packagesResult is the structured result of packages_get and
// packages_put, the answer of the routes.
type packagesResult struct {
	View     string         `json:"view"`
	Packages fleet.Packages `json:"packages"`
	DryRun   bool           `json:"dryRun,omitempty"`
}

// packagesGet is the tool packages_get.
func packagesGet(ctx context.Context, a *fw.Agent, in packagesGetArgs) *sdk.CallToolResult {
	ctx, _, _ = manager.OnBehalf(ctx)
	out, err := fleet.PackagesOf(ctx, a, in.Pool)
	if err != nil {
		return mcp.ErrorResult(explain(err))
	}
	return mcp.Result(packagesText(out), packagesResult{View: "packages", Packages: out})
}

// packagesPut is the tool packages_put.
func packagesPut(ctx context.Context, a *fw.Agent, in packagesPutArgs) *sdk.CallToolResult {
	ctx, _, _ = manager.OnBehalf(ctx)
	out, err := fleet.PutPackages(ctx, a, in.Pool, fleet.PackagesRequest{On: in.On, Ask: in.Ask, Python: in.Python, Tools: in.Tools}, in.DryRun)
	if err != nil {
		return mcp.ErrorResult(explain(err))
	}
	text := packagesText(out)
	if in.DryRun {
		text = "Dry run: nothing changed. With this request:\n" + text
	}
	return mcp.Result(text, packagesResult{View: "packages", Packages: out, DryRun: in.DryRun})
}

// packagesText says the packages of a pool in words, with the words that
// a person sees on the page: package, Python package, and Program.
func packagesText(p fleet.Packages) string {
	var b strings.Builder
	if !p.On {
		fmt.Fprintf(&b, "The packages of the pool %s are off. Its workers use only the programs of their image.", p.Pool)
		if !p.Available {
			b.WriteString(" The front cannot change them here.")
		}
		if p.Applying {
			b.WriteString("\nWhen a worker is idle, it gets the change. Work in progress does not stop.")
		}
		return b.String()
	}
	fmt.Fprintf(&b, "The packages of the pool %s are on.", p.Pool)
	list := func(what string, items []pools.PackageItem) {
		if len(items) == 0 {
			fmt.Fprintf(&b, "\n%s: none.", what)
			return
		}
		fmt.Fprintf(&b, "\n%s:", what)
		for _, it := range items {
			b.WriteString("\n- " + it.Name)
			if it.By != "" {
				b.WriteString(", added by " + it.By)
			}
			if it.At != nil {
				b.WriteString(" at " + it.At.UTC().Format(time.RFC3339))
			}
			if it.Run != "" {
				fmt.Fprintf(&b, ", for the agent %s in the run %s", it.Agent, it.Run)
			}
		}
	}
	list("Python packages", p.Python)
	list("Programs", p.Tools)
	if p.Ask {
		b.WriteString("\nThe agent can ask for a package. A person approves each one.")
	} else {
		b.WriteString("\nThe agent cannot ask for a package. It can only list its packages.")
	}
	ready, failed := 0, 0
	for _, w := range p.Workers {
		switch w.State {
		case fleet.EnvReady:
			ready++
		case fleet.EnvFailed:
			failed++
			why := "the front holds no reason"
			if w.Failure != nil && w.Failure.Error != "" {
				why = w.Failure.Error
			}
			fmt.Fprintf(&b, "\nThe install failed on %s: %s", w.Task, why)
		}
	}
	switch n := len(p.Workers); {
	case n == 0:
		b.WriteString("\nThe pool has no worker now.")
	case ready == n:
		fmt.Fprintf(&b, "\nReady on %d of %d %s.", ready, n, plural(n, "worker", "workers"))
	case failed == 0:
		fmt.Fprintf(&b, "\nInstalling on %d of %d %s.", n-ready, n, plural(n, "worker", "workers"))
	}
	if p.Applying {
		b.WriteString("\nWhen a worker is idle, it gets the change. Work in progress does not stop.")
	}
	return b.String()
}

// orphans is the tool orphans_list.
func orphans(ctx context.Context, a *fw.Agent) *sdk.CallToolResult {
	ctx, _, _ = manager.OnBehalf(ctx)
	list, err := fleet.Orphans(ctx, a)
	if err != nil {
		return mcp.ErrorResult(explain(err))
	}
	after := fleet.OrphansAfter(a)
	var b strings.Builder
	if len(list) == 0 {
		b.WriteString("The fleet has no orphan Task.")
	} else {
		fmt.Fprintf(&b, "%d orphan %s:", len(list), plural(len(list), "Task", "Tasks"))
	}
	for _, o := range list {
		fmt.Fprintf(&b, "\n- %s: the child of %s, %s, %d open %s", o.Member, o.Parent, o.Phase, o.Open, plural(o.Open, "run", "runs"))
		if o.Pool != "" {
			b.WriteString(", of the pool " + o.Pool)
		}
	}
	if after > 0 {
		fmt.Fprintf(&b, "\nThe front removes an orphan that sleeps for %s.", after)
	} else {
		b.WriteString("\nThe front removes no orphan by itself.")
	}
	return mcp.Result(b.String(), orphansResult{Orphans: list, RemoveAfter: int64(after.Seconds())})
}

// removeOrphans is the tool orphans_remove.
func removeOrphans(ctx context.Context, a *fw.Agent) *sdk.CallToolResult {
	ctx, _, _ = manager.OnBehalf(ctx)
	out, err := fleet.RemoveOrphans(ctx, a)
	if err != nil {
		return mcp.ErrorResult(explain(err))
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Removed %d orphan %s.", len(out.Removed), plural(len(out.Removed), "Task", "Tasks"))
	for _, m := range out.Removed {
		b.WriteString("\n- " + m)
	}
	for _, f := range out.Failed {
		fmt.Fprintf(&b, "\nThe delete of %s failed: %s", f.Member, f.Error)
	}
	return mcp.Result(b.String(), out)
}
