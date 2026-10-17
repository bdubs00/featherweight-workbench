package connections

import (
	"context"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// DoctorCheck is the line of fw doctor about the connections
// (cli.DoctorCheck): the services that act for a person and the store of
// their grants. It writes nothing: the file store makes its directory and
// its key on the first connection. A service that no person can connect
// fails the line. cmd/fw-workbench registers it.
func DoctorCheck(ctx context.Context, cfg *fw.Config, secrets fw.Secrets, timeout time.Duration) (bool, string) {
	if secrets == nil {
		secrets = fw.EnvSecrets{}
	}
	var names, signIn []string
	for _, m := range personal(cfg) {
		names = append(names, m.Name)
		if strings.TrimSpace(m.Auth.OAuth.ClientID) == "" {
			signIn = append(signIn, m.Name)
		}
	}
	gc, err := Load(cfg)
	if err != nil {
		return false, "connections: " + strings.TrimPrefix(err.Error(), "connections: ")
	}
	sctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	s, err := NewStore(sctx, gc, cfg, secrets)
	if err != nil {
		return false, "connections: " + strings.TrimPrefix(err.Error(), "connections: ")
	}
	if c, ok := s.(io.Closer); ok {
		_ = c.Close()
	}
	store := Describe(sctx, gc, secrets)
	if len(names) == 0 {
		return true, "connections: no service acts for a person; grants: " + store
	}
	who, verb := strings.Join(names, ", "), "act"
	if len(names) == 1 {
		verb = "acts"
	}
	if !slices.Contains(cfg.Auth.Modes(), "oidc") {
		return false, "connections: " + who + " " + verb + " for a person, but auth.mode (FW_AUTH_MODE) has no oidc, so no person can connect"
	}
	hasClient := cfg.Auth.OIDC != nil && strings.TrimSpace(cfg.Auth.OIDC.ClientID) != ""
	if len(signIn) > 0 && !hasClient {
		use := "use"
		if len(signIn) == 1 {
			use = "uses"
		}
		return false, "connections: " + strings.Join(signIn, ", ") + " " + use + " the sign-in client, but auth.oidc.clientId (FW_AUTH_OIDC_CLIENT_ID) is not set"
	}
	note := ""
	if !hasClient {
		note = "; browser sign-in is off (FW_AUTH_OIDC_CLIENT_ID), so a person who opens a connect link pastes a token first"
	}
	return true, "connections: " + who + " " + verb + " for a person; grants: " + store + note
}
