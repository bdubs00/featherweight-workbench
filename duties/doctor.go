package duties

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/internal/bucket"
)

// DoctorCheck is the line of fw doctor about the duties (cli.DoctorCheck):
// where the duties live, the hosts that a check may reach, and the secrets
// that a duty may name. A configuration that does not fit, or a secret of
// the list that holds no value, fails the check. cmd/fw-workbench
// registers it.
func DoctorCheck(ctx context.Context, cfg *fw.Config, secrets fw.Secrets, _ time.Duration) (bool, string) {
	c, err := LoadConfig(cfg)
	if err != nil {
		return false, err.Error()
	}
	where, on := bucket.Where(cfg, dutiesBucket)
	if !on {
		return true, "duties: off; the duties live " + where
	}
	if secrets == nil {
		secrets = fw.EnvSecrets{}
	}
	ok := true
	parts := []string{"the duties live in " + where}
	if len(c.CheckHosts) == 0 {
		parts = append(parts, "a check reaches any public address and no private one (workbench.duties.checkHosts)")
	} else {
		parts = append(parts, "a check reaches "+strings.Join(c.CheckHosts, ", "))
	}
	for _, r := range c.Secrets {
		v, err := secrets.Get(ctx, r.Name)
		line := fmt.Sprintf("a duty may name %s", r.Name)
		if len(r.Hosts) > 0 {
			line += " for " + strings.Join(r.Hosts, ", ")
		}
		if err != nil || strings.TrimSpace(v) == "" {
			ok = false
			line += ", but it holds no secret"
		}
		parts = append(parts, line)
	}
	return ok, "duties: " + strings.Join(parts, "; ")
}
