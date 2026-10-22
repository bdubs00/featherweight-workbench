package pools

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/ax"
	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/internal/bucket"
	v1alpha1 "github.com/google/ax/pkg/apis/v1alpha1"
	"gopkg.in/yaml.v3"
)

// templateFixture is a template base, the default, with a model behind a
// proxy, and a pool triage that names it and sets only what is its own.
const templateFixture = `
templates:
  - name: base
    default: true
    generation: 2
    image: ` + testImage + `
    env:
      - name: OPENAI_API_KEY
        value: the-proxy-needs-no-key
      - name: TICKETS_TOKEN
        fromEnv: FRONT_TICKETS_TOKEN
    egress:
      rules:
        - http: {hostnames: [nats.agents.svc.cluster.local], ports: [8080]}
        - http: {hostnames: [model-proxy.models.svc.cluster.local], ports: [8080]}
    resources:
      requests: {cpu: 500m, memory: 1Gi}
      limits: {cpu: "2", memory: 2Gi}
    files:
      - path: notes/house.md
        content: Be brief.
    fw:
      model: openai/some-model
      bus: {url: "ws://nats.agents.svc.cluster.local:8080"}
      providers:
        openai: {baseUrl: "http://model-proxy.models.svc.cluster.local:8080/v1", apiKeyEnv: OPENAI_API_KEY}
      access: {admins: ["kind:token"]}
      policy: {default: allow}
pools:
  - name: triage
    template: base
    replicas: 2
    generation: 1
    env:
      - name: OPENAI_API_KEY
        value: another-placeholder
    resources:
      limits: {memory: 4Gi}
    files:
      - path: notes/triage.md
        content: Sort each ticket.
    fw:
      effort: low
      agents:
        triage:
          description: Sorts new tickets.
`

// loadAll loads text with the front of the fixture.
func loadAll(t *testing.T, text string) ([]*Pool, []*Template) {
	t.Helper()
	list, templates, err := LoadAll(configOf(t, text), testFront())
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	return list, templates
}

// workerFileOf decodes the fw.yaml that a pool renders.
func workerFileOf(t *testing.T, p *Pool) *fw.Config {
	t.Helper()
	wc := fw.DefaultConfig()
	if err := yaml.Unmarshal([]byte(p.files[0].Content), wc); err != nil {
		t.Fatal(err)
	}
	return wc
}

// A template is checked at the start, and a pool that names it takes every
// key that it does not set: the image, the egress, the variables and the
// files beside its own, each resource value that it leaves out, the keys
// of fw that it does not set, and the generation of the template on top of
// its own.
func TestTemplateInheritance(t *testing.T) {
	list, templates := loadAll(t, templateFixture)
	if len(templates) != 1 || len(list) != 1 {
		t.Fatalf("templates %d, pools %d", len(templates), len(list))
	}
	info := templates[0].Info()
	if info.Name != "base" || !info.Default || info.Atespace != "team" || info.Model != "openai/some-model" || info.Generation != 2 {
		t.Fatalf("info = %+v", info)
	}
	if d := DefaultTemplate(templates); d == nil || d.Name != "base" || TemplateNames(templates) != "base (the default)" {
		t.Fatalf("default = %v, names %q", d, TemplateNames(templates))
	}
	p := list[0]
	if p.Template != "base" || p.Image != testImage || p.Atespace != "team" || p.Generation != 3 || len(p.Egress.Rules) != 2 {
		t.Fatalf("pool = template %q image %q atespace %q generation %d", p.Template, p.Image, p.Atespace, p.Generation)
	}
	values := map[string]string{}
	for _, e := range p.Env {
		if e.Value != nil {
			values[e.Name] = *e.Value
		} else {
			values[e.Name] = "from " + e.FromEnv
		}
	}
	if values["OPENAI_API_KEY"] != "another-placeholder" || values["TICKETS_TOKEN"] != "from FRONT_TICKETS_TOKEN" || len(p.Env) != 2 {
		t.Fatalf("env = %v", values)
	}
	want := ax.ResourcesSpec{Requests: ax.ResourceList{CPU: "500m", Memory: "1Gi"}, Limits: ax.ResourceList{CPU: "2", Memory: "4Gi"}}
	if p.Resources != want {
		t.Fatalf("resources = %+v", p.Resources)
	}
	var paths []string
	for _, f := range p.files {
		paths = append(paths, f.Path)
	}
	if !slices.Equal(paths, []string{ConfigFile, "notes/house.md", "notes/triage.md"}) {
		t.Fatalf("files = %v", paths)
	}
	wc := workerFileOf(t, p)
	if wc.Model != "openai/some-model" || wc.Effort != "low" || wc.Agents["triage"].Description != "Sorts new tickets." || wc.Providers["openai"].APIKeyEnv != "OPENAI_API_KEY" {
		t.Fatalf("worker = model %q effort %q agents %v", wc.Model, wc.Effort, wc.Agents)
	}
	st := Initial(p, "waits")
	if st.Source != SourceFile || st.Template != "base" || st.Generation != 3 {
		t.Fatalf("status = %+v", st)
	}
	// The front reads the variables that the templates copy at its start.
	if got := FromEnv(append(list, Probes(templates)...)); !slices.Equal(got, []string{"FRONT_TICKETS_TOKEN", "FW_API_TOKEN"}) {
		t.Fatalf("FromEnv = %v", got)
	}
	if _, err := Resolve(Probes(templates), func(string) (string, bool) { return "", false }); err == nil || !strings.Contains(err.Error(), "FRONT_TICKETS_TOKEN (for the template base)") {
		t.Fatalf("Resolve = %v", err)
	}

	// Templates alone load, and a template without a model is valid.
	alone := strings.Split(templateFixture, "pools:\n")[0]
	alone = strings.Replace(alone, "      model: openai/some-model\n", "", 1)
	if list, templates := loadAll(t, alone); len(list) != 0 || len(templates) != 1 || templates[0].Info().Model != "" {
		t.Fatalf("templates alone = %d pools, %d templates", len(list), len(templates))
	}
}

