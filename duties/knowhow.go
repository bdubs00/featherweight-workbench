package duties

// This file reads the block workbench.knowhow, the configuration of
// Know-how that the duties need: the share of comparison runs. A
// comparison run goes without what the agent learned, so a person can see
// what learning is worth. With a share of 0.05, 1 in 20 runs of a duty is
// a comparison run (docs/KNOWHOW.md section 6.6). The package knowhow
// reads the other keys of the same block.

import (
	"fmt"
	"log/slog"
	"math"
	"os"
	"strconv"
	"strings"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/teams"
)

// The keys and the variables of the share of comparison runs. Each
// variable wins over the file. heldBack is the old name of
// comparisonRuns: it still works, and the log says that it is the old
// name. When both names are set, the new name wins.
const (
	KeyComparisonRuns = "workbench.knowhow.comparisonRuns"
	KeyHeldBack       = "workbench.knowhow.heldBack"
	EnvComparisonRuns = "FW_WORKBENCH_KNOWHOW_COMPARISON_RUNS"
	EnvHeldBack       = "FW_WORKBENCH_KNOWHOW_HELD_BACK"
)

// DefaultComparisonRuns is the share of the runs of each duty that are
// comparison runs: they get none of what the agent learned. It is 0: the
// comparison needs about 400 firings before it says anything, and a
// comparison run does its work without what the agent learned. So no run
// is a comparison run until a person sets a share.
const DefaultComparisonRuns = 0.0

// KnowhowConfig is the block workbench.knowhow. ComparisonRuns is the
// share of the runs of each duty that the engine makes comparison runs,
// with the metadata fw.knowhow: off, from 0 to 1. Nil takes
// DefaultComparisonRuns, and 0 makes no comparison run. HeldBack is the
// old name of ComparisonRuns, and it counts only while ComparisonRuns is
// nil.
type KnowhowConfig struct {
	ComparisonRuns *float64 `yaml:"comparisonRuns"`
	HeldBack       *float64 `yaml:"heldBack"`
}

// Share returns the share of comparison runs.
func (c KnowhowConfig) Share() float64 {
	switch {
	case c.ComparisonRuns != nil:
		return *c.ComparisonRuns
	case c.HeldBack != nil:
		return *c.HeldBack
	}
	return DefaultComparisonRuns
}

// knowhowBlock is the part of the workbench block that this file reads.
type knowhowBlock struct {
	Knowhow KnowhowConfig `yaml:"knowhow"`
}

// LoadKnowhow reads workbench.knowhow and the variables, and checks the
// share. For each old name that is set, it logs one line that names the
// new name and says which value counts. It returns an error that names
// the key of a value that does not fit.
func LoadKnowhow(cfg *fw.Config, log *slog.Logger) (KnowhowConfig, error) {
	var b knowhowBlock
	if cfg != nil {
		if _, err := cfg.Extension(teams.Block, &b); err != nil {
			return KnowhowConfig{}, fmt.Errorf("duties: %w", err)
		}
	}
	c := b.Knowhow
	if c.HeldBack != nil {
		oldName(log, KeyHeldBack, KeyComparisonRuns, c.ComparisonRuns != nil)
		if c.ComparisonRuns == nil {
			c.ComparisonRuns = c.HeldBack
		}
		c.HeldBack = nil
	}
	name, v := EnvComparisonRuns, strings.TrimSpace(os.Getenv(EnvComparisonRuns))
	if old := strings.TrimSpace(os.Getenv(EnvHeldBack)); old != "" {
		oldName(log, EnvHeldBack, EnvComparisonRuns, v != "")
		if v == "" {
			name, v = EnvHeldBack, old
		}
	}
	if v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return KnowhowConfig{}, fmt.Errorf("duties: %s=%q is not a number", name, v)
		}
		c.ComparisonRuns = &f
	}
	if s := c.Share(); math.IsNaN(s) || s < 0 || s > 1 {
		return KnowhowConfig{}, fmt.Errorf("duties: %s (%s) must be 0 to 1, not %g", KeyComparisonRuns, EnvComparisonRuns, s)
	}
	return c, nil
}

// oldName logs the line of an old name that is set. With both names set,
// the line says that the new name wins.
func oldName(log *slog.Logger, old, current string, both bool) {
	if log == nil {
		return
	}
	if both {
		log.Warn(fmt.Sprintf("duties: %s is the old name of %s. Both are set, so %s wins and %s counts for nothing.", old, current, current, old))
		return
	}
	log.Warn(fmt.Sprintf("duties: %s is the old name of %s. Its value counts. Use %s instead.", old, current, current))
}
