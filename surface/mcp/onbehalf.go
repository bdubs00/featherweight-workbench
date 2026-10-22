package mcp

// This file applies the on-behalf-of of the package manager to the tools
// of the server (docs/ARCHITECTURE.md section 19). When the static token of
// the fleet calls for a run of a worker, each tool that reads or acts
// calls manager.OnBehalf at its start, so it reads and acts as the person
// of that run: run, run_status, approve, cancel, send, fork, run_budget,
// whoami, agents_list, agent_get, agent_put, agent_delete, runs_list,
// run_events, run_file, run_result, agent_builder, capabilities,
// dashboard, usage, enqueue, batch, and fleet. The tools connections and
// disconnect keep the caller, because the grants of a person are no
// business of a run, and so do the exposed tools of mcp.expose and the
// tools that other packages register, which read the run behind a call
// with CallRun themselves.
//
// What an actor does carries its name: the note of a decision, the sender
// of a message, the metadata key manager.MetaVia of a run that it starts,
// and a line of the log for a cancel and a change of a budget. A manager
// run that acts on a run of a bridge needs the delegation of the operator
// of the bridge (manager.Delegated).

import (
	"context"
	"errors"
	"maps"
	"strings"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/fleet"
	"github.com/bdubs00/featherweight-workbench/internal/behalf"
	"github.com/bdubs00/featherweight-workbench/manager"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// callerOf is the subject of the caller of the request. When the server
// acts for the person of a run, that is the static token, which the owner
// of a run records as the decider.
func callerOf(ctx context.Context, req sdk.Request) string {
	if c := behalf.Caller(ctx); c != nil {
		return c.Subject
	}
	return principalOf(ctx, req)
}

// batchOf returns the batch of a run, fw.batch, from this process or from
// the view of the fleet, decided as a read of the principal in ctx. A run
// that neither knows gives "" and no error: the action that follows fails
// on it.
func (s *server) batchOf(ctx context.Context, id string) (string, error) {
	r, err := s.a.Get(ctx, id)
	if err == nil {
		return r.Metadata[fw.MetaBatch], nil
	}
	if !errors.Is(err, fw.ErrRunNotFound) {
		return "", err
	}
	f := fleet.Of(s.a)
	if f == nil {
		return "", nil
	}
	r, err = f.Run(ctx, id)
	switch {
	case errors.Is(err, fw.ErrRunNotFound):
		return "", nil
	case err != nil:
		return "", err
	}
	return r.Metadata[fw.MetaBatch], nil
}

// delegated refuses an action of a manager run on a run of a bridge whose
// operator did not delegate it, and returns nil to go on.
func (s *server) delegated(ctx context.Context, act manager.Actor, id, action string) *sdk.CallToolResult {
	if !act.IsManager(s.a.Config()) {
		return nil
	}
	batch, err := s.batchOf(ctx, id)
	if err != nil {
		return failResult(err)
	}
	err = manager.Delegated(ctx, s.a.Config(), act, batch, action)
	var refused *manager.NotDelegated
	switch {
	case errors.As(err, &refused):
		s.log.Info("mcp refused an action of the manager on a run of a bridge", "run", id, "batch", batch, "action", action, "actor", act.String())
		return errorResult("Forbidden: " + trimForbidden(refused.Error()) + ". Tell the operator, who can delegate it on the bridge or do it in bossmode.")
	case err != nil:
		return errorResult("Unavailable: " + err.Error() + ". Try again in a few seconds.")
	}
	return nil
}

// trimForbidden removes the text of fw.ErrForbidden in front of a reason.
func trimForbidden(msg string) string {
	if rest, ok := strings.CutPrefix(msg, fw.ErrForbidden.Error()+": "); ok {
		return rest
	}
	return msg
}

// mayOnMember decides, before the server forwards an action of an actor
// to the member that owns a run, each action for the person of the actor
// (fleet.AllowRun). The request carries the static token, so the owner
// decides about the token, and the front decides about the person here. It
// returns nil to go on.
func (s *server) mayOnMember(ctx context.Context, act manager.Actor, acting bool, id string, actions ...string) *sdk.CallToolResult {
	if !acting || !act.ForPerson() {
		return nil
	}
	for _, action := range actions {
		if err := fleet.AllowRun(ctx, fleet.Of(s.a), id, action); err != nil {
			return failResult(err)
		}
	}
	return nil
}

// viaRefusal is the answer to a caller that names the metadata key of an
// actor itself.
const viaRefusal = "Invalid request: the metadata key " + manager.MetaVia + " names the run that started a run, and only the front sets it."

// hasVia reports whether metadata names manager.MetaVia.
func hasVia(md map[string]string) bool {
	_, ok := md[manager.MetaVia]
	return ok
}

// withVia returns a copy of md with manager.MetaVia for the actor.
func withVia(md map[string]string, act manager.Actor) map[string]string {
	out := maps.Clone(md)
	if out == nil {
		out = map[string]string{}
	}
	out[manager.MetaVia] = act.Via()
	return out
}
