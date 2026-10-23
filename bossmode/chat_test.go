package bossmode

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	fwconfig "github.com/bdubs00/featherweight-kernel/config"
	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/fleet"
)

// Load reads the quick chat from the block, and the variables win over
// the file; a name that cannot be a profile or a member stops the load.
func TestLoadChat(t *testing.T) {
	t.Setenv(EnvChatAgent, "")
	t.Setenv(EnvChatMember, "")
	t.Setenv(EnvManager, "")
	cfg := withBlock(t, "bossmode: {enabled: true, chat: {agent: chat, member: default/manager-0}}\n")
	c, err := Load(cfg)
	if err != nil || c.Chat != (Chat{Agent: "chat", Member: "default/manager-0"}) {
		t.Fatalf("Load = %+v, %v", c, err)
	}
	t.Setenv(EnvChatAgent, " helper ")
	t.Setenv(EnvChatMember, "ax:team/chat-1")
	if c, err = Load(cfg); err != nil || c.Chat != (Chat{Agent: "helper", Member: "ax:team/chat-1"}) {
		t.Errorf("Load with the variables = %+v, %v", c, err)
	}
	t.Setenv(EnvChatAgent, "")
	t.Setenv(EnvChatMember, "")
	if c, err = Load(withBlock(t, "bossmode: {enabled: true}\n")); err != nil || c.Chat != (Chat{}) {
		t.Errorf("Load without a quick chat = %+v, %v", c, err)
	}
	for block, key := range map[string]string{
		"bossmode: {chat: {agent: two words}}\n":                                                  "workbench.bossmode.chat.agent",
		"bossmode: {chat: {agent: a/b}}\n":                                                        "workbench.bossmode.chat.agent",
		"bossmode: {chat: {agent: chat, member: \"two words\"}}\n":                                "workbench.bossmode.chat.member",
		"bossmode: {chat: {member: default/manager-0}}\n":                                         "names no profile",
		"bossmode: {chat: {agent: chat, member: " + strings.Repeat("m", MaxChatMember+1) + "}}\n": "at most 256",
	} {
		if _, err := Load(withBlock(t, block)); err == nil || !strings.Contains(err.Error(), key) {
			t.Errorf("%s: %v", strings.TrimSpace(block), err)
		}
	}
	t.Setenv(EnvChatMember, "default/manager-0")
	if _, err := Load(withBlock(t, "bossmode: {enabled: true}\n")); err == nil || !strings.Contains(err.Error(), EnvChatMember) || !strings.Contains(err.Error(), EnvChatAgent) {
		t.Errorf("a member of a variable without an agent: %v", err)
	}
}

// The settings carry chat: null without a quick chat, the member of the
// configuration when it names one, and else the member that holds the
// profile, as for the manager.
func TestSettingsChat(t *testing.T) {
	t.Setenv(EnvChatAgent, "")
	t.Setenv(EnvChatMember, "")
	fr := newFront(t, nil)
	resp, body := get(t, fr.url+"/v1/bossmode", "ada")
	if resp.StatusCode != http.StatusOK || !strings.HasSuffix(body, `"chat":null}`+"\n") {
		t.Fatalf("settings without a quick chat: %d %s", resp.StatusCode, body)
	}

	t.Setenv(EnvChatAgent, "chat")
	fr = newFront(t, func(cfg *fw.Config) {
		cfg.Agents["chat"] = fw.AgentProfile{Access: &fw.Access{Operators: []string{"*"}}}
	})
	// The front holds every profile, so without another member that holds
	// the profile, the front is the member.
	var got Settings
	_, body = get(t, fr.url+"/v1/bossmode", "ada")
	if err := json.Unmarshal([]byte(body), &got); err != nil || got.Chat == nil || *got.Chat != (QuickChat{Agent: "chat", Member: "front"}) {
		t.Errorf("settings with the front alone = %s, %v", body, err)
	}
	// A Task that ran the profile holds it.
	fr.event(t, "ax:team/manager-0", "01CHAT1", 1, fw.EvRunQueued, queued("ada", "chat"))
	_, body = get(t, fr.url+"/v1/bossmode", "ada")
	if err := json.Unmarshal([]byte(body), &got); err != nil || got.Chat == nil || *got.Chat != (QuickChat{Agent: "chat", Member: "team/manager-0"}) {
		t.Errorf("settings with a Task = %s, %v", body, err)
	}
	if !strings.Contains(body, `"chat":{"agent":"chat","member":"team/manager-0"}`) {
		t.Errorf("the shape of chat = %s", body)
	}

	// A member of the configuration wins.
	t.Setenv(EnvChatMember, "team/chat-0")
	fr = newFront(t, nil)
	_, body = get(t, fr.url+"/v1/bossmode", "ada")
	if err := json.Unmarshal([]byte(body), &got); err != nil || got.Chat == nil || *got.Chat != (QuickChat{Agent: "chat", Member: "team/chat-0"}) {
		t.Errorf("settings with a member = %s, %v", body, err)
	}
}

