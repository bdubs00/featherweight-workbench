package web

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/surface/native"
	"github.com/bdubs00/featherweight-workbench/connections"
)

// Values that must never reach a log line.
const (
	testCode     = "code-good-1234567890"
	testState    = "state-secret-1234567890"
	testVerifier = "verifier-secret-1234567890"
	testNonce    = "nonce-secret-1234567890"
)

// fakeConnector is a tool source with one tool, tickets.list, that acts
// for the person of the run, and the connections.Connector of that
// service. It connects a person who brings testCode with the verifier of
// the start.
type fakeConnector struct {
	mu        sync.Mutex
	connected map[string]bool
	redirects []string
	finished  []connections.Attempt
	url       string // the authorization request, or "" for the default
}

func newFakeConnector() *fakeConnector { return &fakeConnector{connected: map[string]bool{}} }

func (f *fakeConnector) Name() string { return "tickets" }

func (f *fakeConnector) Connection(_ context.Context, subject string) (connections.Connection, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return connections.Connection{Server: "tickets", Title: "Tickets", Connected: f.connected[subject]}, nil
}

func (f *fakeConnector) Begin(_ context.Context, _, redirect string) (connections.Attempt, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.redirects = append(f.redirects, redirect)
	u := f.url
	if u == "" {
		u = "https://id.example/authorize?state=" + testState + "&redirect_uri=" + url.QueryEscape(redirect)
	}
	return connections.Attempt{Server: "tickets", URL: u, State: testState, Verifier: testVerifier, Nonce: testNonce, Redirect: redirect}, nil
}

func (f *fakeConnector) Finish(ctx context.Context, subject string, at connections.Attempt, code string) (connections.Connection, error) {
	f.mu.Lock()
	f.finished = append(f.finished, at)
	f.mu.Unlock()
	if code != testCode || at.Verifier != testVerifier {
		return connections.Connection{}, errors.New("connections: server tickets: oauth: token endpoint refused: invalid_grant")
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

func (f *fakeConnector) Reject(context.Context, string, string) {}

func (f *fakeConnector) Forget(ctx context.Context, subject string) { _ = f.Disconnect(ctx, subject) }

func (f *fakeConnector) lastRedirect() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.redirects) == 0 {
		return ""
	}
	return f.redirects[len(f.redirects)-1]
}

func (f *fakeConnector) Tools(context.Context) ([]fw.Tool, error) {
	return []fw.Tool{fw.NewTool(fw.ToolSpec{Name: "tickets.list", Source: "tickets"}, func(ctx context.Context, _ json.RawMessage) (fw.ToolResult, error) {
		info := fw.RunInfoFrom(ctx)
		if _, err := f.Token(ctx, info.Principal); err != nil {
			if err := fw.RequestConnect(ctx, fw.ConnectRequest{Server: "tickets", Title: "Tickets", Reason: fw.ConnectMissing}); err != nil {
				return fw.ToolResult{}, err
			}
		}
		return fw.TextResult("tickets of " + info.Principal), nil
	})}, nil
}

// connectPeople authenticates a person, the static token, a workload, and
// an anonymous caller.
var connectPeople = tokenMap{
	"alice-token":    {Subject: "alice", Name: "alice@example.com", Kind: fw.KindUser},
	"static-token":   {Subject: "token", Kind: fw.KindToken},
	"workload-token": {Subject: "spiffe://td/ns/x", Kind: fw.KindWorkload},
	"anon-token":     {Subject: "anonymous", Kind: fw.KindAnonymous},
}

// connectEnv is an agent with a service that acts for a person.
type connectEnv struct {
	a     *fw.Agent
	fc    *fakeConnector
	url   string
	clock *testClock
	log   *lockedBuffer
}

