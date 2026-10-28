package routines

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/surface/native"
	"github.com/bdubs00/featherweight-workbench/teams"
)

// withBlock gives cfg the block workbench of the text.
func withBlock(t *testing.T, cfg *fw.Config, text string) {
	t.Helper()
	var n yaml.Node
	if err := yaml.Unmarshal([]byte(text), &n); err != nil {
		t.Fatal(err)
	}
	cfg.Extensions = map[string]yaml.Node{teams.Block: *n.Content[0]}
}

// The routines are on by default. The key enabled and its variable turn
// them off, the variable wins over the file, and a value that is not on or
// off names the variable.
func TestLoadTheSwitch(t *testing.T) {
	t.Setenv(EnvEnabled, "")
	t.Setenv(EnvReadyAfter, "")
	t.Setenv(EnvReadyAfterRead, "")
	t.Setenv(EnvReadyAfterWrite, "")
	t.Setenv(EnvReadyAgreement, "")
	c, err := Load(fw.DefaultConfig())
	if err != nil || !c.On() || c.ReadyAfter != (ReadyAfter{Read: 20, Write: 50}) || c.ReadyAgreement != DefaultReadyAgreement {
		t.Fatalf("defaults = %+v, %v", c, err)
	}
	cfg := fw.DefaultConfig()
	withBlock(t, cfg, "routines: {enabled: false, readyAfter: 30, readyAgreement: 0.8}\n")
	if c, err := Load(cfg); err != nil || c.On() || c.ReadyAfter != (ReadyAfter{Read: 30, Write: 30}) || c.ReadyAgreement != 0.8 {
		t.Fatalf("the file = %+v, %v", c, err)
	}
	withBlock(t, cfg, "routines: {readyAfter: {read: 5}}\n")
	if c, err := Load(cfg); err != nil || c.ReadyAfter != (ReadyAfter{Read: 5, Write: DefaultReadyAfterWrite}) {
		t.Fatalf("a bound per risk = %+v, %v", c, err)
	}
	withBlock(t, cfg, "routines: {readyAfter: {read: 0, write: -1}}\n")
	if _, err := Load(cfg); err == nil || !strings.Contains(err.Error(), "workbench.routines.readyAfter.write") {
		t.Fatalf("a bound below 1 = %v", err)
	}
	withBlock(t, cfg, "routines: {enabled: false, readyAfter: 30, readyAgreement: 0.8}\n")
	t.Setenv(EnvEnabled, "on")
	t.Setenv(EnvReadyAfter, "10")
	t.Setenv(EnvReadyAfterWrite, "12")
	if c, err := Load(cfg); err != nil || !c.On() || c.ReadyAfter != (ReadyAfter{Read: 10, Write: 12}) {
		t.Fatalf("the variables did not win: %+v, %v", c, err)
	}
	t.Setenv(EnvEnabled, "maybe")
	if _, err := Load(cfg); err == nil || !strings.Contains(err.Error(), EnvEnabled+`="maybe" is not on or off`) {
		t.Fatalf("a bad switch = %v", err)
	}
}

// With the routines off, the service keeps no store on a bus that keeps
// one, mounts no route, gives the pools no routines and never waits for
// them, and the doctor line says off.
func TestRoutinesOff(t *testing.T) {
	t.Setenv(EnvEnabled, "off")
	t.Setenv(EnvReadyAfter, "")
	t.Setenv(EnvReadyAgreement, "")
	cfg := fw.DefaultConfig()
	cfg.Home = t.TempDir()
	a, err := fw.New(fw.WithConfig(cfg), fw.WithoutDefaults(), fw.WithAuthn(people{}),
		fw.WithLedger(fw.NewMemLedger()), fw.WithBus(fw.NewMemBus()), fw.WithSurfaces(native.New()),
		fw.WithServices(Start), fw.WithLogger(quiet()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		a.Close(ctx)
	})
	if Of(a) != nil {
		t.Fatal("the routines are on")
	}
	h, err := a.Handler()
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/routines", nil)
	req.Header.Set("Authorization", "Bearer alice")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(b), `"off":true`) {
		t.Fatalf("GET /v1/routines = %d %s; want 200 with off", resp.StatusCode, b)
	}
	for _, c := range []*fw.Config{nil, cfg} {
		var got map[string][]fw.Routine
		if c == nil {
			got, err = reaching(context.Background(), a, nil)
		} else {
			got, err = reaching(context.Background(), nil, c)
		}
		if err != nil || got != nil {
			t.Fatalf("reaching = %v, %v; want none", got, err)
		}
	}
	if ok, line := Status(cfg); !ok || !strings.HasPrefix(line, "routines: off") {
		t.Fatalf("status = %v, %q", ok, line)
	}
}
