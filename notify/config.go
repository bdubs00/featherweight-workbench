package notify

// This file reads the configuration of the notifications, the block
// notifications under the top-level key workbench of fw.yaml. The front
// never picks a public ntfy server by itself: without a server of the
// person and without workbench.notifications.ntfy.server, the
// notifications of that person are off.

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"path"
	"strings"
	"syscall"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/teams"
)

// Config is the block workbench.notifications.
type Config struct {
	Ntfy NtfyConfig `yaml:"ntfy"`
}

// NtfyConfig is the block workbench.notifications.ntfy. Server is the ntfy
// server of a person who names none, such as https://ntfy.example.com, and
// "" for none. Hosts lists the hosts that a person may name as a server of
// their own, as globs such as ntfy.example.com. An empty list lets a
// person name any host on a public address, and never a loopback,
// private, or link-local address. The host of Server is always allowed.
type NtfyConfig struct {
	Server string   `yaml:"server"`
	Hosts  []string `yaml:"hosts"`
}

// block is the part of the workbench block that this package reads.
type block struct {
	Notifications Config `yaml:"notifications"`
}

// LoadConfig reads workbench.notifications and checks it. It returns an
// error that names the key of a value that does not fit.
func LoadConfig(cfg *fw.Config) (Config, error) {
	var b block
	if cfg != nil {
		if _, err := cfg.Extension(teams.Block, &b); err != nil {
			return Config{}, fmt.Errorf("notify: %w", err)
		}
	}
	c := b.Notifications
	var problems []string
	c.Ntfy.Server = strings.TrimRight(strings.TrimSpace(c.Ntfy.Server), "/")
	if c.Ntfy.Server != "" {
		if why := serverProblem(c.Ntfy.Server); why != "" {
			problems = append(problems, fmt.Sprintf("workbench.notifications.ntfy.server %q: %s", c.Ntfy.Server, why))
		}
	}
	for i, h := range c.Ntfy.Hosts {
		c.Ntfy.Hosts[i] = strings.ToLower(strings.TrimSpace(h))
		if _, err := path.Match(c.Ntfy.Hosts[i], "x"); err != nil || c.Ntfy.Hosts[i] == "" {
			problems = append(problems, fmt.Sprintf("workbench.notifications.ntfy.hosts[%d] %q is not a host or a glob of hosts", i, h))
		}
	}
	if len(problems) > 0 {
		return Config{}, fmt.Errorf("notify: %s", strings.Join(problems, "; "))
	}
	return c, nil
}

// serverProblem says what does not fit in the address of an ntfy server,
// or "".
func serverProblem(raw string) string {
	u, err := url.Parse(raw)
	switch {
	case err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.Hostname() == "":
		return "write an http or https address, such as https://ntfy.example.com"
	case u.User != nil:
		return "the address holds a user or a password; put the token in ntfy.token instead"
	case u.RawQuery != "" || u.Fragment != "" || strings.HasSuffix(raw, "?"):
		return "the address has a query or a fragment; write the address of the server alone"
	}
	return ""
}

// hostOf returns the host of an address without its port, in lower case.
func hostOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Hostname())
}

// listed reports whether a host is the host of the server of the
// configuration or matches one of its hosts.
func (c Config) listed(host string) bool {
	if host == "" {
		return false
	}
	if c.Ntfy.Server != "" && hostOf(c.Ntfy.Server) == host {
		return true
	}
	for _, g := range c.Ntfy.Hosts {
		if ok, _ := path.Match(g, host); ok {
			return true
		}
	}
	return false
}

// hostProblem says why a person may not name the server raw, or "": a
// host that workbench.notifications.ntfy.hosts does not list, when the
// list is not empty.
func (c Config) hostProblem(raw string) string {
	host := hostOf(raw)
	if len(c.Ntfy.Hosts) == 0 || c.listed(host) {
		return ""
	}
	return fmt.Sprintf("the host %s is not one that workbench.notifications.ntfy.hosts lists; an operator adds it there first", host)
}

// errPrivate means that a server of a person resolved to an address that
// the front does not reach for a person.
var errPrivate = errors.New("the server resolves to a loopback, private, or link-local address, which the front reaches only for the server of workbench.notifications.ntfy or a host of workbench.notifications.ntfy.hosts")

// clients returns the HTTP clients of the ntfy servers: one for the
// servers that the configuration names, and one for any other server of a
// person, whose dialer refuses a loopback, private, link-local, or
// unspecified address after the name resolves. Neither follows a
// redirect.
func (c Config) clients() (listed, public *http.Client) {
	build := func(guard bool) *http.Client {
		dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
		if guard {
			dialer.Control = func(_, address string, _ syscall.RawConn) error {
				host, _, err := net.SplitHostPort(address)
				if err != nil {
					return err
				}
				ip := net.ParseIP(host)
				if ip == nil || ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast() {
					return errPrivate
				}
				return nil
			}
		}
		tr := http.DefaultTransport.(*http.Transport).Clone()
		tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			return dialer.DialContext(ctx, network, addr)
		}
		return &http.Client{Transport: tr, Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error {
			return errors.New("the server answered with a redirect, which the front does not follow")
		}}
	}
	return build(false), build(true)
}
