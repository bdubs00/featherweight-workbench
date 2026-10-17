package dashboard

import (
	"context"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/surface/view"
	"github.com/bdubs00/featherweight-workbench/teams"
)

// Whoami is the caller as the workbench sees it: the view of the kernel,
// with the principal and its role on every agent, and the teams of the
// caller (package teams). The native route GET /v1/whoami and the MCP tool
// whoami return it.
type Whoami struct {
	view.Whoami
	Teams []string `json:"teams,omitempty"`
}

// WhoamiOf describes the caller in ctx.
func WhoamiOf(ctx context.Context, a *fw.Agent) (Whoami, error) {
	w, err := view.WhoamiOf(ctx, a)
	if err != nil {
		return Whoami{}, err
	}
	return Whoami{Whoami: w, Teams: teams.Of(teams.Patterns(a.Config()), fw.PrincipalFrom(ctx))}, nil
}
