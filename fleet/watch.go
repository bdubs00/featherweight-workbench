package fleet

// This file lets another package follow the view as it changes
// (docs/ARCHITECTURE.md section 4.7): Watch sends one change per run
// update, per change of the state of a member, and per change of the phase
// of a Task, and Visible decides whether a caller can see one change. It
// also words what a run of a worker does now (ActivityPhrase) from the
// input of the tool call that the stream carried.

import (
	"bytes"
	"container/list"
	"context"
	"encoding/json"
	"maps"
	"sync"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/surface/view"
)

// Kinds of a Change.
const (
	ChangeRun    = "run"    // a run of a member changed: Run holds the run
	ChangeMember = "member" // the state of a member changed: State holds it
	ChangeTask   = "task"   // the phase of the Task of a member changed: Phase holds it
)

// WatchBuffer is the number of changes that one watcher holds for a
// consumer that has not read them yet. A change for a key that the
// buffer holds replaces it, and a full buffer drops its oldest change.
const WatchBuffer = 1024

// ActivityInputLimit is the largest tool input in bytes that the view
// keeps for the activity of a run (ActivityPhrase). A larger input keeps
// a shorter copy of each of its strings, and an input that does not fit
// even so is left out, so the phrase then names the tool alone.
const ActivityInputLimit = 4 << 10

// Change is one change of the view, for Watch.
//
// Kind is ChangeRun, ChangeMember, or ChangeTask, and Member names the
// member. For ChangeRun, Run is a copy of the run as GET /v1/fleet/runs
// lists it, with Member set and its texts cut to TextLimit, and Activity
// words what the run does now (ActivityPhrase), empty for a run that does
// not work. For ChangeMember, State is the state of the member in the
// view: MemberLive, MemberQuiet, or MemberStopped. For ChangeTask, Phase
// is the phase of the Task as the ax API gives it, such as Running or
// Suspended. Gone is true when the member left the view; State and Phase
// are then empty. Time is when the view saw the change.
//
// Every watcher receives the same value, so a receiver must not change
// Run.
type Change struct {
	Kind     string    `json:"kind"`
	Member   string    `json:"member"`
	Run      *fw.Run   `json:"run,omitempty"`
	Activity string    `json:"activity,omitempty"`
	State    string    `json:"state,omitempty"`
	Phase    string    `json:"phase,omitempty"`
	Gone     bool      `json:"gone,omitempty"`
	Time     time.Time `json:"time"`

	// served names the agents that a member served when it left the
	// view, so Visible can still decide about it.
	served []string
}

// Key returns what makes two changes about the same thing: the kind, the
// member, and the run id for ChangeRun. A watcher keeps only the newest
// change of each key.
func (c Change) Key() string {
	if c.Kind == ChangeRun && c.Run != nil {
		return c.Kind + "\x00" + c.Member + "\x00" + c.Run.ID
	}
	return c.Kind + "\x00" + c.Member
}

// Watcher is a View that sends its changes. The fleet implements it, and
// a surface reaches it through Of with a type assertion.
//
// Watch returns a channel that receives one Change per run update, per
// change of the state of a member, and per change of the phase of a Task,
// from the call on, until ctx ends or the view closes; then the channel
// closes. The changes of other members only: the runs of this process are
// not in it. A slow consumer never holds up the view. Each watcher keeps
// at most WatchBuffer changes that wait for the consumer: a newer change
// with the same Key replaces a waiting one, and when the buffer is full,
// the oldest waiting change goes. A consumer that must not miss a state
// reads the view again, for example after a reconnect.
//
// Visible reports whether the principal in ctx can see a change: a run as
// GET /v1/fleet/runs decides for that run and that caller, and a member or
// a Task as GET /v1/fleet decides for the member. It decides with the
// current rules each time and keeps nothing. A decision point that does
// not answer is an error.
type Watcher interface {
	Watch(ctx context.Context) <-chan Change
	Visible(ctx context.Context, c Change) (bool, error)
}

// The fleet sends its changes.
var _ Watcher = (*Fleet)(nil)

// watchers are the watchers of a fleet.
type watchers struct {
	mu  sync.Mutex
	set map[*watcher]struct{}
}