func newConnectEnv(t *testing.T, m fw.Model, change func(*fw.Config)) *connectEnv {
	t.Helper()
	t.Setenv("FW_ATESPACE", "")
	t.Setenv(sharedKeySecret, "")
	t.Setenv(sharedKeySecret+"_FILE", "")
	e := &connectEnv{fc: newFakeConnector(), clock: &testClock{t: time.Now()}, log: &lockedBuffer{}}
	cfg := fw.DefaultConfig()
	cfg.Home = t.TempDir()
	if change != nil {
		change(cfg)
	}
	a, err := fw.New(
		fw.WithoutDefaults(),
		fw.WithConfig(cfg),
		fw.WithAuthn(connectPeople),
		fw.WithModel(m),
		fw.WithLedger(fw.NewMemLedger()),
		fw.WithBus(fw.NewMemBus()),
		fw.WithToolSources(e.fc),
		fw.WithLogger(slog.New(slog.NewTextHandler(e.log, &slog.HandlerOptions{Level: slog.LevelDebug}))),
		fw.WithSurfaces(native.NewWith(newExtension(e.clock.now))),
	)
	if err != nil {
		t.Fatal(err)
	}
	connections.Install(a, connections.New(a, e.fc))
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		a.Close(ctx)
	})
	h, err := a.Handler()
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	e.a, e.url = a, srv.URL
	return e
}

// start posts a connection of tickets as alice and returns the URL of the
// answer and the connect cookie.
func (e *connectEnv) start(t *testing.T, body string, header ...string) (string, *http.Cookie) {
	t.Helper()
	resp, b := do(t, "POST", e.url+"/v1/connections/tickets", body, append([]string{"Authorization", "Bearer alice-token"}, header...)...)
	var ans struct{ URL string }
	decodeJSON(t, resp, b, http.StatusOK, &ans)
	if resp.Header.Get("Cache-Control") != "no-store" {
		t.Errorf("start headers = %v", resp.Header)
	}
	var cookie *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == connectCookie {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatalf("start: no cookie in %v", resp.Header["Set-Cookie"])
	}
	if !cookie.HttpOnly || cookie.Path != "/ui/connect" || cookie.SameSite != http.SameSiteLaxMode || cookie.MaxAge != 600 {
		t.Errorf("cookie = %+v", cookie)
	}
	// The cookie is signed, not hidden, but it never carries the URL.
	if raw, _ := url.QueryUnescape(cookie.Value); strings.Contains(raw, "authorize") {
		t.Errorf("cookie holds the URL")
	}
	return ans.URL, cookie
}

// callback opens the callback with a query and a cookie. It checks the
// headers of every answer: no cache, no referrer, and a deleted cookie.
func (e *connectEnv) callback(t *testing.T, query url.Values, cookie *http.Cookie, header ...string) (*http.Response, string) {
	t.Helper()
	resp, body := get(t, e.url+"/ui/connect/callback?"+query.Encode(), cookie, header...)
	if resp.Header.Get("Cache-Control") != "no-store" || resp.Header.Get("Referrer-Policy") != "no-referrer" {
		t.Errorf("callback headers = %v", resp.Header)
	}
	cleared := false
	for _, c := range resp.Cookies() {
		if c.Name == connectCookie && c.MaxAge < 0 && c.Value == "" && c.Path == "/ui/connect" {
			cleared = true
		}
	}
	if !cleared {
		t.Errorf("callback keeps the cookie: %v", resp.Header["Set-Cookie"])
	}
	return resp, body
}

// noSecrets fails when the log holds a code, a state, a verifier, a nonce,
// or the refresh token.
func (e *connectEnv) noSecrets(t *testing.T) {
	t.Helper()
	log := e.log.String()
	for _, v := range []string{testCode, testState, testVerifier, testNonce, "rt-secret"} {
		if strings.Contains(log, v) {
			t.Errorf("the log holds %q", v)
		}
	}
}

func goodQuery() url.Values { return url.Values{"code": {testCode}, "state": {testState}} }

