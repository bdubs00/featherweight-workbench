// Package access is the control-plane authorization of the workbench
// (docs/ARCHITECTURE.md section 12). With workbench.access.external, a
// control plane outside the process, behind an MCP tool or a bus subject,
// decides who can do what: the front sends the facts of each check in the
// protocol of the kernel (the kernel architecture, section 13.5) and
// enforces the answer. It makes no decision of its own, so the control plane
// is the single source of truth. A worker kernel has no such plug: it
// decides with the rules of access alone.
package access

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// Block is the top-level key of fw.yaml that the workbench claims.
const Block = "workbench"

// Variables that win over the file.
const (
	EnvMCP     = "FW_WORKBENCH_ACCESS_EXTERNAL_MCP"
	EnvTool    = "FW_WORKBENCH_ACCESS_EXTERNAL_TOOL"
	EnvBus     = "FW_WORKBENCH_ACCESS_EXTERNAL_BUS"
	EnvTimeout = "FW_WORKBENCH_ACCESS_EXTERNAL_TIMEOUT"
)

// Config is the block access under workbench.
type Config struct {
	// External names the control plane: an entry of tools.mcp and a tool on
	// it, or a bus subject, with a timeout. Nil leaves the rules of access
	// in charge.
	External *fw.ExternalPolicyConfig `yaml:"external"`
}

type block struct {
	Access Config `yaml:"access"`
}

// Load reads workbench.access.external from the configuration and the
// variables, with its defaults set: the tool authorize and the timeout of
// the kernel. It returns nil when no control plane is named. A block that
// names both transports or neither, a negative timeout, a bus subject with
// a wildcard, or an MCP server that tools.mcp does not hold is an error.
func Load(cfg *fw.Config) (*fw.ExternalPolicyConfig, error) {
	var b block
	if cfg != nil {
		if _, err := cfg.Extension(Block, &b); err != nil {
			return nil, err
		}
	}
	e := b.Access.External
	set := func(env string, dst *string) {
		if v, ok := lookup(env); ok {
			if e == nil {
				e = &fw.ExternalPolicyConfig{}
			}
			*dst = v
		}
	}
	var mcp, tool, bus string
	set(EnvMCP, &mcp)
	set(EnvTool, &tool)
	set(EnvBus, &bus)
	if e == nil {
		return nil, nil
	}
	c := *e
	if mcp != "" {
		c.MCP = mcp
	}
	if tool != "" {
		c.Tool = tool
	}
	if bus != "" {
		c.Bus = bus
	}
	if v, ok := lookup(EnvTimeout); ok {
		d, err := time.ParseDuration(v)
		if err != nil {
			return nil, fmt.Errorf("access: %s=%q is not a duration such as 5s", EnvTimeout, v)
		}
		c.Timeout = d
	}
	c.MCP, c.Tool, c.Bus = strings.TrimSpace(c.MCP), strings.TrimSpace(c.Tool), strings.TrimSpace(c.Bus)
	if c.Tool == "" {
		c.Tool = fw.DefaultAuthzTool
	}
	if c.Timeout == 0 {
		c.Timeout = fw.DefaultDecisionTimeout
	}
	if err := check(cfg, c); err != nil {
		return nil, err
	}
	return &c, nil
}

// check lists the problems of the block, as the kernel checks
// policy.external.
func check(cfg *fw.Config, c fw.ExternalPolicyConfig) error {
	const key = "workbench.access.external"
	var p []string
	if c.Transport() == "" {
		if c.MCP != "" && c.Bus != "" {
			p = append(p, fmt.Sprintf("%s sets both mcp %q and bus %q; set exactly one", key, c.MCP, c.Bus))
		} else {
			p = append(p, fmt.Sprintf("%s sets neither mcp nor bus; set exactly one", key))
		}
	}
	if c.Timeout < 0 {
		p = append(p, fmt.Sprintf("%s.timeout %s is negative", key, c.Timeout))
	}
	if c.Bus != "" && strings.ContainsAny(c.Bus, " \t*>") {
		p = append(p, fmt.Sprintf("%s.bus %q is not a subject: it holds a space or a wildcard", key, c.Bus))
	}
	if c.MCP != "" && cfg != nil && !hasServer(cfg, c.MCP) {
		p = append(p, fmt.Sprintf("%s.mcp names %q, but tools.mcp has no entry with that name", key, c.MCP))
	}
	if len(p) > 0 {
		return fmt.Errorf("access: %s", strings.Join(p, "; "))
	}
	return nil
}

// hasServer reports whether tools.mcp has an entry with that name, with the
// loose match of the kernel.
func hasServer(cfg *fw.Config, name string) bool {
	for _, m := range cfg.Tools.MCP {
		if fw.SameServerName(m.Name, name) {
			return true
		}
	}
	return false
}

// Of returns the control plane of a configuration, or nil, for the command
// line (cli.ExternalAccess). A block that does not load counts as none; the
// start of the process reports the error.
func Of(cfg *fw.Config) *fw.ExternalPolicyConfig {
	e, err := Load(cfg)
	if err != nil {
		return nil
	}
	return e
}

// ControlServers names the MCP server of the block for the kernel
// (fw.RegisterControlServers), so the model never sees its tools.
func ControlServers(cfg *fw.Config) []string {
	var b block
	if cfg == nil {
		return nil
	}
	if _, err := cfg.Extension(Block, &b); err != nil {
		return nil
	}
	name := ""
	if b.Access.External != nil {
		name = b.Access.External.MCP
	}
	if v, ok := lookup(EnvMCP); ok {
		name = v
	}
	if name = strings.TrimSpace(name); name == "" {
		return nil
	}
	return []string{name}
}

func lookup(name string) (string, bool) {
	v, ok := os.LookupEnv(name)
	if !ok || strings.TrimSpace(v) == "" {
		return "", false
	}
	return strings.TrimSpace(v), true
}
