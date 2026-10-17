// Package mcp is the MCP server surface of fw: a work intake equal to the
// native API, A2A, and the bus. See docs/ARCHITECTURE.md section 8.4.
//
// The server offers the tools run, run_status, approve, cancel, and send,
// the management tools whoami, agents_list, agent_get, agent_put,
// agent_delete, runs_list, run_events, run_file, run_result,
// agent_builder, capabilities, dashboard, and the deprecated usage, every
// tool that mcp.expose names, the resources fw://runs/{id},
// fw://runs/{id}/ledger, fw://runs/{id}/files/{path}, fw://skills/{name},
// and the MCP App ui://fw/dashboard, and one prompt per agent profile. The
// library is github.com/modelcontextprotocol/go-sdk.
//
// Over HTTP the server is dual-era. A request that carries the header
// MCP-Protocol-Version 2026-07-28 or later goes to a stateless handler and
// is served without a session. Every other request goes to a stateful
// handler, where initialize opens a legacy 2025-11-25 session with
// Mcp-Session-Id. ServeStdio serves the same server over stdio for fw mcp.
//
// A run that needs an approval travels in one of three shapes:
//
//   - A modern client that declares elicitation gets an input_required
//     result with one elicitation/create request and an HMAC-signed
//     requestState (SEP-2322). The client retries run with the answer, and
//     the retry continues the same run.
//   - A legacy session that declares elicitation gets elicitation/create as
//     a server request on the open call.
//   - Any other client gets the run object in the state input-required. The
//     text names the approve tool and the native endpoint
//     POST /v1/runs/{id}/approvals/{approval}.
//
// A run that waits for the person to connect a service (fw.ConnectRequest)
// asks for a browser visit instead. A client with URL elicitation gets the
// address of the dashboard route #connect/<server>/<run> in URL mode, as an
// input request or as elicitation/create, and waits. The end of the
// connection in the browser decides the approval, and a legacy session
// then gets notifications/elicitation/complete. A client with the form only
// gets a form with the address and the choices done and skip. Any other
// client gets the address in the text of the run. A decline or skip
// denies the approval, so the run goes on without the service. When a tool
// source acts for a person, the tools connections and disconnect list and
// remove the grants of the caller.
//
// The tasks extension (io.modelcontextprotocol/tasks) is not served,
// because the SDK has no support for tasks/get or CreateTaskResult.
//
// The key that signs requestState comes from the secret FW_MCP_STATE_KEY.
// Without it, the server reads or creates a random key in the file
// mcp-state.key under the home directory, so a requestState survives a
// restart of one process. Replicas behind one endpoint share the secret,
// because each replica has its own file. When the file cannot be read or
// written, the key lives in memory only.
package mcp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/connections"
	"github.com/bdubs00/featherweight-workbench/dashboard"
	"github.com/modelcontextprotocol/go-sdk/auth"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// DefaultPath is the route of the MCP endpoint when mcp.path is empty.
const DefaultPath = "/mcp"

// modernProtocol is the first revision without protocol sessions.
const modernProtocol = "2026-07-28"

// legacySessionTimeout closes a legacy session that sends no request for
// this long.
const legacySessionTimeout = time.Hour

// maxRequestBytes bounds one request: a run or a message with files in
// base64, plus 1 MB for the rest.
const maxRequestBytes = fw.MaxAttachmentBytes/3*4 + 1<<20

func init() {
	fw.RegisterSurface("mcp", func(*fw.Config) (fw.Surface, error) { return New(), nil })
	// A program that links the MCP server serves it by default.
	fw.RegisterDefaults(func(c *fw.Config) { c.Surfaces = append(c.Surfaces, "mcp") })
}

// New returns the MCP surface. Its Mount serves Handler at Config.MCP.Path.
func New() fw.Surface { return fw.SurfaceFunc(mount) }

func mount(mux *http.ServeMux, a *fw.Agent) error {
	h, err := Handler(a)
	if err != nil {
		return err
	}
	mux.Handle(mountPath(a.Config()), h)
	return nil
}

func mountPath(cfg *fw.Config) string {
	if p := strings.TrimSpace(cfg.MCP.Path); p != "" {
		return p
	}
	return DefaultPath
}

// Handler builds the HTTP handler of the MCP endpoint: the Origin check,
// then the agent Authn, then the Streamable HTTP transport for both eras.
func Handler(a *fw.Agent) (http.Handler, error) {
	srv, err := NewServer(a)
	if err != nil {
		return nil, err
	}
	return newHandler(a, srv), nil
}

