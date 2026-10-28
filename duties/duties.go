// Package duties keeps the duties of a fleet (docs/ARCHITECTURE.md
// section 22). A duty is what an agent does on its own: who does it, when,
// what, and within which limits. When is a schedule in words, such as
// "every weekday at 9am" (package schedule), or a call of another app.
// What asks the agent with a message. A duty can check first: it reads an
// HTTP address and wakes the model only when the answer holds work.
//
// The package runs as a service of the kernel (fw.RegisterService). On a
// NATS bus the duties live in the key-value buckets <stream>_DUTIES,
// <stream>_DUTY_RUNS, and <stream>_DUTY_SEEN, so every replica of the
// front reads one truth (store.go). On the bus mem they live in the memory
// of the process. On another bus the package logs once that the duties
// are off and serves nothing. Each replica runs the engine (engine.go):
// it computes the next time of each duty, claims that time in the bucket,
// and only the replica that wins the claim puts the run on the queue
// (fleet.EnqueueFor). The package mounts the routes under /v1/duties on
// the native surface (native.Extend) and adds the MCP tools of the duties
// (mcp.RegisterTools). Each tool calls the Go function of its route.
//
// The later parts of the feature plug in here: RegisterAction for the
// actions post, open, and manager, RegisterEvents and Fire for the starts
// on events of the fleet, RegisterLook for a check with a tool of the
// agent, OnNotice for the notifications, and OnChange for the live stream
// of bossmode.
package duties

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/surface/native"
	"github.com/bdubs00/featherweight-workbench/fleet"
	"github.com/bdubs00/featherweight-workbench/internal/bucket"
	"github.com/bdubs00/featherweight-workbench/sources"
	"github.com/bdubs00/featherweight-workbench/surface/mcp"
)

func init() {
	fw.RegisterService(Start)
	native.Extend(mount)
	mcp.RegisterTools(tools)
	sources.RegisterTakeover(takeover)
}

// Keys of the metadata that a duty gives each run, beside fw.batch and
// fw.title. MetaDuty is the key of the kernel, fw.MetaDuty, so a routine
// bound to a Duty finds the runs of that Duty.
const (
	MetaDuty    = fw.MetaDuty             // the name of the duty
	MetaSlot    = "workbench.dutySlot"    // the time of the start, RFC 3339 in UTC
	MetaAttempt = "workbench.dutyAttempt" // the attempt, from 2, for a retry of the duty
	MetaReview  = fleet.MetaReview        // 1 when the owner checks each result (Duty.Review)
	MetaFound   = fleet.MetaFound         // 1 when a result reaches people only when the run finds something (Notify.ResultWhen found)
)

// BatchOf returns the batch of the runs of a duty, duty:<name>.
func BatchOf(name string) string { return "duty:" + name }

// NameOf returns the duty of a run, from its metadata, or "".
func NameOf(r *fw.Run) string {
	if r == nil {
		return ""
	}
	return r.Metadata[MetaDuty]
}

// services ties the duties of an agent to the agent.
var services sync.Map // *fw.Agent to *Service

// Of returns the duties of an agent, or nil when the process keeps none.
func Of(a *fw.Agent) *Service {
	if a == nil {
		return nil
	}
	if v, ok := services.Load(a); ok {
		return v.(*Service)
	}
	return nil
}

// attach makes s the duties of a. A nil s detaches.
func attach(a *fw.Agent, s *Service) {
	if s == nil {
		services.Delete(a)
		return
	}
	services.Store(a, s)
}

