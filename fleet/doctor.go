package fleet

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/internal/bucket"
	"github.com/bdubs00/featherweight-workbench/pools"
	v1alpha1 "github.com/google/ax/pkg/apis/v1alpha1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"gopkg.in/yaml.v3"
)

// DoctorCheck is the line of fw doctor about the fleet (cli.DoctorCheck):
// off, or on with its sources, the queues, the wake on queue depth, the
// place of the index of the runs, the state of the suspend controller, the
// pools, the templates, the count of the runtime pools, and each Task
// outside any pool that serves an agent of a pool or reads the intake of a
// pool (doctorOutside). A pool or a template that does not fit, one that
// copies a variable that this process lacks, a runtime pool that names a
// template that the configuration does not hold or that takes the intake
// of another pool, and a store of the runtime pools that does not answer
// within timeout fail the check. A Task outside any pool is a risk and
// fails nothing. cmd/fw-workbench registers it.
func DoctorCheck(ctx context.Context, cfg *fw.Config, _ fw.Secrets, timeout time.Duration) (bool, string) {
	c, err := Load(cfg)
	if err != nil {
		return false, "fleet: " + err.Error()
	}
	list, templates, err := LoadAll(cfg, c)
	if err != nil {
		return false, "fleet: " + err.Error()
	}
	if _, err := pools.Resolve(append(slices.Clone(list), pools.Probes(templates)...), nil); err != nil {
		return false, "fleet: " + err.Error()
	}
	if !c.On() {
		return true, "fleet: off; set workbench.fleet.atespaces (" + EnvAtespaces + ") or workbench.fleet.egress (" + EnvEgress + ") to read one"
	}
	if c.Egress != "" && (cfg.Bus.Driver == "" || cfg.Bus.Driver == "mem") {
		return false, "fleet: workbench.fleet.egress is " + c.Egress + ", but the bus is mem, so no worker reaches this process; set bus.driver"
	}
	var parts []string
	if len(c.Atespaces) > 0 {
		parts = append(parts, "reads the Tasks of "+strings.Join(c.Atespaces, ", ")+" from "+firstNonEmpty(cfg.AX.Server, "ax.server"))
	}
	if c.Egress != "" {
		parts = append(parts, "reads the events of the workers on "+c.Egress+" over the "+cfg.Bus.Driver+" bus")
	}
	if c.Intake != "" {
		parts = append(parts, "sends a job to the intake of the pool that runs its agent, else to "+c.Intake+".<agent>")
		if cfg.Bus.Driver == "nats" {
			parts = append(parts, "reads the depth and the dead letters of each queue from the nats server")
		} else {
			parts = append(parts, "reads no queue depth, because the bus is "+firstNonEmpty(cfg.Bus.Driver, "mem")+" and not nats")
		}
		if c.wakeOnQueue() {
			parts = append(parts, "wakes the sleeping Tasks for the jobs that wait in a queue")
		} else if len(c.Atespaces) > 0 {
			parts = append(parts, "wakes the Tasks after its own enqueue only (workbench.fleet.wakeOnQueue is off)")
		}
	}
	if where, on := bucket.Where(cfg, IndexBucket); on {
		parts = append(parts, "keeps the index of the runs in "+where)
	} else {
		parts = append(parts, "keeps no index of the runs, so after a restart it knows no run of a sleeping Task; the index lives "+where)
	}
	if d := c.suspendAfter(); d > 0 {
		parts = append(parts, "suspends a Task that idles for "+d.String())
	} else {
		parts = append(parts, "suspends no Task (set workbench.fleet.suspendAfter)")
	}
	if d := c.orphansAfter(); d > 0 {
		parts = append(parts, "removes an orphan Task that sleeps for "+d.String())
	}
	for _, p := range list {
		part := fmt.Sprintf("keeps the pool %s of %d %s of %s on %s", p.Name, p.Wanted(), plural(p.Wanted(), "Task", "Tasks"), p.Agent, p.Intake)
		if p.Template != "" {
			part += " from the template " + p.Template
		}
		parts = append(parts, part)
	}
	all := slices.Clone(list)
	if len(templates) > 0 {
		parts = append(parts, "offers the worker templates "+pools.TemplateNames(templates))
		line, ok, built := doctorRuntime(ctx, cfg, list, templates, timeout)
		parts = append(parts, line)
		if !ok {
			return false, "fleet: " + strings.Join(parts, "; ")
		}
		// The pools of the file share no intake by now (LoadAll), so a
		// shared intake involves a runtime pool.
		all = append(all, built...)
		if shared := pools.SharedIntakes(all); len(shared) > 0 {
			return false, "fleet: " + strings.Join(append(parts, shared...), "; ")
		}
	}
	parts = append(parts, doctorOutside(ctx, cfg, c, all, timeout)...)
	return true, "fleet: " + strings.Join(parts, "; ")
}

