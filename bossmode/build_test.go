package bossmode

import (
	"encoding/json"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"testing/fstest"
)

// innerBuild is set in the go test that TestBuildWithThePlaceholder runs.
const innerBuild = "FW_BOSSMODE_INNER_BUILD"

// The embedded page is the build in static/ when it holds index.html, and
// the placeholder otherwise.
func TestEmbeddedPage(t *testing.T) {
	placeholder := []byte("<!doctype html>\n" + placeholderMark + "\n")
	for _, tc := range []struct {
		name  string
		fsys  fstest.MapFS
		built bool
		index string
	}{
		{"without a build", fstest.MapFS{"static/.gitkeep": {}, "placeholder/index.html": {Data: placeholder}}, false, string(placeholder)},
		{"with a build", fstest.MapFS{"static/.gitkeep": {}, "static/index.html": {Data: []byte("page")}, "static/assets/index-1.js": {Data: []byte("js")}, "placeholder/index.html": {Data: placeholder}}, true, "page"},
	} {
		page, built, err := embeddedFS(tc.fsys)
		if err != nil || built != tc.built {
			t.Fatalf("%s: built %v, %v", tc.name, built, err)
		}
		if b, err := fs.ReadFile(page, indexFile); err != nil || string(b) != tc.index {
			t.Errorf("%s: index.html = %q, %v", tc.name, b, err)
		}
	}
}

// git keeps only .gitkeep under bossmode/static and ignores a build of the
// page there, so a checkout embeds the placeholder.
func TestStaticTracksNoBuild(t *testing.T) {
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("no git")
	}
	if out, err := exec.Command(git, "rev-parse", "--is-inside-work-tree").Output(); err != nil || strings.TrimSpace(string(out)) != "true" {
		t.Skip("not a checkout")
	}
	out, err := exec.Command(git, "ls-files", "static", "placeholder").Output()
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Fields(string(out)); strings.Join(got, " ") != "placeholder/index.html static/.gitkeep" {
		t.Errorf("git tracks %v under static and placeholder", got)
	}
	for _, p := range []string{"static/index.html", "static/assets/index-1.js", "static/feather.svg"} {
		if err := exec.Command(git, "check-ignore", "-q", p).Run(); err != nil {
			t.Errorf("git does not ignore %s", p)
		}
	}
	if err := exec.Command(git, "check-ignore", "-q", "static/.gitkeep").Run(); err == nil {
		t.Error("git ignores static/.gitkeep")
	}
}

// go build succeeds with the placeholder: a go test of this package, with
// every file of a build of the page in static hidden by an overlay, builds
// and serves the placeholder.
func TestBuildWithThePlaceholder(t *testing.T) {
	if os.Getenv(innerBuild) != "" {
		t.Skip("the inner build")
	}
	if testing.Short() {
		t.Skip("builds the package again")
	}
	goTool, err := exec.LookPath("go")
	if err != nil {
		goTool = filepath.Join(runtime.GOROOT(), "bin", "go")
		if _, err := os.Stat(goTool); err != nil {
			t.Skip("no go tool")
		}
	}
	dir, err := filepath.Abs("static")
	if err != nil {
		t.Fatal(err)
	}
	hide := map[string]string{}
	err = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && d.Name() != ".gitkeep" {
			hide[p] = ""
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(map[string]any{"Replace": hide})
	if err != nil {
		t.Fatal(err)
	}
	overlay := filepath.Join(t.TempDir(), "overlay.json")
	if err := os.WriteFile(overlay, b, 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(goTool, "test", "-overlay="+overlay, "-count=1", "-v", "-run", "^TestStaticPlaceholder$", ".")
	cmd.Env = append(os.Environ(), innerBuild+"=1")
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "--- PASS: TestStaticPlaceholder") {
		t.Fatalf("go test with the placeholder: %v\n%s", err, out)
	}
}
