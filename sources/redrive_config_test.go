package sources

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
)

func TestRedriveConfiguration(t *testing.T) {
	for _, tc := range []struct{ driver, extra, want string }{
		{"nats", "redrive: {}", ""},
		{"mem", "redrive: {}", "needs bus.driver nats"},
		{"nats", "redrive: {maxAttempts: 1}", "redrive.maxAttempts"},
		{"nats", "redrive: {maxAttempts: 11}", "redrive.maxAttempts"},
		{"nats", "redrive: {cooldown: 59s}", "redrive.cooldown"},
		{"nats", "redrive: {cooldown: 1h}, dedupFor: 1h", "redrive.cooldown"},
		{"nats", "metadata: {workbench.sourceGeneration: forged}", "set by the source"},
	} {
		cfg := config(t, `sources:
 - {name: tickets, agent: triage, key: "{{.id}}", auth: {mode: none}, `+tc.extra+`}`)
		cfg.Bus.Driver = tc.driver
		list, err := Load(cfg)
		if tc.want != "" {
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("%s: %v", tc.extra, err)
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		r := list[0].Redrive
		if r == nil || r.MaxAttempts != 3 || r.Cooldown != 5*time.Minute {
			t.Fatalf("defaults=%+v", r)
		}
	}
	cfg := config(t, `sources:
 - {name: tickets, agent: triage, key: "{{.id}}", auth: {mode: none}}`)
	list, err := Load(cfg)
	if err != nil || list[0].Redrive != nil {
		t.Fatalf("default must stay disabled: %+v %v", list, err)
	}
}

func TestRedriveReservesGenerationMetadata(t *testing.T) {
	cfg := withDefaults(Source{Name: "tickets", Agent: "triage", Key: "{{.id}}", Redrive: &Redrive{}, Metadata: map[string]string{}})
	for i := range 61 {
		cfg.Metadata[fmt.Sprintf("field%d", i)] = "value"
	}
	tpl, err := compileTemplates(cfg)
	if err != nil {
		t.Fatal(err)
	}
	src := &source{cfg: cfg, tpl: tpl}
	if _, err := src.render(1, map[string]any{"id": "42"}); err == nil || !strings.Contains(err.Error(), "no room") {
		t.Fatalf("render=%v", err)
	}
}

func TestRedriveLegacyClaimsFailClosed(t *testing.T) {
	srv := runNATS(t)
	c := &clock{t: time.Now()}
	n := redriveStore(t, srv.ClientURL(), c)
	delete(n.redrives, "tickets")
	tk, err := n.Take(t.Context(), "tickets", "legacy")
	if err != nil {
		t.Fatal(err)
	}
	if err := n.Finish(t.Context(), tk); err != nil {
		t.Fatal(err)
	}
	n.redrives["tickets"] = Redrive{MaxAttempts: 3, Cooldown: time.Minute}
	if err := n.recordFailure(t.Context(), redriveSource(), deadRecord(t, redriveSource(), tk, nil)); err != nil {
		t.Fatal(err)
	}
	c.add(10 * time.Minute)
	if _, err := n.Take(t.Context(), "tickets", "legacy"); err != ErrDuplicate {
		t.Fatalf("legacy=%v", err)
	}
}

// A pending claim from before redrive keeps the takeover of Take, as the
// first attempt, and a claim that redrive released does not block a source
// that turned redrive off.
func TestRedriveLegacyPendingAndReleased(t *testing.T) {
	srv := runNATS(t)
	c := &clock{t: time.Now()}
	n := redriveStore(t, srv.ClientURL(), c)
	ctx := t.Context()
	delete(n.redrives, "tickets")
	if _, err := n.Take(ctx, "tickets", "stuck"); err != nil {
		t.Fatal(err)
	}
	n.redrives["tickets"] = Redrive{MaxAttempts: 3, Cooldown: time.Minute}
	if _, err := n.Take(ctx, "tickets", "stuck"); !errors.Is(err, ErrInFlight) {
		t.Fatalf("young legacy claim = %v", err)
	}
	c.add(staleClaim + time.Second)
	tk, err := n.Take(ctx, "tickets", "stuck")
	if err != nil {
		t.Fatal(err)
	}
	if tk.generation == "" || tk.attempt != 1 {
		t.Fatalf("stale legacy claim = %+v", tk)
	}
	if err := n.Release(ctx, tk); err != nil {
		t.Fatal(err)
	}
	if got := readClaim(t, n, "stuck"); got.State != stateReleased {
		t.Fatalf("release = %+v", got)
	}
	delete(n.redrives, "tickets")
	again, err := n.Take(ctx, "tickets", "stuck")
	if err != nil {
		t.Fatalf("released claim without redrive = %v", err)
	}
	if err := n.Finish(ctx, again); err != nil {
		t.Fatal(err)
	}
	if _, err := n.Take(ctx, "tickets", "stuck"); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("done = %v", err)
	}
}

