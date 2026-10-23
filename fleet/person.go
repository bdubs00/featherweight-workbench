package fleet

// This file holds what the front needs to act for a person on another
// member (docs/ARCHITECTURE.md section 4.5). The front decides each action
// of a person with the rules of the view (allow.go), the rules of what the
// person sees. The request then goes to the member with the static token
// of the fleet as its credential and with HeaderOnBehalfOf, which names
// the person. So a worker trusts the credential of the fleet alone, and
// the credential of a person never leaves the front.

import (
	"context"
	"fmt"
	"strings"
	"unicode"

	"github.com/bdubs00/featherweight-kernel/fw"
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

type onBehalfKey struct{}

// WithOnBehalfOf returns ctx with the subject of the person that a request
// to a member acts for. Forward sends it in HeaderOnBehalfOf. An empty
// subject gives ctx as it is.
func WithOnBehalfOf(ctx context.Context, subject string) context.Context {
	if subject == "" {
		return ctx
	}
	return context.WithValue(ctx, onBehalfKey{}, subject)
}

// OnBehalfOf returns the subject that WithOnBehalfOf put in ctx, or "".
func OnBehalfOf(ctx context.Context) string {
	s, _ := ctx.Value(onBehalfKey{}).(string)
	return s
}

// headerSubject reports whether a subject fits HeaderOnBehalfOf as it is:
// not empty, no control character, and no space at either end, which a
// member would cut off.
func headerSubject(s string) bool {
	return s != "" && strings.TrimSpace(s) == s && !strings.ContainsFunc(s, unicode.IsControl)
}

// ForPerson returns ctx for a request that the front sends to a member for
// the person p, after it decided the action for p: the static token of the
// fleet as the credential (Token), and the subject of p for
// HeaderOnBehalfOf. A subject that fits no header gives an error that
// wraps fw.ErrForbidden, so the front does not act for it.
func ForPerson(ctx context.Context, cfg *fw.Config, p *fw.Principal) (context.Context, error) {
	if p == nil || !headerSubject(p.Subject) {
		return nil, fmt.Errorf("%w: the front names a person to another member by the subject of the person, and this credential names no subject that a header can carry", fw.ErrForbidden)
	}
	tok, err := Token(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return WithOnBehalfOf(fw.WithCredential(ctx, "Bearer "+tok), p.Subject), nil
}
