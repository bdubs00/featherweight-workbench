package knowhow

import (
	"context"
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
	fr := newFront(t, nil)
	ctx := context.Background()
	wiki, closeWiki, err := pages.Open(ctx, fr.a.Config(), quiet())
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
	const path = "launch/quillwork-3.md"
	page, err := wiki.Write(ctx, path, "# Quillwork 3\nLaunch day: 2026-11-12\n", "alice", "the launch", "")
	if err != nil {
		t.Fatal(err)
	}

	// A kernel whose notes this front keeps, with the static token.
	t.Setenv("FW_TEST_FRONT_TOKEN", "fleet")
	cfg := fw.DefaultConfig()
	cfg.Home = t.TempDir()
	cfg.Tools.Memory.Front = &fw.MemoryFrontConfig{URL: srv.URL, TokenEnv: "FW_TEST_FRONT_TOKEN"}
	store, err := memory.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
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