// watcher is one consumer of Watch: a bounded queue of changes by key.
type watcher struct {
	mu    sync.Mutex
	queue *list.List               // of Change, the oldest first
	byKey map[string]*list.Element // the element of each key in queue
	wake  chan struct{}            // capacity 1: a change waits
}

// Watch implements Watcher.
func (f *Fleet) Watch(ctx context.Context) <-chan Change {
	out := make(chan Change)
	w := &watcher{queue: list.New(), byKey: map[string]*list.Element{}, wake: make(chan struct{}, 1)}
	wctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(f.ctx, cancel)
	f.watch.mu.Lock()
	if f.watch.set == nil {
		f.watch.set = map[*watcher]struct{}{}
	}
	f.watch.set[w] = struct{}{}
	f.watch.mu.Unlock()
	go func() {
		defer func() {
			stop()
			cancel()
			f.watch.mu.Lock()
			delete(f.watch.set, w)
			f.watch.mu.Unlock()
			close(out)
		}()
		for {
			c, ok := w.pop()
			if !ok {
				select {
				case <-wctx.Done():
					return
				case <-w.wake:
					continue
				}
			}
			select {
			case out <- c:
			case <-wctx.Done():
				return
			}
		}
	}()
	return out
}

// watched reports whether a watcher listens, so the view builds no change
// that nobody reads.
func (f *Fleet) watched() bool {
	f.watch.mu.Lock()
	defer f.watch.mu.Unlock()
	return len(f.watch.set) > 0
}

// emit hands a change to every watcher. It never blocks: a watcher queues
// the change (push). The platform calls it under its lock.
func (f *Fleet) emit(c Change) {
	f.watch.mu.Lock()
	defer f.watch.mu.Unlock()
	for w := range f.watch.set {
		w.push(c)
	}
}

