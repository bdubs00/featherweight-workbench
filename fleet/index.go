package fleet

// This file keeps the index of the runs (docs/ARCHITECTURE.md section
// 4.16): one entry per run of a Task that the view learned while the run
// was not finished, in a key-value bucket of the bus, <stream>_RUNS. The
// view learns a run from the egress stream and from a read of a live Task,
// and neither tells a front that starts again about a run of a Task that
// sleeps. The index does: at its start the view loads it, so a request for
// such a run, such as a decision, finds the member that holds it, the
// forward wakes the Task, and the kernel answers from its ledger.
//
// The view writes an entry when a run changes its state, not at each
// event, and removes the entry when it drops the run: after the expire
// time of a finished run, or when the Task leaves the view. The writes and
// the load run beside the view, so a bucket that does not exist or does
// not answer never holds the view up. The index holds no text of a run:
// the member, the agent, the person, the lineage, the batch, the mark of a
// run of the front for itself (MetaInternal), the state,
// the pending approval without its input, and the times.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"sync"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/internal/bucket"
)

// IndexBucket is the bucket of the index of the runs: <stream>_RUNS on a
// NATS bus.
var IndexBucket = bucket.Config{
	Name:        "RUNS",
	Description: "The index of the runs of the workers of a featherweight fleet: one entry per run by its id, with its member, its agent, its person, its last known state, and the time. The server writes it.",
	History:     1,
	MaxValue:    4 << 10,
}

// Bounds of the index.
const (
	// indexTimeout bounds one read or write of the bucket.
	indexTimeout = 5 * time.Second
	// indexPauseMax bounds the pause after a load or a write that failed.
	// The pause starts at one second and doubles.
	indexPauseMax = time.Minute
	// indexTries bounds the tries of one write that other replicas race.
	indexTries = 4
)

// OpenIndex opens the bucket of the index on the bus of cfg: the bucket
// <stream>_RUNS of the NATS server for the bus nats, the memory of the
// process for the bus mem, and no bucket for another bus. The function
// that it returns closes the bucket. A test replaces it.
var OpenIndex = func(ctx context.Context, cfg *fw.Config, log *slog.Logger) (bucket.Bucket, func(), error) {
	b, closeFn, err := bucket.Open(ctx, cfg, IndexBucket, log)
	if errors.Is(err, bucket.ErrNoStore) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("fleet: the index of the runs: %w", err)
	}
	return b, closeFn, nil
}

// indexKey matches a run id that the index keeps under its own key. A run
// id of the kernel is a ULID, and an id that does not match stays out of
// the index.
var indexKey = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// indexEntry is the value of one entry of the index.
type indexEntry struct {
	Member    string        `json:"member"`
	Agent     string        `json:"agent"`
	State     fw.RunState   `json:"state"`
	Seq       uint64        `json:"seq"`
	At        time.Time     `json:"at"`
	Principal string        `json:"principal,omitempty"`
	Origin    string        `json:"origin,omitempty"`
	Parent    string        `json:"parent,omitempty"`
	Depth     int           `json:"depth,omitempty"`
	Batch     string        `json:"batch,omitempty"`
	Internal  string        `json:"internal,omitempty"`
	Pending   *indexPending `json:"pending,omitempty"`
	CreatedAt time.Time     `json:"createdAt"`
}

// indexPending is the approval that a run waits for, without its input.
type indexPending struct {
	ID          string    `json:"id"`
	Tool        string    `json:"tool,omitempty"`
	Reason      string    `json:"reason,omitempty"`
	RequestedAt time.Time `json:"requestedAt"`
}

// indexMark is what the view knows that the index holds of a run: in is
// true once the view wrote an entry or loaded one, with the state, the
// member, and the pending approval of that entry.
type indexMark struct {
	in      bool
	state   fw.RunState
	member  string
	pending string
}

// entryOfRun is the entry of a run of the view at the time at.
func entryOfRun(r *platformRun, at time.Time) *indexEntry {
	run := r.run
	e := &indexEntry{Member: r.member, Agent: run.Agent, State: run.State, Seq: run.Seq, At: at.UTC(),
		Principal: run.Principal, Origin: run.Origin, Parent: run.Parent, Depth: run.Depth,
		Batch: run.Metadata[fw.MetaBatch], Internal: Internal(run), CreatedAt: run.CreatedAt}
	if p := run.Pending; p != nil {
		e.Pending = &indexPending{ID: p.ID, Tool: p.Tool, Reason: cut(p.Reason, 512), RequestedAt: p.RequestedAt}
	}
	return e
}

