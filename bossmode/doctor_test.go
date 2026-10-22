package bossmode

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/manager"
	"gopkg.in/yaml.v3"
)

// withBlock gives a configuration the block workbench of fw.yaml.
func withBlock(t *testing.T, text string) *fw.Config {
	t.Helper()
	cfg := fw.DefaultConfig()
	var n yaml.Node
	if err := yaml.Unmarshal([]byte(text), &n); err != nil {
		t.Fatal(err)
	}
	if cfg.Extensions == nil {
		cfg.Extensions = map[string]yaml.Node{}
	}
	cfg.Extensions[Block] = *n.Content[0]
	return cfg
}

// Load reads the block bossmode, and the manager through the package
// manager, which reads that key alone; the variables win over the file.
func TestLoad(t *testing.T) {
	t.Setenv(EnvEnabled, "")
	t.Setenv(EnvDir, "")
	t.Setenv(EnvManager, "")
	cfg := withBlock(t, "bossmode: {enabled: true, dir: /srv/page, manager: manager}\nbridges: {placement: spawn, maxMembers: 4}\n")
	c, err := Load(cfg)
	if err != nil || c != (Config{Enabled: true, Dir: "/srv/page", Manager: "manager"}) {
		t.Fatalf("Load = %+v, %v", c, err)
	}
	if m := manager.Profile(cfg); m != c.Manager {
		t.Errorf("the manager of bossmode %q is not the manager of the package manager %q", c.Manager, m)
	}
	t.Setenv(EnvEnabled, "off")
	t.Setenv(EnvDir, "/other")
	t.Setenv(EnvManager, "boss")
	c, err = Load(cfg)
	if err != nil || c != (Config{Dir: "/other", Manager: "boss"}) {
		t.Errorf("Load with the variables = %+v, %v", c, err)
	}
	t.Setenv(EnvEnabled, "maybe")
	if _, err := Load(cfg); err == nil || !strings.Contains(err.Error(), EnvEnabled) {
		t.Errorf("an enabled that is no switch: %v", err)
	}
	t.Setenv(EnvEnabled, "on")
	t.Setenv(EnvManager, "two words")
	if _, err := Load(cfg); err == nil || !strings.Contains(err.Error(), "workbench.bossmode.manager") {
		t.Errorf("a manager that is no name: %v", err)
	}
}

func TestDoctorCheck(t *testing.T) {
	ctx := context.Background()
	check := func(cfg *fw.Config) (bool, string) {
		t.Helper()
		return DoctorCheck(ctx, cfg, nil, 0)
	}
	if ok, line := check(fw.DefaultConfig()); !ok || !strings.Contains(line, "bossmode: off") || !strings.Contains(line, EnvEnabled) {
		t.Errorf("off: %v %s", ok, line)
	}
	// The one line also names the stores: the wiki, the faces, and the
	// bridges, on or off, and a store that does not load fails it.
	for _, want := range []string{"; pages: ", "; faces: ", "; bridges: "} {
		if _, line := check(fw.DefaultConfig()); !strings.Contains(line, want) {
			t.Errorf("the line has no %q: %s", want, line)
		}
	}
	if ok, line := check(withBlock(t, "bossmode: {enabled: true}\nbridges: {placement: sideways}\n")); ok || !strings.Contains(line, "workbench.bridges.placement") {
		t.Errorf("bridges that do not load: %v %s", ok, line)
	}
	ok, line := check(withBlock(t, "bossmode: {enabled: true}\n"))
	if !ok || !strings.Contains(line, "placeholder") || !strings.Contains(line, "names no manager") || !strings.Contains(line, "fw_bossmode_streams_open") {
		t.Errorf("embedded: %v %s", ok, line)
	}
	dir := t.TempDir()
	ok, line = check(withBlock(t, "bossmode: {enabled: true, dir: "+dir+"}\n"))
	if ok || !strings.Contains(line, "holds no index.html") {
		t.Errorf("a directory without the page: %v %s", ok, line)
	}
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("page"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := withBlock(t, "bossmode: {enabled: true, dir: "+dir+", manager: manager}\n")
	ok, line = check(cfg)
	if ok || !strings.Contains(line, "holds index.html") || !strings.Contains(line, "the manager manager is no profile") {
		t.Errorf("a manager without a profile: %v %s", ok, line)
	}
	cfg.Agents = map[string]fw.AgentProfile{"manager": {}}
	if ok, line = check(cfg); !ok || !strings.Contains(line, "the manager manager is a profile of fw.agents") {
		t.Errorf("a manager of fw.agents: %v %s", ok, line)
	}
	if ok, line = check(withBlock(t, "bossmode: {enabled: true, dir: "+filepath.Join(dir, "index.html")+"}\n")); ok || !strings.Contains(line, "is not a directory") {
		t.Errorf("a file as the directory: %v %s", ok, line)
	}
	// In a process that serves streams, the line counts them.
	fr := newFront(t, nil)
	openStream(t, fr, "ada", "")
	if _, line = check(withBlock(t, "bossmode: {enabled: true}\n")); !strings.Contains(line, "1 of at most 64 streams open") {
		t.Errorf("with a stream: %s", line)
	}
}
