// Package bridges is the bridges of bossmode (docs/ARCHITECTURE.md section
// 18). A bridge is one large job split into seats, with one agent on each
// seat and one channel where the agents and the operator talk. The package
// keeps the record of each bridge and its channel, seats the members on
// the queues of their pools or under the run of the manager, fans each
// message out to the runs of the members as a follow-up, mirrors the life
// of each run into the channel as system lines, and records the reactions
// of people, with a heart on the wiki.
//
// The package runs as a service of the kernel (fw.RegisterService). On a
// NATS bus the records live in the key-value bucket <stream>_BRIDGES and
// the channels in <stream>_CHANNELS, so every replica of the front reads
// one truth. On the bus mem both live in the memory of the process, and
// two processes share nothing. On another bus the package logs once that
// the bridges are off and serves nothing. It mounts the routes under
// /v1/bridges on the native surface (native.Extend), adds the MCP tools
// bridge_read, bridge_post, bridge_list, bridge_open, bridge_add_member,
// bridge_remove_member, and bridge_close (mcp.RegisterTools), and reads
// workbench.bridges, FW_WORKBENCH_BRIDGES_*, and workbench.bossmode.manager.
//
// Two hooks are the seams of the live stream of bossmode: OnMessage gets
// every new or changed entry of a channel, and OnMember every new or
// changed seat, with its presence and its activity. The package bossmode
// registers both and publishes what they get. UsePhrase sets the words of
// the activity of a run; init sets fleet.ActivityPhrase, which words a
// tool call with the input that the view keeps, as the stream of bossmode
// words it.
//
// The service follows the changes of the fleet view (fleet.Watcher) and
// reads the view at each change of a run of a bridge, a member, or a Task,
// and only every sweepEvery beside them. Without a fleet that sends its
// changes, it reads the view every pollEvery. The service also gives its
// store to the package manager (manager.SetBridges), so a manager run asks
// the record of a bridge what its operator delegated.
package bridges

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/surface/native"
	"github.com/bdubs00/featherweight-workbench/fleet"
	"github.com/bdubs00/featherweight-workbench/internal/bucket"
	"github.com/bdubs00/featherweight-workbench/manager"
	"github.com/bdubs00/featherweight-workbench/surface/mcp"
)

func init() {
	fw.RegisterService(Start)
	native.Extend(mount)
	mcp.RegisterTools(tools)
	// The roster words a tool call as the stream of bossmode words the run.
	UsePhrase(fleet.ActivityPhrase)
}

// Times of the service.
const (
	// pollEvery is the time between two reads of the view of the fleet
	// when the fleet sends no changes.
	pollEvery = 2 * time.Second
	// sweepEvery is the time between two reads of the view while the
	// service follows the changes of the fleet: the reads for what no
	// change announces, such as a read of an owner that failed, the report
	// of a closed bridge, and a run of this process.
	sweepEvery = 10 * time.Second
	// pollGap is the shortest time between two reads that changes start,
	// so a burst of changes costs one read.
	pollGap = 250 * time.Millisecond
	// followRetry is the time between two looks for a fleet that sends
	// its changes.
	followRetry = time.Second
	// retryFirst is the first pause before a delivery is tried again, and
	// retryMax the longest; the pause doubles. retryFor bounds the tries
	// of one delivery.
	retryFirst = time.Second
	retryMax   = time.Minute
	retryFor   = 10 * time.Minute
	// reportWait bounds the wait for the report of a closed bridge.
	reportWait = 10 * time.Minute
	// lostEvery is the time between two reads of a run whose card shows
	// the state unknown.
	lostEvery = time.Minute
	// wholeEvery is the time between two reads of the whole call of a card
	// whose first read failed (wholeDue).
	wholeEvery = 10 * time.Second
	// sleepHold is how long the Task of a seat stays quiet, with no
	// replica that sees it in another state, before the channel says that
	// it went to sleep (sleepLines). A settled view also misses the newest
	// run of a seat this long before the channel says that the seat lost
	// it (goneSeats).
	sleepHold = 30 * time.Second
	// workers is the number of deliveries that run at once, and queueSize
	// the deliveries that wait.
	workers   = 4
	queueSize = 4096
)

