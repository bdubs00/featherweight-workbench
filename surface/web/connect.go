package web

// This file serves connections: a person gives the workbench a grant to
// act for them at an outside service, such as an MCP server whose entry
// has auth.oauth.grant: user (the package connections).
//
//   - GET /v1/connections lists the services that act for the caller.
//   - POST /v1/connections/{server} starts a connection. It answers the
//     authorization request that the page sends the browser to, and sets
//     the connect cookie.
//   - DELETE /v1/connections/{server} deletes the grant of the caller.
//   - GET /ui/connect/callback ends a connection. The authorization server
//     sends the browser there with the code.
//
// Only a signed-in person (a principal of kind user) can connect. The
// callback carries no bearer token, because it is a browser navigation, so
// the connect cookie says who started the connection. The cookie holds the
// start of the connection (the state, the PKCE verifier, the nonce, and the
// redirect URI), the subject, the name, and the fragment of the page to
// return to. This process signs it with the key of the sign-in cookie
// under another purpose, so a sign-in cookie never opens as a connect
// cookie. The cookie lasts 10 minutes, is HttpOnly and SameSite=Lax, has
// the path /ui/connect, and is Secure on https. No code, token, state,
// verifier, or secret enters a log line or an error message.

import (
	"context"
	"crypto/subtle"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/surface/native"
	"github.com/bdubs00/featherweight-workbench/connections"
)

// Routes, the cookie, and the limits of a connection.
const (
	connectCallbackPath = "/ui/connect/callback"
	connectCookie       = "fw_connect"
	connectCookiePath   = "/ui/connect"
	connectTTL          = 10 * time.Minute // the life of one connection in progress
	connectDomain       = "fw.connect.v1"  // the purpose of the MAC of a connect cookie
	connectNotPerson    = "Only a signed-in person can connect a service."
)

// connectState is the content of the connect cookie. Start has no URL:
// the browser already went there, and the cookie stays small.
type connectState struct {
	Start   connections.Attempt `json:"c"`
	Subject string              `json:"u"`
	Kind    string              `json:"k"`
	Name    string              `json:"m,omitempty"`
	Issuer  string              `json:"i,omitempty"`
	Next    string              `json:"x,omitempty"`
	Expires int64               `json:"e"` // Unix seconds
}

// Errors of the connect cookie.
var (
	errConnectForged  = errors.New("native: the connect cookie has a bad signature")
	errConnectExpired = errors.New("native: the connect cookie has expired")
)

// connects is the connection flow of one agent. The key comes on first
// use: the key of the browser sign-in when it is on, else cookieKey.
type connects struct {
	s *server

	once sync.Once
	key  []byte
}

func (c *connects) sealer() sealer {
	c.once.Do(func() {
		if si := c.s.signin; si != nil {
			c.key = si.key
			return
		}
		secret := ""
		if oc := c.s.a.Config().Auth.OIDC; oc != nil {
			secret = strings.TrimSpace(oc.ClientSecret)
		}
		c.key, _ = cookieKey(c.s.a, secret)
	})
	return sealer{key: c.key, domain: connectDomain}
}

// seal signs the state of a connection for the cookie.
func (c *connects) seal(st connectState) string { return c.sealer().seal(st) }

// open verifies the connect cookie and returns its state.
func (c *connects) open(v string) (connectState, error) {
	var st connectState
	if err := c.sealer().open(v, &st); err != nil || st.Start.State == "" || st.Start.Verifier == "" ||
		st.Start.Redirect == "" || st.Start.Server == "" || st.Subject == "" || st.Kind != fw.KindUser {
		return connectState{}, errConnectForged
	}
	if c.s.now().Unix() > st.Expires {
		return connectState{}, errConnectExpired
	}
	return st, nil
}

// cookie is the connect cookie with a value and a life in seconds. A
// negative life deletes the cookie.
func (c *connects) cookie(r *http.Request, value string, maxAge int) *http.Cookie {
	return &http.Cookie{
		Name:     connectCookie,
		Value:    value,
		Path:     connectCookiePath,
		MaxAge:   maxAge,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   isHTTPS(r),
	}
}

// redirectURI is the callback address of a connection, beside the
// callback of the browser sign-in: the redirect URI of the sign-in with
// "connect/callback" for its last element "callback", so a proxy that
// serves the page under a prefix keeps it. Without a sign-in redirect URI,
// it is /ui/connect/callback on the public address of the page, else on
// the origin of the request. Every authorization server registers it.
func (c *connects) redirectURI(r *http.Request) string {
	base := ""
	if oc := c.s.a.Config().Auth.OIDC; oc != nil {
		base = strings.TrimSpace(oc.RedirectURL)
	}
	if base != "" {
		if u, err := url.Parse(base); err == nil && u.Scheme != "" && u.Host != "" {
			out := url.URL{Scheme: u.Scheme, Host: u.Host, Path: connectCallbackPath}
			if prefix, ok := strings.CutSuffix(u.Path, "/callback"); ok {
				out.Path = prefix + "/connect/callback"
			}
			return out.String()
		}
	}
	if si := c.s.signin; si != nil && si.page != "" {
		return si.page + "/connect/callback"
	}
	return requestOrigin(r) + connectCallbackPath
}

