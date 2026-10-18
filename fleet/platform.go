package fleet

// This file reads a fleet from the platforms instead of from records
// (docs/ARCHITECTURE.md section 4.8). The Task list of the ax
// API gives the members, their addresses, and their phases, and the egress
// stream of the workers gives the state of every run. A member that
// publishes no record, a bare kernel in a Task, is visible this way. The
// view reads the running Tasks once when they appear and lives on the
// stream after that; it never polls a Task, because a request wakes a
// Task that sleeps.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	v1alpha1 "github.com/google/ax/pkg/apis/v1alpha1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// Phases of an ax Task, as the view reads them.
const (
	phaseRunning   = "Running"
	phaseSuspended = "Suspended"
	phasePending   = "Pending"
)

// listPage is how many Tasks one ListTasks call asks for.
const listPage = 500

// EgressTypes are the ledger event types that a worker publishes for the
// view of the front: enough for the state, the title, the pending
// approval, and the turns of a run, and nothing of its content. A worker
// sets bus.egressTypes to this list.
var EgressTypes = []string{
	fw.EvRunQueued, fw.EvRunStarted, fw.EvRunResumed, fw.EvRunSuspended, fw.EvRunFinished,
	fw.EvApprovalRequested, fw.EvApprovalDecided, fw.EvRunAnnotated, fw.EvTurnStarted, fw.EvMessageReceived,
}

// platform is the view of the members that the platforms know.
type platform struct {
	f         *Fleet
	atespaces []string
	egress    string
	server    string
	prices    fw.Prices
	tokenEnv  string // the Secrets name of the static token for the first read of a running Task

	mu      sync.Mutex
	members map[string]*platformMember // by member name
	runs    map[string]*platformRun    // by run id
	conn    *grpc.ClientConn
	client  v1alpha1.AXClient
}

// platformMember is one member as a platform reports it. State is empty
// for a member that only the stream knows; the view then judges it by the
// time of its last event.
type platformMember struct {
	name     string
	url      string
	atespace string
	state    string
	phase    string // the phase of the Task as the ax API last gave it
	seen     time.Time
	filled   bool // the runs of the member were read once
	// resuming is true while a ResumeTask call runs for the member
	// (suspend.go), so a burst of requests makes one call.
	resuming bool
	// liveSince is when the phase of the Task last became Running; the
	// suspend controller counts idle time from it when the member has
	// no run (suspend.go).
	liveSince time.Time
	// agents names the profiles that the member serves, from its own
	// list read once and from its runs, so a job for one of them can wake
	// the member (suspend.go).
	agents map[string]bool
}

// platformRun is one run of a member, projected from the stream.
type platformRun struct {
	run     *fw.Run
	member  string
	updated time.Time
}

// startPlatform starts the sources that the configuration names: the Task
// list for the atespaces, and the egress stream.
func (f *Fleet) startPlatform(atespaces []string, egress, server, tokenEnv string) error {
	p := &platform{
		f:         f,
		atespaces: atespaces,
		egress:    strings.TrimSpace(egress),
		server:    strings.TrimSpace(server),
		prices:    f.a.Prices(),
		tokenEnv:  tokenEnv,
		members:   map[string]*platformMember{},
		runs:      map[string]*platformRun{},
	}
	if len(p.atespaces) > 0 {
		if p.server == "" {
			return errors.New("fleet: workbench.fleet.atespaces needs ax.server (FW_AX_SERVER), the ax API that lists the Tasks")
		}
		conn, err := grpc.NewClient(p.server, grpc.WithTransportCredentials(insecure.NewCredentials()))
		if err != nil {
			return fmt.Errorf("fleet: connect to the ax server %s: %w", p.server, err)
		}
		p.conn, p.client = conn, v1alpha1.NewAXClient(conn)
	}
	if p.egress != "" {
		stop, err := f.bus.Subscribe(f.ctx, p.egress+".>", p.onEvent)
		if err != nil {
			if p.conn != nil {
				_ = p.conn.Close()
			}
			return fmt.Errorf("fleet: subscribe to the egress stream %s: %w", p.egress, err)
		}
		f.viewMu.Lock()
		f.stops = append(f.stops, stop)
		f.viewMu.Unlock()
	}
	f.platform = p
	if p.client != nil {
		go p.loop(f.ctx)
	}
	f.log.Info("fleet reads the platforms", "atespaces", strings.Join(atespaces, ","), "egress", p.egress, "server", p.server)
	return nil
}

// closePlatform ends the sources.
func (f *Fleet) closePlatform() {
	p := f.platform
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.conn != nil {
		_ = p.conn.Close()
		p.conn, p.client = nil, nil
	}
}

// loop reads the Task list at every interval.
func (p *platform) loop(ctx context.Context) {
	p.poll(ctx)
	t := time.NewTicker(p.f.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.poll(ctx)
		}
	}
}

