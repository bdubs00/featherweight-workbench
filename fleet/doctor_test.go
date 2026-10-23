package fleet

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/pools"
	v1alpha1 "github.com/google/ax/pkg/apis/v1alpha1"
	"gopkg.in/yaml.v3"
)

// The doctor names, as a risk, each Task outside any pool that serves an
// agent that a pool runs, or that reads the intake of a pool: where the
// jobs of the agent go, and what the Task gets of them, as far as its
// configuration file and its variables tell the queue that it reads. A
// Task of a pool, a stopped Task, and a Task that serves no agent of a
// pool on a queue of its own are no risk, and the doctor says which Tasks
// it cannot tell. The risks fail nothing.
func TestDoctorNamesATaskOutsideThePools(t *testing.T) {
	for _, e := range []string{EnvAtespaces, EnvEgress, EnvName, EnvSuspendAfter, EnvIntake, EnvWakeOnQueue} {
		t.Setenv(e, "")
	}
	t.Setenv("FW_ATESPACE", "")
	t.Setenv(EnvAtespaces, "team")
	t.Setenv(EnvEgress, "fw.events")
	t.Setenv(EnvIntake, "fw.jobs")
	t.Setenv("FW_API_TOKEN", "fleet-secret")
	ax := newPoolAX(t)
	cfg := fw.DefaultConfig()
	cfg.Bus.Driver, cfg.Bus.URL = "nats", "nats://nats.agents.svc.cluster.local:4222"
	cfg.AX.Server, cfg.AX.Router = ax.addr, "atenet-router"
	var n yaml.Node
	if err := yaml.Unmarshal([]byte(intakeBlock), &n); err != nil {
		t.Fatal(err)
	}
	cfg.Extensions = map[string]yaml.Node{Block: *n.Content[0]}
	// task puts a Task with the configuration file file in its Workspace,
	// or with no Workspace for an empty file.
	task := func(name, phase, file string, env ...*v1alpha1.EnvVar) {
		spec := &v1alpha1.TaskSpec{Image: "x", Env: env}
		if file != "" {
			spec.Workspaces = []*v1alpha1.WorkspaceRef{{Name: name + "-ws", Path: "/workspace"}}
			ax.mu.Lock()
			ax.workspaces["team/"+name+"-ws"] = &v1alpha1.Workspace{Metadata: &v1alpha1.ObjectMeta{Name: name + "-ws", Atespace: "team"},
				Spec: &v1alpha1.WorkspaceSpec{Files: []*v1alpha1.File{{Path: ".fw/fw.yaml", Content: file}}}}
			ax.mu.Unlock()
		}
		ax.put(&v1alpha1.Task{Metadata: &v1alpha1.ObjectMeta{Name: name, Atespace: "team"}, Spec: spec}, phase)
	}
	reviewer := "agents:\n  reviewer: {}\nbus:\n  intake: fw.jobs.reviewer\n"
	task("crew-reviewer-0", "Running", "agents:\n  reviewer: {}\nbus:\n  intake: fw.jobs.crew-reviewer\n", &v1alpha1.EnvVar{Name: pools.EnvPool, Value: "crew-reviewer"})
	task("fw-fleet-reviewer", "Suspended", reviewer)
	task("gone", "Failed", reviewer)
	task("old-manager", "Running", "agents:\n  manager: {}\n  notes: {}\nbus:\n  intake: fw.jobs.notes\n", &v1alpha1.EnvVar{Name: "FW_BUS_INTAKE", Value: "fw.jobs.manager"})
	task("quiet", "Running", "agents:\n  reviewer: {}\n")
	task("templated", "Running", "agents:\n  reviewer: {}\nbus:\n  intake: fw.jobs.${AGENT}\n")
	task("wild", "Running", "agents:\n  writer: {}\nbus:\n  intake: fw.jobs.>\n")
	task("writer", "Running", "agents:\n  writer: {}\nbus:\n  intake: fw.jobs.writer\n")
	task("bare", "Running", "")

	ok, line := DoctorCheck(context.Background(), cfg, fw.EnvSecrets{}, 5*time.Second)
	if !ok {
		t.Fatalf("the doctor fails: %q", line)
	}
	for _, want := range []string{
		"sends a job to the intake of the pool that runs its agent, else to fw.jobs.<agent>",
		"keeps the pool crew-reviewer of 1 Task of reviewer on fw.jobs.crew-reviewer",
		"the Task fw-fleet-reviewer outside any pool serves the agent reviewer, which the pool crew-reviewer runs; jobs for reviewer go to crew-reviewer, and this Task gets only jobs that others publish on fw.jobs.reviewer",
		"the Task old-manager outside any pool serves the agent manager, which the pool manager runs; jobs for manager go to manager on fw.jobs.manager, and this Task reads that queue too, so it takes some of them",
		"the Task quiet outside any pool serves the agent reviewer, which the pool crew-reviewer runs; jobs for reviewer go to crew-reviewer, and this Task reads no queue",
		"the Task templated outside any pool serves the agent reviewer, which the pool crew-reviewer runs; jobs for reviewer go to crew-reviewer, and the front cannot tell which queue this Task reads",
		"the Task wild outside any pool reads fw.jobs.>, which covers the queues of the pools crew-reviewer, manager, so it takes jobs of those pools",
		"cannot tell what the Task bare outside any pool serves, because the front reads no configuration file in its Workspace",
	} {
		if !strings.Contains(line, want) {
			t.Errorf("the doctor line lacks %q:\n%s", want, line)
		}
	}
	for _, no := range []string{"crew-reviewer-0", "gone", "the Task writer", "agent notes"} {
		if strings.Contains(line, no) {
			t.Errorf("the doctor line names %q:\n%s", no, line)
		}
	}

	// An ax API that does not answer leaves the risks unknown, and the
	// line says so.
	cfg.AX.Server = "127.0.0.1:1"
	if ok, line := DoctorCheck(context.Background(), cfg, fw.EnvSecrets{}, time.Second); !ok || !strings.Contains(line, "cannot tell whether a Task outside any pool serves an agent of a pool in team, because the ax API did not list its Tasks") {
		t.Fatalf("the doctor without the ax API = %v %q", ok, line)
	}
}

