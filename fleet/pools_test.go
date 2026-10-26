package fleet

import (
	"context"
	"log/slog"
	"net"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/config"
	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/surface/view"
	"github.com/bdubs00/featherweight-workbench/pools"
	v1alpha1 "github.com/google/ax/pkg/apis/v1alpha1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
	"gopkg.in/yaml.v3"
)

// poolAX is the ax API on a local listener with whole Tasks: a Task is
// created suspended and is immutable, a resume runs it and sets the
// condition WorkspaceReady, and a Workspace is written whole.
type poolAX struct {
	v1alpha1.UnimplementedAXServer
	addr string

	mu         sync.Mutex
	tasks      map[string]*v1alpha1.Task
	workspaces map[string]*v1alpha1.Workspace
	clock      int64
}

func newPoolAX(t *testing.T) *poolAX {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &poolAX{addr: lis.Addr().String(), tasks: map[string]*v1alpha1.Task{}, workspaces: map[string]*v1alpha1.Workspace{}}
	srv := grpc.NewServer()
	v1alpha1.RegisterAXServer(srv, f)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	return f
}

func (f *poolAX) put(t *v1alpha1.Task, phase string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := proto.Clone(t).(*v1alpha1.Task)
	f.clock++
	c.Metadata.CreateTime = timestamppb.New(time.Unix(1_700_000_000+f.clock, 0))
	c.Status = &v1alpha1.TaskStatus{Phase: phase, Conditions: []*v1alpha1.Condition{{Type: "WorkspaceReady", Status: "True"}}}
	f.tasks[c.Metadata.Atespace+"/"+c.Metadata.Name] = c
}

func (f *poolAX) get(atespace, name string) *v1alpha1.Task {
	f.mu.Lock()
	defer f.mu.Unlock()
	if t, ok := f.tasks[atespace+"/"+name]; ok {
		return proto.Clone(t).(*v1alpha1.Task)
	}
	return nil
}

func (f *poolAX) names() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for k := range f.tasks {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func (f *poolAX) ListTasks(_ context.Context, req *v1alpha1.ListTasksRequest) (*v1alpha1.ListTasksResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*v1alpha1.Task
	for k, t := range f.tasks {
		if strings.HasPrefix(k, req.GetAtespace()+"/") {
			out = append(out, proto.Clone(t).(*v1alpha1.Task))
		}
	}
	return &v1alpha1.ListTasksResponse{Tasks: out}, nil
}

func (f *poolAX) GetTask(_ context.Context, req *v1alpha1.GetTaskRequest) (*v1alpha1.Task, error) {
	if t := f.get(req.GetAtespace(), req.GetName()); t != nil {
		return t, nil
	}
	return nil, status.Error(codes.NotFound, "no such task")
}

func (f *poolAX) CreateTask(_ context.Context, req *v1alpha1.CreateTaskRequest) (*v1alpha1.Task, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t := proto.Clone(req.GetTask()).(*v1alpha1.Task)
	k := t.Metadata.Atespace + "/" + t.Metadata.Name
	if _, ok := f.tasks[k]; ok {
		return nil, status.Error(codes.FailedPrecondition, "the task exists and is immutable")
	}
	f.clock++
	t.Metadata.CreateTime = timestamppb.New(time.Unix(1_700_000_000+f.clock, 0))
	t.Status = &v1alpha1.TaskStatus{Phase: "Suspended"}
	f.tasks[k] = t
	return proto.Clone(t).(*v1alpha1.Task), nil
}

func (f *poolAX) ResumeTask(_ context.Context, req *v1alpha1.ResumeTaskRequest) (*v1alpha1.Task, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.tasks[req.GetAtespace()+"/"+req.GetName()]
	if !ok {
		return nil, status.Error(codes.NotFound, "no such task")
	}
	t.Status.Phase = "Running"
	if len(t.Status.Conditions) == 0 {
		t.Status.Conditions = []*v1alpha1.Condition{{Type: "WorkspaceReady", Status: "True"}}
	}
	return proto.Clone(t).(*v1alpha1.Task), nil
}

func (f *poolAX) DeleteTask(_ context.Context, req *v1alpha1.DeleteTaskRequest) (*v1alpha1.DeleteTaskResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := req.GetAtespace() + "/" + req.GetName()
	if _, ok := f.tasks[k]; !ok {
		return nil, status.Error(codes.NotFound, "no such task")
	}
	delete(f.tasks, k)
	return &v1alpha1.DeleteTaskResponse{}, nil
}

func (f *poolAX) GetWorkspace(_ context.Context, req *v1alpha1.GetWorkspaceRequest) (*v1alpha1.Workspace, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if ws, ok := f.workspaces[req.GetAtespace()+"/"+req.GetName()]; ok {
		return proto.Clone(ws).(*v1alpha1.Workspace), nil
	}
	return nil, status.Error(codes.NotFound, "no such workspace")
}

func (f *poolAX) UpdateWorkspace(_ context.Context, req *v1alpha1.UpdateWorkspaceRequest) (*v1alpha1.Workspace, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ws := proto.Clone(req.GetWorkspace()).(*v1alpha1.Workspace)
	f.workspaces[ws.Metadata.Atespace+"/"+ws.Metadata.Name] = ws
	return proto.Clone(ws).(*v1alpha1.Workspace), nil
}

func (f *poolAX) DeleteWorkspace(_ context.Context, req *v1alpha1.DeleteWorkspaceRequest) (*v1alpha1.DeleteWorkspaceResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.workspaces, req.GetAtespace()+"/"+req.GetName())
	return &v1alpha1.DeleteWorkspaceResponse{}, nil
}