// A template that does not fit stops the start with its key and what to
// set, as a pool does, every problem at once.
func TestTemplateChecks(t *testing.T) {
	cases := []struct{ name, from, to, want string }{
		{"agents in a template", "      policy: {default: allow}\npools:", "      policy: {default: allow}\n      agents: {x: {}}\npools:", "workbench.templates[0] (base): remove fw.agents"},
		{"a key of a pool", "    default: true\n", "    default: true\n    replicas: 2\n", "field replicas not found"},
		{"image without digest", "    image: " + testImage + "\n    env:\n      - name: OPENAI", "    image: ghcr.io/example/fw:latest\n    env:\n      - name: OPENAI", "workbench.templates[0] (base): image \"ghcr.io/example/fw:latest\" has no digest"},
		{"a name that is no name", "  - name: base\n", "  - name: Base_1\n", "name \"Base_1\" is not a lowercase name"},
		{"negative generation", "    generation: 2\n", "    generation: -1\n", "generation -1 is below 0"},
		{"no egress", "        - http: {hostnames: [nats.agents.svc.cluster.local], ports: [8080]}\n        - http: {hostnames: [model-proxy.models.svc.cluster.local], ports: [8080]}\n    resources:", "        []\n    resources:", "set egress.rules"},
		{"blind token", "      access: {admins: [\"kind:token\"]}\n", "", "an agent that a person creates from the template"},
		{"unknown template", "    template: base\n", "    template: nope\n", "workbench.pools[0] (triage): template \"nope\" is not in workbench.templates; the templates are base (the default)"},
		{"the token in env", "      - name: TICKETS_TOKEN\n        fromEnv: FRONT", "      - name: FW_API_TOKEN\n        fromEnv: FRONT", "workbench.templates[0] (base).env[1]: remove FW_API_TOKEN"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			text := strings.Replace(templateFixture, c.from, c.to, 1)
			if text == templateFixture {
				t.Fatal("the case changes nothing")
			}
			_, _, err := LoadAll(configOf(t, text), testFront())
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("LoadAll = %v, want %q", err, c.want)
			}
		})
	}
	// Two templates with one name, and two defaults, are errors.
	second := strings.Replace(strings.Split(strings.TrimPrefix(templateFixture, "\ntemplates:\n"), "pools:\n")[0], "  - name: base\n", "  - name: base\n", 1)
	two := "\ntemplates:\n" + strings.Split(strings.TrimPrefix(templateFixture, "\ntemplates:\n"), "pools:\n")[0] + second
	_, _, err := LoadAll(configOf(t, two), testFront())
	for _, want := range []string{"has the name of workbench.templates[0]", "sets default, and so does workbench.templates[0] (base); set default on one template only"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("LoadAll of two templates = %v, want %q", err, want)
		}
	}
	// The front needs a fleet for its templates too.
	front := testFront()
	front.Egress = ""
	if _, _, err := LoadAll(configOf(t, strings.Split(templateFixture, "pools:\n")[0]), front); err == nil || !strings.Contains(err.Error(), "workbench.templates needs workbench.fleet.egress") {
		t.Fatalf("LoadAll without egress = %v", err)
	}
}

