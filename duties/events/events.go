// Package events starts the duties on events of the fleet: when.event of a
// duty (docs/ARCHITECTURE.md section 23.5). It serves the six kinds of the
// contract, each from a source that every replica of the front sees, or
// from the replica that made the change:
//
//   - run.failed: a top-level run that failed in the last 15 minutes, from
//     the runs of this process and of the view of the fleet
//   - question.waiting: a run that waits for a person, once it waited for
//     when.event.after
//   - page.changed: a change of a page of the wiki (pages.OnChange)
//   - bridge.closed: the close of a bridge (bridges.OnMessage)
//   - routine.ready: a routine that may go live (routines.OnChange)
//   - cap.reached: a duty that reached a daily cap (duties.OnNotice)
//
// For each event that a duty waits for, a replica creates the start of
// that duty and that event in the key-value bucket <stream>_DUTY_EVENTS
// (memory on the bus mem). Of two replicas that see one event, one creates
// it. A start waits for when.event.after, and then one replica claims it
// with a compare-and-swap and calls duties.Service.FireChain, so each
// event starts each duty once. A start carries the depth of its chain: an
// event of a run that an event started goes one deeper, and the duties
// stop a chain at a depth of 3. A run of a duty never starts that same
// duty again through its own event.
package events

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/bridges"
	"github.com/bdubs00/featherweight-workbench/duties"
	"github.com/bdubs00/featherweight-workbench/fleet"
	"github.com/bdubs00/featherweight-workbench/internal/bucket"
	"github.com/bdubs00/featherweight-workbench/pages"
	"github.com/bdubs00/featherweight-workbench/routines"
)

// Kinds are the kinds of event that this package serves, sorted.
var Kinds = []string{
	duties.EventBridgeClosed,
	duties.EventCapReached,
	duties.EventPageChanged,
	duties.EventQuestionWaiting,
	duties.EventRoutineReady,
	duties.EventRunFailed,
}

func init() {
	fw.RegisterService(Start)
	duties.RegisterEvents(Kinds, Check)
	duties.OnNotice(onNotice)
	pages.OnChange(onPage)
	bridges.OnMessage(onMessage)
	routines.OnChange(onRoutine)
}

// eventsBucket holds one entry per event and duty that it starts.
var eventsBucket = bucket.Config{
	Name:        "DUTY_EVENTS",
	Description: "The events of a featherweight fleet that start duties, under start.<duty>.<hash of the event>: the event, when the duty starts, and what the start did.",
	History:     1,
	MaxValue:    64 << 10,
	TTL:         8 * 24 * time.Hour,
}

// Bounds of the events.
const (
	// MaxAfter is the longest when.event.after.
	MaxAfter = 24 * time.Hour
	// failedWindow is how old a failed run can be and still start a duty,
	// so a replica that starts reads no old failure as new.
	failedWindow = 15 * time.Minute
	// defaultEvery is the longest pause between two passes.
	defaultEvery = 5 * time.Second
	// claimFor is how long a claim of a start holds before another
	// replica takes it over.
	claimFor = 2 * time.Minute
	// maxEventBytes bounds the JSON of an event for {event}.
	maxEventBytes = 4 << 10
)

// States of a start.
const (
	stateDue   = "due"   // it waits for its time
	stateFired = "fired" // a replica fired it
)

// Check lists the problems of when.event of a duty, each as
// "when.event.<field>: text". duties.RegisterEvents takes it.
func Check(e *duties.Event) []string {
	var p []string
	if after := strings.TrimSpace(e.After); after != "" && after != "0" {
		if d, err := time.ParseDuration(after); err == nil && (d < 0 || d > MaxAfter) {
			p = append(p, fmt.Sprintf("when.event.after: %s is out of bounds; write a span from 0 to 24h, such as 1h.", after))
		}
	}
	if e.Agent != "" {
		switch {
		case e.Kind == duties.EventBridgeClosed:
			p = append(p, "when.event.agent: a closed bridge names no agent; leave agent out.")
		case strings.ContainsAny(e.Agent, " \t\r\n/*") || len(e.Agent) > 128:
			p = append(p, fmt.Sprintf("when.event.agent: %q is not the name of an agent.", e.Agent))
		}
	}
	if e.Path != "" {
		if e.Kind != duties.EventPageChanged {
			p = append(p, "when.event.path: only page.changed takes a path; leave it out.")
		} else if _, err := path.Match(e.Path, "x"); err != nil {
			p = append(p, fmt.Sprintf("when.event.path: %q is not a glob of pages, such as reports/*.md.", e.Path))
		}
	}
	return p
}

