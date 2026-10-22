package pools

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	v1alpha1 "github.com/google/ax/pkg/apis/v1alpha1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
	"gopkg.in/yaml.v3"
)

// fakeAX is the ax API in memory, with the rules that the pools rely on:
// a Task is created suspended and is immutable, a resume runs it and sets
// the condition WorkspaceReady, and a Workspace is written whole. It
// records every call that changes something, in order.
type fakeAX struct {
	v1alpha1.AXClient // nil: a call that the fake does not serve panics

	mu         sync.Mutex
	tasks      map[string]*v1alpha1.Task      // by atespace/name
	workspaces map[string]*v1alpha1.Workspace // by atespace/name
	calls      []string
	clock      int64
	// createErr, resumeErr, and deleteErr fail the call for a Task name.
	createErr, resumeErr, deleteErr map[string]error
}

func newFakeAX() *fakeAX {
	return &fakeAX{tasks: map[string]*v1alpha1.Task{}, workspaces: map[string]*v1alpha1.Workspace{},
		createErr: map[string]error{}, resumeErr: map[string]error{}, deleteErr: map[string]error{}}
}

func (f *fakeAX) record(format string, args ...any) {
	f.calls = append(f.calls, fmt.Sprintf(format, args...))
}

// put stores a Task as another party made it, with a phase.
func (f *fakeAX) put(t *v1alpha1.Task, phase string, started bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := proto.Clone(t).(*v1alpha1.Task)
	f.clock++
	c.Metadata.CreateTime = timestamppb.New(time.Unix(1_700_000_000+f.clock, 0))
	c.Status = &v1alpha1.TaskStatus{Phase: phase}
	if started {
		c.Status.Conditions = append(c.Status.Conditions, &v1alpha1.Condition{Type: conditionWorkspaceReady, Status: "True"})
	}
	f.tasks[c.Metadata.Atespace+"/"+c.Metadata.Name] = c
}

// list returns the Tasks of an atespace, as ListTasks does.
func (f *fakeAX) list(atespace string) []*v1alpha1.Task {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*v1alpha1.Task
	for k, t := range f.tasks {
		if strings.HasPrefix(k, atespace+"/") {
			out = append(out, proto.Clone(t).(*v1alpha1.Task))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Metadata.Name < out[j].Metadata.Name })
	return out
}

func (f *fakeAX) task(atespace, name string) *v1alpha1.Task {
	f.mu.Lock()
	defer f.mu.Unlock()
	if t, ok := f.tasks[atespace+"/"+name]; ok {
		return proto.Clone(t).(*v1alpha1.Task)
	}
	return nil
}

func (f *fakeAX) took() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.calls
	f.calls = nil
	return out
}

func (f *fakeAX) GetTask(_ context.Context, req *v1alpha1.GetTaskRequest, _ ...grpc.CallOption) (*v1alpha1.Task, error) {
	if t := f.task(req.GetAtespace(), req.GetName()); t != nil {
		return t, nil
	}
	return nil, status.Errorf(codes.NotFound, "task %q not found", req.GetName())
}

func (f *fakeAX) CreateTask(_ context.Context, req *v1alpha1.CreateTaskRequest, _ ...grpc.CallOption) (*v1alpha1.Task, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t := proto.Clone(req.GetTask()).(*v1alpha1.Task)
	k := t.Metadata.Atespace + "/" + t.Metadata.Name
	f.record("create %s", k)
	if err := f.createErr[t.Metadata.Name]; err != nil {
		return nil, err
	}
	if _, ok := f.tasks[k]; ok {
		return nil, status.Errorf(codes.FailedPrecondition, "task %s already exists and is immutable", k)
	}
	if err := v1alpha1.ValidateTask(t); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	f.clock++
	t.Metadata.CreateTime = timestamppb.New(time.Unix(1_700_000_000+f.clock, 0))
	t.Status = &v1alpha1.TaskStatus{Phase: phaseSuspended}
	f.tasks[k] = t
	return proto.Clone(t).(*v1alpha1.Task), nil
}

func (f *fakeAX) ResumeTask(_ context.Context, req *v1alpha1.ResumeTaskRequest, _ ...grpc.CallOption) (*v1alpha1.Task, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := req.GetAtespace() + "/" + req.GetName()
	f.record("resume %s", k)
	if err := f.resumeErr[req.GetName()]; err != nil {
		return nil, err
	}
	t, ok := f.tasks[k]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "task %q not found", req.GetName())
	}
	t.Status.Phase = phaseRunning
	has := false
	for _, c := range t.Status.Conditions {
		has = has || c.Type == conditionWorkspaceReady
	}
	if !has {
		t.Status.Conditions = append(t.Status.Conditions, &v1alpha1.Condition{Type: conditionWorkspaceReady, Status: "True"})
	}
	return proto.Clone(t).(*v1alpha1.Task), nil
}

func (f *fakeAX) DeleteTask(_ context.Context, req *v1alpha1.DeleteTaskRequest, _ ...grpc.CallOption) (*v1alpha1.DeleteTaskResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := req.GetAtespace() + "/" + req.GetName()
	f.record("delete %s", k)
	if err := f.deleteErr[req.GetName()]; err != nil {
		return nil, err
	}
	if _, ok := f.tasks[k]; !ok {
		return nil, status.Errorf(codes.NotFound, "task %q not found", req.GetName())
	}
	delete(f.tasks, k)
	return &v1alpha1.DeleteTaskResponse{}, nil
}

