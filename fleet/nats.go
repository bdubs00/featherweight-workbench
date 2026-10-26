package fleet

// This file reads the server of a NATS bus for the front: the consumers of
// the stream and the messages that it stores, for the depth of the intake
// queues and their dead letters (queues.go), and a key-value bucket that
// holds the job records, so every replica of the front counts the same
// jobs (jobs.go). The kernel driver of the bus hides its connection, so the
// front opens one connection of its own with the same keys of bus.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/pools"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// natsStream is the stream of the kernel driver when bus.options.stream
// names none.
const natsStream = "FW"

// JobsBucket is the key-value bucket of the job records on a NATS server.
const JobsBucket = "fw_workbench_jobs"

// openBusServer opens the reader of the server of the bus. It returns nil
// values for a bus that is not NATS. A test replaces it.
var openBusServer = func(ctx context.Context, cfg *fw.Config, expire time.Duration, log *slog.Logger) (queueSource, jobStore, func(), error) {
	if cfg == nil || cfg.Bus.Driver != "nats" {
		return nil, nil, nil, nil
	}
	n, err := openNATS(ctx, cfg, expire, log)
	if err != nil {
		return nil, nil, nil, err
	}
	return n, n, n.close, nil
}

// natsServer is one connection of the front to the NATS server of the bus.
type natsServer struct {
	nc     *nats.Conn
	js     jetstream.JetStream
	stream string
	ttl    time.Duration
	log    *slog.Logger

	mu sync.Mutex
	kv jetstream.KeyValue // made at the first use
}

