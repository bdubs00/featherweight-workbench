package knowhow

// This file reads the configuration of Know-how: the block knowhow under
// the top-level key workbench of fw.yaml, which the package teams claims,
// and the variables FW_WORKBENCH_KNOWHOW_*, which win over the file.

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/teams"
)

// Variables that set the configuration. Each wins over the file.
const (
	EnvReviewAgent     = "FW_WORKBENCH_KNOWHOW_REVIEW_AGENT"
	EnvReviewEvery     = "FW_WORKBENCH_KNOWHOW_REVIEW_EVERY"
	EnvProposeEnabled  = "FW_WORKBENCH_KNOWHOW_PROPOSE_ENABLED"
	EnvProposeDecider  = "FW_WORKBENCH_KNOWHOW_PROPOSE_DECIDER"
	EnvAgingRestDays   = "FW_WORKBENCH_KNOWHOW_AGING_REST_DAYS"
	EnvAgingPutAwayDay = "FW_WORKBENCH_KNOWHOW_AGING_PUT_AWAY_DAYS"
)

// Defaults and bounds of the configuration.
const (
	DefaultReviewEvery = 24 * time.Hour
	MinReviewEvery     = time.Hour
	MaxReviewEvery     = 30 * 24 * time.Hour

	DefaultProposeDays        = 30
	DefaultProposeMinRuns     = 10
	DefaultProposeMinVerified = 2

	DefaultRestDays    = 14
	DefaultPutAwayDays = 30
	MaxAgingDays       = 3650
)

// KnowhowConfig configures Know-how (docs/ARCHITECTURE.md section 28):
// the after-work review, the proposals of routines, and the aging of the
// notes. Its yaml block is workbench.knowhow.
type KnowhowConfig struct {
	Review  ReviewConfig  `yaml:"review"`
	Propose ProposeConfig `yaml:"propose"`
	Aging   AgingConfig   `yaml:"aging"`
}

// ReviewConfig names the profile of the after-work review and how often a
// run of it starts. Without Agent, no review runs: nothing picks a model
// or a price by default, because the profile names its own model and
// budget.
type ReviewConfig struct {
	Agent string        `yaml:"agent"`
	Every time.Duration `yaml:"every"`
}

// On reports whether the after-work review runs.
func (c ReviewConfig) On() bool { return c.Agent != "" }

// ProposeConfig configures the daily proposals of routines (section 6.5
// of docs/KNOWHOW.md). Enabled nil means on. A Duty needs MinRuns
// completed runs over the last Days days, MinVerified of them verified.
// Decider names the decider of the questions of a proposal, a key of
// deciders of the workers; without it, a proposal with questions stays
// out and the journal says why.
type ProposeConfig struct {
	Enabled     *bool  `yaml:"enabled"`
	Days        int    `yaml:"days"`
	MinRuns     int    `yaml:"minRuns"`
	MinVerified int    `yaml:"minVerified"`
	Decider     string `yaml:"decider"`
}

// On reports whether the proposals run.
func (c ProposeConfig) On() bool { return c.Enabled == nil || *c.Enabled }

// AgingConfig sets when a note that no run used rests (RestDays) and when
// it is put away (PutAwayDays).
type AgingConfig struct {
	RestDays    int `yaml:"restDays"`
	PutAwayDays int `yaml:"putAwayDays"`
}

// block is the part of the workbench block that this package reads.
type block struct {
	Knowhow KnowhowConfig `yaml:"knowhow"`
}