// event is one event of the fleet: its kind, its id, when it happened, the
// agent and the page path that the filters of a duty read, the duty and
// the depth of the chain of the run behind it, and its JSON for {event}.
// Run and Approval name the question of question.waiting.
type event struct {
	Kind     string
	ID       string
	At       time.Time
	Agent    string
	Path     string
	Duty     string
	Chain    int
	Run      string
	Approval string
	Body     map[string]any
}

// start is the entry of one event that starts one duty.
type start struct {
	Duty     string          `json:"duty"`
	Kind     string          `json:"kind"`
	ID       string          `json:"id"`
	Event    json.RawMessage `json:"event"`
	Chain    int             `json:"chain"`
	At       time.Time       `json:"at"`
	Due      time.Time       `json:"due"`
	Run      string          `json:"run,omitempty"`
	Approval string          `json:"approval,omitempty"`
	State    string          `json:"state"`
	Claim    string          `json:"claim,omitempty"`
	ClaimAt  time.Time       `json:"claimAt,omitzero"`
	Outcome  string          `json:"outcome,omitempty"`
}

// startKey is the key of the start of a duty by an event.
func startKey(duty, kind, id string) string {
	sum := sha256.Sum256([]byte(kind + "/" + id))
	return "start." + duty + "." + hex.EncodeToString(sum[:16])
}

// services holds the services of the process, for the hooks that name no
// agent.
var services sync.Map // *fw.Agent to *Service

// Of returns the events of an agent, or nil.
func Of(a *fw.Agent) *Service {
	if a == nil {
		return nil
	}
	if v, ok := services.Load(a); ok {
		return v.(*Service)
	}
	return nil
}

// Service serves the events of the fleet for the duties of one agent.
type Service struct {
	a       *fw.Agent
	b       bucket.Bucket
	log     *slog.Logger
	now     func() time.Time
	replica string
	every   time.Duration

	// The seams of the fleet, which a test replaces.
	runs    func(ctx context.Context) ([]*fw.Run, error)
	run     func(ctx context.Context, id string) (*fw.Run, error)
	routine func(ctx context.Context, agent, name string) (routines.Record, error)

	mu     sync.Mutex
	noted  map[string]time.Time // the keys that this replica created or found, by the time it met them
	closed bool

	poke      chan struct{}
	work      sync.WaitGroup
	cancel    context.CancelFunc
	done      chan struct{}
	closers   []func()
	closeOnce sync.Once
}

// Start is the service of the events (fw.Service). On a bus that keeps no
// store it logs once that the starts on events are off.
func Start(ctx context.Context, a *fw.Agent) (io.Closer, error) {
	log := a.Logger().With("component", "duties-events")
	b, closeFn, err := bucket.Open(ctx, a.Config(), eventsBucket, log)
	if errors.Is(err, bucket.ErrNoStore) {
		where, _ := bucket.Where(a.Config(), eventsBucket)
		log.Warn("events: the starts of the duties on events are off; they live "+where, "bus", a.Config().Bus.Driver)
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("events: %w", err)
	}
	s := newService(a, b, log)
	if closeFn != nil {
		s.closers = append(s.closers, closeFn)
	}
	services.Store(a, s)
	s.start()
	return s, nil
}

// newService builds the events of an agent on a bucket, with the seams of
// the fleet.
func newService(a *fw.Agent, b bucket.Bucket, log *slog.Logger) *Service {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	s := &Service{a: a, b: b, log: log, now: time.Now, replica: replicaOf(a), every: defaultEvery, noted: map[string]time.Time{}, poke: make(chan struct{}, 1)}
	s.runs, s.run = s.runsOfFleet, s.runOfFleet
	s.routine = func(ctx context.Context, agent, name string) (routines.Record, error) {
		st := routines.Of(a)
		if st == nil {
			return routines.Record{}, errors.New("events: the routines are off")
		}
		return st.Get(ctx, agent, name)
	}
	return s
}

// replicaOf names this process in a claim.
func replicaOf(a *fw.Agent) string {
	if v := fleet.Of(a); v != nil {
		return v.Name()
	}
	host, _ := os.Hostname()
	return host
}

