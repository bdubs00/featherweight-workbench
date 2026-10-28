package knowhow

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/fleet"
)

// A note without a source that can go out of date is unchecked; one with
// the version of a page or an item is checked. The record holds the hash
// and the description of the text, and the journal says learned.
func TestNoteStates(t *testing.T) {
	fr := newFront(t, nil)
	res := fr.write(t, duty, "gotchas/labels.md", "# Labels\nGitea refuses a label name with a space.", Source{Ref: "run:01KRUN"})
	if !res.Created || res.Note.State != StateUnchecked || res.Note.Hash != Hash("# Labels\nGitea refuses a label name with a space.") || res.Note.Description != "Labels" {
		t.Fatalf("the new note = %+v", res)
	}
	res = fr.write(t, duty, "pages.md", "The label page says what each label means.", Source{Ref: "page:agents/triage/labels.md", Version: Hash("v1")})
	if res.Note.State != StateChecked {
		t.Fatalf("a note with a page = %s", res.Note.State)
	}
	j := fr.journal(t)
	if len(j) != 2 || j[0].What != WhatLearned || j[0].Subject.Note != "pages.md" || j[0].Undo == nil || j[0].Undo.Revision != 0 || j[0].Undo.After == 0 {
		t.Fatalf("journal = %+v", j)
	}
	// The same text again changes nothing.
	again := fr.write(t, duty, "pages.md", "The label page says what each label means.", Source{Ref: "page:agents/triage/labels.md", Version: Hash("v1")})
	if again.Changed || len(fr.journal(t)) != 2 {
		t.Fatalf("a write of the same text changed something: %+v", again)
	}
}

// A write names the hash that it read. A write with another hash fails
// with the hash of the note as it is, absent fails for a note that exists,
// and a put-away note counts as absent.
func TestNoteCAS(t *testing.T) {
	fr := newFront(t, nil)
	ctx := context.Background()
	first := fr.write(t, duty, "a.md", "one")
	_, err := fr.s.Write(ctx, WriteRequest{Agent: "triage", Scope: duty, Name: "a.md", Text: "two", IfHash: Hash("other"), By: ByAgent})
	var ce *ConflictError
	if !errors.As(err, &ce) || ce.Hash != first.Note.Hash {
		t.Fatalf("a write with another hash = %v", err)
	}
	if _, err := fr.s.Write(ctx, WriteRequest{Agent: "triage", Scope: duty, Name: "a.md", Text: "two", IfHash: IfAbsent, By: ByAgent}); !errors.Is(err, ErrConflict) {
		t.Fatalf("absent for a note that exists = %v", err)
	}
	if _, err := fr.s.Write(ctx, WriteRequest{Agent: "triage", Scope: duty, Name: "a.md", Text: "two", IfHash: "nothash", By: ByAgent}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a condition that is no hash = %v", err)
	}
	res, err := fr.s.Write(ctx, WriteRequest{Agent: "triage", Scope: duty, Name: "a.md", Text: "two", IfHash: first.Note.Hash, By: ByAgent})
	if err != nil || res.Created || !res.Changed || res.Note.Text != "two" {
		t.Fatalf("a write with the hash = %+v, %v", res, err)
	}
	if _, err := fr.s.PutAway(ctx, PutAwayRequest{Agent: "triage", Scope: duty, Name: "a.md", IfHash: Hash("two"), By: ByAgent}); err != nil {
		t.Fatal(err)
	}
	if n := fr.note(t, duty, "a.md"); n.State != StatePutAway || n.Text != "two" {
		t.Fatalf("a put-away note = %+v", n)
	}
	if res, err := fr.s.Write(ctx, WriteRequest{Agent: "triage", Scope: duty, Name: "a.md", Text: "three", IfHash: IfAbsent, By: ByAgent}); err != nil || !res.Created {
		t.Fatalf("absent over a put-away note = %+v, %v", res, err)
	}
}

