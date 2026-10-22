package bossmode

// This file serves the static files of the page at /bossmode
// (docs/ARCHITECTURE.md section 17.2): from workbench.bossmode.dir, or from
// the copy in static/ that the binary embeds.

import (
	"embed"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/bdubs00/featherweight-kernel/surface/native"
)

// static holds the page that the binary embeds. The repository holds a
// placeholder index.html that names the key to set. A build that embeds
// the page copies the dist directory of featherweight-bossmode here.
//
//go:embed static
var static embed.FS

// CSP is the content security policy of every answer of the page. The
// build of the page holds no inline script, no inline style element, and
// no data URI; the faces are blob images.
const CSP = "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' blob:; connect-src 'self'; font-src 'self'; object-src 'none'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'"

// The cache of the files of the page: index.html changes with each build,
// so it is never kept; the files under assets/ have a hash in their names,
// so a new build gives new names; any other file is checked again.
const (
	cacheIndex  = "no-store"
	cacheAssets = "public, max-age=3600"
	cacheOther  = "no-cache"
)

// indexFile is the page itself.
const indexFile = "index.html"

// pageRoute serves the files of the page. dir is workbench.bossmode.dir,
// and empty serves the embedded copy.
func pageRoute(dir string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", CSP)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		name, ok := cleanFile(r.PathValue("file"))
		if !ok {
			h.Set("Cache-Control", cacheOther)
			native.WriteProblem(w, r, http.StatusBadRequest, "bossmode: the path of a file of the page holds .., a backslash, or a control character")
			return
		}
		// A path without a file extension is a place of the page, which
		// routes on the client, so it gets the page.
		if name == "" || path.Ext(name) == "" {
			name = indexFile
		}
		fsys, done, err := pageFS(dir)
		if err != nil {
			missingPage(w, r)
			return
		}
		defer done()
		f, err := fsys.Open(name)
		if err != nil {
			if name == indexFile {
				missingPage(w, r)
				return
			}
			h.Set("Cache-Control", cacheOther)
			native.WriteProblem(w, r, http.StatusNotFound, "bossmode: the page has no file "+name)
			return
		}
		defer f.Close()
		st, err := f.Stat()
		rs, seekable := f.(io.ReadSeeker)
		if err != nil || st.IsDir() || !seekable {
			h.Set("Cache-Control", cacheOther)
			native.WriteProblem(w, r, http.StatusNotFound, "bossmode: the page has no file "+name)
			return
		}
		switch {
		case name == indexFile:
			h.Set("Cache-Control", cacheIndex)
		case strings.HasPrefix(name, "assets/"):
			h.Set("Cache-Control", cacheAssets)
		default:
			h.Set("Cache-Control", cacheOther)
		}
		ctype := mime.TypeByExtension(path.Ext(name))
		if ctype == "" {
			ctype = "application/octet-stream"
		}
		h.Set("Content-Type", ctype)
		http.ServeContent(w, r, name, st.ModTime(), rs)
	}
}

// cleanFile returns the name of a file of the page relative to its root,
// "" for the root. It refuses a path with an element "..", a backslash, or
// a control character, which can reach the route escaped, such as %2e%2e.
func cleanFile(raw string) (string, bool) {
	for _, r := range raw {
		if r == '\\' || unicode.IsControl(r) {
			return "", false
		}
	}
	for _, el := range strings.Split(raw, "/") {
		if el == ".." {
			return "", false
		}
	}
	name := strings.TrimPrefix(path.Clean("/"+raw), "/")
	return name, fs.ValidPath(name) || name == ""
}

// pageFS opens the files of the page: the directory under an os.Root, so
// no name and no link leads out of it, or the embedded copy. done closes
// what pageFS opened.
func pageFS(dir string) (fs.FS, func(), error) {
	if dir == "" {
		sub, err := fs.Sub(static, "static")
		return sub, func() {}, err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, nil, err
	}
	return root.FS(), func() { _ = root.Close() }, nil
}

// missingPage answers 503 with a small page when the directory of the
// page holds no index.html: the page is not built there yet. It names the
// key, not the directory.
func missingPage(w http.ResponseWriter, _ *http.Request) {
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", cacheIndex)
	w.WriteHeader(http.StatusServiceUnavailable)
	fmt.Fprint(w, `<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><title>Bossmode</title></head>
<body><h1>Bossmode</h1><p>The page is missing: the directory that workbench.bossmode.dir names holds no index.html. Build featherweight-bossmode and put its dist directory there.</p><p><a href="/ui">Open the dashboard</a></p></body></html>
`)
}

// checkDir checks workbench.bossmode.dir at the start: empty, or a
// directory.
func checkDir(dir string) error {
	if dir == "" {
		return nil
	}
	st, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("bossmode: workbench.bossmode.dir (%s) %q: %w", EnvDir, dir, err)
	}
	if !st.IsDir() {
		return fmt.Errorf("bossmode: workbench.bossmode.dir (%s) %q is not a directory; set it to the dist directory of the build of the page", EnvDir, dir)
	}
	return nil
}

// hasIndex reports whether a directory holds index.html.
func hasIndex(dir string) bool {
	st, err := os.Stat(filepath.Join(dir, indexFile))
	return err == nil && !st.IsDir()
}

// embeddedPlaceholder reports whether the embedded copy is the placeholder
// of the repository, which says that the page is not built in.
func embeddedPlaceholder() bool {
	b, err := static.ReadFile("static/" + indexFile)
	return err != nil || strings.Contains(string(b), placeholderMark)
}

// placeholderMark is a comment in the placeholder index.html.
const placeholderMark = "<!-- fw-workbench: bossmode placeholder -->"