// poolBlock is a block workbench with two workers of the profile triage.
const poolBlock = `
pools:
  - name: triage
    replicas: 2
    image: ghcr.io/example/fw@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
    env:
      - name: TICKETS_TOKEN
        fromEnv: FRONT_TICKETS_TOKEN
    egress:
      hosts:
        - http://nats.agents.svc.cluster.local:8080
    fw:
      model: openai/some-model
      bus: {url: "ws://nats.agents.svc.cluster.local:8080"}
      agents:
        triage:
          description: Sorts new tickets.
`

// poolFront configures a front that keeps the pools of block, over the ax
// API and the router of a test.
func poolFront(t *testing.T, ax *poolAX, router string, block string) func(*fw.Config, *Config) {
	useMemOverlays(t)
	return func(cfg *fw.Config, fc *Config) {
		off := time.Duration(0)
		fc.Atespaces = []string{"team"}
		fc.Interval = 50 * time.Millisecond
		fc.SuspendAfter = &off
		cfg.AX.Server, cfg.AX.Router = ax.addr, router
		// The workers share the bus of the front; the test runs the mem bus
		// of the process, and the configuration names nats as the cluster
		// would.
		cfg.Bus.Driver, cfg.Bus.URL = "nats", "nats://nats.agents.svc.cluster.local:4222"
		var n yaml.Node
		if err := yaml.Unmarshal([]byte(block), &n); err != nil {
			t.Fatal(err)
		}
		cfg.Extensions = map[string]yaml.Node{Block: *n.Content[0]}
		list, err := LoadPools(cfg, *fc)
		if err != nil {
			t.Fatal(err)
		}
		fc.Pools = list
	}
}

// useMemOverlays makes the fronts of a test keep the overlays of their
// pools in memory, because the configuration names the bus nats.
func useMemOverlays(t *testing.T) {
	t.Helper()
	old := OpenOverlays
	OpenOverlays = func(context.Context, *fw.Config, *slog.Logger) (pools.OverlayStore, func(), error) {
		return pools.NewMemOverlays(), nil, nil
	}
	t.Cleanup(func() { OpenOverlays = old })
}

// setRunState changes the state of a run in the view, as its next event
// on the stream would.
func setRunState(f *Fleet, id string, state fw.RunState) {
	p := f.platform
	p.mu.Lock()
	defer p.mu.Unlock()
	p.runs[id].run.State = state
}

