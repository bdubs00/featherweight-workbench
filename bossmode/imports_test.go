package bossmode

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/skills"
	"github.com/bdubs00/featherweight-workbench/surface/mcp"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

const lintSkill = "---\nname: lint\ndescription: Lints the change.\n---\nLint $ARGUMENTS.\n"

// importFront is a front whose admin is ada, with one configured skill,
// taken, and the marketplace team on a test server.
func importFront(t *testing.T) *front {
	t.Helper()
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	mux.HandleFunc("/lint/SKILL.md", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(lintSkill)) })
	mux.HandleFunc("/index.json", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]map[string]string{{"name": "lint", "description": "Lints the change.", "url": srv.URL + "/lint/SKILL.md"}})
	})
	t.Setenv(EnvMarketplaces, "team="+srv.URL+"/index.json")
	fr := newFront(t, func(c *fw.Config) {
		c.Access.Admins = []string{"ada"}
		c.Agents["manager"] = fw.AgentProfile{Access: &fw.Access{Operators: []string{"ada", "bob"}}}
	})
	putSkill(t, fr.a, "taken", "---\nname: taken\n---\nBody.\n")
	fr.h.imp.Fetcher().AllowPrivate = true
	return fr
}

// An admin lists the marketplaces, previews a skill, installs it, replaces
// it, and removes it; the catalog lists it between the install and the
// removal; another person gets 403 on each route.
func TestImportRoutes(t *testing.T) {
	fr := importFront(t)
	resp, body := get(t, fr.url+"/v1/bossmode/marketplaces", "ada")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `"name":"team"`) || !strings.Contains(body, `"kind":"manifest"`) || !strings.Contains(body, `"name":"lint"`) {
		t.Fatalf("marketplaces = %d %s", resp.StatusCode, body)
	}
	resp, body = send(t, http.MethodPost, fr.url+"/v1/bossmode/skills/preview", "ada", `{"marketplace":"team","name":"lint"}`)
	var p skills.Preview
	if resp.StatusCode != http.StatusOK || json.Unmarshal([]byte(body), &p) != nil || p.Name != "lint" || p.Body != "Lint $ARGUMENTS.\n" || !p.Installable || p.Digest == "" {
		t.Fatalf("preview = %d %s", resp.StatusCode, body)
	}
	if _, body := get(t, fr.url+"/v1/bossmode/skills", "ada"); strings.Contains(body, `"lint"`) {
		t.Fatalf("a preview installed the skill: %s", body)
	}

	if resp, body := send(t, http.MethodPost, fr.url+"/v1/bossmode/skills", "ada", `{"marketplace":"team","name":"lint"}`); resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, "digest") {
		t.Fatalf("install without the digest = %d %s", resp.StatusCode, body)
	}
	if resp, body := send(t, http.MethodPost, fr.url+"/v1/bossmode/skills", "ada", `{"marketplace":"team","name":"lint","digest":"sha256:00"}`); resp.StatusCode != http.StatusConflict || !strings.Contains(body, "preview") {
		t.Fatalf("install with another digest = %d %s", resp.StatusCode, body)
	}
	resp, body = send(t, http.MethodPost, fr.url+"/v1/bossmode/skills", "ada", `{"marketplace":"team","name":"lint","digest":"`+p.Digest+`"}`)
	var in Installed
	if resp.StatusCode != http.StatusCreated || json.Unmarshal([]byte(body), &in) != nil || in.Skill.Name != "lint" || in.Skill.Origin != skills.OriginImported || in.Replaced {
		t.Fatalf("install = %d %s", resp.StatusCode, body)
	}
	_, body = get(t, fr.url+"/v1/bossmode/skills", "ada")
	var c Catalog
	_ = json.Unmarshal([]byte(body), &c)
	if len(c.Skills) != 2 || c.Skills[0].Name != "lint" || c.Skills[0].Origin != skills.OriginImported {
		t.Fatalf("catalog after the install = %s", body)
	}
	if resp, body := send(t, http.MethodPost, fr.url+"/v1/bossmode/skills", "ada", `{"text":`+jsonText(lintSkill)+`}`); resp.StatusCode != http.StatusConflict || !strings.Contains(body, "replace") {
		t.Fatalf("second install = %d %s", resp.StatusCode, body)
	}
	resp, body = send(t, http.MethodPost, fr.url+"/v1/bossmode/skills", "ada", `{"text":`+jsonText(strings.Replace(lintSkill, "Lint", "Check", 1))+`,"replace":true}`)
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `"replaced":true`) {
		t.Fatalf("replace = %d %s", resp.StatusCode, body)
	}
	for _, c := range []struct {
		body   string
		status int
	}{
		{`{"text":"No name.\n"}`, http.StatusBadRequest},
		{`{"text":"---\nname: taken\n---\nB.\n","replace":true}`, http.StatusConflict},
		{`{"files":[{"path":"SKILL.md","text":` + jsonText(strings.Repeat("x", skills.MaxImportBytes+1)) + `}]}`, http.StatusRequestEntityTooLarge},
		{`{"url":"` + fr.url + `/v1/whoami","digest":"sha256:00"}`, http.StatusBadGateway},
		{`{}`, http.StatusBadRequest},
		{`{"text":"x","unknown":1}`, http.StatusBadRequest},
	} {
		resp, body := send(t, http.MethodPost, fr.url+"/v1/bossmode/skills", "ada", c.body)
		if resp.StatusCode != c.status {
			t.Errorf("install %.60s = %d %s, want %d", c.body, resp.StatusCode, body, c.status)
		}
	}

	for _, c := range []struct {
		method, path, body string
	}{
		{http.MethodGet, "/v1/bossmode/marketplaces", ""},
		{http.MethodPost, "/v1/bossmode/skills/preview", `{"text":"x"}`},
		{http.MethodPost, "/v1/bossmode/skills", `{"text":"x"}`},
		{http.MethodDelete, "/v1/bossmode/skills/lint", ""},
	} {
		if resp, body := send(t, c.method, fr.url+c.path, "bob", c.body); resp.StatusCode != http.StatusForbidden || !strings.Contains(body, "access.admins") {
			t.Errorf("bob %s %s = %d %s", c.method, c.path, resp.StatusCode, body)
		}
	}

	if resp, body := send(t, http.MethodDelete, fr.url+"/v1/bossmode/skills/taken", "ada", ""); resp.StatusCode != http.StatusConflict {
		t.Fatalf("remove a configured skill = %d %s", resp.StatusCode, body)
	}
	if resp, body := send(t, http.MethodDelete, fr.url+"/v1/bossmode/skills/lint", "ada", ""); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("remove = %d %s", resp.StatusCode, body)
	}
	if resp, _ := send(t, http.MethodDelete, fr.url+"/v1/bossmode/skills/lint", "ada", ""); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("remove again = %d", resp.StatusCode)
	}
	if _, body := get(t, fr.url+"/v1/bossmode/skills", "ada"); strings.Contains(body, `"lint"`) {
		t.Fatalf("the catalog lists a removed skill: %s", body)
	}
}

