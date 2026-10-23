package bridges

// This file derives the presence of a member, the status light of the
// roster (docs/ARCHITECTURE.md section 18.7), from the newest run of the
// member in the bridge and the state of the Task that holds it, and for a
// seat without a run from the record of its bridge. Nothing stores a
// presence: the routes and the hooks compute it from the view.

import (
	"strings"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/surface/view"
	"github.com/bdubs00/featherweight-workbench/fleet"
)

// Presence returns the presence of a member: PresenceWorking,
// PresenceNeedsYou, PresenceIdle, PresenceAsleep, PresenceLimit,
// PresenceError, PresenceFired, or PresenceWaiting. runs holds the runs of the member in
// the bridge, in any order; the newest one decides. taskState is the state
// of the Task that holds the newest run: fleet.MemberLive,
// fleet.MemberQuiet, fleet.MemberStopped, TaskBlocked, or "" when the view
// does not know the Task, which counts as live.
//
// The rules, the first that holds wins:
//
//   - fired: the seat is fired.
//   - waiting: the view holds no run of the member.
//   - limit: the newest run stopped at a limit of its budget, on a Task
//     that is neither blocked nor stopped. No fault: a person continues
//     the run with more room (section 18.9).
//   - error: the newest run failed, or the Task is blocked or stopped.
//   - needs-you: the newest run waits for an approval, an answer, or a
//     connection, also on a Task that sleeps, because a decision wakes it.
//   - asleep: the Task is quiet.
//   - working: the newest run is queued, running, or suspended.
//   - idle: the newest run finished, and the Task is live.
//
// The routes and the hooks apply two more rules first, from the record of
// the bridge (decorate): a seat whose newest run no worker of the fleet
// holds, as the channel said with the line lost, is idle when the channel
// holds an end of that run, and in error otherwise; and a seat without a
// run is waiting when something seats it, and not-started otherwise.
func Presence(m Member, runs []*fw.Run, taskState string) string {
	if m.Fired {
		return PresenceFired
	}
	var newest *fw.Run
	for _, r := range runs {
		if r == nil {
			continue
		}
		if newest == nil || r.CreatedAt.After(newest.CreatedAt) || (r.CreatedAt.Equal(newest.CreatedAt) && r.ID > newest.ID) {
			newest = r
		}
	}
	if newest == nil {
		return PresenceWaiting
	}
	switch {
	case taskState == TaskBlocked, taskState == fleet.MemberStopped, newest.Blocked != "":
		return PresenceError
	case limited(newest):
		return PresenceLimit
	case newest.State == fw.StateFailed:
		return PresenceError
	case newest.State == fw.StateInputRequired:
		return PresenceNeedsYou
	case taskState == fleet.MemberQuiet:
		return PresenceAsleep
	case !newest.State.Terminal():
		return PresenceWorking
	}
	return PresenceIdle
}

// activityOf words the activity of a run (fw.Run.Activity) for a member.
// The phrase comes from the function of UsePhrase. Without one, a run that
// thinks gets the phrase of view.ActivityPhrase, and a run that waits for
// a tool call gets an empty phrase, because the view holds no tool input.
func activityOf(r *fw.Run) *Activity {
	if r == nil || r.Activity == nil {
		return nil
	}
	act := r.Activity
	out := &Activity{Turn: act.Turn, Tool: act.Tool, CallID: act.CallID, Since: act.Since}
	switch fn := phraseFn(); {
	case fn != nil:
		out.Phrase = fn(r)
	case act.Tool == "":
		out.Phrase = view.ActivityPhrase(r, nil)
	}
	return out
}

// decorate returns the members of a record with the presence and the
// activity of each, from the runs of the bridge and the states of the
// Tasks.
func decorate(rec *record, runs map[string]*fw.Run, states map[string]string) []Member {
	out := rec.public().Members
	for i := range out {
		m := &out[i]
		var list []*fw.Run
		for _, id := range m.Runs {
			if r := runs[id]; r != nil {
				list = append(list, r)
			}
		}
		var newest *fw.Run
		for _, r := range list {
			if newest == nil || r.CreatedAt.After(newest.CreatedAt) {
				newest = r
			}
		}
		m.Presence = Presence(*m, list, taskOf(newest, states))
		if !m.Fired {
			m.Activity = activityOf(newest)
		}
		if p, ok := gonePresence(rec, *m, runs); ok {
			m.Presence, m.Activity = p, nil
		}
		if p, ok := unseatedPresence(rec, *m, runs); ok {
			m.Presence = p
		}
	}
	return out
}

// unseatedPresence is the presence of a seat that has no run yet: waiting
// when something seats it (willSeat), and not-started otherwise. It
// reports false for any other seat.
func unseatedPresence(rec *record, m Member, runs map[string]*fw.Run) (string, bool) {
	if m.Fired || len(m.Runs) > 0 {
		return "", false
	}
	if willSeat(rec, runs[managerRun(rec)]) {
		return PresenceWaiting, true
	}
	return PresenceNotStarted, true
}

// willSeat reports whether something seats a seat of the bridge that has
// no run: the job of a queue bridge waits on the intake queue, or the
// manager run of a spawn bridge works. mgr is that manager run as the view
// holds it, or nil. A manager run that the view does not hold counts as
// working until the channel holds an end of it, because the view can get
// a new run late.
func willSeat(rec *record, mgr *fw.Run) bool {
	if rec.Placement == PlacementQueue {
		return true
	}
	id := managerRun(rec)
	switch {
	case id == "":
		return false
	case mgr != nil:
		return !mgr.State.Terminal()
	}
	mk := rec.Marks[id]
	return mk == nil || mk.Done == 0
}

// gonePresence is the presence of a seat whose newest run no worker of
// the fleet holds, once the channel said so (goneLines): idle when the
// channel holds an end of that run, and error otherwise, because the work
// of the run is lost. It reports false for any other seat.
func gonePresence(rec *record, m Member, runs map[string]*fw.Run) (string, bool) {
	id := m.newest()
	if m.Fired || id == "" || runs[id] != nil {
		return "", false
	}
	if sm := rec.Seats[m.Handle]; sm == nil || sm.Gone != id {
		return "", false
	}
	if mk := rec.Marks[id]; mk != nil && mk.Done > 0 {
		return PresenceIdle, true
	}
	return PresenceError, true
}

// shownKey is what a hook got of a member: its presence, its activity, and
// the facts of the seat, so a hook gets a member again only when one of
// them changed.
func shownKey(m Member) string {
	var b strings.Builder
	b.WriteString(m.Presence)
	b.WriteByte('|')
	if a := m.Activity; a != nil {
		b.WriteString(a.Tool + "|" + a.CallID + "|" + a.Phrase + "|" + a.Since.String())
	}
	b.WriteByte('|')
	b.WriteString(m.Task)
	if m.Fired {
		b.WriteString("|fired")
	}
	if m.Muted {
		b.WriteString("|muted")
	}
	b.WriteString("|" + strings.Join(m.Runs, ","))
	return b.String()
}

// needsYou counts the members whose presence is needs-you.
func needsYou(members []Member) int {
	n := 0
	for _, m := range members {
		if m.Presence == PresenceNeedsYou {
			n++
		}
	}
	return n
}
