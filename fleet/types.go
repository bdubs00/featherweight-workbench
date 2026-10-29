package fleet

// This file is the contract of the fleet: the record of a member as the
// front computes it from the platforms, the member as a view shows it, the
// View that the surfaces read, and the registry that ties a View to an
// agent. The kernel knows none of it. See docs/ARCHITECTURE.md section 4.

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/bdubs00/featherweight-kernel/ax"
	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/pools"
)

// Defaults and limits of the fleet.
const (
	DefaultInterval = 30 * time.Second
	// DefaultSuspendAfter is how long a Task stays idle before the front
	// suspends it, when the configuration names atespaces and no other
	// value (suspend.go).
	DefaultSuspendAfter = 10 * time.Minute
	DefaultExpire       = 7 * 24 * time.Hour
	// DefaultOrphansAfter is how long an orphan Task sleeps before the
	// front removes it, when the configuration names atespaces and no
	// other value (orphans.go). It is the retention of a child Task of the
	// kernel, so an orphan stays as long as a child of a live parent.
	DefaultOrphansAfter = ax.DefaultChildRetention

	// FinishedRuns is how many finished runs a record holds, newest first.
	FinishedRuns = 50
	// HiddenFinishedRuns is how many finished runs that the lists leave
	// out (Listed), such as check runs, a record holds besides
	// FinishedRuns, newest first. So a check every few minutes never
	// pushes the work of people out of the view, and a check run that
	// ended a moment ago stays in the view for the front that waits for
	// it.
	HiddenFinishedRuns = 10
	// TextLimit is the size in bytes to which a record cuts the input, the
	// output, and the error of a run.
	TextLimit = 2 << 10
	// PendingInputLimit is the largest input of a pending tool call that a
	// record holds. A larger input is left out.
	PendingInputLimit = 4 << 10
	// RecordLimit is the largest record in bytes. The oldest finished runs
	// are left out first.
	RecordLimit = 512 << 10
	// QuietAfter is how many intervals without an event make a member that
	// only the stream knows quiet.
	QuietAfter = 3
)

// HeaderVia names the view that forwarded a request. A member never
// forwards a request that carries it, so a loop cannot form.
const HeaderVia = "Fw-Fleet-Via"

// HeaderOnBehalfOf names the person that a request of the static token of
// the fleet acts for, by the subject of the principal. The front sets it
// when it acts for a person on another member (person.go): the front
// decided the action for that person, and the request carries the static
// token. The kernel of the member honors it from the static token alone
// and records the person as the decider of an approval and as the person
// of a message, a change of a budget, and a cancel. Forward sets it from
// the context alone (WithOnBehalfOf), so the header of a caller never
// reaches a member.
const HeaderOnBehalfOf = "Fw-On-Behalf-Of"

// States of a member in the view.
const (
	MemberLive    = "live"    // the Task runs, or an event arrived within QuietAfter intervals
	MemberQuiet   = "quiet"   // the Task sleeps or is pending, or no event arrived in time
	MemberStopped = "stopped" // the Task is in a phase that does not run again
)

// Record is one member as the front computes it from the platforms
// (platform.go). Runs holds every run that is not finished and the newest
// finished runs, cut to size (see the limits above). Omitted counts the
// finished runs that the record leaves out. Access is the authorization of
// the front, which decides for every worker with its own configuration.
type Record struct {
	Name     string        `json:"name"`
	URL      string        `json:"url,omitempty"`
	Atespace string        `json:"atespace,omitempty"`
	Access   Access        `json:"access"`
	Agents   []RecordAgent `json:"agents"`
	Runs     []*fw.Run     `json:"runs"`
	Omitted  int           `json:"omitted,omitempty"`
	// State is the state of the member when a platform says it, such as the
	// phase of an ax Task. A member that only the stream knows has none.
	State string `json:"state,omitempty"`
	// Parent is the parent member of a child Task of spawn_task, such as
	// "ax:default/manager-0", and "" for any other member. A child serves
	// the profiles of its parent, but it takes no new work of a person:
	// the front sends no job and no start to it (suspend.go).
	Parent string `json:"parent,omitempty"`
	// BlockedReason says why the status of the Task said blocked when the
	// suspend controller last read it: a storage failure stopped the
	// process, so the front suspended the Task to restart it (suspend.go).
	// It is empty when the member is not blocked, and the next event of
	// the member clears it.
	BlockedReason string    `json:"blockedReason,omitempty"`
	UpdatedAt     time.Time `json:"updatedAt"`
}