func (s *Service) start() {
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel, s.done = cancel, make(chan struct{})
	go func() {
		defer close(s.done)
		for {
			next := s.pass(ctx)
			wait := s.every
			if !next.IsZero() {
				wait = min(max(next.Sub(s.now()), 50*time.Millisecond), s.every)
			}
			t := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				t.Stop()
				return
			case <-s.poke:
				t.Stop()
			case <-t.C:
			}
		}
	}()
}

// Close stops the service. The starts that wait stay for the other
// replicas.
func (s *Service) Close() error {
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.closed = true
		s.mu.Unlock()
		if s.cancel != nil {
			s.cancel()
			<-s.done
		}
		s.work.Wait()
		if v, ok := services.Load(s.a); ok && v == s {
			services.Delete(s.a)
		}
		for _, f := range s.closers {
			f()
		}
	})
	return nil
}

func (s *Service) wake() {
	select {
	case s.poke <- struct{}{}:
	default:
	}
}

// async runs f on a goroutine of its own, for a hook that must not block.
func (s *Service) async(f func(ctx context.Context)) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.work.Add(1)
	s.mu.Unlock()
	go func() {
		defer s.work.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		f(ctx)
	}()
}

// each calls f for each service of the process.
func each(f func(s *Service)) {
	services.Range(func(_, v any) bool {
		f(v.(*Service))
		return true
	})
}

// pass observes the runs of the fleet and fires the starts that are due.
// It returns when the next start that waits is due, or the zero time.
func (s *Service) pass(ctx context.Context) time.Time {
	s.observe(ctx)
	return s.fireDue(ctx)
}

// waiters returns the duties that wait for events of a kind, by kind.
func (s *Service) waiters(ctx context.Context) (map[string][]duties.Record, error) {
	svc := duties.Of(s.a)
	if svc == nil {
		return nil, nil
	}
	recs, err := svc.Records(ctx)
	if err != nil {
		return nil, err
	}
	out := map[string][]duties.Record{}
	for _, r := range recs {
		if e := r.Duty.When.Event; e != nil && !r.Duty.Paused {
			out[e.Kind] = append(out[e.Kind], r)
		}
	}
	return out, nil
}

// note creates the start of each duty that waits for the event. Of two
// replicas that note one event, one creates each start.
func (s *Service) note(ctx context.Context, ev event, waiting []duties.Record) {
	body := map[string]any{"kind": ev.Kind}
	for k, v := range ev.Body {
		body[k] = v
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return
	}
	if len(raw) > maxEventBytes {
		raw, _ = json.Marshal(map[string]any{"kind": ev.Kind, "id": ev.ID, "cut": true})
	}
	created := false
	for _, r := range waiting {
		e := r.Duty.When.Event
		switch {
		case e.Agent != "" && e.Agent != ev.Agent:
			continue
		case e.Path != "" && !matchPath(e.Path, ev.Path):
			continue
		case ev.Duty != "" && ev.Duty == r.Duty.Name:
			// A run of a duty never starts that same duty again.
			continue
		case ev.At.Before(r.Since):
			continue
		}
		after, _ := time.ParseDuration(strings.TrimSpace(e.After))
		key := startKey(r.Duty.Name, ev.Kind, ev.ID)
		if s.seen(key) {
			continue
		}
		st := start{Duty: r.Duty.Name, Kind: ev.Kind, ID: ev.ID, Event: raw, Chain: ev.Chain + 1, At: ev.At.UTC(), Due: ev.At.Add(max(after, 0)).UTC(),
			Run: ev.Run, Approval: ev.Approval, State: stateDue}
		value, err := json.Marshal(st)
		if err != nil {
			continue
		}
		_, err = s.b.Create(ctx, key, value)
		switch {
		case err == nil:
			created = true
			s.remember(key)
			s.log.Info("events: an event starts a duty", "duty", r.Duty.Name, "kind", ev.Kind, "event", ev.ID, "due", st.Due)
		case errors.Is(err, bucket.ErrConflict):
			s.remember(key)
		default:
			s.log.Warn("events: the start of a duty on an event did not land", "duty", r.Duty.Name, "kind", ev.Kind, "error", err)
		}
	}
	if created {
		s.wake()
	}
}

