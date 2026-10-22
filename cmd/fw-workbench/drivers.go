package main

// The blank imports select the drivers of the binary. Each driver registers
// itself in init. The binary carries every driver of the kernel binary fw,
// and the packages of the workbench on top.

import (
	"context"

	"github.com/bdubs00/featherweight-kernel/cli"
	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/surface/a2a"
	"github.com/bdubs00/featherweight-kernel/telemetry"
	"github.com/bdubs00/featherweight-workbench/access"
	"github.com/bdubs00/featherweight-workbench/agents"
	"github.com/bdubs00/featherweight-workbench/connections"
	"github.com/bdubs00/featherweight-workbench/fleet"
	"github.com/bdubs00/featherweight-workbench/manager"
	"github.com/bdubs00/featherweight-workbench/sources"
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
	_ "github.com/bdubs00/featherweight-kernel/tokens"
	_ "github.com/bdubs00/featherweight-kernel/tools/a2a"
	_ "github.com/bdubs00/featherweight-kernel/tools/builtin"
	_ "github.com/bdubs00/featherweight-kernel/tools/mcp"

	_ "github.com/bdubs00/featherweight-kernel/authn/oidc"
	_ "github.com/bdubs00/featherweight-workbench/faces"
	_ "github.com/bdubs00/featherweight-workbench/pages"
	_ "github.com/bdubs00/featherweight-workbench/surface/web"
	_ "github.com/bdubs00/featherweight-workbench/titles"
	_ "github.com/bdubs00/featherweight-workbench/tools/builder"

	// A sandbox image has no zone files, and ax sets no TZ, so the binary
	// carries the zone database (about 450 KB).
	_ "time/tzdata"
)

func init() {
	// The kernel writes no agent profile; the verbs put and delete of
	// fw-workbench agents do, through the package agents.
	cli.AgentsPut = func(ctx context.Context, a *fw.Agent, p fw.AgentProfile) error {
		_, _, err := agents.Put(ctx, a, p)
		return err
	}
	cli.AgentsDelete = agents.Delete
	cli.RegisterDoctorCheck(connections.DoctorCheck)
	cli.RegisterDoctorCheck(fleet.DoctorCheck)
	cli.RegisterDoctorCheck(sources.DoctorCheck)
	cli.RegisterDoctorCheck(manager.DoctorCheck)
	// With workbench.access.external, a control plane decides who can do
	// what; every agent of the binary gets the plug, and the verbs know it.
	// The guard of the manager wraps the plug, or the rules of access
	// without one, so a person that the static token acts for never gets
	// more than the token (docs/ARCHITECTURE.md section 19).
	cli.AgentOptions = func() []fw.Option { return []fw.Option{fw.WithAuthzFrom(manager.Guard(access.Build))} }
	cli.ExternalAccess = access.Of
	cli.NewLogger = telemetry.Logger
	cli.SetBuildInfo = telemetry.SetBuildInfo
	cli.ServeMCPStdio = mcp.ServeStdio
	cli.CardJSON = a2a.CardJSON
}
