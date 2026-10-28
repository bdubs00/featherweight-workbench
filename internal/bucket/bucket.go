// Package bucket is the key-value store under the stores of the front,
// such as the wiki (package pages) and the faces of the agents (package
// faces). On a NATS bus a store is a key-value bucket of the JetStream
// server of the bus, <stream>_<NAME>, where <stream> is bus.options.stream
// (FW by default), so every replica of the front reads one truth. On the
// bus mem a store lives in the memory of the process, and two processes
// share nothing. Another bus has no store.
//
// A store keeps one entry per record. A write names the revision that it
// read (Update, Delete) or wants a key without a live value (Create), and
// the bucket refuses the write with ErrConflict when another writer came
// first. Two replicas of the front therefore never lose a write: the loser
// reads again and decides again. The bucket keeps the last values of each
// key, up to its history, with the delete markers, so a store reads the
// older versions of a record from the bucket itself. A bucket with a TTL
// forgets each value once it is older than the TTL, as a history of runs
// needs.
package bucket

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// DefaultStream is the stream of the kernel driver of the NATS bus when
// bus.options.stream names none.
const DefaultStream = "FW"

// MaxHistory is the most values of one key that a bucket keeps, the bound
// of a NATS server.
const MaxHistory = jetstream.KeyValueMaxHistory

// Errors of a bucket.
var (
	// ErrNotFound means that the key holds no live value, or that the
	// bucket keeps no value of the key at the revision.
	ErrNotFound = errors.New("bucket: no value has the key")
	// ErrConflict means that the key changed since the writer read it, or
	// that it holds a live value and the writer wanted none.
	ErrConflict = errors.New("bucket: the key changed since it was read")
	// ErrNoStore means that the bus of the configuration has no store:
	// it is neither nats nor mem.
	ErrNoStore = errors.New("bucket: the bus keeps no store")
)

// Entry is one value of a key. Deleted marks a delete marker in a
// history, which has no value.
type Entry struct {
	Key      string
	Value    []byte
	Revision uint64
	Created  time.Time
	Deleted  bool
}

// Bucket is a key-value bucket. A revision is a number that grows with
// every write of the bucket, so the revisions of one key grow but are not
// consecutive.
type Bucket interface {
	// Get returns the newest value of key, or ErrNotFound when the key has
	// no value or its newest entry is a delete marker.
	Get(ctx context.Context, key string) (Entry, error)
	// At returns the value of key at the revision, or ErrNotFound when the
	// bucket keeps no value of key at that revision.
	At(ctx context.Context, key string, rev uint64) (Entry, error)
	// Create writes the value when key has no live value, else
	// ErrConflict. It returns the new revision.
	Create(ctx context.Context, key string, value []byte) (uint64, error)
	// Update writes the value when the newest entry of key has the
	// revision last, else ErrConflict. It returns the new revision.
	Update(ctx context.Context, key string, value []byte, last uint64) (uint64, error)
	// Delete puts a delete marker on key when its newest entry has the
	// revision last, else ErrConflict. The history stays.
	Delete(ctx context.Context, key string, last uint64) error
	// History returns the entries of key that the bucket keeps, oldest
	// first, with the delete markers. A key without entries has none.
	History(ctx context.Context, key string) ([]Entry, error)
	// Keys returns every key with a live value and the revision of that
	// value, without the value, sorted by key.
	Keys(ctx context.Context) ([]Entry, error)
	// Describe names the bucket for a person.
	Describe() string
}

// Config names a bucket and sets its limits.
type Config struct {
	// Name follows the stream in the name of the bucket on NATS, such as
	// PAGES for <stream>_PAGES.
	Name string
	// Description says what the bucket holds, for an operator who lists
	// the buckets of the server.
	Description string
	// History is how many values of each key the bucket keeps, 1 to
	// MaxHistory.
	History int
	// MaxValue bounds one value in bytes. Zero leaves the bound of the
	// server.
	MaxValue int
	// TTL is how long a value lives after its write. The bucket then
	// forgets it, and a key whose values are all gone has no value. Zero
	// keeps every value.
	TTL time.Duration
}

// Stream returns the stream of the NATS bus of cfg: bus.options.stream,
// else DefaultStream.
func Stream(cfg *fw.Config) string {
	if cfg != nil {
		if s := strings.TrimSpace(cfg.Bus.Options["stream"]); s != "" {
			return s
		}
	}
	return DefaultStream
}