// runtimeEntry is a runtime pool of the agent helper from the template
// base.
func runtimeEntry() Runtime {
	return Runtime{Name: "helper", Template: "base", Agent: "helper", Replicas: 2, Generation: 1, By: "alice", At: time.Unix(1_800_000_000, 0).UTC()}
}

// A runtime pool renders from its template and its overrides. Its profile
// comes from the overlay alone, so it renders no Task before the overlay
// holds the profile, and the model check waits for the overlay. Its Tasks
// carry the marks of a pool of the file, and its generation is the one of
// its entry plus the one of its template.
func TestRuntimePoolRenders(t *testing.T) {
	_, templates := loadAll(t, templateFixture)
	e := runtimeEntry()
	e.Env = []RuntimeEnv{{Name: "OPENAI_API_KEY", Value: "override"}, {Name: "EXTRA", Value: "1"}}
	e.Resources = &ax.ResourcesSpec{Limits: ax.ResourceList{CPU: "4"}}
	base, err := FromRuntime(e, templates)
	if err != nil {
		t.Fatal(err)
	}
	if base.Source() != SourceRuntime || base.Generation != 3 || base.Template != "base" || base.Intake != "fw.jobs.helper" || base.Wanted() != 2 {
		t.Fatalf("base = source %q generation %d template %q intake %q", base.Source(), base.Generation, base.Template, base.Intake)
	}
	if base.Resources.Limits.CPU != "4" || base.Resources.Limits.Memory != "2Gi" {
		t.Fatalf("resources = %+v", base.Resources)
	}
	if len(base.Profiles()) != 0 {
		t.Fatalf("a runtime pool before its overlay runs %v", base.Profiles())
	}
	again, _ := FromRuntime(e, templates)
	if base.Hash() == "" || again.Hash() != base.Hash() {
		t.Fatalf("the hash is not stable: %q %q", base.Hash(), again.Hash())
	}
	// A change of the entry raises its generation and changes the hash.
	e2 := e
	e2.Generation = 2
	if next, _ := FromRuntime(e2, templates); next.Generation != 4 || next.Hash() == base.Hash() {
		t.Fatalf("generation 2 = %d, hash %q", next.Generation, next.Hash())
	}

	if _, err := base.WithOverlay(Overlay{}); !errors.Is(err, ErrNoProfile) || !strings.Contains(err.Error(), "waits for the profile helper") {
		t.Fatalf("WithOverlay without the profile = %v", err)
	}
	o := Overlay{Profiles: []OverlayProfile{{Profile: fw.AgentProfile{Name: "helper", Description: "Helps.", Access: &fw.Access{Owners: []string{"alice"}}}, By: "alice"}}}
	merged, err := base.WithOverlay(o)
	if err != nil {
		t.Fatal(err)
	}
	if merged.Hash() == base.Hash() || merged.baseHash() != base.Hash() || merged.AgentAccess() == nil || merged.AgentAccess().Owners[0] != "alice" {
		t.Fatalf("merged = hash %q base %q", merged.Hash(), merged.baseHash())
	}
	if prof, ok := merged.Profiles()["helper"]; !ok || prof.Description != "Helps." {
		t.Fatalf("merged profiles = %v", merged.Profiles())
	}
	task, ws, err := merged.Render(1, testLookup)
	if err != nil {
		t.Fatal(err)
	}
	env := map[string]string{}
	for _, v := range task.Spec.Env {
		env[v.Name] = v.Value
	}
	if env[EnvPool] != "helper" || env[EnvSpec] != merged.Hash() || env[EnvGeneration] != "3" || env[EnvBase] != base.Hash() ||
		env["OPENAI_API_KEY"] != "override" || env["EXTRA"] != "1" || env["TICKETS_TOKEN"] != "tickets-secret" || env["FW_API_TOKEN"] != "front-secret-token" {
		t.Fatalf("env = %v", env)
	}
	if task.Metadata.Name != "helper-1" || !strings.HasPrefix(ws.Spec.Files[0].Content, header("helper")) || !strings.Contains(ws.Spec.Files[0].Content, "Helps.") {
		t.Fatalf("task %q, workspace %q", task.Metadata.Name, ws.Spec.Files[0].Content)
	}
	st := Initial(merged, "waits")
	if st.Source != SourceRuntime || st.By != "alice" || st.At == nil || st.Template != "base" || st.Generation != 3 {
		t.Fatalf("status = %+v", st)
	}

	// Without a model in the template, the profile must name one.
	noModel := strings.Replace(templateFixture, "      model: openai/some-model\n", "", 1)
	_, bare := loadAll(t, strings.Replace(noModel, "      effort: low\n", "      effort: low\n      model: openai/some-model\n", 1))
	base, err = FromRuntime(runtimeEntry(), bare)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := base.WithOverlay(o); err == nil || !strings.Contains(err.Error(), "set fw.model") {
		t.Fatalf("a profile without a model on a template without one = %v", err)
	}
	withModel := Overlay{Profiles: []OverlayProfile{{Profile: fw.AgentProfile{Name: "helper", Model: "openai/some-model", Access: &fw.Access{Owners: []string{"alice"}}}}}}
	if _, err := base.WithOverlay(withModel); err != nil {
		t.Fatalf("a profile with a model = %v", err)
	}
}

