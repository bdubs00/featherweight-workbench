package bridges

// This file reads the configuration of the bridges: the block bridges under
// the top-level key workbench of fw.yaml, which the package teams claims,
// and the variables FW_WORKBENCH_BRIDGES_*, which win over the file. This
// package is the one reader of the block: the settings of the page of
// bossmode ask Load. The profile of the manager, workbench.bossmode.manager,
// comes from the package manager (manager.Load), its one reader.

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/internal/bucket"
	"github.com/bdubs00/featherweight-workbench/manager"
	"github.com/bdubs00/featherweight-workbench/teams"
)

// Variables that set the configuration. Each wins over the file.
const (
	EnvPlacement          = "FW_WORKBENCH_BRIDGES_PLACEMENT"
	EnvMaxMembers         = "FW_WORKBENCH_BRIDGES_MAX_MEMBERS"
	EnvMaxMessages        = "FW_WORKBENCH_BRIDGES_MAX_MESSAGES"
	EnvMaxFanoutPerMinute = "FW_WORKBENCH_BRIDGES_MAX_FANOUT_PER_MINUTE"
	EnvMaxPairMessages    = "FW_WORKBENCH_BRIDGES_MAX_PAIR_MESSAGES"
	EnvMaxTurns           = "FW_WORKBENCH_BRIDGES_MAX_TURNS"
	// EnvManager names the profile of the manager. The package manager
	// reads it with workbench.bossmode.manager (manager.Load).
	EnvManager = manager.EnvManager
)

// Defaults and bounds of the configuration.
const (
	DefaultPlacement          = PlacementQueue
	DefaultMaxMembers         = 16
	DefaultMaxMessages        = 2000
	DefaultMaxFanoutPerMinute = 30
	DefaultMaxPairMessages    = 6
	// DefaultMaxTurns is the default of caps.maxTurns: the turns of every
	// run of a bridge together. Each message gives a seat its allowance
	// again (grant), so this cap is what bounds a bridge whose agents
	// keep each other busy. A healthy bridge of a few seats spends some
	// hundred turns.
	DefaultMaxTurns = 1000

	// MaxMembersLimit bounds maxMembers, so a record stays small.
	MaxMembersLimit = 64
	// MaxMessagesLimit bounds maxMessages.
	MaxMessagesLimit = 100000
	// MaxFanoutLimit bounds maxFanoutPerMinute and maxPairMessages.
	MaxFanoutLimit = 1000
	// MaxTurnsLimit bounds maxTurns.
	MaxTurnsLimit = 1000000
	// MaxCostLimit bounds maxCostUSD, in dollars.
	MaxCostLimit = 100000
)

// Config configures the bridges. Placement is the default placement of a
// new bridge, MaxMembers bounds the seats of a bridge, and the four caps
// are the defaults of the caps of a new bridge. Manager is the profile of
// the manager, from workbench.bossmode.manager through manager.Load, and
// empty means no manager. No key names a model.
type Config struct {
	Placement          string `yaml:"placement"`
	MaxMembers         int    `yaml:"maxMembers"`
	MaxMessages        int    `yaml:"maxMessages"`
	MaxFanoutPerMinute int    `yaml:"maxFanoutPerMinute"`
	MaxPairMessages    int    `yaml:"maxPairMessages"`
	MaxTurns           int    `yaml:"maxTurns"`
	Manager            string `yaml:"-"`
}

// block is the part of the workbench block that this package reads.
type block struct {
	Bridges Config `yaml:"bridges"`
}

// Load reads the configuration of the bridges from the block and the
// variables, fills the defaults, and checks the values. It returns an
// error that names the key for a value that does not fit.
func Load(cfg *fw.Config) (Config, error) {
	var b block
	if cfg != nil {
		if _, err := cfg.Extension(teams.Block, &b); err != nil {
			return Config{}, fmt.Errorf("bridges: %w", err)
		}
	}
	c := b.Bridges
	name, err := manager.Load(cfg)
	if err != nil {
		return Config{}, err
	}
	c.Manager = name
	if v, ok := os.LookupEnv(EnvPlacement); ok && strings.TrimSpace(v) != "" {
		c.Placement = v
	}
	for _, e := range []struct {
		env string
		dst *int
	}{{EnvMaxMembers, &c.MaxMembers}, {EnvMaxMessages, &c.MaxMessages}, {EnvMaxFanoutPerMinute, &c.MaxFanoutPerMinute}, {EnvMaxPairMessages, &c.MaxPairMessages}, {EnvMaxTurns, &c.MaxTurns}} {
		v, ok := os.LookupEnv(e.env)
		if v = strings.TrimSpace(v); !ok || v == "" {
			continue
		}
		n, err := strconv.Atoi(v)
		if err != nil {
			return Config{}, fmt.Errorf("bridges: %s=%q is not a whole number", e.env, v)
		}
		*e.dst = n
	}
	c.Placement = strings.ToLower(strings.TrimSpace(c.Placement))
	if c.Placement == "" {
		c.Placement = DefaultPlacement
	}
	if c.MaxMembers == 0 {
		c.MaxMembers = DefaultMaxMembers
	}
	if c.MaxMessages == 0 {
		c.MaxMessages = DefaultMaxMessages
	}
	if c.MaxFanoutPerMinute == 0 {
		c.MaxFanoutPerMinute = DefaultMaxFanoutPerMinute
	}
	if c.MaxPairMessages == 0 {
		c.MaxPairMessages = DefaultMaxPairMessages
	}
	if c.MaxTurns == 0 {
		c.MaxTurns = DefaultMaxTurns
	}
	var problems []string
	if c.Placement != PlacementQueue && c.Placement != PlacementSpawn {
		problems = append(problems, fmt.Sprintf("workbench.bridges.placement (%s) must be queue or spawn, not %q", EnvPlacement, c.Placement))
	}
	if c.MaxMembers < 1 || c.MaxMembers > MaxMembersLimit {
		problems = append(problems, fmt.Sprintf("workbench.bridges.maxMembers (%s) must be 1 to %d, not %d", EnvMaxMembers, MaxMembersLimit, c.MaxMembers))
	}
	if err := checkCaps(c.caps()); err != "" {
		problems = append(problems, "the defaults of workbench.bridges: "+err)
	}
	if len(problems) > 0 {
		return Config{}, fmt.Errorf("bridges: %s", strings.Join(problems, "; "))
	}
	return c, nil
}

