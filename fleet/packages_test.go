package fleet

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/surface/view"
	"github.com/bdubs00/featherweight-workbench/internal/bucket"
	"github.com/bdubs00/featherweight-workbench/pools"
	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// packagesBlock is a pool of two workers with packages on, whose profile
// alice owns, on a front with a package mirror.
const packagesBlock = `
packages:
  mirrors:
    python: http://pkg-cache.fw-fleet.svc.cluster.local:8080/pypi/simple
    tools: http://pkg-cache.fw-fleet.svc.cluster.local:8080/conda-forge
  egress: [http://pkg-cache.fw-fleet.svc.cluster.local:8080]
pools:
  - name: triage
    replicas: 2
    image: ghcr.io/example/fw@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
    egress:
      hosts:
        - http://nats.agents.svc.cluster.local:8080
    packages: {on: true, ask: true, python: [ansible-lint]}
    fw:
      model: openai/some-model
      bus: {url: "ws://nats.agents.svc.cluster.local:8080"}
      access: {admins: ["kind:token"]}
      agents:
        triage:
          description: Sorts new tickets.
          access: {owners: [alice], viewers: [bob]}
`

// workspaceOf returns the fw.yaml of a Workspace of the ax API.
func (f *poolAX) workspaceOf(atespace, name string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if ws := f.workspaces[atespace+"/"+name]; ws != nil && len(ws.Spec.Files) > 0 {
		return ws.Spec.Files[0].Content
	}
	return ""
}

// packagesRig is two replicas of a front that share the bus, the store of
// the overlays, and the bucket of the states, over one ax API.
type packagesRig struct {
	t        *testing.T
	bus      *fw.MemBus
	ax       *poolAX
	tasks    *fakeTasks
	overlays *pools.MemOverlays
	states   bucket.Bucket
	fronts   []*member
	seq      uint64
}

func newPackagesRig(t *testing.T, replicas int) *packagesRig {
	t.Helper()
	t.Setenv("FW_ATESPACE", "team")
	t.Setenv("FW_API_TOKEN", "fleet-secret")
	r := &packagesRig{t: t, bus: fw.NewMemBus(), ax: newPoolAX(t), tasks: newFakeTasks(t), overlays: pools.NewMemOverlays(), states: bucket.NewMem(EnvBucket)}
	mut := poolFront(t, r.ax, strings.TrimPrefix(r.tasks.srv.URL, "http://"), packagesBlock)
	oldO, oldE := OpenOverlays, OpenEnvironments
	OpenOverlays = func(context.Context, *fw.Config, *slog.Logger) (pools.OverlayStore, func(), error) {
		return r.overlays, nil, nil
	}
	OpenEnvironments = func(context.Context, *fw.Config, *slog.Logger) (bucket.Bucket, func(), error) {
		return r.states, nil, nil
	}
	t.Cleanup(func() { OpenOverlays, OpenEnvironments = oldO, oldE })
	for i := range replicas {
		r.fronts = append(r.fronts, newMember(t, r.bus, "front"+string(rune('a'+i)), mut))
	}
	idle := time.Now().Add(-time.Hour)
	for _, task := range []string{"team/triage-0", "team/triage-1"} {
		r.tasks.set(task, view.Status{Idle: true, IdleSince: &idle})
	}
	return r
}

// publish sends an event of the environment of a worker, as its kernel
// does.
func (r *packagesRig) publish(task, typ, id string, at time.Time, data any) {
	r.t.Helper()
	raw, err := json.Marshal(data)
	if err != nil {
		r.t.Fatal(err)
	}
	if err := r.bus.Publish(context.Background(), EnvSubject, fw.Event{ID: id, Type: typ, Time: at, Node: "ax:team/" + task, Data: raw}); err != nil {
		r.t.Fatal(err)
	}
}

// approve puts an approval of the tool env of the run on the stream of
// each front, with its decision.
func (r *packagesRig) approve(task, run, input, decision, decider string) {
	r.t.Helper()
	r.seq++
	ask, _ := json.Marshal(fw.ApprovalRequestedData{ApprovalID: run + "-ask", Tool: "env", Input: json.RawMessage(input), Reason: "rule: ask env: command=add"})
	dec, _ := json.Marshal(fw.ApprovalDecidedData{ApprovalID: run + "-ask", Decision: decision, Decider: decider})
	for _, f := range r.fronts {
		for i, e := range []fw.Event{
			{ID: run + "-1", Type: fw.EvApprovalRequested, Subject: run, Seq: r.seq*10 + 1, Node: "ax:team/" + task, Time: time.Now(), Data: ask},
			{ID: run + "-2", Type: fw.EvApprovalDecided, Subject: run, Seq: r.seq*10 + 2, Node: "ax:team/" + task, Time: time.Now(), Data: dec},
		} {
			if err := f.f.platform.onEvent(e); err != nil {
				r.t.Fatalf("event %d: %v", i, err)
			}
		}
	}
}

