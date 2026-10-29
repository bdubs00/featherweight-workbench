package bridges

// This file opens a bridge and seats its members (docs/ARCHITECTURE.md
// section 18.3). With the placement queue, the front puts one run request
// per seat on the queue of the pool of the seat, as the operator, through
// the publish that the MCP tool enqueue uses (fleet.EnqueueFor). With the
// placement spawn, the front starts one run of the manager on the Task of
// the manager, and the manager seats each member with spawn_task, so the
// seats are child runs of the manager run and carry its batch.

import (
	"bytes"
	"context"
	"embed"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"text/template"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/fleet"
	"github.com/bdubs00/featherweight-workbench/pages"
	"github.com/bdubs00/featherweight-workbench/teams"
)

//go:embed instructions.tmpl plan.tmpl
var templateFiles embed.FS

// templates holds the instructions of a seat and the plan of a spawn
// bridge for the manager.
var templates = template.Must(template.New("bridges").ParseFS(templateFiles, "instructions.tmpl", "plan.tmpl"))

// Bounds of the plan of a bridge.
const (
	maxTitleRunes = 200
	maxGoalBytes  = 16 << 10
	maxTaskBytes  = 4 << 10
	// learnedHead bounds the part of agents/<agent>/learned.md that the
	// data block of a seat carries.
	learnedHead = 4 << 10
)

