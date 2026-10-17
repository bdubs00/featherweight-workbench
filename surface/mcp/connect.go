package mcp

// This file asks a person to connect a service when a run waits for it
// (fw.ConnectRequest), and serves the tools connections and disconnect.
// The person connects in the browser: the URL of the request opens the
// dashboard at #connect/<server>/<run>, which signs the person in and
// sends the browser to the authorization server. The end of the connection
// decides the pending approval, so the MCP client only waits. A decline
// skips the service.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/dashboard"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Names of the connection tools.
const (
	toolConnections = "connections"
	toolDisconnect  = "disconnect"
)

// viewConnections is the view of the dashboard that the connections tool
// opens.
const viewConnections = "connections"

// headerOrigin carries the public origin of an HTTP request into the
// handlers, because the SDK hands them the header and not the host. The
// handler sets it on every request, so a client cannot choose it.
const headerOrigin = "Fw-Request-Origin"

// connectDoneWait is how long a "Done" answer of the form waits for the
// connection to end before the form asks again.
const connectDoneWait = 30 * time.Second

// Answers of the connect form for a client without URL elicitation.
const (
	choiceDone = "done"
	choiceSkip = "skip"
)

// withRequestOrigin records the public origin of r in headerOrigin.
func withRequestOrigin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Header.Set(headerOrigin, requestOrigin(r))
		next.ServeHTTP(w, r)
	})
}

// requestOrigin is the origin that the client used: the scheme and host of
// the request, or the forwarded ones behind a proxy.
func requestOrigin(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if v := firstForwarded(r, "X-Forwarded-Proto"); v != "" {
		scheme = strings.ToLower(v)
	}
	host := r.Host
	if v := firstForwarded(r, "X-Forwarded-Host"); v != "" {
		host = v
	}
	if host == "" || (scheme != "http" && scheme != "https") {
		return ""
	}
	return scheme + "://" + host
}

// firstForwarded is the first comma-separated value of a header, trimmed.
func firstForwarded(r *http.Request, header string) string {
	v, _, _ := strings.Cut(r.Header.Get(header), ",")
	return strings.TrimSpace(v)
}

// base is the address of the server for links to the dashboard: the
// public page URL of the configuration without "/ui", else the origin of
// the HTTP request. It is "" over stdio without a page URL.
func (s *server) base(req sdk.Request) string {
	if u := dashboard.PageURL(s.a.Config()); u != "" {
		return strings.TrimSuffix(u, "/ui")
	}
	if req != nil {
		if ex := req.GetExtra(); ex != nil && ex.Header != nil {
			return ex.Header.Get(headerOrigin)
		}
	}
	return ""
}

// connectPath is the route of the dashboard that connects server, and
// returns to the run when run is not "".
func connectPath(server, run string) string {
	p := "/ui#connect/" + url.PathEscape(server)
	if run != "" {
		p += "/" + url.PathEscape(run)
	}
	return p
}

// connectURL is the absolute address of connectPath, or "" without a base.
func connectURL(base, server, run string) string {
	if base == "" {
		return ""
	}
	return base + connectPath(server, run)
}

// connectTitle is the name of the service of a request for a person.
func connectTitle(c *fw.ConnectRequest) string {
	if c.Title != "" {
		return c.Title
	}
	return c.Server
}

// connectShape is how a waiting run call asks for a connection.
type connectShape int

const (
	connectNative     connectShape = iota // return the run in input-required, with the link
	connectInputURL                       // input_required result with a URL request
	connectInputForm                      // input_required result with the Done/Skip form
	connectElicitURL                      // elicitation/create in URL mode on a legacy session
	connectElicitForm                     // elicitation/create with the form on a legacy session
)

// connectShapeOf picks the shape from the capabilities of the client. A
// client that declares elicitation without a mode supports the form.
func connectShapeOf(req *sdk.CallToolRequest) connectShape {
	caps := req.ClientCapabilities()
	if caps == nil || caps.Elicitation == nil {
		return connectNative
	}
	e := caps.Elicitation
	urlOK := e.URL != nil
	formOK := e.Form != nil || e.URL == nil
	modern := req.ProtocolVersion() >= modernProtocol
	switch {
	case urlOK && modern:
		return connectInputURL
	case urlOK:
		return connectElicitURL
	case formOK && modern:
		return connectInputForm
	case formOK:
		return connectElicitForm
	}
	return connectNative
}

