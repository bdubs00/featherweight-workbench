package fleet

// This file finds and removes the orphan Tasks of the fleet: the child
// Tasks whose parent Task is gone (docs/ARCHITECTURE.md section 14.10).
// The kernel in a Task removes its own child Tasks, and those of an earlier
// Task of the same name (the kernel, ax/sweep.go). A pool that replaces a
// Task under another name leaves the child Tasks of the old Task behind,
// and no kernel removes them. The front lists them for an admin, who
// removes them from the page at once, and a sweep of the front removes
// each one that slept for workbench.fleet.orphansAfter.

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/bdubs00/featherweight-kernel/ax"
	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/pools"
	v1alpha1 "github.com/google/ax/pkg/apis/v1alpha1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// deleteTimeout bounds one DeleteTask call of a removal.
const deleteTimeout = 30 * time.Second

// The times of the sweep of the orphan Tasks; tests shorten them. The first
// pass waits for the view to read the Task list and the runs of the live
// Tasks.
var (
	orphanFirst = 5 * time.Minute
	orphanEvery = 15 * time.Minute
)

// The conditions of a Task that the sweep reads, as ax sets them.
const (
	condReady          = "Ready"
	condWorkspaceReady = "WorkspaceReady"
	reasonSuspended    = "TaskSuspended"
	phaseFailed        = "Failed"
)

// ErrNoTaskList means that the fleet reads no Task list, so it cannot tell
// which Tasks lost their parent.
var ErrNoTaskList = errors.New("fleet: the front reads no Task list of the ax API (workbench.fleet.atespaces)")

// ErrTaskList means that a Task list of the ax API failed.
var ErrTaskList = errors.New("fleet: the ax API did not give the Task list")

// Orphan is a child Task whose parent Task is gone, or a Task under such a
// Task. Member is the Task as "<atespace>/<task>", and Parent the parent
// that the Task names in FW_PARENT_MEMBER, as "ax:<atespace>/<task>". Open
// counts the runs of the Task that the view holds as not ended.
type Orphan struct {
	Member    string    `json:"member"`
	Parent    string    `json:"parent"`
	Phase     string    `json:"phase"`
	Pool      string    `json:"pool,omitempty"`
	CreatedAt time.Time `json:"createdAt,omitzero"`
	Open      int       `json:"open"`
}

// Removal is the answer of a removal of the orphan Tasks: the Tasks that
// went, and each Task whose delete failed, with the reason.
type Removal struct {
	Removed []string        `json:"removed"`
	Failed  []RemovalFailed `json:"failed"`
}

// RemovalFailed is one Task that a removal could not delete.
type RemovalFailed struct {
	Member string `json:"member"`
	Error  string `json:"error"`
}

// Orphans lists the orphan Tasks of the atespaces of the fleet of a, each
// after the Tasks under it. It reads the Task lists again, so a Task that a
// person removed a moment ago is not in the answer. Only an admin of the
// process lists them.
func Orphans(ctx context.Context, a *fw.Agent) ([]Orphan, error) {
	p, client, err := orphanPlatform(ctx, a)
	if err != nil {
		return nil, err
	}
	return p.orphans(ctx, client)
}

// OrphansAfter returns how long an orphan Task of the fleet of a sleeps
// before the front removes it, or zero when the sweep is off.
func OrphansAfter(a *fw.Agent) time.Duration {
	if f, _ := Of(a).(*Fleet); f != nil {
		return f.orphansAfter
	}
	return 0
}

// RemoveOrphans deletes the orphan Tasks of the fleet of a, each Task
// before the Task above it, with the runs that they hold. A Task that ax
// no longer has counts as removed. Only an admin of the process removes
// them.
func RemoveOrphans(ctx context.Context, a *fw.Agent) (Removal, error) {
	p, client, err := orphanPlatform(ctx, a)
	if err != nil {
		return Removal{}, err
	}
	return p.removeOrphans(ctx, client)
}

func (p *platform) orphans(ctx context.Context, client v1alpha1.AXClient) ([]Orphan, error) {
	tasks, err := p.orphanTasks(ctx, client)
	if err != nil {
		return nil, err
	}
	open := p.openRuns()
	out := make([]Orphan, 0, len(tasks))
	for _, t := range tasks {
		name := t.GetMetadata().GetAtespace() + "/" + t.GetMetadata().GetName()
		out = append(out, Orphan{
			Member:    name,
			Parent:    taskVar(t, ax.EnvParentMember),
			Phase:     t.GetStatus().GetPhase(),
			Pool:      taskVar(t, pools.EnvPool),
			CreatedAt: createdOf(t),
			Open:      open[name],
		})
	}
	return out, nil
}

