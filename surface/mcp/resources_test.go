package mcp

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/bdubs00/featherweight-kernel/fw"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// skillServer is a tool source that serves skills as an MCP server with
// the skills extension does: each skill under <server>/<name>, read at the
// moment of use.
type skillServer struct {
	name   string
	skills map[string]string // name without the server -> SKILL.md

	mu    sync.Mutex
	reads []string
}

func (s *skillServer) Tools(context.Context) ([]fw.Tool, error) { return nil, nil }

func (s *skillServer) SourceSkills(context.Context) []fw.Skill {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []fw.Skill
	for name := range s.skills {
		out = append(out, fw.Skill{
			Name:        s.name + "/" + name,
			Description: "The server skill " + name + ".",
			Origin:      fw.SkillOriginMCP,
			Server:      s.name,
			URI:         "skill://" + s.name + "/" + name + "/SKILL.md",
		})
	}
	return out
}

func (s *skillServer) ReadSkill(_ context.Context, sk fw.Skill, file string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reads = append(s.reads, sk.Name)
	if file != "" {
		return "", os.ErrNotExist
	}
	return s.skills[strings.TrimPrefix(sk.Name, s.name+"/")], nil
}

// add serves one more skill, as a server does after list_changed.
func (s *skillServer) add(name, body string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.skills[name] = body
}

func (s *skillServer) SkillFiles(fw.Skill) []string { return nil }

func (s *skillServer) readCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.reads)
}

// The skill resources list the skills of the agent as the kernel serves
// them, the skills of an MCP server included, and a name with the slash of
// a server reads, escaped or not.
func TestSkillResourcesOfServer(t *testing.T) {
	ctx := testCtx(t)
	cfg := testConfig(t)
	dir := filepath.Join(cfg.Home, "skills", "deploy")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\nname: deploy\ndescription: Deploy the service\n---\n# Deploy\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	src := &skillServer{name: "github", skills: map[string]string{"review": "---\nname: review\ndescription: Review a pull request\n---\n# Review from the server\n"}}
	a := newAgent(t, newScript(endTurn("hello")).model(), fw.WithConfig(cfg), fw.WithToolSources(src))
	cs := mustConnect(t, ctx, serveAgent(t, a, nil).URL, clientSetup{})

	list, err := cs.ListResources(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var uris []string
	for _, r := range list.Resources {
		if strings.HasPrefix(r.URI, skillPrefix) {
			uris = append(uris, r.URI)
		}
	}
	if strings.Join(uris, " ") != "fw://skills/deploy fw://skills/github/review" {
		t.Fatalf("skill resources = %v", uris)
	}
	if n := src.readCount(); n != 0 {
		t.Errorf("the list read %d skills of the server; it reads none ahead of need", n)
	}
	for _, uri := range []string{"fw://skills/github/review", "fw://skills/github%2Freview"} {
		res, err := cs.ReadResource(ctx, &sdk.ReadResourceParams{URI: uri})
		if err != nil || len(res.Contents) != 1 || res.Contents[0].Text != src.skills["review"] || res.Contents[0].URI != uri {
			t.Fatalf("%s = %+v, %v", uri, res, err)
		}
	}
	if n := src.readCount(); n != 2 {
		t.Errorf("reads of the server = %d, want one per read of the resource", n)
	}
	res, err := cs.ReadResource(ctx, &sdk.ReadResourceParams{URI: "fw://skills/deploy"})
	if err != nil || !strings.Contains(res.Contents[0].Text, "# Deploy") {
		t.Fatalf("local skill = %+v, %v", res, err)
	}
	// A skill that the server serves after the start is in no list, and
	// the template reads it with its slash.
	src.add("triage", "# Triage\n")
	res, err = cs.ReadResource(ctx, &sdk.ReadResourceParams{URI: "fw://skills/github/triage"})
	if err != nil || res.Contents[0].Text != "# Triage\n" {
		t.Fatalf("later skill = %+v, %v", res, err)
	}
	for _, uri := range []string{"fw://skills/github/other", "fw://skills/github", "fw://skills/github/", "fw://skills//review", "fw://skills/github/../deploy", "fw://skills/github%2F..%2Fdeploy", "fw://skills/%zz"} {
		if _, err := cs.ReadResource(ctx, &sdk.ReadResourceParams{URI: uri}); err == nil {
			t.Errorf("%s was readable", uri)
		}
	}
	tmpl, err := cs.ListResourceTemplates(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, rt := range tmpl.ResourceTemplates {
		if rt.Name == "skill" {
			found = rt.URITemplate == "fw://skills/{+name}" && strings.Contains(rt.Description, "<server>/<name>")
		}
	}
	if !found {
		t.Errorf("skill template = %+v", tmpl.ResourceTemplates)
	}
}

func TestSkillFromURI(t *testing.T) {
	for uri, want := range map[string]string{
		"fw://skills/review":           "review",
		"fw://skills/github/review":    "github/review",
		"fw://skills/github%2Freview":  "github/review",
		"fw://skills/a%20b":            "a b",
		"fw://skills/":                 "",
		"fw://skills/..":               "",
		"fw://skills/%2E%2E":           "",
		"fw://skills/github/./review":  "",
		"fw://skills/github//review":   "",
		"fw://skills/a%0Ab":            "",
		"fw://runs/01A":                "",
		"fw://skills/github/review/x/": "",
	} {
		got, ok := skillFromURI(uri)
		if ok != (want != "") || got != want {
			t.Errorf("skillFromURI(%q) = %q, %v", uri, got, ok)
		}
	}
	if got := skillURI("github/a b"); got != "fw://skills/github/a%20b" {
		t.Errorf("skillURI = %q", got)
	}
}
