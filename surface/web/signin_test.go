package web

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/surface/native"
	"github.com/bdubs00/featherweight-workbench/fleet"
)

// fakeIssuer is an OpenID Connect issuer with a discovery document and a
// token endpoint. A test plays the browser at the authorization endpoint:
// it reads the redirect of /ui/signin and tells the issuer what it saw.
type fakeIssuer struct {
	srv *httptest.Server

	mu          sync.Mutex
	discoveries int
	methods     []string // token_endpoint_auth_methods_supported
	code        string   // the code that the token endpoint accepts
	challenge   string   // the PKCE challenge of the authorization request
	redirect    string   // the redirect URI of the authorization request
	nonce       string   // the nonce of the ID token
	access      string   // the access token of the answer
	idSubject   string   // the subject of the ID token, or "" for no ID token
	status      int      // the status of the token endpoint when not 0
	forms       []url.Values
	auths       []string // the Authorization headers at the token endpoint
}

func newFakeIssuer(t *testing.T) *fakeIssuer {
	t.Helper()
	is := &fakeIssuer{}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		is.mu.Lock()
		is.discoveries++
		doc := map[string]any{
			"issuer":                 is.srv.URL,
			"authorization_endpoint": is.srv.URL + "/authorize?tenant=t1",
			"token_endpoint":         is.srv.URL + "/token",
		}
		if is.methods != nil {
			doc["token_endpoint_auth_methods_supported"] = is.methods
		}
		is.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(doc)
	})
	mux.HandleFunc("POST /token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		is.mu.Lock()
		defer is.mu.Unlock()
		is.forms = append(is.forms, r.PostForm)
		is.auths = append(is.auths, r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		f := r.PostForm
		if is.status != 0 {
			w.WriteHeader(is.status)
			_, _ = io.WriteString(w, `{"error":"invalid_grant","error_description":"the code is used"}`)
			return
		}
		if f.Get("grant_type") != "authorization_code" || f.Get("code") != is.code || f.Get("redirect_uri") != is.redirect ||
			pkceChallenge(f.Get("code_verifier")) != is.challenge {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":"invalid_grant"}`)
			return
		}
		ans := map[string]any{"access_token": is.access, "token_type": "Bearer", "expires_in": 300}
		if is.idSubject != "" {
			ans["id_token"] = idToken(is.idSubject, is.nonce)
		}
		_ = json.NewEncoder(w).Encode(ans)
	})
	is.srv = httptest.NewServer(mux)
	t.Cleanup(is.srv.Close)
	return is
}

// idToken is an ID token with a subject and a nonce. Its signature is
// fake: the test Authn checks the payload only.
func idToken(sub, nonce string) string {
	enc := base64.RawURLEncoding.EncodeToString
	claims, _ := json.Marshal(map[string]string{"sub": sub, "nonce": nonce})
	return enc([]byte(`{"alg":"RS256"}`)) + "." + enc(claims) + ".c2ln"
}

// authorized records what the browser sees at the authorization endpoint,
// and returns the code that the issuer then accepts.
func (is *fakeIssuer) authorized(q url.Values, access, idSubject string) string {
	is.mu.Lock()
	defer is.mu.Unlock()
	is.code = "code-" + q.Get("state")[:8]
	is.challenge = q.Get("code_challenge")
	is.redirect = q.Get("redirect_uri")
	is.nonce = q.Get("nonce")
	is.access = access
	is.idSubject = idSubject
	return is.code
}

func (is *fakeIssuer) tokenCalls() ([]url.Values, []string) {
	is.mu.Lock()
	defer is.mu.Unlock()
	return append([]url.Values(nil), is.forms...), append([]string(nil), is.auths...)
}

// signinAuthn accepts the access token "access-good" as alice, and an ID
// token of the subject carol, as an OIDC driver with the client id in its
// audience does.
type signinAuthn struct{}

func (signinAuthn) Authenticate(r *http.Request) (*fw.Principal, error) {
	tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if tok == "access-good" {
		return &fw.Principal{Subject: "alice", Kind: fw.KindUser}, nil
	}
	if parts := strings.Split(tok, "."); len(parts) == 3 {
		raw, _ := base64.RawURLEncoding.DecodeString(parts[1])
		var c struct{ Sub string }
		if json.Unmarshal(raw, &c) == nil && c.Sub == "carol" {
			return &fw.Principal{Subject: "carol", Kind: fw.KindUser}, nil
		}
	}
	return nil, fw.ErrUnauthorized
}

