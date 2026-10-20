// Package fleet is the fleet of the workbench (docs/ARCHITECTURE.md
// section 4): one view of the workers of a fleet, read from the platforms.
// The ax API gives the Tasks, the egress stream of the workers gives the
// state of every run, and the front projects both into one view with the
// projection of the kernel. The package runs as a service of the kernel
// (fw.RegisterService), answers requests about the runs of other members
// through the relay of the native surface (native.Relay), forwards an
// action to the Task that owns the run through the atenet router
// (forward.go), puts the pieces of a job on the intake queues of the
// workers (enqueue.go), suspends the Tasks that idle (suspend.go), and
// keeps the Tasks of the pools of the configuration (pools.go). The
// kernel knows no fleet, and a worker publishes nothing about itself: it
// is a bare kernel, and the platforms keep the state.
package fleet

import (
	"context"
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
	"github.com/bdubs00/featherweight-kernel/surface/native"
	"github.com/bdubs00/featherweight-workbench/pools"
)

func init() {
	fw.RegisterService(Start)
	native.Extend(func(k *native.Kit) error {
		k.SetRelay(NewRelay(k.Agent()))
		return nil
	})
}

// Start is the service of the fleet (fw.Service). When the configuration
// names a source, the atespaces of the Tasks or the egress stream of the
// workers, it opens the fleet of the agent and attaches the View, so the
// surfaces and the relay find it through Of. Without a source it does
// nothing.
func Start(ctx context.Context, a *fw.Agent) (io.Closer, error) {
	c, err := Load(a.Config())
	if err != nil {
		return nil, err
	}
	if c.Pools, err = LoadPools(a.Config(), c); err != nil {
		return nil, fmt.Errorf("fleet: %w", err)
	}
	if !c.On() {
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

// Fleet is the default View. Build it with Open.
type Fleet struct {
	a        *fw.Agent
	log      *slog.Logger
	bus      fw.Bus // nil when the configuration names no egress stream and the agent has no bus
	name     string
	atespace string
	interval time.Duration
	expire   time.Duration
	// suspendAfter is the grace period of the suspend controller, zero
	// when it is off (suspend.go).
	suspendAfter time.Duration
	router       string
	client       *http.Client

	ctx    context.Context // ends at Close; the sources live in it
	cancel context.CancelFunc

	closeOnce sync.Once
	closed    atomic.Bool
	viewMu    sync.Mutex // guards stops
	stops     []func()

	// The members that the platforms know (platform.go).
	platform *platform
}

// Open builds the fleet of an agent from a configuration that Load filled
// and starts its sources: the Task list of the atespaces, read from the ax
// API at every interval, and the egress stream of the workers. A bus that
// is not reachable yet does not hold up the start.
func Open(ctx context.Context, a *fw.Agent, fc Config) (*Fleet, error) {
	cfg := a.Config()
	if !fc.On() {
		return nil, errors.New("fleet: workbench.fleet.atespaces or workbench.fleet.egress must name a source")
	}
	egress := strings.TrimSpace(fc.Egress)
	if egress != "" && a.Bus() == nil {
		return nil, errors.New("fleet: workbench.fleet.egress needs a bus, and the agent has none")
	}
	f := &Fleet{
		a:        a,
		log:      a.Logger().With("component", "fleet"),
		bus:      a.Bus(),
		name:     fc.Name,
		atespace: atespace(),
		interval: fc.Interval,
		expire:   fc.Expire,
		router:   strings.TrimSpace(cfg.AX.Router),
		client:   newClient(),
	}
	if f.interval <= 0 {
		f.interval = DefaultInterval
	}
	if f.expire <= 0 {
		f.expire = DefaultExpire
	}
	f.suspendAfter = fc.suspendAfter()
	if _, ok := f.bus.(*fw.MemBus); ok && egress != "" {
		f.log.Warn("the fleet reads the egress stream on the mem bus, which reaches this process only; set bus.driver to nats or redis", "egress", egress)
	}
	f.ctx, f.cancel = context.WithCancel(context.Background())
	tokenEnv := strings.TrimSpace(cfg.Auth.TokenEnv)
	if tokenEnv == "" {
		tokenEnv = "FW_API_TOKEN"
	}
	var vals *pools.Values
	if len(fc.Pools) > 0 {
		if len(fc.Atespaces) == 0 {
			f.cancel()
			return nil, errors.New("fleet: the pools need workbench.fleet.atespaces, the atespaces whose Tasks the front lists")
		}
		// The front reads the variables that the Tasks of the pools copy
		// once, and a missing one stops the start, not the first create.
		v, err := pools.Resolve(fc.Pools, nil)
		if err != nil {
			f.cancel()
			return nil, fmt.Errorf("fleet: %w", err)
		}
		vals = v
	}
	if err := f.startPlatform(fc.Atespaces, egress, cfg.AX.Server, tokenEnv, fc.Pools, vals); err != nil {
		f.cancel()
		return nil, err
	}
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

// Close stops the sources. A second call does nothing.
func (f *Fleet) Close() error {
	f.closeOnce.Do(func() {
		f.closed.Store(true)
		f.viewMu.Lock()
		for _, stop := range f.stops {
			stop()
		}
		f.stops = nil
		f.closePlatform()
		f.viewMu.Unlock()
		f.cancel()
		f.client.CloseIdleConnections()
	})
	return nil
}
