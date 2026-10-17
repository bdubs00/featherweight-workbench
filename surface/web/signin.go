package web

// This file serves the browser sign-in of the dashboard: the authorization
// code flow of OpenID Connect with PKCE (RFC 7636) against the issuer of
// auth.oidc. GET /ui/signin sends the person to the issuer. GET
// /ui/callback exchanges the code, keeps a token that the authentication of
// the agent accepts, and hands it to the page in sessionStorage. The
// setting auth.oidc.clientId turns the routes on. Pasting a token stays as
// the path for a developer.
//
// The state of one sign-in lives in a cookie that this process signs: the
// PKCE verifier, the state, the nonce, the fragment of the page to return
// to, and the redirect URI. The cookie lasts 10 minutes, is HttpOnly and
// SameSite=Lax, has the path /ui, and is Secure on https. No token, code,
// verifier, or secret enters a log line or an error message.

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/dashboard"
	"github.com/bdubs00/featherweight-workbench/fleet"
)

// Routes, the cookie, and the limits of the browser sign-in.
const (
	signinPath    = "/ui/signin"
	callbackPath  = "/ui/callback"
	signinCookie  = "fw_signin"
	signinHop     = "hop"            // the query parameter that marks a sign-in that moved to the host of the callback
	signinTTL     = 10 * time.Minute // the life of one sign-in
	discoveryTTL  = time.Hour        // the life of a cached discovery document
	issuerTimeout = 15 * time.Second // one request to the issuer
	maxNextBytes  = 1024
	maxIssuerDoc  = 1 << 20 // the largest discovery document or token answer
	tokenStorage  = "fw.token"
)

// signin is the browser sign-in of one agent.
type signin struct {
	issuer    string
	clientID  string
	secret    string // the Secrets name of the client secret, "" for a public client
	redirect  string // auth.oidc.redirectUrl, or ""
	page      string // dashboard.PageURL, or ""
	scopes    []string
	insecure  bool
	client    *http.Client
	secrets   fw.Secrets
	key       []byte // signs the cookie
	keySource string // where key comes from: a secret name, keyFromFile, or keyInMemory
	now       func() time.Time

	mu      sync.Mutex
	doc     *discoveryDoc
	expires time.Time
}

// discoveryDoc holds the fields of the discovery document that the sign-in
// reads.
type discoveryDoc struct {
	Issuer                string   `json:"issuer"`
	AuthorizationEndpoint string   `json:"authorization_endpoint"`
	TokenEndpoint         string   `json:"token_endpoint"`
	AuthMethods           []string `json:"token_endpoint_auth_methods_supported"`
}

// signinState is the content of the sign-in cookie.
type signinState struct {
	Verifier string `json:"v"`
	State    string `json:"s"`
	Nonce    string `json:"n"`
	Next     string `json:"x,omitempty"`
	Redirect string `json:"r"`
	Expires  int64  `json:"e"` // Unix seconds
}

