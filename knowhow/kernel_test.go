package knowhow

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/memory"
	"github.com/bdubs00/featherweight-workbench/pages"
)

// TestKernelNoteWithAPage drives the memory store of a kernel against this
// front (docs/KNOWHOW.md section 4.2): a note whose line "Source:
// page:<path>" names a page of the wiki reaches the front with the page
// and its hash as a source, so the note is checked and goes out of date
// when a person changes the page. With the switch "Ask me before it keeps
// what it learns" on, the front holds the next note of the agent, and the
// kernel records the change as waiting and tells the model so.
func TestKernelNoteWithAPage(t *testing.T) {
	fr, wiki, store := kernelFront(t)
	ctx := context.Background()
	const path = "launch/quillwork-3.md"
	page, err := wiki.Write(ctx, path, "# Quillwork 3\nLaunch day: 2026-11-12\n", "alice", "the launch", "")
	if err != nil {
		t.Fatal(err)
	}
	var changes []fw.MemoryChangedData
	run := fw.WithRunInfo(ctx, fw.RunInfo{RunID: fw.NewID(), Agent: "triage"})
	run = fw.WithMemoryRecorder(run, func(d fw.MemoryChangedData) { changes = append(changes, d) })
	scope := store.Scope(fw.RunInfoFrom(run))
	text := "Launch day is 2026-11-12.\nSource: page:" + path + "\n"
	if _, err := store.Remember(run, scope, fw.MemoryRecord{Ref: memory.NoteRef(scope, "launch.md"), Body: text}, ""); err != nil {
		t.Fatal(err)
	}
	n := fr.note(t, Scope{Kind: ScopeAgent}, "launch.md")
	if n.State != StateChecked || n.By != ByAgent || len(n.Sources) != 1 || n.Sources[0] != (Source{Ref: "page:" + path, Version: page.Hash}) {
		t.Fatalf("the note at the front = %+v", n)
	}

	// A person changes the page, and the note goes out of date.
	if _, err := wiki.Write(ctx, path, "# Quillwork 3\nLaunch day: 2026-11-13\n", "alice", "a new day", page.Hash); err != nil {
		t.Fatal(err)
	}
	if n := fr.note(t, Scope{Kind: ScopeAgent}, "launch.md"); n.State != StateOutOfDate || n.OutOfDate == nil || n.OutOfDate.Source != "page:"+path {
		t.Fatalf("the note after the page changed = %+v", n)
	}

	// Ask first: the next note of the agent waits for a person.
	if _, err := fr.s.SetAskFirst(ctx, "triage", true, "person:alice"); err != nil {
		t.Fatal(err)
	}
	changes = nil
	if _, err := store.Remember(run, scope, fw.MemoryRecord{Ref: memory.NoteRef(scope, "webinar.md"), Body: "The webinar is on 2026-11-19.\n"}, ""); err != nil {
		t.Fatal(err)
	}
	if len(changes) != 1 || !changes[0].Waiting {
		t.Fatalf("changes = %+v", changes)
	}
	if text, replace := memory.Answer(changes, true); !replace || !strings.Contains(text, "waits for a person") {
		t.Fatalf("the answer to the model = %q", text)
	}
	if n := fr.note(t, Scope{Kind: ScopeAgent}, "webinar.md"); n.State != StateWaiting {
		t.Fatalf("the note under ask first = %+v", n)
	}
}

// kernelFront returns this front with a wiki, and the memory store of a
// kernel whose notes the front keeps, with the static token.
func kernelFront(t *testing.T) (*front, *pages.Store, *memory.Store) {
	t.Helper()
	fr := newFront(t, nil)
	wiki, closeWiki, err := pages.Open(context.Background(), fr.a.Config(), quiet())
	if err != nil {
		t.Fatal(err)
	}
	pages.Attach(fr.a, wiki)
	t.Cleanup(func() {
		pages.Attach(fr.a, nil)
		if closeWiki != nil {
			closeWiki()
		}
	})
	// The routes of the wiki mount when the handler is built, so this
	// front serves a handler built after the wiki is attached.
	h, err := fr.a.Handler()
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	t.Setenv("FW_TEST_FRONT_TOKEN", "fleet")
	cfg := fw.DefaultConfig()
	cfg.Home = t.TempDir()
	cfg.Tools.Memory.Front = &fw.MemoryFrontConfig{URL: srv.URL, TokenEnv: "FW_TEST_FRONT_TOKEN"}
	store, err := memory.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return fr, wiki, store
}

