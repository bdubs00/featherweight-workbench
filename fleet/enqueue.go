package fleet

// This file puts the run requests of a job on the intake queues of the
// workers (docs/ARCHITECTURE.md section 4.9). The workers of a pool
// consume the intake of their pool, bus.intake of each of them, and a
// worker of no pool consumes "<intake>.<agent>" for the profile <agent>.
// The front places each job on the queue of the pool that runs its agent
// (QueueFor), so a job reaches the workers that should run it, also when a
// Task of no pool serves a profile of the same name. It publishes one
// request per piece of work there. The kernel of the worker starts each
// request as the person that it names and gives the run the batch of the
// job in its metadata, so every list and every metric knows the job.

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/pools"
)

// EvRunRequest is the type of a bus message that carries a run request for
// the intake of a worker. The data is a fw.RunRequest.
const EvRunRequest = "fw.run.request"

// MaxEnqueue is the most requests that one call puts on a queue.
const MaxEnqueue = 500

// ErrNoIntake means that the configuration names no intake prefix, so no
// queue takes a job.
var ErrNoIntake = errors.New("fleet: workbench.fleet.intake is empty, so no queue takes a job")

// CanEnqueue reports whether this process can put a job on a queue: its
// configuration names workbench.fleet.intake. Several fronts can share one
// bus, and a front without an intake serves its routes but starts no run
// on a schedule or an event, because the job would find no queue.
func CanEnqueue(cfg *fw.Config) bool {
	c, err := Load(cfg)
	return err == nil && c.Intake != ""
}

// Queue returns the queue of a profile that no pool runs: "<intake>.<agent>",
// which a worker of no pool reads. A job goes to the queue of QueueFor,
// which knows the pools.
func Queue(cfg *fw.Config, agent string) (string, error) {
	c, err := Load(cfg)
	if err != nil {
		return "", err
	}
	if c.Intake == "" {
		return "", ErrNoIntake
	}
	agent = strings.TrimSpace(agent)
	if agent == "" || strings.ContainsAny(agent, " \t.*>") {
		return "", fmt.Errorf("%w: the agent %q is not a name for a queue", fw.ErrInvalidRequest, agent)
	}
	return c.Intake + "." + agent, nil
}

// QueueFor returns the queue of a job for the profile agent: the intake of
// the pool whose workers run it, else the queue of the profile, Queue. A
// caller that knows the pool names it in pool, as a seat of a bridge does.
// Without pool, the pool whose agent is the profile wins, as for the writes
// and the reads of a profile (PoolAgentNamed), and the queue of the profile
// takes the job only when no pool runs it. A pool that does not exist or
// does not run the profile, and a profile without pool that several pools
// run as their agent, are errors that wrap fw.ErrInvalidRequest.
func QueueFor(ctx context.Context, a *fw.Agent, agent, pool string) (string, error) {
	agent, pool = strings.TrimSpace(agent), strings.TrimSpace(pool)
	// A pool can run a profile whose name is no token of a subject, so a
	// name that Queue refuses waits for the pools.
	queue, err := Queue(a.Config(), agent)
	if agent == "" || (err != nil && !errors.Is(err, fw.ErrInvalidRequest)) {
		return "", err
	}
	qv, ok := Of(a).(QueueView)
	if !ok {
		if pool != "" {
			return "", noPools()
		}
		return queue, err
	}
	in, found, perr := qv.PoolQueue(ctx, agent, pool)
	switch {
	case perr != nil:
		return "", perr
	case found:
		return in, nil
	case pool != "":
		return "", fmt.Errorf("%w: no pool is named %s", fw.ErrInvalidRequest, pool)
	}
	return queue, err
}

// noPools is the error for a pool that a caller names on a front that
// keeps none.
func noPools() error {
	return fmt.Errorf("%w: this process keeps no pool, so leave out pool", fw.ErrInvalidRequest)
}