// newSignin builds the sign-in of an agent, or returns nil when
// auth.oidc.clientId is empty. It refuses a configuration that cannot work.
// cookieKey says where the key of the cookie comes from.
func newSignin(a *fw.Agent, now func() time.Time) (*signin, error) {
	cfg := a.Config()
	if cfg == nil || cfg.Auth.OIDC == nil || strings.TrimSpace(cfg.Auth.OIDC.ClientID) == "" {
		return nil, nil
	}
	oc := cfg.Auth.OIDC
	si := &signin{
		issuer:   strings.TrimSpace(oc.Issuer),
		clientID: strings.TrimSpace(oc.ClientID),
		secret:   strings.TrimSpace(oc.ClientSecret),
		redirect: strings.TrimSpace(oc.RedirectURL),
		page:     dashboard.PageURL(cfg),
		insecure: oc.Insecure,
		secrets:  a.Secrets(),
		now:      now,
	}
	if si.issuer == "" {
		return nil, errors.New("native: auth.oidc.clientId turns on browser sign-in, which needs auth.oidc.issuer; set it, or set FW_AUTH_OIDC_ISSUER")
	}
	if err := checkIssuerURL("auth.oidc.issuer", si.issuer, si.insecure); err != nil {
		return nil, err
	}
	if si.redirect != "" {
		u, err := url.Parse(si.redirect)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.Fragment != "" {
			return nil, fmt.Errorf("native: auth.oidc.redirectUrl %q is not an absolute http or https URL", si.redirect)
		}
	}
	si.scopes = []string{"openid"}
	for _, sc := range oc.Scopes {
		if sc = strings.TrimSpace(sc); sc != "" && !slices.Contains(si.scopes, sc) {
			si.scopes = append(si.scopes, sc)
		}
	}
	client, err := issuerClient(oc.CA)
	if err != nil {
		return nil, err
	}
	si.client = client
	si.key, si.keySource = cookieKey(a, si.secret)
	a.Logger().Info("native: browser sign-in is on", "issuer", si.issuer, "clientId", si.clientID, "confidential", si.secret != "", "cookieKey", si.keySource)
	if si.keySource == keyFromFile && likelyReplicas(a) {
		// Each replica has its own home, and so its own key file. A
		// callback that reaches another replica then fails.
		a.Logger().Warn("native: the key of the sign-in cookie comes from a file of this process, and this agent likely has replicas with files of their own; set "+sharedKeySecret+" on every replica, or set auth.oidc.clientSecret",
			"file", filepath.Join(homeOf(a), signinKeyFile), "fleet", fleet.Of(a) != nil, "ledger", a.Config().Ledger.Driver, "bus", a.Config().Bus.Driver)
	}
	return si, nil
}

// likelyReplicas reports whether the agent likely runs as one of several
// processes: it has a fleet, or it uses a ledger or a bus that processes
// share.
func likelyReplicas(a *fw.Agent) bool {
	if fleet.Of(a) != nil {
		return true
	}
	cfg := a.Config()
	switch cfg.Ledger.Driver {
	case "", "mem", "file":
	default:
		return true
	}
	return cfg.Bus.Driver != "" && cfg.Bus.Driver != "mem"
}

// homeOf is the home directory of the agent.
func homeOf(a *fw.Agent) string {
	if home := a.Config().Home; home != "" {
		return home
	}
	return fw.DefaultHome()
}

// Sources of the cookie key.
const (
	sharedKeySecret = "FW_MCP_STATE_KEY" // the signing secret that replicas share
	signinKeyFile   = "signin.key"       // the key file under the home directory
	signinKeyBytes  = 32
	keyFromFile     = "file"
	keyInMemory     = "memory"
	// signinKeyLabel is the purpose of the key. The key is an HMAC of its
	// source with this label, so it never equals the MCP state key, which
	// is a plain hash of the same secret.
	signinKeyLabel = "fw native sign-in cookie v1"
)

