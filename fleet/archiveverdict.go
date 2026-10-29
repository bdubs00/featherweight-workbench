package fleet

// This file keeps the reaction of a person on a run that only the archive
// holds (archive.go). The pool replaced the worker of such a run, so no
// kernel takes the reaction, and a write to the run answered 409 before.
// But a thumb on a result is a verdict of a person that the front keeps,
// not a message to the agent: Looks right and Needs work on a result of
// last week must land, or the result waits for a check for ever. So the
// front writes the reaction into the archive as the fw.run.reacted event
// that the kernel would have written, and the summary of the run applies
// it as it applies every event of the stream. The board, the inbox, and
// Results then read the verdict from the facts of the work of the run.
// No quality receipt records, because no kernel holds the run, and a note
// of Needs work reaches no agent.

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// ReactArchived records the reaction d of the person in ctx on the run id
// that only the archive holds, and returns the run after it. The caller
// needs the operator role on the run, as for a reaction on a live run. A
// thumb on the run as a whole (seq 0) of a run whose result a person
// checks goes on its newest fw.run.finished, as AtEnd puts it for a live
// run. A reaction that the person has on the event already changes
// nothing, and a removal of a reaction that the person does not have
// changes nothing. It returns fw.ErrRunNotFound when the archive does not
// hold the run for the caller, and ErrArchived when the fleet keeps no
// archive.
func ReactArchived(ctx context.Context, a *fw.Agent, id string, d fw.ReactionData) (*fw.Run, error) {
	f, _ := Of(a).(*Fleet)
	if f == nil || f.arch() == nil {
		return nil, ErrArchived
	}
	run, ok, err := ArchivedRun(ctx, a, id)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("fleet: run %s: %w", id, fw.ErrRunNotFound)
	}
	if err := f.allowArchived(ctx, run, fw.ActionRunOperate); err != nil {
		return nil, err
	}
	d.Reaction = strings.ToLower(strings.TrimSpace(d.Reaction))
	switch d.Reaction {
	case fw.ReactionUp, fw.ReactionDown, fw.ReactionHeart:
	default:
		return nil, fmt.Errorf("%w: a reaction is %q, %q, or %q, not %q", fw.ErrInvalidRequest, fw.ReactionUp, fw.ReactionDown, fw.ReactionHeart, d.Reaction)
	}
	if len(d.Note) > fw.MaxReactionNoteBytes || !utf8.ValidString(d.Note) {
		return nil, fmt.Errorf("%w: the note of a reaction is at most %d bytes of text", fw.ErrInvalidRequest, fw.MaxReactionNoteBytes)
	}
	person := fw.ActingFor(ctx)
	if p := fw.PrincipalFrom(ctx); person == "" && p != nil {
		person = p.Subject
	}
	switch p := fw.PrincipalFrom(ctx); {
	case d.By == "":
		d.By = person
	case d.By != person && p != nil && p.Kind != fw.KindToken && p.Kind != fw.KindWorkload:
		return nil, fmt.Errorf("%w: only the static token or a workload can react for another person", fw.ErrForbidden)
	}
	if d.Removed {
		d.Note = ""
	}
	d.Receipt = false
	events, err := f.arch().events(ctx, id)
	if err != nil {
		return nil, err
	}
	var last, end uint64
	for _, e := range events {
		last = max(last, e.Seq)
		if e.Type == fw.EvRunFinished {
			end = max(end, e.Seq)
		}
	}
	if d.Seq == 0 && end > 0 && toCheck(run) && (d.Reaction == fw.ReactionUp || d.Reaction == fw.ReactionDown) {
		d.Seq = end
	}
	if d.Seq > last {
		return nil, fmt.Errorf("%w: run %s has no event %d; its latest event is %d", fw.ErrInvalidRequest, id, d.Seq, last)
	}
	has := false
	for _, e := range events {
		var old fw.ReactionData
		if e.Type == fw.EvRunReacted && e.Decode(&old) == nil && old.Seq == d.Seq && old.Reaction == d.Reaction && old.By == d.By {
			has = !old.Removed
		}
	}
	if has == d.Removed {
		e, err := fw.NewEvent(id, fw.EvRunReacted, d)
		if err != nil {
			return nil, err
		}
		// The event comes after the last event of the run, as the kernel
		// would have appended it.
		e.Seq, e.Time = last+1, time.Now().UTC()
		if err := f.arch().add(ctx, e); err != nil {
			return nil, err
		}
	}
	got, ok, err := ArchivedRun(ctx, a, id)
	if err != nil || !ok {
		return run, err
	}
	return got, nil
}

// allowArchived decides an action of the caller in ctx on a run of the
// archive, as visibleArchived decides the view: with the access block of
// the profile of the agent in this process, and with the ancestors of the
// run that the archive holds, because whoever can act on an ancestor can
// act on the run.
func (f *Fleet) allowArchived(ctx context.Context, run *fw.Run, action string) error {
	cfg := f.a.Config()
	rec := &Record{Name: run.Member, Access: Access{Mode: f.a.AccessMode(), Default: cfg.Access.Default, Admins: cfg.Access.Admins}, Runs: []*fw.Run{run}}
	summaries := f.arch().summaries(ctx)
	for cur, depth := run, 0; cur.Parent != "" && depth < maxLineage; depth++ {
		parent := summaries[cur.Parent].listed()
		if parent == nil {
			break
		}
		rec.Runs = append(rec.Runs, parent)
		cur = parent
	}
	seen := map[string]bool{}
	for _, r := range rec.Runs {
		if r.Agent == "" || seen[r.Agent] {
			continue
		}
		seen[r.Agent] = true
		ag := RecordAgent{Name: r.Agent}
		if prof, ok := f.a.ProfileNamed(r.Agent); ok {
			ag.Description, ag.Access = prof.Description, prof.Access
		}
		rec.Agents = append(rec.Agents, ag)
	}
	return f.decide(ctx, rec, run.Agent, run, action)
}

// onChange holds the functions that run after a change that moves a card
// of the board: each reaction that a person sends through the relay,
// because a thumb is a verdict, and each end and each message of a run
// that the archive keeps, such as the new answer after Needs work. A
// package that keeps a copy of the board, such as work, forgets it then.
var onChange struct {
	mu sync.Mutex
	fs []func(*fw.Agent)
}

// OnChange adds f to the functions that run after each request of
// POST /v1/runs/{id}/reactions that the relay sees, whatever its answer,
// and after each fw.run.finished and fw.message.received that the archive
// keeps. A package calls it from init.
func OnChange(f func(*fw.Agent)) {
	onChange.mu.Lock()
	defer onChange.mu.Unlock()
	onChange.fs = append(onChange.fs, f)
}

// changed runs the functions of OnChange for a.
func changed(a *fw.Agent) {
	onChange.mu.Lock()
	fs := onChange.fs
	onChange.mu.Unlock()
	for _, f := range fs {
		f(a)
	}
}