// connectFormSchema asks a client without URL elicitation to open the link
// itself and then say Done, or Skip.
const connectFormSchema = `{"type":"object","properties":{` +
	`"choice":{"type":"string","title":"Choice","description":"done after you connected in the browser. skip goes on without the service.","enum":["done","skip"]}` +
	`},"required":["choice"]}`

// connectParams is the elicitation of a pending connect in URL mode, or in
// form mode when url is false. Without a link, the form says where in the
// dashboard the person connects.
func connectParams(r *fw.Run, link string, urlMode bool) *sdk.ElicitParams {
	p := r.Pending
	if urlMode {
		return &sdk.ElicitParams{Mode: "url", Message: p.Reason, URL: link, ElicitationID: p.ID}
	}
	where := link
	if where == "" {
		where = "the fw dashboard at " + connectPath(p.Connect.Server, r.ID)
	}
	msg := fmt.Sprintf("%s\n\nOpen %s to connect %s. Then choose done. Choose skip to go on without %s.",
		p.Reason, where, connectTitle(p.Connect), connectTitle(p.Connect))
	return &sdk.ElicitParams{Mode: "form", Message: msg, RequestedSchema: json.RawMessage(connectFormSchema)}
}

// askConnect asks for the pending connect of r in the shape that the client
// supports. It returns a result for the client, or nil when the run left
// the pending connect and await goes on.
func (s *server) askConnect(ctx context.Context, req *sdk.CallToolRequest, r *fw.Run, principal string) (*sdk.CallToolResult, error) {
	base := s.base(req)
	link := connectURL(base, r.Pending.Connect.Server, r.ID)
	shape := connectShapeOf(req)
	if link == "" && (shape == connectInputURL || shape == connectElicitURL) {
		// URL mode needs an absolute URL. Without one, the form says where
		// the person connects.
		if shape == connectInputURL {
			shape = connectInputForm
		} else {
			shape = connectElicitForm
		}
	}
	switch shape {
	case connectInputURL, connectInputForm:
		return &sdk.CallToolResult{
			InputRequests: sdk.InputRequestMap{r.Pending.ID: connectParams(r, link, shape == connectInputURL)},
			RequestState:  s.sealState(r.ID, r.Pending.ID, principal),
		}, nil
	case connectElicitURL, connectElicitForm:
		urlMode := shape == connectElicitURL
		res, err := req.Session.Elicit(ctx, connectParams(r, link, urlMode))
		if err != nil {
			s.log.Warn("mcp connect elicitation failed; returning the run in input-required", "run", r.ID, "error", err)
			return runResultAt(r, base), nil
		}
		left, err := s.connectAnswer(ctx, r, res, principal, urlMode)
		if err != nil {
			return workError("", err), nil
		}
		if urlMode && res.Action == "accept" && left {
			// A client that waits for the end of the URL interaction learns it here.
			if err := req.Session.NotifyElicitationComplete(ctx, &sdk.ElicitationCompleteParams{ElicitationID: r.Pending.ID}); err != nil {
				s.log.Debug("mcp elicitation complete notification failed", "run", r.ID, "error", err)
			}
		}
		return nil, nil
	}
	return runResultAt(r, base), nil
}

// connectAnswer acts on the answer to a connect request. An accept in URL
// mode waits until the connection ends. A form answer done waits a short
// time, so the form asks again when the person has not connected yet. A
// decline, a cancel, and skip deny the approval, which skips the service.
// Every wait ends early when ctx ends: the run then keeps waiting. It
// reports whether the run left the pending connect.
func (s *server) connectAnswer(ctx context.Context, r *fw.Run, res *sdk.ElicitResult, principal string, urlMode bool) (bool, error) {
	p := r.Pending
	skip := func(note string) (bool, error) {
		err := s.decide(ctx, r.ID, fw.ApprovalDecision{ApprovalID: p.ID, Decision: "deny", Decider: principal, Note: note})
		return err == nil, err
	}
	switch res.Action {
	case "accept":
	case "decline":
		return skip("declined by the MCP client")
	default:
		return skip("canceled by the MCP client")
	}
	if urlMode {
		return s.waitPast(ctx, r.ID, p.ID, 0), nil
	}
	choice, _ := res.Content["choice"].(string)
	if strings.EqualFold(strings.TrimSpace(choice), choiceSkip) {
		return skip("skipped in the MCP client")
	}
	return s.waitPast(ctx, r.ID, p.ID, connectDoneWait), nil
}

