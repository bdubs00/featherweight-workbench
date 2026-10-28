// Package actions gives a duty the actions beside ask (docs/ARCHITECTURE.md
// section 24.1): post, which posts the message in a bridge that stays
// open; open, which opens a bridge from a playbook; and manager, which
// sends the message to the manager of the fleet. Each action goes through
// the Go function of the route that a person uses: post through the post
// of a bridge, open through the open of a bridge, and manager through the
// queue of the manager, as a run of the duty.
//
// The access of a duty is decided when a person writes it. Check runs
// then, with the person of the write, and lists the problems of the
// action, such as a bridge that the person cannot use. Start runs in the
// engine, as the front itself, and acts for the principal of the runs of
// the duty: duty:<name>, or its owner with runAs owner.
//
// The package registers the actions with duties.RegisterAction in init. A
// binary turns them on with a blank import.
package actions

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/bridges"
	"github.com/bdubs00/featherweight-workbench/duties"
	"github.com/bdubs00/featherweight-workbench/faces"
	"github.com/bdubs00/featherweight-workbench/fleet"
	"github.com/bdubs00/featherweight-workbench/manager"
	"github.com/bdubs00/featherweight-workbench/playbooks"
)

func init() {
	duties.RegisterAction(duties.ActionPost, duties.Action{Check: checkPost, Start: startPost})
	duties.RegisterAction(duties.ActionOpen, duties.Action{Check: checkOpen, Start: startOpen})
	duties.RegisterAction(duties.ActionManager, duties.Action{Check: checkManager, Start: startManager})
}

// problem writes a problem of the field at the dotted path.
func problem(path, format string, args ...any) string {
	return path + ": " + fmt.Sprintf(format, args...)
}

// principalOf returns the principal that a start of a duty acts for: the
// principal of its run request.
func principalOf(req fw.RunRequest) *fw.Principal {
	if req.Principal == "" {
		return nil
	}
	kind := req.PrincipalKind
	if kind == "" {
		kind = fw.KindUser
	}
	return &fw.Principal{Subject: req.Principal, Kind: kind}
}

// SenderName names a duty as the sender of a post in a bridge.
func SenderName(duty string) string { return "the duty " + duty }

// words returns the words of an error without the names of the packages
// and the kinds of error in front.
func words(err error) string {
	return strings.TrimSuffix(bridges.Detail(err), ".")
}

// checkPost lists the problems of do.post for the person who writes the
// duty: a bridge that the person can use, that stays open, and members
// that it has.
func checkPost(ctx context.Context, a *fw.Agent, d *duties.Duty) []string {
	p := d.Do.Post
	var out []string
	if strings.TrimSpace(p.Message) == "" {
		out = append(out, problem("do.post.message", "The message is empty. Say what the duty posts."))
	}
	id := strings.TrimSpace(p.Bridge)
	if id == "" {
		return append(out, problem("do.post.bridge", "Name the bridge, by its id, in which the duty posts."))
	}
	s := bridges.Of(a)
	if s == nil {
		return append(out, problem("do.post", "The bridges are off in this server."))
	}
	ok, err := s.Visible(ctx, id)
	switch {
	case err != nil:
		return append(out, problem("do.post.bridge", "The server did not decide whether you can use the bridge %s: %s.", id, words(err)))
	case !ok:
		return append(out, problem("do.post.bridge", "No bridge that you can use has the id %s.", id))
	}
	state, handles, err := s.Peek(ctx, id)
	if err != nil {
		return append(out, problem("do.post.bridge", "The bridge %s does not read: %s.", id, words(err)))
	}
	if state != bridges.StateOpen {
		out = append(out, problem("do.post.bridge", "The bridge %s is closed. A duty posts in a bridge that stays open.", id))
	}
	for _, h := range p.To {
		h = strings.TrimPrefix(strings.TrimSpace(h), "@")
		if h != bridges.HandleManager && !slices.Contains(handles, h) {
			out = append(out, problem("do.post.to", "%s is no member of the bridge %s.", h, id))
		}
	}
	return out
}

// startPost posts the message of a start in the bridge, as the principal
// of the duty, through the post of the route.
func startPost(ctx context.Context, a *fw.Agent, d *duties.Duty, req fw.RunRequest) (duties.Started, error) {
	s := bridges.Of(a)
	if s == nil {
		return duties.Started{}, errors.New("the bridges are off in this server")
	}
	p := d.Do.Post
	m, err := s.PostFor(ctx, strings.TrimSpace(p.Bridge), bridges.PostSpec{Text: req.Input, To: p.To}, principalOf(req), SenderName(d.Name))
	if err != nil {
		return duties.Started{}, fmt.Errorf("the post in the bridge %s: %s", p.Bridge, words(err))
	}
	return duties.Started{Note: fmt.Sprintf("Posted message %d in the bridge %s.", m.Seq, p.Bridge)}, nil
}