// Name returns the name of the bucket c on the NATS bus of cfg:
// <stream>_<c.Name>.
func Name(cfg *fw.Config, c Config) string { return Stream(cfg) + "_" + c.Name }

// Where says where the bucket c of the configuration cfg keeps its
// values, for a log line or a doctor line, and reports whether the bus
// keeps a store at all.
func Where(cfg *fw.Config, c Config) (string, bool) {
	switch driver(cfg) {
	case "nats":
		return "the key-value bucket " + Name(cfg, c) + " of the NATS bus", true
	case "mem":
		return "the memory of this process, because the bus is mem; another process shares nothing", true
	}
	return "nowhere, because the bus is " + driver(cfg) + " and a store needs the bus nats or mem", false
}

func driver(cfg *fw.Config) string {
	if cfg == nil {
		return "mem"
	}
	d := strings.TrimSpace(cfg.Bus.Driver)
	if d == "" {
		return "mem"
	}
	return d
}

// Open opens the bucket c for the bus of cfg: a bucket of the NATS server
// for the bus nats, a bucket in memory for the bus mem, and ErrNoStore for
// another bus. The function that it returns closes the connection, and it
// is nil for a bucket in memory. A NATS server that is not reachable yet
// does not stop the start: the client keeps trying, and each request fails
// until it connects.
func Open(ctx context.Context, cfg *fw.Config, c Config, log *slog.Logger) (Bucket, func(), error) {
	return open(ctx, cfg, c, log, true)
}

// OpenOnce is Open for a command that runs once, such as an export: it
// fails at once when the NATS server does not answer.
func OpenOnce(ctx context.Context, cfg *fw.Config, c Config, log *slog.Logger) (Bucket, func(), error) {
	return open(ctx, cfg, c, log, false)
}

func open(ctx context.Context, cfg *fw.Config, c Config, log *slog.Logger, retry bool) (Bucket, func(), error) {
	if c.History < 1 || c.History > MaxHistory {
		return nil, nil, fmt.Errorf("bucket: the history of %s must be 1 to %d, not %d", c.Name, MaxHistory, c.History)
	}
	switch driver(cfg) {
	case "mem":
		return NewMem(c), nil, nil
	case "nats":
	default:
		return nil, nil, fmt.Errorf("%w: the bus is %s; set bus.driver to nats, or mem for one process", ErrNoStore, driver(cfg))
	}
	nc, js, err := dial(ctx, cfg, log, retry)
	if err != nil {
		return nil, nil, err
	}
	return NewNATS(js, Name(cfg, c), c), nc.Close, nil
}

// dial connects to the server of bus.url with the keys credentials and
// token of bus.options, as the kernel driver of the bus does. With retry,
// a server that does not answer yet does not fail the dial.
func dial(ctx context.Context, cfg *fw.Config, log *slog.Logger, retry bool) (*nats.Conn, jetstream.JetStream, error) {
	opt := cfg.Bus.Options
	nopts := []nats.Option{
		nats.Name("fw-workbench"),
		nats.MaxReconnects(-1),
		nats.RetryOnFailedConnect(retry),
		nats.ReconnectWait(time.Second),
	}
	if p := strings.TrimSpace(opt["credentials"]); p != "" {
		nopts = append(nopts, nats.UserCredentials(p))
	}
	if name := strings.TrimSpace(opt["token"]); name != "" {
		tok, err := fw.EnvSecrets{}.Get(ctx, name)
		if err != nil {
			return nil, nil, fmt.Errorf("bucket: the token of the bus: %w", err)
		}
		nopts = append(nopts, nats.Token(tok))
	}
	addr := cfg.Bus.URL
	if addr == "" {
		addr = nats.DefaultURL
	}
	nc, err := nats.Connect(addr, nopts...)
	if err != nil {
		return nil, nil, fmt.Errorf("bucket: connect to the bus server: %w", redactURL(err))
	}
	js, err := jetstream.New(nc)
	if err != nil {
		nc.Close()
		return nil, nil, fmt.Errorf("bucket: open JetStream on the bus server: %w", err)
	}
	if !nc.IsConnected() && log != nil {
		log.Warn("bucket: the bus server is not reachable yet; the stores answer once it connects")
	}
	return nc, js, nil
}

// redactURL drops the URL from a parse error, because the URL can hold a
// password or a token.
func redactURL(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}

