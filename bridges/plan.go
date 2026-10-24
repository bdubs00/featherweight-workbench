package bridges

// This file checks the plan of a bridge, the body of POST /v1/bridges, and
// opens a bridge for the routes and for the packages of the front, such as
// a playbook that a duty opens (docs/ARCHITECTURE.md section 24). The
// route, the tool bridge_open, and a playbook check the plan with the same
// function, checkPlan.

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// PlanProblem is one problem of the plan of a bridge: the dotted path of
// its field, such as members[0].handle, and the problem in words.
type PlanProblem struct {
	Path string
	Text string
}

// CheckPlan lists what does not fit in the shape of the plan of a bridge,
// in the order of its fields, as Open checks it. With needTitle false,
// the plan needs no title, as a playbook. It checks the shape alone: Open
// also checks the role of the caller on each agent and the queue of each
// seat.
func (s *Store) CheckPlan(spec OpenSpec, needTitle bool) []PlanProblem {
	return checkPlan(spec, s.cfg, needTitle)
}

// checkPlan is CheckPlan with the configuration of the bridges.
func checkPlan(spec OpenSpec, cfg Config, needTitle bool) []PlanProblem {
	var p []PlanProblem
	add := func(path, format string, args ...any) {
		p = append(p, PlanProblem{Path: path, Text: fmt.Sprintf(format, args...)})
	}
	title := strings.TrimSpace(spec.Title)
	switch {
	case needTitle && title == "":
		add("title", "a bridge needs a title")
	case utf8.RuneCountInString(title) > maxTitleRunes:
		add("title", "the title of a bridge holds at most %d characters", maxTitleRunes)
	}
	if len(spec.Goal) > maxGoalBytes {
		add("goal", "the goal of a bridge holds at most %d bytes", maxGoalBytes)
	}
	switch {
	case len(spec.Members) == 0:
		add("members", "a bridge needs at least one member")
	case len(spec.Members) > cfg.MaxMembers:
		add("members", "a bridge holds at most %d members (workbench.bridges.maxMembers)", cfg.MaxMembers)
	}
	placement := strings.ToLower(strings.TrimSpace(spec.Placement))
	if placement != "" && placement != PlacementQueue && placement != PlacementSpawn {
		add("placement", "the placement of a bridge is queue or spawn, not %q", spec.Placement)
	}
	seen := map[string]bool{}
	for i, m := range spec.Members {
		m.Handle, m.Agent, m.Task, m.Pool = strings.TrimSpace(m.Handle), strings.TrimSpace(m.Agent), strings.TrimSpace(m.Task), strings.TrimSpace(m.Pool)
		at := fmt.Sprintf("members[%d]", i)
		for _, q := range seatProblems(m) {
			add(at+"."+q.Path, "%s", q.Text)
		}
		if seen[m.Handle] {
			add(at+".handle", "the handle %s is taken twice", m.Handle)
		}
		seen[m.Handle] = true
	}
	caps := cfg.caps()
	spec.Caps.apply(&caps)
	if problem := checkCaps(caps); problem != "" {
		add("caps", "the caps of the bridge: %s", problem)
	}
	return p
}

