package pools

// This file holds the worker templates (docs/ARCHITECTURE.md section
// 14.10), the block workbench.templates of fw.yaml. A template says what a
// worker is without its agent: the image, the atespace, the variables, the
// egress, the resources, the files, and the base configuration of the
// worker. The deployer writes the templates once. A pool of the file can
// name a template and takes every key of it that the pool does not set. A
// runtime pool, which a person creates while the front runs, renders from
// its template, its overrides, and the overlay of the pool.

import (
	"fmt"
	"slices"
	"strings"

	"github.com/bdubs00/featherweight-kernel/ax"
	"github.com/bdubs00/featherweight-kernel/fw"
	v1alpha1 "github.com/google/ax/pkg/apis/v1alpha1"
)

// templatesKey is the key of the templates in the block workbench.
const templatesKey = "templates"

// Where a pool comes from (Pool.Source).
const (
	SourceFile    = "file"
	SourceRuntime = "runtime"
)

// The pool with which Load checks a template. A template has no agent, so
// the check gives it the profile probeAgent with an access block that
// names an owner, as agent_put gives a new profile its creator. The Tasks
// of the check take the name probeName, so the name of a template sets no
// bound on the names of Tasks.
const (
	probeAgent = "agent"
	probeName  = "template"
	probeOwner = "a-person"
)

// Template is one entry of workbench.templates: the sandbox of a worker
// without the agent. The keys mean what the keys of a pool of the same
// name mean (section 14.1). FW is the base configuration of the workers,
// such as the model, the providers, the bus, the policy, and the tools,
// and it names no agents. Default marks the template that a new agent
// gets when its request names none; at most one template sets it.
// Generation orders the configurations of the template, as the generation
// of a pool does: a pool from the template adds it to its own generation.
type Template struct {
	Name       string           `yaml:"name"`
	Default    bool             `yaml:"default,omitempty"`
	Atespace   string           `yaml:"atespace,omitempty"`
	Image      string           `yaml:"image"`
	Generation int              `yaml:"generation,omitempty"`
	Env        []EnvVar         `yaml:"env,omitempty"`
	Egress     *ax.EgressSpec   `yaml:"egress,omitempty"`
	Resources  ax.ResourcesSpec `yaml:"resources,omitempty"`
	Files      []ax.File        `yaml:"files,omitempty"`
	FW         map[string]any   `yaml:"fw,omitempty"`

	index int   // the position in workbench.templates
	probe *Pool // the template checked as a pool of one worker
}

// TemplateInfo is a template as the front shows it to an operator: no
// image, no variable, and no egress rule.
type TemplateInfo struct {
	Name       string `json:"name"`
	Default    bool   `json:"default"`
	Atespace   string `json:"atespace"`
	Model      string `json:"model,omitempty"`
	Generation int    `json:"generation,omitempty"`
}

// Info returns the template as the front shows it.
func (t *Template) Info() TemplateInfo {
	out := TemplateInfo{Name: t.Name, Default: t.Default, Atespace: t.Atespace, Generation: t.Generation}
	if t.probe != nil {
		out.Atespace = t.probe.Atespace
		if t.probe.worker != nil {
			out.Model = t.probe.worker.Model
		}
	}
	return out
}

// Infos returns the templates as the front shows them, in the order of the
// configuration, and an empty list for none.
func Infos(list []*Template) []TemplateInfo {
	out := make([]TemplateInfo, 0, len(list))
	for _, t := range list {
		out = append(out, t.Info())
	}
	return out
}

// Probes returns the pools with which Load checked the templates. Resolve
// takes them beside the pools, so the front reads the variables that the
// templates copy at its start.
func Probes(list []*Template) []*Pool {
	var out []*Pool
	for _, t := range list {
		if t.probe != nil {
			out = append(out, t.probe)
		}
	}
	return out
}

// TemplateNamed returns the template of that name, or nil.
func TemplateNamed(list []*Template, name string) *Template {
	for _, t := range list {
		if t.Name == name {
			return t
		}
	}
	return nil
}

// DefaultTemplate returns the template with default: true, or nil.
func DefaultTemplate(list []*Template) *Template {
	for _, t := range list {
		if t.Default {
			return t
		}
	}
	return nil
}

// TemplateNames names the templates for a person, such as "python (the
// default), go", or "none".
func TemplateNames(list []*Template) string {
	if len(list) == 0 {
		return "none"
	}
	var out []string
	for _, t := range list {
		if t.Default {
			out = append(out, t.Name+" (the default)")
		} else {
			out = append(out, t.Name)
		}
	}
	return strings.Join(out, ", ")
}

// templateChoice says which templates exist, for an error.
func templateChoice(list []*Template) string {
	if len(list) == 0 {
		return "workbench.templates names no template"
	}
	return "the templates are " + TemplateNames(list)
}

// key names the template in an error.
func (t *Template) key() string {
	if t.Name == "" {
		return fmt.Sprintf("workbench.templates[%d]", t.index)
	}
	return fmt.Sprintf("workbench.templates[%d] (%s)", t.index, t.Name)
}

// decodeTemplates reads workbench.templates strictly: a key that a
// template does not know, such as replicas or agent, is an error that
// names it.
func decodeTemplates(cfg *fw.Config) ([]*Template, error) {
	var out []*Template
	if err := decodeKey(cfg, templatesKey, &out, "workbench.templates is a list of templates, each with name, image, egress, and fw, and no agent"); err != nil {
		return nil, err
	}
	return out, nil
}

