package dashboard

import (
	"context"
	"encoding/json"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/surface/view"
)

// pngBytes start like a PNG file, so the sniffer names the type.
var pngBytes = []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x00\x00\x01\x00\x00\x00\x01\x08\x06\x00\x00\x00")

// writeRun runs an agent whose one tool call writes files through the
// workspace, and returns the agent, the run id, and the workspace root.
func writeRun(t *testing.T, files map[string][]byte) (*fw.Agent, string, string) {
	t.Helper()
	root := t.TempDir()
	cfg := fw.DefaultConfig()
	cfg.Home = filepath.Join(root, ".fw")
	cfg.Tools.Files.Root = root
	writer := fw.NewTool(fw.ToolSpec{Name: "writer"}, func(ctx context.Context, _ json.RawMessage) (fw.ToolResult, error) {
		for name, data := range files {
			if err := fw.FilesFrom(ctx).WriteFile(name, data); err != nil {
				return fw.ErrorResult(err.Error()), nil
			}
		}
		return fw.TextResult("written"), nil
	})
	var turn atomic.Int32
	model := fw.ModelFunc{GenerateFn: func(context.Context, *fw.Request) (*fw.Response, error) {
		if turn.Add(1) == 1 {
			return &fw.Response{Content: []fw.Block{{Type: fw.BlockToolUse, ID: "c1", Name: "writer", Input: json.RawMessage(`{}`)}}, StopReason: fw.StopToolUse}, nil
		}
		return &fw.Response{Content: []fw.Block{fw.Text("done")}, StopReason: fw.StopEndTurn}, nil
	}}
	a := newAgent(t, fw.WithConfig(cfg), fw.WithModel(model), fw.WithTools(writer))
	r, err := a.Run(context.Background(), "write")
	if err != nil || r.State != fw.StateCompleted {
		t.Fatalf("run = %+v, %v", r, err)
	}
	return a, r.ID, root
}

func TestTranscriptFileItems(t *testing.T) {
	a, id, _ := writeRun(t, map[string][]byte{"a.txt": []byte("one")})
	events, _, err := view.EventsPage(context.Background(), a, id, 1, 100)
	if err != nil {
		t.Fatal(err)
	}
	var file *Item
	items := Transcript(events)
	for i := range items {
		if items[i].Kind == ItemFile {
			file = &items[i]
		}
	}
	if file == nil || file.Path != "a.txt" || file.Text != fw.FileCreated || file.CallID != "c1" || file.Tool != "writer" || file.Size != 3 {
		t.Fatalf("file item = %+v in %+v", file, items)
	}
}
