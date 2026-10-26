// Package envcheck holds a test that compares the block environment that a
// pool renders with the package env of the kernel. The workbench does not
// link that package, because its init gives a process the tool env, the
// service of the install, and the defaults of the policy. This test links
// it in a binary of its own, so no other test sees those defaults.
package envcheck

import (
	"bytes"
	"slices"
	"strings"
	"testing"

	"github.com/bdubs00/featherweight-kernel/env"
	"github.com/bdubs00/featherweight-kernel/env/pkglist"
	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/pools"
	"gopkg.in/yaml.v3"
)

const image = "ghcr.io/example/fw@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

const block = `
packages:
  mirrors:
    python: http://pkg-cache.fw-fleet.svc.cluster.local:8080/pypi/simple
    tools: http://pkg-cache.fw-fleet.svc.cluster.local:8080/conda-forge
  egress: [http://pkg-cache.fw-fleet.svc.cluster.local:8080]
pools:
  - name: lab
    replicas: 1
    image: ` + image + `
    egress:
      hosts: [http://nats.agents.svc.cluster.local:8080]
    packages: {on: true, ask: true, python: [ansible-lint, "pyyaml>=6"], tools: [iproute2]}
    fw:
      model: openai/some-model
      bus: {url: "ws://nats.agents.svc.cluster.local:8080"}
      agents:
        lab: {description: Works in the lab.}
`

// The names of the tool env and its rules in the workbench are those of
// the kernel.
func TestNamesMatchTheKernel(t *testing.T) {
	if pools.EnvTool != env.Name {
		t.Fatalf("pools.EnvTool = %q, the kernel names %q", pools.EnvTool, env.Name)
	}
	if !slices.Equal(pools.EnvAskRules, env.AskRules) {
		t.Fatalf("pools.EnvAskRules = %v, the kernel adds %v", pools.EnvAskRules, env.AskRules)
	}
	if pools.ListPython != pkglist.Python || pools.ListTools != pkglist.Tools {
		t.Fatal("the names of the lists differ from the kernel")
	}
}

// The block environment that a pool with packages renders decodes as the
// block of the kernel, key by key, and its lists and mirrors pass the
// checks of the kernel.
func TestRenderedBlockFitsTheKernel(t *testing.T) {
	var n yaml.Node
	if err := yaml.Unmarshal([]byte(block), &n); err != nil {
		t.Fatal(err)
	}
	cfg := fw.DefaultConfig()
	cfg.Bus.Driver, cfg.Bus.URL = "nats", "nats://nats.agents.svc.cluster.local:4222"
	cfg.Extensions = map[string]yaml.Node{"workbench": *n.Content[0]}
	mirror, err := pools.LoadMirror(cfg)
	if err != nil {
		t.Fatal(err)
	}
	list, err := pools.Load(cfg, pools.Front{
		Atespaces: []string{"team"}, Egress: "fw.events", Intake: "fw.jobs", Bus: cfg.Bus,
		AX: fw.AXConfig{Server: "ax:8080", Router: "router"}, TokenEnv: "FW_API_TOKEN", Packages: mirror,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, ws, err := list[0].Render(0, pools.Placeholders)
	if err != nil {
		t.Fatal(err)
	}
	var file struct {
		Environment yaml.Node `yaml:"environment"`
	}
	if err := yaml.Unmarshal([]byte(ws.Spec.Files[0].Content), &file); err != nil {
		t.Fatal(err)
	}
	if file.Environment.Kind == 0 {
		t.Fatalf("the file has no block environment:\n%s", ws.Spec.Files[0].Content)
	}
	text, err := yaml.Marshal(&file.Environment)
	if err != nil {
		t.Fatal(err)
	}
	var c env.Config
	dec := yaml.NewDecoder(bytes.NewReader(text))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		t.Fatalf("the block does not decode as the block of the kernel: %v\n%s", err, text)
	}
	if c.Packages == nil || c.Packages.Python == nil || c.Packages.Tools == nil || c.Mirrors == nil || c.On == nil || !*c.On || c.Ask == nil || !*c.Ask {
		t.Fatalf("the block = %+v", c)
	}
	if !slices.Equal(*c.Packages.Python, []string{"ansible-lint", "pyyaml>=6"}) || !slices.Equal(*c.Packages.Tools, []string{"iproute2"}) {
		t.Fatalf("the lists = %v %v", *c.Packages.Python, *c.Packages.Tools)
	}
	for _, l := range [][]string{*c.Packages.Python, *c.Packages.Tools} {
		if err := pkglist.Check(l); err != nil {
			t.Fatal(err)
		}
	}
	for _, u := range []string{c.Mirrors.Python, c.Mirrors.Tools} {
		if err := pkglist.CheckMirror(u); err != nil || !strings.HasPrefix(u, "http://pkg-cache") {
			t.Fatalf("mirror %q: %v", u, err)
		}
	}
	if c.Python != "" || c.Tools != "" || c.Placement != "" {
		t.Fatalf("the block names a manifest or a placement: %+v", c)
	}
}
