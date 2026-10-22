package bossmode

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/surface/native"
	"github.com/bdubs00/featherweight-workbench/fleet"
	"github.com/bdubs00/featherweight-workbench/internal/bucket"
	"github.com/bdubs00/featherweight-workbench/pools"
	"gopkg.in/yaml.v3"
)

// templatesBlock holds two worker templates, of which fast is the
// default.
const templatesBlock = `
templates:
  - name: general
    image: ghcr.io/example/fw@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
    egress:
      hosts:
        - http://nats.agents.svc.cluster.local:8080
    fw:
      bus: {url: "ws://nats.agents.svc.cluster.local:8080"}
      access: {admins: ["kind:token"]}
  - name: fast
    default: true
    image: ghcr.io/example/fw@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
    egress:
      hosts:
        - http://nats.agents.svc.cluster.local:8080
    fw:
      model: openai/some-model
      bus: {url: "ws://nats.agents.svc.cluster.local:8080"}
      access: {admins: ["kind:token"]}
`

// The settings name the worker templates of the front, with the default,
// so the page offers the choice when it creates an agent.
func TestSettingsNameTheTemplates(t *testing.T) {
	t.Setenv("FW_ATESPACE", "")
	t.Setenv(EnvEnabled, "on")
	t.Setenv("FW_API_TOKEN", "fleet-secret-token")
	oldO, oldR := fleet.OpenOverlays, fleet.OpenRuntime
	fleet.OpenOverlays = func(context.Context, *fw.Config, *slog.Logger) (pools.OverlayStore, func(), error) {
		return pools.NewMemOverlays(), nil, nil
	}
	fleet.OpenRuntime = func(context.Context, *fw.Config, *slog.Logger, bool) (*pools.Store, func(), error) {
		return pools.NewStore(bucket.NewMem(pools.StoreBucket), nil), nil, nil
	}
	t.Cleanup(func() { fleet.OpenOverlays, fleet.OpenRuntime = oldO, oldR })
	cfg := fw.DefaultConfig()
	cfg.Home = t.TempDir()
	cfg.Agents = map[string]fw.AgentProfile{"default": {}}
	cfg.Bus.Driver, cfg.Bus.URL = "nats", "nats://nats.agents.svc.cluster.local:4222"
	cfg.AX.Server, cfg.AX.Router = "127.0.0.1:1", "127.0.0.1:1"
	var n yaml.Node
	if err := yaml.Unmarshal([]byte(templatesBlock), &n); err != nil {
		t.Fatal(err)
	}
	cfg.Extensions = map[string]yaml.Node{fleet.Block: *n.Content[0]}
	a, err := fw.New(fw.WithConfig(cfg), fw.WithoutDefaults(), fw.WithAuthn(people{}),
		fw.WithModel(fw.ModelFunc{GenerateFn: func(context.Context, *fw.Request) (*fw.Response, error) { return nil, context.Canceled }}),
		fw.WithLedger(fw.NewMemLedger()), fw.WithBus(fw.NewMemBus()), fw.WithSurfaces(native.New()),
		fw.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
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
	if fc.Pools, fc.Templates, err = fleet.LoadAll(cfg, fc); err != nil {
		t.Fatal(err)
	}
	f, err := fleet.Open(context.Background(), a, fc)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	fleet.Attach(a, f)
	t.Cleanup(func() { fleet.Attach(a, nil) })
	h, err := a.Handler()
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	resp, body := get(t, srv.URL+"/v1/bossmode", "ada")
	var got Settings
	if resp.StatusCode != http.StatusOK || json.Unmarshal([]byte(body), &got) != nil {
		t.Fatalf("settings: %d %s", resp.StatusCode, body)
	}
	if len(got.Templates) != 2 || got.Templates[0] != (Template{Name: "general"}) || got.Templates[1] != (Template{Name: "fast", Default: true}) {
		t.Fatalf("templates = %s", body)
	}
}
