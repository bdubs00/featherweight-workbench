package pages

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bdubs00/featherweight-kernel/fw"
	"gopkg.in/yaml.v3"
)

func writeFile(t *testing.T, dir, name, text string) {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestExportImport(t *testing.T) {
	ctx := context.Background()
	src := t.TempDir()
	writeFile(t, src, "index.md", "# Home\n")
	writeFile(t, src, "agents/reviewer/learned.md", "# Learned\n")
	writeFile(t, src, "_index.md", "# Old index\n")
	writeFile(t, src, "notes/readme.txt", "not a page")
	writeFile(t, src, ".git/config", "[core]")
	writeFile(t, src, "notes/.draft.md", "hidden")
	writeFile(t, src, "bad/front.md", "---\ntitle: [x\n---\n")
	writeFile(t, src, "big.md", strings.Repeat("x", 2048))
	if err := os.Symlink("/etc/passwd", filepath.Join(src, "link.md")); err != nil {
		t.Fatal(err)
	}
	s := memStore(Config{MaxPageBytes: 1024})
	out, err := Import(ctx, s, src, "local:alice")
	if err != nil {
		t.Fatal(err)
	}
	if out.Written != 2 || out.Unchanged != 0 {
		t.Fatalf("Import = %+v", out)
	}
	skipped := strings.Join(out.Skipped, "\n")
	for _, want := range []string{"_index.md: the index", "notes/readme.txt: not a page", "notes/.draft.md: a hidden file", "bad/front.md: the server matter is not valid YAML", "big.md: a page holds at most 1 KB", "link.md: a symbolic link"} {
		if !strings.Contains(skipped, want) {
			t.Errorf("the skipped files lack %q:\n%s", want, skipped)
		}
	}
	if strings.Contains(skipped, ".git") {
		t.Errorf("the import entered .git:\n%s", skipped)
	}
	page, err := s.Read(ctx, "agents/reviewer/learned.md", 0)
	if err != nil || page.By != "local:alice" || !strings.HasPrefix(page.Summary, "import from ") {
		t.Fatalf("an imported page = %+v, %v", page.PageInfo, err)
	}
	// A second import of the same files changes nothing.
	if out, err := Import(ctx, s, src, "local:alice"); err != nil || out.Written != 0 || out.Unchanged != 2 {
		t.Fatalf("a second Import = %+v, %v", out, err)
	}

	dst := t.TempDir()
	writeFile(t, dst, "keep.txt", "stays")
	exp, err := Export(ctx, s, dst)
	if err != nil || exp.Pages != 2 || !exp.Index {
		t.Fatalf("Export = %+v, %v", exp, err)
	}
	for name, want := range map[string]string{"index.md": "# Home\n", "agents/reviewer/learned.md": "# Learned\n", "keep.txt": "stays"} {
		got, err := os.ReadFile(filepath.Join(dst, filepath.FromSlash(name)))
		if err != nil || string(got) != want {
			t.Errorf("%s = %q, %v", name, got, err)
		}
	}
	if idx, err := os.ReadFile(filepath.Join(dst, IndexPath)); err != nil || !strings.Contains(string(idx), "agents/reviewer/learned.md") {
		t.Errorf("the exported index = %q, %v", idx, err)
	}
	if _, err := Import(ctx, s, filepath.Join(dst, "none"), "x"); err == nil {
		t.Fatal("an import of a directory that does not exist")
	}
}

func withBlock(t *testing.T, src string) *fw.Config {
	t.Helper()
	cfg := fw.DefaultConfig()
	var n yaml.Node
	if err := yaml.Unmarshal([]byte(src), &n); err != nil {
		t.Fatal(err)
	}
	cfg.Extensions = map[string]yaml.Node{"workbench": *n.Content[0]}
	return cfg
}

func TestLoad(t *testing.T) {
	for _, env := range []string{EnvMaxPageBytes, EnvMaxPages, EnvHistory} {
		t.Setenv(env, "")
	}
	c, err := Load(fw.DefaultConfig())
	if err != nil || c != (Config{MaxPageBytes: 262144, MaxPages: 10000, History: 64}) {
		t.Fatalf("the defaults = %+v, %v", c, err)
	}
	c, err = Load(withBlock(t, "pages: {maxPageBytes: 1024, maxPages: 5, history: 3}\nteams: [x]\n"))
	if err != nil || c != (Config{MaxPageBytes: 1024, MaxPages: 5, History: 3}) {
		t.Fatalf("the block = %+v, %v", c, err)
	}
	t.Setenv(EnvMaxPages, "7")
	t.Setenv(EnvHistory, "10")
	if c, err = Load(withBlock(t, "pages: {maxPages: 5}")); err != nil || c.MaxPages != 7 || c.History != 10 {
		t.Fatalf("the variables = %+v, %v", c, err)
	}
	t.Setenv(EnvHistory, "65")
	if _, err := Load(nil); err == nil || !strings.Contains(err.Error(), "workbench.pages.history") {
		t.Fatalf("a history of 65 = %v", err)
	}
	t.Setenv(EnvHistory, "many")
	if _, err := Load(nil); err == nil || !strings.Contains(err.Error(), EnvHistory) {
		t.Fatalf("a history that is not a number = %v", err)
	}
	t.Setenv(EnvHistory, "")
	if _, err := Load(withBlock(t, "pages: {maxPageBytes: 9999999}")); err == nil || !strings.Contains(err.Error(), "maxPageBytes") {
		t.Fatalf("a page above the bound = %v", err)
	}
	if _, err := Load(withBlock(t, "pages: [1, 2]")); err == nil {
		t.Fatal("a block that does not fit loaded")
	}
}

func TestStatus(t *testing.T) {
	for _, env := range []string{EnvMaxPageBytes, EnvMaxPages, EnvHistory} {
		t.Setenv(env, "")
	}
	cfg := fw.DefaultConfig()
	if ok, line := Status(cfg); !ok || !strings.Contains(line, "the memory of this process") {
		t.Fatalf("Status on mem = %v %q", ok, line)
	}
	cfg.Bus.Driver = "nats"
	if ok, line := Status(cfg); !ok || !strings.Contains(line, "FW_PAGES") || !strings.Contains(line, "10000 pages") {
		t.Fatalf("Status on nats = %v %q", ok, line)
	}
	cfg.Bus.Driver = "redis"
	if ok, line := Status(cfg); !ok || !strings.HasPrefix(line, "pages: off") {
		t.Fatalf("Status on redis = %v %q", ok, line)
	}
	t.Setenv(EnvMaxPages, "0x")
	if ok, _ := Status(cfg); ok {
		t.Fatal("Status of a configuration that does not fit is ok")
	}
}
