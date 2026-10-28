package knowhow

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// The kernel of a worker keeps a note with the static token: a new note
// answers 201 with its hash in ETag, a push with the hash that it read
// changes it, and a push with another hash answers 412 with the hash of
// the note as it is.
func TestRoutesWorkerPush(t *testing.T) {
	fr := newFront(t, nil)
	base := fr.url + "/v1/knowhow/notes/triage/principal/gotchas/labels.md?principal=" + url.QueryEscape("duty:nightly")
	resp, body := send(t, "PUT", base, "fleet", `{"text": "Gitea refuses a label with a space.", "ifHash": "absent", "by": "agent", "run": "`+fw.NewID()+`", "sources": [{"ref": "item:gitea/issue/42", "version": "2026-10-26T10:00:00Z"}]}`)
	mustStatus(t, resp, body, http.StatusCreated)
	var w writeAnswer
	decodeJSON(t, body, &w)
	if w.Note.Hash != Hash("Gitea refuses a label with a space.") || w.Note.State != StateChecked || resp.Header.Get("ETag") != `"`+w.Note.Hash+`"` || w.Note.Text != "" {
		t.Fatalf("the answer = %+v, %v", w, resp.Header)
	}
	resp, body = send(t, "PUT", base, "fleet", `{"text": "two", "ifHash": "`+Hash("old")+`"}`)
	mustStatus(t, resp, body, http.StatusPreconditionFailed)
	var p struct {
		Hash string `json:"hash"`
	}
	decodeJSON(t, body, &p)
	if p.Hash != w.Note.Hash || resp.Header.Get("ETag") != `"`+w.Note.Hash+`"` || !strings.Contains(body, "This note changed in another run") {
		t.Fatalf("412 = %s, %v", body, resp.Header)
	}
	resp, body = send(t, "PUT", base, "fleet", `{"text": "two", "ifHash": "`+w.Note.Hash+`", "by": "boss"}`)
	mustStatus(t, resp, body, http.StatusBadRequest)
	resp, body = send(t, "PUT", base, "fleet", `{"text": "two", "ifHash": "`+w.Note.Hash+`"}`)
	mustStatus(t, resp, body, http.StatusOK)
	// The pull lists the scope with its states and hashes, without text.
	resp, body = send(t, "GET", fr.url+"/v1/knowhow/notes?agent=triage&scope=principal&principal=duty:nightly", "fleet", "")
	mustStatus(t, resp, body, http.StatusOK)
	var list struct {
		Notes []struct {
			Note
			SourcesCount int `json:"sourcesCount"`
		} `json:"notes"`
	}
	decodeJSON(t, body, &list)
	if len(list.Notes) != 1 || list.Notes[0].Hash != Hash("two") || list.Notes[0].Text != "" || list.Notes[0].SourcesCount != 0 {
		t.Fatalf("list = %s", body)
	}
	resp, body = send(t, "GET", base, "fleet", "")
	mustStatus(t, resp, body, http.StatusOK)
	if !strings.Contains(body, `"text":"two"`) {
		t.Fatalf("get = %s", body)
	}
	// The kernel deletes the note: the front puts it away, and the list
	// leaves it out unless putAway is true.
	resp, body = send(t, "DELETE", base+"&ifHash="+Hash("two"), "fleet", "")
	mustStatus(t, resp, body, http.StatusOK)
	_, body = send(t, "GET", fr.url+"/v1/knowhow/notes?agent=triage&scope=principal&principal=duty:nightly", "fleet", "")
	if strings.Contains(body, "labels.md") {
		t.Fatalf("a put-away note is listed: %s", body)
	}
	_, body = send(t, "GET", fr.url+"/v1/knowhow/notes?agent=triage&scope=principal&principal=duty:nightly&putAway=true", "fleet", "")
	if !strings.Contains(body, `"state":"put-away"`) {
		t.Fatalf("putAway=true = %s", body)
	}
	// With the switch on, a push of the agent waits: 202.
	if _, err := fr.s.SetAskFirst(context.Background(), "triage", true, "person:alice"); err != nil {
		t.Fatal(err)
	}
	resp, body = send(t, "PUT", fr.url+"/v1/knowhow/notes/triage/principal/new.md?principal=duty:nightly", "fleet", `{"text": "new", "ifHash": "absent"}`)
	mustStatus(t, resp, body, http.StatusAccepted)
	if !strings.Contains(body, `"waiting"`) || !strings.Contains(body, `"state":"waiting"`) {
		t.Fatalf("202 = %s", body)
	}
}