// The front keeps the Tasks of a pool: it creates the missing ones with
// its own token, waits while a Task of an older configuration holds a run
// that the stream reported, replaces it once it is idle, leaves a Task of
// no pool alone, and shows the state of the pool.
func TestPoolsKeepTheTasks(t *testing.T) {
	t.Setenv("FW_ATESPACE", "team")
	t.Setenv("FW_API_TOKEN", "fleet-secret")
	t.Setenv("FRONT_TICKETS_TOKEN", "tickets-secret")
	ctx := context.Background()
	ax := newPoolAX(t)
	tasks := newFakeTasks(t)
	// A Task of an older configuration of the pool holds a run, and a Task
	// of a person sits beside the pool.
	old := &v1alpha1.Task{Metadata: &v1alpha1.ObjectMeta{Name: "triage-1", Atespace: "team"}, Spec: &v1alpha1.TaskSpec{
		Image: "ghcr.io/example/fw@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		Env:   []*v1alpha1.EnvVar{{Name: pools.EnvPool, Value: "triage"}, {Name: pools.EnvSpec, Value: "oldhash"}},
	}}
	ax.put(old, "Running")
	ax.put(&v1alpha1.Task{Metadata: &v1alpha1.ObjectMeta{Name: "notes", Atespace: "team"}, Spec: &v1alpha1.TaskSpec{Image: "x"}}, "Running")
	idle := time.Now().Add(-time.Hour)
	tasks.set("team/triage-1", view.Status{Runs: view.RunCounts{Active: 1, Total: 1}})
	tasks.set("team/triage-0", view.Status{Idle: true, IdleSince: &idle})

	bus := fw.NewMemBus()
	front := newMember(t, bus, "front", poolFront(t, ax, strings.TrimPrefix(tasks.srv.URL, "http://"), poolBlock))
	inject(front.f, "team/triage-1", "ax:team/triage-1", "team", MemberLive, time.Now(),
		&fw.Run{ID: "01RUN", Agent: "triage", State: fw.StateRunning, Principal: "alice", CreatedAt: time.Now()})

	eventually(t, "the missing Task is created and resumed", func() bool {
		t0 := ax.get("team", "triage-0")
		return t0 != nil && t0.Status.Phase == "Running"
	})
	made := ax.get("team", "triage-0")
	env := map[string]string{}
	for _, e := range made.Spec.Env {
		env[e.Name] = e.Value
	}
	hash := front.f.platform.pools[0].Hash()
	if env["FW_API_TOKEN"] != "fleet-secret" || env["TICKETS_TOKEN"] != "tickets-secret" || env[pools.EnvSpec] != hash || made.Status.Phase != "Running" {
		t.Fatalf("the new Task = %v, %s", env, made.Status.Phase)
	}
	eventually(t, "the pool says why the old Task stays", func() bool {
		ps, err := front.f.Pools(ctx)
		return err == nil && len(ps) == 1 && slices.ContainsFunc(ps[0].Gaps, func(g string) bool {
			return strings.Contains(g, "triage-1 runs an older configuration and holds the active run 01RUN")
		})
	})
	if t1 := ax.get("team", "triage-1"); t1 == nil || t1.Spec.Env[1].Value != "oldhash" {
		t.Fatal("the Task with an active run was replaced")
	}

	// The run ends: the old Task goes, and the pool makes it again.
	setRunState(front.f, "01RUN", fw.StateCompleted)
	tasks.set("team/triage-1", view.Status{Idle: true, IdleSince: &idle, Runs: view.RunCounts{Total: 1}})
	eventually(t, "the old Task is replaced", func() bool {
		t1 := ax.get("team", "triage-1")
		if t1 == nil {
			return false
		}
		for _, e := range t1.Spec.Env {
			if e.Name == pools.EnvSpec {
				return e.Value == hash
			}
		}
		return false
	})
	eventually(t, "the pool is ready", func() bool {
		ps, err := front.f.Pools(ctx)
		return err == nil && len(ps) == 1 && ps[0].Wanted == 2 && ps[0].Ready == 2 && len(ps[0].Gaps) == 0
	})
	if got := ax.names(); !slices.Equal(got, []string{"team/notes", "team/triage-0", "team/triage-1"}) {
		t.Fatalf("Tasks = %v", got)
	}
	if n := tasks.count("team/triage-1"); n == 0 {
		t.Fatal("the server replaced the Task without a status read")
	}
}

