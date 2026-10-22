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
	"github.com/bdubs00/featherweight-workbench/skills"
)

// Block is the top-level key of fw.yaml that the workbench claims.
const Block = "workbench"

// Variables that set the configuration. Each wins over the file.
const (
	EnvEnabled = "FW_WORKBENCH_BOSSMODE_ENABLED"
	EnvDir     = "FW_WORKBENCH_BOSSMODE_DIR"
	// EnvMarketplaces sets the marketplaces of skills: name=url,name=url.
	EnvMarketplaces = "FW_WORKBENCH_BOSSMODE_MARKETPLACES"
	// EnvManager sets the key manager, which the package manager reads.
	EnvManager = manager.EnvManager
	// The variables of the theme of the page (Theme).
	EnvThemeAccent = "FW_WORKBENCH_BOSSMODE_THEME_ACCENT"
	EnvThemeName   = "FW_WORKBENCH_BOSSMODE_THEME_NAME"
	EnvThemeLogo   = "FW_WORKBENCH_BOSSMODE_THEME_LOGO"
	// The variables of the quick chat of the page (Chat).
	EnvChatAgent  = "FW_WORKBENCH_BOSSMODE_CHAT_AGENT"
	EnvChatMember = "FW_WORKBENCH_BOSSMODE_CHAT_MEMBER"
)

// Bounds and defaults of the theme.
const (
	// DefaultAccent is the accent of the page without a theme.
	DefaultAccent = "#FF4D00"
	// MaxThemeName bounds the name of the organization, in characters.
	MaxThemeName = 64
	// MaxLogoBytes bounds the file of the logo.
	MaxLogoBytes = 256 << 10
	// MaxChatMember bounds the name of the member of the quick chat, in
	// bytes.
	MaxChatMember = 256
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
// the deployment. Chat is the quick chat of the page. Marketplaces lists
// the marketplaces of skills that an admin imports from
// (docs/ARCHITECTURE.md section 20.7).
type Config struct {
	Enabled      bool                 `yaml:"enabled"`
	Dir          string               `yaml:"dir"`
	Theme        Theme                `yaml:"theme"`
	Chat         Chat                 `yaml:"chat"`
	Manager      string               `yaml:"-"`
	Marketplaces []skills.Marketplace `yaml:"marketplaces"`
}

// Chat is the quick chat of the page (docs/ARCHITECTURE.md section 17.7):
// a conversation of a person with a general assistant beside the bridges.
// Agent is the profile that the page runs for a quick chat; empty means
// no quick chat. Member is the fleet member that runs it, for POST
// /v1/runs?member=; empty lets the settings pick the member as they pick
// the member of the manager. Bossmode starts no run of it: the page does.
type Chat struct {
	Agent  string `yaml:"agent"`
	Member string `yaml:"member"`
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
	if c.Chat, err = loadChat(c.Chat); err != nil {
		return Config{}, err
	}
	if v, ok := lookup(EnvMarketplaces); ok {
		list, err := skills.ParseMarketplaces(v)
		if err != nil {
			return Config{}, fmt.Errorf("bossmode: %s: %w", EnvMarketplaces, err)
		}
		c.Marketplaces = list
	}
	if err := skills.CheckMarketplaces(c.Marketplaces); err != nil {
		return Config{}, fmt.Errorf("bossmode: workbench.bossmode.%w", err)
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

// loadChat applies the variables of the quick chat over the block and
// checks the names. A member without an agent is an error, because the
// member runs nothing then.
func loadChat(c Chat) (Chat, error) {
	if v, ok := lookup(EnvChatAgent); ok {
		c.Agent = v
	}
	if v, ok := lookup(EnvChatMember); ok {
		c.Member = v
	}
	c.Agent, c.Member = strings.TrimSpace(c.Agent), strings.TrimSpace(c.Member)
	if strings.ContainsAny(c.Agent, " \t\r\n/") || strings.IndexFunc(c.Agent, unicode.IsControl) >= 0 {
		return Chat{}, fmt.Errorf("bossmode: workbench.bossmode.chat.agent (%s) %q is not a profile name", EnvChatAgent, c.Agent)
	}
	switch {
	case len(c.Member) > MaxChatMember:
		return Chat{}, fmt.Errorf("bossmode: workbench.bossmode.chat.member (%s) has %d bytes; it holds at most %d", EnvChatMember, len(c.Member), MaxChatMember)
	case !utf8.ValidString(c.Member) || strings.IndexFunc(c.Member, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0:
		return Chat{}, fmt.Errorf("bossmode: workbench.bossmode.chat.member (%s) %q is not a member name; it holds no space and no control character", EnvChatMember, c.Member)
	case c.Member != "" && c.Agent == "":
		return Chat{}, fmt.Errorf("bossmode: workbench.bossmode.chat.member (%s) names the member %s, and workbench.bossmode.chat.agent (%s) names no profile; set the agent of the quick chat, or remove the member", EnvChatMember, c.Member, EnvChatAgent)
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
