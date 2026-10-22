package bossmode

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/fleet"
	"github.com/bdubs00/featherweight-workbench/pages"
)

// replicaPair is two fronts on one mem bus, as two replicas of a front
// behind one address, whose profile manager only ada operates, so ada
// reads the wiki and carol does not. The second one runs the wiki.
func replicaPair(t *testing.T) (*front, *front) {
	t.Helper()
	return replicaPairOn(t, "")
}

// replicaPairOn is replicaPair with the egress of the fleet in the
// configuration, so the relay shares the prefix of the egress stream.
func replicaPairOn(t *testing.T, egress string) (*front, *front) {
	t.Helper()
	t.Setenv(fleet.EnvEgress, egress)
	bus := fw.NewMemBus()
	onlyAda := func(c *fw.Config) {
		c.Agents["manager"] = fw.AgentProfile{Access: &fw.Access{Operators: []string{"ada"}}}
	}
	one, two := newFrontOn(t, bus, onlyAda), newFrontOn(t, bus, onlyAda)
	cl, err := pages.Start(context.Background(), two.a)
	if err != nil || cl == nil {
		t.Fatalf("start the wiki: %v, %v", cl, err)
	}
	t.Cleanup(func() { _ = cl.Close() })
	return one, two
}

// ringOf returns the events that the ring of the hub holds, oldest first.
func ringOf(h *Hub) []entry {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []entry
	for id := h.last - uint64(h.n) + 1; id <= h.last && h.n > 0; id++ {
		out = append(out, h.ring[id%RingSize])
	}
	return out
}

// ofKind returns the events of the ring with the kind.
func ofKind(h *Hub, kind string) []entry {
	var out []entry
	for _, e := range ringOf(h) {
		if e.kind == kind {
			out = append(out, e)
		}
	}
	return out
}

// nextOf returns the next event of the kind on a stream.
func nextOf(t *testing.T, s *sse, kind string) frame {
	t.Helper()
	for {
		if f := s.next(t); f.kind == kind {
			return f
		}
	}
}

