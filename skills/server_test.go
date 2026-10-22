package skills

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// serverSkills is a tool source that serves skills, as an MCP server with
// the skills extension does, and counts the reads of each SKILL.md.
type serverSkills struct {
	mu    sync.Mutex
	text  map[string]string
	reads map[string]int
	fail  bool
}

func (s *serverSkills) Tools(context.Context) ([]fw.Tool, error) { return nil, nil }

func (s *serverSkills) SourceSkills(context.Context) []fw.Skill {
	return []fw.Skill{
		{Name: "gh/review", Description: "Reviews a pull request.", Origin: fw.SkillOriginMCP, Server: "gh", URI: "skill://gh/review/SKILL.md"},
		{Name: "gh/hidden", Description: "Only for agents.", Origin: fw.SkillOriginMCP, Server: "gh", URI: "skill://gh/hidden/SKILL.md"},
	}
}

func (s *serverSkills) ReadSkill(_ context.Context, sk fw.Skill, file string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reads[sk.Name+"|"+file]++
	if s.fail {
		return "", errors.New("the server does not answer")
	}
	return s.text[sk.Name], nil
}

func (s *serverSkills) SkillFiles(fw.Skill) []string { return []string{"checklist.md"} }

func (s *serverSkills) count(name string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reads[name+"|"]
}

// A skill of an MCP server is in the catalog with the origin mcp, its
// server, and its URI, and the catalog reads no file of it. The allowlist
// matches it by its qualified name. A slash command reads it at the moment
// of use, with the keys of its front matter, and a server that does not
// answer gives ErrUnavailable.
func TestServerSkills(t *testing.T) {
	src := &serverSkills{reads: map[string]int{}, text: map[string]string{
		"gh/review": "---\nname: review\narguments: [pr]\n---\nReview pull request $pr.\n",
		"gh/hidden": "---\nname: hidden\nuser-invocable: false\n---\nBody.\n",
	}}
	a, root, _ := newAgent(t, fw.WithToolSources(src))
	writeSkill(t, root, "local", "---\nname: local\n---\nBody.\n", nil)
	var review Entry
	for _, e := range Catalog(a) {
		if e.Name == "gh/review" {
			review = e
		}
	}
	if review.Origin != OriginMCP || review.Server != "gh" || review.URI != "skill://gh/review/SKILL.md" || len(review.Files) != 1 || !review.UserInvocable {
		t.Fatalf("server skill = %+v", review)
	}
	if n := src.count("gh/review"); n != 0 {
		t.Fatalf("the catalog read the server skill %d times", n)
	}
	refs := Allowed(a, []string{"gh/*"})
	if len(refs) != 2 || refs[0].Server != "gh" || refs[0].Origin != OriginMCP {
		t.Fatalf("allowed = %+v", refs)
	}
	x, err := Prepare(a, Command{Name: "gh/review", Args: "42"}, "")
	if err != nil || x.Message != "Skill: gh/review\n\nReview pull request 42." || src.count("gh/review") != 1 {
		t.Fatalf("prepare = %+v, %v, %d reads", x, err, src.count("gh/review"))
	}
	if _, err := Prepare(a, Command{Name: "gh/hidden"}, ""); !errors.Is(err, ErrNotInvocable) {
		t.Fatalf("hidden = %v", err)
	}
	src.mu.Lock()
	src.fail = true
	src.mu.Unlock()
	if _, err := Prepare(a, Command{Name: "gh/review"}, ""); !errors.Is(err, ErrUnavailable) || !strings.Contains(err.Error(), "does not answer") {
		t.Fatalf("down = %v", err)
	}
	// A local skill stays local, and an import never takes a name with a
	// slash.
	if e, ok := Named(a, "local"); !ok || e.Origin != OriginLocal || e.Server != "" {
		t.Fatalf("local = %+v", e)
	}
	im := newImporter(t, a, newHost(t), nil)
	if p, _ := im.Preview(context.Background(), Source{Text: "---\nname: gh/review\n---\nB.\n"}); p.Installable {
		t.Fatalf("an import takes a qualified name: %+v", p)
	}
}
