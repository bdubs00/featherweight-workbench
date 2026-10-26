// Package playbooks keeps the playbooks of a fleet (docs/ARCHITECTURE.md
// section 24.2). A playbook is the plan of a bridge without a title, saved
// under a name: a goal, the placement, the seats, the caps, and the
// delegation. A duty opens a bridge from it on a schedule, such as weekly
// patching (do.open), and a person opens one from the page.
//
// The package runs as a service of the kernel (fw.RegisterService). On a
// NATS bus the playbooks live in the key-value bucket <stream>_PLAYBOOKS,
// one entry per playbook with the last ten values, so every replica of
// the front reads one truth. On the bus mem they live in the memory of the
// process. On another bus the package logs once that the playbooks are off
// and serves nothing. Each write is a compare-and-swap on the revision
// that the writer read. The package mounts the routes /v1/playbooks on the
// native surface (native.Extend) and adds the MCP tools of the playbooks
// (mcp.RegisterTools). Each tool calls the Go function of its route.
//
// The access rules are those of the bridges: a person who can open a
// bridge from a playbook can read it and write it. That is its owner, and
// a person with the operator role on the agent of each of its seats.
package playbooks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/surface/native"
	"github.com/bdubs00/featherweight-workbench/bridges"
	"github.com/bdubs00/featherweight-workbench/internal/bucket"
	"github.com/bdubs00/featherweight-workbench/surface/mcp"
)

func init() {
	fw.RegisterService(Start)
	native.Extend(mount)
	mcp.RegisterTools(tools)
}

// bucketConfig is the bucket of the playbooks.
var bucketConfig = bucket.Config{
	Name:        "PLAYBOOKS",
	Description: "The playbooks of a featherweight fleet: the plan of a bridge without a title, one entry per playbook.",
	History:     10,
	MaxValue:    128 << 10,
}

// Errors of the playbooks.
var (
	// ErrNotFound means that no playbook that the caller can read has the
	// name. A route answers 404, so a caller learns nothing of a playbook
	// that it cannot read.
	ErrNotFound = errors.New("playbooks: not found")
	// ErrConflict means that the playbook changed since the writer read
	// it. A route answers 409.
	ErrConflict = errors.New("playbooks: conflict")
	// ErrOff means that the process keeps no playbook, or no bridge to
	// open from one. A route answers 503.
	ErrOff = errors.New("playbooks: off")
)

// ProblemsError is a playbook that does not fit, with each problem in
// words. Each problem starts with the dotted path of its field and a
// colon, such as "seats[0].handle: ...".
type ProblemsError struct{ Problems []string }

func (e *ProblemsError) Error() string { return strings.Join(e.Problems, " ") }

// storeAttempts bounds the tries of a write that other writers race.
const storeAttempts = 8

// namePattern is the form of the name of a playbook, as of a duty.
var namePattern = regexp.MustCompile(`^[a-z0-9_-]{1,63}$`)

// Playbook is a playbook as a person writes it: the body of POST
// /v1/bridges without a title, with the seats in Seats.
type Playbook struct {
	Name       string                   `json:"name"`
	Goal       string                   `json:"goal"`
	Placement  string                   `json:"placement,omitempty"`
	Seats      []bridges.SeatSpec       `json:"seats"`
	Caps       *bridges.CapsPatch       `json:"caps,omitempty"`
	Delegation *bridges.DelegationPatch `json:"delegation,omitempty"`
}

// Spec returns the plan of a bridge of the playbook with the title.
func (p *Playbook) Spec(title string) bridges.OpenSpec {
	return bridges.OpenSpec{Title: title, Goal: p.Goal, Placement: p.Placement, Members: append([]bridges.SeatSpec(nil), p.Seats...), Caps: p.Caps, Delegation: p.Delegation}
}

