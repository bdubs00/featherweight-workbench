package behalf

import (
	"context"
	"errors"
	"testing"

	"github.com/bdubs00/featherweight-kernel/fw"
)

var (
	token = &fw.Principal{Subject: fw.KindToken, Kind: fw.KindToken}
	alice = &fw.Principal{Subject: "alice", Kind: fw.KindUser}
	bob   = &fw.Principal{Subject: "bob", Kind: fw.KindUser}
)

// rules is a front where alice is an admin, the token owns the agent
// shared, and nobody else has a role: the agent private is closed.
func rules() fw.Authz {
	profiles := map[string]fw.AgentProfile{
		"shared":  {Name: "shared", Access: &fw.Access{Owners: []string{"token"}, Operators: []string{"alice", "bob"}}},
		"private": {Name: "private", Access: &fw.Access{Owners: []string{"carol"}}},
	}
	return fw.NewRulesAuthz(fw.AccessConfig{Default: "closed", Admins: []string{"alice"}}, func(n string) (fw.AgentProfile, bool) {
		p, ok := profiles[n]
		return p, ok
	})
}

// A check for a person that the token acts for passes only when it passes
// for the token too, so the person never gets more than the token.
func TestGuardNeverWidensTheCaller(t *testing.T) {
	z := Guard(rules())
	ctx := For(fw.WithPrincipal(context.Background(), token), alice)
	if p := fw.PrincipalFrom(ctx); p != alice {
		t.Fatalf("principal = %+v", p)
	}
	if c := Caller(ctx); c != token {
		t.Fatalf("caller = %+v", c)
	}
	// alice is an admin, and the token is not.
	if err := z.Allow(context.Background(), alice, fw.ActionAgentView, fw.Target{Agent: "private"}); err != nil {
		t.Fatalf("alice alone: %v", err)
	}
	err := z.Allow(ctx, alice, fw.ActionAgentView, fw.Target{Agent: "private"})
	if !errors.Is(err, fw.ErrForbidden) {
		t.Fatalf("alice through the token = %v", err)
	}
	// Both operate shared, so the check passes; only the token owns it.
	if err := z.Allow(ctx, alice, fw.ActionAgentRun, fw.Target{Agent: "shared"}); err != nil {
		t.Fatalf("both can run shared: %v", err)
	}
	if err := z.Allow(fw.WithPrincipal(context.Background(), token), token, fw.ActionAgentEdit, fw.Target{Agent: "shared"}); err != nil {
		t.Fatalf("the token owns shared: %v", err)
	}
	bobCtx := For(fw.WithPrincipal(context.Background(), token), bob)
	if err := z.Allow(bobCtx, bob, fw.ActionAgentEdit, fw.Target{Agent: "shared"}); !errors.Is(err, fw.ErrForbidden) {
		t.Fatalf("bob edits shared through the token: %v", err)
	}
	// A batch decides each check the same way.
	errs := z.(fw.BatchAuthz).AllowAll(ctx, alice, []fw.AuthzCheck{
		{Action: fw.ActionAgentView, Target: fw.Target{Agent: "private"}},
		{Action: fw.ActionAgentRun, Target: fw.Target{Agent: "shared"}},
	})
	if !errors.Is(errs[0], fw.ErrForbidden) || errs[1] != nil {
		t.Fatalf("batch = %v", errs)
	}
	// A check for another principal than the person goes as it is.
	if err := z.Allow(ctx, bob, fw.ActionAgentRun, fw.Target{Agent: "shared"}); err != nil {
		t.Fatalf("bob in a context that acts for alice: %v", err)
	}
}

// Guard keeps what the plug does about batches and lineage, and guards
// once.
func TestGuardKeepsThePlug(t *testing.T) {
	plain := Guard(rules())
	if _, ok := plain.(fw.LineageAuthz); ok {
		t.Fatal("a guard over the rules decides the lineage")
	}
	if !Guarded(plain) || Guard(plain) != plain {
		t.Fatal("a guard is guarded twice")
	}
	lin := Guard(lineageAuthz{rules()})
	if _, ok := lin.(fw.LineageAuthz); !ok || !Guarded(lin) {
		t.Fatalf("a guard over a LineageAuthz is %T", lin)
	}
	if Guard(nil) != nil || Guarded(rules()) {
		t.Fatal("Guard of nil, or the rules alone, is guarded")
	}
}

type lineageAuthz struct{ fw.Authz }

func (lineageAuthz) DecidesLineage() {}

// Trusted believes the run keys from the static token alone, also after
// the server put the person in the place of the token.
func TestTrusted(t *testing.T) {
	r := Run{Run: "r-1", Agent: "manager", Principal: "alice", PrincipalKind: fw.KindUser}
	base := WithRun(context.Background(), r)
	if got, ok := Trusted(fw.WithPrincipal(base, token)); !ok || got != r {
		t.Fatalf("the token: %+v, %v", got, ok)
	}
	for _, p := range []*fw.Principal{alice, {Subject: "spiffe://td/w", Kind: fw.KindWorkload}, nil} {
		if _, ok := Trusted(fw.WithPrincipal(base, p)); ok {
			t.Errorf("%+v named a run", p)
		}
	}
	acting := For(fw.WithPrincipal(base, token), alice)
	if got, ok := Trusted(acting); !ok || got != r {
		t.Fatalf("after For: %+v, %v", got, ok)
	}
	// A principal that changed again after For has no caller behind it.
	again := fw.WithPrincipal(acting, bob)
	if Caller(again) != nil {
		t.Fatal("the caller survived a new principal")
	}
	if _, ok := Trusted(again); ok {
		t.Fatal("a new principal after For named a run")
	}
	if _, ok := Trusted(fw.WithPrincipal(context.Background(), token)); ok {
		t.Fatal("a call without run keys named a run")
	}
	// For without a person or without a caller changes nothing.
	plain := fw.WithPrincipal(context.Background(), token)
	if For(plain, nil) != plain || For(context.Background(), alice) != context.Background() {
		t.Fatal("For changed a context that it cannot act in")
	}
}