// waitPast waits until the run leaves the approval, ends, or ctx ends.
// A max above zero bounds the wait. It reports whether the run left the
// approval.
func (s *server) waitPast(ctx context.Context, runID, approvalID string, max time.Duration) bool {
	if max > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, max)
		defer cancel()
	}
	_, err := s.a.WaitFor(ctx, runID, func(r *fw.Run) bool {
		return r.State.Terminal() || r.Pending == nil || r.Pending.ID != approvalID
	})
	return err == nil
}

// connectText says what a run that waits for a connection needs, and the
// ways to give it or to skip it.
func connectText(r *fw.Run, base string) string {
	p := r.Pending
	title := connectTitle(p.Connect)
	var b strings.Builder
	fmt.Fprintf(&b, "Run %s waits for the person who started it to connect %s. %s\n", r.ID, title, p.Reason)
	if link := connectURL(base, p.Connect.Server, r.ID); link != "" {
		fmt.Fprintf(&b, "Open %s to connect. ", link)
	} else {
		fmt.Fprintf(&b, "Connect it in the fw dashboard at %s. ", connectPath(p.Connect.Server, r.ID))
	}
	b.WriteString("The run goes on by itself when the connection ends.\n")
	fmt.Fprintf(&b, "To go on without %s, deny with the approve tool (runId %s, approvalId %s, decision deny), "+
		`or with POST /v1/runs/%s/approvals/%s and the body {"decision":"deny"}.`,
		title, r.ID, p.ID, r.ID, p.ID)
	return b.String()
}

// connectRefusal is the text of an approve call that tried to allow a
// connect: the kernel error, and the way to connect.
func connectRefusal(r *fw.Run, base string, err error) string {
	msg := strings.TrimPrefix(err.Error(), "fw: invalid request: ")
	if r == nil || r.Pending == nil || r.Pending.Connect == nil {
		return "Invalid request: " + msg + "."
	}
	where := connectURL(base, r.Pending.Connect.Server, r.ID)
	if where == "" {
		where = "the fw dashboard at " + connectPath(r.Pending.Connect.Server, r.ID)
	}
	return fmt.Sprintf("Invalid request: %s. Open %s to connect %s, or deny the approval to go on without it.",
		msg, where, connectTitle(r.Pending.Connect))
}

// Input schema of disconnect.
const disconnectSchema = `{"type":"object","properties":{` +
	`"server":{"type":"string","minLength":1,"description":"The service to disconnect: the server field of an entry of connections."}` +
	`},"required":["server"]}`

// connectionItem is one connection in the result of connections, with the
// address that connects it.
type connectionItem struct {
	fw.Connection
	ConnectURL string `json:"connectUrl,omitempty"`
}

// connectionsResult is the structured result of connections.
type connectionsResult struct {
	View        string           `json:"view"`
	Connections []connectionItem `json:"connections"`
}

type disconnectArgs struct {
	Server string `json:"server"`
}