func (signinAuthn) Challenge(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="fw"`)
	w.WriteHeader(http.StatusUnauthorized)
}

// lockedBuffer is a log sink for several goroutines.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// testClock is a clock that a test moves.
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// signinEnv is an agent with browser sign-in against a fake issuer.
type signinEnv struct {
	issuer *fakeIssuer
	url    string
	clock  *testClock
	log    *lockedBuffer
}

// newSigninEnv serves an agent with browser sign-in. change edits the
// configuration before the agent starts.
func newSigninEnv(t *testing.T, change func(*fw.Config), opts ...fw.Option) *signinEnv {
	t.Helper()
	e := &signinEnv{issuer: newFakeIssuer(t), clock: &testClock{t: time.Now()}, log: &lockedBuffer{}}
	a, err := signinAgent(t, e, change, opts...)
	if err != nil {
		t.Fatal(err)
	}
	h, err := a.Handler()
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	e.url = srv.URL
	return e
}

func signinAgent(t *testing.T, e *signinEnv, change func(*fw.Config), opts ...fw.Option) (*fw.Agent, error) {
	t.Helper()
	t.Setenv("FW_ATESPACE", "")
	// The environment of the test never supplies the shared key.
	t.Setenv(sharedKeySecret, "")
	t.Setenv(sharedKeySecret+"_FILE", "")
	cfg := fw.DefaultConfig()
	cfg.Home = t.TempDir()
	cfg.Auth.Mode = "oidc"
	cfg.Auth.OIDC = &fw.OIDCConfig{Issuer: e.issuer.srv.URL, Insecure: true, ClientID: "fw-dashboard", Scopes: []string{"fw", "openid"}}
	if change != nil {
		change(cfg)
	}
	base := []fw.Option{
		fw.WithoutDefaults(),
		fw.WithConfig(cfg),
		fw.WithAuthn(signinAuthn{}),
		fw.WithModel(newScript().model()),
		fw.WithLedger(fw.NewMemLedger()),
		fw.WithBus(fw.NewMemBus()),
		fw.WithLogger(slog.New(slog.NewTextHandler(e.log, &slog.HandlerOptions{Level: slog.LevelDebug}))),
		fw.WithSurfaces(native.NewWith(newExtension(e.clock.now))),
	}
	a, err := fw.New(append(base, opts...)...)
	if err != nil {
		return nil, err
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		a.Close(ctx)
	})
	return a, nil
}

// browser sends requests without following redirects.
var browser = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

func get(t *testing.T, target string, cookie *http.Cookie, header ...string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, target, nil)
	if err != nil {
		t.Fatal(err)
	}
	if cookie != nil {
		req.AddCookie(cookie)
	}
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	resp, err := browser.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

// start opens /ui/signin and returns the query of the authorization
// request and the sign-in cookie.
func (e *signinEnv) start(t *testing.T, next string, header ...string) (url.Values, *http.Cookie) {
	t.Helper()
	resp, body := get(t, e.url+"/ui/signin?next="+url.QueryEscape(next), nil, header...)
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("signin: status %d: %s", resp.StatusCode, body)
	}
	loc, err := url.Parse(resp.Header.Get("Location"))
	if err != nil || !strings.HasPrefix(loc.String(), e.issuer.srv.URL+"/authorize?") {
		t.Fatalf("signin: Location = %q", resp.Header.Get("Location"))
	}
	var cookie *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == signinCookie {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatalf("signin: no cookie in %v", resp.Header["Set-Cookie"])
	}
	return loc.Query(), cookie
}

// callback opens /ui/callback with a query and a cookie. It checks the
// headers of every answer: no cache, no referrer, and a deleted cookie.
func (e *signinEnv) callback(t *testing.T, query url.Values, cookie *http.Cookie) (*http.Response, string) {
	t.Helper()
	resp, body := get(t, e.url+"/ui/callback?"+query.Encode(), cookie)
	if resp.Header.Get("Cache-Control") != "no-store" || resp.Header.Get("Referrer-Policy") != "no-referrer" ||
		!strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html") || !strings.Contains(resp.Header.Get("Content-Security-Policy"), "default-src 'none'") {
		t.Errorf("callback headers = %v", resp.Header)
	}
	cleared := false
	for _, c := range resp.Cookies() {
		if c.Name == signinCookie && c.MaxAge < 0 && c.Value == "" && c.Path == "/ui" {
			cleared = true
		}
	}
	if !cleared {
		t.Errorf("callback keeps the cookie: %v", resp.Header["Set-Cookie"])
	}
	return resp, body
}

// noSecrets fails when the log holds a value that must never be logged.
func (e *signinEnv) noSecrets(t *testing.T, values ...string) {
	t.Helper()
	log := e.log.String()
	for _, v := range values {
		if v != "" && strings.Contains(log, v) {
			t.Errorf("the log holds a secret value %.12q...", v)
		}
	}
}

var scriptNonce = regexp.MustCompile(`<script nonce="([^"]+)">`)

// checkDone checks the page that ends a sign-in: one script with the nonce
// of the policy, which stores the token and returns to /ui with next.
func checkDone(t *testing.T, resp *http.Response, body, token, next string) {
	t.Helper()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("callback: status %d: %s", resp.StatusCode, body)
	}
	m := scriptNonce.FindStringSubmatch(body)
	csp := resp.Header.Get("Content-Security-Policy")
	if m == nil || !strings.Contains(csp, "script-src 'nonce-"+m[1]+"'") || strings.Contains(csp, "unsafe-inline") || strings.Count(body, "<script") != 1 {
		t.Fatalf("policy %q, page %s", csp, body)
	}
	tok, _ := json.Marshal(token)
	target := "/ui" + next
	if strings.HasPrefix(next, "/") {
		target = next
	}
	nx, _ := json.Marshal(target)
	if !strings.Contains(body, `sessionStorage.setItem("fw.token", `+string(tok)+`)`) || !strings.Contains(body, `location.replace(`+string(nx)+`)`) {
		t.Fatalf("page = %s", body)
	}
}

