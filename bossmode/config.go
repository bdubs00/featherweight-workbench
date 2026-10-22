package bossmode

// This file reads the configuration of bossmode: the block bossmode under
// the top-level key workbench of fw.yaml, which the package teams claims,
// and the variables FW_WORKBENCH_BOSSMODE_*, which win over the file. The
// package manager reads the key manager of the block, and the package
// bridges reads the block bridges, so each key has one reader; Load and
// the settings of the page ask them.

import (
	"fmt"
	"os"
	"strings"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/manager"
)

// Block is the top-level key of fw.yaml that the workbench claims.
const Block = "workbench"

// Variables that set the configuration. Each wins over the file.
const (
	EnvEnabled = "FW_WORKBENCH_BOSSMODE_ENABLED"
	EnvDir     = "FW_WORKBENCH_BOSSMODE_DIR"
	// EnvManager sets the key manager, which the package manager reads.
	EnvManager = manager.EnvManager
)

// Config configures bossmode.
//
// Enabled turns bossmode on: the page at /bossmode, the settings at
// /v1/bossmode, and the stream at /v1/bossmode/stream. It is off by
// default. Dir is the directory of the static files of the page, the dist
// directory of the build of featherweight-bossmode. Empty serves the copy
// that the binary embeds: the page when the build put it into
// bossmode/static, else the placeholder in bossmode/placeholder. Manager
// is the profile name of the manager agent, from manager.Load. Empty means
// no manager. Bossmode reads it for the settings of the page and for the
// doctor line, and starts no run of it.
type Config struct {
	Enabled bool   `yaml:"enabled"`
	Dir     string `yaml:"dir"`
	Manager string `yaml:"-"`
}

// block is the part of the workbench block that this package reads.
type block struct {
	Bossmode Config `yaml:"bossmode"`
}

// Load reads the configuration of bossmode from the block and the
// variables, and the manager from manager.Load. It returns an error that
// names the key for a value that does not fit.
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
	c.Dir = strings.TrimSpace(c.Dir)
	if c.Manager, err = manager.Load(cfg); err != nil {
		return Config{}, err
	}
	return c, nil
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
