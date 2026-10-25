package web

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// signIn runs a sign-in as carol and returns the refresh cookie of the
// callback, or nil when the callback deleted it.
func (e *signinEnv) signIn(t *testing.T) *http.Cookie {
	t.Helper()
	q, cookie := e.start(t, "#/")
	code := e.issuer.authorized(q, "opaque-access", "carol")
	resp, body := e.callback(t, url.Values{"code": {code}, "state": {q.Get("state")}}, cookie)
	checkDone(t, resp, body, idToken("carol", q.Get("nonce")), "#/")
	for _, c := range resp.Cookies() {
		if c.Name == refreshCookie {
			if c.MaxAge < 0 {
				return nil
			}
			return c
		}
	}
	t.Fatalf("callback sets no refresh cookie: %v", resp.Header["Set-Cookie"])
	return nil
}

// renew posts to /ui/refresh with a refresh cookie, as the page does.
func (e *signinEnv) renew(t *testing.T, c *http.Cookie, header ...string) (*http.Response, []byte) {
	t.Helper()
	h := []string{"Sec-Fetch-Site", "same-origin"}
	if c != nil {
		h = append(h, "Cookie", c.Name+"="+c.Value)
	}
	return do(t, "POST", e.url+refreshPath, "", append(h, header...)...)
}

// refreshCookieIn returns the refresh cookie that an answer sets, or nil.
func refreshCookieIn(resp *http.Response) *http.Cookie {
	for _, c := range resp.Cookies() {
		if c.Name == refreshCookie {
			return c
		}
	}
	return nil
}

func TestSigninRenew(t *testing.T) {
	e := newSigninEnv(t, nil)
	e.issuer.scopes = []string{"openid", "email", "offline_access"}
	e.issuer.refresh = "refresh-one"
	e.issuer.refreshLife = 3600

	// An issuer that lists offline_access gets it in the request.
	q, _ := e.start(t, "")
	if q.Get("scope") != "openid fw offline_access" {
		t.Fatalf("scope = %q", q.Get("scope"))
	}
	c := e.signIn(t)
	if c == nil || !c.HttpOnly || c.SameSite != http.SameSiteStrictMode || c.Path != refreshPath || c.MaxAge != 3600 || c.Secure {
		t.Fatalf("refresh cookie = %+v", c)
	}
	if strings.Contains(c.Value, "refresh-one") || strings.Contains(c.Value, "carol") {
		t.Fatal("the refresh cookie holds its token in plain text")
	}

	// The renewal answers a new token with its expiry. A public client
	// sends its id and the refresh token.
	resp, b := e.renew(t, c)
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("renew: %d %s", resp.StatusCode, b)
	}
	var ans renewed
	if err := json.Unmarshal(b, &ans); err != nil || ans.ExpiresAt != "2030-01-01T00:00:00Z" || !strings.Contains(ans.Token, ".") {
		t.Fatalf("renew answer = %s", b)
	}
	if refreshCookieIn(resp) != nil {
		t.Fatalf("a renewal without rotation sets the cookie: %v", resp.Header["Set-Cookie"])
	}
	forms, _ := e.issuer.tokenCalls()
	last := forms[len(forms)-1]
	if last.Get("grant_type") != "refresh_token" || last.Get("refresh_token") != "refresh-one" || last.Get("client_id") != "fw-dashboard" {
		t.Fatalf("refresh request = %v", last)
	}
	resp, b = do(t, "GET", e.url+"/v1/whoami", "", "Authorization", "Bearer "+ans.Token)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("whoami with the renewed token: %d %s", resp.StatusCode, b)
	}

	// A rotated refresh token replaces the cookie, and the old one fails.
	e.issuer.rotate = "refresh-two"
	resp, b = e.renew(t, c)
	next := refreshCookieIn(resp)
	if resp.StatusCode != http.StatusOK || next == nil || next.Value == c.Value || next.MaxAge <= 0 {
		t.Fatalf("rotation: %d %s %v", resp.StatusCode, b, resp.Header["Set-Cookie"])
	}
	resp, b = e.renew(t, c)
	if p := decodeProblem(t, resp, b, http.StatusUnauthorized); p.Signin != "/ui/signin" {
		t.Fatalf("old refresh token: %s", b)
	}
	if gone := refreshCookieIn(resp); gone == nil || gone.MaxAge >= 0 {
		t.Fatalf("a refused refresh token keeps the cookie: %v", resp.Header["Set-Cookie"])
	}
	if resp, b = e.renew(t, next); resp.StatusCode != http.StatusOK {
		t.Fatalf("rotated cookie: %d %s", resp.StatusCode, b)
	}

	// An issuer that does not answer keeps the cookie for the next try.
	e.issuer.renewStatus = http.StatusServiceUnavailable
	resp, b = e.renew(t, next)
	decodeProblem(t, resp, b, http.StatusBadGateway)
	if refreshCookieIn(resp) != nil {
		t.Fatalf("an issuer that is down deletes the cookie: %v", resp.Header["Set-Cookie"])
	}
	e.issuer.renewStatus = 0

	// A renewal that names another person ends the sign-in.
	e.issuer.renewAccess = "access-good"
	resp, b = e.renew(t, next)
	decodeProblem(t, resp, b, http.StatusUnauthorized)
	e.issuer.renewAccess = ""

	// So does a renewed token that the authentication refuses.
	c = e.signIn(t)
	e.issuer.renewAs = "dave"
	resp, b = e.renew(t, c)
	decodeProblem(t, resp, b, http.StatusUnauthorized)
	e.issuer.renewAs = ""

	// The cookie ends with its refresh token.
	c = e.signIn(t)
	e.clock.add(2 * time.Hour)
	resp, b = e.renew(t, c)
	decodeProblem(t, resp, b, http.StatusUnauthorized)

	e.noSecrets(t, "refresh-one", "refresh-two", c.Value, next.Value, ans.Token)
}

