package skills

// This file expands a skill for a slash command (docs/ARCHITECTURE.md
// section 20.3): the placeholders of the body take the arguments, and the
// message that a run gets starts with the line "Skill: <name>".

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// MaxMessageBytes bounds the message of a slash command: the line Skill,
// the expanded body, and the text of the person.
const MaxMessageBytes = 64 << 10

// Errors of a slash command.
var (
	// ErrUnknown is a skill that the catalog does not hold.
	ErrUnknown = errors.New("skills: unknown skill")
	// ErrNotInvocable is a skill whose front matter says
	// user-invocable: false.
	ErrNotInvocable = errors.New("skills: the skill is not for people")
	// ErrTooLarge is a message over MaxMessageBytes.
	ErrTooLarge = errors.New("skills: too large")
	// ErrUnavailable is a skill whose SKILL.md the front cannot read now,
	// such as a skill of an MCP server that does not answer or whose file
	// does not match its listing.
	ErrUnavailable = errors.New("skills: the skill cannot be read")
)

// Command is a slash command: the skill and its arguments, as a route
// takes it.
type Command struct {
	Name string `json:"name"`
	Args string `json:"args,omitempty"`
}

// Expansion is a slash command made ready for a run. Message is the whole
// message that a run gets. Summary is the text of the entry of a channel:
// the line Skill and one line that sums the rest up.
type Expansion struct {
	Entry   Entry
	Command Command
	Message string
	Summary string
}

// Expand fills the placeholders of a body with the arguments, as a slash
// command does:
//
//   - $ARGUMENTS is the whole text of the arguments.
//   - $ARGUMENTS[N] and $N are the argument N, counted from 0, with the
//     arguments split on white space. A placeholder for an argument that
//     the command does not give stays as it is.
//   - $name is the argument at the place of name in the list arguments of
//     the front matter, or an empty text when the command gives no
//     argument at that place.
//   - \$ before a placeholder keeps the placeholder as it is, without the
//     backslash.
//
// When the body holds no placeholder that the command fills and the
// arguments are not empty, the body gets one more line, "ARGUMENTS:
// <args>". A $name counts as filled also when it takes an empty text, and
// a $N or $ARGUMENTS[N] that stays as it is does not count.
func Expand(body, args string, names []string) string {
	words := strings.Fields(args)
	index := make(map[string]int, len(names))
	for i, n := range names {
		index[n] = i
	}
	arg := func(n int) (string, bool) {
		if n < 0 || n >= len(words) {
			return "", false
		}
		return words[n], true
	}
	var b strings.Builder
	b.Grow(len(body) + len(args))
	took := false
	for i := 0; i < len(body); {
		c := body[i]
		if c == '\\' && i+1 < len(body) && body[i+1] == '$' {
			if n := placeholderLen(body[i+1:], index); n > 0 {
				b.WriteString(body[i+1 : i+1+n])
				i += 1 + n
				continue
			}
		}
		if c != '$' {
			b.WriteByte(c)
			i++
			continue
		}
		rest := body[i+1:]
		switch {
		case len(rest) > 0 && isDigit(rest[0]):
			d := digits(rest)
			n, err := strconv.Atoi(rest[:d])
			if v, ok := arg(n); err == nil && ok {
				b.WriteString(v)
				took = true
			} else {
				b.WriteString(body[i : i+1+d])
			}
			i += 1 + d
		case len(rest) > 0 && isIdentStart(rest[0]):
			w := ident(rest)
			name := rest[:w]
			switch {
			case name == "ARGUMENTS" && w < len(rest) && rest[w] == '[':
				if d := digits(rest[w+1:]); d > 0 && w+1+d < len(rest) && rest[w+1+d] == ']' {
					n, err := strconv.Atoi(rest[w+1 : w+1+d])
					end := 1 + w + 1 + d + 1
					if v, ok := arg(n); err == nil && ok {
						b.WriteString(v)
						took = true
					} else {
						b.WriteString(body[i : i+end])
					}
					i += end
					continue
				}
				b.WriteString(args)
				took = true
				i += 1 + w
			case name == "ARGUMENTS":
				b.WriteString(args)
				took = true
				i += 1 + w
			default:
				if at, ok := index[name]; ok {
					v, _ := arg(at)
					b.WriteString(v)
					took = true
				} else {
					b.WriteString(body[i : i+1+w])
				}
				i += 1 + w
			}
		default:
			b.WriteByte(c)
			i++
		}
	}
	out := b.String()
	if !took && strings.TrimSpace(args) != "" {
		out = strings.TrimRight(out, "\n") + "\n\nARGUMENTS: " + args
	}
	return out
}

