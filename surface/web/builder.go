package web

// This file serves POST /v1/builder: the conversation that creates an
// agent. The builder prompt and the "Save as agent" input live in package
// dashboard, so the page, this route, and the MCP tool agent_builder start
// the same conversation.

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/dashboard"
	"github.com/bdubs00/featherweight-workbench/fleet"
)

// builderBody is the body of POST /v1/builder. Input is the first message
// of the person. FromRun is the id of a run to turn into an agent. A body
// holds exactly one of the two. Files are attachments of the first
// message, as in POST /v1/runs.
type builderBody struct {
	Input   string     `json:"input"`
	FromRun string     `json:"fromRun"`
	Files   []fileBody `json:"files"`
}

// builder answers POST /v1/builder. It starts a run of the agent builder
// (dashboard.BuilderRequest) with the input, or with the input that
// dashboard.SaveAsAgentInput makes from the run fromRun. That run must be
// a completed top-level run that the caller started, for a caller who can
// create agents, else the answer is 403. The start is the start of POST
// /v1/runs: the principal, the origin, and the header Idempotency-Key
// apply. It answers 201 with the run object.
//
// A fromRun that another fleet member owns is read from that member with
// the credential of the caller, because a worker serves the native API of
// the kernel alone and no builder route: the first message comes from the
// ledger of the owner (dashboard.SaveAsAgentInputOn), on the same terms,
// and the builder runs in this process.
func (s *server) builder(w http.ResponseWriter, r *http.Request) {
	var body builderBody
	if !s.decodeFiles(w, r, &body, func() int { return len(body.Files) }) {
		return
	}
	input, fromRun := strings.TrimSpace(body.Input), strings.TrimSpace(body.FromRun)
	if (input == "") == (fromRun == "") {
		problem(w, r, http.StatusBadRequest, "send exactly one of the fields input and fromRun")
		return
	}
	if fromRun != "" {
		member, ok := fleet.Owner(s.a, w, r, fromRun)
		if !ok {
			return
		}
		var err error
		if member != "" {
			input, err = dashboard.SaveAsAgentInputOn(r.Context(), s.a, fleet.Of(s.a), member, fromRun)
		} else {
			input, err = dashboard.SaveAsAgentInput(r.Context(), s.a, fromRun)
		}
		switch {
		case errors.Is(err, fw.ErrRunNotFound):
			problem(w, r, http.StatusNotFound, "no run has the id "+strconv.Quote(fromRun))
			return
		case err != nil:
			s.fail(w, r, err)
			return
		}
	} else {
		input = body.Input
	}
	req := dashboard.BuilderRequest(input)
	req.Files = append(req.Files, attachments(body.Files)...)
	run, ok := s.start(w, r, req)
	if !ok {
		return
	}
	w.Header().Set("Location", "/v1/runs/"+run.ID)
	writeJSON(w, http.StatusCreated, run)
}
