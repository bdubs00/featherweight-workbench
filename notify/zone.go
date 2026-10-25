package notify

import (
	"context"
	"strings"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/duties"
)

func init() {
	duties.RegisterZone(savedZone)
}

// savedZone returns the time zone that a person saved with the
// notifications, or "" for a person who saved none. A new schedule of a
// duty that names no time zone takes it, so a duty that the manager writes
// for the person runs in the time of the person.
func savedZone(ctx context.Context, a *fw.Agent, subject string) string {
	s := Of(a)
	if s == nil {
		return ""
	}
	rec, err := s.load(ctx, subject)
	if err != nil || rec == nil || rec.Settings.QuietHours == nil {
		return ""
	}
	return strings.TrimSpace(rec.Settings.QuietHours.TimeZone)
}
