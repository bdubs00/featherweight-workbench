package skills

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// writeSkill writes a skill directory with its SKILL.md and other files.
func writeSkill(t *testing.T, root, dir, text string, files map[string]string) {
	t.Helper()
	d := filepath.Join(root, dir)
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, fw.SkillFile), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	for p, body := range files {
		full := filepath.Join(d, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// newAgent returns an agent whose home holds the skills directory, and a
// buffer with its log.
func newAgent(t *testing.T, opts ...fw.Option) (*fw.Agent, string, *bytes.Buffer) {
	t.Helper()
	cfg := fw.DefaultConfig()
	cfg.Home = t.TempDir()
	cfg.Auth.Mode = "none"
	logs := &bytes.Buffer{}
	a, err := fw.New(append([]fw.Option{
		fw.WithConfig(cfg),
		fw.WithoutDefaults(),
		fw.WithModel(fw.ModelFunc{}),
		fw.WithLedger(fw.NewMemLedger()),
		fw.WithBus(fw.NewMemBus()),
		fw.WithLogger(slog.New(slog.NewTextHandler(logs, nil))),
	}, opts...)...)
	if err != nil {
		t.Fatalf("fw.New: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		a.Close(ctx)
	})
	return a, filepath.Join(cfg.Home, "skills"), logs
}

func TestCatalogReadsTheFrontMatter(t *testing.T) {
	a, root, logs := newAgent(t)
	writeSkill(t, root, "present", `---
name: present
description: Has every key.
user-invocable: false
disable-model-invocation: true
argument-hint: "<file> [mode]"
arguments:
  - file
  - name: mode
    description: How to read the file.
---
Read $file in $mode.
`, map[string]string{"scripts/run.sh": "echo hi\n", "ref.md": "x"})
	writeSkill(t, root, "absent", "---\nname: absent\ndescription: Has no key.\n---\nBody.\n", nil)
	writeSkill(t, root, "wrong", `---
name: wrong
description: Has keys of the wrong type.
user-invocable: maybe
disable-model-invocation: [yes]
argument-hint: {a: b}
arguments: 5
---
Body.
`, nil)
	writeSkill(t, root, "hints", "---\nname: hints\nargument-hint: [issue] [priority]\narguments: issue priority\n---\nFix $issue at $priority.\n", nil)
	writeSkill(t, root, "list", "---\nname: list\nargument-hint: [issue]\n---\nBody.\n", nil)
	writeSkill(t, filepath.Join(root, ImportedDir), "fetched", "---\nname: fetched\ndescription: An import.\n---\nBody.\n", nil)
	writeSkill(t, filepath.Join(root, ImportedDir), "absent", "---\nname: absent\ndescription: Shadowed.\n---\nBody.\n", nil)

	got := map[string]Entry{}
	var names []string
	for _, e := range Catalog(a) {
		got[e.Name] = e
		names = append(names, e.Name)
	}
	if want := []string{"absent", "fetched", "hints", "list", "present", "wrong"}; !reflect.DeepEqual(names, want) {
		t.Fatalf("names = %v, want %v", names, want)
	}
	p := got["present"]
	if p.UserInvocable || p.ModelInvocable || p.ArgumentHint != "<file> [mode]" || p.Origin != OriginLocal || p.URI != "present/SKILL.md" {
		t.Fatalf("present = %+v", p)
	}
	if want := []Argument{{Name: "file"}, {Name: "mode", Description: "How to read the file."}}; !reflect.DeepEqual(p.Arguments, want) {
		t.Fatalf("arguments = %+v", p.Arguments)
	}
	if want := []string{"ref.md", "scripts/run.sh"}; !reflect.DeepEqual(p.Files, want) {
		t.Fatalf("files = %v", p.Files)
	}
	ab := got["absent"]
	if !ab.UserInvocable || !ab.ModelInvocable || ab.ArgumentHint != "" || len(ab.Arguments) != 0 || ab.Files == nil || ab.Description != "Has no key." {
		t.Fatalf("absent = %+v", ab)
	}
	w := got["wrong"]
	if !w.UserInvocable || !w.ModelInvocable || w.ArgumentHint != "" || len(w.Arguments) != 0 {
		t.Fatalf("wrong = %+v", w)
	}
	for _, key := range []string{KeyUserInvocable, KeyDisableModel, KeyArgumentHint, KeyArguments} {
		if !strings.Contains(logs.String(), key) {
			t.Fatalf("the log names no problem with %s:\n%s", key, logs.String())
		}
	}
	// A hint of two lists is no YAML: the keys come one per line.
	h := got["hints"]
	if h.ArgumentHint != "[issue] [priority]" {
		t.Fatalf("hints = %+v", h)
	}
	if l := got["list"]; l.ArgumentHint != "[issue]" {
		t.Fatalf("list = %+v", l)
	}
	f := got["fetched"]
	if f.Origin != OriginImported || f.URI != "fetched/SKILL.md" {
		t.Fatalf("fetched = %+v", f)
	}
	body, err := p.Body()
	if err != nil || body != "Read $file in $mode.\n" {
		t.Fatalf("body = %q, %v", body, err)
	}
}

func TestAllowedMatchesAsTheKernel(t *testing.T) {
	a, root, _ := newAgent(t)
	writeSkill(t, root, "review-go", "---\nname: review-go\ndescription: Go.\n---\n", nil)
	writeSkill(t, root, "review-sql", "---\nname: review-sql\n---\n", nil)
	writeSkill(t, root, "deploy", "---\nname: deploy\n---\n", nil)
	names := func(refs []Ref) []string {
		out := []string{}
		for _, r := range refs {
			out = append(out, r.Name)
		}
		return out
	}
	if got := names(Allowed(a, []string{"review-*"})); !reflect.DeepEqual(got, []string{"review-go", "review-sql"}) {
		t.Fatalf("glob = %v", got)
	}
	if got := names(Allowed(a, nil)); len(got) != 3 {
		t.Fatalf("nil = %v", got)
	}
	if got := Allowed(a, []string{}); got == nil || len(got) != 0 {
		t.Fatalf("empty = %v", got)
	}
	if got := Allowed(a, []string{"review-go"}); got[0].Description != "Go." || got[0].Origin != "" {
		t.Fatalf("ref = %+v", got[0])
	}
}

func TestExpand(t *testing.T) {
	cases := []struct {
		name, body, args string
		names            []string
		want             string
	}{
		{"whole", "Fix $ARGUMENTS now.", "issue 42", nil, "Fix issue 42 now."},
		{"indexed", "A=$ARGUMENTS[0] B=$ARGUMENTS[1]", "x y", nil, "A=x B=y"},
		{"dollar number", "$0 then $1", "x y", nil, "x then y"},
		{"two digits", "$10", "a b c d e f g h i j k", nil, "k"},
		{"named", "Fix $issue on $branch.", "42 main", []string{"issue", "branch"}, "Fix 42 on main."},
		{"named missing", "Fix $issue on $branch.", "42", []string{"issue", "branch"}, "Fix 42 on ."},
		{"index missing stays", "Use $3 and $ARGUMENTS[5].", "a", nil, "Use $3 and $ARGUMENTS[5].\n\nARGUMENTS: a"},
		{"no placeholder appends", "Review the change.\n", "the auth module", nil, "Review the change.\n\nARGUMENTS: the auth module"},
		{"no placeholder no args", "Review the change.", "", nil, "Review the change."},
		{"other dollar stays", "echo $HOME $PATH", "x", nil, "echo $HOME $PATH\n\nARGUMENTS: x"},
		{"escape", `Keep \$1 and use $1.`, "a b", nil, "Keep $1 and use b."},
		{"empty args whole", "Args: [$ARGUMENTS]", "", nil, "Args: []"},
		{"longer name stays", "$ARGUMENTSX $issues", "1", []string{"issue"}, "$ARGUMENTSX $issues\n\nARGUMENTS: 1"},
		{"bracket text", "$ARGUMENTS[x]", "a", nil, "a[x]"},
		{"spaces split", "<$0|$1>", "  a \t b  ", nil, "<a|b>"},
	}
	for _, c := range cases {
		if got := Expand(c.body, c.args, c.names); got != c.want {
			t.Errorf("%s: Expand(%q, %q) = %q, want %q", c.name, c.body, c.args, got, c.want)
		}
	}
}

func TestPrepare(t *testing.T) {
	a, root, _ := newAgent(t)
	writeSkill(t, root, "fix", "---\nname: fix\ndescription: Fixes an issue.\narguments: [issue]\n---\nFix issue $issue.\n", nil)
	writeSkill(t, root, "hidden", "---\nname: hidden\nuser-invocable: false\n---\nBody.\n", nil)
	writeSkill(t, root, "huge", "---\nname: huge\n---\n"+strings.Repeat("x", MaxMessageBytes+1), nil)
	writeSkill(t, root, "edge", "---\nname: edge\n---\n"+strings.Repeat("x", MaxMessageBytes-20), nil)

	x, err := Prepare(a, Command{Name: "fix", Args: "42"}, "Then open a pull request.")
	if err != nil {
		t.Fatal(err)
	}
	if want := "Skill: fix\n\nFix issue 42.\n\nThen open a pull request."; x.Message != want {
		t.Fatalf("message = %q", x.Message)
	}
	if want := "Skill: fix\nFixes an issue. arguments: 42. Then open a pull request."; x.Summary != want {
		t.Fatalf("summary = %q", x.Summary)
	}
	if _, err := Prepare(a, Command{Name: "nope"}, ""); !errors.Is(err, ErrUnknown) {
		t.Fatalf("unknown: %v", err)
	}
	if _, err := Prepare(a, Command{Name: "hidden"}, ""); !errors.Is(err, ErrNotInvocable) {
		t.Fatalf("hidden: %v", err)
	}
	if _, err := Prepare(a, Command{Name: "huge"}, ""); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("huge: %v", err)
	}
	if _, err := Prepare(a, Command{Name: "edge"}, strings.Repeat("y", 64)); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("edge with text: %v", err)
	}
	if _, err := Prepare(a, Command{}, ""); !errors.Is(err, fw.ErrInvalidRequest) {
		t.Fatalf("no name: %v", err)
	}
}
