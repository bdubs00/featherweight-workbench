package pools

// This file holds the packages of a pool (docs/ARCHITECTURE.md section
// 14.11). The block workbench.packages names the package mirror of the
// deployment. Without it, the feature is off for the whole front. An entry
// of workbench.pools, a template of workbench.templates, and the overlay
// of a pool can turn the packages on for the workers of the pool and name
// them in two lists: Python packages for uv, and Programs from
// conda-forge for pixi. The overlay wins over the configuration. The
// render then gives the workers the block environment of the kernel with
// the lists and the mirrors, the egress of the mirror, the tool env, and
// the policy rules that keep each change of the packages with a person.

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/bdubs00/featherweight-kernel/ax"
	"github.com/bdubs00/featherweight-kernel/env/pkglist"
	"github.com/bdubs00/featherweight-kernel/fw"
	v1alpha1 "github.com/google/ax/pkg/apis/v1alpha1"
)

// packagesKey is the key of the package mirror in the block workbench.
const packagesKey = "packages"

// The names of the tool env of the kernel and of its two field rules
// (the kernel architecture, section 18.7). The workbench does not link the
// package env of the kernel, because its init gives a process the tool
// and the service too, so it names them here. A test compares them
// with the kernel.
const EnvTool = "env"

// EnvAskRules are the policy rules that name an add and a remove of a
// package. The render puts them in fw.policy.ask, or in fw.policy.deny
// when the agent cannot ask.
var EnvAskRules = []string{"env: command=add", "env: command=remove"}

// The two lists of a pool, with the keys of the kernel.
const (
	ListPython = pkglist.Python
	ListTools  = pkglist.Tools
)

// PackageMirror is the block workbench.packages: the mirror of each
// manager, which the workers reach instead of the public registries, and
// the egress hosts that reach the mirror. A pool with packages gets both.
type PackageMirror struct {
	Mirrors PackageMirrors `yaml:"mirrors" json:"mirrors"`
	Egress  []string       `yaml:"egress,omitempty" json:"egress,omitempty"`
}

// PackageMirrors names the index of each manager: Python for uv and pip,
// Tools for pixi in place of https://conda.anaconda.org/conda-forge.
type PackageMirrors struct {
	Python string `yaml:"python" json:"python"`
	Tools  string `yaml:"tools" json:"tools"`
}

// LoadMirror reads workbench.packages strictly. It returns nil and no
// error when the block names none, so the feature is off. Both mirrors
// must be http or https URLs without a user or a password, and each
// egress host must be a host of the egress of a Task.
func LoadMirror(cfg *fw.Config) (*PackageMirror, error) {
	var m *PackageMirror
	if err := decodeKey(cfg, packagesKey, &m, "workbench.packages holds mirrors (python and tools) and egress"); err != nil {
		return nil, err
	}
	if m == nil {
		return nil, nil
	}
	var problems []string
	for _, x := range []struct{ key, raw string }{{"python", m.Mirrors.Python}, {"tools", m.Mirrors.Tools}} {
		u := strings.TrimSpace(x.raw)
		if u == "" {
			problems = append(problems, fmt.Sprintf("set workbench.packages.mirrors.%s, the package index that the workers use", x.key))
			continue
		}
		if err := pkglist.CheckMirror(u); err != nil {
			problems = append(problems, fmt.Sprintf("workbench.packages.mirrors.%s %v", x.key, err))
		}
	}
	m.Mirrors.Python, m.Mirrors.Tools = strings.TrimSpace(m.Mirrors.Python), strings.TrimSpace(m.Mirrors.Tools)
	for i, h := range m.Egress {
		m.Egress[i] = strings.TrimSpace(h)
		if _, err := v1alpha1.ParseEgressHost(m.Egress[i]); err != nil {
			problems = append(problems, fmt.Sprintf("workbench.packages.egress[%d]: %v", i, err))
		}
	}
	if len(problems) > 0 {
		return nil, fmt.Errorf("pools: %s", strings.Join(problems, "; "))
	}
	return m, nil
}

