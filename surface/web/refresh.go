package web

// This file keeps a browser sign-in alive. An issuer such as Cloudflare
// Access gives tokens that live a few minutes. When the token answer of the
// sign-in holds a refresh token, the callback keeps it in the refresh
// cookie, and the page trades it for a new token at POST /ui/refresh before
// its token expires. DELETE /ui/refresh forgets it when the person signs
// out.
//
// The refresh cookie is encrypted with AES-GCM under a key that comes from
// the key of the sign-in cookie, so every replica that shares that key
// opens it. It is HttpOnly and SameSite=Strict, has the path /ui/refresh,
// and is Secure on https. A script of the page never reads it, and a page
// of another site never sends it. No refresh token enters a log line or an
// answer.

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/bdubs00/featherweight-kernel/surface/native"
	mcpclient "github.com/bdubs00/featherweight-kernel/tools/mcp"
)

// The route and the cookie of the renewal.
const (
	refreshPath   = "/ui/refresh"
	refreshCookie = "fw_refresh"
	// refreshDomain is the purpose of the key of the refresh cookie and
	// the additional data of its encryption.
	refreshDomain = "fw.refresh.v1"
	// maxCookieBytes is the longest refresh cookie value. Browsers keep
	// about 4096 bytes per cookie, with the name and the attributes.
	maxCookieBytes = 3800
)

// refreshState is the content of the refresh cookie: the refresh token,
// the subject of the sign-in, and the end of the refresh token in Unix
// seconds, or 0 when the issuer gave none.
type refreshState struct {
	Token   string `json:"t"`
	Subject string `json:"s"`
	Expires int64  `json:"e,omitempty"`
}

// errRefreshForged means that the refresh cookie does not open.
var errRefreshForged = errors.New("native: the refresh cookie does not open")

// refreshAEAD is the cipher of the refresh cookie.
func (si *signin) refreshAEAD() cipher.AEAD {
	m := hmac.New(sha256.New, si.key)
	m.Write([]byte(refreshDomain))
	block, _ := aes.NewCipher(m.Sum(nil)) // a 32-byte key never fails
	aead, _ := cipher.NewGCM(block)
	return aead
}

// sealRefresh encrypts the state of the refresh cookie.
func (si *signin) sealRefresh(st refreshState) string {
	aead := si.refreshAEAD()
	plain, _ := json.Marshal(st)
	nonce := make([]byte, aead.NonceSize())
	_, _ = rand.Read(nonce)
	return base64.RawURLEncoding.EncodeToString(aead.Seal(nonce, nonce, plain, []byte(refreshDomain)))
}

// openRefresh decrypts the refresh cookie. A cookie that does not open,
// or whose refresh token ended, is an error.
func (si *signin) openRefresh(v string) (refreshState, error) {
	raw, err := base64.RawURLEncoding.DecodeString(v)
	aead := si.refreshAEAD()
	if err != nil || len(raw) < aead.NonceSize() {
		return refreshState{}, errRefreshForged
	}
	plain, err := aead.Open(nil, raw[:aead.NonceSize()], raw[aead.NonceSize():], []byte(refreshDomain))
	if err != nil {
		return refreshState{}, errRefreshForged
	}
	var st refreshState
	if json.Unmarshal(plain, &st) != nil || st.Token == "" || st.Subject == "" {
		return refreshState{}, errRefreshForged
	}
	if st.Expires > 0 && si.now().Unix() >= st.Expires {
		return refreshState{}, errSigninExpired
	}
	return st, nil
}

// refreshCookieOf is the refresh cookie with a value and a life in
// seconds. A life of 0 makes a cookie of the browser session, and a
// negative life deletes the cookie.
func (si *signin) refreshCookieOf(r *http.Request, value string, maxAge int) *http.Cookie {
	return &http.Cookie{
		Name:     refreshCookie,
		Value:    value,
		Path:     refreshPath,
		MaxAge:   maxAge,
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   isHTTPS(r),
	}
}

