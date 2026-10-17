package web

// This file holds the views for people: the home of the caller, the result
// of a run, the transcript of a run, the deprecated usage of the runs, the
// page, and the OAuth protected-resource document.

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/surface/view"
	"github.com/bdubs00/featherweight-workbench/dashboard"
	"github.com/bdubs00/featherweight-workbench/teams"
)

// usageDocs is the documentation of the metrics that replace GET
// /v1/usage.
const usageDocs = "https://github.com/bdubs00/featherweight-kernel/blob/main/docs/ARCHITECTURE.md#observability"

// usage answers GET /v1/usage with the usage of the visible runs in total,
// by agent, and by principal.
//
// Deprecated: the dashboard no longer shows usage. Operational reporting
// comes from the Prometheus metrics of /metrics. The route keeps its body
// for compatibility and adds the header Deprecation: true and a Link
// header with rel="deprecation" that points to the documentation of the
// metrics.
func (s *server) usage(w http.ResponseWriter, r *http.Request) {
	rep, err := dashboard.Usages(r.Context(), s.a)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	w.Header().Set("Deprecation", "true")
	w.Header().Set("Link", "<"+usageDocs+`>; rel="deprecation"; type="text/markdown"`)
	writeJSON(w, http.StatusOK, rep)
}

// home answers GET /v1/home with the first screen of the caller
// (dashboard.Home): the runs that need the caller, the runs in progress,
// and the recent results, from the runs of the whole fleet that the caller
// can view.
func (s *server) home(w http.ResponseWriter, r *http.Request) {
	runs, err := view.Runs(r.Context(), s.a, "", "")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	h, err := dashboard.HomeOf(r.Context(), s.a, runs, s.now())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	// A list is an array in JSON, also when it is empty.
	for _, l := range []*[]dashboard.WorkItem{&h.NeedsYou, &h.InProgress, &h.Recent} {
		if *l == nil {
			*l = []dashboard.WorkItem{}
		}
	}
	writeJSON(w, http.StatusOK, h)
}

// whoami answers GET /v1/whoami with the caller: the principal, its role
// on every agent that it can see, and its teams (dashboard.Whoami).
func (s *server) whoami(w http.ResponseWriter, r *http.Request) {
	who, err := dashboard.WhoamiOf(r.Context(), s.a)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, who)
}

// othersLimit bounds the runs that GET /v1/home/others lists.
const othersLimit = 200

// others answers GET /v1/home/others with the top-level runs of people
// outside the teams of the caller, newest first, at most othersLimit, as
// {"runs": [...]}. The page groups them by person behind one row of home.
func (s *server) others(w http.ResponseWriter, r *http.Request) {
	runs, err := view.Runs(r.Context(), s.a, "", "")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	top := make([]*fw.Run, 0, len(runs))
	for _, run := range runs {
		if run != nil && run.Parent == "" {
			top = append(top, run)
		}
	}
	out, err := teams.By(r.Context(), s.a.Config(), top, teams.ByOthers)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if len(out) > othersLimit {
		out = out[:othersLimit]
	}
	writeJSON(w, http.StatusOK, map[string]any{"runs": out})
}

