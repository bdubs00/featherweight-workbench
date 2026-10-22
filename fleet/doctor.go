package fleet

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/internal/bucket"
	"github.com/bdubs00/featherweight-workbench/pools"
)

// DoctorCheck is the line of fw doctor about the fleet (cli.DoctorCheck):
// off, or on with its sources, the queues, the wake on queue depth, the
// place of the index of the runs, the state of the suspend controller, the
// pools, the templates, and the count of the runtime pools. A pool or a
// template that does not fit, one that copies a variable that this process
// lacks, a runtime pool that names a template that the configuration does
// not hold, and a store of the runtime pools that does not answer within
// timeout fail the check. cmd/fw-workbench registers it.
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
		parts = append(parts, "sends jobs to "+c.Intake+".<agent>")
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
	for _, p := range list {
		if p.Template != "" {
			parts = append(parts, fmt.Sprintf("keeps the pool %s of %d Tasks of %s on %s from the template %s", p.Name, p.Wanted(), p.Agent, p.Intake, p.Template))
			continue
		}
		parts = append(parts, fmt.Sprintf("keeps the pool %s of %d Tasks of %s on %s", p.Name, p.Wanted(), p.Agent, p.Intake))
	}
	if len(templates) > 0 {
		parts = append(parts, "offers the worker templates "+pools.TemplateNames(templates))
		line, ok := doctorRuntime(ctx, cfg, templates, timeout)
		parts = append(parts, line)
		if !ok {
			return false, "fleet: " + strings.Join(parts, "; ")
		}
	}
	return true, "fleet: " + strings.Join(parts, "; ")
}

// doctorRuntime counts the runtime pools of the store of cfg for the
// doctor and reports false when the store does not answer, or when a
// runtime pool names a template that templates does not hold.
func doctorRuntime(ctx context.Context, cfg *fw.Config, templates []*pools.Template, timeout time.Duration) (string, bool) {
	where, on := bucket.Where(cfg, pools.StoreBucket)
	if !on {
		return "keeps no runtime pool, because they live " + where, true
	}
	if strings.TrimSpace(cfg.Bus.Driver) == "mem" || strings.TrimSpace(cfg.Bus.Driver) == "" {
		return "keeps its runtime pools in the memory of the front process, which this check does not reach", true
	}
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	store, closeStore, err := OpenRuntime(ctx, cfg, slog.New(slog.DiscardHandler), true)
	if err != nil || store == nil {
		return fmt.Sprintf("could not read the runtime pools in %s: %v", where, err), false
	}
	if closeStore != nil {
		defer closeStore()
	}
	entries, err := store.List(ctx)
	if err != nil {
		return fmt.Sprintf("could not read the runtime pools in %s: %v", where, err), false
	}
	deleting := 0
	var missing []string
	for _, e := range entries {
		if e.Deleting {
			deleting++
		}
		if pools.TemplateNamed(templates, e.Template) == nil {
			missing = append(missing, fmt.Sprintf("the runtime pool %s names the template %s, which workbench.templates does not hold, so the front changes nothing in it", e.Name, e.Template))
		}
	}
	line := fmt.Sprintf("keeps %d runtime %s in %s", len(entries), plural(len(entries), "pool", "pools"), where)
	if deleting > 0 {
		line += fmt.Sprintf(", %d of them being deleted", deleting)
	}
	if len(missing) > 0 {
		return line + "; " + strings.Join(missing, "; "), false
	}
	return line, true
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
