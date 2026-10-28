package bossmode

// This file feeds the stream with the events of the other packages of
// bossmode (docs/ARCHITECTURE.md section 17.5): bridge.message and
// bridge.member from the hooks of the bridges, page from the hook of the
// wiki, face from the hook of the faces, routine from the hook of the
// routines, and knowhow from the hook of Know-how. Each event goes to the stream
// of each agent of the process that runs bossmode, through PublishTo, with
// the decision of its scope bound to that agent: the stream asks the rule
// of the routes of the package once per event and per stream, for the
// caller of the stream, and keeps no decision for another caller or
// another event. A hook runs on the replica that made the change, so the
// event also goes to the other replicas of the front, which decide with
// their own stores (relay.go). A seat is the exception: each replica gives
// the seats of its own view, so bridge.member stays on its replica.

import (
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/bridges"
	"github.com/bdubs00/featherweight-workbench/faces"
	"github.com/bdubs00/featherweight-workbench/knowhow"
	"github.com/bdubs00/featherweight-workbench/pages"
	"github.com/bdubs00/featherweight-workbench/routines"
)

// Kinds of the stream that the other packages of bossmode feed.
const (
	KindBridgeMessage = "bridge.message"
	KindBridgeMember  = "bridge.member"
	KindPage          = "page"
	KindFace          = "face"
	KindRoutine       = "routine"
	KindKnowhow       = "knowhow"
)

// RoutineEvery is the shortest time between two events routine of one
// routine. A change within it goes out once at its end.
const RoutineEvery = 2 * time.Second

// KnowhowEvery is the shortest time between two events knowhow of one
// agent. A change within it goes out once at its end.
const KnowhowEvery = 2 * time.Second

func init() {
	bridges.OnMessage(feedMessage)
	bridges.OnCatchUp(feedCatchUp)
	bridges.OnMember(feedMember)
	pages.OnChange(feedPage)
	faces.OnChange(feedFace)
	routines.OnChange(feedRoutine)
	knowhow.OnChange(feedKnowhow)
}

// bridgeMessage is the data of bridge.message. Partial is true when the
// entry with its whole decision is larger than MaxEventBytes: the event
// then carries the entry without decision.input and decision.schema, and
// the page reads the whole entry from GET /v1/bridges/{id}/messages.
type bridgeMessage struct {
	Bridge  string          `json:"bridge"`
	Message bridges.Message `json:"message"`
	Partial bool            `json:"partial,omitempty"`
}

// bridgeMember is the data of bridge.member.
type bridgeMember struct {
	Bridge string         `json:"bridge"`
	Member bridges.Member `json:"member"`
}

// pageEvent is the data of page: the page without its text, and deleted
// for a page that a removal took away.
type pageEvent struct {
	pages.PageInfo
	Deleted bool `json:"deleted,omitempty"`
}

// agents returns the agents of the process whose stream runs.
func agents() []*fw.Agent {
	var out []*fw.Agent
	hubs.Range(func(k, _ any) bool {
		out = append(out, k.(*fw.Agent))
		return true
	})
	return out
}

// feedMessage publishes a new or changed entry of a channel.
func feedMessage(bridge string, m bridges.Message) {
	for _, a := range agents() {
		if bridges.Of(a) == nil {
			continue
		}
		publishMessage(a, bridge, m)
	}
}

// feedCatchUp publishes an entry that another replica wrote, which the
// watch of the bridges of a found (bridges.OnCatchUp), to the streams of
// a alone: every replica finds the entry on its own, so the relay of the
// bus carries no second copy.
func feedCatchUp(a *fw.Agent, bridge string, m bridges.Message) {
	if Of(a) == nil {
		return
	}
	publishMessage(a, bridge, m, onlyHere())
}

// publishMessage puts an entry of a channel on the stream of a, for the
// callers who can see the bridge. An entry with a large decision goes
// without the input and the schema of the call, marked partial, and the
// page reads it whole.
func publishMessage(a *fw.Agent, bridge string, m bridges.Message, opts ...Option) {
	opts = append([]Option{ForBridge(bridge)}, opts...)
	err := PublishTo(a, KindBridgeMessage, bridge, bridgeMessage{Bridge: bridge, Message: m}, opts...)
	if errors.Is(err, ErrTooLarge) && m.Decision != nil {
		d := *m.Decision
		d.Input, d.Schema = nil, nil
		m.Decision = &d
		err = PublishTo(a, KindBridgeMessage, bridge, bridgeMessage{Bridge: bridge, Message: m, Partial: true}, opts...)
	}
	if err != nil {
		logOf(a).Warn("bossmode: an entry of a channel is not on the stream", "bridge", bridge, "seq", m.Seq, "error", err)
	}
}

// feedMember publishes a new or changed seat to the streams of this
// replica. Each replica watches the view on its own and gives the seats
// that it sees (bridges.OnMember), also the changes that another replica
// made to the record. A seat from the view of another replica can show
// another presence, and the roster of the page then flaps between two
// views.
func feedMember(bridge string, m bridges.Member) {
	for _, a := range agents() {
		if bridges.Of(a) == nil {
			continue
		}
		if err := PublishTo(a, KindBridgeMember, bridge, bridgeMember{Bridge: bridge, Member: m}, ForBridge(bridge), onlyHere()); err != nil {
			logOf(a).Warn("bossmode: a seat is not on the stream", "bridge", bridge, "member", m.Handle, "error", err)
		}
	}
}

