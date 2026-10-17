package agents

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bdubs00/featherweight-kernel/cli"
	"github.com/bdubs00/featherweight-kernel/config"
	"github.com/bdubs00/featherweight-kernel/fw"
	"gopkg.in/yaml.v3"
)

// wireCLI sets the hooks of the verb agents as cmd/fw-workbench does, with
// a quiet fake model, and returns a runner of the command line.
func wireCLI(t *testing.T) (home string, run func(args ...string) (int, string, string)) {
	t.Helper()
	for _, n := range config.EnvNames() {
		t.Setenv(n, "")
	}
	t.Setenv("FW_CONFIG", "")
	t.Setenv("FW_ATESPACE", "test")
	home = t.TempDir()
	t.Setenv("FW_HOME", home)
	t.Chdir(t.TempDir())
	model := fw.ModelFunc{GenerateFn: func(context.Context, *fw.Request) (*fw.Response, error) {
		return &fw.Response{Content: []fw.Block{fw.Text("ok")}, StopReason: fw.StopEndTurn}, nil
	}}
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	cli.AgentOptions = func() []fw.Option { return []fw.Option{fw.WithLogger(quiet), fw.WithModel(model)} }
	cli.AgentsPut = func(ctx context.Context, a *fw.Agent, p fw.AgentProfile) error {
		_, _, err := Put(ctx, a, p)
		return err
	}
	cli.AgentsDelete = Delete
	t.Cleanup(func() { cli.AgentOptions, cli.AgentsPut, cli.AgentsDelete = nil, nil, nil })
	run = func(args ...string) (int, string, string) {
		var stdout, stderr strings.Builder
		code := cli.Run(context.Background(), args, strings.NewReader(""), &stdout, &stderr)
		return code, stdout.String(), stderr.String()
	}
	return home, run
}

// fw-workbench agents put and delete write and remove the file of a
// profile, as the local principal, and refuse a profile that fw.yaml
// defines.
func TestAgentsVerbWrites(t *testing.T) {
	home, run := wireCLI(t)
	path := filepath.Join(t.TempDir(), "fw.yaml")
	if err := os.WriteFile(path, []byte("agents:\n  fixed:\n    description: From fw.yaml\n    model: anthropic/claude-haiku-5-5\n    access: {owners: [alice], viewers: [\"*\"]}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FW_CONFIG", path)

	// Put: a new profile from a file, owned by the local principal.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "prompt.md"), []byte("You write.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "writer.yaml")
	if err := os.WriteFile(file, []byte("description: Writes\nsystem: prompt.md\ntools: [files]\nskills: []\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := run("agents", "put", file)
	if code != 0 {
		t.Fatalf("put: exit %d\n%s", code, errOut)
	}
	if !strings.Contains(out, "created the profile writer under "+filepath.Join(home, "agents")) || !strings.Contains(out, "next start") {
		t.Errorf("put output: %s", out)
	}
	stored, err := os.ReadFile(filepath.Join(home, "agents", "writer.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var p fw.AgentProfile
	if err := yaml.Unmarshal(stored, &p); err != nil {
		t.Fatal(err)
	}
	if p.Name != "writer" || p.System != "You write.\n" || p.Access == nil || !strings.HasPrefix(p.Access.Owners[0], "local:") || p.Skills == nil || len(p.Skills) != 0 {
		t.Errorf("stored profile: %+v access %+v", p, p.Access)
	}
	owner := p.Access.Owners[0]

	// The next process lists and runs the new profile.
	code, out, _ = run("agents")
	if code != 0 || strings.Count(out, "\n") != 3 || !strings.Contains(out, "writer") {
		t.Errorf("list: exit %d\n%s", code, out)
	}
	if code, _, errOut := run("run", "--agent", "writer", "go"); code != 0 {
		t.Errorf("run: exit %d\n%s", code, errOut)
	}

	// Replace with -name: the file has no access block, so the profile
	// keeps its owner. Then delete, which the owner can.
	code, out, _ = run("agents", "put", "--name", "writer", file)
	if code != 0 || !strings.Contains(out, "replaced the profile writer") {
		t.Errorf("replace: exit %d\n%s", code, out)
	}
	var got fw.AgentProfile
	code, out, _ = run("agents", "--json", "writer")
	if code != 0 || json.Unmarshal([]byte(out), &got) != nil || got.Access == nil || got.Access.Owners[0] != owner {
		t.Errorf("after the replace: exit %d\n%s", code, out)
	}
	code, out, errOut = run("agents", "delete", "writer")
	if code != 0 || !strings.Contains(out, "deleted the profile writer") {
		t.Errorf("delete: exit %d\n%s%s", code, out, errOut)
	}
	if _, err := os.Stat(filepath.Join(home, "agents", "writer.yaml")); err == nil {
		t.Error("the file remains after the delete")
	}

	// A fw.yaml profile: forbidden without the admin role, and fixed with it.
	if code, _, errOut := run("agents", "put", "--name", "fixed", file); code != 1 || !strings.Contains(errOut, "forbidden") {
		t.Errorf("put over fw.yaml without a role: exit %d\n%s", code, errOut)
	}
	t.Setenv("FW_ACCESS_ADMINS", "local:*")
	if code, _, errOut := run("agents", "put", "--name", "fixed", file); code != 1 || !strings.Contains(errOut, "change it there") {
		t.Errorf("put over fw.yaml: exit %d\n%s", code, errOut)
	}
	if code, _, errOut := run("agents", "delete", "fixed"); code != 1 || !strings.Contains(errOut, "change it there") {
		t.Errorf("delete of fw.yaml: exit %d\n%s", code, errOut)
	}
	if code, _, errOut := run("agents", "delete", "nope"); code != 1 || !strings.Contains(errOut, "not found") {
		t.Errorf("delete unknown: exit %d\n%s", code, errOut)
	}
	if err := os.WriteFile(file, []byte("sytem: typo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, _, errOut := run("agents", "put", file); code != 1 || !strings.Contains(errOut, "sytem") {
		t.Errorf("bad file: exit %d\n%s", code, errOut)
	}
}
