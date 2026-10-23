package pools

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/bdubs00/featherweight-kernel/config"
	"github.com/bdubs00/featherweight-kernel/fw"
)

// A routine of the overlay reaches the profile of the configuration and
// the profile of the overlay that the pool runs, with each text escaped,
// so a template never reads a variable of the worker. An agent that the
// pool does not run gets nothing, the hash covers the routines, and an
// empty entry takes the routines of a profile away.
func TestWithOverlayCarriesTheRoutines(t *testing.T) {
	base := loadFixture(t, fixture)[0]
	hash := base.Hash()
	ack := fw.Routine{Name: "acknowledge", Version: 2, Standing: fw.RoutineShadow,
		Steps:  []fw.RoutineStep{{Tool: "tickets.comment", Input: `{"body": "Costs $${0} and ${HOME}", "number": {{ find .input "#(\\d+)$" }}}`}},
		Answer: "done"}
	o := Overlay{}.WithProfile(OverlayProfile{Profile: fw.AgentProfile{Name: "helper", System: "You help."}})
	o.Routines = map[string][]fw.Routine{
		"triage": {ack},
		"helper": {{Name: "close", Version: 1, Standing: fw.RoutineLive, Steps: []fw.RoutineStep{{Tool: "files", Input: "{}"}}}},
		"nobody": {ack},
	}
	merged, err := base.WithOverlay(o)
	if err != nil {
		t.Fatal(err)
	}
	if merged.Hash() == hash {
		t.Fatal("the routines kept the hash")
	}
	file := merged.files[0].Content
	if !strings.Contains(file, `$$$${0} and $${HOME}`) || strings.Contains(file, "nobody") {
		t.Fatalf("the worker file does not escape the routines or holds an agent of no pool:\n%s", file)
	}
	wc := workerOf(t, merged)
	if !slices.Contains(wc.Bus.EgressTypes, fw.EvRoutineCompared) || slices.Contains(workerOf(t, base).Bus.EgressTypes, fw.EvRoutineCompared) {
		t.Fatalf("the events of the routines go out from a pool without routines, or not from one with them: %v", wc.Bus.EgressTypes)
	}
	got := wc.Agents["triage"].Routines
	if len(got) != 1 || got[0].Name != "acknowledge" || got[0].Version != 2 || got[0].Standing != fw.RoutineShadow || wc.Agents["triage"].Description != "Sorts new tickets." {
		t.Fatalf("the routines of triage = %+v in %+v", got, wc.Agents["triage"])
	}
	if h := wc.Agents["helper"]; len(h.Routines) != 1 || h.Routines[0].Standing != fw.RoutineLive || h.System != "You help.\n" {
		t.Fatalf("the profile of the overlay = %+v", h)
	}
	if _, ok := wc.Agents["nobody"]; ok {
		t.Fatal("an agent that the pool does not run got a profile")
	}
	var dry strings.Builder
	if err := Write(&dry, []*Pool{merged}); err != nil || !strings.Contains(dry.String(), "give the profile triage the routines acknowledge version 2 (shadow)") {
		t.Fatalf("the dry run does not name the routines: %v\n%s", err, dry.String())
	}

	// A change of a routine changes the hash, and an empty entry leaves
	// the profile without routines.
	o.Routines["triage"] = []fw.Routine{{Name: "acknowledge", Version: 3, Standing: fw.RoutineShadow, Steps: ack.Steps}}
	next, err := base.WithOverlay(o)
	if err != nil || next.Hash() == merged.Hash() {
		t.Fatalf("a new version kept the hash: %v", err)
	}
	o.Routines["triage"] = []fw.Routine{}
	none, err := base.WithOverlay(o)
	if err != nil {
		t.Fatal(err)
	}
	if r := workerOf(t, none).Agents["triage"].Routines; len(r) != 0 {
		t.Fatalf("an empty entry left %+v", r)
	}

	// The worker checks a routine as it loads its profile: a question
	// needs a decider that the workers know.
	o.Routines["triage"] = []fw.Routine{{Name: "ask", Version: 1, Standing: fw.RoutineShadow, Decider: "clef",
		Questions: map[string]fw.RoutineQuestion{"kind": {Question: fw.Question{Type: fw.QuestionNoul}, Act: 0.9}},
		Steps:     []fw.RoutineStep{{Tool: "files", Input: "{}"}}}}
	if _, err := base.WithOverlay(o); err == nil || !strings.Contains(err.Error(), `no model answers questions under the name "clef"`) {
		t.Fatalf("a routine with an unknown decider = %v", err)
	}
}

// The worker loads its fw.yaml as the kernel does, with ${NAME} expanded,
// and gets each text of a routine as the store holds it, a $ included.
// An empty entry takes away the routines that the configuration of the
// pool gives a profile, no entry keeps them, and a list replaces them.
func TestRoutinesSurviveTheWorkerLoad(t *testing.T) {
	withConfig := strings.Replace(fixture, "          tools: [files, tickets.search, tickets.create_ticket]\n",
		"          tools: [files, tickets.search, tickets.create_ticket]\n"+
			"          routines:\n"+
			"            - {name: from-config, version: 1, standing: live, steps: [{tool: files, input: \"{}\"}]}\n", 1)
	base := loadFixture(t, withConfig)[0]
	load := func(p *Pool) *fw.Config {
		t.Helper()
		path := filepath.Join(t.TempDir(), "fw.yaml")
		if err := os.WriteFile(path, []byte(p.files[0].Content), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg, err := config.Load(path)
		if err != nil {
			t.Fatalf("the worker cannot load its configuration: %v\n%s", err, p.files[0].Content)
		}
		return cfg
	}
	if r := load(base).Agents["triage"].Routines; len(r) != 1 || r[0].Name != "from-config" {
		t.Fatalf("the routines of the configuration = %+v", r)
	}
	input := `{"body": "Costs $5, ${HOME}, $${0}, and $$", "number": {{ find .input "#(\\d+)$" }}}`
	kept, err := base.WithOverlay(Overlay{Routines: map[string][]fw.Routine{"helper": {{Name: "x", Version: 1, Standing: fw.RoutineShadow, Steps: []fw.RoutineStep{{Tool: "files", Input: "{}"}}}}}})
	if err != nil {
		t.Fatal(err)
	}
	if r := load(kept).Agents["triage"].Routines; len(r) != 1 || r[0].Name != "from-config" {
		t.Fatalf("an agent without an entry lost its routines: %+v", r)
	}
	replaced, err := base.WithOverlay(Overlay{Routines: map[string][]fw.Routine{"triage": {{Name: "dollars", Version: 2, Standing: fw.RoutineShadow, Steps: []fw.RoutineStep{{Tool: "files", Input: input}}}}}})
	if err != nil {
		t.Fatal(err)
	}
	r := load(replaced).Agents["triage"].Routines
	if len(r) != 1 || r[0].Name != "dollars" || r[0].Steps[0].Input != input {
		t.Fatalf("the worker reads %+v, want the input %q", r, input)
	}
	empty, err := base.WithOverlay(Overlay{Routines: map[string][]fw.Routine{"triage": {}}})
	if err != nil {
		t.Fatal(err)
	}
	if r := load(empty).Agents["triage"].Routines; len(r) != 0 {
		t.Fatalf("an empty entry left %+v", r)
	}
	if empty.Hash() == base.Hash() {
		t.Fatal("taking the routines away kept the hash")
	}
}