// natsBucket is a key-value bucket of a NATS server. A read never makes
// the bucket: a store without a bucket is empty. The first write of the
// process makes the bucket, or sets the limits of the configuration on a
// bucket that exists.
type natsBucket struct {
	js   jetstream.JetStream
	name string
	cfg  Config

	mu   sync.Mutex
	kv   jetstream.KeyValue
	made bool // this process made the bucket or set its limits
}

// NewNATS returns the bucket name of a JetStream server with the limits of
// c. A test passes the JetStream of its own server.
func NewNATS(js jetstream.JetStream, name string, c Config) Bucket {
	return &natsBucket{js: js, name: name, cfg: c}
}

func (b *natsBucket) Describe() string { return "the key-value bucket " + b.name }

// open returns the bucket. For a write it makes the bucket, or sets the
// limits once per process. For a read it returns nil when the bucket does
// not exist.
func (b *natsBucket) open(ctx context.Context, write bool) (jetstream.KeyValue, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.kv != nil && (b.made || !write) {
		return b.kv, nil
	}
	if !write {
		kv, err := b.js.KeyValue(ctx, b.name)
		if gone(err) {
			return nil, nil
		}
		if err != nil {
			return nil, fmt.Errorf("bucket: open %s: %w", b.name, err)
		}
		b.kv = kv
		return kv, nil
	}
	kv, err := b.js.CreateOrUpdateKeyValue(ctx, jetstream.KeyValueConfig{
		Bucket:       b.name,
		Description:  b.cfg.Description,
		History:      uint8(b.cfg.History),
		MaxValueSize: int32(b.cfg.MaxValue),
		TTL:          b.cfg.TTL,
		Storage:      jetstream.FileStorage,
	})
	if err != nil {
		return nil, fmt.Errorf("bucket: make %s: %w", b.name, err)
	}
	b.kv, b.made = kv, true
	return kv, nil
}

// forget drops the handle of the bucket, so the next request opens it
// again, as after a restart of a server without storage.
func (b *natsBucket) forget() {
	b.mu.Lock()
	b.kv, b.made = nil, false
	b.mu.Unlock()
}

// gone reports whether an error means that the bucket is gone.
func gone(err error) bool {
	return errors.Is(err, jetstream.ErrBucketNotFound) || errors.Is(err, jetstream.ErrStreamNotFound)
}

// read runs a read on the bucket. A bucket that does not exist answers
// ErrNotFound.
func (b *natsBucket) read(ctx context.Context, f func(jetstream.KeyValue) error) error {
	kv, err := b.open(ctx, false)
	if err != nil {
		return err
	}
	if kv == nil {
		return ErrNotFound
	}
	err = f(kv)
	if gone(err) {
		b.forget()
		return ErrNotFound
	}
	return err
}

// write runs a write on the bucket, and makes the bucket again once when
// it is gone.
func (b *natsBucket) write(ctx context.Context, f func(jetstream.KeyValue) error) error {
	for try := 0; ; try++ {
		kv, err := b.open(ctx, true)
		if err == nil {
			err = f(kv)
		}
		if gone(err) && try == 0 {
			b.forget()
			continue
		}
		return err
	}
}

func entryOf(e jetstream.KeyValueEntry) Entry {
	return Entry{
		Key:      e.Key(),
		Value:    slices.Clone(e.Value()),
		Revision: e.Revision(),
		Created:  e.Created(),
		Deleted:  e.Operation() != jetstream.KeyValuePut,
	}
}

func (b *natsBucket) Get(ctx context.Context, key string) (Entry, error) {
	var out Entry
	err := b.read(ctx, func(kv jetstream.KeyValue) error {
		e, err := kv.Get(ctx, key)
		if errors.Is(err, jetstream.ErrKeyNotFound) || errors.Is(err, jetstream.ErrKeyDeleted) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("bucket: read %s: %w", b.name, err)
		}
		out = entryOf(e)
		return nil
	})
	return out, err
}

func (b *natsBucket) At(ctx context.Context, key string, rev uint64) (Entry, error) {
	var out Entry
	err := b.read(ctx, func(kv jetstream.KeyValue) error {
		e, err := kv.GetRevision(ctx, key, rev)
		if errors.Is(err, jetstream.ErrKeyNotFound) || errors.Is(err, jetstream.ErrKeyDeleted) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("bucket: read %s at a revision: %w", b.name, err)
		}
		out = entryOf(e)
		if out.Deleted {
			return ErrNotFound
		}
		return nil
	})
	return out, err
}

