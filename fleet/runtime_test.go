package fleet

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/surface/view"
	"github.com/bdubs00/featherweight-workbench/agents"
	"github.com/bdubs00/featherweight-workbench/internal/bucket"
	"github.com/bdubs00/featherweight-workbench/pools"
	"gopkg.in/yaml.v3"
)

// templateBlock is a block workbench with the default template base,
// whose workers reach the bus and run a model behind a proxy.
const templateBlock = `
templates:
  - name: base
    default: true
    image: ghcr.io/example/fw@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
    egress:
      rules:
        - http: {hostnames: [nats.agents.svc.cluster.local], ports: [8080]}
    fw:
      model: openai/some-model
      bus: {url: "ws://nats.agents.svc.cluster.local:8080"}
      access: {admins: ["kind:token"]}
`

// useMemRuntime makes the fronts of a test keep their runtime pools in one
// store in memory, which the test reads too.
func useMemRuntime(t *testing.T) *pools.Store {
	t.Helper()
	store := pools.NewStore(bucket.NewMem(pools.StoreBucket), nil)
	old := OpenRuntime
	OpenRuntime = func(context.Context, *fw.Config, *slog.Logger, bool) (*pools.Store, func(), error) {
		return store, nil, nil
	}
	t.Cleanup(func() { OpenRuntime = old })
	return store
}

// runtimeFront configures a front with the pools and the templates of
// block, over the ax API and the router of a test. creators and admins set
// the access of the front.
func runtimeFront(t *testing.T, ax *poolAX, router, block string, admins, creators []string) func(*fw.Config, *Config) {
	t.Helper()
	useMemOverlays(t)
	return func(cfg *fw.Config, fc *Config) {
		off := time.Duration(0)
		fc.Atespaces = []string{"team"}
		fc.Interval = 50 * time.Millisecond
		fc.SuspendAfter = &off
		cfg.AX.Server, cfg.AX.Router = ax.addr, router
		cfg.Bus.Driver, cfg.Bus.URL = "nats", "nats://nats.agents.svc.cluster.local:4222"
		cfg.Access.Admins, cfg.Access.Creators = admins, creators
		var n yaml.Node
		if err := yaml.Unmarshal([]byte(block), &n); err != nil {
			t.Fatal(err)
		}
		cfg.Extensions = map[string]yaml.Node{Block: *n.Content[0]}
		list, templates, err := LoadAll(cfg, *fc)
		if err != nil {
			t.Fatal(err)
		}
		fc.Pools, fc.Templates = list, templates
	}
}

// stateOf returns the state of the pool name as the process sees it.
func stateOf(f *Fleet, name string) pools.Status {
	ps, _ := f.Pools(context.Background())
	for _, p := range ps {
		if p.Name == name {
			return p
		}
	}
	return pools.Status{}
}

func hasGapWith(st pools.Status, part string) bool {
	for _, g := range st.Gaps {
		if strings.Contains(g, part) {
			return true
		}
	}
	return false
}

