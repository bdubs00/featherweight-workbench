package pages

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-workbench/internal/bucket"
	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// memStore returns a wiki in memory with the limits of c.
func memStore(c Config) *Store {
	if c.History == 0 {
		c.History = DefaultHistory
	}
	return New(bucket.NewMem(bucketConfig(withDefaults(c))), c, quiet())
}

func withDefaults(c Config) Config {
	if c.MaxPageBytes == 0 {
		c.MaxPageBytes = DefaultMaxPageBytes
	}
	if c.MaxPages == 0 {
		c.MaxPages = DefaultMaxPages
	}
	if c.History == 0 {
		c.History = DefaultHistory
	}
	return c
}

func mustWrite(t *testing.T, s *Store, p, text, ifHash string) Page {
	t.Helper()
	page, err := s.Write(context.Background(), p, text, "alice", "a change", ifHash)
	if err != nil {
		t.Fatalf("Write %s: %v", p, err)
	}
	return page
}

func TestWriteReadAndVersions(t *testing.T) {
	ctx := context.Background()
	s := memStore(Config{})
	at := time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return at }
	text := "---\ntitle: The plan\ntags: [migration, puppet]\n---\n# Ignored heading\n\nMove the modules.\n"
	p1, err := s.WriteAs(ctx, "notes/plan.md", text, "agent:migrator", "r-1", "first draft", IfAbsent)
	if err != nil {
		t.Fatal(err)
	}
	if p1.Hash != Hash(text) || len(p1.Hash) != 64 || p1.Title != "The plan" || strings.Join(p1.Tags, ",") != "migration,puppet" {
		t.Fatalf("the page = %+v", p1.PageInfo)
	}
	if p1.By != "agent:migrator" || p1.Run != "r-1" || p1.Summary != "first draft" || !p1.At.Equal(at) || p1.Size != len(text) {
		t.Fatalf("the record = %+v", p1.PageInfo)
	}
	got, err := s.Read(ctx, "notes/plan.md", 0)
	if err != nil || got.Text != text || got.Version != p1.Version {
		t.Fatalf("Read = %+v, %v", got, err)
	}
	p2 := mustWrite(t, s, "notes/plan.md", "# Plan two\n", p1.Hash)
	if p2.Version <= p1.Version || p2.Title != "Plan two" {
		t.Fatalf("the second version = %+v", p2.PageInfo)
	}
	old, err := s.Read(ctx, "notes/plan.md", p1.Version)
	if err != nil || old.Text != text {
		t.Fatalf("Read of version %d = %+v, %v", p1.Version, old, err)
	}
	if _, err := s.Read(ctx, "notes/plan.md", p2.Version+100); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Read of a version that does not exist = %v", err)
	}
	h, err := s.History(ctx, "notes/plan.md")
	if err != nil || len(h) != 2 || h[0].Version != p2.Version || h[1].Summary != "first draft" || h[1].Run != "r-1" {
		t.Fatalf("History = %+v, %v", h, err)
	}
	// The text that the page holds already changes nothing.
	same := mustWrite(t, s, "notes/plan.md", "# Plan two\n", IfAny)
	if same.Version != p2.Version {
		t.Fatalf("a write of the same text made version %d", same.Version)
	}
	if h, _ := s.History(ctx, "notes/plan.md"); len(h) != 2 {
		t.Fatalf("History after the same text = %d versions", len(h))
	}
}