func (p *platform) removeOrphans(ctx context.Context, client v1alpha1.AXClient) (Removal, error) {
	out := Removal{Removed: []string{}, Failed: []RemovalFailed{}}
	tasks, err := p.orphanTasks(ctx, client)
	if err != nil {
		return out, err
	}
	for _, t := range tasks {
		name := t.GetMetadata().GetAtespace() + "/" + t.GetMetadata().GetName()
		if err := p.deleteOrphan(ctx, client, t, "an admin removed it", "principal", subjectOf(ctx)); err != nil {
			out.Failed = append(out.Failed, RemovalFailed{Member: name, Error: status.Convert(err).Message()})
			continue
		}
		out.Removed = append(out.Removed, name)
	}
	return out, nil
}

// deleteOrphan deletes the orphan Task t and logs why, with the attributes
// attrs. A Task that ax no longer has counts as deleted.
func (p *platform) deleteOrphan(ctx context.Context, client v1alpha1.AXClient, t *v1alpha1.Task, why string, attrs ...any) error {
	atespace, task := t.GetMetadata().GetAtespace(), t.GetMetadata().GetName()
	name := atespace + "/" + task
	cctx, cancel := context.WithTimeout(ctx, deleteTimeout)
	_, err := client.DeleteTask(cctx, &v1alpha1.DeleteTaskRequest{Atespace: atespace, Name: task})
	cancel()
	if err != nil && status.Code(err) != codes.NotFound {
		p.f.log.Warn("fleet: delete an orphan Task", "member", name, "error", err)
		return err
	}
	p.f.log.Info("fleet: removed an orphan Task", append([]any{"member", name, "parent", taskVar(t, ax.EnvParentMember), "reason", why}, attrs...)...)
	return nil
}

// sweepLoop removes the orphan Tasks that are due (sweepOrphans) soon
// after the start and then every orphanEvery, until ctx ends.
func (p *platform) sweepLoop(ctx context.Context) {
	t := time.NewTimer(orphanFirst)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		p.sweepOrphans(ctx, time.Now())
		t.Reset(orphanEvery)
	}
}

// sweepOrphans removes each orphan Task that is due (orphanDue), each Task
// before the Task above it, and returns them. An orphan above one that
// stays stays too, so no Task loses its parent while the sweep keeps it. A
// Task list that fails removes nothing.
func (p *platform) sweepOrphans(ctx context.Context, now time.Time) []string {
	after := p.f.orphansAfter
	p.mu.Lock()
	client := p.client
	p.mu.Unlock()
	if client == nil || after <= 0 {
		return nil
	}
	tasks, err := p.orphanTasks(ctx, client)
	if err != nil {
		p.f.log.Warn("fleet: the sweep of the orphan Tasks waits for the next pass", "error", err)
		return nil
	}
	open := p.openRuns()
	byMember := map[string]*v1alpha1.Task{}
	for _, t := range tasks {
		byMember["ax:"+t.GetMetadata().GetAtespace()+"/"+t.GetMetadata().GetName()] = t
	}
	keep := map[string]bool{}
	for key, t := range byMember {
		if orphanDue(t, open[strings.TrimPrefix(key, "ax:")], after, now) {
			continue
		}
		for k := key; !keep[k]; {
			keep[k] = true
			up := taskVar(byMember[k], ax.EnvParentMember)
			if byMember[up] == nil {
				break
			}
			k = up
		}
	}
	var removed []string
	for _, t := range tasks {
		key := "ax:" + t.GetMetadata().GetAtespace() + "/" + t.GetMetadata().GetName()
		if keep[key] {
			continue
		}
		if p.deleteOrphan(ctx, client, t, "it slept for "+after.String()) == nil {
			removed = append(removed, strings.TrimPrefix(key, "ax:"))
		}
	}
	return removed
}

// orphanDue reports whether the sweep removes the orphan Task t: the view
// holds no run of it that has not ended (open), and it sleeps since after
// ago, or it failed before it ran. The time of the sleep is the time of the
// condition Ready with the reason TaskSuspended; a Task without that
// condition counts from its creation.
func orphanDue(t *v1alpha1.Task, open int, after time.Duration, now time.Time) bool {
	if open > 0 {
		return false
	}
	switch t.GetStatus().GetPhase() {
	case phaseFailed:
		return taskCondition(t, condWorkspaceReady) == nil
	case phaseSuspended:
		since := createdOf(t)
		if c := taskCondition(t, condReady); c != nil && c.GetReason() == reasonSuspended && c.GetUpdateTime() != nil {
			since = c.GetUpdateTime().AsTime()
		}
		return !since.IsZero() && now.Sub(since) >= after
	}
	return false
}