// Packages is the key packages of a pool or a template in the
// configuration file. On turns the packages on for the workers. Ask lets
// the agent ask for a package with the tool env, and a person approves
// each add and each remove. Without it, the tool only lists. Python and
// Tools name the packages, such as ansible-lint or nodejs>=22.
type Packages struct {
	On     bool     `yaml:"on" json:"on"`
	Ask    bool     `yaml:"ask" json:"ask"`
	Python []string `yaml:"python,omitempty" json:"python,omitempty"`
	Tools  []string `yaml:"tools,omitempty" json:"tools,omitempty"`
}

// PackageItem is one package of a list. A package of the overlay names
// the subject that added it (By) and the time (At), and for an approval
// of the tool env, the agent and the run that asked.
type PackageItem struct {
	Name  string     `json:"name"`
	By    string     `json:"by,omitempty"`
	At    *time.Time `json:"at,omitempty"`
	Agent string     `json:"agent,omitempty"`
	Run   string     `json:"run,omitempty"`
}

// PackageSetting is the packages of a pool: the key packages of its
// configuration, or the entry of its overlay, which wins. By and At say
// who wrote the entry of the overlay last, and when. Applied holds the
// ids of the last events fw.env.changed that the front wrote into the
// entry, so a second replica of the front, or a second delivery, writes
// an approval once.
type PackageSetting struct {
	On      bool          `json:"on"`
	Ask     bool          `json:"ask"`
	Python  []PackageItem `json:"python"`
	Tools   []PackageItem `json:"tools"`
	By      string        `json:"by,omitempty"`
	At      *time.Time    `json:"at,omitempty"`
	Applied []string      `json:"applied,omitempty"`
}

// MaxApplied bounds Applied of a setting.
const MaxApplied = 64

// Clone returns a deep copy of the setting.
func (s PackageSetting) Clone() PackageSetting {
	s.Python, s.Tools, s.Applied = slices.Clone(s.Python), slices.Clone(s.Tools), slices.Clone(s.Applied)
	if s.At != nil {
		at := *s.At
		s.At = &at
	}
	return s
}

// List returns the list of the manager, ListPython or ListTools.
func (s *PackageSetting) List(manager string) []PackageItem {
	if manager == ListTools {
		return s.Tools
	}
	return s.Python
}

// setList replaces the list of the manager.
func (s *PackageSetting) setList(manager string, list []PackageItem) {
	if manager == ListTools {
		s.Tools = list
		return
	}
	s.Python = list
}

// Names returns the names of a list.
func Names(list []PackageItem) []string {
	out := make([]string, 0, len(list))
	for _, it := range list {
		out = append(out, it.Name)
	}
	return out
}

// Same reports whether two settings give the workers the same packages:
// the same switches and the same names in the same order.
func (s PackageSetting) Same(o PackageSetting) bool {
	return s.On == o.On && s.Ask == o.Ask && slices.Equal(Names(s.Python), Names(o.Python)) && slices.Equal(Names(s.Tools), Names(o.Tools))
}

// CleanPackages checks a list of packages for a person: each name is
// trimmed, a name that the list holds twice counts once, and the kernel
// refuses a name that is empty, starts with a dash, holds a control
// character, or holds more than 200 characters, and a list of more than
// 100 names. key names the list in an error. The error names the bad
// item.
func CleanPackages(key string, list []string) ([]string, error) {
	out := make([]string, 0, len(list))
	for i, raw := range list {
		name := strings.TrimSpace(raw)
		if err := pkglist.CheckRequirement(name); err != nil {
			return nil, fmt.Errorf("%s[%d] %v", key, i, err)
		}
		if !slices.Contains(out, name) {
			out = append(out, name)
		}
	}
	if len(out) > pkglist.MaxItems {
		return nil, fmt.Errorf("%s holds %d packages; the limit is %d", key, len(out), pkglist.MaxItems)
	}
	return out, nil
}