func newHandler(a *fw.Agent, srv *sdk.Server) http.Handler {
	log := a.Logger().With("component", "mcp")
	get := func(*http.Request) *sdk.Server { return srv }
	// A modern request is complete on its own. Its handler context ends with
	// the HTTP request, which ends the run call but not the run.
	modern := sdk.NewStreamableHTTPHandler(get, &sdk.StreamableHTTPOptions{
		Stateless:                    true,
		PropagateRequestCancellation: true,
		MaxRequestBodyBytes:          maxRequestBytes,
		Logger:                       log,
	})
	legacy := sdk.NewStreamableHTTPHandler(get, &sdk.StreamableHTTPOptions{
		SessionTimeout:      legacySessionTimeout,
		MaxRequestBodyBytes: maxRequestBytes,
		Logger:              log,
	})
	route := withRequestOrigin(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Mcp-Session-Id") == "" && r.Header.Get("Mcp-Protocol-Version") >= modernProtocol {
			modern.ServeHTTP(w, r)
			return
		}
		legacy.ServeHTTP(w, r)
	}))
	// The SDK carries a per-request TokenInfo into every handler and binds a
	// legacy session to its user. fw fills it from the principal that the
	// agent Authn found, so that every call sees the principal of its own
	// request and not the one of the request that opened the session.
	bearer := auth.RequireBearerToken(func(ctx context.Context, _ string, _ *http.Request) (*auth.TokenInfo, error) {
		p := fw.PrincipalFrom(ctx)
		if p == nil {
			return nil, auth.ErrInvalidToken
		}
		return &auth.TokenInfo{UserID: p.Subject, Scopes: p.Scopes}, nil
	}, &auth.RequireBearerTokenOptions{AllowMissingExpiration: true})(route)
	withToken := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hasBearer(r) {
			bearer.ServeHTTP(w, r)
			return
		}
		route.ServeHTTP(w, r)
	})
	return checkOrigin(a.Config().MCP.Origins, log, fw.RequireAuth(a, withToken))
}

func hasBearer(r *http.Request) bool {
	f := strings.Fields(r.Header.Get("Authorization"))
	return len(f) == 2 && strings.EqualFold(f[0], "bearer")
}

