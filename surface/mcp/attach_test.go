package mcp

import (
	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/fleet"
)

// attach gives an agent a fake fleet view and returns the agent.
func attach(a *fw.Agent, v fleet.View) *fw.Agent {
	fleet.Attach(a, v)
	return a
}
