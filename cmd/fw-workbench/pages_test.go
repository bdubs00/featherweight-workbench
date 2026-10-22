package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
)

func writeConfig(t *testing.T, text string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fw.yaml")
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// pages export and pages import move the wiki of the bucket on the NATS bus
// to files and back.
func TestPagesExportImport(t *testing.T) {
	srv, err := server.NewServer(&server.Options{Host: "127.0.0.1", Port: -1, JetStream: true, StoreDir: t.TempDir(), NoLog: true, NoSigs: true})
	if err != nil {
		t.Fatal(err)
	}
	go srv.Start()
	t.Cleanup(srv.Shutdown)
	if !srv.ReadyForConnections(5 * time.Second) {
		t.Fatal("the NATS server did not start")
	}
	cfg := writeConfig(t, "bus:\n  driver: nats\n  url: "+srv.ClientURL()+"\n")
	src := t.TempDir()
	if err := os.MkdirAll(filepath.Join(src, "notes"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "notes", "plan.md"), []byte("# Plan\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "notes", "plan.txt"), []byte("no page"), 0o644); err != nil {
		t.Fatal(err)
	}
	var out, errs strings.Builder
	if code := runPages([]string{"import", "-config", cfg, src}, &out, &errs); code != 0 {
		t.Fatalf("import: exit %d: %s", code, errs.String())
	}
	if !strings.Contains(out.String(), "Wrote 1 page from") || !strings.Contains(errs.String(), "left out notes/plan.txt") {
		t.Fatalf("import said %q and %q", out.String(), errs.String())
	}
	dst := t.TempDir()
	out.Reset()
	errs.Reset()
	if code := runPages([]string{"export", "-config", cfg, dst}, &out, &errs); code != 0 {
		t.Fatalf("export: exit %d: %s", code, errs.String())
	}
	if !strings.Contains(out.String(), "Wrote 1 page and the index _index.md from the key-value bucket FW_PAGES") {
		t.Fatalf("export said %q", out.String())
	}
	if got, err := os.ReadFile(filepath.Join(dst, "notes", "plan.md")); err != nil || string(got) != "# Plan\n" {
		t.Fatalf("the exported page = %q, %v", got, err)
	}
}

// Both verbs need the bus nats, and say so.
func TestPagesNeedsNATS(t *testing.T) {
	for bus, want := range map[string]string{
		"":                       "the bus of the configuration is mem",
		"bus: {driver: mem}\n":   "the bus of the configuration is mem",
		"bus: {driver: redis}\n": "the bus of the configuration is redis",
	} {
		cfg := writeConfig(t, bus+"listen: \":0\"\n")
		var out, errs strings.Builder
		if code := runPages([]string{"export", "-config", cfg, t.TempDir()}, &out, &errs); code != 1 || !strings.Contains(errs.String(), want) || !strings.Contains(errs.String(), "nats") {
			t.Errorf("%q: exit %d: %s", bus, code, errs.String())
		}
	}
	var out, errs strings.Builder
	if code := runPages(nil, &out, &errs); code != 2 || !strings.Contains(errs.String(), "pages export") {
		t.Fatalf("no verb: exit %d: %s", code, errs.String())
	}
	errs.Reset()
	if code := runPages([]string{"export"}, &out, &errs); code != 2 {
		t.Fatalf("no directory: exit %d", code)
	}
}

// A NATS server that does not answer fails the verb at once.
func TestPagesServerDown(t *testing.T) {
	cfg := writeConfig(t, "bus:\n  driver: nats\n  url: nats://127.0.0.1:1\n")
	var out, errs strings.Builder
	start := time.Now()
	if code := runPages([]string{"export", "-config", cfg, t.TempDir()}, &out, &errs); code != 1 || !strings.Contains(errs.String(), "connect to the bus server") {
		t.Fatalf("exit %d: %s", code, errs.String())
	}
	if time.Since(start) > 30*time.Second {
		t.Fatalf("the verb waited %s", time.Since(start))
	}
}
