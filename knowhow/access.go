package knowhow

// This file decides who reads and who changes the Know-how of an agent
// (docs/KNOWHOW.md section 5.3). A worker reaches the routes with the
// static token of the fleet, and the process itself calls without a
// principal: both read and change every note. A person needs the right to
// view the agent. A person reads the notes of the agent and the notes of
// their own principal, and an operator of the agent also reads the notes
// of its workload principals, such as duty:<name>. A change of the notes
// of the agent or of a workload principal needs the right to edit the
// agent. A person changes the notes of their own principal with the right
// to run the agent, because their runs write those notes anyway. No
// person reads or changes the notes of another person.

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/faces"
)

// ErrHidden means that the caller cannot view the agent or the scope, or
// that no agent of the fleet has the name. A route answers 404, so a
// caller learns nothing of what it cannot view.
var ErrHidden = errors.New("knowhow: nothing that you can view has the name")

// caller is who calls: trusted for the process itself and the static
// token, else the subject and the kind of the principal.
type caller struct {
	trusted bool
	subject string
	kind    string
}

func callerOf(ctx context.Context) caller {
	p := fw.PrincipalFrom(ctx)
	if p == nil || p.Kind == fw.KindToken {
		return caller{trusted: true}
	}
	return caller{subject: p.Subject, kind: p.Kind}
}

// by is the writer of a change of the caller.
func (c caller) by() string {
	if c.trusted {
		return ByAgent
	}
	return PersonPrefix + c.subject
}

// person reports whether a person calls: a user of the issuer or the local
// user of the process.
func (c caller) person() bool { return c.kind == fw.KindUser || c.kind == fw.KindLocal }

// isWorkload reports whether a principal of a scope is a workload, such as
// a Duty (duty:<name>) or a SPIFFE identity.
func isWorkload(principal string) bool {
	for _, p := range []string{"duty:", "spiffe://", "knowhow:", "workload:"} {
		if strings.HasPrefix(principal, p) {
			return true
		}
	}
	return false
}

// rights holds what the caller can do on one agent.
type rights struct {
	c         caller
	run, edit bool
}

// rightsOn decides the rights of the caller on the agent. A caller that
// cannot view the agent gets ErrHidden.
func (s *Service) rightsOn(ctx context.Context, agent string) (rights, error) {
	if err := CheckAgent(agent); err != nil {
		return rights{}, err
	}
	c := callerOf(ctx)
	if c.trusted {
		return rights{c: c, run: true, edit: true}, nil
	}
	if s.a == nil {
		return rights{}, fmt.Errorf("%w: %s", ErrHidden, agent)
	}
	err := faces.Check(ctx, s.a, agent, fw.ActionAgentView)
	switch {
	case errors.Is(err, fw.ErrProfileNotFound), errors.Is(err, fw.ErrForbidden):
		return rights{}, fmt.Errorf("%w: %s", ErrHidden, agent)
	case err != nil:
		return rights{}, err
	}
	r := rights{c: c}
	for _, x := range []struct {
		action string
		to     *bool
	}{{fw.ActionAgentRun, &r.run}, {fw.ActionAgentEdit, &r.edit}} {
		err := faces.Check(ctx, s.a, agent, x.action)
		switch {
		case err == nil:
			*x.to = true
		case errors.Is(err, fw.ErrForbidden), errors.Is(err, fw.ErrProfileNotFound):
		default:
			return rights{}, err
		}
	}
	if r.edit {
		r.run = true
	}
	return r, nil
}

// read reports whether the rights read the notes of a scope.
func (r rights) read(sc Scope) bool {
	switch {
	case r.c.trusted, sc.Kind == ScopeAgent:
		return true
	case sc.Principal == r.c.subject:
		return true
	case isWorkload(sc.Principal):
		return r.run
	}
	return false
}

// change decides a change of the notes of a scope: nil, ErrHidden for a
// scope that the caller cannot read, or fw.ErrForbidden with the role to
// ask for.
func (r rights) change(sc Scope) error {
	switch {
	case r.c.trusted:
		return nil
	case !r.read(sc):
		return fmt.Errorf("%w: no note of that scope", ErrHidden)
	case sc.Kind == ScopePrincipal && sc.Principal == r.c.subject:
		if r.run {
			return nil
		}
		return fmt.Errorf("%w: a change of your own notes needs the operator role on the agent", fw.ErrForbidden)
	case r.edit:
		return nil
	}
	return fmt.Errorf("%w: a change of the notes of the agent or of a Duty needs the owner role on the agent", fw.ErrForbidden)
}

// seeEntry reports whether the rights read an entry of the journal or a
// waiting change about a subject.
func (r rights) seeSubject(sub Subject) bool {
	if sub.Note == "" {
		return true
	}
	return r.read(sub.scope())
}

// changeSubject decides an undo or a decision on a subject: the change of
// a note scope, or the right to edit the agent for a routine.
func (r rights) changeSubject(sub Subject) error {
	if sub.Note != "" {
		return r.change(sub.scope())
	}
	if r.c.trusted || r.edit {
		return nil
	}
	return fmt.Errorf("%w: a change of a routine needs the owner role on the agent", fw.ErrForbidden)
}
