package connections

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/tokens"
)

// fakeConnector connects the service tickets for anybody whose code is
// "good", and is the tool source whose tool acts for the person.
type fakeConnector struct {
	mu        sync.Mutex
	connected map[string]bool
	results   []string
}

func newFakeConnector() *fakeConnector { return &fakeConnector{connected: map[string]bool{}} }

func (*fakeConnector) Name() string { return "tickets" }

func (f *fakeConnector) Connection(_ context.Context, subject string) (Connection, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return Connection{Server: "tickets", Title: "Tickets", Connected: f.connected[subject]}, nil
}

func (*fakeConnector) Begin(_ context.Context, _, redirect string) (Attempt, error) {
	return Attempt{Server: "tickets", URL: "https://id.example/authorize?state=s1", State: "s1", Verifier: "v", Redirect: redirect}, nil
}

func (f *fakeConnector) Finish(ctx context.Context, subject string, _ Attempt, code string) (Connection, error) {
	if code != "good" {
		return Connection{}, errors.New("bad code")
	}
	f.mu.Lock()
	f.connected[subject] = true
	f.mu.Unlock()
	return f.Connection(ctx, subject)
}

func (f *fakeConnector) Disconnect(_ context.Context, subject string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.connected, subject)
	return nil
}

func (f *fakeConnector) Token(_ context.Context, subject string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.connected[subject] {
		return "", fw.ErrNotConnected
	}
	return "at-" + subject, nil
}

func (*fakeConnector) Reject(context.Context, string, string) {}

func (f *fakeConnector) Forget(ctx context.Context, subject string) { _ = f.Disconnect(ctx, subject) }

func (f *fakeConnector) Tools(context.Context) ([]fw.Tool, error) {
	return []fw.Tool{fw.NewTool(fw.ToolSpec{Name: "tickets.list", Source: "tickets"}, func(ctx context.Context, _ json.RawMessage) (fw.ToolResult, error) {
		info := fw.RunInfoFrom(ctx)
		tokens := fw.AgentFrom(ctx).PersonTokens()
		if tokens == nil {
			return fw.ErrorResult("no connections of people"), nil
		}
		if _, err := tokens.Token(ctx, info.Principal, "tickets"); errors.Is(err, fw.ErrNotConnected) {
			if err := fw.RequestConnect(ctx, fw.ConnectRequest{Server: "tickets", Title: "Tickets", Reason: fw.ConnectMissing}); err != nil {
				return fw.ToolResult{}, err
			}
		}
		res := "tickets of " + info.Principal
		f.mu.Lock()
		f.results = append(f.results, res)
		f.mu.Unlock()
		return fw.TextResult(res), nil
	})}, nil
}

// toolThenDone is a model that calls tool once and then ends.
func toolThenDone(tool string) fw.Model {
	var mu sync.Mutex
	step := 0
	return fw.ModelFunc{StreamFn: func(context.Context, *fw.Request, func(fw.Delta)) (*fw.Response, error) {
		mu.Lock()
		defer mu.Unlock()
		step++
		if step == 1 {
			return &fw.Response{Content: []fw.Block{{Type: fw.BlockToolUse, ID: "c1", Name: tool, Input: json.RawMessage(`{}`)}}, StopReason: fw.StopToolUse}, nil
		}
		return &fw.Response{Content: []fw.Block{fw.Text("done")}, StopReason: fw.StopEndTurn}, nil
	}}
}

func newAgent(t *testing.T, cfg *fw.Config, opts ...fw.Option) *fw.Agent {
	t.Helper()
	base := []fw.Option{fw.WithoutDefaults(), fw.WithAuthn(fw.NoAuthn{}), fw.WithModel(toolThenDone("tickets.list")),
		fw.WithLedger(fw.NewMemLedger()), fw.WithBus(fw.NewMemBus()), fw.WithLogger(slog.New(slog.DiscardHandler))}
	if cfg != nil {
		base = append(base, fw.WithConfig(cfg))
	}
	a, err := fw.New(append(base, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		a.Close(ctx)
	})
	return a
}

