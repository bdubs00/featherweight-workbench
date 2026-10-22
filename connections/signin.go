package connections

// This file reads the switch of the browser sign-in. The sign-in itself
// lives in package web; this package reads the switch too, so the doctor
// can say that a person who opens a connect link pastes a token first.

import (
	"fmt"
	"os"
	"strings"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// EnvSignin turns the browser sign-in on or off. It wins over the key
// signin of the block workbench of fw.yaml. The default is on. With off, a
// client id of auth.oidc serves the connections of people alone: the routes
// /ui/signin and /ui/callback do not exist, and a 401 names no sign-in, so
// the page shows its token form. A deployment whose issuer has no login
// page for a browser sets it off.
const EnvSignin = "FW_WORKBENCH_SIGNIN"

// SigninOn reads the switch of the browser sign-in: the variable, else the
// key signin of the block workbench, else on. It returns an error for a
// value that is not on or off.
func SigninOn(cfg *fw.Config) (bool, error) {
	v, from := strings.TrimSpace(os.Getenv(EnvSignin)), EnvSignin
	if v == "" && cfg != nil {
		var b struct {
			Signin string `yaml:"signin"`
		}
		if _, err := cfg.Extension("workbench", &b); err != nil {
			return false, fmt.Errorf("connections: %w", err)
		}
		v, from = strings.TrimSpace(b.Signin), "workbench.signin"
	}
	switch strings.ToLower(v) {
	case "", "on", "true", "yes", "1":
		return true, nil
	case "off", "false", "no", "0":
		return false, nil
	}
	return false, fmt.Errorf("connections: %s=%q is not on or off", from, v)
}
