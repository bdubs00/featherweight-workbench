package routines

// This file holds what the routes and the tools share: the checks of the
// caller, a write of a body, and a move of a standing with its runbook.

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/fleet"
)

// ErrHidden means that the caller cannot view the agent, or that no agent
// of the fleet has the name. A route answers 404, so a caller learns
// nothing of an agent that it cannot view.
var ErrHidden = errors.New("routines: no agent that you can view has the name")

// allowed decides an action on the routines of an agent for the caller
// in ctx. A caller that cannot view the agent gets ErrHidden; one that can
// view it but not do the action gets the refusal of the kernel.
func allowed(ctx context.Context, a *fw.Agent, agent, action string) error {
	err := Check(ctx, a, agent, fw.ActionAgentView)
	switch {
	case errors.Is(err, fw.ErrProfileNotFound), errors.Is(err, fw.ErrForbidden):
		return fmt.Errorf("%w: %s", ErrHidden, agent)
	case err != nil:
		return err
	case action == fw.ActionAgentView:
		return nil
	}
	return Check(ctx, a, agent, action)
}

// visible returns the routines of the agent for the caller in ctx, or of
// every agent that the caller can view when agent is empty, the newest
// change first. A named agent that the caller cannot view is ErrHidden. An
// agent of the store that the caller cannot view, or that the fleet no
// longer runs, is left out of the whole list.
func visible(ctx context.Context, a *fw.Agent, s *Store, agent string) ([]Record, error) {
	if agent != "" {
		if err := allowed(ctx, a, agent, fw.ActionAgentView); err != nil {
			return nil, err
		}
		return s.List(ctx, agent)
	}
	all, err := s.All(ctx)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	out := []Record{}
	for _, rec := range all {
		ok, decided := seen[rec.Agent]
		if !decided {
			err := allowed(ctx, a, rec.Agent, fw.ActionAgentView)
			switch {
			case err == nil:
				ok = true
			case errors.Is(err, ErrHidden):
			default:
				return nil, err
			}
			seen[rec.Agent] = ok
		}
		if ok {
			out = append(out, rec)
		}
	}
	return out, nil
}

// putBody is the body of PUT /v1/routines/{agent}/{name} and the request of
// a write. Standing is proposed or rehearsing for a new routine, and empty
// or the standing of a routine that exists.
type putBody struct {
	Routine  *fw.Routine `json:"routine"`
	Standing string      `json:"standing,omitempty"`
	Source   *Source     `json:"source,omitempty"`
	Replay   *Replay     `json:"replay,omitempty"`
	Note     string      `json:"note,omitempty"`
}

// put writes the body of a routine for the caller in ctx. A new routine in
// the standing proposed needs the operator role on the agent
// (fw.ActionAgentRun). Every other write needs the owner role
// (fw.ActionAgentEdit), as agent_put does.
func put(ctx context.Context, a *fw.Agent, s *Store, agent, name string, in putBody) (Record, PutResult, error) {
	if in.Routine == nil {
		return Record{}, PutResult{}, fmt.Errorf("%w: the body is {\"routine\": {...}, \"standing\": \"proposed\", \"source\": {...}}, and routine is missing", ErrInvalid)
	}
	if err := checkKey(agent, name); err != nil {
		return Record{}, PutResult{}, err
	}
	body := *in.Routine
	switch body.Name {
	case "":
		body.Name = name
	case name:
	default:
		return Record{}, PutResult{}, fmt.Errorf("%w: the routine is named %s in the body and %s in the path", ErrInvalid, body.Name, name)
	}
	err := allowed(ctx, a, agent, fw.ActionAgentEdit)
	createOnly := false
	if errors.Is(err, fw.ErrForbidden) && (in.Standing == "" || in.Standing == Proposed) {
		// An operator proposes a new routine. Only an owner changes one,
		// also one that another writer creates in the meantime.
		if _, _, gerr := s.get(ctx, agent, name); errors.Is(gerr, ErrNotFound) {
			err = allowed(ctx, a, agent, fw.ActionAgentRun)
			in.Standing, createOnly = Proposed, true
		}
	}
	if err != nil {
		return Record{}, PutResult{}, err
	}
	by, note, _ := caller(ctx, in.Note)
	return s.Put(ctx, PutRequest{
		Agent: agent, Routine: body, Standing: in.Standing, Source: in.Source, Replay: in.Replay, By: by, Note: note,
		CreateOnly: createOnly, Check: checkFor(a, agent),
	})
}

// checkFor checks the routines that reach the profile of the agent with a
// write: the pools that run the agent must accept them. When no pool runs
// the agent, the kernel checked each body without the configuration of
// the workers, which holds the models that answer the questions, and
// nothing more is checked here.
func checkFor(a *fw.Agent, agent string) func(context.Context, []fw.Routine) error {
	return func(ctx context.Context, list []fw.Routine) error {
		_, err := fleet.CheckRoutines(ctx, a, agent, list)
		return err
	}
}

// moveBody is the body of POST /v1/routines/{agent}/{name}/standing.
// Version is optional: the version of the body that the caller read.
type moveBody struct {
	Standing string `json:"standing"`
	Note     string `json:"note,omitempty"`
	Version  int    `json:"version,omitempty"`
}

// move moves a routine to another standing for the caller in ctx, which
// needs the owner role on the agent. A move to live writes the runbook of
// the routine to the wiki, and a later move writes it again. A runbook
// that does not land leaves the move as it is, and the log says why.
func move(ctx context.Context, a *fw.Agent, s *Store, agent, name string, in moveBody) (Record, bool, error) {
	if err := checkKey(agent, name); err != nil {
		return Record{}, false, err
	}
	if err := allowed(ctx, a, agent, fw.ActionAgentEdit); err != nil {
		return Record{}, false, err
	}
	by, note, person := caller(ctx, in.Note)
	rec, moved, err := s.Move(ctx, MoveRequest{
		Agent: agent, Name: name, To: strings.TrimSpace(in.Standing), Note: note, By: by, Person: person, Version: in.Version,
		Check: checkFor(a, agent),
	})
	if err != nil || !moved {
		return rec, moved, err
	}
	a.Logger().Info("routines: a routine moved", "agent", agent, "routine", name, "version", rec.Version(), "standing", rec.Standing, "by", by)
	// A sign-off writes the runbook, and each later move writes it again,
	// so the page always says the standing of the routine.
	full, err := s.Get(ctx, agent, name)
	if err == nil {
		_, err = writeRunbook(ctx, a, full)
	}
	if err != nil {
		a.Logger().Warn("routines: the runbook of a routine did not follow its move", "agent", agent, "routine", name, "error", err)
	}
	return rec, true, nil
}

// reason returns the words of an error of the package without its prefix.
func reason(err error) string {
	s := err.Error()
	for _, e := range []error{ErrInvalid, ErrNotFound, ErrLimit, ErrConflict, ErrRetired, ErrHidden, fw.ErrInvalidRequest, fw.ErrForbidden} {
		s = strings.TrimPrefix(s, e.Error()+": ")
	}
	return s
}
