package manager

import (
	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/internal/behalf"
)

// Guard returns a builder of the authorization plug for fw.WithAuthzFrom.
// The plug is the one that build returns, or the rules of access with the
// mode rules when build is nil or returns none, as the kernel builds them
// itself. Either way, Guard wraps it with behalf.Guard: a check for a
// person that the static token acts for (OnBehalf) passes only when it
// passes for the token too. cmd/fw-workbench wraps the control plane of
// access with it, so every verb of the binary has the guard.
func Guard(build func(*fw.Agent) (fw.Authz, string, error)) func(*fw.Agent) (fw.Authz, string, error) {
	return func(a *fw.Agent) (fw.Authz, string, error) {
		if build != nil {
			z, mode, err := build(a)
			if err != nil {
				return nil, "", err
			}
			if z != nil {
				return behalf.Guard(z), mode, nil
			}
		}
		return behalf.Guard(fw.NewRulesAuthz(a.Config().Access, a.ProfileNamed)), fw.AccessModeRules, nil
	}
}

// Guarded reports whether the authorization plug of an agent keeps the
// guard of Guard. Without it, a check for a person that the static token
// acts for is the check of the person alone.
func Guarded(a *fw.Agent) bool { return a != nil && behalf.Guarded(a.Authz()) }