// A person reads the notes of the agent and of their own principal; an
// operator also reads the notes of a Duty. A person changes their own
// notes with the operator role, and the notes of the agent or a Duty with
// the owner role. Nobody reads the notes of another person, and a caller
// who cannot view the agent gets 404.
func TestRoutesPeople(t *testing.T) {
	fr := newFront(t, nil)
	fr.write(t, Scope{Kind: ScopeAgent}, "agent.md", "of the agent")
	fr.write(t, duty, "duty.md", "of the duty")
	fr.write(t, Scope{Kind: ScopePrincipal, Principal: "bob"}, "bob.md", "of bob")
	fr.write(t, Scope{Kind: ScopePrincipal, Principal: "carol"}, "carol.md", "of carol")
	names := func(who string) string {
		resp, body := send(t, "GET", fr.url+"/v1/knowhow/notes?agent=triage", who, "")
		mustStatus(t, resp, body, http.StatusOK)
		var list struct {
			Notes []Note `json:"notes"`
		}
		decodeJSON(t, body, &list)
		var out []string
		for _, n := range list.Notes {
			out = append(out, n.Name)
		}
		return strings.Join(out, ",")
	}
	for who, want := range map[string]string{
		"alice": "agent.md,duty.md",
		"bob":   "agent.md,bob.md,duty.md",
		"carol": "agent.md,carol.md",
		"fleet": "agent.md,bob.md,carol.md,duty.md",
	} {
		if got := names(who); got != want {
			t.Errorf("%s reads %s, want %s", who, got, want)
		}
	}
	resp, body := send(t, "GET", fr.url+"/v1/knowhow/notes?agent=triage", "dave", "")
	mustStatus(t, resp, body, http.StatusNotFound)
	resp, body = send(t, "GET", fr.url+"/v1/knowhow/notes/triage/principal/bob.md?principal=bob", "alice", "")
	mustStatus(t, resp, body, http.StatusNotFound)
	resp, body = send(t, "GET", fr.url+"/v1/knowhow/notes/triage/principal/duty.md?principal=duty:nightly", "carol", "")
	mustStatus(t, resp, body, http.StatusNotFound)
	// bob, an operator, writes his own note, and it records him.
	resp, body = send(t, "PUT", fr.url+"/v1/knowhow/notes/triage/principal/bob.md", "bob", `{"text": "bob again", "by": "agent", "run": "x"}`)
	mustStatus(t, resp, body, http.StatusOK)
	if n := fr.note(t, Scope{Kind: ScopePrincipal, Principal: "bob"}, "bob.md"); n.By != "person:bob" || n.Run != "" {
		t.Fatalf("a note of a person records %q %q", n.By, n.Run)
	}
	// carol only views: her own notes need the operator role.
	resp, body = send(t, "PUT", fr.url+"/v1/knowhow/notes/triage/principal/carol.md", "carol", `{"text": "carol again"}`)
	mustStatus(t, resp, body, http.StatusForbidden)
	// bob cannot change the notes of the agent or of a Duty; alice can.
	resp, body = send(t, "PUT", fr.url+"/v1/knowhow/notes/triage/agent/agent.md", "bob", `{"text": "x"}`)
	mustStatus(t, resp, body, http.StatusForbidden)
	resp, body = send(t, "DELETE", fr.url+"/v1/knowhow/notes/triage/principal/duty.md?principal=duty:nightly", "bob", "")
	mustStatus(t, resp, body, http.StatusForbidden)
	resp, body = send(t, "PUT", fr.url+"/v1/knowhow/notes/triage/agent/agent.md", "alice", `{"text": "by alice"}`)
	mustStatus(t, resp, body, http.StatusOK)
	// Now the agent cannot change the note of a person.
	resp, body = send(t, "PUT", fr.url+"/v1/knowhow/notes/triage/agent/agent.md", "fleet", `{"text": "by the agent"}`)
	mustStatus(t, resp, body, http.StatusForbidden)
	// A bad scope and a bad name answer 400.
	resp, body = send(t, "PUT", fr.url+"/v1/knowhow/notes/triage/shared/a.md", "fleet", `{"text": "x"}`)
	mustStatus(t, resp, body, http.StatusBadRequest)
	resp, body = send(t, "PUT", fr.url+"/v1/knowhow/notes/triage/agent/.hidden.md", "fleet", `{"text": "x"}`)
	mustStatus(t, resp, body, http.StatusBadRequest)
}

