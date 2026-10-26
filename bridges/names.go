package bridges

// This file names the people of a bridge in the texts that the front
// writes: the system lines, the instructions of a seat, the plan of the
// manager, the hearts in the wiki, and the results of the tools
// (docs/ARCHITECTURE.md section 18.6). A text names a person by a name
// that people read: the name that the sign-in gives the principal, which
// is the email or the display name (fw.Principal.Name), when the front
// knows it. The subject of a principal is often an id that the issuer
// made, such as a UUID, and no text names a person by such an id: it says
// "a person" instead. The page of bossmode shows "you" for the person who
// reads it. Toward the agents, the people of a bridge are operator, one
// word for one thing, so the header of a follow-up and bridge_read name a
// person as operator, and an agent answers with @operator.
//
// The front learns the name of a person from each call of a route of the
// bridges (learn), because the run of the manager carries the subject of
// its person alone (section 19.2). A name that the front learned names the
// person in a text only. It never enters a principal, so no rule of the
// access matches a person by it.

import (
	"context"
	"regexp"
	"strings"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// maxNames bounds the names that a process keeps.
const maxNames = 10000

// somePerson names a person whose name the front does not know.
const somePerson = "a person"

// uuidLike is the shape of a UUID.
var uuidLike = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// opaque reports whether a subject is an id that the issuer made and that
// says nothing to a person: a UUID, or 16 or more digits, hex digits, and
// dashes, also after a prefix that names the provider, such as
// google-oauth2|1098...
func opaque(subject string) bool {
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

// learn keeps the name of a person who calls a route, by subject: the
// name of the principal when it differs from the subject and says more.
func (s *Store) learn(p *fw.Principal) {
	if p == nil || p.Subject == "" {
		return
	}
	name := oneLine(p.Name, 256)
	if name == "" || name == p.Subject || opaque(name) {
		return
	}
	s.namesMu.Lock()
	defer s.namesMu.Unlock()
	if s.names[p.Subject] == name {
		return
	}
	if s.names == nil || len(s.names) >= maxNames {
		s.names = map[string]string{}
	}
	s.names[p.Subject] = name
}

// knownName returns a name of a person that people read: name, when it is
// one, else the name that the front learned for the subject, else the
// subject when it is no opaque id. It returns "" when the front knows no
// such name.
func (s *Store) knownName(subject, name string) string {
	if name = oneLine(name, 256); name != "" && !opaque(name) {
		return name
	}
	if subject == "" {
		return ""
	}
	s.namesMu.Lock()
	learned := s.names[subject]
	s.namesMu.Unlock()
	if learned != "" {
		return learned
	}
	if !opaque(subject) {
		return subject
	}
	return ""
}

// personName names a person in a text: a name that people read, else "a
// person".
func (s *Store) personName(subject, name string) string {
	return orWords(s.knownName(subject, name), somePerson)
}

// operatorName names the operator of a bridge in a text: the name that
// the record keeps from the open, a name that the front learned since, or
// else the words other.
func (s *Store) operatorName(rec *record, other string) string {
	return orWords(s.knownName(rec.Operator, rec.OperatorName), other)
}

// actorKey is the key of the context of an action that a run takes for a
// person, such as the manager.
type actorKey struct{}

// actingAs marks ctx as the context of an action that the agent as takes
// for the person of its run, so a line names the agent and the person,
// such as "the manager for alice@example.com".
func actingAs(ctx context.Context, as string) context.Context {
	return context.WithValue(ctx, actorKey{}, as)
}

// nameOf names who acts in a system line: a person by a name that people
// read, the agent of a run for its person (actingAs), or the front.
func (s *Store) nameOf(ctx context.Context, p *fw.Principal) string {
	if as, _ := ctx.Value(actorKey{}).(string); as != "" {
		if p == nil {
			return "the " + as
		}
		return "the " + as + " for " + s.personName(p.Subject, "")
	}
	if p == nil {
		return "the server"
	}
	if p.Subject == "" && strings.TrimSpace(p.Name) == "" {
		return "somebody"
	}
	return s.personName(p.Subject, p.Name)
}