// push queues a change. A change of a key that waits replaces it in its
// place; a full queue drops its oldest change first.
func (w *watcher) push(c Change) {
	key := c.Key()
	w.mu.Lock()
	if el, ok := w.byKey[key]; ok {
		el.Value = c
	} else {
		if w.queue.Len() >= WatchBuffer {
			old := w.queue.Front()
			delete(w.byKey, old.Value.(Change).Key())
			w.queue.Remove(old)
		}
		w.byKey[key] = w.queue.PushBack(c)
	}
	w.mu.Unlock()
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

// pop takes the oldest change that waits.
func (w *watcher) pop() (Change, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	el := w.queue.Front()
	if el == nil {
		return Change{}, false
	}
	c := w.queue.Remove(el).(Change)
	delete(w.byKey, c.Key())
	return c, true
}

// Visible implements Watcher.
func (f *Fleet) Visible(ctx context.Context, c Change) (bool, error) {
	if f.platform == nil || c.Member == "" {
		return false, nil
	}
	if c.Kind == ChangeRun {
		if c.Run == nil {
			return false, nil
		}
		rec := f.platform.lineageRecord(c.Member, c.Run)
		ok, _, err := f.allow(ctx, rec, []*fw.Run{c.Run}, false)
		if err != nil {
			return false, err
		}
		return ok[0], nil
	}
	rec := f.platform.memberRecord(c.Member, time.Now())
	if rec == nil {
		// The member left the view: the agents that it served decide.
		rec = f.servedRecord(c.Member, c.served)
	}
	runOK, roles, err := f.allow(ctx, rec, rec.Runs, true)
	if err != nil {
		return false, err
	}
	if fw.PrincipalFrom(ctx) == nil {
		return true, nil
	}
	for _, ok := range runOK {
		if ok {
			return true, nil
		}
	}
	for _, role := range roles {
		if role != "" {
			return true, nil
		}
	}
	return false, nil
}

// servedRecord is a record without runs for a member that left the view,
// with the agents that it served and the access of this process for each.
func (f *Fleet) servedRecord(member string, agents []string) *Record {
	cfg := f.a.Config()
	rec := &Record{Name: member, Access: Access{Mode: f.a.AccessMode(), Default: cfg.Access.Default, Admins: cfg.Access.Admins}}
	for _, name := range agents {
		ag := RecordAgent{Name: name}
		if prof, ok := f.a.ProfileNamed(name); ok {
			ag.Description, ag.Access = prof.Description, prof.Access
		}
		rec.Agents = append(rec.Agents, ag)
	}
	return rec
}

// lineageRecord is the part of the record of a member that the decision
// about one run reads: the run, its ancestors on the member as the view
// holds them, and the agents of those runs. The rules see a run when they
// see an ancestor of it, and a control plane gets the lineage, so the
// decision is the one of the whole record.
func (p *platform) lineageRecord(member string, run *fw.Run) *Record {
	cfg := p.f.a.Config()
	rec := &Record{Name: member, Access: Access{Mode: p.f.a.AccessMode(), Default: cfg.Access.Default, Admins: cfg.Access.Admins}}
	chain := []*fw.Run{run}
	p.mu.Lock()
	cur := run
	for depth := 0; cur.Parent != "" && depth < maxLineage; depth++ {
		r := p.runs[cur.Parent]
		if r == nil || r.member != member || !r.queued {
			break
		}
		cur = forgetful(p.listed(cur.Parent, r))
		chain = append(chain, cur)
	}
	p.mu.Unlock()
	rec.Runs = chain
	seen := map[string]bool{}
	for _, r := range chain {
		if r.Agent == "" || seen[r.Agent] {
			continue
		}
		seen[r.Agent] = true
		ag := RecordAgent{Name: r.Agent}
		if prof, ok := p.f.a.ProfileNamed(r.Agent); ok {
			ag.Description, ag.Access = prof.Description, prof.Access
		}
		rec.Agents = append(rec.Agents, ag)
	}
	return rec
}

// noteMember sends a member change when the state that the view shows for
// the member changed since the last one. The caller holds p.mu.
func (p *platform) noteMember(m *platformMember, now time.Time) {
	state := p.shownState(m, now)
	if p.shown[m.name] == state {
		return
	}
	p.shown[m.name] = state
	if p.f.watched() {
		p.f.emit(Change{Kind: ChangeMember, Member: m.name, State: state, Time: now})
	}
}

// shownState is the state of a member as the view shows it (Fleet.state):
// the phase of its Task, else live within QuietAfter intervals of its last
// event, else quiet. The caller holds p.mu.
func (p *platform) shownState(m *platformMember, now time.Time) string {
	if m.state != "" {
		return m.state
	}
	if now.Sub(m.seen) <= p.quiet {
		return MemberLive
	}
	return MemberQuiet
}

// noteGone sends the changes of a member that left the view. The caller
// holds p.mu.
func (p *platform) noteGone(m *platformMember, now time.Time) {
	delete(p.shown, m.name)
	if !p.f.watched() {
		return
	}
	served := make([]string, 0, len(m.agents))
	for name := range m.agents {
		served = append(served, name)
	}
	p.f.emit(Change{Kind: ChangeMember, Member: m.name, Gone: true, Time: now, served: served})
	if m.atespace != "" {
		p.f.emit(Change{Kind: ChangeTask, Member: m.name, Gone: true, Time: now, served: served})
	}
}

// noteTask sends a Task change for a new phase. The caller holds p.mu.
func (p *platform) noteTask(m *platformMember, now time.Time) {
	if p.f.watched() {
		p.f.emit(Change{Kind: ChangeTask, Member: m.name, Phase: m.phase, Time: now})
	}
}

// noteRun sends the change of a run that the view updated. A run without
// identity is not listed, so it sends nothing until it has one. The
// caller holds p.mu.
func (p *platform) noteRun(id string, r *platformRun, now time.Time) {
	if !r.queued || r.run.ID == "" || !p.f.watched() {
		return
	}
	c := detach(shorten(p.listed(id, r)))
	c.Member = r.member
	p.f.emit(Change{Kind: ChangeRun, Member: r.member, Run: c, Activity: view.ActivityPhrase(c, r.inputOf(c)), Time: now})
}

// watchLoop sends the member changes that come with time alone: a member
// that only the stream knows turns quiet when no event arrived for
// QuietAfter intervals.
func (p *platform) watchLoop(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			now := time.Now()
			p.mu.Lock()
			for _, m := range p.members {
				if m.state == "" {
					p.noteMember(m, now)
				}
			}
			p.mu.Unlock()
		}
	}
}

