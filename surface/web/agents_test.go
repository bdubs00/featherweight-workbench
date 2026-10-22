package web

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/surface/view"
	"github.com/bdubs00/featherweight-workbench/agents"
)

// PUT and DELETE /v1/agents/{name} create, replace, and remove a profile
// with the rights of the caller.
func TestPutAndDeleteAgent(t *testing.T) {
	a := manageAgent(t, newScript().model())
	srv := serve(t, a)
	url := srv.URL + "/v1/agents/reviewer"

	// A stranger gets a 403 that names the role.
	resp, b := do(t, "PUT", url, `{"description":"mine"}`, "Authorization", asBob)
	if p := decodeProblem(t, resp, b, http.StatusForbidden); !strings.Contains(p.Detail, "bob agent.edit needs an owner entry on the agent reviewer") {
		t.Fatalf("problem = %+v", p)
	}
	resp, b = do(t, "PUT", srv.URL+"/v1/agents/bobs", `{}`, "Authorization", asBob)
	if p := decodeProblem(t, resp, b, http.StatusForbidden); !strings.Contains(p.Detail, "access.creators") {
		t.Fatalf("problem = %+v", p)
	}
	resp, b = do(t, "DELETE", url, "", "Authorization", asBob)
	if p := decodeProblem(t, resp, b, http.StatusForbidden); !strings.Contains(p.Detail, "owner entry") {
		t.Fatalf("problem = %+v", p)
	}

	// The owner replaces the profile; the access block survives.
	var v view.AgentView
	resp, b = do(t, "PUT", url, `{"description":"Reviews pull requests","system":"You review.","tools":["files"],"skills":[],"effort":"low","timeZone":"Europe/Berlin","policy":{"default":"ask"},"budget":{"maxTurns":5,"deadline":600000000000}}`, "Authorization", asAlice)
	decodeJSON(t, resp, b, http.StatusOK, &v)
	if v.Name != "reviewer" || v.Description != "Reviews pull requests" || v.Role != "owner" || v.Access == nil || v.Access.Owners[0] != "alice" || v.Access.Viewers[0] != "bob" ||
		v.Skills == nil || len(v.Skills) != 0 || v.Budget == nil || v.Budget.Deadline.Minutes() != 10 || v.Policy.Default != "ask" || v.TimeZone != "Europe/Berlin" {
		t.Fatalf("put = %+v", v)
	}
	// GET, a kernel route, returns the time zone.
	resp, b = do(t, "GET", url, "", "Authorization", asAlice)
	if !strings.Contains(string(b), `"timeZone":"Europe/Berlin"`) {
		t.Fatalf("get = %d %s", resp.StatusCode, b)
	}
	// A new profile answers 201 and names the creator as its owner.
	resp, b = do(t, "PUT", srv.URL+"/v1/agents/fresh", `{"name":"fresh","description":"new"}`, "Authorization", asAlice)
	decodeJSON(t, resp, b, http.StatusCreated, &v)
	if resp.Header.Get("Location") != "/v1/agents/fresh" || v.Access == nil || v.Access.Owners[0] != "alice" || v.Role != "owner" {
		t.Fatalf("create = %+v, location %q", v, resp.Header.Get("Location"))
	}
	// Bad bodies are 400 problems.
	for name, body := range map[string]string{
		"other name": `{"name":"other"}`,
		"effort":     `{"effort":"turbo"}`,
		"policy":     `{"policy":{"default":"maybe"}}`,
		"unknown":    `{"colour":"blue"}`,
		"model":      `{"model":"nope"}`,
		"time zone":  `{"timeZone":"Mars/Olympus"}`,
	} {
		resp, b = do(t, "PUT", srv.URL+"/v1/agents/fresh", body, "Authorization", asAlice)
		p := decodeProblem(t, resp, b, http.StatusBadRequest)
		if name == "time zone" && !strings.Contains(p.Detail, "timeZone") {
			t.Errorf("time zone problem = %+v", p)
		}
	}
	resp, b = do(t, "PUT", srv.URL+"/v1/agents/..bad", `{}`, "Authorization", asAlice)
	decodeProblem(t, resp, b, http.StatusBadRequest)

	// Delete: 404 for an unknown name, 204 for the owner, then 404 again.
	resp, b = do(t, "DELETE", srv.URL+"/v1/agents/nope", "", "Authorization", asAlice)
	if p := decodeProblem(t, resp, b, http.StatusNotFound); !strings.Contains(p.Detail, "nope") {
		t.Fatalf("problem = %+v", p)
	}
	resp, b = do(t, "DELETE", srv.URL+"/v1/agents/fresh", "", "Authorization", asAlice)
	if resp.StatusCode != http.StatusNoContent || len(b) != 0 {
		t.Fatalf("delete = %d %s", resp.StatusCode, b)
	}
	resp, b = do(t, "GET", srv.URL+"/v1/agents/fresh", "", "Authorization", asAlice)
	decodeProblem(t, resp, b, http.StatusNotFound)
}

