// Package fleet is the fleet of the workbench (docs/ARCHITECTURE.md
// section 4): a set of fw processes that share one bus and one subject. It
// runs as a service of the kernel (fw.RegisterService), answers requests
// about the runs of other members through the relay of the native surface
// (native.Relay), and adds its gauges to the metrics surface
// (metrics.RegisterCollector). The kernel knows no fleet.
//
// Each process publishes a record of itself and of its runs to a retained
// subject on the bus, and a heartbeat when nothing changed. The view of the
// other members starts at the first request that needs it. It watches the
// records, decides what a caller sees from the rules that each owner
// published, and forwards a request about a run of another member to that
// member with the credential of the caller.
package fleet

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/surface/metrics"
	"github.com/bdubs00/featherweight-kernel/surface/native"
)

func init() {
	fw.RegisterService(Start)
	native.Extend(func(k *native.Kit) error {
		k.SetRelay(NewRelay(k.Agent()))
		return nil
	})
	metrics.RegisterCollector(NewCollector)
}

// Start is the service of the fleet (fw.Service). When the configuration
// sets a subject, it opens the fleet of the agent and attaches the View, so
// the surfaces and the relay find it through Of. Without a subject it does
// nothing.
func Start(ctx context.Context, a *fw.Agent) (io.Closer, error) {
	c, err := Load(a.Config())
	if err != nil {
		return nil, err
	}
	if c.Subject == "" {
		return nil, nil
	}
	f, err := Open(ctx, a, c)
	if err != nil {
		return nil, err
	}
	Attach(a, f)
	return service{a: a, f: f}, nil
}

// service stops the fleet of an agent and detaches its View.
type service struct {
	a *fw.Agent
	f *Fleet
}

func (s service) Close() error {
	Attach(s.a, nil)
	return s.f.Close()
}

// debounce is the time between a change of a run and the record that
// publishes it. A burst of events therefore gives one record.
const debounce = time.Second

// closeTimeout limits the last record at shutdown.
const closeTimeout = 5 * time.Second

// changes are the ledger events that change a run object in a record.
var changes = map[string]bool{
	fw.EvRunQueued:         true,
	fw.EvRunStarted:        true,
	fw.EvApprovalRequested: true,
	fw.EvApprovalDecided:   true,
	fw.EvTurnFinished:      true,
	fw.EvRunSuspended:      true,
	fw.EvRunResumed:        true,
	fw.EvRunFinished:       true,
	fw.EvMessageReceived:   true,
}

// Fleet is the default View. Build it with Open.
type Fleet struct {
	a        *fw.Agent
	log      *slog.Logger
	bus      fw.Bus
	ret      fw.Retainer // nil when the bus keeps no event
	name     string
	url      string
	subject  string
	atespace string
	interval time.Duration
	expire   time.Duration
	forward  []string
	router   string
	client   *http.Client

	ctx    context.Context // ends at Close; the watches live in it
	cancel context.CancelFunc

	// The publisher.
	dirty     chan struct{}
	stop      chan struct{}
	done      chan struct{}
	pubMu     sync.Mutex // one publish at a time
	last      []byte     // the content of the last record, see contentKey
	closeOnce sync.Once
	closed    atomic.Bool

	// The view.
	viewMu   sync.Mutex // guards viewOn and stops
	viewOn   bool
	stops    []func()
	markMu   sync.Mutex    // guards mark and markDone
	mark     time.Time     // the time of the heartbeat that settle waits for
	markDone chan struct{} // closed when that heartbeat arrives

	mu      sync.RWMutex
	records map[string]*Record   // the newest record of each other member
	alive   map[string]time.Time // the newest heartbeat of each other member
}

// Open builds the fleet of an agent from a configuration that Load filled,
// publishes the first record, and starts the publisher. A bus that does not
// answer at start is a warning, not a failure, so a member boots while the
// bus is down.
func Open(ctx context.Context, a *fw.Agent, fc Config) (*Fleet, error) {
	cfg := a.Config()
	subject := strings.TrimSpace(fc.Subject)
	if subject == "" {
		return nil, errors.New("fleet: workbench.fleet.subject is empty")
	}
	if a.Bus() == nil {
		return nil, errors.New("fleet: the agent has no bus")
	}
	f := &Fleet{
		a:        a,
		log:      a.Logger().With("component", "fleet"),
		bus:      a.Bus(),
		name:     fc.Name,
		url:      strings.TrimSpace(fc.URL),
		subject:  subject,
		atespace: atespace(),
		interval: fc.Interval,
		expire:   fc.Expire,
		forward:  append([]string(nil), fc.Forward...),
		router:   strings.TrimSpace(cfg.AX.Router),
		client:   newClient(),
		dirty:    make(chan struct{}, 1),
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
		records:  map[string]*Record{},
		alive:    map[string]time.Time{},
	}
	if f.interval <= 0 {
		f.interval = DefaultInterval
	}
	if f.expire <= 0 {
		f.expire = DefaultExpire
	}
	if r, ok := f.bus.(fw.Retainer); ok {
		f.ret = r
	}
	if _, ok := f.bus.(*fw.MemBus); ok {
		f.log.Warn("the fleet uses the mem bus, which reaches this process only; set bus.driver to nats or redis", "subject", subject)
	}
	f.ctx, f.cancel = context.WithCancel(context.Background())
	a.Observe(fw.ObserverFunc(f.observe))
	// The loop publishes the first record, so a bus that is not reachable
	// yet, as under ax before the sandbox opens its egress, does not hold
	// up the start.
	go f.loop()
	f.log.Info("fleet member started", "name", f.name, "subject", subject, "url", f.url, "retained", f.ret != nil)
	return f, nil
}

