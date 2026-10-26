package pools

// This file is the store of the runtime pools (docs/ARCHITECTURE.md
// section 14.10). Each runtime pool is one entry of a bucket (package
// internal/bucket), pool.<name> with the name in base64url, and the value
// is the entry as JSON. On a NATS bus the bucket is <stream>_POOLS, so
// every replica of the front reads one truth. Every write is a
// compare-and-swap on the revision of the entry: the front reads the
// entry, changes it, and writes at the revision that it read, so two
// replicas never lose a pool. The bucket keeps the last values of each
// entry with the delete marker, so a pool that a person creates again
// under an old name continues the generations of the old one.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/bdubs00/featherweight-kernel/ax"
	"github.com/bdubs00/featherweight-workbench/internal/bucket"
)

// StoreBucket is the bucket of the runtime pools.
var StoreBucket = bucket.Config{
	Name:        "POOLS",
	Description: "The runtime pools of a featherweight front: the pools that people create from a worker template, one entry per pool.",
	History:     10,
	MaxValue:    256 << 10,
}

// storeAttempts bounds the tries of a write that other writers race.
const storeAttempts = 8

// Errors of the store.
var (
	// ErrRuntimeNotFound means that the store holds no runtime pool of the
	// name.
	ErrRuntimeNotFound = errors.New("pools: no runtime pool has the name")
	// ErrRuntimeExists means that the store holds a runtime pool of the
	// name already.
	ErrRuntimeExists = errors.New("pools: a runtime pool has the name")
	// ErrRuntimeConflict means that other writers changed the entry each
	// time a write tried, or since the caller read it.
	ErrRuntimeConflict = errors.New("pools: the runtime pool changed")
)

// Stored is an entry of the store with Rev, the revision of its value in
// the bucket.
type Stored struct {
	Runtime
	Rev uint64
}

// Store keeps the runtime pools in a bucket. Build it with NewStore.
type Store struct {
	b   bucket.Bucket
	log *slog.Logger
}

// NewStore returns the store on the bucket b.
func NewStore(b bucket.Bucket, log *slog.Logger) *Store {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Store{b: b, log: log}
}

// Describe names the place of the store for a person.
func (s *Store) Describe() string { return s.b.Describe() }

const storeKeyPrefix = "pool."

func storeKey(name string) string {
	return storeKeyPrefix + base64.RawURLEncoding.EncodeToString([]byte(name))
}