func jsonText(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// skill_preview and skill_install work for an admin, refuse another
// person, and refuse a call of a run of the fleet.
func TestImportTools(t *testing.T) {
	fr := importFront(t)
	cs := connectMCP(t, fr.a, "ada")
	res := callTool(t, cs, ToolSkillPreview, map[string]any{"text": lintSkill}, nil)
	if res.IsError || !strings.Contains(textOf(res), "Preview of the skill lint: Lints the change.") || !strings.Contains(textOf(res), "Lint $ARGUMENTS.") {
		t.Fatalf("skill_preview = %s", textOf(res))
	}
	if res := callTool(t, cs, ToolSkillInstall, map[string]any{"marketplace": "team", "name": "lint"}, nil); !res.IsError || !strings.Contains(textOf(res), "digest") {
		t.Fatalf("skill_install without the digest = %s", textOf(res))
	}
	var pv skills.Preview
	b, _ := json.Marshal(callTool(t, cs, ToolSkillPreview, map[string]any{"marketplace": "team", "name": "lint"}, nil).StructuredContent)
	_ = json.Unmarshal(b, &pv)
	res = callTool(t, cs, ToolSkillInstall, map[string]any{"marketplace": "team", "name": "lint", "digest": pv.Digest}, nil)
	if res.IsError || !strings.Contains(textOf(res), "Installed the skill lint") {
		t.Fatalf("skill_install = %s", textOf(res))
	}
	res = callTool(t, cs, ToolSkillInstall, map[string]any{"text": lintSkill}, nil)
	if !res.IsError || !strings.Contains(textOf(res), "Conflict:") {
		t.Fatalf("second skill_install = %s", textOf(res))
	}
	res = callTool(t, cs, ToolSkillInstall, map[string]any{"text": lintSkill, "replace": true}, nil)
	if res.IsError || !strings.Contains(textOf(res), "Replaced the skill lint") {
		t.Fatalf("replace = %s", textOf(res))
	}
	for _, name := range []string{ToolSkillPreview, ToolSkillInstall} {
		if res := callTool(t, connectMCP(t, fr.a, "bob"), name, map[string]any{"text": lintSkill}, nil); !res.IsError || !strings.Contains(textOf(res), "access.admins") {
			t.Errorf("%s for bob = %s", name, textOf(res))
		}
		res := callTool(t, connectMCP(t, fr.a, staticToken), name, map[string]any{"text": lintSkill}, sdk.Meta{mcp.MetaRun: "worker-run", mcp.MetaAgent: "manager"})
		if !res.IsError || !strings.Contains(textOf(res), "A run of the fleet imports no skill") {
			t.Errorf("%s for a run = %s", name, textOf(res))
		}
	}
}