// derivedRedirect reports whether the redirect URI of a connection comes
// from the origin of the request, so the callback arrives on that origin.
func (c *connects) derivedRedirect() bool {
	if oc := c.s.a.Config().Auth.OIDC; oc != nil && strings.TrimSpace(oc.RedirectURL) != "" {
		return false
	}
	return c.s.signin == nil || c.s.signin.page == ""
}

// listConnections answers GET /v1/connections with every service that
// acts for the caller and its state.
func (s *server) listConnections(w http.ResponseWriter, r *http.Request) {
	conns, err := connections.List(r.Context(), s.a, fw.PrincipalFrom(r.Context()))
	if err != nil {
		s.failConnect(w, r, err)
		return
	}
	if conns == nil {
		conns = []connections.Connection{}
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{"connections": conns})
}

// connectBody is the body of POST /v1/connections/{server}. Next is the
// fragment of the page to return to after the connection, such as
// "#run/<id>". The body is optional.
type connectBody struct {
	Next string `json:"next"`
}

// startConnection answers POST /v1/connections/{server}. It asks the
// service for an authorization request, keeps the start in the connect
// cookie, and answers {"url": <authorization request>}. The page then
// sends the browser to the URL.
func (s *server) startConnection(w http.ResponseWriter, r *http.Request) {
	var body connectBody
	if !s.decodeOptional(w, r, &body) {
		return
	}
	p := fw.PrincipalFrom(r.Context())
	if p == nil || p.Kind != fw.KindUser || strings.TrimSpace(p.Subject) == "" {
		problem(w, r, http.StatusForbidden, connectNotPerson)
		return
	}
	server := r.PathValue("server")
	start, err := connections.Begin(r.Context(), s.a, p, server, s.connects.redirectURI(r))
	if err != nil {
		s.failConnect(w, r, err)
		return
	}
	target, perr := url.Parse(start.URL)
	if perr != nil || (target.Scheme != "https" && target.Scheme != "http") || target.Host == "" {
		s.a.Logger().Error("native: a service gave an authorization request that is not an http URL", "server", server)
		problem(w, r, http.StatusBadGateway, "the service "+server+" gave an authorization address that is not valid")
		return
	}
	if start.Server == "" {
		start.Server = server
	}
	expires := s.now().Add(connectTTL)
	if !start.Expires.IsZero() && start.Expires.Before(expires) {
		expires = start.Expires
	}
	st := connectState{
		Start:   start,
		Subject: p.Subject,
		Kind:    p.Kind,
		Name:    p.Name,
		Issuer:  p.Issuer,
		Next:    cleanNext(body.Next),
		Expires: expires.Unix(),
	}
	st.Start.URL = ""
	http.SetCookie(w, s.connects.cookie(r, s.connects.seal(st), int(connectTTL/time.Second)))
	w.Header().Set("Cache-Control", "no-store")
	s.a.Logger().Info("native: connection started", "subject", p.Subject, "server", start.Server)
	writeJSON(w, http.StatusOK, map[string]string{"url": start.URL})
}

// deleteConnection answers DELETE /v1/connections/{server}: the service
// forgets the grant of the caller.
func (s *server) deleteConnection(w http.ResponseWriter, r *http.Request) {
	p := fw.PrincipalFrom(r.Context())
	if err := connections.Disconnect(r.Context(), s.a, p, r.PathValue("server")); err != nil {
		s.failConnect(w, r, err)
		return
	}
	s.a.Logger().Info("native: connection deleted", "subject", p.Subject, "server", r.PathValue("server"))
	w.WriteHeader(http.StatusNoContent)
}

