package bossmode

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
)

func queued(principal, agent string) fw.RunQueuedData {
	return fw.RunQueuedData{Agent: agent, Input: "do it", Principal: principal}
}

// mark publishes an event that every caller gets, so a test knows that a
// stream has read everything before it.
func mark(t *testing.T, name string) {
	t.Helper()
	if err := Publish("test.mark", name, map[string]string{"mark": name}); err != nil {
		t.Fatal(err)
	}
}

// A person gets the runs that the person can see and no run of another
// person, as GET /v1/fleet/runs decides; a member goes to a caller who
// sees it.
func TestStreamShowsWhatTheCallerSees(t *testing.T) {
	fr := newFront(t, nil)
	ada := openStream(t, fr, "ada", "")
	bob := openStream(t, fr, "bob", "")
	carol := openStream(t, fr, "carol", "")
	fr.event(t, "ax:team/alpha", "01ADA", 1, fw.EvRunQueued, queued("ada", "default"))
	fr.event(t, "ax:team/beta", "01BOB", 1, fw.EvRunQueued, queued("bob", "default"))
	mark(t, "one")

	runsOf := func(s *sse) (runs []string, members int) {
		for _, f := range s.until(t, "test.mark") {
			switch f.kind {
			case KindRun:
				var r struct {
					ID     string          `json:"id"`
					Member string          `json:"member"`
					Act    json.RawMessage `json:"activity"`
				}
				if err := json.Unmarshal([]byte(f.data), &r); err != nil {
					t.Fatalf("run data %s: %v", f.data, err)
				}
				if !strings.HasPrefix(r.Member, "team/") || string(r.Act) != "null" {
					t.Errorf("run data = %s, want the member and activity null", f.data)
				}
				runs = append(runs, r.ID)
			case KindMember:
				members++
			}
		}
		return runs, members
	}
	if runs, members := runsOf(ada); strings.Join(runs, ",") != "01ADA" || members != 1 {
		t.Errorf("ada got the runs %v and %d member events, want 01ADA and 1", runs, members)
	}
	if runs, members := runsOf(bob); strings.Join(runs, ",") != "01BOB" || members != 1 {
		t.Errorf("bob got the runs %v and %d member events, want 01BOB and 1", runs, members)
	}
	if runs, members := runsOf(carol); len(runs) != 0 || members != 0 {
		t.Errorf("carol got the runs %v and %d member events, want none", runs, members)
	}
}

// The data of a run names what it does now, with the words of the step.
func TestStreamRunActivity(t *testing.T) {
	fr := newFront(t, nil)
	s := openStream(t, fr, "ada", "")
	fr.event(t, "ax:team/alpha", "01ADA", 1, fw.EvRunQueued, queued("ada", "default"))
	fr.event(t, "ax:team/alpha", "01ADA", 2, fw.EvRunStarted, fw.RunStartedData{})
	fr.event(t, "ax:team/alpha", "01ADA", 3, fw.EvTurnStarted, fw.TurnData{Turn: 1})
	fr.event(t, "ax:team/alpha", "01ADA", 4, fw.EvToolRequested, fw.ToolRequestedData{Tool: "bash", CallID: "c1", Input: json.RawMessage(`{"command":"go test ./..."}`)})
	for {
		f := s.next(t)
		if f.kind != KindRun {
			continue
		}
		var r struct {
			Seq int `json:"seq"`
			Act *struct {
				Turn   int    `json:"turn"`
				Tool   string `json:"tool"`
				CallID string `json:"callId"`
				Phrase string `json:"phrase"`
				Since  string `json:"since"`
			} `json:"activity"`
		}
		if err := json.Unmarshal([]byte(f.data), &r); err != nil {
			t.Fatal(err)
		}
		if r.Seq < 4 {
			continue
		}
		if r.Act == nil || r.Act.Turn != 1 || r.Act.Tool != "bash" || r.Act.CallID != "c1" || r.Act.Phrase != `Ran "go test ./..."` || r.Act.Since == "" {
			t.Fatalf("run data = %s", f.data)
		}
		return
	}
}

// Ids grow, Last-Event-ID resumes after an id, and an id that the ring no
// longer holds, a future id, and a value that is no id each give one
// reset with the data null and the id of the newest event.
func TestStreamResume(t *testing.T) {
	fr := newFront(t, nil)
	s := openStream(t, fr, "ada", "")
	for _, name := range []string{"a", "b", "c"} {
		mark(t, name)
	}
	var ids []uint64
	for range 3 {
		f := s.next(t)
		n, err := strconv.ParseUint(f.id, 10, 64)
		if err != nil || f.kind != "test.mark" {
			t.Fatalf("frame = %+v", f)
		}
		ids = append(ids, n)
	}
	if ids[1] != ids[0]+1 || ids[2] != ids[1]+1 {
		t.Fatalf("ids = %v, want a counter", ids)
	}
	s.close()
	mark(t, "d")
	again := openStream(t, fr, "ada", strconv.FormatUint(ids[0], 10))
	var got []string
	for range 3 {
		f := again.next(t)
		var d map[string]string
		_ = json.Unmarshal([]byte(f.data), &d)
		got = append(got, d["mark"])
	}
	if strings.Join(got, "") != "bcd" {
		t.Fatalf("the resume gave %v, want b, c, d", got)
	}

	for range RingSize + 10 {
		mark(t, "x")
	}
	newest := fr.h.newestID()
	for _, last := range []string{strconv.FormatUint(ids[0], 10), strconv.FormatUint(newest+100, 10), "no-id"} {
		r := openStream(t, fr, "ada", last)
		f := r.next(t)
		if f.kind != KindReset || f.data != "null" || f.id != strconv.FormatUint(newest, 10) {
			t.Errorf("Last-Event-ID %s: first frame = %+v, want reset with null at %d", last, f, newest)
		}
		mark(t, "after")
		if f := r.next(t); f.kind != "test.mark" || !strings.Contains(f.data, "after") {
			t.Errorf("Last-Event-ID %s: after the reset = %+v", last, f)
		}
		newest = fr.h.newestID()
		r.close()
	}
}