// runOfEntry is the run of the view that an entry gives: the facts of
// the entry and no text.
func runOfEntry(id string, e indexEntry) *fw.Run {
	run := &fw.Run{ID: id, Agent: e.Agent, State: e.State, Seq: e.Seq, Principal: e.Principal, Origin: e.Origin,
		Parent: e.Parent, Depth: e.Depth, CreatedAt: e.CreatedAt, UpdatedAt: e.At}
	if e.Batch != "" || e.Internal != "" {
		run.Metadata = map[string]string{}
		if e.Batch != "" {
			run.Metadata[fw.MetaBatch] = e.Batch
		}
		if e.Internal != "" {
			// A run of the front for itself stays out of the lists also
			// while the view knows only its entry.
			run.Metadata[MetaInternal] = e.Internal
		}
	}
	if p := e.Pending; p != nil && e.State == fw.StateInputRequired {
		run.Pending = &fw.Approval{ID: p.ID, Tool: p.Tool, Reason: p.Reason, RequestedAt: p.RequestedAt}
	}
	return run
}

// pendingID is the id of the approval that a run waits for, or "".
func pendingID(r *fw.Run) string {
	if r.Pending == nil {
		return ""
	}
	return r.Pending.ID
}

// indexOp is one write that waits: an entry to put, or with del a removal
// of the entry of a run whose seq is at most seq.
type indexOp struct {
	entry *indexEntry
	del   bool
	seq   uint64
}

// runIndex writes the index beside the view. The view queues one write per
// run (put, del), and a newer write of a run replaces the one that waits,
// so the queue holds at most one write per run of the view.
type runIndex struct {
	b   bucket.Bucket
	log *slog.Logger

	mu      sync.Mutex
	pending map[string]indexOp
	wake    chan struct{} // capacity 1: a write waits
}

func newRunIndex(b bucket.Bucket, log *slog.Logger) *runIndex {
	return &runIndex{b: b, log: log, pending: map[string]indexOp{}, wake: make(chan struct{}, 1)}
}

// put queues the entry of a run. It never blocks.
func (x *runIndex) put(id string, e *indexEntry) { x.queue(id, indexOp{entry: e}) }

// del queues the removal of the entry of a run whose seq is at most seq.
// It never blocks.
func (x *runIndex) del(id string, seq uint64) { x.queue(id, indexOp{del: true, seq: seq}) }

func (x *runIndex) queue(id string, op indexOp) {
	x.mu.Lock()
	x.pending[id] = op
	x.mu.Unlock()
	select {
	case x.wake <- struct{}{}:
	default:
	}
}

// take returns the writes that wait and empties the queue.
func (x *runIndex) take() map[string]indexOp {
	x.mu.Lock()
	defer x.mu.Unlock()
	if len(x.pending) == 0 {
		return nil
	}
	out := x.pending
	x.pending = map[string]indexOp{}
	return out
}

// requeue puts back the writes that failed, unless a newer write of the
// same run waits.
func (x *runIndex) requeue(failed map[string]indexOp) {
	x.mu.Lock()
	defer x.mu.Unlock()
	for id, op := range failed {
		if _, newer := x.pending[id]; !newer {
			x.pending[id] = op
		}
	}
}

// writeLoop writes the queued entries until ctx ends. A write that fails
// waits for a pause of one second that doubles up to indexPauseMax, and
// the log names the first failure and the first write that works again.
func (x *runIndex) writeLoop(ctx context.Context) {
	pause, failing := time.Duration(0), false
	for {
		batch := x.take()
		if len(batch) == 0 {
			select {
			case <-ctx.Done():
				return
			case <-x.wake:
				continue
			}
		}
		failed := map[string]indexOp{}
		var lastErr error
		for id, op := range batch {
			if ctx.Err() != nil {
				return
			}
			if err := x.write(ctx, id, op); err != nil {
				failed[id], lastErr = op, err
			}
		}
		if len(failed) == 0 {
			if failing {
				x.log.Info("fleet: the index of the runs takes writes again", "bucket", x.b.Describe())
			}
			pause, failing = 0, false
			continue
		}
		x.requeue(failed)
		if !failing {
			x.log.Warn("fleet: write the index of the runs; the front tries again", "bucket", x.b.Describe(), "runs", len(failed), "error", lastErr)
		}
		failing = true
		pause = min(max(2*pause, time.Second), indexPauseMax)
		t := time.NewTimer(pause)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
	}
}

