// Package conversations keeps what each person does to the conversations
// of the lists (docs/ARCHITECTURE.md section 4.20): a title of their own
// for a run, and whether the run is hidden from their lists. It changes
// no run: the ledger and the archive keep every run, and a hidden run
// stays readable by its id. A person sees only their own titles and
// hides only from their own lists.
//
// The package runs as a service of the kernel (fw.RegisterService). On a
// NATS bus the entries live in the key-value bucket <stream>_CONVERSATIONS,
// one entry per person, so every replica of the front reads one truth. On
// the bus mem they live in the memory of the process. On another bus the
// package logs once that the conversations keep no titles, and its route
// and its tool answer that. It mounts PATCH /v1/conversations/{run} on the
// native surface (native.Extend) and adds the MCP tool conversation_patch
// (mcp.RegisterTools). The lists of runs of the workbench read the entries
// of the caller with Mark.
package conversations

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/surface/native"
	"github.com/bdubs00/featherweight-workbench/fleet"
	"github.com/bdubs00/featherweight-workbench/internal/bucket"
	"github.com/bdubs00/featherweight-workbench/surface/mcp"
)

func init() {
	fw.RegisterService(Start)
	native.Extend(mount)
	mcp.RegisterTools(tools)
}

// bucketConfig is the bucket of the entries of the people.
var bucketConfig = bucket.Config{
	Name:        "CONVERSATIONS",
	Description: "What each person of a featherweight fleet does to the conversations of their lists: a title of their own and hidden, one entry per person.",
	History:     1,
	MaxValue:    512 << 10,
}

// Bounds of the entries.
const (
	// MaxTitleRunes bounds the title of a person for a conversation.
	MaxTitleRunes = 100
	// MaxRuns bounds the conversations of one person that have a title or
	// are hidden.
	MaxRuns = 1000
	// attempts bounds the tries of a write that other replicas race.
	attempts = 8
)

// Errors of the package.
var (
	// ErrOff means that the process keeps no entries. A route answers 503.
	ErrOff = errors.New("conversations: off")
	// ErrNoPerson means that the caller names no person. A route answers
	// 403.
	ErrNoPerson = errors.New("conversations: no person")
)

// Pref is what one person did to one conversation: a title of their own,
// whether it is hidden from their lists, and when they last changed it.
type Pref struct {
	Title  string    `json:"title,omitempty"`
	Hidden bool      `json:"hidden,omitempty"`
	At     time.Time `json:"at"`
}

// entry is the value of the entry of one person: the prefs by run id.
type entry struct {
	Runs map[string]Pref `json:"runs"`
}

// Patch is the body of PATCH /v1/conversations/{run} and the arguments of
// the tool conversation_patch: a title of the caller for the run, where an
// empty title gives the run its own title back, and hidden. A field that
// is absent stays as it was.
type Patch struct {
	Title  *string `json:"title,omitempty"`
	Hidden *bool   `json:"hidden,omitempty"`
}

// Answer is the answer of a patch: the run, the title of the caller, and
// whether the run is hidden from the lists of the caller.
type Answer struct {
	Run     string `json:"run"`
	MyTitle string `json:"myTitle,omitempty"`
	Hidden  bool   `json:"hidden"`
}

// Store keeps the entries of the people of one agent.
type Store struct {
	a   *fw.Agent
	b   bucket.Bucket
	log *slog.Logger
	now func() time.Time
}

// stores ties the store of an agent to the agent.
var stores sync.Map // *fw.Agent to *Store

// Of returns the store of an agent, or nil when the process keeps none.
func Of(a *fw.Agent) *Store {
	if a == nil {
		return nil
	}
	if v, ok := stores.Load(a); ok {
		return v.(*Store)
	}
	return nil
}

// Attach makes s the store of a. A nil s detaches.
func Attach(a *fw.Agent, s *Store) {
	if s == nil {
		stores.Delete(a)
		return
	}
	stores.Store(a, s)
}