// Store is the bridges of one agent: the records, the channels, the
// fan-out, and the watch of the view. Build it with Open, and find the one
// of an agent with Of.
type Store struct {
	a   *fw.Agent
	log *slog.Logger
	cfg Config
	b   *buckets

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	jobs   chan *job
	wake   chan struct{}

	// The times of the service, which a test shortens.
	pollEvery   time.Duration
	sweepEvery  time.Duration
	pollGap     time.Duration
	followRetry time.Duration
	retryFirst  time.Duration
	retryMax    time.Duration
	retryFor    time.Duration
	reportWait  time.Duration
	lostEvery   time.Duration
	sleepHold   time.Duration
	now         func() time.Time

	// following is true while the service follows the changes of the
	// fleet (follow), and polls counts the reads of the view.
	following atomic.Bool
	polls     atomic.Int64

	// startMu makes the starts of new manager runs of this replica go one
	// at a time (newManager).
	startMu sync.Mutex

	// names holds the names of the people who called a route, by subject
	// (learn).
	namesMu sync.Mutex
	names   map[string]string

	// What this replica knows of the view, for the watch (watch.go).
	mu       sync.Mutex
	checked  map[string]uint64    // by run id: the seq of the run at its last read of the ends
	tries    map[string]int       // by run id: the reads of a decision that failed
	lostAt   map[string]time.Time // by run id: the last read for the cards of the run that show unknown
	wholeAt  map[string]time.Time // by run id: the last read of the whole call that the run waits for (wholeDue)
	shown    map[string]string    // by bridge and handle: the presence and activity that a hook got
	canceled map[string]bool      // by run id: the runs of fired seats that a cancel went to
	missing  map[string]missing   // by bridge and handle: the newest run of a seat that a settled view did not hold

	// feeds holds what this replica gave its streams of each channel
	// (catchup.go).
	feeds feeds
}

// New returns a store on two buckets. The service starts nothing until
// run.
func New(a *fw.Agent, records, channels bucket.Bucket, cfg Config, log *slog.Logger) *Store {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Store{
		a:           a,
		log:         log,
		cfg:         cfg,
		b:           &buckets{records: records, channels: channels},
		ctx:         ctx,
		cancel:      cancel,
		jobs:        make(chan *job, queueSize),
		wake:        make(chan struct{}, 1),
		pollEvery:   pollEvery,
		sweepEvery:  sweepEvery,
		pollGap:     pollGap,
		followRetry: followRetry,
		retryFirst:  retryFirst,
		retryMax:    retryMax,
		retryFor:    retryFor,
		reportWait:  reportWait,
		lostEvery:   lostEvery,
		sleepHold:   sleepHold,
		now:         time.Now,
		checked:     map[string]uint64{},
		tries:       map[string]int{},
		lostAt:      map[string]time.Time{},
		wholeAt:     map[string]time.Time{},
		shown:       map[string]string{},
		canceled:    map[string]bool{},
		missing:     map[string]missing{},
		feeds:       feeds{by: map[string]*feedMark{}},
	}
}

// run starts the workers of the fan-out, the watch of the view, and the
// follower of the changes of the fleet.
func (s *Store) run() {
	s.runWorkers()
	s.wg.Add(2)
	go s.watch()
	go s.follow()
}

// runWorkers starts the workers of the fan-out.
func (s *Store) runWorkers() {
	for i := 0; i < workers; i++ {
		s.wg.Add(1)
		go s.work()
	}
}

// Close stops the watch and the fan-out. A delivery that waits for a retry
// is lost; the channel holds its message.
func (s *Store) Close() {
	s.cancel()
	s.wg.Wait()
}

// Wake makes the watch read the view now, before its next tick, and at
// most once per pollGap. The follower of the changes of the fleet calls it
// for each change that can concern a bridge (follow), so the channel
// follows a run without the wait of the poll. It never blocks.
func (s *Store) Wake() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// Describe names where the store keeps the bridges.
func (s *Store) Describe() string { return s.b.records.Describe() }

// stores ties the bridges of an agent to the agent. Start attaches them,
// and the routes and the tools find them through Of.
var stores sync.Map // *fw.Agent to *Store

// Of returns the bridges of an agent, or nil when the process keeps none:
// the bus keeps no store, or the process does not serve.
func Of(a *fw.Agent) *Store {
	if a == nil {
		return nil
	}
	s, ok := stores.Load(a)
	if !ok {
		return nil
	}
	return s.(*Store)
}

// Attach makes s the bridges of a. A nil s detaches.
func Attach(a *fw.Agent, s *Store) {
	if s == nil {
		stores.Delete(a)
		return
	}
	stores.Store(a, s)
}

