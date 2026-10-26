// Package sources turns the events of outside systems into runs
// (docs/ARCHITECTURE.md section 13). A source is an entry of
// workbench.sources in fw.yaml. A webhook source takes events on POST
// /v1/sources/<name>, and a poll source reads an HTTP endpoint at an
// interval. Each source renders its templates over each event into a run
// request of its agent profile and puts it on the queue of the pool that
// runs that profile through fleet.EnqueueFor, the path of the MCP tool
// enqueue, so the fleet wakes the sleeping Tasks that take the queue.
//
// The same event never makes a second run within the window of its
// source. Before the publish, the source claims the key of the event in a
// NATS JetStream key-value bucket that every replica of the front shares
// (nats.go). Only the replica that wins the claim publishes, and without a
// claim nothing is published. The package runs as a service of the kernel
// (fw.RegisterService), so a process that serves has it and a verb that
// runs one piece of work does not, and it counts what each source did for
// a collector of metrics (stats.go).
package sources

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/surface/native"
	"github.com/bdubs00/featherweight-workbench/fleet"
)

func init() {
	fw.RegisterService(Start)
	native.Extend(extension)
}

// registry ties the running sources to their agent.
var registry sync.Map // *fw.Agent -> *Sources

// of returns the sources of an agent, or nil.
func of(a *fw.Agent) *Sources {
	if a == nil {
		return nil
	}
	if v, ok := registry.Load(a); ok {
		return v.(*Sources)
	}
	return nil
}

// attach makes s the sources of a. A nil s detaches.
func attach(a *fw.Agent, s *Sources) {
	if s == nil {
		registry.Delete(a)
		return
	}
	registry.Store(a, s)
}

// Start is the service of the sources (fw.Service). When the configuration
// names sources, it opens the store of the claims, starts the poll
// sources, and attaches the sources to the agent, so the route finds
// them. Without sources it does nothing. A configuration that cannot work
// stops the start.
func Start(ctx context.Context, a *fw.Agent) (io.Closer, error) {
	list, err := Load(a.Config())
	if err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, nil
	}
	if a.Bus() == nil {
		return nil, errors.New("sources: workbench.sources needs a bus, and the agent has none")
	}
	st, err := openStore(ctx, a, list)
	if err != nil {
		return nil, err
	}
	s, err := newSources(ctx, a, list, st)
	if err != nil {
		_ = st.Close()
		return nil, err
	}
	attach(a, s)
	s.start()
	return s, nil
}

// openStore opens the store of the claims for the bus of the agent: the
// JetStream buckets for the nats bus, and the memory for the mem bus,
// which reaches no other process. Any other bus has no store.
func openStore(ctx context.Context, a *fw.Agent, list []Source) (store, error) {
	windows := map[string]time.Duration{}
	for _, s := range list {
		windows[s.Name] = s.DedupFor
	}
	switch d := a.Config().Bus.Driver; d {
	case "nats":
		st, err := openNATS(ctx, a.Config(), a.Secrets(), windows, memberOf(a), a.Logger())
		if err != nil {
			return nil, err
		}
		st.redrives = map[string]Redrive{}
		for _, src := range list {
			if src.Redrive != nil {
				st.redrives[src.Name] = *src.Redrive
			}
		}
		return st, nil
	case "", "mem":
		a.Logger().Warn("sources: the bus is mem, so the de-duplication keys live in memory, and a restart forgets them")
		return newMemStore(windows), nil
	default:
		return nil, fmt.Errorf("sources: the de-duplication of workbench.sources needs the nats bus, and bus.driver is %s", d)
	}
}

// memberOf names this process in a claim: its name in the fleet, else the
// host name.
func memberOf(a *fw.Agent) func() string {
	return func() string {
		if v := fleet.Of(a); v != nil {
			return v.Name()
		}
		host, _ := os.Hostname()
		return host
	}
}

// Sources are the running sources of one agent. Build them with Start.
type Sources struct {
	a      *fw.Agent
	log    *slog.Logger
	store  store
	now    func() time.Time
	list   []*source
	byName map[string]*source

	ctx       context.Context // ends at Close; the polls live in it
	cancel    context.CancelFunc
	wg        sync.WaitGroup
	closeOnce sync.Once
}

// source is one running source. queue is the queue of its jobs when the
// sources started (fleet.QueueFor), whose dead letter the redrive reads.
// moved is the queue that the last publish took, so the log names a move
// to another queue once (noteQueue).
type source struct {
	cfg    Source
	tpl    *templates
	queue  string
	verify verifier // a webhook source
	poll   *poller  // a poll source
	count  *counter

	mu    sync.Mutex
	moved string
}

