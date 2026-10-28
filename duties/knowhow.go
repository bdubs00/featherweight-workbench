package duties

// This file reads the block workbench.knowhow, the configuration of
// Know-how that the duties need: the share of the runs of each duty that
// the engine holds back from Know-how (docs/KNOWHOW.md section 6.6). The
// package knowhow reads the other keys of the same block.

import (
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/teams"
)

// EnvHeldBack sets workbench.knowhow.heldBack. It wins over the file.
const EnvHeldBack = "FW_WORKBENCH_KNOWHOW_HELD_BACK"

// DefaultHeldBack is the share of the runs of each duty that get none of
// what the agent learned, for the comparison of Know-how.
const DefaultHeldBack = 0.05

// KnowhowConfig is the block workbench.knowhow. HeldBack is the share of
// the runs of each duty that the engine holds back from Know-how, with
// the metadata fw.knowhow: off, from 0 to 1. Nil takes DefaultHeldBack,
// and 0 holds no run back.
type KnowhowConfig struct {
	HeldBack *float64 `yaml:"heldBack"`
}

// Share returns the share of held-back runs.
func (c KnowhowConfig) Share() float64 {
	if c.HeldBack == nil {
		return DefaultHeldBack
	}
	return *c.HeldBack
}

// knowhowBlock is the part of the workbench block that this file reads.
type knowhowBlock struct {
	Knowhow KnowhowConfig `yaml:"knowhow"`
}

// LoadKnowhow reads workbench.knowhow and EnvHeldBack, and checks the
// share. It returns an error that names the key of a value that does not
// fit.
func LoadKnowhow(cfg *fw.Config) (KnowhowConfig, error) {
	var b knowhowBlock
	if cfg != nil {
		if _, err := cfg.Extension(teams.Block, &b); err != nil {
			return KnowhowConfig{}, fmt.Errorf("duties: %w", err)
		}
	}
	c := b.Knowhow
	if v := strings.TrimSpace(os.Getenv(EnvHeldBack)); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return KnowhowConfig{}, fmt.Errorf("duties: %s=%q is not a number", EnvHeldBack, v)
		}
		c.HeldBack = &f
	}
	if s := c.Share(); math.IsNaN(s) || s < 0 || s > 1 {
		return KnowhowConfig{}, fmt.Errorf("duties: workbench.knowhow.heldBack (%s) must be 0 to 1, not %g", EnvHeldBack, s)
	}
	return c, nil
}
