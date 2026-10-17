package fleet_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/surface/metrics"
	_ "github.com/bdubs00/featherweight-workbench/fleet"
)

func newAgent(t *testing.T, cfg *fw.Config, s fw.Surface, opts ...fw.Option) *fw.Agent {
	t.Helper()
	t.Setenv("FW_ATESPACE", "")
	model := fw.ModelFunc{GenerateFn: func(context.Context, *fw.Request) (*fw.Response, error) {
		return &fw.Response{Content: []fw.Block{fw.Text("ok")}, StopReason: fw.StopEndTurn, Usage: fw.Usage{InputTokens: 3, OutputTokens: 2}}, nil
	}}
	a, err := fw.New(append([]fw.Option{
		fw.WithConfig(cfg),
		fw.WithoutDefaults(),
		fw.WithAuthn(fw.TokenAuthn{Token: "s3cret"}),
		fw.WithModel(model),
		fw.WithLedger(fw.NewMemLedger()),
		fw.WithBus(fw.NewMemBus()),
		fw.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))),
		fw.WithSurfaces(s),
	}, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		a.Close(ctx)
	})
	return a
}

func scrape(t *testing.T, h http.Handler, path string) (int, string) {
	t.Helper()
	srv := httptest.NewServer(h)
	defer srv.Close()
	resp, err := http.Get(srv.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(b)
}

// retain puts the record of another fleet member on the bus, as that member
// would publish it.
func retain(t *testing.T, bus fw.Retainer, subject string, rec fw.FleetRecord) {
	t.Helper()
	rec.Version = fw.FleetVersion
	if err := bus.Retain(context.Background(), fw.FleetRecordSubject(subject, rec.Name), fw.MustEvent(rec.Name, fw.EvFleetRecord, rec)); err != nil {
		t.Fatal(err)
	}
}

// TestFleetGauges scrapes a front that has no agents. It reports the
// members that sleep from their retained records, without a request to
// them.
func TestFleetGauges(t *testing.T) {
	const subject = "test.fleet"
	bus := fw.NewMemBus()
	now := time.Now().UTC()
	asked := now.Add(-2 * time.Hour)
	retain(t, bus, subject, fw.FleetRecord{
		Name: "default/reviewer", URL: "ax:default/reviewer", Interval: time.Second,
		Access:    fw.FleetAccess{Mode: fw.AccessModeRules, Default: "private"},
		Agents:    []fw.FleetAgent{{Name: "reviewer"}},
		UpdatedAt: now.Add(-time.Hour), // a Task that sleeps
		Runs: []*fw.Run{
			{ID: "01RUNNING", Agent: "reviewer", State: fw.StateRunning, Principal: "alice@example.com", CreatedAt: now},
			{ID: "01WAITING", Agent: "reviewer", State: fw.StateInputRequired, CreatedAt: now,
				Pending: &fw.Approval{ID: "ap1", Tool: "bash", RequestedAt: asked}},
			{ID: "01DONE", Agent: "reviewer", State: fw.StateCompleted, CreatedAt: now},
		},
	})
	retain(t, bus, subject, fw.FleetRecord{
		Name: "builder", URL: "http://builder:8080", Interval: time.Hour,
		Access:    fw.FleetAccess{Mode: fw.AccessModeExternal},
		Agents:    []fw.FleetAgent{{Name: "builder"}},
		UpdatedAt: now,
		Runs:      []*fw.Run{{ID: "01QUEUED", Agent: "builder", State: fw.StateQueued, CreatedAt: now}},
	})
	cfg := fw.DefaultConfig()
	cfg.Home = t.TempDir()
	cfg.Fleet.Subject = subject
	cfg.Fleet.Name = "front"
	a := newAgent(t, cfg, metrics.New(), fw.WithBus(bus))
	if a.Fleet() == nil {
		t.Fatal("the agent has no fleet")
	}
	h, err := a.Handler()
	if err != nil {
		t.Fatal(err)
	}
	status, body := scrape(t, h, "/metrics")
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	for _, want := range []string{
		`fw_fleet_member_up{member="front"} 1`,
		`fw_fleet_member_up{member="default/reviewer"} 0`,
		`fw_fleet_member_up{member="builder"} 1`,
		`fw_fleet_runs{member="default/reviewer",state="running"} 1`,
		`fw_fleet_runs{member="default/reviewer",state="input-required"} 1`,
		`fw_fleet_runs{member="default/reviewer",state="suspended"} 0`,
		`fw_fleet_runs{member="builder",state="queued"} 1`,
		`fw_fleet_runs{member="front",state="running"} 0`,
		`fw_fleet_member_last_seen_timestamp_seconds{member="default/reviewer"} `,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("scrape lacks %q", want)
		}
	}
	const oldest = `fw_fleet_approvals_oldest_timestamp_seconds{member="default/reviewer"} `
	_, rest, ok := strings.Cut(body, oldest)
	line, _, _ := strings.Cut(rest, "\n")
	if v, err := strconv.ParseFloat(line, 64); !ok || err != nil || int64(v) != asked.Unix() {
		t.Errorf("oldest approval = %q, want %d", line, asked.Unix())
	}
	for _, leak := range []string{"01RUNNING", "01WAITING", "alice@example.com", "ap1", "http://builder:8080"} {
		if strings.Contains(body, leak) {
			t.Errorf("the scrape holds %q", leak)
		}
	}
}