// cookieKey returns the key that signs the sign-in cookie, and its source.
// The sources come in this order:
//
//  1. The secret FW_MCP_STATE_KEY, which replicas behind one address share.
//  2. The client secret, which every replica of one client has.
//  3. The random key in the file signin.key under the home directory,
//     which cookieKey creates with mode 0600 when it is missing. Processes
//     that share the home directory share the key.
//  4. A random key in memory, with one warning. A sign-in then returns to
//     the process where it started.
//
// Log lines name the source and the cause, never a key or a secret.
func cookieKey(a *fw.Agent, clientSecret string) ([]byte, string) {
	derive := func(source []byte) []byte {
		m := hmac.New(sha256.New, source)
		m.Write([]byte(signinKeyLabel))
		return m.Sum(nil)
	}
	if sec := a.Secrets(); sec != nil {
		for _, name := range []string{sharedKeySecret, clientSecret} {
			if name == "" {
				continue
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			v, err := sec.Get(ctx, name)
			cancel()
			if err == nil && v != "" {
				return derive([]byte(v)), name
			}
		}
	}
	path := filepath.Join(homeOf(a), signinKeyFile)
	key, err := loadKeyFile(path)
	if err == nil {
		return derive(key), keyFromFile
	}
	a.Logger().Warn("native: the key of the sign-in cookie lives in memory only, so a sign-in fails when its callback reaches another process; set "+sharedKeySecret+" on every replica",
		"file", path, "error", err)
	return derive(randomKey()), keyInMemory
}

// loadKeyFile reads the key in path, or creates it with mode 0600 when the
// file does not exist. Two processes that create it at once agree on one
// key: the file appears whole through a hard link, and the process that
// loses reads the key of the other.
func loadKeyFile(path string) ([]byte, error) {
	key, err := readKeyFile(path)
	if !errors.Is(err, fs.ErrNotExist) {
		return key, err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("native: create the directory of the sign-in key: %w", err)
	}
	tmp, err := os.CreateTemp(dir, "."+signinKeyFile+"-*")
	if err != nil {
		return nil, fmt.Errorf("native: create the sign-in key: %w", err)
	}
	defer os.Remove(tmp.Name())
	key = randomKey()
	werr := tmp.Chmod(0o600)
	if werr == nil {
		_, werr = tmp.Write(key)
	}
	if werr == nil {
		werr = tmp.Sync()
	}
	if cerr := tmp.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		return nil, fmt.Errorf("native: write the sign-in key: %w", werr)
	}
	switch err := os.Link(tmp.Name(), path); {
	case err == nil:
		return key, nil
	case errors.Is(err, fs.ErrExist):
		return readKeyFile(path) // another process created it first
	}
	// A file system without hard links gets a rename, which replaces a
	// file that another process created in the same instant.
	if err := os.Rename(tmp.Name(), path); err != nil {
		return nil, fmt.Errorf("native: write the sign-in key: %w", err)
	}
	return key, nil
}

// readKeyFile reads a key file. A file of another length is an error, so a
// damaged key never signs.
func readKeyFile(path string) ([]byte, error) {
	key, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
		return nil, fmt.Errorf("native: read the sign-in key: %w", err)
	}
	if len(key) != signinKeyBytes {
		return nil, fmt.Errorf("native: the sign-in key file holds %d bytes, not %d; remove it to make a new key", len(key), signinKeyBytes)
	}
	return key, nil
}

// randomKey returns signinKeyBytes random bytes.
func randomKey() []byte {
	key := make([]byte, signinKeyBytes)
	_, _ = rand.Read(key)
	return key
}

// checkIssuerURL refuses a URL that is not absolute, or that uses http
// without auth.oidc.insecure.
func checkIssuerURL(key, raw string, insecure bool) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return fmt.Errorf("native: %s %q is not an https URL", key, raw)
	}
	if u.Scheme == "http" && !insecure {
		return fmt.Errorf("native: %s %q uses http; use https, or set auth.oidc.insecure for a test", key, raw)
	}
	return nil
}

// issuerClient is the HTTP client for the issuer. ca is empty, the path of
// a PEM bundle, or the PEM text itself, as auth.oidc.ca says.
func issuerClient(ca string) (*http.Client, error) {
	tc := &tls.Config{MinVersion: tls.VersionTLS12}
	if ca = strings.TrimSpace(ca); ca != "" {
		pem := []byte(ca)
		if !strings.HasPrefix(ca, "-----BEGIN") {
			b, err := os.ReadFile(ca)
			if err != nil {
				return nil, fmt.Errorf("native: read auth.oidc.ca: %w", err)
			}
			pem = b
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, errors.New("native: auth.oidc.ca holds no certificate")
		}
		tc.RootCAs = pool
	}
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	return &http.Client{
		Timeout: issuerTimeout,
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           dialer.DialContext,
			TLSClientConfig:       tc,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 10 * time.Second,
			IdleConnTimeout:       90 * time.Second,
			MaxIdleConns:          4,
		},
		// The answers of the issuer come as they are.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}, nil
}

