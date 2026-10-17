package connections

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
)

type mapSecrets map[string]string

func (m mapSecrets) Get(_ context.Context, name string) (string, error) {
	v, ok := m[name]
	if !ok {
		return "", errors.New("no secret " + name)
	}
	return v, nil
}

func testSecrets() fw.Secrets {
	return mapSecrets{"OIDC_SECRET": "oidc-secret", "OWN_SECRET": "oidc-secret"}
}

// resourceServer serves the RFC 9728 metadata of an MCP endpoint at /mcp
// that names issuer as its authorization server.
func resourceServer(t *testing.T, issuer string) *httptest.Server {
	t.Helper()
	var srv *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/oauth-protected-resource/mcp", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"resource":"` + srv.URL + `/mcp","authorization_servers":["` + issuer + `"]}`))
	})
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// newPersonConnector builds the connector of an entry with grant user,
// with the fake IdP as the sign-in issuer. Without a clientId in o, the
// entry uses the sign-in client.
func newPersonConnector(t *testing.T, name, endpoint string, idp *fakeIdP, o fw.OAuthClientConfig, b *broker, store Store) *connector {
	t.Helper()
	o.Grant = fw.GrantUser
	k, err := newConnector(fw.MCPServerConfig{Name: name, Title: strings.ToUpper(name[:1]) + name[1:], Endpoint: endpoint, Auth: fw.MCPAuthConfig{OAuth: &o}},
		&fw.OIDCConfig{Issuer: idp.URL, ClientID: "fw", ClientSecret: "OIDC_SECRET"}, b, store, testSecrets(), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// browse connects the person as signedInAs, as the callback of the page
// does.
func browse(t *testing.T, k *connector, idp *fakeIdP, subject, signedInAs string) error {
	t.Helper()
	ctx := context.Background()
	at, err := k.Begin(ctx, subject, "https://fw.example/ui/connect/callback")
	if err != nil {
		return err
	}
	_, err = k.Finish(ctx, subject, at, idp.authorize(t, at.URL, signedInAs))
	return err
}

const ticketsEndpoint = "https://tickets.example.com/mcp"

func TestConnectThenToken(t *testing.T) {
	ctx := context.Background()
	idp := newFakeIdP(t)
	k := newPersonConnector(t, "tickets", ticketsEndpoint, idp, fw.OAuthClientConfig{}, newBroker(), NewMemStore())
	c, err := k.Connection(ctx, "ada")
	if err != nil || c.Connected || !c.SignIn || c.Issuer != idp.URL || c.Title != "Tickets" {
		t.Fatalf("Connection before = %+v, %v", c, err)
	}
	if _, err := k.Token(ctx, "ada"); !errors.Is(err, fw.ErrNotConnected) {
		t.Fatalf("Token before = %v", err)
	}
	if err := browse(t, k, idp, "ada", "ada"); err != nil {
		t.Fatal(err)
	}
	c, _ = k.Connection(ctx, "ada")
	if !c.Connected || c.Account != "ada@example.com" || c.Since.IsZero() {
		t.Fatalf("Connection after = %+v", c)
	}
	tok, err := k.Token(ctx, "ada")
	if ai, ok := idp.lookup(tok); err != nil || !ok || ai.subject != "ada" || ai.aud != ticketsEndpoint {
		t.Fatalf("Token = %v, %v (%+v)", ok, err, ai)
	}
	// The second token comes from the cache: no refresh.
	refreshes, _, _ := idp.counts()
	if tok2, _ := k.Token(ctx, "ada"); tok2 != tok {
		t.Fatalf("second token differs")
	}
	if r2, _, _ := idp.counts(); r2 != refreshes {
		t.Fatalf("refreshes %d -> %d", refreshes, r2)
	}
	// Each refresh carried the resource of the MCP server.
	idp.mu.Lock()
	defer idp.mu.Unlock()
	for _, f := range idp.forms {
		if f.Get("grant_type") == grantRefresh && f.Get("resource") != ticketsEndpoint {
			t.Fatalf("refresh without the resource: %v", f)
		}
	}
}

func TestAuthorizationRequest(t *testing.T) {
	idp := newFakeIdP(t)
	b := newBroker()
	store := NewMemStore()
	k := newPersonConnector(t, "tickets", ticketsEndpoint, idp, fw.OAuthClientConfig{Scopes: []string{"tickets.read"}, AuthParams: map[string]string{"prompt": "consent", "state": "evil"}}, b, store)
	newPersonConnector(t, "docs", "https://docs.example.com/mcp", idp, fw.OAuthClientConfig{}, b, store)
	at, err := k.Begin(context.Background(), "ada", "https://fw.example/ui/connect/callback")
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(at.URL)
	q := u.Query()
	if q.Get("scope") != "tickets.read openid offline_access" || q.Get("prompt") != "consent" || q.Get("state") != at.State ||
		q.Get("code_challenge") != pkceChallenge(at.Verifier) || q.Get("nonce") != at.Nonce || q.Get("redirect_uri") != at.Redirect {
		t.Fatalf("authorization request = %v", q)
	}
	// One connection of the sign-in client covers both entries.
	if res := q["resource"]; len(res) != 2 {
		t.Fatalf("resources = %v", res)
	}
	if at.Server != "tickets" || time.Until(at.Expires) < 9*time.Minute {
		t.Fatalf("attempt = %+v", at)
	}
	if _, err := k.Begin(context.Background(), "", "https://fw.example/cb"); !errors.Is(err, ErrNotPerson) {
		t.Fatalf("Begin without a subject = %v", err)
	}
	if _, err := k.Begin(context.Background(), "ada", ""); err == nil {
		t.Fatal("Begin without a redirect address")
	}
}

func TestConcurrentTokensRefreshOnce(t *testing.T) {
	ctx := context.Background()
	idp := newFakeIdP(t)
	b := newBroker()
	store := NewMemStore()
	k := newPersonConnector(t, "tickets", ticketsEndpoint, idp, fw.OAuthClientConfig{}, b, store)
	if err := browse(t, k, idp, "ada", "ada"); err != nil {
		t.Fatal(err)
	}
	// Forget the access tokens, so every call needs a refresh.
	b.mu.Lock()
	clear(b.tokens)
	b.mu.Unlock()
	before, _, _ := idp.counts()
	idp.refreshDelay = 50 * time.Millisecond
	var wg sync.WaitGroup
	var bad atomic.Int32
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tok, err := k.Token(ctx, "ada")
			if ai, ok := idp.lookup(tok); err != nil || !ok || ai.subject != "ada" {
				bad.Add(1)
			}
		}()
	}
	wg.Wait()
	after, _, _ := idp.counts()
	if bad.Load() != 0 || after-before != 1 {
		t.Fatalf("bad tokens %d, refreshes %d, want 1", bad.Load(), after-before)
	}
	// The store holds the rotated refresh token, the one the server knows.
	gs, _ := store.List(ctx, "ada")
	idp.mu.Lock()
	defer idp.mu.Unlock()
	if len(gs) != 1 || idp.families[idp.rts[gs[0].RefreshToken]].current != gs[0].RefreshToken || gs[0].Refreshed.IsZero() {
		t.Fatalf("stored grant is not the current one")
	}
}