// The consumer of the dead letter of a source has a name of its own: the
// same for every replica of the source, another for another source or
// queue, and never the name of the consumer of a worker intake.
func TestRedriveDurableName(t *testing.T) {
	a := redriveSource()
	b := redriveSource()
	if redriveDurable(a) != redriveDurable(b) {
		t.Fatal("two replicas of one source got two consumers")
	}
	other := redriveSource()
	other.cfg.Name = "alerts"
	queue := redriveSource()
	queue.queue = "fw.jobs.oncall"
	for _, s := range []*source{other, queue} {
		if redriveDurable(s) == redriveDurable(a) {
			t.Fatalf("%s on %s shares the consumer of tickets", s.cfg.Name, s.queue)
		}
	}
	if name := redriveDurable(a); strings.HasPrefix(name, "fw-") || strings.ContainsAny(name, ".*> ") {
		t.Fatalf("durable %q", name)
	}
}

// A store that does not answer keeps the record of the dead letter: the
// consumer does not acknowledge it, and the claim stays as it was.
func TestRedriveStoreErrorKeepsRecord(t *testing.T) {
	srv := runNATS(t)
	c := &clock{t: time.Now()}
	n := redriveStore(t, srv.ClientURL(), c)
	src := redriveSource()
	tk, err := n.Take(t.Context(), "tickets", "9")
	if err != nil {
		t.Fatal(err)
	}
	if err := n.Finish(t.Context(), tk); err != nil {
		t.Fatal(err)
	}
	gone, cancel := context.WithCancel(t.Context())
	cancel()
	if err := n.recordFailure(gone, src, deadRecord(t, src, tk, nil)); err == nil {
		t.Fatal("a store error counted as handled")
	}
	if got := readClaim(t, n, "9"); got.State != stateDone {
		t.Fatalf("claim = %+v", got)
	}
}

// A record that does not read is skipped with one line a minute that names
// the source and nothing of the record.
func TestRedriveMalformedRecordLog(t *testing.T) {
	srv := runNATS(t)
	c := &clock{t: time.Now()}
	n := redriveStore(t, srv.ClientURL(), c)
	logs := &logBuffer{}
	n.log = slog.New(slog.NewTextHandler(logs, nil))
	src := redriveSource()
	other, err := json.Marshal(fw.Event{Type: fw.EvRunFinished, Data: json.RawMessage(`{"secret":"ticket text"}`)})
	if err != nil {
		t.Fatal(err)
	}
	for _, data := range [][]byte{[]byte("ticket text"), other, []byte("{")} {
		if err := n.recordFailure(t.Context(), src, data); err != nil {
			t.Fatal(err)
		}
	}
	out := logs.String()
	if strings.Count(out, "does not read") != 1 || !strings.Contains(out, "source=tickets") || strings.Contains(out, "ticket text") {
		t.Fatalf("logs = %q", out)
	}
}
