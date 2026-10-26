package pages

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// jsonText is the JSON body of a write of the text.
func jsonText(text string) string {
	b, _ := json.Marshal(map[string]string{"text": text})
	return string(b)
}

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
	var info Page
	decodeJSON(t, out, &info)
	if info.Hash != Hash(text) || resp.Header.Get("ETag") != strconv.Quote(info.Hash) || info.By != "alice" || info.Title != "Plan" || info.Summary != "first draft" || info.Text != text {
		t.Fatalf("PUT answered %+v, ETag %s", info, resp.Header.Get("ETag"))
	}
	// A second create fails with 412 and the hash of the page, in the
	// problem and in ETag.
	resp, out = do(t, "PUT", base+"/notes/plan.md", "alice", body, "Content-Type", "application/json", "If-None-Match", "*")
	var problem struct {
		Status int    `json:"status"`
		Detail string `json:"detail"`
		Hash   string `json:"hash"`
	}
	decodeJSON(t, out, &problem)
	if resp.StatusCode != http.StatusPreconditionFailed || resp.Header.Get("ETag") != strconv.Quote(info.Hash) || problem.Hash != info.Hash || problem.Status != 412 ||
		resp.Header.Get("Content-Type") != "application/problem+json" {
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

	// If-Match makes a compare-and-swap, quoted or weak.
	resp, out = do(t, "PUT", base+"/notes/plan.md", "alice", `{"text": "# Plan two\n", "summary": "second"}`, "Content-Type", "application/json", "If-Match", `W/"`+info.Hash+`"`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT with If-Match = %d %s", resp.StatusCode, out)
	}
	var second PageInfo
	decodeJSON(t, out, &second)
	if second.Summary != "second" || second.Version <= info.Version {
		t.Fatalf("the second version = %+v", second)
	}
	resp, out = do(t, "PUT", base+"/notes/plan.md", "alice", jsonText("# Stale\n"), "If-Match", strconv.Quote(info.Hash))
	if resp.StatusCode != http.StatusPreconditionFailed || !strings.Contains(out, "read it again") || resp.Header.Get("ETag") != strconv.Quote(second.Hash) ||
		!strings.Contains(out, `"hash":"`+second.Hash+`"`) {
		t.Fatalf("a stale If-Match = %d %s", resp.StatusCode, out)
	}
	if resp, out = do(t, "PUT", base+"/notes/new.md", "alice", jsonText("x"), "If-Match", "*"); resp.StatusCode != http.StatusPreconditionFailed || strings.Contains(out, `"hash"`) {
		t.Fatalf("If-Match: * on a new page = %d %s", resp.StatusCode, out)
	}
	if resp, _ = do(t, "PUT", base+"/notes/new.md", "alice", jsonText("x"), "If-Match", "*", "If-None-Match", "*"); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("both conditions = %d", resp.StatusCode)
	}
	// An empty, unquoted, or quoted special tag is no condition of its own.
	for _, tag := range []string{`""`, `W/""`, `W/`, `"absent"`, `"*"`, second.Hash} {
		if resp, out = do(t, "PUT", base+"/notes/plan.md", "alice", jsonText("# Over\n"), "If-Match", tag); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("If-Match: %s = %d %s", tag, resp.StatusCode, out)
		}
	}
	// The body is JSON, and a raw text is refused with what to send.
	if resp, out = do(t, "PUT", base+"/notes/raw.md", "alice", "# Raw\n", "Content-Type", "text/markdown"); resp.StatusCode != http.StatusBadRequest || !strings.Contains(out, `{\"text\"`) {
		t.Fatalf("a raw body = %d %s", resp.StatusCode, out)
	}
	if resp, _ = do(t, "PUT", base+"/notes/raw.md", "alice", `{"summary": "no text"}`); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("a body without text = %d", resp.StatusCode)
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
	var found struct {
		Q       string `json:"q"`
		Results []struct {
			Path    string `json:"path"`
			Title   string `json:"title"`
			Snippet string `json:"snippet"`
		} `json:"results"`
		Cut *bool `json:"cut"`
	}
	decodeJSON(t, out, &found)
	if resp.StatusCode != http.StatusOK || found.Q != "plan two" || len(found.Results) != 1 || found.Results[0].Path != "notes/plan.md" ||
		found.Results[0].Title != "Plan two" || found.Cut == nil || *found.Cut {
		t.Fatalf("search = %d %s", resp.StatusCode, out)
	}
	if resp, _ = do(t, "GET", base+"/search", "bob", ""); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("search without q = %d", resp.StatusCode)
	}

	// The tree, and the index, which a person reads and cannot write.
	resp, out = do(t, "GET", base, "bob", "")
	var tree struct {
		Pages []PageInfo `json:"pages"`
	}
	decodeJSON(t, out, &tree)
	if resp.StatusCode != http.StatusOK || len(tree.Pages) != 1 || tree.Pages[0].Path != "notes/plan.md" || tree.Pages[0].Hash != second.Hash {
		t.Fatalf("GET /v1/pages = %d %s", resp.StatusCode, out)
	}
	if resp, out = do(t, "GET", base+"/_index.md", "bob", "", "Accept", "text/markdown"); resp.StatusCode != http.StatusOK || !strings.Contains(out, "notes/plan.md") {
		t.Fatalf("GET the index = %d %s", resp.StatusCode, out)
	}
	if resp, out = do(t, "PUT", base+"/_index.md", "alice", jsonText("mine")); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("PUT the index = %d %s", resp.StatusCode, out)
	}

	// Paths and sizes that the wiki refuses.
	if resp, _ = do(t, "PUT", base+"/notes/what%3F.md", "alice", jsonText("x")); resp.StatusCode != http.StatusCreated || resp.Header.Get("Location") != "/v1/pages/notes/what%3F.md" {
		t.Fatalf("PUT of a path with a question mark = %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	for p, why := range map[string]string{"notes/plan.txt": "ends with .md", ".hidden.md": "hidden", "a.md/b.md": "only the last part", "search": "GET /v1/pages/search", "notes/history": "history of a page"} {
		if resp, out = do(t, "PUT", base+"/"+p, "alice", jsonText("x")); resp.StatusCode != http.StatusBadRequest || !strings.Contains(out, why) {
			t.Errorf("PUT %s = %d %s", p, resp.StatusCode, out)
		}
	}
	if resp, _ = do(t, "PUT", base+"/big.md", "alice", jsonText(strings.Repeat("x", DefaultMaxPageBytes+1))); resp.StatusCode != http.StatusRequestEntityTooLarge {
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
	// The removed pages, for the fold of the page: the path, the title and
	// the newest version with a text, and the time of the removal.
	resp, out = do(t, "GET", base+"/removed", "bob", "")
	var gone struct{ Removed []RemovedPage }
	if err := json.Unmarshal([]byte(out), &gone); resp.StatusCode != http.StatusOK || err != nil || len(gone.Removed) != 1 ||
		gone.Removed[0].Path != "notes/plan.md" || gone.Removed[0].Version != second.Version || gone.Removed[0].RemovedAt.IsZero() {
		t.Fatalf("GET removed = %d %s", resp.StatusCode, out)
	}
	if resp, _ = do(t, "GET", base+"/notes/plan.md?version="+strconv.FormatUint(gone.Removed[0].Version, 10), "bob", ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("GET of the version to restore = %d", resp.StatusCode)
	}
	// A page that comes back is no removed page.
	if resp, out = do(t, "PUT", base+"/notes/plan.md", "alice", jsonText("# Plan\n\nBack.")); resp.StatusCode != http.StatusCreated {
		t.Fatalf("PUT back = %d %s", resp.StatusCode, out)
	}
	if resp, out = do(t, "GET", base+"/removed", "bob", ""); resp.StatusCode != http.StatusOK || out != `{"removed":[]}`+"\n" {
		t.Fatalf("GET removed after the restore = %d %s", resp.StatusCode, out)
	}
	if resp, _ = do(t, "GET", base+"/removed", "carol", ""); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("GET removed of a caller without a role = %d", resp.StatusCode)
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
		resp, out := do(t, "PUT", base+"/by-"+c.token+".md", c.token, jsonText("# By "+c.token+"\n"))
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

// GET /v1/pages?prefix= lists only the pages whose path starts with the
// text, as the tool pages_list does, and an empty prefix lists them all.
func TestRoutesListPrefix(t *testing.T) {
	a := newAgent(t, nil)
	base := serve(t, a) + "/v1/pages"
	for _, p := range []string{"agents/reviewer/learned.md", "agents/reviewer-two/notes.md", "agents/triage/learned.md", "runbooks/cron.md"} {
		if resp, out := do(t, "PUT", base+"/"+p, "alice", jsonText("# "+p+"\n")); resp.StatusCode != http.StatusCreated {
			t.Fatalf("PUT %s = %d %s", p, resp.StatusCode, out)
		}
	}
	paths := func(query string) []string {
		t.Helper()
		resp, out := do(t, "GET", base+query, "bob", "")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET /v1/pages%s = %d %s", query, resp.StatusCode, out)
		}
		var tree struct {
			Pages []PageInfo `json:"pages"`
		}
		decodeJSON(t, out, &tree)
		if tree.Pages == nil {
			t.Fatalf("GET /v1/pages%s answered no list: %s", query, out)
		}
		got := []string{}
		for _, p := range tree.Pages {
			got = append(got, p.Path)
		}
		return got
	}
	for query, want := range map[string]string{
		"?prefix=agents/reviewer/": "agents/reviewer/learned.md",
		"?prefix=agents/reviewer":  "agents/reviewer-two/notes.md agents/reviewer/learned.md",
		"?prefix=agents%2F":        "agents/reviewer-two/notes.md agents/reviewer/learned.md agents/triage/learned.md",
		"?prefix=":                 "agents/reviewer-two/notes.md agents/reviewer/learned.md agents/triage/learned.md runbooks/cron.md",
		"":                         "agents/reviewer-two/notes.md agents/reviewer/learned.md agents/triage/learned.md runbooks/cron.md",
		"?prefix=wiki/":            "",
	} {
		if got := strings.Join(paths(query), " "); got != want {
			t.Errorf("GET /v1/pages%s = %q, want %q", query, got, want)
		}
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
