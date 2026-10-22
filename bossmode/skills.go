package bossmode

// This file serves the skills of the front to the page of bossmode
// (docs/ARCHITECTURE.md section 20): the catalog at GET
// /v1/bossmode/skills and the MCP tool skills_list, which give the same
// data. The package skills reads the catalog.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/surface/native"
	"github.com/bdubs00/featherweight-workbench/fleet"
	"github.com/bdubs00/featherweight-workbench/manager"
	"github.com/bdubs00/featherweight-workbench/skills"
	"github.com/bdubs00/featherweight-workbench/surface/mcp"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Names of the MCP tools of the skills.
const (
	ToolSkillsList = "skills_list"
)

// Catalog is the answer of GET /v1/bossmode/skills.
type Catalog struct {
	Skills []skills.Entry `json:"skills"`
}

// mountSkills mounts the routes of the skills.
func mountSkills(k *native.Kit) {
	k.Handle("GET /v1/bossmode/skills", catalogRoute(k))
	k.Handle("POST /v1/bossmode/runs/{id}/skill", skillRunRoute(k))
}

// catalogRoute answers GET /v1/bossmode/skills with every skill that the
// front knows.
func catalogRoute(k *native.Kit) http.HandlerFunc {
	a := k.Agent()
	return func(w http.ResponseWriter, r *http.Request) {
		if err := mayReadCatalog(r.Context(), a); err != nil {
			k.Fail(w, r, err)
			return
		}
		native.WriteJSON(w, http.StatusOK, Catalog{Skills: skills.Catalog(a)})
	}
}

// mayReadCatalog decides whether the caller in ctx reads the catalog: the
// process itself, the static token of the fleet, and anyone with the
// operator role on an agent of this process or of a pool. It returns an
// error that wraps fw.ErrForbidden, or the error of a decision point that
// did not answer.
func mayReadCatalog(ctx context.Context, a *fw.Agent) error {
	p := fw.PrincipalFrom(ctx)
	if p == nil || p.Kind == fw.KindToken {
		return nil
	}
	profiles := a.Profiles()
	checks := make([]fw.AuthzCheck, 0, len(profiles))
	for _, pr := range profiles {
		checks = append(checks, fw.AuthzCheck{Action: fw.ActionAgentRun, Target: fw.Target{Agent: pr.Name}})
	}
	for _, err := range a.AuthorizeAll(ctx, checks) {
		if err == nil {
			return nil
		}
		if errors.Is(err, fw.ErrAuthzUnavailable) {
			return err
		}
	}
	list, _, err := fleet.PoolAgents(ctx, a)
	if err != nil {
		return err
	}
	for _, pa := range list {
		if pa.Role == fw.AccessOperator.String() || pa.Role == fw.AccessOwner.String() {
			return nil
		}
	}
	return fmt.Errorf("%w: the skills of the front are for the people who can start an agent; ask an owner for the operator role on an agent", fw.ErrForbidden)
}

// skillTools builds the tools of the skills for one MCP server. A process
// without bossmode gets none.
func skillTools(s *mcp.Server) []mcp.Tool {
	a := s.Agent()
	c, err := Load(a.Config())
	if err != nil || !c.Enabled {
		return nil
	}
	no := false
	return []mcp.Tool{
		mcp.NewTool(&sdk.Tool{
			Name:        ToolSkillsList,
			Title:       "Skills",
			Description: "List the skills of the front: each with its name, description, origin, whether a person can hand it to a run as a slash command, whether a model loads it on its own, its arguments, and its files. A skill is a set of instructions that an agent reads.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
			Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true, OpenWorldHint: &no},
		}, func(ctx context.Context, _ *sdk.CallToolRequest, _ struct{}) (*sdk.CallToolResult, any, error) {
			ctx, _, _ = manager.OnBehalf(ctx)
			if err := mayReadCatalog(ctx, a); err != nil {
				return mcp.ErrorResult(mcp.Explain(err)), nil, nil
			}
			list := skills.Catalog(a)
			return mcp.Result(catalogText(list), Catalog{Skills: list}), nil, nil
		}),
		skillSendTool(a),
	}
}

// catalogText is the catalog as text for a model: one line per skill.
func catalogText(list []skills.Entry) string {
	if len(list) == 0 {
		return "The front knows no skill."
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d skills of the front:", len(list))
	for _, e := range list {
		b.WriteString("\n- " + e.Name)
		if e.Description != "" {
			b.WriteString(": " + e.Description)
		}
		var notes []string
		if e.Origin != "" {
			notes = append(notes, "from "+e.Origin)
		}
		if !e.UserInvocable {
			notes = append(notes, "only an agent loads it")
		}
		if !e.ModelInvocable {
			notes = append(notes, "only a person hands it to a run")
		}
		if e.ArgumentHint != "" {
			notes = append(notes, "arguments "+e.ArgumentHint)
		} else if len(e.Arguments) > 0 {
			names := make([]string, len(e.Arguments))
			for i, arg := range e.Arguments {
				names[i] = arg.Name
			}
			notes = append(notes, "arguments "+strings.Join(names, ", "))
		}
		if len(e.Files) > 0 {
			notes = append(notes, fmt.Sprintf("%d more files", len(e.Files)))
		}
		if len(notes) > 0 {
			b.WriteString(" (" + strings.Join(notes, "; ") + ")")
		}
	}
	return b.String()
}
