package manager

import (
	"context"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/internal/behalf"
)

// MetaVia is the metadata key of a run that an actor starts, through run
// or enqueue: "<agent>:<run>", the profile and the run of the actor, such
// as manager:01K.... fw.CheckMetadata reserves no such key, and the MCP
// server refuses it from a caller, so only the server sets it.
const MetaVia = "fw.via"

// maxField bounds each field of an actor in a text, as the kernel bounds
// the sender of a message (fw.MaxMessageFromBytes).
const maxField = fw.MaxMessageFromBytes

// Actor is a run of a worker that calls the MCP server of the front with
// the static token of the fleet: the id of the run, its profile, the node
// name of the process that holds it, and the person of the run with the
// kind of that principal. Person and PersonKind are empty when _meta names
// no person, as for a run without one or for a server that the worker does
// not mark trusted.
type Actor struct {
	Run        string `json:"run"`
	Agent      string `json:"agent,omitempty"`
	Node       string `json:"node,omitempty"`
	Person     string `json:"person,omitempty"`
	PersonKind string `json:"personKind,omitempty"`
}

type actorKey struct{}

// OnBehalf returns the context in which the MCP server serves a call, the
// actor behind the call, and whether a run of the fleet makes the call.
// The handlers of surface/mcp call it at their start.
//
// A run of the fleet makes the call when the caller of the request is the
// static token of the fleet and _meta names a run (mcp.CallRun). Only a
// worker holds that token, and the kernel of the worker fills _meta from
// the ledger of the run, so a person cannot pose as a run, and a run
// cannot pose as another person. For any other caller, OnBehalf returns ctx
// as it is, no actor, and false.
//
// When _meta names the person of the run, the context holds that person
// as its principal, with the subject and the kind that _meta names and no
// name, no groups, and no scopes, as fw.PrincipalOfRun gives a run. A rule
// that names the person by a group or by a name other than the subject
// therefore does not match, so the context can only narrow. The token
// stays the caller behind the person (behalf.For), so the guard of the
// authorization (Guard) lets a check pass only when it passes for the
// token too, and a forwarded request still carries the credential of the
// token. A run whose person is the token itself, or a run without a
// person, keeps the caller as it is. A context that holds an actor already
// gives that actor again.
func OnBehalf(ctx context.Context) (context.Context, Actor, bool) {
	if act, ok := ActorFrom(ctx); ok {
		return ctx, act, true
	}
	m, ok := behalf.Trusted(ctx)
	if !ok {
		return ctx, Actor{}, false
	}
	act := Actor{Run: m.Run, Agent: m.Agent, Node: m.Node, Person: m.Principal, PersonKind: m.PrincipalKind}
	if act.Person == "" {
		act.PersonKind = ""
	}
	ctx = context.WithValue(ctx, actorKey{}, act)
	if p := act.principal(); p != nil {
		ctx = behalf.For(ctx, p)
	}
	return ctx, act, true
}

// ActorFrom returns the actor that OnBehalf put in ctx.
func ActorFrom(ctx context.Context) (Actor, bool) {
	act, ok := ctx.Value(actorKey{}).(Actor)
	return act, ok
}

// principal is the principal that the server acts as for the actor, or
// nil when the actor names no person to act for.
func (a Actor) principal() *fw.Principal {
	if a.Person == "" || a.PersonKind == fw.KindToken {
		return nil
	}
	return &fw.Principal{Subject: a.Person, Kind: a.PersonKind}
}

// ForPerson reports whether the server acts as the person of the actor,
// so the principal of the call is that person and not the token.
func (a Actor) ForPerson() bool { return a.principal() != nil }

// IsManager reports whether the actor is a run of the manager: its profile
// is the profile that workbench.bossmode.manager names (Profile). Without
// a manager, no run is one.
func (a Actor) IsManager(cfg *fw.Config) bool {
	m := Profile(cfg)
	return m != "" && a.Agent == m
}

// String names the actor in a text for people and the log: "manager run
// 01K... for alice", "run 01K... for alice" without a profile, and no
// "for" part without a person. Each field is cut to the bound of the
// sender of a message, and a control character becomes a space, so a
// value of _meta cannot add a line to a note.
func (a Actor) String() string {
	s := "run " + clean(a.Run)
	if a.Agent != "" {
		s = clean(a.Agent) + " " + s
	}
	if a.Person != "" {
		s += " for " + clean(a.Person)
	}
	return s
}

// Note is the note of a decision that the actor makes, as the owner of
// the run records it: "by manager run 01K... for alice: <note>", or the
// first part alone without a note.
func (a Actor) Note(note string) string {
	s := "by " + a.String()
	if note = strings.TrimSpace(note); note != "" {
		s += ": " + note
	}
	return s
}

// From is the sender of a message that the actor sends to a run
// (fw.WithMessageFrom): an agent with the name of the profile of the actor
// and its run.
func (a Actor) From() fw.MessageFrom {
	name := clean(a.Agent)
	if name == "" {
		name = "agent"
	}
	return fw.MessageFrom{Kind: fw.FromAgent, Name: name, Run: clean(a.Run)}
}

// Lead is the first line of a message that the actor sends, which names
// the actor and the person: "From the manager run 01K... for alice.".
func (a Actor) Lead() string { return "From the " + a.String() + "." }

// Via is the value of MetaVia on a run that the actor starts.
func (a Actor) Via() string {
	agent := clean(a.Agent)
	if agent == "" {
		agent = "run"
	}
	return agent + ":" + clean(a.Run)
}

// clean cuts s to maxField bytes at a rune and turns each control
// character into a space.
func clean(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
	if len(s) <= maxField {
		return s
	}
	cut := maxField
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}
