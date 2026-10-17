package dashboard

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/surface/view"
)

func TestPage(t *testing.T) {
	p := Page()
	if len(p) == 0 || len(p) > 200<<10 {
		t.Fatalf("page is %d bytes", len(p))
	}
	head := strings.ToLower(string(p[:64]))
	if !strings.HasPrefix(head, "<!doctype html>") {
		t.Fatalf("page starts with %q", head)
	}
	for _, want := range []string{"ui/initialize", "ui/notifications/initialized", "ui/notifications/tool-result", "tools/call", "/v1/whoami", "sessionStorage", "ui/notifications/size-changed", "ui/resource-teardown", `name="timeZone"`} {
		if !bytes.Contains(p, []byte(want)) {
			t.Errorf("page lacks %q", want)
		}
	}
	if re := regexp.MustCompile(`(?i)<script[^>]+src=|<link[^>]+href=|@import|https?://`); re.Match(p) {
		t.Errorf("page loads an external resource: %s", re.Find(p))
	}
	if MIMEType != "text/html;profile=mcp-app" || !strings.HasPrefix(URI, "ui://") {
		t.Errorf("MIME type %q, URI %q", MIMEType, URI)
	}
	meta := ToolMeta()
	ui, _ := meta[MetaKey].(map[string]any)
	if ui["resourceUri"] != URI {
		t.Errorf("tool meta = %v", meta)
	}
}

func newAgent(t *testing.T, opts ...fw.Option) *fw.Agent {
	t.Helper()
	t.Setenv("FW_ATESPACE", "")
	model := fw.ModelFunc{GenerateFn: func(context.Context, *fw.Request) (*fw.Response, error) {
		return &fw.Response{Content: []fw.Block{fw.Text("ok")}, StopReason: fw.StopEndTurn, Usage: fw.Usage{InputTokens: 10, OutputTokens: 5}}, nil
	}}
	base := []fw.Option{
		fw.WithoutDefaults(),
		fw.WithAuthn(fw.NoAuthn{}),
		fw.WithModel(model),
		fw.WithLedger(fw.NewMemLedger()),
		fw.WithBus(fw.NewMemBus()),
		fw.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))),
	}
	a, err := fw.New(append(append(base, fw.WithTitles(fw.TitlesInput)), opts...)...)
	if err != nil {
		t.Fatalf("fw.New: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		a.Close(ctx)
	})
	return a
}

func mustWhoami(t *testing.T, ctx context.Context, a *fw.Agent) view.Whoami {
	t.Helper()
	w, err := view.WhoamiOf(ctx, a)
	if err != nil {
		t.Fatalf("WhoamiOf: %v", err)
	}
	return w
}

// countingAuthz allows everything and counts its calls. It implements
// fw.BatchAuthz, so a batch of checks is one call.
type countingAuthz struct {
	mu     sync.Mutex
	single int
	batch  int
	checks int
	err    error
}

func (c *countingAuthz) Allow(context.Context, *fw.Principal, string, fw.Target) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.single++
	return c.err
}

func (c *countingAuthz) AllowAll(_ context.Context, _ *fw.Principal, checks []fw.AuthzCheck) []error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.batch++
	c.checks += len(checks)
	out := make([]error, len(checks))
	for i := range out {
		out[i] = c.err
	}
	return out
}

func (c *countingAuthz) calls() (single, batch, checks int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.single, c.batch, c.checks
}

func TestUsages(t *testing.T) {
	ctx := context.Background()
	a := newAgent(t,
		fw.WithProfile(fw.AgentProfile{Name: "reviewer", Description: "Reviews code", Access: &fw.Access{Owners: []string{"alice"}, Viewers: []string{"bob"}}}),
		fw.WithProfile(fw.AgentProfile{Name: "writer", Access: &fw.Access{Operators: []string{"group:docs"}}}),
	)
	alice := fw.WithPrincipal(ctx, &fw.Principal{Subject: "alice", Kind: fw.KindUser})
	bob := fw.WithPrincipal(ctx, &fw.Principal{Subject: "bob", Kind: fw.KindUser, Groups: []string{"docs"}})
	if _, err := a.Run(alice, "review", fw.AsAgent("reviewer")); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Run(bob, "write", fw.AsAgent("writer")); err != nil {
		t.Fatal(err)
	}

	rep, err := Usages(alice, a)
	if err != nil || rep.Total.Runs != 1 || rep.ByAgent["reviewer"].Runs != 1 || rep.ByPrincipal["alice"].Turns != 1 || len(rep.ByAgent) != 1 {
		t.Fatalf("alice report = %+v, %v", rep, err)
	}
	rep, _ = Usages(ctx, a)
	if rep.Total.Runs != 2 || !strings.Contains(rep.String(), "By principal") {
		t.Fatalf("report = %+v", rep)
	}
}
