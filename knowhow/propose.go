package knowhow

// This file is the pass that proposes routines (docs/KNOWHOW.md section
// 6.5). It runs once a day, and an owner starts it at once with the route
// POST /v1/knowhow/propose. For each Duty of each agent, it takes the
// completed runs of the archive of the last propose.days days. It needs
// propose.minRuns runs, propose.minVerified of them verified, and it
// leaves out every run that a receipt sent back, that its grader failed,
// that a person gave a thumbs down, or that a routine did. It calls
// routine.Propose and then routine.LeaveOneOut. A proposal with a free
// hole or a problem, or one that did not hold on a run that it did not
// see, stays out, and the journal says why once per reason. A good
// proposal goes to the store of the routines bound to its Duty, as
// proposed, and moves to rehearsing at once, unless the switch of the
// agent is on: then the move waits for a person. The journal records each
// step.

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/routine"
	"github.com/bdubs00/featherweight-workbench/fleet"
	"github.com/bdubs00/featherweight-workbench/routines"
)

// Bounds of a proposal.
const (
	// maxProposeRuns bounds the runs of one Duty that a proposal reads,
	// the newest ones.
	maxProposeRuns = 50
	// ProposedBy names the proposals of this pass in the source of a
	// routine.
	ProposedBy = "Know-how, from the runs of its Duty"
)

// ProposeResult says what a pass did: the routines that it proposed, by
// agent/name, and the Duties whose proposal stayed out, by agent/duty,
// with the reason.
type ProposeResult struct {
	Proposed []string          `json:"proposed"`
	StaysOut map[string]string `json:"staysOut"`
}

// proposal is the entry of the last reason why the proposal of a Duty
// stayed out, so the journal says each reason once.
type proposal struct {
	Reason string    `json:"reason"`
	At     time.Time `json:"at"`
}

func proposalKey(agent, duty string) string { return "prop." + agentKey(agent) + "." + token(duty) }

// RoutineName is the name of the routine of a Duty: duty- and the name of
// the Duty in lower case, with - for every other character, at most 64.
func RoutineName(duty string) string {
	var b strings.Builder
	b.WriteString("duty-")
	dash := false
	for _, r := range strings.ToLower(duty) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
			dash = false
		} else if !dash {
			b.WriteByte('-')
			dash = true
		}
	}
	name := strings.TrimRight(b.String(), "-")
	if len(name) > 64 {
		name = strings.TrimRight(name[:64], "-")
	}
	return name
}

// Propose runs the pass over the archive.
func (s *Service) Propose(ctx context.Context) (ProposeResult, error) { return s.ProposeFor(ctx, nil) }

// ProposeFor runs the pass over the runs of the agents that keep takes,
// or of every agent for a nil keep. The route POST /v1/knowhow/propose
// runs it at once for the agents of an owner.
func (s *Service) ProposeFor(ctx context.Context, keep func(agent string) bool) (ProposeResult, error) {
	out := ProposeResult{Proposed: []string{}, StaysOut: map[string]string{}}
	st := s.routineStore()
	if st == nil {
		return out, nil
	}
	since := s.now().UTC().AddDate(0, 0, -s.cfg.Propose.Days)
	groups := map[string]map[string][]fleet.ArchiveEntry{}
	for _, e := range s.archive(ctx) {
		r := e.Run
		duty := dutyOf(r)
		if duty == "" || r.State != fw.StateCompleted || r.Depth != 0 || r.CreatedAt.Before(since) || CheckAgent(r.Agent) != nil ||
			r.Agent == s.cfg.Review.Agent || r.Metadata[MetaReview] != "" || (keep != nil && !keep(r.Agent)) {
			continue
		}
		if groups[r.Agent] == nil {
			groups[r.Agent] = map[string][]fleet.ArchiveEntry{}
		}
		groups[r.Agent][duty] = append(groups[r.Agent][duty], e)
	}
	agents := make([]string, 0, len(groups))
	for agent := range groups {
		agents = append(agents, agent)
	}
	sort.Strings(agents)
	for _, agent := range agents {
		duties := make([]string, 0, len(groups[agent]))
		for duty := range groups[agent] {
			duties = append(duties, duty)
		}
		sort.Strings(duties)
		for _, duty := range duties {
			if ctx.Err() != nil {
				return out, ctx.Err()
			}
			name, why, err := s.proposeDuty(ctx, st, agent, duty, groups[agent][duty])
			switch {
			case err != nil:
				s.log.Warn("knowhow: the proposal of a Duty failed", "agent", agent, "duty", duty, "error", err)
			case name != "":
				out.Proposed = append(out.Proposed, agent+"/"+name)
			case why != "":
				out.StaysOut[agent+"/"+duty] = why
			}
		}
	}
	return out, nil
}

