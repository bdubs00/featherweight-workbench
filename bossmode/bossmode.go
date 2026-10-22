// Package bossmode serves the operator front of the fleet
// (docs/ARCHITECTURE.md section 17): the static files of the page at
// /bossmode, its settings at /v1/bossmode, and one stream of server-sent
// events at /v1/bossmode/stream. The stream carries the changes of the
// fleet view (fleet.Watcher), the entries and the seats of the bridges,
// the pages of the wiki, and the faces of the agents, which the hooks of
// those packages feed (feeds.go), and the events that other packages
// publish with Publish. It shows each caller only what the caller can see.
//
// The package is a service of the kernel (fw.RegisterService), mounts its
// routes through native.Extend, reads workbench.bossmode and
// FW_WORKBENCH_BOSSMODE_*, and adds a gauge to the metrics and a line to
// fw doctor, which also names the stores of the wiki, the faces, and the
// bridges. Nothing in it calls a model.
package bossmode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/surface/metrics"
	"github.com/bdubs00/featherweight-kernel/surface/native"
	"github.com/bdubs00/featherweight-workbench/fleet"
	"github.com/bdubs00/featherweight-workbench/skills"
	"github.com/bdubs00/featherweight-workbench/surface/mcp"
	"github.com/bdubs00/featherweight-workbench/surface/web"
)

// Path is the route of the page.
const Path = "/bossmode"

// Limits of the stream.
const (
	// RingSize is the number of the newest events that the front keeps
	// for a stream that resumes with Last-Event-ID.
	RingSize = 4096
	// MaxStreams is the number of streams that one process serves at once.
	// One more gets 429.
	MaxStreams = 64
	// KeepAlive is the time between two comments on a stream without an
	// event, so a proxy keeps the stream open.
	KeepAlive = 15 * time.Second
	// StreamLife is the longest time that one stream stays open. The page
	// then opens a new stream with Last-Event-ID, so its credential is
	// checked again, and a token that expired ends the stream within this
	// time.
	StreamLife = 15 * time.Minute
	// MaxEventBytes is the largest data of an event that Publish takes.
	MaxEventBytes = 16 << 10
)

// Kinds of the stream that this package sends itself. The kinds run,
// member, and task carry the changes of the fleet view, and reset tells a
// stream that it cannot resume.
const (
	KindRun    = fleet.ChangeRun
	KindMember = fleet.ChangeMember
	KindTask   = fleet.ChangeTask
	KindReset  = "reset"
)

func init() {
	fw.RegisterService(Start)
	native.Extend(extension)
	metrics.RegisterCollector(newCollector)
	mcp.RegisterTools(skillTools)
	// After a sign-in, the browser returns to the page.
	web.AllowSigninReturn(Path + "/")
}

// hubs ties the hub of the stream to the agent that it serves.
var hubs sync.Map // *fw.Agent to *Hub

// Of returns the hub of an agent, or nil while bossmode is off or has not
// started.
func Of(a *fw.Agent) *Hub {
	if a == nil {
		return nil
	}
	if v, ok := hubs.Load(a); ok {
		return v.(*Hub)
	}
	return nil
}

func attach(a *fw.Agent, h *Hub) {
	if h == nil {
		hubs.Delete(a)
		return
	}
	hubs.Store(a, h)
}

// Start is the service of bossmode (fw.Service). With
// workbench.bossmode.enabled, it starts the hub of the stream, attaches it
// to the agent, and feeds it the changes of the fleet view. Without it, it
// does nothing. A directory of the page that is not a directory stops the
// start.
func Start(_ context.Context, a *fw.Agent) (io.Closer, error) {
	c, err := Load(a.Config())
	if err != nil {
		return nil, err
	}
	if !c.Enabled {
		return nil, nil
	}
	if err := checkDir(c.Dir); err != nil {
		return nil, err
	}
	h := newHub(a, c)
	if c.Dir != "" && !hasIndex(c.Dir) {
		h.log.Warn("bossmode: the directory of the page holds no index.html, so /bossmode answers 503 until the page is built there", "dir", c.Dir)
	}
	attach(a, h)
	go h.feed()
	return closer{a: a, h: h}, nil
}

// closer stops the hub of an agent and detaches it.
type closer struct {
	a *fw.Agent
	h *Hub
}

func (c closer) Close() error {
	attach(c.a, nil)
	return c.h.Close()
}

