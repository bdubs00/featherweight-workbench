package sources

// This file keeps the claims of the sources in NATS JetStream, so every
// replica of the front shares them and a restart keeps them. Each source
// has one key-value bucket, "<stream>_SOURCE_<name>", where <stream> is
// the stream of the bus (bus.options.stream, FW by default). The TTL of
// the bucket is the window of the source, so the server forgets a key
// when its window ends. The store makes a bucket that is missing, and it
// sets the TTL of a bucket that exists to the window of the
// configuration. A claim is a create-if-absent of the key, which the
// server decides, so two replicas that claim one key at the same time get
// one claim between them.
//
// The front has its own connection for the claims, from bus.url and the
// credentials of the bus. The kernel bus keeps its connection to itself.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// storeTimeout bounds one request to the store.
const storeTimeout = 5 * time.Second

// bucketSuffix comes between the stream name and the source name in the
// name of a bucket.
const bucketSuffix = "_SOURCE_"

// claim is the value of a key in a bucket.
type claim struct {
	State string    `json:"state"`
	Key   string    `json:"key"`
	By    string    `json:"by,omitempty"`
	At    time.Time `json:"at"`
}

// natsStore is the store of a front whose bus is NATS.
type natsStore struct {
	nc      *nats.Conn
	js      jetstream.JetStream
	stream  string
	windows map[string]time.Duration
	by      func() string
	now     func() time.Time
	stale   time.Duration
	log     *slog.Logger

	mu      sync.Mutex
	buckets map[string]jetstream.KeyValue
}

// openNATS connects to the NATS server of the bus of cfg for the claims of
// the sources with the windows. A server that is not reachable at start
// does not stop the start: the client keeps trying, and every claim fails
// with ErrStore until it connects.
func openNATS(ctx context.Context, cfg *fw.Config, secrets fw.Secrets, windows map[string]time.Duration, by func() string, log *slog.Logger) (*natsStore, error) {
	opts := []nats.Option{nats.Name("fw-workbench-sources")}
	if p := strings.TrimSpace(cfg.Bus.Options["credentials"]); p != "" {
		opts = append(opts, nats.UserCredentials(p))
	}
	if name := strings.TrimSpace(cfg.Bus.Options["token"]); name != "" {
		if secrets == nil {
			secrets = fw.EnvSecrets{}
		}
		tok, err := secrets.Get(ctx, name)
		if err != nil {
			return nil, fmt.Errorf("sources: the token of the bus: %w", err)
		}
		opts = append(opts, nats.Token(tok))
	}
	addr := cfg.Bus.URL
	if addr == "" {
		addr = nats.DefaultURL
	}
	stream := strings.TrimSpace(cfg.Bus.Options["stream"])
	if stream == "" {
		stream = "FW"
	}
	return dialNATS(addr, stream, windows, by, log, opts...)
}

// dialNATS connects to addr. A test calls it with the address of a server
// in the process.
func dialNATS(addr, stream string, windows map[string]time.Duration, by func() string, log *slog.Logger, opts ...nats.Option) (*natsStore, error) {
	if log == nil {
		log = slog.Default()
	}
	opts = append([]nats.Option{
		nats.MaxReconnects(-1),
		nats.RetryOnFailedConnect(true),
		nats.ReconnectWait(time.Second),
		nats.DisconnectErrHandler(func(_ *nats.Conn, err error) {
			if err != nil {
				log.Warn("sources: the de-duplication store is out of reach", "error", redact(err))
			}
		}),
		nats.ReconnectHandler(func(*nats.Conn) { log.Info("sources: the de-duplication store is back") }),
	}, opts...)
	nc, err := nats.Connect(addr, opts...)
	if err != nil {
		return nil, fmt.Errorf("sources: connect to NATS: %w", redact(err))
	}
	js, err := jetstream.New(nc)
	if err != nil {
		nc.Close()
		return nil, fmt.Errorf("sources: JetStream: %w", err)
	}
	if by == nil {
		by = func() string { return "" }
	}
	return &natsStore{nc: nc, js: js, stream: stream, windows: windows, by: by, now: time.Now, stale: staleClaim,
		log: log, buckets: map[string]jetstream.KeyValue{}}, nil
}

func (n *natsStore) Describe() string {
	return "in the JetStream key-value buckets " + n.stream + bucketSuffix + "<source>"
}

// bucketName is the name of the bucket of a source.
func (n *natsStore) bucketName(source string) string { return n.stream + bucketSuffix + source }

// bucket returns the bucket of a source, and makes it when it is missing.
func (n *natsStore) bucket(ctx context.Context, source string) (jetstream.KeyValue, error) {
	n.mu.Lock()
	kv := n.buckets[source]
	n.mu.Unlock()
	if kv != nil {
		return kv, nil
	}
	window := n.windows[source]
	if window <= 0 {
		window = DefaultDedupFor
	}
	kv, err := n.js.CreateOrUpdateKeyValue(ctx, jetstream.KeyValueConfig{
		Bucket:      n.bucketName(source),
		Description: "The de-duplication keys of the source " + source + " of fw-workbench",
		History:     1,
		TTL:         window,
		Storage:     jetstream.FileStorage,
	})
	if err != nil {
		return nil, fmt.Errorf("make the bucket %s: %w", n.bucketName(source), err)
	}
	n.mu.Lock()
	n.buckets[source] = kv
	n.mu.Unlock()
	return kv, nil
}

