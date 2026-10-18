package fleet

// This file is the contract of the fleet: the record that a member
// publishes, the member as a view shows it, the View that the surfaces
// read, and the registry that ties a View to an agent. The kernel knows
// none of it. See docs/ARCHITECTURE.md section 4.

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// Defaults and limits of the fleet.
const (
	DefaultInterval = 30 * time.Second
	// DefaultSuspendAfter is how long a Task stays idle before the front
	// suspends it, when the configuration names atespaces and no other
	// value (suspend.go).
	DefaultSuspendAfter = 10 * time.Minute
	DefaultExpire       = 7 * 24 * time.Hour
	Version             = 1 // the version of the record shape

	// FinishedRuns is how many finished runs a record holds, newest first.
	FinishedRuns = 50
	// TextLimit is the size in bytes to which a record cuts the input, the
	// output, and the error of a run.
	TextLimit = 2 << 10
	// PendingInputLimit is the largest input of a pending tool call that a
	// record holds. A larger input is left out.
	PendingInputLimit = 4 << 10
	// RecordLimit is the largest record in bytes. The oldest finished runs
	// are left out first.
	RecordLimit = 512 << 10
	// QuietAfter is how many intervals without a message make a member
	// quiet.
	QuietAfter = 3
)

// HeaderVia names the view that forwarded a request. A member never
// forwards a request that carries it, so a stale record cannot cause a loop.
const HeaderVia = "Fw-Fleet-Via"

// Bus event types of the fleet. The data of EvRecord is a Record, and the
// data of EvAlive is an Alive.
const (
	EvRecord = "fw.fleet.record"
	EvAlive  = "fw.fleet.alive"
)

// States of a member in the view.
const (
	MemberLive    = "live"    // published within QuietAfter intervals
	MemberQuiet   = "quiet"   // did not; under ax usually a Task that sleeps
	MemberStopped = "stopped" // sent a last record at shutdown
)

// Record is what a member publishes about itself. Runs holds every run
// that is not finished and the newest finished runs, cut to size (see the
// limits above). Omitted counts the finished runs that the record leaves
// out. Interval is the interval of the member, so a view judges its
// heartbeats by its own setting.
type Record struct {
	Version  int           `json:"version"`
	Name     string        `json:"name"`
	URL      string        `json:"url,omitempty"`
	Atespace string        `json:"atespace,omitempty"`
	Interval time.Duration `json:"interval,omitempty"`
	Access   Access        `json:"access"`
	Agents   []RecordAgent `json:"agents"`
	Runs     []*fw.Run     `json:"runs"`
	Omitted  int           `json:"omitted,omitempty"`
	Stopped  bool          `json:"stopped,omitempty"`
	// State is the state of the member when a platform says it, such as the
	// phase of an ax Task, in place of the judgment by heartbeats.
	State     string    `json:"state,omitempty"`
	UpdatedAt time.Time `json:"updatedAt"`
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

// Alive is the heartbeat of a member.
type Alive struct {
	Name string    `json:"name"`
	At   time.Time `json:"at"`
}

// Member is one member as the view shows it to a caller. Agents holds the
// agents that the caller can view. Active and InputRequired count the runs
// that the caller can view. CanForward reports whether this process can
// send a request to the member.
type Member struct {
	Name          string        `json:"name"`
	URL           string        `json:"url,omitempty"`
	Self          bool          `json:"self,omitempty"`
	State         string        `json:"state"`
	LastSeen      time.Time     `json:"lastSeen"`
	CanForward    bool          `json:"canForward"`
	AccessMode    string        `json:"accessMode"`
	Agents        []RecordAgent `json:"agents"`
	Active        int           `json:"active"`
	InputRequired int           `json:"inputRequired"`
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
// fw.CredentialFrom(ctx), and HeaderVia. It returns ErrNoRoute when the
// member is unknown, has no URL, or has a URL outside the forward globs,
// and ErrNoCredential when ctx carries no bearer credential.
type View interface {
	Name() string
	Members(ctx context.Context) ([]Member, error)
	Runs(ctx context.Context) ([]*fw.Run, error)
	Run(ctx context.Context, id string) (*fw.Run, error)
	Forward(ctx context.Context, member string, req *http.Request) (*http.Response, error)
}

// Errors of the fleet. A surface answers ErrNoRoute with 502 and
// ErrNoCredential with 403: the caller is authenticated here, but it
// carries no credential that the owner can check.
var (
	ErrNoRoute      = errors.New("fleet: no route to the fleet member")
	ErrNoCredential = errors.New("fleet: the request carries no bearer credential to forward")
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

// Token turns a member name into one subject token: each character other
// than a letter, a digit, "-", or "_" becomes "_".
func Token(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "_"
	}
	return b.String()
}

// RecordSubject is the subject of the record of a member.
func RecordSubject(prefix, name string) string { return prefix + ".members." + Token(name) }

// AliveSubject is the subject of the heartbeat of a member.
func AliveSubject(prefix, name string) string { return prefix + ".alive." + Token(name) }
