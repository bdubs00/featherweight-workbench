package faces

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/surface/native"
	"github.com/bdubs00/featherweight-workbench/fleet"
	"github.com/bdubs00/featherweight-workbench/internal/bucket"
	"github.com/bdubs00/featherweight-workbench/pools"
	"github.com/bdubs00/featherweight-workbench/titles"
	"gopkg.in/yaml.v3"
)

// poolsBlock is a pool of one worker of the profile triage, which alice
// owns and bob views.
const poolsBlock = `
pools:
  - name: triage
    replicas: 1
    image: ghcr.io/example/fw@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
    egress:
      hosts:
        - http://nats.agents.svc.cluster.local:8080
    fw:
      model: openai/some-model
      bus: {url: "ws://nats.agents.svc.cluster.local:8080"}
      access: {admins: ["kind:token"]}
      agents:
        triage:
          description: Sorts new tickets.
          access: {owners: [alice], viewers: [bob]}
`

// pooledAgent is a front with the pool of poolsBlock and the faces in
// memory. The ax API does not answer, so no pass runs.
func pooledAgent(t *testing.T) *fw.Agent {
	t.Helper()
	t.Setenv("FW_API_TOKEN", "fleet-secret-token")
	t.Setenv(titles.EnvTitles, titles.ModeInput)
	t.Setenv("FW_ATESPACE", "")
	store := pools.NewMemOverlays()
	old := fleet.OpenOverlays
	fleet.OpenOverlays = func(context.Context, *fw.Config, *slog.Logger) (pools.OverlayStore, func(), error) {
		return store, nil, nil
	}
	t.Cleanup(func() { fleet.OpenOverlays = old })
	cfg := fw.DefaultConfig()
	cfg.Home = t.TempDir()
	cfg.Access.Default = "closed"
	cfg.Agents = map[string]fw.AgentProfile{"default": {}}
	cfg.Bus.Driver, cfg.Bus.URL = "nats", "nats://nats.agents.svc.cluster.local:4222"
	cfg.AX.Server = "127.0.0.1:1"
	var n yaml.Node
	if err := yaml.Unmarshal([]byte(poolsBlock), &n); err != nil {
		t.Fatal(err)
	}
	cfg.Extensions = map[string]yaml.Node{fleet.Block: *n.Content[0]}
	a, err := fw.New(fw.WithConfig(cfg), fw.WithoutDefaults(), fw.WithAuthn(callers),
		fw.WithModel(fw.ModelFunc{GenerateFn: func(context.Context, *fw.Request) (*fw.Response, error) { return nil, errors.New("no run") }}),
		fw.WithLedger(fw.NewMemLedger()), fw.WithBus(fw.NewMemBus()), fw.WithLogger(quiet()), fw.WithSurfaces(native.New()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		a.Close(ctx)
	})
	off := time.Duration(0)
	fc := fleet.Config{Name: "front", Egress: "fw.events", Atespaces: []string{"team"}, Interval: time.Hour, Expire: fleet.DefaultExpire, SuspendAfter: &off}
	if fc.Pools, err = fleet.LoadPools(cfg, fc); err != nil {
		t.Fatal(err)
	}
	f, err := fleet.Open(context.Background(), a, fc)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	fleet.Attach(a, f)
	t.Cleanup(func() { fleet.Attach(a, nil) })
	Attach(a, New(bucket.NewMem(bucketConfig), quiet()))
	return a
}

// The face of an agent of a pool changes with the owner role on the
// profile in the pool.
func TestCheckWithPools(t *testing.T) {
	a := pooledAgent(t)
	as := func(token string) context.Context { return fw.WithPrincipal(context.Background(), callers[token]) }
	if err := Check(as("alice"), a, "triage", fw.ActionAgentEdit); err != nil {
		t.Fatalf("the owner in the pool = %v", err)
	}
	if err := Check(as("bob"), a, "triage", fw.ActionAgentView); err != nil {
		t.Fatalf("a viewer in the pool reads = %v", err)
	}
	if err := Check(as("bob"), a, "triage", fw.ActionAgentEdit); !errors.Is(err, fw.ErrForbidden) {
		t.Fatalf("a viewer in the pool edits = %v", err)
	}
	if err := Check(as("fleet"), a, "triage", fw.ActionAgentEdit); err != nil {
		t.Fatalf("the token = %v", err)
	}
	if err := Check(as("fleet"), a, "nobody", fw.ActionAgentEdit); err == nil {
		t.Fatal("a face for an agent that no pool runs")
	}
	url := serve(t, a) + "/v1/faces/triage"
	if resp, out := do(t, "PUT", url, "alice", fox); resp.StatusCode != 200 {
		t.Fatalf("PUT of the owner in the pool = %d %s", resp.StatusCode, out)
	}
	if resp, out := do(t, "PUT", url, "bob", fox); resp.StatusCode != 403 {
		t.Fatalf("PUT of a viewer in the pool = %d %s", resp.StatusCode, out)
	}
}
