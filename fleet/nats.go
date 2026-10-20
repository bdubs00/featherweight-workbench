package fleet

// This file reads the server of a NATS bus for the front: the consumers of
// the stream and the messages that it stores, for the depth of the intake
// queues and their dead letters (queues.go). The kernel driver of the bus
// hides its connection, so the front opens one connection of its own with
// the same keys of bus.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// natsStream is the stream of the kernel driver when bus.options.stream
// names none.
const natsStream = "FW"

// openBusServer opens the reader of the server of the bus. It returns nil
// values for a bus that is not NATS. A test replaces it.
var openBusServer = func(ctx context.Context, cfg *fw.Config, log *slog.Logger) (queueSource, func(), error) {
	if cfg == nil || cfg.Bus.Driver != "nats" {
		return nil, nil, nil
	}
	n, err := openNATS(ctx, cfg, log)
	if err != nil {
		return nil, nil, err
	}
	return n, n.close, nil
}

// natsServer is one connection of the front to the NATS server of the bus.
type natsServer struct {
	nc     *nats.Conn
	js     jetstream.JetStream
	stream string
}

// openNATS connects to the server of bus.url with the keys credentials and
// token of bus.options, as the kernel driver does. A server that is not
// reachable yet does not stop the start: the client keeps trying, and each
// read fails until it connects.
func openNATS(ctx context.Context, cfg *fw.Config, log *slog.Logger) (*natsServer, error) {
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
	return &natsServer{nc: nc, js: js, stream: stream}, nil
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
