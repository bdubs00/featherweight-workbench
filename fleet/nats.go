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
	return &natsServer{nc: nc, js: js, stream: stream, ttl: expire}, nil
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
		Description: "The jobs that the front of a featherweight fleet put on the intake queues, by batch and person.",
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
