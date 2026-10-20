package fleet

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/surface/view"
	"github.com/bdubs00/featherweight-workbench/agents"
	"github.com/bdubs00/featherweight-workbench/pools"
	v1alpha1 "github.com/google/ax/pkg/apis/v1alpha1"
	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// ownedBlock is a pool of one worker whose profile triage alice owns, with
// a second profile writer that asks before every ticket tool. The static
// token is an admin of the workers, so the front can read every run.
const ownedBlock = `
pools:
  - name: triage
    replicas: 1
    image: ghcr.io/example/fw@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
    agent: triage
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
          access: {owners: [alice]}
        writer:
          description: Writes answers.
          policy: {ask: ["tickets.*"]}
`

// envOfTask returns the value of a variable of a Task.
func envOfTask(t *v1alpha1.Task, name string) string {
	for _, e := range t.GetSpec().GetEnv() {
		if e.GetName() == name {
			return e.GetValue()
		}
	}
	return ""
}

// overlaysOf returns the store in memory of a front of a test.
func overlaysOf(t *testing.T, f *Fleet) *pools.MemOverlays {
	t.Helper()
	s, ok := f.overlays.(*pools.MemOverlays)
	if !ok {
		t.Fatalf("the store of the overlays is %T", f.overlays)
	}
	return s
}

// A profile that agent_put writes into a pool reaches every Task of the
// pool: the hash changes, and the pool replaces each idle Task with the
// merged fw.yaml, with no restart of the front.
func TestPoolProfileRollsTheTasks(t *testing.T) {
	t.Setenv("FW_ATESPACE", "team")
	t.Setenv("FW_API_TOKEN", "fleet-secret")
	t.Setenv("FRONT_TICKETS_TOKEN", "tickets-secret")
	ax := newPoolAX(t)
	tasks := newFakeTasks(t)
	front := newMember(t, fw.NewMemBus(), "front", poolFront(t, ax, strings.TrimPrefix(tasks.srv.URL, "http://"), poolBlock))
	ctx := context.Background()
	hashOf := func(name string) string { return envOfTask(ax.get("team", name), pools.EnvSpec) }
	eventually(t, "the pool is ready", func() bool {
		ps, err := front.f.Pools(ctx)
		return err == nil && len(ps) == 1 && ps[0].Ready == 2
	})
	old := hashOf("triage-0")
	idle := time.Now().Add(-time.Hour)
	tasks.set("team/triage-0", view.Status{Idle: true, IdleSince: &idle})
	tasks.set("team/triage-1", view.Status{Idle: true, IdleSince: &idle})

	at, err := PlaceOf(ctx, front.a, "triage", "")
	if err != nil || PoolOf(at) != "triage" {
		t.Fatalf("PlaceOf = %v, %v", PoolOf(at), err)
	}
	v, created, err := agents.PutIn(ctx, front.a, at, fw.AgentProfile{Name: "triage", Description: "Sorts every ticket at once."})
	if err != nil || created || v.Description != "Sorts every ticket at once." {
		t.Fatalf("PutIn = %+v, %v, %v", v, created, err)
	}
	eventually(t, "every Task runs the merged configuration", func() bool {
		h0, h1 := hashOf("triage-0"), hashOf("triage-1")
		return h0 != "" && h0 != old && h0 == h1
	})
	ax.mu.Lock()
	ws := ax.workspaces["team/triage-0-ws"]
	ax.mu.Unlock()
	if ws == nil || !strings.Contains(ws.Spec.Files[0].Content, "Sorts every ticket at once.") {
		t.Fatalf("the Workspace = %+v", ws)
	}
	if front.a.ProfileSet()["triage"].Description != "" {
		t.Fatal("the profile went into the store of the front")
	}
}