// addConnectionTools adds connections and disconnect when a tool source
// acts for a person, so a configuration without one keeps its tool list.
func (s *server) addConnectionTools() {
	if len(s.a.Connectors()) == 0 {
		return
	}
	sdk.AddTool(s.srv, &sdk.Tool{
		Name:        toolConnections,
		Title:       "Connections",
		Description: "List the services that act for you, the signed-in person, such as an issue tracker that an agent uses as you. Each one says whether you connected it, and gives the address that connects it in the browser. A run that needs a service that you did not connect waits until you connect it.",
		InputSchema: json.RawMessage(emptySchema),
		Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true, OpenWorldHint: ptr(false)},
	}, s.connections)
	sdk.AddTool(s.srv, &sdk.Tool{
		Name:        toolDisconnect,
		Title:       "Disconnect a service",
		Description: "Remove your grant for a service that acts for you, and revoke it at the service when the service allows it. A later run that needs the service asks you to connect again.",
		InputSchema: json.RawMessage(disconnectSchema),
		Annotations: &sdk.ToolAnnotations{DestructiveHint: ptr(true), IdempotentHint: true, OpenWorldHint: ptr(true)},
	}, s.disconnect)
}

// connectError is the text of an error of the connection tools.
func connectError(err error) *sdk.CallToolResult {
	switch {
	case errors.Is(err, fw.ErrNotConnectable):
		return errorResult("Forbidden: only a signed-in person can connect a service. Sign in with your own account, not a token of the agent or of a workload.")
	case errors.Is(err, fw.ErrConnectorNotFound):
		return errorResult("Not found: no service of that name acts for a person. Read the names with the connections tool.")
	}
	return failResult(err)
}

func (s *server) connections(ctx context.Context, req *sdk.CallToolRequest, _ struct{}) (*sdk.CallToolResult, any, error) {
	conns, err := s.a.Connections(ctx, fw.PrincipalFrom(ctx))
	if err != nil {
		return connectError(err), nil, nil
	}
	base := s.base(req)
	out := connectionsResult{View: viewConnections, Connections: []connectionItem{}}
	var b strings.Builder
	if len(conns) == 0 {
		b.WriteString("No service acts for you here.")
	} else {
		fmt.Fprintf(&b, "%d services act for you:\n", len(conns))
	}
	for _, c := range conns {
		item := connectionItem{Connection: c, ConnectURL: connectURL(base, c.Server, "")}
		out.Connections = append(out.Connections, item)
		b.WriteString(connectionLine(item))
	}
	return result(strings.TrimRight(b.String(), "\n"), out), nil, nil
}

// connectionLine is one connection on one line, with what to do next.
func connectionLine(c connectionItem) string {
	title := c.Title
	if title == "" {
		title = c.Server
	}
	var b strings.Builder
	fmt.Fprintf(&b, "- %s (%s): ", title, c.Server)
	if c.Connected {
		b.WriteString("connected")
		if c.Account != "" {
			b.WriteString(" as " + c.Account)
		}
		if !c.Since.IsZero() {
			b.WriteString(" since " + c.Since.UTC().Format(time.RFC3339))
		}
		fmt.Fprintf(&b, ". Remove it with disconnect (server %s).", c.Server)
	} else {
		b.WriteString("not connected. ")
		if c.ConnectURL != "" {
			b.WriteString("Connect it at " + c.ConnectURL + ".")
		} else {
			b.WriteString("Connect it in the fw dashboard at " + connectPath(c.Server, "") + ".")
		}
	}
	return b.String() + "\n"
}

func (s *server) disconnect(ctx context.Context, req *sdk.CallToolRequest, in disconnectArgs) (*sdk.CallToolResult, any, error) {
	server := strings.TrimSpace(in.Server)
	if server == "" {
		return errorResult("Invalid request: the argument server is required."), nil, nil
	}
	p := fw.PrincipalFrom(ctx)
	if err := s.a.Disconnect(ctx, p, server); err != nil {
		return connectError(err), nil, nil
	}
	s.log.Info("mcp connection removed", "server", server, "subject", p.Subject)
	conns, err := s.a.Connections(ctx, p)
	if err != nil {
		return connectError(err), nil, nil
	}
	base := s.base(req)
	out := connectionsResult{View: viewConnections, Connections: []connectionItem{}}
	title := server
	for _, c := range conns {
		out.Connections = append(out.Connections, connectionItem{Connection: c, ConnectURL: connectURL(base, c.Server, "")})
		if fw.SameServerName(c.Server, server) && c.Title != "" {
			title = c.Title
		}
	}
	return result(fmt.Sprintf("Disconnected %s. A run that needs it asks you to connect again.", title), out), nil, nil
}