// flakyStore loses the next Put, as a process that stops between the
// answer of the server and the write does.
type flakyStore struct {
	*MemStore
	lose atomic.Bool
}

func (f *flakyStore) Put(ctx context.Context, g Grant, prev uint64) (uint64, error) {
	if f.lose.Swap(false) {
		return 0, errors.New("disk full")
	}
	return f.MemStore.Put(ctx, g, prev)
}

func TestLostRefreshExpiresTheGrant(t *testing.T) {
	ctx := context.Background()
	idp := newFakeIdP(t)
	store := &flakyStore{MemStore: NewMemStore()}
	b := newBroker()
	k := newPersonConnector(t, "tickets", ticketsEndpoint, idp, fw.OAuthClientConfig{}, b, store)
	if err := browse(t, k, idp, "ada", "ada"); err != nil {
		t.Fatal(err)
	}
	// The refresh succeeds at the server, but the new refresh token never
	// reaches the store.
	b.mu.Lock()
	clear(b.tokens)
	b.mu.Unlock()
	store.lose.Store(true)
	if _, err := k.Token(ctx, "ada"); err == nil || !strings.Contains(err.Error(), "store the refreshed grant") {
		t.Fatalf("Token with a lost write = %v", err)
	}
	// A new process starts with the old refresh token on disk. The server
	// sees it again and revokes the grant, so the person connects again.
	k2 := newPersonConnector(t, "tickets", ticketsEndpoint, idp, fw.OAuthClientConfig{}, newBroker(), store)
	if _, err := k2.Token(ctx, "ada"); !errors.Is(err, fw.ErrConnectionExpired) {
		t.Fatalf("Token after the restart = %v", err)
	}
	if _, err := k2.Token(ctx, "ada"); !errors.Is(err, fw.ErrNotConnected) {
		t.Fatalf("Token after the refusal = %v", err)
	}
	if gs, _ := store.List(ctx, "ada"); len(gs) != 0 {
		t.Fatalf("a refused grant stayed: %v", gs)
	}
	if err := browse(t, k2, idp, "ada", "ada"); err != nil {
		t.Fatal(err)
	}
	if tok, err := k2.Token(ctx, "ada"); err != nil || tok == "" {
		t.Fatalf("Token after a new connection = %q, %v", tok, err)
	}
}

