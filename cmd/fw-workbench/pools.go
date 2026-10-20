package main

// This file is the verb pools of fw-workbench, which the kernel does not
// know (docs/ARCHITECTURE.md section 13). pools render prints the
// Workspaces and the Tasks that the front applies for the pools of a
// configuration, with a placeholder for every secret, so a person reviews
// a change before the front acts on it.

import (
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/bdubs00/featherweight-kernel/config"
	"github.com/bdubs00/featherweight-workbench/fleet"
	"github.com/bdubs00/featherweight-workbench/pools"
)

const poolsUsage = `Usage: fw-workbench pools render [-config file] [file]

render prints the Workspaces and the Tasks that the front applies for the
pools under workbench.pools, as YAML documents in the order of the apply.
Every value that the front copies from its own environment, such as
FW_API_TOKEN, is a placeholder. The file is the configuration of the front:
the argument, else -config, else $FW_CONFIG, ./fw.yaml, or $FW_HOME/fw.yaml.
`

// runPools runs the verb pools and returns the exit code.
func runPools(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "render" {
		fmt.Fprint(stderr, poolsUsage)
		if len(args) > 0 && (args[0] == "help" || args[0] == "-h" || args[0] == "--help") {
			return 0
		}
		return 2
	}
	fs := flag.NewFlagSet("fw-workbench pools render", flag.ContinueOnError)
	fs.SetOutput(stderr)
	file := fs.String("config", "", "the configuration `file` of the front")
	fs.Usage = func() { fmt.Fprint(stderr, poolsUsage) }
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	path := *file
	switch fs.NArg() {
	case 0:
	case 1:
		path = fs.Arg(0)
	default:
		fs.Usage()
		return 2
	}
	if path == "" {
		path = config.Find()
	}
	if path == "" {
		fmt.Fprintln(stderr, "fw-workbench: no configuration file; name one, or set FW_CONFIG")
		return 1
	}
	cfg, err := config.Load(path)
	if err != nil {
		fmt.Fprintf(stderr, "fw-workbench: %v\n", err)
		return 1
	}
	fc, err := fleet.Load(cfg)
	if err != nil {
		fmt.Fprintf(stderr, "fw-workbench: %v\n", err)
		return 1
	}
	list, err := fleet.LoadPools(cfg, fc)
	if err != nil {
		fmt.Fprintf(stderr, "fw-workbench: %v\n", err)
		return 1
	}
	if err := pools.Write(stdout, list); err != nil {
		fmt.Fprintf(stderr, "fw-workbench: %v\n", err)
		return 1
	}
	return 0
}
