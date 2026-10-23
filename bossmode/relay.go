package bossmode

// This file carries the events of Publish to the other replicas of the
// front (docs/ARCHITECTURE.md section 17.5). A hook runs on the replica
// that made a change, so each hub also publishes its events on the bus,
// on the subject <workbench.fleet.egress>.bossmode.<kind>, else
// fw.bossmode.<kind>, and puts the events of the other replicas into its
// own ring. Each event names the replica of its hub, so a hub skips its
// own events. A replica decides a relayed event with its own stores,
// which the replicas share, as it decides a local event: the event
// carries the bridge, the path of the page, or the agent that the
// decision needs.

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/bridges"
	"github.com/bdubs00/featherweight-workbench/faces"
	"github.com/bdubs00/featherweight-workbench/fleet"
	"github.com/bdubs00/featherweight-workbench/pages"
)

const (
	// EvRelay is the type of the bus event that carries an event of the
	// stream to the other replicas of the front.
	EvRelay = "fw.workbench.bossmode.relayed"
	// relayQueue bounds the events that wait for the bus. A hook must not
	// block, so a hub drops an event past the bound, and the log says so.
	relayQueue = 1024
	// relayTimeout bounds one publish on the bus.
	relayTimeout = 5 * time.Second
	// relayEnvelope is the room of the fields of a relayed event beside
	// its data: the replica, the kind, the id, and the scope.
	relayEnvelope = 4 << 10
)

// scope is a decision of the stream that every replica makes the same way,
// because it reads the stores that the replicas share: the callers who can
// see the bridge Bridge, the callers who read the wiki for the page Page,
// or the callers who can view the agent Agent.
type scope struct {
	Bridge string `json:"bridge,omitempty"`
	Page   string `json:"page,omitempty"`
	Agent  string `json:"agent,omitempty"`
}

// relayed is the data of an EvRelay event: the replica of the hub that
// published it, the kind, the id, and the data of the event, and its
// scope, or none for an event that every caller gets.
type relayed struct {
	Replica string          `json:"replica"`
	Kind    string          `json:"kind"`
	ID      string          `json:"id,omitempty"`
	Scope   *scope          `json:"scope,omitempty"`
	Data    json.RawMessage `json:"data"`
}

// outgoing is one event that waits for the bus.
type outgoing struct {
	subject string
	e       fw.Event
}

// RelayPrefix returns the subject prefix of the events that the replicas
// of the front relay: <workbench.fleet.egress>.bossmode when the
// configuration names the egress of the fleet, else fw.bossmode.
func RelayPrefix(cfg *fw.Config) string {
	if c, err := fleet.Load(cfg); err == nil && c.Egress != "" {
		return c.Egress + ".bossmode"
	}
	return "fw.bossmode"
}

// relaySubject is the subject of an event of the kind. A kind can hold
// dots, which part a subject into tokens, so the subject leaves out an
// empty token; the event itself names its kind.
func relaySubject(prefix, kind string) string {
	var parts []string
	for _, p := range strings.Split(kind, ".") {
		if p != "" {
			parts = append(parts, p)
		}
	}
	if len(parts) == 0 {
		parts = []string{"event"}
	}
	return prefix + "." + strings.Join(parts, ".")
}