// Service is the duties of one agent: the store, the engine, and the
// rules of the operator. Build it with Start, or newService in a test.
type Service struct {
	a       *fw.Agent
	st      *Store
	cfg     Config
	log     *slog.Logger
	now     func() time.Time
	replica string
	client  *http.Client
	every   time.Duration // the longest pause of the engine

	// The seams of the engine, which a test replaces. output reads the
	// output of a run for the block last.
	enqueue func(ctx context.Context, agent, pool string, reqs []fw.RunRequest) (string, int, error)
	runs    func(ctx context.Context, name string) (map[string]*fw.Run, error)
	secret  func(ctx context.Context, name string) (string, error)
	output  func(ctx context.Context, id string) string

	// The seams of a write of a check (writecheck.go): the wait for a run
	// that the front put on a queue, its cancel, and the longest wait.
	await     func(ctx context.Context, id string) (*fw.Run, error)
	stopRun   func(ctx context.Context, id string) error
	writeWait time.Duration

	mu      sync.Mutex
	cursors map[string]time.Time       // per duty, the newest time of its schedule that this replica handled
	open    map[string]map[string]bool // per duty, the dates of its history with a start that works
	seeded  map[string]bool            // per duty, whether this replica read its history once
	warned  map[string]string          // per duty, the last problem of its schedule in the log

	// heldBack is the share of the runs of each duty that the engine
	// holds back from Know-how (workbench.knowhow.heldBack).
	heldBack float64

	// passive is true on a replica that cannot put a job on a queue
	// (fleet.CanEnqueue): it serves the routes and runs no engine, and a
	// start on it fails with ErrNoStarts.
	passive bool

	// The polls of the duties of the templates (imported.go): the time of
	// the last poll of each duty on this replica, the duties whose poll
	// works now, and the polls that work.
	polled  map[string]time.Time
	polling map[string]bool
	pollers sync.WaitGroup

	poke      chan struct{}
	closers   []func()
	cancel    context.CancelFunc
	done      chan struct{}
	closeOnce sync.Once
}

// Start is the service of the duties (fw.Service). It opens the buckets on
// the bus of the configuration, starts the engine, and attaches the duties
// to the agent. On a bus that keeps no store it logs once that the duties
// are off and returns nil. A configuration that does not fit stops the
// start.
func Start(ctx context.Context, a *fw.Agent) (io.Closer, error) {
	log := a.Logger().With("component", "duties")
	cfg, err := LoadConfig(a.Config())
	if err != nil {
		return nil, err
	}
	knowhow, err := LoadKnowhow(a.Config())
	if err != nil {
		return nil, err
	}
	var closers []func()
	open := func(c bucket.Config) (bucket.Bucket, error) {
		b, closeFn, err := bucket.Open(ctx, a.Config(), c, log)
		if closeFn != nil {
			closers = append(closers, closeFn)
		}
		return b, err
	}
	closeAll := func() {
		for _, f := range closers {
			f()
		}
	}
	duties, err := open(dutiesBucket)
	if errors.Is(err, bucket.ErrNoStore) {
		where, _ := bucket.Where(a.Config(), dutiesBucket)
		log.Warn("duties: the duties are off; they live "+where, "bus", a.Config().Bus.Driver)
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("duties: %w", err)
	}
	runs, err := open(runsBucket)
	if err != nil {
		closeAll()
		return nil, fmt.Errorf("duties: %w", err)
	}
	seen, err := open(seenBucket)
	if err != nil {
		closeAll()
		return nil, fmt.Errorf("duties: %w", err)
	}
	s := newService(a, NewStore(duties, runs, seen, log), cfg, log)
	s.heldBack = knowhow.Share()
	s.closers = closers
	s.passive = !fleet.CanEnqueue(a.Config())
	// The entries of workbench.sources become duties before the engine
	// starts, so the engine runs them from its first pass.
	ictx, cancel := context.WithTimeout(ctx, 30*time.Second)
	s.importSources(ictx)
	cancel()
	attach(a, s)
	where, _ := bucket.Where(a.Config(), dutiesBucket)
	if s.passive {
		log.Info("duties: this replica serves the duties and starts none, because workbench.fleet.intake is empty. A replica with an intake starts them.", "store", where, "replica", s.replica)
		return s, nil
	}
	s.start()
	log.Info("duties: the duties are on", "store", where, "replica", s.replica, "heldBack", s.heldBack)
	return s, nil
}

// Engine reports whether this replica starts duties. A replica without
// workbench.fleet.intake serves the routes and starts none.
func (s *Service) Engine() bool { return s != nil && !s.passive }

