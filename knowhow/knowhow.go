// Package knowhow keeps what the agents of a fleet learn on the job
// (docs/KNOWHOW.md, and docs/ARCHITECTURE.md section 25): the notes with
// their sources, the journal "What it learned" with undo, the switch "Ask
// me before it keeps what it learns" with the changes that wait for a
// person, the daily pass that rests and puts away the notes that no run
// used, the candidates of the after-work review and its two MCP tools,
// the schedule of the review, the daily proposals of routines from the
// archive of the runs, and the four numbers of an agent.
//
// The package runs as a service of the kernel (fw.RegisterService). On a
// NATS bus it keeps everything in the key-value bucket <stream>_KNOWHOW,
// so every replica of the front reads one truth. On the bus mem it keeps
// it in the memory of the process. On another bus it logs once that
// Know-how is off and serves nothing. It mounts the routes under
// /v1/knowhow on the native surface (native.Extend), adds the MCP tools
// knowhow_candidates and knowhow_keep (mcp.RegisterTools), and counts the
// uses of each note from fw.memory.used on the egress stream that the
// fleet reads (fleet.Tap). It reads the archive of the runs of the fleet
// (fleet.ArchiveAll) and never a ledger of a worker.
//
// The seams of the other packages call the functions PageWritten,
// ItemChanged, RoutineMoved, and AskFirst. OnChange gives the stream of
// bossmode each change of the Know-how of an agent.
package knowhow

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/surface/native"
	"github.com/bdubs00/featherweight-workbench/fleet"
	"github.com/bdubs00/featherweight-workbench/internal/bucket"
	"github.com/bdubs00/featherweight-workbench/routines"
	"github.com/bdubs00/featherweight-workbench/surface/mcp"
)

func init() {
	fw.RegisterService(Start)
	native.Extend(mount)
	mcp.RegisterTools(tools)
	fleet.Tap(tapped, fw.EvMemoryUsed)
}

// bucketConfig is the bucket of Know-how.
var bucketConfig = bucket.Config{
	Name:        "KNOWHOW",
	Description: "What the agents of a featherweight fleet learned: the notes with their sources, the journal, the switch of each agent, the changes that wait for a person, and the candidates of the after-work review.",
	History:     History,
	MaxValue:    maxValue,
}

// Bounds of the store.
const (
	// History is how many values of each entry the bucket keeps, so an
	// undo finds the text of a note before a change.
	History = 16
	// maxValue bounds one entry of the bucket.
	maxValue = 1 << 20
	// writeAttempts bounds the tries of a write that other writers race.
	writeAttempts = 8
	// tick is how often the passes of the service look for work.
	tick = time.Minute
)

// services ties Know-how to an agent.
var services sync.Map // *fw.Agent to *Service

// Of returns the Know-how of an agent, or nil when the process keeps none.
func Of(a *fw.Agent) *Service {
	if a == nil {
		return nil
	}
	if v, ok := services.Load(a); ok {
		return v.(*Service)
	}
	return nil
}

// Attach makes s the Know-how of a. A nil s detaches.
func Attach(a *fw.Agent, s *Service) {
	if s == nil {
		services.Delete(a)
		return
	}
	services.Store(a, s)
}

// all returns every service of the process.
func all() []*Service {
	var out []*Service
	services.Range(func(_, v any) bool {
		out = append(out, v.(*Service))
		return true
	})
	return out
}

// archiveReader reads the archive of the runs. The fleet gives it; a test
// replaces it.
type archiveReader interface {
	All(ctx context.Context) ([]fleet.ArchiveEntry, bool)
	Events(ctx context.Context, id string) ([]fw.Event, error)
}

// fleetArchive is the archive of the fleet of an agent.
type fleetArchive struct{ a *fw.Agent }

func (f fleetArchive) All(ctx context.Context) ([]fleet.ArchiveEntry, bool) {
	return fleet.ArchiveAll(ctx, f.a)
}

func (f fleetArchive) Events(ctx context.Context, id string) ([]fw.Event, error) {
	return fleet.ArchiveEvents(ctx, f.a, id)
}

// Service is the Know-how of one agent of the process: the store, the
// passes, and the configuration. Build it with New.
type Service struct {
	a   *fw.Agent
	b   bucket.Bucket
	cfg KnowhowConfig
	log *slog.Logger
	now func() time.Time

	// The seams of the passes, which a test replaces: the archive, the
	// queue of a run, and the store of the routines.
	arch     archiveReader
	enqueue  func(ctx context.Context, agent string, reqs []fw.RunRequest) (string, int, error)
	routines func() *routines.Store
	check    func(ctx context.Context, agent string, list []fw.Routine) error

	mu      sync.Mutex
	notes   map[string]cachedNote // by key: a note at a revision
	uses    map[string]cachedUses // by key: the uses of a note at a revision
	facts   map[string]runFacts   // by run id
	appends map[string]int        // by agent: journal entries since the last prune
	idMS    uint64                // the millisecond of the last id
	idRand  [10]byte              // the random part of the last id

	usesIn  chan fleet.RunEvent
	full    bool
	retried map[string]bool // the events that count again later
	scanned time.Time       // the last read of the candidates for the review
}

