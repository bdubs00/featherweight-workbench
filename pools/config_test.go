package pools

import (
	"slices"
	"strings"
	"testing"

	"github.com/bdubs00/featherweight-kernel/ax"
	"github.com/bdubs00/featherweight-kernel/fw"
	"gopkg.in/yaml.v3"
)

// The defaults come from the front and the configuration: the only
// atespace, the only profile, its queue, and the keys of the bus that the
// front reads. The fw.yaml of a worker carries them, and the person wins
// over a default but never over a key that the pool sets.
func TestLoadFillsTheDefaults(t *testing.T) {
	list := loadFixture(t, fixture)
	if len(list) != 1 {
		t.Fatalf("pools = %d", len(list))
	}
	p := list[0]
	if p.Atespace != "team" || p.Agent != "triage" || p.Intake != "fw.jobs.triage" || p.Wanted() != 3 {
		t.Fatalf("defaults = %q %q %q %d", p.Atespace, p.Agent, p.Intake, p.Wanted())
	}
	if len(p.Hash()) != 16 {
		t.Fatalf("hash = %q", p.Hash())
	}
	if len(p.Warnings()) != 0 {
		t.Fatalf("warnings = %v", p.Warnings())
	}
	file := p.files[0]
	if file.Path != ConfigFile || !strings.HasPrefix(file.Content, header("triage")) {
		t.Fatalf("first file = %q", file.Path)
	}
	wc := fw.DefaultConfig()
	if err := yaml.Unmarshal([]byte(file.Content), wc); err != nil {
		t.Fatal(err)
	}
	b := wc.Bus
	if b.Driver != "nats" || b.URL != "ws://nats.agents.svc.cluster.local:8080" || b.Intake != "fw.jobs.triage" || b.Egress != "fw.events" ||
		!slices.Equal(b.EgressTypes, testFront().EgressTypes) || b.EgressMaxBytes != 4096 || b.Options["concurrency"] != "1" {
		t.Fatalf("bus = %+v", b)
	}
	if wc.Auth.Mode != "token" || wc.AX.Router != "atenet-router" || wc.Model != "openai/some-model" || wc.Policy.Default != "allow" {
		t.Fatalf("worker = auth %q, router %q, model %q, policy %q", wc.Auth.Mode, wc.AX.Router, wc.Model, wc.Policy.Default)
	}
	if p.files[1].Path != "notes/triage.md" {
		t.Fatalf("files = %+v", p.files)
	}
	if got := FromEnv(list); !slices.Equal(got, []string{"FRONT_TICKETS_TOKEN", "FW_API_TOKEN"}) {
		t.Fatalf("FromEnv = %v", got)
	}
}

// The worker cuts the long strings of its stream to the bound of the
// front, unless the pool sets its own bound, and a pool that sets 0
// publishes the data whole.
func TestLoadEgressMaxBytes(t *testing.T) {
	for set, want := range map[string]int{"": 4096, ", egressMaxBytes: 1000": 1000, ", egressMaxBytes: 0": 0} {
		text := strings.Replace(fixture, `bus: {url: "ws://nats.agents.svc.cluster.local:8080"}`, `bus: {url: "ws://nats.agents.svc.cluster.local:8080"`+set+`}`, 1)
		list := loadFixture(t, text)
		wc := fw.DefaultConfig()
		if err := yaml.Unmarshal([]byte(list[0].files[0].Content), wc); err != nil {
			t.Fatal(err)
		}
		if wc.Bus.EgressMaxBytes != want {
			t.Errorf("bus %q: egressMaxBytes = %d, want %d", set, wc.Bus.EgressMaxBytes, want)
		}
	}
}

