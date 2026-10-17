package fleet

import (
	"context"
	"io"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// DoctorCheck is the line of fw doctor about the fleet (cli.DoctorCheck):
// off, on a bus that only this process sees, or on a shared bus, with the
// address where other members reach this one. cmd/fw-workbench registers
// it.
func DoctorCheck(ctx context.Context, cfg *fw.Config, _ fw.Secrets, timeout time.Duration) (bool, string) {
	c, err := Load(cfg)
	if err != nil {
		return false, "fleet: " + err.Error()
	}
	if c.Subject == "" {
		return true, "fleet: off; set workbench.fleet.subject (" + EnvSubject + ") to join one"
	}
	if cfg.Bus.Driver == "" || cfg.Bus.Driver == "mem" {
		return false, "fleet: workbench.fleet.subject is " + c.Subject + ", but the bus is mem, so no other process sees this one; set bus.driver"
	}
	bctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	retains := false
	if b, err := fw.OpenBus(bctx, cfg); err == nil {
		_, retains = b.(fw.Retainer)
		if cl, ok := b.(io.Closer); ok {
			_ = cl.Close()
		}
	}
	keeps := "keeps the last record of each member"
	if !retains {
		keeps = "keeps no records, so a new view sees a member after its next record"
	}
	reach := "other members can only read this one; set workbench.fleet.url to let them act on its runs"
	if c.URL != "" {
		reach = "other members reach this one at " + c.URL
	}
	return true, "fleet: " + c.Subject + " on the " + cfg.Bus.Driver + " bus, which " + keeps + "; " + reach
}
