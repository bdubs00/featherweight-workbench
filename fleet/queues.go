package fleet

// This file reads the intake queues of the workers (docs/ARCHITECTURE.md
// section 4.13). A worker that runs the profile <agent> consumes the queue
// "<intake>.<agent>" through one durable consumer that every worker of the
// profile shares. At every interval the front reads, for each queue of a
// profile that it knows, the jobs that no worker took yet and the jobs that
// a worker holds and has not acknowledged, from the server of the bus. It
// also counts the dead letters of each queue, the records that a worker
// sends to "<queue>.dead" for a job that cannot start or that failed on its
// last attempt. When jobs wait and no awake Task that serves the profile
// has room, the front wakes the sleeping Tasks that serve it, as it does
// after its own enqueue (suspend.go).

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// DeadSuffix ends the subject of the dead letters of a queue:
// "<queue>.dead".
const DeadSuffix = ".dead"

// queueReadTimeout bounds one read of the queues from the server.
const queueReadTimeout = 10 * time.Second

// stallIntervals is how many intervals the jobs of a queue can wait
// without one of them leaving before the front wakes the sleeping Tasks
// that serve the profile even though an awake Task seems to have room.
// holdIntervals is how many intervals a Task that a wake for a queue
// reached stays out of the next wake for a queue, so it has time to take a
// job.
const (
	stallIntervals = 3
	holdIntervals  = 4
)

// queueSource reads the intake queues from the server of the bus. The NATS
// driver has one (nats.go); for another driver the front reads no queue.
type queueSource interface {
	// snapshot returns the consumers of the stream, the subject filters of
	// the stream, and the number of messages that the stream stores for
	// each subject that matches filter.
	snapshot(ctx context.Context, filter string) (queueSnapshot, error)
	// lastTime returns the time at which the stream stored its last
	// message on a subject.
	lastTime(ctx context.Context, subject string) (time.Time, error)
}

// queueSnapshot is what the server of the bus says about the queues at
// one moment.
type queueSnapshot struct {
	consumers []consumerState
	filters   []string          // the subjects that the stream captures
	stored    map[string]uint64 // stored messages by subject
}

// consumerState is one durable consumer of the stream: its name, the
// subjects that it takes, the messages that match them and that no worker
// fetched yet, and the messages that a worker fetched and has not
// acknowledged.
type consumerState struct {
	name       string
	filters    []string
	pending    uint64
	ackPending uint64
}

// queueState is what the front knows of one intake queue. depthKnown is
// true when the last read of the server succeeded, and pending and
// ackPending come from it. Without a consumer, no worker ever took the
// queue, so every message that the stream stores for it waits. dead counts
// the dead letters of the queue: with deadStored, the records that the
// stream holds on "<queue>.dead", which every replica reads alike; else the
// records that this replica received since it started. deadAt is the time
// of the last one.
type queueState struct {
	queue, agent string
	depthKnown   bool
	consumer     bool
	pending      uint64
	ackPending   uint64
	dead         uint64
	deadAt       time.Time
	deadStored   bool
	deadKnown    bool // the front counts the dead letters of the queue at all
}

// deadCount is what one replica received on the dead-letter subject of a
// queue.
type deadCount struct {
	n  uint64
	at time.Time
}

// queueWait is what the front remembers of a queue between two reads: the
// jobs that waited, and since when none of them left.
type queueWait struct {
	pending uint64
	since   time.Time
}

// queues is the state of the intake queues of the front.
type queues struct {
	src queueSource // nil when the server of the bus tells nothing

	mu      sync.Mutex
	states  map[string]queueState // by queue
	readAt  time.Time             // when the last read of the server succeeded
	err     string                // the error of the last read, so the log names a new one once
	dead    map[string]*deadCount // by queue, from the subscriptions
	subs    map[string]bool       // the queues whose dead letters the front subscribes to
	stored  map[string]uint64     // the dead letters that the stream held at the last read, by queue
	storeAt map[string]time.Time  // the time of the last of them, by queue
	waits   map[string]queueWait  // by queue; only the read loop touches it
}

func newQueues(src queueSource) *queues {
	return &queues{
		src:     src,
		states:  map[string]queueState{},
		dead:    map[string]*deadCount{},
		subs:    map[string]bool{},
		stored:  map[string]uint64{},
		storeAt: map[string]time.Time{},
		waits:   map[string]queueWait{},
	}
}

// startQueues starts the read of the intake queues when the configuration
// names their prefix.
func (f *Fleet) startQueues(src queueSource) {
	if f.intake == "" {
		return
	}
	f.queues = newQueues(src)
	go f.queueLoop()
}

// queueLoop reads the queues at every interval until the fleet closes.
func (f *Fleet) queueLoop() {
	t := time.NewTicker(f.interval)
	defer t.Stop()
	for {
		f.readQueues(f.ctx, time.Now())
		select {
		case <-f.ctx.Done():
			return
		case <-t.C:
		}
	}
}