// The journal shows a person the entries that they can read, an undo by a
// person answers 200 and a stale undo 409, the switch needs the owner,
// and a waiting change takes a decision of a person.
func TestRoutesJournalSettingsWaiting(t *testing.T) {
	fr := newFront(t, nil)
	fr.write(t, duty, "a.md", "one")
	fr.write(t, duty, "a.md", "two")
	fr.write(t, Scope{Kind: ScopePrincipal, Principal: "bob"}, "b.md", "of bob")
	journal := func(who string) []Entry {
		resp, body := send(t, "GET", fr.url+"/v1/knowhow/journal?agent=triage", who, "")
		mustStatus(t, resp, body, http.StatusOK)
		var out struct {
			Entries []Entry `json:"entries"`
		}
		decodeJSON(t, body, &out)
		return out.Entries
	}
	if j := journal("alice"); len(j) != 2 || j[0].Subject.Note != "a.md" {
		t.Fatalf("alice reads %+v", j)
	}
	if j := journal("bob"); len(j) != 3 {
		t.Fatalf("bob reads %d entries", len(j))
	}
	if j := journal("carol"); len(j) != 0 {
		t.Fatalf("carol reads %d entries", len(j))
	}
	j := journal("alice")
	resp, body := send(t, "POST", fr.url+"/v1/knowhow/journal/"+j[1].ID+"/undo", "alice", "")
	mustStatus(t, resp, body, http.StatusConflict)
	resp, body = send(t, "POST", fr.url+"/v1/knowhow/journal/"+j[0].ID+"/undo", "carol", "")
	mustStatus(t, resp, body, http.StatusNotFound)
	resp, body = send(t, "POST", fr.url+"/v1/knowhow/journal/"+j[0].ID+"/undo", "bob", "")
	mustStatus(t, resp, body, http.StatusForbidden)
	resp, body = send(t, "POST", fr.url+"/v1/knowhow/journal/"+j[0].ID+"/undo", "alice", "")
	mustStatus(t, resp, body, http.StatusOK)
	if n := fr.note(t, duty, "a.md"); n.Text != "one" {
		t.Fatalf("after the undo = %q", n.Text)
	}
	// The switch.
	resp, body = send(t, "GET", fr.url+"/v1/knowhow/settings/triage", "carol", "")
	mustStatus(t, resp, body, http.StatusOK)
	if !strings.Contains(body, `"askFirst":false`) || !strings.Contains(body, ReviewOff) {
		t.Fatalf("settings = %s", body)
	}
	resp, body = send(t, "PUT", fr.url+"/v1/knowhow/settings/triage", "bob", `{"askFirst": true}`)
	mustStatus(t, resp, body, http.StatusForbidden)
	resp, body = send(t, "PUT", fr.url+"/v1/knowhow/settings/triage", "alice", `{"askFirst": true}`)
	mustStatus(t, resp, body, http.StatusOK)
	if !strings.Contains(body, `"askFirst":true`) || !strings.Contains(body, `"by":"person:alice"`) {
		t.Fatalf("settings = %s", body)
	}
	// A change of the agent waits, and alice keeps it.
	fr.write(t, duty, "a.md", "three")
	resp, body = send(t, "GET", fr.url+"/v1/knowhow/waiting?agent=triage", "alice", "")
	mustStatus(t, resp, body, http.StatusOK)
	var waiting struct {
		Waiting []Waiting `json:"waiting"`
	}
	decodeJSON(t, body, &waiting)
	if len(waiting.Waiting) != 1 || waiting.Waiting[0].Text != "three" {
		t.Fatalf("waiting = %s", body)
	}
	resp, body = send(t, "POST", fr.url+"/v1/knowhow/waiting/"+waiting.Waiting[0].ID, "bob", `{"keep": true}`)
	mustStatus(t, resp, body, http.StatusForbidden)
	resp, body = send(t, "POST", fr.url+"/v1/knowhow/waiting/"+waiting.Waiting[0].ID, "alice", `{"keep": true}`)
	mustStatus(t, resp, body, http.StatusOK)
	if n := fr.note(t, duty, "a.md"); n.Text != "three" {
		t.Fatalf("after the keep = %q", n.Text)
	}
	// A change that went stale answers 409.
	fr.write(t, duty, "a.md", "four")
	if _, err := fr.s.Write(context.Background(), WriteRequest{Agent: "triage", Scope: duty, Name: "a.md", Text: "by alice", By: "person:alice"}); err != nil {
		t.Fatal(err)
	}
	_, body = send(t, "GET", fr.url+"/v1/knowhow/waiting?agent=triage", "alice", "")
	decodeJSON(t, body, &waiting)
	resp, body = send(t, "POST", fr.url+"/v1/knowhow/waiting/"+waiting.Waiting[0].ID, "alice", `{"keep": true}`)
	mustStatus(t, resp, body, http.StatusConflict)
}

