// Package connections lets a person give the workbench a grant to act for
// them at an outside service, and gives the kernel the tokens of that
// person (docs/ARCHITECTURE.md section 11).
//
// A tools.mcp entry of the kernel with auth.oauth.grant user acts for the
// person who started the run. The kernel pauses a call that needs the
// person (fw.RequestConnect) and gets the tokens of the person through
// fw.PersonTokens. This package is that PersonTokens: it keeps the grants
// of people in a Store, runs the OAuth flows, and resumes the paused calls
// with fw.Agent.Connected when a connection ends. It runs as a service of
// the kernel (fw.RegisterService), so a process that serves has it and a
// verb that runs one piece of work does not.
package connections

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/tokens"
)

func init() {
	fw.RegisterService(Start)
}

// Errors of connections.
var (
	// ErrNotPerson means that the caller is not a person who can connect a
	// service, such as a workload or the static token.
	ErrNotPerson = errors.New("connections: only a signed-in person can connect a service")
	// ErrUnknownService means that no service of that name acts for a
	// person.
	ErrUnknownService = errors.New("connections: no service of that name acts for a person")
)

// connectTTL is the life of a connection in progress.
const connectTTL = 10 * time.Minute

// Connection is the state of one service that acts for a person, as a
// surface shows it. It never carries a token. SignIn is true when the
// service trusts the sign-in issuer (auth.oidc), so one connection covers
// every such service with the same client.
type Connection struct {
	Server    string    `json:"server"`
	Title     string    `json:"title"`
	Issuer    string    `json:"issuer,omitempty"`
	Connected bool      `json:"connected"`
	Account   string    `json:"account,omitempty"`
	Since     time.Time `json:"since,omitzero"`
	Refreshed time.Time `json:"refreshed,omitzero"`
	SignIn    bool      `json:"signIn,omitempty"`
}

// Attempt is one connection in progress. URL is the authorization request
// that the browser of the person opens. The surface keeps the whole value
// in an HttpOnly cookie that it signs, because Verifier (the PKCE verifier)
// is a secret, and gives it back to Finish with the code from the callback.
// State is the state parameter of the request.
type Attempt struct {
	Server   string    `json:"server"`
	URL      string    `json:"url"`
	State    string    `json:"state"`
	Verifier string    `json:"verifier"`
	Nonce    string    `json:"nonce,omitempty"`
	Redirect string    `json:"redirect"`
	Expires  time.Time `json:"expires"`
}

// A Connector connects one service for people and gives their tokens. Open
// builds one per tools.mcp entry with grant user (connector.go). A test
// installs its own.
//
// Name is the name of the service, the tools.mcp entry name. Connection
// says whether subject has a grant. Begin builds the authorization request
// with PKCE, the state, and the redirect address of the callback. Finish
// exchanges the code, checks that the person at the sign-in issuer is
// subject, and stores the grant. Disconnect deletes the grant and revokes
// it at the server when the server offers revocation (RFC 7009). Token,
// Reject, and Forget are fw.PersonTokens for this one service.
type Connector interface {
	Name() string
	Connection(ctx context.Context, subject string) (Connection, error)
	Begin(ctx context.Context, subject, redirect string) (Attempt, error)
	Finish(ctx context.Context, subject string, at Attempt, code string) (Connection, error)
	Disconnect(ctx context.Context, subject string) error
	Token(ctx context.Context, subject string) (string, error)
	Reject(ctx context.Context, subject, used string)
	Forget(ctx context.Context, subject string)
}

// Connections are the services that act for people in one process. It
// implements fw.PersonTokens.
type Connections struct {
	a       *fw.Agent
	list    []Connector
	closers []io.Closer
	log     *slog.Logger
}

var _ fw.PersonTokens = (*Connections)(nil)

// New returns the connections of a with the given connectors.
func New(a *fw.Agent, connectors ...Connector) *Connections {
	log := slog.Default()
	if a != nil {
		log = a.Logger()
	}
	return &Connections{a: a, list: slices.Clone(connectors), log: log}
}

// Connectors returns the connectors in the order of the configuration.
func (c *Connections) Connectors() []Connector { return slices.Clone(c.list) }

// connector returns the connector named server. Names match loosely, as
// fw.SameServerName says.
func (c *Connections) connector(server string) (Connector, error) {
	for _, k := range c.list {
		if fw.SameServerName(k.Name(), server) {
			return k, nil
		}
	}
	return nil, fmt.Errorf("%w: %s", ErrUnknownService, server)
}

// subjectOf returns the subject of a person who can connect.
func subjectOf(p *fw.Principal) (string, error) {
	if p == nil || p.Kind != fw.KindUser || strings.TrimSpace(p.Subject) == "" {
		return "", ErrNotPerson
	}
	return p.Subject, nil
}

