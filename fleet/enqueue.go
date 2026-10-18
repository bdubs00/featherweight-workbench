package fleet

// This file puts the run requests of a job on the intake queues of the
// workers (docs/ARCHITECTURE.md section 4.9). A worker that
// runs the profile <agent> consumes bus.intake "<intake>.<agent>", and the
// front publishes one request per piece of work there. The kernel of the
// worker starts each request as the person that it names and gives the run
// the batch of the job in its metadata, so every list and every metric
// knows the job.

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// EvRunRequest is the type of a bus message that carries a run request for
// the intake of a worker. The data is a fw.RunRequest.
const EvRunRequest = "fw.run.request"

// MaxEnqueue is the most requests that one call puts on a queue.
const MaxEnqueue = 500

// ErrNoIntake means that the configuration names no intake prefix, so no
// queue takes a job.
var ErrNoIntake = errors.New("fleet: workbench.fleet.intake is empty, so no queue takes a job")

// Queue returns the intake queue of a profile: "<intake>.<agent>".
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

// Enqueue puts the requests on the intake queue of the agent and returns
// the queue. Each request keeps its fields; the caller sets the person, the
// batch, and the idempotency key. A publish that fails stops the call and
// returns how many requests went out before it. After the publish, the
// fleet wakes the suspended Tasks that serve the agent (suspend.go).
func Enqueue(ctx context.Context, a *fw.Agent, agent string, reqs []fw.RunRequest) (queue string, sent int, err error) {
	if a == nil || a.Bus() == nil {
		return "", 0, errors.New("fleet: the agent has no bus")
	}
	if len(reqs) > MaxEnqueue {
		return "", 0, fmt.Errorf("%w: %d requests in one call; the limit is %d", fw.ErrInvalidRequest, len(reqs), MaxEnqueue)
	}
	queue, err = Queue(a.Config(), agent)
	if err != nil {
		return "", 0, err
	}
	for i, req := range reqs {
		req.Agent = strings.TrimSpace(agent)
		if strings.TrimSpace(req.Input) == "" && len(req.Files) == 0 {
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
		f.WakeFor(agent)
	}
	return queue, sent, nil
}
