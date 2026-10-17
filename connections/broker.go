package connections

// This file is the token broker of the fleet: the routes on which a member
// of the fleet asks the front for the access token of a person at a
// service (docs/ARCHITECTURE.md section 11.9). The kernel package tokens
// defines the protocol and is the client; this file is the server. A
// member proves itself with the static token of the fleet or a workload
// identity, never as a person, and it names the run of the person that
// needs the token. The front finds the run in its fleet records, checks
// that the person started it, and answers with a token from its grants.
// The refresh token stays here.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/surface/native"
	"github.com/bdubs00/featherweight-kernel/tokens"
	"github.com/bdubs00/featherweight-workbench/fleet"
)

// lookupTimeout bounds the search for the run in the fleet records.
const lookupTimeout = 5 * time.Second

func init() {
	native.Extend(func(k *native.Kit) error {
		b := &routes{k: k}
		k.Handle("POST "+tokens.PathToken, b.token)
		k.Handle("POST "+tokens.PathReject, b.reject)
		k.Handle("POST "+tokens.PathForget, b.forget)
		return nil
	})
}

// routes serves the routes of the token broker for one agent.
type routes struct{ k *native.Kit }

// refuse writes a tokens.Refusal. detail never holds a token.
func refuse(w http.ResponseWriter, status int, reason, detail string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(tokens.Refusal{Reason: reason, Detail: detail})
}

// member checks that the caller is a member of the fleet: the static
// token or a workload, never a person. On failure it writes the refusal.
func (b *routes) member(w http.ResponseWriter, r *http.Request) bool {
	p := fw.PrincipalFrom(r.Context())
	if p == nil || (p.Kind != fw.KindToken && p.Kind != fw.KindWorkload) {
		refuse(w, http.StatusForbidden, tokens.ReasonForbidden, "only a member of the fleet asks for the token of a person")
		return false
	}
	return true
}

// token answers POST /v1/tokens.
func (b *routes) token(w http.ResponseWriter, r *http.Request) {
	if !b.member(w, r) {
		return
	}
	var req tokens.TokenRequest
	if !b.k.Decode(w, r, &req) {
		return
	}
	subject, server, run := strings.TrimSpace(req.Subject), strings.TrimSpace(req.Server), strings.TrimSpace(req.Run)
	if subject == "" || server == "" || run == "" {
		refuse(w, http.StatusBadRequest, tokens.ReasonInvalid, "the request needs the subject, the server, and the run")
		return
	}
	a := b.k.Agent()
	f := fleet.Of(a)
	if f == nil {
		refuse(w, http.StatusServiceUnavailable, tokens.ReasonNoFleet, "this process has no fleet, so it cannot check the run")
		return
	}
	// The process itself looks for the run, without the authorization of
	// the caller, because the member is trusted and the records of the
	// fleet are the check.
	lctx, cancel := context.WithTimeout(context.Background(), lookupTimeout)
	defer cancel()
	known, err := f.Run(lctx, run)
	switch {
	case errors.Is(err, fw.ErrRunNotFound):
		refuse(w, http.StatusNotFound, tokens.ReasonUnknownRun, "the fleet does not know the run "+run)
		return
	case err != nil:
		refuse(w, http.StatusBadGateway, tokens.ReasonError, "the fleet could not look for the run: "+strings.TrimPrefix(err.Error(), "fleet: "))
		return
	case known.Principal != subject:
		refuse(w, http.StatusForbidden, tokens.ReasonForbidden, "the run "+run+" is not a run of that person")
		return
	}
	c := Of(a)
	if c == nil {
		refuse(w, http.StatusNotFound, tokens.ReasonUnknownService, "no service acts for a person here")
		return
	}
	tok, exp, err := c.TokenInfo(r.Context(), subject, server)
	switch {
	case err == nil:
	case errors.Is(err, ErrUnknownService):
		refuse(w, http.StatusNotFound, tokens.ReasonUnknownService, "no service named "+server+" acts for a person")
		return
	case errors.Is(err, fw.ErrNotConnected):
		refuse(w, http.StatusConflict, tokens.ReasonNotConnected, "the person has not connected "+server)
		return
	case errors.Is(err, fw.ErrConnectionExpired):
		refuse(w, http.StatusConflict, tokens.ReasonExpired, "the authorization server refused the grant of the person at "+server)
		return
	default:
		a.Logger().Warn("the token broker could not get a token", "subject", subject, "server", server, "member", known.Member, "error", err)
		refuse(w, http.StatusBadGateway, tokens.ReasonError, strings.TrimPrefix(err.Error(), "connections: "))
		return
	}
	a.Logger().Info("the token broker gave a token", "subject", subject, "server", server, "run", run, "member", known.Member)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(tokens.TokenAnswer{Token: tok, ExpiresAt: exp})
}

// reject answers POST /v1/tokens/reject: the service refused a token.
func (b *routes) reject(w http.ResponseWriter, r *http.Request) {
	if !b.member(w, r) {
		return
	}
	var req tokens.RejectRequest
	if !b.k.Decode(w, r, &req) {
		return
	}
	if c := Of(b.k.Agent()); c != nil {
		c.Reject(r.Context(), req.Subject, req.Server, req.Token)
	}
	w.WriteHeader(http.StatusNoContent)
}

// forget answers POST /v1/tokens/forget: the service kept refusing the
// tokens of the person, so the grant goes.
func (b *routes) forget(w http.ResponseWriter, r *http.Request) {
	if !b.member(w, r) {
		return
	}
	var req tokens.ForgetRequest
	if !b.k.Decode(w, r, &req) {
		return
	}
	if c := Of(b.k.Agent()); c != nil {
		c.Forget(r.Context(), req.Subject, req.Server)
		b.k.Agent().Logger().Info("the token broker dropped a grant", "subject", req.Subject, "server", req.Server)
	}
	w.WriteHeader(http.StatusNoContent)
}