// New returns the store of an agent on a bucket.
func New(a *fw.Agent, b bucket.Bucket, log *slog.Logger) *Store {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Store{a: a, b: b, log: log, now: time.Now}
}

// Start is the service of the conversations (fw.Service). It opens the
// bucket on the bus of the configuration and attaches the store to the
// agent. On a bus that keeps no store it logs once that the conversations
// keep no titles and returns nil.
func Start(ctx context.Context, a *fw.Agent) (io.Closer, error) {
	log := a.Logger().With("component", "conversations")
	b, closeFn, err := bucket.Open(ctx, a.Config(), bucketConfig, log)
	if errors.Is(err, bucket.ErrNoStore) {
		where, _ := bucket.Where(a.Config(), bucketConfig)
		log.Warn("conversations: the conversations keep no titles and hide nothing; they live "+where, "bus", a.Config().Bus.Driver)
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("conversations: %w", err)
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

// keyOf is the key of the entry of a person: p.<subject in base64url>.
func keyOf(subject string) string {
	return "p." + base64.RawURLEncoding.EncodeToString([]byte(subject))
}

// person returns the subject of the caller in ctx, or ErrNoPerson.
func person(ctx context.Context) (string, error) {
	p := fw.PrincipalFrom(ctx)
	if p == nil || p.Subject == "" {
		return "", fmt.Errorf("%w: a conversation gets a title or hides for a signed-in person", ErrNoPerson)
	}
	return p.Subject, nil
}

// Prefs returns what the caller in ctx did to their conversations, by run
// id. A caller without a person, and a process without a store, have
// none.
func (s *Store) Prefs(ctx context.Context) (map[string]Pref, error) {
	if s == nil {
		return nil, nil
	}
	subject, err := person(ctx)
	if err != nil {
		return nil, nil
	}
	e, err := s.b.Get(ctx, keyOf(subject))
	if errors.Is(err, bucket.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("conversations: read the entry of %s: %w", subject, err)
	}
	var v entry
	if err := json.Unmarshal(e.Value, &v); err != nil {
		return nil, fmt.Errorf("conversations: the entry of %s does not read: %w", subject, err)
	}
	return v.Runs, nil
}

// CheckTitle returns the title in one line with its spaces folded, or an
// error in words for a title that is longer than MaxTitleRunes or holds a
// control character.
func CheckTitle(title string) (string, error) {
	if strings.IndexFunc(title, func(r rune) bool { return unicode.IsControl(r) && !unicode.IsSpace(r) }) >= 0 || !utf8.ValidString(title) {
		return "", fmt.Errorf("%w: a title is one line of text without a control character", fw.ErrInvalidRequest)
	}
	title = strings.Join(strings.Fields(title), " ")
	if utf8.RuneCountInString(title) > MaxTitleRunes {
		return "", fmt.Errorf("%w: a title holds at most %d characters", fw.ErrInvalidRequest, MaxTitleRunes)
	}
	return title, nil
}

// Patch changes what the caller in ctx did to the conversation run, as
// PATCH /v1/conversations/{run} does: a title of their own, where an
// empty title gives the run its own title back, and hidden. The caller
// needs the view of the run, here, on a member, or in the archive; any
// other run is fw.ErrRunNotFound. The run itself does not change.
func (s *Store) Patch(ctx context.Context, run string, p Patch) (Answer, error) {
	if s == nil {
		return Answer{}, fmt.Errorf("%w: this server keeps no titles of conversations, because its bus keeps no store; an admin runs it with the bus nats or mem", ErrOff)
	}
	subject, err := person(ctx)
	if err != nil {
		return Answer{}, err
	}
	run = strings.TrimSpace(run)
	if p.Title == nil && p.Hidden == nil {
		return Answer{}, fmt.Errorf("%w: send title, hidden, or both", fw.ErrInvalidRequest)
	}
	title := ""
	if p.Title != nil {
		if title, err = CheckTitle(*p.Title); err != nil {
			return Answer{}, err
		}
	}
	if err := viewable(ctx, s.a, run); err != nil {
		return Answer{}, err
	}
	var out Pref
	_, err = bucket.Modify(ctx, s.b, keyOf(subject), attempts, func(cur []byte, exists bool) ([]byte, error) {
		v := entry{Runs: map[string]Pref{}}
		if exists {
			if err := json.Unmarshal(cur, &v); err != nil {
				return nil, fmt.Errorf("conversations: the entry of %s does not read: %w", subject, err)
			}
			if v.Runs == nil {
				v.Runs = map[string]Pref{}
			}
		}
		pref, had := v.Runs[run]
		if p.Title != nil {
			pref.Title = title
		}
		if p.Hidden != nil {
			pref.Hidden = *p.Hidden
		}
		pref.At = s.now().UTC()
		out = pref
		if pref.Title == "" && !pref.Hidden {
			if !had {
				return nil, nil
			}
			delete(v.Runs, run)
		} else {
			if !had && len(v.Runs) >= MaxRuns {
				return nil, fmt.Errorf("%w: you keep %d conversations with a title of your own or hidden, the most that the server keeps; give one its own title back, or show it again", fw.ErrInvalidRequest, MaxRuns)
			}
			v.Runs[run] = pref
		}
		return json.Marshal(v)
	})
	if err != nil {
		return Answer{}, err
	}
	s.log.Info("conversations: a person changed a conversation", "run", run, "titled", out.Title != "", "hidden", out.Hidden, "principal", subject)
	return Answer{Run: run, MyTitle: out.Title, Hidden: out.Hidden}, nil
}

// viewable reports with nil whether the caller in ctx can view the run:
// a run of this process, of a member of the view, or of the archive.
func viewable(ctx context.Context, a *fw.Agent, id string) error {
	notFound := fmt.Errorf("%w: no run that you can view has the id %s", fw.ErrRunNotFound, id)
	if id == "" {
		return notFound
	}
	switch _, err := a.Get(ctx, id); {
	case err == nil:
		return nil
	case errors.Is(err, fw.ErrForbidden):
		// A run that the caller cannot view is no run of their lists.
		return notFound
	case !errors.Is(err, fw.ErrRunNotFound):
		return err
	}
	if v := fleet.Of(a); v != nil {
		if _, err := v.Run(ctx, id); err == nil {
			return nil
		} else if !errors.Is(err, fw.ErrRunNotFound) {
			return err
		}
	}
	if _, ok, err := fleet.ArchivedRun(ctx, a, id); err != nil {
		return err
	} else if ok {
		return nil
	}
	return notFound
}

// Listed is a run of a list of the caller with what the caller did to it:
// the title of the caller, and hidden, which only a list with hidden runs
// shows.
type Listed struct {
	MyTitle string
	Hidden  bool
}

// Mark returns the runs of a list of the caller in ctx without the runs
// that the caller hid, unless withHidden, and what the caller did to each
// run that it returns, by id. A process without a store, and a caller
// without a person, mark nothing. A store that does not answer marks
// nothing either, and the log says so, so a list never fails for it.
func Mark(ctx context.Context, a *fw.Agent, runs []*fw.Run, withHidden bool) ([]*fw.Run, map[string]Listed) {
	s := Of(a)
	prefs, err := s.Prefs(ctx)
	if err != nil {
		s.log.Warn("conversations: the list shows the runs without the titles of the person", "error", err)
		return runs, nil
	}
	if len(prefs) == 0 {
		return runs, nil
	}
	out := make([]*fw.Run, 0, len(runs))
	marks := map[string]Listed{}
	for _, r := range runs {
		p, ok := prefs[r.ID]
		if ok && p.Hidden && !withHidden {
			continue
		}
		out = append(out, r)
		if ok {
			marks[r.ID] = Listed{MyTitle: p.Title, Hidden: p.Hidden}
		}
	}
	return out, marks
}