func TestSigninFlow(t *testing.T) {
	e := newSigninEnv(t, nil)

	// A 401 names the sign-in route.
	resp, b := do(t, "GET", e.url+"/v1/whoami", "")
	if p := decodeProblem(t, resp, b, http.StatusUnauthorized); p.Signin != "/ui/signin" {
		t.Fatalf("problem = %s", b)
	}

	q, cookie := e.start(t, "#/run/01ABC")
	for k, want := range map[string]string{
		"tenant":                "t1",
		"response_type":         "code",
		"client_id":             "fw-dashboard",
		"redirect_uri":          e.url + "/ui/callback",
		"scope":                 "openid fw",
		"code_challenge_method": "S256",
	} {
		if got := q.Get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	if len(q.Get("state")) < 43 || len(q.Get("nonce")) < 43 || len(q.Get("code_challenge")) != 43 || q.Get("state") == q.Get("nonce") {
		t.Fatalf("authorization request = %v", q)
	}
	if !cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode || cookie.Path != "/ui" || cookie.MaxAge != 600 || cookie.Secure {
		t.Fatalf("cookie = %+v", cookie)
	}
	code := e.issuer.authorized(q, "access-good", "carol")
	resp, body := e.callback(t, url.Values{"code": {code}, "state": {q.Get("state")}}, cookie)
	checkDone(t, resp, body, "access-good", "#/run/01ABC")

	// A public client sends its id and the verifier, and no secret.
	forms, auths := e.issuer.tokenCalls()
	if len(forms) != 1 || forms[0].Get("client_id") != "fw-dashboard" || forms[0].Get("client_secret") != "" || auths[0] != "" ||
		len(forms[0].Get("code_verifier")) != 43 {
		t.Fatalf("token request = %v, %q", forms, auths)
	}
	// The token works on the API.
	resp, b = do(t, "GET", e.url+"/v1/whoami", "", "Authorization", "Bearer access-good")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("whoami with the token: %d %s", resp.StatusCode, b)
	}
	// The second sign-in reads the cached discovery document.
	e.start(t, "")
	e.issuer.mu.Lock()
	n := e.issuer.discoveries
	e.issuer.mu.Unlock()
	if n != 1 {
		t.Errorf("discovery requests = %d", n)
	}
	e.noSecrets(t, "access-good", code, q.Get("state"), q.Get("nonce"), forms[0].Get("code_verifier"), cookie.Value)
}

func TestSigninIDToken(t *testing.T) {
	const secret = "s3cret-client-value"
	secrets := fw.SecretsFunc(func(_ context.Context, name string) (string, error) {
		if name == "FW_TEST_CLIENT_SECRET" {
			return secret, nil
		}
		return "", fw.ErrSecretNotFound
	})
	confidential := func(c *fw.Config) { c.Auth.OIDC.ClientSecret = "FW_TEST_CLIENT_SECRET" }

	// The access token is opaque to this server, so the ID token is kept.
	// A confidential client authenticates with Basic.
	e := newSigninEnv(t, confidential, fw.WithSecrets(secrets))
	q, cookie := e.start(t, "#agents")
	code := e.issuer.authorized(q, "opaque-access", "carol")
	resp, body := e.callback(t, url.Values{"code": {code}, "state": {q.Get("state")}}, cookie)
	id := idToken("carol", q.Get("nonce"))
	checkDone(t, resp, body, id, "#agents")
	forms, auths := e.issuer.tokenCalls()
	basic := "Basic " + base64.StdEncoding.EncodeToString([]byte("fw-dashboard:"+secret))
	if len(forms) != 1 || auths[0] != basic || forms[0].Get("client_secret") != "" || forms[0].Get("client_id") != "" {
		t.Fatalf("token request = %v", forms)
	}
	if strings.Contains(body, "opaque-access") {
		t.Fatal("the page holds the refused access token")
	}
	e.noSecrets(t, secret, id, "opaque-access", code, forms[0].Get("code_verifier"))

	// An issuer that takes client_secret_post only gets the secret in the
	// form.
	e = newSigninEnv(t, confidential, fw.WithSecrets(secrets))
	e.issuer.methods = []string{"client_secret_post"}
	q, cookie = e.start(t, "")
	code = e.issuer.authorized(q, "access-good", "")
	resp, body = e.callback(t, url.Values{"code": {code}, "state": {q.Get("state")}}, cookie)
	checkDone(t, resp, body, "access-good", "")
	forms, auths = e.issuer.tokenCalls()
	if len(forms) != 1 || auths[0] != "" || forms[0].Get("client_secret") != secret || forms[0].Get("client_id") != "fw-dashboard" {
		t.Fatalf("token request = %v %q", forms, auths)
	}
	e.noSecrets(t, secret, "access-good", code)

	// A secret that the Secrets plug does not have fails with a page.
	e = newSigninEnv(t, func(c *fw.Config) { c.Auth.OIDC.ClientSecret = "FW_MISSING_SECRET" }, fw.WithSecrets(secrets))
	q, cookie = e.start(t, "")
	code = e.issuer.authorized(q, "access-good", "")
	resp, body = e.callback(t, url.Values{"code": {code}, "state": {q.Get("state")}}, cookie)
	if resp.StatusCode != http.StatusInternalServerError || !strings.Contains(body, "client secret") {
		t.Fatalf("missing secret: %d %s", resp.StatusCode, body)
	}
	if forms, _ := e.issuer.tokenCalls(); len(forms) != 0 {
		t.Fatalf("token requests without the secret = %d", len(forms))
	}
}