// View is a playbook as a read gives it: the playbook with the revision of
// its entry, its owner, and the time of its last change.
type View struct {
	Playbook
	Revision  uint64    `json:"revision"`
	Owner     string    `json:"owner"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// Summary is a playbook in a list: its name, goal, placement, seats,
// revision, owner, and the time of its last change.
type Summary struct {
	Name      string             `json:"name"`
	Goal      string             `json:"goal"`
	Placement string             `json:"placement,omitempty"`
	Seats     []bridges.SeatSpec `json:"seats"`
	Revision  uint64             `json:"revision"`
	Owner     string             `json:"owner"`
	UpdatedAt time.Time          `json:"updatedAt"`
}

// Body is the body of a write: the playbook, and the revision that the
// writer read, which makes the write a compare-and-swap. A read is a valid
// body: the write takes and leaves out the fields that only a read has.
type Body struct {
	Playbook
	Revision  uint64          `json:"revision,omitempty"`
	Owner     string          `json:"owner,omitempty"`
	UpdatedAt json.RawMessage `json:"updatedAt,omitempty"`
}

// stored is a playbook in the bucket, with its owner and its last change.
type stored struct {
	Playbook
	Owner     string    `json:"owner"`
	OwnerKind string    `json:"ownerKind,omitempty"`
	UpdatedAt time.Time `json:"updatedAt"`
	UpdatedBy string    `json:"updatedBy,omitempty"`

	rev uint64
}

func (st *stored) view() View {
	return View{Playbook: st.Playbook, Revision: st.rev, Owner: st.Owner, UpdatedAt: st.UpdatedAt}
}

// Service is the playbooks of one agent. Build it with Start, or New in a
// test.
type Service struct {
	a   *fw.Agent
	b   bucket.Bucket
	log *slog.Logger
	now func() time.Time
	// bridges returns the bridges of the agent, which decide the roles and
	// check the plans. A test replaces it.
	bridges func() *bridges.Store
}

// services ties the playbooks of an agent to the agent.
var services sync.Map // *fw.Agent to *Service

// Of returns the playbooks of an agent, or nil when the process keeps
// none.
func Of(a *fw.Agent) *Service {
	if a == nil {
		return nil
	}
	if v, ok := services.Load(a); ok {
		return v.(*Service)
	}
	return nil
}

// Attach makes s the playbooks of a. A nil s detaches.
func Attach(a *fw.Agent, s *Service) {
	if s == nil {
		services.Delete(a)
		return
	}
	services.Store(a, s)
}

// New returns the playbooks of an agent on a bucket.
func New(a *fw.Agent, b bucket.Bucket, log *slog.Logger) *Service {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Service{a: a, b: b, log: log, now: time.Now, bridges: func() *bridges.Store { return bridges.Of(a) }}
}

// Start is the service of the playbooks (fw.Service). It opens the bucket
// on the bus of the configuration and attaches the playbooks to the agent.
// On a bus that keeps no store it logs once that the playbooks are off and
// returns nil.
func Start(ctx context.Context, a *fw.Agent) (io.Closer, error) {
	log := a.Logger().With("component", "playbooks")
	b, closeFn, err := bucket.Open(ctx, a.Config(), bucketConfig, log)
	if errors.Is(err, bucket.ErrNoStore) {
		where, _ := bucket.Where(a.Config(), bucketConfig)
		log.Warn("playbooks: the playbooks are off; they live "+where, "bus", a.Config().Bus.Driver)
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("playbooks: %w", err)
	}
	s := New(a, b, log)
	Attach(a, s)
	return closer(func() {
		if Of(a) == s {
			Attach(a, nil)
		}
		if closeFn != nil {
			closeFn()
		}
	}), nil
}

type closer func()

func (c closer) Close() error {
	c()
	return nil
}

// CheckName returns an error in words for a name that is not the name of
// a playbook.
func CheckName(name string) error {
	if !namePattern.MatchString(name) {
		return fmt.Errorf("the name %q must be 1 to 63 of a-z, 0-9, '-', and '_'", name)
	}
	return nil
}

// store returns the bridges, or ErrOff.
func (s *Service) store() (*bridges.Store, error) {
	b := s.bridges()
	if b == nil {
		return nil, fmt.Errorf("%w: the bridges are off in this process, so no bridge opens from a playbook", ErrOff)
	}
	return b, nil
}

// get reads a playbook of the bucket, or ErrNotFound.
func (s *Service) get(ctx context.Context, name string) (*stored, error) {
	if CheckName(name) != nil {
		return nil, notFound(name)
	}
	e, err := s.b.Get(ctx, name)
	if errors.Is(err, bucket.ErrNotFound) {
		return nil, notFound(name)
	}
	if err != nil {
		return nil, fmt.Errorf("playbooks: read the playbook %s: %w", name, err)
	}
	var st stored
	if err := json.Unmarshal(e.Value, &st); err != nil {
		return nil, fmt.Errorf("playbooks: the playbook %s does not read: %w", name, err)
	}
	st.rev = e.Revision
	return &st, nil
}

// notFound returns ErrNotFound with the name.
func notFound(name string) error {
	return fmt.Errorf("%w: no playbook that you can read is named %s", ErrNotFound, name)
}

// caller names who acts in ctx: the subject and the kind of the
// principal, else fw-workbench for the process itself.
func caller(ctx context.Context) (string, string) {
	if p := fw.PrincipalFrom(ctx); p != nil && p.Subject != "" {
		return p.Subject, p.Kind
	}
	return "fw-workbench", ""
}

// operatesAll checks that the caller in ctx has the operator role on the
// agent of each seat, as POST /v1/bridges checks it. It returns
// fw.ErrForbidden with the first agent that the caller cannot operate.
func operatesAll(ctx context.Context, b *bridges.Store, seats []bridges.SeatSpec) error {
	seen := map[string]bool{}
	for _, m := range seats {
		key := m.Agent + "\x00" + m.Pool
		if seen[key] {
			continue
		}
		seen[key] = true
		ok, err := b.Operates(ctx, strings.TrimSpace(m.Agent), strings.TrimSpace(m.Pool))
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("%w: the playbook needs the operator role on the agent %s of the seat %s", fw.ErrForbidden, m.Agent, m.Handle)
		}
	}
	return nil
}

// readable reports whether the caller in ctx can read a playbook: the
// process itself, its owner, and a person with the operator role on the
// agent of each of its seats. An error means that a decision point did
// not answer.
func readable(ctx context.Context, b *bridges.Store, st *stored) (bool, error) {
	p := fw.PrincipalFrom(ctx)
	if p == nil || (p.Subject != "" && p.Subject == st.Owner) {
		return true, nil
	}
	err := operatesAll(ctx, b, st.Seats)
	if errors.Is(err, fw.ErrForbidden) {
		return false, nil
	}
	return err == nil, err
}

// load reads a playbook for the caller in ctx, who must be able to read
// it, else ErrNotFound.
func (s *Service) load(ctx context.Context, b *bridges.Store, name string) (*stored, error) {
	st, err := s.get(ctx, name)
	if err != nil {
		return nil, err
	}
	ok, err := readable(ctx, b, st)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, notFound(name)
	}
	return st, nil
}

// List returns the playbooks that the caller in ctx can read, by name.
func (s *Service) List(ctx context.Context) ([]Summary, error) {
	b, err := s.store()
	if err != nil {
		return nil, err
	}
	keys, err := s.b.Keys(ctx)
	if err != nil {
		return nil, fmt.Errorf("playbooks: list the playbooks: %w", err)
	}
	out := []Summary{}
	for _, k := range keys {
		st, err := s.get(ctx, k.Key)
		if errors.Is(err, ErrNotFound) {
			continue // a delete between the list and the read
		}
		if err != nil {
			return nil, err
		}
		ok, err := readable(ctx, b, st)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		out = append(out, Summary{Name: st.Name, Goal: st.Goal, Placement: st.Placement, Seats: seatsOf(st.Seats), Revision: st.rev, Owner: st.Owner, UpdatedAt: st.UpdatedAt})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// seatsOf returns the seats of a playbook, an empty list for none.
func seatsOf(seats []bridges.SeatSpec) []bridges.SeatSpec {
	if seats == nil {
		return []bridges.SeatSpec{}
	}
	return seats
}

// Get returns one playbook for the caller in ctx, who must be able to read
// it.
func (s *Service) Get(ctx context.Context, name string) (View, error) {
	b, err := s.store()
	if err != nil {
		return View{}, err
	}
	st, err := s.load(ctx, b, name)
	if err != nil {
		return View{}, err
	}
	v := st.view()
	v.Seats = seatsOf(v.Seats)
	return v, nil
}

// Lookup returns a playbook by its name for the front itself, with no
// check of the caller, or an error that wraps ErrNotFound. The action
// open of a duty reads its playbook with it.
func (s *Service) Lookup(ctx context.Context, name string) (*Playbook, error) {
	st, err := s.get(ctx, name)
	if err != nil {
		return nil, err
	}
	p := st.Playbook
	return &p, nil
}

// problems lists what does not fit in a playbook, each with the dotted
// path of its field, with the checks of the plan of a bridge.
func problems(b *bridges.Store, name string, p Playbook) []string {
	var out []string
	if err := CheckName(name); err != nil {
		out = append(out, "name: "+sentence(err.Error()))
	}
	for _, q := range b.CheckPlan(p.Spec(""), false) {
		path := q.Path
		if rest, ok := strings.CutPrefix(path, "members"); ok {
			path = "seats" + rest
		}
		out = append(out, path+": "+sentence(q.Text))
	}
	return out
}

// sentence writes words as a sentence: a capital first, a period last.
func sentence(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return s
	}
	s = strings.ToUpper(s[:1]) + s[1:]
	if !strings.HasSuffix(s, ".") {
		s += "."
	}
	return s
}

// Put creates or changes a playbook for the caller in ctx and reports
// whether it is new. The caller needs the operator role on the agent of
// each seat, as to open a bridge from it, and for a change the right to
// read the old playbook. A new playbook belongs to the caller, and a
// change keeps the owner. A body with a revision is a compare-and-swap:
// a stale revision gets ErrConflict. A playbook that does not fit gets a
// *ProblemsError.
func (s *Service) Put(ctx context.Context, name string, body Body) (View, bool, error) {
	b, err := s.store()
	if err != nil {
		return View{}, false, err
	}
	p := body.Playbook
	switch strings.TrimSpace(p.Name) {
	case "", name:
		p.Name = name
	default:
		return View{}, false, &ProblemsError{Problems: []string{fmt.Sprintf("name: The playbook is named %s in the body and %s in the path.", p.Name, name)}}
	}
	p.Goal, p.Placement = strings.TrimSpace(p.Goal), strings.ToLower(strings.TrimSpace(p.Placement))
	for i := range p.Seats {
		m := &p.Seats[i]
		m.Handle, m.Agent, m.Task, m.Pool = strings.TrimSpace(m.Handle), strings.TrimSpace(m.Agent), strings.TrimSpace(m.Task), strings.TrimSpace(m.Pool)
	}
	if q := problems(b, name, p); len(q) > 0 {
		return View{}, false, &ProblemsError{Problems: q}
	}
	if err := operatesAll(ctx, b, p.Seats); err != nil {
		return View{}, false, err
	}
	by, kind := caller(ctx)
	now := s.now().UTC()
	for range storeAttempts {
		old, err := s.load(ctx, b, name)
		switch {
		case errors.Is(err, ErrNotFound):
			if _, gerr := s.get(ctx, name); gerr == nil {
				// The playbook exists, and the caller cannot read it.
				return View{}, false, err
			}
			if body.Revision != 0 {
				return View{}, false, fmt.Errorf("%w: no playbook is named %s; leave out revision to create it", ErrConflict, name)
			}
			st := &stored{Playbook: p, Owner: by, OwnerKind: kind, UpdatedAt: now, UpdatedBy: by}
			value, err := json.Marshal(st)
			if err != nil {
				return View{}, false, fmt.Errorf("playbooks: encode the playbook %s: %w", name, err)
			}
			rev, err := s.b.Create(ctx, name, value)
			if errors.Is(err, bucket.ErrConflict) {
				continue // another writer created it a moment ago
			}
			if err != nil {
				return View{}, false, fmt.Errorf("playbooks: write the playbook %s: %w", name, err)
			}
			st.rev = rev
			s.log.Info("playbooks: a playbook was created", "playbook", name, "by", by)
			return st.view(), true, nil
		case err != nil:
			return View{}, false, err
		}
		if body.Revision != 0 && body.Revision != old.rev {
			return View{}, false, fmt.Errorf("%w: the playbook %s has the revision %d, and the write names %d; read it again", ErrConflict, name, old.rev, body.Revision)
		}
		st := &stored{Playbook: p, Owner: old.Owner, OwnerKind: old.OwnerKind, UpdatedAt: now, UpdatedBy: by}
		value, err := json.Marshal(st)
		if err != nil {
			return View{}, false, fmt.Errorf("playbooks: encode the playbook %s: %w", name, err)
		}
		rev, err := s.b.Update(ctx, name, value, old.rev)
		if errors.Is(err, bucket.ErrConflict) {
			if body.Revision != 0 {
				return View{}, false, fmt.Errorf("%w: another writer changed the playbook %s; read it again", ErrConflict, name)
			}
			continue
		}
		if err != nil {
			return View{}, false, fmt.Errorf("playbooks: write the playbook %s: %w", name, err)
		}
		st.rev = rev
		s.log.Info("playbooks: a playbook was changed", "playbook", name, "by", by, "revision", rev)
		return st.view(), false, nil
	}
	return View{}, false, fmt.Errorf("%w: other writers changed the playbook %s %d times while this write tried; try again", ErrConflict, name, storeAttempts)
}

// Delete removes a playbook for the caller in ctx, who must be able to
// read it. A duty that opens it then fails at its next start, and says
// why.
func (s *Service) Delete(ctx context.Context, name string) error {
	b, err := s.store()
	if err != nil {
		return err
	}
	st, err := s.load(ctx, b, name)
	if err != nil {
		return err
	}
	err = s.b.Delete(ctx, name, st.rev)
	if errors.Is(err, bucket.ErrConflict) {
		return fmt.Errorf("%w: the playbook %s changed since the server read it; try again", ErrConflict, name)
	}
	if err != nil {
		return fmt.Errorf("playbooks: remove the playbook %s: %w", name, err)
	}
	by, _ := caller(ctx)
	s.log.Info("playbooks: a playbook was removed", "playbook", name, "by", by)
	return nil
}