// PackageName returns the name of the package of a requirement of the
// manager, in the form that compares two requirements: "PyYAML>=6" and
// "pyyaml" name one Python package.
func PackageName(manager, req string) string {
	return pkglist.Name(manager, req)
}

// itemsOf returns the items of a list of names, without an author.
func itemsOf(names []string) []PackageItem {
	out := make([]PackageItem, 0, len(names))
	for _, n := range names {
		out = append(out, PackageItem{Name: n})
	}
	return out
}

// checkPackages checks the key packages of a pool or a template and
// trims its names. A pool that turns the packages on needs
// workbench.packages.
func (p *Pool) checkPackages(front Front) []string {
	if p.Packages == nil {
		return nil
	}
	var problems []string
	c := *p.Packages
	for _, l := range []struct {
		key  string
		list *[]string
	}{{"packages.python", &c.Python}, {"packages.tools", &c.Tools}} {
		clean, err := CleanPackages(l.key, *l.list)
		if err != nil {
			problems = append(problems, p.key()+": "+err.Error())
			continue
		}
		*l.list = clean
	}
	if c.On && front.Packages == nil {
		problems = append(problems, p.key()+": packages.on needs workbench.packages, the package mirror of the front; add it, or remove packages from the pool")
	}
	p.Packages = &c
	return problems
}

// ConfigPackages returns the packages of the configuration of the pool:
// the key packages of the pool, else the one of its template, else the
// packages off.
func (p *Pool) ConfigPackages() PackageSetting {
	if p.Packages == nil {
		return PackageSetting{Python: []PackageItem{}, Tools: []PackageItem{}}
	}
	return PackageSetting{On: p.Packages.On, Ask: p.Packages.Ask, Python: itemsOf(p.Packages.Python), Tools: itemsOf(p.Packages.Tools)}
}

// PackageSetting returns the packages that the workers of the pool get:
// the entry of the overlay when the overlay holds one, else the packages
// of the configuration.
func (p *Pool) PackageSetting() PackageSetting {
	if o := p.overlay.Packages; o != nil {
		s := o.Clone()
		if s.Python == nil {
			s.Python = []PackageItem{}
		}
		if s.Tools == nil {
			s.Tools = []PackageItem{}
		}
		return s
	}
	return p.ConfigPackages()
}

// PackagesOn reports whether the workers of the pool get packages: the
// front has a package mirror, and the setting of the pool is on.
func (p *Pool) PackagesOn() bool {
	return p.front.Packages != nil && p.PackageSetting().On
}

// taskEgress returns spec.egress of each Task: the egress of the pool,
// and on a front with a package mirror the hosts of the mirror that it
// does not hold yet. Every pool of such a front reaches the mirror, so a
// change of the setting changes no Task.
func (p *Pool) taskEgress() *ax.EgressSpec {
	if p.front.Packages == nil || len(p.front.Packages.Egress) == 0 {
		return p.Egress
	}
	out := &ax.EgressSpec{}
	if p.Egress != nil {
		out.Hosts = slices.Clone(p.Egress.Hosts)
	}
	for _, h := range p.front.Packages.Egress {
		if !slices.Contains(out.Hosts, h) {
			out.Hosts = append(out.Hosts, h)
		}
	}
	return out
}