func TestConditions(t *testing.T) {
	ctx := context.Background()
	s := memStore(Config{})
	if _, err := s.Write(ctx, "a.md", "one", "alice", "", IfExists); !errors.Is(err, ErrConflict) {
		t.Fatalf("IfExists on a new page = %v", err)
	}
	if _, err := s.Write(ctx, "a.md", "one", "alice", "", "0123"); !errors.Is(err, ErrConflict) {
		t.Fatalf("a hash on a new page = %v", err)
	}
	p := mustWrite(t, s, "a.md", "one", IfAbsent)
	_, err := s.Write(ctx, "a.md", "two", "alice", "", IfAbsent)
	var e *Error
	if !errors.As(err, &e) || !errors.Is(err, ErrConflict) || e.Hash != p.Hash {
		t.Fatalf("IfAbsent on a page that exists = %v", err)
	}
	if _, err := s.Write(ctx, "a.md", "two", "alice", "", Hash("stale")); !errors.As(err, &e) || e.Hash != p.Hash || !strings.Contains(e.Reason, "read it again") {
		t.Fatalf("a stale hash = %v", err)
	}
	// A hash in upper case matches.
	mustWrite(t, s, "a.md", "two", strings.ToUpper(p.Hash))
	mustWrite(t, s, "a.md", "three", IfExists)
	if err := s.Delete(ctx, "a.md", "alice", p.Hash); !errors.Is(err, ErrConflict) {
		t.Fatalf("Delete with a stale hash = %v", err)
	}
	if err := s.Delete(ctx, "a.md", "alice", IfAbsent); !errors.Is(err, ErrInvalid) {
		t.Fatalf("Delete with IfAbsent = %v", err)
	}
	if err := s.Delete(ctx, "a.md", "alice", Hash("three")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Read(ctx, "a.md", 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Read after Delete = %v", err)
	}
	if err := s.Delete(ctx, "a.md", "alice", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a second Delete = %v", err)
	}
	h, err := s.History(ctx, "a.md")
	if err != nil || len(h) != 4 || !h[0].Deleted {
		t.Fatalf("History after Delete = %+v, %v", h, err)
	}
	// A removed page takes a new text as a new page.
	mustWrite(t, s, "a.md", "four", IfAbsent)
}

// Many writers with the hash that they all read: exactly one wins.
func TestCompareAndSwapRace(t *testing.T) {
	ctx := context.Background()
	s := memStore(Config{})
	base := mustWrite(t, s, "race.md", "base", IfAny)
	var wg sync.WaitGroup
	var mu sync.Mutex
	wins, conflicts := 0, 0
	for i := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.Write(ctx, "race.md", fmt.Sprintf("writer %d", i), "w", "", base.Hash)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				wins++
			case errors.Is(err, ErrConflict):
				conflicts++
			default:
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if wins != 1 || conflicts != 15 {
		t.Fatalf("wins = %d, conflicts = %d", wins, conflicts)
	}
}

func TestPathsAndLimits(t *testing.T) {
	ctx := context.Background()
	s := memStore(Config{MaxPageBytes: 64, MaxPages: 2})
	for _, p := range []string{"", "plan", "notes/plan.txt", "/abs.md", "../up.md", "a/./b.md", ".hidden.md", "a/.git/b.md",
		"a\nb.md", "a.md/b.md", "1/2/3/4/5/6/7/8/9.md", strings.Repeat("x", 600) + ".md", "bad\xffutf8.md", "search", "notes/plan.md/history"} {
		if _, err := s.Write(ctx, p, "x", "alice", "", IfAny); !errors.Is(err, ErrInvalid) {
			t.Errorf("Write %q = %v", p, err)
		}
	}
	if _, err := s.Write(ctx, IndexPath, "mine", "alice", "", IfAny); !errors.Is(err, ErrInvalid) || !strings.Contains(Reason(err), "index.md") {
		t.Fatalf("a write of the index = %v", err)
	}
	if err := s.Delete(ctx, IndexPath, "alice", ""); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a removal of the index = %v", err)
	}
	if _, err := s.Write(ctx, "big.md", strings.Repeat("x", 65), "alice", "", IfAny); !errors.Is(err, ErrLimit) {
		t.Fatalf("a page above the limit = %v", err)
	}
	if _, err := s.Write(ctx, "bad.md", "not \xff utf-8", "alice", "", IfAny); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a text that is not UTF-8 = %v", err)
	}
	mustWrite(t, s, "one.md", "1", IfAny)
	mustWrite(t, s, "日本/二.md", "2", IfAny)
	// The index does not count, and a write into a page that exists does.
	if _, err := s.Write(ctx, "three.md", "3", "alice", "", IfAny); !errors.Is(err, ErrLimit) || !strings.Contains(Reason(err), "maxPages") {
		t.Fatalf("a page above the count = %v", err)
	}
	mustWrite(t, s, "one.md", "one", IfAny)
}