// newSources builds the sources of an agent from a configuration that
// Load checked. It reads every secret now, so a source whose secret is
// missing stops the start instead of running open.
func newSources(ctx context.Context, a *fw.Agent, list []Source, st store) (*Sources, error) {
	secrets := a.Secrets()
	if secrets == nil {
		secrets = fw.EnvSecrets{}
	}
	s := &Sources{a: a, log: a.Logger().With("component", "sources"), store: st, now: time.Now, byName: map[string]*source{}}
	for _, c := range list {
		tpl, err := compileTemplates(c)
		if err != nil {
			return nil, fmt.Errorf("sources: workbench.sources.%s: %w", c.Name, err)
		}
		// The fleet starts before the sources, so it knows the pools.
		queue, err := fleet.QueueFor(ctx, a, c.Agent, c.Pool)
		if err != nil {
			return nil, fmt.Errorf("sources: workbench.sources.%s: %w", c.Name, err)
		}
		src := &source{cfg: c, tpl: tpl, queue: queue}
		src.count = &counter{st: Stat{Source: c.Name, Kind: c.Kind(), Agent: c.Agent}}
		if c.Poll != nil {
			value, err := secretOf(ctx, secrets, c.Name, "poll.secret", c.Poll.Secret)
			if err != nil {
				return nil, err
			}
			src.poll = newPoller(*c.Poll, value)
		} else {
			src.verify = verifier{mode: c.Auth.Mode, header: c.Auth.Header}
			if c.Auth.Mode == AuthNone {
				s.log.Warn("sources: a webhook source takes events without a credential, because its auth.mode is none", "source", c.Name)
			} else {
				value, err := secretOf(ctx, secrets, c.Name, "auth.secret", c.Auth.Secret)
				if err != nil {
					return nil, err
				}
				src.verify.secret = []byte(value)
			}
		}
		s.list = append(s.list, src)
		s.byName[c.Name] = src
	}
	s.ctx, s.cancel = context.WithCancel(context.Background())
	return s, nil
}

// secretOf reads the secret that the variable name holds, or "" when the
// key names none. An empty or missing secret is an error that names the
// variable and never the value.
func secretOf(ctx context.Context, secrets fw.Secrets, source, key, name string) (string, error) {
	if name == "" {
		return "", nil
	}
	v, err := secrets.Get(ctx, name)
	if err == nil && strings.TrimSpace(v) == "" {
		err = fw.ErrSecretNotFound
	}
	if err != nil {
		return "", fmt.Errorf("sources: workbench.sources.%s: %s names %s, which holds no secret: %w", source, key, name, err)
	}
	return strings.TrimSpace(v), nil
}

// start starts the poll sources and the consumers of the dead letter of
// the sources with redrive.
func (s *Sources) start() {
	for _, src := range s.list {
		if src.cfg.Redrive != nil {
			s.wg.Add(1)
			go s.consumeOutcomes(src)
		}
		if src.poll != nil {
			s.wg.Add(1)
			go s.polls(src)
		}
	}
	names := make([]string, 0, len(s.list))
	for _, src := range s.list {
		names = append(names, src.cfg.Name+" ("+src.cfg.Kind()+")")
	}
	s.log.Info("sources: started", "sources", strings.Join(names, ", "), "store", s.store.Describe())
}

// Close stops the polls, detaches the sources, and closes the store.
func (s *Sources) Close() error {
	var err error
	s.closeOnce.Do(func() {
		s.cancel()
		s.wg.Wait()
		if of(s.a) == s {
			attach(s.a, nil)
		}
		err = s.store.Close()
	})
	return err
}

// Stats returns the counts of the sources, in the order of the
// configuration.
func (s *Sources) Stats() []Stat {
	out := make([]Stat, 0, len(s.list))
	for _, src := range s.list {
		out = append(out, src.count.snapshot())
	}
	return out
}

// noteQueue logs once for each queue that a publish of src took in place
// of the queue of the start of the sources: a pool that runs the agent of
// the source came or went since. The redrive reads the dead letter of the
// queue of the start until the front starts again.
func (s *Sources) noteQueue(src *source, queue string) {
	src.mu.Lock()
	log := queue != src.queue && queue != src.moved
	src.moved = queue
	src.mu.Unlock()
	if log {
		s.log.Warn("sources: the jobs of a source go to another queue than at the start; the redrive reads the dead letter of the first queue until the front starts again",
			"source", src.cfg.Name, "agent", src.cfg.Agent, "queue", queue, "start", src.queue)
	}
}