// caps returns the default caps of a new bridge.
func (c Config) caps() Caps {
	return Caps{MaxMessages: c.MaxMessages, MaxFanoutPerMinute: c.MaxFanoutPerMinute, MaxPairMessages: c.MaxPairMessages, MaxTurns: c.MaxTurns}
}

// checkCaps says what is wrong with caps, or "".
func checkCaps(c Caps) string {
	var problems []string
	if c.MaxMessages < 1 || c.MaxMessages > MaxMessagesLimit {
		problems = append(problems, fmt.Sprintf("maxMessages (%s) must be 1 to %d, not %d", EnvMaxMessages, MaxMessagesLimit, c.MaxMessages))
	}
	if c.MaxFanoutPerMinute < 1 || c.MaxFanoutPerMinute > MaxFanoutLimit {
		problems = append(problems, fmt.Sprintf("maxFanoutPerMinute (%s) must be 1 to %d, not %d", EnvMaxFanoutPerMinute, MaxFanoutLimit, c.MaxFanoutPerMinute))
	}
	if c.MaxPairMessages < 1 || c.MaxPairMessages > MaxFanoutLimit {
		problems = append(problems, fmt.Sprintf("maxPairMessages (%s) must be 1 to %d, not %d", EnvMaxPairMessages, MaxFanoutLimit, c.MaxPairMessages))
	}
	if c.MaxTurns < 1 || c.MaxTurns > MaxTurnsLimit {
		problems = append(problems, fmt.Sprintf("maxTurns (%s) must be 1 to %d, not %d", EnvMaxTurns, MaxTurnsLimit, c.MaxTurns))
	}
	if c.MaxCostUSD < 0 || c.MaxCostUSD > MaxCostLimit || c.MaxCostUSD != c.MaxCostUSD {
		problems = append(problems, fmt.Sprintf("maxCostUSD must be 0 for no cap, or up to %d dollars, not %g", MaxCostLimit, c.MaxCostUSD))
	}
	return strings.Join(problems, "; ")
}

// Names of the buckets, after the stream.
const (
	bridgesBucket  = "BRIDGES"
	channelsBucket = "CHANNELS"
)

// Bounds of the values of the buckets: a record with its seats and its
// marks, and an entry with a cut text, its decision, and its reactions.
const (
	maxRecordBytes = 512 << 10
	maxEntryBytes  = 64 << 10
)

// bucketConfigs are the two buckets of the bridges.
func bucketConfigs() (records, channels bucket.Config) {
	records = bucket.Config{
		Name:        bridgesBucket,
		Description: "The bridges of a featherweight fleet, one record per bridge, by its id.",
		History:     4,
		MaxValue:    maxRecordBytes,
	}
	channels = bucket.Config{
		Name:        channelsBucket,
		Description: "The channels of the bridges of a featherweight fleet: one entry per message under <bridge>.<seq>, and the read marks of people under <bridge>.read.<hash>.",
		History:     1,
		MaxValue:    maxEntryBytes,
	}
	return records, channels
}

// Status says where the bridges of the configuration cfg live and with
// which defaults, for a line of fw-workbench doctor. It reports false for
// a configuration that does not fit. A bus that keeps no store gives true
// and a line that says that the bridges are off.
func Status(cfg *fw.Config) (bool, string) {
	c, err := Load(cfg)
	if err != nil {
		return false, err.Error()
	}
	rc, cc := bucketConfigs()
	where, on := bucket.Where(cfg, rc)
	if !on {
		return true, "bridges: off; the bridges live " + where
	}
	if strings.HasPrefix(where, "the key-value bucket ") {
		where = "the key-value buckets " + bucket.Name(cfg, rc) + " and " + bucket.Name(cfg, cc) + " of the NATS bus"
	}
	manager := "no manager"
	if c.Manager != "" {
		manager = "the manager " + c.Manager
	}
	return true, fmt.Sprintf("bridges: the bridges live in %s, with the placement %s, at most %d members, the caps %d messages, %d deliveries a minute, %d messages between two agents, and %d turns, and %s", where, c.Placement, c.MaxMembers, c.MaxMessages, c.MaxFanoutPerMinute, c.MaxPairMessages, c.MaxTurns, manager)
}