// Access is the authorization of a member. Default and Admins are set in
// the rules mode only.
type Access struct {
	Mode    string   `json:"mode"`
	Default string   `json:"default,omitempty"`
	Admins  []string `json:"admins,omitempty"`
}

// RecordAgent is one agent profile of a member. Role is the role of the
// caller on the agent: "owner", "operator", or "viewer". It is empty in a
// record, and a view sets it for each caller.
type RecordAgent struct {
	Name        string     `json:"name"`
	Description string     `json:"description,omitempty"`
	Access      *fw.Access `json:"access,omitempty"`
	Teams       []string   `json:"teams,omitempty"` // the teams of the agent (section 8)
	Role        string     `json:"role,omitempty"`
}

// Member is one member as the view shows it to a caller. Agents holds the
// agents that the caller can view. Active and InputRequired count the runs
// that the caller can view. CanForward reports whether this process can
// send a request to the member. BlockedReason is the reason of
// Record.BlockedReason: it tells a person why the front suspended a Task
// that a storage failure blocked.
type Member struct {
	Name          string        `json:"name"`
	URL           string        `json:"url,omitempty"`
	Self          bool          `json:"self,omitempty"`
	Parent        string        `json:"parent,omitempty"`
	State         string        `json:"state"`
	LastSeen      time.Time     `json:"lastSeen"`
	CanForward    bool          `json:"canForward"`
	AccessMode    string        `json:"accessMode"`
	Agents        []RecordAgent `json:"agents"`
	Active        int           `json:"active"`
	InputRequired int           `json:"inputRequired"`
	BlockedReason string        `json:"blockedReason,omitempty"`
}

// View is the view of the other members of the fleet.
//
// Members lists the members that the principal in ctx can see, this
// process first. A member is visible when the caller can view one of its
// agents or one of its runs. Runs lists the runs of the other members that
// the principal can view, newest first, each with Member set. Run returns
// one of them, or fw.ErrRunNotFound when the view does not know the run or
// the principal cannot view it. Every decision follows the rules of the
// owner (section 4.4).
//
// Forward sends req to a member and returns its response, which the caller
// closes. req carries the method, the path under /v1 with its query, the
// body, and the headers Accept, Content-Type, Idempotency-Key, and
// Last-Event-ID. Forward sets the address, the credential from
// fw.CredentialFrom(ctx), HeaderVia, and HeaderOnBehalfOf from
// OnBehalfOf(ctx). It returns ErrNoRoute when the member is unknown, has
// no URL, or has a URL outside the forward globs, and ErrNoCredential when
// ctx carries no bearer credential.
type View interface {
	Name() string
	Members(ctx context.Context) ([]Member, error)
	Runs(ctx context.Context) ([]*fw.Run, error)
	Run(ctx context.Context, id string) (*fw.Run, error)
	Forward(ctx context.Context, member string, req *http.Request) (*http.Response, error)
}

// BatchView is a View that lists the runs of one batch whole. A record
// holds the runs that are not finished and the newest FinishedRuns
// finished runs of a member, so Runs cuts a large job; Batch does not.
//
// Batch returns every run of the batch that the view holds and that the
// principal in ctx can view, each with Member set, newest first. That
// includes the runs of a worker that the view no longer lists, because a
// job counts them. Forgotten counts the runs of the batch that the view
// dropped since it started: a finished run after the expire time, and
// every run of a Task that left the Task list. A view knows only the runs
// that it saw since it started, so a sum over the runs is partial when
// Forgotten is above zero. Forgotten counts only the dropped runs that
// the principal in ctx could view, decided as for the runs, so a batch
// name that two people use tells neither of the runs of the other.
type BatchView interface {
	Batch(ctx context.Context, batch string) (BatchRuns, error)
}