// newService builds the duties of an agent on a store, with the seams of
// the fleet and the secrets of the agent.
func newService(a *fw.Agent, st *Store, cfg Config, log *slog.Logger) *Service {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	s := &Service{
		a: a, st: st, cfg: cfg, log: log, now: time.Now, replica: replicaOf(a),
		client: cfg.client(), every: defaultEvery,
		cursors: map[string]time.Time{}, open: map[string]map[string]bool{}, seeded: map[string]bool{}, warned: map[string]string{},
		poke: make(chan struct{}, 1),
	}
	s.enqueue = func(ctx context.Context, agent, pool string, reqs []fw.RunRequest) (string, int, error) {
		return fleet.EnqueueFor(ctx, a, agent, pool, reqs)
	}
	s.runs = s.runsOfFleet
	s.output = s.outputOf
	s.await = func(ctx context.Context, id string) (*fw.Run, error) { return fleet.Await(ctx, a, id) }
	s.stopRun = func(ctx context.Context, id string) error { return fleet.CancelRun(ctx, a, id) }
	s.writeWait = WriteCheckTimeout
	s.secret = func(ctx context.Context, name string) (string, error) {
		secrets := a.Secrets()
		if secrets == nil {
			secrets = fw.EnvSecrets{}
		}
		v, err := secrets.Get(ctx, name)
		if err == nil && strings.TrimSpace(v) == "" {
			err = fw.ErrSecretNotFound
		}
		return strings.TrimSpace(v), err
	}
	return s
}

// replicaOf names this process in the history: its name in the fleet,
// else the host name.
func replicaOf(a *fw.Agent) string {
	if v := fleet.Of(a); v != nil {
		return v.Name()
	}
	host, _ := os.Hostname()
	return host
}

// start starts the engine.
func (s *Service) start() {
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel, s.done = cancel, make(chan struct{})
	go s.loop(ctx)
}

// Close stops the engine and detaches the duties. Runs that work go on.
func (s *Service) Close() error {
	s.closeOnce.Do(func() {
		if s.cancel != nil {
			s.cancel()
			<-s.done
		}
		s.pollers.Wait()
		if Of(s.a) == s {
			attach(s.a, nil)
		}
		for _, f := range s.closers {
			f()
		}
	})
	return nil
}

// Status says where the duties of the configuration cfg live, for a line
// of fw-workbench doctor. It reports false for a configuration that does
// not fit.
func Status(cfg *fw.Config) (bool, string) {
	if _, err := LoadConfig(cfg); err != nil {
		return false, err.Error()
	}
	where, on := bucket.Where(cfg, dutiesBucket)
	if !on {
		return true, "duties: off; the duties live " + where
	}
	return true, "duties: the duties live in " + where
}

// ErrNotAvailable means that a part of a duty needs work that this build
// does not have yet, such as a check with a tool of the agent.
var ErrNotAvailable = errors.New("duties: not available yet")

// Started is what an action of a registered package started: the run that
// the engine follows to its end, or none, and a note for the history.
type Started struct {
	Run  string
	Note string
}

// Action carries out an action of do other than ask. Check lists the
// problems of the action in a duty, in words. Start does the work of one
// start: request is the run request that ask would put on the queue, with
// the placeholders filled, the metadata, the budget, and the principal,
// for an action that starts a run of its own.
type Action struct {
	Check func(ctx context.Context, a *fw.Agent, d *Duty) []string
	Start func(ctx context.Context, a *fw.Agent, d *Duty, request fw.RunRequest) (Started, error)
}

// Looker reads the document of a check: the JSON in which the check finds
// its items. The package duties/looks registers the look tool, a check
// run of the kernel through the tool of the agent.
type Looker func(ctx context.Context, a *fw.Agent, d *Duty) ([]byte, error)

// Names of the looks of a check: LookNameHTTP reads an address in the
// front, and LookNameTool calls a tool of the agent in a check run of the
// kernel.
const (
	LookNameHTTP = "http"
	LookNameTool = "tool"
)

// Notice is an event of a duty for the notifications: a failed run, a
// duty that paused itself, a daily cap that a duty reached, or a run that
// waits for a person. Who names the people of the notify list of the
// duty for the kind, with owner already the subject of the owner.
type Notice struct {
	Kind   string    `json:"kind"`
	Duty   string    `json:"duty"`
	Agent  string    `json:"agent"`
	Owner  string    `json:"owner"`
	Who    []string  `json:"who"`
	Run    string    `json:"run,omitempty"`
	Slot   time.Time `json:"slot,omitzero"`
	Reason string    `json:"reason"`
	At     time.Time `json:"at"`
}

// Kinds of a notice.
const (
	NoticeFailed   = "failed"    // a run failed after its retries
	NoticePaused   = "paused"    // the duty paused itself
	NoticeCap      = "cap"       // the duty reached its daily cap
	NoticeNeedsYou = "needs-you" // a run of the duty waits for a person
	NoticeResult   = "result"    // a start completed, for the people of notify.result
)