// withPackages returns the block fw of the workers m with the
// infrastructure of the packages, which every pool of a front with a
// package mirror gets, whatever its setting: the block environment with
// the mirrors, the tool env in tools.builtin when the block sets that
// list, the two rules of the tool env in policy.ask when the block sets
// that list, and the tool env in each tool list of a profile that is not
// empty. With hash, the block environment holds no switch and no lists,
// because the hash of a pool leaves the setting of the packages out. Else
// it holds on, ask, and the lists of the setting, with empty lists while
// the setting is off. The
// front sends a change of the setting to the running workers
// (PUT /v1/environment), so a change replaces no worker. m does not
// change: each level that the function changes is a copy.
func withPackages(m map[string]any, s PackageSetting, mirror *PackageMirror, hash bool) map[string]any {
	out := make(map[string]any, len(m)+1)
	for k, v := range m {
		out[k] = v
	}
	names := func(list []PackageItem) []any {
		l := make([]any, 0, len(list))
		if !s.On {
			return l
		}
		for _, it := range list {
			l = append(l, escapeDollars(it.Name))
		}
		return l
	}
	env := map[string]any{
		"mirrors": map[string]any{ListPython: escapeDollars(mirror.Mirrors.Python), ListTools: escapeDollars(mirror.Mirrors.Tools)},
	}
	if !hash {
		// A new Task boots with the switch and the lists of its pool. The
		// hash leaves them out, so a change of the setting keeps the Task.
		env["on"], env["ask"] = s.On, s.Ask
		env["packages"] = map[string]any{ListPython: names(s.Python), ListTools: names(s.Tools)}
	}
	out["environment"] = env
	if tools, ok := m["tools"].(map[string]any); ok {
		if _, set := tools["builtin"]; set {
			t := copyMap(tools)
			t["builtin"] = addStrings(tools["builtin"], EnvTool)
			out["tools"] = t
		}
	}
	if policy, ok := m["policy"].(map[string]any); ok {
		if _, set := policy["ask"]; set {
			pol := copyMap(policy)
			pol["ask"] = addStrings(policy["ask"], EnvAskRules...)
			out["policy"] = pol
		}
	}
	if agents, ok := m["agents"].(map[string]any); ok {
		a := copyMap(agents)
		for name, v := range agents {
			prof, ok := v.(map[string]any)
			if !ok {
				continue
			}
			list := stringList(prof["tools"])
			if len(list) == 0 || slices.Contains(list, EnvTool) || slices.Contains(list, "*") {
				continue
			}
			c := copyMap(prof)
			c["tools"] = addStrings(prof["tools"], EnvTool)
			a[name] = c
		}
		out["agents"] = a
	}
	return out
}

// copyMap returns a shallow copy of m, and an empty map for nil.
func copyMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m)+1)
	for k, v := range m {
		out[k] = v
	}
	return out
}

// addStrings returns the list of strings v with each of add that it does
// not hold, as a new list.
func addStrings(v any, add ...string) []string {
	list := stringList(v)
	out := make([]string, 0, len(list)+len(add))
	for _, s := range list {
		out = append(out, strings.TrimSpace(s))
	}
	for _, s := range add {
		if !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	return out
}

// ChangePackages applies an add or a remove of the tool env of a worker
// to a setting, for the overlay of the pool. An add of a package whose
// name the list holds already replaces that item, so "pyyaml>=7" takes
// the place of "pyyaml>=6". A remove takes away each item of that name.
// It reports whether the setting changed.
func ChangePackages(s *PackageSetting, manager, command string, item PackageItem) bool {
	if manager != ListPython && manager != ListTools {
		return false
	}
	key := PackageName(manager, item.Name)
	if key == "" {
		return false
	}
	list := s.List(manager)
	var next []PackageItem
	switch command {
	case "add":
		for _, it := range list {
			if it.Name == item.Name {
				return false
			}
		}
		done := false
		for _, it := range list {
			if PackageName(manager, it.Name) == key {
				if !done {
					next = append(next, item)
					done = true
				}
				continue
			}
			next = append(next, it)
		}
		if !done {
			next = append(next, item)
		}
	case "remove":
		for _, it := range list {
			if PackageName(manager, it.Name) != key {
				next = append(next, it)
			}
		}
		if len(next) == len(list) {
			return false
		}
	default:
		return false
	}
	if next == nil {
		next = []PackageItem{}
	}
	s.setList(manager, next)
	return true
}

// NoteApplied records the id of an event in Applied, and reports false
// when the setting holds it already. An event without an id is always
// new.
func (s *PackageSetting) NoteApplied(id string) bool {
	if id == "" {
		return true
	}
	if slices.Contains(s.Applied, id) {
		return false
	}
	s.Applied = append(s.Applied, id)
	if n := len(s.Applied); n > MaxApplied {
		s.Applied = slices.Clone(s.Applied[n-MaxApplied:])
	}
	return true
}
