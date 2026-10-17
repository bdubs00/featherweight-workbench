package connections

// This file is the Connector of one tools.mcp entry with grant user: the
// authorization request, the exchange of the code, the stored grant, and
// the tokens of each person (docs/ARCHITECTURE.md section 11).

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	mcpclient "github.com/bdubs00/featherweight-kernel/tools/mcp"
)

// connector connects one entry for people.
type connector struct {
	cfg   fw.MCPServerConfig
	u     *userAuth
	store Store
	log   *slog.Logger
	now   func() time.Time
}

var _ Connector = (*connector)(nil)

// newConnector builds the connector of an entry. oidc is auth.oidc, or nil.
func newConnector(c fw.MCPServerConfig, oidc *fw.OIDCConfig, b *broker, store Store, secrets fw.Secrets, log *slog.Logger) (*connector, error) {
	u, err := newUserAuth(c, oidc, b, secrets, log)
	if err != nil {
		return nil, err
	}
	return &connector{cfg: c, u: u, store: store, log: log, now: time.Now}, nil
}

// Name implements Connector.
func (k *connector) Name() string { return k.cfg.Name }

// title is the name of the service for a person.
func (k *connector) title() string {
	if t := strings.TrimSpace(k.cfg.Title); t != "" {
		return t
	}
	return k.cfg.Name
}

// Connection implements Connector. A server whose authorization server
// cannot be found shows as not connected.
func (k *connector) Connection(ctx context.Context, subject string) (Connection, error) {
	c := Connection{Server: k.cfg.Name, Title: k.title(), SignIn: k.u.signIn}
	m, err := k.u.ensureMeta(ctx)
	if err != nil {
		k.log.Warn("connection state unknown", "server", k.cfg.Name, "error", err)
		return c, nil
	}
	c.Issuer = m.issuer
	g, _, err := k.u.broker.load(ctx, k.store, k.u.key(m, subject))
	if err != nil {
		if !errors.Is(err, ErrGrantNotFound) {
			k.log.Warn("grant unreadable", "server", k.cfg.Name, "subject", subject, "error", err)
		}
		return c, nil
	}
	c.Connected, c.Account, c.Since, c.Refreshed = true, g.Account, g.Created, g.Refreshed
	return c, nil
}

// Begin implements Connector: an authorization request with PKCE (RFC
// 7636), the state, a nonce with openid, and the resource of every entry
// that the connection covers (RFC 8707).
func (k *connector) Begin(ctx context.Context, subject, redirect string) (Attempt, error) {
	u := k.u
	if subject == "" {
		return Attempt{}, ErrNotPerson
	}
	if _, err := url.Parse(redirect); err != nil || redirect == "" {
		return Attempt{}, fmt.Errorf("connections: server %s: the redirect address %q is not a URL", k.cfg.Name, redirect)
	}
	m, err := u.ensureMeta(ctx)
	if err != nil {
		return Attempt{}, err
	}
	if m.authorize == "" {
		return Attempt{}, fmt.Errorf("connections: server %s: oauth: the authorization server %s has no authorization_endpoint; set auth.oauth.authorizeUrl", k.cfg.Name, m.issuer)
	}
	state, err1 := randomString(32)
	verifier, err2 := randomString(32)
	nonce, err3 := randomString(16)
	if err := errors.Join(err1, err2, err3); err != nil {
		return Attempt{}, fmt.Errorf("connections: server %s: random values: %w", k.cfg.Name, err)
	}
	scopes := u.authScopes(m)
	q := url.Values{
		"response_type":         {"code"},
		"client_id":             {u.clientID},
		"redirect_uri":          {redirect},
		"state":                 {state},
		"code_challenge":        {pkceChallenge(verifier)},
		"code_challenge_method": {"S256"},
	}
	if len(scopes) > 0 {
		q.Set("scope", strings.Join(scopes, " "))
	}
	at := Attempt{Server: k.cfg.Name, State: state, Verifier: verifier, Redirect: redirect, Expires: k.now().Add(connectTTL)}
	if u.sameIdP(m) {
		q.Set("nonce", nonce)
		at.Nonce = nonce
	}
	for _, r := range u.broker.resourcesFor(u) {
		q.Add("resource", r)
	}
	for key, v := range u.params {
		if !reservedParams[key] {
			q.Set(key, v)
		}
	}
	sep := "?"
	if strings.Contains(m.authorize, "?") {
		sep = "&"
	}
	at.URL = m.authorize + sep + q.Encode()
	return at, nil
}

