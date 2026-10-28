package sources

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"gopkg.in/yaml.v3"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/surface/native"
	"github.com/bdubs00/featherweight-workbench/fleet"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// textModel ends every turn with the same text.
func textModel(out string) fw.Model {
	return fw.ModelFunc{StreamFn: func(context.Context, *fw.Request, func(fw.Delta)) (*fw.Response, error) {
		return &fw.Response{Content: []fw.Block{fw.Text(out)}, StopReason: fw.StopEndTurn}, nil
	}}
}

// config is a configuration whose workbench block is the YAML text, with
// the intake queues at fw.jobs.
func config(t *testing.T, block string) *fw.Config {
	t.Helper()
	t.Setenv(fleet.EnvIntake, "fw.jobs")
	cfg := fw.DefaultConfig()
	cfg.Home = t.TempDir()
	cfg.Agents = map[string]fw.AgentProfile{"default": {}}
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(block), &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Content) > 0 {
		cfg.Extensions = map[string]yaml.Node{fleet.Block: *doc.Content[0]}
	}
	return cfg
}

// secrets holds the variables name, value, name, value.
func secrets(kv ...string) fw.Secrets {
	m := map[string]string{}
	for i := 0; i+1 < len(kv); i += 2 {
		m[kv[i]] = kv[i+1]
	}
	return fw.SecretsFunc(func(_ context.Context, name string) (string, error) {
		if v, ok := m[name]; ok {
			return v, nil
		}
		return "", fw.ErrSecretNotFound
	})
}

// logBuffer collects the log lines of a front.
type logBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *logBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// front is a front with the sources of its configuration, served over
// HTTP.
type front struct {
	a    *fw.Agent
	s    *Sources
	url  string
	logs *logBuffer
}

// newFront starts a front on the bus with the workbench block, the
// secrets, and the service of the sources. mut changes the configuration
// first.
func newFront(t *testing.T, bus fw.Bus, block string, sec fw.Secrets, mut func(*fw.Config)) *front {
	t.Helper()
	f, err := openFront(t, bus, block, sec, mut)
	if err != nil {
		t.Fatalf("fw.New: %v", err)
	}
	return f
}

// openFront is newFront that returns the error of the start. The services
// of before start ahead of the sources, as the fleet does.
func openFront(t *testing.T, bus fw.Bus, block string, sec fw.Secrets, mut func(*fw.Config), before ...fw.Service) (*front, error) {
	t.Helper()
	cfg := config(t, block)
	if mut != nil {
		mut(cfg)
	}
	logs := &logBuffer{}
	a, err := fw.New(fw.WithConfig(cfg), fw.WithoutDefaults(), fw.WithAuthn(fw.NoAuthn{}), fw.WithModel(textModel("done")),
		fw.WithLedger(fw.NewMemLedger()), fw.WithBus(bus), fw.WithSecrets(sec),
		fw.WithLogger(slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))),
		fw.WithSurfaces(native.NewWith(extension)), fw.WithServices(append(before, Start)...))
	if err != nil {
		return nil, err
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		a.Close(ctx)
	})
	h, err := a.Handler()
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return &front{a: a, s: of(a), url: srv.URL, logs: logs}, nil
}

// newWorker is a bare kernel that takes the queue intake from the bus.
func newWorker(t *testing.T, bus fw.Bus, intake string) *fw.Agent {
	t.Helper()
	cfg := fw.DefaultConfig()
	cfg.Home = t.TempDir()
	cfg.Bus.Intake = intake
	cfg.Agents = map[string]fw.AgentProfile{"default": {}}
	a, err := fw.New(fw.WithConfig(cfg), fw.WithoutDefaults(), fw.WithAuthn(fw.NoAuthn{}), fw.WithModel(textModel("done")),
		fw.WithLedger(fw.NewMemLedger()), fw.WithBus(bus), fw.WithLogger(quiet()))
	if err != nil {
		t.Fatalf("fw.New: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		a.Close(ctx)
	})
	return a
}

// runsOf lists the runs of a worker.
func runsOf(t *testing.T, a *fw.Agent) []*fw.Run {
	t.Helper()
	runs, err := a.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return runs
}