// List lists the services that act for a person, with the state of each
// for p.
func (c *Connections) List(ctx context.Context, p *fw.Principal) ([]Connection, error) {
	subject, err := subjectOf(p)
	if err != nil {
		return nil, err
	}
	out := []Connection{}
	for _, k := range c.list {
		conn, err := k.Connection(ctx, subject)
		if err != nil {
			return nil, err
		}
		out = append(out, conn)
	}
	return out, nil
}

// Begin starts a connection of server for p. redirect is the absolute
// address of the callback route of the surface.
func (c *Connections) Begin(ctx context.Context, p *fw.Principal, server, redirect string) (Attempt, error) {
	subject, err := subjectOf(p)
	if err != nil {
		return Attempt{}, err
	}
	k, err := c.connector(server)
	if err != nil {
		return Attempt{}, err
	}
	return k.Begin(ctx, subject, redirect)
}

// Finish ends a connection that Begin started: it stores the grant, then
// lets every paused call of p go on whose service is now connected. It
// returns the connections of p after the change.
func (c *Connections) Finish(ctx context.Context, p *fw.Principal, at Attempt, code string) ([]Connection, error) {
	subject, err := subjectOf(p)
	if err != nil {
		return nil, err
	}
	k, err := c.connector(at.Server)
	if err != nil {
		return nil, err
	}
	if _, err := k.Finish(ctx, subject, at, code); err != nil {
		return nil, err
	}
	conns, err := c.List(ctx, p)
	if err != nil {
		return nil, err
	}
	var connected []string
	for _, cn := range conns {
		if cn.Connected {
			connected = append(connected, cn.Server)
		}
	}
	if c.a != nil && len(connected) > 0 {
		c.a.Connected(subject, connected...)
		c.announce(ctx, subject, connected)
	}
	return conns, nil
}

// announce tells the other processes of the fleet that the person connected
// the services, through the bus notice of the token broker (the kernel
// package tokens). Each member turns it into fw.Agent.Connected.
func (c *Connections) announce(ctx context.Context, subject string, servers []string) {
	bus := c.a.Bus()
	if bus == nil {
		return
	}
	topic := strings.TrimSpace(c.a.Config().Tokens.Subject)
	if topic == "" {
		topic = fw.DefaultTokensSubject
	}
	ev, err := fw.NewEvent("", tokens.EvConnected, tokens.Connected{Subject: subject, Servers: servers})
	if err == nil {
		err = bus.Publish(context.WithoutCancel(ctx), topic, ev)
	}
	if err != nil {
		c.log.Warn("the notice of a connection did not reach the bus", "subject", subject, "error", err)
	}
}

// TokenInfo is Token with the expiry of the token, when the connector
// knows it. The token broker gives the expiry to the members.
func (c *Connections) TokenInfo(ctx context.Context, subject, server string) (string, time.Time, error) {
	k, err := c.connector(server)
	if err != nil {
		return "", time.Time{}, err
	}
	tok, err := k.Token(ctx, subject)
	if err != nil {
		return "", time.Time{}, err
	}
	var exp time.Time
	if e, ok := k.(interface {
		TokenExpiry(ctx context.Context, subject string) (time.Time, bool)
	}); ok {
		if at, known := e.TokenExpiry(ctx, subject); known {
			exp = at
		}
	}
	return tok, exp, nil
}

// Disconnect deletes the grant of p for server.
func (c *Connections) Disconnect(ctx context.Context, p *fw.Principal, server string) error {
	subject, err := subjectOf(p)
	if err != nil {
		return err
	}
	k, err := c.connector(server)
	if err != nil {
		return err
	}
	return k.Disconnect(ctx, subject)
}

// Token implements fw.PersonTokens.
func (c *Connections) Token(ctx context.Context, subject, server string) (string, error) {
	k, err := c.connector(server)
	if err != nil {
		return "", err
	}
	return k.Token(ctx, subject)
}

// Reject implements fw.PersonTokens.
func (c *Connections) Reject(ctx context.Context, subject, server, used string) {
	if k, err := c.connector(server); err == nil {
		k.Reject(ctx, subject, used)
	}
}

// Forget implements fw.PersonTokens.
func (c *Connections) Forget(ctx context.Context, subject, server string) {
	if k, err := c.connector(server); err == nil {
		k.Forget(ctx, subject)
	}
}

// Close closes what Open opened, such as a store that holds a connection.
func (c *Connections) Close() error {
	var errs []error
	for i := len(c.closers) - 1; i >= 0; i-- {
		errs = append(errs, c.closers[i].Close())
	}
	return errors.Join(errs...)
}

// all ties the Connections to the agent that they serve.
var all sync.Map // *fw.Agent to *Connections

// Of returns the connections of a, or nil when a keeps none.
func Of(a *fw.Agent) *Connections {
	if a == nil {
		return nil
	}
	v, ok := all.Load(a)
	if !ok {
		return nil
	}
	return v.(*Connections)
}