// Finish implements Connector. It exchanges the code, checks the person
// when the authorization server is the sign-in issuer, and stores the
// grant.
func (k *connector) Finish(ctx context.Context, subject string, at Attempt, code string) (Connection, error) {
	u := k.u
	if subject == "" {
		return Connection{}, ErrNotPerson
	}
	if !fw.SameServerName(at.Server, k.cfg.Name) {
		return Connection{}, fmt.Errorf("connections: server %s: the connection belongs to %s", k.cfg.Name, at.Server)
	}
	if !at.Expires.IsZero() && k.now().After(at.Expires) {
		return Connection{}, fmt.Errorf("connections: server %s: the connection took longer than %s; start it again", k.cfg.Name, connectTTL)
	}
	if code == "" || at.Verifier == "" {
		return Connection{}, fmt.Errorf("connections: server %s: the connection has no code", k.cfg.Name)
	}
	m, err := u.ensureMeta(ctx)
	if err != nil {
		return Connection{}, err
	}
	form := url.Values{
		"grant_type":    {grantCode},
		"code":          {code},
		"redirect_uri":  {at.Redirect},
		"code_verifier": {at.Verifier},
	}
	for _, r := range u.broker.resourcesFor(u) {
		form.Add("resource", r)
	}
	ta, err := u.post(ctx, m, m.token, form)
	if err != nil {
		return Connection{}, fmt.Errorf("connections: server %s: exchange the code: %w", k.cfg.Name, err)
	}
	if ta.RefreshToken == "" {
		return Connection{}, fmt.Errorf("connections: server %s: the authorization server gave no refresh token, so the agent cannot act for the person later; allow offline access for the client, or set auth.oauth.authParams for a server without the offline_access scope", k.cfg.Name)
	}
	var account string
	if u.sameIdP(m) {
		if ta.IDToken == "" {
			return Connection{}, fmt.Errorf("connections: server %s: the sign-in issuer gave no ID token, so the workbench cannot check who signed in", k.cfg.Name)
		}
		c, err := idClaims(ta.IDToken)
		if err != nil {
			return Connection{}, fmt.Errorf("connections: server %s: %w", k.cfg.Name, err)
		}
		switch {
		case !sameIssuer(claimString(c, "iss"), m.issuer):
			return Connection{}, fmt.Errorf("connections: server %s: the ID token comes from another issuer", k.cfg.Name)
		case !hasAudience(c, u.clientID):
			return Connection{}, fmt.Errorf("connections: server %s: the ID token is for another client", k.cfg.Name)
		case at.Nonce != "" && claimString(c, "nonce") != at.Nonce:
			return Connection{}, fmt.Errorf("connections: server %s: the ID token belongs to another sign-in", k.cfg.Name)
		case claimString(c, "sub") != subject:
			return Connection{}, fmt.Errorf("connections: server %s: the person signed in at %s as another account than the one that uses the workbench; sign out there and connect again", k.cfg.Name, m.issuer)
		}
		account = accountOf(c)
	} else if ta.IDToken != "" {
		if c, err := idClaims(ta.IDToken); err == nil && (at.Nonce == "" || claimString(c, "nonce") == at.Nonce) {
			account = accountOf(c)
		}
	}
	now := k.now()
	g := Grant{
		Subject:      subject,
		Issuer:       m.issuer,
		ClientID:     u.clientID,
		RefreshToken: ta.RefreshToken,
		Scopes:       strings.Fields(ta.Scope),
		Account:      account,
		Created:      now,
	}
	if len(g.Scopes) == 0 {
		g.Scopes = u.authScopes(m)
	}
	if secs := firstPositive(mcpclient.ExpiresIn(ta.RefreshExpiresIn), mcpclient.ExpiresIn(ta.RefreshTokenExpiresIn)); secs > 0 {
		g.Expires = now.Add(time.Duration(secs) * time.Second)
	}
	key := g.Key()
	l := u.broker.lockFor(key)
	l.Lock()
	_, err = k.store.Put(ctx, g, 0)
	u.broker.clear(key)
	if err == nil && u.exchange != nil && u.access(ta) == nil {
		// In exchange mode the access token of the code is the base token.
		u.broker.keep(tokenKey{grant: key}, ta)
	}
	l.Unlock()
	if err != nil {
		return Connection{}, fmt.Errorf("connections: server %s: store the grant: %w", k.cfg.Name, err)
	}
	k.log.Info("grant stored", "server", k.cfg.Name, "subject", subject, "issuer", m.issuer, "account", account)
	return k.Connection(ctx, subject)
}

// Disconnect implements Connector. It deletes the grant, drops the tokens
// of the person, and revokes the refresh token when the authorization
// server offers revocation (RFC 7009).
func (k *connector) Disconnect(ctx context.Context, subject string) error {
	u := k.u
	m, err := u.ensureMeta(ctx)
	if err != nil {
		return err
	}
	key := u.key(m, subject)
	l := u.broker.lockFor(key)
	l.Lock()
	g, _, gerr := u.broker.load(ctx, k.store, key)
	err = k.store.Delete(ctx, key)
	u.broker.clear(key)
	l.Unlock()
	if err != nil {
		return fmt.Errorf("connections: server %s: delete the grant: %w", k.cfg.Name, err)
	}
	if gerr == nil {
		u.revoke(context.WithoutCancel(ctx), m, g.RefreshToken)
		k.log.Info("grant deleted", "server", k.cfg.Name, "subject", subject, "issuer", m.issuer)
	}
	return nil
}

// Token implements Connector.
func (k *connector) Token(ctx context.Context, subject string) (string, error) {
	return k.u.broker.token(ctx, k.u, k.store, subject)
}

// TokenExpiry returns the expiry of the cached token of a person at this
// server, for the token broker. It reports false when no token is cached
// or the server gave no lifetime.
func (k *connector) TokenExpiry(ctx context.Context, subject string) (time.Time, bool) {
	m, err := k.u.ensureMeta(ctx)
	if err != nil {
		return time.Time{}, false
	}
	b := k.u.broker
	b.mu.Lock()
	defer b.mu.Unlock()
	t, ok := b.tokens[tokenKey{grant: k.u.key(m, subject), server: k.u.server}]
	return t.exp, ok && !t.exp.IsZero()
}

// Reject implements Connector: it drops a token that the MCP server
// refused.
func (k *connector) Reject(ctx context.Context, subject, used string) {
	m, err := k.u.ensureMeta(ctx)
	if err != nil {
		return
	}
	k.u.broker.invalidate(tokenKey{grant: k.u.key(m, subject), server: k.u.server}, used)
}

// Forget implements Connector: it deletes the grant of a person whose
// tokens the MCP server keeps refusing.
func (k *connector) Forget(ctx context.Context, subject string) {
	k.u.broker.dropGrant(ctx, k.u, k.store, subject)
}