// checkTemplates checks the templates: a name of its own for each, at
// most one default, and the checks of a pool on each (check).
func checkTemplates(list []*Template, front Front) []string {
	var problems []string
	names := map[string]int{}
	def := -1
	for i, t := range list {
		if t == nil {
			problems = append(problems, fmt.Sprintf("workbench.templates[%d] is empty", i))
			continue
		}
		t.index = i
		t.Name = strings.TrimSpace(t.Name)
		if j, ok := names[t.Name]; ok && t.Name != "" {
			problems = append(problems, fmt.Sprintf("%s has the name of workbench.templates[%d]; each template needs a name of its own", t.key(), j))
		}
		names[t.Name] = i
		if t.Default {
			if def >= 0 {
				problems = append(problems, fmt.Sprintf("%s sets default, and so does workbench.templates[%d] (%s); set default on one template only", t.key(), def, list[def].Name))
			} else {
				def = i
			}
		}
		problems = append(problems, t.check(front)...)
	}
	return problems
}

// check checks one template as a pool of one worker with a profile that
// has an owner, as far as the checks of a pool apply without an agent. A
// template without a model passes: a pool from it refuses to render until
// the pool, its profile, or the template sets one.
func (t *Template) check(front Front) []string {
	add := func(format string, args ...any) string { return t.key() + ": " + fmt.Sprintf(format, args...) }
	switch {
	case t.Name == "":
		return []string{add("set name, the name of the template, such as python")}
	case v1alpha1.ValidateName(t.Name) != nil:
		return []string{add("name %q is not a lowercase name of letters, digits, and hyphens that starts and ends with a letter or a digit", t.Name)}
	}
	var problems []string
	if t.Generation < 0 {
		problems = append(problems, add("generation %d is below 0; set 0 or more, and raise it with each change of the template", t.Generation))
	}
	if _, set := t.FW["agents"]; set {
		problems = append(problems, add("remove fw.agents: a template says what a worker is, and the pool or the person who creates an agent names the agent"))
	}
	one := 1
	p := &Pool{Name: probeName, Replicas: &one, Agent: probeAgent}
	t.apply(p)
	p.FW = merge(p.FW, map[string]any{"agents": map[string]any{probeAgent: map[string]any{"access": map[string]any{"owners": []any{probeOwner}}}}})
	p.label, p.lenient, p.probe = t.key(), true, true
	problems = append(problems, p.complete(front)...)
	t.probe = p
	return problems
}

// apply gives the pool p every key of the template that p does not set:
// the image, the atespace, and the egress when p sets none. The variables
// and the files of both go together, and a variable or a file of p
// replaces the one of the same name or path. Each of the four resource
// values that p sets replaces the one of the template. fw merges key by
// key, and the keys of p win. The generation of the template adds to the
// generation of p, so a deploy that raises the generation of a template
// orders each pool of it.
func (t *Template) apply(p *Pool) {
	p.Template = t.Name
	if strings.TrimSpace(p.Image) == "" {
		p.Image = t.Image
	}
	if strings.TrimSpace(p.Atespace) == "" {
		p.Atespace = t.Atespace
	}
	if p.Egress == nil {
		p.Egress = t.Egress
	}
	p.Env = mergeEnv(t.Env, p.Env)
	p.Files = mergeFiles(t.Files, p.Files)
	p.Resources = mergeResources(t.Resources, p.Resources)
	p.FW = merge(t.FW, p.FW)
	if p.Generation >= 0 && t.Generation > 0 {
		p.Generation += t.Generation
	}
}

// mergeEnv returns the variables of base with those of over: a variable of
// over replaces the one of base of the same name, in its place, and the
// others follow. The result is a new list.
func mergeEnv(base, over []EnvVar) []EnvVar {
	if len(base) == 0 && len(over) == 0 {
		return nil
	}
	out := slices.Clone(base)
	for _, e := range over {
		i := slices.IndexFunc(out, func(b EnvVar) bool { return strings.TrimSpace(b.Name) == strings.TrimSpace(e.Name) })
		if i >= 0 && strings.TrimSpace(e.Name) != "" {
			out[i] = e
			continue
		}
		out = append(out, e)
	}
	return out
}

// mergeFiles is mergeEnv for the files, by path.
func mergeFiles(base, over []ax.File) []ax.File {
	if len(base) == 0 && len(over) == 0 {
		return nil
	}
	clean := func(path string) string { return strings.TrimPrefix(strings.TrimSpace(path), "./") }
	out := slices.Clone(base)
	for _, f := range over {
		i := slices.IndexFunc(out, func(b ax.File) bool { return clean(b.Path) == clean(f.Path) })
		if i >= 0 && clean(f.Path) != "" {
			out[i] = f
			continue
		}
		out = append(out, f)
	}
	return out
}

// mergeResources returns base with each value that over sets.
func mergeResources(base, over ax.ResourcesSpec) ax.ResourcesSpec {
	pick := func(b, o string) string {
		if strings.TrimSpace(o) != "" {
			return o
		}
		return b
	}
	return ax.ResourcesSpec{
		Requests: ax.ResourceList{CPU: pick(base.Requests.CPU, over.Requests.CPU), Memory: pick(base.Requests.Memory, over.Requests.Memory)},
		Limits:   ax.ResourceList{CPU: pick(base.Limits.CPU, over.Limits.CPU), Memory: pick(base.Limits.Memory, over.Limits.Memory)},
	}
}