// packages reads the answer for alice.
func (r *packagesRig) packages() Packages {
	r.t.Helper()
	out, err := PackagesOf(as("alice"), r.fronts[0].a, "triage")
	if err != nil {
		r.t.Fatal(err)
	}
	return out
}

func envWorkerOf(p Packages, task string) WorkerPackages {
	for _, w := range p.Workers {
		if w.Task == task {
			return w
		}
	}
	return WorkerPackages{}
}

// The front keeps the last event of the environment of each worker and
// answers the state of each worker: materializing before the first event
// of the Task, ready with the versions of the packages of the setting,
// and failed with the failure. An event of an earlier Task of the same
// name counts as none.
func TestPackagesStates(t *testing.T) {
	r := newPackagesRig(t, 1)
	ctx := context.Background()
	front := r.fronts[0]
	eventually(t, "the pool is ready", func() bool {
		ps, err := front.f.Pools(ctx)
		return err == nil && len(ps) == 1 && ps[0].Ready == 2
	})
	p := r.packages()
	if !p.On || !p.Ask || !p.Available || p.Applying || len(p.Python) != 1 || p.Python[0].Name != "ansible-lint" || p.Python[0].By != "" || len(p.Workers) != 2 {
		t.Fatalf("packages = %+v", p)
	}
	if w := envWorkerOf(p, "triage-0"); w.State != EnvMaterializing || w.At != nil {
		t.Fatalf("triage-0 before an event = %+v", w)
	}
	task := r.ax.get("team", "triage-0")
	if !slices.Contains(task.Spec.Egress.Hosts, "http://pkg-cache.fw-fleet.svc.cluster.local:8080") {
		t.Fatalf("the egress of the Task = %v", task.Spec.Egress.Hosts)
	}

	at := time.Now().UTC().Truncate(time.Second)
	r.publish("triage-0", EvEnvReady, "e1", at, map[string]any{"digest": "d1", "python": map[string]any{"packages": []map[string]any{
		{"name": "ansible-lint", "version": "24.9.2"}, {"name": "ruamel.yaml", "version": "0.18.6"}}}})
	r.publish("triage-1", EvEnvFailed, "e2", at, map[string]any{"digest": "d1", "step": "python", "error": "uv sync failed", "output": "No solution found", "retryAt": at.Add(time.Minute)})
	eventually(t, "the states arrive", func() bool {
		p := r.packages()
		return envWorkerOf(p, "triage-0").State == EnvReady && envWorkerOf(p, "triage-1").State == EnvFailed
	})
	p = r.packages()
	w0, w1 := envWorkerOf(p, "triage-0"), envWorkerOf(p, "triage-1")
	if w0.Digest != "d1" || w0.At == nil || !w0.At.Equal(at) || w0.Installed == nil || len(w0.Installed.Python) != 1 || w0.Installed.Python[0].Version != "24.9.2" {
		t.Fatalf("triage-0 = %+v %+v", w0, w0.Installed)
	}
	if w1.Failure == nil || w1.Failure.Step != "python" || w1.Failure.Output != "No solution found" || w1.Failure.RetryAt == nil {
		t.Fatalf("triage-1 = %+v", w1)
	}
	// An older event, and an event of a Task that no pool owns, change
	// nothing.
	r.publish("triage-0", EvEnvFailed, "e0", at.Add(-time.Hour), map[string]any{"step": "python"})
	r.publish("notes", EvEnvFailed, "e3", at, map[string]any{"step": "python"})
	r.publish("triage-0-01k8zj4x7ad9c0n3d5r6s7t8v9", EvEnvFailed, "e4", at, map[string]any{"step": "python"})
	time.Sleep(50 * time.Millisecond)
	if w := envWorkerOf(r.packages(), "triage-0"); w.State != EnvReady {
		t.Fatalf("an older event changed the state: %+v", w)
	}
	keys, _ := r.states.Keys(ctx)
	if len(keys) != 2 {
		t.Fatalf("the bucket holds %d states", len(keys))
	}
	// An event from before the creation of the Task belongs to an earlier
	// Task of that name.
	if err := r.states.Delete(ctx, envKey("team/triage-0"), keys[0].Revision); err != nil && !errors.Is(err, bucket.ErrConflict) {
		t.Fatal(err)
	}
	_ = r.states.Delete(ctx, envKey("team/triage-0"), keys[1].Revision)
	r.publish("triage-0", EvEnvReady, "e5", time.Unix(1_600_000_000, 0), map[string]any{"digest": "d0"})
	eventually(t, "the event of the earlier Task arrives", func() bool {
		_, ok, _ := front.f.envState(ctx, "team/triage-0")
		return ok
	})
	if w := envWorkerOf(r.packages(), "triage-0"); w.State != EnvMaterializing || w.At != nil {
		t.Fatalf("triage-0 with an event of an earlier Task = %+v", w)
	}
}