// proposeDuty proposes the routine of one Duty from its runs. It returns
// the name of a routine that it proposed, or the reason why the proposal
// stayed out, or neither when the Duty has too few runs or a routine
// already.
func (s *Service) proposeDuty(ctx context.Context, st *routines.Store, agent, duty string, entries []fleet.ArchiveEntry) (string, string, error) {
	name := RoutineName(duty)
	if routines.CheckName(name) != nil {
		return "", "", nil
	}
	if _, err := st.Get(ctx, agent, name); err == nil {
		return "", "", nil
	}
	if len(entries) < s.cfg.Propose.MinRuns {
		return "", "", nil
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Run.CreatedAt.After(entries[j].Run.CreatedAt) })
	if len(entries) > maxProposeRuns {
		entries = entries[:maxProposeRuns]
	}
	var runs [][]fw.Event
	var verified, used []string
	var from, to time.Time
	for _, e := range entries {
		f, events, err := s.readRun(ctx, e)
		if err != nil {
			return "", "", err
		}
		if f.rejected() || f.Down || f.failedGrade() || f.ByRoutine || len(events) == 0 {
			continue
		}
		runs = append(runs, events)
		used = append(used, e.Run.ID)
		if f.verified() {
			verified = append(verified, e.Run.ID)
		}
		if from.IsZero() || e.Run.CreatedAt.Before(from) {
			from = e.Run.CreatedAt
		}
		if e.Run.CreatedAt.After(to) {
			to = e.Run.CreatedAt
		}
	}
	if len(runs) < s.cfg.Propose.MinRuns || len(verified) < s.cfg.Propose.MinVerified {
		return "", "", nil
	}
	opts := routine.ProposeOptions{Name: name, Decider: s.cfg.Propose.Decider}
	r, prop := routine.Propose(runs, opts)
	if !prop.Complete {
		return "", s.staysOut(ctx, agent, duty, name, incomplete(prop), verified), nil
	}
	if len(r.Questions) > 0 && r.Decider == "" {
		return "", s.staysOut(ctx, agent, duty, name, "The routine needs a decider for its questions, and workbench.knowhow.propose.decider names none.", verified), nil
	}
	loo := routine.LeaveOneOut(ctx, runs, opts)
	if !loo.Holds() {
		return "", s.staysOut(ctx, agent, duty, name, didNotHold(loo), verified), nil
	}
	r.Duty = duty
	rec, res, err := st.Put(withSelf(ctx), routines.PutRequest{
		Agent: agent, Routine: r, Standing: routines.Proposed,
		Source: &routines.Source{Runs: len(used), From: from.UTC().Format(time.DateOnly), To: to.UTC().Format(time.DateOnly), ProposedBy: ProposedBy},
		By:     ByKnowhow, Note: "proposed from the runs of the Duty " + cut(duty, 200), CreateOnly: true, Check: s.checkFor(agent),
	})
	if err != nil {
		return "", s.staysOut(ctx, agent, duty, name, "The store of the routines refused the proposal: "+reason(err), verified), nil
	}
	if !res.Created {
		return "", "", nil
	}
	shown := verified
	if len(shown) > 10 {
		shown = shown[:10]
	}
	why := fmt.Sprintf("%d runs of the Duty %s did the same steps, %d of them verified, and the routine held on each run that it did not see.", len(used), duty, len(verified))
	if _, err := s.appendJournal(ctx, Entry{Agent: agent, What: WhatProposed, Subject: Subject{Routine: name}, Why: why, Runs: shown, By: ByKnowhow}); err != nil {
		s.log.Warn("knowhow: the journal did not take a proposal", "agent", agent, "routine", name, "error", err)
	}
	s.forget(ctx, agent, duty)
	if s.AskFirst(ctx, agent) {
		if _, err := s.addWaiting(ctx, Waiting{Agent: agent, What: WhatRehearsing, Subject: Subject{Routine: name}, Why: why, Runs: shown,
			By: ByKnowhow, Pin: fmt.Sprintf("v%d", rec.Version())}); err != nil {
			return name, "", err
		}
		return name, "", nil
	}
	_, moved, err := st.Move(withSelf(ctx), routines.MoveRequest{Agent: agent, Name: name, To: routines.Rehearsing, By: ByKnowhow,
		Note: "Know-how rehearses a new proposal at once", Version: rec.Version(), Check: s.checkFor(agent)})
	if err != nil {
		return name, "", fmt.Errorf("knowhow: move the routine %s of %s to rehearsing: %w", name, agent, err)
	}
	if moved {
		if _, err := s.appendJournal(ctx, Entry{Agent: agent, What: WhatRehearsing, Subject: Subject{Routine: name},
			Why: fmt.Sprintf("The routine %s rehearses beside the agent and does not act yet.", name), Runs: shown, By: ByKnowhow,
			Undo: &Undo{From: routines.Proposed, To: routines.Rehearsing, Version: rec.Version()}}); err != nil {
			s.log.Warn("knowhow: the journal did not take a move", "agent", agent, "routine", name, "error", err)
		}
	}
	return name, "", nil
}

