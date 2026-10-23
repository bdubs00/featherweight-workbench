package bossmode

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/bridges"
	"github.com/bdubs00/featherweight-workbench/fleet"
	"github.com/bdubs00/featherweight-workbench/skills"
	"github.com/bdubs00/featherweight-workbench/surface/mcp"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// startRun starts a run of the profile manager as who and waits until it
// finished its turn.
func startRun(t *testing.T, fr *front, who string) string {
	t.Helper()
	resp, body := send(t, http.MethodPost, fr.url+"/v1/runs", who, `{"agent":"manager","input":"Hello."}`)
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusAccepted {
		t.Fatalf("start = %d %s", resp.StatusCode, body)
	}
	var r fw.Run
	if err := json.Unmarshal([]byte(body), &r); err != nil || r.ID == "" {
		t.Fatalf("start = %s", body)
	}
	waitIdle(t, fr.a, r.ID)
	return r.ID
}

// waitIdle waits until a run of the front ended its turn.
func waitIdle(t *testing.T, a *fw.Agent, id string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		r, err := a.Get(context.Background(), id)
		if err == nil && r.State == fw.StateCompleted {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("run %s did not end its turn: %+v %v", id, r, err)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// pinnedMessages returns the follow-ups of a run of the front with their
// pinned mark, their sender, and their text.
func pinnedMessages(t *testing.T, a *fw.Agent, id string) []fw.MessageReceivedData {
	t.Helper()
	var out []fw.MessageReceivedData
	for e, err := range a.Ledger().Read(context.Background(), id, 0) {
		if err != nil {
			t.Fatal(err)
		}
		if e.Type != fw.EvMessageReceived {
			continue
		}
		var d fw.MessageReceivedData
		if err := e.Decode(&d); err != nil {
			t.Fatal(err)
		}
		out = append(out, d)
	}
	return out
}

// POST /v1/bossmode/runs/{id}/skill expands the skill and sends it to a run
// of the front as a pinned message of the person, and refuses an unknown
// skill, a skill that is not for people, a message over its bound, and a
// message that the pinned messages of the run cannot hold.
func TestSkillToARunOfTheFront(t *testing.T) {
	fr := skillsFront(t)
	putSkill(t, fr.a, "big", "---\nname: big\n---\n"+strings.Repeat("y", 40<<10))
	putSkill(t, fr.a, "huge", "---\nname: huge\n---\n"+strings.Repeat("z", skills.MaxMessageBytes+1))
	id := startRun(t, fr, "ada")
	resp, body := send(t, http.MethodPost, fr.url+"/v1/bossmode/runs/"+id+"/skill", "ada", `{"name":"fix","args":"42","text":"Report back."}`)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("skill = %d %s", resp.StatusCode, body)
	}
	var r fw.Run
	if err := json.Unmarshal([]byte(body), &r); err != nil || r.ID != id {
		t.Fatalf("skill = %s", body)
	}
	waitIdle(t, fr.a, id)
	msgs := pinnedMessages(t, fr.a, id)
	if len(msgs) != 1 || !msgs[0].Pinned || msgs[0].By != "ada" || len(msgs[0].Content) != 1 || msgs[0].Content[0].Text != "Skill: fix\n\nFix issue 42.\n\nReport back." {
		t.Fatalf("messages = %+v", msgs)
	}

	for _, c := range []struct {
		body   string
		status int
		words  string
	}{
		{`{"name":"nope"}`, http.StatusNotFound, "no skill has the name"},
		{`{"name":"fetched"}`, http.StatusBadRequest, "user-invocable: false"},
		{`{"name":"huge"}`, http.StatusRequestEntityTooLarge, "at most 65536"},
		{`{"name":"fix","extra":1}`, http.StatusBadRequest, ""},
	} {
		resp, body := send(t, http.MethodPost, fr.url+"/v1/bossmode/runs/"+id+"/skill", "ada", c.body)
		if resp.StatusCode != c.status || !strings.Contains(body, c.words) {
			t.Errorf("%s = %d %s, want %d with %q", c.body, resp.StatusCode, body, c.status, c.words)
		}
	}
	// The first big skill fits, and the second one passes the bound of the
	// pinned messages of the run: 413 with the words of the kernel.
	if resp, body := send(t, http.MethodPost, fr.url+"/v1/bossmode/runs/"+id+"/skill", "ada", `{"name":"big"}`); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("big = %d %s", resp.StatusCode, body)
	}
	waitIdle(t, fr.a, id)
	resp, body = send(t, http.MethodPost, fr.url+"/v1/bossmode/runs/"+id+"/skill", "ada", `{"name":"big"}`)
	if resp.StatusCode != http.StatusRequestEntityTooLarge || !strings.Contains(body, "the bound is") {
		t.Fatalf("big again = %d %s", resp.StatusCode, body)
	}
	// A person who cannot operate the run gets the refusal of the kernel,
	// before the front reads any skill: an unknown skill answers as a known
	// one does.
	for _, name := range []string{"fix", "nope", "huge"} {
		resp, body := send(t, http.MethodPost, fr.url+"/v1/bossmode/runs/"+id+"/skill", "bob", `{"name":"`+name+`"}`)
		if (resp.StatusCode != http.StatusForbidden && resp.StatusCode != http.StatusNotFound) || strings.Contains(body, "no skill has the name") || strings.Contains(body, "65536") {
			t.Fatalf("bob with %s = %d %s", name, resp.StatusCode, body)
		}
	}
	if resp, body := send(t, http.MethodPost, fr.url+"/v1/bossmode/runs/nope/skill", "ada", `{"name":"fix"}`); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown run = %d %s", resp.StatusCode, body)
	}
}

