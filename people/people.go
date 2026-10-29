// Package people names the people of a front (docs/ARCHITECTURE.md
// section 18.6). A page and a text name a person by a display name, never
// by the subject of the sign-in, which is often an id that the issuer made,
// such as the UUID of Cloudflare Access. The display name of a person is,
// in this order:
//
//  1. the name claim of the sign-in, when the issuer gives one;
//  2. a name that the person set (PUT /v1/people/me);
//  3. the part of the email before the @, in readable form, so
//     jane.q.doe@example.com reads as Jane Q Doe;
//  4. the subject, when it is no id that the issuer made;
//  5. else "A person".
//
// The static token of the front is "Admin token". The front keeps what it
// learns of each person in a key-value bucket of the bus, one entry per
// subject, so a name survives a restart of the front and reaches every
// replica: the claims of a browser sign-in, and the name of the principal
// of each call of a route that learns it. A name never enters a principal,
// so no rule of the access matches a person by it.
package people

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/surface/native"
	"github.com/bdubs00/featherweight-workbench/internal/bucket"
)

func init() {
	fw.RegisterService(Service)
	native.Extend(mount)
}

// Names of a person that is no person of a sign-in.
const (
	// AdminToken names the static token of the front.
	AdminToken = "Admin token"
	// SomePerson names a person whose name the front does not know.
	SomePerson = "A person"
)

// MaxName bounds a name that a person sets, in characters.
const MaxName = 64

// Record is what the front knows of the name of one person: the name claim
// of the sign-in, the email, and the name that the person set.
type Record struct {
	Subject string    `json:"subject"`
	Claim   string    `json:"claim,omitempty"`
	Email   string    `json:"email,omitempty"`
	Set     string    `json:"set,omitempty"`
	At      time.Time `json:"at"`
}

// bucketConfig is the bucket of the names of the people.
var bucketConfig = bucket.Config{
	Name:        "PEOPLE",
	Description: "The names of the people of a featherweight front: the name claim of the sign-in, the email, and the name that a person set, one entry per subject.",
	History:     2,
	MaxValue:    4 << 10,
}

// cacheFor is how long a replica keeps a record before it reads the
// bucket again, so a name that another replica learned reaches it.
const cacheFor = time.Minute

// maxCached bounds the records that a replica keeps.
const maxCached = 10000

// Store keeps the names of the people of one front.
type Store struct {
	b   bucket.Bucket
	now func() time.Time

	mu    sync.Mutex
	cache map[string]cached
}

type cached struct {
	rec Record
	at  time.Time
}

// NewStore returns a store on a bucket.
func NewStore(b bucket.Bucket) *Store {
	return &Store{b: b, now: time.Now, cache: map[string]cached{}}
}

var stores sync.Map // *fw.Agent to *Store

// Of returns the store of an agent, or nil.
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

