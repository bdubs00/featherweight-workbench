package bridges

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// The check of a team by a person. Needs work posts the note in the
// channel to every seat and keeps the bridge open, and it needs a note.
// Looks right closes the bridge with the verdict accepted, and an open
// again clears the verdict. A bridge that a person closed without a check
// takes the verdict alone. A person without the right to use the bridge
// cannot check it.
func TestCheck(t *testing.T) {
	a := newAgent(t, nil)
	s := newStore(t, a, nil)
	url := serve(t, a)
	it := open(t, url)
	alice := fw.WithPrincipal(context.Background(), callers["alice"])
	if _, err := s.Check(alice, it.ID, false, "  "); !errors.Is(err, fw.ErrInvalidRequest) {
		t.Fatalf("Needs work without a note = %v", err)
	}
	got, err := s.Check(alice, it.ID, false, "Name the module in the title.")
	if err != nil || got.State != StateOpen || got.Verdict != "" {
		t.Fatalf("Needs work = %+v, %v", got, err)
	}
	var note *Message
	for _, m := range channel(t, s, it.ID) {
		if m.From.Kind == KindPerson && m.Text == SendBackPrefix+"Name the module in the title." {
			note = &m
		}
	}
	if note == nil || len(note.To) != 0 || note.From.Name != "alice" {
		t.Fatalf("the note in the channel = %+v", note)
	}
	if _, err := s.Check(fw.WithPrincipal(context.Background(), callers["carol"]), it.ID, true, ""); !errors.Is(err, fw.ErrForbidden) {
		t.Fatalf("a check by carol = %v", err)
	}
	got, err = s.Check(alice, it.ID, true, "")
	if err != nil || got.State != StateClosed || got.Verdict != VerdictAccepted || got.ClosedAt == nil {
		t.Fatalf("Looks right = %+v, %v", got, err)
	}
	if l := withEvent(channel(t, s, it.ID), EventClosed); len(l) != 1 || l[0].Text != "Alice said that the result looks right and closed the channel" {
		t.Fatalf("the line of the close = %+v", l)
	}
	// A closed bridge takes no note.
	if _, err := s.Check(alice, it.ID, false, "More."); !errors.Is(err, ErrConflict) {
		t.Fatalf("Needs work on a closed bridge = %v", err)
	}
	resp, body := do(t, http.MethodPatch, url+"/v1/bridges/"+it.ID, "alice", `{"state":"open"}`)
	var again Item
	decodeJSON(t, body, &again)
	if resp.StatusCode != http.StatusOK || again.State != StateOpen || again.Verdict != "" || strings.Contains(body, `"verdict"`) {
		t.Fatalf("open again = %d %s", resp.StatusCode, body)
	}
	do(t, http.MethodPatch, url+"/v1/bridges/"+it.ID, "alice", `{"state":"closed","delegation":{"report":false}}`)
	got, err = s.Check(alice, it.ID, true, "")
	if err != nil || got.State != StateClosed || got.Verdict != VerdictAccepted {
		t.Fatalf("Looks right on a closed bridge = %+v, %v", got, err)
	}
	if l := withEvent(channel(t, s, it.ID), EventChanged); len(l) == 0 || l[len(l)-1].Text != "Alice said that the result looks right" {
		t.Fatalf("the line of the verdict = %+v", l)
	}
	// The route of a person takes no verdict.
	if resp, body := do(t, http.MethodPatch, url+"/v1/bridges/"+it.ID, "alice", `{"verdict":"accepted"}`); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("PATCH with a verdict = %d %s", resp.StatusCode, body)
	}
}

// A seat that asked a question and whose run the channel then called gone
// still waits for the person who asked for the bridge: an answer seats the
// member again on a new run, which reads the channel, and the person can
// settle the question without an answer. So Waiting lists it, and the
// count needsYou of the bridge holds it, for that person and for another
// viewer, who reads whom it waits for (live walk m1, which replaces walk
// finding R1).
func TestWaitingGone(t *testing.T) {
	a := newAgent(t, nil)
	s := newStore(t, a, nil)
	rec := &record{Bridge: Bridge{ID: "lost-q", Title: "Exporter", Operator: "alice", OperatorName: "Alice", State: StateOpen, Placement: PlacementQueue, Members: []Member{
		{Handle: "ops", Agent: "migrator", Task: "Install", Runs: []string{"r-old"}},
	}}, Seats: map[string]*seatMark{"ops": {Gone: "r-old", Ask: 3}}, Marks: map[string]*runMark{"r-old": {Seat: "ops", Done: 2}}}
	if err := s.b.create(context.Background(), rec); err != nil {
		t.Fatal(err)
	}
	ctx := fw.WithPrincipal(context.Background(), callers["alice"])
	waits, err := s.Waiting(ctx)
	if err != nil || len(waits) != 1 || !waits[0].Gone || waits[0].Question != 3 {
		t.Fatalf("Waiting = %+v, %v", waits, err)
	}
	it, err := s.item(ctx, rec, "alice", true)
	if err != nil || it.NeedsYou != 1 || it.WaitsFor != "you" {
		t.Fatalf("the item of the bridge = %+v, %v", it, err)
	}
	if it, err := s.item(ctx, rec, "bob", true); err != nil || it.NeedsYou != 1 || it.WaitsFor != "Alice" {
		t.Fatalf("the item for another viewer = %+v, %v", it, err)
	}
	ms := decorate(rec, nil, nil)
	if len(ms) != 1 || !ms[0].Gone || ms[0].Presence != PresenceNeedsYou || ms[0].Question != 3 {
		t.Fatalf("the seat = %+v", ms)
	}
}

