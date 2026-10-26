package fleet

// This file keeps the archive of the runs of the workers
// (docs/ARCHITECTURE.md section 4.18). A run lives in the ledger of its
// Task, and a new Task starts with an empty /workspace, so a pool that
// replaced a Task erased its runs. The egress stream of the workers holds
// their events, so the front reads that stream from its start with a
// durable consumer of the stream of the bus and keeps each run in a
// bucket: the summary of the run, the projection of its events, and each
// event that the stream carried. When no member holds a run, the routes of
// runs answer from the archive, and the run carries "archived": true. A
// run of the archive is read-only: a write to it answers 409.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/internal/bucket"
	"github.com/nats-io/nats.go/jetstream"
)

// ArchiveBucket is the bucket of the archive of the runs.
var ArchiveBucket = bucket.Config{
	Name:        "ARCHIVE",
	Description: "The archive of the runs of the workers of a featherweight fleet: the summary of each run and each event that the egress stream carried. The server writes it from a durable consumer of the stream.",
	History:     1,
	MaxValue:    1 << 20,
}

// ArchiveConsumer is the durable consumer of the stream of the bus that
// feeds the archive. Every replica of the front shares it, and it hands
// out one event at a time, so the replicas write the archive in the order
// of the stream.
const ArchiveConsumer = "fw-workbench-archive"

// ReadOnlyRun is the detail of the answer 409 to a write to a run of the
// archive.
const ReadOnlyRun = "The worker of this conversation was replaced, so the conversation is read-only. Start a new one."

// ErrArchived means that a run lives in the archive alone, so it takes no
// write.
var ErrArchived = errors.New(ReadOnlyRun)

// Bounds of the archive.
const (
	// archiveSeen is how many events a summary remembers, so a second
	// delivery of an event applies once.
	archiveSeen = 64
	// archiveRefresh is how often a replica reads the summaries that the
	// other replica wrote.
	archiveRefresh = 10 * time.Second
	// archiveAttempts bounds the tries of a write of a summary that
	// another writer races.
	archiveAttempts = 8
	// archiveTimeout bounds one write of the archive.
	archiveTimeout = 10 * time.Second
)

// OpenArchive opens the bucket of the archive on the bus of cfg: the
// bucket <stream>_ARCHIVE of the NATS server for the bus nats, the memory
// of the process for the bus mem, and no archive for another bus. A test
// replaces it.
var OpenArchive = func(ctx context.Context, cfg *fw.Config, log *slog.Logger) (bucket.Bucket, func(), error) {
	b, closeFn, err := bucket.Open(ctx, cfg, ArchiveBucket, log)
	if errors.Is(err, bucket.ErrNoStore) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("fleet: the archive of the runs: %w", err)
	}
	return b, closeFn, nil
}

// OpenArchiveSource feeds the archive with the events of the egress
// stream, each at least once, until the function that it returns stops
// it. On the bus nats it reads the stream of the bus from its start with
// the durable consumer ArchiveConsumer, which every replica shares and
// which hands out one event at a time. On another bus it feeds the
// archive from the subscription of the egress stream, from the start of
// the process. A test replaces it.
var OpenArchiveSource = func(ctx context.Context, f *Fleet, cfg *fw.Config, egress string, h func(fw.Event) error) (func(), error) {
	if strings.TrimSpace(cfg.Bus.Driver) == "nats" {
		return natsArchiveSource(ctx, cfg, egress, h, f.log)
	}
	if f.bus == nil {
		return nil, nil
	}
	stop, err := f.bus.Subscribe(f.ctx, egress+".>", h)
	if err != nil {
		return nil, fmt.Errorf("fleet: subscribe the archive to %s: %w", egress, err)
	}
	return stop, nil
}

