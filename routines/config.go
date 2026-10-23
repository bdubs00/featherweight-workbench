package routines

// This file reads the configuration of the routines: the block routines
// under the top-level key workbench of fw.yaml, which the package teams
// claims, and the variables FW_WORKBENCH_ROUTINES_*, which win over the
// file.

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/teams"
)

// Variables that set the configuration. Each wins over the file.
const (
	EnvReadyAfter     = "FW_WORKBENCH_ROUTINES_READY_AFTER"
	EnvReadyAgreement = "FW_WORKBENCH_ROUTINES_READY_AGREEMENT"
)

// Defaults and bounds of the configuration.
const (
	DefaultReadyAfter     = 200
	DefaultReadyAgreement = 0.95
	// MaxReadyAfter bounds readyAfter.
	MaxReadyAfter = 1000000
)

// Config configures the routines. A rehearsing routine is ready for a
// sign-off when, over its current version, it would have acted on at least
// ReadyAfter runs, it agreed with the agent on at least ReadyAgreement of
// them, and no run where it agreed came back. A zero value takes the
// default.
type Config struct {
	ReadyAfter     int     `yaml:"readyAfter"`
	ReadyAgreement float64 `yaml:"readyAgreement"`
}

// block is the part of the workbench block that this package reads.
type block struct {
	Routines Config `yaml:"routines"`
}

// Load reads the configuration of the routines from the block and the
// variables, fills the defaults, and checks the values. It returns an
// error that names the key for a value that does not fit.
func Load(cfg *fw.Config) (Config, error) {
	var b block
	if cfg != nil {
		if _, err := cfg.Extension(teams.Block, &b); err != nil {
			return Config{}, fmt.Errorf("routines: %w", err)
		}
	}
	c := b.Routines
	if v := strings.TrimSpace(os.Getenv(EnvReadyAfter)); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return Config{}, fmt.Errorf("routines: %s=%q is not a whole number", EnvReadyAfter, v)
		}
		c.ReadyAfter = n
	}
	if v := strings.TrimSpace(os.Getenv(EnvReadyAgreement)); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return Config{}, fmt.Errorf("routines: %s=%q is not a number", EnvReadyAgreement, v)
		}
		c.ReadyAgreement = f
	}
	if c.ReadyAfter == 0 {
		c.ReadyAfter = DefaultReadyAfter
	}
	if c.ReadyAgreement == 0 {
		c.ReadyAgreement = DefaultReadyAgreement
	}
	var problems []string
	if c.ReadyAfter < 1 || c.ReadyAfter > MaxReadyAfter {
		problems = append(problems, fmt.Sprintf("workbench.routines.readyAfter (%s) must be 1 to %d, not %d", EnvReadyAfter, MaxReadyAfter, c.ReadyAfter))
	}
	if c.ReadyAgreement <= 0 || c.ReadyAgreement > 1 {
		problems = append(problems, fmt.Sprintf("workbench.routines.readyAgreement (%s) must be above 0 and at most 1, not %g", EnvReadyAgreement, c.ReadyAgreement))
	}
	if len(problems) > 0 {
		return Config{}, fmt.Errorf("routines: %s", strings.Join(problems, "; "))
	}
	return c, nil
}
