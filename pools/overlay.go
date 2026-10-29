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
	"path"
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
// profile without routines as it is, so its hash stays. Packages holds
// the packages of the workers (packages.go), nil when the overlay sets
// none and the configuration of the pool decides.
//
// RoutinesFront is true when the front keeps routines. A pool whose
// workers reach the front (FrontServer) then gets routines.front instead
// of Routines: each run reads the routines of its agent from the front at
// its start, so the routines are not part of the merged file or of the
// hash, and a move of a routine replaces no Task (withRoutinesFront). A
// pool that does not reach the front gets Routines merged as before.
type Overlay struct {
	Profiles      []OverlayProfile        `json:"profiles,omitempty"`
	Allow         []OverlayRule           `json:"allow,omitempty"`
	Packages      *PackageSetting         `json:"packages,omitempty"`
	Routines      map[string][]fw.Routine `json:"-"`
	RoutinesFront bool                    `json:"-"`
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
	return len(o.Profiles) == 0 && len(o.Allow) == 0 && len(o.Routines) == 0 && o.Packages == nil
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
	out := Overlay{Allow: slices.Clone(o.Allow), Packages: o.Packages, Routines: o.Routines, RoutinesFront: o.RoutinesFront}
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
	out := Overlay{Profiles: slices.Clone(o.Profiles), Packages: o.Packages, Routines: o.Routines, RoutinesFront: o.RoutinesFront}
	for _, old := range o.Allow {
		if old.Rule != r.Rule {
			out.Allow = append(out.Allow, old)
		}
	}
	out.Allow = append(out.Allow, r)
	out.sort()
	return out
}