func TestSigninFailures(t *testing.T) {
	failed := func(t *testing.T, resp *http.Response, body string, status int, says string) {
		t.Helper()
		if resp.StatusCode != status || !strings.Contains(body, says) || !strings.Contains(body, `<a href="/ui">`) ||
			strings.Contains(body, "<script") || strings.Contains(body, "sessionStorage") {
			t.Fatalf("status %d (want %d), page %s", resp.StatusCode, status, body)
		}
	}
	e := newSigninEnv(t, nil)

	t.Run("forged state", func(t *testing.T) {
		q, cookie := e.start(t, "#x")
		code := e.issuer.authorized(q, "access-good", "carol")
		before, _ := e.issuer.tokenCalls()
		resp, body := e.callback(t, url.Values{"code": {code}, "state": {"forged-" + q.Get("state")}}, cookie)
		failed(t, resp, body, http.StatusBadRequest, "does not belong to this sign-in")
		if after, _ := e.issuer.tokenCalls(); len(after) != len(before) {
			t.Fatal("a forged state reached the token endpoint")
		}
	})
	t.Run("forged cookie", func(t *testing.T) {
		q, cookie := e.start(t, "#x")
		code := e.issuer.authorized(q, "access-good", "carol")
		body, sig, _ := strings.Cut(cookie.Value, ".")
		raw, _ := base64.RawURLEncoding.DecodeString(body)
		forged := strings.Replace(string(raw), `"x":"#x"`, `"x":"#y"`, 1)
		cookie.Value = base64.RawURLEncoding.EncodeToString([]byte(forged)) + "." + sig
		resp, page := e.callback(t, url.Values{"code": {code}, "state": {q.Get("state")}}, cookie)
		failed(t, resp, page, http.StatusBadRequest, "not valid")
	})
	t.Run("no cookie", func(t *testing.T) {
		q, _ := e.start(t, "#x")
		code := e.issuer.authorized(q, "access-good", "carol")
		resp, body := e.callback(t, url.Values{"code": {code}, "state": {q.Get("state")}}, nil)
		failed(t, resp, body, http.StatusBadRequest, "no sign-in in progress")
	})
	t.Run("expired cookie", func(t *testing.T) {
		q, cookie := e.start(t, "#x")
		code := e.issuer.authorized(q, "access-good", "carol")
		e.clock.add(signinTTL + time.Second)
		resp, body := e.callback(t, url.Values{"code": {code}, "state": {q.Get("state")}}, cookie)
		failed(t, resp, body, http.StatusBadRequest, "longer than 10 minutes")
	})
	t.Run("bad next", func(t *testing.T) {
		for _, next := range []string{"https://evil.example/#x", "//evil.example", "#a\nb", "#a\u0085b", "#" + strings.Repeat("x", maxNextBytes)} {
			q, cookie := e.start(t, next)
			code := e.issuer.authorized(q, "access-good", "carol")
			resp, body := e.callback(t, url.Values{"code": {code}, "state": {q.Get("state")}}, cookie)
			checkDone(t, resp, body, "access-good", "")
		}
	})
	t.Run("refused token", func(t *testing.T) {
		q, cookie := e.start(t, "#x")
		code := e.issuer.authorized(q, "access-bad", "mallory")
		resp, body := e.callback(t, url.Values{"code": {code}, "state": {q.Get("state")}}, cookie)
		failed(t, resp, body, http.StatusForbidden, "does not accept the token")
		if strings.Contains(body, "access-bad") {
			t.Fatal("the page holds the refused token")
		}
		e.noSecrets(t, "access-bad", code)
	})
	t.Run("other nonce", func(t *testing.T) {
		q, cookie := e.start(t, "#x")
		code := e.issuer.authorized(q, "access-good", "carol")
		e.issuer.mu.Lock()
		e.issuer.nonce = "another-nonce"
		e.issuer.mu.Unlock()
		resp, body := e.callback(t, url.Values{"code": {code}, "state": {q.Get("state")}}, cookie)
		failed(t, resp, body, http.StatusBadRequest, "ID token")
	})
	t.Run("issuer refuses", func(t *testing.T) {
		q, cookie := e.start(t, "#x")
		resp, body := e.callback(t, url.Values{"error": {"access_denied"}, "state": {q.Get("state")}}, cookie)
		failed(t, resp, body, http.StatusForbidden, "access_denied")
		q, cookie = e.start(t, "#x")
		resp, body = e.callback(t, url.Values{"error": {"<b>bold</b>"}, "state": {q.Get("state")}}, cookie)
		failed(t, resp, body, http.StatusForbidden, "refused the sign-in")
		if strings.Contains(body, "<b>") {
			t.Fatalf("page = %s", body)
		}
	})
	t.Run("code refused", func(t *testing.T) {
		q, cookie := e.start(t, "#x")
		e.issuer.authorized(q, "access-good", "carol")
		resp, body := e.callback(t, url.Values{"code": {"not-the-code"}, "state": {q.Get("state")}}, cookie)
		failed(t, resp, body, http.StatusBadGateway, "did not accept the authorization code")
		e.noSecrets(t, "not-the-code")
	})
	t.Run("no code", func(t *testing.T) {
		q, cookie := e.start(t, "#x")
		resp, body := e.callback(t, url.Values{"state": {q.Get("state")}}, cookie)
		failed(t, resp, body, http.StatusBadRequest, "no authorization code")
	})
}