// keepRefresh sets the refresh cookie from a token answer for the subject
// of the sign-in. An answer without a refresh token deletes the cookie, so
// a sign-in never renews as the person of an older one. The cookie lives
// as long as the refresh token when the issuer says how long, else until
// until (Unix seconds) when it is not 0, else as long as the browser
// session.
func (s *server) keepRefresh(w http.ResponseWriter, r *http.Request, tok tokenAnswer, subject string, until int64) {
	si := s.signin
	if tok.RefreshToken == "" || subject == "" {
		http.SetCookie(w, si.refreshCookieOf(r, "", -1))
		return
	}
	st := refreshState{Token: tok.RefreshToken, Subject: subject, Expires: until}
	life := firstPositive(mcpclient.ExpiresIn(tok.RefreshExpiresIn), mcpclient.ExpiresIn(tok.RefreshTokenExpiresIn))
	switch {
	case life > 0:
		st.Expires = si.now().Unix() + life
	case until > 0:
		life = max(until-si.now().Unix(), 1)
	}
	v := si.sealRefresh(st)
	if len(v) > maxCookieBytes {
		s.a.Logger().Warn("native: the refresh token of the issuer is too long for a cookie, so the sign-in ends with its token", "bytes", len(tok.RefreshToken))
		http.SetCookie(w, si.refreshCookieOf(r, "", -1))
		return
	}
	http.SetCookie(w, si.refreshCookieOf(r, v, int(life)))
}

// firstPositive returns the first value above 0, or 0.
func firstPositive(vs ...int64) int64 {
	for _, v := range vs {
		if v > 0 {
			return v
		}
	}
	return 0
}

// fromThisPage reports whether a request comes from a page of this origin.
// A browser names the site of a request in Sec-Fetch-Site. An older browser
// that does not names its origin in Origin.
func fromThisPage(r *http.Request) bool {
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" {
		return site == "same-origin"
	}
	if o := r.Header.Get("Origin"); o != "" {
		return originOf(o) == originOf(requestOrigin(r))
	}
	return true
}

// renewed is the answer of a renewal: the new token, and the time when it
// expires when that is known.
type renewed struct {
	Token     string `json:"token"`
	ExpiresAt string `json:"expiresAt,omitempty"`
}

