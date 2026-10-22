package bridges

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/skills"
)

// writeSkill writes a skill into the skills directory of the home of the
// agent.
func writeSkill(t *testing.T, s *Store, name, text string) {
	t.Helper()
	d := filepath.Join(s.a.Config().Home, "skills", name)
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, fw.SkillFile), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A message with skill reaches every member as the expanded skill, pinned,
// from the person, and the channel holds the line Skill, a summary, and
// the command.
func TestSlashCommandInABridge(t *testing.T) {
	s, _, fm, url, id := fanBridge(t, "")
	writeSkill(t, s, "fix", "---\nname: fix\ndescription: Fixes an issue.\narguments: [issue]\n---\nFix issue $issue and test it.\n")
	resp, out := do(t, http.MethodPost, url+"/v1/bridges/"+id+"/messages", "alice", `{"skill":{"name":"fix","args":"42"},"text":"Then report."}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("post = %d %s", resp.StatusCode, out)
	}
	var m Message
	decodeJSON(t, out, &m)
	if m.Skill == nil || *m.Skill != (skills.Command{Name: "fix", Args: "42"}) || !m.Cut || m.From.Kind != KindPerson || m.From.Name != "alice" {
		t.Fatalf("entry = %+v", m)
	}
	if m.Text != "Skill: fix\nFixes an issue. arguments: 42. Then report." {
		t.Fatalf("entry text = %q", m.Text)
	}
	if !strings.Contains(out, `"skill":{"name":"fix","args":"42"}`) {
		t.Fatalf("entry JSON = %s", out)
	}
	eventually(t, "both members get the skill", func() bool {
		return len(fm.find(http.MethodPost, pathMig)) == 1 && len(fm.find(http.MethodPost, pathRev)) == 1
	})
	got := fm.find(http.MethodPost, pathRev)[0]
	content, _ := got.Body["content"].(string)
	from, _ := got.Body["from"].(map[string]any)
	if got.Body["pinned"] != true || from["kind"] != fw.FromPerson || from["name"] != "alice" {
		t.Fatalf("delivery = %+v", got.Body)
	}
	want := `[bridge "Migrate logrotate" ` + id + `] to everyone from alice: Skill: fix` + "\n\nFix issue 42 and test it.\n\nThen report."
	if content != want {
		t.Fatalf("content = %q, want %q", content, want)
	}
	// A plain message is no pinned message.
	do(t, http.MethodPost, url+"/v1/bridges/"+id+"/messages", "alice", `{"text":"Thanks.","to":["reviewer"]}`)
	eventually(t, "the reviewer gets the plain message", func() bool { return len(fm.find(http.MethodPost, pathRev)) == 2 })
	if p, ok := fm.find(http.MethodPost, pathRev)[1].Body["pinned"]; ok {
		t.Fatalf("a plain message is pinned: %v", p)
	}
}

// A skill that the catalog does not hold answers 404, a skill whose front
// matter says user-invocable: false answers 400, an expansion over 64 KB
// answers 413, and a run cannot post a skill.
func TestSlashCommandRefusals(t *testing.T) {
	s, _, fm, url, id := fanBridge(t, "")
	writeSkill(t, s, "hidden", "---\nname: hidden\nuser-invocable: false\n---\nBody.\n")
	writeSkill(t, s, "huge", "---\nname: huge\n---\n"+strings.Repeat("x", skills.MaxMessageBytes+1))
	for _, c := range []struct {
		body   string
		status int
		words  string
	}{
		{`{"skill":{"name":"nope"}}`, http.StatusNotFound, `no skill has the name \"nope\"`},
		{`{"skill":{"name":"hidden"}}`, http.StatusBadRequest, "user-invocable: false"},
		{`{"skill":{"name":"huge"}}`, http.StatusRequestEntityTooLarge, "at most 65536"},
		{`{"skill":{"name":""}}`, http.StatusBadRequest, "needs the name of a skill"},
		{`{}`, http.StatusBadRequest, "a message needs a text or a skill"},
	} {
		resp, out := do(t, http.MethodPost, url+"/v1/bridges/"+id+"/messages", "alice", c.body)
		if resp.StatusCode != c.status || !strings.Contains(out, c.words) {
			t.Errorf("%s = %d %s, want %d with %q", c.body, resp.StatusCode, out, c.status, c.words)
		}
	}
	if _, err := s.post(context.Background(), id, sender{handle: "migrator-1", run: "r-mig"}, PostSpec{Skill: &skills.Command{Name: "hidden"}}); !errIs(err, fw.ErrInvalidRequest) {
		t.Fatalf("a run posts a skill: %v", err)
	}
	if n := len(fm.find(http.MethodPost, pathMig)); n != 0 {
		t.Fatalf("a refused skill reached a member %d times", n)
	}
}

// A member whose pinned messages are full refuses the skill, and the
// channel says so in a line that names the member and the bound.
func TestSlashCommandPinnedBound(t *testing.T) {
	s, _, fm, url, id := fanBridge(t, "")
	writeSkill(t, s, "fix", "---\nname: fix\n---\nFix it.\n")
	fm.mu.Lock()
	fm.status[pathRev] = http.StatusBadRequest
	fm.detail[pathRev] = "fw: invalid request: the pinned messages of run r-rev would hold 70000 bytes, and the bound is 65536 bytes (64 KB)"
	fm.mu.Unlock()
	resp, out := do(t, http.MethodPost, url+"/v1/bridges/"+id+"/messages", "alice", `{"skill":{"name":"fix"},"to":["reviewer"]}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("post = %d %s", resp.StatusCode, out)
	}
	eventually(t, "the channel says that the reviewer did not get the skill", func() bool {
		for _, m := range withEvent(channel(t, s, id), EventFailed) {
			if strings.Contains(m.Text, "reviewer did not get message") && strings.Contains(m.Text, "the bound is 65536 bytes") {
				return true
			}
		}
		return false
	})
}
