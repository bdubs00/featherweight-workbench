// Package away applies whileAway of the duties: who decides an approval
// or a question of a run of a duty while its owner is away, after how
// long, and what happens then (docs/ARCHITECTURE.md section 23.4). The
// kernel needs no change: the front decides with the route that a person
// uses.
//
// Each replica of the front watches the runs that carry the metadata
// workbench.duty and wait for a person. For each approval that waits, one
// replica creates its entry in the key-value bucket <stream>_DUTY_AWAY
// (memory on the bus mem), and every later step of that approval is a
// compare-and-swap on the entry, so exactly one replica does each step.
// The steps are these:
//
//   - A question, a connect, a tool that the policy marks destructive, a
//     duty whose decider is nobody, and a front without a manager go to
//     the person: the front sends a needs-you notice through the duties.
//   - Any other approval goes to the manager in one message that names the
//     duty, the tool, the input, and the reason, and asks for a verdict in
//     a fixed JSON shape. The front reads the verdict strictly and applies
//     it with its own authority. The manager gets no right to approve. A
//     verdict that does not read sends the approval to the person.
//   - After whileAway.timeout without a decision, the front applies then:
//     it denies the call, or it cancels the run, and tells the people of
//     notify.needsYou.
//   - A person can always decide first. Then the front does nothing more,
//     and the history names the person.
//
// Each decision lands in the history of the duty (duties.Decided).
package away

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/duties"
	"github.com/bdubs00/featherweight-workbench/fleet"
	"github.com/bdubs00/featherweight-workbench/internal/bucket"
	"github.com/bdubs00/featherweight-workbench/manager"
)

func init() {
	fw.RegisterService(Start)
}

// awayBucket holds one entry per approval of a run of a duty that waited
// for a person. An entry lives a day longer than the longest timeout.
var awayBucket = bucket.Config{
	Name:        "DUTY_AWAY",
	Description: "The approvals of the runs of the duties of a featherweight fleet that waited for a person, under wait.<run>.<approval>: who decides each one while its owner is away, and what the server did.",
	History:     1,
	MaxValue:    64 << 10,
	TTL:         duties.MaxAwayFor + 24*time.Hour,
}

// Times of the watch.
const (
	// defaultEvery is the pause between two passes.
	defaultEvery = 10 * time.Second
	// claimFor is how long a claim of a step holds before another replica
	// takes it over: its replica stopped in the middle.
	claimFor = 2 * time.Minute
	// managerLost is how long the front waits for a sign of the run of the
	// manager before the person decides.
	managerLost = 15 * time.Minute
)

// Routes of an approval.
const (
	RouteManager = "manager" // the manager gives a verdict
	RoutePerson  = "person"  // a person decides, or the timeout applies
)

// States of an entry.
const (
	stateOpen = "open" // the approval waits
	stateDone = "done" // somebody decided, the timeout applied, or the approval ended
)

// wait is the entry of one approval of a run of a duty. Asked is when the
// run began to wait, and Deadline when then applies. Route says who
// decides, and Why why a person decides. Told is true once the person
// heard of the approval. Manager is the run of the manager that gives the
// verdict. Claim and ClaimAt hold the replica that does a step. Outcome
// says what the front did.
type wait struct {
	Duty     string    `json:"duty"`
	Run      string    `json:"run"`
	Member   string    `json:"member,omitempty"`
	Approval string    `json:"approval"`
	Agent    string    `json:"agent"`
	Tool     string    `json:"tool,omitempty"`
	Question bool      `json:"question,omitempty"`
	Ask      string    `json:"ask,omitempty"` // the words of a question, for its notice
	Asked    time.Time `json:"asked"`
	Deadline time.Time `json:"deadline"`
	Timeout  string    `json:"timeout"`
	Then     string    `json:"then"`
	Route    string    `json:"route"`
	Why      string    `json:"why,omitempty"`
	Told     bool      `json:"told,omitempty"`
	Manager  string    `json:"manager,omitempty"`
	State    string    `json:"state"`
	Claim    string    `json:"claim,omitempty"`
	ClaimAt  time.Time `json:"claimAt,omitzero"`
	Outcome  string    `json:"outcome,omitempty"`
	Updated  time.Time `json:"updated"`
}

