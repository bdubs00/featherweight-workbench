package sources

import (
	"context"
	"testing"

	fwconfig "github.com/bdubs00/featherweight-kernel/config"
	"github.com/bdubs00/featherweight-workbench/fleet"

	// The package teams claims the block workbench of fw.yaml.
	_ "github.com/bdubs00/featherweight-workbench/teams"
)

// TestExampleLoads reads the example of examples/sources with the loader
// of fw.yaml, so the example keeps working.
func TestExampleLoads(t *testing.T) {
	t.Setenv(fleet.EnvIntake, "")
	cfg, err := fwconfig.Load("../examples/sources/fw.yaml")
	if err != nil {
		t.Fatal(err)
	}
	list, err := Load(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].Kind() != "webhook" || list[1].Kind() != "poll" || list[1].Poll.Every.String() != "2m0s" ||
		list[0].Redrive == nil || list[0].Redrive.Cooldown.String() != "10m0s" || list[1].Redrive != nil {
		t.Fatalf("sources = %+v", list)
	}
	sec := secrets("TICKETS_WEBHOOK_KEY", "k", "ALERTS_API_TOKEN", "t")
	if ok, line := DoctorCheck(context.Background(), cfg, sec, 0); !ok {
		t.Fatalf("doctor = %q", line)
	}
	events, err := decodeEvents([]byte(`{"id": "T-1042", "updated": "2026-10-19T10:05:00Z", "summary": "The printer is on fire", "requester": {"name": "Ada"}}`), list[0].Items)
	if err != nil {
		t.Fatal(err)
	}
	src := compiled(t, list[0])
	it, err := src.render(1, events[0])
	if err != nil || it.key != "T-1042/2026-10-19T10:05:00Z" || it.req.Metadata["ticket"] != "T-1042" {
		t.Fatalf("render = %+v, %v", it, err)
	}
}