// An idle stream gets a comment at every keep-alive, and a stream ends at
// the end of its life, so the page connects again with a fresh check.
func TestStreamKeepAliveAndLife(t *testing.T) {
	fr := newFront(t, nil)
	fr.h.keepAlive, fr.h.life = 30*time.Millisecond, 400*time.Millisecond
	s := openStream(t, fr, "ada", "")
	deadline := time.After(5 * time.Second)
	for got := false; !got; {
		select {
		case f, ok := <-s.frames:
			if !ok {
				t.Fatal("the stream ended before a keep-alive")
			}
			got = f.comment == "keep-alive"
		case <-deadline:
			t.Fatal("no keep-alive")
		}
	}
	for {
		select {
		case _, ok := <-s.frames:
			if !ok {
				return
			}
		case <-deadline:
			t.Fatal("the stream outlived its life")
		}
	}
}

// The first bytes of the body go out at once after the headers: the
// comment ": connected", before any event and long before a keep-alive, so
// a proxy that holds an answer until its first bytes passes it on at once.
func TestStreamSaysConnectedAtOnce(t *testing.T) {
	fr := newFront(t, nil)
	fr.h.keepAlive = time.Hour
	start := time.Now()
	s := openStream(t, fr, "ada", "")
	select {
	case f, ok := <-s.frames:
		if !ok || f.comment != "connected" || f.kind != "" || f.id != "" {
			t.Fatalf("first frame = %+v, %v", f, ok)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no first bytes within 2 seconds")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("the first bytes took %v", d)
	}
	// The events follow as before.
	mark(t, "after")
	if f := s.next(t); f.kind != "test.mark" {
		t.Fatalf("next frame = %+v", f)
	}
}

// The stream needs a credential, and a process serves at most MaxStreams.
func TestStreamAuthAndBound(t *testing.T) {
	fr := newFront(t, nil)
	if resp, _ := get(t, fr.url+"/v1/bossmode/stream", ""); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("without a token: %d", resp.StatusCode)
	}
	open := make([]*sse, 0, MaxStreams)
	for range MaxStreams {
		open = append(open, openStream(t, fr, "ada", ""))
	}
	resp, body := get(t, fr.url+"/v1/bossmode/stream", "ada")
	if resp.StatusCode != http.StatusTooManyRequests || resp.Header.Get("Retry-After") == "" || !strings.Contains(body, "64 streams") {
		t.Fatalf("stream %d: %d %v %s", MaxStreams+1, resp.StatusCode, resp.Header, body)
	}
	if n := fr.h.Streams(); n != MaxStreams {
		t.Errorf("Streams = %d", n)
	}
	open[0].close()
	deadline := time.Now().Add(5 * time.Second)
	for fr.h.Streams() >= MaxStreams {
		if time.Now().After(deadline) {
			t.Fatal("a closed stream still counts")
		}
		time.Sleep(10 * time.Millisecond)
	}
	openStream(t, fr, "ada", "")
}

// Publish puts an event on every stream, or with Allow on the streams of
// the callers that it allows, and refuses a reserved kind and a large
// event. Without a hub it does nothing.
func TestPublish(t *testing.T) {
	fr := newFront(t, nil)
	ada := openStream(t, fr, "ada", "")
	bob := openStream(t, fr, "bob", "")
	onlyAda := Allow(func(ctx context.Context) bool {
		p := fw.PrincipalFrom(ctx)
		return p != nil && p.Subject == "ada"
	})
	if err := Publish("bridge.message", "br-1", map[string]any{"bridge": "br-1", "message": map[string]string{"text": "hi"}}, onlyAda); err != nil {
		t.Fatal(err)
	}
	if err := PublishTo(fr.a, "page", "notes/a.md", map[string]string{"path": "notes/a.md"}); err != nil {
		t.Fatal(err)
	}
	f := ada.next(t)
	if f.kind != "bridge.message" || f.data != `{"bridge":"br-1","message":{"text":"hi"}}` {
		t.Errorf("ada got %+v", f)
	}
	if f := ada.next(t); f.kind != "page" {
		t.Errorf("ada got %+v", f)
	}
	if f := bob.next(t); f.kind != "page" {
		t.Errorf("bob got %+v, want only the page", f)
	}
	for _, kind := range []string{"run", "member", "task", "reset", "", "Bridge", "a\nb", strings.Repeat("k", 65)} {
		if err := Publish(kind, "x", 1); !errors.Is(err, ErrKind) {
			t.Errorf("Publish(%q) = %v", kind, err)
		}
	}
	if err := Publish("big", "x", strings.Repeat("x", MaxEventBytes)); !errors.Is(err, ErrTooLarge) {
		t.Errorf("a large event: %v", err)
	}
	if err := Publish("bad", "x", func() {}); err == nil {
		t.Error("data that does not encode is no error")
	}
	if err := PublishTo(&fw.Agent{}, "page", "x", 1); err != nil {
		t.Errorf("PublishTo without a hub: %v", err)
	}
}
