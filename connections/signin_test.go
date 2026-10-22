package connections

import (
	"strings"
	"testing"

	"github.com/bdubs00/featherweight-kernel/fw"
	"gopkg.in/yaml.v3"
)

func TestSigninOn(t *testing.T) {
	withBlock := func(text string) *fw.Config {
		cfg := fw.DefaultConfig()
		var n yaml.Node
		if err := yaml.Unmarshal([]byte(text), &n); err != nil {
			t.Fatal(err)
		}
		cfg.Extensions = map[string]yaml.Node{"workbench": *n.Content[0]}
		return cfg
	}
	t.Setenv(EnvSignin, "")
	for name, tc := range map[string]struct {
		cfg  *fw.Config
		env  string
		want bool
		err  string
	}{
		"default":      {cfg: fw.DefaultConfig(), want: true},
		"nil":          {cfg: nil, want: true},
		"block off":    {cfg: withBlock("signin: off\nfleet: {name: front}\n"), want: false},
		"block on":     {cfg: withBlock("signin: on\n"), want: true},
		"env wins":     {cfg: withBlock("signin: on\n"), env: "off", want: false},
		"env bad":      {cfg: fw.DefaultConfig(), env: "maybe", err: `FW_WORKBENCH_SIGNIN="maybe" is not on or off`},
		"block bad":    {cfg: withBlock("signin: later\n"), err: `workbench.signin="later" is not on or off`},
		"other blocks": {cfg: withBlock("bossmode: {enabled: on}\n"), want: true},
	} {
		t.Setenv(EnvSignin, tc.env)
		on, err := SigninOn(tc.cfg)
		if tc.err != "" {
			if err == nil || !strings.Contains(err.Error(), tc.err) {
				t.Errorf("%s: err = %v, want %q", name, err, tc.err)
			}
			continue
		}
		if err != nil || on != tc.want {
			t.Errorf("%s: SigninOn = %v, %v; want %v", name, on, err, tc.want)
		}
	}
}
