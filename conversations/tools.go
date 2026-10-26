package conversations

// This file holds the MCP tool conversation_patch (docs/ARCHITECTURE.md
// section 4.20), which calls Store.Patch as PATCH
// /v1/conversations/{run} does. For a call of a run of the fleet that
// names its person, the tool acts as that person (manager.OnBehalf).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/bdubs00/featherweight-workbench/manager"
	"github.com/bdubs00/featherweight-workbench/surface/mcp"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// ToolPatch is the name of the tool.
const ToolPatch = "conversation_patch"

const patchSchema = `{"type":"object","properties":{
	"run":{"type":"string","minLength":1,"description":"The id of the run of the conversation."},
	"title":{"type":"string","maxLength":200,"description":"Your own title of the conversation, one line of at most 100 characters. An empty title gives the conversation its own title back."},
	"hidden":{"type":"boolean","description":"true hides the conversation from your lists, and false shows it again. The run and its archive stay."}
},"required":["run"]}`

type patchArgs struct {
	Run string `json:"run"`
	Patch
}

func ptr[T any](v T) *T { return &v }

// tools builds the tool for one MCP server.
func tools(srv *mcp.Server) []mcp.Tool {
	a := srv.Agent()
	return []mcp.Tool{
		mcp.NewTool(&sdk.Tool{
			Name:        ToolPatch,
			Title:       "Rename or hide a conversation",
			Description: "For a person: give a conversation a title of your own, or hide it from your lists, as Rename and Delete of the page do. Only your own lists change: the run, its archive, and the lists of other people stay as they are. A hidden conversation stays readable by its id.",
			InputSchema: json.RawMessage(patchSchema),
			Annotations: &sdk.ToolAnnotations{DestructiveHint: ptr(false), IdempotentHint: true, OpenWorldHint: ptr(false)},
		}, func(ctx context.Context, _ *sdk.CallToolRequest, in patchArgs) (*sdk.CallToolResult, any, error) {
			ctx, _, _ = manager.OnBehalf(ctx)
			ans, err := Of(a).Patch(ctx, in.Run, in.Patch)
			switch {
			case errors.Is(err, ErrOff), errors.Is(err, ErrNoPerson):
				r := reason(err)
				return mcp.ErrorResult(strings.ToUpper(r[:1]) + r[1:] + "."), nil, nil
			case err != nil:
				return mcp.ErrorResult(mcp.Explain(err)), nil, nil
			}
			text := fmt.Sprintf("The conversation %s keeps its own title in your lists", ans.Run)
			if ans.MyTitle != "" {
				text = fmt.Sprintf("The conversation %s is %q in your lists", ans.Run, ans.MyTitle)
			}
			if ans.Hidden {
				text += ", and it is hidden from them."
			} else {
				text += "."
			}
			return mcp.Result(text, ans), nil, nil
		}),
	}
}
