package web

import (
	"fmt"
	"net/http"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/agents"
)

// putAgent answers PUT /v1/agents/{name}. The body is an fw.AgentProfile.
// A body that names another profile is refused. A new profile answers 201,
// and a replaced one 200, both with the saved profile. With ?dryRun=true,
// it checks the profile, saves nothing, and answers 200 with an
// agents.Draft.
func (s *server) putAgent(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var p fw.AgentProfile
	if !s.decode(w, r, &p) {
		return
	}
	if p.Name != "" && p.Name != name {
		problem(w, r, http.StatusBadRequest, fmt.Sprintf("the body names the profile %q but the path names %q", p.Name, name))
		return
	}
	p.Name = name
	if r.URL.Query().Get("dryRun") == "true" {
		d, err := agents.Preview(r.Context(), s.a, p)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, d)
		return
	}
	v, created, err := agents.Put(r.Context(), s.a, p)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if created {
		w.Header().Set("Location", "/v1/agents/"+name)
		writeJSON(w, http.StatusCreated, v)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// deleteAgent answers DELETE /v1/agents/{name} with 204.
func (s *server) deleteAgent(w http.ResponseWriter, r *http.Request) {
	if err := agents.Delete(r.Context(), s.a, r.PathValue("name")); err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