func waitKey(run, approval string) string { return "wait." + run + "." + approval }

// services ties the watch to its agent.
var services sync.Map // *fw.Agent to *Service

// Of returns the watch of an agent, or nil.
func Of(a *fw.Agent) *Service {
	if a == nil {
		return nil
	}
	if v, ok := services.Load(a); ok {
		return v.(*Service)
	}
	return nil
}

// Service is the watch of the runs of the duties of one agent that wait
// for a person.
type Service struct {
	a       *fw.Agent
	b       bucket.Bucket
	log     *slog.Logger
	now     func() time.Time
	replica string
	every   time.Duration

	// The seams of the fleet, which a test replaces.
	waiting     func(ctx context.Context) ([]*fw.Run, error)
	run         func(ctx context.Context, id string) (*fw.Run, error)
	decide      func(ctx context.Context, r *fw.Run, approval, decision, note string) error
	cancelRun   func(ctx context.Context, r *fw.Run) error
	enqueue     func(ctx context.Context, agent string, req fw.RunRequest) error
	decider     func(ctx context.Context, r *fw.Run, approval string) (by, verdict string, ok bool)
	destructive func(ctx context.Context, r *fw.Run) bool
	manager     func() string

	cancel    context.CancelFunc
	done      chan struct{}
	closers   []func()
	closeOnce sync.Once
}

// Start is the service of the watch (fw.Service). It opens the bucket on
// the bus of the configuration and starts the watch. On a bus that keeps
// no store it logs once that the decisions while away are off, and the
// engine of the duties tells of each question itself.
func Start(ctx context.Context, a *fw.Agent) (io.Closer, error) {
	log := a.Logger().With("component", "duties-away")
	if !fleet.CanEnqueue(a.Config()) {
		log.Info("away: this replica decides nothing while away, because workbench.fleet.intake is empty. A replica with an intake decides.")
		return nil, nil
	}
	b, closeFn, err := bucket.Open(ctx, a.Config(), awayBucket, log)
	if errors.Is(err, bucket.ErrNoStore) {
		where, _ := bucket.Where(a.Config(), awayBucket)
		log.Warn("away: the decisions while away are off; they live "+where, "bus", a.Config().Bus.Driver)
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("away: %w", err)
	}
	s := newService(a, b, log)
	if closeFn != nil {
		s.closers = append(s.closers, closeFn)
	}
	services.Store(a, s)
	duties.SetAwayHandler(true)
	s.start()
	return s, nil
}

// newService builds the watch of an agent on a bucket, with the seams of
// the fleet.
func newService(a *fw.Agent, b bucket.Bucket, log *slog.Logger) *Service {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	s := &Service{a: a, b: b, log: log, now: time.Now, replica: replicaOf(a), every: defaultEvery}
	s.waiting, s.run, s.decide, s.cancelRun = s.waitingOfFleet, s.runOfFleet, s.decideOnFleet, s.cancelOnFleet
	s.enqueue, s.decider, s.destructive = s.enqueueOnFleet, s.deciderOnFleet, s.destructiveByPolicy
	s.manager = func() string { return manager.Profile(a.Config()) }
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
			s.pass(ctx)
			t := time.NewTimer(s.every)
			select {
			case <-ctx.Done():
				t.Stop()
				return
			case <-t.C:
			}
		}
	}()
}

// Close stops the watch. The entries stay for the other replicas.
func (s *Service) Close() error {
	s.closeOnce.Do(func() {
		if s.cancel != nil {
			s.cancel()
			<-s.done
		}
		if v, ok := services.Load(s.a); ok && v == s {
			services.Delete(s.a)
			duties.SetAwayHandler(false)
		}
		for _, f := range s.closers {
			f()
		}
	})
	return nil
}

