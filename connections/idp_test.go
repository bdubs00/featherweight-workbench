package connections

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeIdP is an authorization server with refresh token rotation. A
// refresh token that comes back after it was rotated revokes its whole
// family, as the strictest servers do.
type fakeIdP struct {
	*httptest.Server
	t        *testing.T
	clientID string
	secret   string

	refreshDelay   time.Duration
	noRefreshToken bool

	mu          sync.Mutex
	n           int
	codes       map[string]codeGrant
	families    map[string]*family
	rts         map[string]string // refresh token to family id
	access      map[string]accessInfo
	refreshes   int
	exchanges   int
	revocations int
	forms       []url.Values
}

type codeGrant struct {
	subject, challenge, redirect, nonce string
}

type family struct {
	subject string
	current string
	revoked bool
}

type accessInfo struct{ subject, aud string }

func newFakeIdP(t *testing.T) *fakeIdP {
	t.Helper()
	idp := &fakeIdP{t: t, clientID: "fw", secret: "oidc-secret",
		codes: map[string]codeGrant{}, families: map[string]*family{}, rts: map[string]string{}, access: map[string]accessInfo{}}
	mux := http.NewServeMux()
	meta := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                                idp.URL,
			"authorization_endpoint":                idp.URL + "/authorize",
			"token_endpoint":                        idp.URL + "/token",
			"revocation_endpoint":                   idp.URL + "/revoke",
			"token_endpoint_auth_methods_supported": []string{"client_secret_post", "client_secret_basic"},
		})
	}
	mux.HandleFunc("GET /.well-known/openid-configuration", meta)
	mux.HandleFunc("GET /.well-known/oauth-authorization-server", meta)
	mux.HandleFunc("POST /token", idp.token)
	mux.HandleFunc("POST /revoke", idp.revoke)
	idp.Server = httptest.NewServer(mux)
	t.Cleanup(idp.Close)
	return idp
}

func (idp *fakeIdP) fail(w http.ResponseWriter, status int, code, desc string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": code, "error_description": desc})
}

// issue makes an access token for a subject and an audience. The caller
// holds idp.mu.
func (idp *fakeIdP) issue(subject, aud string) string {
	idp.n++
	tok := fmt.Sprintf("at-%d-%s", idp.n, subject)
	idp.access[tok] = accessInfo{subject: subject, aud: aud}
	return tok
}

// rotate gives a family a new refresh token. The caller holds idp.mu.
func (idp *fakeIdP) rotate(id string) string {
	idp.n++
	rt := fmt.Sprintf("rt-%d", idp.n)
	idp.families[id].current = rt
	idp.rts[rt] = id
	return rt
}

func unsignedJWT(claims map[string]any) string {
	h := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	b, _ := json.Marshal(claims)
	return h + "." + base64.RawURLEncoding.EncodeToString(b) + ".sig"
}

func (idp *fakeIdP) token(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		idp.fail(w, 400, "invalid_request", err.Error())
		return
	}
	f := r.PostForm
	id, secret := f.Get("client_id"), f.Get("client_secret")
	if u, p, ok := r.BasicAuth(); ok {
		id, secret = u, p
	}
	idp.mu.Lock()
	idp.forms = append(idp.forms, f)
	idp.mu.Unlock()
	if id != idp.clientID || secret != idp.secret {
		idp.fail(w, 401, "invalid_client", "bad client")
		return
	}
	aud := "base"
	if res := f["resource"]; len(res) == 1 {
		aud = res[0]
	}
	answer := map[string]any{"token_type": "Bearer", "expires_in": 300}
	switch f.Get("grant_type") {
	case grantCode:
		idp.mu.Lock()
		cg, ok := idp.codes[f.Get("code")]
		delete(idp.codes, f.Get("code"))
		if !ok || pkceChallenge(f.Get("code_verifier")) != cg.challenge || f.Get("redirect_uri") != cg.redirect {
			idp.mu.Unlock()
			idp.fail(w, 400, "invalid_grant", "bad code")
			return
		}
		idp.n++
		fid := fmt.Sprintf("f%d", idp.n)
		idp.families[fid] = &family{subject: cg.subject}
		if !idp.noRefreshToken {
			answer["refresh_token"] = idp.rotate(fid)
		}
		answer["access_token"] = idp.issue(cg.subject, aud)
		answer["id_token"] = unsignedJWT(map[string]any{"iss": idp.URL, "aud": idp.clientID, "sub": cg.subject, "nonce": cg.nonce, "preferred_username": cg.subject + "@example.com"})
		answer["scope"] = "openid offline_access"
		idp.mu.Unlock()
	case grantRefresh:
		if idp.refreshDelay > 0 {
			time.Sleep(idp.refreshDelay)
		}
		idp.mu.Lock()
		idp.refreshes++
		rt := f.Get("refresh_token")
		fid, ok := idp.rts[rt]
		fam := idp.families[fid]
		switch {
		case !ok || fam.revoked:
			idp.mu.Unlock()
			idp.fail(w, 400, "invalid_grant", "Token is not active")
			return
		case fam.current != rt:
			fam.revoked = true
			idp.mu.Unlock()
			idp.fail(w, 400, "invalid_grant", "Maximum allowed refresh token reuse exceeded")
			return
		}
		answer["refresh_token"] = idp.rotate(fid)
		answer["access_token"] = idp.issue(fam.subject, aud)
		idp.mu.Unlock()
	case grantTokenExchange:
		idp.mu.Lock()
		idp.exchanges++
		ai, ok := idp.access[f.Get("subject_token")]
		if !ok || ai.aud != "base" || f.Get("subject_token_type") != tokenTypeAccess {
			idp.mu.Unlock()
			idp.fail(w, 400, "invalid_grant", "bad subject token")
			return
		}
		answer["access_token"] = idp.issue(ai.subject, f.Get("audience"))
		answer["issued_token_type"] = tokenTypeAccess
		idp.mu.Unlock()
	default:
		idp.fail(w, 400, "unsupported_grant_type", f.Get("grant_type"))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(answer)
}

func (idp *fakeIdP) revoke(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	idp.mu.Lock()
	defer idp.mu.Unlock()
	idp.revocations++
	if fid, ok := idp.rts[r.PostForm.Get("token")]; ok {
		idp.families[fid].revoked = true
	}
}

// authorize plays the browser: the person signs in as subject and the
// server answers with a code.
func (idp *fakeIdP) authorize(t *testing.T, raw, subject string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil || !strings.HasPrefix(raw, idp.URL+"/authorize?") {
		t.Fatalf("authorization URL %q", raw)
	}
	q := u.Query()
	if q.Get("response_type") != "code" || q.Get("code_challenge_method") != "S256" || q.Get("state") == "" || q.Get("client_id") != idp.clientID {
		t.Fatalf("authorization request %v", q)
	}
	idp.mu.Lock()
	defer idp.mu.Unlock()
	idp.n++
	code := fmt.Sprintf("code-%d", idp.n)
	idp.codes[code] = codeGrant{subject: subject, challenge: q.Get("code_challenge"), redirect: q.Get("redirect_uri"), nonce: q.Get("nonce")}
	return code
}

func (idp *fakeIdP) counts() (refreshes, exchanges, revocations int) {
	idp.mu.Lock()
	defer idp.mu.Unlock()
	return idp.refreshes, idp.exchanges, idp.revocations
}

func (idp *fakeIdP) lookup(tok string) (accessInfo, bool) {
	idp.mu.Lock()
	defer idp.mu.Unlock()
	ai, ok := idp.access[tok]
	return ai, ok
}
