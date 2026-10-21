package pools

import (
	"slices"
	"testing"

	v1alpha1 "github.com/google/ax/pkg/apis/v1alpha1"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// A change of the overlay changes the hash of the pool and not its
// generation. The Tasks carry the hash of the configuration alone in
// FW_POOL_BASE, so the pool takes them for older and replaces them, and
// does not take the overlay for a second configuration of the generation.
func TestGenerationOverlayChangeRolls(t *testing.T) {
	ax, check := newFakeAX(), newFakeCheck()
	r := newReplica(t, withGeneration(7, ""), ax, check)
	for i := range 3 {
		r.worker(i, phaseRunning, true)
	}
	r.pass()
	if got := ax.took(); !slices.Equal(got, []string{"workspace team/triage-generation"}) {
		t.Fatalf("first pass calls = %v", got)
	}
	base := r.p.Hash()
	merged, err := r.p.WithOverlay(Overlay{}.WithProfile(OverlayProfile{Profile: fw.AgentProfile{Name: "triage", Description: "Sorts every ticket.", Tools: []string{"files"}}}))
	if err != nil {
		t.Fatal(err)
	}
	if merged.Hash() == base || merged.baseHash() != base {
		t.Fatalf("hash %s, base %s, configuration %s", merged.Hash(), merged.baseHash(), base)
	}
	r.p = merged
	for range 12 {
		if st := r.pass(); hasGap(st, "conflict:") {
			t.Fatalf("the overlay counted as a conflict: %v", st.Gaps)
		}
		ax.took()
	}
	tasks := ax.list("team")
	if len(tasks) != 3 {
		t.Fatalf("%d Tasks, want 3", len(tasks))
	}
	for _, task := range tasks {
		if envOf(task, EnvSpec) != merged.Hash() || envOf(task, EnvBase) != base || genOf(task) != 7 {
			t.Fatalf("%s carries %s=%s %s=%s generation %d", task.Metadata.Name, EnvSpec, envOf(task, EnvSpec), EnvBase, envOf(task, EnvBase), genOf(task))
		}
	}
}

// A Task of the same generation from a front without FW_POOL_BASE compares
// by its full hash, so another hash is still a conflict.
func TestGenerationWithoutBaseComparesTheHash(t *testing.T) {
	ax, check := newFakeAX(), newFakeCheck()
	r := newReplica(t, withGeneration(7, ""), ax, check)
	task, _, err := r.p.Render(0, testLookup)
	if err != nil {
		t.Fatal(err)
	}
	var env []*v1alpha1.EnvVar
	for _, e := range task.Spec.Env {
		switch e.Name {
		case EnvBase:
			continue
		case EnvSpec:
			e.Value = "otherhash"
		}
		env = append(env, e)
	}
	task.Spec.Env = env
	ax.put(task, phaseRunning, true)
	if st := r.pass(); !hasGap(st, "conflict:") {
		t.Fatalf("gaps = %v", st.Gaps)
	}
	if got := ax.took(); len(got) != 0 {
		t.Fatalf("calls = %v", got)
	}
}
