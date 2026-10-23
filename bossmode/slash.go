package bossmode

// This file hands a skill to a running conversation as a slash command of
// a person (docs/ARCHITECTURE.md section 20.3): the route POST
// /v1/bossmode/runs/{id}/skill for a conversation that is no bridge, such
// as the manager conversation, and the MCP tool skill_send for a run or a
// bridge. The front expands the skill of the catalog and sends the message
// pinned, so it survives every compaction of the run. A bridge takes the
// slash command on its own route, POST /v1/bridges/{id}/messages with
// skill. A run never hands a skill to another run: only a person does.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/surface/native"
	"github.com/bdubs00/featherweight-workbench/bridges"
	"github.com/bdubs00/featherweight-workbench/fleet"
	"github.com/bdubs00/featherweight-workbench/skills"
	"github.com/bdubs00/featherweight-workbench/surface/mcp"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// ToolSkillSend is the MCP tool of a slash command.
const ToolSkillSend = "skill_send"

// sendTimeout bounds the forward of a slash command to the owner of a run.
// A Task that sleeps wakes for it.
const sendTimeout = 60 * time.Second

// SkillRequest is the body of POST /v1/bossmode/runs/{id}/skill: the
// skill, its arguments, and a text of the person that follows the skill.
type SkillRequest struct {
	Name string `json:"name"`
	Args string `json:"args,omitempty"`
	Text string `json:"text,omitempty"`
}

// ErrPinnedBound is the refusal of the owner of a run to pin a message,
// because the message takes the pinned messages of the run past
// fw.MaxPinnedBytes.
var ErrPinnedBound = errors.New("bossmode: the pinned messages of the run are full")

// memberError is an answer of the owner of a run other than 2xx.
type memberError struct {
	member string
	status int
	detail string
}

func (e *memberError) Error() string {
	return fmt.Sprintf("bossmode: the fleet member %s answered %d: %s", e.member, e.status, e.detail)
}

// pinnedRefusal reports whether the words of a refusal of the kernel say
// that the pinned messages of a run are full. The kernel refuses with
// fw.ErrInvalidRequest and names the bound, and it exports no error of its
// own for it, so the front reads the words.
func pinnedRefusal(detail string) bool {
	return strings.Contains(detail, "pinned messages of run") && strings.Contains(detail, "the bound is")
}

// skillRunRoute answers POST /v1/bossmode/runs/{id}/skill: 202 with the
// run object, as POST /v1/runs/{id}/messages answers.
func skillRunRoute(k *native.Kit) http.HandlerFunc {
	a := k.Agent()
	return func(w http.ResponseWriter, r *http.Request) {
		var body SkillRequest
		if !k.Decode(w, r, &body) {
			return
		}
		// The caller must reach the run before the front reads a skill for
		// it, so a slash command tells nothing about the skills to a caller
		// who cannot act on the run.
		target, err := runTarget(r.Context(), a, r.PathValue("id"))
		if err != nil {
			failSkill(k, w, r, err)
			return
		}
		ctx, err := toOwner(r.Context(), a, target)
		if err != nil {
			failSkill(k, w, r, err)
			return
		}
		x, err := skills.Prepare(a, skills.Command{Name: body.Name, Args: body.Args}, body.Text)
		if err != nil {
			failSkill(k, w, r, err)
			return
		}
		run, err := sendSkill(ctx, a, target, x.Message)
		if err != nil {
			failSkill(k, w, r, err)
			return
		}
		a.Logger().Info("bossmode: a person sent a skill to a run", "run", run.ID, "skill", x.Entry.Name, "member", run.Member, "principal", subjectOf(r.Context()))
		native.WriteJSON(w, http.StatusAccepted, run)
	}
}

