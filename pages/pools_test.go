package pages

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/surface/native"
	"github.com/bdubs00/featherweight-workbench/fleet"
	"github.com/bdubs00/featherweight-workbench/pools"
	"github.com/bdubs00/featherweight-workbench/titles"
	"gopkg.in/yaml.v3"
)

// poolsBlock is a pool of one worker of the profile triage, which alice
// owns, bob operates, and carol views.
const poolsBlock = `
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
      agents:
        triage:
          description: Sorts new tickets.
          access: {owners: [alice], operators: [bob], viewers: [carol]}
`

// pooledAgent is a front with the pool of poolsBlock and the wiki in
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
	Attach(a, memStore(Config{}))
	return a
}

// The roles on the agents of a pool count: the owner of a profile of a
// pool writes the wiki, and an operator reads it, with no role on the
// agents of the front.
func TestAllowedWithPools(t *testing.T) {
	a := pooledAgent(t)
	as := func(token string) context.Context { return fw.WithPrincipal(context.Background(), callers[token]) }
	cases := []struct {
		token       string
		read, write bool
	}{{"alice", true, true}, {"bob", true, false}, {"carol", false, false}, {"fleet", true, true}}
	for _, c := range cases {
		if err := Allowed(as(c.token), a, false); (err == nil) != c.read {
			t.Errorf("%s reads: %v", c.token, err)
		}
		err := Allowed(as(c.token), a, true)
		if (err == nil) != c.write {
			t.Errorf("%s writes: %v", c.token, err)
		}
		if err != nil && !errors.Is(err, fw.ErrForbidden) {
			t.Errorf("%s: the refusal %v is not ErrForbidden", c.token, err)
		}
	}
}
