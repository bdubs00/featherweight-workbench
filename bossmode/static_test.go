package bossmode

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/surface/native"
	"github.com/bdubs00/featherweight-workbench/bridges"
	"github.com/bdubs00/featherweight-workbench/conversations"
	"github.com/bdubs00/featherweight-workbench/internal/bucket"
)

// pageDir writes a built page into a directory inside a parent that also
// holds a secret file beside it.
func pageDir(t *testing.T) string {
	t.Helper()
	parent := t.TempDir()
	if err := os.WriteFile(filepath.Join(parent, "secret.txt"), []byte("the secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(parent, "dist")
	for name, body := range map[string]string{
		"index.html":                  "<!doctype html><title>bossmode</title><script type=module src=/bossmode/assets/index-BeZQCCyd.js></script>",
		"assets/index-BeZQCCyd.js":    "console.log('page')",
		"assets/index-D259mp1z.css":   "body{}",
		"feather.svg":                 "<svg xmlns='http://www.w3.org/2000/svg'/>",
		"assets/fonts/inter.v4.woff2": "font",
		".env":                        "SECRET=1",
		".git/config":                 "[core]",
	} {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// A link inside the directory to the file outside it.
	if err := os.Symlink(filepath.Join(parent, "secret.txt"), filepath.Join(dir, "leak.txt")); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestStaticFromDir(t *testing.T) {
	dir := pageDir(t)
	t.Setenv(EnvDir, dir)
	fr := newFront(t, nil)
	cases := []struct {
		path, ctype, cache, body string
		status                   int
	}{
		{"/bossmode/", "text/html; charset=utf-8", cacheIndex, "<title>bossmode</title>", http.StatusOK},
		{"/bossmode/bridge/01K/channel", "text/html; charset=utf-8", cacheIndex, "<title>bossmode</title>", http.StatusOK},
		{"/bossmode/assets/index-BeZQCCyd.js", "text/javascript; charset=utf-8", cacheAssets, "console.log", http.StatusOK},
		{"/bossmode/assets/index-D259mp1z.css", "text/css; charset=utf-8", cacheAssets, "body{}", http.StatusOK},
		{"/bossmode/assets/fonts/inter.v4.woff2", "font/woff2", cacheAssets, "font", http.StatusOK},
		{"/bossmode/feather.svg", "image/svg+xml", cacheOther, "<svg", http.StatusOK},
		{"/bossmode/missing.js", "", "", "no file missing.js", http.StatusNotFound},
		{"/bossmode/assets", "text/html; charset=utf-8", cacheIndex, "<title>bossmode</title>", http.StatusOK},
		{"/bossmode/leak.txt", "", "", "no file", http.StatusNotFound},
		{"/bossmode/.env", "", "", "no file", http.StatusNotFound},
		{"/bossmode/.git/config", "", "", "no file", http.StatusNotFound},
		{"/bossmode/..%2fsecret.txt", "", "", "holds ..", http.StatusBadRequest},
		{"/bossmode/%2e%2e/secret.txt", "", "", "holds ..", http.StatusBadRequest},
		{"/bossmode/assets/..%2f..%2f..%2fsecret.txt", "", "", "holds ..", http.StatusBadRequest},
		{"/bossmode/a%5cb.js", "", "", "backslash", http.StatusBadRequest},
	}
	for _, c := range cases {
		resp, body := get(t, fr.url+c.path, "")
		if resp.StatusCode != c.status || !strings.Contains(body, c.body) || strings.Contains(body, "the secret") {
			t.Errorf("%s: %d %q, want %d with %q", c.path, resp.StatusCode, body, c.status, c.body)
			continue
		}
		if got := resp.Header.Get("Content-Security-Policy"); got != CSP {
			t.Errorf("%s: policy %q", c.path, got)
		}
		if resp.Header.Get("X-Content-Type-Options") != "nosniff" || resp.Header.Get("Referrer-Policy") != "no-referrer" {
			t.Errorf("%s: headers %v", c.path, resp.Header)
		}
		if c.ctype != "" && resp.Header.Get("Content-Type") != c.ctype {
			t.Errorf("%s: type %q, want %q", c.path, resp.Header.Get("Content-Type"), c.ctype)
		}
		if c.cache != "" && resp.Header.Get("Cache-Control") != c.cache {
			t.Errorf("%s: cache %q, want %q", c.path, resp.Header.Get("Cache-Control"), c.cache)
		}
	}
	// The policy is the one that the page needs, and no more.
	if CSP != "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' blob:; connect-src 'self'; font-src 'self'; object-src 'none'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'" {
		t.Errorf("CSP = %q", CSP)
	}
	// A directory without the page answers 503, and names the key.
	if err := os.Remove(filepath.Join(dir, "index.html")); err != nil {
		t.Fatal(err)
	}
	resp, body := get(t, fr.url+"/bossmode/", "")
	if resp.StatusCode != http.StatusServiceUnavailable || !strings.Contains(body, "workbench.bossmode.dir") || strings.Contains(body, dir) {
		t.Errorf("without index.html: %d %s", resp.StatusCode, body)
	}
}

// /bossmode without the slash moves to /bossmode/ for good, with its
// query, so the address of the page always has the form that the sign-in
// returns to. The page itself keeps its answers.
func TestSlash(t *testing.T) {
	t.Setenv(EnvDir, pageDir(t))
	fr := newFront(t, nil)
	stay := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	for path, want := range map[string]string{
		"/bossmode":            "/bossmode/",
		"/bossmode?bridge=01K": "/bossmode/?bridge=01K",
	} {
		resp, err := stay.Get(fr.url + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusMovedPermanently || resp.Header.Get("Location") != want {
			t.Errorf("%s: %d to %q, want 301 to %q", path, resp.StatusCode, resp.Header.Get("Location"), want)
		}
		if resp.Header.Get("Content-Security-Policy") != CSP || resp.Header.Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("%s: headers %v", path, resp.Header)
		}
	}
	resp, err := stay.Get(fr.url + "/bossmode/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Cache-Control") != cacheIndex {
		t.Errorf("/bossmode/: %d %v", resp.StatusCode, resp.Header)
	}
	// A browser that follows the move gets the page.
	if resp, body := get(t, fr.url+"/bossmode", ""); resp.StatusCode != http.StatusOK || !strings.Contains(body, "<title>bossmode</title>") || resp.Request.URL.Path != "/bossmode/" {
		t.Errorf("/bossmode after the move: %d %s", resp.StatusCode, body)
	}
}

// Without a directory, the route serves the embedded placeholder, which
// names the key to set.
func TestStaticPlaceholder(t *testing.T) {
	if !embeddedPlaceholder() {
		if os.Getenv(innerBuild) != "" {
			t.Fatal("the overlay of the build left a build of the page in static")
		}
		t.Skip("this binary embeds a build of the page (make page); TestBuildWithThePlaceholder builds it without one")
	}
	fr := newFront(t, nil)
	for _, path := range []string{"/bossmode/", "/bossmode/bridge/01K"} {
		resp, body := get(t, fr.url+path, "")
		if resp.StatusCode != http.StatusOK || !strings.Contains(body, "workbench.bossmode.dir") || !strings.Contains(body, EnvDir) {
			t.Errorf("%s: %d %s", path, resp.StatusCode, body)
		}
		if resp.Header.Get("Content-Security-Policy") != CSP || resp.Header.Get("Cache-Control") != cacheIndex {
			t.Errorf("%s: headers %v", path, resp.Header)
		}
	}
	if resp, _ := get(t, fr.url+"/bossmode/app.js", ""); resp.StatusCode != http.StatusNotFound {
		t.Errorf("a missing embedded file: %d", resp.StatusCode)
	}
}

// The stream names the build of the page in the directory, read at each
// start of a stream, so a tab learns of a new build that a deploy put
// there. A page without the element gives no header.
func TestStreamNamesThePageBuild(t *testing.T) {
	dir := pageDir(t)
	t.Setenv(EnvDir, dir)
	fr := newFront(t, nil)
	index := func(head string) {
		t.Helper()
		body := "<!doctype html><html><head>" + head + "<title>bossmode</title></head><body><script type=module src=/bossmode/assets/index-BeZQCCyd.js></script></body></html>"
		if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	build := func() string {
		t.Helper()
		s := openStream(t, fr, "ada", "")
		defer s.close()
		return s.resp.Header.Get(HeaderPageBuild)
	}
	if got := build(); got != "" {
		t.Fatalf("a page without the element: %s = %q", HeaderPageBuild, got)
	}
	index(`<meta charset="utf-8"><meta name="bossmode-build" content="9f2c4e1a7b3d5f60">`)
	if got := build(); got != "9f2c4e1a7b3d5f60" {
		t.Fatalf("%s = %q", HeaderPageBuild, got)
	}
	index(`<meta name="bossmode-build" content="0a1b2c3d4e5f6071" />`)
	if got := build(); got != "0a1b2c3d4e5f6071" {
		t.Fatalf("after a new build: %s = %q", HeaderPageBuild, got)
	}
	// A value outside the letters of a build never reaches the header.
	for _, head := range []string{`<meta name="bossmode-build" content="a b">`, `<meta name="bossmode-build" content="">`, `<meta name="bossmode-build" content="` + strings.Repeat("a", 65) + `">`} {
		index(head)
		if got := build(); got != "" {
			t.Errorf("%s: %s = %q", head, HeaderPageBuild, got)
		}
	}
}

// Without enabled, no route of bossmode is there, and the service starts
// nothing.
func TestOff(t *testing.T) {
	off := newFrontOff(t)
	for _, path := range []string{"/bossmode", "/bossmode/", "/v1/bossmode", "/v1/bossmode/stream"} {
		if resp, _ := get(t, off+path, "ada"); resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s with bossmode off: %d", path, resp.StatusCode)
		}
	}
}

// newFrontOff is a front with bossmode off; it returns its address.
func newFrontOff(t *testing.T) string {
	t.Helper()
	t.Setenv(EnvEnabled, "off")
	cfg := fw.DefaultConfig()
	cfg.Home = t.TempDir()
	a, err := fw.New(fw.WithConfig(cfg), fw.WithoutDefaults(), fw.WithAuthn(people{}),
		fw.WithLedger(fw.NewMemLedger()), fw.WithBus(fw.NewMemBus()), fw.WithSurfaces(native.New()),
		fw.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		a.Close(ctx)
	})
	if cl, err := Start(context.Background(), a); err != nil || cl != nil {
		t.Fatalf("Start with bossmode off = %v, %v", cl, err)
	}
	h, err := a.Handler()
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv.URL
}

// The settings name the manager for the caller: its member and the newest
// run that the caller started, and the keys of the bridges.
func TestSettings(t *testing.T) {
	t.Setenv(EnvManager, "manager")
	t.Setenv(bridges.EnvPlacement, "spawn")
	fr := newFront(t, nil)
	fr.event(t, "ax:team/manager-0", "01MGR1", 1, fw.EvRunQueued, queued("ada", "manager"))
	fr.event(t, "ax:team/manager-0", "01MGR2", 1, fw.EvRunQueued, queued("bob", "manager"))
	// A newer manager run of a bridge for ada is no conversation of the page.
	inBridge := queued("ada", "manager")
	inBridge.Metadata = map[string]string{fw.MetaBatch: bridges.BatchOf("b1")}
	fr.event(t, "ax:team/manager-0", "01MGR3", 1, fw.EvRunQueued, inBridge)
	var got Settings
	resp, body := get(t, fr.url+"/v1/bossmode", "ada")
	if resp.StatusCode != http.StatusOK || json.Unmarshal([]byte(body), &got) != nil {
		t.Fatalf("settings: %d %s", resp.StatusCode, body)
	}
	if got.Manager == nil || *got.Manager != (Manager{Agent: "manager", Member: "team/manager-0", Run: "01MGR1"}) || got.Placement != "spawn" || got.MaxMembers != bridges.DefaultMaxMembers {
		t.Errorf("settings for ada = %s", body)
	}
	_, body = get(t, fr.url+"/v1/bossmode", "carol")
	if err := json.Unmarshal([]byte(body), &got); err != nil || got.Manager == nil || got.Manager.Run != "" || got.Manager.Member != "team/manager-0" {
		t.Errorf("settings for carol = %s", body)
	}
	if resp, _ := get(t, fr.url+"/v1/bossmode", ""); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("settings without a token: %d", resp.StatusCode)
	}
	// A conversation that ada deleted from her lists is no newest run.
	conversations.Attach(fr.a, conversations.New(fr.a, bucket.NewMem(bucket.Config{Name: "CONVERSATIONS"}), nil))
	t.Cleanup(func() { conversations.Attach(fr.a, nil) })
	ada := fw.WithPrincipal(context.Background(), &fw.Principal{Subject: "ada", Kind: fw.KindUser})
	hide := true
	if _, err := conversations.Of(fr.a).Patch(ada, "01MGR1", conversations.Patch{Hidden: &hide}); err != nil {
		t.Fatal(err)
	}
	_, body = get(t, fr.url+"/v1/bossmode", "ada")
	if err := json.Unmarshal([]byte(body), &got); err != nil || got.Manager == nil || got.Manager.Run != "" {
		t.Errorf("settings for ada after she hid her conversation = %s", body)
	}
	_, body = get(t, fr.url+"/v1/bossmode", "bob")
	if err := json.Unmarshal([]byte(body), &got); err != nil || got.Manager == nil || got.Manager.Run != "01MGR2" {
		t.Errorf("settings for bob = %s", body)
	}
}

func TestSettingsWithoutManager(t *testing.T) {
	fr := newFront(t, nil)
	resp, body := get(t, fr.url+"/v1/bossmode", "ada")
	if resp.StatusCode != http.StatusOK || body != `{"manager":null,"placement":"queue","maxMembers":16,"theme":{"accent":"#FF4D00","name":"","logo":""},"templates":[],"chat":null,"routines":false,"knowhow":false,"packages":false}`+"\n" {
		t.Errorf("settings: %d %q", resp.StatusCode, body)
	}
}

// A front whose binary serves the board of the work says work: true, so
// the page shows the places Work and Results. A front without it leaves
// the key out, and the page is the page of main.
func TestSettingsWithWork(t *testing.T) {
	t.Cleanup(func() { workOn.Store(false) })
	fr := newFront(t, nil)
	OfferWork()
	resp, body := get(t, fr.url+"/v1/bossmode", "ada")
	if resp.StatusCode != http.StatusOK || !strings.HasSuffix(body, `"routines":false,"knowhow":false,"packages":false,"work":true}`+"\n") {
		t.Errorf("settings: %d %q", resp.StatusCode, body)
	}
}
