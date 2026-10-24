package notify

// This file reads the configuration of the notifications, the block
// notifications under the top-level key workbench of fw.yaml. Nothing in
// it sends a notification by itself: a person adds a channel, or a duty
// names a shared channel. The front never picks a public ntfy server, an
// SMTP server, or an agent by itself.

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/mail"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/internal/hostguard"
	"github.com/bdubs00/featherweight-workbench/teams"
)

// Config is the block workbench.notifications.
type Config struct {
	// Ntfy is the ntfy server of the front and the hosts that a person
	// may name as an ntfy server.
	Ntfy NtfyConfig `yaml:"ntfy"`
	// Hosts lists the private hosts that a channel may reach, as globs
	// such as *.lab.internal. A public host needs no entry.
	Hosts []string `yaml:"hosts"`
	// Link is the base address of the page, such as
	// https://fleet.example.com/bossmode/, for the link of a
	// notification. Without it, a notification has no link.
	Link string `yaml:"link"`
	// Email is the SMTP server of the kind email. Without a host, the
	// front does not offer email.
	Email EmailConfig `yaml:"email"`
	// Agent turns on the kind agent and bounds its runs.
	Agent AgentConfig `yaml:"agent"`
	// Shared lists the channels of the operator that a person picks by
	// name and that a duty names as channel:<name>.
	Shared []SharedConfig `yaml:"shared"`
}

// NtfyConfig is the block workbench.notifications.ntfy. Server is the ntfy
// server of a channel that names none, such as https://ntfy.example.com,
// and "" for none. Hosts lists the hosts that a person may name as a
// server of their own, as globs such as ntfy.example.com. An empty list
// lets a person name any host. The host of Server is always allowed.
type NtfyConfig struct {
	Server string   `yaml:"server"`
	Hosts  []string `yaml:"hosts"`
}

// EmailConfig is the block workbench.notifications.email: the SMTP server
// (Host and Port, 587 when Port is 0), the address From of each mail, and
// the account User with the variable PasswordEnv that holds its password.
// Without User, the front sends without a login.
type EmailConfig struct {
	Host        string `yaml:"host"`
	Port        int    `yaml:"port"`
	From        string `yaml:"from"`
	User        string `yaml:"user"`
	PasswordEnv string `yaml:"passwordEnv"`
}

// AgentConfig is the block workbench.notifications.agent. Enabled offers
// the kind agent. PerDay bounds the runs of the kind agent per person and
// day, DefaultAgentPerDay when it is 0.
type AgentConfig struct {
	Enabled bool `yaml:"enabled"`
	PerDay  int  `yaml:"perDay"`
}

// SharedConfig is one shared channel of the operator: its name, its label,
// its kind, the variable URLEnv that holds its address, and the variable
// SecretEnv that holds its secret, if it has one: the key of the HMAC of a
// webhook, or the access token of an ntfy server. The address of a shared
// channel of the kind ntfy is the address of its topic, such as
// https://ntfy.example.com/ops.
type SharedConfig struct {
	Name      string `yaml:"name"`
	Label     string `yaml:"label"`
	Kind      string `yaml:"kind"`
	URLEnv    string `yaml:"urlEnv"`
	SecretEnv string `yaml:"secretEnv"`
}

// Defaults and bounds of the configuration.
const (
	// DefaultAgentPerDay bounds the runs of the kind agent per person and
	// day when workbench.notifications.agent.perDay is 0.
	DefaultAgentPerDay = 20
	// DefaultSMTPPort is the port of the SMTP server when
	// workbench.notifications.email.port is 0.
	DefaultSMTPPort = 587
	// maxLabel bounds the label of a channel, in bytes.
	maxLabel = 80
)

// block is the part of the workbench block that this package reads.
type block struct {
	Notifications Config `yaml:"notifications"`
}

// sharedPattern is the form of the name of a shared channel.
var sharedPattern = regexp.MustCompile(`^[a-z0-9_-]{1,63}$`)