// The workers of a pool take files of at most tools.files.maxFileBytes of
// their configuration, and of the default of the kernel without one.
func TestPoolMaxFileBytes(t *testing.T) {
	if got := loadFixture(t, fixture)[0].MaxFileBytes(); got != fw.DefaultMaxFileBytes {
		t.Errorf("default = %d", got)
	}
	text := strings.Replace(fixture, "      tools:\n        builtin: [files, clock]", "      tools:\n        files: {maxFileBytes: 16777216}\n        builtin: [files, clock]", 1)
	if text == fixture {
		t.Fatal("the fixture changed")
	}
	if got := loadFixture(t, text)[0].MaxFileBytes(); got != 16<<20 {
		t.Errorf("set = %d", got)
	}
	if got := (&Pool{}).MaxFileBytes(); got != fw.DefaultMaxFileBytes {
		t.Errorf("no worker = %d", got)
	}
}

// A value that does not fit is an error that names its key and what to
// set, every problem at once.
func TestLoadNamesWhatToSet(t *testing.T) {
	cases := []struct {
		name, from, to, want string
	}{
		{"image without digest", testImage, "ghcr.io/example/fw:latest", "has no digest"},
		{"no model", "      model: openai/some-model\n", "", "set fw.model"},
		{"negative replicas", "replicas: 3", "replicas: -1", "replicas -1 is below 0"},
		{"negative generation", "replicas: 3", "replicas: 3\n    generation: -1", "generation -1 is below 0"},
		{"the generation in env", "- name: OPENAI_API_KEY", "- name: FW_POOL_GENERATION", "remove FW_POOL_GENERATION: the server sets it to the generation of the pool"},
		{"no replicas", "replicas: 3", "", "set replicas"},
		{"the token in the file", "- name: OPENAI_API_KEY", "- name: FW_API_TOKEN", "remove FW_API_TOKEN: the server gives each worker its own static token"},
		{"value and fromEnv", "fromEnv: FRONT_TICKETS_TOKEN", "fromEnv: FRONT_TICKETS_TOKEN\n        value: x", "sets value and fromEnv"},
		{"unknown key of a pool", "replicas: 3", "replicas: 3\n    replica: 2", "field replica not found"},
		{"unknown key of the workers", "effort: low", "effort: low\n      modle: x", "fw.modle is not a key of the configuration of fw"},
		{"unknown nested key of the workers", "effort: low", "effort: low\n      time: {zones: x}", "field zones not found"},
		{"atespace of no fleet", "replicas: 3", "replicas: 3\n    atespace: other", "is not in workbench.fleet.atespaces"},
		{"intake in fw", `bus: {url: "ws://nats.agents.svc.cluster.local:8080"}`, `bus: {url: "ws://nats.agents.svc.cluster.local:8080", intake: x}`, "remove fw.bus.intake"},
		{"token blind", "description: Sorts new tickets.", "description: Sorts new tickets.\n          access: {operators: [\"group:support\"]}", "add kind:token to fw.access.admins"},
		{"no egress", "    egress:\n      hosts:\n        - http://nats.agents.svc.cluster.local:8080\n        - http://model-proxy.models.svc.cluster.local:8080\n        - https://mcp.example.com\n", "", "set egress.hosts"},
		{"bad hostname", "https://mcp.example.com", "https://MCP.Example.com", "egress.hosts[2]"},
		{"no token mode", "effort: low", "effort: low\n      auth: {mode: none}", "leaves out token"},
		{"reserved file", "path: notes/triage.md", "path: .fw/fw.yaml", "the pool writes .fw/fw.yaml from fw"},
		{"a configuration variable", "- name: OPENAI_API_KEY", "- name: FW_ACCESS_DEFAULT", "remove FW_ACCESS_DEFAULT: a variable FW_* changes the configuration of the worker"},
		{"a variable of ax", "- name: OPENAI_API_KEY", "- name: AX_TASK_YAML", "ax sets the variables AX_*"},
		{"a path in a host", "- https://mcp.example.com", "- https://mcp.example.com/mcp", "egress.hosts[2]"},
		{"a credential in the bus", `bus: {url: "ws://nats.agents.svc.cluster.local:8080"}`, `bus: {url: "ws://user:s3cret@nats.agents.svc.cluster.local:8080"}`, "fw.bus.url holds a credential"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			text := strings.Replace(fixture, c.from, c.to, 1)
			if text == fixture {
				t.Fatalf("the case changes nothing")
			}
			_, err := Load(configOf(t, text), testFront())
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("Load = %v, want %q", err, c.want)
			}
		})
	}

	// The front needs a fleet that sees the workers.
	front := testFront()
	front.Egress, front.Bus.Driver = "", "mem"
	_, err := Load(configOf(t, fixture), front)
	for _, want := range []string{"needs workbench.fleet.egress", "set bus.driver to nats or redis"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("Load = %v, want %q", err, want)
		}
	}
	// Two pools with one name, once trimmed, are an error.
	two := fixture + strings.Replace(strings.TrimPrefix(fixture, "\npools:\n"), "- name: triage", "- name: \" triage\"", 1)
	if _, err := Load(configOf(t, two), testFront()); err == nil || !strings.Contains(err.Error(), "has the name of workbench.pools[0]") {
		t.Fatalf("Load of two pools = %v", err)
	}
	// The pool never copies a bus URL of the front that holds a credential.
	front = testFront()
	front.Bus.URL = "nats://user:s3cretpass@nats.agents.svc.cluster.local:4222"
	noURL := strings.Replace(fixture, `bus: {url: "ws://nats.agents.svc.cluster.local:8080"}`, "", 1)
	if _, err := Load(configOf(t, noURL), front); err == nil || strings.Contains(err.Error(), "s3cretpass") || !strings.Contains(err.Error(), "the bus URL of the server holds a credential") {
		t.Fatalf("Load = %v", err)
	}
	// A literal password beside a reference is a credential.
	lit := strings.Replace(fixture, `bus: {url: "ws://nats.agents.svc.cluster.local:8080"}`, `bus: {url: "ws://${NATS_USER}:literalpass@nats.agents.svc.cluster.local:8080"}`, 1)
	if _, err := Load(configOf(t, lit), testFront()); err == nil || !strings.Contains(err.Error(), "fw.bus.url holds a credential") {
		t.Fatalf("Load with a literal password = %v", err)
	}
	// A reference that the worker expands is no credential.
	ref := strings.Replace(fixture, `bus: {url: "ws://nats.agents.svc.cluster.local:8080"}`, `bus: {url: "ws://fw:$${NATS_PASSWORD}@nats.agents.svc.cluster.local:8080"}`, 1)
	if _, err := Load(configOf(t, strings.ReplaceAll(ref, "$$", "$")), testFront()); err != nil {
		t.Fatalf("Load with a reference = %v", err)
	}
	// A configuration without pools is no error.
	if list, err := Load(fw.DefaultConfig(), Front{}); err != nil || list != nil {
		t.Fatalf("Load without pools = %v, %v", list, err)
	}
}