// write applies one write to the bucket. It reads the entry first, so a
// write that the index already holds, from this replica or another one,
// costs no write, and an older state never replaces a newer one. A race
// with another writer reads again, up to indexTries times.
func (x *runIndex) write(ctx context.Context, id string, op indexOp) error {
	for try := 0; try < indexTries; try++ {
		cctx, cancel := context.WithTimeout(ctx, indexTimeout)
		err := x.writeOnce(cctx, id, op)
		cancel()
		if !errors.Is(err, bucket.ErrConflict) {
			return err
		}
	}
	return nil // other replicas write the entry now; the newest write stays
}

func (x *runIndex) writeOnce(ctx context.Context, id string, op indexOp) error {
	cur, err := x.b.Get(ctx, id)
	if errors.Is(err, bucket.ErrNotFound) {
		if op.del {
			return nil
		}
		value, err := json.Marshal(op.entry)
		if err != nil {
			return err
		}
		_, err = x.b.Create(ctx, id, value)
		return err
	}
	if err != nil {
		return err
	}
	var have indexEntry
	known := json.Unmarshal(cur.Value, &have) == nil && have.Member != ""
	if op.del {
		if known && have.Seq > op.seq {
			return nil // another replica knows a newer state of the run
		}
		return x.b.Delete(ctx, id, cur.Revision)
	}
	e := op.entry
	if known && (have.Seq > e.Seq || (have.Seq == e.Seq && have.State == e.State && have.Member == e.Member && pendingOf(have) == pendingOf(*e))) {
		return nil
	}
	value, err := json.Marshal(e)
	if err != nil {
		return err
	}
	_, err = x.b.Update(ctx, id, value, cur.Revision)
	return err
}

func pendingOf(e indexEntry) string {
	if e.Pending == nil {
		return ""
	}
	return e.Pending.ID
}