// A runtime pool lives through the store and the pass: it waits for the
// profile of its agent, creates its Tasks with the marks of a pool once
// agent_put writes the profile, replaces them after a change of its entry,
// which raises its generation, and goes away after a delete once its Tasks
// are gone, with its marker. Only the creators make one, and only the
// owner of the agent and an admin change it.
func TestRuntimePoolLifecycle(t *testing.T) {
	t.Setenv("FW_ATESPACE", "team")
	t.Setenv("FW_API_TOKEN", "fleet-secret")
	ax := newPoolAX(t)
	tasks := newFakeTasks(t)
	store := useMemRuntime(t)
	front := newMember(t, fw.NewMemBus(), "front", runtimeFront(t, ax, strings.TrimPrefix(tasks.srv.URL, "http://"), templateBlock, []string{"carol"}, []string{"alice"}))
	ctx := context.Background()
	alice, bob, carol := as("alice"), as("bob"), as("carol")
	two := 2

	if _, err := CreatePool(bob, front.a, PoolRequest{Name: "helper", Agent: "helper", Replicas: &two}); !errors.Is(err, fw.ErrForbidden) {
		t.Fatalf("a create of bob = %v", err)
	}
	if _, err := CreatePool(alice, front.a, PoolRequest{Name: "helper", Agent: "helper"}); !errors.Is(err, fw.ErrInvalidRequest) || !strings.Contains(err.Error(), "set replicas") {
		t.Fatalf("a create without replicas = %v", err)
	}
	if _, err := CreatePool(alice, front.a, PoolRequest{Name: "helper", Agent: "helper", Replicas: &two, Template: "nope"}); !errors.Is(err, fw.ErrInvalidRequest) || !strings.Contains(err.Error(), "the templates are base (the default)") {
		t.Fatalf("a create of an unknown template = %v", err)
	}
	st, err := CreatePool(alice, front.a, PoolRequest{Name: "helper", Agent: "helper", Replicas: &two})
	if err != nil || st.Source != pools.SourceRuntime || st.Template != "base" || st.Generation != 1 || st.Wanted != 2 || !hasGapWith(st, "waits for the profile helper") {
		t.Fatalf("CreatePool = %+v, %v", st, err)
	}
	if _, err := CreatePool(alice, front.a, PoolRequest{Name: "helper", Agent: "helper", Replicas: &two}); !errors.Is(err, ErrPoolExists) {
		t.Fatalf("a second create = %v", err)
	}
	eventually(t, "a pass of the pool waits for its profile", func() bool {
		return hasGapWith(stateOf(front.f, "helper"), "the pool waits: the runtime pool helper waits for the profile helper")
	})
	if names := ax.names(); len(names) != 0 {
		t.Fatalf("a pool without a profile made Tasks: %v", names)
	}
	// The creator owns the pool until the profile names its owners.
	if _, err := PatchPool(bob, front.a, "helper", PoolPatch{Replicas: &two}); !errors.Is(err, fw.ErrForbidden) {
		t.Fatalf("a change of bob = %v", err)
	}

	at, err := PlaceOf(alice, front.a, "helper", "")
	if err != nil || PoolOf(at) != "helper" {
		t.Fatalf("PlaceOf = %v, %v", PoolOf(at), err)
	}
	if _, _, err := agents.PutIn(alice, front.a, at, fw.AgentProfile{Name: "helper", Description: "Helps."}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the pool creates its Tasks", func() bool {
		return ax.get("team", "helper-0") != nil && ax.get("team", "helper-1") != nil
	})
	task := ax.get("team", "helper-0")
	if envOfTask(task, pools.EnvPool) != "helper" || envOfTask(task, pools.EnvGeneration) != "1" || envOfTask(task, pools.EnvSpec) == "" || envOfTask(task, pools.EnvBase) == "" {
		t.Fatalf("the marks of the Task = %v", task.GetSpec().GetEnv())
	}
	ax.mu.Lock()
	marker := ax.workspaces["team/helper-generation"]
	ws := ax.workspaces["team/helper-0-ws"]
	ax.mu.Unlock()
	if marker == nil || !strings.Contains(marker.Spec.Files[0].Content, "\n1\n") || ws == nil || !strings.Contains(ws.Spec.Files[0].Content, "Helps.") {
		t.Fatalf("marker %v, workspace %v", marker, ws)
	}
	if got := stateOf(front.f, "helper"); got.Source != pools.SourceRuntime || got.By != "alice" {
		t.Fatalf("the state = %+v", got)
	}
	if _, err := PatchPool(bob, front.a, "helper", PoolPatch{Replicas: &two}); !errors.Is(err, fw.ErrForbidden) {
		t.Fatalf("a change of bob after the profile = %v", err)
	}
	if _, err := PatchPool(alice, front.a, "nope", PoolPatch{Replicas: &two}); !errors.Is(err, ErrPoolNotFound) {
		t.Fatalf("a change of no pool = %v", err)
	}

	// A change raises the generation, and the pool replaces its Tasks.
	idle := time.Now().Add(-time.Hour)
	tasks.set("team/helper-0", view.Status{Idle: true, IdleSince: &idle})
	tasks.set("team/helper-1", view.Status{Idle: true, IdleSince: &idle})
	one := 1
	st, err = PatchPool(carol, front.a, "helper", PoolPatch{Replicas: &one})
	if err != nil || st.Wanted != 1 || st.Generation != 2 {
		t.Fatalf("PatchPool of an admin = %+v, %v", st, err)
	}
	eventually(t, "the pool runs one Task of generation 2", func() bool {
		t0 := ax.get("team", "helper-0")
		return t0 != nil && envOfTask(t0, pools.EnvGeneration) == "2" && ax.get("team", "helper-1") == nil
	})

	// A delete sets the replicas to 0, and the pass removes the entry and
	// the marker once the Tasks are gone.
	tasks.set("team/helper-0", view.Status{Idle: true, IdleSince: &idle})
	st, err = DeletePool(alice, front.a, "helper")
	if err != nil || !st.Deleting || st.Wanted != 0 {
		t.Fatalf("DeletePool = %+v, %v", st, err)
	}
	if _, err := PatchPool(alice, front.a, "helper", PoolPatch{Replicas: &two}); !errors.Is(err, ErrPoolConflict) {
		t.Fatalf("a change of a deleted pool = %v", err)
	}
	eventually(t, "the deleted pool is gone", func() bool {
		list, _ := store.List(ctx)
		ax.mu.Lock()
		_, marked := ax.workspaces["team/helper-generation"]
		ax.mu.Unlock()
		return len(list) == 0 && len(ax.names()) == 0 && !marked
	})
	if got := stateOf(front.f, "helper"); got.Name != "" {
		t.Fatalf("the view keeps the deleted pool: %+v", got)
	}
}