// A runtime pool that does not fit names each problem.
func TestRuntimePoolChecks(t *testing.T) {
	_, templates := loadAll(t, templateFixture)
	cases := []struct {
		name string
		mut  func(*Runtime)
		want string
	}{
		{"no template", func(e *Runtime) { e.Template = "nope" }, "names the template \"nope\", which workbench.templates does not hold; the templates are base (the default)"},
		{"bad agent", func(e *Runtime) { e.Agent = "a b" }, "agent \"a b\" is not the name of an agent profile"},
		{"too many replicas", func(e *Runtime) { e.Replicas = MaxRuntimeReplicas + 1 }, "the most Tasks of a runtime pool"},
		{"negative replicas", func(e *Runtime) { e.Replicas = -1 }, "replicas -1 is below 0"},
		{"a reserved variable", func(e *Runtime) { e.Env = []RuntimeEnv{{Name: "FW_POOL", Value: "x"}} }, "remove FW_POOL"},
		{"the token", func(e *Runtime) { e.Env = []RuntimeEnv{{Name: "FW_API_TOKEN", Value: "x"}} }, "remove FW_API_TOKEN"},
		{"a long value", func(e *Runtime) { e.Env = []RuntimeEnv{{Name: "X", Value: strings.Repeat("x", MaxRuntimeEnvValue+1)}} }, "holds more than 4096 bytes"},
		{"a bad quantity", func(e *Runtime) { e.Resources = &ax.ResourcesSpec{Requests: ax.ResourceList{Memory: "lots"}} }, "resources.requests.memory \"lots\" is not a quantity"},
		{"a bad name", func(e *Runtime) { e.Name = "Helper" }, "is not a lowercase name"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := runtimeEntry()
			c.mut(&e)
			if _, err := FromRuntime(e, templates); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("FromRuntime = %v, want %q", err, c.want)
			}
		})
	}
}