// result answers GET /v1/runs/{id}/result with what the run produced and
// what it needs next (dashboard.Result). The read authorizes the caller as
// GET /v1/runs/{id} does.
func (s *server) result(w http.ResponseWriter, r *http.Request) {
	res, err := dashboard.ResultOf(r.Context(), s.a, r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// transcript answers GET /v1/runs/{id}/transcript: the run object and its
// chat items (see dashboard.Transcript), as {"run", "items", "next",
// "prev"}. limit bounds the events that the page reads.
//
// The query parameter from reads forward from that sequence number, and
// next is the sequence number to ask from when more events follow, and 0
// when none do. The query parameter before reads back: the last limit
// events before that sequence number. prev is the sequence number to pass
// as before for the page in front, and 0 at the first page. Without from
// and before, the page holds the last limit events, so a long conversation
// opens at its recent end. from and before together answer 400. The read
// of the run authorizes the caller, as for the events.
func (s *server) transcript(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	run, err := s.a.Get(r.Context(), id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	q := r.URL.Query()
	if q.Get("from") != "" && q.Get("before") != "" {
		problem(w, r, http.StatusBadRequest, "the query parameters from and before do not go together; from reads forward and before reads back")
		return
	}
	var from, before uint64
	for _, p := range []struct {
		name string
		v    *uint64
	}{{"from", &from}, {"before", &before}} {
		if v := q.Get(p.name); v != "" {
			n, err := strconv.ParseUint(v, 10, 64)
			if err != nil {
				problem(w, r, http.StatusBadRequest, "the query parameter "+p.name+" must be a sequence number")
				return
			}
			*p.v = n
		}
	}
	limit := defaultEventsLimit
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			problem(w, r, http.StatusBadRequest, "the query parameter limit must be a positive number")
			return
		}
		limit = min(n, maxEventsLimit)
	}
	var events []fw.Event
	var next, prev uint64
	if from > 0 {
		events, next, err = view.EventsPage(r.Context(), s.a, id, from, limit)
		if from > 1 {
			// The sequence numbers of a run start at 1, so events come
			// before the page.
			prev = from
		}
	} else {
		events, prev, err = dashboard.TranscriptBefore(r.Context(), s.a, id, before, limit)
		if before > 0 && before <= run.Seq {
			// The events from before on follow the page.
			next = before
		}
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"run": run, "items": dashboard.Transcript(events), "next": next, "prev": prev})
}

// ui serves the dashboard page. The policy header lets the page run its
// own inline script and style and call this origin only. It limits no
// navigation, so the page can send the person to /ui/signin.
func (s *server) ui(w http.ResponseWriter, _ *http.Request) {
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Content-Security-Policy", "default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; connect-src 'self'; img-src data:; base-uri 'none'; form-action 'none'")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(dashboard.Page())
}

// protectedResource serves the RFC 9728 document that tells an OAuth
// client which authorization servers issue tokens for this server. The
// resource identifier is mcp.auth.audience, else a2a.url, else the origin
// of the request.
func (s *server) protectedResource(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, ProtectedResource(s.a.Config(), requestOrigin(r)))
}

// ProtectedResource builds the RFC 9728 protected-resource metadata of a
// configuration. The fallback names the resource when the configuration
// does not. The authorization servers come from mcp.auth.authorizationServers,
// or from the OIDC issuer when that list is empty.
func ProtectedResource(cfg *fw.Config, fallback string) map[string]any {
	resource := strings.TrimSpace(cfg.MCP.Auth.Audience)
	if resource == "" {
		resource = strings.TrimSpace(cfg.A2A.URL)
	}
	if resource == "" {
		resource = fallback
	}
	servers := append([]string(nil), cfg.MCP.Auth.AuthorizationServers...)
	if len(servers) == 0 && cfg.Auth.OIDC != nil && cfg.Auth.OIDC.Issuer != "" {
		servers = []string{cfg.Auth.OIDC.Issuer}
	}
	if servers == nil {
		servers = []string{}
	}
	doc := map[string]any{
		"resource":                 resource,
		"authorization_servers":    servers,
		"bearer_methods_supported": []string{"header"},
	}
	if cfg.Auth.OIDC != nil && len(cfg.Auth.OIDC.Scopes) > 0 {
		doc["scopes_supported"] = cfg.Auth.OIDC.Scopes
	}
	return doc
}

// requestOrigin is the scheme and host of a request, as a proxy forwarded
// it when the headers say so. A chain of proxies lists several values in
// one header, and the first one is what the client sent, as isHTTPS reads
// it.
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
	return scheme + "://" + host
}

// firstForwarded is the first comma-separated value of a forwarded header,
// trimmed.
func firstForwarded(r *http.Request, header string) string {
	v, _, _ := strings.Cut(r.Header.Get(header), ",")
	return strings.TrimSpace(v)
}