// queueAgents lists the profiles whose queues the front reads: the
// profiles of this process, which runs the same configuration as the
// workers, and the profiles that the members of the view serve. A name
// that cannot be a token of a subject is left out.
func (f *Fleet) queueAgents() []string {
	set := map[string]bool{}
	for _, p := range f.a.Profiles() {
		set[p.Name] = true
	}
	if p := f.platform; p != nil {
		p.mu.Lock()
		for _, m := range p.members {
			for ag := range m.agents {
				set[ag] = true
			}
		}
		p.mu.Unlock()
	}
	out := make([]string, 0, len(set))
	for ag := range set {
		if ag != "" && !strings.ContainsAny(ag, " \t\r\n.*>") {
			out = append(out, ag)
		}
	}
	sort.Strings(out)
	return out
}

// readQueues reads the queues once, keeps their state, and wakes the Tasks
// for the jobs that wait.
func (f *Fleet) readQueues(ctx context.Context, now time.Time) {
	q := f.queues
	agents := f.queueAgents()
	names := make([]string, len(agents))
	for i, ag := range agents {
		names[i] = f.intake + "." + ag
	}
	f.subscribeDead(names)
	var snap queueSnapshot
	var err error
	if q.src != nil {
		cctx, cancel := context.WithTimeout(ctx, queueReadTimeout)
		snap, err = q.src.snapshot(cctx, f.intake+".>")
		cancel()
		switch {
		case err != nil && err.Error() != q.err:
			f.log.Warn("fleet: read the intake queues; the queue gauges and the wake on queue depth wait for the next read", "error", err)
			q.err = err.Error()
		case err == nil && q.err != "":
			f.log.Info("fleet: reads the intake queues again")
			q.err = ""
		}
	}
	known := q.src != nil && err == nil
	var depths map[string]queueDepth
	if known {
		depths = depthsOf(snap, names)
		f.readDeadTimes(ctx, depths)
	}
	q.mu.Lock()
	states := make([]queueState, 0, len(names))
	next := make(map[string]queueState, len(names))
	for i, name := range names {
		st := queueState{queue: name, agent: agents[i]}
		if d, ok := depths[name]; ok {
			st.depthKnown, st.consumer, st.pending, st.ackPending = true, d.consumer, d.pending, d.ackPending
			st.deadStored = d.deadStored
		}
		switch {
		case st.deadStored:
			st.dead, st.deadAt, st.deadKnown = q.stored[name], q.storeAt[name], true
		case q.subs[name]:
			if dc := q.dead[name]; dc != nil {
				st.dead, st.deadAt = dc.n, dc.at
			}
			st.deadKnown = true
		}
		states = append(states, st)
		next[name] = st
	}
	q.states = next
	if known {
		q.readAt = now
	}
	q.mu.Unlock()
	f.wakeQueued(states, now)
}

// queueDepth is the depth of one queue in a snapshot.
type queueDepth struct {
	consumer   bool
	pending    uint64
	ackPending uint64
	dead       uint64
	deadStored bool
}

// depthsOf reads the depth of each queue from a snapshot. The consumer of
// a queue is the durable consumer whose subjects name the queue alone; the
// kernel makes one per queue, and when several match, the first by name
// counts. Without a consumer, every message that the stream stores for the
// queue waits. The dead letters count when the stream captures their
// subject.
func depthsOf(snap queueSnapshot, queues []string) map[string]queueDepth {
	cons := append([]consumerState(nil), snap.consumers...)
	sort.Slice(cons, func(i, j int) bool { return cons[i].name < cons[j].name })
	out := make(map[string]queueDepth, len(queues))
	for _, q := range queues {
		var d queueDepth
		for _, c := range cons {
			if len(c.filters) == 1 && c.filters[0] == q {
				d.consumer, d.pending, d.ackPending = true, c.pending, c.ackPending
				break
			}
		}
		if !d.consumer {
			d.pending = snap.stored[q]
		}
		dead := q + DeadSuffix
		for _, filter := range snap.filters {
			if fw.SubjectMatch(filter, dead) {
				d.deadStored = true
				break
			}
		}
		if d.deadStored {
			d.dead = snap.stored[dead]
		}
		out[q] = d
	}
	return out
}

// readDeadTimes keeps the count of the dead letters that the stream holds
// for each queue, and reads the time of the last one when the count
// changed, so a read that finds no new record asks the server nothing.
func (f *Fleet) readDeadTimes(ctx context.Context, depths map[string]queueDepth) {
	q := f.queues
	for name, d := range depths {
		if !d.deadStored {
			continue
		}
		q.mu.Lock()
		same := q.stored[name] == d.dead && (d.dead == 0 || !q.storeAt[name].IsZero())
		q.stored[name] = d.dead
		if d.dead == 0 {
			delete(q.storeAt, name)
		}
		q.mu.Unlock()
		if same || d.dead == 0 {
			continue
		}
		cctx, cancel := context.WithTimeout(ctx, queueReadTimeout)
		at, err := q.src.lastTime(cctx, name+DeadSuffix)
		cancel()
		if err != nil {
			f.log.Warn("fleet: read the last dead letter of a queue", "queue", name, "error", err)
			continue
		}
		q.mu.Lock()
		q.storeAt[name] = at
		q.mu.Unlock()
	}
}

