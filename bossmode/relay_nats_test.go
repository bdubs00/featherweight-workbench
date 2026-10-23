package bossmode

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	_ "github.com/bdubs00/featherweight-kernel/bus/nats"
	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/surface/native"
	"github.com/bdubs00/featherweight-workbench/fleet"
	"github.com/nats-io/nats-server/v2/server"
)

// On a NATS bus, as the replicas of the front run on the homelab, an entry
// of a channel that one replica publishes reaches the ring of the other
// over the subject of the relay, whose prefix is the egress stream. Each
// replica has a bus of its own, so no hook of one process feeds the other.
func TestRelayOnNATS(t *testing.T) {
	srv, err := server.NewServer(&server.Options{Host: "127.0.0.1", Port: -1, JetStream: true, StoreDir: t.TempDir(), NoLog: true, NoSigs: true})
	if err != nil {
		t.Fatal(err)
	}
	go srv.Start()
	t.Cleanup(srv.Shutdown)
	if !srv.ReadyForConnections(5 * time.Second) {
		t.Fatal("the NATS server did not start")
	}
	t.Setenv("FW_ATESPACE", "")
	t.Setenv(EnvEnabled, "on")
	t.Setenv(fleet.EnvEgress, "fw.events")
	replica := func() *fw.Agent {
		cfg := fw.DefaultConfig()
		cfg.Home = t.TempDir()
		cfg.Bus.Driver, cfg.Bus.URL, cfg.Bus.Egress = "nats", srv.ClientURL(), "fw.events"
		cfg.Agents = map[string]fw.AgentProfile{"default": {}}
		a, err := fw.New(fw.WithConfig(cfg), fw.WithoutDefaults(), fw.WithAuthn(people{}),
			fw.WithModel(fw.ModelFunc{GenerateFn: func(context.Context, *fw.Request) (*fw.Response, error) {
				return &fw.Response{Content: []fw.Block{fw.Text("done")}, StopReason: fw.StopEndTurn}, nil
			}}),
			fw.WithLedger(fw.NewMemLedger()), fw.WithSurfaces(native.New()),
			fw.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
		if err != nil {
			t.Fatal(err)
		}
		f, err := fleet.Open(context.Background(), a, fleet.Config{Name: "front", Egress: "fw.events", Interval: time.Hour, Expire: fleet.DefaultExpire})
		if err != nil {
			t.Fatal(err)
		}
		fleet.Attach(a, f)
		cl, err := Start(context.Background(), a)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_ = cl.Close()
			fleet.Attach(a, nil)
			_ = f.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			a.Close(ctx)
		})
		return a
	}
	one, two := replica(), replica()
	if Of(one).prefix != "fw.events.bossmode" {
		t.Fatalf("the prefix = %q", Of(one).prefix)
	}
	card := map[string]any{"bridge": "b1", "message": map[string]any{"seq": 39, "event": "needs-you", "decision": map[string]any{"run": "r1", "id": "ap1", "state": "pending"}}}
	if err := PublishTo(one, KindBridgeMessage, "b1", card, ForBridge("b1")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, Of(two), KindBridgeMessage, 1)
	if n := len(ofKind(Of(one), KindBridgeMessage)); n != 1 {
		t.Fatalf("the replica that published holds %d entries", n)
	}
}