// A pool is visible to the people who can view its agent.
func TestPoolsFollowTheAccessOfTheAgent(t *testing.T) {
	t.Setenv("FW_ATESPACE", "team")
	t.Setenv("FW_API_TOKEN", "fleet-secret")
	t.Setenv("FRONT_TICKETS_TOKEN", "tickets-secret")
	ax := newPoolAX(t)
	tasks := newFakeTasks(t)
	block := strings.Replace(poolBlock, "description: Sorts new tickets.", "description: Sorts new tickets.\n          access: {owners: [alice]}\n      access: {admins: [\"kind:token\"]}", 1)
	front := newMember(t, fw.NewMemBus(), "front", func(cfg *fw.Config, fc *Config) {
		cfg.Access.Default = "closed"
		poolFront(t, ax, strings.TrimPrefix(tasks.srv.URL, "http://"), block)(cfg, fc)
	})
	alice := fw.WithPrincipal(context.Background(), &fw.Principal{Subject: "alice", Kind: fw.KindUser})
	bob := fw.WithPrincipal(context.Background(), &fw.Principal{Subject: "bob", Kind: fw.KindUser})
	if ps, err := front.f.Pools(alice); err != nil || len(ps) != 1 || ps[0].Name != "triage" {
		t.Fatalf("Pools of alice = %+v, %v", ps, err)
	}
	if ps, err := front.f.Pools(bob); err != nil || len(ps) != 0 {
		t.Fatalf("Pools of bob = %+v, %v", ps, err)
	}
}

// A variable that a pool copies from the front and that the front lacks
// stops the start, and the error names it.
func TestPoolsNeedTheirVariables(t *testing.T) {
	t.Setenv("FW_ATESPACE", "team")
	t.Setenv("FW_API_TOKEN", "fleet-secret")
	ax := newPoolAX(t)
	cfg := fw.DefaultConfig()
	cfg.Home = t.TempDir()
	fc := Config{Name: "front", Interval: time.Second, Expire: DefaultExpire, Egress: "fw.events"}
	poolFront(t, ax, "router", poolBlock)(cfg, &fc)
	a, err := fw.New(fw.WithConfig(cfg), fw.WithoutDefaults(), fw.WithAuthn(fw.NoAuthn{}), fw.WithModel(textModel("done")),
		fw.WithLedger(fw.NewMemLedger()), fw.WithBus(fw.NewMemBus()), fw.WithLogger(quietLogger()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close(context.Background()) })
	if _, err := Open(context.Background(), a, fc); err == nil || !strings.Contains(err.Error(), "FRONT_TICKETS_TOKEN") {
		t.Fatalf("Open = %v", err)
	}
}

