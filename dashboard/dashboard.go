// Package dashboard holds the management dashboard of fw: one HTML page
// and the shapes that the surfaces serve to it.
//
// The page is an MCP App (SEP-1865, specification 2026-01-26). The MCP
// server serves it as the resource ui://fw/dashboard with the MIME type
// text/html;profile=mcp-app, and every management tool points at it through
// the _meta.ui.resourceUri key, so a host that supports MCP Apps renders
// the dashboard in place of the text result. The native surface serves the
// same page at GET /ui, where it calls the native API with a bearer token.
//
// Both surfaces build their answers from the functions here and from
// package view of the kernel, so the page reads one shape whichever
// transport it is on: a principal with its roles,
// agent profiles with the role of the caller, the home lists, runs with
// their results, chat items, event lines, and the catalog of tools. The
// page shows no usage. Operators read the usage of the runs in the
// Prometheus metrics. Usages and the field AgentView.Usage stay for the
// native and MCP contracts.
package dashboard

import (
	"context"
	_ "embed"
	"fmt"
	"net/url"
	"sort"
	"strings"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/surface/view"
)

//go:embed index.html
var page []byte

// Names of the MCP Apps extension, as the specification at
// https://github.com/modelcontextprotocol/ext-apps/blob/main/specification/2026-01-26/apps.mdx
// defines them.
const (
	// URI is the resource URI of the dashboard page.
	URI = "ui://fw/dashboard"
	// MIMEType is the MIME type of an MCP App resource.
	MIMEType = "text/html;profile=mcp-app"
	// MetaKey is the _meta key that links a tool to its UI resource and that
	// carries the preferences of a resource.
	MetaKey = "ui"
	// Extension is the identifier of the MCP Apps extension.
	Extension = "io.modelcontextprotocol/ui"
	// ProtocolVersion is the revision of the MCP Apps protocol that the page
	// speaks over the postMessage bridge.
	ProtocolVersion = "2026-01-26"
)

// PageURL returns the address of the dashboard page for a browser: the
// scheme and the host of mcp.auth.audience, else of a2a.url, followed by
// /ui. It returns "" when neither names an absolute URL. A gateway in front
// of fw serves the page at that public address.
func PageURL(cfg *fw.Config) string {
	if cfg == nil {
		return ""
	}
	for _, raw := range []string{cfg.MCP.Auth.Audience, cfg.A2A.URL} {
		u, err := url.Parse(strings.TrimSpace(raw))
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			continue
		}
		return u.Scheme + "://" + u.Host + "/ui"
	}
	return ""
}

// Page returns the HTML of the dashboard.
func Page() []byte { return page }

// ToolMeta is the _meta of a tool whose result the dashboard renders.
func ToolMeta() map[string]any {
	return map[string]any{MetaKey: map[string]any{"resourceUri": URI}}
}

// ResourceMeta is the _meta of the dashboard resource. The page loads no
// external resource and makes no network request inside a host, so it
// declares no domain and keeps the restrictive default policy of the host.
func ResourceMeta() map[string]any {
	return map[string]any{MetaKey: map[string]any{"prefersBorder": true}}
}

// Report is the usage of the runs that the caller can see, in total, by
// agent, and by principal.
//
// Deprecated: the page no longer shows usage, and operators read the
// Prometheus metrics. The type stays for the native and MCP contracts.
type Report struct {
	Total       view.Usage            `json:"total"`
	ByAgent     map[string]view.Usage `json:"byAgent"`
	ByPrincipal map[string]view.Usage `json:"byPrincipal"`
}

// String is the report as a text table.
func (r Report) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "Total: %s\n", r.Total)
	b.WriteString(section("By agent", r.ByAgent))
	b.WriteString(section("By principal", r.ByPrincipal))
	return strings.TrimRight(b.String(), "\n")
}

func section(title string, m map[string]view.Usage) string {
	if len(m) == 0 {
		return ""
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString(title + ":\n")
	for _, k := range keys {
		name := k
		if name == "" {
			name = "(no principal)"
		}
		fmt.Fprintf(&b, "- %s: %s\n", name, m[k])
	}
	return b.String()
}

// Usages sums the runs that the caller can see.
//
// Deprecated: the page no longer shows usage, and operators read the
// Prometheus metrics. The function stays for the native and MCP
// contracts, GET /v1/usage and the tool usage.
func Usages(ctx context.Context, a *fw.Agent) (Report, error) {
	runs, err := a.List(ctx)
	if err != nil {
		return Report{}, err
	}
	rep := Report{ByAgent: map[string]view.Usage{}, ByPrincipal: map[string]view.Usage{}}
	for _, r := range runs {
		rep.Total.Add(r)
		u := rep.ByAgent[r.Agent]
		u.Add(r)
		rep.ByAgent[r.Agent] = u
		u = rep.ByPrincipal[r.Principal]
		u.Add(r)
		rep.ByPrincipal[r.Principal] = u
	}
	return rep, nil
}
