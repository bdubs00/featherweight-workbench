package fleet

import (
	"context"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/tools/builder"
)

func init() {
	builder.RegisterToolsCheck(proposalTools)
}

// proposalTools says which tools of a proposal of propose_agent the workers
// of the agent do not offer: the pool that runs the agent, or the runtime
// pool of the default template for a new agent. The Check of the place
// refuses the same tools when the person taps Create agent, so the builder
// learns it first. It answers "" for an agent of this process and for a
// place that does not tell.
func proposalTools(ctx context.Context, a *fw.Agent, name string, tools []string) string {
	at, err := PlaceFor(ctx, a, name, "", nil)
	if err != nil {
		return ""
	}
	pl, ok := at.(*poolPlace)
	if !ok || pl.pool == nil {
		return ""
	}
	return pl.pool.ToolsProblem(name, tools)
}