// taskCondition returns the condition of a Task of the type, or nil.
func taskCondition(t *v1alpha1.Task, typ string) *v1alpha1.Condition {
	for _, c := range t.GetStatus().GetConditions() {
		if c.GetType() == typ {
			return c
		}
	}
	return nil
}

// orphanPlatform returns the platform of the fleet of a and its client of
// the ax API for the caller in ctx: an admin of the process, or the
// process itself.
func orphanPlatform(ctx context.Context, a *fw.Agent) (*platform, v1alpha1.AXClient, error) {
	err := a.Authorize(ctx, fw.ActionAdmin, fw.Target{})
	if errors.Is(err, fw.ErrForbidden) {
		return nil, nil, fmt.Errorf("%w: the orphan Tasks of the fleet need the admin role of the process (access.admins), because a Task can hold the runs of every person", fw.ErrForbidden)
	}
	if err != nil {
		return nil, nil, err
	}
	f, _ := Of(a).(*Fleet)
	if f == nil || f.platform == nil {
		return nil, nil, ErrNoTaskList
	}
	p := f.platform
	p.mu.Lock()
	client := p.client
	p.mu.Unlock()
	if client == nil {
		return nil, nil, ErrNoTaskList
	}
	return p, client, nil
}

// orphanTasks reads the Task list of each atespace of the fleet and
// returns the orphan Tasks (orphansOf). A list that fails stops the read,
// because a Task that the list leaves out looks gone.
func (p *platform) orphanTasks(ctx context.Context, client v1alpha1.AXClient) ([]*v1alpha1.Task, error) {
	listed := map[string][]*v1alpha1.Task{}
	for _, atespace := range p.atespaces {
		tasks, err := listTasks(ctx, client, atespace)
		if err != nil {
			return nil, fmt.Errorf("%w of the atespace %s: %s", ErrTaskList, atespace, status.Convert(err).Message())
		}
		listed[atespace] = tasks
	}
	return orphansOf(listed), nil
}

// openRuns counts the runs that the view holds as not ended, by member.
func (p *platform) openRuns() map[string]int {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := map[string]int{}
	for _, r := range p.runs {
		if !r.run.State.Terminal() {
			out[r.member]++
		}
	}
	return out
}

// orphansOf picks the orphan Tasks out of the Task lists, by atespace. A
// Task is an orphan when the parent that it names is not in the list of
// its atespace, when ax created that parent after the Task, so the parent
// is a later Task of the same name, or when the parent is an orphan
// itself. A parent in an atespace that the lists leave out is unknown, so
// its Task stays out of the answer, and so does a Task that ax deletes
// already. The answer holds each Task before the Task above it, so a
// removal in this order never leaves a Task without its parent.
func orphansOf(listed map[string][]*v1alpha1.Task) []*v1alpha1.Task {
	byMember := map[string]*v1alpha1.Task{}
	for atespace, tasks := range listed {
		for _, t := range tasks {
			if name := t.GetMetadata().GetName(); name != "" {
				// The callers name a Task by its atespace and its name.
				t.Metadata.Atespace = atespace
				byMember["ax:"+atespace+"/"+name] = t
			}
		}
	}
	// depth is 0 for a Task that is no orphan, else 1 plus the depth of the
	// parent, so a deeper Task goes first. A cycle of parents is no orphan.
	depth := map[string]int{}
	visiting := map[string]bool{}
	var depthOf func(member string) int
	depthOf = func(member string) int {
		if d, ok := depth[member]; ok {
			return d
		}
		if visiting[member] {
			return 0
		}
		visiting[member] = true
		defer delete(visiting, member)
		t := byMember[member]
		parent := taskVar(t, ax.EnvParentMember)
		d := 0
		if parent != "" {
			space, _, _ := strings.Cut(strings.TrimPrefix(parent, "ax:"), "/")
			above, inList := byMember[parent]
			_, known := listed[space]
			switch {
			case !strings.HasPrefix(parent, "ax:") || !known:
			case !inList:
				d = 1
			case createdOf(above).After(createdOf(t)) && !createdOf(t).IsZero():
				d = 1
			default:
				if pd := depthOf(parent); pd > 0 {
					d = pd + 1
				}
			}
		}
		depth[member] = d
		return d
	}
	var members []string
	for member, t := range byMember {
		if depthOf(member) > 0 && t.GetStatus().GetPhase() != v1alpha1.PhaseTerminating {
			members = append(members, member)
		}
	}
	sort.Slice(members, func(i, j int) bool {
		if di, dj := depth[members[i]], depth[members[j]]; di != dj {
			return di > dj
		}
		return members[i] < members[j]
	})
	out := make([]*v1alpha1.Task, 0, len(members))
	for _, member := range members {
		out = append(out, byMember[member])
	}
	return out
}