// SettledView is a View that tells whether it holds every run that the
// fleet holds. Settled is true once the view read the Task list of every
// atespace whole, with the runs of the live Tasks, and loaded the index of
// the runs (index.go), which names the runs of the Tasks that sleep. A run
// that a settled view does not hold is in no Task of the fleet now, so the
// bridges say that a seat lost its run only on such a view. A view without
// an index never settles.
type SettledView interface {
	Settled() bool
}

// The fleet tells when it settled.
var _ SettledView = (*Fleet)(nil)

// PoolView is a View that keeps pools of Tasks (pools.go). Pools returns
// the state of every pool whose agent the principal in ctx can view, in
// the order of the configuration, after the last pass of each.
type PoolView interface {
	Pools(ctx context.Context) ([]pools.Status, error)
}

// QueueView is a View that places the jobs of its pools (enqueue.go).
// PoolQueue returns the intake of the pool whose workers take a job of the
// profile agent: the pool of the name pool, else the pool that runs the
// profile, the pool whose agent it is first. It reports false when pool is
// empty and no pool runs the profile, so the job goes to the queue of the
// profile (Queue). Whom the caller can see changes nothing: the place of a
// job is the same for every caller.
type QueueView interface {
	PoolQueue(ctx context.Context, agent, pool string) (queue string, found bool, err error)
}

// The fleet places the jobs of its pools.
var _ QueueView = (*Fleet)(nil)

// BatchRuns is the answer of BatchView.Batch.
//
// Unknown counts the runs of the view that have no identity yet, because
// the view missed their fw.run.queued and no read of their member gave it,
// such as a run of a Task that the stream reached before the one read of
// the Task after a restart of the front. Such a run can be a run of the
// batch, so a sum over the runs is partial when Unknown is above zero, and
// UnknownActive counts those that are not finished. Batch counts them only
// on the members that hold a run of the batch that the caller can view,
// and only while such a run works: a caller learns nothing of the members
// where the caller has no run of the batch, and a run of someone else
// keeps no finished batch from being finished.
//
// Jobs counts the run requests that the caller put on the intake queues
// under the batch, one per idempotency key, from the job record of the
// front (jobs.go), and Queued counts those that no Task took yet: the jobs
// less the top-level runs of the caller that a queue started, those that
// the view forgot included, and never more than the jobs that the queues
// still hold once the front read the queues after the last enqueue. Both
// are zero when the front knows no such record, as for a batch of another
// front or one older than the expire time. JobsShared is true when the
// record lives on the server of the bus, so every replica of the front
// counts the same jobs; else each replica counts the jobs that it sent.
type BatchRuns struct {
	Runs          []*fw.Run
	Forgotten     int
	Unknown       int
	UnknownActive int
	Jobs          int
	Queued        int
	JobsShared    bool
}

// Errors of the fleet. A surface answers ErrNoRoute with 502 and
// ErrNoCredential with 403: the caller is authenticated here, but it
// carries no credential that the owner can check. ErrNoToken is 503: the
// front holds no static token of the fleet, so it acts for no person on
// another member until an operator sets the token.
var (
	ErrNoRoute      = errors.New("fleet: no route to the fleet member")
	ErrNoCredential = errors.New("fleet: the request carries no bearer credential to forward")
	ErrNoToken      = errors.New("fleet: the server holds no static token of the fleet")
)

// views ties a View to the agent that it serves. Start attaches the fleet
// of an agent, and a test attaches a fake.
var views sync.Map // *fw.Agent to View

// Of returns the View of an agent, or nil when the fleet is off.
func Of(a *fw.Agent) View {
	if a == nil {
		return nil
	}
	v, ok := views.Load(a)
	if !ok {
		return nil
	}
	return v.(View)
}

// Attach makes v the View of a. A nil v detaches.
func Attach(a *fw.Agent, v View) {
	if v == nil {
		views.Delete(a)
		return
	}
	views.Store(a, v)
}