// member is a fake fleet member that answers the follow-ups of the front,
// and records the body, the credential, and the person of each.
type member struct {
	mu      sync.Mutex
	srv     *httptest.Server
	bodies  []map[string]any
	auths   []string
	persons []string
	status  int
	detail  string
}

func newMember(t *testing.T) *member {
	m := &member{}
	m.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(b, &body)
		m.mu.Lock()
		m.bodies = append(m.bodies, body)
		m.auths = append(m.auths, r.Header.Get("Authorization"))
		m.persons = append(m.persons, r.Header.Get(fleet.HeaderOnBehalfOf))
		status, detail := m.status, m.detail
		m.mu.Unlock()
		if status != 0 {
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(map[string]string{"detail": detail})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(fw.Run{ID: strings.Split(strings.TrimPrefix(r.URL.Path, "/v1/runs/"), "/")[0], State: fw.StateRunning})
	}))
	t.Cleanup(m.srv.Close)
	return m
}

// view is a fleet view with one run of the member default/manager-0, whose
// forwards reach the fake member. It lets every person operate the run,
// unless refuse is set (fleet.Authorizer).
type view struct {
	target string
	refuse bool
}

func (v view) AllowRun(context.Context, string, string) error {
	if v.refuse {
		return fmt.Errorf("%w: ada run.operate needs an operator entry on the agent manager", fw.ErrForbidden)
	}
	return nil
}

func (v view) AllowAgent(context.Context, string, string, string) error { return nil }

func (v view) Name() string                                    { return "front" }
func (v view) Members(context.Context) ([]fleet.Member, error) { return nil, nil }
func (v view) Runs(context.Context) ([]*fw.Run, error)         { return nil, nil }
func (v view) Run(_ context.Context, id string) (*fw.Run, error) {
	if id != "remote-1" {
		return nil, fmt.Errorf("view: %w", fw.ErrRunNotFound)
	}
	return &fw.Run{ID: id, Agent: "manager", State: fw.StateCompleted, Member: "default/manager-0"}, nil
}
func (v view) Forward(ctx context.Context, _ string, req *http.Request) (*http.Response, error) {
	out, err := http.NewRequestWithContext(ctx, req.Method, v.target+req.URL.Path, req.Body)
	if err != nil {
		return nil, err
	}
	out.Header = req.Header.Clone()
	out.Header.Set("Authorization", fw.CredentialFrom(ctx))
	// As the fleet does, the person comes from the context alone.
	out.Header.Del(fleet.HeaderOnBehalfOf)
	if who := fleet.OnBehalfOf(ctx); who != "" {
		out.Header.Set(fleet.HeaderOnBehalfOf, who)
	}
	return http.DefaultClient.Do(out)
}

