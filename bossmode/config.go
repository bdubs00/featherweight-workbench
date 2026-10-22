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
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

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
	// The variables of the theme of the page (Theme).
	EnvThemeAccent = "FW_WORKBENCH_BOSSMODE_THEME_ACCENT"
	EnvThemeName   = "FW_WORKBENCH_BOSSMODE_THEME_NAME"
	EnvThemeLogo   = "FW_WORKBENCH_BOSSMODE_THEME_LOGO"
)

// Bounds and defaults of the theme.
const (
	// DefaultAccent is the accent of the page without a theme.
	DefaultAccent = "#FF4D00"
	// MaxThemeName bounds the name of the organization, in characters.
	MaxThemeName = 64
	// MaxLogoBytes bounds the file of the logo.
	MaxLogoBytes = 256 << 10
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
// doctor line, and starts no run of it. Theme is the look of the page for
// the deployment.
type Config struct {
	Enabled bool   `yaml:"enabled"`
	Dir     string `yaml:"dir"`
	Theme   Theme  `yaml:"theme"`
	Manager string `yaml:"-"`
}

// Theme is the look of the page for one deployment, which the settings of
// the page carry (docs/ARCHITECTURE.md section 17.4). Accent is a CSS
// color in hex, #rgb, #rrggbb, or #rrggbbaa; empty means DefaultAccent.
// Name is the name of the organization, one line of at most MaxThemeName
// characters. Logo is the path of an SVG or a PNG file on the front, at
// most MaxLogoBytes, which the service reads at its start (readLogo).
type Theme struct {
	Accent string `yaml:"accent"`
	Name   string `yaml:"name"`
	Logo   string `yaml:"logo"`
}

// accentPattern is a color in hex with three, six, or eight digits.
var accentPattern = regexp.MustCompile(`^#([0-9a-fA-F]{3}|[0-9a-fA-F]{6}|[0-9a-fA-F]{8})$`)

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
	if c.Theme, err = loadTheme(c.Theme); err != nil {
		return Config{}, err
	}
	if c.Manager, err = manager.Load(cfg); err != nil {
		return Config{}, err
	}
	return c, nil
}

// loadTheme applies the variables of the theme over the block and checks
// the accent and the name. The service reads the logo at its start.
func loadTheme(t Theme) (Theme, error) {
	if v, ok := lookup(EnvThemeAccent); ok {
		t.Accent = v
	}
	if v, ok := lookup(EnvThemeName); ok {
		t.Name = v
	}
	if v, ok := lookup(EnvThemeLogo); ok {
		t.Logo = v
	}
	t.Accent, t.Name, t.Logo = strings.TrimSpace(t.Accent), strings.TrimSpace(t.Name), strings.TrimSpace(t.Logo)
	if t.Accent != "" && !accentPattern.MatchString(t.Accent) {
		return Theme{}, fmt.Errorf("bossmode: workbench.bossmode.theme.accent (%s) %q is no color; write it in hex as #rgb, #rrggbb, or #rrggbbaa", EnvThemeAccent, t.Accent)
	}
	if n := utf8.RuneCountInString(t.Name); n > MaxThemeName {
		return Theme{}, fmt.Errorf("bossmode: workbench.bossmode.theme.name (%s) has %d characters; it holds at most %d", EnvThemeName, n, MaxThemeName)
	}
	if !utf8.ValidString(t.Name) || strings.IndexFunc(t.Name, unicode.IsControl) >= 0 {
		return Theme{}, fmt.Errorf("bossmode: workbench.bossmode.theme.name (%s) must be one line of UTF-8 without a control character", EnvThemeName)
	}
	return t, nil
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
