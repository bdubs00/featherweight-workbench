package pages

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/bdubs00/featherweight-kernel/fw"
)

func TestRoutes(t *testing.T) {
	a := newAgent(t, nil)
	base := serve(t, a) + "/v1/pages"

	// A new page with If-None-Match: * answers 201, its hash, and its place.
	text := "---\ntitle: Plan\n---\nMove the cron module.\n"
	body := fmt.Sprintf(`{"text": %q, "summary": "first draft"}`, text)
	resp, out := do(t, "PUT", base+"/notes/plan.md", "alice", body, "Content-Type", "application/json", "If-None-Match", "*")
	if resp.StatusCode != http.StatusCreated || resp.Header.Get("Location") != "/v1/pages/notes/plan.md" {
		t.Fatalf("PUT = %d %s", resp.StatusCode, out)
	}
	var info PageInfo
	decodeJSON(t, out, &info)
	if info.Hash != Hash(text) || resp.Header.Get("ETag") != strconv.Quote(info.Hash) || info.By != "alice" || info.Title != "Plan" || info.Summary != "first draft" {
		t.Fatalf("PUT answered %+v, ETag %s", info, resp.Header.Get("ETag"))
	}
	if strings.Contains(out, `"text"`) {
		t.Errorf("PUT answered the text: %s", out)
	}
	// A second create fails with 412 and the hash of the page.
	resp, out = do(t, "PUT", base+"/notes/plan.md", "alice", body, "Content-Type", "application/json", "If-None-Match", "*")
	if resp.StatusCode != http.StatusPreconditionFailed || resp.Header.Get("ETag") != strconv.Quote(info.Hash) {
		t.Fatalf("a second create = %d %s", resp.StatusCode, out)
	}

	// The read answers JSON with the hash, the text alone for markdown,
	// and 304 for the hash that the client holds.
	resp, out = do(t, "GET", base+"/notes/plan.md", "bob", "")
	var page Page
	decodeJSON(t, out, &page)
	if resp.StatusCode != http.StatusOK || page.Text != text || page.Hash != info.Hash || resp.Header.Get("ETag") != strconv.Quote(info.Hash) {
		t.Fatalf("GET = %d %s", resp.StatusCode, out)
	}
	resp, out = do(t, "GET", base+"/notes/plan.md", "bob", "", "Accept", "text/markdown")
	if resp.StatusCode != http.StatusOK || out != text || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/markdown") {
		t.Fatalf("GET markdown = %d %q", resp.StatusCode, out)
	}
	if resp, _ = do(t, "GET", base+"/notes/plan.md", "bob", "", "If-None-Match", `W/"`+info.Hash+`"`); resp.StatusCode != http.StatusNotModified {
		t.Fatalf("GET with the hash = %d", resp.StatusCode)
	}

	// If-Match makes a compare-and-swap, quoted or weak; a raw body takes
	// ?summary=.
	resp, out = do(t, "PUT", base+"/notes/plan.md?summary=second", "alice", "# Plan two\n", "Content-Type", "text/markdown", "If-Match", `W/"`+info.Hash+`"`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT with If-Match = %d %s", resp.StatusCode, out)
	}
	var second PageInfo
	decodeJSON(t, out, &second)
	if second.Summary != "second" || second.Version <= info.Version {
		t.Fatalf("the second version = %+v", second)
	}
	resp, out = do(t, "PUT", base+"/notes/plan.md", "alice", "# Stale\n", "If-Match", strconv.Quote(info.Hash))
	if resp.StatusCode != http.StatusPreconditionFailed || !strings.Contains(out, "read it again") || resp.Header.Get("ETag") != strconv.Quote(second.Hash) {
		t.Fatalf("a stale If-Match = %d %s", resp.StatusCode, out)
	}
	if resp, out = do(t, "PUT", base+"/notes/new.md", "alice", "x", "If-Match", "*"); resp.StatusCode != http.StatusPreconditionFailed {
		t.Fatalf("If-Match: * on a new page = %d %s", resp.StatusCode, out)
	}
	if resp, _ = do(t, "PUT", base+"/notes/new.md", "alice", "x", "If-Match", "*", "If-None-Match", "*"); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("both conditions = %d", resp.StatusCode)
	}

	// An older version, the history, and the search.
	resp, out = do(t, "GET", base+"/notes/plan.md?version="+strconv.FormatUint(info.Version, 10), "bob", "", "Accept", "text/markdown")
	if resp.StatusCode != http.StatusOK || out != text {
		t.Fatalf("GET of version %d = %d %q", info.Version, resp.StatusCode, out)
	}
	if resp, _ = do(t, "GET", base+"/notes/plan.md?version=x", "bob", ""); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("GET of version x = %d", resp.StatusCode)
	}
	resp, out = do(t, "GET", base+"/notes/plan.md/history", "bob", "")
	var hist struct {
		Path     string    `json:"path"`
		Versions []Version `json:"versions"`
	}
	decodeJSON(t, out, &hist)
	if resp.StatusCode != http.StatusOK || hist.Path != "notes/plan.md" || len(hist.Versions) != 2 || hist.Versions[1].Summary != "first draft" {
		t.Fatalf("history = %d %s", resp.StatusCode, out)
	}
	resp, out = do(t, "GET", base+"/search?q=plan+two", "bob", "")
	var found Found
	decodeJSON(t, out, &found)
	if resp.StatusCode != http.StatusOK || len(found.Hits) != 1 || found.Hits[0].Path != "notes/plan.md" {
		t.Fatalf("search = %d %s", resp.StatusCode, out)
	}
	if resp, _ = do(t, "GET", base+"/search", "bob", ""); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("search without q = %d", resp.StatusCode)
	}

	// The tree, and the index, which a person reads and cannot write.
	resp, out = do(t, "GET", base, "bob", "")
	var tree struct {
		Pages []PageInfo `json:"pages"`
		Index string     `json:"index"`
	}
	decodeJSON(t, out, &tree)
	if resp.StatusCode != http.StatusOK || len(tree.Pages) != 1 || tree.Index != IndexPath || tree.Pages[0].Path != "notes/plan.md" {
		t.Fatalf("GET /v1/pages = %d %s", resp.StatusCode, out)
	}
	if resp, out = do(t, "GET", base+"/_index.md", "bob", "", "Accept", "text/markdown"); resp.StatusCode != http.StatusOK || !strings.Contains(out, "notes/plan.md") {
		t.Fatalf("GET the index = %d %s", resp.StatusCode, out)
	}
	if resp, out = do(t, "PUT", base+"/_index.md", "alice", "mine"); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("PUT the index = %d %s", resp.StatusCode, out)
	}

	// Paths and sizes that the wiki refuses.
	if resp, _ = do(t, "PUT", base+"/notes/what%3F.md", "alice", "x"); resp.StatusCode != http.StatusCreated || resp.Header.Get("Location") != "/v1/pages/notes/what%3F.md" {
		t.Fatalf("PUT of a path with a question mark = %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	for _, p := range []string{"notes/plan.txt", ".hidden.md", "a.md/b.md"} {
		if resp, out = do(t, "PUT", base+"/"+p, "alice", "x"); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("PUT %s = %d %s", p, resp.StatusCode, out)
		}
	}
	if resp, _ = do(t, "PUT", base+"/big.md", "alice", strings.Repeat("x", DefaultMaxPageBytes+1)); resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("PUT of a page above the limit = %d", resp.StatusCode)
	}
	if resp, _ = do(t, "GET", base+"/none.md", "bob", ""); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET of no page = %d", resp.StatusCode)
	}

	// The removal, with a compare-and-swap.
	if resp, _ = do(t, "DELETE", base+"/notes/plan.md", "alice", "", "If-Match", strconv.Quote(info.Hash)); resp.StatusCode != http.StatusPreconditionFailed {
		t.Fatalf("DELETE with a stale If-Match = %d", resp.StatusCode)
	}
	if resp, _ = do(t, "DELETE", base+"/notes/plan.md", "alice", "", "If-Match", strconv.Quote(second.Hash)); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE = %d", resp.StatusCode)
	}
	if resp, _ = do(t, "DELETE", base+"/notes/plan.md", "alice", ""); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("a second DELETE = %d", resp.StatusCode)
	}
	resp, out = do(t, "GET", base+"/notes/plan.md/history", "bob", "")
	if resp.StatusCode != http.StatusOK || !strings.Contains(out, `"deleted":true`) {
		t.Fatalf("history after DELETE = %d %s", resp.StatusCode, out)
	}
}