// FirstLine reads the first line that says something, without the marks
// of Markdown and the handle of a teammate in front, and the item of a
// bridge carries the first line of the newest message of an agent.
func TestFirstLine(t *testing.T) {
	for in, want := range map[string]string{
		"## Done\n\nThe role passes.":          "Done",
		"\n---\n**The table is in** `x.md`.":   "The table is in x.md.",
		"@reviewer The role is ready for you.": "The role is ready for you.",
		"@reviewer\n- Ready.":                  "Ready.",
		"   ":                                  "",
	} {
		if got := FirstLine(in, 200); got != want {
			t.Errorf("FirstLine(%q) = %q, want %q", in, got, want)
		}
	}
	if got := FirstLine("abcdefghij", 4); got != "abcd..." {
		t.Errorf("a long line = %q", got)
	}
	a := newAgent(t, nil)
	s := newStore(t, a, nil)
	rec := &record{Bridge: Bridge{ID: "sum-1", Title: "Notes", Operator: "alice", State: StateOpen, Placement: PlacementQueue, Members: []Member{
		{Handle: "writer", Agent: "migrator", Task: "Write", Runs: []string{"r1"}},
	}}}
	if err := s.b.create(context.Background(), rec); err != nil {
		t.Fatal(err)
	}
	ctx := fw.WithPrincipal(context.Background(), callers["alice"])
	if _, err := s.post(ctx, rec.ID, sender{handle: "writer", run: "r1"}, PostSpec{Text: "**The notes of 2.4 are in the wiki.**\n\nDetails follow."}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.post(ctx, rec.ID, sender{subject: "alice"}, PostSpec{Text: "Thanks."}); err != nil {
		t.Fatal(err)
	}
	rec, _, _ = s.b.get(context.Background(), rec.ID)
	it, err := s.item(ctx, rec, "alice", false)
	if err != nil || it.Summary != "The notes of 2.4 are in the wiki." {
		t.Fatalf("the summary = %q, %v", it.Summary, err)
	}
}

// The people of a channel carry a display name, never the UUID of a
// sign-in: the person who asked for it first, then the people who posted.
// An entry of a person that kept an email reads as the display name, and
// the static token is Admin token (live walk M3).
func TestPeopleOfAChannel(t *testing.T) {
	a := newAgent(t, nil)
	s := newStore(t, a, nil)
	const frank = "0c1d2e3f-1b7d-4e2a-9c1f-2a3b4c5d6e7f"
	rec := &record{Bridge: Bridge{ID: "ppl-1", Title: "Miners", Operator: frank, OperatorName: "frank.miller@example.com", State: StateOpen, Placement: PlacementQueue, Members: []Member{
		{Handle: "writer", Agent: "migrator", Task: "Write", Runs: []string{"r1"}},
	}}}
	if err := s.b.create(context.Background(), rec); err != nil {
		t.Fatal(err)
	}
	ctx := fw.WithPrincipal(context.Background(), callers["alice"])
	for _, snd := range []sender{{subject: "token"}, {subject: frank, display: "frank.miller@example.com"}} {
		if _, err := s.post(ctx, rec.ID, snd, PostSpec{Text: "Go on."}); err != nil {
			t.Fatal(err)
		}
	}
	rec, _, _ = s.b.get(context.Background(), rec.ID)
	it, err := s.item(ctx, rec, "alice", false)
	if err != nil {
		t.Fatal(err)
	}
	want := []Person{{Subject: frank, Display: "Frank Miller", Role: RoleAsked}, {Subject: "token", Display: "Admin token", Role: RolePosted}}
	if len(it.People) != 2 || it.People[0] != want[0] || it.People[1] != want[1] {
		t.Fatalf("the people = %+v", it.People)
	}
	for _, m := range channel(t, s, rec.ID) {
		if m.From.Kind == KindPerson && (strings.Contains(m.From.Display, "@") || strings.Contains(m.From.Display, frank)) {
			t.Fatalf("an entry names a person by %q", m.From.Display)
		}
	}
}

// A channel has a topic, the whole request: the topic of its open, else
// its goal, else a title that is a whole sentence. A short title gives no
// topic (live walk M8).
func TestTopicOf(t *testing.T) {
	for _, c := range []struct {
		b    Bridge
		want string
	}{
		{Bridge{Title: "Backup policy", Topic: "Write the backup policy of the lab.", Goal: "A policy."}, "Write the backup policy of the lab."},
		{Bridge{Title: "Backup policy", Goal: "A policy for every host."}, "A policy for every host."},
		{Bridge{Title: "Write homelab/overview.md: a one-page overview of the homelab"}, "Write homelab/overview.md: a one-page overview of the homelab"},
		{Bridge{Title: "Backup policy"}, ""},
	} {
		rec := &record{Bridge: c.b}
		if got := rec.public().Topic; got != c.want {
			t.Errorf("the topic of %+v = %q, want %q", c.b, got, c.want)
		}
	}
}