// A slash command for a run of another member goes through the forward of
// the fleet, pinned. The front decides for the person, and the message
// carries the static token and names the person. A refusal of the owner to
// pin it answers 413, an owner that refuses the token 403 with words for
// an operator, and an owner that fails 502. A person whom the front
// refuses gets 403, and nothing reaches the owner.
func TestSkillToARunOfAMember(t *testing.T) {
	t.Setenv("FW_API_TOKEN", staticToken)
	fr := skillsFront(t)
	m := newMember(t)
	fleet.Attach(fr.a, view{target: m.srv.URL})
	resp, body := send(t, http.MethodPost, fr.url+"/v1/bossmode/runs/remote-1/skill", "ada", `{"name":"fix","args":"7"}`)
	if resp.StatusCode != http.StatusAccepted || !strings.Contains(body, `"member":"default/manager-0"`) {
		t.Fatalf("skill = %d %s", resp.StatusCode, body)
	}
	m.mu.Lock()
	got, auth, person := m.bodies[0], m.auths[0], m.persons[0]
	m.mu.Unlock()
	if got["pinned"] != true || got["content"] != "Skill: fix\n\nFix issue 7." || got["from"] != nil || auth != "Bearer "+staticToken || person != "ada" {
		t.Fatalf("forward = %+v with %q for %q", got, auth, person)
	}
	for _, c := range []struct {
		status int
		detail string
		want   int
		words  string
	}{
		{http.StatusBadRequest, "fw: invalid request: the pinned messages of run remote-1 would hold 70000 bytes, and the bound is 65536 bytes (64 KB)", http.StatusRequestEntityTooLarge, "the bound is 65536"},
		{http.StatusUnauthorized, "no", http.StatusForbidden, "the worker default/manager-0 refused the token of the fleet; check FW_API_TOKEN on that worker"},
		{http.StatusServiceUnavailable, "the Task wakes", http.StatusBadGateway, "the Task wakes"},
		{http.StatusForbidden, "not yours", http.StatusForbidden, "not yours"},
	} {
		m.mu.Lock()
		m.status, m.detail = c.status, c.detail
		m.mu.Unlock()
		resp, body := send(t, http.MethodPost, fr.url+"/v1/bossmode/runs/remote-1/skill", "ada", `{"name":"fix"}`)
		if resp.StatusCode != c.want || !strings.Contains(body, c.words) {
			t.Errorf("owner %d = %d %s, want %d with %q", c.status, resp.StatusCode, body, c.want, c.words)
		}
	}
	m.mu.Lock()
	m.status, m.detail = 0, ""
	before := len(m.bodies)
	m.mu.Unlock()
	fleet.Attach(fr.a, view{target: m.srv.URL, refuse: true})
	resp, body = send(t, http.MethodPost, fr.url+"/v1/bossmode/runs/remote-1/skill", "ada", `{"name":"fix"}`)
	m.mu.Lock()
	after := len(m.bodies)
	m.mu.Unlock()
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(body, "operator entry on the agent manager") || after != before {
		t.Fatalf("a refused person = %d %s, the owner got %d requests", resp.StatusCode, body, after-before)
	}
	// Without the token, the front acts for no person.
	fleet.Attach(fr.a, view{target: m.srv.URL})
	t.Setenv("FW_API_TOKEN", "")
	resp, body = send(t, http.MethodPost, fr.url+"/v1/bossmode/runs/remote-1/skill", "ada", `{"name":"fix"}`)
	if resp.StatusCode != http.StatusServiceUnavailable || !strings.Contains(body, "the front holds no static token of the fleet") {
		t.Fatalf("without the token = %d %s", resp.StatusCode, body)
	}
}

// skill_send hands a skill to a run and to the members of a bridge as the
// person, and refuses a call that a run of the fleet makes.
func TestSkillSendTool(t *testing.T) {
	fr := storeFront(t)
	putSkill(t, fr.a, "fix", "---\nname: fix\ndescription: Fixes an issue.\narguments: [issue]\n---\nFix issue $issue.\n")
	id := startRun(t, fr, "ada")
	cs := connectMCP(t, fr.a, "ada")
	res := callTool(t, cs, ToolSkillSend, map[string]any{"name": "fix", "args": "9", "runId": id}, nil)
	if res.IsError || !strings.Contains(textOf(res), "Sent the skill fix to the run "+id) {
		t.Fatalf("skill_send run = %s", textOf(res))
	}
	waitIdle(t, fr.a, id)
	if msgs := pinnedMessages(t, fr.a, id); len(msgs) != 1 || !msgs[0].Pinned || msgs[0].By != "ada" || msgs[0].Content[0].Text != "Skill: fix\n\nFix issue 9." {
		t.Fatalf("messages = %+v", msgs)
	}

	resp, body := send(t, http.MethodPost, fr.url+"/v1/bridges", "ada", `{"title":"Slash","goal":"Try a skill.","members":[{"handle":"boss-1","agent":"manager","task":"Watch"}]}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("open = %d %s", resp.StatusCode, body)
	}
	var it struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal([]byte(body), &it)
	res = callTool(t, cs, ToolSkillSend, map[string]any{"name": "fix", "args": "3", "bridge": it.ID}, nil)
	if res.IsError || !strings.Contains(textOf(res), "Posted the skill fix to the bridge "+it.ID) {
		t.Fatalf("skill_send bridge = %s", textOf(res))
	}
	_, page := get(t, fr.url+"/v1/bridges/"+it.ID+"/messages", "ada")
	var p struct {
		Messages []bridges.Message `json:"messages"`
	}
	_ = json.Unmarshal([]byte(page), &p)
	found := false
	for _, m := range p.Messages {
		found = found || (m.Skill != nil && m.Skill.Name == "fix" && m.Skill.Args == "3" && m.From.Name == "ada")
	}
	if !found {
		t.Fatalf("the channel holds no slash command: %s", page)
	}
	for _, args := range []map[string]any{
		{"name": "fix"},
		{"name": "fix", "runId": id, "bridge": it.ID},
		{"name": "nope", "runId": id},
	} {
		if res := callTool(t, cs, ToolSkillSend, args, nil); !res.IsError {
			t.Errorf("skill_send %v = %s", args, textOf(res))
		}
	}
	tok := connectMCP(t, fr.a, staticToken)
	res = callTool(t, tok, ToolSkillSend, map[string]any{"name": "fix", "runId": id}, sdk.Meta{mcp.MetaRun: "worker-run", mcp.MetaAgent: "manager"})
	if !res.IsError || !strings.Contains(textOf(res), "A run hands no skill") {
		t.Fatalf("skill_send of a run = %s", textOf(res))
	}
}
