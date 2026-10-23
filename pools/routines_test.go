package pools

import (
	"strings"
	"testing"

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
