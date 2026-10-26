package faces

// This file holds the MCP tool faces_put (docs/ARCHITECTURE.md section
// 16.4), which the package adds to the MCP server of the workbench through
// mcp.RegisterTools. The manager calls it to give an agent a face that it
// drew. Whether the call waits for a person is the policy of the profile
// that calls it, such as ask for faces_put in the profile of the manager,
// so the operator sees the face on the approval card before it lands.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/surface/mcp"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// ToolPut is the name of the tool that sets a face.
const ToolPut = "faces_put"

const putSchema = `{"type":"object","properties":{
	"agent":{"type":"string","minLength":1,"maxLength":64,"description":"The name of the agent profile whose face this is, such as reviewer."},
	"svg":{"type":"string","minLength":1,"maxLength":32768,"description":"The face as the text of one SVG document of at most 32 KB, with the root element svg in the namespace http://www.w3.org/2000/svg. It can use the elements svg, g, path, circle, ellipse, rect, line, polyline, polygon, text, tspan, defs, linearGradient, radialGradient, stop, clipPath, mask, title, desc, and style, with the presentation and geometry attributes. It cannot use script, foreignObject, use, image, a, an event attribute such as onload, an href, url( other than url(#id) in fill, stroke, clip-path, or mask, a DOCTYPE, an entity, or a processing instruction."}
},"required":["agent","svg"]}`

type putArgs struct {
	Agent string `json:"agent"`
	SVG   string `json:"svg"`
}

// putResult is the structured result of faces_put: the face without the
// SVG, and the SVG that the store keeps.
type putResult struct {
	Face Face   `json:"face"`
	SVG  string `json:"svg"`
}

// tools builds the tool of the faces for one MCP server. A process without
// faces gets none.
func tools(s *mcp.Server) []mcp.Tool {
	a := s.Agent()
	if Of(a) == nil {
		return nil
	}
	return []mcp.Tool{mcp.NewTool(&sdk.Tool{
		Name:        ToolPut,
		Title:       "Set face",
		Description: "Set the face of an agent: an SVG that the operator view shows beside the agent. The server keeps only an SVG that passes its sanitizer, and it refuses any other SVG with the reason, so fix the SVG and call again. A face replaces the face of the agent; the server keeps the last ten.",
		InputSchema: json.RawMessage(putSchema),
		Annotations: &sdk.ToolAnnotations{DestructiveHint: ptr(false), IdempotentHint: true, OpenWorldHint: ptr(false)},
	}, func(ctx context.Context, _ *sdk.CallToolRequest, in putArgs) (*sdk.CallToolResult, any, error) {
		return put(ctx, a, in), nil, nil
	})}
}

func ptr[T any](v T) *T { return &v }

// put sets the face of an agent for the caller in ctx.
func put(ctx context.Context, a *fw.Agent, in putArgs) *sdk.CallToolResult {
	s := Of(a)
	if s == nil {
		return mcp.ErrorResult("The faces are off in this process. They run in a process of fw-workbench serve, with the bus nats or mem.")
	}
	if err := Check(ctx, a, in.Agent, fw.ActionAgentEdit); err != nil {
		return mcp.ErrorResult(mcp.Explain(err))
	}
	by := writer(ctx)
	f, err := s.Put(ctx, in.Agent, []byte(in.SVG), by)
	var ref *Refusal
	switch {
	case errors.As(err, &ref):
		return mcp.ErrorResult("Refused: " + ref.Reason + ". Nothing changed. Fix the SVG and call faces_put again.")
	case errors.Is(err, ErrInvalid):
		return mcp.ErrorResult("Invalid request: " + strings.TrimPrefix(err.Error(), ErrInvalid.Error()+": ") + ".")
	case errors.Is(err, ErrConflict):
		return mcp.ErrorResult("Conflict: " + strings.TrimPrefix(err.Error(), ErrConflict.Error()+": ") + ".")
	case err != nil:
		return mcp.ErrorResult(mcp.Explain(err))
	}
	a.Logger().Info("faces: face set", "agent", in.Agent, "by", by, "sha256", f.Hash, "size", len(f.SVG))
	text := fmt.Sprintf("Set the face of %s (hash %s). GET /v1/faces/%s serves it.", in.Agent, f.Hash, in.Agent)
	return mcp.Result(text, putResult{Face: f, SVG: string(f.SVG)})
}