// doctorRuntime counts the runtime pools of the store of cfg for the
// doctor and reports false when the store does not answer, or when a
// runtime pool names a template that templates does not hold. It also
// returns the runtime pools that build, without a pool that a person
// deleted and without a pool whose name a pool of the file, of files,
// holds.
func doctorRuntime(ctx context.Context, cfg *fw.Config, files []*pools.Pool, templates []*pools.Template, timeout time.Duration) (string, bool, []*pools.Pool) {
	where, on := bucket.Where(cfg, pools.StoreBucket)
	if !on {
		return "keeps no runtime pool, because they live " + where, true, nil
	}
	if strings.TrimSpace(cfg.Bus.Driver) == "mem" || strings.TrimSpace(cfg.Bus.Driver) == "" {
		return "keeps its runtime pools in the memory of the server process, which this check does not reach", true, nil
	}
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	store, closeStore, err := OpenRuntime(ctx, cfg, slog.New(slog.DiscardHandler), true)
	if err != nil || store == nil {
		return fmt.Sprintf("could not read the runtime pools in %s: %v", where, err), false, nil
	}
	if closeStore != nil {
		defer closeStore()
	}
	entries, err := store.List(ctx)
	if err != nil {
		return fmt.Sprintf("could not read the runtime pools in %s: %v", where, err), false, nil
	}
	shadowed := map[string]bool{}
	for _, p := range files {
		shadowed[p.Name] = true
	}
	deleting := 0
	var missing []string
	var built []*pools.Pool
	for _, e := range entries {
		if e.Deleting {
			deleting++
		}
		if pools.TemplateNamed(templates, e.Template) == nil {
			missing = append(missing, fmt.Sprintf("the runtime pool %s names the template %s, which workbench.templates does not hold, so the server changes nothing in it", e.Name, e.Template))
			continue
		}
		if p, err := pools.FromRuntime(e.Runtime, templates); err == nil && !e.Deleting && !shadowed[e.Name] {
			built = append(built, p)
		}
	}
	line := fmt.Sprintf("keeps %d runtime %s in %s", len(entries), plural(len(entries), "pool", "pools"), where)
	if deleting > 0 {
		line += fmt.Sprintf(", %d of them being deleted", deleting)
	}
	if len(missing) > 0 {
		return line + "; " + strings.Join(missing, "; "), false, nil
	}
	return line, true, built
}

// doctorOutside words, as a risk, each Task of the atespaces outside any
// pool, a Task without FW_POOL, that serves a profile that a pool of all
// runs (outsideRisk), or that reads the intake of a pool. A job for the
// profile goes to the intake of the pool (QueueFor), so such a Task gets
// only the jobs that others publish on the queue that it reads, and a Task
// that reads the intake of a pool takes jobs of that pool. The doctor reads
// the Task list and the configuration file in the Workspace of each such
// Task, as the front learns what a sleeping Task serves (learn), so it
// wakes no Task. all holds the pools of the file and the runtime pools. It
// returns no part without pools, atespaces, or ax.server, and a part that
// says what it cannot tell when the ax API does not answer.
func doctorOutside(ctx context.Context, cfg *fw.Config, c Config, all []*pools.Pool, timeout time.Duration) []string {
	server := strings.TrimSpace(cfg.AX.Server)
	if len(all) == 0 || len(c.Atespaces) == 0 || server == "" {
		return nil
	}
	cannot := "cannot tell whether a Task outside any pool serves an agent of a pool"
	conn, err := grpc.NewClient(server, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return []string{fmt.Sprintf("%s, because it reaches no ax API at %s: %v", cannot, server, err)}
	}
	defer conn.Close()
	client := v1alpha1.NewAXClient(conn)
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var parts, unread []string
	for _, atespace := range c.Atespaces {
		tasks, err := listTasks(ctx, client, atespace)
		if err != nil {
			parts = append(parts, fmt.Sprintf("%s in %s, because the ax API did not list its Tasks: %v", cannot, atespace, err))
			continue
		}
		sort.Slice(tasks, func(i, j int) bool { return tasks[i].GetMetadata().GetName() < tasks[j].GetMetadata().GetName() })
		for _, t := range tasks {
			// A Task of a pool, and a child Task of one, carries FW_POOL
			// and takes the intake of its pool, and a stopped Task takes
			// no job.
			if taskVar(t, pools.EnvPool) != "" || stateOfPhase(t.GetStatus().GetPhase()) == MemberStopped {
				continue
			}
			name := t.GetMetadata().GetName()
			if len(c.Atespaces) > 1 {
				name = atespace + "/" + name
			}
			who := "the Task " + name + " outside any pool"
			var wf workerFile
			path, content, err := configFile(ctx, client, t)
			if err != nil || path == "" || yaml.Unmarshal([]byte(content), &wf) != nil {
				unread = append(unread, name)
				continue
			}
			queue, known := wf.queue(t)
			said := false
			for _, agent := range slices.Sorted(maps.Keys(wf.Agents)) {
				if by := runners(all, agent, (*pools.Pool).Profiles); len(by) > 0 {
					parts = append(parts, outsideRisk(who, agent, by, all, queue, known))
					said = true
				}
			}
			// A Task that serves no agent of a pool still takes the jobs of
			// a pool whose intake it reads.
			if takes := covered(all, queue); !said && known && len(takes) > 0 {
				parts = append(parts, who+" reads "+takesText(queue, takes))
			}
		}
	}
	if len(unread) > 0 {
		parts = append(parts, fmt.Sprintf("cannot tell what the %s %s outside any pool %s, because the server reads no configuration file in %s",
			plural(len(unread), "Task", "Tasks"), strings.Join(unread, ", "), plural(len(unread), "serves", "serve"), plural(len(unread), "its Workspace", "their Workspaces")))
	}
	return parts
}

