package pages

// This file reads the configuration of the wiki: the block pages under the
// top-level key workbench of fw.yaml, which the package teams claims, and
// the variables FW_WORKBENCH_PAGES_*, which win over the file.

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/internal/bucket"
	"github.com/bdubs00/featherweight-workbench/teams"
)

// Variables that set the configuration. Each wins over the file.
const (
	EnvMaxPageBytes = "FW_WORKBENCH_PAGES_MAX_PAGE_BYTES"
	EnvMaxPages     = "FW_WORKBENCH_PAGES_MAX_PAGES"
	EnvHistory      = "FW_WORKBENCH_PAGES_HISTORY"
)

// Defaults and bounds of the configuration.
const (
	DefaultMaxPageBytes = 256 << 10
	DefaultMaxPages     = 10000
	DefaultHistory      = 64

	// MaxPageBytesLimit bounds maxPageBytes, so a page with its record
	// stays below the largest message of a NATS server by default (1 MB).
	MaxPageBytesLimit = 512 << 10
	// MaxPagesLimit bounds maxPages.
	MaxPagesLimit = 100000
)

// Config configures the wiki. MaxPageBytes bounds the text of one page,
// MaxPages the number of pages, and History the number of versions of each
// page that the store keeps, the newest included. A zero value takes the
// default.
type Config struct {
	MaxPageBytes int `yaml:"maxPageBytes"`
	MaxPages     int `yaml:"maxPages"`
	History      int `yaml:"history"`
}

// block is the part of the workbench block that this package reads.
type block struct {
	Pages Config `yaml:"pages"`
}

// Load reads the configuration of the wiki from the block and the
// variables, fills the defaults, and checks the values. It returns an
// error that names the key for a value that does not fit.
func Load(cfg *fw.Config) (Config, error) {
	var b block
	if cfg != nil {
		if _, err := cfg.Extension(teams.Block, &b); err != nil {
			return Config{}, fmt.Errorf("pages: %w", err)
		}
	}
	c := b.Pages
	for _, e := range []struct {
		env string
		dst *int
	}{{EnvMaxPageBytes, &c.MaxPageBytes}, {EnvMaxPages, &c.MaxPages}, {EnvHistory, &c.History}} {
		v, ok := os.LookupEnv(e.env)
		if v = strings.TrimSpace(v); !ok || v == "" {
			continue
		}
		n, err := strconv.Atoi(v)
		if err != nil {
			return Config{}, fmt.Errorf("pages: %s=%q is not a whole number", e.env, v)
		}
		*e.dst = n
	}
	if c.MaxPageBytes == 0 {
		c.MaxPageBytes = DefaultMaxPageBytes
	}
	if c.MaxPages == 0 {
		c.MaxPages = DefaultMaxPages
	}
	if c.History == 0 {
		c.History = DefaultHistory
	}
	var problems []string
	if c.MaxPageBytes < 1 || c.MaxPageBytes > MaxPageBytesLimit {
		problems = append(problems, fmt.Sprintf("workbench.pages.maxPageBytes (%s) must be 1 to %d, not %d", EnvMaxPageBytes, MaxPageBytesLimit, c.MaxPageBytes))
	}
	if c.MaxPages < 1 || c.MaxPages > MaxPagesLimit {
		problems = append(problems, fmt.Sprintf("workbench.pages.maxPages (%s) must be 1 to %d, not %d", EnvMaxPages, MaxPagesLimit, c.MaxPages))
	}
	if c.History < 1 || c.History > bucket.MaxHistory {
		problems = append(problems, fmt.Sprintf("workbench.pages.history (%s) must be 1 to %d, the most versions that a NATS bucket keeps, not %d", EnvHistory, bucket.MaxHistory, c.History))
	}
	if len(problems) > 0 {
		return Config{}, fmt.Errorf("pages: %s", strings.Join(problems, "; "))
	}
	return c, nil
}

// bucketName follows the stream in the name of the bucket of the wiki.
const bucketName = "PAGES"

// recordBytes bounds the record of a page beside its text: the writer, the
// time, the summary, the hash, the run, the title, and the tags.
const recordBytes = 16 << 10

// bucketConfig is the bucket of the wiki for the configuration c.
func bucketConfig(c Config) bucket.Config {
	return bucket.Config{
		Name:        bucketName,
		Description: "The pages of the wiki of a featherweight fleet, one entry per page, by its path in base64url.",
		History:     c.History,
		MaxValue:    c.MaxPageBytes + recordBytes,
	}
}

// Status says where the wiki of the configuration cfg lives and with
// which limits, for a line of fw-workbench doctor. It reports false for a
// configuration that does not fit. A bus that keeps no store gives true
// and a line that says that the wiki is off.
func Status(cfg *fw.Config) (bool, string) {
	c, err := Load(cfg)
	if err != nil {
		return false, err.Error()
	}
	where, on := bucket.Where(cfg, bucketConfig(c))
	if !on {
		return true, "pages: off; the wiki lives " + where
	}
	return true, fmt.Sprintf("pages: the wiki lives in %s, with at most %d pages of at most %d KB each and %d versions of each page", where, c.MaxPages, c.MaxPageBytes>>10, c.History)
}
