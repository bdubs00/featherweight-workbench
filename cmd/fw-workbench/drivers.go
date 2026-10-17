package main

// The blank imports select the drivers of the binary. Each driver registers
// itself in init. The binary carries every driver of the kernel binary fw,
// and the packages of the workbench on top.

import (
	"github.com/bdubs00/featherweight-kernel/cli"
	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/surface/a2a"
	"github.com/bdubs00/featherweight-kernel/telemetry"
	"github.com/bdubs00/featherweight-workbench/surface/mcp"

	_ "github.com/bdubs00/featherweight-kernel/authn/spiffe"
	_ "github.com/bdubs00/featherweight-kernel/bus/nats"
	_ "github.com/bdubs00/featherweight-kernel/bus/redis"
	_ "github.com/bdubs00/featherweight-kernel/config"
	_ "github.com/bdubs00/featherweight-kernel/model/anthropic"
	_ "github.com/bdubs00/featherweight-kernel/model/gemini"
	_ "github.com/bdubs00/featherweight-kernel/model/openai"
	_ "github.com/bdubs00/featherweight-kernel/surface/metrics"
	_ "github.com/bdubs00/featherweight-kernel/surface/native"
	_ "github.com/bdubs00/featherweight-kernel/tools/a2a"
	_ "github.com/bdubs00/featherweight-kernel/tools/builtin"
	_ "github.com/bdubs00/featherweight-kernel/tools/mcp"

	_ "github.com/bdubs00/featherweight-workbench/authn/oidc"
	_ "github.com/bdubs00/featherweight-workbench/fleet"
	_ "github.com/bdubs00/featherweight-workbench/surface/web"
	_ "github.com/bdubs00/featherweight-workbench/tools/builder"

	// A sandbox image has no zone files, and ax sets no TZ, so the binary
	// carries the zone database (about 450 KB).
	_ "time/tzdata"
)

func init() {
	// The workbench names each run with a title that a model writes. The
	// kernel takes the first sentence of the input, with no extra call.
	fw.RegisterDefaults(func(c *fw.Config) { c.Titles = fw.TitlesModel })
	cli.NewLogger = telemetry.Logger
	cli.SetBuildInfo = telemetry.SetBuildInfo
	cli.ServeMCPStdio = mcp.ServeStdio
	cli.CardJSON = a2a.CardJSON
}
