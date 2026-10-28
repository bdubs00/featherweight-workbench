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
	"gopkg.in/yaml.v3"
)

// Variables that set the configuration. Each wins over the file.
const (
	// EnvEnabled turns the routines on or off.
	EnvEnabled = "FW_WORKBENCH_ROUTINES_ENABLED"
	// EnvReadyAfter sets both bounds of readyAfter with one number.
	EnvReadyAfter = "FW_WORKBENCH_ROUTINES_READY_AFTER"
	// EnvReadyAfterRead and EnvReadyAfterWrite set one bound each, and
	// win over EnvReadyAfter.
	EnvReadyAfterRead  = "FW_WORKBENCH_ROUTINES_READY_AFTER_READ"
	EnvReadyAfterWrite = "FW_WORKBENCH_ROUTINES_READY_AFTER_WRITE"
	EnvReadyAgreement  = "FW_WORKBENCH_ROUTINES_READY_AGREEMENT"
)

// Defaults and bounds of the configuration.
const (
	// DefaultReadyAfterRead and DefaultReadyAfterWrite are the runs that a
	// rehearsing routine of each risk acts on before it is ready
	// (docs/KNOWHOW.md section 6.5).
	DefaultReadyAfterRead  = 20
	DefaultReadyAfterWrite = 50
	DefaultReadyAgreement  = 0.95
	// MaxReadyAfter bounds each bound of readyAfter.
	MaxReadyAfter = 1000000
)

// ReadyAfter holds the runs that a rehearsing routine must act on before
// it is ready, by the risk of the routine: Read for a routine whose steps
// only read, and Write for every other routine, and for a routine whose
// risk the evidence does not know yet. In the file, one number sets both,
// as readyAfter: 200, and a map sets each, as {read: 20, write: 50}. A
// zero bound takes its default.
type ReadyAfter struct {
	Read  int `yaml:"read" json:"read"`
	Write int `yaml:"write" json:"write"`
}

// UnmarshalYAML reads one number for both bounds, or a map of the two.
func (r *ReadyAfter) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		var v int
		if err := n.Decode(&v); err != nil {
			return fmt.Errorf("readyAfter is a whole number or {read, write}: %w", err)
		}
		r.Read, r.Write = v, v
		return nil
	}
	type plain ReadyAfter
	var p plain
	if err := n.Decode(&p); err != nil {
		return err
	}
	*r = ReadyAfter(p)
	return nil
}

// For returns the bound of a risk: Read for fw.RiskRead, else Write.
func (r ReadyAfter) For(risk string) int {
	if risk == fw.RiskRead {
		return r.Read
	}
	return r.Write
}

// Config configures the routines. Enabled turns them on and off: nil means
// on, where the bus keeps a store, and false turns off the store, the
// routes, the tools, the evidence, and the routines that the store gives
// the pools. The profiles of a pool then keep the routines of their own
// configuration. A rehearsing routine is ready when, over its current
// version, it would have acted on at least the runs of ReadyAfter for its
// risk, it agreed with the agent on at least ReadyAgreement of them, and
// no run where it agreed came back. A zero value takes the default.
type Config struct {
	Enabled        *bool      `yaml:"enabled"`
	ReadyAfter     ReadyAfter `yaml:"readyAfter"`
	ReadyAgreement float64    `yaml:"readyAgreement"`
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
	for _, e := range []struct {
		name  string
		bound []*int
	}{
		{EnvReadyAfter, []*int{&c.ReadyAfter.Read, &c.ReadyAfter.Write}},
		{EnvReadyAfterRead, []*int{&c.ReadyAfter.Read}},
		{EnvReadyAfterWrite, []*int{&c.ReadyAfter.Write}},
	} {
		v := strings.TrimSpace(os.Getenv(e.name))
		if v == "" {
			continue
		}
		n, err := strconv.Atoi(v)
		if err != nil {
			return Config{}, fmt.Errorf("routines: %s=%q is not a whole number", e.name, v)
		}
		for _, b := range e.bound {
			*b = n
		}
	}
	if v := strings.TrimSpace(os.Getenv(EnvReadyAgreement)); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return Config{}, fmt.Errorf("routines: %s=%q is not a number", EnvReadyAgreement, v)
		}
		c.ReadyAgreement = f
	}
	if c.ReadyAfter.Read == 0 {
		c.ReadyAfter.Read = DefaultReadyAfterRead
	}
	if c.ReadyAfter.Write == 0 {
		c.ReadyAfter.Write = DefaultReadyAfterWrite
	}
	if c.ReadyAgreement == 0 {
		c.ReadyAgreement = DefaultReadyAgreement
	}
	var problems []string
	for _, b := range []struct {
		key, env string
		n        int
	}{{"read", EnvReadyAfterRead, c.ReadyAfter.Read}, {"write", EnvReadyAfterWrite, c.ReadyAfter.Write}} {
		if b.n < 1 || b.n > MaxReadyAfter {
			problems = append(problems, fmt.Sprintf("workbench.routines.readyAfter.%s (%s) must be 1 to %d, not %d", b.key, b.env, MaxReadyAfter, b.n))
		}
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