// discover returns the discovery document of the issuer. A document stays
// in the cache for discoveryTTL. A failure is not cached.
func (si *signin) discover(ctx context.Context) (*discoveryDoc, error) {
	si.mu.Lock()
	if si.doc != nil && si.now().Before(si.expires) {
		d := si.doc
		si.mu.Unlock()
		return d, nil
	}
	si.mu.Unlock()
	where := strings.TrimSuffix(si.issuer, "/") + "/.well-known/openid-configuration"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, where, nil)
	if err != nil {
		return nil, fmt.Errorf("native: discovery at %s: %w", si.issuer, err)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := si.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("native: discovery at %s: %w", si.issuer, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("native: discovery at %s: status %d", si.issuer, resp.StatusCode)
	}
	var d discoveryDoc
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxIssuerDoc)).Decode(&d); err != nil {
		return nil, fmt.Errorf("native: discovery at %s: the document is not JSON: %w", si.issuer, err)
	}
	if strings.TrimSuffix(d.Issuer, "/") != strings.TrimSuffix(si.issuer, "/") {
		return nil, fmt.Errorf("native: discovery at %s: the document names the issuer %q", si.issuer, d.Issuer)
	}
	if err := checkIssuerURL("authorization_endpoint", d.AuthorizationEndpoint, si.insecure); err != nil {
		return nil, fmt.Errorf("native: discovery at %s: %w", si.issuer, err)
	}
	if err := checkIssuerURL("token_endpoint", d.TokenEndpoint, si.insecure); err != nil {
		return nil, fmt.Errorf("native: discovery at %s: %w", si.issuer, err)
	}
	si.mu.Lock()
	si.doc, si.expires = &d, si.now().Add(discoveryTTL)
	si.mu.Unlock()
	return &d, nil
}

// redirectURI is the callback address that the issuer sends the person
// back to: auth.oidc.redirectUrl, else /ui/callback on the public address
// of the page, else /ui/callback on the origin of the request.
func (si *signin) redirectURI(r *http.Request) string {
	switch {
	case si.redirect != "":
		return si.redirect
	case si.page != "":
		return si.page + "/callback"
	}
	return requestOrigin(r) + callbackPath
}

// originOf is the scheme and the host of an absolute URL in lower case,
// without the default port of the scheme. It returns "" for another value.
func originOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	scheme, host := strings.ToLower(u.Scheme), strings.ToLower(u.Host)
	if h, port, err := net.SplitHostPort(host); err == nil && ((scheme == "https" && port == "443") || (scheme == "http" && port == "80")) {
		host = h
		if strings.Contains(h, ":") {
			host = "[" + h + "]"
		}
	}
	return scheme + "://" + host
}

// signinOn is the address of the sign-in route on the host of the redirect
// URI, with next and the mark of the move. The path is the path of the
// redirect URI with "signin" for its last element "callback", so a proxy
// that serves the page under a prefix keeps it. Another path gives
// /ui/signin. It returns "" for a redirect URI that is not absolute.
func signinOn(redirect, next string) string {
	u, err := url.Parse(redirect)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	target := url.URL{Scheme: u.Scheme, Host: u.Host, Path: signinPath}
	if prefix, ok := strings.CutSuffix(u.Path, "/callback"); ok {
		target.Path = prefix + "/signin"
	}
	q := url.Values{signinHop: {"1"}}
	if next != "" {
		q.Set("next", next)
	}
	target.RawQuery = q.Encode()
	return target.String()
}

// signinDomain is the purpose of the MAC of a sign-in cookie.
const signinDomain = "fw.signin.v1"

// seal signs the state of a sign-in for the cookie.
func (si *signin) seal(st signinState) string {
	return sealer{key: si.key, domain: signinDomain}.seal(st)
}

// Errors of the sign-in cookie.
var (
	errSigninForged  = errors.New("native: the sign-in cookie has a bad signature")
	errSigninExpired = errors.New("native: the sign-in cookie has expired")
)