// GET /v1/knowhow/runs reads the archive for one agent and one principal,
// with an exact match of the principal, and cuts each field to 2 KB.
func TestRoutesRuns(t *testing.T) {
	fr := newFront(t, nil)
	long := strings.Repeat("x", 3000)
	for i, p := range []string{"duty:nightly", "duty:nightly-2", "bob", "duty:nightly"} {
		run := fmt.Sprintf("01KRUN%020d", i)
		rec := record(run, day0.Add(time.Duration(i)*time.Minute)).queued("triage", fmt.Sprintf("ticket %d about labels", i), p, nil).started("").finished(long, 0.01)
		fr.arch.put(rec.out, day0)
	}
	fr.arch.put(record("01KOTHER0000000000000000000", day0).queued("private", "labels", "duty:nightly", nil).finished("x", 0).out, day0)
	resp, body := send(t, "GET", fr.url+"/v1/knowhow/runs?agent=triage&principal=duty:nightly&q=labels", "fleet", "")
	mustStatus(t, resp, body, http.StatusOK)
	var out struct {
		Runs []RunHit `json:"runs"`
	}
	decodeJSON(t, body, &out)
	if len(out.Runs) != 2 || out.Runs[0].ID != fmt.Sprintf("01KRUN%020d", 3) || len(out.Runs[0].Output) > runFieldCut+3 || !strings.HasSuffix(out.Runs[0].Output, "…") || out.Runs[0].Outcome != "completed" {
		t.Fatalf("runs = %+v", out.Runs)
	}
	resp, body = send(t, "GET", fr.url+"/v1/knowhow/runs?agent=triage&principal=duty:nightly&q=ticket+3", "fleet", "")
	decodeJSON(t, body, &out)
	if len(out.Runs) != 1 {
		t.Fatalf("q = %s", body)
	}
	// bob reads his own runs without naming himself, and not those of
	// another principal; alice, an owner, reads those of a Duty.
	resp, body = send(t, "GET", fr.url+"/v1/knowhow/runs?agent=triage", "bob", "")
	decodeJSON(t, body, &out)
	if resp.StatusCode != http.StatusOK || len(out.Runs) != 1 {
		t.Fatalf("bob = %d %s", resp.StatusCode, body)
	}
	resp, body = send(t, "GET", fr.url+"/v1/knowhow/runs?agent=triage&principal=bob", "carol", "")
	mustStatus(t, resp, body, http.StatusNotFound)
	resp, body = send(t, "GET", fr.url+"/v1/knowhow/runs?agent=triage&principal=duty:nightly", "alice", "")
	mustStatus(t, resp, body, http.StatusOK)
	resp, body = send(t, "GET", fr.url+"/v1/knowhow/runs?agent=triage", "fleet", "")
	mustStatus(t, resp, body, http.StatusBadRequest)
}