// forget drops the handle of a bucket, so the next request makes the
// bucket again, as after a restart of a server without storage.
func (n *natsStore) forget(source string) {
	n.mu.Lock()
	delete(n.buckets, source)
	n.mu.Unlock()
}

// lost reports whether an error means that the bucket is gone.
func lost(err error) bool {
	return errors.Is(err, jetstream.ErrBucketNotFound) || errors.Is(err, jetstream.ErrStreamNotFound) || errors.Is(err, jetstream.ErrNoStreamResponse)
}

// keyID is the name of a key in a bucket: the SHA-256 of the key in hex,
// because a key of an event can hold any character.
func keyID(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

func (n *natsStore) Take(ctx context.Context, source, key string) (ticket, error) {
	if !n.nc.IsConnected() {
		return ticket{}, fmt.Errorf("%w: not connected to NATS", ErrStore)
	}
	ctx, cancel := context.WithTimeout(ctx, storeTimeout)
	defer cancel()
	id := keyID(key)
	val, err := json.Marshal(claim{State: statePending, Key: key, By: n.by(), At: n.now().UTC()})
	if err != nil {
		return ticket{}, fmt.Errorf("%w: %w", ErrStore, err)
	}
	remade := false
	// Each round meets a claim that changed under it, such as a release or
	// an expiry. Three rounds lose only to a key that changes all the
	// time, and that key is in flight.
	for range 3 {
		kv, err := n.bucket(ctx, source)
		if err != nil {
			return ticket{}, fmt.Errorf("%w: %w", ErrStore, err)
		}
		rev, err := kv.Create(ctx, id, val)
		if err == nil {
			return ticket{source: source, key: key, id: id, rev: rev}, nil
		}
		if lost(err) && !remade {
			n.forget(source)
			remade = true
			continue
		}
		if !errors.Is(err, jetstream.ErrKeyExists) {
			return ticket{}, fmt.Errorf("%w: %w", ErrStore, err)
		}
		e, err := kv.Get(ctx, id)
		if errors.Is(err, jetstream.ErrKeyNotFound) || errors.Is(err, jetstream.ErrKeyDeleted) {
			continue
		}
		if err != nil {
			return ticket{}, fmt.Errorf("%w: %w", ErrStore, err)
		}
		var c claim
		if json.Unmarshal(e.Value(), &c) != nil || c.State != statePending {
			// A value that does not read is a claim of a newer version or a
			// damaged one: it counts as done, so it never makes a second
			// run.
			return ticket{}, ErrDuplicate
		}
		if n.now().Sub(e.Created()) < n.stale {
			return ticket{}, ErrInFlight
		}
		// The replica of the pending claim stopped before its end. The
		// next delivery takes the claim, and the compare of the revision
		// lets only one caller take it.
		rev, err = kv.Update(ctx, id, val, e.Revision())
		if err == nil {
			n.log.Warn("sources: took over a claim that stayed pending", "source", source, "by", c.By, "since", e.Created())
			return ticket{source: source, key: key, id: id, rev: rev}, nil
		}
		if !errors.Is(err, jetstream.ErrKeyRevisionMismatch) && !errors.Is(err, jetstream.ErrKeyExists) {
			return ticket{}, fmt.Errorf("%w: %w", ErrStore, err)
		}
	}
	return ticket{}, ErrInFlight
}

func (n *natsStore) Finish(ctx context.Context, t ticket) error {
	ctx, cancel := context.WithTimeout(ctx, storeTimeout)
	defer cancel()
	kv, err := n.bucket(ctx, t.source)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrStore, err)
	}
	val, err := json.Marshal(claim{State: stateDone, Key: t.key, By: n.by(), At: n.now().UTC()})
	if err != nil {
		return err
	}
	if _, err := kv.Update(ctx, t.id, val, t.rev); err != nil {
		return fmt.Errorf("%w: %w", ErrStore, err)
	}
	return nil
}

func (n *natsStore) Release(ctx context.Context, t ticket) error {
	ctx, cancel := context.WithTimeout(ctx, storeTimeout)
	defer cancel()
	kv, err := n.bucket(ctx, t.source)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrStore, err)
	}
	if err := kv.Delete(ctx, t.id, jetstream.LastRevision(t.rev)); err != nil {
		return fmt.Errorf("%w: %w", ErrStore, err)
	}
	return nil
}

func (n *natsStore) Close() error {
	n.nc.Close()
	return nil
}

// redact drops the URL from a parse error, because the URL can hold a
// password or a token.
func redact(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}
