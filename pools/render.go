package pools

// This file renders the Workspace and the Task of one worker of a pool, as
// the front applies them through the ax API and as the dry run prints
// them. A variable that names a variable of the front takes its value from
// a Lookup, so the dry run prints a placeholder where the front writes a
// secret.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/bdubs00/featherweight-kernel/ax"
	v1alpha1 "github.com/google/ax/pkg/apis/v1alpha1"
	"gopkg.in/yaml.v3"
)

// Lookup returns the value of a variable of the front.
type Lookup func(name string) (string, bool)

// Placeholders is the Lookup of the dry run: it gives each variable of the
// front a placeholder that names it, never its value.
func Placeholders(name string) (string, bool) {
	return "<secret: " + name + " of the server>", true
}

// Values holds the values of the variables of the front that the pools
// copy into their Tasks. The front reads them once at its start and keeps
// them in memory only.
type Values struct {
	m     map[string]string
	order []string // the forms of the values to redact, the longest first
}

// redactMin is the shortest value that Redact replaces. A shorter value is
// no secret, and replacing it would garble any text.
const redactMin = 8

// Lookup returns the value of a variable of the front (a Lookup).
func (v *Values) Lookup(name string) (string, bool) {
	if v == nil {
		return "", false
	}
	s, ok := v.m[name]
	return s, ok
}

// Redact replaces every value in s with a placeholder, so an error of the
// ax API that echoes a Task never carries a secret into a log line or a
// view. It leaves a value shorter than redactMin as it is.
func (v *Values) Redact(s string) string {
	if v == nil {
		return s
	}
	for _, val := range v.order {
		s = strings.ReplaceAll(s, val, "<secret>")
	}
	return s
}

// what names the pool for a person: the pool, or the template that a probe
// checks.
func (p *Pool) what() string {
	if p.probe {
		return "the template " + p.Template
	}
	return "the pool " + p.Name
}