// handlePattern is the shape of a handle: lower-case letters, digits,
// dots, dashes, and underscores, at most 32.
var handlePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,31}$`)

// agentPattern is the shape of the name of a profile.
var agentPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// OpenSpec is the plan of a new bridge, as POST /v1/bridges and the tool
// bridge_open take it. Delegation and Caps are partial: a missing key
// takes the default.
type OpenSpec struct {
	Title      string           `json:"title"`
	Goal       string           `json:"goal"`
	Placement  string           `json:"placement,omitempty"`
	Members    []SeatSpec       `json:"members"`
	Delegation *DelegationPatch `json:"delegation,omitempty"`
	Caps       *CapsPatch       `json:"caps,omitempty"`
	// Owner is the person who owns the duty that opens the bridge through
	// OpenFor. The owner sees and uses the bridge as its operator does. A
	// body of a route never sets it.
	Owner string `json:"-"`
}

// SeatSpec is one seat of the plan: the handle, the agent behind it, its
// task, and optionally the pool that runs the agent, when several pools
// run it, and the budget of its runs.
type SeatSpec struct {
	Handle string      `json:"handle"`
	Agent  string      `json:"agent"`
	Task   string      `json:"task"`
	Pool   string      `json:"pool,omitempty"`
	Budget *SeatBudget `json:"budget,omitempty"`
}

// DelegationPatch changes the delegation of a bridge: a nil field stays.
type DelegationPatch struct {
	Approve *bool `json:"approve,omitempty"`
	Budget  *bool `json:"budget,omitempty"`
	Members *bool `json:"members,omitempty"`
	Report  *bool `json:"report,omitempty"`
}

// CapsPatch changes the caps of a bridge: a nil field stays.
type CapsPatch struct {
	MaxMessages        *int     `json:"maxMessages,omitempty"`
	MaxFanoutPerMinute *int     `json:"maxFanoutPerMinute,omitempty"`
	MaxPairMessages    *int     `json:"maxPairMessages,omitempty"`
	MaxTurns           *int     `json:"maxTurns,omitempty"`
	MaxCostUSD         *float64 `json:"maxCostUSD,omitempty"`
}

// apply changes the delegation d.
func (p *DelegationPatch) apply(d *Delegation) {
	if p == nil {
		return
	}
	set := func(dst *bool, v *bool) {
		if v != nil {
			*dst = *v
		}
	}
	set(&d.Approve, p.Approve)
	set(&d.Budget, p.Budget)
	set(&d.Members, p.Members)
	set(&d.Report, p.Report)
}

// apply changes the caps c.
func (p *CapsPatch) apply(c *Caps) {
	if p == nil {
		return
	}
	set := func(dst *int, v *int) {
		if v != nil {
			*dst = *v
		}
	}
	set(&c.MaxMessages, p.MaxMessages)
	set(&c.MaxFanoutPerMinute, p.MaxFanoutPerMinute)
	set(&c.MaxPairMessages, p.MaxPairMessages)
	set(&c.MaxTurns, p.MaxTurns)
	if p.MaxCostUSD != nil {
		c.MaxCostUSD = *p.MaxCostUSD
	}
}

// invalid is an error of a request that a route answers with 400.
func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", fw.ErrInvalidRequest, fmt.Sprintf(format, args...))
}

// checkSeat checks one seat of a plan (seatProblems).
func checkSeat(m SeatSpec) error {
	if p := seatProblems(m); len(p) > 0 {
		return invalid("%s", p[0].Text)
	}
	return nil
}

// operates reports whether the caller in ctx has the operator role on an
// agent: on the profile in the pool that runs it, else on the profile of
// this process, else on an agent that a member of the view serves. The
// process itself operates every agent. An error means that the decision
// point did not answer.
func (s *Store) operates(ctx context.Context, agent, pool string) (bool, error) {
	if fw.PrincipalFrom(ctx) == nil {
		return true, nil
	}
	pa, ok, err := fleet.PoolAgentNamed(ctx, s.a, agent, pool)
	if err != nil {
		return false, err
	}
	if ok {
		return pa.Role == fw.AccessOwner.String() || pa.Role == fw.AccessOperator.String(), nil
	}
	if _, ok := s.a.ProfileNamed(agent); ok {
		err := s.a.Authorize(ctx, fw.ActionAgentRun, fw.Target{Agent: agent})
		if errors.Is(err, fw.ErrAuthzUnavailable) {
			return false, err
		}
		return err == nil, nil
	}
	if v := fleet.Of(s.a); v != nil {
		members, err := v.Members(ctx)
		if err != nil {
			return false, err
		}
		for _, m := range members {
			for _, ag := range m.Agents {
				if ag.Name == agent && (ag.Role == fw.AccessOwner.String() || ag.Role == fw.AccessOperator.String()) {
					return true, nil
				}
			}
		}
	}
	return false, nil
}

// poolOf returns the pool that runs an agent, or "".
func (s *Store) poolOf(ctx context.Context, agent, pool string) string {
	pa, ok, err := fleet.PoolAgentNamed(s.asFront(ctx), s.a, agent, pool)
	if err != nil || !ok {
		return pool
	}
	return pa.Pool
}

// open opens a bridge from a plan for the person by, and seats its
// members. mgr names the manager run when the manager opens the bridge
// through bridge_open.
func (s *Store) open(ctx context.Context, spec OpenSpec, by *fw.Principal, mgr *Manager) (*record, error) {
	if p := checkPlan(spec, s.cfg, true); len(p) > 0 {
		return nil, invalid("%s", p[0].Text)
	}
	title := strings.TrimSpace(spec.Title)
	placement := strings.ToLower(strings.TrimSpace(spec.Placement))
	if placement == "" {
		placement = s.cfg.Placement
	}
	for i := range spec.Members {
		m := &spec.Members[i]
		m.Handle, m.Agent, m.Task, m.Pool = strings.TrimSpace(m.Handle), strings.TrimSpace(m.Agent), strings.TrimSpace(m.Task), strings.TrimSpace(m.Pool)
	}
	for _, m := range spec.Members {
		ok, err := s.operates(ctx, m.Agent, m.Pool)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, fmt.Errorf("%w: opening the bridge needs the operator role on the agent %s of the seat %s", fw.ErrForbidden, m.Agent, m.Handle)
		}
	}
	caps := s.cfg.caps()
	spec.Caps.apply(&caps)
	delegation := Delegation{Report: true}
	spec.Delegation.apply(&delegation)

	now := s.now().UTC()
	rec := &record{Bridge: Bridge{
		ID:         strings.ToLower(fw.NewID()),
		Title:      title,
		Goal:       strings.TrimSpace(spec.Goal),
		State:      StateOpen,
		Placement:  placement,
		CreatedAt:  now,
		Manager:    mgr,
		Delegation: delegation,
		Caps:       caps,
	}}
	rec.Owner = strings.TrimSpace(spec.Owner)
	if by != nil {
		rec.Operator, rec.OperatorKind = by.Subject, by.Kind
		rec.Teams = teams.Of(teams.Patterns(s.a.Config()), by)
		if n := s.knownName(by.Subject, by.Name); n != by.Subject {
			rec.OperatorName = n
		}
	}
	for _, m := range spec.Members {
		rec.Members = append(rec.Members, Member{Handle: m.Handle, Agent: m.Agent, Pool: s.poolOf(ctx, m.Agent, m.Pool), Task: m.Task, Runs: []string{}})
		seat := rec.seat(m.Handle)
		seat.Budget = m.Budget
		seat.Seatings = 1
	}
	// A bridge that a run opens through bridge_open starts with a post of
	// that run, which says what it did, before the line that the bridge
	// opened. The page folds the system lines of the opening under it.
	var first []Message
	if mgr != nil {
		post := newMessage(From{Kind: KindAgent, Handle: HandleManager, Run: mgr.Run}, openerText(rec.Members))
		post.Seq, post.At = rec.alloc(), now
		first = append(first, post)
	}
	switch placement {
	case PlacementQueue:
		for _, m := range rec.Members {
			if _, err := fleet.QueueFor(s.asFront(ctx), s.a, m.Agent, m.Pool); err != nil {
				return nil, fmt.Errorf("%w: the seat %s cannot reach a queue: %s", fw.ErrInvalidRequest, m.Handle, strings.TrimPrefix(strings.TrimPrefix(err.Error(), "fleet: "), fw.ErrInvalidRequest.Error()+": "))
			}
		}
	case PlacementSpawn:
		run, err := s.startManager(ctx, rec, s.planOf(ctx, rec, rec.Members, nil), "bridge:"+rec.ID+":manager")
		if err != nil {
			return nil, err
		}
		rec.Manager = run
	}
	opened := system(EventOpened, fmt.Sprintf("%s opened the channel with %s", s.nameOf(ctx, by), plural(len(rec.Members), "member", "members")))
	opened.Seq, opened.At = rec.alloc(), now
	if err := s.b.create(ctx, rec); err != nil {
		return nil, err
	}
	s.land(ctx, rec.ID, append(first, opened)...)
	for _, m := range rec.Members {
		emitMember(rec.ID, Member{Handle: m.Handle, Agent: m.Agent, Pool: m.Pool, Task: m.Task, Runs: []string{}, Presence: PresenceWaiting})
	}
	if placement == PlacementQueue {
		s.enqueue(ctx, rec, rec.Members, nil)
	}
	s.log.Info("bridges: a bridge is open", "bridge", rec.ID, "placement", placement, "members", len(rec.Members), "operator", rec.Operator)
	return rec, nil
}

// openerText is the post of the run that opened a bridge through
// bridge_open: what it did, from the open request alone, with no model
// call. It names each agent that it put on the channel with the first
// line of its task.
func openerText(members []Member) string {
	var b strings.Builder
	b.WriteString("I opened this channel and put " + countWords(len(members), "agent", "agents") + " on it.")
	for _, m := range members {
		b.WriteString("\n- **" + m.Handle + "**")
		if task := oneLine(firstLineOf(m.Task), 200); task != "" {
			b.WriteString(": " + task)
		}
	}
	return b.String()
}

// firstLineOf returns the first line of a text that holds words.
func firstLineOf(s string) string {
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			return l
		}
	}
	return ""
}

// numberWords are the numbers that a sentence writes in words.
var numberWords = []string{"no", "one", "two", "three", "four", "five", "six", "seven", "eight", "nine", "ten"}

// countWords writes a count with its noun, the small numbers in words,
// such as two agents.
func countWords(n int, one, many string) string {
	noun := many
	if n == 1 {
		noun = one
	}
	if n >= 0 && n < len(numberWords) {
		return numberWords[n] + " " + noun
	}
	return fmt.Sprintf("%d %s", n, noun)
}

// plural writes a number with the singular or the plural noun.
func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

// land writes new entries of the channel, whose seqs a change of the
// record gave, and calls the hook for each. The whole text of a cut entry
// goes first, under a key of its own, so an agent that sees the entry can
// read it whole. A write that fails leaves a gap in the channel, which a
// page reads past, and the log names it.
func (s *Store) land(ctx context.Context, id string, msgs ...Message) {
	for _, m := range msgs {
		// This replica gives the entry to its streams itself, so its
		// catch-up does not (catchup.go).
		s.fed(id, m.Seq)
		if m.Cut && m.whole != "" {
			if err := s.b.putWhole(s.asFront(ctx), id, m.Seq, m.whole); err != nil {
				s.log.Warn("bridges: the whole text of an entry is lost", "bridge", id, "seq", m.Seq, "error", err)
			}
		}
		if err := s.b.putEntry(s.asFront(ctx), id, m); err != nil {
			s.log.Error("bridges: an entry of the channel is lost", "bridge", id, "seq", m.Seq, "error", err)
			continue
		}
		emitMessage(id, m)
	}
}

// enqueue puts one run request per seat on the queue of the pool of the
// seat, as the operator, with the batch of the bridge, the seat in the
// metadata, and the instructions of the bridge: the intake of its pool,
// else the queue of its agent (fleet.QueueFor). inputs holds the input of
// the run of a seat by handle (inputOf). A seat whose request does not
// reach the queue gets a line in the channel.
func (s *Store) enqueue(ctx context.Context, rec *record, seats []Member, inputs map[string]string) {
	// The seats of one agent on one pool share a queue.
	type place struct{ agent, pool string }
	byPlace := map[place][]fw.RunRequest{}
	var order []place
	handles := map[place][]string{}
	for _, m := range seats {
		at := place{m.Agent, m.Pool}
		req := s.requestOf(ctx, rec, m, inputOf(m, inputs))
		if _, ok := byPlace[at]; !ok {
			order = append(order, at)
		}
		byPlace[at] = append(byPlace[at], req)
		handles[at] = append(handles[at], m.Handle)
	}
	for _, at := range order {
		queue, sent, err := fleet.EnqueueFor(s.asFront(ctx), s.a, at.agent, at.pool, byPlace[at])
		if err == nil {
			continue
		}
		s.log.Warn("bridges: a seat did not reach its queue", "bridge", rec.ID, "agent", at.agent, "pool", at.pool, "queue", queue, "error", err)
		for _, h := range handles[at][sent:] {
			s.postLines(ctx, rec.ID, system(EventFailed, fmt.Sprintf("The server failed to queue the run of %s: %s", h, oneLine(strings.TrimPrefix(err.Error(), "fleet: "), 300))))
		}
	}
}

// inputOf is the input of a new run of a seat: its entry in inputs, by
// handle, or else the task of the seat.
func inputOf(m Member, inputs map[string]string) string {
	if in := inputs[m.Handle]; in != "" {
		return in
	}
	return m.Task
}

// requestOf is the run request of a seat on the queue of its pool, with
// the input of the run, and the head of the learned page of its agent as
// a data block.
func (s *Store) requestOf(ctx context.Context, rec *record, m Member, input string) fw.RunRequest {
	seat := rec.seat(m.Handle)
	title := oneLine(m.Handle+": "+m.Task, maxTitleRunes)
	req := fw.RunRequest{
		Input:          input,
		Instructions:   s.instructionsOf(ctx, rec, m),
		Context:        s.learnedContext(ctx, m.Agent),
		Metadata:       map[string]string{fw.MetaBatch: BatchOf(rec.ID), MetaMember: m.Handle, fw.MetaTitle: title},
		Principal:      rec.Operator,
		PrincipalKind:  rec.OperatorKind,
		IdempotencyKey: fmt.Sprintf("bridge:%s:%s:%d", rec.ID, m.Handle, max(seat.Seatings, 1)),
		Profile:        s.frontTools(ctx, m),
	}
	if b := budgetOf(seat.Budget); b != nil {
		req.Budget = b
	}
	return req
}

// budgetOf turns the budget of a seat into a budget of the kernel, or nil.
func budgetOf(b *SeatBudget) *fw.Budget {
	if b == nil || (b.MaxTurns == 0 && b.Deadline == "") {
		return nil
	}
	out := fw.Budget{MaxTurns: b.MaxTurns}
	if d, err := time.ParseDuration(b.Deadline); err == nil {
		out.Deadline = d
	}
	return &out
}

// bridgeTools are the tools of the front that a seat needs, as the policy
// names them through the tools.mcp entry front of a pool.
var bridgeTools = []string{"front.bridge_read", "front.bridge_post", "front.pages_read", "front.pages_write", "front.pages_search"}

// frontTools returns an inline profile that adds the tools of the front
// to a seat, when the pool needs it: the profile of the pool lists its
// tools, names a tool of the front, so its workers reach the front, and
// leaves out the tools of a bridge. Otherwise it returns nil, and the run
// keeps the tools of its profile. The front configures no pool: a pool
// that reaches the front carries the entry front in tools.mcp.
func (s *Store) frontTools(ctx context.Context, m Member) *fw.AgentProfile {
	pa, ok, err := fleet.PoolAgentNamed(s.asFront(ctx), s.a, m.Agent, m.Pool)
	if err != nil || !ok || len(pa.Tools) == 0 {
		return nil
	}
	reaches, covered := false, map[string]bool{}
	for _, t := range pa.Tools {
		switch {
		case t == "*" || t == "front.*":
			return nil
		case strings.HasPrefix(t, "front."):
			reaches = true
			covered[t] = true
		}
	}
	if !reaches {
		return nil
	}
	tools := append([]string(nil), pa.Tools...)
	for _, t := range bridgeTools {
		if !covered[t] {
			tools = append(tools, t)
		}
	}
	if len(tools) == len(pa.Tools) {
		return nil
	}
	return &fw.AgentProfile{Tools: tools}
}

// rosterLine is one seat in the instructions of a bridge.
type rosterLine struct {
	Handle, Agent, Task string
	You                 bool
}

// instructionsData is the data of the template of the instructions. Peer
// is the handle of a teammate for an example, or teammate when the seat
// has none.
type instructionsData struct {
	ID, Title, Goal, Operator string
	Handle, Agent, Task, Peer string
	Roster                    []rosterLine
}

// instructionsOf renders the instructions of a seat: the bridge, the
// goal, the roster with the task of each seat, and the rules of the
// channel and of the wiki. The head of the page agents/<agent>/learned.md
// is text that earlier runs and people wrote, so it goes in a data block
// of the run (learnedContext), never in the instructions.
func (s *Store) instructionsOf(ctx context.Context, rec *record, m Member) string {
	d := instructionsData{ID: rec.ID, Title: rec.Title, Goal: rec.Goal, Operator: s.operatorName(rec, "The operator"), Handle: m.Handle, Agent: m.Agent, Task: m.Task}
	if d.Goal == "" {
		d.Goal = rec.Title
	}
	for _, o := range rec.Members {
		if o.Fired {
			continue
		}
		d.Roster = append(d.Roster, rosterLine{Handle: o.Handle, Agent: o.Agent, Task: oneLine(o.Task, 300), You: o.Handle == m.Handle})
		if d.Peer == "" && o.Handle != m.Handle {
			d.Peer = o.Handle
		}
	}
	if d.Peer == "" {
		d.Peer = "teammate"
	}
	var b bytes.Buffer
	if err := templates.ExecuteTemplate(&b, "instructions.tmpl", d); err != nil {
		s.log.Error("bridges: the template of the instructions failed", "error", err)
	}
	return b.String()
}

// learnedOf returns the head of the page agents/<agent>/learned.md, up to
// learnedHead bytes and cut at a line, or "" without the page.
func (s *Store) learnedOf(ctx context.Context, agent string) string {
	w := pages.Of(s.a)
	if w == nil {
		return ""
	}
	p, err := w.Read(s.asFront(ctx), learnedPath(agent), 0)
	if err != nil {
		return ""
	}
	text := strings.TrimSpace(p.Text)
	if head, cut := cutText(text, learnedHead); cut {
		if i := strings.LastIndexByte(head, '\n'); i > 0 {
			head = head[:i]
		}
		text = strings.TrimSpace(head)
	}
	return text
}

// learnedPath is the page of the hearts of an agent.
func learnedPath(agent string) string { return "agents/" + agent + "/learned.md" }

// ContextLearned is the source of the data block of a seat that holds the
// head of the page agents/<agent>/learned.md (docs/KNOWHOW.md section
// 6.2).
const ContextLearned = "learned"

// learnedContext returns the data blocks of a run of a seat: the head of
// the page agents/<agent>/learned.md, after a line that says what it is,
// or none without the page.
func (s *Store) learnedContext(ctx context.Context, agent string) []fw.RunContext {
	text := s.learnedOf(ctx, agent)
	if text == "" {
		return nil
	}
	head := "People marked these results of earlier work of " + agent + " with a heart. They come from the page " + learnedPath(agent) + " of the wiki.\n\n"
	return []fw.RunContext{{Source: ContextLearned, Text: head + text}}
}

// planSeat is one seat in the plan of a spawn bridge.
type planSeat struct {
	Handle, Agent, Input string
	MaxTurns             int
	Deadline             string
	Instructions         string
}

// planOf renders the plan of a spawn bridge for the manager, for the
// seats that are not fired: every seat at the open, and the seats to seat
// now for a new manager run (place). inputs holds the input of the run of
// a seat by handle (inputOf).
func (s *Store) planOf(ctx context.Context, rec *record, seats []Member, inputs map[string]string) string {
	d := struct {
		ID, Title, Goal, Operator string
		Seats                     []planSeat
	}{ID: rec.ID, Title: rec.Title, Goal: rec.Goal, Operator: s.operatorName(rec, "the operator")}
	if d.Goal == "" {
		d.Goal = rec.Title
	}
	for _, m := range seats {
		if m.Fired {
			continue
		}
		ps := planSeat{Handle: m.Handle, Agent: m.Agent, Input: oneLine(inputOf(m, inputs), maxTaskBytes), Instructions: s.instructionsOf(ctx, rec, m)}
		if b := rec.seat(m.Handle).Budget; b != nil {
			ps.MaxTurns, ps.Deadline = b.MaxTurns, b.Deadline
		}
		d.Seats = append(d.Seats, ps)
	}
	var b bytes.Buffer
	if err := templates.ExecuteTemplate(&b, "plan.tmpl", d); err != nil {
		s.log.Error("bridges: the template of the plan failed", "error", err)
	}
	return b.String()
}

// managerMember returns the fleet member that runs the manager: a Task of
// the pool whose agent is the profile of the manager, the first live one,
// else the first; without such a pool, the first member of the view whose
// agents hold the profile.
func (s *Store) managerMember(ctx context.Context) (string, error) {
	if s.cfg.Manager == "" {
		return "", invalid("a spawn bridge needs a manager, and the configuration names none; set workbench.bossmode.manager (%s), or open a queue bridge", EnvManager)
	}
	v := fleet.Of(s.a)
	if v == nil {
		return "", invalid("a spawn bridge needs the fleet, because the manager runs on a Task of it")
	}
	ctx = s.asFront(ctx)
	if pv, ok := v.(fleet.PoolView); ok {
		list, err := pv.Pools(ctx)
		if err != nil {
			return "", err
		}
		for _, p := range list {
			if p.Agent != s.cfg.Manager || len(p.Tasks) == 0 {
				continue
			}
			pick := p.Tasks[0].Name
			for _, t := range p.Tasks {
				if t.Phase == "Running" {
					pick = t.Name
					break
				}
			}
			return p.Atespace + "/" + pick, nil
		}
	}
	members, err := v.Members(ctx)
	if err != nil {
		return "", err
	}
	for _, m := range members {
		if m.Self {
			continue
		}
		for _, ag := range m.Agents {
			if ag.Name == s.cfg.Manager {
				return m.Name, nil
			}
		}
	}
	return "", fmt.Errorf("%w: no Task of the fleet runs the manager profile %s, so the manager cannot seat a spawn bridge", ErrConflict, s.cfg.Manager)
}

// startManager starts a run of the manager on its Task with the input,
// as the operator, with the batch of the bridge, through the forward of
// the fleet with the static token, which names the operator as the person
// of the run. It returns the manager of the bridge.
func (s *Store) startManager(ctx context.Context, rec *record, input, idem string) (*Manager, error) {
	member, err := s.managerMember(ctx)
	if err != nil {
		return nil, err
	}
	body := map[string]any{
		"agent":    s.cfg.Manager,
		"input":    input,
		"title":    oneLine("Channel: "+rec.Title, maxTitleRunes),
		"metadata": map[string]string{fw.MetaBatch: BatchOf(rec.ID)},
		"wait":     false,
	}
	if rec.Operator != "" {
		body["principal"] = rec.Operator
		if rec.OperatorKind != "" {
			body["principalKind"] = rec.OperatorKind
		}
	}
	var run fw.Run
	if err := s.call(ctx, member, http.MethodPost, "/v1/runs", body, idem, &run); err != nil {
		return nil, fmt.Errorf("bridges: start the manager on %s: %w", member, err)
	}
	if run.ID == "" {
		return nil, fmt.Errorf("bridges: the manager on %s answered no run", member)
	}
	return &Manager{Member: member, Run: run.ID}, nil
}

// managerRun is the manager run of a record, or "".
func managerRun(rec *record) string {
	if rec.Manager == nil {
		return ""
	}
	return rec.Manager.Run
}

// orOperator names the operator, or "operator" when the bridge has none.
func orOperator(s string) string {
	if s == "" {
		return HandleOperator
	}
	return s
}