// A runtime pool whose name a pool of the file holds runs not at all: the
// file wins, the state of the pool of the file says so, the view lists the
// pool once, a create of the name fails, and a delete removes the entry at
// once.
func TestRuntimePoolConflictWithTheFile(t *testing.T) {
	t.Setenv("FW_ATESPACE", "team")
	t.Setenv("FW_API_TOKEN", "fleet-secret")
	t.Setenv("FRONT_TICKETS_TOKEN", "tickets-secret")
	ax := newPoolAX(t)
	tasks := newFakeTasks(t)
	store := useMemRuntime(t)
	ctx := context.Background()
	if _, err := store.Create(ctx, pools.Runtime{Name: "triage", Template: "base", Agent: "other", Replicas: 1, By: "mallory"}); err != nil {
		t.Fatal(err)
	}
	front := newMember(t, fw.NewMemBus(), "front", runtimeFront(t, ax, strings.TrimPrefix(tasks.srv.URL, "http://"), poolBlock+strings.TrimPrefix(templateBlock, "\n"), nil, nil))
	eventually(t, "the pool of the file names the conflict", func() bool {
		return hasGapWith(stateOf(front.f, "triage"), "the store of the runtime pools holds a pool of this name too, which mallory created; the file wins")
	})
	ps, err := front.f.Pools(ctx)
	if err != nil || len(ps) != 1 || ps[0].Source != pools.SourceFile || ps[0].Agent != "triage" {
		t.Fatalf("Pools = %+v, %v", ps, err)
	}
	two := 2
	if _, err := CreatePool(as("alice"), front.a, PoolRequest{Name: "triage", Agent: "triage", Replicas: &two}); !errors.Is(err, ErrPoolExists) || !strings.Contains(err.Error(), "configuration file") {
		t.Fatalf("a create of the name of a pool of the file = %v", err)
	}
	if _, err := PatchPool(as("alice"), front.a, "nope", PoolPatch{Replicas: &two}); !errors.Is(err, ErrPoolNotFound) {
		t.Fatalf("PatchPool = %v", err)
	}
	st, err := DeletePool(as("mallory"), front.a, "triage")
	if err != nil || !st.Deleting {
		t.Fatalf("DeletePool = %+v, %v", st, err)
	}
	if list, _ := store.List(ctx); len(list) != 0 {
		t.Fatalf("the entry stays: %+v", list)
	}
	if _, err := PatchPool(as("alice"), front.a, "triage", PoolPatch{Replicas: &two}); !errors.Is(err, fw.ErrInvalidRequest) || !strings.Contains(err.Error(), "change it in workbench.pools") {
		t.Fatalf("a change of a pool of the file = %v", err)
	}
	eventually(t, "the conflict ends", func() bool {
		return !hasGapWith(stateOf(front.f, "triage"), "holds a pool of this name too")
	})
}