// The hash covers what the Tasks share, and the name of a variable of the
// front instead of its value, so the dry run computes the hash of the
// front, and a secret never changes it. A new revision changes it.
func TestHashFollowsTheConfiguration(t *testing.T) {
	a := loadFixture(t, fixture)[0].Hash()
	if b := loadFixture(t, fixture)[0].Hash(); a != b {
		t.Fatalf("hash of one configuration = %s and %s", a, b)
	}
	if b := loadFixture(t, strings.Replace(fixture, "replicas: 3", "replicas: 5", 1))[0].Hash(); a != b {
		t.Fatalf("the replica count changed the hash")
	}
	// A generation of 0 leaves the hash of a pool without one, so an
	// upgrade of the front replaces no Task.
	if b := loadFixture(t, strings.Replace(fixture, "replicas: 3", "replicas: 3\n    generation: 0", 1))[0].Hash(); a != b {
		t.Fatalf("generation 0 changed the hash")
	}
	for _, change := range []struct{ from, to string }{
		{"replicas: 3", "replicas: 3\n    revision: \"2\""},
		{"replicas: 3", "replicas: 3\n    generation: 1"},
		{"effort: low", "effort: medium"},
		{"the-proxy-needs-no-key", "another"},
		{"FRONT_TICKETS_TOKEN", "OTHER_TICKETS_TOKEN"},
	} {
		if b := loadFixture(t, strings.Replace(fixture, change.from, change.to, 1))[0].Hash(); a == b {
			t.Errorf("%s to %s kept the hash", change.from, change.to)
		}
	}
}