func TestExchangeMode(t *testing.T) {
	ctx := context.Background()
	idp := newFakeIdP(t)
	b := newBroker()
	store := NewMemStore()
	kt := newPersonConnector(t, "tickets", ticketsEndpoint, idp, fw.OAuthClientConfig{Exchange: &fw.TokenExchangeConfig{Audience: "tickets-mcp"}}, b, store)
	kd := newPersonConnector(t, "docs", "https://docs.example.com/mcp", idp, fw.OAuthClientConfig{Exchange: &fw.TokenExchangeConfig{Audience: "docs-mcp"}}, b, store)
	if err := browse(t, kt, idp, "ada", "ada"); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		k   *connector
		aud string
	}{{kt, "tickets-mcp"}, {kd, "docs-mcp"}} {
		tok, err := c.k.Token(ctx, "ada")
		if ai, ok := idp.lookup(tok); err != nil || !ok || ai.subject != "ada" || ai.aud != c.aud {
			t.Fatalf("%s token = %+v, %v, %v", c.k.Name(), ai, ok, err)
		}
	}
	// One connection serves both, the code gave the base token, and each
	// entry exchanged it once.
	refreshes, exchanges, _ := idp.counts()
	if refreshes != 0 || exchanges != 2 {
		t.Fatalf("refreshes %d, exchanges %d", refreshes, exchanges)
	}
	idp.mu.Lock()
	defer idp.mu.Unlock()
	for _, f := range idp.forms {
		if f.Get("grant_type") == grantTokenExchange && f.Get("resource") != "" {
			t.Fatalf("an exchange with an audience sent resource: %v", f)
		}
	}
}

func TestFinishChecks(t *testing.T) {
	ctx := context.Background()
	idp := newFakeIdP(t)
	store := NewMemStore()
	k := newPersonConnector(t, "tickets", ticketsEndpoint, idp, fw.OAuthClientConfig{}, newBroker(), store)
	if err := browse(t, k, idp, "ada", "mallory"); err == nil || !strings.Contains(err.Error(), "another account") {
		t.Fatalf("Finish as another person = %v", err)
	}
	if gs, _ := store.List(ctx, "ada"); len(gs) != 0 {
		t.Fatalf("grants = %v", gs)
	}
	at, _ := k.Begin(ctx, "ada", "https://fw.example/cb")
	at.Expires = time.Now().Add(-time.Second)
	if _, err := k.Finish(ctx, "ada", at, idp.authorize(t, at.URL, "ada")); err == nil || !strings.Contains(err.Error(), "longer than") {
		t.Fatalf("Finish after the expiry = %v", err)
	}
	at, _ = k.Begin(ctx, "ada", "https://fw.example/cb")
	at.Verifier = "wrong"
	if _, err := k.Finish(ctx, "ada", at, idp.authorize(t, at.URL, "ada")); err == nil || !strings.Contains(err.Error(), "invalid_grant") {
		t.Fatalf("Finish with a wrong verifier = %v", err)
	}
	at, _ = k.Begin(ctx, "ada", "https://fw.example/cb")
	at.Server = "docs"
	if _, err := k.Finish(ctx, "ada", at, "code"); err == nil || !strings.Contains(err.Error(), "belongs to docs") {
		t.Fatalf("Finish for another server = %v", err)
	}
	if _, err := k.Finish(ctx, "", at, "code"); !errors.Is(err, ErrNotPerson) {
		t.Fatalf("Finish without a subject = %v", err)
	}
	idp.noRefreshToken = true
	if err := browse(t, k, idp, "ada", "ada"); err == nil || !strings.Contains(err.Error(), "no refresh token") {
		t.Fatalf("Finish without a refresh token = %v", err)
	}
}

