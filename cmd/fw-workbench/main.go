// Command fw-workbench is the fw kernel with the workbench for people: the
// page at /ui, the browser sign-in, the MCP server, the fleet, and the
// agent builder. It takes every verb of fw, and the verb mcp serves the MCP
// server over standard input and output.
package main

import (
	"context"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/bdubs00/featherweight-kernel/cli"
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
	cli.Version, cli.Commit = version, commit
	args := os.Args[1:]
	if filepath.Base(os.Args[0]) == runnerName {
		args = append([]string{"serve"}, args...)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := cli.Run(ctx, args, os.Stdin, os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}
