// Package pools keeps sets of ax Tasks that run one agent profile
// (docs/ARCHITECTURE.md section 14). A pool names the image, the fw
// configuration of the workers, their environment, their egress, and the
// number of Tasks. This package reads and checks the pools of the block
// workbench.pools and the worker templates of workbench.templates, builds
// the runtime pools that people create from a template, renders the
// Workspace and the Task of each worker, and reconciles the Tasks of a
// pool with the ax API. The fleet runs the reconciler at every interval
// and shows the state of each pool.
package pools

import (
	"bytes"
	"errors"
	"fmt"
	"net/url"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/bdubs00/featherweight-kernel/ax"
	"github.com/bdubs00/featherweight-kernel/config"
	"github.com/bdubs00/featherweight-kernel/fw"
	v1alpha1 "github.com/google/ax/pkg/apis/v1alpha1"
	"gopkg.in/yaml.v3"
)

// block is the top-level key of fw.yaml that the workbench claims (the
// package teams registers it), and key is the key of the pools in it.
const (
	block = "workbench"
	key   = "pools"
)

// Variables that the front sets in every Task of a pool. FW_POOL marks the
// owner, and FW_POOL_SPEC holds the hash of the rendered specification, so
// the front sees which Tasks run an older configuration. FW_POOL_GENERATION
// holds the generation of the pool when it is above 0, so a front sees a
// Task of a newer configuration than its own. FW_POOL_BASE holds, beside
// it, the hash of the configuration alone, without the overlay of the
// front, so a front tells two configurations of one generation from a
// change of the overlay.
const (
	EnvPool       = "FW_POOL"
	EnvSpec       = "FW_POOL_SPEC"
	EnvGeneration = "FW_POOL_GENERATION"
	EnvBase       = "FW_POOL_BASE"
)

// Defaults and fixed places of a pool.
const (
	// DefaultIntake is the subject prefix of the intake queues when
	// workbench.fleet.intake names none.
	DefaultIntake = "fw.jobs"
	// WorkspacePath is where the Workspace of a worker is bound. The kernel
	// reads its configuration from ConfigFile under it.
	WorkspacePath = "/workspace"
	ConfigFile    = ".fw/fw.yaml"
	// DefaultTokenEnv is the variable of the static token when auth.tokenEnv
	// names none.
	DefaultTokenEnv = "FW_API_TOKEN"
)

// Pool is one entry of workbench.pools. Load fills the defaults and the
// rendered configuration of the workers.
//
// Name names the pool and its Tasks, <name>-<index>. Atespace is the
// atespace of the Tasks; it defaults to the only atespace of
// workbench.fleet.atespaces. Replicas is the number of Tasks, and it has
// no default. Image is the worker image by digest. Agent is the profile
// of fw.agents whose jobs the workers take; it defaults to the only
// profile. Intake is the queue of the workers. By default it is a queue of
// the pool alone: "<workbench.fleet.intake>.<agent>" for a pool named after
// its agent (NameAfter), such as a runtime pool of agent_put, and
// "<workbench.fleet.intake>.<name>" for any other pool (defaultIntake).
// Revision is free text: a new value replaces every Task, for example to
// start every worker again with a new Workspace. Generation orders the
// configurations of the pool: a front never replaces a Task of a higher
// generation than its own, so a replica with an older configuration
// leaves the pool to the newer one. 0 keeps the order of the hash alone.
//
// Env holds the variables of the workers, each with a value or with
// FromEnv, the name of a variable of the front, so the file holds no
// secret. Egress is spec.egress of each Task, Resources its resources,
// and Files the extra files of its Workspace. Packages turns the packages
// of the workers on and names them (packages.go). The overlay of the pool
// wins over it. FW is the fw.yaml of the workers: the agents, the policy,
// the model, and the providers.
//
// Template names an entry of workbench.templates. The pool then takes
// every key of the template that it does not set (Template.apply), and its
// generation is its own plus the one of the template.
type Pool struct {
	Name       string           `yaml:"name"`
	Template   string           `yaml:"template,omitempty"`
	Atespace   string           `yaml:"atespace,omitempty"`
	Replicas   *int             `yaml:"replicas"`
	Image      string           `yaml:"image"`
	Agent      string           `yaml:"agent,omitempty"`
	Intake     string           `yaml:"intake,omitempty"`
	Revision   string           `yaml:"revision,omitempty"`
	Generation int              `yaml:"generation,omitempty"`
	Env        []EnvVar         `yaml:"env,omitempty"`
	Egress     *ax.EgressSpec   `yaml:"egress,omitempty"`
	Resources  ax.ResourcesSpec `yaml:"resources,omitempty"`
	Files      []ax.File        `yaml:"files,omitempty"`
	Packages   *Packages        `yaml:"packages,omitempty"`
	FW         map[string]any   `yaml:"fw"`

	// The parts that Load fills.
	index    int        // the position in workbench.pools
	tokenEnv string     // the variable under which a worker reads the static token
	worker   *fw.Config // the configuration of the workers, decoded
	env      []EnvVar   // Env and the variables of the front, in the order of the Task
	files    []ax.File  // the rendered fw.yaml first, then Files
	hash     string
	hashFile string // the fw.yaml that the hash covers: without the lists of the packages (packages.go)
	base     string // the hash without the overlay, when WithOverlay merged one
	warnings []string
	front    Front   // what the pool took from the front, for a render with an overlay
	overlay  Overlay // the overlay that WithOverlay merged, empty for the configuration alone

	// intakeSet is true when the configuration sets Intake, so the intake
	// is no default (defaultIntake).
	intakeSet bool

	// label names the pool in an error in place of its position in
	// workbench.pools: a template or a runtime pool. lenient leaves out
	// the check of the model, for a template and for a runtime pool before
	// its overlay gives it a profile. probe marks the pool with which Load
	// checks a template. runtime is the entry of a runtime pool, nil for a
	// pool of the file.
	label   string
	lenient bool
	probe   bool
	runtime *Runtime

	// The skills of the front that the merged render ships (skills.go):
	// the files of the Workspace, the names of the skills, and the files
	// that it left out.
	skillFiles   []ax.File
	skillNames   []string
	skillSkipped []string
}

