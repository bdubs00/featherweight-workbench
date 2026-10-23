package bridges

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
)

// The record of a new bridge has the shape of the contract, and the
// channel takes the messages of people in seq order with the cut at 4 KB.
func TestRecordAndChannel(t *testing.T) {
	a := newAgent(t, nil)
	s := newStore(t, a, nil)
	url := serve(t, a)

	it := open(t, url)
	if it.State != StateOpen || it.Placement != PlacementQueue || it.Operator != "alice" || it.Manager != nil || it.LastSeq != 1 {
		t.Fatalf("record = %+v", it.Bridge)
	}
	if it.Delegation != (Delegation{Report: true}) || it.Caps != (Caps{MaxMessages: 2000, MaxFanoutPerMinute: 30, MaxPairMessages: 6}) {
		t.Fatalf("delegation %+v caps %+v", it.Delegation, it.Caps)
	}
	if len(it.Members) != 2 || it.MemberCount != 2 || it.Members[0].Handle != "migrator-1" || it.Members[0].Presence != PresenceWorking || it.Members[0].Runs == nil {
		t.Fatalf("members = %+v", it.Members)
	}
	opened := channel(t, s, it.ID)
	if len(opened) != 1 || opened[0].Event != EventOpened || opened[0].From.Kind != KindSystem || opened[0].Text != "Alice opened the bridge with 2 members." {
		t.Fatalf("channel = %+v", opened)
	}

	resp, body := do(t, http.MethodPost, url+"/v1/bridges/"+it.ID+"/messages", "alice", `{"text":"Start with the cron part."}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("post = %d %s", resp.StatusCode, body)
	}
	var m Message
	decodeJSON(t, body, &m)
	if m.Seq != 2 || m.From != (From{Kind: KindPerson, Name: "alice", Display: "Alice"}) || m.Cut || len(m.To) != 0 || m.ReplyTo != nil || m.Reactions[ReactionHeart] == nil {
		t.Fatalf("entry = %+v", m)
	}
	if !strings.Contains(body, `"replyTo":null`) || !strings.Contains(body, `"to":[]`) || !strings.Contains(body, `"decision":null`) {
		t.Fatalf("entry JSON = %s", body)
	}
	long := strings.Repeat("é", 3000) // 6000 bytes
	resp, body = do(t, http.MethodPost, url+"/v1/bridges/"+it.ID+"/messages", "alice", `{"text":"`+long+`","replyTo":2}`)
	decodeJSON(t, body, &m)
	if resp.StatusCode != http.StatusCreated || !m.Cut || len(m.Text) > entryTextBytes || len(m.Text) < entryTextBytes-1 || m.ReplyTo == nil || *m.ReplyTo != 2 {
		t.Fatalf("long post = %d cut %v len %d", resp.StatusCode, m.Cut, len(m.Text))
	}
	for _, c := range []struct{ body, want string }{
		{`{"text":"hi","to":["nobody"]}`, "no member nobody"},
		{`{"text":"hi","replyTo":99}`, "no entry 99"},
		{`{"text":"  "}`, "needs text or files"},
	} {
		resp, body := do(t, http.MethodPost, url+"/v1/bridges/"+it.ID+"/messages", "alice", c.body)
		if resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, c.want) {
			t.Errorf("%s = %d %s", c.body, resp.StatusCode, body)
		}
	}

	// Paging, as the transcript route pages.
	page := func(q string) Page {
		t.Helper()
		resp, body := do(t, http.MethodGet, url+"/v1/bridges/"+it.ID+"/messages"+q, "alice", "")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("messages%s = %d %s", q, resp.StatusCode, body)
		}
		var p Page
		decodeJSON(t, body, &p)
		return p
	}
	seqs := func(p Page) []uint64 {
		var out []uint64
		for _, m := range p.Messages {
			out = append(out, m.Seq)
		}
		return out
	}
	if p := page(""); p.Bridge != it.ID || len(p.Messages) != 3 || p.Prev != 0 || p.Next != 0 {
		t.Fatalf("whole page = %v prev %d next %d", seqs(p), p.Prev, p.Next)
	}
	if p := page("?limit=2"); len(p.Messages) != 2 || p.Messages[0].Seq != 2 || p.Prev != 2 || p.Next != 0 {
		t.Fatalf("last 2 = %v prev %d next %d", seqs(p), p.Prev, p.Next)
	}
	if p := page("?before=2&limit=2"); len(p.Messages) != 1 || p.Messages[0].Seq != 1 || p.Prev != 0 || p.Next != 2 {
		t.Fatalf("before 2 = %v prev %d next %d", seqs(p), p.Prev, p.Next)
	}
	if p := page("?from=1&limit=2"); len(p.Messages) != 2 || p.Messages[1].Seq != 2 || p.Next != 3 {
		t.Fatalf("from 1 = %v next %d", seqs(p), p.Next)
	}
	if resp, _ := do(t, http.MethodGet, url+"/v1/bridges/"+it.ID+"/messages?from=1&before=3", "alice", ""); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("from and before = %d", resp.StatusCode)
	}
	if resp, _ := do(t, http.MethodGet, url+"/v1/bridges/nope/messages", "alice", ""); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown bridge = %d", resp.StatusCode)
	}
}

// The read mark of a person moves forward only, and the unread count holds
// the messages of others and the cards that wait, not the messages of the
// caller and not the plain system lines.
func TestReadMarksAndUnread(t *testing.T) {
	a := newAgent(t, nil)
	s := newStore(t, a, nil)
	url := serve(t, a)
	it := open(t, url)
	do(t, http.MethodPost, url+"/v1/bridges/"+it.ID+"/messages", "alice", `{"text":"one"}`)
	do(t, http.MethodPost, url+"/v1/bridges/"+it.ID+"/messages", "alice", `{"text":"two"}`)
	if _, err := s.post(context.Background(), it.ID, sender{handle: "migrator-1", run: "r1"}, PostSpec{Text: "a status"}); err != nil {
		t.Fatal(err)
	}
	list := func(token string) []Item {
		t.Helper()
		resp, body := do(t, http.MethodGet, url+"/v1/bridges", token, "")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("list as %s = %d %s", token, resp.StatusCode, body)
		}
		var out struct {
			Bridges []Item `json:"bridges"`
		}
		decodeJSON(t, body, &out)
		return out.Bridges
	}
	// alice posted twice, so her mark is at her last message: the message
	// of the agent is unread.
	if l := list("alice"); len(l) != 1 || l[0].LastSeq != 4 || l[0].Read != 3 || l[0].Unread != 1 || l[0].Members != nil || l[0].MemberCount != 2 {
		t.Fatalf("alice = %+v", l)
	}
	// bob is on the team of alice and operates both agents.
	if l := list("bob"); len(l) != 1 || l[0].Read != 0 || l[0].Unread != 3 {
		t.Fatalf("bob = %+v", l)
	}
	// dave is on the team but cannot operate the agents; carol is on no
	// team of alice.
	if l := list("dave"); len(l) != 0 {
		t.Fatalf("dave = %+v", l)
	}
	if l := list("carol"); len(l) != 0 {
		t.Fatalf("carol = %+v", l)
	}
	if resp, body := do(t, http.MethodPut, url+"/v1/bridges/"+it.ID+"/read", "bob", `{"seq":3}`); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("read = %d %s", resp.StatusCode, body)
	}
	do(t, http.MethodPut, url+"/v1/bridges/"+it.ID+"/read", "bob", `{"seq":1}`)
	if l := list("bob"); l[0].Read != 3 || l[0].Unread != 1 {
		t.Fatalf("bob after read = %+v", l[0])
	}
	// A card that waits counts until its decision lands.
	if err := s.postLines(context.Background(), it.ID, system(EventChanged, "a plain line")); err != nil {
		t.Fatal(err)
	}
	card := system(EventNeedsYou, "migrator-1 wants to run a command.")
	card.Decision = &Decision{Run: "r1", State: DecisionPending}
	var seq uint64
	if _, err := s.b.update(context.Background(), it.ID, func(rec *record) error {
		card.Seq = rec.alloc()
		seq = card.Seq
		rec.Cards = append(rec.Cards, card.Seq)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	s.land(context.Background(), it.ID, card)
	if l := list("bob"); l[0].Unread != 2 {
		t.Fatalf("bob with a card = %+v", l[0])
	}
	if _, err := s.b.update(context.Background(), it.ID, func(rec *record) error { rec.dropCard(seq); return nil }); err != nil {
		t.Fatal(err)
	}
	if l := list("bob"); l[0].Unread != 1 {
		t.Fatalf("bob after the decision = %+v", l[0])
	}
}

// Writers that race on one bridge never lose a message: every post gets
// its own seq.
func TestPostsRaceWithoutLoss(t *testing.T) {
	a := newAgent(t, nil)
	s := newStore(t, a, nil)
	url := serve(t, a)
	it := open(t, url)
	var wg sync.WaitGroup
	const writers = 8
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.post(context.Background(), it.ID, sender{subject: "alice"}, PostSpec{Text: "hi"}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	msgs := channel(t, s, it.ID)
	if len(msgs) != writers+1 {
		t.Fatalf("channel = %d entries", len(msgs))
	}
	for i, m := range msgs {
		if m.Seq != uint64(i+1) {
			t.Fatalf("entry %d has seq %d", i, m.Seq)
		}
	}
}
