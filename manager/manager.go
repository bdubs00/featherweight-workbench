// Package manager holds what makes an agent of the fleet a manager in
// bossmode (docs/ARCHITECTURE.md section 19). It holds no agent: the
// manager is a profile that a pool runs on a Task of its own, with the MCP
// server of the front as its tool server, and examples/bossmode holds the
// pool and the profile.
//
// The package gives the MCP server of the front four things:
//
//   - OnBehalf, which each handler of the server calls at its start. When
//     the static token of the fleet calls for a run of a worker, and _meta
//     names the person of that run, the server acts as that person: the
//     view shows what the person sees, and a run starts as the person.
//   - Guard, the builder of the authorization plug of the process, which
//     lets a check for such a person pass only when it passes for the
//     token too. So the on-behalf-of narrows what the token can do and
//     never widens it.
//   - The attribution of what an actor does: the note of a decision, the
//     sender of a message, and the metadata key MetaVia of a run.
//   - Delegated, which asks the bridges whether the operator of a bridge
//     delegated an action to the manager (Bridges, SetBridges).
//
// DoctorCheck is the line manager of fw-workbench doctor.
package manager

import (
	"fmt"
	"os"
	"strings"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// Block is the top-level key of fw.yaml that the workbench claims.
const Block = "workbench"

// EnvManager names the profile of the manager. It wins over the file.
const EnvManager = "FW_WORKBENCH_BOSSMODE_MANAGER"

func init() {
	fw.RegisterExtension(Block)
}

// block is the part of the block workbench that this package reads: the
// key manager of workbench.bossmode. This package is the one reader of
// that key, because it sits below the packages bridges and bossmode, which
// ask it through Load. The package bossmode reads the other keys of
// workbench.bossmode.
type block struct {
	Bossmode struct {
		Manager string `yaml:"manager"`
	} `yaml:"bossmode"`
}

// Profile returns the profile of the manager: EnvManager when it is set,
// else workbench.bossmode.manager, else "", which means no manager. A
// block that does not decode, and a value that is not a profile name,
// give "", and the doctor line names the error.
func Profile(cfg *fw.Config) string {
	name, _ := Load(cfg)
	return name
}

// Load is Profile with the error of a block that does not decode and of a
// value that is not a profile name, such as a value with a space or a
// slash. The packages bridges and bossmode read the profile of the manager
// through it, so the key has one reader.
func Load(cfg *fw.Config) (string, error) {
	name := ""
	if v, ok := os.LookupEnv(EnvManager); ok && strings.TrimSpace(v) != "" {
		name = strings.TrimSpace(v)
	} else if cfg != nil {
		var b block
		if _, err := cfg.Extension(Block, &b); err != nil {
			return "", fmt.Errorf("manager: read workbench.bossmode.manager: %w", err)
		}
		name = strings.TrimSpace(b.Bossmode.Manager)
	}
	if strings.ContainsAny(name, " \t\r\n/") {
		return "", fmt.Errorf("manager: workbench.bossmode.manager (%s) %q is not a profile name", EnvManager, name)
	}
	return name, nil
}
