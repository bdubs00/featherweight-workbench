package sources

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// DoctorCheck is the line of fw doctor about the sources (cli.DoctorCheck):
// none, or each source with its kind, its agent, and its pool when it names
// one, and where the keys live. A source whose secret is missing, or a bus
// that keeps no keys, fails the check. cmd/fw-workbench registers it.
func DoctorCheck(ctx context.Context, cfg *fw.Config, secrets fw.Secrets, _ time.Duration) (bool, string) {
	list, err := Load(cfg)
	if err != nil {
		return false, err.Error()
	}
	if len(list) == 0 {
		return true, "sources: none; add workbench.sources to turn the events of other systems into runs"
	}
	if secrets == nil {
		secrets = fw.EnvSecrets{}
	}
	ok := true
	var parts []string
	for _, s := range list {
		var line string
		var name string
		switch {
		case s.Poll != nil:
			line = fmt.Sprintf("%s polls %s every %s for %s", s.Name, newPoller(*s.Poll, "").host, s.Poll.Every, s.Agent)
			name = s.Poll.Secret
		case s.Auth.Mode == AuthNone:
			line = fmt.Sprintf("%s takes webhooks without a credential for %s", s.Name, s.Agent)
		default:
			line = fmt.Sprintf("%s takes webhooks with %s for %s", s.Name, s.Auth.Mode, s.Agent)
			name = s.Auth.Secret
		}
		if s.Pool != "" {
			line += " on the pool " + s.Pool
		}
		if _, err := secretOf(ctx, secrets, s.Name, "the secret", name); err != nil {
			ok = false
			line += fmt.Sprintf(", but %s holds no secret", name)
		}
		parts = append(parts, line)
	}
	switch d := cfg.Bus.Driver; d {
	case "nats":
		parts = append(parts, "keeps the keys in JetStream")
	case "", "mem":
		parts = append(parts, "keeps the keys in memory, which a restart forgets (set bus.driver nats)")
	default:
		ok = false
		parts = append(parts, "keeps no keys, because the bus is "+d+" (set bus.driver nats)")
	}
	return ok, "sources: " + strings.Join(parts, "; ")
}
