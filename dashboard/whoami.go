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
	// Links are the addresses that the deployment names in the block
	// workbench.links of fw.yaml, such as metrics, the Grafana dashboard of
	// the fleet. The page offers them in the account menu.
	Links map[string]string `json:"links,omitempty"`
}

// links is the part of the workbench block that names the addresses.
type links struct {
	Links map[string]string `yaml:"links"`
}

// Links returns the addresses of workbench.links, or nil.
func Links(cfg *fw.Config) map[string]string {
	var b links
	if cfg == nil {
		return nil
	}
	if _, err := cfg.Extension("workbench", &b); err != nil || len(b.Links) == 0 {
		return nil
	}
	return b.Links
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
	out.Links = Links(a.Config())
	return out, nil
}
