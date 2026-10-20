package pools

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"gopkg.in/yaml.v3"
)

// workerOf decodes the fw.yaml that a pool renders for its workers.
func workerOf(t *testing.T, p *Pool) *fw.Config {
	t.Helper()
	wc := fw.DefaultConfig()
	if err := yaml.Unmarshal([]byte(p.files[0].Content), wc); err != nil {
		t.Fatal(err)
	}
	return wc
}

// The store in memory puts, replaces, lists, and deletes the entries of the
// overlay of each pool, sorted.
func TestMemOverlays(t *testing.T) {
	ctx := context.Background()
	s := NewMemOverlays()
	if o, err := s.Read(ctx, "triage"); err != nil || !o.Empty() {
		t.Fatalf("empty Read = %+v, %v", o, err)
	}
	at := time.Unix(1_700_000_000, 0)
	for _, name := range []string{"writer", "helper"} {
		if err := s.PutProfile(ctx, "triage", OverlayProfile{Profile: fw.AgentProfile{Name: name}, By: "alice", At: at}); err != nil {
			t.Fatal(err)
		}
	}
	_ = s.PutProfile(ctx, "triage", OverlayProfile{Profile: fw.AgentProfile{Name: "writer", Description: "second"}, By: "bob", At: at})
	_ = s.AddRule(ctx, "triage", OverlayRule{Rule: "tickets.create_ticket", By: "alice", At: at})
	_ = s.AddRule(ctx, "triage", OverlayRule{Rule: "bash", By: "alice", At: at})
	_ = s.AddRule(ctx, "triage", OverlayRule{Rule: "bash", By: "bob", At: at})
	o, _ := s.Read(ctx, "triage")
	if len(o.Profiles) != 2 || o.Profiles[0].Profile.Name != "helper" || o.Profiles[1].Profile.Description != "second" || o.Profiles[1].By != "bob" {
		t.Fatalf("profiles = %+v", o.Profiles)
	}
	if len(o.Allow) != 2 || o.Allow[0].Rule != "bash" || o.Allow[0].By != "bob" {
		t.Fatalf("rules = %+v", o.Allow)
	}
	if ok, err := s.DeleteProfile(ctx, "triage", "helper"); !ok || err != nil {
		t.Fatalf("DeleteProfile = %v, %v", ok, err)
	}
	if ok, _ := s.DeleteProfile(ctx, "triage", "helper"); ok {
		t.Fatal("a second delete found the profile")
	}
	if o, _ := s.Read(ctx, "other"); !o.Empty() {
		t.Fatalf("another pool = %+v", o)
	}
	if o, _ := s.Read(ctx, "triage"); len(o.Profiles) != 1 || len(o.Allow) != 2 {
		t.Fatalf("after the delete = %+v", o)
	}
}

