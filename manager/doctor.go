package manager

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/fleet"
	"github.com/bdubs00/featherweight-workbench/pools"
	"gopkg.in/yaml.v3"
)

// worker is the part of the fw.yaml of a pool that the doctor line reads.
type worker struct {
	Auth struct {
		TokenEnv string `yaml:"tokenEnv"`
	} `yaml:"auth"`
	Spawn struct {
		Driver string `yaml:"driver"`
	} `yaml:"spawn"`
	Tools struct {
		MCP []struct {
			Name    string `yaml:"name"`
			Trusted bool   `yaml:"trusted"`
			Auth    struct {
				Bearer string `yaml:"bearer"`
			} `yaml:"auth"`
		} `yaml:"mcp"`
	} `yaml:"tools"`
}

// DoctorCheck is the line manager of fw-workbench doctor (cli.DoctorCheck):
// the profile that workbench.bossmode.manager names, the pool that runs
// it, whether the tools.mcp entry through which the profile reaches the
// front is trusted, and where the delegation of a bridge comes from. The
// entry for the front is the one that sends the static token of the fleet
// as its bearer, because the front acts for the person of a run only for
// that token. The check fails when a manager is named and no pool of the configuration
// runs it, when the profile reaches the front through no such entry, and
// when no such entry is trusted, because then _meta names no person and
// the front acts as the token. The line reads the configuration alone: a
// profile that the overlay of a pool adds does not count.
func DoctorCheck(_ context.Context, cfg *fw.Config, _ fw.Secrets, _ time.Duration) (bool, string) {
	name, err := Load(cfg)
	if err != nil {
		return false, "manager: " + strings.TrimPrefix(err.Error(), "manager: ")
	}
	if name == "" {
		return true, "manager: off; set workbench.bossmode.manager (" + EnvManager + ") to the profile of the manager"
	}
	fc, err := fleet.Load(cfg)
	if err != nil {
		return false, "manager: " + err.Error()
	}
	list, err := fleet.LoadPools(cfg, fc)
	if err != nil {
		return false, "manager: the pools do not load, so no pool runs the profile " + name + "; see the line fleet"
	}
	var pool *pools.Pool
	var prof fw.AgentProfile
	for _, p := range list {
		if found, ok := p.Profiles()[name]; ok {
			pool, prof = p, found
			if p.Agent == name {
				break
			}
		}
	}
	if pool == nil {
		return false, fmt.Sprintf("manager: no pool of workbench.pools runs the profile %s; add a pool whose agent is %s, as examples/bossmode shows", name, name)
	}
	w, err := workerOf(pool)
	if err != nil {
		return false, fmt.Sprintf("manager: read fw of the pool %s: %v", pool.Name, err)
	}
	tokenEnv := strings.TrimSpace(w.Auth.TokenEnv)
	if tokenEnv == "" {
		tokenEnv = pools.DefaultTokenEnv
	}
	var front, trusted []string
	for _, m := range w.Tools.MCP {
		if strings.TrimSpace(m.Auth.Bearer) != tokenEnv || !reaches(prof.Tools, m.Name) {
			continue
		}
		front = append(front, m.Name)
		if m.Trusted {
			trusted = append(trusted, m.Name)
		}
	}
	tasks := "Tasks"
	if pool.Wanted() == 1 {
		tasks = "Task"
	}
	where := fmt.Sprintf("the profile %s runs in the pool %s of %d %s", name, pool.Name, pool.Wanted(), tasks)
	switch {
	case len(front) == 0:
		return false, fmt.Sprintf("manager: %s, but it reaches no tools.mcp entry that sends the static token (auth.bearer %s), so it cannot call the MCP tools of this server", where, tokenEnv)
	case len(trusted) == 0:
		return false, fmt.Sprintf("manager: %s, but the tools.mcp entry %s that reaches the server is not trusted, so _meta names no person and the server acts as the token; set trusted: true", where, strings.Join(front, ", "))
	}
	parts := []string{where, fmt.Sprintf("the trusted tools.mcp entry %s reaches the server, so the server acts for the person of each manager run", strings.Join(trusted, ", "))}
	if strings.TrimSpace(w.Spawn.Driver) == "ax" {
		parts = append(parts, "the pool seats the members of a spawn bridge in child Tasks")
	} else {
		parts = append(parts, "the pool has no spawn.driver ax, so it seats no member of a spawn bridge")
	}
	// The service of the bridges sets them in a process that serves. The
	// doctor runs in a process of its own, which starts no service, so
	// there the line names where the delegation comes from.
	if currentBridges() != nil {
		parts = append(parts, "the bridges say what the operator of each bridge delegates")
	} else {
		parts = append(parts, "the service of the bridges tells the server what the operator of each bridge delegates; the line bossmode says where the bridges live, and without them the manager can approve, change, and cancel no run of a bridge")
	}
	return true, "manager: " + strings.Join(parts, "; ")
}

// workerOf reads the part of fw of a pool that the doctor line needs.
func workerOf(p *pools.Pool) (worker, error) {
	var w worker
	b, err := yaml.Marshal(p.FW)
	if err != nil {
		return w, fmt.Errorf("manager: encode fw: %w", err)
	}
	if err := yaml.Unmarshal(b, &w); err != nil {
		return w, fmt.Errorf("manager: decode fw: %w", err)
	}
	return w, nil
}

// reaches reports whether the tool list of a profile names a tool of the
// MCP server, such as front.* or front.runs_list. An empty list allows
// every tool, and so does the glob *.
func reaches(tools []string, server string) bool {
	if len(tools) == 0 {
		return true
	}
	return slices.ContainsFunc(tools, func(t string) bool {
		t = strings.TrimSpace(t)
		if t == "*" {
			return true
		}
		prefix, _, ok := strings.Cut(t, ".")
		return ok && fw.SameServerName(prefix, server)
	})
}