// A runtime pool whose default intake is the intake that a pool of the
// file sets takes the jobs of that pool, so the doctor fails and says
// which pool of the file to give another intake.
func TestDoctorRefusesARuntimePoolOnTheIntakeOfAPool(t *testing.T) {
	for _, e := range []string{EnvAtespaces, EnvEgress, EnvName, EnvSuspendAfter, EnvIntake, EnvWakeOnQueue} {
		t.Setenv(e, "")
	}
	t.Setenv("FW_ATESPACE", "")
	t.Setenv(EnvAtespaces, "team")
	t.Setenv(EnvEgress, "fw.events")
	t.Setenv(EnvIntake, "fw.jobs")
	t.Setenv("FW_API_TOKEN", "fleet-secret")
	store := useMemRuntime(t)
	ctx := context.Background()
	cfg := fw.DefaultConfig()
	cfg.Bus.Driver, cfg.Bus.URL = "nats", "nats://nats.agents.svc.cluster.local:4222"
	block := templateBlock + `
pools:
  - name: tickets
    template: base
    replicas: 0
    agent: triage
    intake: fw.jobs.helper
    fw:
      agents:
        triage: {description: Sorts new tickets.}
`
	var n yaml.Node
	if err := yaml.Unmarshal([]byte(block), &n); err != nil {
		t.Fatal(err)
	}
	cfg.Extensions = map[string]yaml.Node{Block: *n.Content[0]}
	cfg.AX.Server, cfg.AX.Router = "127.0.0.1:1", "atenet-router"
	if ok, line := DoctorCheck(ctx, cfg, fw.EnvSecrets{}, time.Second); !ok || !strings.Contains(line, "keeps the pool tickets of 0 Tasks of triage on fw.jobs.helper from the template base") {
		t.Fatalf("the doctor without runtime pools = %v %q", ok, line)
	}
	if _, err := store.Create(ctx, pools.Runtime{Name: "helper", Template: "base", Agent: "helper", Replicas: 1}); err != nil {
		t.Fatal(err)
	}
	ok, line := DoctorCheck(ctx, cfg, fw.EnvSecrets{}, time.Second)
	if ok || !strings.Contains(line, "the runtime pool helper: the intake fw.jobs.helper is the intake of workbench.pools[0] (tickets) too, so the workers of both pools would take the jobs of each other; set intake to a queue of its own on workbench.pools[0] (tickets)") {
		t.Fatalf("the doctor with a shared intake = %v %q", ok, line)
	}
}
