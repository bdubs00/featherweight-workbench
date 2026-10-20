package sources

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/fleet"
)

func TestLoadDefaults(t *testing.T) {
	cfg := config(t, `
sources:
  - name: tickets
    agent: triage
    auth: {mode: hmac, secret: TICKETS_KEY}
    key: "{{.id}}"
  - name: queue
    agent: triage
    poll: {url: "https://tickets.example.com/api/new"}
    key: "{{.id}}"
    runAs: alice
    runAsKind: user
    dedupFor: 1h
`)
	list, err := Load(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("Load = %+v", list)
	}
	hook, poll := list[0], list[1]
	if hook.DedupFor != DefaultDedupFor || hook.RunAs != "source:tickets" || hook.RunAsKind != fw.KindWorkload ||
		hook.Auth.Header != DefaultSignatureHeader || hook.Kind() != "webhook" {
		t.Fatalf("webhook = %+v", hook)
	}
	if poll.DedupFor != time.Hour || poll.RunAs != "alice" || poll.RunAsKind != fw.KindUser || poll.Kind() != "poll" ||
		poll.Poll.Every != DefaultEvery || poll.Poll.Timeout != DefaultPollTimeout {
		t.Fatalf("poll = %+v %+v", poll, poll.Poll)
	}
}

func TestLoadWithoutSources(t *testing.T) {
	if list, err := Load(config(t, `teams: ["team-*"]`)); err != nil || list != nil {
		t.Fatalf("Load = %v, %v", list, err)
	}
	if list, err := Load(nil); err != nil || list != nil {
		t.Fatalf("Load(nil) = %v, %v", list, err)
	}
}

// TestLoadRefuses lists the configurations that stop the start, above all
// a webhook without a credential that does not say none.
func TestLoadRefuses(t *testing.T) {
	cases := []struct{ name, entry, want string }{
		{"no auth", `{name: a, agent: x, key: "{{.id}}"}`, "auth.mode is required"},
		{"bad mode", `{name: a, agent: x, key: "{{.id}}", auth: {mode: basic}}`, `auth.mode "basic"`},
		{"bearer without secret", `{name: a, agent: x, key: "{{.id}}", auth: {mode: bearer}}`, "auth.secret"},
		{"none with secret", `{name: a, agent: x, key: "{{.id}}", auth: {mode: none, secret: X}}`, "auth.secret is set"},
		{"no key", `{name: a, agent: x, auth: {mode: none}}`, "key is required"},
		{"no agent", `{name: a, key: "{{.id}}", auth: {mode: none}}`, "agent is required"},
		{"agent with a dot", `{name: a, agent: x.y, key: "{{.id}}", auth: {mode: none}}`, "not a name for a queue"},
		{"bad name", `{name: Tickets, agent: x, key: "{{.id}}", auth: {mode: none}}`, "the name"},
		{"bad template", `{name: a, agent: x, key: "{{.id", auth: {mode: none}}`, "does not parse"},
		{"reserved metadata", `{name: a, agent: x, key: "{{.id}}", auth: {mode: none}, metadata: {fw.batch: x}}`, "set by the source"},
		{"kernel metadata", `{name: a, agent: x, key: "{{.id}}", auth: {mode: none}, metadata: {idempotencyKey: x}}`, "reserved"},
		{"short window", `{name: a, agent: x, key: "{{.id}}", auth: {mode: none}, dedupFor: 10s}`, "dedupFor"},
		{"bad kind", `{name: a, agent: x, key: "{{.id}}", auth: {mode: none}, runAsKind: token}`, "runAsKind"},
		{"poll with auth", `{name: a, agent: x, key: "{{.id}}", auth: {mode: none}, poll: {url: "https://h/x"}}`, "auth applies to a webhook"},
		{"poll without url", `{name: a, agent: x, key: "{{.id}}", poll: {url: "/x"}}`, "poll.url"},
		{"poll with a user", `{name: a, agent: x, key: "{{.id}}", poll: {url: "https://u:p@h/x"}}`, "user or a password"},
		{"poll too often", `{name: a, agent: x, key: "{{.id}}", poll: {url: "https://h/x", every: 1s}}`, "poll.every"},
		{"poll header without secret", `{name: a, agent: x, key: "{{.id}}", poll: {url: "https://h/x", header: X-Key}}`, "poll.header"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Load(config(t, "sources:\n  - "+c.entry+"\n"))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("Load = %v, want an error with %q", err, c.want)
			}
		})
	}
	t.Run("twice", func(t *testing.T) {
		_, err := Load(config(t, "sources:\n  - {name: a, agent: x, key: k, auth: {mode: none}}\n  - {name: a, agent: x, key: k, auth: {mode: none}}\n"))
		if err == nil || !strings.Contains(err.Error(), "used twice") {
			t.Fatalf("Load = %v", err)
		}
	})
	t.Run("no intake", func(t *testing.T) {
		cfg := config(t, "sources:\n  - {name: a, agent: x, key: k, auth: {mode: none}}\n")
		t.Setenv(fleet.EnvIntake, "")
		_, err := Load(cfg)
		if err == nil || !strings.Contains(err.Error(), "workbench.fleet.intake") {
			t.Fatalf("Load = %v", err)
		}
	})
}