// chatPools is a fleet block with the pool manager, whose workers run the
// profiles manager and chat.
const chatPools = `
fleet:
  atespaces: [team]
  egress: fw.events
pools:
  - name: manager
    replicas: 1
    agent: manager
    image: ghcr.io/example/fw@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
    egress:
      hosts:
        - http://nats.example:8080
    fw:
      model: openai/some-model
      bus: {url: "ws://nats.example:8080"}
      access: {admins: ["kind:token"]}
      agents:
        manager: {}
        chat: {}
`

// The doctor line names the quick chat, where its profile is defined, and
// whether a pool runs it; a profile that nothing defines fails the line.
func TestDoctorChat(t *testing.T) {
	t.Setenv(EnvChatAgent, "")
	t.Setenv(EnvChatMember, "")
	t.Setenv(EnvManager, "")
	t.Setenv("FW_ATESPACE", "")
	t.Setenv("FW_API_TOKEN", "fleet-secret-token")
	check := func(cfg *fw.Config) (bool, string) {
		t.Helper()
		return DoctorCheck(t.Context(), cfg, nil, 0)
	}
	if ok, line := check(withBlock(t, "bossmode: {enabled: true}\n")); !ok || !strings.Contains(line, "names no quick chat (workbench.bossmode.chat.agent)") {
		t.Errorf("no quick chat: %v %s", ok, line)
	}
	cfg := withBlock(t, "bossmode: {enabled: true, chat: {agent: chat}}\n")
	if ok, line := check(cfg); ok || !strings.Contains(line, "the quick chat chat is no profile of fw.agents and of no pool") {
		t.Errorf("a quick chat without a profile: %v %s", ok, line)
	}
	cfg.Agents = map[string]fw.AgentProfile{"chat": {}}
	if ok, line := check(cfg); !ok || !strings.Contains(line, "the quick chat chat is a profile of fw.agents, and no pool runs it") {
		t.Errorf("a quick chat of fw.agents: %v %s", ok, line)
	}
	cfg = withBlock(t, "bossmode: {enabled: true, manager: manager, chat: {agent: chat, member: team/manager-0}}\n"+chatPools)
	cfg.Bus.Driver, cfg.Bus.URL = "nats", "nats://nats.example:4222"
	cfg.AX.Server, cfg.AX.Router = "127.0.0.1:1", "127.0.0.1:1"
	ok, line := check(cfg)
	if !ok || !strings.Contains(line, "the manager manager is the agent of the pool manager") || !strings.Contains(line, "the quick chat chat is a profile of the pool manager, and runs on the member team/manager-0") {
		t.Errorf("a quick chat of a pool: %v %s", ok, line)
	}
}

// The example of bossmode names the quick chat, and its profile is the
// second profile of the pool of the manager: a general assistant with the
// tools of the deployer, no model, and the memory of one person. The
// doctor line names it in that pool.
func TestTheExampleHasTheQuickChat(t *testing.T) {
	for _, name := range []string{fleet.EnvAtespaces, fleet.EnvEgress, fleet.EnvIntake, EnvManager, EnvChatAgent, EnvChatMember, "FW_ATESPACE"} {
		t.Setenv(name, "")
	}
	b, err := os.ReadFile(filepath.Join("..", "examples", "bossmode", "fw.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	// The example names no model, so the pool loads only with one.
	text := strings.Replace(string(b), "        maxRuns: 4\n", "        model: anthropic/example-model\n        maxRuns: 4\n", 1)
	path := filepath.Join(t.TempDir(), "fw.yaml")
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := fwconfig.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	c, err := Load(cfg)
	if err != nil || c.Chat != (Chat{Agent: "chat"}) || c.Manager != "manager" {
		t.Fatalf("Load = %+v, %v", c, err)
	}
	fc, err := fleet.Load(cfg)
	if err != nil {
		t.Fatal(err)
	}
	list, err := fleet.LoadPools(cfg, fc)
	if err != nil || len(list) != 1 {
		t.Fatalf("pools = %v, %v", list, err)
	}
	p := list[0]
	if p.Agent != "manager" || p.Intake != "fw.jobs.manager" {
		t.Fatalf("the pool takes %s from %s", p.Agent, p.Intake)
	}
	prof, ok := p.Profiles()["chat"]
	if !ok {
		t.Fatalf("the pool runs no profile chat: %v", p.Profiles())
	}
	want := []string{"files", "recall", "memory", "clock", "front.pages_list", "front.pages_read", "front.pages_write", "front.pages_search", "front.pages_history"}
	if !slices.Equal(prof.Tools, want) || prof.Model != "" || prof.System == "" || prof.Access == nil || !slices.Equal(prof.Access.Operators, []string{"*"}) {
		t.Fatalf("chat = %+v", prof)
	}
	tools, _ := p.FW["tools"].(map[string]any)
	memory, _ := tools["memory"].(map[string]any)
	if memory["scope"] != "principal" {
		t.Errorf("the memory of the workers = %v", tools["memory"])
	}
	t.Setenv(EnvEnabled, "on")
	ok, line := DoctorCheck(t.Context(), cfg, nil, 0)
	if !strings.Contains(line, "the quick chat chat is a profile of the pool manager, and the settings pick its member") || !strings.Contains(line, "the manager manager is the agent of the pool manager") {
		t.Errorf("doctor = %v %s", ok, line)
	}
}