// waitFor waits until the ring of the hub holds n events of the kind.
func waitFor(t *testing.T, h *Hub, kind string, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for len(ofKind(h, kind)) < n {
		if time.Now().After(deadline) {
			t.Fatalf("the ring holds %d events %s, want %d", len(ofKind(h, kind)), kind, n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func as(subject string) context.Context {
	return fw.WithPrincipal(context.Background(), &fw.Principal{Subject: subject, Kind: fw.KindUser})
}

// An event of one replica reaches the stream of the other through the bus,
// on the subject of the relay, once, and the replica that published it
// keeps its own copy alone. A page event reaches the callers that the
// other replica allows with its own wiki: ada, who reads the wiki, and not
// carol. An event whose decision is a function of its replica, and a seat,
// stay on the replica that published them.
func TestRelayReachesTheOtherReplica(t *testing.T) {
	one, two := replicaPair(t)
	if one.h.replica == two.h.replica || one.h.prefix != "fw.bossmode" {
		t.Fatalf("the replicas are %q and %q on %q", one.h.replica, two.h.replica, one.h.prefix)
	}
	seen := make(chan string, 16)
	stop, err := one.bus.Subscribe(context.Background(), "fw.bossmode.>", func(e fw.Event) error {
		var r relayed
		_ = e.Decode(&r)
		seen <- e.Type + " " + r.Kind
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stop)
	ada := openStream(t, two, "ada", "")

	if err := PublishTo(one.a, "test.note", "n1", map[string]int{"n": 1}); err != nil {
		t.Fatal(err)
	}
	if f := nextOf(t, ada, "test.note"); f.data != `{"n":1}` {
		t.Fatalf("the note on the other replica = %+v", f)
	}
	select {
	case got := <-seen:
		if got != EvRelay+" test.note" {
			t.Fatalf("the bus carried %q", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no event on the subject of the relay")
	}

	page := pageEvent{PageInfo: pages.PageInfo{Path: "notes/plan.md", Title: "Plan", By: "ada"}}
	if err := PublishTo(one.a, KindPage, page.Path, page, ForPage(page.Path)); err != nil {
		t.Fatal(err)
	}
	if f := nextOf(t, ada, KindPage); !strings.Contains(f.data, `"path":"notes/plan.md"`) {
		t.Fatalf("the page on the other replica = %+v", f)
	}
	got := ofKind(two.h, KindPage)
	if len(got) != 1 {
		t.Fatalf("the ring of the other replica holds %d pages", len(got))
	}
	if ok, err := two.h.visible(as("carol"), got[0]); err != nil || ok {
		t.Fatalf("carol sees the page on the other replica: %v, %v", ok, err)
	}
	if ok, err := two.h.visible(as("ada"), got[0]); err != nil || !ok {
		t.Fatalf("ada sees the page on the other replica: %v, %v", ok, err)
	}

	if err := PublishTo(one.a, "test.local", "l1", map[string]int{"n": 2}, Allow(func(context.Context) bool { return true })); err != nil {
		t.Fatal(err)
	}
	if err := PublishTo(one.a, KindBridgeMember, "b1", map[string]string{"bridge": "b1"}, ForBridge("b1"), onlyHere()); err != nil {
		t.Fatal(err)
	}
	if err := PublishTo(one.a, "test.after", "a1", map[string]int{"n": 3}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, two.h, "test.after", 1)
	time.Sleep(50 * time.Millisecond)
	if n, m := len(ofKind(two.h, "test.local")), len(ofKind(two.h, KindBridgeMember)); n != 0 || m != 0 {
		t.Fatalf("the other replica got %d events with a function and %d seats", n, m)
	}
	for _, kind := range []string{"test.note", KindPage, "test.local", KindBridgeMember, "test.after"} {
		if n := len(ofKind(one.h, kind)); n != 1 {
			t.Fatalf("the replica that published holds %d events %s, want its own one", n, kind)
		}
	}
}

// On the prefix of the egress stream, the view of the fleet, which reads
// that stream, leaves the events of the relay alone: they name no node and
// no run.
func TestRelayOnTheEgressLeavesTheViewAlone(t *testing.T) {
	one, two := replicaPairOn(t, "fw.events")
	if two.h.prefix != "fw.events.bossmode" {
		t.Fatalf("the prefix = %q", two.h.prefix)
	}
	if err := PublishTo(one.a, "test.note", "n1", map[string]int{"n": 1}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, two.h, "test.note", 1)
	time.Sleep(50 * time.Millisecond)
	for _, fr := range []*front{one, two} {
		members, err := fr.f.Members(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range members {
			if !m.Self {
				t.Fatalf("the view holds the member %+v", m)
			}
		}
		if runs, err := fr.f.Runs(context.Background()); err != nil || len(runs) != 0 {
			t.Fatalf("the view holds the runs %+v, %v", runs, err)
		}
	}
}

// A hub skips an event that names its own replica, and drops an event
// that it cannot trust: a kind that Publish refuses, data above
// MaxEventBytes, and an event of a hook without its scope. It puts the
// data on one line.
func TestRelaySkipsWhatItCannotTrust(t *testing.T) {
	fr := newFront(t, nil)
	h := fr.h
	relay := func(r relayed) {
		t.Helper()
		e, err := fw.NewEvent("", EvRelay, r)
		if err != nil {
			t.Fatal(err)
		}
		if err := h.onRelay(e); err != nil {
			t.Fatal(err)
		}
	}
	relay(relayed{Replica: h.replica, Kind: "test.own", Data: json.RawMessage(`{}`)})
	relay(relayed{Replica: "other", Kind: KindRun, Data: json.RawMessage(`{}`)})
	relay(relayed{Replica: "other", Kind: "test.big", Data: json.RawMessage(`"` + strings.Repeat("x", MaxEventBytes) + `"`)})
	relay(relayed{Replica: "other", Kind: KindBridgeMessage, ID: "b1", Data: json.RawMessage(`{"bridge":"b1"}`)})
	relay(relayed{Replica: "other", Kind: "test.lines", Data: json.RawMessage("{\n\"a\": 1\n}")})
	relay(relayed{Replica: "other", Kind: KindBridgeMessage, ID: "b2", Scope: &scope{Bridge: "b2"}, Data: json.RawMessage(`{"bridge":"b2"}`)})
	var kinds []string
	for _, e := range ringOf(h) {
		if strings.HasPrefix(e.kind, "test.") || e.kind == KindRun || e.kind == KindBridgeMessage {
			kinds = append(kinds, e.kind+" "+string(e.data))
		}
	}
	if want := []string{`test.lines {"a":1}`, `bridge.message {"bridge":"b2"}`}; strings.Join(kinds, "|") != strings.Join(want, "|") {
		t.Fatalf("the ring holds %q, want %q", kinds, want)
	}
	// This front runs no bridges, so nobody sees the bridge b2.
	if ok, err := h.visible(as("ada"), ofKind(h, KindBridgeMessage)[0]); err != nil || ok {
		t.Fatalf("a bridge of no store is visible: %v, %v", ok, err)
	}
}

// The subject of the relay follows the egress of the fleet, and a kind
// with an empty token still makes a subject.
func TestRelaySubject(t *testing.T) {
	t.Setenv(fleet.EnvEgress, "fw.events")
	if got := RelayPrefix(fw.DefaultConfig()); got != "fw.events.bossmode" {
		t.Fatalf("RelayPrefix = %q", got)
	}
	t.Setenv(fleet.EnvEgress, "")
	if got := RelayPrefix(fw.DefaultConfig()); got != "fw.bossmode" {
		t.Fatalf("RelayPrefix without an egress = %q", got)
	}
	for kind, want := range map[string]string{"bridge.message": "fw.bossmode.bridge.message", "a..b.": "fw.bossmode.a.b", ".": "fw.bossmode.event"} {
		if got := relaySubject("fw.bossmode", kind); got != want {
			t.Errorf("relaySubject(%q) = %q, want %q", kind, got, want)
		}
	}
}