// natsArchiveSource reads the stream of the bus with the durable consumer
// of the archive. A stream that does not exist yet, and a server that
// does not answer, make it try again every few seconds. When the archive
// fails to write an event, the event comes again after a pause.
func natsArchiveSource(ctx context.Context, cfg *fw.Config, egress string, h func(fw.Event) error, log *slog.Logger) (func(), error) {
	n, err := openNATS(ctx, cfg, 0, log)
	if err != nil {
		return nil, err
	}
	cctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			if err := consumeArchive(cctx, n, egress, h, log); err != nil && cctx.Err() == nil {
				log.Warn("fleet: the archive reads the stream again after a pause", "stream", n.stream, "error", err)
			}
			select {
			case <-cctx.Done():
				return
			case <-time.After(5 * time.Second):
			}
		}
	}()
	return func() {
		// The close of the connection ends a request that waits for a
		// server that does not answer.
		cancel()
		n.close()
		<-done
	}, nil
}

// consumeArchive runs the consumer of the archive until ctx ends or the
// consumer fails.
func consumeArchive(ctx context.Context, n *natsServer, egress string, h func(fw.Event) error, log *slog.Logger) error {
	cons, err := n.js.CreateOrUpdateConsumer(ctx, n.stream, jetstream.ConsumerConfig{
		Durable:       ArchiveConsumer,
		Description:   "The archive of the runs of a featherweight front: every replica reads the egress stream of the workers through it.",
		FilterSubject: egress + ".>",
		DeliverPolicy: jetstream.DeliverAllPolicy,
		AckPolicy:     jetstream.AckExplicitPolicy,
		AckWait:       time.Minute,
		MaxAckPending: 1,
	})
	if err != nil {
		return fmt.Errorf("the consumer %s of the stream %s: %w", ArchiveConsumer, n.stream, err)
	}
	failed := make(chan error, 1)
	cc, err := cons.Consume(func(m jetstream.Msg) {
		var e fw.Event
		if json.Unmarshal(m.Data(), &e) != nil {
			_ = m.Term()
			return
		}
		if err := h(e); err != nil {
			log.Warn("fleet: the archive could not keep an event; it comes again", "run", e.Subject, "type", e.Type, "error", err)
			_ = m.NakWithDelay(5 * time.Second)
			return
		}
		_ = m.Ack()
	}, jetstream.ConsumeErrHandler(func(_ jetstream.ConsumeContext, err error) {
		if errors.Is(err, jetstream.ErrConsumerDeleted) || errors.Is(err, jetstream.ErrConsumerNotFound) {
			select {
			case failed <- err:
			default:
			}
		}
	}))
	if err != nil {
		return fmt.Errorf("consume %s: %w", ArchiveConsumer, err)
	}
	defer cc.Stop()
	select {
	case <-ctx.Done():
		return nil
	case err := <-failed:
		return err
	}
}

// archivedRun is the summary of a run in the archive: the projection of
// its events, the member that held it, the parent that a fw.run.child
// event of another run names, and the keys of the last events that the
// summary applied.
type archivedRun struct {
	Run     *fw.Run   `json:"run,omitempty"`
	Ended   time.Time `json:"ended,omitzero"`
	Queued  bool      `json:"queued,omitempty"`
	Member  string    `json:"member,omitempty"`
	Parent  string    `json:"parent,omitempty"`
	Events  int       `json:"events"`
	Seen    []string  `json:"seen,omitempty"`
	Updated time.Time `json:"updated"`
}

// listed returns the run of the summary as the lists show it, with its
// member, or nil for a summary that knows no run yet: no fw.run.queued gave
// it its identity.
func (r archivedRun) listed() *fw.Run {
	if r.Run == nil || r.Run.ID == "" || !r.Queued {
		return nil
	}
	c := *r.Run
	c.Member = r.Member
	if c.Parent == "" && r.Parent != "" {
		c.Parent, c.Depth = r.Parent, max(c.Depth, 1)
	}
	return &c
}

func archiveRunKey(id string) string { return "run." + id }

// archiveEventsOf is the first token of the keys of the events of a run,
// so a NATS server lists the keys of one run alone (bucket.KeysUnder).
func archiveEventsOf(id string) string { return "events_" + id }