// List returns every runtime pool of the store, sorted by name. An entry
// that does not decode is left out, and the log names it.
func (s *Store) List(ctx context.Context) ([]Stored, error) {
	keys, err := s.b.Keys(ctx)
	if err != nil {
		return nil, fmt.Errorf("pools: list the runtime pools in %s: %w", s.b.Describe(), err)
	}
	var out []Stored
	for _, k := range keys {
		if !strings.HasPrefix(k.Key, storeKeyPrefix) {
			continue
		}
		e, err := s.b.Get(ctx, k.Key)
		if errors.Is(err, bucket.ErrNotFound) {
			continue // removed since the list
		}
		if err != nil {
			return nil, fmt.Errorf("pools: read a runtime pool in %s: %w", s.b.Describe(), err)
		}
		st, err := decodeStored(e)
		if err != nil {
			s.log.Warn("pools: an entry of the runtime pools does not decode; the front leaves it out", "key", k.Key, "error", err)
			continue
		}
		out = append(out, st)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Get returns the runtime pool of the name, or an error that wraps
// ErrRuntimeNotFound.
func (s *Store) Get(ctx context.Context, name string) (Stored, error) {
	e, err := s.b.Get(ctx, storeKey(name))
	if errors.Is(err, bucket.ErrNotFound) {
		return Stored{}, fmt.Errorf("%w: %s", ErrRuntimeNotFound, name)
	}
	if err != nil {
		return Stored{}, fmt.Errorf("pools: read the runtime pool %s: %w", name, err)
	}
	return decodeStored(e)
}

func decodeStored(e bucket.Entry) (Stored, error) {
	var r Runtime
	if err := json.Unmarshal(e.Value, &r); err != nil {
		return Stored{}, fmt.Errorf("pools: decode a runtime pool: %w", err)
	}
	return Stored{Runtime: r, Rev: e.Revision}, nil
}

// Create writes a new runtime pool. The store sets its generation: one
// above the highest generation that the bucket keeps for the name, so a
// pool of an old name orders after the old pool, else 1. A name that the
// store holds already is an error that wraps ErrRuntimeExists.
func (s *Store) Create(ctx context.Context, r Runtime) (Stored, error) {
	key := storeKey(r.Name)
	history, err := s.b.History(ctx, key)
	if err != nil {
		return Stored{}, fmt.Errorf("pools: read the history of the runtime pool %s: %w", r.Name, err)
	}
	r.Generation = 1
	for _, e := range history {
		var old Runtime
		if e.Deleted || json.Unmarshal(e.Value, &old) != nil {
			continue
		}
		r.Generation = max(r.Generation, old.Generation+1)
	}
	value, err := json.Marshal(r)
	if err != nil {
		return Stored{}, fmt.Errorf("pools: encode the runtime pool %s: %w", r.Name, err)
	}
	rev, err := s.b.Create(ctx, key, value)
	if errors.Is(err, bucket.ErrConflict) {
		return Stored{}, fmt.Errorf("%w: %s", ErrRuntimeExists, r.Name)
	}
	if err != nil {
		return Stored{}, fmt.Errorf("pools: write the runtime pool %s: %w", r.Name, err)
	}
	return Stored{Runtime: r, Rev: rev}, nil
}

// Update reads the runtime pool of the name, applies f, and writes the
// result at the revision that it read. A change of the configuration of
// the workers, the template, the agent, the queue, the revision, the
// variables, or the resources, raises the generation, so the Tasks of the
// pool are replaced. A change of the replicas or of the mark of a delete
// keeps it, so no running worker goes for it. A result that changes
// nothing writes nothing. When another writer came first, it reads again
// and applies f again. An error of f stops the update and comes back as it
// is.
func (s *Store) Update(ctx context.Context, name string, f func(*Runtime) error) (Stored, error) {
	for range storeAttempts {
		cur, err := s.Get(ctx, name)
		if err != nil {
			return Stored{}, err
		}
		next := cur.Runtime
		next.Env = append([]RuntimeEnv(nil), cur.Env...)
		if cur.Resources != nil {
			r := *cur.Resources
			next.Resources = &r
		}
		if err := f(&next); err != nil {
			return Stored{}, err
		}
		next.Name, next.Generation = cur.Name, cur.Generation
		if same(cur.Runtime, next) {
			return cur, nil
		}
		if workerConfig(cur.Runtime) != workerConfig(next) {
			next.Generation++
		}
		value, err := json.Marshal(next)
		if err != nil {
			return Stored{}, fmt.Errorf("pools: encode the runtime pool %s: %w", name, err)
		}
		rev, err := s.b.Update(ctx, storeKey(name), value, cur.Rev)
		if errors.Is(err, bucket.ErrConflict) {
			continue
		}
		if err != nil {
			return Stored{}, fmt.Errorf("pools: write the runtime pool %s: %w", name, err)
		}
		return Stored{Runtime: next, Rev: rev}, nil
	}
	return Stored{}, fmt.Errorf("%w: other writers changed the runtime pool %s %d times while this write tried; try again", ErrRuntimeConflict, name, storeAttempts)
}

// Remove removes the runtime pool of the name when its entry still has the
// revision rev, else the error wraps ErrRuntimeConflict. An entry that is
// gone is no error. The history of the entry stays in the bucket.
func (s *Store) Remove(ctx context.Context, name string, rev uint64) error {
	err := s.b.Delete(ctx, storeKey(name), rev)
	if errors.Is(err, bucket.ErrConflict) {
		if _, gerr := s.Get(ctx, name); errors.Is(gerr, ErrRuntimeNotFound) {
			return nil
		}
		return fmt.Errorf("%w: %s changed since the server read it", ErrRuntimeConflict, name)
	}
	if err != nil {
		return fmt.Errorf("pools: remove the runtime pool %s: %w", name, err)
	}
	return nil
}

// workerConfig returns the part of an entry that the workers run, as one
// comparable text: the template, the agent, the queue, the revision, the
// variables, and the resources.
func workerConfig(r Runtime) string {
	b, _ := json.Marshal(struct {
		Template, Agent, Intake, Revision string
		Env                               []RuntimeEnv
		Resources                         *ax.ResourcesSpec
	}{r.Template, r.Agent, r.Intake, r.Revision, r.Env, r.Resources})
	return string(b)
}

// same reports whether two entries hold the same values.
func same(a, b Runtime) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}