// A person allows an add of the tool env on one worker. The worker
// installs it and publishes fw.env.changed, and each replica of the front
// writes the package into the overlay of the pool once, with the person,
// the agent, and the run. A change that no person allowed, a change on a
// pool whose agent cannot ask, and the same event again write nothing.
// The pool then replaces each idle worker with the new list.
func TestPackagesApprovedChange(t *testing.T) {
	r := newPackagesRig(t, 2)
	ctx := context.Background()
	front := r.fronts[0]
	eventually(t, "the pool is ready", func() bool {
		ps, err := front.f.Pools(ctx)
		return err == nil && len(ps) == 1 && ps[0].Ready == 2
	})
	old := envOfTask(r.ax.get("team", "triage-0"), pools.EnvSpec)
	changed := func(id, run, pkg string) {
		r.publish("triage-0", EvEnvChanged, id, time.Now(), map[string]any{"digest": "d2", "previous": "d1", "manager": "python", "command": "add", "package": pkg, "run": run, "agent": "triage"})
	}

	// No person allowed this one.
	changed("c0", "01NONE", "requests")
	r.approve("triage-0", "01DENY", `{"command":"add","manager":"python","package":"rich"}`, "deny", "alice")
	changed("c1", "01DENY", "rich")
	time.Sleep(50 * time.Millisecond)
	if o, _ := r.overlays.Read(ctx, "triage"); o.Packages != nil {
		t.Fatalf("a change without an approval wrote the overlay: %+v", o.Packages)
	}

	r.approve("triage-0", "01RUN", `{"command":"add","manager":"python","package":"pyyaml"}`, "allow", "alice")
	changed("c2", "01RUN", "pyyaml")
	eventually(t, "the overlay takes the package", func() bool {
		o, _ := r.overlays.Read(ctx, "triage")
		return o.Packages != nil && len(o.Packages.Python) == 2
	})
	o, _ := r.overlays.Read(ctx, "triage")
	it := o.Packages.Python[1]
	if it.Name != "pyyaml" || it.By != "alice" || it.Agent != "triage" || it.Run != "01RUN" || it.At == nil || o.Packages.Python[0].Name != "ansible-lint" ||
		!o.Packages.On || !o.Packages.Ask || !slices.Equal(o.Packages.Applied, []string{"c2"}) {
		t.Fatalf("the overlay = %+v", o.Packages)
	}
	hashes := func() (string, string) {
		return envOfTask(r.ax.get("team", "triage-0"), pools.EnvSpec), envOfTask(r.ax.get("team", "triage-1"), pools.EnvSpec)
	}
	eventually(t, "every worker runs the new list", func() bool {
		h0, h1 := hashes()
		return h0 != "" && h0 != old && h0 == h1
	})
	ws := r.ax.workspaceOf("team", "triage-1-ws")
	if !strings.Contains(ws, "pyyaml") {
		t.Fatalf("the Workspace of triage-1 = %s", ws)
	}
	// A person removes the package; the same event again does not bring
	// it back.
	if _, err := PutPackages(as("alice"), front.a, "triage", PackagesRequest{On: ptrTo(true), Ask: true, Python: []string{"ansible-lint"}}, false); err != nil {
		t.Fatal(err)
	}
	changed("c2", "01RUN", "pyyaml")
	time.Sleep(50 * time.Millisecond)
	if o, _ := r.overlays.Read(ctx, "triage"); len(o.Packages.Python) != 1 {
		t.Fatalf("a repeated event wrote again: %+v", o.Packages)
	}

	// The list is the list of the configuration again, so the workers go
	// back to its hash.
	eventually(t, "every worker runs the list of the configuration again", func() bool {
		h0, h1 := hashes()
		return h0 == old && h1 == old
	})

	// Without ask, the pool takes no change of a worker.
	if _, err := PutPackages(as("alice"), front.a, "triage", PackagesRequest{On: ptrTo(true), Python: []string{"ansible-lint"}}, false); err != nil {
		t.Fatal(err)
	}
	r.approve("triage-0", "01ASK", `{"command":"add","manager":"python","package":"httpx"}`, "allow", "alice")
	changed("c3", "01ASK", "httpx")
	time.Sleep(50 * time.Millisecond)
	if o, _ := r.overlays.Read(ctx, "triage"); len(o.Packages.Python) != 1 {
		t.Fatalf("a pool whose agent cannot ask took a change: %+v", o.Packages)
	}
}