// Start is the service of the bridges (fw.Service). It opens the two
// buckets on the bus of the configuration, attaches the store to the
// agent, and starts the watch of the view and the workers of the fan-out.
// On a bus that keeps no store it logs once that the bridges are off and
// returns nil. A configuration that does not fit stops the start.
func Start(ctx context.Context, a *fw.Agent) (io.Closer, error) {
	log := a.Logger().With("component", "bridges")
	cfg, err := Load(a.Config())
	if err != nil {
		return nil, err
	}
	rc, cc := bucketConfigs()
	records, closeRecords, err := bucket.Open(ctx, a.Config(), rc, log)
	if errors.Is(err, bucket.ErrNoStore) {
		where, _ := bucket.Where(a.Config(), rc)
		log.Warn("bridges: the bridges are off; they live "+where, "bus", a.Config().Bus.Driver)
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	channels, closeChannels, err := bucket.Open(ctx, a.Config(), cc, log)
	if err != nil {
		if closeRecords != nil {
			closeRecords()
		}
		return nil, err
	}
	s := New(a, records, channels, cfg, log)
	s.run()
	Attach(a, s)
	// A manager run asks the record of a bridge what its operator
	// delegated (manager.Delegated).
	manager.SetBridges(s)
	log.Info("bridges: the bridges are on", "store", s.Describe(), "placement", cfg.Placement, "maxMembers", cfg.MaxMembers, "manager", cfg.Manager)
	return service{a: a, s: s, close: []func(){closeRecords, closeChannels}}, nil
}

// service detaches the bridges of an agent and closes their connections.
type service struct {
	a     *fw.Agent
	s     *Store
	close []func()
}

func (x service) Close() error {
	Attach(x.a, nil)
	manager.ClearBridges(x.s)
	x.s.Close()
	for _, f := range x.close {
		if f != nil {
			f()
		}
	}
	return nil
}

// hooks holds the functions that OnMessage, OnMember, and UsePhrase
// register.
var hooks struct {
	mu      sync.Mutex
	message []func(bridge string, m Message)
	member  []func(bridge string, m Member)
	catchUp []func(a *fw.Agent, bridge string, m Message)
	phrase  func(r *fw.Run) string
}

// OnMessage registers a function that gets every new entry of a channel,
// and every entry that changes, with the same seq: a reaction, or a card
// whose decision landed. The live stream of bossmode registers one in init
// and sends each entry to the pages as the event bridge.message. The
// function runs on the goroutine that changed the entry, so it must not
// block; each replica calls it for the changes that it makes.
func OnMessage(fn func(bridge string, m Message)) {
	hooks.mu.Lock()
	defer hooks.mu.Unlock()
	hooks.message = append(hooks.message, fn)
}

// OnMember registers a function that gets every new seat and every change
// of a seat: its presence, its activity, its task, muted, or fired. The
// live stream of bossmode sends each one as the event bridge.member. Each
// replica watches the view on its own, so each replica calls it for the
// changes of presence that it sees. The function must not block.
func OnMember(fn func(bridge string, m Member)) {
	hooks.mu.Lock()
	defer hooks.mu.Unlock()
	hooks.member = append(hooks.member, fn)
}

// UsePhrase sets the function that words the activity of a run for the
// activity of a member. The fleet keeps the tool input of the call that a
// run waits for, so it words the call as a result words a step; init sets
// fleet.ActivityPhrase, the words of the run events of the stream of
// bossmode. Without a function the package words only a run that thinks,
// and leaves the phrase of a tool call empty. The last call wins.
func UsePhrase(fn func(r *fw.Run) string) {
	hooks.mu.Lock()
	defer hooks.mu.Unlock()
	hooks.phrase = fn
}

// emitMessage calls the OnMessage hooks.
func emitMessage(bridge string, m Message) {
	hooks.mu.Lock()
	list := append([]func(string, Message){}, hooks.message...)
	hooks.mu.Unlock()
	for _, fn := range list {
		fn(bridge, m)
	}
}

// emitMember calls the OnMember hooks.
func emitMember(bridge string, m Member) {
	hooks.mu.Lock()
	list := append([]func(string, Member){}, hooks.member...)
	hooks.mu.Unlock()
	for _, fn := range list {
		fn(bridge, m)
	}
}

// phraseFn returns the function of UsePhrase, or nil.
func phraseFn() func(r *fw.Run) string {
	hooks.mu.Lock()
	defer hooks.mu.Unlock()
	return hooks.phrase
}

// subjectOf is the subject of the principal in ctx, or "".
func subjectOf(ctx context.Context) string {
	if p := fw.PrincipalFrom(ctx); p != nil {
		return p.Subject
	}
	return ""
}

// clean trims a text and drops its control characters but the line
// breaks and the tabs.
func clean(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return r
		}
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, strings.TrimSpace(s))
}