// archiveEventKey is the key of an event of a run: its sequence number,
// so the keys of a run sort in the order of its ledger, and its id, so a
// second delivery finds the event.
func archiveEventKey(id string, e fw.Event) string {
	name := e.ID
	if name == "" {
		name = e.Type
	}
	return fmt.Sprintf("%s.%020d.%s", archiveEventsOf(id), e.Seq, base64.RawURLEncoding.EncodeToString([]byte(name)))
}

// archive is the archive of the runs in a bucket, with a copy of the
// summaries in memory for the lists.
type archive struct {
	b      bucket.Bucket
	log    *slog.Logger
	prices fw.Prices

	mu      sync.Mutex
	runs    map[string]archivedRun // by run id
	readAt  time.Time
	loaded  bool
	reading sync.Mutex
}

func newArchive(b bucket.Bucket, prices fw.Prices, log *slog.Logger) *archive {
	return &archive{b: b, prices: prices, log: log, runs: map[string]archivedRun{}}
}

// add keeps one event of the egress stream: the event under its own key,
// and the projection of the run in its summary. An event that the archive
// holds already changes nothing, so a second delivery and a second
// replica write once. A fw.run.child event also names the parent in the
// summary of the child.
func (a *archive) add(ctx context.Context, e fw.Event) error {
	id := e.Subject
	if id == "" || !indexKey.MatchString(id) || e.Type == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, archiveTimeout)
	defer cancel()
	key := archiveEventKey(id, e)
	raw, err := json.Marshal(e)
	if err != nil {
		return nil
	}
	if ArchiveBucket.MaxValue > 0 && len(raw) > ArchiveBucket.MaxValue {
		// The summary still applies the event. The archive keeps it
		// without its data.
		c := e
		c.Data = nil
		raw, _ = json.Marshal(c)
	}
	if _, err := a.b.Create(ctx, key, raw); err != nil && !errors.Is(err, bucket.ErrConflict) {
		return fmt.Errorf("fleet: keep the event %s of the run %s: %w", e.Type, id, err)
	}
	member, _, _ := memberOf(strings.TrimSpace(e.Node))
	if err := a.modify(ctx, id, func(r *archivedRun) bool {
		if slices.Contains(r.Seen, key) {
			return false
		}
		if r.Run == nil {
			r.Run = &fw.Run{}
		}
		// The stream keeps the order of a run, but a bus can bring an event
		// late. A late fw.run.queued still gives the run its identity, and
		// a late end still ends a run that did not end. Another late event
		// changes nothing in the summary, and the archive keeps it.
		switch late := e.Seq != 0 && e.Seq < r.Run.Seq; {
		case !late:
			r.Run.Apply(e, a.prices)
			r.Ended = endedAfter(r.Ended, r.Run, e)
		case e.Type == fw.EvRunQueued && !r.Queued:
			r.Run.Apply(e, a.prices)
		case (e.Type == fw.EvRunFinished || e.Type == fw.EvRunSuspended) && !r.Run.State.Terminal():
			r.Run.Apply(e, a.prices)
			r.Ended = endedAfter(r.Ended, r.Run, e)
		}
		if e.Type == fw.EvRunQueued {
			r.Queued = true
		}
		if member != "" {
			r.Member = member
		}
		r.Events++
		r.Seen = append(r.Seen, key)
		if len(r.Seen) > archiveSeen {
			r.Seen = slices.Clone(r.Seen[len(r.Seen)-archiveSeen:])
		}
		return true
	}); err != nil {
		return err
	}
	if e.Type == fw.EvRunChild {
		var d fw.RunChildData
		if e.Decode(&d) == nil && indexKey.MatchString(d.Child) && d.Child != id {
			return a.modify(ctx, d.Child, func(r *archivedRun) bool {
				if r.Parent == id {
					return false
				}
				r.Parent = id
				return true
			})
		}
	}
	return nil
}

