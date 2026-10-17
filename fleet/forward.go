package fleet

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// headerTargetActor routes a request through the atenet router.
const headerTargetActor = "ate-target-actor"

// forwardHeaders are the headers of a request that reach the owner.
var forwardHeaders = []string{"Accept", "Content-Type", "Idempotency-Key", "Last-Event-ID"}

// Timeouts of a forwarded request. The client has no overall limit,
// because an event stream stays open. The router resumes a Task that
// sleeps before it answers, so the response headers can take a while.
const (
	dialTimeout           = 10 * time.Second
	tlsTimeout            = 10 * time.Second
	responseHeaderTimeout = 2 * time.Minute
)

func newClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           (&net.Dialer{Timeout: dialTimeout, KeepAlive: 30 * time.Second}).DialContext,
			TLSHandshakeTimeout:   tlsTimeout,
			ResponseHeaderTimeout: responseHeaderTimeout,
			IdleConnTimeout:       90 * time.Second,
			MaxIdleConns:          32,
			ForceAttemptHTTP2:     true,
		},
		// A redirect sends the credential to an address that the
		// configuration did not name. The caller gets the redirect itself.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// Forward implements fw.Fleet. The credential goes to an ax address through
// the router, or to a URL that a glob of fleet.forward names, and nowhere
// else.
func (f *Fleet) Forward(ctx context.Context, member string, req *http.Request) (*http.Response, error) {
	if req == nil || req.URL == nil {
		return nil, fmt.Errorf("fleet: forward to %s: no request", member)
	}
	if via := req.Header.Get(fw.HeaderFleetVia); via != "" {
		return nil, fmt.Errorf("fleet: %w: the view %s forwarded this request, and a member forwards a request once only", fw.ErrFleetNoRoute, via)
	}
	if member == f.name {
		return nil, fmt.Errorf("fleet: %w: %s is this member", fw.ErrFleetNoRoute, member)
	}
	path := req.URL.Path
	if !strings.HasPrefix(path, "/v1/") {
		return nil, fmt.Errorf("fleet: forward to %s: the path %q is not under /v1/", member, path)
	}
	if err := f.startView(ctx); err != nil {
		return nil, err
	}
	var rec *fw.FleetRecord
	for _, r := range f.remotes(time.Now()) {
		if r.rec.Name == member {
			rec = r.rec
			break
		}
	}
	if rec == nil {
		return nil, fmt.Errorf("fleet: %w: the view knows no member %q", fw.ErrFleetNoRoute, member)
	}
	base, actor, err := f.route(rec.URL)
	if err != nil {
		return nil, fmt.Errorf("fleet: %w: %s: %v", fw.ErrFleetNoRoute, member, err)
	}
	cred := fw.CredentialFrom(ctx)
	if cred == "" {
		return nil, fmt.Errorf("fleet: forward to %s: %w", member, fw.ErrFleetNoCredential)
	}
	u := *base
	u.Path = strings.TrimSuffix(base.Path, "/") + path
	u.RawPath = ""
	u.RawQuery = req.URL.RawQuery
	var body = req.Body
	if body == http.NoBody {
		body = nil
	}
	out, err := http.NewRequestWithContext(ctx, req.Method, u.String(), body)
	if err != nil {
		return nil, fmt.Errorf("fleet: forward to %s: %w", member, err)
	}
	if body != nil {
		out.ContentLength = req.ContentLength
		out.GetBody = req.GetBody
	}
	for _, h := range forwardHeaders {
		if v := req.Header.Values(h); len(v) > 0 {
			out.Header[http.CanonicalHeaderKey(h)] = append([]string(nil), v...)
		}
	}
	out.Header.Set("Authorization", cred)
	out.Header.Set(fw.HeaderFleetVia, f.name)
	if actor != "" {
		out.Header.Set(headerTargetActor, actor)
	}
	resp, err := f.client.Do(out)
	if err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("fleet: forward to %s: %w", member, ctx.Err())
		}
		return nil, fmt.Errorf("fleet: %w: %s did not answer: %v", fw.ErrFleetNoRoute, member, err)
	}
	return resp, nil
}

// canForward reports whether a URL of a record can receive a credential.
func (f *Fleet) canForward(raw string) bool {
	_, _, err := f.route(raw)
	return err == nil
}

// route returns the base URL of a member and, for an ax address, the value
// of the header ate-target-actor. It refuses an empty URL, a malformed one,
// and an http or https URL that no glob of fleet.forward names.
func (f *Fleet) route(raw string) (*url.URL, string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, "", fmt.Errorf("the member has no URL; set fleet.url on it")
	}
	if target, ok := strings.CutPrefix(raw, "ax:"); ok {
		space, task, ok := strings.Cut(target, "/")
		if !ok || space == "" || task == "" || strings.Contains(task, "/") {
			return nil, "", fmt.Errorf("the URL %q is not ax:<atespace>/<task>", raw)
		}
		if f.router == "" {
			return nil, "", fmt.Errorf("ax.router is empty")
		}
		base, err := url.Parse("http://" + f.router)
		if err != nil || base.Host == "" || base.User != nil {
			return nil, "", fmt.Errorf("ax.router %q is not a host", f.router)
		}
		return base, target, nil
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, "", fmt.Errorf("the URL %q is neither an http or https URL nor an ax address", raw)
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
		return nil, "", fmt.Errorf("the URL %q carries user information, a query, or a fragment", raw)
	}
	canon := u.Scheme + "://" + u.Host + strings.TrimSuffix(u.EscapedPath(), "/")
	for _, g := range f.forward {
		if globMatch(strings.TrimSuffix(strings.TrimSpace(g), "/"), canon) {
			return u, "", nil
		}
	}
	return nil, "", fmt.Errorf("the URL %s matches no glob of fleet.forward", canon)
}

// globMatch matches a glob over a URL. A "*" matches any text without a
// slash, so a glob over a host never matches a path that ends like a host.
// Every other character matches itself, and case does not matter.
func globMatch(glob, s string) bool {
	glob, s = strings.ToLower(glob), strings.ToLower(s)
	// match[j] reports whether the glob so far matches s[:j].
	match := make([]bool, len(s)+1)
	match[0] = true
	for i := 0; i < len(glob); i++ {
		next := make([]bool, len(s)+1)
		if glob[i] == '*' {
			next[0] = match[0]
			for j := 1; j <= len(s); j++ {
				next[j] = match[j] || (next[j-1] && s[j-1] != '/')
			}
		} else {
			for j := 1; j <= len(s); j++ {
				next[j] = match[j-1] && s[j-1] == glob[i]
			}
		}
		match = next
	}
	return match[len(s)]
}