// open verifies the sign-in cookie and returns its state.
func (si *signin) open(v string) (signinState, error) {
	var st signinState
	if err := (sealer{key: si.key, domain: signinDomain}).open(v, &st); err != nil || st.State == "" || st.Verifier == "" || st.Redirect == "" {
		return signinState{}, errSigninForged
	}
	if si.now().Unix() > st.Expires {
		return signinState{}, errSigninExpired
	}
	return st, nil
}

// sealer signs a JSON value for a cookie with HMAC-SHA256. The domain
// separates the purposes of one key, so a cookie of one purpose never
// opens as a cookie of another.
type sealer struct {
	key    []byte
	domain string
}

// errSealForged means that a sealed value has a bad signature or body.
var errSealForged = errors.New("native: the sealed value has a bad signature")

func (z sealer) mac(body string) []byte {
	m := hmac.New(sha256.New, z.key)
	m.Write([]byte(z.domain + "\n"))
	m.Write([]byte(body))
	return m.Sum(nil)
}

// seal returns the value in JSON, in base64url, with its signature.
func (z sealer) seal(v any) string {
	b, _ := json.Marshal(v)
	body := base64.RawURLEncoding.EncodeToString(b)
	return body + "." + base64.RawURLEncoding.EncodeToString(z.mac(body))
}

// open verifies a sealed value and decodes it into v.
func (z sealer) open(sealed string, v any) error {
	body, sig, ok := strings.Cut(sealed, ".")
	if !ok {
		return errSealForged
	}
	got, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil || !hmac.Equal(got, z.mac(body)) {
		return errSealForged
	}
	raw, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil {
		return errSealForged
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return errSealForged
	}
	return nil
}

// cookie is the sign-in cookie with a value and a life in seconds. A
// negative life deletes the cookie.
func (si *signin) cookie(r *http.Request, value string, maxAge int) *http.Cookie {
	return &http.Cookie{
		Name:     signinCookie,
		Value:    value,
		Path:     "/ui",
		MaxAge:   maxAge,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   isHTTPS(r),
	}
}

// isHTTPS reports whether the request came over https, directly or through
// a proxy that says so in X-Forwarded-Proto, as requestOrigin trusts it.
func isHTTPS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	return strings.EqualFold(firstForwarded(r, "X-Forwarded-Proto"), "https")
}

// cleanNext returns the fragment that the page returns to after the
// sign-in: a value that starts with "#", has no control character, and
// fits maxNextBytes. Any other value becomes "".
func cleanNext(v string) string {
	if !strings.HasPrefix(v, "#") || len(v) > maxNextBytes || !utf8.ValidString(v) {
		return ""
	}
	for _, r := range v {
		if unicode.IsControl(r) {
			return ""
		}
	}
	return v
}

// randomString is 32 random bytes in base64url: a PKCE verifier (43
// characters, as RFC 7636 asks), a state, or a nonce.
func randomString() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// pkceChallenge is the S256 challenge of a verifier.
func pkceChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// signinStart answers GET /ui/signin?next=<fragment>. It sends the person
// to the authorization endpoint of the issuer with a new state, nonce, and
// PKCE challenge, and keeps them in the sign-in cookie. A request on
// another host than the redirect URI goes to this route on that host
// first, once.
func (s *server) signinStart(w http.ResponseWriter, r *http.Request) {
	si := s.signin
	privatePage(w)
	next := cleanNext(r.URL.Query().Get("next"))
	redirect := si.redirectURI(r)
	// The cookie belongs to the host that serves this route, and the
	// callback reads it on the host of the redirect URI. A sign-in that
	// starts on another host, such as through a port-forward or an
	// internal name, moves to this route on the host of the callback
	// first. The mark of the move stops a second move: a proxy that hides
	// the host of the browser makes the origins differ again, and the
	// browser is on the right host then.
	if r.URL.Query().Get(signinHop) == "" && originOf(redirect) != originOf(requestOrigin(r)) {
		if target := signinOn(redirect, next); target != "" {
			w.Header().Set("Location", target)
			w.WriteHeader(http.StatusFound)
			return
		}
	}
	doc, err := si.discover(r.Context())
	if err != nil {
		s.a.Logger().Warn("native: browser sign-in cannot read the discovery document", "issuer", si.issuer, "error", err)
		signinFailed(w, http.StatusBadGateway, "The sign-in service did not answer, so the sign-in cannot start.")
		return
	}
	st := signinState{
		Verifier: randomString(),
		State:    randomString(),
		Nonce:    randomString(),
		Next:     next,
		Redirect: redirect,
		Expires:  si.now().Add(signinTTL).Unix(),
	}
	target, err := url.Parse(doc.AuthorizationEndpoint)
	if err != nil {
		signinFailed(w, http.StatusBadGateway, "The sign-in service has an authorization address that is not valid.")
		return
	}
	q := target.Query()
	q.Set("response_type", "code")
	q.Set("client_id", si.clientID)
	q.Set("redirect_uri", st.Redirect)
	q.Set("scope", strings.Join(si.scopes, " "))
	q.Set("state", st.State)
	q.Set("nonce", st.Nonce)
	q.Set("code_challenge", pkceChallenge(st.Verifier))
	q.Set("code_challenge_method", "S256")
	target.RawQuery = q.Encode()
	http.SetCookie(w, si.cookie(r, si.seal(st), int(signinTTL/time.Second)))
	w.Header().Set("Location", target.String())
	w.WriteHeader(http.StatusFound)
}

