package agents

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// memStore keeps the puts and the deletes.
type memStore struct {
	puts    []fw.AgentProfile
	deletes []string
}

func (m *memStore) Put(_ context.Context, p fw.AgentProfile) error {
	m.puts = append(m.puts, p)
	return nil
}
func (m *memStore) Delete(_ context.Context, name string) error {
	m.deletes = append(m.deletes, name)
	return nil
}

func useMemStore(t *testing.T) *memStore {
	t.Helper()
	store := &memStore{}
	old := OpenStore
	OpenStore = func(*fw.Config) (Store, error) { return store, nil }
	t.Cleanup(func() { OpenStore = old })
	return store
}

func newAgent(t *testing.T, cfg *fw.Config, opts ...fw.Option) *fw.Agent {
	t.Helper()
	model := fw.ModelFunc{GenerateFn: func(context.Context, *fw.Request) (*fw.Response, error) {
		return &fw.Response{Content: []fw.Block{fw.Text("ok")}, StopReason: fw.StopEndTurn}, nil
	}}
	base := []fw.Option{fw.WithoutDefaults(), fw.WithAuthn(fw.NoAuthn{}), fw.WithModel(model),
		fw.WithLedger(fw.NewMemLedger()), fw.WithBus(fw.NewMemBus()),
		fw.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil)))}
	if cfg != nil {
		base = append(base, fw.WithConfig(cfg))
	}
	a, err := fw.New(append(base, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close(context.Background()) })
	return a
}

// The creator owns a new profile, an edit keeps the access block, and the
// roles decide who may change or remove a profile.
func TestPutAndDelete(t *testing.T) {
	store := useMemStore(t)
	ctx := context.Background()
	cfg := fw.DefaultConfig()
	cfg.Home = t.TempDir()
	cfg.Access = fw.AccessConfig{Default: "closed", Admins: []string{"root"}, Creators: []string{"kind:user"}}
	a := newAgent(t, cfg)
	alice := fw.WithPrincipal(ctx, &fw.Principal{Subject: "alice", Kind: fw.KindUser})
	bob := fw.WithPrincipal(ctx, &fw.Principal{Subject: "bob", Kind: fw.KindUser})
	wl := fw.WithPrincipal(ctx, &fw.Principal{Subject: "spiffe://td/x", Kind: fw.KindWorkload})

	if _, _, err := Put(wl, a, fw.AgentProfile{Name: "bot"}); !errors.Is(err, fw.ErrForbidden) {
		t.Fatalf("workload create = %v", err)
	}
	if _, _, err := Put(alice, a, fw.AgentProfile{Name: "../bad"}); !errors.Is(err, fw.ErrInvalidRequest) {
		t.Fatalf("bad name = %v", err)
	}
	if _, _, err := Put(alice, a, fw.AgentProfile{Name: "writer", System: "Write well.", Effort: "turbo"}); !errors.Is(err, fw.ErrInvalidRequest) {
		t.Fatalf("bad effort = %v", err)
	}
	v, created, err := Put(alice, a, fw.AgentProfile{Name: " writer ", System: "Write well.", Effort: "LOW", TimeZone: " America/Chicago ", Policy: &fw.Rules{Default: "Ask"}})
	if err != nil || !created || v.Name != "writer" || v.Effort != "low" || v.TimeZone != "America/Chicago" || v.Policy.Default != "ask" || v.Role != "owner" {
		t.Fatalf("create = %+v, %v, %v", v, created, err)
	}
	p, ok := a.ProfileNamed("writer")
	if !ok || p.Access == nil || strings.Join(p.Access.Owners, ",") != "alice" || len(store.puts) != 1 || store.puts[0].Access.Owners[0] != "alice" {
		t.Fatalf("profile = %+v, store puts = %d", p, len(store.puts))
	}
	// The creator owns the profile: alice edits, bob cannot. An edit without
	// an access block keeps the owners.
	if _, _, err := Put(bob, a, fw.AgentProfile{Name: "writer", System: "Mine now."}); !errors.Is(err, fw.ErrForbidden) {
		t.Fatalf("bob edit = %v", err)
	}
	v, created, err = Put(alice, a, fw.AgentProfile{Name: "writer", System: "Write better."})
	if err != nil || created || v.Access == nil || v.Access.Owners[0] != "alice" || v.System != "Write better." {
		t.Fatalf("edit = %+v, %v, %v", v, created, err)
	}
	if _, _, err := Put(alice, a, fw.AgentProfile{Name: "writer", Access: &fw.Access{Owners: []string{" alice ", ""}, Operators: []string{"bob"}}}); err != nil {
		t.Fatal(err)
	}
	if r, err := a.Run(bob, "go", fw.AsAgent("writer")); err != nil || r.State != fw.StateCompleted {
		t.Fatalf("run by an operator = %+v, %v", r, err)
	}
	if err := Delete(bob, a, "writer"); !errors.Is(err, fw.ErrForbidden) {
		t.Fatalf("bob delete = %v", err)
	}
	if err := Delete(alice, a, "writer"); err != nil {
		t.Fatal(err)
	}
	if _, ok := a.ProfileNamed("writer"); ok || len(store.deletes) != 1 {
		t.Fatalf("profile still there, deletes = %d", len(store.deletes))
	}
	if err := Delete(alice, a, "writer"); !errors.Is(err, fw.ErrAgentNotFound) {
		t.Fatalf("second delete = %v", err)
	}
}

