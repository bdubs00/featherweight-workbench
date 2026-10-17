package fleet

// This file reads the configuration of the fleet: the block fleet under the
// top-level key workbench of fw.yaml, which the package teams claims, and
// the variables FW_WORKBENCH_FLEET_*, which win over the file.

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/bdubs00/featherweight-kernel/ax"
	"github.com/bdubs00/featherweight-kernel/fw"
)

// Block is the top-level key of fw.yaml that the workbench claims.
const Block = "workbench"

// Variables that set the configuration. Each wins over the file.
const (
	EnvSubject  = "FW_WORKBENCH_FLEET_SUBJECT"
	EnvName     = "FW_WORKBENCH_FLEET_NAME"
	EnvURL      = "FW_WORKBENCH_FLEET_URL"
	EnvForward  = "FW_WORKBENCH_FLEET_FORWARD"
	EnvInterval = "FW_WORKBENCH_FLEET_INTERVAL"
	EnvExpire   = "FW_WORKBENCH_FLEET_EXPIRE"
)

// Config configures the fleet. An empty Subject turns the fleet off.
//
// Name is the name of this member. The default is "<atespace>/<task>"
// under ax, else "<host>:<port>". URL is the address where other members
// reach this one: an http or https URL, or "ax:<atespace>/<task>" for an
// ax Task, which the atenet router at ax.router reaches. An empty URL makes
// this member read-only to the others. Forward lists globs over the http
// and https URLs that can receive the credential of a caller; an ax
// address always can. In such a glob, "*" matches any text without a
// slash, and the glob matches the scheme, the host, and the path of the
// URL. A URL with user information, a query, or a fragment never receives
// a credential. Interval is the time between two heartbeats, and Expire is
// the age after which the view ignores a record.
type Config struct {
	Subject  string        `yaml:"subject"`
	Name     string        `yaml:"name"`
	URL      string        `yaml:"url"`
	Forward  []string      `yaml:"forward"`
	Interval time.Duration `yaml:"interval"`
	Expire   time.Duration `yaml:"expire"`
}

// block is the part of the workbench block that this package reads.
type block struct {
	Fleet Config `yaml:"fleet"`
}

// Load reads the configuration of the fleet from the block and the
// variables, fills the defaults, and checks the values. It returns an error
// that names the key for a value that does not fit.
func Load(cfg *fw.Config) (Config, error) {
	var b block
	if cfg != nil {
		if _, err := cfg.Extension(Block, &b); err != nil {
			return Config{}, err
		}
	}
	c := b.Fleet
	if v, ok := lookup(EnvSubject); ok {
		c.Subject = v
	}
	if v, ok := lookup(EnvName); ok {
		c.Name = v
	}
	if v, ok := lookup(EnvURL); ok {
		c.URL = v
	}
	if v, ok := lookup(EnvForward); ok {
		c.Forward = nil
		for _, g := range strings.Split(v, ",") {
			if g = strings.TrimSpace(g); g != "" {
				c.Forward = append(c.Forward, g)
			}
		}
	}
	for _, e := range []struct {
		env string
		dst *time.Duration
	}{{EnvInterval, &c.Interval}, {EnvExpire, &c.Expire}} {
		if v, ok := lookup(e.env); ok {
			d, err := time.ParseDuration(v)
			if err != nil {
				return Config{}, fmt.Errorf("fleet: %s=%q is not a duration such as 30s or 2h", e.env, v)
			}
			*e.dst = d
		}
	}
	c.Subject, c.Name, c.URL = strings.TrimSpace(c.Subject), strings.TrimSpace(c.Name), strings.TrimSpace(c.URL)
	if c.Interval <= 0 {
		c.Interval = DefaultInterval
	}
	if c.Expire <= 0 {
		c.Expire = DefaultExpire
	}
	if c.Subject != "" {
		c.Name, c.URL = defaults(c.Name, c.URL, cfg)
	}
	if problems := check(c); len(problems) > 0 {
		return Config{}, fmt.Errorf("fleet: %s", strings.Join(problems, "; "))
	}
	return c, nil
}

func lookup(name string) (string, bool) {
	v, ok := os.LookupEnv(name)
	v = strings.TrimSpace(v)
	return v, ok && v != ""
}

// defaults fills the name and the URL of this member. Under ax, the Task
// gives both. Elsewhere, the name is "<host>:<port>" from the host name and
// the port of the listener, and the URL stays empty.
func defaults(name, u string, cfg *fw.Config) (string, string) {
	if task, _ := ax.Stashed(); task != nil {
		if atespace, taskName := ax.Identity(task); atespace != "" && taskName != "" {
			if name == "" {
				name = atespace + "/" + taskName
			}
			if u == "" {
				u = "ax:" + atespace + "/" + taskName
			}
		}
	}
	if name == "" {
		host, _ := os.Hostname()
		if host == "" {
			host = "fw"
		}
		name = host
		if cfg != nil {
			if _, port, err := net.SplitHostPort(cfg.Listen); err == nil && port != "" {
				name = host + ":" + port
			}
		}
	}
	return name, u
}

// check lists the problems of a configuration: the subject has no empty
// token, space, or wildcard; the name has no space and at most 128
// characters; the URL is http, https, or ax:<atespace>/<task>; each forward
// glob is over http or https URLs; and the durations are not negative.
func check(c Config) []string {
	var p []string
	if subj := c.Subject; subj != "" {
		for _, t := range strings.Split(subj, ".") {
			if t == "" || strings.ContainsAny(t, " \t*>") {
				p = append(p, fmt.Sprintf("workbench.fleet.subject %q is not a subject: it holds an empty token, a space, or a wildcard", subj))
				break
			}
		}
	}
	if n := c.Name; n != "" && (len(n) > 128 || strings.ContainsAny(n, " \t\r\n")) {
		p = append(p, fmt.Sprintf("workbench.fleet.name %q holds a space or is longer than 128 characters", n))
	}
	if raw := c.URL; raw != "" {
		if target, ok := strings.CutPrefix(raw, "ax:"); ok {
			space, task, ok := strings.Cut(target, "/")
			if !ok || space == "" || task == "" || strings.Contains(task, "/") {
				p = append(p, fmt.Sprintf("workbench.fleet.url %q is not ax:<atespace>/<task>", raw))
			}
		} else if u, err := url.Parse(raw); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			p = append(p, fmt.Sprintf("workbench.fleet.url %q is neither an http or https URL nor ax:<atespace>/<task>", raw))
		}
	}
	for i, g := range c.Forward {
		if !strings.HasPrefix(g, "http://") && !strings.HasPrefix(g, "https://") {
			p = append(p, fmt.Sprintf("workbench.fleet.forward[%d] %q is not a glob over http or https URLs", i, g))
		}
	}
	if c.Interval < 0 {
		p = append(p, fmt.Sprintf("workbench.fleet.interval %s is negative", c.Interval))
	}
	if c.Expire < 0 {
		p = append(p, fmt.Sprintf("workbench.fleet.expire %s is negative", c.Expire))
	}
	return p
}