// detach gives a copy of a run its own maps and values, so the view can go
// on changing the run while a watcher reads the copy.
func detach(c *fw.Run) *fw.Run {
	c.Metadata = maps.Clone(c.Metadata)
	c.Reactions = maps.Clone(c.Reactions)
	if c.TreeUsage != nil {
		u := *c.TreeUsage
		c.TreeUsage = &u
	}
	if c.Outcome != nil {
		o := *c.Outcome
		c.Outcome = &o
	}
	if c.Activity != nil {
		a := *c.Activity
		c.Activity = &a
	}
	return c
}

// noteToolInput keeps the input of the latest tool call of a run for its
// activity, cut to ActivityInputLimit (boundInput). fw.tool.responded of
// that call, the end of the turn, and the end of the run forget it; an
// approval keeps it, because the call goes on after the decision.
func (r *platformRun) noteToolInput(e fw.Event) {
	switch e.Type {
	case fw.EvToolRequested:
		var d fw.ToolRequestedData
		if e.Decode(&d) != nil {
			return
		}
		r.callID, r.input = d.CallID, boundInput(d.Input, ActivityInputLimit)
	case fw.EvToolResponded:
		var d struct {
			CallID string `json:"callId"`
		}
		if e.Decode(&d) == nil && d.CallID == r.callID {
			r.callID, r.input = "", nil
		}
	case fw.EvTurnFinished, fw.EvRunFinished:
		r.callID, r.input = "", nil
	}
}

// inputOf returns the input that the view keeps for the activity of run,
// or nil when it keeps none for that call.
func (r *platformRun) inputOf(run *fw.Run) json.RawMessage {
	if run.Activity == nil || run.Activity.CallID == "" || run.Activity.CallID != r.callID {
		return nil
	}
	return r.input
}

// toolInput returns the input that the view keeps for a call of a run.
func (f *Fleet) toolInput(id, callID string) (json.RawMessage, bool) {
	p := f.platform
	if p == nil {
		return nil, false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	r := p.runs[id]
	if r == nil || r.callID == "" || r.callID != callID {
		return nil, false
	}
	return r.input, true
}

// ActivityPhrase words what a run of the fleet does now, such as
// `Ran "go test ./..."` or "Thinking in turn 7", with view.ActivityPhrase.
// The input of the call comes from the fleet view that holds the run: the
// stream carries the inputs of the calls of the workers, cut to size, and
// the view keeps the latest one of each run up to ActivityInputLimit. For a
// run that no view holds, such as a run of this process, and for an input
// that the view left out, the phrase names the tool. It returns "" for a
// run without an activity.
func ActivityPhrase(r *fw.Run) string {
	if r == nil || r.Activity == nil {
		return ""
	}
	var input json.RawMessage
	if r.Activity.CallID != "" {
		views.Range(func(_, v any) bool {
			f, ok := v.(*Fleet)
			if !ok {
				return true
			}
			in, found := f.toolInput(r.ID, r.Activity.CallID)
			if found {
				input = in
			}
			return !found
		})
	}
	return view.ActivityPhrase(r, input)
}

// boundInput returns a JSON input of at most limit bytes. A larger input
// keeps its shape with each string cut shorter, halving the length of the
// strings until the input fits, and an input that does not fit with
// strings of 16 bytes, or that does not decode, gives nil.
func boundInput(raw json.RawMessage, limit int) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	if len(raw) <= limit {
		return raw
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if dec.Decode(&v) != nil {
		return nil
	}
	for n := limit / 2; n >= 16; n /= 2 {
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(false)
		if enc.Encode(cutValues(v, n)) != nil {
			return nil
		}
		if out := bytes.TrimRight(buf.Bytes(), "\n"); len(out) <= limit {
			return out
		}
	}
	return nil
}

// cutValues returns a copy of a decoded JSON value with each string cut
// to n bytes at a rune boundary (cut), so the input stays valid UTF-8.
func cutValues(v any, n int) any {
	switch x := v.(type) {
	case string:
		return cut(x, n)
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = cutValues(e, n)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = cutValues(e, n)
		}
		return out
	}
	return v
}