// checkOrigin answers 403 to a request whose Origin header is not in
// allowed. A request without Origin passes, because non-browser clients
// send none. The entry "*" allows every origin.
func checkOrigin(allowed []string, log *slog.Logger, next http.Handler) http.Handler {
	set := map[string]bool{}
	for _, o := range allowed {
		set[normOrigin(o)] = true
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" && !set["*"] && !set[normOrigin(origin)] {
			log.Warn("mcp request from an origin that is not allowed", "origin", origin)
			http.Error(w, "Forbidden: origin not allowed", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func normOrigin(o string) string {
	return strings.ToLower(strings.TrimRight(strings.TrimSpace(o), "/"))
}

// ServeStdio serves the MCP server over stdin and stdout until ctx ends or
// the client closes the stream. The principal in ctx, if any, is the
// principal of every call.
func ServeStdio(ctx context.Context, a *fw.Agent) error {
	return serve(ctx, a, &sdk.StdioTransport{})
}

func serve(ctx context.Context, a *fw.Agent, t sdk.Transport) error {
	srv, err := NewServer(a)
	if err != nil {
		return err
	}
	if err := srv.Run(ctx, t); err != nil && !errors.Is(err, context.Canceled) {
		return fmt.Errorf("mcp: serve: %w", err)
	}
	return nil
}

// server holds what the handlers of one MCP server share.
type server struct {
	a        *fw.Agent
	srv      *sdk.Server
	log      *slog.Logger
	key      []byte        // signs requestState
	stateTTL time.Duration // lifetime of a requestState
	gw       *gateway      // nil when mcp.expose is empty
}

// NewServer builds the MCP server that the HTTP and the stdio transports
// share.
func NewServer(a *fw.Agent) (*sdk.Server, error) {
	if a == nil {
		return nil, errors.New("mcp: agent is nil")
	}
	cfg := a.Config()
	s := &server{
		a:        a,
		log:      a.Logger().With("component", "mcp"),
		stateTTL: cfg.MCP.TaskTTL,
	}
	s.key = stateKey(a, s.log)
	if s.stateTTL <= 0 {
		s.stateTTL = 24 * time.Hour
	}
	version := cfg.Version
	if version == "" {
		version = "dev"
	}
	// An empty value drops the logging capability, which revision 2026-07-28
	// deprecates. Prompt lists resolve current profiles and access per request.
	// Profile changes happen outside the SDK registry, so prompt list-change
	// notifications are disabled. The MCP Apps extension names the dashboard.
	caps := &sdk.ServerCapabilities{Prompts: &sdk.PromptCapabilities{}}
	caps.AddExtension(dashboard.Extension, map[string]any{"mimeTypes": []string{dashboard.MIMEType}})
	s.srv = sdk.NewServer(&sdk.Implementation{
		Name:    "featherweight-kernel",
		Title:   "featherweight agent",
		Version: version,
	}, &sdk.ServerOptions{
		Instructions: instructions(a),
		Logger:       s.log,
		Capabilities: caps,
		SetCacheable: setCacheable,
	})
	s.addWorkTools()
	s.addManageTools()
	s.addConnectionTools()
	s.addResources()
	s.addPrompts()
	if len(cfg.MCP.Expose) > 0 {
		s.gw = newGateway(s, cfg.MCP.Expose)
		s.srv.AddReceivingMiddleware(s.gw.middleware)
	}
	return s.srv, nil
}

// setCacheable marks discovery public, because it is the same for every
// caller, and the tool list private, because it depends on the scopes of
// the token. A run changes at any time, so a read is never cached.
func setCacheable(_ context.Context, req sdk.Request, c *sdk.Cacheable) {
	switch req.(type) {
	case *sdk.ServerRequest[*sdk.DiscoverParams]:
		c.CacheScope, c.TTLMs = "public", 60_000
	case *sdk.ListToolsRequest:
		c.CacheScope, c.TTLMs = "private", 60_000
	case *sdk.ReadResourceRequest:
		c.CacheScope, c.TTLMs = "private", 0
	}
}

// instructions describes the protocol without profile data. Discovery is
// public; agents_list and prompts/list apply the caller's access rules.
func instructions(a *fw.Agent) string {
	var b strings.Builder
	b.WriteString("featherweight-kernel (fw) runs agent work. The run tool starts a run of an agent profile. ")
	b.WriteString("A profile is a configured agent with its own system prompt, tools, policy, and budget.\n\n")
	b.WriteString("Use agents_list to discover the current profiles you can view, and agent_get to read one in full.\n")
	b.WriteString(`
Tools:
- run starts a run with input, and optionally title, agent, budget, and requestId. With wait true (the default) the call returns when the run finishes or needs input. It sends one progress notification per turn when the request carries a progress token. With wait false it returns the run object at once.
- run also takes instructions, text for this run that follows the system prompt, and profile, an inline profile (system, instructions, skills, tools, model, effort, timeZone, policy, budget) that overlays the named profile for this run without a configuration change.
- run_status returns the run object. send adds a follow-up message to a run. cancel stops a run. approve decides a pending approval.
- Every result holds a text and the run object in structuredContent. isError is true when the run failed.
- dashboard opens the fw dashboard, an MCP App that starts work, shows the work that needs you, the work in progress, and the recent results, and edits and builds agents. The page loads its own data, so open it once, when the person wants to see it. Without the page, dashboard returns the same home as text, and the management tools whoami, agents_list, agent_get, agent_put, agent_delete, runs_list, run_events, run_result, run_file, agent_builder, and capabilities do the same work one call at a time.
- run_result says what a run produced and what it needs next. run_events pages through the events of a run: from reads forward and before reads back. run_file reads a file that a run changed; with raw true it returns the bytes, up to 4 MiB. capabilities lists the tools of the runtime in plain words.
- agent_builder starts a conversation that creates an agent. Answer it with send, read its proposal with run_result or run_events, and create the agent with agent_put from the profile of the proposal.
- usage is deprecated: operators read the Prometheus metrics of fw.

Approvals: the policy can hold a tool call until somebody allows or denies it. When the client supports form elicitation, run asks through a form with the fields decision (allow or deny) and note. Otherwise run returns the run in the state input-required. Then decide with the approve tool, or with POST /v1/runs/{id}/approvals/{approval} on the native API. The run waits for a decision from any surface.

Disconnects: when the connection closes, the run call ends but the run continues. Read it again with run_status or the resource fw://runs/{id}. Only the cancel tool stops a run.

Retries: pass requestId to make a retried run call return the same run instead of a second one.

Resources: fw://runs/{id} is the run object as JSON. fw://runs/{id}/ledger is the event log of the run as JSONL. fw://runs/{id}/files/{path} is a file that the run changed, up to 4 MiB. These three read the runs of this process only. fw://skills/{name} is the SKILL.md file of a skill from the skills directories of the configuration; the resource list names every skill.

Prompts: there is one prompt per profile. It holds the system prompt of the profile and the optional argument input.`)
	if c := connections.Of(a); c != nil && len(c.Connectors()) > 0 {
		b.WriteString(`

Connections: some tools act for the person who started the run, at a service such as an issue tracker. If that person has not connected the service, or the service refused the grant, the run waits in input-required, and pending.connect names the service. A client with URL elicitation gets the address to open in the browser. Otherwise the text of the run gives it. The run goes on by itself when the person connects. To go on without the service, deny the approval. The approve tool cannot allow it. connections lists the services that act for the caller, with the address that connects each one, and disconnect removes one.`)
	}
	b.WriteString(fleetInstructions(a))
	return b.String()
}

// profileSummary is the first line of the system prompt, shortened.
func profileSummary(p fw.AgentProfile) string {
	for _, line := range strings.Split(p.System, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			return truncate(line, 200)
		}
	}
	return "the " + p.Name + " agent profile"
}

// truncate shortens s to at most n runes and marks the cut.
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "..."
}