// read returns every entry of the index. An entry that does not decode is
// removed.
func (x *runIndex) read(ctx context.Context) (map[string]indexEntry, error) {
	cctx, cancel := context.WithTimeout(ctx, indexTimeout)
	keys, err := x.b.Keys(cctx)
	cancel()
	if err != nil {
		return nil, err
	}
	out := make(map[string]indexEntry, len(keys))
	for _, k := range keys {
		cctx, cancel := context.WithTimeout(ctx, indexTimeout)
		e, err := x.b.Get(cctx, k.Key)
		cancel()
		if errors.Is(err, bucket.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		var ie indexEntry
		if json.Unmarshal(e.Value, &ie) != nil || ie.Member == "" || !indexKey.MatchString(k.Key) {
			x.del(k.Key, ^uint64(0))
			continue
		}
		out[k.Key] = ie
	}
	return out, nil
}

// loadIndex reads the index once, at the start of the view, and gives the
// view the runs that it holds (applyIndex). A load that fails is tried
// again after a pause of one second that doubles up to indexPauseMax, so
// a bucket that does not answer yet never holds up the view. Until the
// load works, the view knows the runs of the stream and of the reads.
func (p *platform) loadIndex(ctx context.Context) {
	x := p.index
	pause := time.Second
	for try := 1; ; try++ {
		entries, err := x.read(ctx)
		if err == nil {
			p.applyIndex(entries, time.Now())
			p.f.log.Info("fleet: loaded the index of the runs", "bucket", x.b.Describe(), "runs", len(entries))
			return
		}
		if ctx.Err() != nil {
			return
		}
		if try == 1 {
			p.f.log.Warn("fleet: read the index of the runs; the front tries again, and knows the runs of the sleeping Tasks once it reads it", "bucket", x.b.Describe(), "error", err)
		}
		t := time.NewTimer(pause)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
		pause = min(2*pause, indexPauseMax)
	}
}

// applyIndex gives the view the runs of the index. A run that the view
// holds already keeps its state, because the stream and the reads are
// newer than the index, and takes the facts of the entry only when it has
// no identity yet. A run that the view dropped since it started, a
// finished run older than the expire time, and a run of a Task that the
// Task list no longer holds leave the index. Every other run joins the
// view with its last known state, until an event or a read of its member
// confirms it.
func (p *platform) applyIndex(entries map[string]indexEntry, now time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for id, e := range entries {
		mark := indexMark{in: true, state: e.State, member: e.Member, pending: pendingOf(e)}
		if cur, ok := p.runs[id]; ok {
			cur.idx = mark
			if !cur.queued {
				cur.identifyFrom(runOfEntry(id, e))
			}
			p.noteRun(id, cur, now)
			continue
		}
		if _, dropped := p.gone[id]; dropped || (e.State.Terminal() && now.Sub(e.At) > p.f.expire) {
			p.index.del(id, e.Seq)
			continue
		}
		name, u, atespace := memberOf("ax:" + e.Member)
		if atespace == "" {
			p.index.del(id, e.Seq)
			continue
		}
		m := p.members[name]
		if m == nil {
			if p.listedOnce[atespace] {
				// The Task list no longer holds the Task, so its runs are gone.
				p.index.del(id, e.Seq)
				continue
			}
			// The first read of the Task list places the member, or removes
			// it with its runs.
			m = &platformMember{name: name, url: u, atespace: atespace, seen: e.At}
			p.members[name] = m
		}
		r := &platformRun{run: runOfEntry(id, e), member: name, updated: e.At, queued: true, stateSeq: e.Seq, fromIndex: true, partial: true, idx: mark}
		p.runs[id] = r
		if m.state == MemberLive && !m.filling {
			// A live Task answers at once, so the next poll reads it again
			// and confirms the run, or drops it when the Task lacks it.
			m.filled = false
		}
		p.noteRun(id, r, now)
	}
	p.indexLoaded = true
}

// identifyFrom gives a run that the stream reached before its
// fw.run.queued the facts of the index: the agent, the person, the
// lineage, the batch, the mark of a run of the front for itself, and the
// start. The state of the stream stays. The
// run has no title and no input until a read of its member.
func (r *platformRun) identifyFrom(e *fw.Run) {
	c := r.run
	c.Agent, c.Principal, c.Origin, c.Parent, c.Depth, c.CreatedAt = e.Agent, e.Principal, e.Origin, e.Parent, e.Depth, e.CreatedAt
	for _, k := range []string{fw.MetaBatch, MetaInternal} {
		v := e.Metadata[k]
		if v == "" {
			continue
		}
		if c.Metadata == nil {
			c.Metadata = map[string]string{}
		}
		if c.Metadata[k] == "" {
			c.Metadata[k] = v
		}
	}
	r.queued, r.partial = true, true
}

// noteIndex queues the entry of a run when its state, its member, or its
// pending approval changed since the index last held it: one write per
// change of state, not one per event. A run that the view learns when it
// is already finished gets no entry. Only a run of a Task gets one,
// because only a Task has an address for the forward. The caller holds
// p.mu.
func (p *platform) noteIndex(id string, r *platformRun, now time.Time) {
	if p.index == nil || !r.queued || r.run.ID == "" || !indexKey.MatchString(id) {
		return
	}
	if m := p.members[r.member]; m == nil || m.atespace == "" {
		return
	}
	st, pend := r.run.State, pendingID(r.run)
	if r.idx.in && r.idx.state == st && r.idx.member == r.member && r.idx.pending == pend {
		return
	}
	if !r.idx.in && st.Terminal() {
		return
	}
	r.idx = indexMark{in: true, state: st, member: r.member, pending: pend}
	p.index.put(id, entryOfRun(r, now))
}

// forgetIndex queues the removal of the entry of a run that the view
// drops. The caller holds p.mu.
func (p *platform) forgetIndex(id string, r *platformRun) {
	if p.index == nil || !r.idx.in {
		return
	}
	r.idx = indexMark{}
	p.index.del(id, r.run.Seq)
}

// Settled implements SettledView: the view read the Task list of every
// atespace once, with the runs of the live Tasks, and it loaded the index
// of the runs. A view without an index never settles, because it cannot
// know the runs of the Tasks that sleep.
func (f *Fleet) Settled() bool {
	p := f.platform
	if p == nil || p.index == nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.indexLoaded {
		return false
	}
	for _, a := range p.atespaces {
		if !p.listedOnce[a] {
			return false
		}
	}
	return true
}
