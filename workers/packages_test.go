package workers

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/bdubs00/featherweight-workbench/fleet"
)

// packagesBlock is the block of the tests with a package mirror and the
// packages of the pool triage on. alice owns its agent, and bob views it.
const packagesBlock = `
packages:
  mirrors:
    python: http://pkg-cache.fw-fleet.svc.cluster.local:8080/pypi/simple
    tools: http://pkg-cache.fw-fleet.svc.cluster.local:8080/conda-forge
  egress: [http://pkg-cache.fw-fleet.svc.cluster.local:8080]
templates:
  - name: base
    default: true
    image: ghcr.io/example/fw@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
    egress:
      hosts:
        - http://nats.agents.svc.cluster.local:8080
    fw:
      model: openai/some-model
      bus: {url: "ws://nats.agents.svc.cluster.local:8080"}
      access: {admins: ["kind:token"]}
pools:
  - name: triage
    template: base
    replicas: 1
    packages: {on: true, ask: true, python: [ansible-lint]}
    fw:
      agents:
        triage:
          description: Sorts new tickets.
          access: {owners: [alice], viewers: [bob]}
`

// Without a package mirror, the routes of the packages answer 404, and
// the tools are not there.
func TestPackagesOffWithoutAMirror(t *testing.T) {
	fr := newFront(t)
	for _, method := range []string{"GET", "PUT"} {
		if resp, body := do(t, method, fr.url+"/v1/pools/triage/packages", "carol", `{"on":true}`); resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s = %d %s", method, resp.StatusCode, body)
		}
	}
	if fleet.PackagesOn(fr.a) {
		t.Fatal("PackagesOn without a mirror")
	}
	cs := connect(t, fr.a, "carol")
	list, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range list.Tools {
		if strings.HasPrefix(tool.Name, "packages_") {
			t.Fatalf("the tool %s is there without a mirror", tool.Name)
		}
	}
}

// The routes read and replace the packages of a pool: a viewer reads, the
// owner of the agent and an admin change them, each name is checked, a
// dry run writes nothing, and the answer has the shape of section 14.11.
func TestPackagesRoutes(t *testing.T) {
	fr := newFrontWith(t, packagesBlock)
	resp, body := do(t, "GET", fr.url+"/v1/pools/triage/packages", "bob", "")
	p := decode[fleet.Packages](t, body)
	if resp.StatusCode != http.StatusOK || p.Pool != "triage" || !p.Available || !p.On || !p.Ask || len(p.Python) != 1 || p.Python[0].Name != "ansible-lint" ||
		p.Tools == nil || p.Workers == nil || p.Applying {
		t.Fatalf("GET = %d %s", resp.StatusCode, body)
	}
	for _, key := range []string{`"pool":`, `"available":`, `"on":`, `"ask":`, `"python":`, `"tools":`, `"applying":`, `"workers":`} {
		if !strings.Contains(body, key) {
			t.Fatalf("the answer lacks %s: %s", key, body)
		}
	}
	if resp, body := do(t, "GET", fr.url+"/v1/pools/nope/packages", "carol", ""); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET of no pool = %d %s", resp.StatusCode, body)
	}
	put := `{"on":true,"ask":true,"python":["ansible-lint","pyyaml>=6"],"tools":["iproute2"]}`
	if resp, body := do(t, "PUT", fr.url+"/v1/pools/triage/packages", "bob", put); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("PUT of bob = %d %s", resp.StatusCode, body)
	}
	for _, c := range []struct {
		body string
		want string
	}{
		{`{"ask":true}`, "set on"},
		{`{"on":true,"python":["-r requirements.txt"]}`, "python[0] starts with a dash"},
		{`{"on":true,"tools":["` + strings.Repeat("x", 201) + `"]}`, "tools[0] is longer than 200 characters"},
		{`{"on":true,"registry":"pypi"}`, "unknown field"},
	} {
		if resp, body := do(t, "PUT", fr.url+"/v1/pools/triage/packages", "alice", c.body); resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, c.want) {
			t.Errorf("PUT %s = %d %s, want %q", c.body, resp.StatusCode, body, c.want)
		}
	}
	resp, body = do(t, "PUT", fr.url+"/v1/pools/triage/packages?dryRun=true", "alice", put)
	if p := decode[fleet.Packages](t, body); resp.StatusCode != http.StatusOK || len(p.Python) != 2 || len(p.Tools) != 1 {
		t.Fatalf("dry run = %d %s", resp.StatusCode, body)
	}
	if o, _ := fr.overlays.Read(context.Background(), "triage"); o.Packages != nil {
		t.Fatal("a dry run wrote the overlay")
	}
	resp, body = do(t, "PUT", fr.url+"/v1/pools/triage/packages", "alice", put)
	p = decode[fleet.Packages](t, body)
	if resp.StatusCode != http.StatusOK || len(p.Python) != 2 || p.Python[0].By != "" || p.Python[1].By != "alice" || p.Python[1].At == nil || p.Tools[0].Name != "iproute2" {
		t.Fatalf("PUT = %d %s", resp.StatusCode, body)
	}
	resp, body = do(t, "PUT", fr.url+"/v1/pools/triage/packages", "carol", `{"on":false,"ask":true,"python":["ansible-lint","pyyaml>=6"],"tools":["iproute2"]}`)
	p = decode[fleet.Packages](t, body)
	if resp.StatusCode != http.StatusOK || p.On || len(p.Python) != 2 || p.Python[1].By != "alice" || len(p.Workers) != 0 {
		t.Fatalf("PUT of an admin = %d %s", resp.StatusCode, body)
	}
}