// openNATS connects to the server of bus.url with the keys credentials and
// token of bus.options, as the kernel driver does. A server that is not
// reachable yet does not stop the start: the client keeps trying, and each
// read fails until it connects.
func openNATS(ctx context.Context, cfg *fw.Config, expire time.Duration, log *slog.Logger) (*natsServer, error) {
	opt := cfg.Bus.Options
	stream := strings.TrimSpace(opt["stream"])
	if stream == "" {
		stream = natsStream
	}
	nopts := []nats.Option{
		nats.Name("fw-workbench"),
		nats.MaxReconnects(-1),
		nats.RetryOnFailedConnect(true),
		nats.ReconnectWait(time.Second),
	}
	if p := strings.TrimSpace(opt["credentials"]); p != "" {
		nopts = append(nopts, nats.UserCredentials(p))
	}
	if name := strings.TrimSpace(opt["token"]); name != "" {
		tok, err := fw.EnvSecrets{}.Get(ctx, name)
		if err != nil {
			return nil, fmt.Errorf("fleet: the token of the bus: %w", err)
		}
		nopts = append(nopts, nats.Token(tok))
	}
	addr := cfg.Bus.URL
	if addr == "" {
		addr = nats.DefaultURL
	}
	nc, err := nats.Connect(addr, nopts...)
	if err != nil {
		return nil, fmt.Errorf("fleet: connect to the bus server: %w", redactURL(err))
	}
	js, err := jetstream.New(nc)
	if err != nil {
		nc.Close()
		return nil, fmt.Errorf("fleet: open JetStream on the bus server: %w", err)
	}
	if !nc.IsConnected() {
		log.Warn("fleet: the bus server is not reachable yet; the front reads the queues once it connects")
	}
	return &natsServer{nc: nc, js: js, stream: stream, ttl: expire, log: log}, nil
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

func (n *natsServer) close() { n.nc.Close() }

// snapshot implements queueSource with three requests: the stream, the
// messages that it stores for each subject under filter, and its
// consumers.
func (n *natsServer) snapshot(ctx context.Context, filter string) (queueSnapshot, error) {
	var out queueSnapshot
	s, err := n.js.Stream(ctx, n.stream)
	if err != nil {
		return out, fmt.Errorf("fleet: the stream %s: %w", n.stream, err)
	}
	info, err := s.Info(ctx, jetstream.WithSubjectFilter(filter))
	if err != nil {
		return out, fmt.Errorf("fleet: the subjects of the stream %s: %w", n.stream, err)
	}
	out.filters = info.Config.Subjects
	out.stored = info.State.Subjects
	list := s.ListConsumers(ctx)
	for ci := range list.Info() {
		c := consumerState{name: ci.Name, pending: ci.NumPending, ackPending: uint64(max(ci.NumAckPending, 0))}
		if ci.Config.FilterSubject != "" {
			c.filters = []string{ci.Config.FilterSubject}
		} else {
			c.filters = ci.Config.FilterSubjects
		}
		out.consumers = append(out.consumers, c)
	}
	if err := list.Err(); err != nil {
		return queueSnapshot{}, fmt.Errorf("fleet: the consumers of the stream %s: %w", n.stream, err)
	}
	return out, nil
}

// lastTime implements queueSource.
func (n *natsServer) lastTime(ctx context.Context, subject string) (time.Time, error) {
	s, err := n.js.Stream(ctx, n.stream)
	if err != nil {
		return time.Time{}, fmt.Errorf("fleet: the stream %s: %w", n.stream, err)
	}
	m, err := s.GetLastMsgForSubject(ctx, subject)
	if err != nil {
		return time.Time{}, fmt.Errorf("fleet: the last message on %s: %w", subject, err)
	}
	return m.Time, nil
}

// bucket returns the bucket of the job records, and makes it on the first
// use. A bucket that cannot be made is asked for again at the next use.
func (n *natsServer) bucket(ctx context.Context) (kvBucket, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.kv != nil {
		return n.kv, nil
	}
	kv, err := n.js.CreateOrUpdateKeyValue(ctx, jetstream.KeyValueConfig{
		Bucket:      JobsBucket,
		Description: "The jobs that the server of a featherweight fleet put on the intake queues, by batch and person.",
		TTL:         n.ttl,
		History:     1,
	})
	if err != nil {
		return nil, fmt.Errorf("fleet: the bucket %s: %w", JobsBucket, err)
	}
	n.kv = kv
	return kv, nil
}

// addJobs implements jobStore.
func (n *natsServer) addJobs(ctx context.Context, k jobKey, queue string, ids []string, now time.Time) error {
	b, err := n.bucket(ctx)
	if err != nil {
		return err
	}
	return kvAddJobs(ctx, b, k, queue, ids, now)
}

// jobs implements jobStore.
func (n *natsServer) jobs(ctx context.Context, k jobKey) (jobRecord, bool, error) {
	b, err := n.bucket(ctx)
	if err != nil {
		return jobRecord{}, false, err
	}
	return kvJobs(ctx, b, k)
}

// kvBucket is the part of a key-value bucket that the job records use.
type kvBucket interface {
	Get(ctx context.Context, key string) (jetstream.KeyValueEntry, error)
	Create(ctx context.Context, key string, value []byte, opts ...jetstream.KVCreateOpt) (uint64, error)
	Update(ctx context.Context, key string, value []byte, revision uint64) (uint64, error)
}

// kvAttempts bounds the tries of a write that another replica raced.
const kvAttempts = 5

// kvKey is the key of a job record: the batch and the person, each in
// base64url, because a key holds letters, digits, and a few marks only.
func kvKey(k jobKey) string {
	enc := base64.RawURLEncoding
	return "job." + enc.EncodeToString([]byte(k.batch)) + "." + enc.EncodeToString([]byte(k.subject))
}

// kvAddJobs adds the ids to the record of a job in the bucket. It reads the
// record and writes it back at the revision that it read, and tries again
// when another replica wrote in between.
func kvAddJobs(ctx context.Context, b kvBucket, k jobKey, queue string, ids []string, now time.Time) error {
	key := kvKey(k)
	for range kvAttempts {
		rec, rev, err := kvRead(ctx, b, key)
		if err != nil {
			return err
		}
		if !rec.add(queue, ids, now) && rev != 0 {
			return nil
		}
		data, err := json.Marshal(rec)
		if err != nil {
			return fmt.Errorf("fleet: encode a job record: %w", err)
		}
		if rev == 0 {
			_, err = b.Create(ctx, key, data)
		} else {
			_, err = b.Update(ctx, key, data, rev)
		}
		switch {
		case err == nil:
			return nil
		case errors.Is(err, jetstream.ErrKeyExists), errors.Is(err, jetstream.ErrKeyRevisionMismatch):
			continue // another replica wrote first
		default:
			return fmt.Errorf("fleet: write a job record: %w", err)
		}
	}
	return fmt.Errorf("fleet: write a job record: other writers won %d times", kvAttempts)
}

// kvJobs reads the record of a job from the bucket.
func kvJobs(ctx context.Context, b kvBucket, k jobKey) (jobRecord, bool, error) {
	rec, rev, err := kvRead(ctx, b, kvKey(k))
	if err != nil {
		return jobRecord{}, false, err
	}
	return rec, rev != 0, nil
}

// kvRead reads a record and its revision, zero when the key is not there.
func kvRead(ctx context.Context, b kvBucket, key string) (jobRecord, uint64, error) {
	var rec jobRecord
	e, err := b.Get(ctx, key)
	switch {
	case errors.Is(err, jetstream.ErrKeyNotFound):
		return rec, 0, nil
	case err != nil:
		return rec, 0, fmt.Errorf("fleet: read a job record: %w", err)
	}
	if err := json.Unmarshal(e.Value(), &rec); err != nil {
		return jobRecord{}, 0, fmt.Errorf("fleet: decode a job record: %w", err)
	}
	return rec, e.Revision(), nil
}

// poolBucketInfix comes between the stream name and the pool name in the
// name of the bucket of the overlay of a pool.
const poolBucketInfix = "_POOL_"

// PoolBucket returns the name of the key-value bucket that holds the
// overlay of a pool on the NATS server of the bus: <stream>_POOL_<pool>.
func PoolBucket(stream, pool string) string { return stream + poolBucketInfix + pool }

// Keys of the entries of an overlay bucket. Each profile and each rule is
// one entry, so two replicas that write two entries never race; a second
// write of one entry replaces it. The name after the prefix is base64url,
// because a key holds letters, digits, and a few marks only.
const (
	overlayProfileKey = "profile."
	overlayRuleKey    = "allow."
)

// overlayPackagesKey is the one entry of the packages of a pool. Its
// writes compare the revision, because a change of a package reads the
// entry first (UpdatePackages).
const overlayPackagesKey = "packages"

// overlayHistory is how many values of each entry a bucket keeps, so an
// operator can read what an entry held before.
const overlayHistory = 10

// natsOverlays keeps the overlays of the pools in the key-value buckets of
// the NATS server (pools.OverlayStore). A read never makes a bucket, and
// the first write to a pool makes its bucket.
type natsOverlays struct {
	n *natsServer

	mu sync.Mutex
	kv map[string]jetstream.KeyValue
}

func newNATSOverlays(n *natsServer) *natsOverlays {
	return &natsOverlays{n: n, kv: map[string]jetstream.KeyValue{}}
}

func (o *natsOverlays) Describe() string {
	return "from the key-value bucket " + PoolBucket(o.n.stream, "<pool>")
}

// bucket returns the bucket of a pool. Without create, a bucket that does
// not exist is jetstream.ErrBucketNotFound.
func (o *natsOverlays) bucket(ctx context.Context, pool string, create bool) (jetstream.KeyValue, error) {
	o.mu.Lock()
	kv := o.kv[pool]
	o.mu.Unlock()
	if kv != nil {
		return kv, nil
	}
	name := PoolBucket(o.n.stream, pool)
	var err error
	if create {
		kv, err = o.n.js.CreateOrUpdateKeyValue(ctx, jetstream.KeyValueConfig{
			Bucket:      name,
			Description: "The profiles and the allow rules that the server of a featherweight fleet adds to the pool " + pool + ".",
			History:     overlayHistory,
			Storage:     jetstream.FileStorage,
		})
	} else {
		kv, err = o.n.js.KeyValue(ctx, name)
	}
	if err != nil {
		return nil, err
	}
	o.mu.Lock()
	o.kv[pool] = kv
	o.mu.Unlock()
	return kv, nil
}

// forget drops the handle of a bucket, so the next request opens it again,
// as after a restart of a server without storage.
func (o *natsOverlays) forget(pool string) {
	o.mu.Lock()
	delete(o.kv, pool)
	o.mu.Unlock()
}

// bucketGone reports whether an error means that the bucket is gone.
func bucketGone(err error) bool {
	return errors.Is(err, jetstream.ErrBucketNotFound) || errors.Is(err, jetstream.ErrStreamNotFound)
}

// Read implements pools.OverlayStore. A pool without a bucket has an empty
// overlay. An entry that does not decode is left out, and the log names
// its key.
func (o *natsOverlays) Read(ctx context.Context, pool string) (pools.Overlay, error) {
	var out pools.Overlay
	kv, err := o.bucket(ctx, pool, false)
	if bucketGone(err) {
		o.forget(pool)
		return out, nil
	}
	if err != nil {
		return out, err
	}
	w, err := kv.WatchAll(ctx, jetstream.IgnoreDeletes())
	if bucketGone(err) {
		o.forget(pool)
		return out, nil
	}
	if err != nil {
		return out, err
	}
	defer func() { _ = w.Stop() }()
	for {
		var e jetstream.KeyValueEntry
		select {
		case <-ctx.Done():
			return pools.Overlay{}, ctx.Err()
		case e = <-w.Updates():
		}
		if e == nil {
			break // the end of the entries that the bucket holds now
		}
		var bad error
		switch {
		case strings.HasPrefix(e.Key(), overlayProfileKey):
			var v pools.OverlayProfile
			if bad = json.Unmarshal(e.Value(), &v); bad == nil {
				out.Profiles = append(out.Profiles, v)
			}
		case strings.HasPrefix(e.Key(), overlayRuleKey):
			var v pools.OverlayRule
			if bad = json.Unmarshal(e.Value(), &v); bad == nil {
				out.Allow = append(out.Allow, v)
			}
		case e.Key() == overlayPackagesKey:
			var v pools.PackageSetting
			if bad = json.Unmarshal(e.Value(), &v); bad == nil {
				out.Packages = &v
			}
		}
		if bad != nil {
			o.n.log.Warn("fleet: an entry of the overlay of a pool does not decode", "pool", pool, "key", e.Key(), "error", bad)
		}
	}
	// The sort of the store in memory is the sort of the overlay.
	sorted := pools.Overlay{Packages: out.Packages}
	for _, e := range out.Profiles {
		sorted = sorted.WithProfile(e)
	}
	for _, r := range out.Allow {
		sorted = sorted.WithRule(r)
	}
	return sorted, nil
}

// put writes one entry, and makes the bucket again once when it is gone.
func (o *natsOverlays) put(ctx context.Context, pool, key string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("fleet: encode an entry of the overlay: %w", err)
	}
	for try := 0; ; try++ {
		kv, err := o.bucket(ctx, pool, true)
		if err == nil {
			_, err = kv.Put(ctx, key, data)
		}
		if bucketGone(err) && try == 0 {
			o.forget(pool)
			continue
		}
		return err
	}
}

