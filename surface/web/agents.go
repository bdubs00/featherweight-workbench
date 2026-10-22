package web

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/surface/view"
	"github.com/bdubs00/featherweight-workbench/agents"
	"github.com/bdubs00/featherweight-workbench/fleet"
	"github.com/bdubs00/featherweight-workbench/pools"
	"github.com/bdubs00/featherweight-workbench/skills"
)

// putAgent answers PUT /v1/agents/{name}. The body is an fw.AgentProfile,
// with the optional key workers. A body that names another profile is
// refused. A new profile answers 201, and a replaced one 200, both with
// the saved profile. With ?dryRun=true, it checks the profile, saves
// nothing, and answers 200 with an agents.Draft. With pools, the profile
// goes to the pool whose workers run it, and ?pool= names the pool for a
// profile that several pools run or that no pool runs yet. With worker
// templates, a profile that no pool runs gets a runtime pool of its own
// when the body carries workers, {template, replicas}, or when a default
// template exists and the request names no pool (fleet.PlaceFor); the
// answer then carries the pool in the key pool.
func (s *server) putAgent(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var body struct {
		fw.AgentProfile
		Workers *fleet.Workers `json:"workers,omitempty"`
	}
	if !s.decode(w, r, &body) {
		return
	}
	p := body.AgentProfile
	if p.Name != "" && p.Name != name {
		problem(w, r, http.StatusBadRequest, fmt.Sprintf("the body names the profile %q but the path names %q", p.Name, name))
		return
	}
	p.Name = name
	at, err := fleet.PlaceFor(r.Context(), s.a, name, r.URL.Query().Get("pool"), body.Workers)
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
		if planned, ok := fleet.PlannedPool(at); ok {
			writeJSON(w, http.StatusOK, struct {
				agents.Draft
				Pool *pools.Status `json:"pool"`
			}{d, &planned})
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
	status := http.StatusOK
	if created {
		w.Header().Set("Location", "/v1/agents/"+name)
		status = http.StatusCreated
	}
	if made := fleet.CreatedPool(at); made != "" {
		out := struct {
			view.AgentView
			Pool *pools.Status `json:"pool,omitempty"`
		}{AgentView: v}
		if st, ok, err := fleet.PoolNamed(r.Context(), s.a, made); err == nil && ok {
			out.Pool = &st
		}
		writeJSON(w, status, out)
		return
	}
	writeJSON(w, status, v)
}

// AgentSkills is the answer of GET /v1/agents/{name}/skills. Allowlist is
// the skills list of the profile as it is, null for every skill. Skills
// holds the skills of the catalog of the front that the list allows, with
// the matching of the kernel (section 20.2). Pool names the pool whose
// workers run the profile, or is empty for a profile of this process.
type AgentSkills struct {
	Agent     string       `json:"agent"`
	Pool      string       `json:"pool,omitempty"`
	Allowlist []string     `json:"allowlist"`
	Skills    []skills.Ref `json:"skills"`
}

// agentSkills answers GET /v1/agents/{name}/skills. The kernel serves the
// view of an agent, GET /v1/agents/{name}, so the resolved skills of the
// page come from this route beside it. With pools, the profile is the one
// that the workers of the pool run, and ?pool= names the pool when several
// pools run it; its skills resolve against the catalog of the front.
func (s *server) agentSkills(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	pool := strings.TrimSpace(r.URL.Query().Get("pool"))
	ctx := r.Context()
	pa, found, err := fleet.PoolAgentNamed(ctx, s.a, name, pool)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	out := AgentSkills{Agent: name}
	switch {
	case found:
		out.Pool, out.Allowlist = pa.Pool, pa.Skills
	case pool != "" && fleet.Pooled(s.a):
		s.fail(w, r, fmt.Errorf("%w: %s in the pool %s", fw.ErrProfileNotFound, name, pool))
		return
	case pool != "":
		problem(w, r, http.StatusBadRequest, "this process keeps no pool, so leave out pool")
		return
	default:
		v, err := view.Agent(ctx, s.a, name)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		out.Allowlist = v.Skills
	}
	out.Skills = skills.Allowed(s.a, out.Allowlist)
	writeJSON(w, http.StatusOK, out)
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