// failSkill answers an error of a slash command with its status.
func failSkill(k *native.Kit, w http.ResponseWriter, r *http.Request, err error) {
	var me *memberError
	switch {
	case errors.Is(err, skills.ErrUnknown):
		native.WriteProblem(w, r, http.StatusNotFound, skillDetail(err))
	case errors.Is(err, skills.ErrNotInvocable):
		native.WriteProblem(w, r, http.StatusBadRequest, skillDetail(err))
	case errors.Is(err, skills.ErrTooLarge), errors.Is(err, ErrPinnedBound):
		native.WriteProblem(w, r, http.StatusRequestEntityTooLarge, skillDetail(err))
	case errors.Is(err, skills.ErrUnavailable):
		native.WriteProblem(w, r, http.StatusBadGateway, skillDetail(err))
	case errors.As(err, &me):
		switch {
		case me.status == http.StatusUnauthorized:
			native.WriteProblem(w, r, http.StatusForbidden, fmt.Sprintf("the fleet member %s did not accept your credential; both members need the same issuer and audience, or the same token", me.member))
		case me.status >= 500:
			native.WriteProblem(w, r, http.StatusBadGateway, fmt.Sprintf("the fleet member %s answered %d: %s", me.member, me.status, me.detail))
		default:
			native.WriteProblem(w, r, me.status, me.detail)
		}
	case errors.Is(err, fleet.ErrNoRoute), errors.Is(err, fleet.ErrNoCredential):
		native.WriteProblem(w, r, http.StatusBadGateway, skillDetail(err))
	case errors.Is(err, fleet.ErrNoToken):
		fleet.Fail(w, r, err)
	default:
		k.Fail(w, r, err)
	}
}

// skillDetail is the words of an error without the names of the packages
// and the kinds of error in front.
func skillDetail(err error) string {
	s := err.Error()
	for _, prefix := range []string{skills.ErrUnknown.Error() + ": ", skills.ErrNotInvocable.Error() + ": ", skills.ErrTooLarge.Error() + ": ", skills.ErrUnavailable.Error() + ": ", ErrPinnedBound.Error() + ": ", "fw: invalid request: ", "bossmode: ", "fleet: "} {
		s = strings.TrimPrefix(s, prefix)
	}
	return s
}

// runTarget finds the run of a slash command for the caller in ctx: a run
// of this process that the caller can operate, as the kernel decides, or a
// run of another member that the view shows the caller. For such a run,
// the front decides the message of a person before it goes out (toOwner).
func runTarget(ctx context.Context, a *fw.Agent, id string) (*fw.Run, error) {
	// A context without a principal asks the kernel whether the run exists
	// at all.
	if _, err := a.Get(context.Background(), id); !errors.Is(err, fw.ErrRunNotFound) {
		run, err := a.Get(ctx, id)
		if err != nil {
			return nil, err
		}
		if err := a.Authorize(ctx, fw.ActionRunOperate, fw.Target{Agent: run.Agent, Run: run}); err != nil {
			return nil, err
		}
		return run, nil
	}
	v := fleet.Of(a)
	if v == nil {
		return nil, fmt.Errorf("bossmode: %w: %s", fw.ErrRunNotFound, id)
	}
	run, err := v.Run(ctx, id)
	if err != nil {
		return nil, err
	}
	if run.Member == "" {
		return nil, fmt.Errorf("bossmode: %w: %s", fw.ErrRunNotFound, id)
	}
	return run, nil
}

// toOwner returns the context of the message of a slash command for a run
// of another member (fleet.ForRun): for a person, the front decides
// fw.ActionRunOperate, and the message goes with the static token of the
// fleet and names the person, whom the owner records as the sender. The
// static token and a workload send their own credential. A run of this
// process keeps ctx, because the kernel decides there.
func toOwner(ctx context.Context, a *fw.Agent, run *fw.Run) (context.Context, error) {
	if run.Member == "" {
		return ctx, nil
	}
	return fleet.ForRun(ctx, a, run.ID, fw.ActionRunOperate)
}