func (f *fakeAX) GetWorkspace(_ context.Context, req *v1alpha1.GetWorkspaceRequest, _ ...grpc.CallOption) (*v1alpha1.Workspace, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if ws, ok := f.workspaces[req.GetAtespace()+"/"+req.GetName()]; ok {
		return proto.Clone(ws).(*v1alpha1.Workspace), nil
	}
	return nil, status.Errorf(codes.NotFound, "workspace %q not found", req.GetName())
}

func (f *fakeAX) UpdateWorkspace(_ context.Context, req *v1alpha1.UpdateWorkspaceRequest, _ ...grpc.CallOption) (*v1alpha1.Workspace, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ws := proto.Clone(req.GetWorkspace()).(*v1alpha1.Workspace)
	if err := v1alpha1.ValidateWorkspace(ws); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	k := ws.Metadata.Atespace + "/" + ws.Metadata.Name
	f.record("workspace %s", k)
	f.workspaces[k] = ws
	return proto.Clone(ws).(*v1alpha1.Workspace), nil
}

func (f *fakeAX) DeleteWorkspace(_ context.Context, req *v1alpha1.DeleteWorkspaceRequest, _ ...grpc.CallOption) (*v1alpha1.DeleteWorkspaceResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := req.GetAtespace() + "/" + req.GetName()
	f.record("delete workspace %s", k)
	delete(f.workspaces, k)
	return &v1alpha1.DeleteWorkspaceResponse{}, nil
}

// fakeCheck plays the view and the status reads of the front: active
// holds the runs that the view knows by member, busy the active runs that
// a Task answers, and failing the members that do not answer.
type fakeCheck struct {
	mu      sync.Mutex
	active  map[string][]string
	busy    map[string]int
	failing map[string]bool
	reads   []string
	// late holds the runs that the stream reports for a member right after
	// its status read, as a job that a worker takes in between.
	late map[string][]string
	// tokens holds the token of each status read, in order.
	tokens []string
}

func newFakeCheck() *fakeCheck {
	return &fakeCheck{active: map[string][]string{}, busy: map[string]int{}, failing: map[string]bool{}}
}

func (c *fakeCheck) Active(member string) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.active[member]
}

func (c *fakeCheck) Busy(_ context.Context, member, token string, _ []string) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reads = append(c.reads, member)
	c.tokens = append(c.tokens, token)
	if runs, ok := c.late[member]; ok {
		c.active[member] = runs
	}
	if c.failing[member] {
		return 0, fmt.Errorf("the router answered 502")
	}
	return c.busy[member], nil
}

// The fixture is a pool of three workers of the profile triage, with a
// model behind a proxy, an MCP server with one write tool on ask, and a
// variable that the front gives from its own environment.
const testImage = "ghcr.io/example/fw@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

const fixture = `
pools:
  - name: triage
    replicas: 3
    image: ` + testImage + `
    env:
      - name: OPENAI_API_KEY
        value: the-proxy-needs-no-key
      - name: TICKETS_TOKEN
        fromEnv: FRONT_TICKETS_TOKEN
    egress:
      hosts:
        - http://nats.agents.svc.cluster.local:8080
        - http://model-proxy.models.svc.cluster.local:8080
        - https://mcp.example.com
    resources:
      requests: {cpu: 500m, memory: 1Gi}
    files:
      - path: notes/triage.md
        content: Sort each ticket into one queue.
    fw:
      model: openai/some-model
      effort: low
      bus: {url: "ws://nats.agents.svc.cluster.local:8080"}
      providers:
        openai: {baseUrl: "http://model-proxy.models.svc.cluster.local:8080/v1", apiKeyEnv: OPENAI_API_KEY}
      policy: {default: allow, ask: [tickets.create_ticket]}
      tools:
        builtin: [files, clock]
        mcp:
          - name: tickets
            endpoint: https://mcp.example.com/mcp
            auth: {bearer: TICKETS_TOKEN}
      agents:
        triage:
          description: Sorts new tickets.
          system: You sort new tickets.
          tools: [files, tickets.search, tickets.create_ticket]
`

// testFront is the front of the fixture.
func testFront() Front {
	return Front{
		Atespaces:   []string{"team"},
		Egress:      "fw.events",
		Intake:      "fw.jobs",
		EgressTypes: []string{fw.EvRunQueued, fw.EvRunFinished},
		MaxBytes:    4096,
		Bus:         fw.BusConfig{Driver: "nats", URL: "nats://nats.agents.svc.cluster.local:4222"},
		AX:          fw.AXConfig{Server: "ax-server:8080", Router: "atenet-router"},
		TokenEnv:    "FW_API_TOKEN",
	}
}

// configOf returns a configuration whose block workbench is the YAML text.
func configOf(t *testing.T, text string) *fw.Config {
	t.Helper()
	var n yaml.Node
	if err := yaml.Unmarshal([]byte(text), &n); err != nil {
		t.Fatal(err)
	}
	cfg := fw.DefaultConfig()
	cfg.Extensions = map[string]yaml.Node{block: *n.Content[0]}
	return cfg
}

// loadFixture loads text with the front of the fixture.
func loadFixture(t *testing.T, text string) []*Pool {
	t.Helper()
	list, err := Load(configOf(t, text), testFront())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return list
}

// testLookup is the environment of the front in the tests.
func testLookup(name string) (string, bool) {
	v, ok := map[string]string{"FW_API_TOKEN": "front-secret-token", "FRONT_TICKETS_TOKEN": "tickets-secret"}[name]
	return v, ok
}