// subscribeDead subscribes to the dead-letter subject of each queue once,
// so the front counts the records that reach it when the stream does not
// store them, and logs each one.
func (f *Fleet) subscribeDead(names []string) {
	if f.bus == nil {
		return
	}
	q := f.queues
	for _, name := range names {
		q.mu.Lock()
		done := q.subs[name]
		q.subs[name] = true
		q.mu.Unlock()
		if done {
			continue
		}
		stop, err := f.bus.Subscribe(f.ctx, name+DeadSuffix, f.onDead(name))
		if err != nil {
			f.log.Warn("fleet: subscribe to the dead letters of a queue; the front counts them from the stream only", "queue", name, "error", err)
			q.mu.Lock()
			delete(q.subs, name)
			q.mu.Unlock()
			// The next read tries again.
			continue
		}
		f.viewMu.Lock()
		if f.closed.Load() {
			f.viewMu.Unlock()
			stop()
			return
		}
		f.stops = append(f.stops, stop)
		f.viewMu.Unlock()
	}
}

// onDead counts one dead letter of a queue. The record names the job, so
// the log line names the event and its run, and nothing of its data.
func (f *Fleet) onDead(queue string) func(fw.Event) error {
	return func(e fw.Event) error {
		at := e.Time
		if at.IsZero() {
			at = time.Now()
		}
		q := f.queues
		q.mu.Lock()
		dc := q.dead[queue]
		if dc == nil {
			dc = &deadCount{}
			q.dead[queue] = dc
		}
		dc.n++
		if at.After(dc.at) {
			dc.at = at
		}
		q.mu.Unlock()
		f.log.Warn("fleet: a job of a queue went to its dead-letter subject", "queue", queue, "id", e.ID, "type", e.Type, "run", e.Subject)
		return nil
	}
}

// queueStates returns the state of every queue that the front reads, by
// queue name, and when the last read of the server succeeded.
func (f *Fleet) queueStates() (map[string]queueState, time.Time) {
	if f.queues == nil {
		return nil, time.Time{}
	}
	q := f.queues
	q.mu.Lock()
	defer q.mu.Unlock()
	out := make(map[string]queueState, len(q.states))
	for k, v := range q.states {
		out[k] = v
	}
	return out, q.readAt
}

// wakeQueued wakes the sleeping Tasks that serve the profile of a queue in
// which jobs wait, when no awake Task that serves the profile has room, or
// when no job left the queue for stallIntervals intervals. An awake Task
// has room when the view holds no run of it that is queued or running: a
// Task under ax runs one run at a time by default. A wake is idempotent, so
// every replica of the front runs this; two replicas can wake one Task,
// which costs a second read of its status.
func (f *Fleet) wakeQueued(states []queueState, now time.Time) {
	p := f.platform
	if p == nil || !f.wakeOnQueue {
		return
	}
	q := f.queues
	room := p.roomFor(now, f.interval)
	for _, st := range states {
		if !st.depthKnown || st.pending == 0 {
			delete(q.waits, st.queue)
			continue
		}
		w, ok := q.waits[st.queue]
		if !ok || st.pending < w.pending {
			w.since = now
		}
		w.pending = st.pending
		q.waits[st.queue] = w
		stalled := now.Sub(w.since) >= stallIntervals*f.interval
		if room[st.agent] && !stalled {
			continue
		}
		if n := f.wakeFor(st.agent, wakeQueue, holdIntervals*f.interval); n > 0 {
			f.log.Info("fleet: jobs wait in a queue; the front wakes the sleeping Tasks that serve it",
				"queue", st.queue, "agent", st.agent, "pending", st.pending, "tasks", n, "stalled", stalled)
		}
	}
}

// roomFor returns the profiles that an awake member with room serves: a
// running Task, or a worker that only the stream knows and that spoke
// within QuietAfter intervals, with no run that is queued or running.
func (p *platform) roomFor(now time.Time, interval time.Duration) map[string]bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	busy := map[string]bool{}
	for _, r := range p.runs {
		if r.run.State == fw.StateQueued || r.run.State == fw.StateRunning {
			busy[r.member] = true
		}
	}
	out := map[string]bool{}
	for _, m := range p.members {
		awake := m.state == MemberLive || (m.state == "" && now.Sub(m.seen) <= QuietAfter*interval)
		if !awake || busy[m.name] {
			continue
		}
		for ag := range m.agents {
			out[ag] = true
		}
	}
	return out
}
