package pools

// This file is the overlay of a pool (docs/ARCHITECTURE.md section 14.9):
// the profiles and the allow rules that people add to the block fw of a
// pool while the front runs, through agent_put, agent_delete, and the
// approval card. The front keeps the overlay of each pool in a store that
// every replica of the front shares. WithOverlay merges it into the fw.yaml
// of the workers, and the hash of the pool covers the merged file, so a
// change of the overlay replaces the Tasks of the pool at the next pass,
// with no restart of the front.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"gopkg.in/yaml.v3"
)

// Overlay is what the front adds to the block fw of a pool. Profiles are
// added to fw.agents or replace the profile of the same name there, sorted
// by name. Allow holds the rules that the merge adds to fw.policy.allow,
// sorted by rule. Routines holds the routines of each agent that the store
// of the routines keeps (docs/ARCHITECTURE.md section 21.3), by agent, each
// with the standing of the kernel: the front reads them from that store at
// each pass, and the store of the overlay keeps none. An agent with an
// entry gets those routines in place of the routines of its profile. An
// empty entry takes the routines of the profile away, and it leaves a
// profile without routines as it is, so its hash stays.
type Overlay struct {
	Profiles []OverlayProfile        `json:"profiles,omitempty"`
	Allow    []OverlayRule           `json:"allow,omitempty"`
	Routines map[string][]fw.Routine `json:"-"`
}

// OverlayProfile is one profile of an overlay, with the subject that wrote
// it and the time.
type OverlayProfile struct {
	Profile fw.AgentProfile `json:"profile"`
	By      string          `json:"by,omitempty"`
	At      time.Time       `json:"at"`
}

// OverlayRule is one allow rule of an overlay: the name of a tool as the
// policy names it, such as github.create_pull_request or bash. Agent and
// Run name the approval that made the rule, By the subject that chose it,
// At the time, and Note what the person wrote, so a person sees why the
// rule exists.
type OverlayRule struct {
	Rule  string    `json:"rule"`
	Agent string    `json:"agent,omitempty"`
	Run   string    `json:"run,omitempty"`
	By    string    `json:"by,omitempty"`
	At    time.Time `json:"at"`
	Note  string    `json:"note,omitempty"`
}

// Empty reports whether the overlay adds nothing.
func (o Overlay) Empty() bool {
	return len(o.Profiles) == 0 && len(o.Allow) == 0 && len(o.Routines) == 0
}

// Profile returns the profile of that name in the overlay.
func (o Overlay) Profile(name string) (OverlayProfile, bool) {
	for _, e := range o.Profiles {
		if e.Profile.Name == name {
			return e, true
		}
	}
	return OverlayProfile{}, false
}

// WithProfile returns a copy of the overlay with e in place of the profile
// of the same name.
func (o Overlay) WithProfile(e OverlayProfile) Overlay {
	out := Overlay{Allow: slices.Clone(o.Allow), Routines: o.Routines}
	for _, old := range o.Profiles {
		if old.Profile.Name != e.Profile.Name {
			out.Profiles = append(out.Profiles, old)
		}
	}
	out.Profiles = append(out.Profiles, e)
	out.sort()
	return out
}

// WithRule returns a copy of the overlay with the rule r, in place of a
// rule with the same text.
func (o Overlay) WithRule(r OverlayRule) Overlay {
	out := Overlay{Profiles: slices.Clone(o.Profiles), Routines: o.Routines}
	for _, old := range o.Allow {
		if old.Rule != r.Rule {
			out.Allow = append(out.Allow, old)
		}
	}
	out.Allow = append(out.Allow, r)
	out.sort()
	return out
}

func (o *Overlay) sort() {
	sort.Slice(o.Profiles, func(i, j int) bool { return o.Profiles[i].Profile.Name < o.Profiles[j].Profile.Name })
	sort.Slice(o.Allow, func(i, j int) bool { return o.Allow[i].Rule < o.Allow[j].Rule })
}

// OverlayStore keeps the overlays of the pools, by the name of the pool.
// Read returns the overlay of a pool, sorted, and an empty overlay for a
// pool without one. PutProfile writes one profile, in place of the profile
// of the same name, DeleteProfile removes one and reports whether it was
// there, and AddRule writes one allow rule, in place of a rule with the
// same text. Describe says in a few words where the overlays live.
type OverlayStore interface {
	Read(ctx context.Context, pool string) (Overlay, error)
	PutProfile(ctx context.Context, pool string, e OverlayProfile) error
	DeleteProfile(ctx context.Context, pool, name string) (bool, error)
	AddRule(ctx context.Context, pool string, r OverlayRule) error
	Describe() string
}