// A Task carries the variables in order, the marks of the pool, and the
// values of the front; the dry run carries placeholders instead.
func TestRenderResolvesTheFront(t *testing.T) {
	p := loadFixture(t, fixture)[0]
	task, ws, err := p.Render(2, testLookup)
	if err != nil {
		t.Fatal(err)
	}
	if task.Metadata.Name != "triage-2" || ws.Metadata.Name != "triage-2-ws" || task.Spec.Workspaces[0].Name != "triage-2-ws" || task.Spec.Workspaces[0].Path != WorkspacePath {
		t.Fatalf("names = %s, %s, %+v", task.Metadata.Name, ws.Metadata.Name, task.Spec.Workspaces)
	}
	env := map[string]string{}
	var order []string
	for _, e := range task.Spec.Env {
		env[e.Name] = e.Value
		order = append(order, e.Name)
	}
	if !slices.Equal(order, []string{"OPENAI_API_KEY", "TICKETS_TOKEN", EnvPool, EnvBashHide, "FW_API_TOKEN", EnvSpec}) {
		t.Fatalf("env order = %v", order)
	}
	if env[EnvBashHide] != "AX_TASK_YAML,FW_API_TOKEN,TICKETS_TOKEN" {
		t.Fatalf("%s = %q", EnvBashHide, env[EnvBashHide])
	}
	if env["FW_API_TOKEN"] != "front-secret-token" || env["TICKETS_TOKEN"] != "tickets-secret" || env[EnvPool] != "triage" || env[EnvSpec] != p.Hash() {
		t.Fatalf("env = %v", env)
	}
	if task.Spec.Resources.Requests.Cpu != "500m" || len(task.Spec.Egress.Hosts) != 3 || task.Spec.Egress.Hosts[2] != "https://mcp.example.com" {
		t.Fatalf("spec = %+v", task.Spec)
	}
	if _, _, err := p.Render(0, func(string) (string, bool) { return "", false }); err == nil || !strings.Contains(err.Error(), "FRONT_TICKETS_TOKEN") {
		t.Fatalf("Render without the server = %v", err)
	}
}

// Resolve reads the variables of the front once and names the ones that
// are missing.
func TestResolve(t *testing.T) {
	list := loadFixture(t, fixture)
	if _, err := Resolve(list, func(n string) (string, bool) {
		if n == "FW_API_TOKEN" {
			return "x", true
		}
		return "", false
	}); err == nil || !strings.Contains(err.Error(), "FRONT_TICKETS_TOKEN (for the pool triage)") || strings.Contains(err.Error(), "FW_API_TOKEN") {
		t.Fatalf("Resolve = %v", err)
	}
	vals, err := Resolve(list, testLookup)
	if err != nil {
		t.Fatal(err)
	}
	if v, ok := vals.Lookup("FW_API_TOKEN"); !ok || v != "front-secret-token" {
		t.Fatalf("lookup = %q, %v", v, ok)
	}
	if _, ok := vals.Lookup("HOME"); ok {
		t.Fatal("the lookup reads a variable that no pool names")
	}
	if got := vals.Redact("ax refused env FW_API_TOKEN=front-secret-token and tickets-secret"); got != "ax refused env FW_API_TOKEN=<secret> and <secret>" {
		t.Fatalf("Redact = %q", got)
	}
	// A static token too short to hide stops the start.
	if _, err := Resolve(list, func(n string) (string, bool) {
		return map[string]string{"FW_API_TOKEN": "abc1234", "FRONT_TICKETS_TOKEN": "x"}[n], true
	}); err == nil ||
		!strings.Contains(err.Error(), "fewer than 8 characters") || strings.Contains(err.Error(), "abc1234") {
		t.Fatalf("Resolve with a short token = %v", err)
	}
}

