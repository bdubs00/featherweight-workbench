// Package routines keeps the routines of the agents of a fleet
// (docs/ARCHITECTURE.md section 21). A routine is a vetted sequence of
// tool calls with typed questions and a check afterward, which the kernel
// runs in place of the model once a person signs it off. The package
// keeps each routine with its standing, its history, and its evidence,
// brings the routines that rehearse or act to the runs of their agent,
// and judges when a rehearsing routine is ready for a sign-off. The
// kernel runs what the profile says and records what happened.
//
// The package runs as a service of the kernel (fw.RegisterService). On a
// NATS bus the routines live in the key-value bucket <stream>_ROUTINES,
// so every replica of the front reads one truth. On the bus mem they live
// in the memory of the process. On another bus the package logs once that
// the routines are off and serves nothing. It mounts the routes under
// /v1/routines on the native surface (native.Extend), adds the MCP tools
// routines_list and routine_standing (mcp.RegisterTools), gives the fleet
// the routines of the agents (fleet.UseRoutines), reads the events of the
// routines from the egress stream that the fleet reads (fleet.Tap), and
// reads workbench.routines and FW_WORKBENCH_ROUTINES_*, whose key enabled
// turns all of it off. Status gives a line
// for fw-workbench doctor, and OnChange gives the live stream of bossmode
// each change of a routine or of its evidence.
package routines

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/surface/native"
	"github.com/bdubs00/featherweight-workbench/faces"
	"github.com/bdubs00/featherweight-workbench/fleet"
	"github.com/bdubs00/featherweight-workbench/internal/bucket"
	"github.com/bdubs00/featherweight-workbench/manager"
	"github.com/bdubs00/featherweight-workbench/surface/mcp"
)

func init() {
	fw.RegisterService(Start)
	native.Extend(mount)
	mcp.RegisterTools(tools)
	fleet.Routines = reaching
	fleet.Tap(tapped, fw.EvRunQueued, fw.EvRoutineDecided, fw.EvRoutineApplied, fw.EvRunAnnotated, fw.EvRoutineCompared)
}

// bucketName follows the stream in the name of the bucket.
const bucketName = "ROUTINES"

// bucketConfig is the bucket of the routines.
var bucketConfig = bucket.Config{
	Name:        bucketName,
	Description: "The routines of the agents of a featherweight fleet: one entry per routine under <agent>/<name>, and its evidence under <agent>/<name>/evidence.",
	History:     History,
	MaxValue:    maxValue,
}

// stores ties the routines of an agent to the agent.
var stores sync.Map // *fw.Agent to *Store

// Of returns the routines of an agent, or nil when the process keeps none.
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

// Attach makes s the routines of a. A nil s detaches.
func Attach(a *fw.Agent, s *Store) {
	if s == nil {
		stores.Delete(a)
		return
	}
	stores.Store(a, s)
}

// ErrOff means that workbench.routines.enabled turns the routines off.
var ErrOff = errors.New("routines: workbench.routines.enabled turns the routines off")

// Open opens the routines of the configuration cfg on its bus. It returns
// ErrOff when the configuration turns the routines off, and
// bucket.ErrNoStore for a bus that keeps no store. The function that it
// returns closes the connection; it is nil for the routines in memory.
func Open(ctx context.Context, cfg *fw.Config, log *slog.Logger) (*Store, func(), error) {
	return open(ctx, cfg, log, bucket.Open)
}

type opener func(ctx context.Context, cfg *fw.Config, c bucket.Config, log *slog.Logger) (bucket.Bucket, func(), error)

func open(ctx context.Context, cfg *fw.Config, log *slog.Logger, openBucket opener) (*Store, func(), error) {
	c, err := Load(cfg)
	if err != nil {
		return nil, nil, err
	}
	if !c.On() {
		return nil, nil, ErrOff
	}
	b, closeFn, err := openBucket(ctx, cfg, bucketConfig, log)
	if err != nil {
		return nil, nil, fmt.Errorf("routines: %w", err)
	}
	return New(b, c, log), closeFn, nil
}