// PutPackages replaces the setting for the owner of the agent of the pool,
// keeps the author of an item that stays, checks each name, and writes
// nothing for a dry run or for a request that changes nothing.
func TestPutPackages(t *testing.T) {
	r := newPackagesRig(t, 1)
	ctx := context.Background()
	front := r.fronts[0]
	eventually(t, "the pool is ready", func() bool {
		ps, err := front.f.Pools(ctx)
		return err == nil && len(ps) == 1 && ps[0].Ready == 2
	})
	req := PackagesRequest{On: ptrTo(true), Ask: true, Python: []string{" ansible-lint ", "pyyaml>=6", "pyyaml>=6"}, Tools: []string{"iproute2"}}
	if _, err := PutPackages(as("bob"), front.a, "triage", req, false); !errors.Is(err, fw.ErrForbidden) {
		t.Fatalf("a change of bob = %v", err)
	}
	if _, err := PackagesOf(as("carol"), front.a, "triage"); !errors.Is(err, ErrPoolNotFound) {
		t.Fatalf("a read of carol, who cannot view the pool = %v", err)
	}
	if _, err := PutPackages(as("alice"), front.a, "nope", req, false); !errors.Is(err, ErrPoolNotFound) {
		t.Fatalf("a change of no pool = %v", err)
	}
	for _, x := range []struct {
		req  PackagesRequest
		want string
	}{
		{PackagesRequest{Python: []string{"x"}}, "set on"},
		{PackagesRequest{On: ptrTo(true), Python: []string{"--index-url=http://evil"}}, "python[0] starts with a dash"},
		{PackagesRequest{On: ptrTo(true), Tools: []string{"bad\x00"}}, "tools[0] holds a control character"},
	} {
		if _, err := PutPackages(as("alice"), front.a, "triage", x.req, false); !errors.Is(err, fw.ErrInvalidRequest) || !strings.Contains(err.Error(), x.want) {
			t.Errorf("PutPackages(%+v) = %v, want %q", x.req, err, x.want)
		}
	}
	dry, err := PutPackages(as("alice"), front.a, "triage", req, true)
	if err != nil || len(dry.Python) != 2 || dry.Python[1].Name != "pyyaml>=6" || dry.Python[1].By != "alice" || !dry.Applying {
		t.Fatalf("dry run = %+v, %v", dry, err)
	}
	if o, _ := r.overlays.Read(ctx, "triage"); o.Packages != nil {
		t.Fatal("a dry run wrote the overlay")
	}
	out, err := PutPackages(as("alice"), front.a, "triage", req, false)
	if err != nil || !out.Applying || len(out.Tools) != 1 || out.Python[0].By != "" || out.Python[1].By != "alice" {
		t.Fatalf("PutPackages = %+v, %v", out, err)
	}
	for _, w := range out.Workers {
		if w.State != EnvPending {
			t.Fatalf("a worker of an older configuration = %+v", w)
		}
	}
	o, _ := r.overlays.Read(ctx, "triage")
	if o.Packages == nil || o.Packages.By != "alice" || !slices.Equal(pools.Names(o.Packages.Python), []string{"ansible-lint", "pyyaml>=6"}) {
		t.Fatalf("the overlay = %+v", o.Packages)
	}
	at := *o.Packages.At
	// The same request writes nothing.
	if _, err := PutPackages(as("alice"), front.a, "triage", req, false); err != nil {
		t.Fatal(err)
	}
	if o, _ := r.overlays.Read(ctx, "triage"); !o.Packages.At.Equal(at) {
		t.Fatal("a request that changes nothing wrote the overlay")
	}
	eventually(t, "the workers run the new configuration", func() bool {
		p := r.packages()
		return !p.Applying && len(p.Workers) == 2 && envWorkerOf(p, "triage-0").State == EnvMaterializing
	})
	// Off keeps the lists, so on again restores them, and the answer
	// names no worker.
	off := PackagesRequest{On: ptrTo(false), Ask: true, Python: []string{"ansible-lint", "pyyaml>=6"}, Tools: []string{"iproute2"}}
	if out, err := PutPackages(as("alice"), front.a, "triage", off, false); err != nil || out.On || len(out.Workers) != 0 || len(out.Python) != 2 || out.Python[1].By != "alice" {
		t.Fatalf("off = %+v, %v", out, err)
	}
	if o, _ := r.overlays.Read(ctx, "triage"); o.Packages.On || len(o.Packages.Python) != 2 || len(o.Packages.Tools) != 1 {
		t.Fatalf("the overlay when off = %+v", o.Packages)
	}
	merged, err := front.f.mergedPool(ctx, front.f.platform.pools[0])
	if err != nil || merged.PackagesOn() {
		t.Fatalf("the merged pool when off = %v", err)
	}
	if _, ws, err := merged.Render(0, pools.Placeholders); err != nil || strings.Contains(ws.Spec.Files[0].Content, "environment:") {
		t.Fatalf("the workers get packages when off: %v", err)
	}
	on := off
	on.On = ptrTo(true)
	if out, err := PutPackages(as("alice"), front.a, "triage", on, true); err != nil || !out.On || len(out.Python) != 2 || out.Python[1].By != "alice" {
		t.Fatalf("on again = %+v, %v", out, err)
	}
}