func (b *natsBucket) Create(ctx context.Context, key string, value []byte) (uint64, error) {
	var rev uint64
	err := b.write(ctx, func(kv jetstream.KeyValue) error {
		var err error
		rev, err = kv.Create(ctx, key, value)
		if errors.Is(err, jetstream.ErrKeyExists) {
			return ErrConflict
		}
		if err != nil && !gone(err) {
			return fmt.Errorf("bucket: write %s: %w", b.name, err)
		}
		return err
	})
	return rev, err
}

func (b *natsBucket) Update(ctx context.Context, key string, value []byte, last uint64) (uint64, error) {
	var rev uint64
	err := b.write(ctx, func(kv jetstream.KeyValue) error {
		var err error
		rev, err = kv.Update(ctx, key, value, last)
		if errors.Is(err, jetstream.ErrKeyRevisionMismatch) || errors.Is(err, jetstream.ErrKeyExists) {
			return ErrConflict
		}
		if err != nil && !gone(err) {
			return fmt.Errorf("bucket: write %s: %w", b.name, err)
		}
		return err
	})
	return rev, err
}

func (b *natsBucket) Delete(ctx context.Context, key string, last uint64) error {
	return b.write(ctx, func(kv jetstream.KeyValue) error {
		err := kv.Delete(ctx, key, jetstream.LastRevision(last))
		if errors.Is(err, jetstream.ErrKeyRevisionMismatch) || errors.Is(err, jetstream.ErrKeyExists) {
			return ErrConflict
		}
		if err != nil && !gone(err) {
			return fmt.Errorf("bucket: delete in %s: %w", b.name, err)
		}
		return err
	})
}

func (b *natsBucket) History(ctx context.Context, key string) ([]Entry, error) {
	var out []Entry
	err := b.read(ctx, func(kv jetstream.KeyValue) error {
		list, err := kv.History(ctx, key)
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("bucket: read the history in %s: %w", b.name, err)
		}
		for _, e := range list {
			out = append(out, entryOf(e))
		}
		return nil
	})
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	return out, err
}

func (b *natsBucket) Keys(ctx context.Context) ([]Entry, error) {
	var out []Entry
	err := b.read(ctx, func(kv jetstream.KeyValue) error {
		w, err := kv.WatchAll(ctx, jetstream.MetaOnly(), jetstream.IgnoreDeletes())
		if err != nil {
			return err
		}
		defer func() { _ = w.Stop() }()
		for {
			var e jetstream.KeyValueEntry
			select {
			case <-ctx.Done():
				return ctx.Err()
			case e = <-w.Updates():
			}
			if e == nil {
				return nil // the end of the entries that the bucket holds now
			}
			out = append(out, Entry{Key: e.Key(), Revision: e.Revision(), Created: e.Created()})
		}
	})
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("bucket: list the keys of %s: %w", b.name, err)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

// keysUnder lists the keys under first with a filter of the server, so
// the server sends those keys only.
func (b *natsBucket) keysUnder(ctx context.Context, first string) ([]string, error) {
	entries, err := b.entriesUnder(ctx, first)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Key)
	}
	return out, nil
}

