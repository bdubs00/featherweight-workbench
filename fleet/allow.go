package fleet

// This file decides an action of the kernel on a run or an agent of
// another member for the principal in ctx, beside the read that the view
// decides (view.go). A surface needs it when it acts for a person through
// the static token of the fleet (docs/ARCHITECTURE.md section 19): the
// request carries the token, so the owner decides about the token, and
// the front decides about the person here, with the same rules as for a
// read (section 4.4).

import (
	"context"
	"fmt"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/internal/behalf"
)

// Authorizer is a View that decides an action of the kernel, such as
// fw.ActionRunOperate, for the principal in ctx on a run or on an agent of
// another member. It decides as the view decides a read: with the rules of
// the front in the rules mode, and else with the Authz of this process,
// which receives the member, the access block, and the lineage. A call
// without a principal is the process itself and passes. *Fleet is one.
//
// AllowRun returns fw.ErrRunNotFound for a run that the view does not
// know, and AllowAgent returns ErrNoRoute for a member that the view does
// not know.
type Authorizer interface {
	AllowRun(ctx context.Context, id, action string) error
	AllowAgent(ctx context.Context, member, agent, action string) error
}

// AllowRun decides an action on a run of another member through v. A view
// that is not an Authorizer decides nothing, so AllowRun refuses with
// fw.ErrForbidden, and a surface never acts for a person without a
// decision.
func AllowRun(ctx context.Context, v View, id, action string) error {
	z, ok := v.(Authorizer)
	if !ok {
		return fmt.Errorf("%w: the view of the fleet cannot decide %s on the run %s", fw.ErrForbidden, action, id)
	}
	return z.AllowRun(ctx, id, action)
}

// AllowAgent decides an action on an agent of another member through v,
// as AllowRun does for a run.
func AllowAgent(ctx context.Context, v View, member, agent, action string) error {
	z, ok := v.(Authorizer)
	if !ok {
		return fmt.Errorf("%w: the view of the fleet cannot decide %s on the agent %s of %s", fw.ErrForbidden, action, agent, member)
	}
	return z.AllowAgent(ctx, member, agent, action)
}

// AllowRun implements Authorizer.
func (f *Fleet) AllowRun(ctx context.Context, id, action string) error {
	owner, found := f.find(id)
	if found == nil {
		return fmt.Errorf("fleet: run %s: %w", id, fw.ErrRunNotFound)
	}
	return f.decide(ctx, owner, found.Agent, found, action)
}

// AllowAgent implements Authorizer.
func (f *Fleet) AllowAgent(ctx context.Context, member, agent, action string) error {
	for _, r := range f.remotes(time.Now()) {
		if r.rec.Name == member {
			return f.decide(ctx, r.rec, agent, nil, action)
		}
	}
	return fmt.Errorf("fleet: %w: the view knows no member %q", ErrNoRoute, member)
}

// decide decides one action for the principal in ctx on an agent of a
// record, and on one of its runs when run is set. Whoever can do the
// action on an ancestor of the run can do it on the run, as the kernel
// decides.
func (f *Fleet) decide(ctx context.Context, rec *Record, agent string, run *fw.Run, action string) error {
	p := fw.PrincipalFrom(ctx)
	if p == nil {
		return nil
	}
	byID := make(map[string]*fw.Run, len(rec.Runs))
	for _, r := range rec.Runs {
		byID[r.ID] = r
	}
	if rec.Access.Mode == fw.AccessModeRules {
		rules := recordRules(rec)
		err := rules.Allow(ctx, p, action, fw.Target{Agent: agent, Run: run})
		for cur, depth := run, 0; err != nil && cur != nil && cur.Parent != "" && depth < maxLineage; depth++ {
			if cur = byID[cur.Parent]; cur == nil {
				break
			}
			if rules.Allow(ctx, p, action, fw.Target{Agent: cur.Agent, Run: cur}) == nil {
				err = nil
			}
		}
		return err
	}
	var access *fw.Access
	for _, ag := range rec.Agents {
		if ag.Name == agent {
			access = ag.Access
		}
	}
	var lin []fw.RunRef
	if run != nil {
		lin = lineage(run, byID)
	}
	return f.a.AuthorizeAll(ctx, []fw.AuthzCheck{{Action: action, Target: fw.Target{
		Agent: agent, Run: run, Member: rec.Name, Access: access, Lineage: lin,
	}}})[0]
}

// recordRules is the Authz of the rules mode for a record: the access
// configuration that the record names and the access blocks of its
// agents. A person that the static token acts for gets only what the
// token gets too (behalf.Guard, section 19).
func recordRules(rec *Record) fw.Authz {
	access := map[string]*fw.Access{}
	known := map[string]bool{}
	for _, ag := range rec.Agents {
		access[ag.Name] = ag.Access
		known[ag.Name] = true
	}
	return behalf.Guard(fw.NewRulesAuthz(fw.AccessConfig{Default: rec.Access.Default, Admins: rec.Access.Admins},
		func(name string) (fw.AgentProfile, bool) {
			if !known[name] {
				return fw.AgentProfile{}, false
			}
			return fw.AgentProfile{Name: name, Access: access[name]}, true
		}))
}
