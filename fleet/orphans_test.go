package fleet

import (
	"context"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/ax"
	"github.com/bdubs00/featherweight-kernel/fw"
	v1alpha1 "github.com/google/ax/pkg/apis/v1alpha1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// orphanAX lists the Tasks of each atespace and deletes them. A delete of
// a Task in fail answers its code.
type orphanAX struct {
	v1alpha1.AXClient
	mu      sync.Mutex
	tasks   map[string][]*v1alpha1.Task
	fail    map[string]codes.Code
	deleted []string
}

func (f *orphanAX) ListTasks(_ context.Context, req *v1alpha1.ListTasksRequest, _ ...grpc.CallOption) (*v1alpha1.ListTasksResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return &v1alpha1.ListTasksResponse{Tasks: append([]*v1alpha1.Task(nil), f.tasks[req.GetAtespace()]...)}, nil
}

func (f *orphanAX) DeleteTask(_ context.Context, req *v1alpha1.DeleteTaskRequest, _ ...grpc.CallOption) (*v1alpha1.DeleteTaskResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	name := req.GetAtespace() + "/" + req.GetName()
	f.deleted = append(f.deleted, name)
	if c, ok := f.fail[name]; ok {
		return nil, status.Error(c, "the delete failed")
	}
	return &v1alpha1.DeleteTaskResponse{}, nil
}

// childTask is a Task that names its parent in FW_PARENT_MEMBER, or a Task
// of a pool without parent.
func childTask(name, parent, phase string, created time.Time) *v1alpha1.Task {
	t := &v1alpha1.Task{Metadata: &v1alpha1.ObjectMeta{Name: name, CreateTime: timestamppb.New(created)}, Spec: &v1alpha1.TaskSpec{}, Status: &v1alpha1.TaskStatus{Phase: phase}}
	if parent != "" {
		t.Spec.Env = append(t.Spec.Env, &v1alpha1.EnvVar{Name: ax.EnvParentMember, Value: parent})
	}
	return t
}

// orphanFleet is the Task list of a lab after a pool roll: the pool Task
// manager-1 replaced manager-0, whose children stay behind.
func orphanFleet() map[string][]*v1alpha1.Task {
	t0 := time.Date(2026, 10, 23, 9, 0, 0, 0, time.UTC)
	return map[string][]*v1alpha1.Task{
		"default": {
			childTask("manager-1", "", "Running", t0.Add(10*time.Hour)),
			// The parent is gone, and a child of the orphan is an orphan too.
			childTask("manager-0-a", "ax:default/manager-0", "Suspended", t0),
			childTask("manager-0-a-x", "ax:default/manager-0-a", "Suspended", t0.Add(time.Hour)),
			// A child of the pool Task that is there stays.
			childTask("manager-1-b", "ax:default/manager-1", "Running", t0.Add(11*time.Hour)),
			// The parent has the name of a later Task: ax created manager-1
			// after this child, so its own parent is gone.
			childTask("helper-c", "ax:default/manager-1", "Suspended", t0.Add(2*time.Hour)),
			// ax deletes this one already.
			childTask("gone-d", "ax:default/manager-0", "Terminating", t0),
			// The parent lives in an atespace that the front does not list.
			childTask("other-e", "ax:elsewhere/x", "Running", t0),
			// A cycle of parents is no orphan.
			childTask("loop-1", "ax:default/loop-2", "Running", t0),
			childTask("loop-2", "ax:default/loop-1", "Running", t0),
		},
	}
}

func names(tasks []*v1alpha1.Task) []string {
	out := []string{}
	for _, t := range tasks {
		out = append(out, t.GetMetadata().GetAtespace()+"/"+t.GetMetadata().GetName())
	}
	return out
}

// An orphan is a child Task whose parent is not in the list, whose parent
// is a later Task of the same name, or whose parent is an orphan. The
// deepest goes first, so a removal never leaves a Task without its parent.
func TestOrphansOf(t *testing.T) {
	got := names(orphansOf(orphanFleet()))
	want := []string{"default/manager-0-a-x", "default/helper-c", "default/manager-0-a"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("orphans = %v, want %v", got, want)
	}
}

// The list counts the open runs of each orphan, and the removal deletes
// each orphan once, the deepest first. A Task that ax no longer has counts
// as removed, and a failed delete comes back with its reason.
func TestRemoveOrphans(t *testing.T) {
	client := &orphanAX{tasks: orphanFleet(), fail: map[string]codes.Code{"default/helper-c": codes.NotFound, "default/manager-0-a": codes.Internal}}
	p := &platform{f: &Fleet{log: quietLogger()}, atespaces: []string{"default"}, runs: map[string]*platformRun{
		"r1": {run: &fw.Run{ID: "r1", State: fw.StateRunning}, member: "default/manager-0-a-x"},
		"r2": {run: &fw.Run{ID: "r2", State: fw.StateCompleted}, member: "default/manager-0-a-x"},
	}}
	ctx := context.Background()
	list, err := p.orphans(ctx, client)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 3 || list[0].Member != "default/manager-0-a-x" || list[0].Parent != "ax:default/manager-0-a" || list[0].Open != 1 || list[0].Phase != "Suspended" || list[2].Open != 0 {
		t.Fatalf("orphans = %+v", list)
	}
	out, err := p.removeOrphans(ctx, client)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"default/manager-0-a-x", "default/helper-c", "default/manager-0-a"}; !reflect.DeepEqual(client.deleted, want) {
		t.Errorf("deleted %v, want %v", client.deleted, want)
	}
	if !reflect.DeepEqual(out.Removed, []string{"default/manager-0-a-x", "default/helper-c"}) || len(out.Failed) != 1 || out.Failed[0] != (RemovalFailed{Member: "default/manager-0-a", Error: "the delete failed"}) {
		t.Errorf("removal = %+v", out)
	}
}
