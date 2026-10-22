package fleet

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/internal/behalf"
)

// The view decides an action other than a read with the rules of the
// front: the starter of a run and the operators of its agent operate it,
// and so does whoever operates an ancestor.
func TestAllowRunWithTheRules(t *testing.T) {
	t.Setenv("FW_ATESPACE", "")
	view := newMember(t, fw.NewMemBus(), "view", rulesFront("closed"))
	inject(view.f, "team/gamma", "ax:team/gamma", "team", MemberLive, time.Now(), rulesRuns()...)
	cases := []struct {
		who    context.Context
		run    string
		action string
		ok     bool
	}{
		{as("alice"), "r1", fw.ActionRunOperate, true},
		{as("alice"), "r3", fw.ActionRunOperate, true}, // the child of her run
		{as("bob"), "r1", fw.ActionRunOperate, false},
		{as("bob"), "r2", fw.ActionRunOperate, true},
		{as("carol", "ops"), "r1", fw.ActionRunView, true},
		{as("carol", "ops"), "r1", fw.ActionRunOperate, false}, // a viewer
		{as("root"), "r2", fw.ActionRunOperate, true},
		{context.Background(), "r1", fw.ActionRunOperate, true},
	}
	for _, c := range cases {
		err := AllowRun(c.who, view.f, c.run, c.action)
		if (err == nil) != c.ok || (err != nil && !errors.Is(err, fw.ErrForbidden)) {
			t.Errorf("%v %s %s = %v, want ok %v", fw.PrincipalFrom(c.who), c.action, c.run, err, c.ok)
		}
	}
	if err := AllowRun(as("alice"), view.f, "nope", fw.ActionRunOperate); !errors.Is(err, fw.ErrRunNotFound) {
		t.Fatalf("an unknown run = %v", err)
	}
	if err := AllowAgent(as("alice"), view.f, "team/gamma", "team", fw.ActionAgentRun); err != nil {
		t.Fatalf("alice runs team: %v", err)
	}
	if err := AllowAgent(as("dave"), view.f, "team/gamma", "bare", fw.ActionAgentRun); !errors.Is(err, fw.ErrForbidden) {
		t.Fatalf("dave runs bare on a closed front: %v", err)
	}
	if err := AllowAgent(as("alice"), view.f, "team/nope", "team", fw.ActionAgentRun); !errors.Is(err, ErrNoRoute) {
		t.Fatalf("an unknown member = %v", err)
	}
	// A view that decides nothing refuses.
	if err := AllowRun(as("alice"), plainView{}, "r1", fw.ActionRunOperate); !errors.Is(err, fw.ErrForbidden) {
		t.Fatalf("a plain view = %v", err)
	}
}

// When the static token acts for a person, the view gives that person only
// what the token can do too: a read, an operation, and a role.
func TestTheViewGuardsAPersonOfTheToken(t *testing.T) {
	t.Setenv("FW_ATESPACE", "")
	token := &fw.Principal{Subject: fw.KindToken, Kind: fw.KindToken}
	forPerson := func(subject string) context.Context {
		return behalf.For(fw.WithPrincipal(context.Background(), token), &fw.Principal{Subject: subject, Kind: fw.KindUser})
	}
	// The token is no admin of this front, and root is.
	weak := newMember(t, fw.NewMemBus(), "weak", rulesFront("closed"))
	inject(weak.f, "team/gamma", "ax:team/gamma", "team", MemberLive, time.Now(), rulesRuns()...)
	if runs, err := weak.f.Runs(as("root")); err != nil || len(runs) != 3 {
		t.Fatalf("root alone sees %v, %v", runIDs(runs), err)
	}
	if runs, err := weak.f.Runs(forPerson("root")); err != nil || len(runs) != 0 {
		t.Fatalf("root through the token sees %v, %v", runIDs(runs), err)
	}
	if err := AllowRun(forPerson("root"), weak.f, "r2", fw.ActionRunOperate); !errors.Is(err, fw.ErrForbidden) {
		t.Fatalf("root operates through the token: %v", err)
	}
	if got := roles(t, weak.f, forPerson("root"), "team/gamma"); len(got) != 0 {
		t.Fatalf("root through the token has the roles %v", got)
	}

	// With the token as an admin, the person decides alone.
	strong := newMember(t, fw.NewMemBus(), "strong", func(c *fw.Config, fc *Config) {
		rulesFront("closed")(c, fc)
		c.Access.Admins = []string{"root", "kind:token"}
	})
	inject(strong.f, "team/gamma", "ax:team/gamma", "team", MemberLive, time.Now(), rulesRuns()...)
	if runs, err := strong.f.Runs(forPerson("alice")); err != nil || !sameSet(runIDs(runs), "r1", "r3") {
		t.Fatalf("alice through the token sees %v, %v", runIDs(runs), err)
	}
	if err := AllowRun(forPerson("alice"), strong.f, "r1", fw.ActionRunOperate); err != nil {
		t.Fatalf("alice operates her run through the token: %v", err)
	}
	if err := AllowRun(forPerson("bob"), strong.f, "r1", fw.ActionRunOperate); !errors.Is(err, fw.ErrForbidden) {
		t.Fatalf("bob operates the run of alice through the token: %v", err)
	}
}

// In the other modes, the Authz of the process decides, and the check
// carries the member, the access block, and the lineage.
func TestAllowRunAsksTheAuthz(t *testing.T) {
	t.Setenv("FW_ATESPACE", "")
	var mu sync.Mutex
	var seen []fw.AuthzCheck
	authz := fw.AuthzFunc(func(_ context.Context, p *fw.Principal, action string, tg fw.Target) error {
		if tg.Member == "" || action != fw.ActionRunOperate {
			return nil
		}
		mu.Lock()
		seen = append(seen, fw.AuthzCheck{Action: action, Target: tg})
		mu.Unlock()
		if p.Subject == "alice" {
			return nil
		}
		return fw.ErrForbidden
	})
	view := newMember(t, fw.NewMemBus(), "view", nil, fw.WithAuthz(authz))
	inject(view.f, "team/gamma", "ax:team/gamma", "team", MemberLive, time.Now(), rulesRuns()...)
	if err := AllowRun(as("alice"), view.f, "r3", fw.ActionRunOperate); err != nil {
		t.Fatalf("alice: %v", err)
	}
	if err := AllowRun(as("bob"), view.f, "r3", fw.ActionRunOperate); !errors.Is(err, fw.ErrForbidden) {
		t.Fatalf("bob: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 {
		t.Fatalf("checks = %+v", seen)
	}
	c := seen[0]
	if c.Action != fw.ActionRunOperate || c.Target.Member != "team/gamma" || c.Target.Run == nil || c.Target.Run.ID != "r3" ||
		len(c.Target.Lineage) != 1 || c.Target.Lineage[0].ID != "r1" {
		t.Fatalf("check = %+v", c)
	}
}

// plainView is a View that decides nothing beyond a read.
type plainView struct{ View }
