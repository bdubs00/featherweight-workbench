// Package fleet is the fleet of the workbench (docs/ARCHITECTURE.md
// section 4): one view of the workers of a fleet, read from the platforms.
// The ax API gives the Tasks, the egress stream of the workers gives the
// state of every run, and the front projects both into one view with the
// projection of the kernel. The package runs as a service of the kernel
// (fw.RegisterService), answers requests about the runs of other members
// through the relay of the native surface (native.Relay), forwards an
// action to the Task that owns the run through the atenet router
// (forward.go), puts the pieces of a job on the intake queues of the
// workers (enqueue.go), reads the depth of those queues and wakes the Tasks
// for the jobs that wait (queues.go), suspends the Tasks that idle
// (suspend.go), keeps the Tasks of the pools of the configuration
// (pools.go) and of the runtime pools that people create from a template
// (runtime.go), keeps an index of the runs in a bucket of the bus so a
// restart forgets no run of a sleeping Task (index.go), adds its series to
// the metrics (metrics.go), and sends its changes to the packages that
// watch it (watch.go). The
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
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/surface/native"
	"github.com/bdubs00/featherweight-workbench/internal/bucket"
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
	if c.Pools, c.Templates, err = LoadAll(a.Config(), c); err != nil {
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
	// when it is off (suspend.go), and orphansAfter how long an orphan
	// Task sleeps before the front removes it, zero when the sweep is off
	// (orphans.go).
	suspendAfter time.Duration
	orphansAfter time.Duration
	router       string
	client       *http.Client
	// intake is the prefix of the intake queues, and wakeOnQueue turns
	// the wake on queue depth on (queues.go).
	intake      string
	wakeOnQueue bool

	ctx    context.Context // ends at Close; the sources live in it
	cancel context.CancelFunc

	closeOnce sync.Once
	closed    atomic.Bool
	viewMu    sync.Mutex // guards stops
	stops     []func()

	// The members that the platforms know (platform.go), and the
	// consumers of their changes (watch.go).
	platform *platform
	watch    watchers

	// The intake queues (queues.go), the job records (jobs.go), and the
	// end of the connection to the server of the bus (nats.go).
	queues      *queues
	jobs        *jobBook
	closeServer func()

	// The routines of the last pass (routines.go).
	routines routineCache

	// The store of the overlays of the pools (profiles.go), nil when the
	// pools run their configuration alone, and its end.
	overlays      pools.OverlayStore
	closeOverlays func()

	// The worker templates, and the store of the runtime pools
	// (runtime.go), nil without templates or on a bus that keeps no store,
	// and its end.
	templates    []*pools.Template
	runtime      *pools.Store
	closeRuntime func()

	// The bucket of the index of the runs (index.go), nil on a bus that
	// keeps no store or when it did not open, and its end.
	indexBucket bucket.Bucket
	closeIndex  func()

	// The package mirror of the front, nil when the packages are off, the
	// bucket of the states of the environments of the workers, and its
	// end (packages.go).
	mirror    *pools.PackageMirror
	envStates bucket.Bucket
	closeEnv  func()
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
	f.orphansAfter = fc.orphansAfter()
	f.intake = strings.TrimSpace(fc.Intake)
	f.wakeOnQueue = fc.wakeOnQueue() && f.intake != "" && len(fc.Atespaces) > 0
	if _, ok := f.bus.(*fw.MemBus); ok && egress != "" {
		f.log.Warn("the fleet reads the egress stream on the mem bus, which reaches this process only; set bus.driver to nats or redis", "egress", egress)
	}
	f.ctx, f.cancel = context.WithCancel(context.Background())
	tokenEnv := strings.TrimSpace(cfg.Auth.TokenEnv)
	if tokenEnv == "" {
		tokenEnv = "FW_API_TOKEN"
	}
	var vals *pools.Values
	if len(fc.Pools) > 0 || len(fc.Templates) > 0 {
		if len(fc.Atespaces) == 0 {
			f.cancel()
			return nil, errors.New("fleet: the pools and the templates need workbench.fleet.atespaces, the atespaces whose Tasks the front lists")
		}
		// The front reads the variables that the Tasks of the pools and of
		// the templates copy once, and a missing one stops the start, not
		// the first create.
		v, err := pools.Resolve(append(slices.Clone(fc.Pools), pools.Probes(fc.Templates)...), nil)
		if err != nil {
			f.cancel()
			return nil, fmt.Errorf("fleet: %w", err)
		}
		vals = v
		// The pass of a pool merges the overlay of the pool, so the store
		// opens before the first pass.
		store, closeStore, err := OpenOverlays(ctx, cfg, f.log)
		if err != nil {
			f.cancel()
			return nil, fmt.Errorf("fleet: the store of the overlays of the pools: %w", err)
		}
		f.overlays, f.closeOverlays = store, closeStore
		f.templates = fc.Templates
		if len(fc.Templates) > 0 {
			// A runtime pool renders from a template, so the store of the
			// runtime pools opens with the templates only.
			rs, closeRS, err := OpenRuntime(ctx, cfg, f.log, false)
			if err != nil {
				f.cancel()
				if f.closeOverlays != nil {
					f.closeOverlays()
				}
				return nil, err
			}
			if rs == nil {
				where, _ := bucket.Where(cfg, pools.StoreBucket)
				f.log.Warn("fleet: the runtime pools are off; they live "+where, "bus", cfg.Bus.Driver)
			}
			f.runtime, f.closeRuntime = rs, closeRS
		}
	}
	// The index of the runs lets a front that starts again know the runs
	// of the Tasks that sleep. Without it the front still works, and knows
	// the runs of the stream and of the reads alone.
	idx, closeIdx, err := OpenIndex(ctx, cfg, f.log)
	switch {
	case err != nil:
		f.log.Warn("fleet: the index of the runs is off; after a restart the front knows no run of a sleeping Task", "error", err)
	case idx == nil:
		where, _ := bucket.Where(cfg, IndexBucket)
		f.log.Warn("fleet: the index of the runs is off; after a restart the front knows no run of a sleeping Task. The index lives "+where, "bus", cfg.Bus.Driver)
	}
	f.indexBucket, f.closeIndex = idx, closeIdx
	if err := f.startPlatform(fc.Atespaces, egress, cfg.AX.Server, tokenEnv, fc.Pools, vals); err != nil {
		f.cancel()
		if f.closeOverlays != nil {
			f.closeOverlays()
		}
		if f.closeRuntime != nil {
			f.closeRuntime()
		}
		if f.closeIndex != nil {
			f.closeIndex()
		}
		return nil, err
	}
	if err := f.startPackages(ctx, cfg); err != nil {
		_ = f.Close()
		return nil, err
	}
	var src queueSource
	var shared jobStore
	if f.intake != "" {
		var err error
		src, shared, f.closeServer, err = openBusServer(ctx, cfg, f.expire, f.log)
		if err != nil {
			// The front still sends jobs; it reads no queue and keeps the
			// job records of this replica alone.
			f.log.Warn("fleet: open the server of the bus; the front reads no intake queue", "error", err)
			src, shared, f.closeServer = nil, nil, nil
		}
	}
	f.jobs = newJobBook(shared, f.expire, f.log)
	f.startQueues(src)
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
		if f.closeServer != nil {
			f.closeServer()
		}
		if f.closeOverlays != nil {
			f.closeOverlays()
		}
		if f.closeRuntime != nil {
			f.closeRuntime()
		}
		if f.closeIndex != nil {
			f.closeIndex()
		}
		if f.closeEnv != nil {
			f.closeEnv()
		}
		f.client.CloseIdleConnections()
	})
	return nil
}