// incomplete words why a proposal is not complete.
func incomplete(p routine.Proposal) string {
	var free []string
	for _, h := range p.Holes {
		if h.Kind == "free" {
			free = append(free, fmt.Sprintf("step %d %s", h.Step, h.Path))
		}
	}
	switch {
	case len(free) > 0:
		return fmt.Sprintf("The routine cannot fill %d values from the runs yet: %s.", len(free), strings.Join(free[:min(len(free), 5)], ", "))
	case len(p.Problems) > 0:
		return "The routine has a problem: " + cut(p.Problems[0], 200)
	}
	return "The runs do not agree on the steps."
}

// didNotHold words a test on runs that the proposal did not see.
func didNotHold(r routine.LeaveOneOutResult) string {
	switch {
	case len(r.Diverged) > 0:
		d := r.Diverged[0]
		return fmt.Sprintf("Tested on runs that it did not see, the routine differed from the agent in %d runs, first in the run %s.", len(r.Diverged), d.Run)
	case r.Incomplete > 0:
		return fmt.Sprintf("Without one of the runs, the routine could not fill every value in %d cases.", r.Incomplete)
	}
	return fmt.Sprintf("Tested on runs that it did not see, the routine could not read %d runs.", r.Unreadable)
}

// staysOut writes the journal when the reason of a Duty changed, and
// returns the reason.
func (s *Service) staysOut(ctx context.Context, agent, duty, name, why string, runs []string) string {
	wrote, _, _, err := s.modify(ctx, proposalKey(agent, duty), func(value []byte, exists bool, _ uint64) ([]byte, error) {
		var p proposal
		if exists && json.Unmarshal(value, &p) == nil && p.Reason == why {
			return nil, nil
		}
		return encode(proposal{Reason: why, At: s.now().UTC()})
	})
	if err == nil && wrote {
		if len(runs) > 10 {
			runs = runs[:10]
		}
		if _, err := s.appendJournal(ctx, Entry{Agent: agent, What: WhatStaysOut, Subject: Subject{Routine: name}, Why: why, Runs: slices.Clone(runs), By: ByKnowhow}); err != nil {
			s.log.Warn("knowhow: the journal did not take a proposal that stayed out", "agent", agent, "duty", duty, "error", err)
		}
	}
	return why
}

// forget removes the reason of a Duty whose routine was proposed.
func (s *Service) forget(ctx context.Context, agent, duty string) {
	if got, err := s.b.Get(ctx, proposalKey(agent, duty)); err == nil {
		_ = s.b.Delete(ctx, proposalKey(agent, duty), got.Revision)
	}
}