// signinRenew answers POST /ui/refresh. It trades the refresh token of the
// refresh cookie for new tokens at the token endpoint of the issuer, and
// answers the first token that the authentication of the agent accepts
// for the same subject. A rotated refresh token replaces the cookie. When
// the issuer refuses the refresh token, the answer is a 401 with the
// sign-in route, and the cookie goes. When the issuer does not answer, the
// answer is a 502, and the cookie stays for the next try.
func (s *server) signinRenew(w http.ResponseWriter, r *http.Request) {
	si := s.signin
	log := s.a.Logger()
	privatePage(w)
	if !fromThisPage(r) {
		renewRefused(w, r, http.StatusForbidden, "Only a page of this server can renew its sign-in.")
		return
	}
	c, err := r.Cookie(refreshCookie)
	if err != nil || c.Value == "" {
		renewRefused(w, r, http.StatusUnauthorized, "This browser has no sign-in to renew, so sign in again.")
		return
	}
	forget := func() { http.SetCookie(w, si.refreshCookieOf(r, "", -1)) }
	st, err := si.openRefresh(c.Value)
	if err != nil {
		forget()
		if !errors.Is(err, errSigninExpired) {
			log.Warn("native: the renewal of a browser sign-in refused a refresh cookie that does not open")
		}
		renewRefused(w, r, http.StatusUnauthorized, "The sign-in of this browser ended, so sign in again.")
		return
	}
	doc, err := si.discover(r.Context())
	if err != nil {
		log.Warn("native: the renewal of a browser sign-in cannot read the discovery document", "issuer", si.issuer, "error", err)
		renewRefused(w, r, http.StatusBadGateway, "The sign-in service did not answer, so the sign-in was not renewed.")
		return
	}
	tok, err := si.tokenRequest(r.Context(), doc, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {st.Token}})
	if err != nil {
		var te *tokenError
		switch {
		case errors.Is(err, errNoClientSecret):
			log.Error("native: the renewal of a browser sign-in has no client secret", "secret", si.secret, "error", err)
			renewRefused(w, r, http.StatusInternalServerError, "This server cannot read its client secret for the sign-in service.")
		case errors.As(err, &te) && te.status >= 400 && te.status < 500:
			forget()
			log.Info("native: the issuer refused to renew a browser sign-in", "issuer", si.issuer, "subject", st.Subject, "status", te.status, "error", te.code)
			renewRefused(w, r, http.StatusUnauthorized, "The sign-in service ended this sign-in, so sign in again.")
		default:
			log.Warn("native: the renewal of a browser sign-in cannot reach the token endpoint", "issuer", si.issuer, "error", err)
			renewRefused(w, r, http.StatusBadGateway, "The sign-in service did not answer, so the sign-in was not renewed.")
		}
		return
	}
	token, kind, p := s.acceptedToken(r, tok)
	switch {
	case token == "":
		forget()
		log.Warn("native: the authentication of the agent accepts no token of a renewed browser sign-in", "issuer", si.issuer,
			"accessToken", tok.AccessToken != "", "idToken", tok.IDToken != "")
		renewRefused(w, r, http.StatusUnauthorized, "This server does not accept the renewed token, so sign in again.")
		return
	case p.Subject != st.Subject:
		forget()
		log.Warn("native: a renewed browser sign-in names another subject", "issuer", si.issuer, "subject", st.Subject, "renewed", p.Subject)
		renewRefused(w, r, http.StatusUnauthorized, "The renewed sign-in names another person, so sign in again.")
		return
	}
	if tok.RefreshToken != "" && tok.RefreshToken != st.Token {
		s.keepRefresh(w, r, tok, st.Subject, st.Expires)
	}
	log.Debug("native: browser sign-in renewed", "subject", p.Subject, "token", kind)
	ans := renewed{Token: token}
	if at, ok := si.expiryOf(token, kind, tok); ok {
		ans.ExpiresAt = at.UTC().Format(time.RFC3339)
	}
	writeJSON(w, http.StatusOK, ans)
}

// signinForget answers DELETE /ui/refresh. It deletes the refresh cookie,
// so a sign-out in the page ends the renewals too.
func (s *server) signinForget(w http.ResponseWriter, r *http.Request) {
	privatePage(w)
	if !fromThisPage(r) {
		renewRefused(w, r, http.StatusForbidden, "Only a page of this server can end its sign-in.")
		return
	}
	http.SetCookie(w, s.signin.refreshCookieOf(r, "", -1))
	w.WriteHeader(http.StatusNoContent)
}

// renewRefused writes the problem document of a renewal that failed. A 401
// names the sign-in route, as the 401 of the API does.
func renewRefused(w http.ResponseWriter, r *http.Request, status int, detail string) {
	p := native.Problem{Type: "about:blank", Title: http.StatusText(status), Status: status, Detail: detail, Instance: r.URL.Path}
	if status == http.StatusUnauthorized {
		p.Signin = signinPath
	}
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(p)
}

// expiryOf returns when a token expires: the exp claim of a JWT, else
// expires_in of the answer for an access token.
func (si *signin) expiryOf(token, kind string, tok tokenAnswer) (time.Time, bool) {
	if parts := strings.Split(token, "."); len(parts) == 3 {
		if raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "=")); err == nil {
			var c struct {
				Exp json.Number `json:"exp"`
			}
			if json.Unmarshal(raw, &c) == nil {
				if v, err := c.Exp.Float64(); err == nil && v > 0 {
					return time.Unix(int64(v), 0), true
				}
			}
		}
	}
	if kind == "access" {
		if secs := mcpclient.ExpiresIn(tok.ExpiresIn); secs > 0 {
			return si.now().Add(time.Duration(secs) * time.Second), true
		}
	}
	return time.Time{}, false
}