func TestConnectionsNeedAPerson(t *testing.T) {
	e := newConnectEnv(t, newScript().model(), nil)
	for _, tok := range []string{"static-token", "workload-token", "anon-token"} {
		auth := "Bearer " + tok
		for _, req := range []struct{ method, path, body string }{
			{"GET", "/v1/connections", ""},
			{"POST", "/v1/connections/tickets", `{}`},
			{"DELETE", "/v1/connections/tickets", ""},
		} {
			resp, b := do(t, req.method, e.url+req.path, req.body, "Authorization", auth)
			p := decodeProblem(t, resp, b, http.StatusForbidden)
			if p.Detail != connectNotPerson {
				t.Errorf("%s %s as %s: detail %q", req.method, req.path, tok, p.Detail)
			}
		}
	}
	resp, b := do(t, "GET", e.url+"/v1/connections", "")
	decodeProblem(t, resp, b, http.StatusUnauthorized)

	resp, b = do(t, "GET", e.url+"/v1/connections", "", "Authorization", "Bearer alice-token")
	var list struct{ Connections []connections.Connection }
	decodeJSON(t, resp, b, http.StatusOK, &list)
	if len(list.Connections) != 1 || list.Connections[0].Server != "tickets" || list.Connections[0].Connected {
		t.Fatalf("connections = %s", b)
	}
	resp, b = do(t, "POST", e.url+"/v1/connections/docs", "", "Authorization", "Bearer alice-token")
	decodeProblem(t, resp, b, http.StatusNotFound)
}

func TestConnectResumesThePausedRun(t *testing.T) {
	ctx := testCtx(t)
	s := newScript(callTool("c1", "tickets.list", `{}`), endTurn("done"))
	e := newConnectEnv(t, s.model(), nil)
	alice := []string{"Authorization", "Bearer alice-token"}

	resp, b := do(t, "POST", e.url+"/v1/runs", `{"input":"list my tickets","wait":true}`, alice...)
	r := decodeRun(t, resp, b, http.StatusOK)
	if r.State != fw.StateInputRequired || r.Pending == nil || r.Pending.Connect == nil || r.Pending.Connect.Server != "tickets" {
		t.Fatalf("run = %+v", r)
	}
	// A click is not a grant.
	resp, b = do(t, "POST", e.url+"/v1/runs/"+r.ID+"/approvals/"+r.Pending.ID, `{"decision":"allow"}`, alice...)
	if p := decodeProblem(t, resp, b, http.StatusBadRequest); !strings.Contains(p.Detail, "connect Tickets to continue") {
		t.Fatalf("allow a connect: %q", p.Detail)
	}

	target, cookie := e.start(t, `{"next":"#run/`+r.ID+`"}`)
	if cookie.Secure {
		t.Error("the cookie of an http request is Secure")
	}
	if !strings.HasPrefix(target, "https://id.example/authorize?") {
		t.Fatalf("url = %q", target)
	}
	if got := e.fc.lastRedirect(); got != e.url+"/ui/connect/callback" {
		t.Fatalf("redirect = %q", got)
	}
	resp, body := e.callback(t, goodQuery(), cookie)
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/ui#run/"+r.ID {
		t.Fatalf("callback: %d %q %s", resp.StatusCode, resp.Header.Get("Location"), body)
	}
	r, err := e.a.Wait(ctx, r.ID)
	if err != nil || r.State != fw.StateCompleted || r.Output != "done" {
		t.Fatalf("run = %+v, %v", r, err)
	}
	reqs := s.requests()
	last := reqs[len(reqs)-1].Messages
	if got := fw.TextOf(last[len(last)-1].Content[0].Content); got != "tickets of alice" {
		t.Fatalf("tool result = %q", got)
	}
	// The cookie carried the whole start back to the service.
	if f := e.fc.finished[0]; f.Verifier != testVerifier || f.Nonce != testNonce || f.Server != "tickets" || f.Redirect != e.url+"/ui/connect/callback" {
		t.Fatalf("finished with %+v", f)
	}

	resp, b = do(t, "GET", e.url+"/v1/connections", "", alice...)
	var list struct{ Connections []connections.Connection }
	decodeJSON(t, resp, b, http.StatusOK, &list)
	if !list.Connections[0].Connected {
		t.Fatalf("connections = %s", b)
	}
	resp, b = do(t, "DELETE", e.url+"/v1/connections/tickets", "", alice...)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete: %d %s", resp.StatusCode, b)
	}
	resp, b = do(t, "GET", e.url+"/v1/connections", "", alice...)
	decodeJSON(t, resp, b, http.StatusOK, &list)
	if list.Connections[0].Connected {
		t.Fatalf("connected after delete: %s", b)
	}
	e.noSecrets(t)
}