// placeholderLen is the length of the placeholder at the start of s, which
// starts with $, or 0 when s starts with no placeholder.
func placeholderLen(s string, index map[string]int) int {
	if len(s) < 2 || s[0] != '$' {
		return 0
	}
	rest := s[1:]
	switch {
	case isDigit(rest[0]):
		return 1 + digits(rest)
	case isIdentStart(rest[0]):
		w := ident(rest)
		name := rest[:w]
		if name == "ARGUMENTS" {
			if w < len(rest) && rest[w] == '[' {
				if d := digits(rest[w+1:]); d > 0 && w+1+d < len(rest) && rest[w+1+d] == ']' {
					return 1 + w + 1 + d + 1
				}
			}
			return 1 + w
		}
		if _, ok := index[name]; ok {
			return 1 + w
		}
	}
	return 0
}

func isDigit(c byte) bool      { return c >= '0' && c <= '9' }
func isIdentStart(c byte) bool { return c == '_' || (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') }

func digits(s string) int {
	n := 0
	for n < len(s) && isDigit(s[n]) {
		n++
	}
	return n
}

func ident(s string) int {
	n := 0
	for n < len(s) && (isIdentStart(s[n]) || isDigit(s[n])) {
		n++
	}
	return n
}

// Prepare expands the slash command cmd with the catalog of the agent and
// the text of the person, which can be empty. It reads SKILL.md at the
// moment of use, and the keys of its front matter with it. It returns an
// error that wraps ErrUnknown for a skill that the catalog does not hold,
// ErrNotInvocable for a skill whose front matter says user-invocable:
// false, ErrTooLarge for a message over MaxMessageBytes, and
// ErrUnavailable for a SKILL.md that cannot be read.
func Prepare(a *fw.Agent, cmd Command, text string) (Expansion, error) {
	cmd.Name = strings.TrimSpace(cmd.Name)
	cmd.Args = strings.TrimSpace(cmd.Args)
	if cmd.Name == "" {
		return Expansion{}, fmt.Errorf("%w: a slash command needs the name of a skill", fw.ErrInvalidRequest)
	}
	e, ok := Named(a, cmd.Name)
	if !ok {
		return Expansion{}, fmt.Errorf("%w: no skill has the name %q", ErrUnknown, cmd.Name)
	}
	e, body, err := e.read(a.Logger())
	if err != nil {
		return Expansion{}, err
	}
	if !e.UserInvocable {
		return Expansion{}, fmt.Errorf("%w: the server matter of the skill %s says user-invocable: false, so only an agent loads it", ErrNotInvocable, e.Name)
	}
	names := make([]string, len(e.Arguments))
	for i, arg := range e.Arguments {
		names[i] = arg.Name
	}
	expanded := strings.TrimSpace(Expand(body, cmd.Args, names))
	if len(expanded) > MaxMessageBytes {
		return Expansion{}, fmt.Errorf("%w: the skill %s expands to %d bytes, and a slash command holds at most %d", ErrTooLarge, e.Name, len(expanded), MaxMessageBytes)
	}
	msg := "Skill: " + e.Name + "\n\n" + expanded
	text = strings.TrimSpace(text)
	if text != "" {
		msg += "\n\n" + text
	}
	if len(msg) > MaxMessageBytes {
		return Expansion{}, fmt.Errorf("%w: the skill %s and the text make %d bytes, and a slash command holds at most %d", ErrTooLarge, e.Name, len(msg), MaxMessageBytes)
	}
	return Expansion{Entry: e, Command: cmd, Message: msg, Summary: summary(e, cmd.Args, text)}, nil
}

// summary is the text of the entry of a channel for a slash command: the
// line Skill, then one line with the description of the skill, the
// arguments, and the text of the person.
func summary(e Entry, args, text string) string {
	var parts []string
	if d := strings.TrimSpace(e.Description); d != "" {
		parts = append(parts, strings.TrimRight(d, "."))
	}
	if args != "" {
		parts = append(parts, "arguments: "+args)
	}
	if text != "" {
		parts = append(parts, text)
	}
	line := ""
	if len(parts) > 0 {
		line = "\n" + oneLine(strings.Join(parts, ". "))
	}
	return "Skill: " + e.Name + line
}