// TestStartNeedsTheSecret refuses to start a source whose variable holds
// no secret, and never names the value of another secret.
func TestStartNeedsTheSecret(t *testing.T) {
	cfg := config(t, `
sources:
  - {name: tickets, agent: default, key: "{{.id}}", auth: {mode: bearer, secret: TICKETS_TOKEN}}
`)
	a, err := fw.New(fw.WithConfig(cfg), fw.WithoutDefaults(), fw.WithAuthn(fw.NoAuthn{}), fw.WithModel(textModel("done")),
		fw.WithLedger(fw.NewMemLedger()), fw.WithBus(fw.NewMemBus()), fw.WithSecrets(secrets("OTHER", "s3cret")), fw.WithLogger(quiet()))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close(context.Background())
	_, err = Start(context.Background(), a)
	if err == nil || !strings.Contains(err.Error(), "TICKETS_TOKEN") || strings.Contains(err.Error(), "s3cret") {
		t.Fatalf("Start = %v", err)
	}
	cfg.Bus.Driver = "redis"
	if _, err := Start(context.Background(), a); err == nil || !strings.Contains(err.Error(), "nats bus") {
		t.Fatalf("Start with the redis bus = %v", err)
	}
}

func TestDoctorCheck(t *testing.T) {
	ctx := context.Background()
	if ok, line := DoctorCheck(ctx, config(t, `teams: []`), nil, 0); !ok || !strings.Contains(line, "sources: none") {
		t.Fatalf("doctor = %v %q", ok, line)
	}
	cfg := config(t, `
sources:
  - {name: tickets, agent: triage, key: "{{.id}}", auth: {mode: bearer, secret: TICKETS_TOKEN}}
  - {name: queue, agent: triage, key: "{{.id}}", poll: {url: "https://tickets.example.com/api?q=1", secret: QUEUE_TOKEN}}
  - {name: open, agent: triage, key: "{{.id}}", auth: {mode: none}}
`)
	cfg.Bus.Driver = "nats"
	ok, line := DoctorCheck(ctx, cfg, secrets("TICKETS_TOKEN", "a", "QUEUE_TOKEN", "b"), 0)
	if !ok || !strings.Contains(line, "tickets takes webhooks with bearer for triage") || !strings.Contains(line, "queue polls tickets.example.com every 1m0s") ||
		!strings.Contains(line, "open takes webhooks without a credential") || !strings.Contains(line, "JetStream") || strings.Contains(line, "q=1") {
		t.Fatalf("doctor = %v %q", ok, line)
	}
	if ok, line := DoctorCheck(ctx, cfg, secrets("TICKETS_TOKEN", "a"), 0); ok || !strings.Contains(line, "QUEUE_TOKEN holds no secret") {
		t.Fatalf("doctor without a secret = %v %q", ok, line)
	}
	cfg.Bus.Driver = "redis"
	if ok, line := DoctorCheck(ctx, cfg, secrets("TICKETS_TOKEN", "a", "QUEUE_TOKEN", "b"), 0); ok || !strings.Contains(line, "bus is redis") {
		t.Fatalf("doctor with redis = %v %q", ok, line)
	}
}