// MemOverlays keeps the overlays in the memory of the process. It serves a
// front on the bus mem, which has no replica, and the tests.
type MemOverlays struct {
	mu sync.Mutex
	m  map[string]Overlay
}

// NewMemOverlays returns an empty store in memory.
func NewMemOverlays() *MemOverlays { return &MemOverlays{m: map[string]Overlay{}} }

// Describe implements OverlayStore.
func (s *MemOverlays) Describe() string {
	return "in the memory of this process, which a restart forgets and no replica shares"
}

// Read implements OverlayStore.
func (s *MemOverlays) Read(_ context.Context, pool string) (Overlay, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o := s.m[pool]
	return Overlay{Profiles: slices.Clone(o.Profiles), Allow: slices.Clone(o.Allow)}, nil
}

// PutProfile implements OverlayStore.
func (s *MemOverlays) PutProfile(_ context.Context, pool string, e OverlayProfile) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[pool] = s.m[pool].WithProfile(e)
	return nil
}

// DeleteProfile implements OverlayStore.
func (s *MemOverlays) DeleteProfile(_ context.Context, pool, name string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o := s.m[pool]
	if _, ok := o.Profile(name); !ok {
		return false, nil
	}
	out := Overlay{Allow: o.Allow}
	for _, e := range o.Profiles {
		if e.Profile.Name != name {
			out.Profiles = append(out.Profiles, e)
		}
	}
	s.m[pool] = out
	return true, nil
}

// AddRule implements OverlayStore.
func (s *MemOverlays) AddRule(_ context.Context, pool string, r OverlayRule) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[pool] = s.m[pool].WithRule(r)
	return nil
}

// Overlay returns the overlay that WithOverlay merged into the pool, empty
// for a pool of the configuration alone.
func (p *Pool) Overlay() Overlay { return p.overlay }

// WithOverlay returns a copy of the pool with the overlay merged into fw:
// each profile of the overlay is added to fw.agents or replaces the profile
// of the same name, and each rule is added to fw.policy.allow. A policy
// asks before it allows (the kernel architecture, section 7.5), so the
// merge also removes from fw.policy.ask the entries that name the same
// tool as a rule; an ask by a glob, a flag, or a pattern still holds. The
// copy is rendered and checked as Load renders a pool, and its hash covers
// the merged fw.yaml. The merge also ships the skills of the front that the
// profiles of the pool allow (shipSkills). The pool itself does not
// change. A runtime pool
// takes the profile of its agent from the overlay alone, so its overlay
// must hold that profile, else the error wraps ErrNoProfile. The copy
// checks the model, which a runtime pool before its overlay leaves out.
func (p *Pool) WithOverlay(o Overlay) (*Pool, error) {
	if p.worker == nil {
		return nil, fmt.Errorf("pools: %s is not loaded", p.key())
	}
	if p.runtime != nil {
		if _, ok := o.Profile(p.Agent); !ok {
			return nil, fmt.Errorf("pools: the runtime pool %s waits for the profile %s, which agent_put with pool %s writes: %w", p.Name, p.Agent, p.Name, ErrNoProfile)
		}
	}
	c := *p
	c.overlay = o
	c.lenient = false
	c.base = p.baseHash()
	merged, err := overlayFW(p.FW, o, p.worker.Agents)
	if err != nil {
		return nil, fmt.Errorf("pools: the overlay of the pool %s: %w", p.Name, err)
	}
	c.FW = merged
	c.skillFiles, c.skillNames, c.skillSkipped = nil, nil, nil
	if problems := c.render(p.front); len(problems) > 0 {
		return nil, fmt.Errorf("pools: the overlay of the pool %s does not fit: %s", p.Name, strings.Join(problems, "; "))
	}
	// The skills of the front that the profiles allow join the Workspace,
	// and the hash covers them.
	if err := c.shipSkills(); err != nil {
		return nil, err
	}
	if len(c.skillFiles) > 0 {
		if problems := c.render(p.front); len(problems) > 0 {
			return nil, fmt.Errorf("pools: the pool %s with the skills of the front does not fit: %s", p.Name, strings.Join(problems, "; "))
		}
	}
	return &c, nil
}

