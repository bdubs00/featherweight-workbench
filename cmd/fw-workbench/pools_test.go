package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
	var out, errs strings.Builder
	if code := runPools([]string{"render", path}, &out, &errs); code != 0 {
		t.Fatalf("exit %d: %s", code, errs.String())
	}
	text := out.String()
	for _, secret := range []string{"a-real-token-value", "a-real-tickets-token"} {
		if strings.Contains(text, secret) {
			t.Fatalf("the dry run holds the secret %q", secret)
		}
	}
	for _, want := range []string{"name: triage-0-ws", "name: triage-2", "kind: Task", "intake: fw.jobs.triage", "<secret: FW_API_TOKEN of the front>", "<secret: TICKETS_TOKEN of the front>"} {
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
