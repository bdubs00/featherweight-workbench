package notify

import (
	"context"
	"strings"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/internal/bucket"
)

// DoctorCheck is the line of fw doctor about the notifications
// (cli.DoctorCheck): where the settings live, the ntfy server of the
// front, and the hosts that a person may name. A configuration that does
// not fit fails the check. cmd/fw-workbench registers it.
func DoctorCheck(_ context.Context, cfg *fw.Config, _ fw.Secrets, _ time.Duration) (bool, string) {
	c, err := LoadConfig(cfg)
	if err != nil {
		return false, err.Error()
	}
	where, on := bucket.Where(cfg, notifyBucket)
	if !on {
		return true, "notify: off; the settings live " + where
	}
	parts := []string{"the settings live in " + where}
	if c.Ntfy.Server == "" {
		parts = append(parts, "no server of the front (workbench.notifications.ntfy.server), so only a person who names a server gets notifications")
	} else {
		parts = append(parts, "the server of the front is "+hostOf(c.Ntfy.Server))
	}
	if len(c.Ntfy.Hosts) == 0 {
		parts = append(parts, "a person may name any server on a public address (workbench.notifications.ntfy.hosts)")
	} else {
		parts = append(parts, "a person may name "+strings.Join(c.Ntfy.Hosts, ", "))
	}
	return true, "notify: " + strings.Join(parts, "; ")
}
