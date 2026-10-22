// Package pages is the wiki of the fleet (docs/ARCHITECTURE.md section
// 15): pages of Markdown that the agents and the people of a fleet keep
// together, with a history of each page. A page has a path such as
// notes/plan.md, a text with optional front matter (title and tags), and
// a hash, the SHA-256 of the whole text. Each write records who wrote,
// when, and why, and a write can name the hash that it read, so a stale
// write fails instead of losing a change.
//
// The package runs as a service of the kernel (fw.RegisterService). On a
// NATS bus the pages live in the key-value bucket <stream>_PAGES, one
// entry per page, so every replica of the front reads one truth. On the
// bus mem they live in the memory of the process, and two processes share
// nothing. On another bus the package logs once that the wiki is off and
// serves nothing. It mounts the routes under /v1/pages on the native
// surface (native.Extend), adds the MCP tools pages_list, pages_read,
// pages_write, pages_search, and pages_history (mcp.RegisterTools), and
// reads workbench.pages and FW_WORKBENCH_PAGES_*. Status gives a line for
// fw-workbench doctor. The verbs pages export and pages import of
// fw-workbench copy the wiki to files and back (Export, Import).
package pages

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"sync"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/surface/native"
	"github.com/bdubs00/featherweight-workbench/fleet"
	"github.com/bdubs00/featherweight-workbench/internal/bucket"
	"github.com/bdubs00/featherweight-workbench/surface/mcp"
)

func init() {
	fw.RegisterService(Start)
	native.Extend(mount)
	mcp.RegisterTools(tools)
}

// stores ties the wiki of an agent to the agent. Start attaches it, and the
// routes and the tools find it through Of.
var stores sync.Map // *fw.Agent to *Store

// Of returns the wiki of an agent, or nil when the process keeps none: the
// bus keeps no store, or the process does not serve.
func Of(a *fw.Agent) *Store {
	if a == nil {
		return nil
	}
	s, ok := stores.Load(a)
	if !ok {
		return nil
	}
	return s.(*Store)
}

// Attach makes s the wiki of a. A nil s detaches. A test attaches a store
// of its own.
func Attach(a *fw.Agent, s *Store) {
	if s == nil {
		stores.Delete(a)
		return
	}
	stores.Store(a, s)
}

// Open opens the wiki of the configuration cfg on its bus, for the service
// and for the verbs. It returns bucket.ErrNoStore for a bus that keeps no
// store. The function that it returns closes the connection; it is nil for
// the wiki in memory.
func Open(ctx context.Context, cfg *fw.Config, log *slog.Logger) (*Store, func(), error) {
	return open(ctx, cfg, log, bucket.Open)
}

// OpenOnce is Open for a verb that runs once: it fails at once when the
// NATS server does not answer.
func OpenOnce(ctx context.Context, cfg *fw.Config, log *slog.Logger) (*Store, func(), error) {
	return open(ctx, cfg, log, bucket.OpenOnce)
}

type opener func(ctx context.Context, cfg *fw.Config, c bucket.Config, log *slog.Logger) (bucket.Bucket, func(), error)

func open(ctx context.Context, cfg *fw.Config, log *slog.Logger, openBucket opener) (*Store, func(), error) {
	c, err := Load(cfg)
	if err != nil {
		return nil, nil, err
	}
	b, closeFn, err := openBucket(ctx, cfg, bucketConfig(c), log)
	if err != nil {
		return nil, nil, fmt.Errorf("pages: %w", err)
	}
	return New(b, c, log), closeFn, nil
}

// Start is the service of the wiki (fw.Service). It opens the wiki on the
// bus of the configuration and attaches it to the agent. On a bus that
// keeps no store it logs once that the wiki is off and returns nil. A
// configuration that does not fit stops the start.
func Start(ctx context.Context, a *fw.Agent) (io.Closer, error) {
	log := a.Logger().With("component", "pages")
	s, closeFn, err := Open(ctx, a.Config(), log)
	if errors.Is(err, bucket.ErrNoStore) {
		where, _ := bucket.Where(a.Config(), bucket.Config{Name: bucketName})
		log.Warn("pages: the wiki is off; it lives "+where, "bus", a.Config().Bus.Driver)
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	Attach(a, s)
	log.Info("pages: the wiki is on", "store", s.Describe(), "maxPages", s.cfg.MaxPages, "maxPageBytes", s.cfg.MaxPageBytes, "history", s.cfg.History)
	return service{a: a, close: closeFn}, nil
}

// service detaches the wiki of an agent and closes its connection.
type service struct {
	a     *fw.Agent
	close func()
}

func (s service) Close() error {
	Attach(s.a, nil)
	if s.close != nil {
		s.close()
	}
	return nil
}

// Allowed decides whether the caller in ctx can read the wiki, or write it
// when write is true. The process itself and the static token of the
// fleet, which the agents of the workers use, read and write. Any other
// caller reads with the operator role on an agent of the process or of a
// pool of the fleet (fw.ActionAgentRun), and writes with the owner role on
// one (fw.ActionAgentEdit). It returns nil to allow, and an error that
// wraps fw.ErrForbidden, or fw.ErrAuthzUnavailable when the decision point
// does not answer.
func Allowed(ctx context.Context, a *fw.Agent, write bool) error {
	p := fw.PrincipalFrom(ctx)
	if p == nil || p.Kind == fw.KindToken {
		return nil
	}
	action, role, verb := fw.ActionAgentRun, fw.AccessOperator, "reading"
	if write {
		action, role, verb = fw.ActionAgentEdit, fw.AccessOwner, "writing"
	}
	set := a.ProfileSet()
	names := make([]string, 0, len(set)+1)
	for name := range set {
		names = append(names, name)
	}
	if len(names) == 0 {
		names = append(names, "default")
	}
	sort.Strings(names)
	checks := make([]fw.AuthzCheck, len(names))
	for i, name := range names {
		checks[i] = fw.AuthzCheck{Action: action, Target: fw.Target{Agent: name}}
	}
	for _, err := range a.AuthorizeAll(ctx, checks) {
		if err == nil {
			return nil
		}
		if errors.Is(err, fw.ErrAuthzUnavailable) {
			return err
		}
	}
	list, pooled, err := fleet.PoolAgents(ctx, a)
	if pooled && err != nil {
		return fmt.Errorf("pages: read the agents of the pools: %w", err)
	}
	for _, pa := range list {
		if pa.Role == fw.AccessOwner.String() || (role == fw.AccessOperator && pa.Role == fw.AccessOperator.String()) {
			return nil
		}
	}
	return fmt.Errorf("%w: %s the wiki needs the %s role on an agent of the fleet", fw.ErrForbidden, verb, role)
}

// writer names the writer of a change from the caller in ctx: agent:<the
// profile> for a run behind an MCP call (mcp.CallRun), else the subject of
// the caller, else ByStore. It also returns the run.
func writer(ctx context.Context) (by, run string) {
	if m, ok := mcp.CallRun(ctx); ok {
		if m.Agent != "" {
			return "agent:" + m.Agent, m.Run
		}
		return "run:" + m.Run, m.Run
	}
	if p := fw.PrincipalFrom(ctx); p != nil && p.Subject != "" {
		return p.Subject, ""
	}
	return ByStore, ""
}
