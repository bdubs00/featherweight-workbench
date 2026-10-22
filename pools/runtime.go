package pools

// This file builds a runtime pool (docs/ARCHITECTURE.md section 14.10): a
// pool that a person creates while the front runs, through the routes
// /v1/pools, the MCP tool pool_put, or agent_put with workers. Its entry
// lives in the store of the front (store.go). At each pass the front
// builds the pool from its template and its overrides, merges the overlay
// of the pool (section 14.9), and runs it as a pool of the file: the same
// marks on its Tasks, the same hash, and the same rules of the generation.

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/bdubs00/featherweight-kernel/ax"
	"github.com/bdubs00/featherweight-kernel/memory"
)

// Bounds of a runtime pool. A person sets the replicas and the overrides,
// so the front bounds them, and the deployer sets the rest in the
// template.
const (
	MaxRuntimeReplicas  = 64
	MaxRuntimeEnv       = 32
	MaxRuntimeEnvValue  = 4096
	maxRuntimeRevision  = 256
)

// Runtime is the entry of a runtime pool in the store of the front. Name
// names the pool and its Tasks, as for a pool of the file. Template names
// the entry of workbench.templates that the pool renders from, and Agent
// the profile whose queue its workers take; the profile itself lives in
// the overlay of the pool. Intake is the queue of the workers, empty for
// the default of the front. Revision is free text that replaces every Task
// when it changes. Generation counts the changes of the entry: the store
// sets it to 1 at the create and raises it with every change, and the
// pool adds the generation of its template (Template.apply).
//
// Env holds the variables that the pool sets over its template, each with
// a value: a runtime pool copies no variable of the front. Resources holds
// the values that the pool sets over the resources of its template. By and
// At say who wrote the entry last, and when. Deleting marks a pool that a
// person deleted: it wants no Task, and the front removes the entry once
// its Tasks are gone.
type Runtime struct {
	Name       string            `json:"name"`
	Template   string            `json:"template"`
	Agent      string            `json:"agent"`
	Replicas   int               `json:"replicas"`
	Intake     string            `json:"intake,omitempty"`
	Revision   string            `json:"revision,omitempty"`
	Generation int               `json:"generation"`
	Env        []RuntimeEnv      `json:"env,omitempty"`
	Resources  *ax.ResourcesSpec `json:"resources,omitempty"`
	By         string            `json:"by,omitempty"`
	At         time.Time         `json:"at"`
	Deleting   bool              `json:"deleting,omitempty"`
}

// RuntimeEnv is one variable that a runtime pool sets over its template.
type RuntimeEnv struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// quantity matches a quantity of a resource, such as 500m, 2, or 1Gi.
var quantity = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?(m|k|M|G|T|Ki|Mi|Gi|Ti)?$`)

// FromRuntime builds the runtime pool of the entry e from its template in
// templates, which LoadAll checked, and checks it as Load checks a pool. The
// profile of the agent of the pool comes from the overlay, so the pool
// holds only the place of the profile in fw.agents, and the check of the
// model waits for the overlay (Pool.WithOverlay). The hash of the pool
// without the overlay, which a Task carries in FW_POOL_BASE, therefore
// covers the template and the entry alone.
func FromRuntime(e Runtime, templates []*Template) (*Pool, error) {
	label := "the runtime pool " + e.Name
	var problems []string
	add := func(format string, args ...any) {
		problems = append(problems, label+": "+fmt.Sprintf(format, args...))
	}
	t := TemplateNamed(templates, e.Template)
	if t == nil || t.probe == nil {
		return nil, fmt.Errorf("pools: %s names the template %q, which workbench.templates does not hold; %s", label, e.Template, templateChoice(templates))
	}
	if !memory.ValidAgentName(e.Agent) {
		add("agent %q is not the name of an agent profile; a name holds 1 to 64 letters, digits, '-', '_', or '.'", e.Agent)
	}
	switch {
	case e.Replicas < 0:
		add("replicas %d is below 0; set 0 or more", e.Replicas)
	case e.Replicas > MaxRuntimeReplicas:
		add("replicas %d is above %d, the most Tasks of a runtime pool; ask the deployer for a pool in workbench.pools", e.Replicas, MaxRuntimeReplicas)
	}
	if e.Generation < 0 {
		add("generation %d is below 0", e.Generation)
	}
	if len(e.Revision) > maxRuntimeRevision {
		add("revision holds more than %d bytes", maxRuntimeRevision)
	}
	if len(e.Env) > MaxRuntimeEnv {
		add("env sets %d variables, and a runtime pool sets at most %d", len(e.Env), MaxRuntimeEnv)
	}
	for i, v := range e.Env {
		if len(v.Value) > MaxRuntimeEnvValue {
			add("env[%d] (%s) holds more than %d bytes", i, v.Name, MaxRuntimeEnvValue)
		}
	}
	if r := e.Resources; r != nil {
		for _, q := range []struct{ key, v string }{
			{"requests.cpu", r.Requests.CPU}, {"requests.memory", r.Requests.Memory},
			{"limits.cpu", r.Limits.CPU}, {"limits.memory", r.Limits.Memory},
		} {
			if v := strings.TrimSpace(q.v); v != "" && !quantity.MatchString(v) {
				add("resources.%s %q is not a quantity such as 500m, 2, or 1Gi", q.key, q.v)
			}
		}
	}
	if len(problems) > 0 {
		return nil, fmt.Errorf("pools: %s", strings.Join(problems, "; "))
	}
	replicas := e.Replicas
	p := &Pool{Name: strings.TrimSpace(e.Name), Replicas: &replicas, Agent: e.Agent, Intake: strings.TrimSpace(e.Intake), Revision: e.Revision, Generation: e.Generation}
	for _, v := range e.Env {
		value := v.Value
		p.Env = append(p.Env, EnvVar{Name: v.Name, Value: &value})
	}
	if e.Resources != nil {
		p.Resources = *e.Resources
	}
	t.apply(p)
	p.FW = merge(p.FW, map[string]any{"agents": map[string]any{e.Agent: map[string]any{}}})
	c := e
	c.Env = append([]RuntimeEnv(nil), e.Env...)
	p.label, p.lenient, p.runtime = label, true, &c
	if problems := p.complete(t.probe.front); len(problems) > 0 {
		return nil, fmt.Errorf("pools: %s", strings.Join(problems, "; "))
	}
	return p, nil
}

// RuntimeStatus returns the state of a runtime pool that does not build,
// from its entry alone, with the reason.
func RuntimeStatus(e Runtime, why string) Status {
	at := e.At
	return Status{Name: e.Name, Agent: e.Agent, Intake: e.Intake, Wanted: e.Replicas, Generation: e.Generation, Tasks: []TaskStatus{},
		Gaps: []string{why}, Source: SourceRuntime, Template: e.Template, By: e.By, At: &at, Deleting: e.Deleting}
}
