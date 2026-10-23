package duties

// This file reads the configuration of the duties, the block duties under
// the top-level key workbench of fw.yaml, and keeps the two rules of the
// operator that a duty cannot change: which secrets of the front a duty
// may name, and which hosts a check may reach. A person who owns an agent
// writes a duty, so a duty never names a secret or reaches a host that the
// operator did not allow.

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"path"
	"strings"
	"syscall"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/sources"
	"github.com/bdubs00/featherweight-workbench/teams"
)

// Config is the block workbench.duties. Secrets lists the variables of the
// front that a duty may name, each with the hosts that a check may send
// it to; a webhook may name any variable of the list. CheckHosts lists
// the hosts that a check may reach, as globs such as *.example.com. An
// empty list lets a check reach any host on a public address, and never a
// loopback, private, or link-local address.
type Config struct {
	Secrets    []SecretRule `yaml:"secrets"`
	CheckHosts []string     `yaml:"checkHosts"`
}

// SecretRule allows a duty to name the variable Name, and a check to send
// its value to the hosts of Hosts, globs such as api.example.com.
type SecretRule struct {
	Name  string   `yaml:"name"`
	Hosts []string `yaml:"hosts"`
}

// block is the part of the workbench block that this package reads.
type block struct {
	Duties Config `yaml:"duties"`
}

// LoadConfig reads workbench.duties and checks it. It returns an error
// that names the key of a value that does not fit.
func LoadConfig(cfg *fw.Config) (Config, error) {
	var b block
	if cfg != nil {
		if _, err := cfg.Extension(teams.Block, &b); err != nil {
			return Config{}, fmt.Errorf("duties: %w", err)
		}
	}
	c := b.Duties
	var problems []string
	for i, r := range c.Secrets {
		c.Secrets[i].Name = strings.TrimSpace(r.Name)
		if !envPattern.MatchString(c.Secrets[i].Name) {
			problems = append(problems, fmt.Sprintf("workbench.duties.secrets[%d].name %q is not the name of a variable", i, r.Name))
		}
		for j, h := range r.Hosts {
			c.Secrets[i].Hosts[j] = strings.ToLower(strings.TrimSpace(h))
			if _, err := path.Match(c.Secrets[i].Hosts[j], "x"); err != nil || c.Secrets[i].Hosts[j] == "" {
				problems = append(problems, fmt.Sprintf("workbench.duties.secrets[%d].hosts[%d] %q is not a host or a glob of hosts", i, j, h))
			}
		}
	}
	for i, h := range c.CheckHosts {
		c.CheckHosts[i] = strings.ToLower(strings.TrimSpace(h))
		if _, err := path.Match(c.CheckHosts[i], "x"); err != nil || c.CheckHosts[i] == "" {
			problems = append(problems, fmt.Sprintf("workbench.duties.checkHosts[%d] %q is not a host or a glob of hosts", i, h))
		}
	}
	if len(problems) > 0 {
		return Config{}, fmt.Errorf("duties: %s", strings.Join(problems, "; "))
	}
	return c, nil
}

// matchHost reports whether host matches one of the globs.
func matchHost(globs []string, host string) bool {
	host = strings.ToLower(host)
	for _, g := range globs {
		if ok, _ := path.Match(g, host); ok {
			return true
		}
	}
	return false
}

// secretRule returns the rule of a variable, or nil.
func (c Config) secretRule(name string) *SecretRule {
	for i := range c.Secrets {
		if c.Secrets[i].Name == name {
			return &c.Secrets[i]
		}
	}
	return nil
}

// lookProblems lists what the rules of the operator refuse in the HTTP
// check of a duty: a secret that the list does not name, a host that the
// secret may not reach, and a host outside checkHosts.
func (c Config) lookProblems(h *LookHTTP) []string {
	var p []string
	host := hostOf(h.URL)
	if host == "" {
		return nil
	}
	if len(c.CheckHosts) > 0 && !matchHost(c.CheckHosts, host) {
		p = append(p, fmt.Sprintf("checkFirst.look.http.url reaches the host %s, which workbench.duties.checkHosts does not list: an operator adds it there.", host))
	}
	if h.Secret != "" {
		switch r := c.secretRule(h.Secret); {
		case r == nil:
			p = append(p, fmt.Sprintf("checkFirst.look.http.secret names %s, which workbench.duties.secrets does not list: an operator adds it there, with the hosts that it may reach.", h.Secret))
		case !matchHost(r.Hosts, host):
			p = append(p, fmt.Sprintf("checkFirst.look.http.secret names %s, which workbench.duties.secrets does not allow for the host %s.", h.Secret, host))
		}
	}
	return p
}

// webhookProblems lists what the rules of the operator refuse in the
// credential of a webhook.
func (c Config) webhookProblems(w *Webhook) []string {
	if w.Auth.Secret == "" || c.secretRule(w.Auth.Secret) != nil {
		return nil
	}
	return []string{fmt.Sprintf("when.webhook.auth.secret names %s, which workbench.duties.secrets does not list: an operator adds it there.", w.Auth.Secret)}
}

// hostOf returns the host of an address without its port, or "".
func hostOf(raw string) string {
	req, err := http.NewRequest(http.MethodGet, raw, nil)
	if err != nil {
		return ""
	}
	return strings.ToLower(req.URL.Hostname())
}

// errBlockedAddress means that a check met an address that it may not
// reach.
var errBlockedAddress = errors.New("the address is loopback, private, or link-local, and workbench.duties.checkHosts lists no host")

// client returns the HTTP client of the checks. A redirect goes to the
// same scheme and host only, as for a poll source. Without checkHosts, the
// dialer refuses a loopback, private, link-local, or unspecified address
// after the name resolves, so a name that resolves to such an address
// does not pass either.
func (c Config) client() *http.Client {
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	if len(c.CheckHosts) == 0 {
		dialer.Control = func(_, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			ip := net.ParseIP(host)
			if ip == nil || ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast() {
				return errBlockedAddress
			}
			return nil
		}
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		return dialer.DialContext(ctx, network, addr)
	}
	return &http.Client{Transport: tr, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if err := sources.SameHost(req, via); err != nil {
			return err
		}
		if len(c.CheckHosts) > 0 && !matchHost(c.CheckHosts, req.URL.Hostname()) {
			return errors.New("the endpoint redirects to a host that workbench.duties.checkHosts does not list")
		}
		return nil
	}}
}