// pass opens an entry for each approval of a run of a duty that began to
// wait, and does the next step of each entry that is open.
func (s *Service) pass(ctx context.Context) {
	svc := duties.Of(s.a)
	if svc == nil {
		return
	}
	runs, err := s.waiting(ctx)
	if err != nil {
		s.log.Warn("away: the runs that wait did not read", "error", err)
	}
	now := map[string]*fw.Run{}
	for _, r := range runs {
		if duties.NameOf(r) == "" || r.State != fw.StateInputRequired || r.Pending == nil {
			continue
		}
		key := waitKey(r.ID, r.Pending.ID)
		now[key] = r
		if _, err := s.b.Get(ctx, key); errors.Is(err, bucket.ErrNotFound) {
			s.open(ctx, svc, r)
		}
	}
	keys, err := bucket.KeysUnder(ctx, s.b, "wait")
	if err != nil {
		s.log.Warn("away: the entries did not read", "error", err)
		return
	}
	for _, k := range keys {
		if ctx.Err() != nil {
			return
		}
		s.step(ctx, svc, k, now[k])
	}
}

// open creates the entry of an approval that began to wait, with the
// route of whileAway of its duty. Of two replicas, one creates it.
func (s *Service) open(ctx context.Context, svc *duties.Service, r *fw.Run) {
	p := r.Pending
	now := s.now().UTC()
	asked := p.RequestedAt
	if asked.IsZero() || asked.After(now) {
		asked = now
	}
	w := wait{
		Duty: duties.NameOf(r), Run: r.ID, Member: r.Member, Approval: p.ID, Agent: r.Agent, Tool: p.Tool,
		Question: len(p.Schema) > 0, Asked: asked.UTC(), State: stateOpen, Updated: now,
	}
	if w.Question {
		w.Ask = trimmed(p.Reason, 300)
	}
	d, err := svc.Lookup(ctx, w.Duty)
	switch {
	case errors.Is(err, duties.ErrNotFound):
		w.State, w.Outcome, w.Route = stateDone, "The duty is gone, so a person decides as for any run.", RoutePerson
	case err != nil:
		s.log.Warn("away: the duty of a run that waits did not read", "duty", w.Duty, "run", r.ID, "error", err)
		return
	default:
		wa := d.WhileAway
		if wa == nil {
			wa = &duties.WhileAway{Decider: duties.DefaultDecider, Timeout: duties.AwayFor(), Then: duties.DefaultThen}
		}
		timeout, err := time.ParseDuration(wa.Timeout)
		if err != nil || timeout <= 0 {
			timeout = duties.DefaultAwayFor
		}
		w.Timeout, w.Then, w.Deadline = wa.Timeout, wa.Then, asked.Add(timeout).UTC()
		w.Route, w.Why = s.route(ctx, r, wa)
	}
	value, err := json.Marshal(w)
	if err != nil {
		return
	}
	if _, err := s.b.Create(ctx, waitKey(r.ID, p.ID), value); err != nil {
		// Another replica opened it first, or the store did not answer.
		return
	}
	s.log.Info("away: a run of a duty waits for a decision", "duty", w.Duty, "run", w.Run, "approval", w.Approval, "tool", w.Tool, "route", w.Route)
}

// route decides who decides an approval: the manager, or the person, with
// the reason in words.
func (s *Service) route(ctx context.Context, r *fw.Run, wa *duties.WhileAway) (string, string) {
	p := r.Pending
	switch {
	case p.Connect != nil:
		return RoutePerson, "The run waits for a person to connect a service."
	case len(p.Schema) > 0:
		return RoutePerson, "A question goes to a person, never to the manager."
	case wa.Decider != duties.DeciderManager:
		return RoutePerson, "The duty says that nobody decides while you are away."
	case s.destructive(ctx, r):
		return RoutePerson, fmt.Sprintf("The policy marks the tool %s destructive, so the manager does not decide it.", p.Tool)
	case s.manager() == "":
		return RoutePerson, "The server has no manager: workbench.bossmode.manager names none."
	}
	return RouteManager, ""
}

