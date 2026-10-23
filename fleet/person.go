package fleet

// This file holds what the front needs to act for a person on another
// member (docs/ARCHITECTURE.md section 4.5). The front decides each action
// of a person with the rules of the view (allow.go), the rules of what the
// person sees. The request then goes to the member with the static token
// of the fleet as its credential and with HeaderOnBehalfOf, which names
// the person. So a worker trusts the credential of the fleet alone, and
// the credential of a person never leaves the front. The relay acts so for
// the run routes of the kernel, and the other surfaces of the front get
// the context of each such request from ForRun and ForStart.

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"unicode"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/internal/behalf"
	"github.com/bdubs00/featherweight-workbench/pools"
)

// IsPerson reports whether p is a person for the fleet: a principal that
// is not the process itself (nil), not the static token, and not a
// workload. The front decides each action of a person on another member
// and acts with the static token. The static token and a workload act on
// another member with their own credential, and the member decides.
func IsPerson(p *fw.Principal) bool {
	return p != nil && p.Kind != fw.KindToken && p.Kind != fw.KindWorkload
}

// TokenEnv names the secret of the static token of the fleet:
// auth.tokenEnv, FW_API_TOKEN by default, as the kernel reads it.
func TokenEnv(cfg *fw.Config) string {
	if cfg != nil {
		if env := strings.TrimSpace(cfg.Auth.TokenEnv); env != "" {
			return env
		}
	}
	return pools.DefaultTokenEnv
}

// Token returns the static token of the fleet from the environment, as the
// platforms and the bridges read it. A secret that is missing or empty
// gives ErrNoToken.
func Token(ctx context.Context, cfg *fw.Config) (string, error) {
	env := TokenEnv(cfg)
	tok, err := fw.EnvSecrets{}.Get(ctx, env)
	if tok = strings.TrimSpace(tok); err != nil || tok == "" {
		return "", fmt.Errorf("%w: the secret %s is empty", ErrNoToken, env)
	}
	return tok, nil
}

// onBehalf is the person that a request to a member acts for, and the
// secret of the static token that the request carries for that person.
type onBehalf struct{ subject, env string }

type onBehalfKey struct{}

// WithOnBehalfOf returns ctx with the subject of the person that a request
// to a member acts for. Forward sends it in HeaderOnBehalfOf. An empty
// subject gives ctx as it is.
func WithOnBehalfOf(ctx context.Context, subject string) context.Context {
	if subject == "" {
		return ctx
	}
	return context.WithValue(ctx, onBehalfKey{}, onBehalf{subject: subject})
}

// OnBehalfOf returns the subject that WithOnBehalfOf put in ctx, or "".
func OnBehalfOf(ctx context.Context) string {
	v, _ := ctx.Value(onBehalfKey{}).(onBehalf)
	return v.subject
}

// TokenEnvOf names the secret of the static token that a request of ctx
// carries for a person, as ForPerson put it there, and "" when ctx names
// no person.
func TokenEnvOf(ctx context.Context) string {
	v, _ := ctx.Value(onBehalfKey{}).(onBehalf)
	switch {
	case v.subject == "":
		return ""
	case v.env == "":
		return pools.DefaultTokenEnv
	}
	return v.env
}

// RefusedToken words a refusal of the static token of the fleet by the
// worker member for an operator, because the person for whom the front
// acted can do nothing about it. status is the answer of the worker: a 401
// says to check the secret env on that worker, and another status gives
// the detail of the worker.
func RefusedToken(member, env string, status int, detail string) string {
	if status == http.StatusUnauthorized {
		return fmt.Sprintf("the worker %s refused the token of the fleet; check %s on that worker", member, env)
	}
	return fmt.Sprintf("the worker %s refused the token of the fleet: %s", member, detail)
}

// headerSubject reports whether a subject fits HeaderOnBehalfOf as it is:
// not empty, no control character, and no space at either end, which a
// member would cut off.
func headerSubject(s string) bool {
	return s != "" && strings.TrimSpace(s) == s && !strings.ContainsFunc(s, unicode.IsControl)
}

// ForPerson returns ctx for a request that the front sends to a member for
// the person p, after it decided the action for p: the static token of the
// fleet as the credential, and the subject of p for HeaderOnBehalfOf. When
// the static token calls for p (behalf.For, section 19), the request
// carries the credential of that call, which is the token. Else the token
// comes from the environment (Token). A subject that fits no header gives
// an error that wraps fw.ErrForbidden, so the front does not act for it.
func ForPerson(ctx context.Context, cfg *fw.Config, p *fw.Principal) (context.Context, error) {
	if p == nil || !headerSubject(p.Subject) {
		return nil, fmt.Errorf("%w: the front names a person to another member by the subject of the person, and this credential names no subject that a header can carry", fw.ErrForbidden)
	}
	who := onBehalf{subject: p.Subject, env: TokenEnv(cfg)}
	if c := behalf.Caller(ctx); c != nil && c.Kind == fw.KindToken && fw.CredentialFrom(ctx) != "" {
		return context.WithValue(ctx, onBehalfKey{}, who), nil
	}
	tok, err := Token(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return context.WithValue(fw.WithCredential(ctx, "Bearer "+tok), onBehalfKey{}, who), nil
}

// ForRun returns the context of a request that a surface of the front
// sends to the member that holds the run id, for the caller of ctx. For a
// person, the front first decides each action on the run with the rules
// of the view (decideFor), and the request then carries the static token
// and names the person (ForPerson). For the static token, a workload, and
// the process, ctx stays as it is: the request carries the credential of
// the caller, and the member decides. A refusal wraps fw.ErrForbidden, and
// a run that the view does not give to the caller is fw.ErrRunNotFound.
func ForRun(ctx context.Context, a *fw.Agent, id string, actions ...string) (context.Context, error) {
	p := fw.PrincipalFrom(ctx)
	if !IsPerson(p) {
		return ctx, nil
	}
	v := Of(a)
	if v == nil {
		return nil, fmt.Errorf("fleet: %w: the fleet is off", ErrNoRoute)
	}
	run, err := v.Run(ctx, id)
	if err != nil {
		return nil, err
	}
	if _, err := decideFor(ctx, v, run, actions); err != nil {
		return nil, err
	}
	return ForPerson(ctx, a.Config(), p)
}

// ForStart is ForRun for a start of a run of the profile agent on member:
// for a person, the front decides fw.ActionAgentRun on that agent at that
// member (AllowAgent). The body of the start names the person, as the
// relay names it (personBody).
func ForStart(ctx context.Context, a *fw.Agent, member, agent string) (context.Context, error) {
	p := fw.PrincipalFrom(ctx)
	if !IsPerson(p) {
		return ctx, nil
	}
	if err := AllowAgent(ctx, Of(a), member, agent, fw.ActionAgentRun); err != nil {
		return nil, err
	}
	return ForPerson(ctx, a.Config(), p)
}

// decideFor decides each action of the person in ctx on run, a run of
// another member as the view gives it, and returns the first action that
// the view refused, with its error. fw.ActionAgentRun decides on the agent
// of the run without the run, as the kernel decides a branch: a branch is
// a new run of that agent.
func decideFor(ctx context.Context, v View, run *fw.Run, actions []string) (string, error) {
	for _, action := range actions {
		var err error
		if action == fw.ActionAgentRun {
			err = AllowAgent(ctx, v, run.Member, run.Agent, action)
		} else {
			err = AllowRun(ctx, v, run.ID, action)
		}
		if err != nil {
			return action, err
		}
	}
	return "", nil
}
