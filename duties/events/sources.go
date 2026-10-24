package events

// This file holds the sources of the events: the runs of the fleet, which
// every replica reads at each pass, and the hooks of the wiki, the
// bridges, the routines, and the duties, which the replica that made the
// change calls.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/bridges"
	"github.com/bdubs00/featherweight-workbench/duties"
	"github.com/bdubs00/featherweight-workbench/fleet"
	"github.com/bdubs00/featherweight-workbench/pages"
)

// observe reads the runs of the fleet for run.failed and
// question.waiting, when a duty waits for one of them.
func (s *Service) observe(ctx context.Context) {
	waiters, err := s.waiters(ctx)
	if err != nil {
		s.log.Warn("events: the duties did not read", "error", err)
		return
	}
	failed, asked := waiters[duties.EventRunFailed], waiters[duties.EventQuestionWaiting]
	if len(failed) == 0 && len(asked) == 0 {
		return
	}
	runs, err := s.runs(ctx)
	if err != nil {
		s.log.Warn("events: the runs did not read", "error", err)
		return
	}
	now := s.now()
	for _, r := range runs {
		switch {
		case len(failed) > 0 && r.Parent == "" && r.State == fw.StateFailed && !owed(r) && now.Sub(r.UpdatedAt) <= failedWindow:
			s.note(ctx, event{
				Kind: duties.EventRunFailed, ID: r.ID, At: r.UpdatedAt, Agent: r.Agent, Duty: duties.NameOf(r), Chain: chainOf(r), Run: r.ID,
				Body: map[string]any{"run": r.ID, "agent": r.Agent, "title": r.Title, "error": cut(r.Error, 1000), "duty": duties.NameOf(r)},
			}, failed)
		case len(asked) > 0 && r.State == fw.StateInputRequired && r.Pending != nil:
			p := r.Pending
			at := p.RequestedAt
			if at.IsZero() {
				at = r.UpdatedAt
			}
			body := map[string]any{"run": r.ID, "agent": r.Agent, "title": r.Title, "tool": p.Tool, "reason": cut(p.Reason, 500), "since": at.UTC(), "duty": duties.NameOf(r)}
			s.note(ctx, event{
				Kind: duties.EventQuestionWaiting, ID: r.ID + "/" + p.ID, At: at, Agent: r.Agent, Duty: duties.NameOf(r), Chain: chainOf(r),
				Run: r.ID, Approval: p.ID, Body: body,
			}, asked)
		}
	}
}

// owed reports whether the intake of the kernel still owes the next step
// of a failed run, or retried it: the run is not the end of its job yet.
func owed(r *fw.Run) bool {
	outcome := r.Metadata[fw.MetaIntakeOutcome]
	return outcome == fw.IntakeRetried || (outcome == "" && r.Metadata[fw.MetaIntakeQueue] != "")
}

// cut cuts a text to at most n bytes, at a boundary of a character.
func cut(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && (s[n]&0xC0) == 0x80 {
		n--
	}
	return s[:n]
}

// noteOne notes an event of a hook for the duties that wait for its kind.
func (s *Service) noteOne(ctx context.Context, ev event) {
	waiters, err := s.waiters(ctx)
	if err != nil {
		s.log.Warn("events: the duties did not read, so an event starts nothing", "kind", ev.Kind, "error", err)
		return
	}
	if list := waiters[ev.Kind]; len(list) > 0 {
		s.note(ctx, ev, list)
	}
}

// source returns the duty and the depth of the chain of the run behind an
// event, or "" and 0.
func (s *Service) source(ctx context.Context, run string) (string, int) {
	if run == "" {
		return "", 0
	}
	r, err := s.run(ctx, run)
	if err != nil {
		return "", 0
	}
	return duties.NameOf(r), chainOf(r)
}