func TestPutAgentDryRun(t *testing.T) {
	a := manageAgent(t, newScript().model())
	srv := serve(t, a)

	// A dry run of a new profile says what it can do and saves nothing.
	var d agents.Draft
	resp, b := do(t, "PUT", srv.URL+"/v1/agents/fresh?dryRun=true", `{"description":"new","tools":["files"]}`, "Authorization", asAlice)
	decodeJSON(t, resp, b, http.StatusOK, &d)
	if d.Exists || d.Name != "fresh" || d.Role != "owner" {
		t.Fatalf("draft = %+v", d)
	}
	if _, ok := a.ProfileNamed("fresh"); ok {
		t.Fatal("a dry run saved the profile")
	}
	// An existing profile reports exists and the role of the caller.
	resp, b = do(t, "PUT", srv.URL+"/v1/agents/reviewer?dryRun=true", `{"description":"changed"}`, "Authorization", asAlice)
	decodeJSON(t, resp, b, http.StatusOK, &d)
	if !d.Exists || d.Role != "owner" {
		t.Fatalf("draft of reviewer = %+v", d)
	}
	if p, _ := a.ProfileNamed("reviewer"); p.Description != "Reviews code" {
		t.Fatalf("a dry run changed reviewer: %+v", p)
	}
	// A dry run checks the rights as a save does.
	resp, b = do(t, "PUT", srv.URL+"/v1/agents/reviewer?dryRun=true", `{}`, "Authorization", asBob)
	decodeProblem(t, resp, b, http.StatusForbidden)
}

// Without pools, ?pool= of the agent routes is a 400, and the route of an
// approval rule answers 409, because a rule needs a pool.
func TestAgentRoutesWithoutPools(t *testing.T) {
	a := manageAgent(t, newScript().model())
	srv := serve(t, a)
	resp, b := do(t, "PUT", srv.URL+"/v1/agents/reviewer?pool=triage", `{"description":"mine"}`, "Authorization", asAlice)
	if p := decodeProblem(t, resp, b, http.StatusBadRequest); !strings.Contains(p.Detail, "keeps no pool") {
		t.Fatalf("problem = %+v", p)
	}
	resp, b = do(t, "DELETE", srv.URL+"/v1/agents/reviewer?pool=triage", "", "Authorization", asAlice)
	decodeProblem(t, resp, b, http.StatusBadRequest)
	resp, b = do(t, "POST", srv.URL+"/v1/runs/01RUN/approvals/01ASK/rule", `{"note":"why"}`, "Authorization", asAlice)
	if p := decodeProblem(t, resp, b, http.StatusConflict); !strings.Contains(p.Detail, "approval rules need a pool") {
		t.Fatalf("problem = %+v", p)
	}
}

// TestAgentSkillsRoute checks GET /v1/agents/{name}/skills: the skills
// of the front that a glob allows, none for an empty list, every skill
// without a list, and 404 for an agent that does not exist.
func TestAgentSkillsRoute(t *testing.T) {
	cfg := fw.DefaultConfig()
	cfg.Home = t.TempDir()
	for _, n := range []string{"review-go", "review-sql", "deploy"} {
		d := filepath.Join(cfg.Home, "skills", n)
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, fw.SkillFile), []byte("---\nname: "+n+"\ndescription: "+n+" text\n---\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	a := newAgent(t, newScript().model(), fw.WithConfig(cfg),
		fw.WithProfile(fw.AgentProfile{Name: "reviewer", Skills: []string{"review-*"}}),
		fw.WithProfile(fw.AgentProfile{Name: "none", Skills: []string{}}),
		fw.WithProfile(fw.AgentProfile{Name: "every"}),
	)
	srv := serve(t, a)
	read := func(name string) AgentSkills {
		resp, b := do(t, http.MethodGet, srv.URL+"/v1/agents/"+name+"/skills", "")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: %d %s", name, resp.StatusCode, b)
		}
		var out AgentSkills
		if err := json.Unmarshal(b, &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	r := read("reviewer")
	if len(r.Skills) != 2 || r.Skills[0].Name != "review-go" || r.Skills[0].Description != "review-go text" || !reflect.DeepEqual(r.Allowlist, []string{"review-*"}) {
		t.Fatalf("reviewer = %+v", r)
	}
	if n := read("none"); len(n.Skills) != 0 || n.Skills == nil || n.Allowlist == nil {
		t.Fatalf("none = %+v", n)
	}
	if e := read("every"); len(e.Skills) != 3 || e.Allowlist != nil {
		t.Fatalf("every = %+v", e)
	}
	resp, b := do(t, http.MethodGet, srv.URL+"/v1/agents/nope/skills", "")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("nope: %d %s", resp.StatusCode, b)
	}
	resp, b = do(t, http.MethodGet, srv.URL+"/v1/agents/reviewer/skills?pool=x", "")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("pool without pools: %d %s", resp.StatusCode, b)
	}
}
