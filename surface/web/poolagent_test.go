package web

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/fleet"
	"github.com/bdubs00/featherweight-workbench/pools"
	"gopkg.in/yaml.v3"
)

// poolAgentsBlock holds the pool triage, whose workers run the profile
// triage, which alice owns. The policy of the workers asks before a ticket
// is created.
const poolAgentsBlock = `
pools:
  - name: triage
    replicas: 1
    image: ghcr.io/example/fw@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
    egress:
      rules:
        - http: {hostnames: [nats.agents.svc.cluster.local], ports: [8080]}
    fw:
      model: openai/some-model
      bus: {url: "ws://nats.agents.svc.cluster.local:8080"}
      access: {admins: ["kind:token"]}
      policy: {default: allow, ask: [tickets.create_ticket]}
      agents:
        triage:
          description: Sorts new tickets.
          tools: [tickets]
          access: {owners: [alice]}
`

// GET /v1/agents/{name} of a profile that the workers of a pool run answers
// that profile, as agent_get reads it, through the relay of the fleet
// (native.AgentRelay): with pool, the role of the caller, and the policy
// of the workers, and with no abilities. A caller who cannot view it gets
// a 404, also when the front holds a profile of the same name that the
// caller can view, and a profile of the front alone keeps the answer of
// the kernel.
func TestGetAgentOfAPool(t *testing.T) {
	t.Setenv("FW_API_TOKEN", "fleet-secret-token")
	overlays := pools.NewMemOverlays()
	old := fleet.OpenOverlays
	fleet.OpenOverlays = func(context.Context, *fw.Config, *slog.Logger) (pools.OverlayStore, func(), error) {
		return overlays, nil, nil
	}
	t.Cleanup(func() { fleet.OpenOverlays = old })
	cfg := fw.DefaultConfig()
	cfg.Home = t.TempDir()
	cfg.Access.Default = "closed"
	cfg.Agents = map[string]fw.AgentProfile{
		"default": {Access: &fw.Access{Viewers: []string{"alice", "bob"}}},
		"triage":  {Description: "The copy of the front.", Access: &fw.Access{Viewers: []string{"alice", "bob"}}},
	}
	cfg.Bus.Driver, cfg.Bus.URL = "nats", "nats://nats.agents.svc.cluster.local:4222"
	cfg.AX.Server, cfg.AX.Router = "127.0.0.1:1", "127.0.0.1:1"
	var n yaml.Node
	if err := yaml.Unmarshal([]byte(poolAgentsBlock), &n); err != nil {
		t.Fatal(err)
	}
	cfg.Extensions = map[string]yaml.Node{fleet.Block: *n.Content[0]}
	a := newAgent(t, newScript().model(), fw.WithConfig(cfg), fw.WithAuthn(people))
	srv := serve(t, a)

	// Without the fleet the kernel answers with the profile of the front.
	resp, b := do(t, "GET", srv.URL+"/v1/agents/triage", "", "Authorization", asAlice)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(b), "The copy of the front.") || strings.Contains(string(b), `"pool"`) {
		t.Fatalf("without the fleet = %d %s", resp.StatusCode, b)
	}

	off := time.Duration(0)
	fc := fleet.Config{Name: "front", Egress: "fw.events", Atespaces: []string{"team"}, Interval: time.Hour, Expire: fleet.DefaultExpire, SuspendAfter: &off}
	list, err := fleet.LoadPools(cfg, fc)
	if err != nil {
		t.Fatal(err)
	}
	fc.Pools = list
	f, err := fleet.Open(context.Background(), a, fc)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	fleet.Attach(a, f)
	t.Cleanup(func() { fleet.Attach(a, nil) })

	resp, b = do(t, "GET", srv.URL+"/v1/agents/triage", "", "Authorization", asAlice)
	var got fleet.PoolAgent
	decodeJSON(t, resp, b, http.StatusOK, &got)
	if got.Name != "triage" || got.Pool != "triage" || got.Role != "owner" || got.Description != "Sorts new tickets." ||
		len(got.Tools) != 1 || got.Tools[0] != "tickets" || got.WorkerPolicy == nil || len(got.WorkerPolicy.Ask) != 1 {
		t.Fatalf("the profile of the pool = %s", b)
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(b, &keys); err != nil || keys["abilities"] != nil || keys["front"] != nil {
		t.Fatalf("the profile of the pool has abilities or front: %s", b)
	}
	if resp, b := do(t, "GET", srv.URL+"/v1/agents/triage?pool=triage", "", "Authorization", asAlice); resp.StatusCode != http.StatusOK {
		t.Fatalf("with the pool named = %d %s", resp.StatusCode, b)
	}
	resp, b = do(t, "GET", srv.URL+"/v1/agents/triage?pool=nope", "", "Authorization", asAlice)
	decodeProblem(t, resp, b, http.StatusNotFound)

	// Bob cannot view the profile in the pool, so he gets a 404, and the
	// copy of the front does not stand in for it.
	resp, b = do(t, "GET", srv.URL+"/v1/agents/triage", "", "Authorization", asBob)
	if p := decodeProblem(t, resp, b, http.StatusNotFound); !strings.Contains(p.Detail, `"triage"`) {
		t.Fatalf("bob = %+v", p)
	}
	// A request without a credential gets the 401 of the kernel.
	resp, b = do(t, "GET", srv.URL+"/v1/agents/triage", "")
	decodeProblem(t, resp, b, http.StatusUnauthorized)

	// A profile of the front itself keeps the answer of the kernel.
	resp, b = do(t, "GET", srv.URL+"/v1/agents/default", "", "Authorization", asBob)
	if resp.StatusCode != http.StatusOK || strings.Contains(string(b), `"pool"`) || !strings.Contains(string(b), `"name":"default"`) {
		t.Fatalf("the profile of the front = %d %s", resp.StatusCode, b)
	}
	resp, b = do(t, "GET", srv.URL+"/v1/agents/nope", "", "Authorization", asAlice)
	decodeProblem(t, resp, b, http.StatusNotFound)
	// The other methods of the path stay with the kernel and this package.
	resp, b = do(t, "POST", srv.URL+"/v1/agents/triage", "{}", "Authorization", asAlice)
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST = %d %s", resp.StatusCode, b)
	}
}