// The summary gives the four numbers, split into normal and held-back
// runs, and no percentage from fewer than 20 held-back runs.
func TestSummary(t *testing.T) {
	fr := newFront(t, nil)
	fr.clk.add(7 * 24 * time.Hour)
	put := func(i int, held, well, byRoutine bool, at time.Time, cost float64) {
		meta := map[string]string{fw.MetaDuty: "nightly"}
		if held {
			meta[fw.MetaKnowhow] = fw.KnowhowOff
		}
		run := fmt.Sprintf("01KSUM%020d", i)
		rec := record(run, at).queued("triage", "ticket", "duty:nightly", meta).started("")
		if byRoutine {
			rec.add(fw.EvRoutineApplied, fw.RoutineAppliedData{Routine: "duty-nightly", OK: true})
		}
		rec.finished("done", cost)
		if !well {
			rec.receipt("rejected")
		}
		fr.arch.put(rec.out, at)
	}
	now := fr.clk.now()
	// The week before: 10 runs at 0.20.
	for i := 0; i < 10; i++ {
		put(100+i, false, true, false, now.Add(-10*24*time.Hour), 0.20)
	}
	// This week: 30 normal runs at 0.10, 27 done well, 6 by a routine,
	// and 5 held-back runs at 0.30, 3 done well.
	for i := 0; i < 30; i++ {
		put(i, false, i >= 3, i < 6, now.Add(-time.Duration(i)*time.Hour), 0.10)
	}
	for i := 0; i < 5; i++ {
		put(50+i, true, i >= 2, false, now.Add(-time.Duration(i)*time.Hour), 0.30)
	}
	// A correction in the journal.
	fr.write(t, duty, "a.md", "one", Source{Ref: "page:p.md", Version: "v1"})
	fr.s.PageWritten(context.Background(), "p.md", "v2")
	fr.write(t, duty, "a.md", "two", Source{Ref: "page:p.md", Version: "v2"})

	resp, body := send(t, "GET", fr.url+"/v1/knowhow/summary?agent=triage&days=7", "carol", "")
	mustStatus(t, resp, body, http.StatusOK)
	var s Summary
	decodeJSON(t, body, &s)
	if s.Normal.Runs != 30 || s.Normal.DoneWell != 27 || s.Normal.ByRoutine != 6 || s.HeldBack.Runs != 5 || s.HeldBack.DoneWell != 3 {
		t.Fatalf("splits = %+v %+v", s.Normal, s.HeldBack)
	}
	if s.DoneByRoutines.Share != 0.2 || s.NotesCorrected != 1 || s.CostPerJob.Trend != "down" || s.CostPerJob.PreviousUSD == nil || *s.CostPerJob.PreviousUSD < 0.199 {
		t.Fatalf("numbers = %+v %+v %d", s.DoneByRoutines, s.CostPerJob, s.NotesCorrected)
	}
	if s.Compare.Enough || s.Compare.With != nil || s.Compare.Without != nil || s.Compare.Line != NotEnough || strings.Contains(body, "%") {
		t.Fatalf("a comparison from 5 held-back runs = %+v", s.Compare)
	}
	// With 20 held-back runs, the line gives the shares.
	for i := 5; i < 20; i++ {
		put(50+i, true, true, false, now.Add(-time.Duration(i)*time.Hour), 0.30)
	}
	out, err := fr.s.SummaryOf(context.Background(), "triage", 7)
	if err != nil {
		t.Fatal(err)
	}
	if !out.Compare.Enough || out.Compare.Line != "Held back 1 in 3 runs. Done well: 90% without what it learned, 90% with it." {
		t.Fatalf("the comparison = %+v", out.Compare)
	}
	resp, body = send(t, "GET", fr.url+"/v1/knowhow/summary?agent=triage&days=99", "carol", "")
	mustStatus(t, resp, body, http.StatusBadRequest)
	resp, body = send(t, "GET", fr.url+"/v1/knowhow/summary?agent=private", "carol", "")
	mustStatus(t, resp, body, http.StatusNotFound)
}