func TestDisconnectRevokes(t *testing.T) {
	ctx := context.Background()
	idp := newFakeIdP(t)
	store := NewMemStore()
	k := newPersonConnector(t, "tickets", ticketsEndpoint, idp, fw.OAuthClientConfig{}, newBroker(), store)
	if err := browse(t, k, idp, "ada", "ada"); err != nil {
		t.Fatal(err)
	}
	gs, _ := store.List(ctx, "ada")
	if len(gs) != 1 {
		t.Fatalf("grants = %v", gs)
	}
	rt := gs[0].RefreshToken
	if err := k.Disconnect(ctx, "ada"); err != nil {
		t.Fatal(err)
	}
	if gs, _ := store.List(ctx, "ada"); len(gs) != 0 {
		t.Fatalf("grants after Disconnect = %v", gs)
	}
	if _, err := k.Token(ctx, "ada"); !errors.Is(err, fw.ErrNotConnected) {
		t.Fatalf("Token after Disconnect = %v", err)
	}
	_, _, revocations := idp.counts()
	idp.mu.Lock()
	revoked := idp.families[idp.rts[rt]].revoked
	idp.mu.Unlock()
	if revocations != 1 || !revoked {
		t.Fatalf("revocations %d, revoked %v", revocations, revoked)
	}
}

func TestRejectAndForget(t *testing.T) {
	ctx := context.Background()
	idp := newFakeIdP(t)
	store := NewMemStore()
	k := newPersonConnector(t, "tickets", ticketsEndpoint, idp, fw.OAuthClientConfig{}, newBroker(), store)
	if err := browse(t, k, idp, "ada", "ada"); err != nil {
		t.Fatal(err)
	}
	tok, _ := k.Token(ctx, "ada")
	before, _, _ := idp.counts()
	// A rejected token goes, and the next token comes from a refresh.
	k.Reject(ctx, "ada", tok)
	tok2, err := k.Token(ctx, "ada")
	after, _, _ := idp.counts()
	if err != nil || tok2 == tok || after != before+1 {
		t.Fatalf("Token after Reject = %q (was %q), %v, refreshes %d -> %d", tok2, tok, err, before, after)
	}
	// A forgotten grant is gone from the store.
	k.Forget(ctx, "ada")
	if gs, _ := store.List(ctx, "ada"); len(gs) != 0 {
		t.Fatalf("grants after Forget = %v", gs)
	}
	if _, err := k.Token(ctx, "ada"); !errors.Is(err, fw.ErrNotConnected) {
		t.Fatalf("Token after Forget = %v", err)
	}
}

func TestInvalidClientKeepsTheGrant(t *testing.T) {
	ctx := context.Background()
	idp := newFakeIdP(t)
	store := NewMemStore()
	b := newBroker()
	k := newPersonConnector(t, "tickets", ticketsEndpoint, idp, fw.OAuthClientConfig{}, b, store)
	if err := browse(t, k, idp, "ada", "ada"); err != nil {
		t.Fatal(err)
	}
	b.mu.Lock()
	clear(b.tokens)
	b.mu.Unlock()
	idp.secret = "rotated"
	_, err := k.Token(ctx, "ada")
	if err == nil || !strings.Contains(err.Error(), "invalid_client") || errors.Is(err, fw.ErrNotConnected) || errors.Is(err, fw.ErrConnectionExpired) {
		t.Fatalf("Token = %v", err)
	}
	if gs, _ := store.List(ctx, "ada"); len(gs) != 1 {
		t.Fatal("a refused client deleted the grant of the person")
	}
}