// hooks holds what the later packages register.
var hooks struct {
	mu      sync.Mutex
	actions map[string]Action
	looks   map[string]Looker
	events  func(e *Event) []string
	kinds   []string
	notice  []func(ctx context.Context, a *fw.Agent, n Notice)
	change  []func(name string)
	channel func(a *fw.Agent, name string) bool
	zone    func(ctx context.Context, a *fw.Agent, subject string) string
}

// RegisterZone sets how the duties read the time zone that a person saved,
// such as the time zone of the notifications of the person. fn answers ""
// for a person who saved none. A package calls it in init. A new schedule
// that names no time zone takes this one, else UTC.
func RegisterZone(fn func(ctx context.Context, a *fw.Agent, subject string) string) {
	hooks.mu.Lock()
	defer hooks.mu.Unlock()
	hooks.zone = fn
}

// savedZone returns the time zone that the person saved, or "" when the
// person saved none or it is no IANA time zone.
func (s *Service) savedZone(ctx context.Context, subject string) string {
	hooks.mu.Lock()
	fn := hooks.zone
	hooks.mu.Unlock()
	if fn == nil || subject == "" {
		return ""
	}
	zone := strings.TrimSpace(fn(ctx, s.a, subject))
	if loc, err := time.LoadLocation(zone); zone == "" || zone == "Local" || err != nil || loc == nil {
		return ""
	}
	return zone
}

// RegisterAction makes the action post, open, or manager available. A
// package calls it in init. Until then, a write of a duty with that
// action fails with "not available yet".
func RegisterAction(name string, act Action) {
	hooks.mu.Lock()
	defer hooks.mu.Unlock()
	if hooks.actions == nil {
		hooks.actions = map[string]Action{}
	}
	hooks.actions[name] = act
}

// RegisterLook makes the look of a check available, such as tool. A
// package calls it in init. Until then, a test and a start of such a check
// fail with ErrNotAvailable.
func RegisterLook(name string, fn Looker) {
	hooks.mu.Lock()
	defer hooks.mu.Unlock()
	if hooks.looks == nil {
		hooks.looks = map[string]Looker{}
	}
	hooks.looks[name] = fn
}

// RegisterEvents makes when.event available for the kinds that the
// package serves: check lists the problems of an event of a duty, in
// words. The package that calls it also calls Fire for each event of the
// fleet that a duty waits for.
func RegisterEvents(kinds []string, check func(e *Event) []string) {
	hooks.mu.Lock()
	defer hooks.mu.Unlock()
	hooks.events = check
	hooks.kinds = append([]string(nil), kinds...)
	slices.Sort(hooks.kinds)
}

// Offered returns what this front serves now: the kinds of event of
// when.event, the actions of do, and the looks of checkFirst. Each list
// is sorted. GET /v1/duties/options answers with it, and the page offers
// no other choice.
func Offered() (events, actions, looks []string) {
	hooks.mu.Lock()
	defer hooks.mu.Unlock()
	events = append([]string{}, hooks.kinds...)
	actions = []string{ActionAsk}
	for name := range hooks.actions {
		actions = append(actions, name)
	}
	looks = []string{LookNameHTTP}
	for name := range hooks.looks {
		looks = append(looks, name)
	}
	slices.Sort(actions)
	slices.Sort(looks)
	return events, slices.Compact(actions), slices.Compact(looks)
}

// RegisterChannels makes the entries channel:<name> of the notify lists
// available: has reports whether the front has a shared channel of the
// name. The package notify calls it in init. Until then, a write of a duty
// that names a shared channel fails with "not available yet".
func RegisterChannels(has func(a *fw.Agent, name string) bool) {
	hooks.mu.Lock()
	defer hooks.mu.Unlock()
	hooks.channel = has
}

func channelCheck() func(a *fw.Agent, name string) bool {
	hooks.mu.Lock()
	defer hooks.mu.Unlock()
	return hooks.channel
}

// OnNotice registers a function that gets each notice of a duty, after the
// change that it tells of. It runs on the goroutine of the engine, so it
// must not block. Each notice comes from the one replica that made the
// change.
func OnNotice(fn func(ctx context.Context, a *fw.Agent, n Notice)) {
	hooks.mu.Lock()
	defer hooks.mu.Unlock()
	hooks.notice = append(hooks.notice, fn)
}

