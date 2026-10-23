// Command fw-workbench is the fw kernel with the workbench for people: the
// page at /ui, the browser sign-in, the MCP server, the fleet, and the
// agent builder. It takes every verb of fw, the verb mcp serves the MCP
// server over standard input and output, the verb pools render prints
// the Tasks that the front applies for its pools, and the verbs pages
// export and pages import copy the wiki of the fleet to files and back.
package main

import (
	"context"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/bdubs00/featherweight-kernel/cli"
	"github.com/bdubs00/featherweight-workbench/internal/memlimit"
)

// Build information, set with -ldflags "-X main.version=... -X main.commit=...".
var (
	version = "dev"
	commit  = ""
)

// runnerName is the name under which ax starts its runner. The container
// image links it to fw-workbench, and fw-workbench then serves.
const runnerName = "ax-task-runner"

func main() {
	// A front in a container gets the memory limit of its container as the
	// soft limit of the runtime, so a large upload does not pass it with
	// garbage (internal/memlimit).
	memlimit.FromCgroup()
	cli.Version, cli.Commit = version, commit
	args := os.Args[1:]
	if filepath.Base(os.Args[0]) == runnerName {
		args = append([]string{"serve"}, args...)
	}
	// The verbs pools and pages belong to the workbench, so they run before
	// the verbs of the kernel.
	if len(args) > 0 && args[0] == "pools" {
		os.Exit(runPools(args[1:], os.Stdout, os.Stderr))
	}
	if len(args) > 0 && args[0] == "pages" {
		os.Exit(runPages(args[1:], os.Stdout, os.Stderr))
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := cli.Run(ctx, args, os.Stdin, os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}
