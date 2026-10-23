package fleet

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/internal/bucket"
)

// placeBlock is three pools without Tasks: crew-reviewer runs the agent
// reviewer on the queue of the pool, manager runs the agent manager and
// holds reviewer and researcher for its child Tasks, and tickets runs the
// agent triage on an intake of its own.
const placeBlock = `
pools:
  - name: crew-reviewer
    replicas: 0
    image: ghcr.io/example/fw@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
    agent: reviewer
    egress:
      hosts:
        - http://nats.agents.svc.cluster.local:8080
    fw:
      model: openai/some-model
      bus: {url: "ws://nats.agents.svc.cluster.local:8080"}
      agents:
        reviewer: {description: Reviews a change.}
  - name: manager
    replicas: 0
    image: ghcr.io/example/fw@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
    agent: manager
    egress:
      hosts:
        - http://nats.agents.svc.cluster.local:8080
    fw:
      model: openai/some-model
      bus: {url: "ws://nats.agents.svc.cluster.local:8080"}
      agents:
        manager: {description: Runs the bridges.}
        reviewer: {description: Reviews a change.}
        researcher: {description: Reads the sources.}
  - name: tickets
    replicas: 0
    image: ghcr.io/example/fw@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
    agent: triage
    intake: fw.jobs.triage
    egress:
      hosts:
        - http://nats.agents.svc.cluster.local:8080
    fw:
      model: openai/some-model
      bus: {url: "ws://nats.agents.svc.cluster.local:8080"}
      agents:
        triage: {description: Sorts new tickets.}
`

// placeFront is a front that keeps the pools of block, with the intake
// prefix fw.jobs, a server of the bus that holds no job, and the index of
// the runs in memory.
func placeFront(t *testing.T, block string) *member {
	t.Helper()
	t.Setenv("FW_ATESPACE", "team")
	t.Setenv("FW_API_TOKEN", "fleet-secret")
	// Queue reads the intake prefix from the configuration of the agent.
	t.Setenv(EnvIntake, "fw.jobs")
	withBusServer(t, &fakeQueues{}, nil)
	useIndex(t, bucket.NewMem(IndexBucket))
	ax := newPoolAX(t)
	tasks := newFakeTasks(t)
	pooled := poolFront(t, ax, strings.TrimPrefix(tasks.srv.URL, "http://"), block)
	return newMember(t, fw.NewMemBus(), "front", func(cfg *fw.Config, fc *Config) {
		fc.Intake = "fw.jobs"
		pooled(cfg, fc)
	})
}

// published collects the run requests that reach the queues, by queue.
type published struct {
	mu   sync.Mutex
	reqs map[string][]fw.RunRequest
}

// listen subscribes to the queues on the bus of a.
func listen(t *testing.T, a *fw.Agent, queues ...string) *published {
	t.Helper()
	p := &published{reqs: map[string][]fw.RunRequest{}}
	for _, queue := range queues {
		stop, err := a.Bus().Subscribe(context.Background(), queue, func(e fw.Event) error {
			var req fw.RunRequest
			if e.Type == EvRunRequest && e.Decode(&req) == nil {
				p.mu.Lock()
				p.reqs[queue] = append(p.reqs[queue], req)
				p.mu.Unlock()
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(stop)
	}
	return p
}

func (p *published) on(queue string) []fw.RunRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]fw.RunRequest(nil), p.reqs[queue]...)
}

