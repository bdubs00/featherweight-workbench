// Package faces keeps the faces of the agents of a fleet
// (docs/ARCHITECTURE.md section 16): a picture, a display name, and a
// one-line tagline for each agent, which the operator view shows beside
// the work of the agent. The picture is an SVG that a person uploads or
// that the manager writes, and only an SVG that passes the sanitizer
// (Sanitize) lands. An agent without a picture has an identicon, a
// symmetric pattern from the hash of its name, which costs no model call.
//
// The package runs as a service of the kernel (fw.RegisterService). On a
// NATS bus the faces live in the key-value bucket <stream>_FACES, one
// entry per agent with the last ten values, so every replica of the front
// reads one truth. On the bus mem they live in the memory of the process,
// and two processes share nothing. On another bus the package logs once
// that the faces are off and serves nothing. It mounts the routes
// /v1/faces/{agent} on the native surface (native.Extend) and adds the MCP
// tool faces_put (mcp.RegisterTools). Status gives a line for
// fw-workbench doctor.
package faces

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
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

// bucketConfig is the bucket of the faces.
var bucketConfig = bucket.Config{
	Name:        "FACES",
	Description: "The faces of the agents of a featherweight fleet: an SVG, a display name, and a tagline, one entry per agent.",
	History:     History,
	MaxValue:    MaxSVGBytes + 4<<10,
}

// stores ties the faces of an agent to the agent.
var stores sync.Map // *fw.Agent to *Store

// Of returns the faces of an agent, or nil when the process keeps none.
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

// Attach makes s the faces of a. A nil s detaches.
func Attach(a *fw.Agent, s *Store) {
	if s == nil {
		stores.Delete(a)
		return
	}
	stores.Store(a, s)
}

// Open opens the faces of the configuration cfg on its bus. It returns
// bucket.ErrNoStore for a bus that keeps no store. The function that it
// returns closes the connection; it is nil for the faces in memory.
func Open(ctx context.Context, cfg *fw.Config, log *slog.Logger) (*Store, func(), error) {
	b, closeFn, err := bucket.Open(ctx, cfg, bucketConfig, log)
	if err != nil {
		return nil, nil, fmt.Errorf("faces: %w", err)
	}
	return New(b, log), closeFn, nil
}

// Start is the service of the faces (fw.Service). It opens the faces on
// the bus of the configuration and attaches them to the agent. On a bus
// that keeps no store it logs once that the faces are off and returns nil.
func Start(ctx context.Context, a *fw.Agent) (io.Closer, error) {
	log := a.Logger().With("component", "faces")
	s, closeFn, err := Open(ctx, a.Config(), log)
	if errors.Is(err, bucket.ErrNoStore) {
		where, _ := bucket.Where(a.Config(), bucketConfig)
		log.Warn("faces: the faces are off; they live "+where, "bus", a.Config().Bus.Driver)
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	Attach(a, s)
	return service{a: a, close: closeFn}, nil
}

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

// Status says where the faces of the configuration cfg live, for a line of
// fw-workbench doctor. A bus that keeps no store gives a line that says
// that the faces are off.
func Status(cfg *fw.Config) (bool, string) {
	where, on := bucket.Where(cfg, bucketConfig)
	if !on {
		return true, "faces: off; the faces live " + where
	}
	return true, fmt.Sprintf("faces: the faces live in %s, with SVGs of at most %d KB and %d versions of each face", where, MaxSVGBytes>>10, History)
}

// Check decides whether the caller in ctx can do the action of the kernel
// on the agent, such as fw.ActionAgentEdit to change its face. The agent
// must be a profile of this process or of a pool of the fleet
// (fleet.PlaceOf), else the error wraps fw.ErrProfileNotFound or
// fw.ErrInvalidRequest. The process itself and the static token of the
// fleet can do every action; any other caller needs the role of the
// action on that agent, in its place.
func Check(ctx context.Context, a *fw.Agent, agent, action string) error {
	if err := CheckAgent(agent); err != nil {
		return fmt.Errorf("%w: %s", fw.ErrInvalidRequest, err.Error())
	}
	at, err := fleet.PlaceOf(ctx, a, agent, "")
	if err != nil {
		return err
	}
	if fleet.PoolOf(at) == "" && !localAgent(a, agent) {
		return fmt.Errorf("%w: %s", fw.ErrProfileNotFound, agent)
	}
	if p := fw.PrincipalFrom(ctx); p == nil || p.Kind == fw.KindToken {
		return nil
	}
	return at.Authorize(ctx, action, agent)
}

// localAgent reports whether the process runs the agent: a profile of it,
// or default when it has no profile.
func localAgent(a *fw.Agent, agent string) bool {
	if _, ok := a.ProfileNamed(agent); ok {
		return true
	}
	return agent == "default" && len(a.ProfileSet()) == 0
}

// writer names the writer of a change from the caller in ctx: agent:<the
// profile> for a run behind an MCP call (mcp.CallRun), else the subject of
// the caller.
func writer(ctx context.Context) string {
	if m, ok := mcp.CallRun(ctx); ok {
		if m.Agent != "" {
			return "agent:" + m.Agent
		}
		return "run:" + m.Run
	}
	if p := fw.PrincipalFrom(ctx); p != nil && p.Subject != "" {
		return p.Subject
	}
	return "fw-workbench"
}