// Start is the service of the routines (fw.Service). It opens the routines
// on the bus of the configuration, attaches them to the agent, and starts
// the projection of the evidence. When the configuration turns the
// routines off, or on a bus that keeps no store, it logs once that the
// routines are off and returns nil. A configuration that does not fit
// stops the start.
func Start(ctx context.Context, a *fw.Agent) (io.Closer, error) {
	log := a.Logger().With("component", "routines")
	s, closeFn, err := Open(ctx, a.Config(), log)
	if errors.Is(err, ErrOff) {
		log.Info("routines: the routines are off; set workbench.routines.enabled or " + EnvEnabled + " to turn them on")
		return nil, nil
	}
	if errors.Is(err, bucket.ErrNoStore) {
		where, _ := bucket.Where(a.Config(), bucketConfig)
		log.Warn("routines: the routines are off; they live "+where, "bus", a.Config().Bus.Driver)
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	pctx, cancel := context.WithCancel(context.Background())
	s.moved = func(ctx context.Context, rec Record) {
		// A move of Know-how writes the runbook as a move of a person
		// does, so the page always says the standing of the routine.
		full, err := s.Get(ctx, rec.Agent, rec.Name())
		if err == nil {
			_, err = writeRunbook(ctx, a, full)
		}
		if err != nil {
			log.Warn("routines: the runbook of a routine did not follow its move", "agent", rec.Agent, "routine", rec.Name(), "error", err)
		}
	}
	s.proj = newProjector(s, log)
	go s.proj.run(pctx)
	Attach(a, s)
	log.Info("routines: the routines are on", "store", s.Describe(), "readyAfterRead", s.cfg.ReadyAfter.Read, "readyAfterWrite", s.cfg.ReadyAfter.Write, "readyAgreement", s.cfg.ReadyAgreement)
	return service{a: a, close: closeFn, cancel: cancel}, nil
}

// service detaches the routines of an agent, stops the projection, and
// closes the connection.
type service struct {
	a      *fw.Agent
	close  func()
	cancel context.CancelFunc
}

func (s service) Close() error {
	Attach(s.a, nil)
	s.cancel()
	if s.close != nil {
		s.close()
	}
	return nil
}

// Status says where the routines of the configuration cfg live and with
// which thresholds, for a line of fw-workbench doctor. It reports false for
// a configuration that does not fit.
func Status(cfg *fw.Config) (bool, string) {
	c, err := Load(cfg)
	if err != nil {
		return false, err.Error()
	}
	if !c.On() {
		return true, "routines: off; workbench.routines.enabled turns them off"
	}
	where, on := bucket.Where(cfg, bucketConfig)
	if !on {
		return true, "routines: off; the routines live " + where
	}
	return true, fmt.Sprintf("routines: the routines live in %s; a rehearsing routine is ready after %d runs that it would have acted on when it only reads, and %d when it writes, with %g of them agreed", where, c.ReadyAfter.Read, c.ReadyAfter.Write, c.ReadyAgreement)
}

// errNotOpen means that the bus keeps the routines and the service of the
// routines did not open them yet.
var errNotOpen = errors.New("routines: the store of the routines is not open yet")

// reaching reads the routines for the fleet (fleet.Routines). For the
// agent a of the front, it reads the store of the agent. While the bus
// keeps a store and the service has not opened it yet, it answers
// errNotOpen, so a pass of the pools waits instead of rendering without
// the routines and replacing every Task twice. Routines that the
// configuration turns off, or a bus that keeps no store, give none, and
// the profiles keep their own. Without an agent, for the dry run, it reads
// the store of the configuration cfg once.
func reaching(ctx context.Context, a *fw.Agent, cfg *fw.Config) (map[string][]fw.Routine, error) {
	if a != nil {
		if s := Of(a); s != nil {
			return s.Reaching(ctx)
		}
		if c, err := Load(a.Config()); err != nil || !c.On() {
			return nil, err
		}
		if _, on := bucket.Where(a.Config(), bucketConfig); on {
			return nil, errNotOpen
		}
		return nil, nil
	}
	s, closeFn, err := open(ctx, cfg, slog.New(slog.DiscardHandler), bucket.OpenOnce)
	if errors.Is(err, ErrOff) || errors.Is(err, bucket.ErrNoStore) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if closeFn != nil {
		defer closeFn()
	}
	return s.Reaching(ctx)
}

// Check decides whether the caller in ctx can do the action of the kernel
// on the agent, as the faces and agent_put decide: fw.ActionAgentView to
// read its routines, fw.ActionAgentRun to propose one, and
// fw.ActionAgentEdit to change a body or a standing (faces.Check). The
// agent must be a profile of this process or of a pool of the fleet.
func Check(ctx context.Context, a *fw.Agent, agent, action string) error {
	if err := CheckAgent(agent); err != nil {
		return fmt.Errorf("%w: %s", fw.ErrInvalidRequest, reason(err))
	}
	return faces.Check(ctx, a, agent, action)
}

// hooks holds the functions that OnChange registers.
var hooks struct {
	mu     sync.Mutex
	change []func(agent, name string)
}

// OnChange registers a function that gets the agent and the name of a
// routine after each change that the store wrote to its record or to its
// evidence. The package bossmode registers one in init and sends the
// change to the page as the event routine. The function runs on the
// goroutine of the change, so it must not block. Each replica calls it for
// the changes that it writes.
func OnChange(fn func(agent, name string)) {
	hooks.mu.Lock()
	defer hooks.mu.Unlock()
	hooks.change = append(hooks.change, fn)
}

// OnMove gets every move of a routine that the store wrote: the agent, the
// name, the standing before the move ("" for a new routine), the standing
// after it, who moved it (ByKnowhow for a move that Know-how made by
// itself), and the note of the move. A step back names its run in the
// note, as "run <id>". The package knowhow sets it at its start and writes
// the journal from it. Nil calls nothing. The function runs on the
// goroutine of the move, so it must not block. Each replica calls it for
// the moves that it writes.
var OnMove func(ctx context.Context, agent, name, from, to, by, note string)

// AskFirst reports whether the agent holds every change of Know-how for a
// person (the switch askFirst of docs/KNOWHOW.md section 5.3). A ready
// routine that only reads then waits for a sign-off instead of going live
// by itself. The package knowhow sets it at its start. Nil means false.
var AskFirst func(ctx context.Context, agent string) bool

// emitMove calls OnMove.
func emitMove(ctx context.Context, agent, name, from, to, by, note string) {
	if fn := OnMove; fn != nil {
		fn(ctx, agent, name, from, to, by, note)
	}
}

// askFirst calls AskFirst.
func askFirst(ctx context.Context, agent string) bool {
	fn := AskFirst
	return fn != nil && fn(ctx, agent)
}

// emitChange calls the OnChange hooks.
func emitChange(agent, name string) {
	hooks.mu.Lock()
	list := append([]func(string, string){}, hooks.change...)
	hooks.mu.Unlock()
	for _, fn := range list {
		fn(agent, name)
	}
}

// caller names who acts in ctx, after manager.OnBehalf: the subject of the
// principal, else fw-workbench for the process itself. person is true for
// a person only: a user of the issuer or the local user of the process,
// never the static token, a workload, or an anonymous caller. note adds the actor, such as
// "by manager run 01K... for ada", to the note of a run of the fleet.
func caller(ctx context.Context, note string) (by, fullNote string, person bool) {
	by, fullNote = "fw-workbench", note
	if p := fw.PrincipalFrom(ctx); p != nil && p.Subject != "" {
		by, person = p.Subject, p.Kind == fw.KindUser || p.Kind == fw.KindLocal
	}
	if act, ok := manager.ActorFrom(ctx); ok {
		fullNote = act.Note(note)
	}
	return by, fullNote, person
}
