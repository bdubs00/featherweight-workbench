package skills

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// The catalog names the origin of each skill as GET /v1/skills of the
// kernel names it, local for a skill of a skills directory and mcp for a
// skill of a server, and only a skill with the marker of an import reads
// imported in the place of local.
func TestCatalogOriginIsTheOriginOfTheKernel(t *testing.T) {
	src := &serverSkills{reads: map[string]int{}, text: map[string]string{}}
	a, root, _ := newAgent(t, fw.WithToolSources(src))
	writeSkill(t, root, "plain", "---\nname: plain\n---\nBody.\n", nil)
	writeSkill(t, root, "fetched", "---\nname: fetched\n---\nBody.\n", nil)
	if err := os.WriteFile(filepath.Join(root, "fetched", OriginFile), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	kernel := map[string]string{}
	for _, s := range a.Skills() {
		kernel[s.Name] = s.OriginName()
	}
	want := map[string]string{"plain": OriginLocal, "fetched": OriginImported, "gh/review": OriginMCP, "gh/hidden": OriginMCP}
	got := map[string]string{}
	for _, e := range Catalog(a) {
		got[e.Name] = e.Origin
		if e.Origin != OriginImported && e.Origin != kernel[e.Name] {
			t.Errorf("%s: the catalog says %q, the kernel %q", e.Name, e.Origin, kernel[e.Name])
		}
	}
	if len(got) != len(want) {
		t.Fatalf("catalog = %v", got)
	}
	for name, origin := range want {
		if got[name] != origin {
			t.Errorf("%s: origin %q, want %q", name, got[name], origin)
		}
	}
	if OriginLocal != "local" || kernel["fetched"] != "local" {
		t.Errorf("the local origin = %q, the kernel names the import %q", OriginLocal, kernel["fetched"])
	}
}
