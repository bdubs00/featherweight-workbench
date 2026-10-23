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
	// EnvEnabled turns the routines on or off.
	EnvEnabled        = "FW_WORKBENCH_ROUTINES_ENABLED"
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

// Config configures the routines. Enabled turns them on and off: nil means
// on, where the bus keeps a store, and false turns off the store, the
// routes, the tools, the evidence, and the routines that the store gives
// the pools. The profiles of a pool then keep the routines of their own
// configuration. A rehearsing routine is ready for a sign-off when, over
// its current version, it would have acted on at least ReadyAfter runs, it
// agreed with the agent on at least ReadyAgreement of them, and no run
// where it agreed came back. A zero value takes the default.
type Config struct {
	Enabled        *bool   `yaml:"enabled"`
	ReadyAfter     int     `yaml:"readyAfter"`
	ReadyAgreement float64 `yaml:"readyAgreement"`
}

// On reports whether the configuration leaves the routines on.
func (c Config) On() bool { return c.Enabled == nil || *c.Enabled }

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
	if v := strings.TrimSpace(os.Getenv(EnvEnabled)); v != "" {
		on, err := parseSwitch(v)
		if err != nil {
			return Config{}, fmt.Errorf("routines: %s=%q is not on or off", EnvEnabled, v)
		}
		c.Enabled = &on
	}
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

// parseSwitch reads on or off, also true, false, yes, no, 1, and 0.
func parseSwitch(v string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "on", "true", "yes", "1":
		return true, nil
	case "off", "false", "no", "0":
		return false, nil
	}
	return false, fmt.Errorf("routines: %q is not on or off", v)
}
