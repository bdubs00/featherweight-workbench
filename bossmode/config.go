package bossmode

// This file reads the configuration of bossmode: the block bossmode under
// the top-level key workbench of fw.yaml, which the package teams claims,
// and the variables FW_WORKBENCH_BOSSMODE_*, which win over the file. It
// also reads the two keys of the block bridges that the settings of the
// page name.

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// Block is the top-level key of fw.yaml that the workbench claims.
const Block = "workbench"

// Variables that set the configuration. Each wins over the file.
const (
	EnvEnabled = "FW_WORKBENCH_BOSSMODE_ENABLED"
	EnvDir     = "FW_WORKBENCH_BOSSMODE_DIR"
	EnvManager = "FW_WORKBENCH_BOSSMODE_MANAGER"
	// EnvPlacement and EnvMaxMembers set the keys of workbench.bridges that
	// the settings of the page name. The package of the bridges owns the
	// block.
	EnvPlacement  = "FW_WORKBENCH_BRIDGES_PLACEMENT"
	EnvMaxMembers = "FW_WORKBENCH_BRIDGES_MAX_MEMBERS"
)

// Defaults of the keys of workbench.bridges that the settings name.
const (
	DefaultPlacement  = "queue"
	DefaultMaxMembers = 16
)

// Config configures bossmode.
//
// Enabled turns bossmode on: the page at /bossmode, the settings at
// /v1/bossmode, and the stream at /v1/bossmode/stream. It is off by
// default. Dir is the directory of the static files of the page, the dist
// directory of the build of featherweight-bossmode. Empty serves the copy
// that the binary embeds, which is a placeholder page unless the build
// put the page into bossmode/static. Manager is the profile name of the
// manager agent. Empty means no manager. Bossmode reads it for the
// settings of the page and for the doctor line, and starts no run of it.
type Config struct {
	Enabled bool   `yaml:"enabled"`
	Dir     string `yaml:"dir"`
	Manager string `yaml:"manager"`
}

// Bridges holds the keys of workbench.bridges that the settings of the
// page name: the default placement of a new bridge, queue or spawn, and
// the largest roster of a bridge.
type Bridges struct {
	Placement  string `yaml:"placement"`
	MaxMembers int    `yaml:"maxMembers"`
}

// block is the part of the workbench block that this package reads.
type block struct {
	Bossmode Config  `yaml:"bossmode"`
	Bridges  Bridges `yaml:"bridges"`
}

// Load reads the configuration of bossmode from the block and the
// variables. It returns an error that names the key for a value that does
// not fit.
func Load(cfg *fw.Config) (Config, error) {
	b, err := readBlock(cfg)
	if err != nil {
		return Config{}, err
	}
	c := b.Bossmode
	if v, ok := lookup(EnvEnabled); ok {
		on, err := parseSwitch(v)
		if err != nil {
			return Config{}, fmt.Errorf("bossmode: %s=%q is not on or off", EnvEnabled, v)
		}
		c.Enabled = on
	}
	if v, ok := lookup(EnvDir); ok {
		c.Dir = v
	}
	if v, ok := lookup(EnvManager); ok {
		c.Manager = v
	}
	c.Dir, c.Manager = strings.TrimSpace(c.Dir), strings.TrimSpace(c.Manager)
	if strings.ContainsAny(c.Manager, " \t\r\n/") {
		return Config{}, fmt.Errorf("bossmode: workbench.bossmode.manager %q is not a profile name", c.Manager)
	}
	return c, nil
}

// LoadBridges reads the keys of workbench.bridges that the settings of the
// page name, with their defaults. A value that does not fit gives the
// default: the package of the bridges checks the block.
func LoadBridges(cfg *fw.Config) Bridges {
	b, _ := readBlock(cfg)
	out := b.Bridges
	if v, ok := lookup(EnvPlacement); ok {
		out.Placement = v
	}
	if v, ok := lookup(EnvMaxMembers); ok {
		n, err := strconv.Atoi(v)
		if err != nil {
			n = 0
		}
		out.MaxMembers = n
	}
	switch out.Placement = strings.ToLower(strings.TrimSpace(out.Placement)); out.Placement {
	case "queue", "spawn":
	default:
		out.Placement = DefaultPlacement
	}
	if out.MaxMembers <= 0 {
		out.MaxMembers = DefaultMaxMembers
	}
	return out
}

func readBlock(cfg *fw.Config) (block, error) {
	var b block
	if cfg == nil {
		return b, nil
	}
	if _, err := cfg.Extension(Block, &b); err != nil {
		return block{}, fmt.Errorf("bossmode: %w", err)
	}
	return b, nil
}

// parseSwitch reads on, off, true, false, yes, no, 1, or 0.
func parseSwitch(v string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "on", "true", "yes", "1":
		return true, nil
	case "off", "false", "no", "0":
		return false, nil
	}
	return false, fmt.Errorf("bossmode: %q is not on or off", v)
}

func lookup(name string) (string, bool) {
	v, ok := os.LookupEnv(name)
	v = strings.TrimSpace(v)
	return v, ok && v != ""
}