// The tools read and replace the packages with the checks of the routes,
// and say the setting in the words of the page.
func TestPackagesTools(t *testing.T) {
	fr := newFrontWith(t, packagesBlock)
	alice, bob := connect(t, fr.a, "alice"), connect(t, fr.a, "bob")
	res, text := call(t, bob, ToolPackagesGet, map[string]any{"pool": "triage"})
	if res.IsError || !strings.Contains(text, "The packages of the pool triage are on.") || !strings.Contains(text, "Python packages:\n- ansible-lint") ||
		!strings.Contains(text, "Programs: none.") || !strings.Contains(text, "The agent can ask for a package. A person approves each one.") {
		t.Fatalf("packages_get = %q", text)
	}
	if res, text := call(t, bob, ToolPackagesPut, map[string]any{"pool": "triage", "on": true, "python": []string{"x"}}); !res.IsError || !strings.Contains(text, "Forbidden") {
		t.Fatalf("packages_put of bob = %q", text)
	}
	if res, text := call(t, alice, ToolPackagesPut, map[string]any{"pool": "triage", "on": true, "tools": []string{"nodejs>=22"}, "dryRun": true}); res.IsError || !strings.Contains(text, "Dry run: nothing changed.") || !strings.Contains(text, "- nodejs>=22, added by alice") {
		t.Fatalf("packages_put with dryRun = %q", text)
	}
	res, text = call(t, alice, ToolPackagesPut, map[string]any{"pool": "triage", "on": true, "ask": false, "python": []string{"ansible-lint"}, "tools": []string{"nodejs>=22"}})
	if res.IsError || !strings.Contains(text, "The agent cannot ask for a package. It can only list its packages.") || strings.Contains(text, "Dry run") {
		t.Fatalf("packages_put = %q", text)
	}
	if o, _ := fr.overlays.Read(context.Background(), "triage"); o.Packages == nil || o.Packages.Ask || len(o.Packages.Tools) != 1 {
		t.Fatalf("the overlay = %+v", o.Packages)
	}
	for _, word := range []string{"uv", "pixi", "conda", "lock file", "PyPI"} {
		if strings.Contains(text, word) {
			t.Fatalf("the text says %q: %s", word, text)
		}
	}
}
