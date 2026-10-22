package fleet

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/bdubs00/featherweight-kernel/config"
	"github.com/bdubs00/featherweight-workbench/pools"
)

// A pool of the file ships the skills of the directories that the front
// reads, fw.Config.SkillDirs: with tools.skills, each of its directories,
// the first one included, where an admin imports a skill, and not
// $FW_HOME/skills, which tools.skills replaces.
func TestPoolShipsTheSkillsOfToolsSkills(t *testing.T) {
	cfg, err := config.Load(filepath.Join("..", "examples", "pools", "fw.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Home = t.TempDir()
	first, second := t.TempDir(), t.TempDir()
	cfg.Tools.Skills = []string{first, second}
	for dir, name := range map[string]string{first: "imported", second: "review", filepath.Join(cfg.Home, "skills"): "home"} {
		if err := os.MkdirAll(filepath.Join(dir, name), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name, "SKILL.md"), []byte("---\nname: "+name+"\ndescription: The skill "+name+".\n---\nDo it.\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if !slices.Equal(cfg.SkillDirs(), []string{first, second}) {
		t.Fatalf("SkillDirs = %v", cfg.SkillDirs())
	}
	fc, err := Load(cfg)
	if err != nil {
		t.Fatal(err)
	}
	list, err := LoadPools(cfg, fc)
	if err != nil || len(list) != 1 {
		t.Fatalf("pools = %v, %v", list, err)
	}
	merged, err := list[0].WithOverlay(pools.Overlay{})
	if err != nil {
		t.Fatal(err)
	}
	if got := merged.Skills(); !slices.Equal(got, []string{"imported", "review"}) {
		t.Fatalf("the pool ships %v", got)
	}
}