// envPattern is the form of the name of a variable.
var envPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

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
	add := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }
	c.Ntfy.Server = strings.TrimRight(strings.TrimSpace(c.Ntfy.Server), "/")
	if c.Ntfy.Server != "" {
		if why := serverProblem(c.Ntfy.Server); why != "" {
			add("workbench.notifications.ntfy.server %q: %s", c.Ntfy.Server, why)
		}
	}
	for i, h := range c.Ntfy.Hosts {
		g, ok := hostguard.CleanGlob(h)
		c.Ntfy.Hosts[i] = g
		if !ok {
			add("workbench.notifications.ntfy.hosts[%d] %q is not a host or a glob of hosts", i, h)
		}
	}
	for i, h := range c.Hosts {
		g, ok := hostguard.CleanGlob(h)
		c.Hosts[i] = g
		if !ok {
			add("workbench.notifications.hosts[%d] %q is not a host or a glob of hosts", i, h)
		}
	}
	c.Link = strings.TrimSpace(c.Link)
	if c.Link != "" {
		u, err := url.Parse(c.Link)
		switch {
		case err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "":
			add("workbench.notifications.link %q: write the http or https address of the page, such as https://fleet.example.com/bossmode/", c.Link)
		case u.User != nil:
			add("workbench.notifications.link %q: the address holds a user or a password", c.Link)
		case u.Fragment != "" || strings.Contains(c.Link, "#"):
			add("workbench.notifications.link %q: the address has a fragment; the front adds #activity/<run> or #duties/<name> itself", c.Link)
		}
	}
	e := &c.Email
	e.Host, e.From, e.User, e.PasswordEnv = strings.TrimSpace(e.Host), strings.TrimSpace(e.From), strings.TrimSpace(e.User), strings.TrimSpace(e.PasswordEnv)
	switch {
	case e.Host == "" && (e.From != "" || e.User != "" || e.PasswordEnv != "" || e.Port != 0):
		add("workbench.notifications.email.host is empty: name the SMTP server, or leave out the block email")
	case e.Host != "":
		e.Host = strings.Trim(e.Host, "[]")
		if strings.ContainsAny(e.Host, "/ \t") || (strings.Contains(e.Host, ":") && net.ParseIP(e.Host) == nil) {
			add("workbench.notifications.email.host %q: write the name of the SMTP server alone, such as smtp.example.com, and its port in port", e.Host)
		}
		if e.Port < 0 || e.Port > 65535 {
			add("workbench.notifications.email.port %d: write a port from 1 to 65535, or 0 for %d", e.Port, DefaultSMTPPort)
		}
		if a, err := mail.ParseAddress(e.From); e.From == "" || err != nil || a.Address == "" {
			add("workbench.notifications.email.from %q: write the address of the sender, such as featherweight@example.com", e.From)
		}
		if e.User != "" && e.PasswordEnv == "" {
			add("workbench.notifications.email.passwordEnv is empty: name the variable that holds the password of %s", e.User)
		}
		if e.PasswordEnv != "" && !envPattern.MatchString(e.PasswordEnv) {
			add("workbench.notifications.email.passwordEnv %q is not the name of a variable", e.PasswordEnv)
		}
	}
	if c.Agent.PerDay < 0 {
		add("workbench.notifications.agent.perDay %d is below zero: write 0 for %d", c.Agent.PerDay, DefaultAgentPerDay)
	}
	seen := map[string]bool{}
	for i := range c.Shared {
		sc := &c.Shared[i]
		sc.Name, sc.Label, sc.Kind = strings.TrimSpace(sc.Name), strings.TrimSpace(sc.Label), strings.ToLower(strings.TrimSpace(sc.Kind))
		sc.URLEnv, sc.SecretEnv = strings.TrimSpace(sc.URLEnv), strings.TrimSpace(sc.SecretEnv)
		key := fmt.Sprintf("workbench.notifications.shared[%d]", i)
		switch {
		case !sharedPattern.MatchString(sc.Name):
			add("%s.name %q: write 1 to 63 of a-z, 0-9, '-', and '_', such as ops-room", key, sc.Name)
		case seen[sc.Name]:
			add("%s.name %q names two shared channels", key, sc.Name)
		}
		seen[sc.Name] = true
		if !sharedKinds[sc.Kind] {
			add("%s.kind %q: pick ntfy, slack, discord, teams, or webhook", key, sc.Kind)
		}
		if !envPattern.MatchString(sc.URLEnv) {
			add("%s.urlEnv %q: name the variable that holds the address of the channel", key, sc.URLEnv)
		}
		if sc.SecretEnv != "" && !envPattern.MatchString(sc.SecretEnv) {
			add("%s.secretEnv %q is not the name of a variable", key, sc.SecretEnv)
		}
		if sc.Label == "" {
			sc.Label = sc.Name
		}
		if len(sc.Label) > maxLabel || strings.ContainsFunc(sc.Label, isControl) {
			add("%s.label: write at most %d characters on one line", key, maxLabel)
		}
	}
	if len(problems) > 0 {
		return Config{}, fmt.Errorf("notify: %s", strings.Join(problems, "; "))
	}
	return c, nil
}

// sharedKinds are the kinds of a shared channel.
var sharedKinds = map[string]bool{KindNtfy: true, KindSlack: true, KindDiscord: true, KindTeams: true, KindWebhook: true}

