// Package notify sends the notifications of the duties by ntfy, per person
// (docs/ARCHITECTURE.md section 23). Each person picks a server and a
// topic, the kinds that reach them, and quiet hours. The front tells a
// person of a question of a run of a duty, of a failed run, of a duty
// that paused itself, of a cap that a duty reached, of a decision while
// the person was away, and once a day of what the duties did.
//
// The package runs as a service of the kernel (fw.RegisterService). On a
// NATS bus the settings live in the key-value bucket <stream>_NOTIFY, so
// every replica of the front reads one truth. On the bus mem they live in
// the memory of the process. The notifications wait in the same bucket
// until a replica sends them, so a restart loses none, and a replica
// claims each one before it sends it, so only one replica sends it. The
// package gets the notices of the duties through duties.OnNotice. It
// mounts GET and PUT /v1/notifications and POST /v1/notifications/test on
// the native surface and adds the MCP tools notifications_get,
// notifications_put, and notification_test, which call the same Go
// functions.
//
// The front never picks a public server by itself. Without a server of
// the person and without workbench.notifications.ntfy.server, the
// notifications of that person are off, and a read says why.
package notify

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/surface/native"
	"github.com/bdubs00/featherweight-workbench/duties"
	"github.com/bdubs00/featherweight-workbench/fleet"
	"github.com/bdubs00/featherweight-workbench/internal/bucket"
	"github.com/bdubs00/featherweight-workbench/surface/mcp"
)

func init() {
	fw.RegisterService(Start)
	native.Extend(mount)
	mcp.RegisterTools(tools)
	duties.OnNotice(onNotice)
}

// notifyBucket is the bucket of the settings and of the outbox.
var notifyBucket = bucket.Config{
	Name:        "NOTIFY",
	Description: "The notifications of a featherweight fleet: the settings of each person under person.<subject>, the notifications that wait under out.<id>, and the day of the last digest of each person under digest.<subject>.",
	History:     1,
	MaxValue:    64 << 10,
}

// defaultEvery is the longest pause of the loop of the outbox.
const defaultEvery = 5 * time.Second

// services ties the notifications of an agent to the agent.
var services sync.Map // *fw.Agent to *Service

// Of returns the notifications of an agent, or nil when the process keeps
// none.
func Of(a *fw.Agent) *Service {
	if a == nil {
		return nil
	}
	if v, ok := services.Load(a); ok {
		return v.(*Service)
	}
	return nil
}

// attach makes s the notifications of a. A nil s detaches.
func attach(a *fw.Agent, s *Service) {
	if s == nil {
		services.Delete(a)
		return
	}
	services.Store(a, s)
}

// Service is the notifications of one agent: the bucket, the rules of the
// operator, and the loop that sends the outbox and the digests.
type Service struct {
	a       *fw.Agent
	b       bucket.Bucket
	cfg     Config
	log     *slog.Logger
	now     func() time.Time
	replica string
	every   time.Duration

	listed, public *http.Client
	// postFn replaces the post to ntfy in a test.
	postFn func(ctx context.Context, st Settings, m *message) error

	mu         sync.Mutex
	lastDigest time.Time
	closed     bool // Close began, so no notice starts more work

	poke      chan struct{}
	work      sync.WaitGroup
	cancel    context.CancelFunc
	done      chan struct{}
	closers   []func()
	closeOnce sync.Once
}

// Start is the service of the notifications (fw.Service). It opens the
// bucket on the bus of the configuration and starts the loop. On a bus
// that keeps no store it logs once that the notifications are off and
// returns nil. A configuration that does not fit stops the start.
func Start(ctx context.Context, a *fw.Agent) (io.Closer, error) {
	log := a.Logger().With("component", "notify")
	cfg, err := LoadConfig(a.Config())
	if err != nil {
		return nil, err
	}
	b, closeFn, err := bucket.Open(ctx, a.Config(), notifyBucket, log)
	if errors.Is(err, bucket.ErrNoStore) {
		where, _ := bucket.Where(a.Config(), notifyBucket)
		log.Warn("notify: the notifications are off; they live "+where, "bus", a.Config().Bus.Driver)
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("notify: %w", err)
	}
	s := newService(a, b, cfg, log)
	if closeFn != nil {
		s.closers = append(s.closers, closeFn)
	}
	attach(a, s)
	s.start()
	where, _ := bucket.Where(a.Config(), notifyBucket)
	server := "none, so a person names one"
	if cfg.Ntfy.Server != "" {
		server = hostOf(cfg.Ntfy.Server)
	}
	log.Info("notify: the notifications are on", "store", where, "server", server)
	return s, nil
}

// newService builds the notifications of an agent on a bucket.
func newService(a *fw.Agent, b bucket.Bucket, cfg Config, log *slog.Logger) *Service {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	s := &Service{a: a, b: b, cfg: cfg, log: log, now: time.Now, replica: replicaOf(a), every: defaultEvery, poke: make(chan struct{}, 1)}
	s.listed, s.public = cfg.clients()
	return s
}

// replicaOf names this process in a claim: its name in the fleet, else
// the host name.
func replicaOf(a *fw.Agent) string {
	if v := fleet.Of(a); v != nil {
		return v.Name()
	}
	host, _ := os.Hostname()
	return host
}

// start starts the loop.
func (s *Service) start() {
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel, s.done = cancel, make(chan struct{})
	go s.loop(ctx)
}

// Close stops the loop and detaches the notifications. The notifications
// that wait stay in the bucket for the next replica.
func (s *Service) Close() error {
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.closed = true
		s.mu.Unlock()
		if s.cancel != nil {
			s.cancel()
			<-s.done
		}
		s.work.Wait()
		if Of(s.a) == s {
			attach(s.a, nil)
		}
		for _, f := range s.closers {
			f()
		}
	})
	return nil
}

// loop sends the outbox and the digests until Close.
func (s *Service) loop(ctx context.Context) {
	defer close(s.done)
	for {
		next := s.pass(ctx)
		wait := s.every
		if !next.IsZero() {
			wait = min(max(next.Sub(s.now()), 50*time.Millisecond), s.every)
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-s.poke:
			t.Stop()
		case <-t.C:
		}
	}
}

// pass sends the notifications that are due and the digests, and returns
// when the next notification that waits goes.
func (s *Service) pass(ctx context.Context) time.Time {
	s.digests(ctx)
	return s.sendDue(ctx)
}

// wake asks for a pass now.
func (s *Service) wake() {
	select {
	case s.poke <- struct{}{}:
	default:
	}
}

// onNotice takes a notice of the duties. It runs on the goroutine of the
// engine, so the work goes on a goroutine of its own.
func onNotice(_ context.Context, a *fw.Agent, n duties.Notice) {
	s := Of(a)
	if s == nil || len(n.Who) == 0 {
		return
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.work.Add(1)
	s.mu.Unlock()
	go func() {
		defer s.work.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		s.accept(ctx, n)
	}()
}