// TestKernelNoteNamesItsRun drives the memory tool of a kernel against
// this front with the note that Haiku wrote on the stack. Its line
// "Source: wiki page <path>" names the page in a loose form, and the note
// reaches the front with the page and its hash, because the wiki has the
// page. Each entry of the journal that the run caused links the run: the
// new note, a note that waited for a person and that a person kept, and a
// note that the agent put away.
func TestKernelNoteNamesItsRun(t *testing.T) {
	fr, wiki, store := kernelFront(t)
	ctx := context.Background()
	const path = "launch/quillwork-3.md"
	page, err := wiki.Write(ctx, path, "# Quillwork 3\nLaunch day: 2026-11-12\n", "alice", "the launch", "")
	if err != nil {
		t.Fatal(err)
	}
	id := fw.NewID()
	run := fw.WithRunInfo(ctx, fw.RunInfo{RunID: id, Agent: "triage"})
	scope := store.Scope(fw.RunInfoFrom(run))
	haiku := "Quillwork 3.0 launch day and webinar date (from wiki page).\nLaunch day: 2026-11-12. Webinar: 2026-11-19.\nSource: wiki page launch/quillwork-3.md\n"
	if _, err := store.Remember(run, scope, fw.MemoryRecord{Ref: memory.NoteRef(scope, "launch-dates.md"), Body: haiku}, ""); err != nil {
		t.Fatal(err)
	}
	n := fr.note(t, Scope{Kind: ScopeAgent}, "launch-dates.md")
	if n.State != StateChecked || n.Run != id || len(n.Sources) != 1 || n.Sources[0] != (Source{Ref: "page:" + path, Version: page.Hash}) {
		t.Fatalf("the note at the front = %+v", n)
	}
	entryOf := func(what, note string) Entry {
		t.Helper()
		for _, e := range fr.journal(t) {
			if e.What == what && e.Subject.Note == note {
				return e
			}
		}
		t.Fatalf("no entry %s of %s in %+v", what, note, fr.journal(t))
		return Entry{}
	}
	if e := entryOf(WhatLearned, "launch-dates.md"); len(e.Runs) != 1 || e.Runs[0] != id {
		t.Fatalf("the entry of the new note = %+v", e)
	}

	// A note that waits for a person, and that a person keeps.
	if _, err := fr.s.SetAskFirst(ctx, "triage", true, "person:alice"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Remember(run, scope, fw.MemoryRecord{Ref: memory.NoteRef(scope, "webinar.md"), Body: "The webinar is on 2026-11-19.\n"}, ""); err != nil {
		t.Fatal(err)
	}
	list, err := fr.s.WaitingList(ctx, "triage", nil)
	if err != nil || len(list) != 1 {
		t.Fatalf("waiting = %+v, %v", list, err)
	}
	kept, err := fr.s.Decide(ctx, list[0].ID, true, "person:alice")
	if err != nil || kept == nil || len(kept.Runs) != 1 || kept.Runs[0] != id {
		t.Fatalf("the entry of the keep = %+v, %v", kept, err)
	}

	// The agent puts a note away.
	if _, err := fr.s.SetAskFirst(ctx, "triage", false, "person:alice"); err != nil {
		t.Fatal(err)
	}
	if err := store.Forget(run, scope, memory.NoteRef(scope, "launch-dates.md")); err != nil {
		t.Fatal(err)
	}
	if e := entryOf(WhatPutAway, "launch-dates.md"); len(e.Runs) != 1 || e.Runs[0] != id {
		t.Fatalf("the entry of the put-away note = %+v", e)
	}
}

// TestStaticTokenWritesAsAPerson drives the memory tool of a kernel and
// the page against this front with the static token. The kernel names its
// writer, so a note of its run stays a note of the agent. The page names
// none, so an owner who corrects a note that went out of date, signed in
// with the static token, writes as a person: the note names the page at
// its version now, and the journal says that a person changed it. A later
// edit takes the version of the page now as well, and so do a put-away
// and the switch: each records the person.
func TestStaticTokenWritesAsAPerson(t *testing.T) {
	fr, wiki, store := kernelFront(t)
	ctx := context.Background()
	const path = "lab/backup.md"
	page, err := wiki.Write(ctx, path, "# Backup\nThe backup server listens on port 7341.\n", "alice", "the port", "")
	if err != nil {
		t.Fatal(err)
	}
	run := fw.WithRunInfo(ctx, fw.RunInfo{RunID: fw.NewID(), Agent: "triage"})
	scope := store.Scope(fw.RunInfoFrom(run))
	if _, err := store.Remember(run, scope, fw.MemoryRecord{Ref: memory.NoteRef(scope, "port.md"), Body: "The backup server listens on port 7341.\nSource: page:" + path + "\n"}, ""); err != nil {
		t.Fatal(err)
	}
	if n := fr.note(t, Scope{Kind: ScopeAgent}, "port.md"); n.By != ByAgent || n.Sources[0].Version != page.Hash {
		t.Fatalf("the note of the run = %+v", n)
	}
	moved, err := wiki.Write(ctx, path, "# Backup\nThe backup server listens on port 7342.\n", "alice", "a new port", page.Hash)
	if err != nil {
		t.Fatal(err)
	}
	n := fr.note(t, Scope{Kind: ScopeAgent}, "port.md")
	if n.State != StateOutOfDate {
		t.Fatalf("the note after the page changed = %+v", n)
	}

	// The page sends the text, the hash, and the sources as they were.
	base := fr.url + "/v1/knowhow/notes/triage/agent/port.md"
	resp, body := send(t, "PUT", base, "fleet", fmt.Sprintf(`{"text": "The backup server listens on port 7342.\nSource: page:%s\n", "ifHash": %q, "sources": [{"ref": "page:%s", "version": %q}]}`, path, n.Hash, path, page.Hash))
	mustStatus(t, resp, body, http.StatusOK)
	n = fr.note(t, Scope{Kind: ScopeAgent}, "port.md")
	if n.By != "person:token" || n.Run != "" || n.State != StateChecked || n.OutOfDate != nil || len(n.Sources) != 1 || n.Sources[0].Version != moved.Hash {
		t.Fatalf("the note after the edit of the owner = %+v", n)
	}
	if e := fr.journal(t)[0]; e.What != WhatUpdated || e.By != "person:token" || e.Why != "A person changed this note." || !e.Corrects {
		t.Fatalf("the entry of the edit = %+v", e)
	}

	// The note of a person never goes out of date, so a later edit takes
	// the version of the page now, not the version that the note names.
	again, err := wiki.Write(ctx, path, "# Backup\nThe backup server listens on port 7342. It runs at 02:00.\n", "alice", "the hour", moved.Hash)
	if err != nil {
		t.Fatal(err)
	}
	n = fr.note(t, Scope{Kind: ScopeAgent}, "port.md")
	resp, body = send(t, "PUT", base, "fleet", fmt.Sprintf(`{"text": "The backup server listens on port 7342 and runs at 02:00.\n", "ifHash": %q, "sources": [{"ref": "page:%s", "version": %q}]}`, n.Hash, path, moved.Hash))
	mustStatus(t, resp, body, http.StatusOK)
	if n = fr.note(t, Scope{Kind: ScopeAgent}, "port.md"); n.Sources[0].Version != again.Hash || n.State != StateChecked {
		t.Fatalf("the note after the second edit = %+v", n)
	}

	// The switch records the person, and so does a put-away of the page.
	resp, body = send(t, "PUT", fr.url+"/v1/knowhow/settings/triage", "fleet", `{"askFirst": true}`)
	mustStatus(t, resp, body, http.StatusOK)
	if !strings.Contains(body, `"by":"person:token"`) {
		t.Fatalf("the switch = %s", body)
	}
	resp, body = send(t, "DELETE", base+"?ifHash="+n.Hash, "fleet", "")
	mustStatus(t, resp, body, http.StatusOK)
	if e := fr.journal(t)[0]; e.What != WhatPutAway || e.By != "person:token" || e.Why != "A person put this note away." {
		t.Fatalf("the entry of the put-away = %+v", e)
	}

	// The kernel still writes as the agent, and with the switch on its
	// change waits for a person.
	if _, err := store.Remember(run, scope, fw.MemoryRecord{Ref: memory.NoteRef(scope, "hour.md"), Body: "The backup runs at 02:00.\n"}, ""); err != nil {
		t.Fatal(err)
	}
	if n := fr.note(t, Scope{Kind: ScopeAgent}, "hour.md"); n.By != ByAgent || n.State != StateWaiting {
		t.Fatalf("the note of the run under the switch = %+v", n)
	}
}