// matchPath reports whether a page path matches a glob of when.event.path,
// or starts with it as a folder, such as reports/ for reports/a.md.
func matchPath(glob, p string) bool {
	if ok, _ := path.Match(glob, p); ok {
		return true
	}
	return strings.HasSuffix(glob, "/") && strings.HasPrefix(p, glob)
}

// seen reports whether this replica met the key of a start already.
func (s *Service) seen(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.noted[key]
	return ok
}

// remember keeps the key of a start for a day, so this replica does not
// ask the bucket again.
func (s *Service) remember(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	s.noted[key] = now
	if len(s.noted) > 10000 {
		for k, at := range s.noted {
			if now.Sub(at) > 24*time.Hour {
				delete(s.noted, k)
			}
		}
	}
}

// fireDue fires each start whose time came and that no other replica
// holds, and returns when the next start that waits is due.
func (s *Service) fireDue(ctx context.Context) time.Time {
	keys, err := bucket.KeysUnder(ctx, s.b, "start")
	if err != nil {
		s.log.Warn("events: the starts did not read", "error", err)
		return time.Time{}
	}
	var next time.Time
	for _, k := range keys {
		if ctx.Err() != nil {
			return next
		}
		if at := s.fireOne(ctx, k); !at.IsZero() && (next.IsZero() || at.Before(next)) {
			next = at
		}
	}
	return next
}

// fireOne fires one start when its time came. A start of question.waiting
// fires only while the question still waits.
func (s *Service) fireOne(ctx context.Context, key string) time.Time {
	e, err := s.b.Get(ctx, key)
	if err != nil {
		return time.Time{}
	}
	var st start
	if json.Unmarshal(e.Value, &st) != nil || st.State != stateDue {
		return time.Time{}
	}
	now := s.now()
	switch {
	case now.Before(st.Due):
		return st.Due
	case st.Claim != "" && now.Sub(st.ClaimAt) < claimFor:
		return st.ClaimAt.Add(claimFor)
	}
	svc := duties.Of(s.a)
	if svc == nil {
		return time.Time{}
	}
	st.Claim, st.ClaimAt = s.replica, now.UTC()
	value, err := json.Marshal(st)
	if err != nil {
		return time.Time{}
	}
	rev, err := s.b.Update(ctx, key, value, e.Revision)
	if err != nil {
		// Another replica claimed it first.
		return time.Time{}
	}
	st.Claim, st.ClaimAt = "", time.Time{}
	if st.Kind == duties.EventQuestionWaiting && !s.stillWaits(ctx, st.Run, st.Approval) {
		st.State, st.Outcome = stateFired, "The question ended before its time."
	} else {
		out, err := svc.FireChain(ctx, st.Duty, st.Event, st.Chain)
		switch {
		case errors.Is(err, duties.ErrNotFound):
			st.State, st.Outcome = stateFired, "The duty is gone."
		case err != nil:
			// The store did not answer: the start goes again at the next
			// pass.
			s.log.Warn("events: a start on an event did not happen; it goes again", "duty", st.Duty, "kind", st.Kind, "error", err)
		default:
			st.State, st.Outcome = stateFired, outcomeWords(out)
			s.log.Info("events: an event started a duty", "duty", st.Duty, "kind", st.Kind, "event", st.ID, "chain", st.Chain, "run", out.Run, "skipped", out.Skipped, "failed", out.Failed)
		}
	}
	if value, err = json.Marshal(st); err == nil {
		_, _ = s.b.Update(ctx, key, value, rev)
	}
	return time.Time{}
}

// outcomeWords says what a start did.
func outcomeWords(out duties.Outcome) string {
	switch {
	case out.Run != "":
		return "run " + out.Run
	case out.Waiting != "":
		return out.Waiting
	case out.Skipped != "":
		return out.Skipped
	case out.Failed != "":
		return out.Failed
	case out.Duplicate:
		return "Another replica started it."
	}
	return "done"
}

// stillWaits reports whether a run still waits on the approval.
func (s *Service) stillWaits(ctx context.Context, run, approval string) bool {
	r, err := s.run(ctx, run)
	return err == nil && r.State == fw.StateInputRequired && r.Pending != nil && r.Pending.ID == approval
}

// chainOf returns the depth of the chain of a run, 0 for a run that no
// event started.
func chainOf(r *fw.Run) int {
	if r == nil {
		return 0
	}
	n, _ := strconv.Atoi(r.Metadata[duties.MetaChain])
	return max(n, 0)
}
