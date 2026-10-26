package pools

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"gopkg.in/yaml.v3"
)

const (
	testMirrorPython = "http://pkg-cache.fw-fleet.svc.cluster.local:8080/pypi/simple"
	testMirrorTools  = "http://pkg-cache.fw-fleet.svc.cluster.local:8080/conda-forge"
	testMirrorHost   = "http://pkg-cache.fw-fleet.svc.cluster.local:8080"
)

// packagesFront is the front of the fixture with a package mirror.
func packagesFront() Front {
	f := testFront()
	f.Packages = &PackageMirror{Mirrors: PackageMirrors{Python: testMirrorPython, Tools: testMirrorTools}, Egress: []string{testMirrorHost}}
	return f
}

// withPackagesKey returns the fixture with the key packages in its pool.
func withPackagesKey(text, packages string) string {
	return strings.Replace(text, "    fw:\n", "    packages: "+packages+"\n    fw:\n", 1)
}

// loadPackages loads text with the front that has a package mirror.
func loadPackages(t *testing.T, text string) *Pool {
	t.Helper()
	list, err := Load(configOf(t, text), packagesFront())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return list[0]
}

// fileMap decodes the fw.yaml that a pool renders as a map.
func fileMap(t *testing.T, p *Pool) map[string]any {
	t.Helper()
	var m map[string]any
	if err := yaml.Unmarshal([]byte(p.files[0].Content), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// valueAt reads a value of a decoded map by its keys.
func valueAt(m map[string]any, keys ...string) any {
	var v any = m
	for _, k := range keys {
		mm, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = mm[k]
	}
	return v
}

// LoadMirror reads workbench.packages strictly and checks both mirrors
// and the egress hosts. Without the block the feature is off.
func TestLoadMirror(t *testing.T) {
	if m, err := LoadMirror(configOf(t, "pools: []\n")); m != nil || err != nil {
		t.Fatalf("no block = %+v, %v", m, err)
	}
	m, err := LoadMirror(configOf(t, "packages:\n  mirrors: {python: \" "+testMirrorPython+" \", tools: "+testMirrorTools+"}\n  egress: [\""+testMirrorHost+"\"]\n"))
	if err != nil || m.Mirrors.Python != testMirrorPython || m.Mirrors.Tools != testMirrorTools || !slices.Equal(m.Egress, []string{testMirrorHost}) {
		t.Fatalf("LoadMirror = %+v, %v", m, err)
	}
	for _, x := range []struct{ text, want string }{
		{"packages:\n  mirrors: {python: " + testMirrorPython + "}\n", "set workbench.packages.mirrors.tools"},
		{"packages:\n  mirrors: {python: \"http://user:secret@cache:8080/simple\", tools: " + testMirrorTools + "}\n", "mirrors.python holds a user or a password"},
		{"packages:\n  mirrors: {python: ftp://cache/simple, tools: " + testMirrorTools + "}\n", "is not an http or an https URL"},
		{"packages:\n  mirrors: {python: " + testMirrorPython + ", tools: " + testMirrorTools + "}\n  egress: [\"not a host\"]\n", "workbench.packages.egress[0]"},
		{"packages:\n  mirrors: {python: " + testMirrorPython + ", tools: " + testMirrorTools + "}\n  registry: pypi\n", "field registry not found"},
	} {
		if _, err := LoadMirror(configOf(t, x.text)); err == nil || !strings.Contains(err.Error(), x.want) {
			t.Errorf("LoadMirror(%q) = %v, want %q", x.text, err, x.want)
		}
		if err != nil && strings.Contains(err.Error(), "secret") {
			t.Errorf("the error repeats the password: %v", err)
		}
	}
}

// A pool with packages needs the mirror of the front, and the check of a
// list trims each name, counts a name once, and refuses a name that the
// kernel refuses.
func TestPackagesChecks(t *testing.T) {
	text := withPackagesKey(fixture, `{on: true, python: [" ansible-lint ", ansible-lint, "pyyaml>=6"], tools: [iproute2]}`)
	if _, err := Load(configOf(t, text), testFront()); err == nil || !strings.Contains(err.Error(), "packages.on needs workbench.packages") {
		t.Fatalf("Load without a mirror = %v", err)
	}
	p := loadPackages(t, text)
	if !slices.Equal(p.Packages.Python, []string{"ansible-lint", "pyyaml>=6"}) || !slices.Equal(p.Packages.Tools, []string{"iproute2"}) {
		t.Fatalf("packages = %+v", p.Packages)
	}
	// A pool that leaves the packages off loads without a mirror.
	if _, err := Load(configOf(t, withPackagesKey(fixture, "{on: false, python: [x]}")), testFront()); err != nil {
		t.Fatalf("a pool with the packages off = %v", err)
	}
	long := strings.Repeat("a", 201)
	many := make([]string, 101)
	for i := range many {
		many[i] = "p" + strings.Repeat("x", i%7) + string(rune('a'+i%26)) + strings.Repeat("y", i/26)
	}
	for _, x := range []struct {
		list []string
		want string
	}{
		{[]string{"--index-url=http://evil"}, "python[0] starts with a dash"},
		{[]string{"ok", "bad\nname"}, "python[1] holds a control character"},
		{[]string{long}, "python[0] is longer than 200 characters"},
		{[]string{"  "}, "python[0] is empty"},
		{[]string{"six", "bad name;rm"}, "python[1] holds a semicolon"},
		{[]string{"requests @ https://example.org/r.whl"}, "python[0] names a URL"},
		{many, "python holds 101 packages; the limit is 100"},
	} {
		if _, err := CleanPackages(ListPython, "python", x.list); err == nil || !strings.Contains(err.Error(), x.want) {
			t.Errorf("CleanPackages(%q) = %v, want %q", x.list, err, x.want)
		}
	}
	if got, err := CleanPackages(ListTools, "tools", nil); err != nil || got == nil || len(got) != 0 {
		t.Fatalf("an empty list = %v, %v", got, err)
	}
	// A program is a conda name with a version, and a person reads the
	// refusal of a name as one sentence.
	if _, err := CleanPackages(ListTools, "tools", []string{"jq", "nodejs 22.*", "conda-forge::jq"}); err == nil || err.Error() != "tools[2] names a channel; a channel is not available, so name a package of conda-forge, such as jq" {
		t.Fatalf("a channel = %v", err)
	}
	_, err := CleanPackages(ListPython, "python", []string{"bad name;rm"})
	var pe *PackageError
	if !errors.As(err, &pe) || pe.Sentence() != `"bad name;rm" holds a semicolon; a condition after a semicolon is not available, so write the name and the version alone, such as pyyaml>=6.` {
		t.Fatalf("the sentence of a refusal = %v", err)
	}
}

// On a front with a package mirror, the render gives the workers the
// block environment with the lists and the mirrors, the egress of the
// mirror, the tool env in the built-in tools that the block names, the two
// rules of the tool env in the ask list that the block names, and the
// tool env in each tool list of a profile. The rules never reach the allow
// list. The setting changes only the lists, which the hash leaves out.
func TestPackagesRender(t *testing.T) {
	plain := loadFixture(t, fixture)[0]
	p := loadPackages(t, withPackagesKey(fixture, `{on: true, ask: true, python: [ansible-lint, "pyyaml>=6"], tools: [iproute2]}`))
	if p.Hash() == plain.Hash() {
		t.Fatal("the packages kept the hash")
	}
	m := fileMap(t, p)
	env, _ := m["environment"].(map[string]any)
	if got := stringList(valueAt(env, "packages", "python")); !slices.Equal(got, []string{"ansible-lint", "pyyaml>=6"}) {
		t.Fatalf("environment.packages.python = %v", got)
	}
	if got := stringList(valueAt(env, "packages", "tools")); !slices.Equal(got, []string{"iproute2"}) {
		t.Fatalf("environment.packages.tools = %v", got)
	}
	if valueAt(env, "mirrors", "python") != testMirrorPython || valueAt(env, "mirrors", "tools") != testMirrorTools || env["on"] != true || env["ask"] != true {
		t.Fatalf("environment.mirrors = %v", valueAt(env, "mirrors"))
	}
	if got := stringList(valueAt(m, "tools", "builtin")); !slices.Equal(got, []string{"files", "clock", "env"}) {
		t.Fatalf("tools.builtin = %v", got)
	}
	if got := stringList(valueAt(m, "policy", "ask")); !slices.Equal(got, []string{"tickets.create_ticket", "env: command=add", "env: command=remove"}) {
		t.Fatalf("policy.ask = %v", got)
	}
	if got := stringList(valueAt(m, "policy", "deny")); len(got) != 0 {
		t.Fatalf("policy.deny with ask = %v", got)
	}
	if got := stringList(valueAt(m, "agents", "triage", "tools")); !slices.Equal(got, []string{"files", "tickets.search", "tickets.create_ticket", "env"}) {
		t.Fatalf("the tools of the profile = %v", got)
	}
	task, _, err := p.Render(0, testLookup)
	if err != nil {
		t.Fatal(err)
	}
	if hosts := task.Spec.Egress.Hosts; len(hosts) != 4 || hosts[3] != testMirrorHost {
		t.Fatalf("egress = %v", hosts)
	}
	if hosts := p.Egress.Hosts; len(hosts) != 3 {
		t.Fatalf("the render changed the egress of the configuration: %v", hosts)
	}
	// Every call of the tool env that changes a package asks a person.
	for _, in := range []string{`{"command":"add","manager":"python","package":"x"}`, `{"command":"remove","manager":"tools","package":"y"}`} {
		if d := p.Decide("triage", EnvTool, []byte(in)); d.Verdict != fw.VerdictAsk {
			t.Fatalf("Decide(%s) = %+v", in, d)
		}
	}
	if d := p.Decide("triage", EnvTool, []byte(`{"command":"list"}`)); d.Verdict != fw.VerdictAllow {
		t.Fatalf("Decide(list) = %+v", d)
	}
	if why := p.ToolsProblem("triage", []string{"env", "files"}); why != "" {
		t.Fatalf("ToolsProblem = %q", why)
	}

	// Without ask, the render is the same: the kernel refuses an add
	// and a remove, and the rules still ask a person.
	q := loadPackages(t, withPackagesKey(fixture, `{on: true, python: [ansible-lint]}`))
	m = fileMap(t, q)
	if got := stringList(valueAt(m, "policy", "deny")); len(got) != 0 {
		t.Fatalf("policy.deny without ask = %v", got)
	}
	if got := stringList(valueAt(m, "agents", "triage", "tools")); !slices.Contains(got, "env") {
		t.Fatalf("the tools of the profile without ask = %v", got)
	}
	if d := q.Decide("triage", EnvTool, []byte(`{"command":"add","manager":"python","package":"x"}`)); d.Verdict != fw.VerdictAsk {
		t.Fatalf("Decide(add) without ask = %+v", d)
	}
	if q.Hash() != p.Hash() {
		t.Fatal("ask and the lists changed the hash")
	}

	// A block without its own lists keeps the defaults of the kernel,
	// which hold the tool and the rules.
	bare := strings.Replace(strings.Replace(fixture, "      policy: {default: allow, ask: [tickets.create_ticket]}\n", "      policy: {default: allow}\n", 1), "        builtin: [files, clock]\n", "", 1)
	r := loadPackages(t, withPackagesKey(bare, `{on: true, ask: true}`))
	m = fileMap(t, r)
	if valueAt(m, "policy", "ask") != nil || valueAt(m, "policy", "deny") != nil || valueAt(m, "tools", "builtin") != nil {
		t.Fatalf("the render set a list that the block leaves to the kernel: %v %v", valueAt(m, "policy"), valueAt(m, "tools"))
	}
	if got, ok := valueAt(m, "environment", "packages", "python").([]any); !ok || len(got) != 0 {
		t.Fatalf("an empty list = %v", valueAt(m, "environment"))
	}

	// A pool with the packages off on a front with a mirror gets the
	// infrastructure with empty lists, and the hash of a pool with them
	// on. On a front without a mirror, a pool renders as before.
	off := loadPackages(t, fixture)
	if off.Hash() != p.Hash() || off.PackagesOn() {
		t.Fatal("the setting changed the hash")
	}
	if got, ok := valueAt(fileMap(t, off), "environment", "packages", "python").([]any); !ok || len(got) != 0 {
		t.Fatalf("the lists of a pool with the packages off = %v", valueAt(fileMap(t, off), "environment"))
	}
	off2 := loadPackages(t, withPackagesKey(fixture, `{on: false, python: [ansible-lint]}`))
	if off2.Hash() != p.Hash() || strings.Contains(off2.files[0].Content, "ansible-lint") || valueAt(fileMap(t, off2), "environment", "on") != false {
		t.Fatal("a pool with the packages off gives its workers its lists")
	}
	if strings.Contains(plain.files[0].Content, "environment:") {
		t.Fatal("a server without a mirror gave the workers the block environment")
	}
}

// A name with ${ never reaches the worker: it is no package name, so the
// configuration does not load, and the worker never expands a variable of
// its own into a package name. The render still escapes each name.
func TestPackagesRenderEscapes(t *testing.T) {
	_, err := Load(configOf(t, withPackagesKey(fixture, `{on: true, python: ["x${FW_API_TOKEN}"]}`)), packagesFront())
	if err == nil || !strings.Contains(err.Error(), "packages.python[0] is not a Python package") {
		t.Fatalf("Load = %v", err)
	}
	if got := escapeDollars("x${FW_API_TOKEN}"); got != "x$${FW_API_TOKEN}" {
		t.Fatalf("escapeDollars = %q", got)
	}
}

// The overlay wins over the configuration: its lists replace the lists
// of the pool, and its switch turns the packages off. A change of the
// packages in the overlay changes the file of the workers and keeps the
// hash.
func TestPackagesOverlayWins(t *testing.T) {
	base := loadPackages(t, withPackagesKey(fixture, `{on: true, ask: true, python: [ansible-lint]}`))
	at := time.Date(2026, 10, 25, 9, 0, 0, 0, time.UTC)
	s := &PackageSetting{On: true, Ask: true, Python: []PackageItem{{Name: "pyyaml", By: "alice", At: &at}}, Tools: []PackageItem{{Name: "nodejs>=22", By: "bob", At: &at, Agent: "triage", Run: "01RUN"}}}
	merged, err := base.WithOverlay(Overlay{}.WithPackages(s))
	if err != nil {
		t.Fatal(err)
	}
	if merged.Hash() != base.Hash() || merged.files[0].Content == base.files[0].Content {
		t.Fatalf("hash %q, configuration %q", merged.Hash(), base.Hash())
	}
	m := fileMap(t, merged)
	if got := stringList(valueAt(m, "environment", "packages", "python")); !slices.Equal(got, []string{"pyyaml"}) {
		t.Fatalf("python = %v", got)
	}
	if got := merged.PackageSetting(); got.Tools[0].Run != "01RUN" || got.Python[0].By != "alice" {
		t.Fatalf("setting = %+v", got)
	}
	if got := base.PackageSetting(); !slices.Equal(Names(got.Python), []string{"ansible-lint"}) || got.Python[0].By != "" {
		t.Fatalf("the configuration = %+v", got)
	}
	off, err := base.WithOverlay(Overlay{}.WithPackages(&PackageSetting{On: false, Python: s.Python}))
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := valueAt(fileMap(t, off), "environment", "packages", "python").([]any); !ok || len(got) != 0 || off.PackagesOn() || off.Hash() != base.Hash() {
		t.Fatal("the overlay did not turn the packages off")
	}
	var b strings.Builder
	if err := Write(&b, []*Pool{merged}); err != nil {
		t.Fatal(err)
	}
	if want := "# The overlay of the server turns the packages on: Python packages pyyaml, Programs nodejs>=22, from the package mirror of the server; the agent can ask for a change, and a person approves each one."; !strings.Contains(b.String(), want) {
		t.Fatalf("the dry run lacks %q:\n%s", want, b.String())
	}
}

// A template gives its packages to a pool that sets none and to a runtime
// pool, and a pool with its own key keeps it.
func TestPackagesOfTemplates(t *testing.T) {
	text := strings.Replace(templateFixture, "    fw:\n      model:", "    packages: {on: true, ask: true, tools: [iproute2]}\n    fw:\n      model:", 1)
	list, templates, err := LoadAll(configOf(t, text), packagesFront())
	if err != nil {
		t.Fatal(err)
	}
	if got := list[0].PackageSetting(); !got.On || !slices.Equal(Names(got.Tools), []string{"iproute2"}) {
		t.Fatalf("the pool of the template = %+v", got)
	}
	rt, err := FromRuntime(runtimeEntry(), templates)
	if err != nil {
		t.Fatal(err)
	}
	if !rt.PackagesOn() {
		t.Fatal("a runtime pool did not take the packages of its template")
	}
	own := strings.Replace(text, "    template: base\n    replicas: 2\n", "    template: base\n    replicas: 2\n    packages: {on: false}\n", 1)
	list, _, err = LoadAll(configOf(t, own), packagesFront())
	if err != nil {
		t.Fatal(err)
	}
	if list[0].PackagesOn() {
		t.Fatal("the key of the pool did not win over the template")
	}
	if _, _, err := LoadAll(configOf(t, text), testFront()); err == nil || !strings.Contains(err.Error(), "workbench.templates[0] (base): packages.on needs workbench.packages") {
		t.Fatalf("a template with packages on a server without a mirror = %v", err)
	}
}

// An add of the tool env adds a package, or replaces the package of the
// same name. A remove takes away each item of that name. A change that
// the list holds already changes nothing.
func TestChangePackages(t *testing.T) {
	s := PackageSetting{Python: []PackageItem{{Name: "ansible-lint"}, {Name: "PyYAML>=6"}}, Tools: []PackageItem{}}
	add := func(manager, cmd, name string) bool {
		return ChangePackages(&s, manager, cmd, PackageItem{Name: name, Run: "r"})
	}
	if !add(ListPython, "add", "requests") || !slices.Equal(Names(s.Python), []string{"ansible-lint", "PyYAML>=6", "requests"}) {
		t.Fatalf("add = %v", Names(s.Python))
	}
	if add(ListPython, "add", "requests") {
		t.Fatal("a second add changed the list")
	}
	if !add(ListPython, "add", "pyyaml>=7") || !slices.Equal(Names(s.Python), []string{"ansible-lint", "pyyaml>=7", "requests"}) {
		t.Fatalf("add of a new version = %v", Names(s.Python))
	}
	if !add(ListPython, "remove", "Ansible_Lint") || !slices.Equal(Names(s.Python), []string{"pyyaml>=7", "requests"}) {
		t.Fatalf("remove = %v", Names(s.Python))
	}
	if add(ListPython, "remove", "ansible-lint") || add(ListTools, "remove", "nodejs") || add("apt", "add", "x") || add(ListTools, "list", "x") {
		t.Fatal("a change that does nothing changed the list")
	}
	if !add(ListTools, "add", "conda-forge::nodejs>=22") || !add(ListTools, "remove", "nodejs") || len(s.Tools) != 0 {
		t.Fatalf("tools = %v", Names(s.Tools))
	}
	if !s.NoteApplied("e1") || s.NoteApplied("e1") || !s.NoteApplied("") {
		t.Fatal("NoteApplied")
	}
	for i := range MaxApplied + 5 {
		s.NoteApplied(string(rune('a'+i%26)) + strings.Repeat("z", i))
	}
	if len(s.Applied) != MaxApplied {
		t.Fatalf("Applied holds %d ids", len(s.Applied))
	}
}

// The store in memory writes the packages of a pool beside its profiles
// and its rules, and a delete of a profile keeps them.
func TestMemOverlaysPackages(t *testing.T) {
	ctx := context.Background()
	s := NewMemOverlays()
	got, err := s.UpdatePackages(ctx, "triage", func(cur *PackageSetting) (*PackageSetting, error) {
		if cur != nil {
			t.Fatalf("a new pool has %+v", cur)
		}
		return nil, nil
	})
	if got != nil || err != nil {
		t.Fatalf("a call that writes nothing = %+v, %v", got, err)
	}
	if o, _ := s.Read(ctx, "triage"); !o.Empty() {
		t.Fatalf("Read = %+v", o)
	}
	_, err = s.UpdatePackages(ctx, "triage", func(*PackageSetting) (*PackageSetting, error) {
		return &PackageSetting{On: true, Python: []PackageItem{{Name: "x"}}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = s.PutProfile(ctx, "triage", OverlayProfile{Profile: fw.AgentProfile{Name: "helper"}})
	_, _ = s.DeleteProfile(ctx, "triage", "helper")
	o, _ := s.Read(ctx, "triage")
	if o.Packages == nil || !o.Packages.On || o.Packages.Python[0].Name != "x" {
		t.Fatalf("Read = %+v", o.Packages)
	}
	o.Packages.Python[0].Name = "changed"
	if again, _ := s.Read(ctx, "triage"); again.Packages.Python[0].Name != "x" {
		t.Fatal("a read shares the entry with the store")
	}
}

// A change of the packages keeps the hash, so the pool replaces no
// worker: the front sends the change to the running workers instead.
func TestPackagesChangeReplacesNoTask(t *testing.T) {
	base := loadPackages(t, withPackagesKey(fixture, `{on: true, ask: true, python: [ansible-lint]}`))
	first, err := base.WithOverlay(Overlay{})
	if err != nil {
		t.Fatal(err)
	}
	r := &rig{t: t, p: first, ax: newFakeAX(), check: newFakeCheck()}
	r.rec = r.reconciler()
	r.pass()
	r.ax.took()
	if st := r.pass(); st.Ready != 3 {
		t.Fatalf("status = %+v", st)
	}
	for _, next := range []*PackageSetting{
		{On: true, Ask: true, Python: []PackageItem{{Name: "ansible-lint"}, {Name: "pyyaml"}}},
		{On: true, Ask: false, Tools: []PackageItem{{Name: "iproute2"}}},
		{On: false},
	} {
		merged, err := base.WithOverlay(Overlay{}.WithPackages(next))
		if err != nil {
			t.Fatal(err)
		}
		r.p = merged
		st := r.pass()
		if calls := r.ax.took(); len(calls) != 0 || st.Ready != 3 || len(st.Gaps) != 0 {
			t.Fatalf("a change of the packages to %+v made calls %v, status %+v", next, calls, st)
		}
	}
}