func TestConnectWithoutNext(t *testing.T) {
	e := newConnectEnv(t, newScript().model(), nil)
	// No body, and a next that is not a fragment, both return to the
	// result of the connection.
	for _, body := range []string{"", `{"next":"javascript:alert(1)"}`} {
		_, cookie := e.start(t, body)
		resp, b := e.callback(t, goodQuery(), cookie)
		if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/ui#connected/tickets" {
			t.Fatalf("body %q: %d %q %s", body, resp.StatusCode, resp.Header.Get("Location"), b)
		}
	}
	resp, b := do(t, "POST", e.url+"/v1/connections/tickets", `{"next":"#x","extra":1}`, "Authorization", "Bearer alice-token")
	decodeProblem(t, resp, b, http.StatusBadRequest)
}

func TestConnectCallbackFailures(t *testing.T) {
	e := newConnectEnv(t, newScript().model(), nil)
	check := func(name string, resp *http.Response, body string, status int, sentence string) {
		t.Helper()
		if resp.StatusCode != status || !strings.Contains(body, sentence) || !strings.Contains(body, `href="/ui#connections"`) {
			t.Errorf("%s: %d %s", name, resp.StatusCode, body)
		}
		if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html") || !strings.Contains(resp.Header.Get("Content-Security-Policy"), "default-src 'none'") {
			t.Errorf("%s: headers %v", name, resp.Header)
		}
	}
	resp, body := e.callback(t, goodQuery(), nil)
	check("no cookie", resp, body, http.StatusBadRequest, "no connection in progress")

	_, cookie := e.start(t, "")
	forged := *cookie
	forged.Value = cookie.Value[:len(cookie.Value)-2] + "xx"
	resp, body = e.callback(t, goodQuery(), &forged)
	check("forged", resp, body, http.StatusBadRequest, "not valid")

	// A cookie that another process signed with its own key.
	other := forged
	st := connectState{Start: connections.Attempt{Server: "tickets", State: testState, Verifier: testVerifier, Redirect: e.url + "/ui/connect/callback"},
		Subject: "alice", Kind: fw.KindUser, Expires: time.Now().Add(time.Minute).Unix()}
	other.Value = sealer{key: randomKey(), domain: connectDomain}.seal(st)
	resp, body = e.callback(t, goodQuery(), &other)
	check("another key", resp, body, http.StatusBadRequest, "not valid")

	resp, body = e.callback(t, url.Values{"code": {testCode}, "state": {"other"}}, cookie)
	check("state", resp, body, http.StatusBadRequest, "does not belong to this connection")

	resp, body = e.callback(t, goodQuery(), cookie, "X-Forwarded-Host", "elsewhere.example")
	check("another host", resp, body, http.StatusBadRequest, "started on another address")

	resp, body = e.callback(t, url.Values{"error": {"access_denied"}, "state": {testState}}, cookie)
	check("denied", resp, body, http.StatusForbidden, "was not allowed")

	resp, body = e.callback(t, url.Values{"error": {"<b>bad</b>"}, "state": {testState}}, cookie)
	check("odd error", resp, body, http.StatusForbidden, "with an error")

	resp, body = e.callback(t, url.Values{"state": {testState}}, cookie)
	check("no code", resp, body, http.StatusBadRequest, "no authorization code")

	resp, body = e.callback(t, url.Values{"code": {"wrong-code-value"}, "state": {testState}}, cookie)
	check("refused code", resp, body, http.StatusBadGateway, "Connecting tickets failed: Server tickets: oauth: token endpoint refused: invalid_grant.")

	e.clock.add(connectTTL + time.Second)
	resp, body = e.callback(t, goodQuery(), cookie)
	check("expired", resp, body, http.StatusBadRequest, "longer than 10 minutes")

	if conns, _ := connections.List(context.Background(), e.a, connectPeople["alice-token"]); conns[0].Connected {
		t.Fatal("a failed callback connected")
	}
	e.noSecrets(t)
	if strings.Contains(e.log.String(), "wrong-code-value") {
		t.Error("the log holds a code")
	}
}

