package titles

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"gopkg.in/yaml.v3"
)

// titleModel answers a title call with title and every other call with
// "done".
type titleModel struct {
	title string
	mu    sync.Mutex
	calls int
}

func (m *titleModel) Generate(_ context.Context, req *fw.Request) (*fw.Response, error) {
	if req.System == System {
		m.mu.Lock()
		m.calls++
		m.mu.Unlock()
		return &fw.Response{Content: []fw.Block{fw.Text(m.title)}, StopReason: fw.StopEndTurn, Usage: fw.Usage{InputTokens: 30, OutputTokens: 4}, Provider: "fake", Model: "t"}, nil
	}
	return &fw.Response{Content: []fw.Block{fw.Text("done")}, StopReason: fw.StopEndTurn, Usage: fw.Usage{InputTokens: 10, OutputTokens: 5}}, nil
}

func (m *titleModel) Stream(ctx context.Context, req *fw.Request, _ func(fw.Delta)) (*fw.Response, error) {
	return m.Generate(ctx, req)
}

func (m *titleModel) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

func newAgent(t *testing.T, m fw.Model) *fw.Agent {
	t.Helper()
	a, err := fw.New(fw.WithoutDefaults(), fw.WithAuthn(fw.NoAuthn{}), fw.WithModel(m),
		fw.WithLedger(fw.NewMemLedger()), fw.WithBus(fw.NewMemBus()),
		fw.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close(context.Background()) })
	return a
}

func waitTitle(t *testing.T, a *fw.Agent, id, want string) *fw.Run {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		r, err := a.Get(context.Background(), id)
		if err == nil && r.Title == want {
			return r
		}
		time.Sleep(10 * time.Millisecond)
	}
	r, _ := a.Get(context.Background(), id)
	t.Fatalf("title = %q, want %q", r.Title, want)
	return nil
}

func TestMode(t *testing.T) {
	t.Setenv(EnvTitles, "")
	cfg := fw.DefaultConfig()
	if Mode(cfg) != ModeModel || Mode(nil) != ModeModel {
		t.Fatal("the default mode is model")
	}
	var n yaml.Node
	if err := yaml.Unmarshal([]byte("titles: Input\n"), &n); err != nil {
		t.Fatal(err)
	}
	cfg.Extensions = map[string]yaml.Node{Block: *n.Content[0]}
	if Mode(cfg) != ModeInput {
		t.Fatalf("mode of the block = %q", Mode(cfg))
	}
	t.Setenv(EnvTitles, "model")
	if Mode(cfg) != ModeModel {
		t.Fatalf("the variable did not win: %q", Mode(cfg))
	}
}

func TestWantWritesATitle(t *testing.T) {
	t.Setenv(EnvTitles, "model")
	ctx := context.Background()
	m := &titleModel{title: "Title: \"Fix checkout retries.\""}
	a := newAgent(t, m)
	r, err := a.Run(ctx, "Investigate the intermittent checkout failure in the payments service")
	if err != nil || r.State != fw.StateCompleted {
		t.Fatalf("run = %+v, %v", r, err)
	}
	Want(a, r)
	got := waitTitle(t, a, r.ID, "Fix checkout retries")
	if got.Usage.InputTokens != 10+30 || got.State != fw.StateCompleted {
		t.Fatalf("the title call is not in the usage, or the run opened again: %+v", got)
	}
	// One call per run per process, also when Want sees the run again.
	Want(a, r, r)
	time.Sleep(50 * time.Millisecond)
	if m.count() != 1 {
		t.Fatalf("title calls = %d, want 1", m.count())
	}
}

func TestWantSkips(t *testing.T) {
	t.Setenv(EnvTitles, "model")
	ctx := context.Background()
	m := &titleModel{title: "Never"}
	a := newAgent(t, m)
	mine, err := a.Run(ctx, "Do the work", fw.WithMetadata(map[string]string{fw.MetaTitle: "Mine"}))
	if err != nil {
		t.Fatal(err)
	}
	child, err := a.Run(ctx, "Sub work", fw.WithParent(mine.ID))
	if err != nil {
		t.Fatal(err)
	}
	remote := &fw.Run{ID: "r", Input: "Elsewhere", Member: "other"}
	Want(a, mine, child, remote, nil)
	t.Setenv(EnvTitles, "input")
	plain, err := a.Run(ctx, "Do the other work")
	if err != nil {
		t.Fatal(err)
	}
	Want(a, plain)
	time.Sleep(50 * time.Millisecond)
	if m.count() != 0 || mine.Title != "Mine" || plain.Title != "Do the other work" {
		t.Fatalf("calls = %d, titles %q %q", m.count(), mine.Title, plain.Title)
	}
}

func TestClean(t *testing.T) {
	cases := map[string]string{
		"Fix checkout retries":                  "Fix checkout retries",
		"**Fix checkout retries.**":             "Fix checkout retries",
		"Sure.\n\nTitle: Release notes for 2.4": "Release notes for 2.4",
		"# “Rotate the keys”":                   "Rotate the keys",
		"   ":                                   "",
		"a​b":                                   "ab",
	}
	for in, want := range cases {
		if got := Clean(in); got != want {
			t.Errorf("Clean(%q) = %q, want %q", in, got, want)
		}
	}
	if got := Clean(strings.Repeat("word ", 40)); len([]rune(got)) > fw.MaxTitleRunes+1 {
		t.Errorf("long title %q", got)
	}
}