// Redact replaces a value of the front and its quoted forms, and leaves a
// short value alone, because a short value is no secret.
func TestRedact(t *testing.T) {
	list := loadFixture(t, fixture)
	vals, err := Resolve(list, func(n string) (string, bool) {
		return map[string]string{"FW_API_TOKEN": `tok"en\value-123`, "FRONT_TICKETS_TOKEN": "abc"}[n], true
	})
	if err != nil {
		t.Fatal(err)
	}
	for in, want := range map[string]string{
		`raw tok"en\value-123 here`:    "raw <secret> here",
		`go "tok\"en\\value-123" here`: `go "<secret>" here`,
		"the pool created abc":         "the pool created abc",
	} {
		if got := vals.Redact(in); got != want {
			t.Errorf("Redact(%q) = %q, want %q", in, got, want)
		}
	}
}

// The dry run prints the Workspaces and the Tasks with a placeholder for
// every secret, in documents that the manifest reader of the kernel
// takes, and the warnings of a pool.
func TestWriteHidesSecrets(t *testing.T) {
	text := strings.Replace(fixture, "- https://mcp.example.com", "- https://other.example.com", 1)
	list := loadFixture(t, text)
	var b strings.Builder
	if err := Write(&b, list); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, secret := range []string{"front-secret-token", "tickets-secret"} {
		if strings.Contains(out, secret) {
			t.Fatalf("the dry run holds the secret %q", secret)
		}
	}
	for _, want := range []string{"<secret: FW_API_TOKEN of the server>", "<secret: FRONT_TICKETS_TOKEN of the server>", "FW_POOL_SPEC=" + list[0].Hash(),
		"Warning: workbench.pools[0] (triage): egress.hosts allows no https connection to mcp.example.com:443, the MCP server tickets"} {
		if !strings.Contains(out, want) {
			t.Errorf("the dry run has no %q", want)
		}
	}
	docs := strings.Split(out, "\n---\n")
	if len(docs) != 6 {
		t.Fatalf("documents = %d", len(docs))
	}
	task, err := ax.ParseTask([]byte(docs[1]))
	if err != nil || task == nil || task.Metadata.Name != "triage-0" || task.Spec.Image != testImage {
		t.Fatalf("ParseTask = %+v, %v", task, err)
	}
	ws, err := ax.ParseWorkspaces([]byte(docs[4]))
	if err != nil || len(ws) != 1 || ws[0].Metadata.Name != "triage-2-ws" || !strings.Contains(ws[0].Spec.Files[0].Content, "intake: fw.jobs.triage") {
		t.Fatalf("ParseWorkspaces = %+v, %v", ws, err)
	}
}

