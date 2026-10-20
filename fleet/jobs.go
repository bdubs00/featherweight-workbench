package fleet

// This file keeps the job records of the front (docs/ARCHITECTURE.md
// section 4.9). A worker shows a job only once it takes it from the queue,
// so the view alone cannot tell how much of a job still waits. The front
// published the requests, so it records them: per batch and per person,
// the idempotency keys of the requests that it put on each queue. The tool
// batch counts the jobs of the caller that no Task took yet from that
// record. On a NATS bus the record lives in a key-value bucket of the
// server, so every replica of the front reads the same record; elsewhere,
// or while the bucket does not answer, each replica keeps the records of
// the jobs that it sent.

import (
	"context"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// jobTimeout bounds one read or write of a shared job record.
const jobTimeout = 5 * time.Second

// jobKey names the record of the jobs that one person put on the queues
// under one batch name.
type jobKey struct{ batch, subject string }

// jobRecord holds the idempotency keys of the requests of a job by queue,
// so a job that is sent twice counts once, and the time of the last
// enqueue.
type jobRecord struct {
	Queues    map[string][]string `json:"queues"`
	UpdatedAt time.Time           `json:"updatedAt"`
}

// add puts the ids on the record of a queue, and reports whether the
// record changed. It moves the time of the last enqueue to now, because a
// new publish puts the requests on the queue again, even when it adds no
// id.
func (r *jobRecord) add(queue string, ids []string, now time.Time) bool {
	if r.Queues == nil {
		r.Queues = map[string][]string{}
	}
	have := map[string]bool{}
	for _, id := range r.Queues[queue] {
		have[id] = true
	}
	changed := false
	for _, id := range ids {
		if !have[id] {
			have[id] = true
			r.Queues[queue] = append(r.Queues[queue], id)
			changed = true
		}
	}
	sort.Strings(r.Queues[queue])
	if now.After(r.UpdatedAt) {
		r.UpdatedAt = now
		changed = true
	}
	return changed
}

// count is the number of jobs of the record.
func (r jobRecord) count() int {
	n := 0
	for _, ids := range r.Queues {
		n += len(ids)
	}
	return n
}

// jobStore keeps the job records where every replica of the front reads
// them.
type jobStore interface {
	addJobs(ctx context.Context, k jobKey, queue string, ids []string, now time.Time) error
	jobs(ctx context.Context, k jobKey) (jobRecord, bool, error)
}

// jobBook is the job records of one front: its own records in memory, and
// the shared store when the bus has one.
type jobBook struct {
	shared jobStore
	expire time.Duration
	log    *slog.Logger

	mu    sync.Mutex
	local map[jobKey]*jobRecord
}

func newJobBook(shared jobStore, expire time.Duration, log *slog.Logger) *jobBook {
	return &jobBook{shared: shared, expire: expire, log: log, local: map[jobKey]*jobRecord{}}
}

// add records the requests of a job that went on a queue.
func (b *jobBook) add(ctx context.Context, k jobKey, queue string, ids []string, now time.Time) {
	b.mu.Lock()
	for key, rec := range b.local {
		if now.Sub(rec.UpdatedAt) > b.expire {
			delete(b.local, key)
		}
	}
	rec := b.local[k]
	if rec == nil {
		rec = &jobRecord{}
		b.local[k] = rec
	}
	rec.add(queue, ids, now)
	b.mu.Unlock()
	if b.shared == nil {
		return
	}
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), jobTimeout)
	defer cancel()
	if err := b.shared.addJobs(cctx, k, queue, ids, now); err != nil {
		b.log.Warn("fleet: write the shared job record; another replica of the front counts no queued job of it", "batch", k.batch, "queue", queue, "error", err)
	}
}

// get returns the record of a job, whether one exists, and whether it
// came from the shared store. A shared store that does not answer, or that
// holds no record that this replica wrote, gives the record of this
// replica.
func (b *jobBook) get(ctx context.Context, k jobKey) (rec jobRecord, ok, shared bool) {
	if b.shared != nil {
		cctx, cancel := context.WithTimeout(ctx, jobTimeout)
		rec, ok, err := b.shared.jobs(cctx, k)
		cancel()
		switch {
		case err != nil:
			b.log.Warn("fleet: read the shared job record; the batch counts the jobs that this replica sent", "batch", k.batch, "error", err)
		case ok:
			return rec, true, true
		}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if r := b.local[k]; r != nil && time.Since(r.UpdatedAt) <= b.expire {
		c := jobRecord{Queues: map[string][]string{}, UpdatedAt: r.UpdatedAt}
		for q, ids := range r.Queues {
			c.Queues[q] = append([]string(nil), ids...)
		}
		return c, true, false
	}
	return jobRecord{}, false, b.shared != nil
}

// jobID is the id of a request in its job record: the idempotency key
// without the batch name in front, else the key, and a new id for a
// request without a key, which counts each time that it goes out.
func jobID(batch string, req fw.RunRequest) string {
	key := req.IdempotencyKey
	if key == "" {
		return "~" + fw.NewID()
	}
	if rest, ok := strings.CutPrefix(key, batch+"/"); ok && rest != "" {
		return rest
	}
	return key
}

// recordJobs records the requests that went on a queue, by batch and
// person. A request without a batch has no job.
func (f *Fleet) recordJobs(ctx context.Context, queue string, reqs []fw.RunRequest) {
	if f.jobs == nil {
		return
	}
	byKey := map[jobKey][]string{}
	var order []jobKey
	for _, req := range reqs {
		batch := req.Metadata[fw.MetaBatch]
		if batch == "" {
			continue
		}
		k := jobKey{batch: batch, subject: req.Principal}
		if _, ok := byKey[k]; !ok {
			order = append(order, k)
		}
		byKey[k] = append(byKey[k], jobID(batch, req))
	}
	now := time.Now().UTC()
	for _, k := range order {
		f.jobs.add(ctx, k, queue, byKey[k], now)
	}
}

// queuedJobs counts the jobs of the caller in a batch and those that no
// Task took yet (BatchRuns). runs are the runs of the batch that the view
// holds and that the caller can view, and forgotten the runs of the batch
// that it dropped.
func (f *Fleet) queuedJobs(ctx context.Context, batch string, runs []*fw.Run, forgotten map[string][]*fw.Run) (jobs, queued int, shared bool) {
	p := fw.PrincipalFrom(ctx)
	if f.jobs == nil || p == nil || p.Subject == "" {
		return 0, 0, false
	}
	rec, ok, shared := f.jobs.get(ctx, jobKey{batch: batch, subject: p.Subject})
	if !ok {
		return 0, 0, shared
	}
	jobs = rec.count()
	taken := 0
	took := func(r *fw.Run) bool {
		return r.Parent == "" && r.Origin == fw.OriginBus && r.Principal == p.Subject
	}
	for _, r := range runs {
		if took(r) {
			taken++
		}
	}
	for _, rs := range forgotten {
		for _, r := range rs {
			if took(r) {
				taken++
			}
		}
	}
	queued = max(jobs-taken, 0)
	// A job that left the queue without a run that the view knows, such as
	// a request that a worker cannot start, waits nowhere. The queues
	// bound the count once the front read them after the last enqueue.
	states, readAt := f.queueStates()
	if readAt.After(rec.UpdatedAt.Add(time.Second)) {
		var held uint64
		all := true
		for q := range rec.Queues {
			st, ok := states[q]
			if !ok || !st.depthKnown {
				all = false
				break
			}
			held += st.pending + st.ackPending
		}
		if all && uint64(queued) > held {
			queued = int(held)
		}
	}
	return jobs, queued, shared
}