// Replicas behind one address accept the sign-in cookies of each other
// when they share FW_MCP_STATE_KEY, the client secret, or the home
// directory. Without any of them, they do not.
func TestSigninAcrossReplicas(t *testing.T) {
	only := func(name, value string) fw.Option {
		return fw.WithSecrets(fw.SecretsFunc(func(_ context.Context, n string) (string, error) {
			if n == name {
				return value, nil
			}
			return "", fw.ErrSecretNotFound
		}))
	}
	confidential := func(c *fw.Config) { c.Auth.OIDC.ClientSecret = "FW_TEST_CLIENT_SECRET" }
	home := t.TempDir()
	sameHome := func(c *fw.Config) { c.Home = home }
	for _, c := range []struct {
		name   string
		change func(*fw.Config)
		opt    fw.Option
		shared bool
	}{
		{"shared state key", nil, only(sharedKeySecret, "replica-key"), true},
		{"client secret", confidential, only("FW_TEST_CLIENT_SECRET", "client-secret"), true},
		{"shared home", sameHome, only("NONE", ""), true},
		{"nothing shared", nil, only("NONE", ""), false},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newSigninEnv(t, c.change, c.opt)
			other := &signinEnv{issuer: e.issuer, clock: e.clock, log: e.log}
			a, err := signinAgent(t, other, c.change, c.opt)
			if err != nil {
				t.Fatal(err)
			}
			h, err := a.Handler()
			if err != nil {
				t.Fatal(err)
			}
			srv := httptest.NewServer(h)
			t.Cleanup(srv.Close)
			other.url = srv.URL

			q, cookie := e.start(t, "#x")
			code := e.issuer.authorized(q, "access-good", "")
			resp, body := other.callback(t, url.Values{"code": {code}, "state": {q.Get("state")}}, cookie)
			if c.shared {
				checkDone(t, resp, body, "access-good", "#x")
				return
			}
			if resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, "not valid") {
				t.Fatalf("another replica: %d %s", resp.StatusCode, body)
			}
		})
	}
}

