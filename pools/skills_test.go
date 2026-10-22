package pools

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// writeSkill writes the files of a skill under dir.
func writeSkill(t *testing.T, dir, name string, files map[string]string) {
	t.Helper()
	for p, content := range files {
		full := filepath.Join(dir, name, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// skillsFixture is the fixture with the skills globs of the profile
// triage, or no skills entry for "".
func skillsFixture(globs string) string {
	if globs == "" {
		return fixture
	}
	return strings.Replace(fixture, "          system: You sort new tickets.\n", "          system: You sort new tickets.\n          skills: "+globs+"\n", 1)
}

// skillPaths returns the paths of the skill files of a pool.
func skillPaths(p *Pool) []string {
	var out []string
	for _, f := range p.files {
		if strings.HasPrefix(f.Path, SkillsPath+"/") {
			out = append(out, f.Path)
		}
	}
	return out
}

// The merged render ships the skills of the front that a profile of the
// pool allows, SKILL.md first, and leaves out a hidden file, a link, and a
// file that is not text. The hash covers the skills, so a changed skill
// gives a new hash, and the hash without the overlay leaves them out.
func TestPoolShipsTheSkillsOfTheFront(t *testing.T) {
	dir := t.TempDir()
	writeSkill(t, dir, "review", map[string]string{"SKILL.md": "---\nname: review\n---\nReview a change.", "scripts/run.sh": "echo review", ".hidden": "x", "bin.dat": "\xff\xfe"})
	writeSkill(t, dir, "deploy", map[string]string{"SKILL.md": "Deploy a change."})
	writeSkill(t, dir, "secret", map[string]string{"SKILL.md": "Never shipped."})
	if err := os.Symlink("/etc/hostname", filepath.Join(dir, "review", "link")); err != nil {
		t.Fatal(err)
	}
	front := testFront()
	front.SkillDirs = []string{dir}
	list, err := Load(configOf(t, skillsFixture(`[review, "dep*"]`)), front)
	if err != nil {
		t.Fatal(err)
	}
	base := list[0]
	if len(skillPaths(base)) != 0 {
		t.Fatalf("the base ships skills: %v", skillPaths(base))
	}
	merged, err := base.WithOverlay(Overlay{})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{SkillsPath + "/deploy/SKILL.md", SkillsPath + "/review/SKILL.md", SkillsPath + "/review/scripts/run.sh"}
	if got := skillPaths(merged); !slices.Equal(got, want) {
		t.Fatalf("skill files = %v", got)
	}
	if !slices.Equal(merged.Skills(), []string{"deploy", "review"}) || !slices.Contains(merged.skillSkipped, "review/link") || !slices.Contains(merged.skillSkipped, "review/bin.dat") || !slices.Contains(merged.skillSkipped, "review/.hidden") {
		t.Fatalf("skills %v, skipped %v", merged.Skills(), merged.skillSkipped)
	}
	if merged.Hash() == base.Hash() || merged.baseHash() != base.Hash() {
		t.Fatalf("hash %q base %q, pool %q", merged.Hash(), merged.baseHash(), base.Hash())
	}
	// The worker reads its skills from its home in the Task, so its
	// configuration needs no tools.skills.
	if wc := workerFileOf(t, merged); len(wc.Tools.Skills) != 0 {
		t.Fatalf("tools.skills = %v", wc.Tools.Skills)
	}
	_, ws, err := merged.Render(0, testLookup)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, f := range ws.Spec.Files {
		found = found || (f.Path == SkillsPath+"/review/scripts/run.sh" && f.Content == "echo review")
	}
	if !found {
		t.Fatal("the Workspace holds no skill file")
	}
	var out strings.Builder
	if err := Write(&out, []*Pool{merged}); err != nil || !strings.Contains(out.String(), "The front ships its skills deploy, review, which the profiles of the pool allow, to /workspace/.fw/skills of each worker.") {
		t.Fatalf("Write = %v %s", err, out.String())
	}

	// A changed skill changes the hash.
	writeSkill(t, dir, "review", map[string]string{"scripts/run.sh": "echo review again"})
	again, err := base.WithOverlay(Overlay{})
	if err != nil || again.Hash() == merged.Hash() || again.baseHash() != base.Hash() {
		t.Fatalf("a changed skill = %v, hash %q", err, again.Hash())
	}

	// A profile without a skills entry sees every skill, and an empty list
	// sees none.
	every, _ := Load(configOf(t, skillsFixture("")), front)
	m, err := every[0].WithOverlay(Overlay{})
	if err != nil || !slices.Equal(m.Skills(), []string{"deploy", "review", "secret"}) {
		t.Fatalf("every skill = %v, %v", m.Skills(), err)
	}
	none, _ := Load(configOf(t, skillsFixture("[]")), front)
	if m, err := none[0].WithOverlay(Overlay{}); err != nil || len(m.Skills()) != 0 || m.Hash() != none[0].Hash() {
		t.Fatalf("no skill = %v, %v", m.Skills(), err)
	}
	// The profile of the overlay counts too.
	o := Overlay{Profiles: []OverlayProfile{{Profile: fw.AgentProfile{Name: "helper", Skills: []string{"secret"}}}}}
	if m, err := none[0].WithOverlay(o); err != nil || !slices.Equal(m.Skills(), []string{"secret"}) {
		t.Fatalf("the skills of an overlay profile = %v, %v", m.Skills(), err)
	}
}

// A pool that ships more than the bounds refuses its merged render with
// a reason that names the bound, and a worker with skills directories of
// its own reads the skills of the Workspace too.
func TestPoolSkillBounds(t *testing.T) {
	dir := t.TempDir()
	many := map[string]string{"SKILL.md": "Many files."}
	for i := range MaxSkillFiles {
		many["f"+strconv.Itoa(i)+".txt"] = "x"
	}
	writeSkill(t, dir, "many", many)
	front := testFront()
	front.SkillDirs = []string{dir}
	list, err := Load(configOf(t, skillsFixture("[many]")), front)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := list[0].WithOverlay(Overlay{}); !errors.Is(err, ErrSkills) || !strings.Contains(err.Error(), "the skill many holds 65 files, and a pool ships at most 64 files of one skill") {
		t.Fatalf("WithOverlay of a skill with many files = %v", err)
	}

	big := t.TempDir()
	writeSkill(t, big, "large", map[string]string{"SKILL.md": "Large.", "data.txt": strings.Repeat("x", MaxSkillBytes)})
	front.SkillDirs = []string{big}
	list, _ = Load(configOf(t, skillsFixture("[large]")), front)
	if _, err := list[0].WithOverlay(Overlay{}); !errors.Is(err, ErrSkills) || !strings.Contains(err.Error(), "hold more than 1048576 bytes, the most that a pool ships") {
		t.Fatalf("WithOverlay of a large skill = %v", err)
	}

	small := t.TempDir()
	writeSkill(t, small, "small", map[string]string{"SKILL.md": "Small."})
	front.SkillDirs = []string{small}
	own := strings.Replace(skillsFixture("[small]"), "        builtin: [files, clock]\n", "        builtin: [files, clock]\n        skills: [/opt/skills]\n", 1)
	list, err = Load(configOf(t, own), front)
	if err != nil {
		t.Fatal(err)
	}
	m, err := list[0].WithOverlay(Overlay{})
	if err != nil {
		t.Fatal(err)
	}
	if wc := workerFileOf(t, m); !slices.Equal(wc.Tools.Skills, []string{"/opt/skills", "/workspace/.fw/skills"}) {
		t.Fatalf("tools.skills = %v", wc.Tools.Skills)
	}
	home := strings.Replace(skillsFixture("[small]"), "      effort: low\n", "      effort: low\n      home: /data/fw\n", 1)
	list, err = Load(configOf(t, home), front)
	if err != nil {
		t.Fatal(err)
	}
	m, err = list[0].WithOverlay(Overlay{})
	if err != nil {
		t.Fatal(err)
	}
	if wc := workerFileOf(t, m); !slices.Equal(wc.Tools.Skills, []string{"/data/fw/skills", "/workspace/.fw/skills"}) {
		t.Fatalf("tools.skills with a home = %v", wc.Tools.Skills)
	}
}