// Without workbench.packages the front serves no packages.
func TestPackagesOff(t *testing.T) {
	t.Setenv("FW_ATESPACE", "team")
	t.Setenv("FW_API_TOKEN", "fleet-secret")
	t.Setenv("FRONT_TICKETS_TOKEN", "tickets-secret")
	ax := newPoolAX(t)
	tasks := newFakeTasks(t)
	front := newMember(t, fw.NewMemBus(), "front", poolFront(t, ax, strings.TrimPrefix(tasks.srv.URL, "http://"), poolBlock))
	if PackagesOn(front.a) {
		t.Fatal("PackagesOn without a mirror")
	}
	if _, err := PackagesOf(as("alice"), front.a, "triage"); !errors.Is(err, ErrPackagesOff) {
		t.Fatalf("PackagesOf = %v", err)
	}
}

func ptrTo[T any](v T) *T { return &v }

// The store on NATS keeps the packages of a pool in one entry beside its
// profiles. A call that writes nothing makes no bucket, and writers that
// race each keep their change, because each write compares the revision.
func TestNATSOverlayPackages(t *testing.T) {
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
	if got, err := store.UpdatePackages(ctx, "triage", func(cur *pools.PackageSetting) (*pools.PackageSetting, error) { return nil, nil }); got != nil || err != nil {
		t.Fatalf("a call that writes nothing = %+v, %v", got, err)
	}
	if _, err := js.KeyValue(ctx, "FW_POOL_triage"); err == nil {
		t.Fatal("a call that writes nothing made the bucket")
	}
	if err := store.PutProfile(ctx, "triage", pools.OverlayProfile{Profile: fw.AgentProfile{Name: "helper"}}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := range 6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := store.UpdatePackages(ctx, "triage", func(cur *pools.PackageSetting) (*pools.PackageSetting, error) {
				s := pools.PackageSetting{On: true, Ask: true}
				if cur != nil {
					s = *cur
				}
				pools.ChangePackages(&s, pools.ListPython, "add", pools.PackageItem{Name: "pkg" + string(rune('a'+i))})
				return &s, nil
			})
			if err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	o, err := store.Read(ctx, "triage")
	if err != nil || o.Packages == nil || len(o.Packages.Python) != 6 || len(o.Profiles) != 1 || !o.Packages.On {
		t.Fatalf("Read = %+v, %v", o.Packages, err)
	}
	if ok, err := store.DeleteProfile(ctx, "triage", "helper"); !ok || err != nil {
		t.Fatalf("DeleteProfile = %v, %v", ok, err)
	}
	if o, _ := store.Read(ctx, "triage"); o.Packages == nil || len(o.Packages.Python) != 6 {
		t.Fatal("a delete of a profile took the packages away")
	}
}