// oauthErrorCode matches an error code of the issuer that a page can show.
var oauthErrorCode = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)

// signinCallback answers GET /ui/callback. It checks the state against the
// cookie, exchanges the code at the token endpoint, and keeps the access
// token when the authentication of the agent accepts it, else the ID
// token when it accepts that. The answer is a small page that stores the
// token in sessionStorage and returns to /ui with the fragment of the
// sign-in. Every answer deletes the cookie.
func (s *server) signinCallback(w http.ResponseWriter, r *http.Request) {
	si := s.signin
	log := s.a.Logger()
	privatePage(w)
	http.SetCookie(w, si.cookie(r, "", -1))
	q := r.URL.Query()
	c, err := r.Cookie(signinCookie)
	if err != nil {
		signinFailed(w, http.StatusBadRequest, "This browser has no sign-in in progress, so start the sign-in again.")
		return
	}
	st, err := si.open(c.Value)
	switch {
	case errors.Is(err, errSigninExpired):
		signinFailed(w, http.StatusBadRequest, "The sign-in took longer than 10 minutes, so start it again.")
		return
	case err != nil:
		log.Warn("native: browser sign-in refused a cookie with a bad signature")
		signinFailed(w, http.StatusBadRequest, "The sign-in state in this browser is not valid, so start the sign-in again.")
		return
	}
	if subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(st.State)) != 1 {
		log.Warn("native: browser sign-in refused an answer with another state")
		signinFailed(w, http.StatusBadRequest, "The answer of the sign-in service does not belong to this sign-in.")
		return
	}
	if e := q.Get("error"); e != "" {
		if !oauthErrorCode.MatchString(e) {
			e = "an error"
		}
		log.Info("native: the issuer refused the browser sign-in", "issuer", si.issuer, "error", e)
		signinFailed(w, http.StatusForbidden, "The sign-in service refused the sign-in with "+e+".")
		return
	}
	code := q.Get("code")
	if code == "" {
		signinFailed(w, http.StatusBadRequest, "The answer of the sign-in service has no authorization code.")
		return
	}
	doc, err := si.discover(r.Context())
	if err != nil {
		log.Warn("native: browser sign-in cannot read the discovery document", "issuer", si.issuer, "error", err)
		signinFailed(w, http.StatusBadGateway, "The sign-in service did not answer.")
		return
	}
	tok, err := si.exchange(r.Context(), doc, code, st)
	if err != nil {
		var te *tokenError
		switch {
		case errors.Is(err, errNoClientSecret):
			log.Error("native: browser sign-in has no client secret", "secret", si.secret, "error", err)
			signinFailed(w, http.StatusInternalServerError, "This server cannot read its client secret for the sign-in service.")
		case errors.As(err, &te):
			log.Warn("native: the token endpoint refused the code of a browser sign-in", "issuer", si.issuer, "status", te.status, "error", te.code)
			signinFailed(w, http.StatusBadGateway, "The sign-in service did not accept the authorization code.")
		default:
			log.Warn("native: browser sign-in cannot reach the token endpoint", "issuer", si.issuer, "error", err)
			signinFailed(w, http.StatusBadGateway, "The sign-in service did not answer the exchange of the code.")
		}
		return
	}
	if tok.IDToken != "" {
		nonce, ok := idTokenNonce(tok.IDToken)
		if !ok || subtle.ConstantTimeCompare([]byte(nonce), []byte(st.Nonce)) != 1 {
			log.Warn("native: browser sign-in refused an ID token with another nonce", "issuer", si.issuer)
			signinFailed(w, http.StatusBadRequest, "The ID token of the sign-in service does not belong to this sign-in.")
			return
		}
	}
	token, kind, p := s.acceptedToken(r, tok)
	if token == "" {
		log.Warn("native: the authentication of the agent accepts no token of the browser sign-in", "issuer", si.issuer,
			"accessToken", tok.AccessToken != "", "idToken", tok.IDToken != "")
		signinFailed(w, http.StatusForbidden, "This server does not accept the token of the sign-in service.")
		return
	}
	log.Info("native: browser sign-in succeeded", "subject", p.Subject, "token", kind)
	signinDone(w, token, st.Next)
}