// Install makes c the connections of a: Of returns it, and the tool
// sources of a get the tokens of people from it. A nil c uninstalls.
func Install(a *fw.Agent, c *Connections) {
	if c == nil {
		all.Delete(a)
		a.SetPersonTokens(nil)
		return
	}
	all.Store(a, c)
	a.SetPersonTokens(c)
}

// List is Connections.List for the agent a. Without connections, it
// returns an empty list for a person.
func List(ctx context.Context, a *fw.Agent, p *fw.Principal) ([]Connection, error) {
	if c := Of(a); c != nil {
		return c.List(ctx, p)
	}
	if _, err := subjectOf(p); err != nil {
		return nil, err
	}
	return []Connection{}, nil
}

// Begin is Connections.Begin for the agent a.
func Begin(ctx context.Context, a *fw.Agent, p *fw.Principal, server, redirect string) (Attempt, error) {
	if c := Of(a); c != nil {
		return c.Begin(ctx, p, server, redirect)
	}
	if _, err := subjectOf(p); err != nil {
		return Attempt{}, err
	}
	return Attempt{}, fmt.Errorf("%w: %s", ErrUnknownService, server)
}

// Finish is Connections.Finish for the agent a.
func Finish(ctx context.Context, a *fw.Agent, p *fw.Principal, at Attempt, code string) ([]Connection, error) {
	if c := Of(a); c != nil {
		return c.Finish(ctx, p, at, code)
	}
	if _, err := subjectOf(p); err != nil {
		return nil, err
	}
	return nil, fmt.Errorf("%w: %s", ErrUnknownService, at.Server)
}

// Disconnect is Connections.Disconnect for the agent a.
func Disconnect(ctx context.Context, a *fw.Agent, p *fw.Principal, server string) error {
	if c := Of(a); c != nil {
		return c.Disconnect(ctx, p, server)
	}
	if _, err := subjectOf(p); err != nil {
		return err
	}
	return fmt.Errorf("%w: %s", ErrUnknownService, server)
}

// personal returns the tools.mcp entries that act for a person.
func personal(cfg *fw.Config) []fw.MCPServerConfig {
	var out []fw.MCPServerConfig
	for _, m := range cfg.Tools.MCP {
		if m.Auth.OAuth.GrantOf() == fw.GrantUser && !cfg.IsControlServer(m.Name) {
			out = append(out, m)
		}
	}
	return out
}

// Start is the fw.Service of the connections. Without an entry that acts
// for a person, it starts nothing. Otherwise it opens the store, builds a
// connector per entry, and installs them on the agent. An entry that acts
// for a person needs oidc in auth.mode, because only a person with an
// OpenID Connect identity can connect.
func Start(ctx context.Context, a *fw.Agent) (io.Closer, error) {
	cfg := a.Config()
	entries := personal(cfg)
	if len(entries) == 0 {
		return nil, nil
	}
	if !slices.Contains(cfg.Auth.Modes(), "oidc") {
		return nil, fmt.Errorf("connections: server %q: auth.oauth.grant user acts for the person who started a run, so auth.mode must include oidc", entries[0].Name)
	}
	gc, err := Load(cfg)
	if err != nil {
		return nil, err
	}
	store, err := NewStore(ctx, gc, cfg, a.Secrets())
	if err != nil {
		return nil, err
	}
	c, err := Open(a, store, entries, cfg.Auth.OIDC, a.Secrets(), a.Logger())
	if err != nil {
		if cl, ok := store.(io.Closer); ok {
			_ = cl.Close()
		}
		return nil, err
	}
	if cl, ok := store.(io.Closer); ok {
		c.closers = append(c.closers, cl)
	}
	Install(a, c)
	return service{a: a, c: c}, nil
}

// Open builds the connections of a: one OAuth connector per entry, which
// share one broker, so entries with the same grant share its lock. oidc is
// auth.oidc, or nil.
func Open(a *fw.Agent, store Store, entries []fw.MCPServerConfig, oidc *fw.OIDCConfig, secrets fw.Secrets, log *slog.Logger) (*Connections, error) {
	if log == nil {
		log = slog.Default()
	}
	log = log.With("component", "connections")
	b := newBroker()
	var connectors []Connector
	for _, e := range entries {
		k, err := newConnector(e, oidc, b, store, secrets, log)
		if err != nil {
			return nil, fmt.Errorf("connections: server %q: %w", e.Name, err)
		}
		connectors = append(connectors, k)
	}
	c := New(a, connectors...)
	c.log = log
	return c, nil
}

// service uninstalls the connections of an agent and closes them.
type service struct {
	a *fw.Agent
	c *Connections
}

func (s service) Close() error {
	Install(s.a, nil)
	return s.c.Close()
}