// Hub is the stream of one agent: the ring of the newest events, the
// count of the open streams, and the feed from the fleet view.
type Hub struct {
	a   *fw.Agent
	cfg Config
	log *slog.Logger

	mu      sync.Mutex
	ring    []entry // circular: the event with the id n is at n % RingSize
	n       int     // the events in the ring
	last    uint64  // the id of the newest event
	wake    chan struct{}
	streams int
	refused uint64

	keepAlive time.Duration
	life      time.Duration

	// imp imports the skills of an admin (section 20.7).
	imp *skills.Importer

	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}
}

// entry is one event of the ring. data is the JSON of the event. change
// is the change of the fleet view behind a run, member, or task event, so
// the stream asks the view for each caller (fleet.Watcher.Visible). allow
// is the decision of a package for its own event, and nil for an event
// that every caller sees.
type entry struct {
	id     uint64
	kind   string
	key    string
	data   []byte
	change *fleet.Change
	allow  func(context.Context) bool
}

func newHub(a *fw.Agent, c Config) *Hub {
	h := &Hub{
		a:         a,
		cfg:       c,
		log:       a.Logger().With("component", "bossmode"),
		ring:      make([]entry, RingSize),
		wake:      make(chan struct{}),
		keepAlive: KeepAlive,
		life:      StreamLife,
		done:      make(chan struct{}),
		// The ids start at the time of the start in microseconds, so an id
		// of an earlier process, or of another replica that started
		// earlier, is below every id of this process and makes the stream
		// say reset.
		last: uint64(time.Now().UnixMicro()),
	}
	h.ctx, h.cancel = context.WithCancel(context.Background())
	f := skills.NewFetcher()
	h.imp = skills.NewImporter(a, f, skills.NewMarket(c.Marketplaces, f))
	return h
}

// Close ends the feed and every open stream.
func (h *Hub) Close() error {
	h.cancel()
	<-h.done
	return nil
}

// Streams returns the number of open streams.
func (h *Hub) Streams() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.streams
}

// feed puts the changes of the fleet view on the stream for as long as the
// hub lives. It waits for the fleet to start, and it watches again when a
// fleet closes and another takes its place.
func (h *Hub) feed() {
	defer close(h.done)
	for {
		if w, ok := fleet.Of(h.a).(fleet.Watcher); ok {
			for c := range w.Watch(h.ctx) {
				h.addChange(c)
			}
		}
		select {
		case <-h.ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}
}

// addChange puts a change of the fleet view on the stream. The data of a
// run is the run object of the view with member, reactions, and activity,
// and the activity also holds its words, phrase. The data of a member or
// a Task is the change itself.
func (h *Hub) addChange(c fleet.Change) {
	var data []byte
	var err error
	if c.Kind == fleet.ChangeRun && c.Run != nil {
		data, err = runData(c)
	} else {
		data, err = json.Marshal(c)
	}
	if err != nil {
		h.log.Warn("bossmode: encode a change of the fleet", "kind", c.Kind, "member", c.Member, "error", err)
		return
	}
	h.add(c.Kind, c.Key(), data, &c, nil)
}

// streamActivity is the activity of a run on the stream: the activity of
// the kernel (fw.RunActivity) and its words.
type streamActivity struct {
	fw.RunActivity
	Phrase string `json:"phrase"`
}

// runData is the JSON of the run of a change, with its activity in words,
// and "activity": null for a run that does not work, so a page that merges
// the fields clears the old one.
func runData(c fleet.Change) ([]byte, error) {
	b, err := json.Marshal(c.Run)
	if err != nil {
		return nil, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(b, &fields); err != nil {
		return nil, err
	}
	act := json.RawMessage("null")
	if c.Run.Activity != nil {
		if act, err = json.Marshal(streamActivity{RunActivity: *c.Run.Activity, Phrase: c.Activity}); err != nil {
			return nil, err
		}
	}
	fields["activity"] = act
	return json.Marshal(fields)
}

// add puts one event into the ring and wakes the streams. It returns the
// id of the event.
func (h *Hub) add(kind, key string, data []byte, change *fleet.Change, allow func(context.Context) bool) uint64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.last++
	h.ring[h.last%RingSize] = entry{id: h.last, kind: kind, key: key, data: data, change: change, allow: allow}
	if h.n < RingSize {
		h.n++
	}
	close(h.wake)
	h.wake = make(chan struct{})
	return h.last
}

// since returns the events after the id after, the id of the newest
// event, and a channel that closes with the next event. lost is true when
// the ring no longer holds every event after after.
func (h *Hub) since(after uint64) (events []entry, last uint64, lost bool, wake <-chan struct{}) {
	h.mu.Lock()
	defer h.mu.Unlock()
	oldest := h.last - uint64(h.n) + 1
	if after+1 < oldest {
		return nil, h.last, true, h.wake
	}
	for id := after + 1; id <= h.last; id++ {
		events = append(events, h.ring[id%RingSize])
	}
	return events, h.last, false, h.wake
}

// resume returns the id after which a stream starts, and whether it starts
// with reset: Last-Event-ID as it came, the newest id without one, and the
// newest id with reset for a value that is not an id of this process. An
// id that the ring no longer holds says reset in the loop (since).
func (h *Hub) resume(lastEventID string) (uint64, bool) {
	h.mu.Lock()
	last := h.last
	h.mu.Unlock()
	if lastEventID == "" {
		return last, false
	}
	n, err := strconv.ParseUint(lastEventID, 10, 64)
	if err != nil || n > last {
		return last, true
	}
	return n, false
}

// acquire counts a new stream, or refuses it past MaxStreams.
func (h *Hub) acquire() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.streams >= MaxStreams {
		h.refused++
		return false
	}
	h.streams++
	return true
}