// A profile that the configuration defines cannot change, and a dry run
// says what a save would do and saves nothing.
func TestLockedAndPreview(t *testing.T) {
	store := useMemStore(t)
	ctx := fw.WithPrincipal(context.Background(), &fw.Principal{Subject: "alice", Kind: fw.KindUser})
	cfg := fw.DefaultConfig()
	cfg.Home = t.TempDir()
	cfg.Agents = map[string]fw.AgentProfile{"fixed": {Description: "From fw.yaml", Access: &fw.Access{Owners: []string{"alice"}}}}
	a := newAgent(t, cfg)
	if _, _, err := Put(ctx, a, fw.AgentProfile{Name: "fixed", Description: "changed"}); !errors.Is(err, fw.ErrInvalidRequest) || !strings.Contains(err.Error(), "change it there") {
		t.Fatalf("put of a fixed profile = %v", err)
	}
	if err := Delete(ctx, a, "fixed"); !errors.Is(err, fw.ErrInvalidRequest) {
		t.Fatalf("delete of a fixed profile = %v", err)
	}
	d, err := Preview(ctx, a, fw.AgentProfile{Name: "fresh", Description: "new", Tools: []string{"files"}})
	if err != nil || d.Exists || d.Name != "fresh" || d.Role != "owner" {
		t.Fatalf("draft = %+v, %v", d, err)
	}
	if _, ok := a.ProfileNamed("fresh"); ok || len(store.puts) != 0 {
		t.Fatal("a dry run saved the profile")
	}
	if _, _, err := Put(ctx, a, fw.AgentProfile{Name: "fresh"}); err != nil {
		t.Fatal(err)
	}
	d, err = Preview(ctx, a, fw.AgentProfile{Name: "fresh", Description: "changed"})
	if err != nil || !d.Exists || d.Role != "owner" {
		t.Fatalf("draft of an existing profile = %+v, %v", d, err)
	}
	if _, err := Preview(ctx, a, fw.AgentProfile{Name: "bad", Policy: &fw.Rules{Default: "maybe"}}); !errors.Is(err, fw.ErrInvalidRequest) {
		t.Errorf("a bad policy default: %v", err)
	}
	if _, err := Preview(ctx, a, fw.AgentProfile{Name: "bad", TimeZone: "Mars/Olympus"}); !errors.Is(err, fw.ErrInvalidRequest) || !strings.Contains(err.Error(), "timeZone") {
		t.Errorf("a bad time zone: %v", err)
	}
}

// The default store writes a file under <home>/agents, and a new kernel
// over the same home reads it back.
func TestFileStoreRoundTrip(t *testing.T) {
	ctx := fw.WithPrincipal(context.Background(), &fw.Principal{Subject: "alice", Kind: fw.KindUser})
	home := t.TempDir()
	cfg := fw.DefaultConfig()
	cfg.Home = home
	a := newAgent(t, cfg)
	if _, _, err := Put(ctx, a, fw.AgentProfile{Name: "writer", System: "You write.", Skills: []string{}, Compaction: &fw.CompactionConfig{Off: true}}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FW_HOME", home)
	t.Setenv("FW_CONFIG", "")
	b := newAgent(t, nil, fw.WithConfigFile(""))
	p, ok := b.ProfileNamed("writer")
	if !ok || p.System != "You write." || p.Access == nil || p.Access.Owners[0] != "alice" || p.Skills == nil || len(p.Skills) != 0 ||
		p.Compaction == nil || !p.Compaction.Off {
		t.Fatalf("profile after a restart = %+v, %v", p, ok)
	}
	if b.ProfileLocked("writer") {
		t.Error("a profile from a file is locked")
	}
}

// Only the store of the routines gives a profile its routines: a put with
// the static token or a person saves the profile without them, and so
// does a dry run.
func TestPutDropsRoutines(t *testing.T) {
	store := useMemStore(t)
	cfg := fw.DefaultConfig()
	cfg.Home = t.TempDir()
	a := newAgent(t, cfg)
	token := fw.WithPrincipal(context.Background(), &fw.Principal{Subject: fw.KindToken, Kind: fw.KindToken})
	live := []fw.Routine{{Name: "close-all", Version: 1, Standing: fw.RoutineLive, Steps: []fw.RoutineStep{{Tool: "bash", Input: `{"command": "true"}`}}}}
	if d, err := Preview(token, a, fw.AgentProfile{Name: "writer", Routines: live}); err != nil || d.Routines != nil {
		t.Fatalf("Preview = %+v, %v", d.Routines, err)
	}
	if _, _, err := Put(token, a, fw.AgentProfile{Name: "writer", Routines: live}); err != nil {
		t.Fatal(err)
	}
	p, _ := a.ProfileNamed("writer")
	if p.Routines != nil || len(store.puts) != 1 || store.puts[0].Routines != nil {
		t.Fatalf("the saved profile holds routines: %+v", p.Routines)
	}
}
