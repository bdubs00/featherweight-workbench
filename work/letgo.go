package work

// This file lets a person let a stopped card go (docs/ARCHITECTURE.md
// section 26.5): a card that stopped and that nobody will move again, such
// as a lost chat of last week, otherwise stays on the board however old it
// is. POST /v1/work/letgo and the MCP tool work_letgo call LetGo. The front
// keeps who let the card go and when in a key-value bucket of the bus, one
// entry per card, and the board then shows the card in Done with the
// verdict let-go. The runs keep their state, so Results counts them as
// before: a let-go is a word of the board only.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/faces"
	"github.com/bdubs00/featherweight-workbench/internal/bucket"
)

// VerdictLetGo is the verdict of a card that a person let go.
const VerdictLetGo = "let-go"

// bucketConfig is the bucket of the board: the cards that people let go.
var bucketConfig = bucket.Config{
	Name:        "WORK",
	Description: "The board of the work of a featherweight fleet: the stopped cards that people let go, one entry per card.",
	History:     4,
	MaxValue:    4 << 10,
}

// Errors of a let-go. A route answers ErrOff with 503, ErrNoCard with
// 404, and ErrNotStopped with 409.
var (
	ErrOff        = errors.New("work: the board keeps no let-go")
	ErrNoCard     = errors.New("work: no card")
	ErrNotStopped = errors.New("work: the card did not stop")
)

// LetGo is the let-go of one card: the card, the subject and the name of
// the person, and the time.
type LetGo struct {
	Card string    `json:"card"`
	By   string    `json:"by"`
	Name string    `json:"name,omitempty"`
	At   time.Time `json:"at"`
}

// LetGoRequest is the body of POST /v1/work/letgo and the arguments of the
// tool work_letgo: the id of the card.
type LetGoRequest struct {
	Card string `json:"card"`
}

// Store keeps the let-go of the cards of one agent, with a copy of each
// entry by its revision, so a read of the board reads again only the
// entries that changed.
type Store struct {
	b   bucket.Bucket
	now func() time.Time

	mu   sync.Mutex
	seen map[string]seenLetGo
}

type seenLetGo struct {
	rev uint64
	l   LetGo
}

var stores sync.Map // *fw.Agent to *Store

