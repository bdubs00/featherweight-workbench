package dashboard

import (
	"context"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/surface/view"
	"github.com/bdubs00/featherweight-workbench/fleet"
	"github.com/bdubs00/featherweight-workbench/teams"
)

// Whoami is the caller as the workbench sees it: the view of the kernel,
// with the principal and its role on every agent, the teams of the caller
// (package teams), and the member name of this process when the fleet is
// on. The native route GET /v1/whoami and the MCP tool whoami return it.
type Whoami struct {
	view.Whoami
	Teams []string `json:"teams,omitempty"`
	// Fleet is the member name of this process when the fleet is on.
	Fleet string `json:"fleet,omitempty"`
}

// WhoamiOf describes the caller in ctx.
func WhoamiOf(ctx context.Context, a *fw.Agent) (Whoami, error) {
	w, err := view.WhoamiOf(ctx, a)
	if err != nil {
		return Whoami{}, err
	}
	out := Whoami{Whoami: w, Teams: teams.Of(teams.Patterns(a.Config()), fw.PrincipalFrom(ctx))}
	if f := fleet.Of(a); f != nil {
		out.Fleet = f.Name()
	}
	return out, nil
}