// agent_put for a profile that no pool runs makes a runtime pool named
// after the agent from the default template, with two workers, or from the
// template and with the replicas of workers. A request that names pool
// keeps the old rules, and without a default template the error names the
// templates. The removal of the profile deletes the pool.
func TestPlaceForCreatesARuntimePool(t *testing.T) {
	t.Setenv("FW_ATESPACE", "team")
	t.Setenv("FW_API_TOKEN", "fleet-secret")
	ax := newPoolAX(t)
	tasks := newFakeTasks(t)
	store := useMemRuntime(t)
	block := templateBlock + `  - name: big
    image: ghcr.io/example/fw@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
    egress:
      rules:
        - http: {hostnames: [nats.agents.svc.cluster.local], ports: [8080]}
    resources:
      limits: {memory: 8Gi}
    fw:
      model: openai/some-model
      bus: {url: "ws://nats.agents.svc.cluster.local:8080"}
      access: {admins: ["kind:token"]}
`
	front := newMember(t, fw.NewMemBus(), "front", runtimeFront(t, ax, strings.TrimPrefix(tasks.srv.URL, "http://"), block, nil, nil))
	ctx := context.Background()
	alice := as("alice")

	at, err := PlaceFor(alice, front.a, "Writer_Bot", "", nil)
	if err != nil || PoolOf(at) != "writer-bot" {
		t.Fatalf("PlaceFor = %v, %v", PoolOf(at), err)
	}
	if d, err := agents.PreviewIn(alice, at, fw.AgentProfile{Name: "Writer_Bot"}); err != nil || d.Exists {
		t.Fatalf("a dry run = %+v, %v", d, err)
	}
	if list, _ := store.List(ctx); len(list) != 0 {
		t.Fatalf("a dry run made a pool: %+v", list)
	}
	if _, created, err := agents.PutIn(alice, front.a, at, fw.AgentProfile{Name: "Writer_Bot", Description: "Writes."}); err != nil || !created {
		t.Fatalf("PutIn = %v, %v", created, err)
	}
	if CreatedPool(at) != "writer-bot" {
		t.Fatalf("CreatedPool = %q", CreatedPool(at))
	}
	e, err := store.Get(ctx, "writer-bot")
	if err != nil || e.Replicas != DefaultWorkers || e.Template != "base" || e.Agent != "Writer_Bot" || e.By != "alice" {
		t.Fatalf("the entry = %+v, %v", e, err)
	}
	o, _ := overlaysOf(t, front.f).Read(ctx, "writer-bot")
	if p, ok := o.Profile("Writer_Bot"); !ok || p.Profile.Access == nil || p.Profile.Access.Owners[0] != "alice" {
		t.Fatalf("the overlay = %+v", o)
	}
	eventually(t, "the pool creates its two Tasks", func() bool {
		return ax.get("team", "writer-bot-0") != nil && ax.get("team", "writer-bot-1") != nil
	})
	// A second save finds the pool and changes the profile.
	at, err = PlaceFor(alice, front.a, "Writer_Bot", "", &Workers{Replicas: ptrInt(5)})
	if err != nil || PoolOf(at) != "writer-bot" {
		t.Fatalf("PlaceFor of an agent with a pool = %v, %v", PoolOf(at), err)
	}
	if _, created, err := agents.PutIn(alice, front.a, at, fw.AgentProfile{Name: "Writer_Bot", Description: "Writes more."}); err != nil || created || CreatedPool(at) != "" {
		t.Fatalf("a second PutIn = %v, %v, %q", created, err, CreatedPool(at))
	}

	at, err = PlaceFor(alice, front.a, "heavy", "", &Workers{Template: "big", Replicas: ptrInt(3)})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := agents.PutIn(alice, front.a, at, fw.AgentProfile{Name: "heavy"}); err != nil {
		t.Fatal(err)
	}
	if e, err := store.Get(ctx, "heavy"); err != nil || e.Replicas != 3 || e.Template != "big" {
		t.Fatalf("the entry of heavy = %+v, %v", e, err)
	}
	if _, err := PlaceFor(alice, front.a, "other", "", &Workers{Template: "nope"}); err == nil || !strings.Contains(err.Error(), "no template is named nope") {
		t.Fatalf("PlaceFor with an unknown template = %v", err)
	}
	if _, err := PlaceFor(alice, front.a, "other", "heavy", &Workers{}); err == nil || !strings.Contains(err.Error(), "not both") {
		t.Fatalf("PlaceFor with pool and workers = %v", err)
	}
	if at, err := PlaceFor(alice, front.a, "helper", "heavy", nil); err != nil || PoolOf(at) != "heavy" {
		t.Fatalf("PlaceFor with pool = %v, %v", PoolOf(at), err)
	}
	if _, err := PlaceOf(alice, front.a, "other", ""); err == nil || !strings.Contains(err.Error(), "The templates are base (the default), big") {
		t.Fatalf("PlaceOf of a new name = %v", err)
	}

	// The removal of the profile of the agent deletes its pool.
	at, err = PlaceOf(alice, front.a, "heavy", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := agents.DeleteIn(alice, front.a, at, "heavy"); err != nil {
		t.Fatal(err)
	}
	if DroppedPool(at) != "heavy" {
		t.Fatalf("DroppedPool = %q", DroppedPool(at))
	}
	if e, err := store.Get(ctx, "heavy"); err == nil && !e.Deleting {
		t.Fatalf("the pool of heavy = %+v", e)
	}
}

// Without a default template, a new agent needs workers, and the error
// names the templates.
func TestPlaceForWithoutADefault(t *testing.T) {
	t.Setenv("FW_ATESPACE", "team")
	t.Setenv("FW_API_TOKEN", "fleet-secret")
	ax := newPoolAX(t)
	tasks := newFakeTasks(t)
	useMemRuntime(t)
	block := strings.Replace(templateBlock, "    default: true\n", "", 1)
	front := newMember(t, fw.NewMemBus(), "front", runtimeFront(t, ax, strings.TrimPrefix(tasks.srv.URL, "http://"), block, nil, nil))
	_, err := PlaceFor(as("alice"), front.a, "writer", "", nil)
	if !errors.Is(err, fw.ErrInvalidRequest) || !strings.Contains(err.Error(), "no template is the default, so pass workers with a template") || !strings.Contains(err.Error(), "The templates are base") {
		t.Fatalf("PlaceFor = %v", err)
	}
	if at, err := PlaceFor(as("alice"), front.a, "writer", "", &Workers{Template: "base"}); err != nil || PoolOf(at) != "writer" {
		t.Fatalf("PlaceFor with workers = %v, %v", PoolOf(at), err)
	}
	if got := TemplatesOf(front.a); len(got) != 1 || got[0].Name != "base" || got[0].Default {
		t.Fatalf("TemplatesOf = %+v", got)
	}
}

// The doctor line names the templates and counts the runtime pools, and it
// fails when a runtime pool names a template that the configuration does
// not hold.
func TestDoctorNamesTheTemplates(t *testing.T) {
	for _, e := range []string{EnvAtespaces, EnvEgress, EnvName, EnvSuspendAfter, EnvIntake, EnvWakeOnQueue} {
		t.Setenv(e, "")
	}
	t.Setenv("FW_ATESPACE", "")
	t.Setenv(EnvAtespaces, "team")
	t.Setenv(EnvEgress, "fw.events")
	t.Setenv("FW_API_TOKEN", "fleet-secret")
	store := useMemRuntime(t)
	ctx := context.Background()
	cfg := fw.DefaultConfig()
	cfg.Bus.Driver, cfg.Bus.URL = "nats", "nats://nats.agents.svc.cluster.local:4222"
	cfg.AX.Server, cfg.AX.Router = "ax-server:8080", "atenet-router"
	var n yaml.Node
	if err := yaml.Unmarshal([]byte(templateBlock), &n); err != nil {
		t.Fatal(err)
	}
	cfg.Extensions = map[string]yaml.Node{Block: *n.Content[0]}
	if _, err := store.Create(ctx, pools.Runtime{Name: "helper", Template: "base", Agent: "helper", Replicas: 1}); err != nil {
		t.Fatal(err)
	}
	ok, line := DoctorCheck(ctx, cfg, fw.EnvSecrets{}, time.Second)
	if !ok || !strings.Contains(line, "offers the worker templates base (the default)") || !strings.Contains(line, "keeps 1 runtime pool in the key-value bucket FW_POOLS") {
		t.Fatalf("doctor = %v %q", ok, line)
	}
	if _, err := store.Create(ctx, pools.Runtime{Name: "lost", Template: "gone", Agent: "lost", Replicas: 1}); err != nil {
		t.Fatal(err)
	}
	ok, line = DoctorCheck(ctx, cfg, fw.EnvSecrets{}, time.Second)
	if ok || !strings.Contains(line, "keeps 2 runtime pools") || !strings.Contains(line, "the runtime pool lost names the template gone, which workbench.templates does not hold") {
		t.Fatalf("doctor with a lost template = %v %q", ok, line)
	}
}

func ptrInt(n int) *int { return &n }
