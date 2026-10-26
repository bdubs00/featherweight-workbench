package mcp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// writeSkills writes one skill directory per name under the skills
// directory of the home.
func writeSkills(t *testing.T, home string, names ...string) {
	t.Helper()
	for _, n := range names {
		d := filepath.Join(home, "skills", n)
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		text := "---\nname: " + n + "\ndescription: The skill " + n + ".\n---\nBody.\n"
		if err := os.WriteFile(filepath.Join(d, fw.SkillFile), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// TestAgentGetResolvesSkills checks that agent_get names the skills that
// the allowlist of a profile allows, for a glob, an empty list, and no
// list, and that the field skills stays the allowlist.
func TestAgentGetResolvesSkills(t *testing.T) {
	ctx := testCtx(t)
	cfg := testConfig(t)
	writeSkills(t, cfg.Home, "review-go", "review-sql", "deploy")
	a := newAgent(t, newScript().model(), fw.WithConfig(cfg),
		fw.WithProfile(fw.AgentProfile{Name: "reviewer", Skills: []string{"review-*"}}),
		fw.WithProfile(fw.AgentProfile{Name: "none", Skills: []string{}}),
		fw.WithProfile(fw.AgentProfile{Name: "every"}),
	)
	ts := serveAgent(t, a, nil)
	cs := mustConnect(t, ctx, ts.URL, clientSetup{})
	names := func(agent string) ([]string, agentResult, string) {
		res := callTool(t, ctx, cs, toolAgentGet, map[string]any{"name": agent})
		if res.IsError {
			t.Fatalf("agent_get %s: %s", agent, textOf(res))
		}
		var out agentResult
		structured(t, res, &out)
		got := []string{}
		for _, r := range out.Agent.ResolvedSkills {
			got = append(got, r.Name)
		}
		return got, out, textOf(res)
	}
	got, out, text := names("reviewer")
	if !reflect.DeepEqual(got, []string{"review-go", "review-sql"}) || !reflect.DeepEqual(out.Agent.Skills, []string{"review-*"}) {
		t.Fatalf("reviewer = %v, allowlist %v", got, out.Agent.Skills)
	}
	if out.Agent.ResolvedSkills[0].Description != "The skill review-go." {
		t.Fatalf("ref = %+v", out.Agent.ResolvedSkills[0])
	}
	if !strings.Contains(text, "It can load these skills of the server: review-go, review-sql.") {
		t.Fatalf("text = %q", text)
	}
	got, out, text = names("none")
	if len(got) != 0 || out.Agent.Skills == nil || !strings.Contains(text, "It can load no skill of the server.") {
		t.Fatalf("none = %v, allowlist %v, text %q", got, out.Agent.Skills, text)
	}
	got, out, _ = names("every")
	if len(got) != 3 || out.Agent.Skills != nil {
		t.Fatalf("every = %v, allowlist %v", got, out.Agent.Skills)
	}
	// The list of an empty allowlist is [] in the JSON, never null.
	res := callTool(t, ctx, cs, toolAgentGet, map[string]any{"name": "none"})
	b, _ := json.Marshal(res.StructuredContent)
	if !strings.Contains(string(b), `"resolvedSkills":[]`) || !strings.Contains(string(b), `"skills":[]`) {
		t.Fatalf("structured = %s", b)
	}
}
