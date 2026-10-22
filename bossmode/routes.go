package bossmode

// This file mounts the routes of bossmode on the native surface and serves
// GET /v1/bossmode, the settings of the page (docs/ARCHITECTURE.md
// section 17.4).

import (
	"context"
	"net/http"
	"sort"
	"strings"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/surface/native"
	"github.com/bdubs00/featherweight-workbench/bridges"
	"github.com/bdubs00/featherweight-workbench/dashboard"
	"github.com/bdubs00/featherweight-workbench/fleet"
)

// extension mounts the routes with workbench.bossmode.enabled: the page
// as a public route, because the page signs in on its own and calls /v1
// with its token, and the settings and the stream behind the
// authentication of the agent. Without it, the routes are not there, and
// each answers 404. A configuration that does not load mounts nothing;
// the service stops the start with the error.
func extension(k *native.Kit) error {
	a := k.Agent()
	c, err := Load(a.Config())
	if err != nil || !c.Enabled {
		return nil
	}
	page := pageRoute(c.Dir)
	k.HandlePublic("GET "+Path, page)
	k.HandlePublic("GET "+Path+"/{file...}", page)
	k.Handle("GET /v1/bossmode", settingsRoute(k, c))
	k.Handle("GET /v1/bossmode/stream", streamRoute(a))
	return nil
}

// Settings is the answer of GET /v1/bossmode: the manager, when the
// configuration names one, the keys of workbench.bridges that the page
// reads, as the package bridges reads them (bridges.Load), and the worker
// templates from which the page offers a choice when it creates an agent
// (fleet.TemplatesOf), an empty list for none.
type Settings struct {
	Manager    *Manager   `json:"manager"`
	Placement  string     `json:"placement"`
	MaxMembers int        `json:"maxMembers"`
	Templates  []Template `json:"templates"`
}

// Template names a worker template for the page: its name, and whether a
// new agent gets it when the page names none.
type Template struct {
	Name    string `json:"name"`
	Default bool   `json:"default"`
}

// Manager names the manager for the page. Agent is the profile of
// workbench.bossmode.manager. Member is the fleet member that runs it: a
// Task of the pool whose agent is the profile, the first live one, else
// the first; without such a pool, the first member, by name, whose agents
// hold the profile, and this process only when no other member does; ""
// when the caller sees none. Run is the newest top-level run of the
// profile that the caller started outside a bridge, or "".
type Manager struct {
	Agent  string `json:"agent"`
	Member string `json:"member"`
	Run    string `json:"run"`
}

func settingsRoute(k *native.Kit, c Config) http.HandlerFunc {
	a := k.Agent()
	return func(w http.ResponseWriter, r *http.Request) {
		// The package bridges reads workbench.bridges. A block that does not
		// load stops the start of the bridges, so a process that serves
		// loads it.
		b, err := bridges.Load(a.Config())
		if err != nil {
			k.Fail(w, r, err)
			return
		}
		out := Settings{Placement: b.Placement, MaxMembers: b.MaxMembers, Templates: []Template{}}
		for _, t := range fleet.TemplatesOf(a) {
			out.Templates = append(out.Templates, Template{Name: t.Name, Default: t.Default})
		}
		if c.Manager != "" {
			m, err := managerOf(r.Context(), a, c.Manager)
			if err != nil {
				k.Fail(w, r, err)
				return
			}
			out.Manager = m
		}
		native.WriteJSON(w, http.StatusOK, out)
	}
}

// managerOf finds the member and the newest run of the manager for the
// caller in ctx, from the fleet view.
func managerOf(ctx context.Context, a *fw.Agent, agent string) (*Manager, error) {
	m := &Manager{Agent: agent}
	if v := fleet.Of(a); v != nil {
		member, err := managerMember(ctx, v, agent)
		if err != nil {
			return nil, err
		}
		m.Member = member
	}
	runs, err := dashboard.Runs(ctx, a, "", "")
	if err != nil {
		return nil, err
	}
	p := fw.PrincipalFrom(ctx)
	for _, r := range runs { // newest first
		// The manager run of a spawn bridge, and the run that writes the
		// report of a bridge, act as the operator too, but they belong to
		// the bridge and are no conversation of the page.
		if strings.HasPrefix(r.Metadata[fw.MetaBatch], bridges.BatchPrefix) {
			continue
		}
		if r.Agent == agent && r.Parent == "" && (p == nil || r.Principal == p.Subject) {
			m.Run = r.ID
			break
		}
	}
	return m, nil
}

// managerMember returns the member that runs the manager, as Manager says.
func managerMember(ctx context.Context, v fleet.View, agent string) (string, error) {
	if pv, ok := v.(fleet.PoolView); ok {
		list, err := pv.Pools(ctx)
		if err != nil {
			return "", err
		}
		for _, p := range list {
			if p.Agent != agent || len(p.Tasks) == 0 {
				continue
			}
			pick := p.Tasks[0]
			for _, t := range p.Tasks {
				if t.Phase == "Running" {
					pick = t
					break
				}
			}
			return p.Atespace + "/" + pick.Name, nil
		}
	}
	members, err := v.Members(ctx)
	if err != nil {
		return "", err
	}
	sort.Slice(members, func(i, j int) bool { return members[i].Name < members[j].Name })
	self := ""
	for _, m := range members {
		for _, ag := range m.Agents {
			if ag.Name != agent {
				continue
			}
			// The front runs the configuration of its workers, so it holds
			// every profile; it is the member of the manager only when no
			// other member is.
			if m.Self {
				self = m.Name
				continue
			}
			return m.Name, nil
		}
	}
	return self, nil
}