func (h *Hub) release() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.streams--
}

// Option shapes an event of Publish.
type Option func(*publishOptions)

type publishOptions struct {
	allow func(context.Context) bool
}

// Allow limits an event of Publish to the callers for whom allow returns
// true. The stream calls allow with the context of each stream, which
// holds the principal of its caller (fw.PrincipalFrom), once per event and
// stream, also when a stream resumes. Without Allow, every caller of the
// stream gets the event.
func Allow(allow func(ctx context.Context) bool) Option {
	return func(o *publishOptions) { o.allow = allow }
}

// Errors of Publish.
var (
	ErrKind     = errors.New("bossmode: the kind of an event is reserved or not a name")
	ErrTooLarge = fmt.Errorf("bossmode: the data of an event is larger than %d bytes", MaxEventBytes)
)

// Publish puts an event on the stream of every caller of every agent of
// the process that runs bossmode, which is one agent in fw-workbench. kind
// names the event, such as bridge.message: 1 to 64 lower-case letters,
// digits, dots, dashes, and underscores, and not one of the kinds of this
// package (run, member, task, and reset). id names the object of the
// event, such as the id of a bridge; the stream sends data alone, as JSON
// and unchanged, so data carries what a page needs. data is at most
// MaxEventBytes as JSON. Without the option Allow, every caller of the
// stream gets the event; with it, only the callers that it allows. While
// bossmode is off, Publish drops the event and returns nil.
func Publish(kind, id string, data any, opts ...Option) error {
	raw, o, err := prepare(kind, data, opts)
	if err != nil {
		return err
	}
	hubs.Range(func(_, v any) bool {
		v.(*Hub).add(kind, id, raw, nil, o.allow)
		return true
	})
	return nil
}

// PublishTo is Publish for the stream of one agent.
func PublishTo(a *fw.Agent, kind, id string, data any, opts ...Option) error {
	raw, o, err := prepare(kind, data, opts)
	if err != nil {
		return err
	}
	if h := Of(a); h != nil {
		h.add(kind, id, raw, nil, o.allow)
	}
	return nil
}

func prepare(kind string, data any, opts []Option) ([]byte, publishOptions, error) {
	var o publishOptions
	for _, opt := range opts {
		opt(&o)
	}
	if !validKind(kind) {
		return nil, o, fmt.Errorf("%w: %q", ErrKind, kind)
	}
	raw, err := json.Marshal(data)
	if err != nil {
		return nil, o, fmt.Errorf("bossmode: encode the data of %s: %w", kind, err)
	}
	if len(raw) > MaxEventBytes {
		return nil, o, fmt.Errorf("%w: %s has %d bytes", ErrTooLarge, kind, len(raw))
	}
	return raw, o, nil
}

// validKind reports whether a package can publish an event of the kind. A
// kind goes on the event line of the stream, so it holds no line break.
func validKind(kind string) bool {
	switch kind {
	case "", KindRun, KindMember, KindTask, KindReset:
		return false
	}
	if len(kind) > 64 {
		return false
	}
	for _, r := range kind {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '.' || r == '-' || r == '_') {
			return false
		}
	}
	return true
}