// OnChange registers a function that gets the name of a duty after each
// change that this replica wrote to the duty or to its history, and ""
// after a change of the switch that pauses every duty. It runs on the
// goroutine of the change, so it must not block.
func OnChange(fn func(name string)) {
	hooks.mu.Lock()
	defer hooks.mu.Unlock()
	hooks.change = append(hooks.change, fn)
}

func actionOf(name string) (Action, bool) {
	hooks.mu.Lock()
	defer hooks.mu.Unlock()
	act, ok := hooks.actions[name]
	return act, ok
}

func lookOf(name string) Looker {
	hooks.mu.Lock()
	defer hooks.mu.Unlock()
	return hooks.looks[name]
}

func eventsCheck() func(e *Event) []string {
	hooks.mu.Lock()
	defer hooks.mu.Unlock()
	return hooks.events
}

// endWithoutRun ends a start whose registered action started no run, such
// as a post in a bridge: the start names no run, so its history links to
// none, and its reason says what the action did.
func (s *Service) endWithoutRun(ctx context.Context, st *stored, date string, e entry, note string) {
	_, wrote, err := s.st.setEntry(ctx, st.Duty.Name, date, e.ID, []string{StateStarting}, func(x *entry) bool {
		x.State, x.Reason, x.Run = StateCompleted, note, ""
		x.FoundText, x.Event = "", ""
		return true
	})
	if err != nil {
		s.log.Warn("duties: the history of a duty did not change", "duty", st.Duty.Name, "error", err)
	}
	if wrote {
		changed(st.Duty.Name)
		e.State, e.Reason, e.Run = StateCompleted, note, ""
		s.resultNotice(ctx, st, e, note)
	}
}

// notify calls the OnNotice hooks.
func (s *Service) notify(ctx context.Context, st *stored, n Notice) {
	d := &st.Duty
	n.Duty, n.Agent, n.Owner, n.At = d.Name, d.Agent, d.Owner, s.now().UTC()
	var list []string
	if d.Notify != nil {
		list = d.Notify.Failure
		switch n.Kind {
		case NoticeNeedsYou:
			list = d.Notify.NeedsYou
		case NoticeResult:
			list = d.Notify.Result
		}
	}
	for _, who := range list {
		if who == RunAsOwner {
			who = d.Owner
		}
		n.Who = append(n.Who, who)
	}
	s.log.Info("duties: a notice", "kind", n.Kind, "duty", n.Duty, "run", n.Run, "reason", n.Reason)
	hooks.mu.Lock()
	fns := append([]func(context.Context, *fw.Agent, Notice){}, hooks.notice...)
	hooks.mu.Unlock()
	for _, fn := range fns {
		fn(ctx, s.a, n)
	}
}

// changed calls the OnChange hooks.
func changed(name string) {
	hooks.mu.Lock()
	fns := append([]func(string){}, hooks.change...)
	hooks.mu.Unlock()
	for _, fn := range fns {
		fn(name)
	}
}

// Lookup returns a duty of the store by its name for the front itself,
// with no check of the caller, or an error that wraps ErrNotFound. A later
// package reads whileAway and notify of the duty of a run with it.
func (s *Service) Lookup(ctx context.Context, name string) (*Duty, error) {
	st, err := s.st.get(ctx, name)
	if err != nil {
		return nil, err
	}
	d := st.Duty
	return &d, nil
}

// Fire starts a duty for an event of the fleet, as a later package that
// serves when.event calls it. event is the JSON of the event, for the
// placeholder {event}. A duty whose start is not an event, or that does
// not exist, is an error.
func (s *Service) Fire(ctx context.Context, name string, event json.RawMessage) (Outcome, error) {
	st, err := s.st.get(ctx, name)
	if err != nil {
		return Outcome{}, err
	}
	if st.Duty.When.Event == nil {
		return Outcome{}, fmt.Errorf("%w: the duty %s starts on no event", fw.ErrInvalidRequest, name)
	}
	if blocked, why := s.stopped(ctx, st); blocked {
		return Outcome{Skipped: why}, nil
	}
	return s.begin(ctx, st, Trigger{Kind: StartEvent, Slot: s.now().UTC(), Event: string(event)})
}