// The front writes the profiles of a pool through the checks of agent_put:
// the owner of a profile changes it, nobody else does, a new profile goes
// into a pool only by name, a profile of the configuration of the pool
// stays, and a profile of the front stays in the store of the front.
func TestPoolProfileWrites(t *testing.T) {
	t.Setenv("FW_ATESPACE", "team")
	t.Setenv("FW_API_TOKEN", "fleet-secret")
	ax := newPoolAX(t)
	tasks := newFakeTasks(t)
	front := newMember(t, fw.NewMemBus(), "front", poolFront(t, ax, strings.TrimPrefix(tasks.srv.URL, "http://"), ownedBlock))
	store := overlaysOf(t, front.f)
	alice, bob := as("alice"), as("bob")

	at, err := PlaceOf(alice, front.a, "triage", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := agents.PutIn(bob, front.a, at, fw.AgentProfile{Name: "triage", System: "Take over."}); !errors.Is(err, fw.ErrForbidden) {
		t.Fatalf("a put of bob = %v", err)
	}
	if _, _, err := agents.PutIn(alice, front.a, at, fw.AgentProfile{Name: "triage", System: "Sort with care."}); err != nil {
		t.Fatal(err)
	}
	o, _ := store.Read(context.Background(), "triage")
	if e, ok := o.Profile("triage"); !ok || e.By != "alice" || e.Profile.System != "Sort with care." || e.Profile.Access == nil || e.Profile.Access.Owners[0] != "alice" {
		t.Fatalf("the overlay = %+v", o)
	}
	pa, ok, err := PoolAgentNamed(alice, front.a, "triage", "")
	if err != nil || !ok || pa.Pool != "triage" || pa.Overlay != OverlayReplaced || pa.By != "alice" || pa.Role != "owner" || pa.System != "Sort with care." {
		t.Fatalf("PoolAgentNamed = %+v, %v, %v", pa, ok, err)
	}
	if pa.WorkerPolicy == nil || pa.WorkerPolicy.Ask[0] != "tickets.create_ticket" {
		t.Fatalf("the policy of the workers = %+v", pa.WorkerPolicy)
	}
	list, on, err := PoolAgents(bob, front.a)
	if err != nil || !on || len(list) != 1 || list[0].Name != "writer" || list[0].Role != "operator" {
		t.Fatalf("PoolAgents of bob = %+v, %v, %v", list, on, err)
	}

	// A new name needs the pool, and the error names the pools.
	if _, err := PlaceOf(alice, front.a, "helper", ""); !errors.Is(err, fw.ErrInvalidRequest) || !strings.Contains(err.Error(), "triage (agent triage)") {
		t.Fatalf("PlaceOf of a new name = %v", err)
	}
	if _, err := PlaceOf(alice, front.a, "helper", "nope"); err == nil || !strings.Contains(err.Error(), "no pool is named nope") {
		t.Fatalf("PlaceOf of an unknown pool = %v", err)
	}
	at, _ = PlaceOf(alice, front.a, "helper", "triage")
	if _, err := agents.PreviewIn(alice, at, fw.AgentProfile{Name: "helper", Model: "nowhere"}); !errors.Is(err, fw.ErrInvalidRequest) || !strings.Contains(err.Error(), "agents.helper.model") {
		t.Fatalf("a preview of a bad profile = %v", err)
	}
	d, err := agents.PreviewIn(alice, at, fw.AgentProfile{Name: "helper"})
	if err != nil || d.Exists || d.Role != "owner" {
		t.Fatalf("PreviewIn = %+v, %v", d, err)
	}
	if o, _ := store.Read(context.Background(), "triage"); len(o.Profiles) != 1 {
		t.Fatalf("a preview wrote the overlay: %+v", o)
	}
	if _, created, err := agents.PutIn(alice, front.a, at, fw.AgentProfile{Name: "helper"}); err != nil || !created {
		t.Fatalf("PutIn of a new profile = %v, %v", created, err)
	}

	// A delete takes the entry of the overlay away, and a profile of the
	// configuration of the pool stays.
	at, _ = PlaceOf(alice, front.a, "triage", "")
	if err := agents.DeleteIn(alice, front.a, at, "triage"); err != nil {
		t.Fatal(err)
	}
	at, _ = PlaceOf(alice, front.a, "triage", "")
	if err := agents.DeleteIn(alice, front.a, at, "triage"); !errors.Is(err, fw.ErrInvalidRequest) || !strings.Contains(err.Error(), "configuration of the pool triage defines") {
		t.Fatalf("a delete of the profile of the configuration = %v", err)
	}
	if o, _ := store.Read(context.Background(), "triage"); len(o.Profiles) != 1 || o.Profiles[0].Profile.Name != "helper" {
		t.Fatalf("the overlay after the deletes = %+v", o)
	}

	// The front keeps its own profile default.
	if at, err := PlaceOf(alice, front.a, "default", ""); err != nil || PoolOf(at) != "" {
		t.Fatalf("PlaceOf of a profile of the front = %v, %v", PoolOf(at), err)
	}
}

// Without pools, every profile lives in the store of the process, and the
// tools of the pools say why they do not apply.
func TestPoolProfilesWithoutPools(t *testing.T) {
	front := newMember(t, fw.NewMemBus(), "front", nil)
	ctx := context.Background()
	if at, err := PlaceOf(ctx, front.a, "anything", ""); err != nil || PoolOf(at) != "" {
		t.Fatalf("PlaceOf = %v, %v", at, err)
	}
	if _, err := PlaceOf(ctx, front.a, "anything", "triage"); !errors.Is(err, fw.ErrInvalidRequest) {
		t.Fatalf("PlaceOf with a pool = %v", err)
	}
	if list, on, err := PoolAgents(ctx, front.a); on || list != nil || err != nil {
		t.Fatalf("PoolAgents = %v, %v, %v", list, on, err)
	}
	if _, _, err := AllowRule(ctx, front.a, "01RUN", "01APP", ""); !errors.Is(err, ErrNoPool) {
		t.Fatalf("AllowRule = %v", err)
	}
}

// An approval of a run of a worker becomes an allow rule of its pool: the
// owner of the profile writes it, with the subject, the time, and the run,
// and the merged policy allows the tool. A rule that cannot loosen the ask
// is refused before anything is written.
func TestAllowRuleFromAnApproval(t *testing.T) {
	t.Setenv("FW_ATESPACE", "team")
	t.Setenv("FW_API_TOKEN", "fleet-secret")
	ax := newPoolAX(t)
	tasks := newFakeTasks(t)
	front := newMember(t, fw.NewMemBus(), "front", poolFront(t, ax, strings.TrimPrefix(tasks.srv.URL, "http://"), ownedBlock))
	store := overlaysOf(t, front.f)
	eventually(t, "the pool made its Task", func() bool { return ax.get("team", "triage-0") != nil })
	now := time.Now()
	ask := func(id, agent, tool, reason string) *fw.Run {
		return &fw.Run{ID: id, Agent: agent, State: fw.StateInputRequired, Principal: "carol", CreatedAt: now,
			Pending: &fw.Approval{ID: id + "-ask", Tool: tool, Reason: reason, Input: []byte(`{"title":"x"}`)}}
	}
	inject(front.f, "team/triage-0", "ax:team/triage-0", "team", MemberLive, now,
		ask("01TRIAGE", "triage", "tickets.create_ticket", "rule: ask tickets.create_ticket"),
		ask("01WRITER", "writer", "tickets.create_ticket", "rule: ask tickets.*"),
		ask("01FLAG", "triage", "files", "rule: ask destructive"))

	if _, _, err := AllowRule(as("bob"), front.a, "01TRIAGE", "01TRIAGE-ask", ""); !errors.Is(err, fw.ErrForbidden) {
		t.Fatalf("a rule of bob = %v", err)
	}
	alice := as("alice")
	if _, _, err := AllowRule(alice, front.a, "01TRIAGE", "01OTHER", ""); !errors.Is(err, fw.ErrInvalidRequest) {
		t.Fatalf("a rule for another approval = %v", err)
	}
	if _, _, err := AllowRule(alice, front.a, "01FLAG", "01FLAG-ask", ""); !errors.Is(err, fw.ErrInvalidRequest) || !strings.Contains(err.Error(), "flag destructive") {
		t.Fatalf("a rule against a flag = %v", err)
	}
	// The process itself owns writer; its own policy asks before every
	// ticket tool, so a rule of the pool changes nothing.
	if _, _, err := AllowRule(context.Background(), front.a, "01WRITER", "01WRITER-ask", ""); !errors.Is(err, fw.ErrInvalidRequest) || !strings.Contains(err.Error(), "tickets.*") {
		t.Fatalf("a rule under an ask of the profile = %v", err)
	}
	if o, _ := store.Read(context.Background(), "triage"); !o.Empty() {
		t.Fatalf("a refused rule wrote the overlay: %+v", o)
	}

	pool, rule, err := AllowRule(alice, front.a, "01TRIAGE", "01TRIAGE-ask", "the tickets are reviewed later")
	if err != nil || pool != "triage" || rule != "tickets.create_ticket" {
		t.Fatalf("AllowRule = %q, %q, %v", pool, rule, err)
	}
	o, _ := store.Read(context.Background(), "triage")
	if len(o.Allow) != 1 || o.Allow[0].By != "alice" || o.Allow[0].Run != "01TRIAGE" || o.Allow[0].Agent != "triage" || o.Allow[0].Note != "the tickets are reviewed later" || o.Allow[0].At.IsZero() {
		t.Fatalf("the rule = %+v", o.Allow)
	}
	merged, err := front.f.mergedPool(context.Background(), front.f.platform.pools[0])
	if err != nil {
		t.Fatal(err)
	}
	if d := merged.Decide("triage", "tickets.create_ticket", nil); d.Verdict != fw.VerdictAllow {
		t.Fatalf("the merged policy = %+v", d)
	}
}

// The store on NATS keeps each entry of an overlay in the bucket of its
// pool, makes the bucket at the first write, and reads a pool without a
// bucket as an empty overlay.
func TestNATSOverlays(t *testing.T) {
	s, err := server.NewServer(&server.Options{Host: "127.0.0.1", Port: -1, JetStream: true, StoreDir: t.TempDir(), NoLog: true, NoSigs: true})
	if err != nil {
		t.Fatal(err)
	}
	go s.Start()
	t.Cleanup(s.Shutdown)
	if !s.ReadyForConnections(5 * time.Second) {
		t.Fatal("the NATS server did not start")
	}
	nc, err := nats.Connect(s.ClientURL())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(nc.Close)
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatal(err)
	}
	store := newNATSOverlays(&natsServer{nc: nc, js: js, stream: "FW", log: quietLogger()})
	ctx := context.Background()
	if o, err := store.Read(ctx, "triage"); err != nil || !o.Empty() {
		t.Fatalf("Read without a bucket = %+v, %v", o, err)
	}
	if ok, err := store.DeleteProfile(ctx, "triage", "x"); ok || err != nil {
		t.Fatalf("DeleteProfile without a bucket = %v, %v", ok, err)
	}
	at := time.Unix(1_700_000_000, 0).UTC()
	for _, name := range []string{"writer", "helper.v2"} {
		if err := store.PutProfile(ctx, "triage", pools.OverlayProfile{Profile: fw.AgentProfile{Name: name, System: "for " + name}, By: "alice", At: at}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.AddRule(ctx, "triage", pools.OverlayRule{Rule: "github.create_pull_request", By: "alice", At: at}); err != nil {
		t.Fatal(err)
	}
	if err := store.AddRule(ctx, "triage", pools.OverlayRule{Rule: "github.create_pull_request", By: "bob", At: at}); err != nil {
		t.Fatal(err)
	}
	o, err := store.Read(ctx, "triage")
	if err != nil || len(o.Profiles) != 2 || o.Profiles[0].Profile.Name != "helper.v2" || o.Profiles[1].Profile.System != "for writer" ||
		len(o.Allow) != 1 || o.Allow[0].By != "bob" || !o.Allow[0].At.Equal(at) {
		t.Fatalf("Read = %+v, %v", o, err)
	}
	if ok, err := store.DeleteProfile(ctx, "triage", "writer"); !ok || err != nil {
		t.Fatalf("DeleteProfile = %v, %v", ok, err)
	}
	if o, _ := store.Read(ctx, "triage"); len(o.Profiles) != 1 {
		t.Fatalf("Read after the delete = %+v", o)
	}
	if _, err := js.KeyValue(ctx, "FW_POOL_triage"); err != nil {
		t.Fatalf("the bucket: %v", err)
	}
	if o, err := store.Read(ctx, "other"); err != nil || !o.Empty() {
		t.Fatalf("Read of another pool = %+v, %v", o, err)
	}
}