// New returns the Know-how of the agent a on the bucket b with the
// configuration c. a can be nil in a test.
func New(a *fw.Agent, b bucket.Bucket, c KnowhowConfig, log *slog.Logger) *Service {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	s := &Service{a: a, b: b, cfg: c, log: log, now: time.Now,
		notes: map[string]cachedNote{}, uses: map[string]cachedUses{}, facts: map[string]runFacts{}, appends: map[string]int{},
		usesIn: make(chan fleet.RunEvent, usesQueue)}
	if a != nil {
		s.arch = fleetArchive{a: a}
		s.enqueue = func(ctx context.Context, agent string, reqs []fw.RunRequest) (string, int, error) {
			return fleet.Enqueue(ctx, a, agent, reqs)
		}
		s.routines = func() *routines.Store { return routines.Of(a) }
		s.check = func(ctx context.Context, agent string, list []fw.Routine) error {
			_, err := fleet.CheckRoutines(ctx, a, agent, list)
			return err
		}
	}
	return s
}

// Config returns the configuration of the service.
func (s *Service) Config() KnowhowConfig { return s.cfg }

// Describe names the place of the store for a person.
func (s *Service) Describe() string { return s.b.Describe() }

// Open opens Know-how of the configuration of a on its bus. It returns
// bucket.ErrNoStore for a bus that keeps no store. The function that it
// returns closes the connection; it is nil for a store in memory.
func Open(ctx context.Context, a *fw.Agent, log *slog.Logger) (*Service, func(), error) {
	c, err := Load(a.Config())
	if err != nil {
		return nil, nil, err
	}
	b, closeFn, err := bucket.Open(ctx, a.Config(), bucketConfig, log)
	if err != nil {
		return nil, nil, fmt.Errorf("knowhow: %w", err)
	}
	return New(a, b, c, log), closeFn, nil
}

// Start is the service of Know-how (fw.Service). It opens the store on the
// bus of the configuration, attaches it to the agent, and starts the
// passes. On a bus that keeps no store, it logs once that Know-how is off
// and returns nil. A configuration that does not fit stops the start.
func Start(ctx context.Context, a *fw.Agent) (io.Closer, error) {
	log := a.Logger().With("component", "knowhow")
	s, closeFn, err := Open(ctx, a, log)
	if errors.Is(err, bucket.ErrNoStore) {
		where, _ := bucket.Where(a.Config(), bucketConfig)
		log.Warn("knowhow: Know-how is off; it lives "+where, "bus", a.Config().Bus.Driver)
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	pctx, cancel := context.WithCancel(context.Background())
	Attach(a, s)
	go s.countUses(pctx)
	go s.loop(pctx)
	review := ReviewOff
	if s.cfg.Review.On() {
		review = fmt.Sprintf("the profile %s reviews the finished work every %s", s.cfg.Review.Agent, s.cfg.Review.Every)
	}
	log.Info("knowhow: Know-how is on", "store", s.Describe(), "review", review, "proposals", s.cfg.Propose.On())
	return service{a: a, close: closeFn, cancel: cancel}, nil
}

// service detaches Know-how from an agent, stops the passes, and closes
// the connection.
type service struct {
	a      *fw.Agent
	close  func()
	cancel context.CancelFunc
}

func (s service) Close() error {
	Attach(s.a, nil)
	s.cancel()
	if s.close != nil {
		s.close()
	}
	return nil
}

// Status says where Know-how of the configuration cfg lives and whether
// the after-work review runs, for a line of fw-workbench doctor. It
// reports false for a configuration that does not fit.
func Status(cfg *fw.Config) (bool, string) {
	c, err := Load(cfg)
	if err != nil {
		return false, err.Error()
	}
	where, on := bucket.Where(cfg, bucketConfig)
	if !on {
		return true, "knowhow: off; Know-how lives " + where
	}
	review := ReviewOff
	if c.Review.On() {
		review = fmt.Sprintf("the profile %s reviews the finished work every %s", c.Review.Agent, c.Review.Every)
	}
	return true, fmt.Sprintf("knowhow: the notes and the journal live in %s; %s", where, review)
}

// loop runs the passes of the service until ctx ends: the candidates of
// the after-work review at each tick, the daily passes once a day, and the
// schedule of the review.
func (s *Service) loop(ctx context.Context) {
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		s.passes(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// passes runs each pass once, as its schedule allows.
func (s *Service) passes(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	pctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	if _, err := s.CandidatePass(pctx); err != nil {
		s.log.Warn("knowhow: the pass of the candidates failed; the next tick tries again", "error", err)
	}
	s.Daily(pctx)
	if _, err := s.ReviewTick(pctx); err != nil {
		s.log.Warn("knowhow: the schedule of the after-work review failed; the next tick tries again", "error", err)
	}
}

// hooks holds the functions that OnChange registers.
var hooks struct {
	mu     sync.Mutex
	change []func(agent string)
}

// OnChange registers a function that gets the agent after each change of
// its notes, its journal, its switch, or its waiting list. The package
// bossmode registers one in init and sends the change to the page as the
// event knowhow. The function runs on the goroutine of the change, so it
// must not block. Each replica calls it for the changes that it writes.
func OnChange(fn func(agent string)) {
	hooks.mu.Lock()
	defer hooks.mu.Unlock()
	hooks.change = append(hooks.change, fn)
}

// emitChange calls the OnChange hooks.
func emitChange(agent string) {
	hooks.mu.Lock()
	list := append([]func(string){}, hooks.change...)
	hooks.mu.Unlock()
	for _, fn := range list {
		fn(agent)
	}
}
