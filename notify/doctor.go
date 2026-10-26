package notify

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/internal/bucket"
)

// DoctorCheck is the line of fw doctor about the notifications
// (cli.DoctorCheck): where the settings live, the kinds that the front
// offers, the ntfy server of the front, the private hosts that a channel
// may reach, the link, and the shared channels. A configuration that does
// not fit, or a variable of a shared channel or of the mail server that
// holds no value, fails the check. cmd/fw-workbench registers it.
func DoctorCheck(ctx context.Context, cfg *fw.Config, secrets fw.Secrets, _ time.Duration) (bool, string) {
	c, err := LoadConfig(cfg)
	if err != nil {
		return false, err.Error()
	}
	where, on := bucket.Where(cfg, notifyBucket)
	if !on {
		return true, "notify: off; the settings live " + where
	}
	if secrets == nil {
		secrets = fw.EnvSecrets{}
	}
	ok := true
	set := func(name string) bool {
		v, err := secrets.Get(ctx, name)
		return err == nil && strings.TrimSpace(v) != ""
	}
	var offered []string
	for _, o := range c.offered() {
		offered = append(offered, o.Kind)
	}
	parts := []string{"the settings live in " + where, "a person picks " + strings.Join(offered, ", ")}
	if c.Ntfy.Server == "" {
		parts = append(parts, "no default ntfy server (workbench.notifications.ntfy.server), so an ntfy channel names its server")
	} else {
		parts = append(parts, "the default ntfy server is "+hostOf(c.Ntfy.Server))
	}
	if len(c.Hosts) == 0 {
		parts = append(parts, "a channel reaches public hosts only (workbench.notifications.hosts)")
	} else {
		parts = append(parts, "a channel also reaches "+strings.Join(c.Hosts, ", "))
	}
	if c.Email.Host != "" {
		line := fmt.Sprintf("mail goes through %s:%d", c.Email.Host, c.smtpPort())
		if c.Email.User != "" && !set(c.Email.PasswordEnv) {
			ok = false
			line += fmt.Sprintf(", but %s holds no password", c.Email.PasswordEnv)
		}
		parts = append(parts, line)
	}
	if c.Agent.Enabled {
		parts = append(parts, fmt.Sprintf("an agent channel runs at most %d times per person and day", c.perDay()))
	}
	if c.Link == "" {
		parts = append(parts, "a notification has no link (workbench.notifications.link)")
	} else {
		parts = append(parts, "links go to "+hostOf(c.Link))
	}
	for _, sc := range c.Shared {
		line := fmt.Sprintf("the shared channel %s (%s)", sc.Name, sc.Kind)
		switch {
		case !set(sc.URLEnv):
			ok = false
			line += fmt.Sprintf(", but %s holds no address", sc.URLEnv)
		case sc.SecretEnv != "" && !set(sc.SecretEnv):
			ok = false
			line += fmt.Sprintf(", but %s holds no secret", sc.SecretEnv)
		}
		parts = append(parts, line)
	}
	return ok, "notify: " + strings.Join(parts, "; ")
}