// WithRawOverlay returns the pool with the overlay attached and nothing
// merged. A place of the front uses it when the overlay does not fit the
// pool any more, so that a person can still remove the entry that does
// not fit; the pool itself keeps its configuration alone.
func (p *Pool) WithRawOverlay(o Overlay) *Pool {
	c := *p
	c.overlay = o
	return &c
}

// overlayFW returns the block fw of a pool with the overlay on top. Neither
// argument changes. configured holds the profiles of the configuration of
// the pool, whose prompt files stay paths.
func overlayFW(base map[string]any, o Overlay, configured map[string]fw.AgentProfile) (map[string]any, error) {
	out := make(map[string]any, len(base))
	for k, v := range base {
		out[k] = v
	}
	if len(o.Profiles) > 0 || len(o.Routines) > 0 {
		agents := map[string]any{}
		if m, ok := base["agents"].(map[string]any); ok {
			for k, v := range m {
				agents[k] = v
			}
		}
		for _, e := range o.Profiles {
			var same *fw.AgentProfile
			if c, ok := configured[e.Profile.Name]; ok {
				same = &c
			}
			if list, ok := o.Routines[e.Profile.Name]; ok {
				e.Profile.Routines = list
			}
			if len(e.Profile.Routines) == 0 {
				e.Profile.Routines = nil
			}
			m, err := profileMap(e.Profile, same)
			if err != nil {
				return nil, err
			}
			agents[e.Profile.Name] = m
		}
		for name, list := range o.Routines {
			if _, ok := o.Profile(name); ok {
				continue
			}
			cur, ok := agents[name].(map[string]any)
			if !ok {
				if agents[name] != nil {
					continue
				}
				if _, configured := agents[name]; !configured {
					continue // the pool does not run the agent
				}
				cur = map[string]any{} // a profile of the configuration with no key
			}
			if had, _ := cur["routines"].([]any); len(list) == 0 && len(had) == 0 {
				// No routine, before and after: the profile stays as it
				// is, so a first proposal replaces no Task.
				continue
			}
			m := make(map[string]any, len(cur)+1)
			for k, v := range cur {
				m[k] = v
			}
			v, err := routinesValue(list)
			if err != nil {
				return nil, fmt.Errorf("encode the routines of %s: %w", name, err)
			}
			m["routines"] = v
			agents[name] = m
		}
		if len(agents) > 0 {
			out["agents"] = agents
		}
	}
	if len(o.Allow) > 0 {
		policy := map[string]any{}
		if m, ok := base["policy"].(map[string]any); ok {
			for k, v := range m {
				policy[k] = v
			}
		}
		allow, ask := stringList(policy["allow"]), stringList(policy["ask"])
		for _, r := range o.Allow {
			rule := escapeDollars(strings.TrimSpace(r.Rule))
			if !slices.Contains(allow, rule) {
				allow = append(allow, rule)
			}
			ask = slices.DeleteFunc(ask, func(s string) bool { return strings.TrimSpace(s) == rule })
		}
		policy["allow"] = allow
		// An absent ask list falls back to the default of the kernel,
		// ask: [destructive], so the list is always written, as [] when
		// the rules took the last entry away.
		if ask == nil {
			ask = []string{}
		}
		policy["ask"] = ask
		out["policy"] = policy
	}
	return out, nil
}

// stringList reads a list of strings of a decoded YAML value.
func stringList(v any) []string {
	var out []string
	switch l := v.(type) {
	case []any:
		for _, x := range l {
			out = append(out, fmt.Sprint(x))
		}
	case []string:
		out = append(out, l...)
	}
	return out
}

// profileMap turns a profile into the value of its key under fw.agents.
// The worker expands ${NAME} in its fw.yaml and reads $$ as one $, so each
// text is escaped and comes back to the worker as a person wrote it: a
// profile never reads a variable of the worker. The worker also reads a
// system prompt or an instruction of one line as the path of a file when
// such a file exists, so such a text gets a line break at its end: a
// profile never reads a file of the worker, such as /proc/self/environ.
// An empty skills list means no skill, while an absent one means every
// skill, so the empty list stays. A text that equals the same field of
// the profile of the configuration (same) stays as it is: it is a prompt
// file of the pool that the worker must still read.
func profileMap(p fw.AgentProfile, same *fw.AgentProfile) (map[string]any, error) {
	keep := func(s string, i int) bool {
		if same == nil {
			return false
		}
		if i < 0 {
			return s == same.System
		}
		return i < len(same.Instructions) && s == same.Instructions[i]
	}
	if !keep(p.System, -1) {
		p.System = noPath(p.System)
	}
	p.Instructions = slices.Clone(p.Instructions)
	for i, s := range p.Instructions {
		if !keep(s, i) {
			p.Instructions[i] = noPath(s)
		}
	}
	var n yaml.Node
	if err := n.Encode(p); err != nil {
		return nil, fmt.Errorf("encode the profile %s: %w", p.Name, err)
	}
	escapeNode(&n)
	m := map[string]any{}
	if err := n.Decode(&m); err != nil {
		return nil, fmt.Errorf("encode the profile %s: %w", p.Name, err)
	}
	delete(m, "name")
	if len(p.Routines) == 0 {
		delete(m, "routines")
	}
	if p.Skills != nil && len(p.Skills) == 0 {
		m["skills"] = []any{}
	}
	return m, nil
}