// step does the next step of an entry: r is the run when the view shows it
// waiting on the approval of the entry, else nil. A step that changes the
// world first claims the entry with a compare-and-swap.
func (s *Service) step(ctx context.Context, svc *duties.Service, key string, r *fw.Run) {
	e, err := s.b.Get(ctx, key)
	if err != nil {
		return
	}
	var w wait
	if err := json.Unmarshal(e.Value, &w); err != nil || w.State == stateDone {
		return
	}
	now := s.now()
	if w.Claim != "" && now.Sub(w.ClaimAt) < claimFor {
		return
	}
	if r == nil {
		// The view does not show the run waiting on the approval. The run
		// itself says whether it still waits. A run that nobody knows past
		// the deadline is gone.
		cur, err := s.run(ctx, w.Run)
		switch {
		case errors.Is(err, fw.ErrRunNotFound) && !now.Before(w.Deadline):
		case err != nil:
			return
		case cur.State == fw.StateInputRequired && cur.Pending != nil && cur.Pending.ID == w.Approval:
			r = cur
		}
	}
	var act func(context.Context, *wait)
	switch {
	case r == nil:
		act = func(ctx context.Context, w *wait) { s.ended(ctx, svc, w) }
	case !now.Before(w.Deadline):
		act = func(ctx context.Context, w *wait) { s.timeout(ctx, svc, w, r) }
	case w.Route == RoutePerson && !w.Told:
		act = func(ctx context.Context, w *wait) { s.tell(ctx, svc, w) }
	case w.Route == RouteManager && w.Manager == "":
		w.Manager = fw.NewID()
		act = func(ctx context.Context, w *wait) { s.askManager(ctx, svc, w, r) }
	case w.Route == RouteManager:
		m, err := s.run(ctx, w.Manager)
		switch {
		case errors.Is(err, fw.ErrRunNotFound) && now.Sub(w.Updated) > managerLost:
			act = func(ctx context.Context, w *wait) {
				s.toPerson(ctx, svc, w, fmt.Sprintf("No worker of the manager took the question within %s.", managerLost))
			}
		case err != nil || !finished(m):
			return
		default:
			act = func(ctx context.Context, w *wait) { s.verdict(ctx, svc, w, r, m) }
		}
	default:
		return
	}
	w.Claim, w.ClaimAt = s.replica, now.UTC()
	value, err := json.Marshal(w)
	if err != nil {
		return
	}
	rev, err := s.b.Update(ctx, key, value, e.Revision)
	if err != nil {
		// Another replica claimed the step first.
		return
	}
	act(ctx, &w)
	w.Claim, w.ClaimAt, w.Updated = "", time.Time{}, s.now().UTC()
	if value, err = json.Marshal(w); err == nil {
		if _, err := s.b.Update(ctx, key, value, rev); err != nil {
			s.log.Warn("away: an entry did not change after its step", "duty", w.Duty, "run", w.Run, "error", err)
		}
	}
}

// finished reports whether a run ended.
func finished(r *fw.Run) bool {
	switch r.State {
	case fw.StateCompleted, fw.StateFailed, fw.StateCanceled:
		return true
	}
	return false
}

// tell sends the needs-you notice of an approval that a person decides.
func (s *Service) tell(ctx context.Context, svc *duties.Service, w *wait) {
	if err := svc.Tell(ctx, duties.NoticeNeedsYou, w.Duty, w.Run, s.askWords(w)); err != nil {
		s.log.Warn("away: the notice of a question did not go", "duty", w.Duty, "run", w.Run, "error", err)
		return
	}
	w.Told = true
}

// askWords says what waits for the person, and what happens without an
// answer.
func (s *Service) askWords(w *wait) string {
	what := fmt.Sprintf("The agent %s asks to run %s.", w.Agent, w.Tool)
	switch {
	case w.Question && w.Ask != "":
		ask := w.Ask
		if !strings.ContainsAny(ask[len(ask)-1:], ".?!") {
			ask += "."
		}
		what = fmt.Sprintf("The agent %s asks you: %s", w.Agent, ask)
	case w.Question:
		what = fmt.Sprintf("The agent %s asks you a question.", w.Agent)
	}
	then := "the server denies it"
	if w.Then == "cancel" {
		then = "the server cancels the run"
	}
	out := what
	if w.Why != "" && !w.Question {
		out += " " + w.Why
	}
	return fmt.Sprintf("%s Decide in Activity, or with approve. Without an answer within %s, %s.", out, w.Timeout, then)
}