// EnvVar is one variable of the workers. Exactly one of Value and FromEnv
// is set. FromEnv names a variable of the front, which the front reads at
// its start and copies into each Task.
type EnvVar struct {
	Name    string  `yaml:"name"`
	Value   *string `yaml:"value,omitempty"`
	FromEnv string  `yaml:"fromEnv,omitempty"`
}

// Front is what the pools take from the configuration of the front.
type Front struct {
	Atespaces   []string       // workbench.fleet.atespaces
	Egress      string         // workbench.fleet.egress
	Intake      string         // workbench.fleet.intake
	EgressTypes []string       // the event types of the stream of a worker
	MaxBytes    int            // bus.egressMaxBytes of a worker that the pool does not set; 0 sets none
	Bus         fw.BusConfig   // the bus of the front, which the workers share
	AX          fw.AXConfig    // the ax API and the router of the front
	TokenEnv    string         // the variable of the static token of the front
	SkillDirs   []string       // the skills directories of the front, which a merged render ships (skills.go)
	Packages    *PackageMirror // workbench.packages, nil when the packages are off (packages.go)
}

// Replicas returns the number of Tasks that the pool wants.
func (p *Pool) replicas() int {
	if p.Replicas == nil {
		return 0
	}
	return *p.Replicas
}

// Wanted returns the number of Tasks that the pool wants.
func (p *Pool) Wanted() int { return p.replicas() }

// Hash returns the hash of the rendered specification of the pool. Every
// Task of the pool that runs the current configuration carries it in
// FW_POOL_SPEC.
func (p *Pool) Hash() string { return p.hash }

// baseHash returns the hash of the configuration of the pool alone,
// without the overlay of the front.
func (p *Pool) baseHash() string {
	if p.base != "" {
		return p.base
	}
	return p.hash
}

// Warnings returns what Load found odd but not wrong, such as a bus that
// no egress rule reaches. The front logs them at its start, and the dry
// run prints them.
func (p *Pool) Warnings() []string { return p.warnings }

// AgentAccess returns the access block of the profile of the pool in the
// configuration of the workers with the overlay, or nil.
func (p *Pool) AgentAccess() *fw.Access {
	return p.Profiles()[p.Agent].Access
}

// Source says where the pool comes from: SourceFile for an entry of
// workbench.pools, SourceRuntime for a pool of the store of the front.
func (p *Pool) Source() string {
	if p.runtime != nil {
		return SourceRuntime
	}
	return SourceFile
}

// Runtime returns the entry of a runtime pool, or nil for a pool of the
// file.
func (p *Pool) Runtime() *Runtime {
	if p.runtime == nil {
		return nil
	}
	c := *p.runtime
	return &c
}

// TaskName returns the name of the Task of the pool at index.
func (p *Pool) TaskName(index int) string { return p.Name + "-" + strconv.Itoa(index) }

// WorkspaceName returns the name of the Workspace of the Task at index.
func (p *Pool) WorkspaceName(index int) string { return p.TaskName(index) + "-ws" }

// key names the pool in an error: its position and its name, or its
// label.
func (p *Pool) key() string {
	if p.label != "" {
		return p.label
	}
	if p.Name == "" {
		return fmt.Sprintf("workbench.pools[%d]", p.index)
	}
	return fmt.Sprintf("workbench.pools[%d] (%s)", p.index, p.Name)
}