// Service opens the bucket of the names on the bus of the configuration
// (fw.Service). On a bus that keeps no store, the names come from the
// calls alone, and a person cannot set a name.
func Service(ctx context.Context, a *fw.Agent) (io.Closer, error) {
	log := a.Logger().With("component", "people")
	b, closeFn, err := bucket.Open(ctx, a.Config(), bucketConfig, log)
	if errors.Is(err, bucket.ErrNoStore) {
		where, _ := bucket.Where(a.Config(), bucketConfig)
		log.Debug("people: the front keeps no names of people; they live "+where, "bus", a.Config().Bus.Driver)
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("people: %w", err)
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

func keyOf(subject string) string {
	return "person." + base64.RawURLEncoding.EncodeToString([]byte(subject))
}

// Get returns the record of a subject, from the copy of this replica when
// it is fresh, else from the bucket. A subject without a record has an
// empty one.
func (s *Store) Get(ctx context.Context, subject string) Record {
	if s == nil || subject == "" {
		return Record{Subject: subject}
	}
	now := s.now()
	s.mu.Lock()
	c, ok := s.cache[subject]
	s.mu.Unlock()
	if ok && now.Sub(c.at) < cacheFor {
		return c.rec
	}
	rec := Record{Subject: subject}
	if e, err := s.b.Get(ctx, keyOf(subject)); err == nil {
		_ = json.Unmarshal(e.Value, &rec)
		rec.Subject = subject
	} else if !errors.Is(err, bucket.ErrNotFound) && ok {
		// A bucket that does not answer leaves the copy as it is.
		return c.rec
	}
	s.keep(rec, now)
	return rec
}

// keep puts a record in the copy of this replica.
func (s *Store) keep(rec Record, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.cache) >= maxCached {
		clear(s.cache)
	}
	s.cache[rec.Subject] = cached{rec: rec, at: at}
}

// change changes the record of a subject with f and writes it when f
// reports a change.
func (s *Store) change(ctx context.Context, subject string, f func(*Record) bool) (Record, error) {
	var out Record
	_, err := bucket.Modify(ctx, s.b, keyOf(subject), 8, func(cur []byte, exists bool) ([]byte, error) {
		rec := Record{Subject: subject}
		if exists {
			_ = json.Unmarshal(cur, &rec)
			rec.Subject = subject
		}
		out = rec
		if !f(&rec) {
			return nil, nil
		}
		rec.At = s.now().UTC().Truncate(time.Second)
		out = rec
		return json.Marshal(rec)
	})
	if err != nil {
		return Record{}, fmt.Errorf("people: keep the name of %s: %w", subject, err)
	}
	s.keep(out, s.now())
	return out, nil
}

// learn keeps the name claim and the email of a subject when they are new.
// An empty value keeps what the record holds.
func (s *Store) learn(ctx context.Context, subject, claim, email string) {
	if s == nil || subject == "" || claim == "" && email == "" {
		return
	}
	if rec := s.Get(ctx, subject); (claim == "" || rec.Claim == claim) && (email == "" || rec.Email == email) {
		return
	}
	_, _ = s.change(ctx, subject, func(r *Record) bool {
		changed := false
		if claim != "" && r.Claim != claim {
			r.Claim, changed = claim, true
		}
		if email != "" && r.Email != email {
			r.Email, changed = email, true
		}
		return changed
	})
}

// Learn keeps what the principal p of a call says of the name of its
// person: its name, as an email when it has the shape of one, else as the
// name claim of the sign-in. The kernel names a principal by the email
// first, so a name that is no email is a name claim that the issuer gave,
// or that the configuration picked (nameClaim). A principal that is no
// person, and a name that is the subject or an id of the issuer, teach
// nothing.
func Learn(ctx context.Context, a *fw.Agent, p *fw.Principal) {
	if p == nil || p.Subject == "" || p.Kind == fw.KindToken || p.Kind == fw.KindWorkload || p.Kind == fw.KindAnonymous {
		return
	}
	name := oneLine(p.Name)
	if name == "" || name == p.Subject || Opaque(name) {
		return
	}
	if IsEmail(name) {
		Of(a).learn(ctx, p.Subject, "", name)
		return
	}
	Of(a).learn(ctx, p.Subject, name, "")
}

// LearnClaims keeps the name claim and the email of the claims of a token
// of a browser sign-in for its subject. The surface of the sign-in calls
// it with the claims of the ID token, which the kernel does not keep.
func LearnClaims(ctx context.Context, a *fw.Agent, subject string, claims map[string]any) {
	str := func(k string) string {
		v, _ := claims[k].(string)
		return oneLine(v)
	}
	if subject == "" {
		subject = str("sub")
	}
	claim := str("name")
	if claim == "" {
		claim = strings.TrimSpace(str("given_name") + " " + str("family_name"))
	}
	if claim == subject || Opaque(claim) || IsEmail(claim) {
		claim = ""
	}
	email := str("email")
	if !IsEmail(email) {
		email = ""
	}
	Of(a).learn(ctx, subject, claim, email)
}

// ErrOff means that the front keeps no names, because its bus keeps no
// store.
var ErrOff = errors.New("people: this front keeps no names of people, because its bus keeps no store")

// SetName keeps the name that the person subject set, or clears it for an
// empty name. A name holds 1 to MaxName characters of one line.
func SetName(ctx context.Context, a *fw.Agent, subject, name string) (Record, error) {
	s := Of(a)
	if s == nil {
		return Record{}, ErrOff
	}
	name = oneLine(name)
	if utf8.RuneCountInString(name) > MaxName {
		return Record{}, fmt.Errorf("%w: a name holds at most %d characters", fw.ErrInvalidRequest, MaxName)
	}
	return s.change(ctx, subject, func(r *Record) bool {
		if r.Set == name {
			return false
		}
		r.Set = name
		return true
	})
}

// Display returns the display name of the person subject, with a name
// that a record of the caller holds beside the subject, such as the email
// that an entry of a channel kept, as hint. The token is "Admin token".
func Display(ctx context.Context, a *fw.Agent, subject, hint string) string {
	return Of(a).Display(ctx, subject, hint)
}

// Display is Display with the store s, which can be nil.
func (s *Store) Display(ctx context.Context, subject, hint string) string {
	if IsToken(subject) {
		return AdminToken
	}
	hint = oneLine(hint)
	var rec Record
	if s != nil && subject != "" {
		rec = s.Get(ctx, subject)
	}
	if rec.Claim != "" {
		return rec.Claim
	}
	if rec.Set != "" {
		return rec.Set
	}
	for _, email := range []string{rec.Email, hint, subject} {
		if IsEmail(email) {
			if n := Readable(email); n != "" {
				return n
			}
		}
	}
	if hint != "" && !Opaque(hint) {
		return hint
	}
	if subject != "" && !Opaque(subject) {
		return subject
	}
	return SomePerson
}

// IsToken reports whether a subject is the static token of the front.
func IsToken(subject string) bool { return subject == "token" }

// email is the shape of an email address.
var email = regexp.MustCompile(`^[^@\s]+@[^@\s]+\.[^@\s]+$`)

// IsEmail reports whether a text has the shape of an email address.
func IsEmail(s string) bool { return email.MatchString(s) }

// Readable returns the part of an email before the @ in readable form: the
// dots, the dashes, the underscores, and the plus signs part the words, a
// part after a plus goes, and each word starts with a capital. A part
// without a letter gives "".
func Readable(addr string) string {
	local, _, _ := strings.Cut(addr, "@")
	local, _, _ = strings.Cut(local, "+")
	words := strings.FieldsFunc(local, func(r rune) bool { return r == '.' || r == '-' || r == '_' })
	letters := false
	for i, w := range words {
		r, n := utf8.DecodeRuneInString(w)
		words[i] = string(unicode.ToUpper(r)) + w[n:]
		letters = letters || strings.IndexFunc(w, unicode.IsLetter) >= 0
	}
	if !letters {
		return ""
	}
	return strings.Join(words, " ")
}

// uuidLike is the shape of a UUID.
var uuidLike = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// Opaque reports whether a subject is an id that the issuer made and that
// says nothing to a person: a UUID, or 16 or more digits, hex digits, and
// dashes, also after a prefix that names the provider, such as
// google-oauth2|1098...
func Opaque(subject string) bool {
	if i := strings.LastIndexByte(subject, '|'); i >= 0 {
		subject = subject[i+1:]
	}
	if uuidLike.MatchString(subject) {
		return true
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

// oneLine folds the spaces of a text and cuts it to 256 bytes.
func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= 256 {
		return s
	}
	n := 256
	for n > 0 && (s[n]&0xC0) == 0x80 {
		n--
	}
	return s[:n]
}
