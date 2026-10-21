package web

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/agents"
	"github.com/bdubs00/featherweight-workbench/fleet"
)

// putAgent answers PUT /v1/agents/{name}. The body is an fw.AgentProfile.
// A body that names another profile is refused. A new profile answers 201,
// and a replaced one 200, both with the saved profile. With ?dryRun=true,
// it checks the profile, saves nothing, and answers 200 with an
// agents.Draft. With pools, the profile goes to the pool whose workers run
// it, and ?pool= names the pool for a profile that several pools run or
// that no pool runs yet (fleet.PlaceOf).
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
	at, err := fleet.PlaceOf(r.Context(), s.a, name, r.URL.Query().Get("pool"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if r.URL.Query().Get("dryRun") == "true" {
		d, err := agents.PreviewIn(r.Context(), at, p)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, d)
		return
	}
	v, created, err := agents.PutIn(r.Context(), s.a, at, p)
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

// deleteAgent answers DELETE /v1/agents/{name} with 204. With pools,
// ?pool= names the pool, as for putAgent.
func (s *server) deleteAgent(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	at, err := fleet.PlaceOf(r.Context(), s.a, name, r.URL.Query().Get("pool"))
	if err == nil {
		err = agents.DeleteIn(r.Context(), s.a, at, name)
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// allowRule answers POST /v1/runs/{id}/approvals/{approval}/rule: it adds
// the tool of the pending approval to the policy of the pool whose workers
// run the agent of the run (fleet.AllowRule), and it decides nothing; the
// page allows the approval next. The optional body holds a note. It
// answers 200 with the pool and the rule, and 409 when no pool runs the
// run.
func (s *server) allowRule(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Note string `json:"note"`
	}
	if !s.decodeOptional(w, r, &body) {
		return
	}
	pool, rule, err := fleet.AllowRule(r.Context(), s.a, r.PathValue("id"), r.PathValue("approval"), body.Note)
	if errors.Is(err, fleet.ErrNoPool) {
		problem(w, r, http.StatusConflict, "approval rules need a pool: "+strings.TrimPrefix(err.Error(), fleet.ErrNoPool.Error()+": ")+"; allow once instead")
		return
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"pool": pool, "rule": rule})
}
