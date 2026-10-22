package bossmode

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/skills"
	"github.com/bdubs00/featherweight-workbench/surface/mcp"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// putSkill writes a skill under the skills directory of the home of the
// front.
func putSkill(t *testing.T, a *fw.Agent, dir, text string) {
	t.Helper()
	d := filepath.Join(a.Config().Home, "skills", dir)
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, fw.SkillFile), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

// connectMCP serves the MCP surface of the agent and connects a client as
// who.
func connectMCP(t *testing.T, a *fw.Agent, who string) *sdk.ClientSession {
	t.Helper()
	h, err := mcp.Handler(a)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	hc := &http.Client{Transport: bearer{token: who}}
	client := sdk.NewClient(&sdk.Implementation{Name: "test-client", Version: "v1"}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	cs, err := client.Connect(ctx, &sdk.StreamableClientTransport{Endpoint: srv.URL + mcp.DefaultPath, HTTPClient: hc, MaxRetries: -1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

type bearer struct{ token string }

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return http.DefaultTransport.RoundTrip(r)
}

// callTool calls a tool with optional _meta.
func callTool(t *testing.T, cs *sdk.ClientSession, name string, args any, meta sdk.Meta) *sdk.CallToolResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := cs.CallTool(ctx, &sdk.CallToolParams{Meta: meta, Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("call %s: %v", name, err)
	}
	return res
}

func textOf(res *sdk.CallToolResult) string {
	var parts []string
	for _, c := range res.Content {
		if tc, ok := c.(*sdk.TextContent); ok {
			parts = append(parts, tc.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// skillsFront is a front whose profile manager only ada operates, with
// three skills: a skill with every key of a slash command, a skill with
// keys of the wrong type, and an imported skill.
func skillsFront(t *testing.T) *front {
	t.Helper()
	fr := newFront(t, func(c *fw.Config) {
		c.Agents["manager"] = fw.AgentProfile{Access: &fw.Access{Operators: []string{"ada"}}}
	})
	putSkill(t, fr.a, "fix", "---\nname: fix\ndescription: Fixes an issue.\nargument-hint: <issue>\narguments: [issue]\ndisable-model-invocation: true\n---\nFix issue $issue.\n")
	putSkill(t, fr.a, "odd", "---\nname: odd\ndescription: Odd keys.\nuser-invocable: 3\narguments: {a: b}\n---\nBody.\n")
	putSkill(t, fr.a, "fetched", "---\nname: fetched\ndescription: An import.\nuser-invocable: false\n---\nBody.\n")
	if err := os.WriteFile(filepath.Join(fr.a.Config().Home, "skills", "fetched", skills.OriginFile), []byte(`{"kind":"text"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	return fr
}

// GET /v1/bossmode/skills answers the catalog to a person who can start
// an agent, refuses a person who cannot, and skills_list gives the same
// data as text.
func TestSkillsCatalog(t *testing.T) {
	fr := skillsFront(t)
	resp, body := get(t, fr.url+"/v1/bossmode/skills", "ada")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("catalog = %d %s", resp.StatusCode, body)
	}
	var c Catalog
	if err := json.Unmarshal([]byte(body), &c); err != nil {
		t.Fatal(err)
	}
	if len(c.Skills) != 3 {
		t.Fatalf("catalog = %s", body)
	}
	fix, fetched, odd := c.Skills[1], c.Skills[0], c.Skills[2]
	if fix.Name != "fix" || !fix.UserInvocable || fix.ModelInvocable || fix.ArgumentHint != "<issue>" || len(fix.Arguments) != 1 || fix.Arguments[0].Name != "issue" || fix.URI != "fix/SKILL.md" || fix.Origin != "local" {
		t.Fatalf("fix = %+v", fix)
	}
	if fetched.Name != "fetched" || fetched.Origin != skills.OriginImported || fetched.UserInvocable {
		t.Fatalf("fetched = %+v", fetched)
	}
	if odd.Name != "odd" || !odd.UserInvocable || len(odd.Arguments) != 0 {
		t.Fatalf("odd = %+v", odd)
	}
	for _, key := range []string{`"userInvocable"`, `"modelInvocable"`, `"argumentHint"`, `"arguments":[]`, `"files":[]`, `"uri"`, `"origin":"local"`} {
		if !strings.Contains(body, key) {
			t.Fatalf("the catalog holds no %s: %s", key, body)
		}
	}
	if resp, body := get(t, fr.url+"/v1/bossmode/skills", "bob"); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("bob = %d %s", resp.StatusCode, body)
	}
	if resp, _ := get(t, fr.url+"/v1/bossmode/skills", ""); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no credential = %d", resp.StatusCode)
	}
	token := fw.WithPrincipal(context.Background(), &fw.Principal{Subject: fw.KindToken, Kind: fw.KindToken})
	if err := mayReadCatalog(token, fr.a); err != nil {
		t.Fatalf("token: %v", err)
	}
	if err := mayReadCatalog(fw.WithPrincipal(context.Background(), &fw.Principal{Subject: "bob", Kind: fw.KindUser}), fr.a); !errors.Is(err, fw.ErrForbidden) {
		t.Fatalf("bob: %v", err)
	}

	cs := connectMCP(t, fr.a, "ada")
	res := callTool(t, cs, ToolSkillsList, map[string]any{}, nil)
	if res.IsError {
		t.Fatalf("skills_list: %s", textOf(res))
	}
	text := textOf(res)
	for _, want := range []string{"3 skills of the front:", "- fix: Fixes an issue. (only a person hands it to a run; arguments <issue>)", "- fetched: An import. (from imported; only an agent loads it)"} {
		if !strings.Contains(text, want) {
			t.Fatalf("skills_list text holds no %q:\n%s", want, text)
		}
	}
	res = callTool(t, connectMCP(t, fr.a, "bob"), ToolSkillsList, map[string]any{}, nil)
	if !res.IsError {
		t.Fatalf("skills_list for bob: %s", textOf(res))
	}
}