// The example of the documentation is a valid configuration of a front
// with a pool.
func TestPoolsExample(t *testing.T) {
	cfg, err := config.Load(filepath.Join("..", "examples", "pools", "fw.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	fc, err := Load(cfg)
	if err != nil {
		t.Fatal(err)
	}
	list, err := LoadPools(cfg, fc)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Name != "triage" || list[0].Wanted() != 3 || list[0].Intake != "fw.jobs.triage" || len(list[0].Warnings()) != 0 {
		t.Fatalf("pools = %+v", list)
	}
}

// A child that a member placed in another Task counts as work of the
// member while the child has not spoken, but not once the view dropped the
// child, and not after childWait.
func TestPoolsCountOnlyAWaitingChild(t *testing.T) {
	t.Setenv("FW_ATESPACE", "team")
	front := newMember(t, fw.NewMemBus(), "front", nil)
	now := time.Now()
	inject(front.f, "team/triage-0", "ax:team/triage-0", "team", MemberLive, now,
		&fw.Run{ID: "01PARENT", Agent: "triage", State: fw.StateCompleted, Principal: "alice", CreatedAt: now})
	p := front.f.platform
	p.mu.Lock()
	p.children["01FRESH"] = childLink{parent: "01PARENT", member: "team/child-a", at: now}
	p.children["01DROPPED"] = childLink{parent: "01PARENT", member: "team/child-b", at: now}
	p.gone["01DROPPED"] = forgotRun{run: &fw.Run{ID: "01DROPPED"}, member: "team/child-b", at: now}
	p.children["01SILENT"] = childLink{parent: "01PARENT", member: "team/child-c", at: now.Add(-2 * childWait)}
	p.mu.Unlock()
	if got := (poolCheck{p}).Active("team/triage-0"); !slices.Equal(got, []string{"01FRESH"}) {
		t.Fatalf("Active = %v", got)
	}
}

// Busy asks the Task with the token that it holds, and reads each run that
// the view holds: a run that the Task does not have, or that ended, is no
// work, a run that it hides from the token is an error, and a run that
// works counts.
func TestPoolsAskTheTaskForEachRun(t *testing.T) {
	t.Setenv("FW_ATESPACE", "team")
	t.Setenv("FW_API_TOKEN", "fleet-secret")
	t.Setenv("FRONT_TICKETS_TOKEN", "tickets-secret")
	ax := newPoolAX(t)
	tasks := newFakeTasks(t)
	front := newMember(t, fw.NewMemBus(), "front", poolFront(t, ax, strings.TrimPrefix(tasks.srv.URL, "http://"), poolBlock))
	// A Task of the Task list, so the view keeps it as a member.
	ax.put(&v1alpha1.Task{Metadata: &v1alpha1.ObjectMeta{Name: "worker-0", Atespace: "team"}, Spec: &v1alpha1.TaskSpec{Image: "x"}}, "Running")
	eventually(t, "the Task is a member", func() bool {
		ms, err := front.f.Members(context.Background())
		return err == nil && memberStates(ms)["team/worker-0"] == MemberLive
	})
	idle := time.Now().Add(-time.Hour)
	tasks.set("team/worker-0", view.Status{Idle: true, IdleSince: &idle})
	tasks.tokens["team/worker-0"] = "the-old-token"
	tasks.runs["team/worker-0"] = map[string]*fw.Run{
		"01ENDED":  {ID: "01ENDED", State: fw.StateCompleted},
		"01WORKS":  {ID: "01WORKS", State: fw.StateInputRequired},
		"01HIDDEN": {ID: "01HIDDEN"},
	}
	check := poolCheck{front.f.platform}
	ctx := context.Background()
	if n, err := check.Busy(ctx, "team/worker-0", "the-old-token", []string{"01GONE", "01ENDED"}); err != nil || n != 0 {
		t.Fatalf("Busy of stale runs = %d, %v", n, err)
	}
	if n, err := check.Busy(ctx, "team/worker-0", "the-old-token", []string{"01ENDED", "01WORKS"}); err != nil || n != 1 {
		t.Fatalf("Busy of a working run = %d, %v", n, err)
	}
	if _, err := check.Busy(ctx, "team/worker-0", "", []string{"01HIDDEN"}); err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("Busy of a hidden run = %v", err)
	}
	if _, err := check.Busy(ctx, "team/worker-0", "a-wrong-token", nil); err == nil {
		t.Fatal("Busy with a wrong token answered")
	}
}

// A Task of a pool that appears after the front started reaches the view
// of the pools at the next read of the Task list, also while the reads of
// the runs of a Task that just started hold the poll. Before, the pools
// showed the Tasks of the last pass, which runs after those reads, so
// during a roll of every pool the page showed the Tasks that were up when
// the front started for many minutes, and a reload read the same.
func TestPoolsShowANewTaskWhileReadsWait(t *testing.T) {
	t.Setenv("FW_ATESPACE", "team")
	t.Setenv("FW_API_TOKEN", "fleet-secret")
	t.Setenv("FRONT_TICKETS_TOKEN", "tickets-secret")
	ctx := context.Background()
	ax := newPoolAX(t)
	tasks := newFakeTasks(t)
	front := newMember(t, fw.NewMemBus(), "front", poolFront(t, ax, strings.TrimPrefix(tasks.srv.URL, "http://"), poolBlock))
	eventually(t, "the pool is ready", func() bool {
		ps, err := front.f.Pools(ctx)
		return err == nil && len(ps) == 1 && ps[0].Ready == 2
	})
	// Another replica of the front, or a roll, brings a Task of the pool
	// that answers no read yet.
	release := make(chan struct{})
	tasks.mu.Lock()
	tasks.block["team/triage-2"] = release
	tasks.mu.Unlock()
	t.Cleanup(func() { close(release) })
	made := ax.get("team", "triage-0")
	made.Metadata.Name = "triage-2"
	ax.put(made, "Running")
	eventually(t, "the pools list the new Task", func() bool {
		ps, err := front.f.Pools(ctx)
		if err != nil || len(ps) != 1 {
			return false
		}
		for _, task := range ps[0].Tasks {
			if task.Name == "triage-2" && task.Phase == "Running" && task.Current {
				return true
			}
		}
		return false
	})
}