// entriesUnder lists the keys under first with the revisions of their
// newest values, without the values, with a filter of the server.
func (b *natsBucket) entriesUnder(ctx context.Context, first string) ([]Entry, error) {
	var out []Entry
	err := b.read(ctx, func(kv jetstream.KeyValue) error {
		w, err := kv.WatchFiltered(ctx, []string{first + ".>"}, jetstream.MetaOnly(), jetstream.IgnoreDeletes())
		if err != nil {
			return err
		}
		defer func() { _ = w.Stop() }()
		for {
			var e jetstream.KeyValueEntry
			select {
			case <-ctx.Done():
				return ctx.Err()
			case e = <-w.Updates():
			}
			if e == nil {
				return nil
			}
			out = append(out, Entry{Key: e.Key(), Revision: e.Revision(), Created: e.Created()})
		}
	})
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("bucket: list the keys under %s of %s: %w", first, b.name, err)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

// memBucket is a bucket in the memory of the process, for the bus mem.
type memBucket struct {
	cfg Config
	now func() time.Time

	mu   sync.Mutex
	rev  uint64
	keys map[string][]Entry // oldest first, with the delete markers
}

// NewMem returns a bucket in memory with the history of c.
func NewMem(c Config) Bucket {
	if c.History < 1 {
		c.History = 1
	}
	return &memBucket{cfg: c, now: time.Now, keys: map[string][]Entry{}}
}

func (m *memBucket) Describe() string { return "the memory of this process" }

// expire drops the entries of key that are older than the TTL of the
// bucket, as the server drops its messages. The caller holds mu.
func (m *memBucket) expire(key string) {
	if m.cfg.TTL <= 0 {
		return
	}
	cut := m.now().UTC().Add(-m.cfg.TTL)
	list := m.keys[key]
	i := 0
	for i < len(list) && list[i].Created.Before(cut) {
		i++
	}
	switch {
	case i == 0:
	case i == len(list):
		delete(m.keys, key)
	default:
		m.keys[key] = slices.Clone(list[i:])
	}
}

// last returns the newest entry of key. The caller holds mu.
func (m *memBucket) last(key string) (Entry, bool) {
	m.expire(key)
	list := m.keys[key]
	if len(list) == 0 {
		return Entry{}, false
	}
	return list[len(list)-1], true
}

// add appends an entry to key and trims the history. The caller holds mu.
func (m *memBucket) add(key string, value []byte, deleted bool) uint64 {
	m.rev++
	e := Entry{Key: key, Value: slices.Clone(value), Revision: m.rev, Created: m.now().UTC(), Deleted: deleted}
	list := append(m.keys[key], e)
	if len(list) > m.cfg.History {
		list = slices.Clone(list[len(list)-m.cfg.History:])
	}
	m.keys[key] = list
	return m.rev
}

func copyEntry(e Entry) Entry {
	e.Value = slices.Clone(e.Value)
	return e
}

func (m *memBucket) Get(_ context.Context, key string) (Entry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.last(key)
	if !ok || e.Deleted {
		return Entry{}, ErrNotFound
	}
	return copyEntry(e), nil
}

func (m *memBucket) At(_ context.Context, key string, rev uint64) (Entry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.expire(key)
	for _, e := range m.keys[key] {
		if e.Revision == rev && !e.Deleted {
			return copyEntry(e), nil
		}
	}
	return Entry{}, ErrNotFound
}

func (m *memBucket) Create(_ context.Context, key string, value []byte) (uint64, error) {
	if m.tooLarge(value) {
		return 0, fmt.Errorf("bucket: the value has %d bytes, more than the %d of the bucket", len(value), m.cfg.MaxValue)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if e, ok := m.last(key); ok && !e.Deleted {
		return 0, ErrConflict
	}
	return m.add(key, value, false), nil
}

func (m *memBucket) Update(_ context.Context, key string, value []byte, last uint64) (uint64, error) {
	if m.tooLarge(value) {
		return 0, fmt.Errorf("bucket: the value has %d bytes, more than the %d of the bucket", len(value), m.cfg.MaxValue)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if e, ok := m.last(key); !ok || e.Revision != last {
		return 0, ErrConflict
	}
	return m.add(key, value, false), nil
}

func (m *memBucket) Delete(_ context.Context, key string, last uint64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e, ok := m.last(key); !ok || e.Revision != last {
		return ErrConflict
	}
	m.add(key, nil, true)
	return nil
}

func (m *memBucket) History(_ context.Context, key string) ([]Entry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.expire(key)
	out := make([]Entry, 0, len(m.keys[key]))
	for _, e := range m.keys[key] {
		out = append(out, copyEntry(e))
	}
	return out, nil
}

func (m *memBucket) Keys(context.Context) ([]Entry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Entry
	for key := range m.keys {
		if e, ok := m.last(key); ok && !e.Deleted {
			out = append(out, Entry{Key: key, Revision: e.Revision, Created: e.Created})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

func (m *memBucket) keysUnder(ctx context.Context, first string) ([]string, error) {
	keys, err := m.Keys(ctx)
	if err != nil {
		return nil, err
	}
	return under(keys, first), nil
}

func (m *memBucket) entriesUnder(ctx context.Context, first string) ([]Entry, error) {
	keys, err := m.Keys(ctx)
	if err != nil {
		return nil, err
	}
	return entriesUnder(keys, first), nil
}

func (m *memBucket) tooLarge(value []byte) bool {
	return m.cfg.MaxValue > 0 && len(value) > m.cfg.MaxValue
}

// lister is a bucket that lists the keys under a first token itself.
type lister interface {
	keysUnder(ctx context.Context, first string) ([]string, error)
	entriesUnder(ctx context.Context, first string) ([]Entry, error)
}

// remover is a bucket that lists the keys whose newest entry is a removal.
type remover interface {
	removed(ctx context.Context) ([]Entry, error)
}

// Removed returns the keys of b whose newest entry is a removal, sorted,
// each with the revision and the time of the removal and Deleted true. A
// bucket that keeps no removals answers none.
func Removed(ctx context.Context, b Bucket) ([]Entry, error) {
	if r, ok := b.(remover); ok {
		return r.removed(ctx)
	}
	return nil, nil
}

func (b *natsBucket) removed(ctx context.Context) ([]Entry, error) {
	var out []Entry
	err := b.read(ctx, func(kv jetstream.KeyValue) error {
		w, err := kv.WatchAll(ctx, jetstream.MetaOnly())
		if err != nil {
			return err
		}
		defer func() { _ = w.Stop() }()
		for {
			var e jetstream.KeyValueEntry
			select {
			case <-ctx.Done():
				return ctx.Err()
			case e = <-w.Updates():
			}
			if e == nil {
				return nil // the end of the entries that the bucket holds now
			}
			if e.Operation() != jetstream.KeyValuePut {
				out = append(out, Entry{Key: e.Key(), Revision: e.Revision(), Created: e.Created(), Deleted: true})
			}
		}
	})
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("bucket: list the removed keys of %s: %w", b.name, err)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

func (m *memBucket) removed(context.Context) ([]Entry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Entry
	for key := range m.keys {
		if e, ok := m.last(key); ok && e.Deleted {
			out = append(out, Entry{Key: key, Revision: e.Revision, Created: e.Created, Deleted: true})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

// KeysUnder returns the keys of b whose first token, the text before the
// first dot, is first, sorted, without their values. A bucket of a NATS
// server asks the server for those keys only, so a store that keeps the
// entries of many records under one bucket reads the keys of one record.
func KeysUnder(ctx context.Context, b Bucket, first string) ([]string, error) {
	if l, ok := b.(lister); ok {
		return l.keysUnder(ctx, first)
	}
	keys, err := b.Keys(ctx)
	if err != nil {
		return nil, err
	}
	return under(keys, first), nil
}

// EntriesUnder returns the keys of b whose first token is first, sorted,
// with the revisions of their newest values and without the values, as
// KeysUnder does. A reader that keeps a copy of the values reads again only
// the keys whose revision changed.
func EntriesUnder(ctx context.Context, b Bucket, first string) ([]Entry, error) {
	if l, ok := b.(lister); ok {
		return l.entriesUnder(ctx, first)
	}
	keys, err := b.Keys(ctx)
	if err != nil {
		return nil, err
	}
	return entriesUnder(keys, first), nil
}

// entriesUnder returns the entries whose keys start with first and a dot.
func entriesUnder(keys []Entry, first string) []Entry {
	var out []Entry
	for _, e := range keys {
		if strings.HasPrefix(e.Key, first+".") {
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// under returns the keys of the entries that start with first and a dot.
func under(keys []Entry, first string) []string {
	var out []string
	for _, e := range keys {
		if strings.HasPrefix(e.Key, first+".") {
			out = append(out, e.Key)
		}
	}
	sort.Strings(out)
	return out
}

// Modify reads the newest value of key, gives it to f, and writes what f
// returns at the revision that it read: Create when the key holds no live
// value, else Update. When another writer came first, it reads again and
// calls f again, up to attempts times, and then returns ErrConflict. f
// gets the value and whether the key holds one, and returns the value to
// write, or nil to write nothing. An error of f ends Modify with that
// error. It reports whether it wrote.
func Modify(ctx context.Context, b Bucket, key string, attempts int, f func(value []byte, exists bool) ([]byte, error)) (bool, error) {
	for range attempts {
		cur, err := b.Get(ctx, key)
		exists := err == nil
		if err != nil && !errors.Is(err, ErrNotFound) {
			return false, err
		}
		value, err := f(cur.Value, exists)
		if err != nil || value == nil {
			return false, err
		}
		if exists {
			_, err = b.Update(ctx, key, value, cur.Revision)
		} else {
			_, err = b.Create(ctx, key, value)
		}
		if errors.Is(err, ErrConflict) {
			continue
		}
		return err == nil, err
	}
	return false, ErrConflict
}