// newReplica returns a random id for the hub of this process.
func newReplica() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// allowOf is the decision of a scope for the streams of the agent a, the
// decision of the hooks of feeds.go: a bridge as GET /v1/bridges/{id}
// decides, a page for a caller who reads the wiki, and an agent for a
// caller who can view it. An empty scope allows nobody.
func allowOf(a *fw.Agent, sc scope) func(context.Context) bool {
	switch {
	case sc.Bridge != "":
		return seeBridge(a, sc.Bridge)
	case sc.Page != "":
		return func(ctx context.Context) bool { return pages.Of(a) != nil && pages.Allowed(ctx, a, false) == nil }
	case sc.Agent != "":
		agent := sc.Agent
		return func(ctx context.Context) bool { return faces.Check(ctx, a, agent, fw.ActionAgentView) == nil }
	}
	return func(context.Context) bool { return false }
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

// scoped reports whether the kind is a kind of the hooks of feeds.go,
// whose events always carry a scope.
func scoped(kind string) bool {
	switch kind {
	case KindBridgeMessage, KindBridgeMember, KindPage, KindFace, KindRoutine:
		return true
	}
	return false
}

// listen subscribes the hub to the events of the other replicas. Without
// a bus, the hub serves the events of this replica alone.
func (h *Hub) listen() {
	bus := h.a.Bus()
	if bus == nil {
		return
	}
	stop, err := bus.Subscribe(h.ctx, h.prefix+".>", h.onRelay)
	if err != nil {
		h.log.Warn("bossmode: the stream gets no event of another replica, because the subscription failed", "subject", h.prefix+".>", "error", err)
		return
	}
	h.stopRelay = stop
}

// relay puts an event of this replica on the bus for the other replicas,
// without waiting: the sender publishes it.
func (h *Hub) relay(kind, id string, data []byte, sc *scope) {
	if h.a.Bus() == nil {
		return
	}
	e, err := fw.NewEvent("", EvRelay, relayed{Replica: h.replica, Kind: kind, ID: id, Scope: sc, Data: data})
	if err != nil {
		h.log.Warn("bossmode: encode an event for the other replicas", "kind", kind, "error", err)
		return
	}
	select {
	case h.out <- outgoing{subject: relaySubject(h.prefix, kind), e: e}:
	default:
		h.mu.Lock()
		first := h.relayDropped == 0
		h.relayDropped++
		h.mu.Unlock()
		if first {
			h.log.Warn("bossmode: the bus is slow, so the other replicas miss events of this replica", "kind", kind, "waiting", relayQueue)
		}
	}
}

// send publishes the events of relay until the hub closes. The log names
// the first failure and the first publish that works again.
func (h *Hub) send() {
	defer close(h.sent)
	failing := false
	for {
		select {
		case <-h.ctx.Done():
			return
		case o := <-h.out:
			ctx, cancel := context.WithTimeout(h.ctx, relayTimeout)
			err := h.a.Bus().Publish(ctx, o.subject, o.e)
			cancel()
			switch {
			case err != nil && !failing:
				failing = true
				h.log.Warn("bossmode: an event did not reach the other replicas", "subject", o.subject, "error", err)
			case err == nil && failing:
				failing = false
				h.log.Info("bossmode: the events reach the other replicas again", "subject", o.subject)
			}
			if err == nil && len(h.out) == 0 {
				// The queue is empty again, so the next drop logs again.
				h.mu.Lock()
				h.relayDropped = 0
				h.mu.Unlock()
			}
		}
	}
}

// onRelay puts an event of another replica into the ring, with the
// decision of its scope for the streams of this replica. It drops an event
// of this replica, an event that does not decode, an event of a kind that
// Publish refuses, an event larger than MaxEventBytes, and an event of a
// hook without its scope.
func (h *Hub) onRelay(e fw.Event) error {
	if e.Type != EvRelay || len(e.Data) > MaxEventBytes+relayEnvelope || h.ctx.Err() != nil {
		return nil
	}
	var r relayed
	if err := e.Decode(&r); err != nil || r.Replica == "" || r.Replica == h.replica {
		return nil
	}
	if !validKind(r.Kind) || len(r.Data) == 0 || len(r.Data) > MaxEventBytes {
		return nil
	}
	// The stream sends the data on one line.
	var data bytes.Buffer
	if err := json.Compact(&data, r.Data); err != nil {
		return nil
	}
	var allow func(context.Context) bool
	switch {
	case r.Scope != nil:
		allow = allowOf(h.a, *r.Scope)
	case scoped(r.Kind):
		return nil
	}
	h.add(r.Kind, r.ID, data.Bytes(), nil, allow)
	return nil
}