// A note that a person wrote never changes by automation: the agent, the
// review, and the passes leave it as it is, and it never rests.
func TestPersonNoteStays(t *testing.T) {
	fr := newFront(t, nil)
	ctx := context.Background()
	if _, err := fr.s.Write(ctx, WriteRequest{Agent: "triage", Scope: duty, Name: "rule.md", Text: "Tickets of the team ops go to bob.", By: "person:alice",
		Sources: []Source{{Ref: "page:team.md", Version: Hash("v1")}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := fr.s.Write(ctx, WriteRequest{Agent: "triage", Scope: duty, Name: "rule.md", Text: "changed", By: ByAgent}); !errors.Is(err, ErrPerson) {
		t.Fatalf("the agent changed a note of a person: %v", err)
	}
	if _, err := fr.s.PutAway(ctx, PutAwayRequest{Agent: "triage", Scope: duty, Name: "rule.md", By: ByReview}); !errors.Is(err, ErrPerson) {
		t.Fatalf("the review put away a note of a person: %v", err)
	}
	fr.s.PageWritten(ctx, "team.md", Hash("v2"))
	fr.clk.add(60 * 24 * time.Hour)
	if _, _, err := fr.s.Age(ctx); err != nil {
		t.Fatal(err)
	}
	if n := fr.note(t, duty, "rule.md"); n.State != StateChecked || n.OutOfDate != nil {
		t.Fatalf("automation changed a note of a person: %+v", n)
	}
	// A person changes it.
	if _, err := fr.s.Write(ctx, WriteRequest{Agent: "triage", Scope: duty, Name: "rule.md", Text: "Tickets of ops go to carol.", By: "person:bob"}); err != nil {
		t.Fatal(err)
	}
}

// A change of a page marks out of date each note that names the page with
// an older version, writes the journal with the source, and opens a
// candidate recheck. A change of an item does the same for its notes. A
// note that names the version as it is stays checked.
func TestOutOfDate(t *testing.T) {
	fr := newFront(t, nil)
	ctx := context.Background()
	fr.write(t, duty, "labels.md", "The label bug means broken.", Source{Ref: "page:agents/triage/labels.md", Version: Hash("v1")})
	fr.write(t, duty, "fresh.md", "Already read v2.", Source{Ref: "page:agents/triage/labels.md", Version: Hash("v2")})
	fr.write(t, duty, "issue.md", "Issue 42 waits for the vendor.", Source{Ref: "item:gitea/issue/42", Version: "2026-10-26T10:00:00Z"})
	fr.write(t, Scope{Kind: ScopeAgent}, "shared.md", "The label bug means broken.", Source{Ref: "page:agents/triage/labels.md", Version: Hash("v1")})

	fr.clk.add(time.Hour)
	PageWritten(ctx, "agents/triage/labels.md", Hash("v2"))
	n := fr.note(t, duty, "labels.md")
	if n.State != StateOutOfDate || n.OutOfDate == nil || n.OutOfDate.Source != "page:agents/triage/labels.md" || !n.OutOfDate.Since.Equal(day0.Add(time.Hour)) {
		t.Fatalf("the note of the page = %+v", n)
	}
	if fr.note(t, duty, "fresh.md").State != StateChecked {
		t.Fatal("a note with the version as it is went out of date")
	}
	if fr.note(t, Scope{Kind: ScopeAgent}, "shared.md").State != StateOutOfDate {
		t.Fatal("a note of the agent did not go out of date")
	}
	j := fr.journal(t)
	if j[0].What != WhatOutOfDate || j[0].Why == "" || j[0].By != ByKnowhow {
		t.Fatalf("journal = %+v", j[0])
	}
	// The same item version changes nothing, another one marks the note.
	ItemChanged(ctx, "gitea", "issue/42", "2026-10-26T10:00:00Z")
	if fr.note(t, duty, "issue.md").State != StateChecked {
		t.Fatal("the same version marked the note")
	}
	ItemChanged(ctx, "gitea", "issue/42", "2026-10-27T08:00:00Z")
	if n := fr.note(t, duty, "issue.md"); n.State != StateOutOfDate || n.OutOfDate.Source != "item:gitea/issue/42" {
		t.Fatalf("the note of the item = %+v", n)
	}
	// A recheck waits for the review for each note that went out of date,
	// in the scope of its note: the notes of the principal and the note of
	// the agent. A note that a person wrote gets none.
	fr.s.Write(ctx, WriteRequest{Agent: "triage", Scope: Scope{Kind: ScopeAgent}, Name: "person.md", Text: "Mine.", By: "person:alice",
		Sources: []Source{{Ref: "page:agents/triage/labels.md", Version: Hash("v2")}}})
	PageWritten(ctx, "agents/triage/labels.md", Hash("v3"))
	open, err := fr.s.Candidates(ctx, "triage", func(c Candidate) bool { return c.Kind == KindRecheck })
	scopes := map[string]Scope{}
	byNote := map[string]Candidate{}
	for _, c := range open {
		scopes[c.Note], byNote[c.Note] = c.Scope, c
	}
	if err != nil || len(open) != 4 || byNote["labels.md"].State != CandOpen || byNote["labels.md"].Sources[0].Version != Hash("v2") ||
		scopes["shared.md"] != (Scope{Kind: ScopeAgent}) || scopes["issue.md"] != duty || scopes["fresh.md"] != duty || len(scopes) != 4 {
		t.Fatalf("rechecks = %+v, %v", open, err)
	}
	if fr.note(t, Scope{Kind: ScopeAgent}, "person.md").State != StateChecked {
		t.Fatal("automation marked a note of a person out of date")
	}
	// A new text replaces it, and the journal counts a correction.
	res := fr.write(t, duty, "labels.md", "The label bug means broken for the requester.", Source{Ref: "page:agents/triage/labels.md", Version: Hash("v2")})
	if res.Note.State != StateChecked || res.Note.OutOfDate != nil || res.Entry == nil || !res.Entry.Corrects {
		t.Fatalf("the corrected note = %+v", res)
	}
}

// Undo puts back the revision before a change; a note that a change made
// is put away by its undo. A later change of the same note makes an undo
// answer ErrLater, and an undo of an undo brings the change back.
func TestUndo(t *testing.T) {
	fr := newFront(t, nil)
	ctx := context.Background()
	fr.write(t, duty, "a.md", "one")
	fr.write(t, duty, "a.md", "two")
	fr.write(t, duty, "a.md", "three")
	j := fr.journal(t)
	if len(j) != 3 || j[0].What != WhatUpdated {
		t.Fatalf("journal = %+v", j)
	}
	if _, err := fr.s.TakeBack(ctx, j[1].ID, "person:alice", true); !errors.Is(err, ErrLater) {
		t.Fatalf("an undo under a later change = %v", err)
	}
	res, err := fr.s.TakeBack(ctx, j[0].ID, "person:alice", true)
	if err != nil || res.Entry.What != WhatUndone || res.Undone.UndoneBy != res.Entry.ID {
		t.Fatalf("undo = %+v, %v", res, err)
	}
	if n := fr.note(t, duty, "a.md"); n.Text != "two" {
		t.Fatalf("after the undo the note = %q", n.Text)
	}
	if _, err := fr.s.TakeBack(ctx, j[0].ID, "person:alice", true); !errors.Is(err, ErrLater) {
		t.Fatalf("a second undo of one entry = %v", err)
	}
	// The undo of the undo brings three back.
	if _, err := fr.s.TakeBack(ctx, res.Entry.ID, "person:alice", true); err != nil {
		t.Fatal(err)
	}
	if n := fr.note(t, duty, "a.md"); n.Text != "three" {
		t.Fatalf("after the undo of the undo = %q", n.Text)
	}
	// The undo of the first write puts a new note away.
	fr.write(t, duty, "b.md", "new")
	j = fr.journal(t)
	if _, err := fr.s.TakeBack(ctx, j[0].ID, "person:alice", true); err != nil {
		t.Fatal(err)
	}
	if n := fr.note(t, duty, "b.md"); n.State != StatePutAway {
		t.Fatalf("the undo of a new note = %+v", n)
	}
	if _, err := fr.s.TakeBack(ctx, "nothing", "person:alice", true); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an undo of no entry = %v", err)
	}
}

// The journal keeps 90 days and 5,000 entries per agent at most.
func TestJournalBounds(t *testing.T) {
	fr := newFront(t, nil)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if _, err := fr.s.appendJournal(ctx, Entry{Agent: "triage", What: WhatLearned, Subject: Subject{Note: "x.md"}, Why: "old"}); err != nil {
			t.Fatal(err)
		}
	}
	fr.clk.add(91 * 24 * time.Hour)
	if _, err := fr.s.appendJournal(ctx, Entry{Agent: "triage", What: WhatLearned, Subject: Subject{Note: "y.md"}, Why: "new"}); err != nil {
		t.Fatal(err)
	}
	if err := fr.s.pruneJournal(ctx, "triage"); err != nil {
		t.Fatal(err)
	}
	if j := fr.journal(t); len(j) != 1 || j[0].Why != "new" {
		t.Fatalf("after 91 days the journal = %+v", j)
	}
	// The page reads back before an entry.
	e2, _ := fr.s.appendJournal(ctx, Entry{Agent: "triage", What: WhatLearned, Subject: Subject{Note: "z.md"}, Why: "newer"})
	list, err := fr.s.Journal(ctx, "triage", 10, e2.ID, nil)
	if err != nil || len(list) != 1 || list[0].Why != "new" {
		t.Fatalf("before = %+v, %v", list, err)
	}
	if len(clipWhy(string(make([]byte, 400)))) > MaxWhyBytes+3 {
		t.Fatal("a long why is not cut")
	}
}

// With the switch on, a new note of the agent waits in the state waiting,
// a change of a note waits beside it, and the note stays as it is. Keep
// makes the change; a target that changed since answers ErrLater; not
// now puts a new note away.
func TestAskFirst(t *testing.T) {
	fr := newFront(t, nil)
	ctx := context.Background()
	fr.write(t, duty, "a.md", "one")
	if _, err := fr.s.SetAskFirst(ctx, "triage", true, "person:alice"); err != nil {
		t.Fatal(err)
	}
	if !AskFirst(ctx, "triage") || AskFirst(ctx, "private") {
		t.Fatal("the switch does not read")
	}
	res := fr.write(t, duty, "b.md", "new")
	if res.Note.State != StateWaiting || res.Waiting == nil {
		t.Fatalf("a new note under the switch = %+v", res)
	}
	res = fr.write(t, duty, "a.md", "two")
	if res.Waiting == nil || res.Changed || fr.note(t, duty, "a.md").Text != "one" {
		t.Fatalf("a change under the switch = %+v", res)
	}
	// A person writes as before.
	if _, err := fr.s.Write(ctx, WriteRequest{Agent: "triage", Scope: duty, Name: "c.md", Text: "mine", By: "person:alice", Hold: true}); err != nil {
		t.Fatal(err)
	}
	if fr.note(t, duty, "c.md").State != StateUnchecked {
		t.Fatal("the switch held a note of a person")
	}
	list, err := fr.s.WaitingList(ctx, "triage", nil)
	if err != nil || len(list) != 2 || list[0].What != WhatLearned || list[1].What != WhatUpdated || list[1].Pin != Hash("one") {
		t.Fatalf("waiting = %+v, %v", list, err)
	}
	if _, err := fr.s.Decide(ctx, list[0].ID, true, "person:alice"); err != nil {
		t.Fatal(err)
	}
	if n := fr.note(t, duty, "b.md"); n.State != StateUnchecked {
		t.Fatalf("a kept note = %+v", n)
	}
	// Someone changes a.md, so the waiting change is stale.
	if _, err := fr.s.Write(ctx, WriteRequest{Agent: "triage", Scope: duty, Name: "a.md", Text: "by bob", By: "person:bob"}); err != nil {
		t.Fatal(err)
	}
	if _, err := fr.s.Decide(ctx, list[1].ID, true, "person:alice"); !errors.Is(err, ErrLater) {
		t.Fatalf("a keep of a stale change = %v", err)
	}
	if _, err := fr.s.Decide(ctx, list[1].ID, false, "person:alice"); err != nil {
		t.Fatal(err)
	}
	// Not now puts a waiting new note away.
	res = fr.write(t, duty, "d.md", "new")
	if _, err := fr.s.Decide(ctx, res.Waiting.ID, false, "person:alice"); err != nil {
		t.Fatal(err)
	}
	if n := fr.note(t, duty, "d.md"); n.State != StatePutAway {
		t.Fatalf("not now = %+v", n)
	}
	if left, _ := fr.s.WaitingList(ctx, "triage", nil); len(left) != 0 {
		t.Fatalf("the waiting list = %+v", left)
	}
}

// fw.memory.used counts one use per event, also when two replicas or a
// second delivery see it, and a use wakes a resting note. A ref of the
// scope principal belongs to the principal of the run. Fourteen days
// without a use rest a note, and thirty put it away; both moves write the
// journal.
func TestUsesAndResting(t *testing.T) {
	fr := newFront(t, nil)
	ctx := context.Background()
	fr.write(t, duty, "a.md", "one")
	fr.write(t, duty, "b.md", "two")
	used := func(id string, refs ...string) fleet.RunEvent {
		var list []fw.MemoryUsedRef
		for _, r := range refs {
			list = append(list, fw.MemoryUsedRef{Ref: r, Hash: Hash("one")})
		}
		e := fw.MustEvent("01KRUNAAAAAAAAAAAAAAAAAAAA", fw.EvMemoryUsed, fw.MemoryUsedData{How: fw.MemoryUsedBrief, Refs: list})
		e.ID, e.Time = id, fr.clk.now()
		return fleet.RunEvent{Agent: "triage", Principal: "duty:nightly", Event: e}
	}
	ev := used("e1", "mem://principal/triage/a.md", "mem://principal/triage/missing.md", "mem://shared/x.md")
	fr.s.CountUse(ctx, ev)
	fr.s.CountUse(ctx, ev)
	if n := fr.note(t, duty, "a.md"); n.Uses != 1 || n.LastUsed == nil || !n.LastUsed.Equal(day0) {
		t.Fatalf("uses = %+v", n)
	}
	// Without the principal, the hash finds the note.
	noPrincipal := used("e2", "mem://principal/triage/a.md")
	noPrincipal.Principal = ""
	fr.s.CountUse(ctx, noPrincipal)
	if n := fr.note(t, duty, "a.md"); n.Uses != 2 {
		t.Fatalf("uses by hash = %d", n.Uses)
	}
	before := len(fr.journal(t))
	fr.clk.add(15 * 24 * time.Hour)
	if r, p, err := fr.s.Age(ctx); err != nil || r != 2 || p != 0 {
		t.Fatalf("Age = %d, %d, %v", r, p, err)
	}
	if fr.note(t, duty, "a.md").State != StateResting || len(fr.journal(t)) != before+2 || fr.journal(t)[0].What != WhatResting {
		t.Fatal("a note did not rest")
	}
	fr.s.CountUse(ctx, used("e3", "mem://principal/triage/a.md"))
	if n := fr.note(t, duty, "a.md"); n.State != StateUnchecked || n.Uses != 3 {
		t.Fatalf("a use did not wake the note: %+v", n)
	}
	fr.clk.add(16 * 24 * time.Hour)
	if _, p, err := fr.s.Age(ctx); err != nil || p != 1 {
		t.Fatalf("put away = %d, %v", p, err)
	}
	if fr.note(t, duty, "b.md").State != StatePutAway || fr.note(t, duty, "a.md").State != StateResting {
		t.Fatal("the pass did not put the old note away")
	}
}

// The count hangs on the egress stream that the fleet reads: an event
// fw.memory.used of a worker reaches the count through fleet.Tap with the
// principal of its run.
func TestUsesFromTheStream(t *testing.T) {
	fr := newFront(t, nil)
	fr.write(t, duty, "a.md", "one")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go fr.s.countUses(ctx)
	run := fw.NewID()
	rec := record(run, day0).queued("triage", "hello", "duty:nightly", nil).add(fw.EvMemoryUsed, fw.MemoryUsedData{How: fw.MemoryUsedBrief, Refs: []fw.MemoryUsedRef{{Ref: "mem://principal/triage/a.md"}}})
	for _, e := range rec.out {
		if err := fr.bus.Publish(ctx, "fw.events.team.default."+e.Type, e); err != nil {
			t.Fatal(err)
		}
	}
	deadline := time.Now().Add(5 * time.Second)
	for fr.note(t, duty, "a.md").Uses != 1 {
		if time.Now().After(deadline) {
			t.Fatal("the stream did not count the use")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// The seams as the other packages call them: a removed page has an empty
// hash, a Gitea item has a key such as lab/helpdesk/issue/42, and a new
// routine moves from no standing.
func TestSeamShapes(t *testing.T) {
	fr := newFront(t, nil)
	ctx := context.Background()
	fr.write(t, duty, "page.md", "The page says so.", Source{Ref: "page:runbook.md", Version: Hash("v1")})
	fr.write(t, duty, "issue.md", "Issue 42 waits.", Source{Ref: "item:gitea/lab/helpdesk/issue/42", Version: "2026-10-26T10:00:00Z"})
	PageWritten(ctx, "runbook.md", "")
	if n := fr.note(t, duty, "page.md"); n.State != StateOutOfDate {
		t.Fatalf("a removed page left the note %s", n.State)
	}
	if j := fr.journal(t); j[0].Why != "The page runbook.md was removed after the note was written." {
		t.Fatalf("why = %q", j[0].Why)
	}
	ItemChanged(ctx, "gitea", "lab/helpdesk/issue/42", "2026-10-27T10:00:00Z")
	if n := fr.note(t, duty, "issue.md"); n.State != StateOutOfDate {
		t.Fatalf("the item left the note %s", n.State)
	}
	RoutineMoved(ctx, "triage", "ack", "", "proposed", "alice", "")
	if j := fr.journal(t); j[0].What != WhatProposed || j[0].Undo != nil || j[0].Subject.Routine != "ack" {
		t.Fatalf("a new routine = %+v", j[0])
	}
}