// poll reads the Tasks of every atespace and updates the members. A Task
// that disappears from the list leaves the view, and a Task that starts to
// run gets its runs read once.
func (p *platform) poll(ctx context.Context) {
	p.mu.Lock()
	client := p.client
	p.mu.Unlock()
	if client == nil {
		return
	}
	now := time.Now()
	seen := map[string]bool{}
	var fill []*platformMember
	for _, atespace := range p.atespaces {
		tasks, err := p.listTasks(ctx, client, atespace)
		if err != nil {
			p.f.log.Warn("fleet: list the Tasks", "atespace", atespace, "error", err)
			// The members of this atespace keep their last state.
			p.mu.Lock()
			for name, m := range p.members {
				if m.atespace == atespace {
					seen[name] = true
				}
			}
			p.mu.Unlock()
			continue
		}
		p.mu.Lock()
		for _, t := range tasks {
			taskName := t.GetMetadata().GetName()
			if taskName == "" {
				continue
			}
			name := atespace + "/" + taskName
			seen[name] = true
			m := p.members[name]
			if m == nil {
				m = &platformMember{name: name, url: "ax:" + name, atespace: atespace}
				p.members[name] = m
			}
			was := m.state
			m.phase = t.GetStatus().GetPhase()
			m.state, m.seen = stateOfPhase(m.phase), now
			if m.state == MemberLive && was != MemberLive {
				m.liveSince = now
			}
			if m.state == MemberLive && !m.filled {
				m.filled = true
				fill = append(fill, m)
			}
		}
		p.mu.Unlock()
	}
	p.mu.Lock()
	for name, m := range p.members {
		if m.atespace != "" && !seen[name] {
			delete(p.members, name)
			for id, r := range p.runs {
				if r.member == name {
					delete(p.runs, id)
				}
			}
		}
	}
	p.mu.Unlock()
	for _, m := range fill {
		p.backfill(ctx, m)
	}
	p.suspendIdle(ctx, now)
}

// listTasks reads every Task of an atespace, page by page.
func (p *platform) listTasks(ctx context.Context, client v1alpha1.AXClient, atespace string) ([]*v1alpha1.Task, error) {
	var out []*v1alpha1.Task
	for offset := int64(0); ; offset += listPage {
		cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		resp, err := client.ListTasks(cctx, &v1alpha1.ListTasksRequest{Atespace: atespace, Limit: listPage, Offset: offset})
		cancel()
		if err != nil {
			return nil, err
		}
		out = append(out, resp.GetTasks()...)
		if len(resp.GetTasks()) < listPage {
			return out, nil
		}
	}
}

// stateOfPhase maps the phase of a Task to the state of a member.
func stateOfPhase(phase string) string {
	switch phase {
	case phaseRunning:
		return MemberLive
	case phaseSuspended, phasePending, "":
		return MemberQuiet
	}
	return MemberStopped
}

// backfill reads the runs and the agents of a running Task once, with the
// static token of the fleet, so the view knows the runs that started before
// the front listened and the profiles that the Task serves. A running Task
// is awake, so the reads wake nobody.
func (p *platform) backfill(ctx context.Context, m *platformMember) {
	var runs struct {
		Runs []*fw.Run `json:"runs"`
	}
	if err := p.read(ctx, m, "/v1/runs", &runs); err != nil {
		p.f.log.Warn("fleet: read the runs of a Task; the view starts from the stream", "member", m.name, "error", err)
	}
	var agents struct {
		Agents []struct {
			Name string `json:"name"`
		} `json:"agents"`
	}
	if err := p.read(ctx, m, "/v1/agents", &agents); err != nil {
		p.f.log.Warn("fleet: read the agents of a Task; its runs name them later", "member", m.name, "error", err)
	}
	now := time.Now()
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, r := range runs.Runs {
		if r == nil || r.ID == "" {
			continue
		}
		m.serves(r.Agent)
		if cur, ok := p.runs[r.ID]; ok && cur.run.Seq >= r.Seq {
			continue
		}
		p.runs[r.ID] = &platformRun{run: r, member: m.name, updated: now}
	}
	for _, ag := range agents.Agents {
		m.serves(ag.Name)
	}
}

// serves records that the member serves a profile. The caller holds p.mu.
func (m *platformMember) serves(agent string) {
	agent = strings.TrimSpace(agent)
	if agent == "" {
		return
	}
	if m.agents == nil {
		m.agents = map[string]bool{}
	}
	m.agents[agent] = true
}