// modify changes the summary of a run with f and writes it at the
// revision that it read. f reports whether it changed the summary.
func (a *archive) modify(ctx context.Context, id string, f func(*archivedRun) bool) error {
	var next archivedRun
	wrote, err := bucket.Modify(ctx, a.b, archiveRunKey(id), archiveAttempts, func(cur []byte, exists bool) ([]byte, error) {
		var r archivedRun
		if exists && json.Unmarshal(cur, &r) != nil {
			r = archivedRun{}
		}
		if !f(&r) {
			return nil, nil
		}
		r.Updated = time.Now().UTC()
		next = r
		return json.Marshal(r)
	})
	if err != nil {
		return fmt.Errorf("fleet: keep the summary of the run %s: %w", id, err)
	}
	if wrote {
		a.mu.Lock()
		a.runs[id] = next
		a.mu.Unlock()
	}
	return nil
}

// summaries returns the copy of the summaries of the archive, by run id.
// At most once in archiveRefresh, it starts a read of the summaries that
// changed in the bucket in the background, so a replica learns the runs
// that the other replica wrote, and a request never waits for the bucket.
// A bucket that does not answer leaves the copy as it is.
func (a *archive) summaries(context.Context) map[string]archivedRun {
	a.mu.Lock()
	due := !a.loaded || time.Since(a.readAt) >= archiveRefresh
	a.mu.Unlock()
	if due && a.reading.TryLock() {
		go func() {
			defer a.reading.Unlock()
			a.refresh(context.Background())
		}()
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make(map[string]archivedRun, len(a.runs))
	for id, r := range a.runs {
		out[id] = r
	}
	return out
}

// refresh reads the summaries of the bucket that the copy does not hold,
// and those of the runs that are not finished.
func (a *archive) refresh(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, archiveTimeout)
	defer cancel()
	keys, err := bucket.KeysUnder(ctx, a.b, "run")
	if err != nil {
		a.log.Warn("fleet: read the summaries of the archive", "error", err)
		a.mu.Lock()
		a.readAt = time.Now()
		a.mu.Unlock()
		return
	}
	a.mu.Lock()
	var want []string
	for _, k := range keys {
		id := strings.TrimPrefix(k, "run.")
		r, ok := a.runs[id]
		if !ok || r.Run == nil || !r.Run.State.Terminal() {
			want = append(want, id)
		}
	}
	a.mu.Unlock()
	for _, id := range want {
		e, err := a.b.Get(ctx, archiveRunKey(id))
		if err != nil {
			continue
		}
		var r archivedRun
		if json.Unmarshal(e.Value, &r) != nil {
			continue
		}
		a.mu.Lock()
		a.runs[id] = r
		a.mu.Unlock()
	}
	a.mu.Lock()
	a.loaded, a.readAt = true, time.Now()
	a.mu.Unlock()
}