// routinesValue turns the routines of an agent into the value of the key
// routines of its profile under fw.agents, with every text escaped as
// profileMap escapes it, so a template never reads a variable of the
// worker.
func routinesValue(list []fw.Routine) ([]any, error) {
	if list == nil {
		list = []fw.Routine{}
	}
	var n yaml.Node
	if err := n.Encode(list); err != nil {
		return nil, err
	}
	escapeNode(&n)
	out := []any{}
	if err := n.Decode(&out); err != nil {
		return nil, err
	}
	return out, nil
}

// noPath returns a text of one line with a line break at its end, so the
// loader of the worker never takes it for a path.
func noPath(s string) string {
	if s == "" || strings.ContainsAny(s, "\n\r") {
		return s
	}
	return s + "\n"
}

// escapeNode escapes the texts under n, as the profile store of the kernel
// does: ${ becomes $${ and $$ becomes $$$$.
func escapeNode(n *yaml.Node) {
	if n.Kind == yaml.ScalarNode {
		n.Value = escapeDollars(n.Value)
		return
	}
	for _, c := range n.Content {
		escapeNode(c)
	}
}

func escapeDollars(s string) string {
	if !strings.Contains(s, "$") {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '$' && i+1 < len(s) && (s[i+1] == '{' || s[i+1] == '$') {
			b.WriteString("$$" + s[i+1:i+2])
			if s[i+1] == '$' {
				b.WriteString("$")
			}
			i++
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// ErrNoProfile means that the overlay of a runtime pool holds no profile of
// the agent of the pool, so the pool has nothing to run yet.
var ErrNoProfile = errors.New("the overlay holds no profile of the agent of the pool")

// Profiles returns the profiles that the workers of the pool run, by name:
// the profiles of fw.agents, with the profiles of the overlay as they were
// written. The agent of a runtime pool comes from the overlay alone: its
// entry in fw.agents only holds the place of the profile.
func (p *Pool) Profiles() map[string]fw.AgentProfile {
	out := map[string]fw.AgentProfile{}
	if p.worker != nil {
		for name, prof := range p.worker.Agents {
			if p.runtime != nil && name == p.Agent {
				continue
			}
			prof.Name = name
			out[name] = prof
		}
	}
	for _, e := range p.overlay.Profiles {
		out[e.Profile.Name] = e.Profile
	}
	return out
}

// Policy returns fw.policy of the workers, with the rules of the overlay.
func (p *Pool) Policy() fw.PolicyConfig {
	if p.worker == nil {
		return fw.PolicyConfig{}
	}
	return p.worker.Policy
}

// Owns reports whether a Task of that name is a Task of the pool by its
// name, <pool>-<index>, in the atespace of the pool.
func (p *Pool) Owns(atespace, task string) bool {
	_, ok := p.indexOf(task)
	return ok && atespace == p.Atespace
}

// Decide returns how the workers of the pool decide a call of the tool with
// input for the profile agent, by fw.policy and by the policy of the
// profile. It knows no flag of the tool, so a rule by a flag, such as ask
// destructive, matches no call here, and the caller checks such a rule
// itself.
func (p *Pool) Decide(agent, tool string, input json.RawMessage) fw.Decision {
	rules := p.Policy().Rules
	if prof, ok := p.Profiles()[agent]; ok && prof.Policy != nil {
		rules = fw.MergeRules(rules, *prof.Policy)
	}
	if len(input) == 0 {
		input = json.RawMessage(`{}`)
	}
	return fw.NewRulesPolicy(rules).Decide(context.Background(), fw.ToolCall{Agent: agent, Spec: fw.ToolSpec{Name: tool}, Input: input})
}
