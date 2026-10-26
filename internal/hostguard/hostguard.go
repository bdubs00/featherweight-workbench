// Package hostguard holds what the checks of the duties and the
// notifications share about the hosts that the front reaches for a
// person: the rule of a glob of a host, and the transport whose dialer
// refuses a loopback, private, or link-local address after the name
// resolves (docs/ARCHITECTURE.md sections 22 and 23).
//
// A glob follows path.Match of Go. It matches the host name of the
// address in lower case, without the port. A * matches any run of
// characters, dots too, so *.example.com matches a.b.example.com and not
// example.com.
package hostguard

import (
	"context"
	"errors"
	"net"
	"net/http"
	"path"
	"strings"
	"syscall"
	"time"
)

// Match reports whether host matches one of the globs. The globs are in
// lower case already, as ValidGlob leaves them.
func Match(globs []string, host string) bool {
	host = strings.ToLower(host)
	for _, g := range globs {
		if ok, _ := path.Match(g, host); ok {
			return true
		}
	}
	return false
}

// CleanGlob trims a glob and writes it in lower case. It reports false
// for an empty glob and for a glob that path.Match does not read.
func CleanGlob(g string) (string, bool) {
	g = strings.ToLower(strings.TrimSpace(g))
	if g == "" {
		return g, false
	}
	if _, err := path.Match(g, "x"); err != nil {
		return g, false
	}
	return g, true
}

// Blocked reports whether the front refuses to dial ip for a person: a
// loopback, private, link-local, unspecified, or multicast address, or no
// address at all.
func Blocked(ip net.IP) bool {
	return ip == nil || ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast()
}

// Private reports whether a host name is a private address before any
// lookup: localhost, or an address that Blocked refuses. A name that
// resolves to such an address only shows at the dial.
func Private(host string) bool {
	host = strings.ToLower(strings.Trim(host, "[]"))
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && Blocked(ip)
}

// Transport returns a clone of the default transport of Go. When refuse is
// not nil, its dialer fails with refuse for an address that Blocked
// refuses, after the name resolves, so a name that resolves to a private
// address does not pass either.
func Transport(refuse error) *http.Transport {
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	if refuse != nil {
		dialer.Control = func(_, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			if Blocked(net.ParseIP(host)) {
				return refuse
			}
			return nil
		}
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		return dialer.DialContext(ctx, network, addr)
	}
	return tr
}

// ErrRedirect is the error of a client that follows no redirect.
var ErrRedirect = errors.New("the server answered with a redirect, which the server does not follow")