// Load reads the configuration of Know-how from the block and the
// variables, fills the defaults, and checks the values. It returns an
// error that names the key for a value that does not fit.
func Load(cfg *fw.Config) (KnowhowConfig, error) {
	var b block
	if cfg != nil {
		if _, err := cfg.Extension(teams.Block, &b); err != nil {
			return KnowhowConfig{}, fmt.Errorf("knowhow: %w", err)
		}
	}
	c := b.Knowhow
	if v, ok := os.LookupEnv(EnvReviewAgent); ok && strings.TrimSpace(v) != "" {
		c.Review.Agent = v
	}
	if v := strings.TrimSpace(os.Getenv(EnvReviewEvery)); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return KnowhowConfig{}, fmt.Errorf("knowhow: %s=%q is not a duration such as 24h", EnvReviewEvery, v)
		}
		c.Review.Every = d
	}
	if v := strings.TrimSpace(os.Getenv(EnvProposeEnabled)); v != "" {
		on, err := parseSwitch(v)
		if err != nil {
			return KnowhowConfig{}, fmt.Errorf("knowhow: %s=%q is not on or off", EnvProposeEnabled, v)
		}
		c.Propose.Enabled = &on
	}
	if v := strings.TrimSpace(os.Getenv(EnvProposeDecider)); v != "" {
		c.Propose.Decider = v
	}
	for _, e := range []struct {
		name string
		to   *int
	}{{EnvAgingRestDays, &c.Aging.RestDays}, {EnvAgingPutAwayDay, &c.Aging.PutAwayDays}} {
		if v := strings.TrimSpace(os.Getenv(e.name)); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil {
				return KnowhowConfig{}, fmt.Errorf("knowhow: %s=%q is not a whole number", e.name, v)
			}
			*e.to = n
		}
	}
	c.Review.Agent = strings.TrimSpace(c.Review.Agent)
	c.Propose.Decider = strings.TrimSpace(c.Propose.Decider)
	if c.Review.Every == 0 {
		c.Review.Every = DefaultReviewEvery
	}
	if c.Propose.Days == 0 {
		c.Propose.Days = DefaultProposeDays
	}
	if c.Propose.MinRuns == 0 {
		c.Propose.MinRuns = DefaultProposeMinRuns
	}
	if c.Propose.MinVerified == 0 {
		c.Propose.MinVerified = DefaultProposeMinVerified
	}
	if c.Aging.RestDays == 0 {
		c.Aging.RestDays = DefaultRestDays
	}
	if c.Aging.PutAwayDays == 0 {
		c.Aging.PutAwayDays = DefaultPutAwayDays
	}
	var problems []string
	if c.Review.Agent != "" && CheckAgent(c.Review.Agent) != nil {
		problems = append(problems, fmt.Sprintf("workbench.knowhow.review.agent (%s) %q is not the name of an agent profile", EnvReviewAgent, c.Review.Agent))
	}
	if c.Review.Every < MinReviewEvery || c.Review.Every > MaxReviewEvery {
		problems = append(problems, fmt.Sprintf("workbench.knowhow.review.every (%s) must be 1h to 720h, not %s", EnvReviewEvery, c.Review.Every))
	}
	if c.Propose.Days < 1 || c.Propose.Days > 365 {
		problems = append(problems, fmt.Sprintf("workbench.knowhow.propose.days must be 1 to 365, not %d", c.Propose.Days))
	}
	if c.Propose.MinRuns < 3 || c.Propose.MinRuns > 1000 {
		problems = append(problems, fmt.Sprintf("workbench.knowhow.propose.minRuns must be 3 to 1000, not %d", c.Propose.MinRuns))
	}
	if c.Propose.MinVerified < 1 || c.Propose.MinVerified > c.Propose.MinRuns {
		problems = append(problems, fmt.Sprintf("workbench.knowhow.propose.minVerified must be 1 to minRuns, not %d", c.Propose.MinVerified))
	}
	if c.Aging.RestDays < 1 || c.Aging.RestDays > MaxAgingDays {
		problems = append(problems, fmt.Sprintf("workbench.knowhow.aging.restDays (%s) must be 1 to %d, not %d", EnvAgingRestDays, MaxAgingDays, c.Aging.RestDays))
	}
	if c.Aging.PutAwayDays <= c.Aging.RestDays || c.Aging.PutAwayDays > MaxAgingDays {
		problems = append(problems, fmt.Sprintf("workbench.knowhow.aging.putAwayDays (%s) must be more than restDays and at most %d, not %d", EnvAgingPutAwayDay, MaxAgingDays, c.Aging.PutAwayDays))
	}
	if len(problems) > 0 {
		return KnowhowConfig{}, fmt.Errorf("knowhow: %s", strings.Join(problems, "; "))
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
	return false, fmt.Errorf("knowhow: %q is not on or off", v)
}

// ReviewOff is the sentence that the page shows while no review runs
// (docs/KNOWHOW.md section 7).
const ReviewOff = "The after-work review is off: no agent is set."
