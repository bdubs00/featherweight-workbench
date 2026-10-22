// Package behalf holds what the MCP server, the fleet, and the manager of
// the workbench share about a call that the static token of the fleet
// makes for a run of a worker: the run behind the call, as the keys of
// _meta name it, and the caller behind a principal that the server puts in
// place of the caller (docs/ARCHITECTURE.md sections 3.1 and 19).
//
// The guard of this package keeps a decision for such a principal inside
// what the caller can do. A decision for the person passes only when the
// same decision passes for the caller too. So acting for a person narrows
// what the static token can do and never widens it.
package behalf

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// Run is the run behind a tools/call: the id of the run, its profile, the
// node name of the process that holds it, and the person of the run with
// the kind of that principal, as the MCP client of the kernel names them
// in _meta (the kernel architecture, section 7.3). Principal and
// PrincipalKind are empty for a run without a person and for a server that
// the worker does not trust.
type Run struct {
	Run           string `json:"run"`
	Agent         string `json:"agent,omitempty"`
	Node          string `json:"node,omitempty"`
	Principal     string `json:"principal,omitempty"`
	PrincipalKind string `json:"principalKind,omitempty"`
}

type runKey struct{}

// WithRun returns ctx with the run keys of a call. Any client can send the
// keys, so they are a claim until Trusted says that the caller is the
// static token of the fleet.
func WithRun(ctx context.Context, r Run) context.Context {
	return context.WithValue(ctx, runKey{}, r)
}

// Trusted returns the run behind the call of ctx and reports whether the
// call names one. It reports false unless the caller of the request is the
// static token of the fleet (principal kind token), because only a worker
// of the fleet holds that token. The caller is the principal of ctx, or
// the caller that For kept when it put a person in its place. Trusted also
// reports false when ctx names no run.
func Trusted(ctx context.Context) (Run, bool) {
	r, ok := ctx.Value(runKey{}).(Run)
	if !ok || r.Run == "" {
		return Run{}, false
	}
	p := fw.PrincipalFrom(ctx)
	if c := Caller(ctx); c != nil {
		p = c
	}
	if p == nil || p.Kind != fw.KindToken {
		return Run{}, false
	}
	return r, true
}

// acting is a person in the place of the caller of a request.
type acting struct {
	caller *fw.Principal
	person *fw.Principal
}

type actingKey struct{}

// For returns ctx with person as its principal, in the place of the
// caller, which is the principal of ctx. It keeps the caller beside the
// person, so the guard decides each check for both. A nil person, or a
// ctx without a principal, gives ctx as it is.
func For(ctx context.Context, person *fw.Principal) context.Context {
	caller := fw.PrincipalFrom(ctx)
	if person == nil || caller == nil {
		return ctx
	}
	ctx = context.WithValue(ctx, actingKey{}, &acting{caller: caller, person: person})
	return fw.WithPrincipal(ctx, person)
}

// Caller returns the caller that acts for the principal of ctx, or nil
// when the principal of ctx is the caller of the request itself. A
// context whose principal changed again after For has no such caller.
func Caller(ctx context.Context) *fw.Principal {
	a, _ := ctx.Value(actingKey{}).(*acting)
	if a == nil || fw.PrincipalFrom(ctx) != a.person {
		return nil
	}
	return a.caller
}

// callerFor returns the caller that acts for p in ctx, or nil when p is
// not the person that For put in ctx.
func callerFor(ctx context.Context, p *fw.Principal) *fw.Principal {
	a, _ := ctx.Value(actingKey{}).(*acting)
	if a == nil || p == nil || p.Subject != a.person.Subject || p.Kind != a.person.Kind {
		return nil
	}
	return a.caller
}

// Guard returns z with the rule of this package: a check for a person that
// a caller acts for (For) passes only when it passes for the caller too. A
// check for any other principal goes to z as it is. Guard keeps the
// batches of z (fw.BatchAuthz) and its decisions about the ancestors of a
// run (fw.LineageAuthz). A nil z gives nil, and a guarded z gives z.
func Guard(z fw.Authz) fw.Authz {
	switch z.(type) {
	case nil:
		return nil
	case *guard, *lineageGuard:
		return z
	}
	g := &guard{z: z}
	if _, ok := z.(fw.LineageAuthz); ok {
		return &lineageGuard{guard: g}
	}
	return g
}

// Guarded reports whether z keeps the rule of Guard.
func Guarded(z fw.Authz) bool {
	switch z.(type) {
	case *guard, *lineageGuard:
		return true
	}
	return false
}

// guard is the Authz that Guard returns.
type guard struct{ z fw.Authz }

// lineageGuard is a guard over an Authz that decides about the ancestors
// of a run itself.
type lineageGuard struct{ *guard }

var (
	_ fw.BatchAuthz   = (*guard)(nil)
	_ fw.LineageAuthz = (*lineageGuard)(nil)
)

// DecidesLineage implements fw.LineageAuthz.
func (*lineageGuard) DecidesLineage() {}

// Allow implements fw.Authz.
func (g *guard) Allow(ctx context.Context, p *fw.Principal, action string, t fw.Target) error {
	return g.AllowAll(ctx, p, []fw.AuthzCheck{{Action: action, Target: t}})[0]
}

// AllowAll implements fw.BatchAuthz. It asks z about the person first, and
// then about the caller for each check that passed for the person, in one
// batch each.
func (g *guard) AllowAll(ctx context.Context, p *fw.Principal, checks []fw.AuthzCheck) []error {
	out := allowAll(ctx, g.z, p, checks)
	c := callerFor(ctx, p)
	if c == nil {
		return out
	}
	var again []int
	for i, err := range out {
		if err == nil {
			again = append(again, i)
		}
	}
	if len(again) == 0 {
		return out
	}
	sub := make([]fw.AuthzCheck, len(again))
	for j, i := range again {
		sub[j] = checks[i]
	}
	for j, err := range allowAll(ctx, g.z, c, sub) {
		if err != nil {
			out[again[j]] = narrowed(c, p, err)
		}
	}
	return out
}

// narrowed is the refusal of a check that passed for the person p and not
// for the caller c. A decision point that did not answer stays that error,
// so the caller tells an outage from a refusal.
func narrowed(c, p *fw.Principal, err error) error {
	if errors.Is(err, fw.ErrAuthzUnavailable) {
		return err
	}
	reason := strings.TrimPrefix(strings.TrimPrefix(err.Error(), fw.ErrForbidden.Error()), ": ")
	return fmt.Errorf("%w: %s acts for %s here, and only what both can do passes, but %s", fw.ErrForbidden, c.Subject, p.Subject, reason)
}

// allowAll asks z about every check for p, in one batch when z takes one.
func allowAll(ctx context.Context, z fw.Authz, p *fw.Principal, checks []fw.AuthzCheck) []error {
	if b, ok := z.(fw.BatchAuthz); ok {
		out := make([]error, len(checks))
		copy(out, b.AllowAll(ctx, p, checks))
		return out
	}
	out := make([]error, len(checks))
	for i, c := range checks {
		out[i] = z.Allow(ctx, p, c.Action, c.Target)
	}
	return out
}
