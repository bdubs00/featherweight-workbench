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

// Forward implements View. The credential goes to an ax address through
// the router, or to a URL that a glob of fleet.forward names, and nowhere
// else.
func (f *Fleet) Forward(ctx context.Context, member string, req *http.Request) (*http.Response, error) {
	if req == nil || req.URL == nil {
		return nil, fmt.Errorf("fleet: forward to %s: no request", member)
	}
	if via := req.Header.Get(HeaderVia); via != "" {
		return nil, fmt.Errorf("fleet: %w: the view %s forwarded this request, and a member forwards a request once only", ErrNoRoute, via)
	}
	if member == f.name {
		return nil, fmt.Errorf("fleet: %w: %s is this member", ErrNoRoute, member)
	}
	path := req.URL.Path
	if !strings.HasPrefix(path, "/v1/") {
		return nil, fmt.Errorf("fleet: forward to %s: the path %q is not under /v1/", member, path)
	}
	var rec *Record
	for _, r := range f.remotes(time.Now()) {
		if r.rec.Name == member {
			rec = r.rec
			break
		}
	}
	if rec == nil {
		return nil, fmt.Errorf("fleet: %w: the view knows no member %q", ErrNoRoute, member)
	}
	base, actor, err := f.route(rec.URL)
	if err != nil {
		return nil, fmt.Errorf("fleet: %w: %s: %v", ErrNoRoute, member, err)
	}
	cred := fw.CredentialFrom(ctx)
	if cred == "" {
		return nil, fmt.Errorf("fleet: forward to %s: %w", member, ErrNoCredential)
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
	out.Header.Set(HeaderVia, f.name)
	if actor != "" {
		out.Header.Set(headerTargetActor, actor)
	}
	resp, err := f.client.Do(out)
	if err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("fleet: forward to %s: %w", member, ctx.Err())
		}
		return nil, fmt.Errorf("fleet: %w: %s did not answer: %v", ErrNoRoute, member, err)
	}
	if actor != "" && f.platform != nil {
		// The Task answered, so it is awake; its phase follows (suspend.go).
		f.platform.resumed(member)
	}
	return resp, nil
}

// canForward reports whether this process can send a request to a member.
func (f *Fleet) canForward(raw string) bool {
	_, _, err := f.route(raw)
	return err == nil
}

// route returns the base URL of the router and the value of the header
// ate-target-actor for the address of a member. A member is reached through
// the atenet router only, under its address "ax:<atespace>/<task>": a
// worker that only the stream knows has no address and cannot be reached.
func (f *Fleet) route(raw string) (*url.URL, string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, "", fmt.Errorf("the member has no address; only a Task of an atespace has one")
	}
	target, ok := strings.CutPrefix(raw, "ax:")
	if !ok {
		return nil, "", fmt.Errorf("the address %q is not ax:<atespace>/<task>", raw)
	}
	space, task, ok := strings.Cut(target, "/")
	if !ok || space == "" || task == "" || strings.Contains(task, "/") {
		return nil, "", fmt.Errorf("the address %q is not ax:<atespace>/<task>", raw)
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
