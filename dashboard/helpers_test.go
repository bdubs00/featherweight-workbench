package dashboard

import (
	"context"
	"encoding/json"

	"github.com/bdubs00/featherweight-kernel/fw"
)

var testTitles = map[string]string{"bash": "Run shell commands", "files": "Read and change files in the workspace", "http": "Fetch web addresses on its allowlist"}

func abilityTool(name, source, desc string, destructive bool) fw.Tool {
	spec := fw.ToolSpec{Name: name, Source: source, Description: desc, Destructive: destructive, Title: testTitles[name]}
	return fw.NewTool(spec, func(context.Context, json.RawMessage) (fw.ToolResult, error) { return fw.TextResult("ok"), nil })
}

type lockedStore struct{}

func (lockedStore) Put(context.Context, fw.AgentProfile) error { return nil }

func (lockedStore) Delete(context.Context, string) error { return nil }

func (lockedStore) Locked(string) bool { return true }