func TestSigninRenewRefusals(t *testing.T) {
	e := newSigninEnv(t, nil)
	e.issuer.refresh = "refresh-one"
	c := e.signIn(t)
	if c == nil || c.MaxAge != 0 {
		t.Fatalf("a refresh token without a life makes a cookie of the browser session: %+v", c)
	}

	// No cookie: a 401 that names the sign-in route.
	resp, b := e.renew(t, nil)
	if p := decodeProblem(t, resp, b, http.StatusUnauthorized); p.Signin != "/ui/signin" {
		t.Fatalf("no cookie: %s", b)
	}
	// A cookie that does not open goes.
	resp, b = e.renew(t, &http.Cookie{Name: refreshCookie, Value: c.Value[:len(c.Value)-4] + "AAAA"})
	decodeProblem(t, resp, b, http.StatusUnauthorized)
	if gone := refreshCookieIn(resp); gone == nil || gone.MaxAge >= 0 {
		t.Fatalf("a forged cookie stays: %v", resp.Header["Set-Cookie"])
	}
	// A request of another site is refused, by Sec-Fetch-Site or by Origin.
	resp, b = do(t, "POST", e.url+refreshPath, "", "Sec-Fetch-Site", "cross-site", "Cookie", c.Name+"="+c.Value)
	decodeProblem(t, resp, b, http.StatusForbidden)
	resp, b = do(t, "POST", e.url+refreshPath, "", "Origin", "https://evil.example", "Cookie", c.Name+"="+c.Value)
	decodeProblem(t, resp, b, http.StatusForbidden)
	resp, b = do(t, "POST", e.url+refreshPath, "", "Origin", e.url, "Cookie", c.Name+"="+c.Value)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("same origin without Sec-Fetch-Site: %d %s", resp.StatusCode, b)
	}

	// A sign-out deletes the cookie.
	resp, b = do(t, "DELETE", e.url+refreshPath, "", "Sec-Fetch-Site", "same-origin", "Cookie", c.Name+"="+c.Value)
	if gone := refreshCookieIn(resp); resp.StatusCode != http.StatusNoContent || gone == nil || gone.MaxAge >= 0 || gone.Path != refreshPath {
		t.Fatalf("delete: %d %s %v", resp.StatusCode, b, resp.Header["Set-Cookie"])
	}

	// A sign-in without a refresh token deletes an older cookie, so a
	// browser never renews as the person of an older sign-in.
	e.issuer.refresh = ""
	if c := e.signIn(t); c != nil {
		t.Fatalf("a sign-in without a refresh token keeps a cookie: %+v", c)
	}
	e.noSecrets(t, "refresh-one", c.Value)
}