// tokenAnswer is the answer of the token endpoint.
type tokenAnswer struct {
	AccessToken string `json:"access_token"`
	IDToken     string `json:"id_token"`
	Error       string `json:"error"`
}

// tokenError is a refusal of the token endpoint: the status and the OAuth
// error code, never the body.
type tokenError struct {
	status int
	code   string
}

func (e *tokenError) Error() string {
	return fmt.Sprintf("native: the token endpoint answered %d %s", e.status, e.code)
}

// errNoClientSecret means that the client secret is not readable.
var errNoClientSecret = errors.New("native: the client secret is not readable")

// exchange trades the code for tokens (RFC 6749 section 4.1.3) with the
// PKCE verifier. A confidential client authenticates with HTTP Basic, or
// with client_secret in the form when the issuer lists only
// client_secret_post. A public client sends client_id in the form.
func (si *signin) exchange(ctx context.Context, doc *discoveryDoc, code string, st signinState) (tokenAnswer, error) {
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {st.Redirect},
		"code_verifier": {st.Verifier},
	}
	var secret string
	if si.secret != "" {
		if si.secrets == nil {
			return tokenAnswer{}, errNoClientSecret
		}
		v, err := si.secrets.Get(ctx, si.secret)
		if err != nil {
			return tokenAnswer{}, fmt.Errorf("%w: %w", errNoClientSecret, err)
		}
		if v == "" {
			return tokenAnswer{}, errNoClientSecret
		}
		secret = v
	}
	basic := secret != "" && (len(doc.AuthMethods) == 0 || slices.Contains(doc.AuthMethods, "client_secret_basic") || !slices.Contains(doc.AuthMethods, "client_secret_post"))
	switch {
	case basic:
	case secret != "":
		form.Set("client_id", si.clientID)
		form.Set("client_secret", secret)
	default:
		form.Set("client_id", si.clientID)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, doc.TokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return tokenAnswer{}, fmt.Errorf("native: build the token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	if basic {
		// RFC 6749 section 2.3.1 encodes both parts before Basic.
		req.SetBasicAuth(url.QueryEscape(si.clientID), url.QueryEscape(secret))
	}
	resp, err := si.client.Do(req)
	if err != nil {
		return tokenAnswer{}, fmt.Errorf("native: the token endpoint did not answer: %w", err)
	}
	defer resp.Body.Close()
	var ans tokenAnswer
	derr := json.NewDecoder(io.LimitReader(resp.Body, maxIssuerDoc)).Decode(&ans)
	if resp.StatusCode != http.StatusOK {
		c := ans.Error
		if !oauthErrorCode.MatchString(c) {
			c = ""
		}
		return tokenAnswer{}, &tokenError{status: resp.StatusCode, code: c}
	}
	if derr != nil {
		return tokenAnswer{}, &tokenError{status: resp.StatusCode, code: "not_json"}
	}
	if ans.AccessToken == "" && ans.IDToken == "" {
		return tokenAnswer{}, &tokenError{status: resp.StatusCode, code: "no_token"}
	}
	return ans, nil
}