func TestFrontMatter(t *testing.T) {
	cases := []struct {
		text  string
		title string
		tags  string
		bad   bool
	}{
		{"# Title\n", "Title", "", false},
		{"text\n\n# Later title #\n", "Later title", "", false},
		{"```\n# not a title\n```\n# Real\n", "Real", "", false},
		{"---\ntitle: From matter\ntags: a, b, a\n---\n# Heading\n", "From matter", "a,b", false},
		{"---\ntags: [x]\n---\n# Heading\n", "Heading", "x", false},
		{"---\r\ntitle: Windows\r\n---\r\nbody\r\n", "Windows", "", false},
		{"---\nno closing line\n", "", "", false},
		{"---\ntitle: [unclosed\n---\n", "", "", true},
		{"---\ntitle: {a: b}\n---\n", "", "", true},
		{"---\ntags: {a: b}\n---\n", "", "", true},
		{"---\ntitle: " + strings.Repeat("t", 201) + "\n---\n", "", "", true},
		{"---\ntags: [" + manyTags(40) + "]\n---\n", "", "", true},
		{"---\n" + strings.Repeat("k: v\n", 2000) + "---\n", "", "", true},
	}
	for _, c := range cases {
		title, tags, err := frontMatter(c.text)
		if c.bad {
			if !errors.Is(err, ErrInvalid) {
				t.Errorf("%q: err = %v", c.text, err)
			}
			continue
		}
		if err != nil || title != c.title || strings.Join(tags, ",") != c.tags {
			t.Errorf("%q: %q %v %v", c.text, title, tags, err)
		}
	}
}

func manyTags(n int) string {
	var tags []string
	for i := range n {
		tags = append(tags, fmt.Sprintf("t%d", i))
	}
	return strings.Join(tags, ", ")
}

func TestListAndIndex(t *testing.T) {
	ctx := context.Background()
	s := memStore(Config{})
	mustWrite(t, s, "index.md", "# Home\n", IfAny)
	mustWrite(t, s, "agents/reviewer/learned.md", "---\ntitle: Learned [by] reviewer\n---\n", IfAny)
	mustWrite(t, s, "notes/plan.md", "no title here", IfAny)
	list, err := s.List(ctx)
	if err != nil || len(list) != 3 || list[0].Path != "agents/reviewer/learned.md" || list[2].Path != "notes/plan.md" {
		t.Fatalf("List = %+v, %v", list, err)
	}
	idx, err := s.Read(ctx, IndexPath, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"The wiki holds 3 pages", "## agents/reviewer/", "- [Learned \\[by\\] reviewer](<agents/reviewer/learned.md>)", "## /", "- [Home](<index.md>)", "- [plan.md](<notes/plan.md>)"} {
		if !strings.Contains(idx.Text, want) {
			t.Errorf("the index lacks %q:\n%s", want, idx.Text)
		}
	}
	if idx.By != ByStore || idx.Title != "Index of the wiki" {
		t.Errorf("the index record = %+v", idx.PageInfo)
	}
	// A change of a text alone leaves the index as it is.
	v := idx.Version
	mustWrite(t, s, "notes/plan.md", "still no title", IfAny)
	if idx, _ := s.Read(ctx, IndexPath, 0); idx.Version != v {
		t.Fatalf("the index changed for a text alone")
	}
	if err := s.Delete(ctx, "index.md", "alice", ""); err != nil {
		t.Fatal(err)
	}
	if idx, _ := s.Read(ctx, IndexPath, 0); strings.Contains(idx.Text, "Home") || !strings.Contains(idx.Text, "holds 2 pages") {
		t.Fatalf("the index after a removal:\n%s", idx.Text)
	}
}

func TestIndexStaysWithinAPage(t *testing.T) {
	s := memStore(Config{MaxPageBytes: 1024})
	var infos []PageInfo
	for i := range 100 {
		infos = append(infos, PageInfo{Path: fmt.Sprintf("dir/page-%03d.md", i)})
	}
	text := s.renderIndex(infos)
	if len(text) > 1024 || !strings.Contains(text, "The index leaves out") {
		t.Fatalf("the index has %d bytes:\n%s", len(text), text)
	}
}

func TestSearch(t *testing.T) {
	ctx := context.Background()
	s := memStore(Config{})
	mustWrite(t, s, "notes/puppet.md", "# Puppet modules\n\nThe cron module moves to Ansible next.\nNothing else.\n", IfAny)
	mustWrite(t, s, "notes/ansible.md", "# Ansible\n\nPlaybooks for cron and for nginx.\n", IfAny)
	mustWrite(t, s, "agents/reviewer/learned.md", "# Learned\n\nCheck the cron schedule twice.\n", IfAny)
	found, err := s.Find(ctx, "CRON ansible", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(found.Hits) != 2 || found.Hits[0].Path != "notes/ansible.md" || found.Cut || found.Read != 3 {
		t.Fatalf("Find = %+v", found)
	}
	if !strings.Contains(found.Hits[1].Snippet, "cron module moves") {
		t.Fatalf("the snippet = %q", found.Hits[1].Snippet)
	}
	if hits, _ := s.Search(ctx, "reviewer", 10); len(hits) != 1 || hits[0].Path != "agents/reviewer/learned.md" {
		t.Fatalf("a search by path = %+v", hits)
	}
	if _, err := s.Find(ctx, " ,. ", 10); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a query without words = %v", err)
	}
	// The budget bounds the texts that one search reads, and a page that
	// the search did not read still matches by its path.
	s.searchBytes = 100
	found, err = s.Find(ctx, "puppet", 10)
	if err != nil || !found.Cut || found.Unread == 0 || len(found.Hits) != 1 || found.Hits[0].Snippet != "" {
		t.Fatalf("Find with a small budget = %+v, %v", found, err)
	}
	found, err = s.Find(ctx, "schedule", 10)
	if err != nil || !found.Cut {
		t.Fatalf("Find past the budget = %+v, %v", found, err)
	}
}