// sendSkill sends the message of a slash command to the run of runTarget
// as the caller in ctx, pinned: a run of this process through the kernel,
// which authorizes the caller, and a run of another member through the
// forward of the fleet, with the context of toOwner. It returns the run
// after the message. A refusal to pin the message wraps ErrPinnedBound. A
// refusal of the static token by the owner is a 403 with words for an
// operator (fleet.RefusedToken).
func sendSkill(ctx context.Context, a *fw.Agent, run *fw.Run, content string) (*fw.Run, error) {
	id := run.ID
	msg := fw.Message{Role: fw.RoleUser, Content: []fw.Block{fw.Text(content)}}
	if run.Member == "" {
		if err := a.Send(ctx, id, msg, fw.OriginHTTP, fw.WithMessagePinned()); err != nil {
			if errors.Is(err, fw.ErrInvalidRequest) && pinnedRefusal(err.Error()) {
				return nil, fmt.Errorf("%w: %s", ErrPinnedBound, skillDetail(err))
			}
			return nil, err
		}
		return a.Get(ctx, id)
	}
	v := fleet.Of(a)
	if v == nil {
		return nil, fmt.Errorf("bossmode: %w: %s", fw.ErrRunNotFound, id)
	}
	b, err := json.Marshal(map[string]any{"content": content, "pinned": true})
	if err != nil {
		return nil, fmt.Errorf("bossmode: encode the message: %w", err)
	}
	fctx, cancel := context.WithTimeout(ctx, sendTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(fctx, http.MethodPost, "/v1/runs/"+url.PathEscape(id)+"/messages", bytes.NewReader(b))
	if err != nil {
		return nil, fmt.Errorf("bossmode: build the request for %s: %w", run.Member, err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	resp, err := v.Forward(fctx, run.Member, req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, fmt.Errorf("bossmode: read the answer of %s: %w", run.Member, err)
	}
	if resp.StatusCode >= http.StatusMultipleChoices {
		var p struct {
			Detail string `json:"detail"`
		}
		detail := strings.TrimSpace(string(body))
		if json.Unmarshal(body, &p) == nil && p.Detail != "" {
			detail = p.Detail
		}
		if resp.StatusCode == http.StatusBadRequest && pinnedRefusal(detail) {
			return nil, fmt.Errorf("%w: %s", ErrPinnedBound, detail)
		}
		if env := fleet.TokenEnvOf(fctx); env != "" && (resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden) {
			return nil, &memberError{member: run.Member, status: http.StatusForbidden, detail: fleet.RefusedToken(run.Member, env, resp.StatusCode, detail)}
		}
		if len(detail) > 500 {
			detail = detail[:500]
		}
		return nil, &memberError{member: run.Member, status: resp.StatusCode, detail: detail}
	}
	var out fw.Run
	if err := json.Unmarshal(body, &out); err != nil || out.ID == "" {
		out = *run
	}
	out.Member = run.Member
	return &out, nil
}

// subjectOf is the subject of the caller in ctx, or "".
func subjectOf(ctx context.Context) string {
	if p := fw.PrincipalFrom(ctx); p != nil {
		return p.Subject
	}
	return ""
}

// skillSendArgs are the arguments of skill_send.
type skillSendArgs struct {
	Name   string   `json:"name"`
	Args   string   `json:"args,omitempty"`
	Text   string   `json:"text,omitempty"`
	RunID  string   `json:"runId,omitempty"`
	Bridge string   `json:"bridge,omitempty"`
	To     []string `json:"to,omitempty"`
}

const skillSendSchema = `{"type":"object","properties":{
	"name":{"type":"string","minLength":1,"description":"The name of the skill, as skills_list names it."},
	"args":{"type":"string","description":"The arguments of the skill, as a person types them after the name."},
	"text":{"type":"string","description":"A message of yours that follows the skill."},
	"runId":{"type":"string","description":"The run that gets the skill, such as a conversation with the manager. Give runId or bridge."},
	"bridge":{"type":"string","description":"The id of the bridge whose members get the skill. Give runId or bridge."},
	"to":{"type":"array","items":{"type":"string"},"description":"With bridge: the handles that get the skill. Without it, every active member."}
},"required":["name"]}`

// skillSendTool is the tool skill_send.
func skillSendTool(a *fw.Agent) mcp.Tool {
	yes, no := true, false
	return mcp.NewTool(&sdk.Tool{
		Name:        ToolSkillSend,
		Title:       "Send a skill",
		Description: "Hand a skill to a running conversation as a slash command of yours: the front expands the skill with the arguments and sends it to the run, or to the members of a bridge, as your message, pinned, so it survives every compaction. It starts work. Only a person sends a skill; a run of the fleet cannot.",
		InputSchema: json.RawMessage(skillSendSchema),
		Annotations: &sdk.ToolAnnotations{DestructiveHint: &yes, OpenWorldHint: &no},
	}, func(ctx context.Context, req *sdk.CallToolRequest, in skillSendArgs) (*sdk.CallToolResult, any, error) {
		if _, ok := mcp.CallRun(ctx); ok {
			return mcp.ErrorResult("A run hands no skill to another run. Only a person sends a skill, through the page of bossmode, the routes, or this tool from a client of the person."), nil, nil
		}
		in.RunID, in.Bridge = strings.TrimSpace(in.RunID), strings.TrimSpace(in.Bridge)
		if (in.RunID == "") == (in.Bridge == "") {
			return mcp.ErrorResult("Invalid request: give runId or bridge, not both."), nil, nil
		}
		cmd := skills.Command{Name: in.Name, Args: in.Args}
		if in.Bridge != "" {
			s := bridges.Of(a)
			if s == nil {
				return mcp.ErrorResult("The bridges are off in this process."), nil, nil
			}
			m, err := s.Post(ctx, in.Bridge, bridges.PostSpec{Text: in.Text, To: in.To, Skill: &cmd})
			if err != nil {
				return mcp.ErrorResult(bridgeWords(err)), nil, nil
			}
			text := fmt.Sprintf("Posted the skill %s to the bridge %s as entry %d. Each member that the message reaches gets it as your message, pinned.", cmd.Name, in.Bridge, m.Seq)
			return mcp.Result(text, map[string]any{"bridge": in.Bridge, "message": m}), nil, nil
		}
		target, err := runTarget(ctx, a, in.RunID)
		if err != nil {
			return mcp.ErrorResult(sendWords(err)), nil, nil
		}
		fctx, err := toOwner(withCredential(ctx, req), a, target)
		if err != nil {
			return mcp.ErrorResult(sendWords(err)), nil, nil
		}
		x, err := skills.Prepare(a, cmd, in.Text)
		if err != nil {
			return mcp.ErrorResult(sendWords(err)), nil, nil
		}
		run, err := sendSkill(fctx, a, target, x.Message)
		if err != nil {
			return mcp.ErrorResult(sendWords(err)), nil, nil
		}
		a.Logger().Info("bossmode: a person sent a skill to a run", "run", run.ID, "skill", x.Entry.Name, "member", run.Member, "principal", subjectOf(ctx))
		text := fmt.Sprintf("Sent the skill %s to the run %s as your message, pinned, so it survives every compaction. The run is %s.", x.Entry.Name, run.ID, run.State)
		return mcp.Result(text, map[string]any{"run": run}), nil, nil
	})
}

// withCredential puts the bearer credential of the HTTP request of a call
// into ctx when ctx has none, for the forward to the owner of a run.
func withCredential(ctx context.Context, req *sdk.CallToolRequest) context.Context {
	if fw.CredentialFrom(ctx) != "" || req == nil {
		return ctx
	}
	ex := req.GetExtra()
	if ex == nil || ex.Header == nil {
		return ctx
	}
	h := strings.TrimSpace(ex.Header.Get("Authorization"))
	scheme, tok, ok := strings.Cut(h, " ")
	if !ok || !strings.EqualFold(scheme, "bearer") || strings.TrimSpace(tok) == "" {
		return ctx
	}
	return fw.WithCredential(ctx, h)
}

// sendWords words an error of skill_send for a run.
func sendWords(err error) string {
	var me *memberError
	switch {
	case errors.Is(err, skills.ErrUnknown):
		return "Not found: " + skillDetail(err) + "."
	case errors.Is(err, skills.ErrNotInvocable):
		return "Invalid request: " + skillDetail(err) + "."
	case errors.Is(err, skills.ErrTooLarge), errors.Is(err, ErrPinnedBound):
		return "Too large: " + skillDetail(err) + "."
	case errors.Is(err, skills.ErrUnavailable):
		return "Unavailable: " + skillDetail(err) + ". Try again in a few seconds."
	case errors.As(err, &me):
		return fmt.Sprintf("The fleet member %s answered %d: %s.", me.member, me.status, me.detail)
	case errors.Is(err, fw.ErrRunNotFound), errors.Is(err, fw.ErrForbidden), errors.Is(err, fw.ErrAuthzUnavailable), errors.Is(err, fw.ErrInvalidRequest), errors.Is(err, fleet.ErrNoToken):
		return mcp.Explain(err)
	}
	return "The skill did not reach the run: " + skillDetail(err) + "."
}

// bridgeWords words an error of skill_send for a bridge.
func bridgeWords(err error) string {
	switch {
	case errors.Is(err, bridges.ErrNotFound):
		return "Not found: " + bridges.Detail(err) + "."
	case errors.Is(err, bridges.ErrConflict):
		return "Conflict: " + bridges.Detail(err) + "."
	case errors.Is(err, bridges.ErrTooLarge):
		return "Too large: " + bridges.Detail(err) + "."
	case errors.Is(err, bridges.ErrUnavailable):
		return "Unavailable: " + bridges.Detail(err) + ". Try again in a few seconds."
	case errors.Is(err, fw.ErrInvalidRequest):
		return "Invalid request: " + bridges.Detail(err) + "."
	}
	return mcp.Explain(err)
}