// The cookie key comes from FW_MCP_STATE_KEY, else the client secret, else
// the file signin.key under the home directory, else memory. Each key is
// an HMAC of its source with a label, so it never equals the MCP state key.
func TestSigninCookieKeyOrder(t *testing.T) {
	label := func(v []byte) []byte {
		m := hmac.New(sha256.New, v)
		m.Write([]byte(signinKeyLabel))
		return m.Sum(nil)
	}
	signinOf := func(t *testing.T, change func(*fw.Config), values map[string]string) (*signin, *lockedBuffer) {
		t.Helper()
		e := &signinEnv{issuer: newFakeIssuer(t), clock: &testClock{t: time.Now()}, log: &lockedBuffer{}}
		secrets := fw.SecretsFunc(func(_ context.Context, name string) (string, error) {
			if v, ok := values[name]; ok {
				return v, nil
			}
			return "", fw.ErrSecretNotFound
		})
		a, err := signinAgent(t, e, change, fw.WithSecrets(secrets))
		if err != nil {
			t.Fatal(err)
		}
		si, err := newSignin(a, time.Now)
		if err != nil || si == nil {
			t.Fatalf("newSignin = %v, %v", si, err)
		}
		return si, e.log
	}
	confidential := func(c *fw.Config) { c.Auth.OIDC.ClientSecret = "FW_TEST_CLIENT_SECRET" }
	both := map[string]string{sharedKeySecret: "state-key", "FW_TEST_CLIENT_SECRET": "client-secret"}

	si, _ := signinOf(t, confidential, both)
	mcpKey := sha256.Sum256([]byte("state-key"))
	if si.keySource != sharedKeySecret || !bytes.Equal(si.key, label([]byte("state-key"))) || bytes.Equal(si.key, mcpKey[:]) {
		t.Errorf("shared key: source %q", si.keySource)
	}
	si, _ = signinOf(t, confidential, map[string]string{"FW_TEST_CLIENT_SECRET": "client-secret"})
	if si.keySource != "FW_TEST_CLIENT_SECRET" || !bytes.Equal(si.key, label([]byte("client-secret"))) {
		t.Errorf("client secret: source %q", si.keySource)
	}

	// A public client without the shared key creates the key file once.
	home := t.TempDir()
	si, log := signinOf(t, func(c *fw.Config) { c.Home = home }, nil)
	path := filepath.Join(home, signinKeyFile)
	raw, err := os.ReadFile(path)
	if err != nil || len(raw) != signinKeyBytes || si.keySource != keyFromFile || !bytes.Equal(si.key, label(raw)) {
		t.Fatalf("file key: source %q, %d bytes, %v", si.keySource, len(raw), err)
	}
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("key file mode = %v, %v", fi.Mode(), err)
	}
	if strings.Contains(log.String(), "WARN") {
		t.Errorf("a key file logs a warning: %s", log)
	}
	again, _ := signinOf(t, func(c *fw.Config) { c.Home = home }, nil)
	if !bytes.Equal(again.key, si.key) {
		t.Error("a second process with the same home has another key")
	}

	// A home that cannot hold the file, or a damaged file, leaves a key in
	// memory with one warning.
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	damaged := t.TempDir()
	if err := os.WriteFile(filepath.Join(damaged, signinKeyFile), []byte("short"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, dir := range map[string]string{"no home": filepath.Join(blocker, "home"), "damaged file": damaged} {
		si, log := signinOf(t, func(c *fw.Config) { c.Home = dir }, nil)
		other, _ := signinOf(t, func(c *fw.Config) { c.Home = dir }, nil)
		if si.keySource != keyInMemory || len(si.key) != sha256.Size || bytes.Equal(si.key, other.key) {
			t.Errorf("%s: source %q", name, si.keySource)
		}
		if n := strings.Count(log.String(), "lives in memory only"); n != 1 {
			t.Errorf("%s: %d warnings: %s", name, n, log)
		}
	}
	if raw, _ := os.ReadFile(filepath.Join(damaged, signinKeyFile)); string(raw) != "short" {
		t.Error("a damaged key file was replaced")
	}
}

// Processes that start together agree on one key file.
func TestLoadKeyFileTogether(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", signinKeyFile)
	const n = 8
	keys := make([][]byte, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			keys[i], errs[i] = loadKeyFile(path)
		}()
	}
	wg.Wait()
	for i := range n {
		if errs[i] != nil || !bytes.Equal(keys[i], keys[0]) || len(keys[i]) != signinKeyBytes {
			t.Fatalf("process %d: %v", i, errs[i])
		}
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil || len(entries) != 1 {
		t.Fatalf("the directory holds %d entries: %v", len(entries), err)
	}
}

func TestSigninIssuerDown(t *testing.T) {
	e := newSigninEnv(t, nil)
	e.issuer.srv.Close()
	resp, body := get(t, e.url+"/ui/signin?next=%23x", nil)
	if resp.StatusCode != http.StatusBadGateway || !strings.Contains(body, `<a href="/ui">`) || len(resp.Cookies()) != 0 {
		t.Fatalf("issuer down: %d %s", resp.StatusCode, body)
	}
}

func TestSigninRedirectURI(t *testing.T) {
	// The configured address wins. The request comes through a proxy on
	// the host of the callback, so the sign-in starts at once.
	e := newSigninEnv(t, func(c *fw.Config) {
		c.Auth.OIDC.RedirectURL = "https://fw.example.com/ui/callback"
		c.MCP.Auth.Audience = "https://other.example.com/mcp"
	})
	q, _ := e.start(t, "", "X-Forwarded-Proto", "https", "X-Forwarded-Host", "fw.example.com")
	if q.Get("redirect_uri") != "https://fw.example.com/ui/callback" {
		t.Errorf("configured: %q", q.Get("redirect_uri"))
	}
	// Then the public address of the page.
	e = newSigninEnv(t, func(c *fw.Config) { c.MCP.Auth.Audience = "https://other.example.com/mcp" })
	q, _ = e.start(t, "", "X-Forwarded-Proto", "https", "X-Forwarded-Host", "other.example.com")
	if q.Get("redirect_uri") != "https://other.example.com/ui/callback" {
		t.Errorf("page address: %q", q.Get("redirect_uri"))
	}
	// Then the request, as a proxy forwards it, and a cookie over https is
	// Secure.
	e = newSigninEnv(t, nil)
	q, cookie := e.start(t, "", "X-Forwarded-Proto", "https", "X-Forwarded-Host", "fw.example.org")
	if q.Get("redirect_uri") != "https://fw.example.org/ui/callback" || !cookie.Secure {
		t.Errorf("request: %q, secure %v", q.Get("redirect_uri"), cookie.Secure)
	}
	// The exchange sends the redirect URI of the authorization request.
	code := e.issuer.authorized(q, "access-good", "")
	resp, body := e.callback(t, url.Values{"code": {code}, "state": {q.Get("state")}}, cookie)
	checkDone(t, resp, body, "access-good", "")
}

// The sign-in cookie belongs to the host that serves /ui/signin, and the
// callback reads it on the host of the redirect URI. A sign-in that starts
// on another host moves to /ui/signin on the host of the callback first,
// once, with the same next.
func TestSigninMovesToTheCallbackHost(t *testing.T) {
	page := func(c *fw.Config) { c.MCP.Auth.Audience = "https://fw.example.com/mcp" }
	e := newSigninEnv(t, page)
	moved := func(t *testing.T, target string, header ...string) string {
		t.Helper()
		resp, body := get(t, target, nil, header...)
		if resp.StatusCode != http.StatusFound || len(resp.Cookies()) != 0 {
			t.Fatalf("signin on another host: %d, %d cookies, %s", resp.StatusCode, len(resp.Cookies()), body)
		}
		return resp.Header.Get("Location")
	}
	// A port-forward or an internal name: the request reaches this process
	// on another origin than the page.
	if loc := moved(t, e.url+"/ui/signin?next="+url.QueryEscape("#runs/01A")); loc != "https://fw.example.com/ui/signin?hop=1&next=%23runs%2F01A" {
		t.Errorf("moved to %q", loc)
	}
	if loc := moved(t, e.url+"/ui/signin"); loc != "https://fw.example.com/ui/signin?hop=1" {
		t.Errorf("moved without next to %q", loc)
	}
	// A bad next does not travel.
	if loc := moved(t, e.url+"/ui/signin?next=javascript:x"); loc != "https://fw.example.com/ui/signin?hop=1" {
		t.Errorf("moved with a bad next to %q", loc)
	}

	// On the host of the callback, the sign-in starts. A default port is
	// the same origin.
	for _, host := range []string{"fw.example.com", "FW.example.com:443"} {
		q, cookie := e.start(t, "#runs", "X-Forwarded-Proto", "https", "X-Forwarded-Host", host)
		if q.Get("redirect_uri") != "https://fw.example.com/ui/callback" || !cookie.Secure {
			t.Errorf("%s: redirect %q, secure %v", host, q.Get("redirect_uri"), cookie.Secure)
		}
	}
	// The mark of the move stops a second move, so a proxy that hides the
	// host never makes a loop.
	resp, body := get(t, e.url+"/ui/signin?hop=1&next=%23runs", nil)
	if loc := resp.Header.Get("Location"); resp.StatusCode != http.StatusFound || !strings.HasPrefix(loc, e.issuer.srv.URL+"/authorize?") {
		t.Fatalf("after the move: %d %q %s", resp.StatusCode, loc, body)
	}

	// The configured redirect URI keeps the prefix of its path.
	e = newSigninEnv(t, func(c *fw.Config) { c.Auth.OIDC.RedirectURL = "https://example.com/fw/ui/callback" })
	if loc := moved(t, e.url+"/ui/signin?next=%23x"); loc != "https://example.com/fw/ui/signin?hop=1&next=%23x" {
		t.Errorf("moved under a prefix to %q", loc)
	}
	e = newSigninEnv(t, func(c *fw.Config) { c.Auth.OIDC.RedirectURL = "https://example.com/oauth/return" })
	if loc := moved(t, e.url+"/ui/signin"); loc != "https://example.com/ui/signin?hop=1" {
		t.Errorf("moved for another callback path to %q", loc)
	}
}

// A key file under the home directory belongs to one process. With a
// fleet, or with a ledger or a bus that processes share, the replicas
// likely have files of their own, so the start logs one warning.
func TestSigninKeyFileWarnsForReplicas(t *testing.T) {
	warned := func(t *testing.T, change func(*fw.Config), v fleet.View, opts ...fw.Option) int {
		t.Helper()
		e := &signinEnv{issuer: newFakeIssuer(t), clock: &testClock{t: time.Now()}, log: &lockedBuffer{}}
		a, err := signinAgent(t, e, change, opts...)
		if err != nil {
			t.Fatal(err)
		}
		if v != nil {
			fleet.Attach(a, v)
		}
		if _, err := a.Handler(); err != nil {
			t.Fatal(err)
		}
		return strings.Count(e.log.String(), "likely has replicas")
	}
	fake := &fakeFleet{name: "view", members: []fleet.Member{{Name: "view", Self: true, State: fleet.MemberLive}}}
	if n := warned(t, nil, fake); n != 1 {
		t.Errorf("fleet: %d warnings", n)
	}
	if n := warned(t, func(c *fw.Config) { c.Bus.Driver = "nats" }, nil); n != 1 {
		t.Errorf("shared bus: %d warnings", n)
	}
	if n := warned(t, nil, nil); n != 0 {
		t.Errorf("one process: %d warnings", n)
	}
	// A shared key or a client secret is the same in every replica.
	shared := fw.WithSecrets(fw.SecretsFunc(func(_ context.Context, n string) (string, error) {
		if n == sharedKeySecret {
			return "replica-key", nil
		}
		return "", fw.ErrSecretNotFound
	}))
	if n := warned(t, nil, fake, shared); n != 0 {
		t.Errorf("fleet with a shared key: %d warnings", n)
	}
}

func TestSigninOff(t *testing.T) {
	// Without a client id, the routes do not exist and a 401 names no
	// sign-in.
	a := newAgent(t, newScript().model(), fw.WithAuthn(people))
	srv := serve(t, a)
	for _, p := range []string{"/ui/signin", "/ui/callback"} {
		if resp, _ := get(t, srv.URL+p, nil); resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s: status %d", p, resp.StatusCode)
		}
	}
	resp, b := do(t, "GET", srv.URL+"/v1/whoami", "")
	decodeProblem(t, resp, b, http.StatusUnauthorized)
	if _, ok := keys(t, b)["signin"]; ok {
		t.Fatalf("problem = %s", b)
	}
}

