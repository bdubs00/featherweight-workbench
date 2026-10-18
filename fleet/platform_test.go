package fleet

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	v1alpha1 "github.com/google/ax/pkg/apis/v1alpha1"
	"google.golang.org/grpc"
)

// fakeAX lists Tasks per atespace with a phase each.
type fakeAX struct {
	v1alpha1.UnimplementedAXServer
	addr string

	mu        sync.Mutex
	tasks     map[string]map[string]string // atespace to task name to phase
	suspended []string                     // the Tasks that SuspendTask moved, in order
}

func newFakeAX(t *testing.T, tasks map[string]map[string]string) *fakeAX {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeAX{addr: lis.Addr().String(), tasks: tasks}
	srv := grpc.NewServer()
	v1alpha1.RegisterAXServer(srv, f)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	return f
}

func (f *fakeAX) ListTasks(_ context.Context, req *v1alpha1.ListTasksRequest) (*v1alpha1.ListTasksResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*v1alpha1.Task
	for name, phase := range f.tasks[req.GetAtespace()] {
		out = append(out, &v1alpha1.Task{Metadata: &v1alpha1.ObjectMeta{Name: name, Atespace: req.GetAtespace()}, Status: &v1alpha1.TaskStatus{Phase: phase}})
	}
	return &v1alpha1.ListTasksResponse{Tasks: out}, nil
}

func (f *fakeAX) set(atespace, name, phase string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if phase == "" {
		delete(f.tasks[atespace], name)
		return
	}
	f.tasks[atespace][name] = phase
}

// fakeRouter plays the atenet router for one Task that already holds a run.
func fakeRouter(t *testing.T, actor string, runs []*fw.Run) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("ate-target-actor") != actor || r.Header.Get("Authorization") != "Bearer fleet-secret" || r.URL.Path != "/v1/runs" {
			http.Error(w, "unexpected request", http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"runs": runs})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// newWorker is a bare kernel on the bus: no fleet, an egress stream, and
// a node name.
func newWorker(t *testing.T, bus fw.Bus, node string) *fw.Agent {
	t.Helper()
	cfg := fw.DefaultConfig()
	cfg.Home = t.TempDir()
	cfg.Node = node
	cfg.Bus.Egress = "fw.events"
	cfg.Bus.EgressTypes = EgressTypes
	cfg.Agents = map[string]fw.AgentProfile{"default": {}}
	a, err := fw.New(fw.WithConfig(cfg), fw.WithoutDefaults(), fw.WithAuthn(fw.NoAuthn{}), fw.WithModel(textModel("done")),
		fw.WithLedger(fw.NewMemLedger()), fw.WithBus(bus), fw.WithLogger(quietLogger()))
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

func memberStates(ms []Member) map[string]string {
	out := map[string]string{}
	for _, m := range ms {
		out[m.Name] = m.State
	}
	return out
}

func TestPlatformSources(t *testing.T) {
	t.Setenv("FW_ATESPACE", "team")
	t.Setenv("FW_API_TOKEN", "fleet-secret")
	ctx := context.Background()
	bus := fw.NewMemBus()
	ax := newFakeAX(t, map[string]map[string]string{"team": {"alpha": "Running", "beta": "Suspended", "gamma": "Running"}})
	gammaRun := &fw.Run{ID: "01GAMMA", Agent: "default", State: fw.StateCompleted, Principal: "bob", Seq: 9, CreatedAt: time.Now().Add(-time.Minute), UpdatedAt: time.Now()}
	router := fakeRouter(t, "team/gamma", []*fw.Run{gammaRun})
	front := newMember(t, bus, "front", func(cfg *fw.Config, fc *Config) {
		fc.Atespaces = []string{"team"}
		fc.Egress = "fw.events"
		cfg.AX.Server = ax.addr
		cfg.AX.Router = strings.TrimPrefix(router.URL, "http://")
	})
	worker := newWorker(t, bus, "ax:team/alpha")

	// The Tasks of the atespace are members, with the state of their phase.
	eventually(t, "the front sees the Tasks", func() bool {
		ms, err := front.f.Members(ctx)
		st := memberStates(ms)
		return err == nil && st["team/alpha"] == MemberLive && st["team/beta"] == MemberQuiet && st["team/gamma"] == MemberLive
	})
	ms, _ := front.f.Members(ctx)
	for _, m := range ms {
		if m.Name == "team/alpha" && (m.URL != "ax:team/alpha" || !m.CanForward) {
			t.Fatalf("alpha = %+v", m)
		}
	}
	// A run of a worker arrives through the egress stream, with its person.
	r, err := worker.Run(as("ada"), "hello")
	if err != nil || r.State != fw.StateCompleted {
		t.Fatalf("worker run = %+v, %v", r, err)
	}
	eventually(t, "the run of the worker is in the view", func() bool {
		got, err := front.f.Run(ctx, r.ID)
		return err == nil && got.Member == "team/alpha" && got.State == fw.StateCompleted && got.Principal == "ada" && got.Agent == "default"
	})
	// The runs of a Task that ran before the front listened come from one
	// read through the router.
	eventually(t, "the run of gamma is in the view", func() bool {
		got, err := front.f.Run(ctx, "01GAMMA")
		return err == nil && got.Member == "team/gamma" && got.Principal == "bob"
	})
	runs, err := front.f.Runs(ctx)
	if err != nil || len(runs) != 2 {
		t.Fatalf("Runs = %d, %v", len(runs), err)
	}
	// The person who started a run sees it, and the observed agent carries
	// the access block of this process.
	runs, err = front.f.Runs(as("ada"))
	if err != nil || len(runs) == 0 {
		t.Fatalf("Runs as ada = %d, %v", len(runs), err)
	}
	ms, _ = front.f.Members(as("ada"))
	for _, m := range ms {
		if m.Name == "team/alpha" && (len(m.Agents) != 1 || m.Agents[0].Name != "default" || m.Active != 0) {
			t.Fatalf("alpha as ada = %+v", m)
		}
	}
	// A Task that leaves the list leaves the view, and a Task that wakes up
	// becomes live.
	ax.set("team", "beta", "")
	ax.set("team", "gamma", "Suspended")
	eventually(t, "the list follows the Tasks", func() bool {
		ms, err := front.f.Members(ctx)
		st := memberStates(ms)
		_, hasBeta := st["team/beta"]
		return err == nil && !hasBeta && st["team/gamma"] == MemberQuiet
	})
	// A worker outside ax is visible through its events alone, without an
	// address.
	outside := newWorker(t, bus, "box:8080")
	if _, err := outside.Run(as("ada"), "hello"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the worker outside ax is in the view", func() bool {
		ms, err := front.f.Members(ctx)
		for _, m := range ms {
			if m.Name == "box:8080" && m.State == MemberLive && !m.CanForward && m.URL == "" {
				return err == nil
			}
		}
		return false
	})
}