// Resolve reads every variable of the front that the pools copy into their
// Tasks, once, with getenv, os.LookupEnv by default. list holds the probes
// of the templates too (Probes), because a runtime pool copies the
// variables of its template. A variable that is
// unset or empty is an error that names it and the pools that need it.
func Resolve(list []*Pool, getenv func(string) (string, bool)) (*Values, error) {
	if getenv == nil {
		getenv = os.LookupEnv
	}
	v := &Values{m: map[string]string{}}
	var missing []string
	for _, name := range FromEnv(list) {
		val, ok := getenv(name)
		if !ok || strings.TrimSpace(val) == "" {
			var users []string
			for _, p := range list {
				for _, e := range p.env {
					if e.FromEnv == name {
						users = append(users, p.what())
						break
					}
				}
			}
			missing = append(missing, fmt.Sprintf("%s (for %s)", name, strings.Join(users, ", ")))
			continue
		}
		v.m[name] = val
		if len(val) >= redactMin {
			// An error can quote the value, as Go or as JSON does.
			q, _ := json.Marshal(val)
			for _, form := range []string{val, strings.Trim(strconv.Quote(val), `"`), strings.Trim(string(q), `"`)} {
				if !slices.Contains(v.order, form) {
					v.order = append(v.order, form)
				}
			}
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("pools: the server has no value for the variable %s; set it in the environment of the server", strings.Join(missing, ", "))
	}
	// The front can hide only a value of redactMin characters or more, so
	// a shorter static token stops the start.
	for _, p := range list {
		for _, e := range p.env {
			if e.Name == p.tokenEnv && e.FromEnv != "" && len(v.m[e.FromEnv]) < redactMin {
				return nil, fmt.Errorf("pools: the static token in %s has fewer than %d characters; the server copies it into every Task, so use a longer token", e.FromEnv, redactMin)
			}
		}
	}
	sort.Slice(v.order, func(i, j int) bool { return len(v.order[i]) > len(v.order[j]) })
	return v, nil
}

// computeHash is the hash of what every Task of the pool shares: the image,
// the variables with the name of each variable of the front instead of its
// value, the egress, the resources, the files with the rendered fw.yaml,
// the revision, and the generation. The fw.yaml of the hash leaves out
// the lists of the packages, so a change of the packages replaces no Task
// (packages.go). The dry run therefore computes the
// same hash as the front, and the hash never depends on a secret. A
// generation of 0 leaves the hash as it was before pools had one.
func (p *Pool) computeHash() string {
	type envEntry struct {
		Name    string  `json:"name"`
		Value   *string `json:"value,omitempty"`
		FromEnv string  `json:"fromEnv,omitempty"`
	}
	in := struct {
		Image      string           `json:"image"`
		Revision   string           `json:"revision,omitempty"`
		Generation int              `json:"generation,omitempty"`
		Env        []envEntry       `json:"env"`
		Egress     *ax.EgressSpec   `json:"egress,omitempty"`
		Resources  ax.ResourcesSpec `json:"resources"`
		Files      []ax.File        `json:"files"`
	}{Image: p.Image, Revision: p.Revision, Generation: p.Generation, Egress: p.taskEgress(), Resources: p.Resources, Files: slices.Clone(p.files)}
	if len(in.Files) > 0 && p.hashFile != "" {
		in.Files[0].Content = p.hashFile
	}
	for _, e := range p.env {
		in.Env = append(in.Env, envEntry{Name: e.Name, Value: e.Value, FromEnv: e.FromEnv})
	}
	b, _ := json.Marshal(in)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}

// Render returns the Task of the pool at index and its Workspace. The
// variables of the front take their values from lookup; a missing one is
// an error.
func (p *Pool) Render(index int, lookup Lookup) (*v1alpha1.Task, *v1alpha1.Workspace, error) {
	name, wsName := p.TaskName(index), p.WorkspaceName(index)
	spec := &v1alpha1.TaskSpec{
		Image:      p.Image,
		Workspaces: []*v1alpha1.WorkspaceRef{{Name: wsName, Path: WorkspacePath}},
		Egress:     egressOf(p.taskEgress()),
	}
	for _, e := range p.env {
		v := ""
		switch {
		case e.FromEnv != "":
			got, ok := lookup(e.FromEnv)
			if !ok {
				return nil, nil, fmt.Errorf("pools: %s needs the variable %s of the server, which is not set", p.key(), e.FromEnv)
			}
			v = got
		case e.Value != nil:
			v = *e.Value
		}
		spec.Env = append(spec.Env, &v1alpha1.EnvVar{Name: e.Name, Value: v})
	}
	spec.Env = append(spec.Env, &v1alpha1.EnvVar{Name: EnvSpec, Value: p.hash})
	if p.Generation > 0 {
		spec.Env = append(spec.Env, &v1alpha1.EnvVar{Name: EnvGeneration, Value: strconv.Itoa(p.Generation)})
		spec.Env = append(spec.Env, &v1alpha1.EnvVar{Name: EnvBase, Value: p.baseHash()})
	}
	if r := p.Resources; r.Requests != (ax.ResourceList{}) || r.Limits != (ax.ResourceList{}) {
		spec.Resources = &v1alpha1.ResourcesSpec{Requests: resourceList(r.Requests), Limits: resourceList(r.Limits)}
	}
	task := &v1alpha1.Task{
		ApiVersion: ax.APIVersion,
		Kind:       ax.KindTask,
		Metadata:   &v1alpha1.ObjectMeta{Name: name, Atespace: p.Atespace},
		Spec:       spec,
	}
	ws := &v1alpha1.Workspace{
		ApiVersion: ax.APIVersion,
		Kind:       ax.KindWorkspace,
		Metadata:   &v1alpha1.ObjectMeta{Name: wsName, Atespace: p.Atespace},
		Spec:       &v1alpha1.WorkspaceSpec{},
	}
	for _, f := range p.files {
		ws.Spec.Files = append(ws.Spec.Files, &v1alpha1.File{Path: f.Path, Content: f.Content})
	}
	return task, ws, nil
}

// markerFile is the file of the Workspace that records the generation of a
// pool.
const markerFile = ".fw/generation"

// MarkerName returns the name of the Workspace that records the newest
// generation of the pool that a front runs, <name>-generation.
func (p *Pool) MarkerName() string { return p.Name + "-generation" }

// markerHeader is the first line of the file of the marker. It names the
// pool, so the front writes only a marker of its own pool. markerOf reads
// it to know a marker of the pool, so its words never change: a marker
// with other words reads as the marker of no pool, and the pool holds.
func markerHeader(pool string) string {
	return "# This file is written by the pool " + pool + " of fw-workbench. It holds the newest generation of the pool that a front runs.\n"
}

// Marker returns the Workspace that records the generation of the pool.
// The ax API keeps no labels, so a Workspace carries the generation to the
// other replicas of the front before any Task of it exists.
func (p *Pool) Marker() *v1alpha1.Workspace {
	return &v1alpha1.Workspace{
		ApiVersion: ax.APIVersion,
		Kind:       ax.KindWorkspace,
		Metadata:   &v1alpha1.ObjectMeta{Name: p.MarkerName(), Atespace: p.Atespace},
		Spec: &v1alpha1.WorkspaceSpec{Files: []*v1alpha1.File{
			{Path: markerFile, Content: markerHeader(p.Name) + strconv.Itoa(p.Generation) + "\n"},
		}},
	}
}

// markerOf returns the generation that a marker records, and false for a
// Workspace that the pool did not write. A value that is not a number of
// 0 or more reads as 0.
func (p *Pool) markerOf(ws *v1alpha1.Workspace) (int, bool) {
	for _, f := range ws.GetSpec().GetFiles() {
		if strings.TrimPrefix(f.GetPath(), "./") != markerFile {
			continue
		}
		rest, ok := strings.CutPrefix(f.GetContent(), markerHeader(p.Name))
		if !ok {
			return 0, false
		}
		n, err := strconv.Atoi(strings.TrimSpace(rest))
		if err != nil || n < 0 {
			return 0, true
		}
		return n, true
	}
	return 0, false
}

// resourceList converts a quantity pair, and returns nil for an empty one.
func resourceList(l ax.ResourceList) *v1alpha1.ResourceList {
	if l == (ax.ResourceList{}) {
		return nil
	}
	return &v1alpha1.ResourceList{Cpu: l.CPU, Memory: l.Memory}
}

// ownsWorkspace reports whether a Workspace is one that the pool rendered:
// its fw.yaml starts with the header of the pool.
func (p *Pool) ownsWorkspace(ws *v1alpha1.Workspace) bool {
	for _, f := range ws.GetSpec().GetFiles() {
		if strings.TrimPrefix(f.GetPath(), "./") == ConfigFile {
			return strings.HasPrefix(f.GetContent(), header(p.Name))
		}
	}
	return false
}

// egressOf converts the egress of a pool to the API type.
func egressOf(e *ax.EgressSpec) *v1alpha1.EgressSpec {
	if e == nil {
		return nil
	}
	return &v1alpha1.EgressSpec{Hosts: append([]string(nil), e.Hosts...)}
}

// Write prints the Workspaces and the Tasks of the pools as one stream of
// YAML documents, in the order in which the front applies them, with a
// placeholder for every variable of the front. A person reviews a change
// with it. The comment before each pool names its queue, its hash, and
// its warnings, and it marks a runtime pool and its template.
func Write(w io.Writer, list []*Pool) error {
	if len(list) == 0 {
		_, err := fmt.Fprintln(w, "# The configuration names no pool under workbench.pools.")
		return err
	}
	var out strings.Builder
	docs := 0
	for _, p := range list {
		var b strings.Builder
		kind := "Pool"
		if p.runtime != nil {
			kind = "Runtime pool"
		}
		fmt.Fprintf(&b, "%s %s: %d %s of the agent %s in the atespace %s, on the queue %s.\n", kind, p.Name, p.replicas(), plural(p.replicas(), "Task", "Tasks"), p.Agent, p.Atespace, p.Intake)
		if p.Template != "" {
			fmt.Fprintf(&b, "The pool takes each key that it does not set from the template %s of workbench.templates.\n", p.Template)
		}
		if r := p.runtime; r != nil {
			fmt.Fprintf(&b, "The store of the server holds this pool, not the configuration file. The entry is generation %d%s.\n", r.Generation, written(r.By, r.At, ""))
			if r.Deleting {
				b.WriteString("A person deleted the pool, so the server deletes each Task of it once the Task is idle, and then the entry.\n")
			} else if _, ok := p.overlay.Profile(p.Agent); !ok {
				fmt.Fprintf(&b, "The overlay holds no profile %s yet, so the server creates no Task of the pool. The documents below hold only the place of the profile.\n", p.Agent)
			}
		}
		if p.Generation > 0 {
			fmt.Fprintf(&b, "Each Task carries %s=%s, %s=%s, and %s=%d. A Task of a lower generation runs an older configuration, and the server leaves a Task of a higher generation alone.\n", EnvPool, p.Name, EnvSpec, p.hash, EnvGeneration, p.Generation)
			fmt.Fprintf(&b, "Before it changes a Task, the server records generation %d in the Workspace %s.\n", p.Generation, p.MarkerName())
		} else {
			fmt.Fprintf(&b, "Each Task carries %s=%s and %s=%s. A Task with another %s runs an older configuration.\n", EnvPool, p.Name, EnvSpec, p.hash, EnvSpec)
		}
		for _, name := range FromEnv([]*Pool{p}) {
			fmt.Fprintf(&b, "The server copies the variable %s from its own environment; this output shows a placeholder.\n", name)
		}
		b.WriteString("This output is for review. Do not apply it: a placeholder is not a value, and the server replaces a Task that holds one.\n")
		for _, warn := range p.warnings {
			fmt.Fprintf(&b, "Warning: %s\n", warn)
		}
		for _, e := range p.overlay.Profiles {
			fmt.Fprintf(&b, "The overlay of the server sets the profile %s in fw.agents%s.\n", e.Profile.Name, written(e.By, e.At, ""))
		}
		for _, r := range p.overlay.Allow {
			fmt.Fprintf(&b, "The overlay of the server adds the rule %q to fw.policy.allow%s.\n", r.Rule, written(r.By, r.At, r.Run))
		}
		if p.routinesFront {
			b.WriteString("The workers read the routines of the server at the start of each run (routines.front), so a move of a routine replaces no Task.\n")
		}
		for _, name := range slices.Sorted(maps.Keys(p.overlay.Routines)) {
			if _, runs := p.Profiles()[name]; !runs || p.routinesFront {
				continue
			}
			var words []string
			for _, r := range p.overlay.Routines[name] {
				words = append(words, fmt.Sprintf("%s version %d (%s)", r.Name, r.Version, r.Standing))
			}
			if len(words) == 0 {
				fmt.Fprintf(&b, "The routines of the server give the profile %s no routine.\n", name)
				continue
			}
			fmt.Fprintf(&b, "The routines of the server give the profile %s the routines %s.\n", name, strings.Join(words, ", "))
		}
		if s := p.PackageSetting(); p.PackagesOn() {
			where := "The configuration of the pool turns the packages on"
			if p.overlay.Packages != nil {
				at := time.Time{}
				if s.At != nil {
					at = *s.At
				}
				where = "The overlay of the server turns the packages on" + written(s.By, at, "")
			}
			ask := "the agent cannot ask for a change"
			if s.Ask {
				ask = "the agent can ask for a change, and a person approves each one"
			}
			fmt.Fprintf(&b, "%s: Python packages %s, Programs %s, from the package mirror of the server; %s. The server sends a change of the packages to the running workers and replaces no Task for it.\n", where, orWord(strings.Join(Names(s.Python), ", "), "none"), orWord(strings.Join(Names(s.Tools), ", "), "none"), ask)
		}
		if len(p.skillNames) > 0 {
			fmt.Fprintf(&b, "The server ships its skills %s, which the profiles of the pool allow, to %s/%s of each worker.\n", strings.Join(p.skillNames, ", "), WorkspacePath, SkillsPath)
		}
		if len(p.skillSkipped) > 0 {
			fmt.Fprintf(&b, "The server leaves out these files of its skills, because they are hidden, links, or not text: %s.\n", strings.Join(p.skillSkipped, ", "))
		}
		if p.replicas() == 0 {
			b.WriteString("The pool wants no Task, so the server deletes each Task of the pool once it is idle.\n")
		}
		if docs > 0 {
			out.WriteString("---\n")
		}
		out.WriteString(comment(b.String()))
		if p.Generation > 0 {
			text, err := encode(p.Marker())
			if err != nil {
				return fmt.Errorf("pools: encode the pool %s: %w", p.Name, err)
			}
			out.WriteString(text)
			if p.replicas() > 0 {
				out.WriteString("---\n")
			}
		}
		for i := 0; i < p.replicas(); i++ {
			task, ws, err := p.Render(i, Placeholders)
			if err != nil {
				return err
			}
			for j, doc := range []any{ws, task} {
				text, err := encode(doc)
				if err != nil {
					return fmt.Errorf("pools: encode the pool %s: %w", p.Name, err)
				}
				if i > 0 || j > 0 {
					out.WriteString("---\n")
				}
				out.WriteString(text)
			}
		}
		docs++
	}
	_, err := io.WriteString(w, out.String())
	return err
}

// written says who wrote an entry of the overlay, when, and for which run.
func written(by string, at time.Time, run string) string {
	var out string
	if by != "" {
		out += " by " + by
	}
	if !at.IsZero() {
		out += " at " + at.UTC().Format(time.RFC3339)
	}
	if run != "" {
		out += " for the run " + run
	}
	return out
}

// encode renders one resource of the ax API as YAML with an indent of two.
func encode(doc any) (string, error) {
	var b strings.Builder
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	if err := enc.Encode(doc); err != nil {
		return "", err
	}
	if err := enc.Close(); err != nil {
		return "", err
	}
	return b.String(), nil
}

// comment turns lines of text into YAML comment lines.
func comment(text string) string {
	var b strings.Builder
	for _, line := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
		b.WriteString("# " + line + "\n")
	}
	return b.String()
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
