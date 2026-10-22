package manager

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	fwconfig "github.com/bdubs00/featherweight-kernel/config"
	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/fleet"
	"github.com/bdubs00/featherweight-workbench/pools"
)

// example is the front of examples/bossmode.
const example = "../examples/bossmode/fw.yaml"

// clearFleetEnv keeps the variables of the fleet of the test process out of
// the configuration of the example.
func clearFleetEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{fleet.EnvAtespaces, fleet.EnvEgress, fleet.EnvIntake, EnvManager, "FW_ATESPACE"} {
		t.Setenv(name, "")
	}
}

// withModel writes the example with a model for the pool into a temporary
// file and returns its path.
func withModel(t *testing.T, model string) string {
	t.Helper()
	b, err := os.ReadFile(example)
	if err != nil {
		t.Fatal(err)
	}
	const anchor = "        maxRuns: 4\n"
	text := string(b)
	if !strings.Contains(text, anchor) {
		t.Fatalf("the example has no %q", anchor)
	}
	text = strings.Replace(text, anchor, "        model: "+model+"\n"+anchor, 1)
	path := filepath.Join(t.TempDir(), "fw.yaml")
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// loadPools reads a front and its pools with the checks of the front.
func loadPools(t *testing.T, path string) (*fw.Config, []*pools.Pool, error) {
	t.Helper()
	cfg, err := fwconfig.Load(path)
	if err != nil {
		t.Fatalf("load %s: %v", path, err)
	}
	fc, err := fleet.Load(cfg)
	if err != nil {
		t.Fatalf("the fleet of %s: %v", path, err)
	}
	list, err := fleet.LoadPools(cfg, fc)
	return cfg, list, err
}

// The example names no model, so the checks of the front refuse its pool
// until the operator sets one. With a model, the pool passes every check
// of the front, and the profile manager is the one of the plan: its
// tools, its policy, its budget, and the trusted entry of the front.
func TestTheExampleIsAPoolOfTheFront(t *testing.T) {
	clearFleetEnv(t)
	if _, _, err := loadPools(t, example); err == nil || !strings.Contains(err.Error(), "set fw.model") {
		t.Fatalf("the example without a model = %v", err)
	}

	cfg, list, err := loadPools(t, withModel(t, "anthropic/example-model"))
	if err != nil {
		t.Fatalf("the pools: %v", err)
	}
	// The front copies the static token from its environment.
	t.Setenv("FW_API_TOKEN", "a-real-token-value")
	if _, err := pools.Resolve(list, nil); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if Profile(cfg) != "manager" {
		t.Fatalf("workbench.bossmode.manager = %q", Profile(cfg))
	}
	if len(list) != 1 {
		t.Fatalf("pools = %d", len(list))
	}
	p := list[0]
	if p.Name != "manager" || p.Wanted() != 1 || p.Agent != "manager" || p.Intake != "fw.jobs.manager" {
		t.Fatalf("pool = %s, %d, %s, %s", p.Name, p.Wanted(), p.Agent, p.Intake)
	}
	if w := p.Warnings(); len(w) > 0 {
		t.Errorf("warnings: %v", w)
	}
	prof := p.Profiles()["manager"]
	if !slices.Equal(prof.Tools, []string{"front.*", "memory", "recall", "clock", "files"}) {
		t.Fatalf("tools = %v", prof.Tools)
	}
	if prof.Policy == nil || prof.Policy.Default != "allow" || !slices.Equal(prof.Policy.Allow, []string{"readonly"}) {
		t.Fatalf("policy = %+v", prof.Policy)
	}
	wantAsk := []string{"front.approve", "front.cancel", "front.run_budget", "front.enqueue", "front.run", "front.bridge_open",
		"front.bridge_remove_member", "front.agent_put", "front.agent_delete", "front.faces_put"}
	if !slices.Equal(prof.Policy.Ask, wantAsk) || slices.Contains(prof.Policy.Ask, "front.pages_write") {
		t.Fatalf("ask = %v", prof.Policy.Ask)
	}
	if b := prof.Budget; b == nil || b.MaxTurns != 400 || b.Deadline != 8*time.Hour || b.MaxToolCalls != 2000 {
		t.Fatalf("budget = %+v", prof.Budget)
	}
	for _, line := range []string{"Read before you act", "Ask before you spend", "front.bridge_open", "front.pages_write",
		"Name the person you act for", "Never claim that a run passed a check", "plain words"} {
		if !strings.Contains(prof.System, line) {
			t.Errorf("the system prompt has no %q", line)
		}
	}
	if p.Policy().Default != "allow" {
		t.Fatalf("the policy of the workers = %+v", p.Policy())
	}
	w, err := workerOf(p)
	if err != nil {
		t.Fatal(err)
	}
	if w.Spawn.Driver != "ax" || len(w.Tools.MCP) != 1 || w.Tools.MCP[0].Name != "front" || !w.Tools.MCP[0].Trusted || w.Tools.MCP[0].Auth.Bearer != "FW_API_TOKEN" {
		t.Fatalf("the worker = %+v", w)
	}

	// The dry run renders the Task with a placeholder for the token.
	var out strings.Builder
	if err := pools.Write(&out, list); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	if strings.Contains(text, "a-real-token-value") {
		t.Fatal("the dry run holds the token")
	}
	for _, want := range []string{"name: manager-0", "kind: Task", "<secret: FW_API_TOKEN of the front>", "maxRuns: 4", "trusted: true"} {
		if !strings.Contains(text, want) {
			t.Errorf("the dry run has no %q", want)
		}
	}
}

// The doctor line names the pool, the trusted entry of the front, the spawn
// driver, and whether the bridges are set.
func TestDoctorLineOfTheExample(t *testing.T) {
	clearFleetEnv(t)
	t.Cleanup(func() { SetBridges(nil) })
	cfg, _, err := loadPools(t, withModel(t, "anthropic/example-model"))
	if err != nil {
		t.Fatal(err)
	}
	SetBridges(nil)
	ok, line := DoctorCheck(context.Background(), cfg, nil, 0)
	for _, want := range []string{"manager: the profile manager runs in the pool manager of 1 Task;", "the trusted tools.mcp entry front reaches the front",
		"child Tasks", "no bridges are set"} {
		if !ok || !strings.Contains(line, want) {
			t.Errorf("doctor = %v %q, want %q", ok, line, want)
		}
	}
	SetBridges(&fakeBridges{yes: true})
	if ok, line := DoctorCheck(context.Background(), cfg, nil, 0); !ok || !strings.Contains(line, "the bridges say what the operator of each bridge delegates") {
		t.Errorf("doctor with bridges = %v %q", ok, line)
	}
}

// The doctor line fails when no pool runs the manager, when the entry of
// the front is not trusted, and when the profile reaches no entry with the
// token; it says off without a manager.
func TestDoctorLineFindsTheGaps(t *testing.T) {
	clearFleetEnv(t)
	path := withModel(t, "anthropic/example-model")
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	variant := func(old, new string) *fw.Config {
		t.Helper()
		if !strings.Contains(string(original), old) {
			t.Fatalf("the example has no %q", old)
		}
		p := filepath.Join(t.TempDir(), "fw.yaml")
		if err := os.WriteFile(p, []byte(strings.Replace(string(original), old, new, 1)), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg, _, err := loadPools(t, p)
		if err != nil {
			t.Fatalf("variant %q: %v", new, err)
		}
		return cfg
	}
	cases := []struct {
		old, new, want string
	}{
		{"    manager: manager\n", "    manager: boss\n", "no pool of workbench.pools runs the profile boss"},
		{"              trusted: true\n", "              trusted: false\n", "the tools.mcp entry front that reaches the front is not trusted"},
		{"                bearer: FW_API_TOKEN\n", "                bearer: OTHER_TOKEN\n", "reaches no tools.mcp entry that sends the static token"},
		{"            tools: [front.*, memory, recall, clock, files]\n", "            tools: [memory, recall, clock, files]\n", "reaches no tools.mcp entry that sends the static token"},
	}
	for _, c := range cases {
		if ok, line := DoctorCheck(context.Background(), variant(c.old, c.new), nil, 0); ok || !strings.Contains(line, c.want) {
			t.Errorf("%q: doctor = %v %q", c.new, ok, line)
		}
	}
	off := variant("    manager: manager\n", "    manager: \"\"\n")
	if ok, line := DoctorCheck(context.Background(), off, nil, 0); !ok || !strings.Contains(line, "manager: off") {
		t.Errorf("without a manager: doctor = %v %q", ok, line)
	}
}