// onNotice turns a cap notice of a duty into cap.reached.
func onNotice(_ context.Context, a *fw.Agent, n duties.Notice) {
	if n.Kind != duties.NoticeCap {
		return
	}
	s := Of(a)
	if s == nil {
		return
	}
	s.async(func(ctx context.Context) {
		_, chain := s.source(ctx, n.Run)
		day := n.At.UTC().Format(time.DateOnly)
		s.noteOne(ctx, event{
			Kind: duties.EventCapReached, ID: n.Duty + "/" + day, At: n.At, Agent: n.Agent, Duty: n.Duty, Chain: chain,
			Body: map[string]any{"duty": n.Duty, "agent": n.Agent, "reason": n.Reason},
		})
	})
}

// onPage turns a change of a page into page.changed, for each service of
// the process. The writer of a page names its agent, agent:<profile>.
func onPage(info pages.PageInfo, deleted bool) {
	each(func(s *Service) {
		s.async(func(ctx context.Context) {
			duty, chain := s.source(ctx, info.Run)
			at := info.At
			if at.IsZero() {
				at = s.now()
			}
			s.noteOne(ctx, event{
				Kind: duties.EventPageChanged, ID: fmt.Sprintf("%s@%d", info.Path, info.Version), At: at,
				Agent: strings.TrimPrefix(info.By, "agent:"), Path: info.Path, Duty: duty, Chain: chain,
				Body: map[string]any{"path": info.Path, "title": info.Title, "by": info.By, "run": info.Run, "deleted": deleted},
			})
		})
	})
}

// onMessage turns the line of the close of a bridge into bridge.closed,
// for each service of the process.
func onMessage(bridge string, m bridges.Message) {
	if m.Event != bridges.EventClosed {
		return
	}
	each(func(s *Service) {
		s.async(func(ctx context.Context) {
			s.noteOne(ctx, event{
				Kind: duties.EventBridgeClosed, ID: fmt.Sprintf("%s/%d", bridge, m.Seq), At: m.At,
				Body: map[string]any{"bridge": bridge, "text": cut(m.Text, 500)},
			})
		})
	})
}

// onRoutine turns a routine that may go live into routine.ready, for each
// service of the process.
func onRoutine(agent, name string) {
	each(func(s *Service) {
		s.async(func(ctx context.Context) {
			rec, err := s.routine(fw.WithPrincipal(ctx, nil), agent, name)
			if err != nil || !rec.Ready {
				return
			}
			s.noteOne(ctx, event{
				Kind: duties.EventRoutineReady, ID: fmt.Sprintf("%s/%s/%d", agent, name, rec.Version()), At: rec.UpdatedAt, Agent: agent,
				Body: map[string]any{"agent": agent, "routine": name, "version": rec.Version()},
			})
		})
	})
}

// runsOfFleet returns the runs of this process and of the view of the
// fleet, for the front itself.
func (s *Service) runsOfFleet(ctx context.Context) ([]*fw.Run, error) {
	ctx = fw.WithPrincipal(ctx, nil)
	out, err := s.a.List(ctx)
	if err != nil {
		return nil, err
	}
	v := fleet.Of(s.a)
	if v == nil {
		return out, nil
	}
	remote, err := v.Runs(ctx)
	if err != nil {
		return out, err
	}
	seen := map[string]bool{}
	for _, r := range out {
		seen[r.ID] = true
	}
	for _, r := range remote {
		if !seen[r.ID] {
			out = append(out, r)
		}
	}
	return out, nil
}

// runOfFleet returns one run of this process or of the view.
func (s *Service) runOfFleet(ctx context.Context, id string) (*fw.Run, error) {
	ctx = fw.WithPrincipal(ctx, nil)
	r, err := s.a.Get(ctx, id)
	if err == nil || !errors.Is(err, fw.ErrRunNotFound) {
		return r, err
	}
	v := fleet.Of(s.a)
	if v == nil {
		return nil, err
	}
	return v.Run(ctx, id)
}
