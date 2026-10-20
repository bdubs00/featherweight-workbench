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
		!slices.Equal(b.EgressTypes, testFront().EgressTypes) || b.Options["concurrency"] != "1" {
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

// A value that does not fit is an error that names its key and what to
// set, every problem at once.
func TestLoadNamesWhatToSet(t *testing.T) {
	cases := []struct {
		name, from, to, want string
	}{
		{"image without digest", testImage, "ghcr.io/example/fw:latest", "has no digest"},
		{"no model", "      model: openai/some-model\n", "", "set fw.model"},
		{"negative replicas", "replicas: 3", "replicas: -1", "replicas -1 is below 0"},
		{"no replicas", "replicas: 3", "", "set replicas"},
		{"the token in the file", "- name: OPENAI_API_KEY", "- name: FW_API_TOKEN", "remove FW_API_TOKEN: the front gives each worker its own static token"},
		{"value and fromEnv", "fromEnv: FRONT_TICKETS_TOKEN", "fromEnv: FRONT_TICKETS_TOKEN\n        value: x", "sets value and fromEnv"},
		{"unknown key of a pool", "replicas: 3", "replicas: 3\n    replica: 2", "field replica not found"},
		{"unknown key of the workers", "effort: low", "effort: low\n      modle: x", "fw.modle is not a key of the configuration of fw"},
		{"unknown nested key of the workers", "effort: low", "effort: low\n      time: {zones: x}", "field zones not found"},
		{"atespace of no fleet", "replicas: 3", "replicas: 3\n    atespace: other", "is not in workbench.fleet.atespaces"},
		{"intake in fw", `bus: {url: "ws://nats.agents.svc.cluster.local:8080"}`, `bus: {url: "ws://nats.agents.svc.cluster.local:8080", intake: x}`, "remove fw.bus.intake"},
		{"token blind", "description: Sorts new tickets.", "description: Sorts new tickets.\n          access: {operators: [\"group:support\"]}", "add kind:token to fw.access.admins"},
		{"no egress", "    egress:\n      rules:\n        - http: {hostnames: [nats.agents.svc.cluster.local], ports: [8080]}\n        - http: {hostnames: [model-proxy.models.svc.cluster.local], ports: [8080]}\n        - https: {hostnames: [mcp.example.com]}\n", "", "set egress.rules"},
		{"bad hostname", "mcp.example.com]", "MCP.Example.com]", "does not fit the ax API"},
		{"no token mode", "effort: low", "effort: low\n      auth: {mode: none}", "leaves out token"},
		{"reserved file", "path: notes/triage.md", "path: .fw/fw.yaml", "the pool writes .fw/fw.yaml from fw"},
		{"a configuration variable", "- name: OPENAI_API_KEY", "- name: FW_ACCESS_DEFAULT", "remove FW_ACCESS_DEFAULT: a variable FW_* changes the configuration of the worker"},
		{"a variable of ax", "- name: OPENAI_API_KEY", "- name: AX_TASK_YAML", "ax sets the variables AX_*"},
		{"two protocols in one rule", "- https: {hostnames: [mcp.example.com]}", "- {https: {hostnames: [mcp.example.com]}, tls: {hostnames: [mcp.example.com]}}", "egress.rules[2] sets 2 protocols"},
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
	if _, err := Load(configOf(t, noURL), front); err == nil || strings.Contains(err.Error(), "s3cretpass") || !strings.Contains(err.Error(), "the bus URL of the front holds a credential") {
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
	for _, change := range []struct{ from, to string }{
		{"replicas: 3", "replicas: 3\n    revision: \"2\""},
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
	if task.Spec.Resources.Requests.Cpu != "500m" || len(task.Spec.Egress.Rules) != 3 || task.Spec.Egress.Rules[2].Https.Hostnames[0] != "mcp.example.com" {
		t.Fatalf("spec = %+v", task.Spec)
	}
	if _, _, err := p.Render(0, func(string) (string, bool) { return "", false }); err == nil || !strings.Contains(err.Error(), "FRONT_TICKETS_TOKEN") {
		t.Fatalf("Render without the front = %v", err)
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
	text := strings.Replace(fixture, "- https: {hostnames: [mcp.example.com]}", "- https: {hostnames: [other.example.com]}", 1)
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
	for _, want := range []string{"<secret: FW_API_TOKEN of the front>", "<secret: FRONT_TICKETS_TOKEN of the front>", "FW_POOL_SPEC=" + list[0].Hash(),
		"Warning: workbench.pools[0] (triage): egress.rules allows no https connection to mcp.example.com:443, the MCP server tickets"} {
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
