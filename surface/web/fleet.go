package web

// This file serves GET /v1/fleet, the members of the fleet
// (docs/ARCHITECTURE.md, the fleet). The native surface of the kernel
// forwards a request about a run that another member owns, and starts a run
// on another member with POST /v1/runs?member=<name>.

import (
	"net/http"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// fleet answers GET /v1/fleet with the name of this member and the members
// that the caller can see.
func (s *server) fleet(w http.ResponseWriter, r *http.Request) {
	f := s.a.Fleet()
	if f == nil {
		problem(w, r, http.StatusNotFound, "the fleet is off; set fleet.subject (FW_FLEET_SUBJECT) to turn it on")
		return
	}
	members, err := f.Members(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if members == nil {
		members = []fw.FleetMember{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"name": f.Name(), "members": members})
}