// read makes one GET through the router to a member, with the static token
// of the fleet, and decodes the JSON answer into out. The caller makes sure
// that the Task is awake, or means to wake it.
func (p *platform) read(ctx context.Context, m *platformMember, path string, out any) error {
	token, err := fw.EnvSecrets{}.Get(ctx, p.tokenEnv)
	if err != nil || strings.TrimSpace(token) == "" {
		return fmt.Errorf("no static token in the secret %s", p.tokenEnv)
	}
	cctx, cancel := context.WithTimeout(fw.WithCredential(ctx, "Bearer "+strings.TrimSpace(token)), readTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := p.f.Forward(cctx, m.name, req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s answered %d", path, resp.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, RecordLimit*4)).Decode(out); err != nil {
		return fmt.Errorf("decode %s: %w", path, err)
	}
	return nil
}

// readTimeout bounds one read of a Task through the router. A wake of a
// sleeping Task takes a few seconds, so it is longer than one request.
const readTimeout = 60 * time.Second

// onEvent applies one event of the egress stream to the run that it
// belongs to. An event without a node cannot be placed and is dropped.
func (p *platform) onEvent(e fw.Event) error {
	node := strings.TrimSpace(e.Node)
	if node == "" || e.Subject == "" {
		return nil
	}
	name, u, atespace := memberOf(node)
	now := time.Now()
	p.mu.Lock()
	defer p.mu.Unlock()
	m := p.members[name]
	if m == nil {
		m = &platformMember{name: name, url: u}
		p.members[name] = m
	}
	if m.atespace == "" {
		m.atespace = atespace
	}
	if m.state == "" || m.state == MemberQuiet {
		// An event is a sign of life, whatever the last phase said.
		m.seen = now
	}
	r := p.runs[e.Subject]
	if r == nil {
		r = &platformRun{run: &fw.Run{}, member: name}
		p.runs[e.Subject] = r
	}
	if e.Seq != 0 && e.Seq <= r.run.Seq && e.Type != fw.EvRunQueued {
		return nil // an old or repeated event
	}
	r.run.Apply(e, p.prices)
	r.member, r.updated = name, now
	m.serves(r.run.Agent)
	return nil
}

// memberOf turns the node of an event into a member name, an address, and
// an atespace. "ax:<atespace>/<task>" is a Task; anything else is a
// process that the view cannot reach.
func memberOf(node string) (name, u, atespace string) {
	if target, ok := strings.CutPrefix(node, "ax:"); ok {
		space, task, found := strings.Cut(target, "/")
		if found && space != "" && task != "" {
			return target, node, space
		}
	}
	return node, "", ""
}

// records returns the members of the platforms as records, for the view.
// Each record carries the runs of the member, the newest finished runs
// first cut to FinishedRuns, and the agents that the member serves and
// that its runs name, with the access block that this process has for
// each agent, because the front decides for a worker with its own
// configuration.
func (p *platform) records(now time.Time) []remote {
	p.mu.Lock()
	defer p.mu.Unlock()
	for id, r := range p.runs {
		if r.run.State.Terminal() && now.Sub(r.updated) > p.f.expire {
			delete(p.runs, id)
		}
	}
	byMember := map[string][]*fw.Run{}
	for _, r := range p.runs {
		if r.run.ID == "" {
			continue
		}
		byMember[r.member] = append(byMember[r.member], r.run)
	}
	cfg := p.f.a.Config()
	out := make([]remote, 0, len(p.members))
	for _, m := range p.members {
		runs := byMember[m.name]
		sort.Slice(runs, func(i, j int) bool {
			if !runs[i].CreatedAt.Equal(runs[j].CreatedAt) {
				return runs[i].CreatedAt.After(runs[j].CreatedAt)
			}
			return runs[i].ID > runs[j].ID
		})
		kept, omitted := pickRuns(runs)
		rec := &Record{
			Name:      m.name,
			URL:       m.url,
			Atespace:  m.atespace,
			Access:    Access{Mode: p.f.a.AccessMode(), Default: cfg.Access.Default, Admins: cfg.Access.Admins},
			Agents:    []RecordAgent{},
			Runs:      kept,
			Omitted:   omitted,
			State:     m.state,
			UpdatedAt: m.seen,
		}
		// The agents of a member are the profiles that it serves, read
		// from the Task once (backfill), and the ones that its runs name,
		// so an idle Task is visible to the people who can use it.
		seenAgent := map[string]bool{}
		addAgent := func(name string) {
			if name == "" || seenAgent[name] {
				return
			}
			seenAgent[name] = true
			ag := RecordAgent{Name: name}
			if prof, ok := p.f.a.ProfileNamed(name); ok {
				ag.Description, ag.Access = prof.Description, prof.Access
			}
			rec.Agents = append(rec.Agents, ag)
		}
		for name := range m.agents {
			addAgent(name)
		}
		for _, r := range runs {
			addAgent(r.Agent)
		}
		sort.Slice(rec.Agents, func(i, j int) bool { return rec.Agents[i].Name < rec.Agents[j].Name })
		seen := m.seen
		for _, r := range runs {
			if r.UpdatedAt.After(seen) {
				seen = r.UpdatedAt
			}
		}
		out = append(out, remote{rec: rec, seen: seen})
	}
	return out
}
