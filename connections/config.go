package connections

// This file reads the configuration of the grants: the block grants under
// the top-level key workbench of fw.yaml, which the workbench claims, and
// the variables FW_WORKBENCH_GRANTS_*, which win over the file. It also
// builds the store that the configuration names.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// Block is the top-level key of fw.yaml that the workbench claims.
const Block = "workbench"

// Variables that set the configuration. Each wins over the file.
const (
	EnvStore = "FW_WORKBENCH_GRANTS_STORE"
	EnvDir   = "FW_WORKBENCH_GRANTS_DIR"
	EnvKey   = "FW_WORKBENCH_GRANTS_KEY"
)

// Names of the built-in stores and the default key.
const (
	StoreFile   = "file"
	StoreMemory = "memory"
	// DefaultKey is the fw.Secrets name of the key that encrypts the
	// grants of the file store. Without it, the store creates a random key
	// in the file grant.key beside the grants.
	DefaultKey = "FW_GRANT_KEY"
)

// Config says where the workbench keeps the grants that people give it.
// Store is file, the default, memory, or a registered driver. Dir is the
// directory of the file store, <home>/grants by default. Key is the
// fw.Secrets name of the key that encrypts the file store, FW_GRANT_KEY by
// default. Without that secret, the store creates a random key in the file
// grant.key in Dir.
type Config struct {
	Store string `yaml:"store"`
	Dir   string `yaml:"dir"`
	Key   string `yaml:"key"`
}

// block is the part of the workbench block that this package reads.
type block struct {
	Grants Config `yaml:"grants"`
}

// Load reads the configuration of the grants from the block and the
// variables and fills the defaults.
func Load(cfg *fw.Config) (Config, error) {
	var b block
	if cfg != nil {
		if _, err := cfg.Extension(Block, &b); err != nil {
			return Config{}, err
		}
	}
	c := b.Grants
	for _, e := range []struct {
		env string
		dst *string
	}{{EnvStore, &c.Store}, {EnvDir, &c.Dir}, {EnvKey, &c.Key}} {
		if v, ok := os.LookupEnv(e.env); ok && strings.TrimSpace(v) != "" {
			*e.dst = strings.TrimSpace(v)
		}
	}
	c.Store = strings.ToLower(strings.TrimSpace(c.Store))
	if c.Store == "" {
		c.Store = StoreFile
	}
	if c.Dir == "" {
		home := ""
		if cfg != nil {
			home = cfg.Home
		}
		if home == "" {
			home = fw.DefaultHome()
		}
		c.Dir = filepath.Join(home, "grants")
	}
	if c.Key == "" {
		c.Key = DefaultKey
	}
	return c, nil
}

// StoreCtor builds a store from the configuration.
type StoreCtor func(ctx context.Context, c Config, cfg *fw.Config, secrets fw.Secrets) (Store, error)

var stores = struct {
	mu sync.RWMutex
	m  map[string]StoreCtor
}{m: map[string]StoreCtor{}}

// RegisterStore registers a store driver by name. The names file and
// memory are built in.
func RegisterStore(name string, c StoreCtor) {
	stores.mu.Lock()
	defer stores.mu.Unlock()
	stores.m[name] = c
}

// NewStore builds the store that c names. The file store does no I/O until
// the first grant.
func NewStore(ctx context.Context, c Config, cfg *fw.Config, secrets fw.Secrets) (Store, error) {
	switch c.Store {
	case "", StoreFile:
		return NewFileStore(c.Dir, grantKeyLoader(secrets, c.Key, filepath.Join(c.Dir, keyFile))), nil
	case StoreMemory:
		return NewMemStore(), nil
	}
	stores.mu.RLock()
	ctor, ok := stores.m[c.Store]
	stores.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("connections: unknown workbench.grants.store %q", c.Store)
	}
	return ctor(ctx, c, cfg, secrets)
}

// Describe says where the grants live, for fw doctor. It names the secret
// of the key, never its value.
func Describe(ctx context.Context, c Config, secrets fw.Secrets) string {
	switch c.Store {
	case "", StoreFile:
		if secrets != nil {
			if v, err := secrets.Get(ctx, c.Key); err == nil && v != "" {
				return fmt.Sprintf("file %s, key from %s", c.Dir, c.Key)
			}
		}
		return fmt.Sprintf("file %s, key in %s (set %s to share one key)", c.Dir, filepath.Join(c.Dir, keyFile), c.Key)
	case StoreMemory:
		return "memory, so they end with the process"
	default:
		return "driver " + c.Store
	}
}