// checkOpen lists the problems of do.open for the person who writes the
// duty: a playbook that the person can read, with seats that the person
// can operate, and a goal that fits a bridge.
func checkOpen(ctx context.Context, a *fw.Agent, d *duties.Duty) []string {
	o := d.Do.Open
	name := strings.TrimSpace(o.Playbook)
	if name == "" {
		return []string{problem("do.open.playbook", "Name the playbook from which the duty opens a bridge.")}
	}
	b, ps := bridges.Of(a), playbooks.Of(a)
	if b == nil || ps == nil {
		return []string{problem("do.open", "The bridges or the playbooks are off in this server.")}
	}
	pb, err := ps.Get(ctx, name)
	switch {
	case errors.Is(err, playbooks.ErrNotFound):
		return []string{problem("do.open.playbook", "No playbook that you can read is named %s.", name)}
	case err != nil:
		return []string{problem("do.open.playbook", "The playbook %s does not read: %s.", name, words(err))}
	}
	var out []string
	for _, m := range pb.Seats {
		ok, err := b.Operates(ctx, m.Agent, m.Pool)
		switch {
		case err != nil:
			return append(out, problem("do.open.playbook", "The server did not decide whether you operate the agent %s: %s.", m.Agent, words(err)))
		case !ok:
			out = append(out, problem("do.open.playbook", "The seat %s of the playbook %s needs the operator role on the agent %s, which you do not have.", m.Handle, name, m.Agent))
		}
	}
	spec := pb.Spec(d.Name)
	if goal := strings.TrimSpace(o.Goal); goal != "" {
		spec.Goal = goal
	}
	for _, q := range b.CheckPlan(spec, false) {
		path := "do.open.playbook"
		if q.Path == "goal" && strings.TrimSpace(o.Goal) != "" {
			path = "do.open.goal"
		}
		out = append(out, problem(path, "%s.", capital(q.Text)))
	}
	return out
}

// capital writes the first letter of s as a capital.
func capital(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// Title returns the title of a bridge that a duty opens: "<duty>: <date>",
// with the date of the start in the time zone of the duty.
func Title(d *duties.Duty, slot time.Time) string {
	return d.Name + ": " + slot.In(d.Location()).Format(time.DateOnly)
}

// slotOf returns the time of a start from its run request, else now.
func slotOf(req fw.RunRequest) time.Time {
	if t, err := time.Parse(time.RFC3339Nano, req.Metadata[duties.MetaSlot]); err == nil {
		return t
	}
	return time.Now()
}

// startOpen opens a bridge from the playbook of the duty, as the front
// for the principal of the duty, through the open of the route. The title
// is "<duty>: <date>", and the goal is the goal of the duty, with its
// placeholders filled, else the goal of the playbook. limits.budget.maxTurns
// of the duty caps the turns of the whole bridge.
func startOpen(ctx context.Context, a *fw.Agent, d *duties.Duty, req fw.RunRequest) (duties.Started, error) {
	b, ps := bridges.Of(a), playbooks.Of(a)
	if b == nil || ps == nil {
		return duties.Started{}, errors.New("the bridges or the playbooks are off in this server")
	}
	name := strings.TrimSpace(d.Do.Open.Playbook)
	pb, err := ps.Lookup(ctx, name)
	if err != nil {
		return duties.Started{}, fmt.Errorf("the playbook %s: %s", name, words(err))
	}
	spec := pb.Spec(Title(d, slotOf(req)))
	spec.Owner = d.Owner
	if b := d.Limits.Budget; b != nil && b.MaxTurns > 0 {
		// The budget of the duty caps the turns of the whole bridge, below
		// the cap of the playbook when it sets a lower one.
		turns := b.MaxTurns
		var caps bridges.CapsPatch
		if spec.Caps != nil {
			caps = *spec.Caps
		}
		if caps.MaxTurns == nil || *caps.MaxTurns > turns {
			caps.MaxTurns = &turns
		}
		spec.Caps = &caps
	}
	if goal := strings.TrimSpace(req.Input); goal != "" {
		spec.Goal = goal
	}
	it, err := b.OpenFor(ctx, spec, principalOf(req))
	if err != nil {
		return duties.Started{}, fmt.Errorf("the bridge from the playbook %s did not open: %s", name, words(err))
	}
	return duties.Started{Note: fmt.Sprintf("Opened the bridge %s %q from the playbook %s.", it.ID, it.Title, name)}, nil
}

// checkManager lists the problems of do.manager for the person who writes
// the duty: a manager of the fleet that the person can run, with a queue.
func checkManager(ctx context.Context, a *fw.Agent, d *duties.Duty) []string {
	var out []string
	if strings.TrimSpace(d.Do.Manager.Message) == "" {
		out = append(out, problem("do.manager.message", "The message is empty. Say what the manager does."))
	}
	mgr := manager.Profile(a.Config())
	if mgr == "" {
		return append(out, problem("do.manager", "This server names no manager. An operator sets workbench.bossmode.manager (%s).", manager.EnvManager))
	}
	if err := faces.Check(ctx, a, mgr, fw.ActionAgentRun); err != nil {
		return append(out, problem("do.manager", "You cannot run the manager %s: %s.", mgr, words(err)))
	}
	if _, err := fleet.QueueFor(fw.WithPrincipal(ctx, nil), a, mgr, ""); err != nil {
		out = append(out, problem("do.manager", "The manager %s has no queue: %s.", mgr, words(err)))
	}
	return out
}

// startManager puts the message of a start on the queue of the manager,
// as a run of the duty: the run request of the start, with its id, its
// metadata, its budget, and its principal, for the profile of the
// manager. The engine follows that run as it follows a run of ask.
func startManager(ctx context.Context, a *fw.Agent, d *duties.Duty, req fw.RunRequest) (duties.Started, error) {
	mgr := manager.Profile(a.Config())
	if mgr == "" {
		return duties.Started{}, errors.New("this server names no manager")
	}
	req.Agent = mgr
	req.Input = "From the duty " + d.Name + ".\n\n" + req.Input
	_, sent, err := fleet.EnqueueFor(fw.WithPrincipal(ctx, nil), a, mgr, "", []fw.RunRequest{req})
	if err == nil && sent == 0 {
		err = errors.New("no request went out")
	}
	if err != nil {
		return duties.Started{}, fmt.Errorf("the queue of the manager %s did not take the message: %s", mgr, words(err))
	}
	return duties.Started{Run: req.ID}, nil
}
