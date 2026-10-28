package pages

// This file holds the rules of the pages that an agent can write
// (docs/KNOWHOW.md section 6.3), and the hook of Know-how on each write.
// An agent writes only below agents/<its name>/ and outside agents/.
// Nobody but the front writes agents/<agent>/learned.md or a page below
// agents/<agent>/knowhow/: the front writes them through Go with ByFront,
// and every route and tool refuses them. A refusal names the path that
// the caller can use. The front reads the agent of a call from
// mcp.CallRun, which names the run behind a call of the static token.

import (
	"context"
	"strings"

	"github.com/bdubs00/featherweight-workbench/surface/mcp"
)

// OnWrite gets the path and the new hash of each page that a write
// created or changed, and the path with an empty hash for each page that
// a removal took away. A write that changes nothing and the index give
// nothing. The package knowhow sets it at its start, so a note that names
// an older version of the page goes out of date. Nil calls nothing. The
// function runs on the goroutine of the write, so it must not block.
var OnWrite func(ctx context.Context, path, hash string)

// wrote calls OnWrite.
func wrote(ctx context.Context, path, hash string) {
	if fn := OnWrite; fn != nil {
		fn(ctx, path, hash)
	}
}

// agentsDir is the folder of the pages of the agents.
const agentsDir = "agents/"

// Pages and folders of an agent that only the front writes.
const (
	learnedPage = "learned.md"
	knowhowDir  = "knowhow/"
)

// Reserved reports whether only the front writes the page at path p:
// agents/<agent>/learned.md and every page below agents/<agent>/knowhow/.
func Reserved(p string) bool {
	_, sub, ok := agentPart(p)
	return ok && (sub == learnedPage || strings.HasPrefix(sub, knowhowDir))
}

// agentPart splits a path below agents/ into the agent and the rest, and
// reports false for a path outside agents/<agent>/.
func agentPart(p string) (agent, rest string, ok bool) {
	after, ok := strings.CutPrefix(p, agentsDir)
	if !ok {
		return "", "", false
	}
	agent, rest, ok = strings.Cut(after, "/")
	if !ok || agent == "" || rest == "" {
		return "", "", false
	}
	return agent, rest, true
}

// mayWrite checks a write or a removal of the page at path p by the
// caller in ctx, by the rules of Know-how. A run behind an MCP call writes
// only below agents/<its agent>/ and outside agents/, and nobody writes a
// page that only the front writes. It returns nil, or ErrRefused with a
// reason that names the path to use.
func mayWrite(ctx context.Context, p string) error {
	agent, isRun := "", false
	if m, ok := mcp.CallRun(ctx); ok {
		agent, isRun = m.Agent, true
	}
	if Reserved(p) {
		return refuse("the front keeps %s, and nobody else writes it; write %s instead", p, instead(agent, p))
	}
	if !isRun || !strings.HasPrefix(p, agentsDir) {
		return nil
	}
	if agent == "" {
		return refuse("a run without an agent writes no page below %s; write a page outside it, such as %s", agentsDir, strings.TrimPrefix(p, agentsDir))
	}
	if owner, _, ok := agentPart(p); ok && owner == agent {
		return nil
	}
	return refuse("the agent %s writes only below %s%s/ and outside %s; write %s instead", agent, agentsDir, agent, agentsDir, instead(agent, p))
}

// refuse is a refusal of a write by the rules of Know-how.
func refuse(format string, args ...any) error {
	return fail(ErrRefused, format, args...)
}

// instead names the path that the caller can use in place of p: the same
// page in the folder of the agent of the caller, with a page that only
// the front writes moved under notes/. Without an agent, it keeps the
// folder of p.
func instead(agent, p string) string {
	owner, rest, ok := agentPart(p)
	if !ok {
		rest = strings.TrimPrefix(p, agentsDir)
	}
	if agent == "" {
		agent = owner
	}
	if agent == "" {
		return rest
	}
	if rest == learnedPage || strings.HasPrefix(rest, knowhowDir) {
		rest = "notes/" + strings.TrimPrefix(rest, knowhowDir)
	}
	out := agentsDir + agent + "/" + rest
	if CheckPath(out) != nil {
		out = agentsDir + agent + "/notes.md"
	}
	return out
}