// A pool that sets no intake takes a queue that no other pool takes by
// default: the queue of its agent when the pool is named after the agent,
// else a queue named after the pool, under the intake prefix of the front
// or fw.jobs. An intake that the pool sets wins, and the worker reads the
// intake of its pool.
func TestLoadGivesEachPoolAQueueOfItsOwn(t *testing.T) {
	front := testFront()
	bare := testFront()
	bare.Intake = ""
	cases := []struct {
		name    string
		changes []string // pairs of old and new text of the fixture
		front   Front
		want    string
	}{
		{"named after its agent", nil, front, "fw.jobs.triage"},
		{"named after another name", []string{"- name: triage", "- name: crew-triage"}, front, "fw.jobs.crew-triage"},
		{"named after an agent that a pool name cannot hold", []string{"- name: triage", "- name: sort-tickets", "        triage:\n", "        sort_tickets:\n"}, front, "fw.jobs.sort_tickets"},
		{"a server without an intake prefix", []string{"- name: triage", "- name: crew-triage"}, bare, "fw.jobs.crew-triage"},
		{"an intake of its own", []string{"replicas: 3", "replicas: 3\n    intake: tickets.sort"}, front, "tickets.sort"},
		{"an intake of its own on a pool named after another name", []string{"- name: triage", "- name: crew-triage", "replicas: 3", "replicas: 3\n    intake: fw.jobs.triage"}, front, "fw.jobs.triage"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			text := fixture
			for i := 0; i+1 < len(c.changes); i += 2 {
				text = strings.Replace(text, c.changes[i], c.changes[i+1], 1)
			}
			list, err := Load(configOf(t, text), c.front)
			if err != nil {
				t.Fatal(err)
			}
			if p := list[0]; p.Intake != c.want || workerFileOf(t, p).Bus.Intake != c.want {
				t.Fatalf("the pool %s of %s takes %s, and its worker reads %s; want %s", p.Name, p.Agent, p.Intake, workerFileOf(t, p).Bus.Intake, c.want)
			}
		})
	}
	// A runtime pool that agent_put named after its agent takes the queue
	// of the agent, and one that a person named otherwise a queue of its
	// own.
	_, templates := loadAll(t, templateFixture)
	for _, c := range []struct{ name, agent, want string }{
		{"helper", "helper", "fw.jobs.helper"},
		{"helper-bot", "Helper_Bot", "fw.jobs.Helper_Bot"},
		{"helpers", "helper", "fw.jobs.helpers"},
	} {
		e := runtimeEntry()
		e.Name, e.Agent = c.name, c.agent
		if p, err := FromRuntime(e, templates); err != nil || p.Intake != c.want {
			t.Errorf("the runtime pool %s of %s = %v, %v; want %s", c.name, c.agent, p, err, c.want)
		}
	}
}

// NameAfter names a pool after an agent as agent_put names its runtime
// pool, and reports false for an agent that no pool name fits.
func TestNameAfter(t *testing.T) {
	for agent, want := range map[string]string{"reviewer": "reviewer", "Helper_Bot": "helper-bot", "a.b": "a-b", "-x-": "x", "___": "", "": ""} {
		if got, ok := NameAfter(agent); got != want || ok != (want != "") {
			t.Errorf("NameAfter(%q) = %q, %v; want %q", agent, got, ok, want)
		}
	}
}

// Two pools on one intake take the jobs of each other, so Load refuses
// them, unless both set that intake and so share the queue on purpose.
func TestLoadRefusesASharedIntake(t *testing.T) {
	second := strings.Replace(strings.TrimPrefix(fixture, "\npools:\n"), "- name: triage", "- name: crew-triage", 1)
	if list, err := Load(configOf(t, fixture+second), testFront()); err != nil || list[0].Intake != "fw.jobs.triage" || list[1].Intake != "fw.jobs.crew-triage" {
		t.Fatalf("two pools of one agent = %v, %v", list, err)
	}
	own := func(text string) string {
		return strings.Replace(text, "replicas: 3", "replicas: 3\n    intake: fw.jobs.triage", 1)
	}
	_, err := Load(configOf(t, fixture+own(second)), testFront())
	if err == nil || !strings.Contains(err.Error(), "workbench.pools[1] (crew-triage): the intake fw.jobs.triage is the intake of workbench.pools[0] (triage) too, so the workers of both pools would take the jobs of each other") {
		t.Fatalf("Load of a shared intake = %v", err)
	}
	if _, err := Load(configOf(t, own(fixture)+second), testFront()); err != nil {
		t.Fatalf("Load of an intake of its own = %v", err)
	}
	if list, err := Load(configOf(t, own(fixture)+own(second)), testFront()); err != nil || list[0].Intake != list[1].Intake {
		t.Fatalf("Load of an intake that both pools set = %v, %v", list, err)
	}
}