func TestSigninConfig(t *testing.T) {
	for name, change := range map[string]func(*fw.Config){
		"http issuer":  func(c *fw.Config) { c.Auth.OIDC.Insecure = false },
		"no issuer":    func(c *fw.Config) { c.Auth.OIDC.Issuer = "" },
		"bad redirect": func(c *fw.Config) { c.Auth.OIDC.RedirectURL = "/ui/callback" },
		"bad ca":       func(c *fw.Config) { c.Auth.OIDC.CA = "-----BEGIN CERTIFICATE-----\nnope\n-----END CERTIFICATE-----" },
	} {
		e := &signinEnv{issuer: newFakeIssuer(t), clock: &testClock{t: time.Now()}, log: &lockedBuffer{}}
		a, err := signinAgent(t, e, change)
		if err != nil {
			t.Fatalf("%s: fw.New: %v", name, err)
		}
		if _, err := a.Handler(); err == nil || !strings.Contains(err.Error(), "native: ") {
			t.Errorf("%s: Handler = %v", name, err)
		}
	}
}

func TestUIPolicyAllowsSignin(t *testing.T) {
	e := newSigninEnv(t, nil)
	resp, _ := get(t, e.url+"/ui", nil)
	csp := resp.Header.Get("Content-Security-Policy")
	// A link or location change to /ui/signin is a navigation, which no
	// directive here limits.
	for _, d := range []string{"navigate-to", "sandbox"} {
		if strings.Contains(csp, d) {
			t.Errorf("policy %q limits navigation with %s", csp, d)
		}
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

// A page that a package registers takes the person back to its own path
// after the sign-in; any other path returns to /ui.
func TestSigninReturnsToPage(t *testing.T) {
	AllowSigninReturn("/bossmode/")
	e := newSigninEnv(t, nil)
	q, cookie := e.start(t, "/bossmode/#bridge/01K")
	code := e.issuer.authorized(q, "access-good", "carol")
	resp, body := e.callback(t, url.Values{"code": {code}, "state": {q.Get("state")}}, cookie)
	checkDone(t, resp, body, "access-good", "/bossmode/#bridge/01K")

	q, cookie = e.start(t, "/elsewhere/#x")
	code = e.issuer.authorized(q, "access-good", "carol")
	resp, body = e.callback(t, url.Values{"code": {code}, "state": {q.Get("state")}}, cookie)
	checkDone(t, resp, body, "access-good", "")
}

func TestSigninNext(t *testing.T) {
	AllowSigninReturn("/bossmode/")
	AllowSigninReturn("no-slash")
	for in, want := range map[string]string{
		"#run/01ABC":                "#run/01ABC",
		"/bossmode/":                "/bossmode/",
		"/bossmode/#bridge/01K":     "/bossmode/#bridge/01K",
		"/bossmode/bridges/01K?x=1": "/bossmode/bridges/01K?x=1",
		"/bossmode":                 "",
		"/bossmodex/":               "",
		"//evil.example/bossmode/":  "",
		"https://evil.example/":     "",
		"/bossmode/\\evil.example":  "",
		"/bossmode/\x00":            "",
		"/bossmode/\t":              "",
		"/bossmode/\xff":            "",
		"no-slash/x":                "",
		"/ui#x":                     "",
		"/bossmode/" + strings.Repeat("x", maxNextBytes): "",
	} {
		if got := signinNext(in); got != want {
			t.Errorf("signinNext(%.30q) = %.30q, want %.30q", in, got, want)
		}
	}
}

func TestCleanNext(t *testing.T) {
	for in, want := range map[string]string{
		"#run/01ABC":          "#run/01ABC",
		"#":                   "#",
		"#a b?c=d&e":          "#a b?c=d&e",
		"#é":                  "#é",
		"":                    "",
		"run":                 "",
		"/ui#x":               "",
		"javascript:alert(1)": "",
		"#a\tb":               "",
		"#a\x00":              "",
		"#a\u009b":            "",
		"#\xff":               "",
		"#" + strings.Repeat("x", maxNextBytes-1): "#" + strings.Repeat("x", maxNextBytes-1),
		"#" + strings.Repeat("x", maxNextBytes):   "",
	} {
		if got := cleanNext(in); got != want {
			t.Errorf("cleanNext(%.20q) = %.20q, want %.20q", in, got, want)
		}
	}
}