// post sends a body to a source with the headers name, value, ....
func post(t *testing.T, url, body string, header ...string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, string(b)
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out: %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// statOf returns the count of one source of a front.
func statOf(t *testing.T, a *fw.Agent, name string) Stat {
	t.Helper()
	for _, st := range Stats(a) {
		if st.Source == name {
			return st
		}
	}
	t.Fatalf("no stat for %s in %+v", name, Stats(a))
	return Stat{}
}

// runNATS starts a NATS server with JetStream in the process.
func runNATS(t *testing.T) *server.Server {
	t.Helper()
	s, err := server.NewServer(&server.Options{Host: "127.0.0.1", Port: -1, JetStream: true, StoreDir: t.TempDir(), NoLog: true, NoSigs: true})
	if err != nil {
		t.Fatal(err)
	}
	go s.Start()
	if !s.ReadyForConnections(10 * time.Second) {
		t.Fatal("the NATS server did not start")
	}
	t.Cleanup(func() {
		s.Shutdown()
		s.WaitForShutdown()
	})
	return s
}

// countBus counts the publishes on the intake queues, and fails them while
// fail is set.
type countBus struct {
	fw.Bus
	sent atomic.Int32
	fail atomic.Bool
}

func (b *countBus) Publish(ctx context.Context, subject string, e fw.Event) error {
	if strings.HasPrefix(subject, "fw.jobs.") {
		if b.fail.Load() {
			return errBusDown
		}
		b.sent.Add(1)
	}
	return b.Bus.Publish(ctx, subject, e)
}

var errBusDown = &busError{}

type busError struct{}

func (*busError) Error() string { return "the bus is down" }

// fakeStore is a store whose Take always fails with err.
type fakeStore struct{ err error }

func (f fakeStore) Take(context.Context, string, string) (ticket, error) { return ticket{}, f.err }
func (f fakeStore) Finish(context.Context, ticket) error                 { return nil }
func (f fakeStore) Release(context.Context, ticket) error                { return nil }
func (f fakeStore) Close() error                                         { return nil }
func (f fakeStore) Describe() string                                     { return "fake" }

// swapStore replaces the store of a front.
func swapStore(f *front, st store) { f.s.store = st }

// placeView is a view of the fleet without members that places the jobs of
// an agent as a fleet with pools does (fleet.QueueView). It holds the
// intake of a pool by "<agent>@<pool>", and by "<agent>@" for the pool
// that runs the agent.
type placeView map[string]string

func (v placeView) Name() string                                    { return "front" }
func (v placeView) Members(context.Context) ([]fleet.Member, error) { return nil, nil }
func (v placeView) Runs(context.Context) ([]*fw.Run, error)         { return nil, nil }
func (v placeView) Run(context.Context, string) (*fw.Run, error) {
	return nil, fw.ErrRunNotFound
}

func (v placeView) Forward(context.Context, string, *http.Request) (*http.Response, error) {
	return nil, fleet.ErrNoRoute
}

func (v placeView) PoolQueue(_ context.Context, agent, pool string) (string, bool, error) {
	queue, ok := v[agent+"@"+pool]
	if !ok && pool != "" {
		return "", false, fmt.Errorf("%w: the pool %s does not run the agent %s", fw.ErrInvalidRequest, pool, agent)
	}
	return queue, ok, nil
}

// attachView is a service that attaches the view v to the agent before
// the sources start, as the fleet does.
func attachView(v fleet.View) fw.Service {
	return func(_ context.Context, a *fw.Agent) (io.Closer, error) {
		fleet.Attach(a, v)
		return detach{a}, nil
	}
}

// detach detaches the view of an agent when the agent closes.
type detach struct{ a *fw.Agent }

func (d detach) Close() error {
	fleet.Attach(d.a, nil)
	return nil
}

// blockOf returns the text of the data block of the source in a request,
// or "".
func blockOf(req fw.RunRequest, source string) string {
	for _, b := range req.Context {
		if b.Source == source {
			return b.Text
		}
	}
	return ""
}