// PoolQueue implements QueueView: the intake of the pool of the name pool,
// which must run the profile agent, else of the pool that runs the profile
// as PlaceOf picks it (poolFor). The pools are those of the file and the
// runtime pools of the store, which a pass may not have read yet.
func (f *Fleet) PoolQueue(ctx context.Context, agent, pool string) (string, bool, error) {
	if !f.keepsPools() {
		if pool != "" {
			return "", false, noPools()
		}
		return "", false, nil
	}
	base, err := f.poolFor(f.poolsNow(ctx), agent, pool)
	if err != nil || base == nil {
		return "", false, err
	}
	if pool != "" && base.Agent != agent && !f.holds(ctx, base, agent) {
		return "", false, fmt.Errorf("%w: the pool %s does not run the agent %s; its workers take the jobs of %s", fw.ErrInvalidRequest, base.Name, agent, base.Agent)
	}
	return base.Intake, true, nil
}

// holds reports whether the workers of a pool run the profile name: in the
// merge of the last pass, else in the overlay that the store holds now, for
// a profile that a person added since.
func (f *Fleet) holds(ctx context.Context, base *pools.Pool, name string) bool {
	if _, ok := f.platform.current(base).Profiles()[name]; ok {
		return true
	}
	merged, err := f.mergedPool(ctx, base)
	if err != nil {
		return false
	}
	_, ok := merged.Profiles()[name]
	return ok
}

// Enqueue puts the requests on the queue of the profile agent, QueueFor
// without a pool, and returns the queue. Each request keeps its fields; the
// caller sets the person, the batch, and the idempotency key. A publish
// that fails stops the call and returns how many requests went out before
// it. The fleet records the requests that went out in the job record of
// their batch (jobs.go), and after the publish it wakes the suspended Tasks
// that take the queue (suspend.go).
func Enqueue(ctx context.Context, a *fw.Agent, agent string, reqs []fw.RunRequest) (queue string, sent int, err error) {
	return EnqueueFor(ctx, a, agent, "", reqs)
}

// EnqueueFor is Enqueue with the pool whose workers run the requests, as a
// seat of a bridge names it: the requests go to the intake of that pool
// (QueueFor). An empty pool is Enqueue.
func EnqueueFor(ctx context.Context, a *fw.Agent, agent, pool string, reqs []fw.RunRequest) (queue string, sent int, err error) {
	if a == nil || a.Bus() == nil {
		return "", 0, errors.New("fleet: the agent has no bus")
	}
	if len(reqs) > MaxEnqueue {
		return "", 0, fmt.Errorf("%w: %d requests in one call; the limit is %d", fw.ErrInvalidRequest, len(reqs), MaxEnqueue)
	}
	queue, err = QueueFor(ctx, a, agent, pool)
	if err != nil {
		return "", 0, err
	}
	agent = strings.TrimSpace(agent)
	// The requests that went out are jobs of their batch, also when a
	// later one fails.
	defer func() {
		if f, ok := Of(a).(*Fleet); ok && f != nil && sent > 0 {
			f.recordJobs(ctx, queue, reqs[:sent])
		}
	}()
	// Each request carries the routines that the store holds now, so a
	// Task that runs an older configuration runs them too.
	routines := enqueueRoutines(ctx, a)
	for i, req := range reqs {
		req.Agent = agent
		// Only the store of the routines gives a run its routines, so a
		// caller never sends a routine through a queue.
		dropRoutines(&req)
		inlineRoutines(&req, routines, req.Agent)
		// A check run reads its work from its check, as the kernel takes
		// it, so its input can be empty.
		if strings.TrimSpace(req.Input) == "" && len(req.Files) == 0 && req.Check == nil {
			return queue, sent, fmt.Errorf("%w: the input of item %d is empty", fw.ErrInvalidRequest, i+1)
		}
		ev, err := fw.NewEvent("", EvRunRequest, req)
		if err != nil {
			return queue, sent, err
		}
		if err := a.Bus().Publish(ctx, queue, ev); err != nil {
			return queue, sent, fmt.Errorf("fleet: publish item %d to %s: %w", i+1, queue, err)
		}
		sent++
	}
	if f, ok := Of(a).(*Fleet); ok && f != nil && sent > 0 {
		f.WakeFor(queue, agent)
	}
	return queue, sent, nil
}