var ada = &fw.Principal{Subject: "ada", Kind: fw.KindUser}

func TestFinishResumesThePausedCall(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	fk := newFakeConnector()
	a := newAgent(t, nil, fw.WithToolSources(fk))
	c := New(a, fk)
	Install(a, c)
	if Of(a) != c || a.PersonTokens() != fw.PersonTokens(c) {
		t.Fatal("Install did not attach the connections")
	}
	pctx := fw.WithPrincipal(ctx, ada)
	r, err := a.Run(pctx, "list my tickets")
	if err != nil || r.State != fw.StateInputRequired || r.Pending == nil || r.Pending.Connect == nil || r.Pending.Connect.Server != "tickets" {
		t.Fatalf("Run = %+v, %v", r, err)
	}
	conns, err := List(ctx, a, ada)
	if err != nil || len(conns) != 1 || conns[0].Connected || conns[0].Title != "Tickets" {
		t.Fatalf("List = %+v, %v", conns, err)
	}
	at, err := Begin(ctx, a, ada, "tickets", "https://fw.example/ui/connect/callback")
	if err != nil || at.Redirect != "https://fw.example/ui/connect/callback" {
		t.Fatalf("Begin = %+v, %v", at, err)
	}
	if _, err := Finish(ctx, a, ada, at, "bad"); err == nil {
		t.Fatal("Finish took a bad code")
	}
	notices := make(chan tokens.Connected, 1)
	gone := make(chan tokens.Disconnected, 1)
	stop, err := a.Bus().Subscribe(ctx, fw.DefaultTokensSubject, func(e fw.Event) error {
		var n tokens.Connected
		var d tokens.Disconnected
		switch {
		case e.Type == tokens.EvConnected && e.Decode(&n) == nil:
			notices <- n
		case e.Type == tokens.EvDisconnected && e.Decode(&d) == nil:
			gone <- d
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	conns, err = Finish(ctx, a, ada, at, "good")
	if err != nil || len(conns) != 1 || !conns[0].Connected {
		t.Fatalf("Finish = %+v, %v", conns, err)
	}
	r, err = a.Wait(ctx, r.ID)
	if err != nil || r.State != fw.StateCompleted {
		t.Fatalf("Wait = %+v, %v", r, err)
	}
	// The other members learn about the connection from the bus.
	select {
	case n := <-notices:
		if n.Subject != "ada" || len(n.Servers) != 1 || n.Servers[0] != "tickets" {
			t.Fatalf("notice = %+v", n)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no notice of the connection on the bus")
	}
	fk.mu.Lock()
	results := strings.Join(fk.results, ",")
	fk.mu.Unlock()
	if results != "tickets of ada" {
		t.Fatalf("results = %q", results)
	}
	if tok, err := c.Token(ctx, "ada", "tickets"); err != nil || tok != "at-ada" {
		t.Fatalf("Token = %q, %v", tok, err)
	}
	if _, err := c.Token(ctx, "ada", "docs"); !errors.Is(err, ErrUnknownService) {
		t.Fatalf("Token of an unknown service = %v", err)
	}
	if err := Disconnect(ctx, a, ada, "tickets"); err != nil {
		t.Fatal(err)
	}
	if conns, _ := List(ctx, a, ada); conns[0].Connected {
		t.Fatal("still connected after Disconnect")
	}
	// The members and the other fronts learn about the disconnect too.
	select {
	case d := <-gone:
		if d.Subject != "ada" || len(d.Servers) != 1 || d.Servers[0] != "tickets" {
			t.Fatalf("notice of the disconnect = %+v", d)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no notice of the disconnect on the bus")
	}
	for _, p := range []*fw.Principal{nil, {Subject: "token", Kind: fw.KindToken}, {Subject: "spiffe://td/x", Kind: fw.KindWorkload}} {
		if _, err := Begin(ctx, a, p, "tickets", "https://fw.example/cb"); !errors.Is(err, ErrNotPerson) {
			t.Fatalf("Begin for %+v = %v", p, err)
		}
	}
	if _, err := Begin(ctx, a, ada, "docs", "https://fw.example/cb"); !errors.Is(err, ErrUnknownService) {
		t.Fatalf("Begin for an unknown service = %v", err)
	}
	Install(a, nil)
	if Of(a) != nil || a.PersonTokens() != nil {
		t.Fatal("Install(nil) left the connections in place")
	}
}

func TestWithoutConnections(t *testing.T) {
	ctx := context.Background()
	a := newAgent(t, nil)
	if conns, err := List(ctx, a, ada); err != nil || len(conns) != 0 || conns == nil {
		t.Fatalf("List = %v, %v", conns, err)
	}
	if _, err := List(ctx, a, &fw.Principal{Subject: "token", Kind: fw.KindToken}); !errors.Is(err, ErrNotPerson) {
		t.Fatalf("List for the token = %v", err)
	}
	if _, err := Begin(ctx, a, ada, "tickets", "https://fw.example/cb"); !errors.Is(err, ErrUnknownService) {
		t.Fatalf("Begin = %v", err)
	}
	if _, err := Finish(ctx, a, ada, Attempt{Server: "tickets"}, "good"); !errors.Is(err, ErrUnknownService) {
		t.Fatalf("Finish = %v", err)
	}
	if err := Disconnect(ctx, a, ada, "tickets"); !errors.Is(err, ErrUnknownService) {
		t.Fatalf("Disconnect = %v", err)
	}
	if err := Disconnect(ctx, a, nil, "tickets"); !errors.Is(err, ErrNotPerson) {
		t.Fatalf("Disconnect for nobody = %v", err)
	}
}

func TestStart(t *testing.T) {
	ctx := context.Background()
	t.Setenv(EnvStore, "memory")
	cfg := fw.DefaultConfig()
	cfg.Home = t.TempDir()
	if c, err := Start(ctx, newAgent(t, cfg)); c != nil || err != nil {
		t.Fatalf("Start without a personal entry = %v, %v", c, err)
	}
	cfg.Tools.MCP = []fw.MCPServerConfig{{Name: "t", Endpoint: "https://t.example.com/mcp", Auth: fw.MCPAuthConfig{OAuth: &fw.OAuthClientConfig{Grant: "user"}}}}
	cfg.Auth.Mode = "token"
	if _, err := Start(ctx, newAgent(t, cfg)); err == nil || !strings.Contains(err.Error(), "oidc") {
		t.Fatalf("Start without oidc = %v", err)
	}
	cfg.Auth.Mode = "oidc"
	cfg.Auth.OIDC = &fw.OIDCConfig{Issuer: "https://id.example.com"}
	if _, err := Start(ctx, newAgent(t, cfg)); err == nil || !strings.Contains(err.Error(), "auth.oidc.clientId") {
		t.Fatalf("Start without a sign-in client = %v", err)
	}
	cfg.Auth.OIDC.ClientID = "fw"
	a := newAgent(t, cfg)
	closer, err := Start(ctx, a)
	if err != nil || closer == nil {
		t.Fatalf("Start = %v, %v", closer, err)
	}
	c := Of(a)
	if c == nil || len(c.Connectors()) != 1 || c.Connectors()[0].Name() != "t" || a.PersonTokens() != fw.PersonTokens(c) {
		t.Fatalf("connections = %+v", c)
	}
	if err := closer.Close(); err != nil {
		t.Fatal(err)
	}
	if Of(a) != nil || a.PersonTokens() != nil {
		t.Fatal("Close left the connections in place")
	}
	// The service runs with the agent.
	b := newAgent(t, cfg, fw.WithServices(Start))
	if c := Of(b); c == nil || len(c.Connectors()) != 1 {
		t.Fatalf("the service installed %+v", c)
	}
	t.Setenv(EnvStore, "bogus")
	if _, err := Start(ctx, newAgent(t, cfg)); err == nil || !strings.Contains(err.Error(), `workbench.grants.store "bogus"`) {
		t.Fatalf("Start with a bogus store = %v", err)
	}
}

func TestLoad(t *testing.T) {
	for _, e := range []string{EnvStore, EnvDir, EnvKey} {
		t.Setenv(e, "")
	}
	cfg := fw.DefaultConfig()
	cfg.Home = "/srv/fw"
	c, err := Load(cfg)
	if err != nil || c.Store != StoreFile || c.Dir != "/srv/fw/grants" || c.Key != DefaultKey {
		t.Fatalf("Load = %+v, %v", c, err)
	}
	t.Setenv(EnvStore, "Memory")
	t.Setenv(EnvDir, "/var/grants")
	t.Setenv(EnvKey, "MY_KEY")
	c, err = Load(cfg)
	if err != nil || c.Store != StoreMemory || c.Dir != "/var/grants" || c.Key != "MY_KEY" {
		t.Fatalf("Load from the variables = %+v, %v", c, err)
	}
}

// A member of a fleet keeps no grant: with tokens.url set, the service
// starts nothing, and the front alone runs the OAuth flows.
func TestStartKeepsNoGrantsBehindABroker(t *testing.T) {
	cfg := fw.DefaultConfig()
	cfg.Auth.Mode = "token"
	cfg.Tokens.URL = "http://front.example"
	cfg.Tools.MCP = []fw.MCPServerConfig{{Name: "tickets", Endpoint: "http://tickets.example/mcp", Auth: fw.MCPAuthConfig{OAuth: &fw.OAuthClientConfig{Grant: fw.GrantUser}}}}
	a, err := fw.New(fw.WithConfig(cfg), fw.WithoutDefaults(), fw.WithAuthn(fw.NoAuthn{}), fw.WithLedger(fw.NewMemLedger()), fw.WithBus(fw.NewMemBus()))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close(context.Background())
	c, err := Start(context.Background(), a)
	if err != nil || c != nil {
		t.Fatalf("Start behind a broker = %v, %v; want nothing", c, err)
	}
}

// A notice of another front drops the tokens and the pending grants of the
// person in the broker of this front. A notice of this front changes
// nothing, because its own Disconnect cleared its broker already.
func TestListenDropsTheTokensOfAnotherFront(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cfg := fw.DefaultConfig()
	cfg.Node = "front-1:8080"
	a := newAgent(t, cfg)
	c := New(a)
	c.broker = newBroker()
	key := func(subject string) GrantKey {
		return GrantKey{Subject: subject, Issuer: "https://as.example", ClientID: "fw"}
	}
	for _, s := range []string{"ada", "bob"} {
		l := c.broker.lockFor(key(s))
		l.Lock()
		c.broker.keep(tokenKey{grant: key(s), server: "tickets"}, &tokenAnswer{AccessToken: "at-" + s})
		c.broker.mu.Lock()
		c.broker.pending[key(s)] = Grant{}
		c.broker.mu.Unlock()
		l.Unlock()
	}
	has := func(s string) (token, pending bool) {
		_, token = c.broker.cached(tokenKey{grant: key(s), server: "tickets"})
		c.broker.mu.Lock()
		_, pending = c.broker.pending[key(s)]
		c.broker.mu.Unlock()
		return token, pending
	}
	stop, err := c.listen(ctx)
	if err != nil || stop == nil {
		t.Fatalf("listen = %v", err)
	}
	defer stop()
	publish := func(node, subject string) {
		ev := fw.MustEvent("", tokens.EvDisconnected, tokens.Disconnected{Subject: subject, Servers: []string{"tickets"}})
		ev.Node = node
		if err := a.Bus().Publish(ctx, fw.DefaultTokensSubject, ev); err != nil {
			t.Fatal(err)
		}
	}
	// The bus keeps the order, so the notice of this front for bob arrives
	// before the notice of the other front for ada.
	publish("front-1:8080", "bob")
	publish("front-2:8080", "ada")
	deadline := time.Now().Add(5 * time.Second)
	for {
		if tok, _ := has("ada"); !tok || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if tok, pending := has("ada"); tok || pending {
		t.Fatalf("ada after the notice of another front: token %v, pending %v", tok, pending)
	}
	if tok, pending := has("bob"); !tok || !pending {
		t.Fatalf("bob after the notice of this server: token %v, pending %v", tok, pending)
	}
}