// Of returns the store of the let-go of an agent, or nil.
func Of(a *fw.Agent) *Store {
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

// NewStore returns a store on a bucket.
func NewStore(b bucket.Bucket) *Store {
	return &Store{b: b, now: time.Now, seen: map[string]seenLetGo{}}
}

// Service is the service of the board (fw.Service). It opens the bucket on
// the bus of the configuration. On a bus that keeps no store, the board
// still answers, and a let-go answers 503.
func Service(ctx context.Context, a *fw.Agent) (io.Closer, error) {
	log := a.Logger().With("component", "work")
	b, closeFn, err := bucket.Open(ctx, a.Config(), bucketConfig, log)
	if errors.Is(err, bucket.ErrNoStore) {
		where, _ := bucket.Where(a.Config(), bucketConfig)
		log.Warn("work: the board keeps no let-go; it lives "+where, "bus", a.Config().Bus.Driver)
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("work: %w", err)
	}
	s := NewStore(b)
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

func letGoKey(card string) string {
	return "letgo." + base64.RawURLEncoding.EncodeToString([]byte(card))
}

// All returns every let-go by the id of its card.
func (s *Store) All(ctx context.Context) (map[string]LetGo, error) {
	out := map[string]LetGo{}
	if s == nil {
		return out, nil
	}
	entries, err := bucket.EntriesUnder(ctx, s.b, "letgo")
	if err != nil {
		return nil, fmt.Errorf("work: read the let-go of the cards: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	live := map[string]bool{}
	for _, e := range entries {
		live[e.Key] = true
		if x, ok := s.seen[e.Key]; ok && x.rev == e.Revision {
			out[x.l.Card] = x.l
			continue
		}
		got, err := s.b.Get(ctx, e.Key)
		if err != nil {
			continue
		}
		var l LetGo
		if json.Unmarshal(got.Value, &l) != nil || l.Card == "" {
			continue
		}
		s.seen[e.Key] = seenLetGo{rev: got.Revision, l: l}
		out[l.Card] = l
	}
	for k := range s.seen {
		if !live[k] {
			delete(s.seen, k)
		}
	}
	return out, nil
}

// keep writes the let-go of a card, unless the card has one: the first
// let-go stays. It returns the let-go that the bucket holds.
func (s *Store) keep(ctx context.Context, l LetGo) (LetGo, error) {
	key := letGoKey(l.Card)
	kept := l
	_, err := bucket.Modify(ctx, s.b, key, 8, func(cur []byte, exists bool) ([]byte, error) {
		if exists {
			var old LetGo
			if json.Unmarshal(cur, &old) == nil && old.Card == l.Card {
				kept = old
				return nil, nil
			}
		}
		kept = l
		return json.Marshal(l)
	})
	if err != nil {
		return LetGo{}, fmt.Errorf("work: keep the let-go of the card %s: %w", l.Card, err)
	}
	return kept, nil
}

// letGo moves a stopped card that a person let go to done, with the
// verdict let-go. A card that moved since, such as a run that a person
// continued, keeps its stage.
func (b *board) letGo(d *draft) {
	l, ok := b.in.LetGo[d.ID]
	if !ok || d.Stage != StageStopped {
		return
	}
	at := l.At
	d.Stage, d.Since, d.Ended, d.Verdict, d.Next = StageDone, at, &at, VerdictLetGo, ""
	d.Reason = "Let go by " + letGoName(l, b.in.Me) + "."
}

// StoppedFor is how long a card stays stopped before it lets itself go: a
// stopped card that nobody moved for this long moves to done, with the
// verdict let-go and one line that says why. So the stopped work of a
// script or of last week does not make the owner its janitor (plan A8).
const StoppedFor = 7 * 24 * time.Hour

// letItself moves a card that stopped StoppedFor ago or earlier to done,
// with the verdict let-go, at the time that it let itself go. A card that a
// person let go keeps that let-go.
func (b *board) letItself(d *draft) {
	if d.Stage != StageStopped || d.Since.IsZero() || b.in.Now.Sub(d.Since) < StoppedFor {
		return
	}
	at := d.Since.Add(StoppedFor)
	d.Stage, d.Since, d.Ended, d.Verdict, d.Next = StageDone, at, &at, VerdictLetGo, ""
	d.Reason = "It stopped " + Span(StoppedFor) + " ago and nobody moved it, so it let itself go."
}

// letGoName names the person of a let-go for the caller me: you for the
// caller, else the name of the person, else a person.
func letGoName(l LetGo, me string) string {
	switch {
	case me != "" && l.By == me:
		return "you"
	case strings.TrimSpace(l.Name) != "":
		return strings.TrimSpace(l.Name)
	case l.By != "" && !opaqueID(l.By):
		return l.By
	}
	return "a person"
}

// LetGoCard lets the stopped card req.Card go for the caller in ctx, a
// person, at the time now, and returns the card in done. Only a person
// whose move the card is (next is you), or an owner of one of its agents,
// can do it. A card that a person let go already stays as it is, so a
// second call does nothing. A card that is not stopped is ErrNotStopped.
func LetGoCard(ctx context.Context, a *fw.Agent, now time.Time, req LetGoRequest) (Card, error) {
	id := strings.TrimSpace(req.Card)
	if id == "" {
		return Card{}, fmt.Errorf("%w: card names no card", fw.ErrInvalidRequest)
	}
	s := Of(a)
	if s == nil {
		return Card{}, fmt.Errorf("%w: this server keeps no let-go, because its bus keeps no store", ErrOff)
	}
	p := fw.PrincipalFrom(ctx)
	if p == nil || p.Subject == "" {
		return Card{}, fmt.Errorf("%w: only a person lets a card go", fw.ErrForbidden)
	}
	// Every card that the caller can view, with the widest window, read
	// now and not from the cache.
	ans, err := build(ctx, a, now, Query{Days: 90, All: true})
	if err != nil {
		return Card{}, err
	}
	i := slices.IndexFunc(ans.Cards, func(c Card) bool { return c.ID == id })
	if i < 0 {
		return Card{}, fmt.Errorf("%w: no card %s that you can view", ErrNoCard, id)
	}
	c := ans.Cards[i]
	switch {
	case c.Stage == StageDone && c.Verdict == VerdictLetGo:
		return c, nil
	case c.Stage != StageStopped:
		return Card{}, fmt.Errorf("%w: the card %s is %s, and only a stopped card can go", ErrNotStopped, id, stageWords[c.Stage])
	}
	if c.Next != NextYou && !ownsAnAgent(ctx, a, c.Agents) {
		return Card{}, fmt.Errorf("%w: only a person whose move the card is, or an owner of its agent, lets it go", fw.ErrForbidden)
	}
	l, err := s.keep(ctx, LetGo{Card: id, By: p.Subject, Name: personName(p.Name), At: now.UTC().Truncate(time.Second)})
	if err != nil {
		return Card{}, err
	}
	cacheOf(a).clear()
	at := l.At
	c.Stage, c.Since, c.Ended, c.Verdict, c.Next = StageDone, at, &at, VerdictLetGo, ""
	c.Reason = "Let go by " + letGoName(l, p.Subject) + "."
	return c, nil
}

// ownsAnAgent reports whether the caller in ctx owns one of the agents
// (agent.edit).
func ownsAnAgent(ctx context.Context, a *fw.Agent, agents []string) bool {
	for _, name := range agents {
		if faces.Check(ctx, a, name, fw.ActionAgentEdit) == nil {
			return true
		}
	}
	return false
}

// personName is a name of a person that people read, or "" for an id
// that the issuer made.
func personName(name string) string {
	name = oneLine(name, 256)
	if name == "" || opaqueID(name) {
		return ""
	}
	return name
}

// opaqueID reports whether a subject is an id that says nothing to a
// person: a UUID, or 16 or more hex digits and dashes, also after a prefix
// that names the provider, as bridges words a person.
func opaqueID(subject string) bool {
	if i := strings.LastIndexByte(subject, '|'); i >= 0 {
		subject = subject[i+1:]
	}
	if len(subject) < 16 {
		return false
	}
	for i := 0; i < len(subject); i++ {
		c := subject[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F' || c == '-') {
			return false
		}
	}
	return true
}
