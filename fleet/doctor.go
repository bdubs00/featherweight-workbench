package fleet

import (
	"context"
	"strings"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// DoctorCheck is the line of fw doctor about the fleet (cli.DoctorCheck):
// off, or on with its sources and the state of the suspend controller.
// cmd/fw-workbench registers it.
func DoctorCheck(_ context.Context, cfg *fw.Config, _ fw.Secrets, _ time.Duration) (bool, string) {
	c, err := Load(cfg)
	if err != nil {
		return false, "fleet: " + err.Error()
	}
	if !c.On() {
		return true, "fleet: off; set workbench.fleet.atespaces (" + EnvAtespaces + ") or workbench.fleet.egress (" + EnvEgress + ") to read one"
	}
	if c.Egress != "" && (cfg.Bus.Driver == "" || cfg.Bus.Driver == "mem") {
		return false, "fleet: workbench.fleet.egress is " + c.Egress + ", but the bus is mem, so no worker reaches this process; set bus.driver"
	}
	var parts []string
	if len(c.Atespaces) > 0 {
		parts = append(parts, "reads the Tasks of "+strings.Join(c.Atespaces, ", ")+" from "+firstNonEmpty(cfg.AX.Server, "ax.server"))
	}
	if c.Egress != "" {
		parts = append(parts, "reads the events of the workers on "+c.Egress+" over the "+cfg.Bus.Driver+" bus")
	}
	if c.Intake != "" {
		parts = append(parts, "sends jobs to "+c.Intake+".<agent>")
	}
	if d := c.suspendAfter(); d > 0 {
		parts = append(parts, "suspends a Task that idles for "+d.String())
	} else {
		parts = append(parts, "suspends no Task (set workbench.fleet.suspendAfter)")
	}
	return true, "fleet: " + strings.Join(parts, "; ")
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
