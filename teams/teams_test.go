package teams

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/bdubs00/featherweight-kernel/fw"
	"gopkg.in/yaml.v3"
)

// withBlock returns a configuration whose workbench block holds the YAML.
func withBlock(t *testing.T, src string) *fw.Config {
	t.Helper()
	cfg := fw.DefaultConfig()
	if src == "" {
		return cfg
	}
	var n yaml.Node
	if err := yaml.Unmarshal([]byte(src), &n); err != nil {
		t.Fatal(err)
	}
	cfg.Extensions = map[string]yaml.Node{Block: *n.Content[0]}
	return cfg
}

func TestPatterns(t *testing.T) {
	t.Setenv(EnvTeams, "")
	if got := Patterns(withBlock(t, "")); got != nil {
		t.Fatalf("no block = %v", got)
	}
	if got := Patterns(withBlock(t, "teams: [team-*, ' ops ', '']\n")); !slices.Equal(got, []string{"team-*", "ops"}) {
		t.Fatalf("block = %v", got)
	}
	// A block of the wrong shape gives no pattern, and the variable wins.
	if got := Patterns(withBlock(t, "teams: 7\n")); got != nil {
		t.Fatalf("bad block = %v", got)
	}
	t.Setenv(EnvTeams, "a-*, b")
	if got := Patterns(withBlock(t, "teams: [team-*]\n")); !slices.Equal(got, []string{"a-*", "b"}) {
		t.Fatalf("variable = %v", got)
	}
}

func TestOfAndOfAgent(t *testing.T) {
	carol := &fw.Principal{Subject: "carol", Kind: fw.KindUser, Groups: []string{"team-pay", "vpn", "team-ops", "team-pay"}}
	if got := Of([]string{"team-*"}, carol); !slices.Equal(got, []string{"team-ops", "team-pay"}) {
		t.Fatalf("teams of carol = %v", got)
	}
	if got := Of(nil, carol); !slices.Equal(got, []string{"team-ops", "team-pay", "vpn"}) {
		t.Fatalf("every group is a team = %v", got)
	}
	if got := Of([]string{"team-*"}, nil); got != nil {
		t.Fatalf("teams of the process = %v", got)
	}
	acc := &fw.Access{Owners: []string{"carol", "group:team-pay"}, Viewers: []string{"group:team-*", "group:vpn"}}
	if got := OfAgent([]string{"team-*"}, acc); !slices.Equal(got, []string{"team-pay"}) {
		t.Fatalf("teams of the agent = %v", got)
	}
	if got := OfAgent(nil, nil); got != nil {
		t.Fatalf("teams of an agent without access = %v", got)
	}
	if !match("team-?", "team-a") || match("team-?", "team-ab") || !match("*", "anything/with/slash") || match("", "x") {
		t.Fatal("match is wrong")
	}
}

func TestAnnotateAndBy(t *testing.T) {
	t.Setenv(EnvTeams, "team-*")
	cfg := fw.DefaultConfig()
	alice := &fw.Principal{Subject: "alice", Kind: fw.KindUser, Groups: []string{"team-pay", "vpn"}}
	ctx := fw.WithPrincipal(context.Background(), alice)
	// A person starts a run: the run keeps the teams of the person.
	got := Annotate(ctx, cfg, &fw.RunRequest{Principal: "alice"}, nil)
	if got[MetaKey] != "team-pay" {
		t.Fatalf("annotation = %v", got)
	}
	// A person with no team adds nothing, and so does the process.
	if got := Annotate(fw.WithPrincipal(context.Background(), &fw.Principal{Subject: "dave"}), cfg, &fw.RunRequest{Principal: "dave"}, nil); got != nil {
		t.Fatalf("annotation of dave = %v", got)
	}
	if got := Annotate(context.Background(), cfg, &fw.RunRequest{}, nil); got != nil {
		t.Fatalf("annotation of the process = %v", got)
	}
	// A child that the process starts keeps the teams of its parent.
	parent := &fw.Run{ID: "p", Principal: "alice", Metadata: map[string]string{MetaKey: "team-pay"}}
	if got := Annotate(context.Background(), cfg, &fw.RunRequest{Parent: "p", Principal: "alice"}, parent); got[MetaKey] != "team-pay" {
		t.Fatalf("child annotation = %v", got)
	}

	runs := []*fw.Run{
		{ID: "1", Principal: "alice", Metadata: map[string]string{MetaKey: "team-pay"}},
		{ID: "2", Principal: "bob", Metadata: map[string]string{MetaKey: "team-ops,team-pay"}},
		{ID: "3", Principal: "carol", Metadata: map[string]string{MetaKey: "team-ops"}},
		{ID: "4", Principal: "dave"},
	}
	ids := func(rs []*fw.Run) (s string) {
		for _, r := range rs {
			s += r.ID
		}
		return s
	}
	for by, want := range map[string]string{"": "1234", "me": "1", "team": "12", "others": "34", " Team ": "12"} {
		got, err := By(ctx, cfg, runs, by)
		if err != nil || ids(got) != want {
			t.Errorf("By(%q) = %s, %v, want %s", by, ids(got), err, want)
		}
	}
	if _, err := By(ctx, cfg, runs, "everyone"); !errors.Is(err, fw.ErrInvalidRequest) {
		t.Errorf("an unknown by = %v", err)
	}
	// The process itself started no run and has no team.
	if got, _ := By(context.Background(), cfg, runs, "others"); ids(got) != "1234" {
		t.Errorf("others of the process = %s", ids(got))
	}
}
