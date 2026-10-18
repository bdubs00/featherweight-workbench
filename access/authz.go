package access

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
)

func init() {
	fw.RegisterExtension(Block)
	fw.RegisterControlServers(ControlServers)
}

// Use names the decision point for fw doctor, which prints
// "workbench.access.external".
const Use = "workbench.access"

// Build builds the authorization plug of a process from its configuration,
// for fw.WithAuthzFrom. Without workbench.access.external it returns nil,
// and the rules of access decide. With it, it builds the decision point on
// the agent and returns a plug that asks the control plane about every
// check, with the mode external.
func Build(a *fw.Agent) (fw.Authz, string, error) {
	ext, err := Load(a.Config())
	if err != nil {
		return nil, "", err
	}
	if ext == nil {
		return nil, "", nil
	}
	dp, err := a.NewDecisionPoint(context.Background(), Use, *ext)
	if err != nil {
		return nil, "", err
	}
	a.Logger().Info("a control plane decides the authorization", "component", "access", "decisionPoint", ext.String())
	return &controlPlane{a: a, dp: dp, ext: *ext, now: time.Now, cache: map[string]cached{}}, fw.AccessModeExternal, nil
}

// controlPlane asks the control plane about every check. It decides about
// a child run from the lineage in the request, so the kernel does not ask
// again for the ancestors (fw.LineageAuthz).
type controlPlane struct {
	a   *fw.Agent
	dp  fw.DecisionPoint
	ext fw.ExternalPolicyConfig
	now func() time.Time // the clock of the cache; a test sets it

	mu    sync.Mutex
	cache map[string]cached
}

type cached struct {
	err     error
	expires time.Time
}

// maxCached bounds the cache of decisions.
const maxCached = 4096

var (
	_ fw.Authz        = (*controlPlane)(nil)
	_ fw.BatchAuthz   = (*controlPlane)(nil)
	_ fw.LineageAuthz = (*controlPlane)(nil)
)

// DecidesLineage implements fw.LineageAuthz.
func (*controlPlane) DecidesLineage() {}

// Allow implements fw.Authz.
func (z *controlPlane) Allow(ctx context.Context, p *fw.Principal, action string, t fw.Target) error {
	return z.AllowAll(ctx, p, []fw.AuthzCheck{{Action: action, Target: t}})[0]
}

// AllowAll implements fw.BatchAuthz. It answers from the cache when it can
// and sends the other checks in one request.
func (z *controlPlane) AllowAll(ctx context.Context, p *fw.Principal, checks []fw.AuthzCheck) []error {
	out := make([]error, len(checks))
	if p == nil {
		return out
	}
	now := z.now()
	queries := make([]fw.AuthzQuery, len(checks))
	keys := make([]string, len(checks))
	for i, c := range checks {
		queries[i] = z.query(c)
		keys[i] = cacheKey(p, queries[i])
	}
	var miss []int
	z.mu.Lock()
	for i := range checks {
		if d, ok := z.cache[keys[i]]; ok && now.Before(d.expires) {
			out[i] = d.err
			continue
		}
		miss = append(miss, i)
	}
	z.mu.Unlock()
	if len(miss) == 0 {
		return out
	}
	req := fw.AuthzRequest{Kind: fw.DecisionAuthz, Version: fw.DecisionVersion, Principal: *p}
	for _, i := range miss {
		req.Checks = append(req.Checks, queries[i])
	}
	ans, err := z.ask(ctx, req)
	if err != nil {
		z.a.Logger().Warn("the control plane did not decide", "component", "access", "decisionPoint", z.ext.String(), "error", err)
		for _, i := range miss {
			out[i] = fmt.Errorf("%w: %s did not answer: %v", fw.ErrAuthzUnavailable, z.ext.String(), err)
		}
		return out
	}
	z.mu.Lock()
	defer z.mu.Unlock()
	for j, i := range miss {
		d := ans.Decisions[j]
		if !d.Allow {
			reason := strings.TrimSpace(d.Reason)
			if reason == "" {
				reason = "the control plane refused it"
			}
			out[i] = fmt.Errorf("%w: %s %s: %s", fw.ErrForbidden, p.Subject, checks[i].Action, reason)
		}
		if d.TTLMs > 0 {
			if len(z.cache) >= maxCached {
				z.sweep(now)
			}
			z.cache[keys[i]] = cached{err: out[i], expires: now.Add(time.Duration(d.TTLMs) * time.Millisecond)}
		}
	}
	return out
}

// query turns a check into the facts that the request carries. A check
// about another member of the fleet brings its own access block and
// lineage; a check about this process reads them here.
func (z *controlPlane) query(c fw.AuthzCheck) fw.AuthzQuery {
	agent := c.Target.Agent
	if agent == "" && c.Target.Run != nil {
		agent = c.Target.Run.Agent
	}
	if c.Target.Member != "" {
		q := fw.AuthzQuery{Action: c.Action, Member: c.Target.Member, Agent: agent, Access: c.Target.Access}
		if r := c.Target.Run; r != nil {
			q.Run = &fw.RunFacts{ID: r.ID, Agent: r.Agent, Principal: r.Principal, State: r.State, Origin: r.Origin, Lineage: c.Target.Lineage}
		}
		return q
	}
	q := fw.AuthzQuery{Action: c.Action, Agent: agent, Run: z.a.RunFacts(c.Target.Run)}
	if agent != "" {
		if prof, ok := z.a.ProfileNamed(agent); ok && prof.Access != nil {
			acc := *prof.Access
			q.Access = &acc
		}
	}
	return q
}

// ask sends one request and checks the shape of the answer.
func (z *controlPlane) ask(ctx context.Context, req fw.AuthzRequest) (fw.AuthzAnswer, error) {
	var ans fw.AuthzAnswer
	body, err := json.Marshal(req)
	if err != nil {
		return ans, err
	}
	ctx, cancel := context.WithTimeout(ctx, z.ext.Timeout)
	defer cancel()
	raw, err := z.dp.Decide(ctx, body)
	if err != nil {
		return ans, err
	}
	if err := json.Unmarshal(raw, &ans); err != nil {
		return ans, fmt.Errorf("the answer is not an AuthzAnswer: %w", err)
	}
	if len(ans.Decisions) != len(req.Checks) {
		return ans, fmt.Errorf("the answer holds %d decisions for %d checks", len(ans.Decisions), len(req.Checks))
	}
	return ans, nil
}

// sweep drops expired decisions, and every decision when none expired. The
// caller holds z.mu.
func (z *controlPlane) sweep(now time.Time) {
	for k, d := range z.cache {
		if !now.Before(d.expires) {
			delete(z.cache, k)
		}
	}
	if len(z.cache) >= maxCached {
		clear(z.cache)
	}
}

// cacheKey identifies a decision by every fact that the request carries.
func cacheKey(p *fw.Principal, q fw.AuthzQuery) string {
	b, _ := json.Marshal(struct {
		P *fw.Principal `json:"p"`
		Q fw.AuthzQuery `json:"q"`
	}{p, q})
	return string(b)
}
