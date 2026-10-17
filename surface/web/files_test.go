package web

import (
	"context"
	"encoding/json"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// writerTool writes each entry of its input, a map of path to text,
// through the workspace of the call.
var writerTool = fw.NewTool(fw.ToolSpec{Name: "writer"}, func(ctx context.Context, in json.RawMessage) (fw.ToolResult, error) {
	var files map[string]string
	_ = json.Unmarshal(in, &files)
	for name, text := range files {
		if err := fw.FilesFrom(ctx).WriteFile(name, []byte(text)); err != nil {
			return fw.ErrorResult(err.Error()), nil
		}
	}
	return fw.TextResult("written"), nil
})