// workerFile is the part of the configuration file of a worker that the
// doctor reads: the profiles and the intake.
type workerFile struct {
	Agents map[string]any `yaml:"agents"`
	Bus    struct {
		Intake string `yaml:"intake"`
	} `yaml:"bus"`
}

// queue returns the queue that the worker of the Task t reads:
// FW_BUS_INTAKE of the Task, which the kernel takes over the file, else
// bus.intake of the file, and "" when it reads none. It reports false for
// a value that the worker expands itself, which the doctor cannot tell.
func (w workerFile) queue(t *v1alpha1.Task) (string, bool) {
	q := strings.TrimSpace(taskVar(t, "FW_BUS_INTAKE"))
	if q == "" {
		q = strings.TrimSpace(w.Bus.Intake)
	}
	return q, !strings.Contains(q, "${")
}

// outsideRisk words the risk of a Task outside any pool, who, that serves
// the profile agent, which the pools by run: where a job for the agent
// goes, and what the Task gets of such jobs, as far as the doctor can tell
// the queue that the Task reads. A queue that is, or covers, the intake of
// a pool of all gives the Task jobs of that pool.
func outsideRisk(who, agent string, by, all []*pools.Pool, queue string, known bool) string {
	runs, goes := "the pool "+by[0].Name+" runs", by[0].Name
	if len(by) > 1 {
		runs, goes = "the pools "+namesOf(by)+" run", "the pool that the job names"
	}
	head := fmt.Sprintf("%s serves the agent %s, which %s; jobs for %s go to %s", who, agent, runs, agent, goes)
	switch {
	case !known:
		return head + ", and the server cannot tell which queue this Task reads"
	case queue == "":
		return head + ", and this Task reads no queue"
	}
	takes := covered(all, queue)
	switch {
	case len(takes) == 0:
		return head + ", and this Task gets only jobs that others publish on " + queue
	case len(by) == 1 && len(takes) == 1 && takes[0] == by[0] && queue == by[0].Intake:
		return fmt.Sprintf("%s on %s, and this Task reads that queue too, so it takes some of them", head, queue)
	}
	return head + ", and this Task reads " + takesText(queue, takes)
}

// covered returns the pools of all whose intake queue is or covers.
func covered(all []*pools.Pool, queue string) []*pools.Pool {
	var out []*pools.Pool
	for _, p := range all {
		if queue != "" && fw.SubjectMatch(queue, p.Intake) {
			out = append(out, p)
		}
	}
	return out
}

// takesText words a queue that takes the jobs of the pools takes, such as
// "fw.jobs.a, the queue of the pool a, so it takes jobs of that pool".
func takesText(queue string, takes []*pools.Pool) string {
	if len(takes) == 1 && queue == takes[0].Intake {
		return fmt.Sprintf("%s, the queue of the pool %s, so it takes jobs of that pool", queue, takes[0].Name)
	}
	return fmt.Sprintf("%s, which covers the %s %s, so it takes jobs of %s", queue,
		plural(len(takes), "queue of the pool", "queues of the pools"), namesOf(takes), plural(len(takes), "that pool", "those pools"))
}

// namesOf names the pools of list, such as "a, b".
func namesOf(list []*pools.Pool) string {
	names := make([]string, len(list))
	for i, p := range list {
		names[i] = p.Name
	}
	return strings.Join(names, ", ")
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