// ended closes the entry of an approval that no longer waits. When the
// front did not decide it, the history names the person who did, if the
// ledger of the run says so. A run of the manager that still works stops.
func (s *Service) ended(ctx context.Context, svc *duties.Service, w *wait) {
	w.State = stateDone
	if w.Outcome == "" {
		w.Outcome = "Somebody decided before the server, or the run ended."
		if cur, err := s.run(ctx, w.Run); err == nil {
			if by, verdict, ok := s.decider(ctx, cur, w.Approval); ok {
				w.Outcome = "decided by " + by
				if err := svc.RecordDecision(ctx, w.Duty, w.Run, duties.Decided{By: by, Verdict: verdict, Tool: w.Tool, Question: w.Question}); err != nil && !errors.Is(err, duties.ErrNotFound) {
					s.log.Warn("away: a decision of a person did not reach the history", "duty", w.Duty, "run", w.Run, "error", err)
				}
			}
		}
	}
	s.stopManager(ctx, w)
}

// timeout applies then of whileAway: it denies the call, or cancels the
// run, records the decision, and tells the people of notify.needsYou.
func (s *Service) timeout(ctx context.Context, svc *duties.Service, w *wait, r *fw.Run) {
	verdict, did := "deny", "denied the call"
	if w.Question {
		did = "declined it"
	}
	reason := fmt.Sprintf("Nobody decided within %s, so the server denied it, as whileAway of the duty %s says.", w.Timeout, w.Duty)
	var err error
	if w.Then == "cancel" {
		verdict, did = "cancel", "canceled the run"
		reason = fmt.Sprintf("Nobody decided within %s, so the server canceled the run, as whileAway of the duty %s says.", w.Timeout, w.Duty)
		err = s.cancelRun(ctx, r)
	} else {
		err = s.decide(ctx, r, w.Approval, "deny", reason)
	}
	if errors.Is(err, errGone) {
		s.ended(ctx, svc, w)
		return
	}
	if err != nil {
		s.log.Warn("away: the timeout of a question did not apply; it goes again", "duty", w.Duty, "run", w.Run, "error", err)
		return
	}
	w.State, w.Outcome = stateDone, "timeout "+verdict
	s.record(ctx, svc, w, duties.Decided{By: duties.DecidedByTimeout, Verdict: verdict, Tool: w.Tool, Reason: reason, Question: w.Question})
	what := fmt.Sprintf("the call of %s by the agent %s", w.Tool, w.Agent)
	if w.Question {
		what = "the question of the agent " + w.Agent
	}
	text := fmt.Sprintf("Nobody answered %s within %s, so the server %s.", what, w.Timeout, did)
	if err := svc.Tell(ctx, duties.NoticeDecided, w.Duty, w.Run, text); err != nil {
		s.log.Warn("away: the notice of a timeout did not go", "duty", w.Duty, "run", w.Run, "error", err)
	}
	s.log.Info("away: the timeout of a question applied", "duty", w.Duty, "run", w.Run, "approval", w.Approval, "verdict", verdict)
	s.stopManager(ctx, w)
}

// record adds a decision to the history of the duty.
func (s *Service) record(ctx context.Context, svc *duties.Service, w *wait, d duties.Decided) {
	if err := svc.RecordDecision(ctx, w.Duty, w.Run, d); err != nil {
		s.log.Warn("away: a decision did not reach the history", "duty", w.Duty, "run", w.Run, "error", err)
	}
}

// stopManager cancels a run of the manager that still works, because the
// approval no longer needs its verdict.
func (s *Service) stopManager(ctx context.Context, w *wait) {
	if w.Manager == "" {
		return
	}
	m, err := s.run(ctx, w.Manager)
	if err != nil || finished(m) {
		return
	}
	if err := s.cancelRun(ctx, m); err != nil && !errors.Is(err, errGone) {
		s.log.Warn("away: a run of the manager that no longer decides did not stop", "run", w.Manager, "error", err)
	}
}

// toPerson sends an approval to the person, with the reason.
func (s *Service) toPerson(ctx context.Context, svc *duties.Service, w *wait, why string) {
	w.Route, w.Why = RoutePerson, why
	s.tell(ctx, svc, w)
}

// trimmed cuts a text to one line of at most n bytes.
func trimmed(text string, n int) string {
	text = strings.Join(strings.Fields(text), " ")
	if len(text) <= n {
		return text
	}
	for n > 0 && (text[n]&0xC0) == 0x80 {
		n--
	}
	return text[:n] + "..."
}