func TestSnippet(t *testing.T) {
	long := strings.Repeat("a", 300) + "needle" + strings.Repeat("b", 300)
	got := snippet(long, 300)
	if !strings.HasPrefix(got, "...") || !strings.HasSuffix(got, "...") || !strings.Contains(got, "needle") || len(got) > 260 {
		t.Fatalf("snippet = %q", got)
	}
	if got := snippet("one\ntwo needle\nthree", 8); got != "two needle" {
		t.Fatalf("snippet = %q", got)
	}
}

func natsServerJS(t *testing.T) (jetstream.JetStream, jetstream.JetStream) {
	t.Helper()
	srv, err := server.NewServer(&server.Options{Host: "127.0.0.1", Port: -1, JetStream: true, StoreDir: t.TempDir(), NoLog: true, NoSigs: true})
	if err != nil {
		t.Fatal(err)
	}
	go srv.Start()
	t.Cleanup(srv.Shutdown)
	if !srv.ReadyForConnections(5 * time.Second) {
		t.Fatal("the NATS server did not start")
	}
	var out [2]jetstream.JetStream
	for i := range out {
		nc, err := nats.Connect(srv.ClientURL())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(nc.Close)
		if out[i], err = jetstream.New(nc); err != nil {
			t.Fatal(err)
		}
	}
	return out[0], out[1]
}

// Two replicas of the front on two connections share the wiki of one
// bucket: a stale write of one fails, the history is the history of the
// bucket, and the index holds the pages of both.
func TestNATSReplicas(t *testing.T) {
	ctx := context.Background()
	js1, js2 := natsServerJS(t)
	c := withDefaults(Config{History: 5})
	r1 := New(bucket.NewNATS(js1, "FW_PAGES", bucketConfig(c)), c, quiet())
	r2 := New(bucket.NewNATS(js2, "FW_PAGES", bucketConfig(c)), c, quiet())
	if list, err := r1.List(ctx); err != nil || len(list) != 0 {
		t.Fatalf("List without a bucket = %v, %v", list, err)
	}
	base := mustWrite(t, r1, "shared.md", "# Shared\n", IfAbsent)
	if _, err := r2.Write(ctx, "shared.md", "from r2", "bob", "", base.Hash); err != nil {
		t.Fatal(err)
	}
	if _, err := r1.Write(ctx, "shared.md", "from r1", "alice", "", base.Hash); !errors.Is(err, ErrConflict) {
		t.Fatalf("the stale write of r1 = %v", err)
	}
	mustWrite(t, r2, "only-r2.md", "# Second\n", IfAny)
	list, err := r1.List(ctx)
	if err != nil || len(list) != 2 || list[1].Path != "shared.md" {
		t.Fatalf("r1 lists %+v, %v", list, err)
	}
	if list[1].By != "bob" {
		t.Fatalf("r1 lists a stale record: %+v", list[1])
	}
	idx, err := r1.Read(ctx, IndexPath, 0)
	if err != nil || !strings.Contains(idx.Text, "only-r2.md") || !strings.Contains(idx.Text, "shared.md") {
		t.Fatalf("the index = %v\n%s", err, idx.Text)
	}
	for i := range 6 {
		mustWrite(t, r1, "shared.md", fmt.Sprintf("version %d", i), IfAny)
	}
	h, err := r2.History(ctx, "shared.md")
	if err != nil || len(h) != 5 || h[0].By != "alice" {
		t.Fatalf("History = %+v, %v", h, err)
	}
	if err := r2.Delete(ctx, "shared.md", "bob", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := r1.Read(ctx, "shared.md", 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("r1 reads a removed page: %v", err)
	}
	if list, _ := r1.List(ctx); len(list) != 1 {
		t.Fatalf("r1 lists a removed page: %+v", list)
	}
}