// FromEnv lists the variables of the front that the Tasks of the pools
// copy, sorted, the static token included.
func FromEnv(list []*Pool) []string {
	seen := map[string]bool{}
	for _, p := range list {
		for _, e := range p.env {
			if e.FromEnv != "" {
				seen[e.FromEnv] = true
			}
		}
	}
	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// Load reads the pools of workbench.pools from the configuration of the
// front, fills the defaults, renders the configuration of the workers, and
// checks every value. It returns no pool and no error when the block
// names none, and one error that lists every problem, each with its key
// and what to set, when a value does not fit. It checks the templates of
// workbench.templates too, because a pool can name one (LoadAll).
func Load(cfg *fw.Config, front Front) ([]*Pool, error) {
	list, _, err := LoadAll(cfg, front)
	return list, err
}

// LoadAll is Load for the pools and the templates of the configuration of
// the front. It checks each template as a pool of one worker, as far as
// the checks of a pool apply without an agent, and gives a pool that names
// a template every key of the template that the pool does not set.
func LoadAll(cfg *fw.Config, front Front) ([]*Pool, []*Template, error) {
	list, err := decode(cfg)
	if err != nil {
		return nil, nil, err
	}
	templates, err := decodeTemplates(cfg)
	if err != nil {
		return nil, nil, err
	}
	if len(list) == 0 && len(templates) == 0 {
		return nil, nil, nil
	}
	if front.TokenEnv == "" {
		front.TokenEnv = DefaultTokenEnv
	}
	what := "workbench.pools"
	if len(list) == 0 {
		what = "workbench.templates"
	}
	problems := front.problems(what)
	problems = append(problems, checkTemplates(templates, front)...)
	names := map[string]int{}
	for i, p := range list {
		if p == nil {
			problems = append(problems, fmt.Sprintf("workbench.pools[%d] is empty", i))
			continue
		}
		p.index = i
		p.Name = strings.TrimSpace(p.Name)
		if j, ok := names[p.Name]; ok && p.Name != "" {
			problems = append(problems, fmt.Sprintf("%s has the name of workbench.pools[%d]; each pool needs a name of its own", p.key(), j))
		}
		names[p.Name] = i
		if p.Template = strings.TrimSpace(p.Template); p.Template != "" {
			t := TemplateNamed(templates, p.Template)
			if t == nil {
				problems = append(problems, fmt.Sprintf("%s: template %q is not in workbench.templates; %s", p.key(), p.Template, templateChoice(templates)))
				continue
			}
			t.apply(p)
		}
		problems = append(problems, p.complete(front)...)
	}
	problems = append(problems, SharedIntakes(list)...)
	if len(problems) > 0 {
		return nil, nil, fmt.Errorf("pools: %s", strings.Join(problems, "; "))
	}
	return list, templates, nil
}

// SharedIntakes lists the pools of list whose intake is the intake of an
// earlier pool of list. The workers of such pools take the jobs of each
// other, so a job can reach a worker that runs another profile. Two pools
// that both set the same intake share the queue on purpose, and that is no
// problem. Load checks the pools of the file with it, and the doctor checks
// them with the runtime pools.
func SharedIntakes(list []*Pool) []string {
	var problems []string
	first := map[string]*Pool{}
	for _, p := range list {
		if p == nil || p.Intake == "" {
			continue
		}
		q, ok := first[p.Intake]
		if !ok {
			first[p.Intake] = p
			continue
		}
		if q.intakeSet && p.intakeSet {
			continue
		}
		// A person sets no intake on a runtime pool, so the pool of the
		// file takes another one.
		fix := fmt.Sprintf("set intake to a queue of its own on one of them, or set intake %s on both to share the queue on purpose", p.Intake)
		switch {
		case p.runtime != nil && q.runtime == nil:
			fix = "set intake to a queue of its own on " + q.key()
		case q.runtime != nil && p.runtime == nil:
			fix = "set intake to a queue of its own on " + p.key()
		}
		problems = append(problems, fmt.Sprintf("%s: the intake %s is the intake of %s too, so the workers of both pools would take the jobs of each other; %s",
			p.key(), p.Intake, q.key(), fix))
	}
	return problems
}

// decode reads workbench.pools strictly: a key that a pool does not know
// is an error that names it. The block fw of a pool is free here and
// checked as a configuration of fw later (complete).
func decode(cfg *fw.Config) ([]*Pool, error) {
	var out []*Pool
	if err := decodeKey(cfg, key, &out, "workbench.pools is a list of pools, each with name, replicas, image, and fw"); err != nil {
		return nil, err
	}
	return out, nil
}

// decodeKey reads the key k of the block workbench into out strictly. A
// configuration without the key leaves out as it is. shape says what the
// key holds, for the error.
func decodeKey(cfg *fw.Config, k string, out any, shape string) error {
	if cfg == nil {
		return nil
	}
	n, ok := cfg.Extensions[block]
	if !ok {
		return nil
	}
	root := &n
	if root.Kind == yaml.DocumentNode && len(root.Content) > 0 {
		root = root.Content[0]
	}
	if root.Kind != yaml.MappingNode {
		return nil
	}
	var node *yaml.Node
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value == k {
			node = root.Content[i+1]
		}
	}
	if node == nil || (node.Kind == yaml.ScalarNode && node.Tag == "!!null") {
		return nil
	}
	data, err := yaml.Marshal(node)
	if err != nil {
		return fmt.Errorf("pools: read workbench.%s: %w", k, err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(out); err != nil {
		return fmt.Errorf("pools: %s: %w", shape, err)
	}
	return nil
}

// problems lists what the front lacks for any pool. what names the block
// that needs it.
func (f Front) problems(what string) []string {
	var p []string
	if len(f.Atespaces) == 0 {
		p = append(p, what+" needs workbench.fleet.atespaces, the atespaces whose Tasks the server lists")
	}
	if strings.TrimSpace(f.Egress) == "" {
		p = append(p, what+" needs workbench.fleet.egress, the stream on which the server sees the runs of the workers")
	}
	if d := strings.TrimSpace(f.Bus.Driver); d == "" || d == "mem" {
		p = append(p, what+" needs a bus that the workers share with the server; set bus.driver to nats or redis")
	}
	if strings.TrimSpace(f.AX.Server) == "" {
		p = append(p, what+" needs ax.server, the ax API that creates the Tasks")
	}
	if strings.TrimSpace(f.AX.Router) == "" {
		p = append(p, what+" needs ax.router, the router through which the server asks a worker whether it works")
	}
	return p
}

// digest matches an image reference that names its digest.
var digest = regexp.MustCompile(`@sha256:[0-9a-f]{64}$`)

// envName matches the name of a variable.
var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// complete fills the defaults of one pool, renders the configuration of
// its workers, and lists its problems.
func (p *Pool) complete(front Front) []string {
	p.front = front
	p.intakeSet = strings.TrimSpace(p.Intake) != ""
	var problems []string
	add := func(format string, args ...any) {
		problems = append(problems, p.key()+": "+fmt.Sprintf(format, args...))
	}
	p.Name = strings.TrimSpace(p.Name)
	switch {
	case p.Name == "":
		add("set name, the name of the pool and the prefix of its Tasks, such as triage")
	case v1alpha1.ValidateName(p.Name) != nil:
		add("name %q is not a lowercase name of letters, digits, and hyphens that starts and ends with a letter or a digit", p.Name)
	}
	switch {
	case p.Replicas == nil:
		add("set replicas, the number of Tasks of the pool; 0 removes every Task")
	case *p.Replicas < 0:
		add("replicas %d is below 0; set 0 or more", *p.Replicas)
	}
	if p.Generation < 0 {
		add("generation %d is below 0; set 0 or more, and raise it with each change of the pool", p.Generation)
	}
	if n := len(p.WorkspaceName(p.replicas())); p.Name != "" && n > v1alpha1.MaxNameLength {
		add("the name %q is too long for %d Tasks: the Workspace %s has %d characters, and ax takes %d", p.Name, p.replicas(), p.WorkspaceName(p.replicas()), n, v1alpha1.MaxNameLength)
	}
	p.Atespace = strings.TrimSpace(p.Atespace)
	if p.Atespace == "" && len(front.Atespaces) == 1 {
		p.Atespace = front.Atespaces[0]
	}
	switch {
	case p.Atespace == "" && len(front.Atespaces) > 1:
		add("set atespace: workbench.fleet.atespaces names %s", strings.Join(front.Atespaces, ", "))
	case p.Atespace != "" && !slices.Contains(front.Atespaces, p.Atespace):
		add("atespace %q is not in workbench.fleet.atespaces; add it there, so the server lists the Tasks of the pool", p.Atespace)
	}
	p.Image = strings.TrimSpace(p.Image)
	switch {
	case p.Image == "":
		add("set image, the worker image by digest, such as ghcr.io/example/fw@sha256:<64 hex digits>")
	case !digest.MatchString(p.Image):
		add("image %q has no digest: Substrate pulls an actor image only by digest, so write it as <image>@sha256:<64 hex digits>", p.Image)
	}
	if p.Egress == nil || len(p.Egress.Hosts) == 0 {
		add("set egress.hosts, the hosts that a worker reaches: without a host a worker reaches neither the bus nor the model")
	} else {
		for i, h := range p.Egress.Hosts {
			if _, err := v1alpha1.ParseEgressHost(h); err != nil {
				add("egress.hosts[%d]: %v", i, err)
			}
		}
	}
	problems = append(problems, p.checkEnv(front)...)
	problems = append(problems, p.checkFiles()...)
	problems = append(problems, p.checkPackages(front)...)
	problems = append(problems, p.render(front)...)
	if len(problems) > 0 {
		return problems
	}
	// The ax API checks a Task and a Workspace with these functions before
	// it saves them, so a problem shows at the start, not at the first
	// create.
	task, ws, err := p.Render(0, Placeholders)
	if err != nil {
		return append(problems, p.key()+": "+err.Error())
	}
	if err := v1alpha1.ValidateTask(task); err != nil {
		add("the Task does not fit the ax API: %v", err)
	}
	if err := v1alpha1.ValidateWorkspace(ws); err != nil {
		add("the Workspace does not fit the ax API: %v", err)
	}
	if p.Generation > 0 {
		if err := v1alpha1.ValidateWorkspace(p.Marker()); err != nil {
			add("the Workspace %s, which records the generation, does not fit the ax API: %v", p.MarkerName(), err)
		}
	}
	if len(problems) == 0 {
		p.warnings = p.reach()
	}
	return problems
}

// EnvBashHide names the variables that the bash tool of a worker does not
// get. The front sets it, because AX_TASK_YAML holds every variable of the
// Task, the static token included.
const EnvBashHide = "FW_BASH_HIDE"

// reserved returns why a pool cannot set the variable name, or "".
func (p *Pool) reserved(name, tokenEnv string) string {
	switch {
	case name == tokenEnv:
		return "the server gives each worker its own static token from its environment, never from the file"
	case name == EnvPool:
		return "the server sets it to the name of the pool"
	case name == EnvSpec:
		return "the server sets it to the hash of the configuration of the pool"
	case name == EnvBase:
		return "the server sets it to the hash of the configuration of the pool without its overlay"
	case name == EnvGeneration:
		return "the server sets it to the generation of the pool; set generation instead"
	case name == "FW_CONFIG":
		return "the pool writes the configuration of the workers to " + WorkspacePath + "/" + ConfigFile
	case strings.HasPrefix(name, "FW_"):
		return "a variable FW_* changes the configuration of the worker after the checks of the pool; set the key in fw instead"
	case strings.HasPrefix(name, "AX_"):
		return "ax sets the variables AX_*"
	}
	return ""
}

// checkEnv checks the variables of the workers.
func (p *Pool) checkEnv(front Front) []string {
	var problems []string
	add := func(i int, format string, args ...any) {
		problems = append(problems, fmt.Sprintf("%s.env[%d]: ", p.key(), i)+fmt.Sprintf(format, args...))
	}
	// The worker reads its token under its own auth.tokenEnv.
	p.tokenEnv = DefaultTokenEnv
	if auth, ok := p.FW["auth"].(map[string]any); ok {
		if v, ok := auth["tokenEnv"].(string); ok && strings.TrimSpace(v) != "" {
			p.tokenEnv = strings.TrimSpace(v)
		}
	}
	seen := map[string]bool{}
	for i, e := range p.Env {
		e.Name = strings.TrimSpace(e.Name)
		e.FromEnv = strings.TrimSpace(e.FromEnv)
		p.Env[i] = e
		switch {
		case e.Name == "":
			add(i, "set name")
			continue
		case !envName.MatchString(e.Name):
			add(i, "name %q is not a variable name of letters, digits, and underscores", e.Name)
		case p.reserved(e.Name, p.tokenEnv) != "":
			add(i, "remove %s: %s", e.Name, p.reserved(e.Name, p.tokenEnv))
		case seen[e.Name]:
			add(i, "%s is set twice", e.Name)
		}
		seen[e.Name] = true
		switch {
		case e.Value != nil && e.FromEnv != "":
			add(i, "%s sets value and fromEnv; set one of them", e.Name)
		case e.Value == nil && e.FromEnv == "":
			add(i, "%s needs value, or fromEnv with the name of a variable of the server", e.Name)
		case e.FromEnv != "" && !envName.MatchString(e.FromEnv):
			add(i, "fromEnv %q is not a variable name", e.FromEnv)
		}
	}
	// The bash tool of a worker gets neither the manifest of the Task,
	// which holds every variable, nor a value that the front copies.
	names := []string{ax.EnvTaskYAML, p.tokenEnv}
	for _, e := range p.Env {
		if e.FromEnv != "" && e.Name != "" {
			names = append(names, e.Name)
		}
	}
	p.env = append([]EnvVar(nil), p.Env...)
	name, hide := p.Name, strings.Join(names, ",")
	p.env = append(p.env,
		EnvVar{Name: EnvPool, Value: &name},
		EnvVar{Name: EnvBashHide, Value: &hide},
		EnvVar{Name: p.tokenEnv, FromEnv: front.TokenEnv},
	)
	return problems
}

// checkFiles checks the extra files of the Workspace.
func (p *Pool) checkFiles() []string {
	var problems []string
	seen := map[string]bool{}
	for i, f := range p.Files {
		path := strings.TrimPrefix(strings.TrimSpace(f.Path), "./")
		key := fmt.Sprintf("%s.files[%d]", p.key(), i)
		switch {
		case path == "":
			problems = append(problems, key+": set path, relative to "+WorkspacePath)
		case strings.HasPrefix(path, "/") || slices.Contains(strings.Split(path, "/"), ".."):
			problems = append(problems, fmt.Sprintf("%s: path %q leaves the Workspace; write it relative to %s", key, f.Path, WorkspacePath))
		case path == ConfigFile:
			problems = append(problems, fmt.Sprintf("%s: the pool writes %s from fw; move the configuration of the workers there", key, ConfigFile))
		case seen[path]:
			problems = append(problems, fmt.Sprintf("%s: %s is listed twice", key, path))
		}
		seen[path] = true
		p.Files[i].Path = path
	}
	return problems
}

// render builds the fw.yaml of the workers from fw and the defaults of the
// front, checks it as the kernel checks a configuration, and computes the
// hash of the pool.
func (p *Pool) render(front Front) []string {
	var problems []string
	add := func(format string, args ...any) {
		problems = append(problems, p.key()+": "+fmt.Sprintf(format, args...))
	}
	if len(p.FW) == 0 {
		add("set fw, the configuration of the workers, with model and agents")
		return problems
	}
	if bus, ok := p.FW["bus"].(map[string]any); ok {
		for _, k := range []struct{ key, from string }{{"intake", "intake of the pool"}, {"egress", "workbench.fleet.egress"}, {"egressTypes", "event types that the server reads"}} {
			if _, set := bus[k.key]; set {
				add("remove fw.bus.%s: the pool sets it from the %s", k.key, k.from)
			}
		}
	}
	keys := topKeys()
	for _, k := range sortedKeys(p.FW) {
		if !keys[k] {
			add("fw.%s is not a key of the configuration of fw", k)
		}
	}
	text, err := p.workerFile(front, false)
	if err != nil {
		add("fw: %v", err)
		return problems
	}
	wc := fw.DefaultConfig()
	dec := yaml.NewDecoder(strings.NewReader(text))
	dec.KnownFields(true)
	if err := dec.Decode(wc); err != nil {
		add("fw is not a configuration of fw: %v", err)
		return problems
	}
	var ve *config.ValidationError
	if err := config.Validate(wc); errors.As(err, &ve) {
		for _, problem := range ve.Problems {
			add("fw.%s", problem)
		}
	} else if err != nil {
		add("fw: %v", err)
	}
	p.worker = wc
	if len(wc.Agents) == 0 {
		add("set fw.agents with the profile that the workers run")
	}
	p.Agent = strings.TrimSpace(p.Agent)
	if p.Agent == "" && len(wc.Agents) == 1 {
		for name := range wc.Agents {
			p.Agent = name
		}
	}
	prof, known := wc.Agents[p.Agent]
	switch {
	case p.Agent == "" && len(wc.Agents) > 1:
		add("set agent, the profile of fw.agents whose queue the workers take")
	case p.Agent != "" && !known:
		add("agent %q is not a key of fw.agents", p.Agent)
	}
	if strings.TrimSpace(wc.Model) == "" && strings.TrimSpace(prof.Model) == "" && !p.lenient {
		add("set fw.model, the provider/model selector of the workers, such as openai/<model>: a pool has no default model")
	}
	if !slices.Contains(wc.Auth.Modes(), "token") {
		add("fw.auth.mode %q leaves out token: the server reads a worker with the static token, so list token there", wc.Auth.Mode)
	}
	switch {
	case wc.Bus.URL == "" && credential(front.Bus.URL):
		add("the bus URL of the server holds a credential, which the pool does not copy into a Workspace; set fw.bus.url, with $${NAME} for the credential and NAME in env with fromEnv")
	case credential(wc.Bus.URL):
		add("fw.bus.url holds a credential, which the Workspace and the dry run show; write $${NAME} for it and set NAME in env with fromEnv")
	}
	if strings.TrimSpace(wc.Bus.Driver) != strings.TrimSpace(front.Bus.Driver) {
		add("fw.bus.driver %q is not the bus of the server, %q: the server sees the runs of a worker only on its own bus", wc.Bus.Driver, front.Bus.Driver)
	}
	switch blind := tokenBlind(wc); {
	case len(blind) > 0 && p.probe:
		add("the static token cannot see the runs of an agent that a person creates from the template, because such an agent has an access block with its owner, so the server cannot tell that a worker is idle before it replaces or deletes it; add kind:token to fw.access.admins")
	case len(blind) > 0:
		add("the static token cannot see the runs of the agent %s, so the server cannot tell that a worker is idle before it replaces or deletes it; add kind:token to fw.access.admins", strings.Join(blind, ", "))
	}
	// The default intake holds the name of the pool, so a pool without a
	// name, which complete refuses, has none.
	if p.Intake == "" && p.Agent != "" && p.Name != "" {
		p.Intake = defaultIntake(front.Intake, p.Name, p.Agent)
	}
	p.Intake = strings.TrimSpace(p.Intake)
	if p.Name != "" && !subject(p.Intake) {
		add("intake %q is not a subject: it holds an empty token, a space, or a wildcard", p.Intake)
	}
	if len(problems) > 0 {
		return problems
	}
	// The intake is part of the file, so the file is rendered again now
	// that it is known.
	if text, err = p.workerFile(front, false); err != nil {
		add("fw: %v", err)
		return problems
	}
	p.files = append(append([]ax.File{{Path: ConfigFile, Content: text}}, p.Files...), p.skillFiles...)
	if p.hashFile, err = p.workerFile(front, true); err != nil {
		add("fw: %v", err)
		return problems
	}
	p.hash = p.computeHash()
	return problems
}

// topKeys returns the top-level keys of a configuration of fw. The kernel
// keeps the blocks that a linked package claims in an inline map, so a
// strict decode takes any key there; a worker claims none.
func topKeys() map[string]bool {
	out := map[string]bool{}
	t := reflect.TypeOf(fw.Config{})
	for i := 0; i < t.NumField(); i++ {
		name, _, _ := strings.Cut(t.Field(i).Tag.Get("yaml"), ",")
		if name != "" && name != "-" {
			out[name] = true
		}
	}
	return out
}

// tokenBlind lists the agents whose runs the static token of the front
// cannot view under the access rules of the worker, sorted. The front
// reads the status of a worker with that token before it deletes the
// Task, and the status counts only the runs that the caller can view.
func tokenBlind(c *fw.Config) []string {
	tok := &fw.Principal{Subject: fw.KindToken, Kind: fw.KindToken}
	if fw.MatchesAny(c.Access.Admins, tok) {
		return nil
	}
	open := !strings.EqualFold(strings.TrimSpace(c.Access.Default), "closed")
	var out []string
	for name, prof := range c.Agents {
		switch {
		case prof.Access == nil && open:
		case prof.Access != nil && fw.AccessRoleOf(prof.Access, tok) >= fw.AccessViewer:
		default:
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// defaultIntake returns the intake of a pool that sets none, a queue that
// no other pool takes by default: "<prefix>.<agent>" for a pool named
// after its agent (NameAfter), such as a runtime pool of agent_put, and
// "<prefix>.<pool>" for any other pool, such as the pool crew-reviewer of
// the agent reviewer. The prefix is workbench.fleet.intake, else
// DefaultIntake.
func defaultIntake(prefix, pool, agent string) string {
	prefix = strings.TrimSpace(prefix)
	if prefix == "" {
		prefix = DefaultIntake
	}
	if name, ok := NameAfter(agent); ok && name == pool {
		return prefix + "." + agent
	}
	return prefix + "." + pool
}

// subject reports whether s is a subject without wildcards.
func subject(s string) bool {
	if s == "" {
		return false
	}
	for _, t := range strings.Split(s, ".") {
		if t == "" || strings.ContainsAny(t, " \t*>") {
			return false
		}
	}
	return true
}

// reference matches a variable reference ${NAME}, which the worker
// expands.
var reference = regexp.MustCompile(`\$\{[^}]*\}`)

// credential reports whether a URL holds a credential as text: a password,
// or a user name without a password, which NATS reads as a token. A
// reference ${NAME} is no credential, and a user name before a password is
// none either.
func credential(raw string) bool {
	if raw == "" {
		return false
	}
	const ref = "fwref"
	u, err := url.Parse(reference.ReplaceAllString(raw, ref))
	if err != nil {
		// The parse error can name the URL, so the caller says no more.
		return strings.Contains(raw, "@")
	}
	if u.User == nil {
		return false
	}
	secret := u.User.Username()
	if pw, ok := u.User.Password(); ok {
		secret = pw
	}
	return strings.ReplaceAll(secret, ref, "") != ""
}

// header is the first line of the fw.yaml of a worker. It tells a reader
// where the file comes from, and the front reads it to know that a
// Workspace belongs to the pool.
//
// Its words never change: the front reads them, and they are part of the
// hash of the pool, so a new wording replaces every worker.
func header(pool string) string {
	return "# This file is rendered by the pool " + pool + " of fw-workbench. Change the pool in the configuration of the front, not this file.\n"
}

// workerFile renders the fw.yaml of the workers: the defaults of the
// front, then fw over them, then the keys that the pool sets. With hash,
// it renders the file that the hash of the pool covers, without the lists
// of the packages.
func (p *Pool) workerFile(front Front, hash bool) (string, error) {
	bus := map[string]any{"driver": front.Bus.Driver}
	if front.Bus.URL != "" && !credential(front.Bus.URL) {
		bus["url"] = front.Bus.URL
	}
	if front.Bus.Driver == "nats" {
		// A Task runs one run at a time, and the NATS intake takes four
		// messages at a time by default, so one worker would hold four jobs
		// while the others idle.
		bus["options"] = map[string]any{"concurrency": "1"}
	}
	defaults := map[string]any{
		"auth": map[string]any{"mode": "token"},
		"bus":  bus,
		"ax":   map[string]any{"server": front.AX.Server, "router": front.AX.Router},
	}
	m := merge(defaults, p.FW)
	if front.Packages != nil {
		// Every pool of a front with a package mirror gets the
		// infrastructure of the packages, and the lists of its setting
		// with the overlay (packages.go).
		m = withPackages(m, p.PackageSetting(), front.Packages, hash)
	}
	b, _ := m["bus"].(map[string]any)
	if b == nil {
		return "", fmt.Errorf("bus is not a mapping")
	}
	b["egress"] = front.Egress
	b["egressTypes"] = append([]string(nil), front.EgressTypes...)
	if runsRoutines(m) {
		// The events of the routines go out only from a pool whose
		// profiles run one, so a front that adds them replaces no Task of
		// a pool without routines; the routines replace the Tasks anyway.
		b["egressTypes"] = append(b["egressTypes"].([]string), RoutineEventTypes...)
	}
	if _, set := b["egressMaxBytes"]; !set && front.MaxBytes > 0 {
		// The stream carries the inputs and the outputs of the tool calls,
		// so the worker cuts their long strings, unless the pool says
		// otherwise.
		b["egressMaxBytes"] = front.MaxBytes
	}
	if p.Intake != "" {
		b["intake"] = p.Intake
	}
	out, err := encode(m)
	if err != nil {
		return "", err
	}
	return header(p.Name) + out, nil
}

// RoutineEventTypes are the event types that a worker publishes beside
// those of the front when a profile of its pool runs a routine: the
// decision, the end, and the comparison of a routine (the evidence of the
// front, docs/ARCHITECTURE.md section 21.4).
var RoutineEventTypes = []string{fw.EvRoutineDecided, fw.EvRoutineApplied, fw.EvRoutineCompared}

// runsRoutines reports whether a profile of the block fw of a worker
// carries a routine.
func runsRoutines(m map[string]any) bool {
	agents, _ := m["agents"].(map[string]any)
	for _, v := range agents {
		if p, ok := v.(map[string]any); ok {
			if list, ok := p["routines"].([]any); ok && len(list) > 0 {
				return true
			}
		}
	}
	return false
}

// merge returns base with over on top. A mapping in both merges key by
// key, and any other value of over replaces the one of base. Neither
// argument changes.
func merge(base, over map[string]any) map[string]any {
	out := make(map[string]any, len(base)+len(over))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range over {
		bm, bok := out[k].(map[string]any)
		om, ook := v.(map[string]any)
		if bok && ook {
			out[k] = merge(bm, om)
			continue
		}
		out[k] = v
	}
	return out
}

// reach lists the connections of the workers that no egress rule allows:
// the bus, the providers, the MCP servers, and the token broker. A worker
// cannot open such a connection under Substrate, so the front warns. It
// checks the http, https, ws, and wss addresses only, and a value that the
// worker expands itself is left out.
func (p *Pool) reach() []string {
	egress := p.taskEgress()
	if p.worker == nil || egress == nil {
		return nil
	}
	type target struct{ what, raw string }
	targets := []target{{"the bus of the workers (fw.bus.url)", p.worker.Bus.URL}}
	for _, name := range sortedKeys(p.worker.Providers) {
		targets = append(targets, target{"the provider " + name + " (fw.providers." + name + ".baseUrl)", p.worker.Providers[name].BaseURL})
	}
	for _, s := range p.worker.Tools.MCP {
		targets = append(targets, target{"the MCP server " + s.Name, s.Endpoint})
	}
	targets = append(targets, target{"the token broker (fw.tokens.url)", p.worker.Tokens.URL})
	var out []string
	for _, t := range targets {
		if t.raw == "" || strings.Contains(t.raw, "${") {
			continue
		}
		u, err := url.Parse(t.raw)
		if err != nil || u.Hostname() == "" {
			continue
		}
		proto, port := "", u.Port()
		switch u.Scheme {
		case "http", "ws":
			proto = "http"
			if port == "" {
				port = "80"
			}
		case "https", "wss":
			proto = "https"
			if port == "" {
				port = "443"
			}
		default:
			continue
		}
		n, _ := strconv.Atoi(port)
		if !allows(egress, proto, strings.ToLower(u.Hostname()), int32(n)) {
			// A plain host, TLS forwarded without decryption, serves an
			// https connection without the trust bundle of the gateway.
			example := fmt.Sprintf("%s:%d", u.Hostname(), n)
			if proto == "http" {
				example = "http://" + example
			}
			out = append(out, fmt.Sprintf("%s: egress.hosts allows no %s connection to %s:%d, %s; add a host such as %s",
				p.key(), proto, u.Hostname(), n, t.what, example))
		}
	}
	return out
}

// allows reports whether a host of e allows a connection of the protocol
// to host and port. A plain host, TLS that the gateway forwards without
// decryption, allows an https connection too.
func allows(e *ax.EgressSpec, proto, host string, port int32) bool {
	for _, entry := range e.Hosts {
		h, err := v1alpha1.ParseEgressHost(entry)
		if err != nil || h.Port != port {
			continue
		}
		if h.Protocol != proto && !(h.Protocol == v1alpha1.EgressTLS && proto == "https") {
			continue
		}
		if hostMatches(h.Host, host) {
			return true
		}
	}
	return false
}

// hostMatches reports whether the name of an egress host, a name or "*."
// and a name, matches host.
func hostMatches(pattern, host string) bool {
	if suffix, ok := strings.CutPrefix(pattern, "*"); ok {
		return strings.HasSuffix(host, suffix) && len(host) > len(suffix)
	}
	return pattern == host
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