// A job goes to the intake of the pool that runs its agent: the pool that
// the caller names, else the pool whose agent the profile is, else the one
// pool that holds the profile. Only a profile that no pool runs takes the
// queue <intake>.<agent>. A pool that does not run the profile, and a pool
// that does not exist, are errors.
func TestQueueForPlacesAJobOnThePoolOfItsAgent(t *testing.T) {
	front := placeFront(t, placeBlock)
	ctx := context.Background()
	for _, c := range []struct{ agent, pool, want string }{
		{"reviewer", "", "fw.jobs.crew-reviewer"},
		{"reviewer", "crew-reviewer", "fw.jobs.crew-reviewer"},
		{"reviewer", "manager", "fw.jobs.manager"},
		{"researcher", "", "fw.jobs.manager"},
		{"triage", "", "fw.jobs.triage"},
		{"nobody", "", "fw.jobs.nobody"},
	} {
		if got, err := QueueFor(ctx, front.a, c.agent, c.pool); err != nil || got != c.want {
			t.Errorf("QueueFor(%s, %q) = %q, %v; want %s", c.agent, c.pool, got, err, c.want)
		}
	}
	for _, c := range []struct{ agent, pool, want string }{
		{"reviewer", "tickets", "the pool tickets does not run the agent reviewer; its workers take the jobs of triage"},
		{"reviewer", "ghost", "no pool is named ghost"},
		{"no.dots", "", "not a name for a queue"},
	} {
		if _, err := QueueFor(ctx, front.a, c.agent, c.pool); !errors.Is(err, fw.ErrInvalidRequest) || !strings.Contains(err.Error(), c.want) {
			t.Errorf("QueueFor(%s, %q) = %v; want %q", c.agent, c.pool, err, c.want)
		}
	}

	// Enqueue places a job by its agent, and EnqueueFor by its pool, and
	// the job record names the queue of each.
	got := listen(t, front.a, "fw.jobs.crew-reviewer", "fw.jobs.manager", "fw.jobs.reviewer", "fw.jobs.nobody")
	job := func(input string) []fw.RunRequest {
		return []fw.RunRequest{{Input: input, Principal: "ada", Metadata: map[string]string{fw.MetaBatch: "job-1"}, IdempotencyKey: "job-1/" + input}}
	}
	for _, c := range []struct{ agent, pool, input, want string }{
		{"reviewer", "", "review", "fw.jobs.crew-reviewer"},
		{"reviewer", "manager", "seat", "fw.jobs.manager"},
		{"nobody", "", "orphan", "fw.jobs.nobody"},
	} {
		queue, sent, err := EnqueueFor(ctx, front.a, c.agent, c.pool, job(c.input))
		if err != nil || sent != 1 || queue != c.want {
			t.Fatalf("EnqueueFor(%s, %q) = %s, %d, %v; want %s", c.agent, c.pool, queue, sent, err, c.want)
		}
	}
	if queue, _, err := Enqueue(ctx, front.a, "reviewer", job("again")); err != nil || queue != "fw.jobs.crew-reviewer" {
		t.Fatalf("Enqueue = %s, %v", queue, err)
	}
	eventually(t, "the requests reach the queues of their pools", func() bool {
		return len(got.on("fw.jobs.crew-reviewer")) == 2 && len(got.on("fw.jobs.manager")) == 1 && len(got.on("fw.jobs.nobody")) == 1
	})
	if reqs := got.on("fw.jobs.reviewer"); len(reqs) != 0 {
		t.Fatalf("the queue of the profile reviewer got %+v", reqs)
	}
	if r := got.on("fw.jobs.manager")[0]; r.Agent != "reviewer" || r.Input != "seat" {
		t.Fatalf("the request of the seat = %+v", r)
	}
	rec, ok, _ := front.f.jobs.get(ctx, jobKey{batch: "job-1", subject: "ada"})
	if !ok || len(rec.Queues["fw.jobs.crew-reviewer"]) != 2 || len(rec.Queues["fw.jobs.manager"]) != 1 || len(rec.Queues["fw.jobs.nobody"]) != 1 || len(rec.Queues) != 3 {
		t.Fatalf("the job record = %+v, %v", rec, ok)
	}
}

// Two pools that both run the agent reviewer take two queues, so a job for
// reviewer names its pool: the pool named after the agent takes
// fw.jobs.reviewer, and the other pool a queue named after it.
func TestQueueForNeedsThePoolOfAnAgentOfTwoPools(t *testing.T) {
	second := strings.Replace(strings.Split(placeBlock, "  - name: manager")[0], "- name: crew-reviewer", "- name: reviewer", 1)
	front := placeFront(t, placeBlock+strings.TrimPrefix(second, "\npools:\n"))
	ctx := context.Background()
	if _, err := QueueFor(ctx, front.a, "reviewer", ""); !errors.Is(err, fw.ErrInvalidRequest) || !strings.Contains(err.Error(), "run the agent reviewer; pass pool to name one") {
		t.Fatalf("QueueFor without a pool = %v", err)
	}
	for pool, want := range map[string]string{"reviewer": "fw.jobs.reviewer", "crew-reviewer": "fw.jobs.crew-reviewer"} {
		if got, err := QueueFor(ctx, front.a, "reviewer", pool); err != nil || got != want {
			t.Errorf("QueueFor(reviewer, %s) = %q, %v; want %s", pool, got, err, want)
		}
	}
}

// Without pools, a job takes the queue of its profile, and a pool is an
// error.
func TestQueueForWithoutPools(t *testing.T) {
	t.Setenv("FW_ATESPACE", "")
	t.Setenv(EnvIntake, "fw.jobs")
	front := newMember(t, fw.NewMemBus(), "front", func(_ *fw.Config, fc *Config) { fc.Intake = "fw.jobs" })
	ctx := context.Background()
	if got, err := QueueFor(ctx, front.a, "reviewer", ""); err != nil || got != "fw.jobs.reviewer" {
		t.Fatalf("QueueFor = %q, %v", got, err)
	}
	if _, err := QueueFor(ctx, front.a, "reviewer", "crew-reviewer"); !errors.Is(err, fw.ErrInvalidRequest) || !strings.Contains(err.Error(), "keeps no pool") {
		t.Fatalf("QueueFor with a pool = %v", err)
	}
	t.Setenv(EnvIntake, "")
	if _, err := QueueFor(ctx, front.a, "reviewer", ""); !errors.Is(err, ErrNoIntake) {
		t.Fatalf("QueueFor without an intake prefix = %v", err)
	}
}