func TestPlatformConfig(t *testing.T) {
	for _, e := range []string{EnvSubject, EnvAtespaces, EnvEgress, EnvName, EnvURL} {
		t.Setenv(e, "")
	}
	t.Setenv("FW_ATESPACE", "")
	cfg := fw.DefaultConfig()
	t.Setenv(EnvSubject, "fw.fleet")
	t.Setenv(EnvAtespaces, "team, lab ,")
	t.Setenv(EnvEgress, "fw.events")
	c, err := Load(cfg)
	if err != nil || len(c.Atespaces) != 2 || c.Atespaces[1] != "lab" || c.Egress != "fw.events" {
		t.Fatalf("Load = %+v, %v", c, err)
	}
	t.Setenv(EnvEgress, "fw.>")
	if _, err := Load(cfg); err == nil || !strings.Contains(err.Error(), "workbench.fleet.egress") {
		t.Fatalf("bad egress = %v", err)
	}
	t.Setenv(EnvEgress, "fw.events")
	t.Setenv(EnvAtespaces, "team/x")
	if _, err := Load(cfg); err == nil || !strings.Contains(err.Error(), "atespaces[0]") {
		t.Fatalf("bad atespace = %v", err)
	}
	t.Setenv(EnvAtespaces, "team")
	t.Setenv(EnvSubject, "")
	if _, err := Load(cfg); err == nil || !strings.Contains(err.Error(), "need workbench.fleet.subject") {
		t.Fatalf("no subject = %v", err)
	}
	if name, u, space := memberOf("ax:team/alpha"); name != "team/alpha" || u != "ax:team/alpha" || space != "team" {
		t.Fatalf("memberOf ax = %q %q %q", name, u, space)
	}
	if name, u, space := memberOf("box:8080"); name != "box:8080" || u != "" || space != "" {
		t.Fatalf("memberOf host = %q %q %q", name, u, space)
	}
	for phase, want := range map[string]string{"Running": MemberLive, "Suspended": MemberQuiet, "Pending": MemberQuiet, "": MemberQuiet, "Failed": MemberStopped} {
		if got := stateOfPhase(phase); got != want {
			t.Fatalf("stateOfPhase(%q) = %q, want %q", phase, got, want)
		}
	}
}