func TestConnectRedirectURI(t *testing.T) {
	t.Run("from the sign-in redirect", func(t *testing.T) {
		e := newConnectEnv(t, newScript().model(), func(c *fw.Config) {
			c.Auth.OIDC = &fw.OIDCConfig{Issuer: "https://id.example", RedirectURL: "https://fw.example/base/ui/callback"}
		})
		target, cookie := e.start(t, "")
		if got := e.fc.lastRedirect(); got != "https://fw.example/base/ui/connect/callback" || !strings.Contains(target, url.QueryEscape(got)) {
			t.Fatalf("redirect = %q, url %q", got, target)
		}
		// The address is fixed, so the callback trusts the cookie of the
		// browser and does not compare hosts.
		resp, body := e.callback(t, goodQuery(), cookie)
		if resp.StatusCode != http.StatusSeeOther {
			t.Fatalf("callback: %d %s", resp.StatusCode, body)
		}
	})
	t.Run("from the request", func(t *testing.T) {
		e := newConnectEnv(t, newScript().model(), nil)
		_, cookie := e.start(t, "", "X-Forwarded-Proto", "https", "X-Forwarded-Host", "fw.example")
		if got := e.fc.lastRedirect(); got != "https://fw.example/ui/connect/callback" {
			t.Fatalf("redirect = %q", got)
		}
		if !cookie.Secure {
			t.Error("the cookie of an https request is not Secure")
		}
		resp, body := e.callback(t, goodQuery(), cookie, "X-Forwarded-Proto", "https", "X-Forwarded-Host", "fw.example")
		if resp.StatusCode != http.StatusSeeOther {
			t.Fatalf("callback: %d %s", resp.StatusCode, body)
		}
	})
}

func TestConnectRefusesABadAuthorizationURL(t *testing.T) {
	e := newConnectEnv(t, newScript().model(), nil)
	e.fc.url = "javascript:alert(1)"
	resp, b := do(t, "POST", e.url+"/v1/connections/tickets", "", "Authorization", "Bearer alice-token")
	decodeProblem(t, resp, b, http.StatusBadGateway)
	for _, c := range resp.Cookies() {
		if c.Name == connectCookie {
			t.Fatal("a refused start set the cookie")
		}
	}
}

func TestSealDomains(t *testing.T) {
	key := randomKey()
	st := signinState{Verifier: "v", State: "s", Redirect: "r", Expires: time.Now().Add(time.Minute).Unix()}
	signed := sealer{key: key, domain: signinDomain}.seal(st)
	var out connectState
	if err := (sealer{key: key, domain: connectDomain}).open(signed, &out); err == nil {
		t.Fatal("a sign-in cookie opened as a connect cookie")
	}
	var back signinState
	if err := (sealer{key: key, domain: signinDomain}).open(signed, &back); err != nil || back != st {
		t.Fatalf("open = %+v, %v", back, err)
	}
}

func TestConnectCause(t *testing.T) {
	for in, want := range map[string]string{
		"fw: no grant":               "No grant.",
		"mcp: signed in as another.": "Signed in as another.",
		"":                           "the service did not say why.",
	} {
		if got := connectCause(errors.New(in)); got != want {
			t.Errorf("connectCause(%q) = %q", in, got)
		}
	}
}