// feedPage publishes a page that a write changed or a removal took away,
// to every caller who can read the wiki (pages.Allowed).
func feedPage(info pages.PageInfo, deleted bool) {
	for _, a := range agents() {
		if pages.Of(a) == nil {
			continue
		}
		if err := PublishTo(a, KindPage, info.Path, pageEvent{PageInfo: info, Deleted: deleted}, ForPage(info.Path)); err != nil {
			logOf(a).Warn("bossmode: a page is not on the stream", "path", info.Path, "error", err)
		}
	}
}

// feedFace publishes the face of an agent after a change, to every caller
// who can view the agent (faces.Check with fw.ActionAgentView).
func feedFace(f faces.Face) {
	for _, a := range agents() {
		if faces.Of(a) == nil {
			continue
		}
		if err := PublishTo(a, KindFace, f.Agent, f, ForAgent(f.Agent)); err != nil {
			logOf(a).Warn("bossmode: a face is not on the stream", "agent", f.Agent, "error", err)
		}
	}
}

// routineEvent is the data of routine: the routine whose record or
// evidence changed. The page reads the routine again.
type routineEvent struct {
	Kind  string `json:"kind"`
	Agent string `json:"agent"`
	Name  string `json:"name"`
}

// routineFeed coalesces the changes of the routines of a hub: last holds
// when each routine last went out, and waiting the routines whose next
// event waits for the end of RoutineEvery. now and after are the clock
// and the timer, which a test replaces.
type routineFeed struct {
	mu      sync.Mutex
	last    map[string]time.Time
	waiting map[string]bool
	now     func() time.Time
	after   func(time.Duration, func())
}

func newRoutineFeed() *routineFeed {
	return &routineFeed{last: map[string]time.Time{}, waiting: map[string]bool{}, now: time.Now,
		after: func(d time.Duration, f func()) { time.AfterFunc(d, f) }}
}

// feedRoutine publishes a change of a routine to every caller who can
// view its agent, on each hub of the process that keeps routines.
func feedRoutine(agent, name string) {
	for _, a := range agents() {
		if h := Of(a); h != nil && routines.Of(a) != nil {
			h.routines.change(agent, name, func() { h.publishRoutine(agent, name) })
		}
	}
}

// change sends a change of a routine at most once per RoutineEvery: the
// first change goes out at once, and the changes after it go out as one,
// at the end of the window.
func (f *routineFeed) change(agent, name string, send func()) {
	key := agent + "/" + name
	f.mu.Lock()
	if f.waiting[key] {
		f.mu.Unlock()
		return
	}
	now := f.now()
	wait := RoutineEvery - now.Sub(f.last[key])
	if wait <= 0 {
		f.last[key] = now
		if len(f.last) > 4096 {
			for k, at := range f.last {
				if now.Sub(at) > RoutineEvery {
					delete(f.last, k)
				}
			}
		}
		f.mu.Unlock()
		send()
		return
	}
	f.waiting[key] = true
	f.mu.Unlock()
	f.after(wait, func() {
		f.mu.Lock()
		delete(f.waiting, key)
		f.last[key] = f.now()
		f.mu.Unlock()
		send()
	})
}

// publishRoutine puts the event routine on the stream of the hub, for the
// callers who can view the agent.
func (h *Hub) publishRoutine(agent, name string) {
	if err := PublishTo(h.a, KindRoutine, agent+"/"+name, routineEvent{Kind: KindRoutine, Agent: agent, Name: name}, ForAgent(agent)); err != nil {
		h.log.Warn("bossmode: a routine is not on the stream", "agent", agent, "routine", name, "error", err)
	}
}

// knowhowEvent is the data of knowhow: the agent whose notes, journal,
// switch, or waiting list changed. The page reads them again.
type knowhowEvent struct {
	Kind  string `json:"kind"`
	Agent string `json:"agent"`
}

// feedKnowhow publishes a change of the Know-how of an agent to every
// caller who can view the agent, on each hub of the process that keeps
// Know-how, at most once per agent in KnowhowEvery.
func feedKnowhow(agent string) {
	for _, a := range agents() {
		if h := Of(a); h != nil && knowhow.Of(a) != nil {
			h.knowhow.change(agent, "", func() { h.publishKnowhow(agent) })
		}
	}
}

// publishKnowhow puts the event knowhow on the stream of the hub, for the
// callers who can view the agent.
func (h *Hub) publishKnowhow(agent string) {
	if err := PublishTo(h.a, KindKnowhow, agent, knowhowEvent{Kind: KindKnowhow, Agent: agent}, ForAgent(agent)); err != nil {
		h.log.Warn("bossmode: a change of Know-how is not on the stream", "agent", agent, "error", err)
	}
}

// logOf is the logger of the hub of an agent.
func logOf(a *fw.Agent) *slog.Logger {
	if h := Of(a); h != nil {
		return h.log
	}
	return a.Logger()
}