// atespace is the atespace of the process, as the kernel reads it.
func atespace() string {
	if v := os.Getenv("FW_ATESPACE"); v != "" {
		return v
	}
	return "local"
}

// Name implements View.
func (f *Fleet) Name() string { return f.name }

// observe marks the record dirty after an event that changes a run.
func (f *Fleet) observe(e fw.Event) {
	if !changes[e.Type] {
		return
	}
	select {
	case f.dirty <- struct{}{}:
	default:
	}
}

// loop publishes the first record, then each change at once, but at most
// once per debounce, and a record or a heartbeat at every interval. A new
// run therefore reaches the other members right away.
func (f *Fleet) loop() {
	defer close(f.done)
	f.publishLogged(publishChange)
	last := time.Now()
	tick := time.NewTicker(f.interval)
	defer tick.Stop()
	var timer *time.Timer
	var due <-chan time.Time
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	for {
		select {
		case <-f.stop:
			return
		case <-f.dirty:
			if timer != nil {
				break // a publish is due already
			}
			if wait := debounce - time.Since(last); wait > 0 {
				timer = time.NewTimer(wait)
				due = timer.C
				break
			}
			f.publishLogged(publishChange)
			last = time.Now()
		case <-due:
			timer, due = nil, nil
			f.publishLogged(publishChange)
			last = time.Now()
		case <-tick.C:
			f.publishLogged(publishTick)
		}
	}
}

// Kinds of a publish.
type publishKind int

const (
	publishChange  publishKind = iota // a run changed: the record when it differs
	publishTick                       // the interval: the record when it differs, else a heartbeat
	publishStopped                    // the last record at shutdown
)

func (f *Fleet) publishLogged(kind publishKind) {
	ctx, cancel := context.WithTimeout(f.ctx, f.interval)
	defer cancel()
	if err := f.publish(ctx, kind); err != nil {
		f.log.Warn("publish failed", "error", err)
	}
}

// publish builds a fresh record and sends it when its content changed, or
// for the last record. At the interval, an unchanged record gives a
// heartbeat. On a bus without Retainer, the interval sends the full record,
// because a later view has no other way to learn it.
func (f *Fleet) publish(ctx context.Context, kind publishKind) error {
	f.pubMu.Lock()
	defer f.pubMu.Unlock()
	rec, err := f.record()
	if err != nil {
		return err
	}
	rec.Stopped = kind == publishStopped
	key, err := contentKey(rec)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	if kind != publishStopped && bytes.Equal(key, f.last) {
		if kind != publishTick {
			return nil
		}
		if f.ret != nil {
			return f.send(ctx, AliveSubject(f.subject, f.name), EvAlive, Alive{Name: f.name, At: now})
		}
	}
	rec.UpdatedAt = now
	if err := f.send(ctx, RecordSubject(f.subject, f.name), EvRecord, rec); err != nil {
		return err
	}
	f.last = key
	return nil
}

// send publishes one event, retained when the bus keeps events.
func (f *Fleet) send(ctx context.Context, subject, typ string, data any) error {
	e, err := fw.NewEvent(f.name, typ, data)
	if err != nil {
		return fmt.Errorf("fleet: %w", err)
	}
	e.Source = "fw://" + f.atespace + "/fleet"
	if f.ret != nil {
		err = f.ret.Retain(ctx, subject, e)
	} else {
		err = f.bus.Publish(ctx, subject, e)
	}
	if err != nil {
		return fmt.Errorf("fleet: publish %s: %w", subject, err)
	}
	return nil
}

// contentKey is the JSON of a record without its time, so two records with
// the same facts compare equal.
func contentKey(rec Record) ([]byte, error) {
	rec.UpdatedAt = time.Time{}
	b, err := json.Marshal(rec)
	if err != nil {
		return nil, fmt.Errorf("fleet: encode the record: %w", err)
	}
	return b, nil
}

// Close stops the publisher, publishes the last record with Stopped set,
// and stops the view. A second call does nothing.
func (f *Fleet) Close() error {
	var err error
	f.closeOnce.Do(func() {
		f.closed.Store(true)
		close(f.stop)
		<-f.done
		ctx, cancel := context.WithTimeout(context.Background(), closeTimeout)
		err = f.publish(ctx, publishStopped)
		cancel()
		f.viewMu.Lock()
		for _, stop := range f.stops {
			stop()
		}
		f.stops = nil
		f.viewMu.Unlock()
		f.cancel()
		f.client.CloseIdleConnections()
	})
	return err
}