// connectCallback answers GET /ui/connect/callback. It checks the state
// against the connect cookie, then lets the service exchange the code and
// store the grant. FinishConnect resumes every paused call of the person
// whose service is now connected. The browser then returns to the page:
// to the fragment of the start, else to #connected/<server>. Every answer
// deletes the cookie.
func (s *server) connectCallback(w http.ResponseWriter, r *http.Request) {
	log := s.a.Logger()
	privatePage(w)
	http.SetCookie(w, s.connects.cookie(r, "", -1))
	q := r.URL.Query()
	c, err := r.Cookie(connectCookie)
	if err != nil {
		connectFailed(w, http.StatusBadRequest, "This browser has no connection in progress. Start it again from the dashboard, on the same address that the sign-in service returns to.")
		return
	}
	st, err := s.connects.open(c.Value)
	switch {
	case errors.Is(err, errConnectExpired):
		connectFailed(w, http.StatusBadRequest, "The connection took longer than 10 minutes, so start it again.")
		return
	case err != nil:
		log.Warn("native: connection refused a cookie with a bad signature")
		connectFailed(w, http.StatusBadRequest, "The connection state in this browser is not valid, so start the connection again.")
		return
	}
	if s.connects.derivedRedirect() && originOf(st.Start.Redirect) != originOf(requestOrigin(r)) {
		log.Warn("native: connection callback arrived on another address than its start", "server", st.Start.Server)
		connectFailed(w, http.StatusBadRequest, "This connection started on another address, so start it again from the view.")
		return
	}
	if subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(st.Start.State)) != 1 {
		log.Warn("native: connection refused an answer with another state", "server", st.Start.Server)
		connectFailed(w, http.StatusBadRequest, "The answer of the sign-in service does not belong to this connection.")
		return
	}
	if e := q.Get("error"); e != "" {
		if !oauthErrorCode.MatchString(e) {
			e = "an error"
		}
		log.Info("native: the authorization server refused a connection", "subject", st.Subject, "server", st.Start.Server, "error", e)
		if e == "access_denied" {
			connectFailed(w, http.StatusForbidden, "The connection to "+st.Start.Server+" was not allowed, so nothing changed.")
			return
		}
		connectFailed(w, http.StatusForbidden, "The sign-in service refused the connection to "+st.Start.Server+" with "+e+".")
		return
	}
	code := q.Get("code")
	if code == "" {
		connectFailed(w, http.StatusBadRequest, "The answer of the sign-in service has no authorization code.")
		return
	}
	p := &fw.Principal{Subject: st.Subject, Name: st.Name, Kind: st.Kind, Issuer: st.Issuer}
	// The exchange goes on when the browser leaves, so a grant that the
	// server issued is not lost.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), time.Minute)
	defer cancel()
	if _, err := connections.Finish(ctx, s.a, p, st.Start, code); err != nil {
		log.Warn("native: connection failed", "subject", st.Subject, "server", st.Start.Server, "error", err)
		switch {
		case errors.Is(err, connections.ErrUnknownService):
			connectFailed(w, http.StatusNotFound, "This agent has no service named "+st.Start.Server+" that acts for a person.")
		case errors.Is(err, connections.ErrNotPerson):
			connectFailed(w, http.StatusForbidden, connectNotPerson)
		default:
			connectFailed(w, http.StatusBadGateway, "Connecting "+st.Start.Server+" failed: "+connectCause(err))
		}
		return
	}
	log.Info("native: connection finished", "subject", st.Subject, "server", st.Start.Server)
	next := st.Next
	if next == "" {
		next = "#connected/" + url.PathEscape(st.Start.Server)
	}
	w.Header().Set("Location", "/ui"+next)
	w.WriteHeader(http.StatusSeeOther)
}

// failConnect writes the problem of an error of the connection routes.
func (s *server) failConnect(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, connections.ErrNotPerson):
		problem(w, r, http.StatusForbidden, connectNotPerson)
	case errors.Is(err, connections.ErrUnknownService):
		problem(w, r, http.StatusNotFound, "no service named "+strconv.Quote(r.PathValue("server"))+" acts for a person")
	default:
		s.fail(w, r, err)
	}
}

// connectCause is the error of a connection as a sentence for the person,
// without the package prefixes.
func connectCause(err error) string {
	msg := err.Error()
	for _, prefix := range []string{"fw: ", "mcp: ", "connections: "} {
		msg = strings.TrimPrefix(msg, prefix)
	}
	if msg == "" {
		return "the service did not say why."
	}
	if !strings.HasSuffix(msg, ".") {
		msg += "."
	}
	return strings.ToUpper(msg[:1]) + msg[1:]
}

// connectFailed writes the page of a failed connection: one sentence that
// says what failed and a link to the connections of the view.
func connectFailed(w http.ResponseWriter, status int, sentence string) {
	writePage(w, status, "Connection failed", sentence, "/ui#connections", "Back to your connections")
}

// decodeOptional is decode for a body that can be empty.
func (s *server) decodeOptional(w http.ResponseWriter, r *http.Request, v any) bool {
	if r.Body == nil || r.Body == http.NoBody || r.ContentLength == 0 {
		return true
	}
	r.Body = http.MaxBytesReader(w, r.Body, native.MaxBodyBytes)
	b, err := io.ReadAll(r.Body)
	if err != nil {
		problem(w, r, http.StatusRequestEntityTooLarge, "the body is too large")
		return false
	}
	if strings.TrimSpace(string(b)) == "" {
		return true
	}
	r.Body = io.NopCloser(strings.NewReader(string(b)))
	return s.decode(w, r, v)
}