func TestOwnClientDiscovered(t *testing.T) {
	ctx := context.Background()
	signIn := newFakeIdP(t)
	other := newFakeIdP(t)
	other.clientID, other.secret = "tickets-client", "oidc-secret"
	rs := resourceServer(t, other.URL)
	k := newPersonConnector(t, "tickets", rs.URL+"/mcp", signIn, fw.OAuthClientConfig{ClientID: "tickets-client", ClientSecret: "OWN_SECRET"}, newBroker(), NewMemStore())
	if err := browse(t, k, other, "ada", "ada-at-tickets"); err != nil {
		t.Fatal(err)
	}
	tok, err := k.Token(ctx, "ada")
	if ai, ok := other.lookup(tok); err != nil || !ok || ai.subject != "ada-at-tickets" {
		t.Fatalf("Token = %+v, %v, %v", ai, ok, err)
	}
	c, _ := k.Connection(ctx, "ada")
	if !c.Connected || c.SignIn || c.Issuer != other.URL {
		t.Fatalf("Connection = %+v", c)
	}
	at, _ := k.Begin(ctx, "ada", "https://fw.example/cb")
	if q, _ := url.Parse(at.URL); strings.Contains(q.Query().Get("scope"), "openid") || at.Nonce != "" {
		t.Fatalf("a foreign server got openid: %s", at.URL)
	}
}

func TestOpenChecksTheClient(t *testing.T) {
	entry := func(o fw.OAuthClientConfig) fw.MCPServerConfig {
		o.Grant = fw.GrantUser
		return fw.MCPServerConfig{Name: "t", Endpoint: ticketsEndpoint, Auth: fw.MCPAuthConfig{OAuth: &o}}
	}
	log := slog.New(slog.DiscardHandler)
	if _, err := Open(nil, NewMemStore(), []fw.MCPServerConfig{entry(fw.OAuthClientConfig{})}, &fw.OIDCConfig{Issuer: "https://id.example"}, nil, log); err == nil || !strings.Contains(err.Error(), "auth.oidc.clientId") {
		t.Fatalf("no sign-in client = %v", err)
	}
	if _, err := Open(nil, NewMemStore(), []fw.MCPServerConfig{entry(fw.OAuthClientConfig{})}, nil, nil, log); err == nil || !strings.Contains(err.Error(), "auth.oidc.clientId") {
		t.Fatalf("no sign-in = %v", err)
	}
	// An entry of its own needs no client secret: a public client is fine.
	c, err := Open(nil, NewMemStore(), []fw.MCPServerConfig{entry(fw.OAuthClientConfig{ClientID: "pub", Issuer: "https://other.example.com"})}, nil, nil, log)
	if err != nil || len(c.Connectors()) != 1 || c.Connectors()[0].Name() != "t" {
		t.Fatalf("public client = %v, %v", c, err)
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

func TestNoTokenInLogsOrErrors(t *testing.T) {
	ctx := context.Background()
	idp := newFakeIdP(t)
	store := &flakyStore{MemStore: NewMemStore()}
	var logs strings.Builder
	var mu sync.Mutex
	log := slog.New(slog.NewTextHandler(writerFunc(func(p []byte) (int, error) {
		mu.Lock()
		defer mu.Unlock()
		return logs.Write(p)
	}), &slog.HandlerOptions{Level: slog.LevelDebug}))
	b := newBroker()
	o := fw.OAuthClientConfig{Grant: fw.GrantUser}
	k, err := newConnector(fw.MCPServerConfig{Name: "tickets", Endpoint: ticketsEndpoint, Auth: fw.MCPAuthConfig{OAuth: &o}},
		&fw.OIDCConfig{Issuer: idp.URL, ClientID: "fw", ClientSecret: "OIDC_SECRET"}, b, store, testSecrets(), log)
	if err != nil {
		t.Fatal(err)
	}
	var texts []string
	if err := browse(t, k, idp, "ada", "ada"); err != nil {
		t.Fatal(err)
	}
	b.mu.Lock()
	clear(b.tokens)
	b.mu.Unlock()
	store.lose.Store(true)
	if _, err := k.Token(ctx, "ada"); err != nil {
		texts = append(texts, err.Error())
	}
	k2 := newPersonConnector(t, "tickets", ticketsEndpoint, idp, fw.OAuthClientConfig{}, newBroker(), store)
	if _, err := k2.Token(ctx, "ada"); err != nil {
		texts = append(texts, err.Error())
	}
	_ = k.Disconnect(ctx, "ada")
	mu.Lock()
	texts = append(texts, logs.String())
	mu.Unlock()
	for _, text := range texts {
		for _, secret := range []string{"rt-", "at-", "oidc-secret", "code-"} {
			if strings.Contains(text, secret) {
				t.Fatalf("%q shows %q", text, secret)
			}
		}
	}
}
