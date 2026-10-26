package main

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/fleet"
	"github.com/bdubs00/featherweight-workbench/internal/bucket"
	"github.com/bdubs00/featherweight-workbench/pools"
)

// pools render prints the Workspaces and the Tasks of the example with a
// placeholder for each secret of the front, and never its value.
func TestPoolsRender(t *testing.T) {
	t.Setenv("FW_API_TOKEN", "a-real-token-value")
	t.Setenv("TICKETS_TOKEN", "a-real-tickets-token")
	example, err := os.ReadFile(filepath.Join("..", "..", "examples", "pools", "fw.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "fw.yaml")
	if err := os.WriteFile(path, example, 0o600); err != nil {
		t.Fatal(err)
	}
	// The front keeps an overlay for the pool, which the dry run merges.
	store := pools.NewMemOverlays()
	_ = store.PutProfile(context.Background(), "triage", pools.OverlayProfile{Profile: fw.AgentProfile{Name: "helper", Description: "Helps the triage."}, By: "alice"})
	old, oldR, oldT := fleet.OpenOverlays, fleet.OpenRuntime, fleet.Routines
	fleet.OpenOverlays = func(context.Context, *fw.Config, *slog.Logger) (pools.OverlayStore, func(), error) {
		return store, nil, nil
	}
	// The store of the routines gives triage a rehearsing routine.
	fleet.Routines = func(context.Context, *fw.Agent, *fw.Config) (map[string][]fw.Routine, error) {
		return map[string][]fw.Routine{"triage": {{Name: "close-duplicate", Version: 2, Standing: fw.RoutineShadow, Steps: []fw.RoutineStep{{Tool: "files", Input: "{}"}}}}}, nil
	}
	// The example offers a template, so the dry run reads the runtime
	// pools too; the store in memory holds none.
	fleet.OpenRuntime = func(context.Context, *fw.Config, *slog.Logger, bool) (*pools.Store, func(), error) {
		return pools.NewStore(bucket.NewMem(pools.StoreBucket), nil), nil, nil
	}
	t.Cleanup(func() { fleet.OpenOverlays, fleet.OpenRuntime, fleet.Routines = old, oldR, oldT })
	var out, errs strings.Builder
	if code := runPools([]string{"render", path}, &out, &errs); code != 0 {
		t.Fatalf("exit %d: %s", code, errs.String())
	}
	text := out.String()
	if !strings.Contains(text, "The routines of the server give the profile triage the routines close-duplicate version 2 (shadow).") || errs.Len() > 0 {
		t.Errorf("the dry run does not name the routine: %s", errs.String())
	}
	for _, secret := range []string{"a-real-token-value", "a-real-tickets-token"} {
		if strings.Contains(text, secret) {
			t.Fatalf("the dry run holds the secret %q", secret)
		}
	}
	for _, want := range []string{"name: triage-0-ws", "name: triage-2", "kind: Task", "intake: fw.jobs.triage", "<secret: FW_API_TOKEN of the server>", "<secret: TICKETS_TOKEN of the server>",
		"The overlay of the server sets the profile helper in fw.agents by alice", "Helps the triage."} {
		if !strings.Contains(text, want) {
			t.Errorf("the dry run has no %q", want)
		}
	}

	// A configuration that does not fit fails with the problem.
	bad := strings.Replace(string(example), "replicas: 3", "replicas: -2", 1)
	if err := os.WriteFile(path, []byte(bad), 0o600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	errs.Reset()
	if code := runPools([]string{"render", "-config", path}, &out, &errs); code != 1 || !strings.Contains(errs.String(), "replicas -2 is below 0") {
		t.Fatalf("exit %d: %s", code, errs.String())
	}
	if code := runPools(nil, &out, &errs); code != 2 {
		t.Fatalf("pools without a verb = %d", code)
	}
}

// pools render prints the runtime pools of the store after the pools of
// the file, each marked, with its overlay merged. A runtime pool whose name
// a pool of the file holds, and one whose template is gone, are left out,
// and standard error names each.
func TestPoolsRenderRuntime(t *testing.T) {
	t.Setenv("FW_API_TOKEN", "a-real-token-value")
	t.Setenv("TICKETS_TOKEN", "a-real-tickets-token")
	example, err := os.ReadFile(filepath.Join("..", "..", "examples", "pools", "fw.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "fw.yaml")
	if err := os.WriteFile(path, example, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	overlays := pools.NewMemOverlays()
	_ = overlays.PutProfile(ctx, "helper", pools.OverlayProfile{Profile: fw.AgentProfile{Name: "helper", Description: "Helps the people.", Access: &fw.Access{Owners: []string{"alice"}}}, By: "alice"})
	store := pools.NewStore(bucket.NewMem(pools.StoreBucket), nil)
	for _, e := range []pools.Runtime{
		{Name: "helper", Template: "general", Agent: "helper", Replicas: 2, By: "alice"},
		{Name: "triage", Template: "general", Agent: "other", Replicas: 1, By: "mallory"},
		{Name: "lost", Template: "gone", Agent: "lost", Replicas: 1},
		{Name: "waiting", Template: "general", Agent: "waiting", Replicas: 1},
	} {
		if _, err := store.Create(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	oldO, oldR, oldT := fleet.OpenOverlays, fleet.OpenRuntime, fleet.Routines
	fleet.OpenOverlays = func(context.Context, *fw.Config, *slog.Logger) (pools.OverlayStore, func(), error) {
		return overlays, nil, nil
	}
	fleet.Routines = func(context.Context, *fw.Agent, *fw.Config) (map[string][]fw.Routine, error) { return nil, nil }
	fleet.OpenRuntime = func(context.Context, *fw.Config, *slog.Logger, bool) (*pools.Store, func(), error) {
		return store, nil, nil
	}
	t.Cleanup(func() { fleet.OpenOverlays, fleet.OpenRuntime, fleet.Routines = oldO, oldR, oldT })
	var out, errs strings.Builder
	if code := runPools([]string{"render", path}, &out, &errs); code != 0 {
		t.Fatalf("exit %d: %s", code, errs.String())
	}
	text := out.String()
	if strings.Contains(text, "a-real-token-value") {
		t.Fatal("the dry run holds the token")
	}
	for _, want := range []string{"# Pool triage:", "# Runtime pool helper: 2 Tasks of the agent helper", "The pool takes each key that it does not set from the template general",
		"The store of the server holds this pool, not the configuration file. The entry is generation 1 by alice", "name: helper-1", "Helps the people.", "intake: fw.jobs.helper",
		"# Runtime pool waiting:", "The overlay holds no profile waiting yet"} {
		if !strings.Contains(text, want) {
			t.Errorf("the dry run has no %q", want)
		}
	}
	if strings.Contains(text, "Runtime pool triage") || strings.Contains(text, "lost-0") {
		t.Errorf("the dry run prints a pool that the front does not run")
	}
	for _, want := range []string{"the runtime pool triage has the name of a pool of the file", "names the template \"gone\""} {
		if !strings.Contains(errs.String(), want) {
			t.Errorf("standard error has no %q: %s", want, errs.String())
		}
	}
}
