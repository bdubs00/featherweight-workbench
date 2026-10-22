package bossmode

// This file feeds the stream with the events of the other packages of
// bossmode (docs/ARCHITECTURE.md section 17.5): bridge.message and
// bridge.member from the hooks of the bridges, page from the hook of the
// wiki, and face from the hook of the faces. Each event goes to the stream
// of each agent of the process that runs bossmode, through PublishTo, with
// an Allow bound to that agent: the stream asks the rule of the routes of
// the package once per event and per stream, for the caller of the
// stream, and keeps no decision for another caller or another event.

import (
	"context"
	"errors"
	"log/slog"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/bridges"
	"github.com/bdubs00/featherweight-workbench/faces"
	"github.com/bdubs00/featherweight-workbench/pages"
)

// Kinds of the stream that the other packages of bossmode feed.
const (
	KindBridgeMessage = "bridge.message"
	KindBridgeMember  = "bridge.member"
	KindPage          = "page"
	KindFace          = "face"
)

func init() {
	bridges.OnMessage(feedMessage)
	bridges.OnMember(feedMember)
	pages.OnChange(feedPage)
	faces.OnChange(feedFace)
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

// seeBridge decides an event of the bridge id for the caller in ctx, as
// GET /v1/bridges/{id} decides (bridges.Store.Visible), with the bridges
// that the agent has at the time of the decision. A store that does not
// answer hides the event.
func seeBridge(a *fw.Agent, id string) func(context.Context) bool {
	return func(ctx context.Context) bool {
		s := bridges.Of(a)
		if s == nil {
			return false
		}
		ok, err := s.Visible(ctx, id)
		return err == nil && ok
	}
}

// feedMessage publishes a new or changed entry of a channel.
func feedMessage(bridge string, m bridges.Message) {
	for _, a := range agents() {
		if bridges.Of(a) == nil {
			continue
		}
		err := PublishTo(a, KindBridgeMessage, bridge, bridgeMessage{Bridge: bridge, Message: m}, Allow(seeBridge(a, bridge)))
		if errors.Is(err, ErrTooLarge) && m.Decision != nil {
			d := *m.Decision
			d.Input, d.Schema = nil, nil
			m.Decision = &d
			err = PublishTo(a, KindBridgeMessage, bridge, bridgeMessage{Bridge: bridge, Message: m, Partial: true}, Allow(seeBridge(a, bridge)))
		}
		if err != nil {
			logOf(a).Warn("bossmode: an entry of a channel is not on the stream", "bridge", bridge, "seq", m.Seq, "error", err)
		}
	}
}

// feedMember publishes a new or changed seat.
func feedMember(bridge string, m bridges.Member) {
	for _, a := range agents() {
		if bridges.Of(a) == nil {
			continue
		}
		if err := PublishTo(a, KindBridgeMember, bridge, bridgeMember{Bridge: bridge, Member: m}, Allow(seeBridge(a, bridge))); err != nil {
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
		allow := func(ctx context.Context) bool { return pages.Allowed(ctx, a, false) == nil }
		if err := PublishTo(a, KindPage, info.Path, pageEvent{PageInfo: info, Deleted: deleted}, Allow(allow)); err != nil {
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
		agent := f.Agent
		allow := func(ctx context.Context) bool { return faces.Check(ctx, a, agent, fw.ActionAgentView) == nil }
		if err := PublishTo(a, KindFace, agent, f, Allow(allow)); err != nil {
			logOf(a).Warn("bossmode: a face is not on the stream", "agent", agent, "error", err)
		}
	}
}

// logOf is the logger of the hub of an agent.
func logOf(a *fw.Agent) *slog.Logger {
	if h := Of(a); h != nil {
		return h.log
	}
	return a.Logger()
}