// WithPackages returns a copy of the overlay with the packages s, or
// without an entry of the packages for nil.
func (o Overlay) WithPackages(s *PackageSetting) Overlay {
	out := Overlay{Profiles: slices.Clone(o.Profiles), Allow: slices.Clone(o.Allow), Routines: o.Routines, RoutinesFront: o.RoutinesFront}
	if s != nil {
		c := s.Clone()
		out.Packages = &c
	}
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
// same text. UpdatePackages reads the entry of the packages, nil when the
// overlay holds none, and gives a copy to f. f returns the next entry, or
// nil to write nothing. The store writes the entry at the revision that it
// read, and calls f again when another writer came first, so two replicas
// of the front never lose a change. UpdatePackages returns the entry that
// the store holds after the call. Describe says in a few words where the
// overlays live.
type OverlayStore interface {
	Read(ctx context.Context, pool string) (Overlay, error)
	PutProfile(ctx context.Context, pool string, e OverlayProfile) error
	DeleteProfile(ctx context.Context, pool, name string) (bool, error)
	AddRule(ctx context.Context, pool string, r OverlayRule) error
	UpdatePackages(ctx context.Context, pool string, f func(cur *PackageSetting) (*PackageSetting, error)) (*PackageSetting, error)
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
	return Overlay{Profiles: slices.Clone(o.Profiles), Allow: slices.Clone(o.Allow)}.WithPackages(o.Packages), nil
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
	out := Overlay{Allow: o.Allow, Packages: o.Packages}
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

// UpdatePackages implements OverlayStore.
func (s *MemOverlays) UpdatePackages(_ context.Context, pool string, f func(*PackageSetting) (*PackageSetting, error)) (*PackageSetting, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o := s.m[pool]
	var cur *PackageSetting
	if o.Packages != nil {
		c := o.Packages.Clone()
		cur = &c
	}
	next, err := f(cur)
	if err != nil {
		return nil, err
	}
	if next == nil {
		return cur, nil
	}
	s.m[pool] = o.WithPackages(next)
	c := next.Clone()
	return &c, nil
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
	// The workers of a pool that reach the front read the routines of the
	// store at the start of each run, so the merge leaves them out.
	c.routinesFront = o.RoutinesFront && reachesFront(p.FW)
	merge := o
	if c.routinesFront {
		merge.Routines = nil
	}
	merged, err := overlayFW(p.FW, merge, p.worker.Agents)
	if err != nil {
		return nil, fmt.Errorf("pools: the overlay of the pool %s: %w", p.Name, err)
	}
	c.FW = withMemoryFront(merged, p.tokenEnv)
	if c.routinesFront {
		c.FW = withRoutinesFront(c.FW, p.tokenEnv)
	}
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
			return nil, fmt.Errorf("pools: the pool %s with the skills of the server does not fit: %s", p.Name, strings.Join(problems, "; "))
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

// FrontServer is the name of the entry of tools.mcp of a pool that reaches
// the MCP server of the front, with the static token of the fleet as its
// bearer. The seats of a bridge and the notes of Know-how find the front
// through it; the front configures no pool.
const FrontServer = "front"

// frontMCPPath is the path of the MCP server of the front, which the
// address of the entry FrontServer ends with.
const frontMCPPath = "/mcp"

// withMemoryFront returns the block fw of a pool with tools.memory.front
// set to the front that its entry FrontServer of tools.mcp reaches: the
// address of the entry without the path of the MCP server, and the
// variable of its bearer, which holds the static token that the workers
// get already, or tokenEnv when the entry names none. The notes of the
// workers then live at the front (docs/KNOWHOW.md sections 4.2 and 6.1).
// A pool without the entry, or one that sets tools.memory.front itself,
// stays as it is. The argument does not change.
func withMemoryFront(fwBlock map[string]any, tokenEnv string) map[string]any {
	tools, _ := fwBlock["tools"].(map[string]any)
	if tools == nil {
		return fwBlock
	}
	memory, _ := tools["memory"].(map[string]any)
	if _, set := memory["front"]; set {
		return fwBlock
	}
	url, bearer := frontOf(fwBlock, tokenEnv)
	if url == "" {
		return fwBlock
	}
	nextMemory := make(map[string]any, len(memory)+1)
	for k, v := range memory {
		nextMemory[k] = v
	}
	nextMemory["front"] = map[string]any{"url": url, "tokenEnv": bearer}
	nextTools := make(map[string]any, len(tools))
	for k, v := range tools {
		nextTools[k] = v
	}
	nextTools["memory"] = nextMemory
	out := make(map[string]any, len(fwBlock))
	for k, v := range fwBlock {
		out[k] = v
	}
	out["tools"] = nextTools
	return out
}

// frontOf returns the address of the front that the entry FrontServer of
// tools.mcp of a block fw reaches, without the path of the MCP server, and
// the variable of its bearer, or tokenEnv when the entry names none. The
// address is "" for a block without the entry.
func frontOf(fwBlock map[string]any, tokenEnv string) (url, bearer string) {
	tools, _ := fwBlock["tools"].(map[string]any)
	list, _ := tools["mcp"].([]any)
	for _, e := range list {
		m, _ := e.(map[string]any)
		if name, _ := m["name"].(string); strings.TrimSpace(name) != FrontServer {
			continue
		}
		endpoint, _ := m["endpoint"].(string)
		url = strings.TrimSuffix(strings.TrimSuffix(strings.TrimSpace(endpoint), "/"), frontMCPPath)
		if auth, ok := m["auth"].(map[string]any); ok {
			bearer, _ = auth["bearer"].(string)
		}
		break
	}
	if bearer = strings.TrimSpace(bearer); bearer == "" {
		bearer = tokenEnv
	}
	return url, bearer
}

// reachesFront reports whether the workers of a block fw reach the front:
// the block has the entry FrontServer of tools.mcp, or it sets
// routines.front itself.
func reachesFront(fwBlock map[string]any) bool {
	if url, _ := frontOf(fwBlock, ""); url != "" {
		return true
	}
	routines, _ := fwBlock["routines"].(map[string]any)
	_, set := routines["front"]
	return set
}

// withRoutinesFront returns the block fw of a pool with routines.front
// set to the front that its entry FrontServer of tools.mcp reaches, as
// withMemoryFront sets the front of the notes. Each run of a worker then
// reads the routines of its agent from the front at its start
// (docs/ROUTINES.md section 3.1). A block that sets routines.front itself
// stays as it is. The argument does not change.
func withRoutinesFront(fwBlock map[string]any, tokenEnv string) map[string]any {
	routines, _ := fwBlock["routines"].(map[string]any)
	if _, set := routines["front"]; set {
		return fwBlock
	}
	url, bearer := frontOf(fwBlock, tokenEnv)
	if url == "" {
		return fwBlock
	}
	next := make(map[string]any, len(routines)+1)
	for k, v := range routines {
		next[k] = v
	}
	next["front"] = map[string]any{"url": url, "tokenEnv": bearer}
	out := make(map[string]any, len(fwBlock))
	for k, v := range fwBlock {
		out[k] = v
	}
	out["routines"] = next
	return out
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

// MaxFileBytes returns tools.files.maxFileBytes of the workers: the most
// bytes of one file that their workspace takes, an attachment included.
// It is fw.DefaultMaxFileBytes when the configuration sets none.
func (p *Pool) MaxFileBytes() int64 {
	if p.worker != nil && p.worker.Tools.Files.MaxFileBytes > 0 {
		return p.worker.Tools.Files.MaxFileBytes
	}
	return fw.DefaultMaxFileBytes
}

// Policy returns fw.policy of the workers, with the rules of the overlay.
func (p *Pool) Policy() fw.PolicyConfig {
	if p.worker == nil {
		return fw.PolicyConfig{}
	}
	return p.worker.Policy
}

// Tools returns the tools that the workers of the pool offer a profile
// that names none: the built-in tools, then <server>.* for each MCP
// server, in the order of fw.tools.
func (p *Pool) Tools() []string {
	if p.worker == nil {
		return nil
	}
	out := append([]string{}, p.worker.Tools.Builtin...)
	for _, s := range p.worker.Tools.MCP {
		out = append(out, s.Name+".*")
	}
	return out
}

// ToolsProblem says in words which entries of tools, the tools list of the
// profile agent, no worker of the pool offers, and what the workers offer.
// It returns "" when the workers offer each entry, and when the pool has
// no configuration of its workers, so it cannot tell. An entry is a glob,
// as the kernel matches it. "*" asks for every tool. An entry without a
// dot names tools by their name: it must match a built-in tool of
// fw.tools.builtin, or a tool ask_<peer> of fw.tools.peers. An entry
// server.tool or server.* needs an MCP server of that name in
// fw.tools.mcp. The tools of a server live in the Tasks, so the front
// cannot tell the tool itself.
func (p *Pool) ToolsProblem(agent string, tools []string) string {
	if p.worker == nil {
		return ""
	}
	t := p.worker.Tools
	var names, servers []string
	for _, b := range t.Builtin {
		if b = strings.TrimSpace(b); b != "" && !slices.Contains(names, b) {
			names = append(names, b)
		}
	}
	if p.front.Packages != nil && !slices.Contains(names, EnvTool) {
		// The kernel gives a worker with the block environment the tool
		// env, also when the block fw names no built-in tool.
		names = append(names, EnvTool)
	}
	for _, peer := range t.Peers {
		if n := strings.TrimSpace(peer.Name); n != "" {
			names = append(names, "ask_"+n)
		}
	}
	for _, s := range t.MCP {
		if n := strings.TrimSpace(s.Name); n != "" {
			servers = append(servers, n)
		}
	}
	var missing []string
	for _, entry := range tools {
		if entry == "*" || slices.Contains(missing, entry) || offersTool(entry, names, servers, len(t.Peers) > 0) {
			continue
		}
		missing = append(missing, entry)
	}
	if len(missing) == 0 {
		return ""
	}
	return fmt.Sprintf("The pool of %s offers no tool %s: %s", agent, wordList(missing, "or"), offerWords(names, servers))
}

// offersTool reports whether a glob of a tools list matches a tool that
// the workers offer: a tool of names for a glob without a dot, else a
// tool of one of the MCP servers. With peers, a glob ask_* without a dot
// passes, because the kernel words the name of a peer tool itself.
func offersTool(entry string, names, servers []string, peers bool) bool {
	server, _, dotted := strings.Cut(entry, ".")
	if !dotted {
		for _, n := range names {
			if globMatches(entry, n) {
				return true
			}
		}
		return peers && strings.HasPrefix(entry, "ask_")
	}
	for _, s := range servers {
		if strings.HasPrefix(entry, s+".") || globMatches(server, s) {
			return true
		}
	}
	return false
}

// globMatches matches a name against a glob of a tools list, as the
// kernel does: the same text, or path.Match.
func globMatches(glob, name string) bool {
	if glob == name {
		return true
	}
	ok, err := path.Match(glob, name)
	return err == nil && ok
}

// offerWords words the tools that the workers offer, such as "it offers
// the tools bash and files, and the tools of the server ansible".
func offerWords(names, servers []string) string {
	var parts []string
	switch len(names) {
	case 0:
	case 1:
		parts = append(parts, "the tool "+names[0])
	default:
		parts = append(parts, "the tools "+wordList(names, "and"))
	}
	switch len(servers) {
	case 0:
	case 1:
		parts = append(parts, "the tools of the server "+servers[0])
	default:
		parts = append(parts, "the tools of the servers "+wordList(servers, "and"))
	}
	if len(parts) == 0 {
		return "it offers no tool at all"
	}
	return "it offers " + strings.Join(parts, ", and ")
}

// wordList joins words in a sentence: "a", "a or b", or "a, b, or c" with
// the word or.
func wordList(words []string, word string) string {
	switch len(words) {
	case 0:
		return ""
	case 1:
		return words[0]
	case 2:
		return words[0] + " " + word + " " + words[1]
	}
	return strings.Join(words[:len(words)-1], ", ") + ", " + word + " " + words[len(words)-1]
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