// perDay returns the bound of the runs of the kind agent per person and
// day.
func (c Config) perDay() int {
	if c.Agent.PerDay > 0 {
		return c.Agent.PerDay
	}
	return DefaultAgentPerDay
}

// smtpPort returns the port of the SMTP server.
func (c Config) smtpPort() int {
	if c.Email.Port > 0 {
		return c.Email.Port
	}
	return DefaultSMTPPort
}

// sharedNamed returns the shared channel of the name, or nil.
func (c Config) sharedNamed(name string) *SharedConfig {
	for i := range c.Shared {
		if c.Shared[i].Name == name {
			return &c.Shared[i]
		}
	}
	return nil
}

// Offer is a kind of channel that the front serves now, with its label.
type Offer struct {
	Kind  string `json:"kind"`
	Label string `json:"label"`
}

// kinds lists every kind of channel with its label, in the order of the
// page.
var kinds = []Offer{
	{KindNtfy, "ntfy"}, {KindSlack, "Slack"}, {KindDiscord, "Discord"}, {KindTeams, "Microsoft Teams"}, {KindWebhook, "Webhook"},
	{KindEmail, "Email"}, {KindAgent, "Agent"}, {KindShared, "Shared channel"},
}

// offers reports whether the front serves the kind now: ntfy, slack,
// discord, teams, and webhook always, email with an SMTP server, agent
// when the operator turned it on, and shared with a shared channel.
func (c Config) offers(kind string) bool {
	switch kind {
	case KindNtfy, KindSlack, KindDiscord, KindTeams, KindWebhook:
		return true
	case KindEmail:
		return c.Email.Host != ""
	case KindAgent:
		return c.Agent.Enabled
	case KindShared:
		return len(c.Shared) > 0
	}
	return false
}

// offered lists the kinds that the front serves now.
func (c Config) offered() []Offer {
	var list []Offer
	for _, k := range kinds {
		if c.offers(k.Kind) {
			list = append(list, k)
		}
	}
	return list
}

// kindLabel returns the label of a kind.
func kindLabel(kind string) string {
	for _, k := range kinds {
		if k.Kind == kind {
			return k.Label
		}
	}
	return kind
}

// linkOf returns the link of a notification: #activity/<run> for a run,
// #duties/<name> for a duty, and the page for any other, after the base
// address of workbench.notifications.link. Without that key, it is "".
func (c Config) linkOf(m *message) string {
	switch {
	case c.Link == "":
		return ""
	case m.Run != "":
		return c.Link + "#activity/" + url.PathEscape(m.Run)
	case m.Duty != "":
		return c.Link + "#duties/" + url.PathEscape(m.Duty)
	}
	return c.Link
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

// listed reports whether a host is one that the operator names: a host of
// workbench.notifications.hosts.
func (c Config) listed(host string) bool {
	return host != "" && hostguard.Match(c.Hosts, host)
}

// ntfyListed reports whether the host of an ntfy server is one that the
// operator names: the host of the server of the configuration, a host of
// workbench.notifications.ntfy.hosts, or a host of
// workbench.notifications.hosts.
func (c Config) ntfyListed(host string) bool {
	if host == "" {
		return false
	}
	if c.Ntfy.Server != "" && hostOf(c.Ntfy.Server) == host {
		return true
	}
	return hostguard.Match(c.Ntfy.Hosts, host) || c.listed(host)
}

// ntfyHostProblem says why a person may not name the ntfy server raw, or
// "": a host that workbench.notifications.ntfy.hosts does not list, when
// the list is not empty.
func (c Config) ntfyHostProblem(raw string) string {
	host := hostOf(raw)
	if len(c.Ntfy.Hosts) == 0 || c.ntfyListed(host) {
		return ""
	}
	return fmt.Sprintf("the host %s is not one that workbench.notifications.ntfy.hosts lists; an operator adds it there first", host)
}

// errPrivate means that an address of a channel resolved to an address
// that the front does not reach for a person.
var errPrivate = errors.New("the address resolves to a loopback, private, or link-local address, which the front reaches only for a host of workbench.notifications.hosts")

// clients returns the HTTP clients of the channels: one for the hosts
// that the operator names, and one for any other host, whose dialer
// refuses a loopback, private, or link-local address after the name
// resolves (hostguard.Transport). Neither follows a redirect.
func (c Config) clients() (listed, public *http.Client) {
	build := func(refuse error) *http.Client {
		return &http.Client{Transport: hostguard.Transport(refuse), Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error {
			return hostguard.ErrRedirect
		}}
	}
	return build(nil), build(errPrivate)
}
