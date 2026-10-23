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
// authentication of the agent. /bossmode without the slash moves to
// /bossmode/ (slashRoute). Without it, the routes are not there, and
// each answers 404. A configuration that does not load mounts nothing;
// the service stops the start with the error.
func extension(k *native.Kit) error {
	a := k.Agent()
	c, err := Load(a.Config())
	if err != nil || !c.Enabled {
		return nil
	}
	k.HandlePublic("GET "+Path, slashRoute)
	k.HandlePublic("GET "+Path+"/{file...}", pageRoute(c.Dir))
	k.Handle("GET /v1/bossmode", settingsRoute(k, c))
	k.Handle("GET "+LogoPath, logoRoute(a))
	k.Handle("GET /v1/bossmode/stream", streamRoute(a, c.Dir))
	k.Handle("GET /v1/bossmode/agents/{agent}/today", todayRoute(k))
	mountSkills(k)
	mountImports(k)
	return nil
}

// Settings is the answer of GET /v1/bossmode: the manager, when the
// configuration names one, the keys of workbench.bridges that the page
// reads, as the package bridges reads them (bridges.Load), the theme of
// the deployment, the worker templates from which the page offers a
// choice when it creates an agent (fleet.TemplatesOf), an empty list for
// none, and the quick chat, when the configuration names one.
type Settings struct {
	Manager    *Manager   `json:"manager"`
	Placement  string     `json:"placement"`
	MaxMembers int        `json:"maxMembers"`
	Theme      PageTheme  `json:"theme"`
	Templates  []Template `json:"templates"`
	Chat       *QuickChat `json:"chat"`
}

// ChatView is the value of the metadata key fw.view (dashboard.ViewKey)
// that the page sets on a run of the quick chat (docs/ARCHITECTURE.md
// section 17.7).
const ChatView = "quickchat"

// QuickChat names the quick chat for the page. Agent is the profile of
// workbench.bossmode.chat.agent. Member is the fleet member that runs it:
// workbench.bossmode.chat.member when it names one, else the member that
// the settings pick for the manager (Manager) for this profile, "" when
// the caller sees none.
type QuickChat struct {
	Agent  string `json:"agent"`
	Member string `json:"member"`
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
		out := Settings{Placement: b.Placement, MaxMembers: b.MaxMembers, Theme: pageTheme(a, c.Theme), Templates: []Template{}}
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
		if c.Chat.Agent != "" {
			qc, err := chatOf(r.Context(), a, c.Chat)
			if err != nil {
				k.Fail(w, r, err)
				return
			}
			out.Chat = qc
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

// chatOf names the quick chat and its member for the caller in ctx. A
// member of the configuration goes out as it is; without one, the member
// comes from the fleet view as for the manager.
func chatOf(ctx context.Context, a *fw.Agent, c Chat) (*QuickChat, error) {
	qc := &QuickChat{Agent: c.Agent, Member: c.Member}
	if qc.Member != "" {
		return qc, nil
	}
	if v := fleet.Of(a); v != nil {
		member, err := managerMember(ctx, v, c.Agent)
		if err != nil {
			return nil, err
		}
		qc.Member = member
	}
	return qc, nil
}

// managerMember returns the member that runs the profile agent, as Manager
// says: the settings pick the member of the manager and of the quick chat
// this way.
func managerMember(ctx context.Context, v fleet.View, agent string) (string, error) {
	pooled := map[string]bool{}
	if pv, ok := v.(fleet.PoolView); ok {
		list, err := pv.Pools(ctx)
		if err != nil {
			return "", err
		}
		for _, p := range list {
			for _, t := range p.Tasks {
				pooled[p.Atespace+"/"+t.Name] = true
			}
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
	// The Tasks of a pool come first. A Task outside every pool, such as a
	// child Task that a manager spawned, lists the profiles of its parent,
	// and after a roll it can sort before the new Task of the pool.
	sort.Slice(members, func(i, j int) bool {
		if pooled[members[i].Name] != pooled[members[j].Name] {
			return pooled[members[i].Name]
		}
		return members[i].Name < members[j].Name
	})
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