// The store writes each runtime pool with a compare-and-swap: a second
// create of a name fails, an update raises the generation and survives a
// writer that came first, a removal at an old revision fails, and a pool
// of an old name continues its generations.
func TestRuntimeStore(t *testing.T) {
	ctx := context.Background()
	s := NewStore(bucket.NewMem(StoreBucket), nil)
	created, err := s.Create(ctx, runtimeEntry())
	if err != nil || created.Generation != 1 || created.Rev == 0 {
		t.Fatalf("Create = %+v, %v", created, err)
	}
	if _, err := s.Create(ctx, runtimeEntry()); !errors.Is(err, ErrRuntimeExists) {
		t.Fatalf("a second Create = %v", err)
	}
	raced := false
	updated, err := s.Update(ctx, "helper", func(e *Runtime) error {
		if !raced {
			raced = true
			// Another replica writes between the read and the write.
			if _, err := s.Update(ctx, "helper", func(e *Runtime) error { e.Revision = "other"; return nil }); err != nil {
				t.Fatal(err)
			}
		}
		e.Replicas = 5
		return nil
	})
	if err != nil || updated.Replicas != 5 || updated.Revision != "other" || updated.Generation != 3 {
		t.Fatalf("Update = %+v, %v", updated, err)
	}
	if _, err := s.Update(ctx, "nope", func(*Runtime) error { return nil }); !errors.Is(err, ErrRuntimeNotFound) {
		t.Fatalf("Update of no pool = %v", err)
	}
	if err := s.Remove(ctx, "helper", created.Rev); !errors.Is(err, ErrRuntimeConflict) {
		t.Fatalf("Remove at an old revision = %v", err)
	}
	list, err := s.List(ctx)
	if err != nil || len(list) != 1 || list[0].Replicas != 5 {
		t.Fatalf("List = %+v, %v", list, err)
	}
	if err := s.Remove(ctx, "helper", updated.Rev); err != nil {
		t.Fatal(err)
	}
	if err := s.Remove(ctx, "helper", updated.Rev); err != nil {
		t.Fatalf("a second Remove = %v", err)
	}
	if list, _ := s.List(ctx); len(list) != 0 {
		t.Fatalf("List after Remove = %+v", list)
	}
	again, err := s.Create(ctx, runtimeEntry())
	if err != nil || again.Generation != 4 {
		t.Fatalf("Create of an old name = %+v, %v", again, err)
	}
}

// A pass runs a runtime pool as a pool of the file: it records the
// generation in the marker and creates the Tasks with the marks of the
// pool. Forget deletes the marker once the Tasks are gone.
func TestRuntimePoolPass(t *testing.T) {
	_, templates := loadAll(t, templateFixture)
	base, err := FromRuntime(runtimeEntry(), templates)
	if err != nil {
		t.Fatal(err)
	}
	merged, err := base.WithOverlay(Overlay{Profiles: []OverlayProfile{{Profile: fw.AgentProfile{Name: "helper", Access: &fw.Access{Owners: []string{"alice"}}}}}})
	if err != nil {
		t.Fatal(err)
	}
	fake, check := newFakeAX(), newFakeCheck()
	vals, err := Resolve([]*Pool{merged}, testLookup)
	if err != nil {
		t.Fatal(err)
	}
	r := &Reconciler{AX: fake, Check: check, Values: vals, Log: quietLog()}
	ctx := context.Background()
	st := r.Reconcile(ctx, merged, fake.list("team"), time.Now())
	if st.Actual != 2 || st.Source != SourceRuntime || st.Generation != 3 {
		t.Fatalf("status = %+v", st)
	}
	for _, name := range []string{"helper-0", "helper-1"} {
		task := fake.task("team", name)
		if task == nil || envOf(task, EnvPool) != "helper" || envOf(task, EnvSpec) != merged.Hash() || envOf(task, EnvGeneration) != "3" {
			t.Fatalf("the Task %s = %v", name, task)
		}
	}
	ws, err := fake.GetWorkspace(ctx, markerRequest(merged))
	if err != nil {
		t.Fatal(err)
	}
	if gen, ok := merged.markerOf(ws); !ok || gen != 3 {
		t.Fatalf("marker = %d, %v", gen, ok)
	}
	// Forget leaves a marker of another pool alone, and deletes its own.
	if err := r.Forget(ctx, merged); err != nil {
		t.Fatal(err)
	}
	if _, err := fake.GetWorkspace(ctx, markerRequest(merged)); err == nil {
		t.Fatal("the marker stays")
	}
	if err := r.Forget(ctx, merged); err != nil {
		t.Fatalf("a second Forget = %v", err)
	}
	if !strings.Contains(strings.Join(fake.took(), "\n"), "delete workspace team/helper-generation") {
		t.Fatal("no delete of the marker")
	}
}

// quietLog is a logger that drops every line.
func quietLog() *slog.Logger { return slog.New(slog.DiscardHandler) }

// markerRequest reads the marker of a pool.
func markerRequest(p *Pool) *v1alpha1.GetWorkspaceRequest {
	return &v1alpha1.GetWorkspaceRequest{Atespace: p.Atespace, Name: p.MarkerName()}
}