// orQueue returns queue, else the queue of the start.
func orQueue(queue, start string) string {
	if queue != "" {
		return queue
	}
	return start
}

// delivery is what one delivery of events did. Failed counts the events
// that made no run because the store or the queue failed, and err is that
// failure. queue is the queue that the publish took, empty without one.
type delivery struct {
	accepted   []rendered
	duplicates []rendered
	busy       []rendered
	failed     int
	err        error
	queue      string
}

// deliver claims the key of each event, publishes a run request for each
// claim on the queue of the source, and ends each claim: a claim whose
// request reached the queue is done, and any other claim is released, so
// the next delivery of its event makes the run. A store that does not
// answer stops the claims, and the events after it make no run. It counts
// what it did.
func (s *Sources) deliver(ctx context.Context, src *source, items []rendered) delivery {
	var d delivery
	var claimed []rendered
	var tickets []ticket
	for i, it := range items {
		t, err := s.store.Take(ctx, src.cfg.Name, it.key)
		if err == nil {
			if t.generation != "" {
				it.req.Metadata[MetaGeneration] = t.generation
				if t.attempt > 1 {
					// A redrive is a new run, so it needs a key of its
					// own: a worker returns the failed run for the old
					// key. No name of a source starts with "/", so this
					// key never equals the key of a first attempt.
					it.req.IdempotencyKey = "/source-redrive/" + t.generation
				}
			}
			claimed, tickets = append(claimed, it), append(tickets, t)
			continue
		}
		if errors.Is(err, ErrDuplicate) {
			d.duplicates = append(d.duplicates, it)
			continue
		}
		if errors.Is(err, ErrInFlight) {
			d.busy = append(d.busy, it)
			continue
		}
		d.err, d.failed = err, len(items)-i
		break
	}
	// The end of a claim must happen even when the caller went away.
	end := context.WithoutCancel(ctx)
	for start := 0; start < len(claimed); start += fleet.MaxEnqueue {
		stop := min(start+fleet.MaxEnqueue, len(claimed))
		reqs := make([]fw.RunRequest, 0, stop-start)
		for _, it := range claimed[start:stop] {
			reqs = append(reqs, it.req)
		}
		queue, sent, err := fleet.EnqueueFor(ctx, s.a, src.cfg.Agent, src.cfg.Pool, reqs)
		if queue != "" {
			d.queue = queue
			s.noteQueue(src, queue)
		}
		for j := start; j < start+sent; j++ {
			d.accepted = append(d.accepted, claimed[j])
			if ferr := s.store.Finish(end, tickets[j]); ferr != nil {
				// The claim stays pending. Another delivery of the event
				// takes it after staleClaim and publishes again, with the
				// same idempotency key.
				s.log.Warn("sources: a claim did not end after its publish", "source", src.cfg.Name, "error", ferr)
			}
		}
		if err != nil {
			for j := start + sent; j < len(claimed); j++ {
				if rerr := s.store.Release(end, tickets[j]); rerr != nil {
					// The claim stays pending, and another delivery of the
					// event takes it after staleClaim.
					s.log.Warn("sources: a claim was not released after a failed publish", "source", src.cfg.Name, "error", rerr)
				}
			}
			d.failed += len(claimed) - (start + sent)
			if d.err == nil {
				d.err = fmt.Errorf("sources: the queue %s did not take the run requests: %w", orQueue(queue, src.queue), err)
			}
			break
		}
	}
	now := s.now()
	src.count.accepted(len(d.accepted), now)
	src.count.duplicate(len(d.duplicates))
	src.count.busy(len(d.busy))
	if d.err != nil {
		src.count.refused(d.failed, d.err.Error(), now)
		s.log.Warn("sources: events made no run", "source", src.cfg.Name, "failed", d.failed, "accepted", len(d.accepted), "error", d.err)
	}
	if len(d.accepted) > 0 || len(d.duplicates) > 0 || len(d.busy) > 0 {
		s.log.Info("sources: events delivered", "source", src.cfg.Name, "queue", orQueue(d.queue, src.queue),
			"accepted", len(d.accepted), "duplicate", len(d.duplicates), "busy", len(d.busy))
	}
	return d
}