// PutProfile implements pools.OverlayStore.
func (o *natsOverlays) PutProfile(ctx context.Context, pool string, e pools.OverlayProfile) error {
	return o.put(ctx, pool, overlayProfileKey+base64.RawURLEncoding.EncodeToString([]byte(e.Profile.Name)), e)
}

// AddRule implements pools.OverlayStore.
func (o *natsOverlays) AddRule(ctx context.Context, pool string, r pools.OverlayRule) error {
	return o.put(ctx, pool, overlayRuleKey+base64.RawURLEncoding.EncodeToString([]byte(r.Rule)), r)
}

// overlayAttempts bounds the tries of a write of the packages that other
// writers race.
const overlayAttempts = 8

// UpdatePackages implements pools.OverlayStore. It reads the entry and
// its revision, and writes the result of f at that revision, so a write
// of another replica in between makes it read again. A read alone makes
// no bucket.
func (o *natsOverlays) UpdatePackages(ctx context.Context, pool string, f func(*pools.PackageSetting) (*pools.PackageSetting, error)) (*pools.PackageSetting, error) {
	for range overlayAttempts {
		var cur *pools.PackageSetting
		var rev uint64
		kv, err := o.bucket(ctx, pool, false)
		switch {
		case bucketGone(err):
			o.forget(pool)
			kv = nil
		case err != nil:
			return nil, err
		}
		if kv != nil {
			e, err := kv.Get(ctx, overlayPackagesKey)
			switch {
			case errors.Is(err, jetstream.ErrKeyNotFound), errors.Is(err, jetstream.ErrKeyDeleted):
			case bucketGone(err):
				o.forget(pool)
				kv = nil
			case err != nil:
				return nil, err
			default:
				var v pools.PackageSetting
				if err := json.Unmarshal(e.Value(), &v); err != nil {
					return nil, fmt.Errorf("fleet: decode the packages of the pool %s: %w", pool, err)
				}
				cur, rev = &v, e.Revision()
			}
		}
		var arg *pools.PackageSetting
		if cur != nil {
			c := cur.Clone()
			arg = &c
		}
		next, err := f(arg)
		if err != nil {
			return nil, err
		}
		if next == nil {
			return cur, nil
		}
		data, err := json.Marshal(next)
		if err != nil {
			return nil, fmt.Errorf("fleet: encode the packages of the pool %s: %w", pool, err)
		}
		if kv == nil {
			if kv, err = o.bucket(ctx, pool, true); err != nil {
				return nil, err
			}
		}
		if rev == 0 {
			_, err = kv.Create(ctx, overlayPackagesKey, data)
		} else {
			_, err = kv.Update(ctx, overlayPackagesKey, data, rev)
		}
		switch {
		case err == nil:
			c := next.Clone()
			return &c, nil
		case errors.Is(err, jetstream.ErrKeyExists), errors.Is(err, jetstream.ErrKeyRevisionMismatch):
			continue // another replica wrote first
		case bucketGone(err):
			o.forget(pool)
			continue
		default:
			return nil, err
		}
	}
	return nil, fmt.Errorf("fleet: write the packages of the pool %s: other writers won %d times; try again", pool, overlayAttempts)
}

// DeleteProfile implements pools.OverlayStore.
func (o *natsOverlays) DeleteProfile(ctx context.Context, pool, name string) (bool, error) {
	kv, err := o.bucket(ctx, pool, false)
	if bucketGone(err) {
		o.forget(pool)
		return false, nil
	}
	if err != nil {
		return false, err
	}
	key := overlayProfileKey + base64.RawURLEncoding.EncodeToString([]byte(name))
	if _, err := kv.Get(ctx, key); errors.Is(err, jetstream.ErrKeyNotFound) || errors.Is(err, jetstream.ErrKeyDeleted) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	if err := kv.Delete(ctx, key); err != nil {
		return false, err
	}
	return true, nil
}