// idTokenNonce reads the nonce claim of an ID token. The signature is the
// job of the authentication of the agent, so the nonce check only binds
// the token to this sign-in.
func idTokenNonce(token string) (string, bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return "", false
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return "", false
	}
	var claims struct {
		Nonce string `json:"nonce"`
	}
	if json.Unmarshal(raw, &claims) != nil || claims.Nonce == "" {
		return "", false
	}
	return claims.Nonce, true
}

// acceptedToken returns the first token of the answer that the
// authentication of the agent accepts, the access token before the ID
// token, with its kind and the principal. A token that makes an anonymous
// principal signs nobody in. It returns "" when no token passes.
func (s *server) acceptedToken(r *http.Request, tok tokenAnswer) (string, string, *fw.Principal) {
	for _, t := range []struct{ token, kind string }{{tok.AccessToken, "access"}, {tok.IDToken, "id"}} {
		if t.token == "" {
			continue
		}
		probe, err := http.NewRequestWithContext(r.Context(), http.MethodGet, "/v1/whoami", nil)
		if err != nil {
			continue
		}
		probe.Host = r.Host
		probe.RemoteAddr = r.RemoteAddr
		probe.Header.Set("Authorization", "Bearer "+t.token)
		p, err := s.a.Authn().Authenticate(probe)
		if err == nil && p != nil && p.Kind != fw.KindAnonymous {
			return t.token, t.kind, p
		}
	}
	return "", "", nil
}

// privatePage sets the headers of a sign-in answer: no cache, no referrer,
// and no type sniffing.
func privatePage(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Cache-Control", "no-store")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Content-Type-Options", "nosniff")
}

// signinDone writes the page that ends a sign-in. Its one script carries a
// nonce of the policy. It stores the token for the page and replaces the
// location with /ui and the fragment, so the callback leaves the history.
func signinDone(w http.ResponseWriter, token, next string) {
	nonce := randomString()
	tokJS, _ := json.Marshal(token)
	nextJS, _ := json.Marshal(next)
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Content-Security-Policy", "default-src 'none'; script-src 'nonce-"+nonce+"'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, `<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><title>Signing in</title></head>
<body><p id="msg">Signing in.</p><noscript><p>The sign-in needs JavaScript. <a href="/ui">Open the dashboard</a>.</p></noscript>
<script nonce="%s">
try { sessionStorage.setItem(%q, %s); location.replace("/ui" + %s); }
catch (e) { document.getElementById("msg").textContent = "This browser does not keep the sign-in. Allow storage for this site and sign in again."; }
</script></body></html>
`, nonce, tokenStorage, tokJS, nextJS)
}

// signinFailed writes the page of a failed sign-in: one sentence that
// says what failed and a link back to /ui.
func signinFailed(w http.ResponseWriter, status int, sentence string) {
	writePage(w, status, "Sign-in failed", sentence, "/ui", "Back to the dashboard")
}

// writePage writes a small page with no script: a title, one sentence,
// and one link.
func writePage(w http.ResponseWriter, status int, title, sentence, href, link string) {
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Content-Security-Policy", "default-src 'none'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
	w.WriteHeader(status)
	fmt.Fprintf(w, `<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><title>%s</title></head>
<body><p>%s</p><p><a href="%s">%s</a></p></body></html>
`, html.EscapeString(title), html.EscapeString(sentence), html.EscapeString(href), html.EscapeString(link))
}