// seatProblems lists what does not fit in one seat of a plan, each with
// the path of its field in the seat.
func seatProblems(m SeatSpec) []PlanProblem {
	var p []PlanProblem
	add := func(path, format string, args ...any) {
		p = append(p, PlanProblem{Path: path, Text: fmt.Sprintf(format, args...)})
	}
	switch {
	case !handlePattern.MatchString(m.Handle):
		add("handle", "the handle %q must be lower-case letters, digits, dots, dashes, or underscores, at most 32, and start with a letter or a digit", m.Handle)
	case m.Handle == HandleOperator || m.Handle == HandleManager:
		add("handle", "the handle %s is reserved: %s addresses the people of a bridge, and %s names its manager", m.Handle, HandleOperator, HandleManager)
	}
	if !agentPattern.MatchString(m.Agent) {
		add("agent", "the seat %s needs an agent, the name of a profile, not %q", m.Handle, m.Agent)
	}
	switch {
	case strings.TrimSpace(m.Task) == "":
		add("task", "the seat %s needs a task", m.Handle)
	case len(m.Task) > maxTaskBytes:
		add("task", "the task of the seat %s holds more than %d bytes", m.Handle, maxTaskBytes)
	}
	if b := m.Budget; b != nil {
		if b.MaxTurns < 0 {
			add("budget.maxTurns", "the budget of the seat %s has a negative maxTurns", m.Handle)
		}
		if b.Deadline != "" {
			if d, err := time.ParseDuration(b.Deadline); err != nil || d <= 0 {
				add("budget.deadline", "the deadline of the seat %s must be a duration such as 30m, not %q", m.Handle, b.Deadline)
			}
		}
	}
	return p
}

// Operates reports whether the caller in ctx has the operator role on an
// agent, as the routes decide for each seat of a bridge: on the profile in
// the pool that runs it, else on the profile of this process, else on an
// agent that a member of the view serves. The process itself operates
// every agent. An error means that the decision point did not answer.
func (s *Store) Operates(ctx context.Context, agent, pool string) (bool, error) {
	return s.operates(ctx, agent, pool)
}

// Open opens a bridge from a plan for the caller in ctx, as POST
// /v1/bridges does, and returns it with its members. The caller needs the
// operator role on the agent of each seat. It returns an error that wraps
// fw.ErrInvalidRequest for a plan that does not fit, fw.ErrForbidden for
// a seat that the caller cannot operate, and the error of a manager that
// did not start.
func (s *Store) Open(ctx context.Context, spec OpenSpec) (Item, error) {
	rec, err := s.open(ctx, spec, fw.PrincipalFrom(ctx), nil)
	if err != nil {
		return Item{}, err
	}
	return s.item(ctx, rec, subjectOf(ctx), true)
}

// OpenFor opens a bridge from a plan as the front itself, for the
// operator by, and returns it with its members. The front decided the
// access before, as a duty decides when a person writes it, so OpenFor
// checks the plan and the queues and no role. The bridge is for by: by is
// its operator, and the runs of its seats act as by.
func (s *Store) OpenFor(ctx context.Context, spec OpenSpec, by *fw.Principal) (Item, error) {
	fctx := s.asFront(ctx)
	rec, err := s.open(fctx, spec, by, nil)
	if err != nil {
		return Item{}, err
	}
	subject := ""
	if by != nil {
		subject = by.Subject
	}
	return s.item(fctx, rec, subject, true)
}

// PostFor posts a message to the channel of an open bridge as the front
// itself, for the sender by, through the function of POST
// /v1/bridges/{id}/messages. The front decided the access before, as a
// duty decides when a person writes it. display names the sender in the
// channel, such as "the duty daily-report". It returns an error that
// wraps ErrNotFound for an unknown bridge and ErrConflict for a closed
// one.
func (s *Store) PostFor(ctx context.Context, id string, spec PostSpec, by *fw.Principal, display string) (Message, error) {
	fctx := s.asFront(ctx)
	if _, _, err := s.b.get(fctx, id); err != nil {
		return Message{}, err
	}
	snd := sender{subject: subjectOfPrincipal(by), display: display}
	return s.post(fctx, id, snd, spec)
}

// Peek returns the state of a bridge, open or closed, and the handles of
// its seats that are not fired, for the front itself, or an error that
// wraps ErrNotFound.
func (s *Store) Peek(ctx context.Context, id string) (string, []string, error) {
	rec, _, err := s.b.get(s.asFront(ctx), id)
	if err != nil {
		return "", nil, err
	}
	var handles []string
	for _, m := range rec.Members {
		if !m.Fired {
			handles = append(handles, m.Handle)
		}
	}
	return rec.State, handles, nil
}