// Who reads and who writes: an operator of an agent reads, an owner
// writes, the static token of the fleet reads and writes, and a caller
// without a role does neither.
func TestRoutesAccess(t *testing.T) {
	a := newAgent(t, nil)
	base := serve(t, a) + "/v1/pages"
	if resp, _ := do(t, "GET", base, "", ""); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("GET without a token = %d", resp.StatusCode)
	}
	cases := []struct {
		token       string
		read, write int
	}{
		{"alice", http.StatusOK, http.StatusCreated},
		{"bob", http.StatusOK, http.StatusForbidden},
		{"carol", http.StatusForbidden, http.StatusForbidden},
		{"fleet", http.StatusOK, http.StatusCreated},
	}
	for _, c := range cases {
		if resp, out := do(t, "GET", base, c.token, ""); resp.StatusCode != c.read {
			t.Errorf("%s reads: %d %s", c.token, resp.StatusCode, out)
		}
		resp, out := do(t, "PUT", base+"/by-"+c.token+".md", c.token, "# By "+c.token+"\n")
		if resp.StatusCode != c.write {
			t.Errorf("%s writes: %d %s", c.token, resp.StatusCode, out)
		}
		if c.write == http.StatusForbidden && !strings.Contains(out, "writing the wiki needs the owner role") {
			t.Errorf("%s got the refusal %s", c.token, out)
		}
	}
	if resp, out := do(t, "GET", base+"/by-fleet.md", "alice", ""); resp.StatusCode != http.StatusOK || !strings.Contains(out, `"by":"token"`) {
		t.Fatalf("the page of the token = %d %s", resp.StatusCode, out)
	}
}

// A process whose bus keeps no store has no wiki and mounts no route.
func TestOffOnAnotherBus(t *testing.T) {
	a := newAgent(t, func(c *fw.Config) { c.Bus.Driver = "redis" })
	if Of(a) != nil {
		t.Fatal("a wiki on the bus redis")
	}
	base := serve(t, a)
	if resp, _ := do(t, "GET", base+"/v1/pages", "alice", ""); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET /v1/pages without a wiki = %d", resp.StatusCode)
	}
}