// The merge adds a profile, replaces the profile of the configuration,
// adds an allow rule and removes the ask that names the same tool, and
// changes the hash. The pool of the configuration stays as it was, and an
// empty overlay renders the same file and the same hash.
func TestWithOverlayMergesAndChangesTheHash(t *testing.T) {
	base := loadFixture(t, fixture)[0]
	file, hash := base.files[0].Content, base.Hash()

	same, err := base.WithOverlay(Overlay{})
	if err != nil || same.Hash() != hash || same.files[0].Content != file {
		t.Fatalf("an empty overlay changed the pool: %v", err)
	}

	o := Overlay{}.
		WithProfile(OverlayProfile{Profile: fw.AgentProfile{Name: "helper", System: "Use ${HOME} and $$5.", Skills: []string{}}}).
		WithProfile(OverlayProfile{Profile: fw.AgentProfile{Name: "triage", Description: "Sorts every ticket.", Tools: []string{"files"}}})
	merged, err := base.WithOverlay(o)
	if err != nil {
		t.Fatal(err)
	}
	if merged.Hash() == hash {
		t.Fatal("a profile of the overlay kept the hash")
	}
	wc := workerOf(t, merged)
	if wc.Agents["triage"].Description != "Sorts every ticket." || !slices.Equal(wc.Agents["triage"].Tools, []string{"files"}) {
		t.Fatalf("the replaced profile = %+v", wc.Agents["triage"])
	}
	// The worker expands ${NAME} and reads $$ as one $, so the file holds
	// the escaped text, and the skills list stays empty.
	if h := wc.Agents["helper"]; h.System != "Use $${HOME} and $$$$5." || h.Skills == nil || len(h.Skills) != 0 {
		t.Fatalf("the added profile = %+v", h)
	}
	if got := merged.Profiles()["helper"].System; got != "Use ${HOME} and $$5." {
		t.Fatalf("Profiles = %q", got)
	}
	if base.files[0].Content != file || base.Hash() != hash || base.Profiles()["triage"].Description != "Sorts new tickets." {
		t.Fatal("the merge changed the pool of the configuration")
	}

	ruled, err := merged.WithOverlay(o.WithRule(OverlayRule{Rule: "tickets.create_ticket", By: "alice"}))
	if err != nil {
		t.Fatal(err)
	}
	if ruled.Hash() == merged.Hash() {
		t.Fatal("a rule of the overlay kept the hash")
	}
	wc = workerOf(t, ruled)
	if !slices.Contains(wc.Policy.Allow, "tickets.create_ticket") || slices.Contains(wc.Policy.Ask, "tickets.create_ticket") {
		t.Fatalf("policy = %+v", wc.Policy.Rules)
	}
	if d := ruled.Decide("triage", "tickets.create_ticket", nil); d.Verdict != fw.VerdictAllow {
		t.Fatalf("Decide after the rule = %+v", d)
	}
	if d := base.Decide("triage", "tickets.create_ticket", nil); d.Verdict != fw.VerdictAsk {
		t.Fatalf("Decide before the rule = %+v", d)
	}
}

// A profile that the workers could not load is an error of the merge, and
// the error names the key.
func TestWithOverlayChecksTheMergedConfiguration(t *testing.T) {
	base := loadFixture(t, fixture)[0]
	_, err := base.WithOverlay(Overlay{}.WithProfile(OverlayProfile{Profile: fw.AgentProfile{Name: "helper", Effort: "very"}}))
	if err == nil || !strings.Contains(err.Error(), "agents.helper.effort") {
		t.Fatalf("WithOverlay = %v", err)
	}
	// The static token must see the runs of every profile.
	_, err = base.WithOverlay(Overlay{}.WithProfile(OverlayProfile{Profile: fw.AgentProfile{Name: "helper", Access: &fw.Access{Owners: []string{"alice"}}}}))
	if err == nil || !strings.Contains(err.Error(), "kind:token") {
		t.Fatalf("WithOverlay of a blind profile = %v", err)
	}
}

// The dry run names each entry of the overlay in the comment of its pool.
func TestWriteNamesTheOverlay(t *testing.T) {
	base := loadFixture(t, fixture)[0]
	at := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
	merged, err := base.WithOverlay(Overlay{}.
		WithProfile(OverlayProfile{Profile: fw.AgentProfile{Name: "helper"}, By: "alice", At: at}).
		WithRule(OverlayRule{Rule: "tickets.create_ticket", By: "bob", At: at, Run: "01RUN"}))
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	if err := Write(&b, []*Pool{merged}); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, want := range []string{
		"# The overlay of the front sets the profile helper in fw.agents by alice at 2026-10-20T09:00:00Z.",
		`# The overlay of the front adds the rule "tickets.create_ticket" to fw.policy.allow by bob at 2026-10-20T09:00:00Z for the run 01RUN.`,
		"FW_POOL_SPEC=" + merged.Hash(),
		"helper:",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("the output lacks %q:\n%s", want, out)
		}
	}
}