// events reads the events of a run from the archive, in the order of its
// ledger.
func (a *archive) events(ctx context.Context, id string) ([]fw.Event, error) {
	keys, err := bucket.KeysUnder(ctx, a.b, archiveEventsOf(id))
	if err != nil {
		return nil, fmt.Errorf("fleet: read the events of the run %s in the archive: %w", id, err)
	}
	out := []fw.Event{}
	for _, k := range keys {
		e, err := a.b.Get(ctx, k)
		if errors.Is(err, bucket.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("fleet: read an event of the run %s in the archive: %w", id, err)
		}
		var ev fw.Event
		if json.Unmarshal(e.Value, &ev) == nil {
			out = append(out, ev)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
	return out, nil
}

// startArchive opens the archive and its source, when the fleet reads an
// egress stream and the bus keeps a store. On a NATS bus it opens them in
// the background, because a dial of a server that does not answer yet
// takes seconds, and the start of the front waits for no store. A source
// that does not open leaves the archive to the reads, and the log says
// why.
func (f *Fleet) startArchive(ctx context.Context, cfg *fw.Config, egress string) {
	if strings.TrimSpace(egress) == "" {
		return
	}
	open := func() {
		b, closeFn, err := OpenArchive(f.ctx, cfg, f.log)
		if err != nil {
			f.log.Warn("fleet: the archive of the runs is off", "error", err)
			return
		}
		if b == nil {
			where, _ := bucket.Where(cfg, ArchiveBucket)
			f.log.Warn("fleet: the archive of the runs is off; a run of a replaced worker is gone. The archive lives "+where, "bus", cfg.Bus.Driver)
			return
		}
		a := newArchive(b, f.a.Prices(), f.log)
		f.archiveMu.Lock()
		if f.closed.Load() {
			f.archiveMu.Unlock()
			if closeFn != nil {
				closeFn()
			}
			return
		}
		f.closeArchive = closeFn
		f.archivePtr.Store(a)
		f.archiveMu.Unlock()
		// The summaries that the bucket holds from before this start load
		// in the background.
		a.summaries(f.ctx)
		stop, err := OpenArchiveSource(f.ctx, f, cfg, egress, func(e fw.Event) error { return a.add(f.ctx, e) })
		if err != nil {
			f.log.Warn("fleet: the archive reads no new event", "error", err)
			return
		}
		if stop != nil {
			f.viewMu.Lock()
			if f.closed.Load() {
				f.viewMu.Unlock()
				stop()
				return
			}
			f.stops = append(f.stops, stop)
			f.viewMu.Unlock()
		}
		f.log.Info("fleet keeps the archive of the runs", "archive", b.Describe(), "consumer", ArchiveConsumer)
	}
	if strings.TrimSpace(cfg.Bus.Driver) == "nats" {
		go open()
		return
	}
	open()
}

// arch returns the archive of the fleet, or nil before it opened or
// without one.
func (f *Fleet) arch() *archive { return f.archivePtr.Load() }

// held reports whether a member of the view or this process holds the
// run, so the archive does not answer for it.
func (f *Fleet) held(id string) bool {
	if _, err := f.a.Get(context.Background(), id); !errors.Is(err, fw.ErrRunNotFound) {
		return true
	}
	_, found := f.find(id)
	return found != nil
}

// archivedFor returns the runs of the archive that no member holds and
// that the caller in ctx can view, each with its member. The front decides
// as for the runs of a member (allow), with the access block of its own
// profile of each agent.
func (f *Fleet) archivedFor(ctx context.Context, keep func(*fw.Run) bool) ([]*fw.Run, error) {
	arch := f.arch()
	if arch == nil {
		return nil, nil
	}
	byMember := map[string][]*fw.Run{}
	for id, r := range arch.summaries(ctx) {
		run := r.listed()
		if run == nil || (keep != nil && !keep(run)) || f.held(id) {
			continue
		}
		byMember[run.Member] = append(byMember[run.Member], run)
	}
	cfg := f.a.Config()
	var out []*fw.Run
	for member, runs := range byMember {
		rec := &Record{Name: member, Access: Access{Mode: f.a.AccessMode(), Default: cfg.Access.Default, Admins: cfg.Access.Admins}, Runs: runs}
		seen := map[string]bool{}
		for _, r := range runs {
			if r.Agent == "" || seen[r.Agent] {
				continue
			}
			seen[r.Agent] = true
			ag := RecordAgent{Name: r.Agent}
			if prof, ok := f.a.ProfileNamed(r.Agent); ok {
				ag.Description, ag.Access = prof.Description, prof.Access
			}
			rec.Agents = append(rec.Agents, ag)
		}
		ok, _, err := f.allow(ctx, rec, runs, false)
		if err != nil {
			return nil, err
		}
		for i, r := range runs {
			if ok[i] {
				out = append(out, r)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.After(out[j].CreatedAt)
		}
		return out[i].ID > out[j].ID
	})
	return out, nil
}

// ArchivedRuns returns the runs of the archive that no member of the fleet
// of a holds and that the caller in ctx can view, newest first, each with
// its member. member keeps only the runs of that member, and parent only
// the children of that run. It returns none when the fleet keeps no
// archive.
func ArchivedRuns(ctx context.Context, a *fw.Agent, member, parent string) ([]*fw.Run, error) {
	f, _ := Of(a).(*Fleet)
	if f == nil {
		return nil, nil
	}
	return f.archivedFor(ctx, func(r *fw.Run) bool {
		return (member == "" || r.Member == member) && (parent == "" || r.Parent == parent)
	})
}

// ArchivedRun returns the run id of the archive when no member holds it
// and the caller in ctx can view it, and reports false otherwise.
func ArchivedRun(ctx context.Context, a *fw.Agent, id string) (*fw.Run, bool, error) {
	f, _ := Of(a).(*Fleet)
	if f == nil || f.arch() == nil {
		return nil, false, nil
	}
	r, ok := f.arch().summaries(ctx)[id]
	run := r.listed()
	if !ok || run == nil || f.held(id) {
		return nil, false, nil
	}
	list, err := f.archivedFor(ctx, func(c *fw.Run) bool { return c.ID == id })
	if err != nil || len(list) == 0 {
		return nil, false, err
	}
	return list[0], true, nil
}

// ArchivedLedger returns the run id of the archive and its events, as
// ArchivedRun decides. The events are those of the egress stream: the
// stream carries no fw.model.responded, and it cuts each long string to
// EgressMaxBytes. TranscriptEvents turns them into the events of a
// transcript.
func ArchivedLedger(ctx context.Context, a *fw.Agent, id string) (*fw.Run, []fw.Event, bool, error) {
	run, ok, err := ArchivedRun(ctx, a, id)
	if err != nil || !ok {
		return nil, nil, ok, err
	}
	f := Of(a).(*Fleet)
	events, err := f.arch().events(ctx, id)
	if err != nil {
		return nil, nil, false, err
	}
	return run, events, true, nil
}

// IsArchived reports whether the run with the id lives in the archive
// alone: the archive holds it, and no member holds it.
func IsArchived(ctx context.Context, a *fw.Agent, id string) bool {
	f, _ := Of(a).(*Fleet)
	if f == nil || f.arch() == nil {
		return false
	}
	_, ok := f.arch().summaries(ctx)[id]
	return ok && !f.held(id)
}

// TranscriptEvents turns the events of a run of the archive into events
// that a transcript and a result read. The stream carries no
// fw.model.responded, so each fw.tool.requested becomes the tool call of
// an answer of the model, and the output of each fw.run.finished becomes
// the last answer of the model before the end.
func TranscriptEvents(events []fw.Event) []fw.Event {
	out := make([]fw.Event, 0, len(events)+len(events)/4)
	for _, e := range events {
		switch e.Type {
		case fw.EvToolRequested:
			var d fw.ToolRequestedData
			if e.Decode(&d) == nil {
				out = append(out, modelEvent(e, fw.Block{Type: fw.BlockToolUse, ID: d.CallID, Name: d.Tool, Input: d.Input}))
			}
		case fw.EvRunFinished:
			var d fw.RunFinishedData
			if e.Decode(&d) == nil && strings.TrimSpace(d.Output) != "" {
				out = append(out, modelEvent(e, fw.Block{Type: fw.BlockText, Text: d.Output}))
			}
		}
		out = append(out, e)
	}
	return out
}

// modelEvent returns a fw.model.responded at the place of e with one
// block.
func modelEvent(e fw.Event, b fw.Block) fw.Event {
	data, _ := json.Marshal(fw.ModelRespondedData{Content: []fw.Block{b}})
	return fw.Event{SpecVersion: e.SpecVersion, ID: e.ID + ".model", Source: e.Source, Type: fw.EvModelResponded, Time: e.Time, Subject: e.Subject, Seq: e.Seq, Node: e.Node, Data: data}
}
