package main

// This file is the verb pages of fw-workbench, which the kernel does not
// know (docs/ARCHITECTURE.md section 15.5). pages export writes the wiki
// of the fleet as files, one per page, and pages import writes files back
// as new versions of their pages. Both reach the wiki in the key-value
// bucket of the NATS bus, so they need the bus nats.

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/user"
	"strings"
	"time"

	"github.com/bdubs00/featherweight-kernel/config"
	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/pages"
)

const pagesUsage = `Usage: fw-workbench pages export [-config file] <dir>
       fw-workbench pages import [-config file] <dir>

export writes each page of the wiki of the fleet as one file under dir, at
the path of the page, and the index as _index.md. It replaces a file of the
same path and leaves every other file as it is.

import writes each file under dir whose name ends with .md as a new version
of the page at its path, with no condition, and names every file that it
leaves out. A file that holds the text of its page already changes nothing.
The writer of each version is local:<user>.

Both verbs reach the wiki in the key-value bucket <stream>_PAGES of the NATS
bus of the configuration, so they need bus.driver nats. The file is the
configuration of the front: -config, else $FW_CONFIG, ./fw.yaml, or
$FW_HOME/fw.yaml.
`

// pagesTimeout bounds one run of the verb.
const pagesTimeout = 5 * time.Minute

// runPages runs the verb pages and returns the exit code.
func runPages(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || (args[0] != "export" && args[0] != "import") {
		fmt.Fprint(stderr, pagesUsage)
		if len(args) > 0 && (args[0] == "help" || args[0] == "-h" || args[0] == "--help") {
			return 0
		}
		return 2
	}
	verb := args[0]
	fs := flag.NewFlagSet("fw-workbench pages "+verb, flag.ContinueOnError)
	fs.SetOutput(stderr)
	file := fs.String("config", "", "the configuration `file` of the front")
	fs.Usage = func() { fmt.Fprint(stderr, pagesUsage) }
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 1 || strings.TrimSpace(fs.Arg(0)) == "" {
		fs.Usage()
		return 2
	}
	dir := fs.Arg(0)
	path := *file
	if path == "" {
		path = config.Find()
	}
	if path == "" {
		fmt.Fprintln(stderr, "fw-workbench: no configuration file; name one with -config, or set FW_CONFIG")
		return 1
	}
	cfg, err := config.Load(path)
	if err != nil {
		fmt.Fprintf(stderr, "fw-workbench: %v\n", err)
		return 1
	}
	if msg := pagesBus(cfg); msg != "" {
		fmt.Fprintln(stderr, "fw-workbench: "+msg)
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), pagesTimeout)
	defer cancel()
	store, closeStore, err := pages.OpenOnce(ctx, cfg, slog.New(slog.DiscardHandler))
	if err != nil {
		fmt.Fprintf(stderr, "fw-workbench: %v\n", err)
		return 1
	}
	if closeStore != nil {
		defer closeStore()
	}
	if verb == "export" {
		out, err := pages.Export(ctx, store, dir)
		if err != nil {
			fmt.Fprintf(stderr, "fw-workbench: %v\n", err)
			return 1
		}
		index := ""
		if out.Index {
			index = " and the index " + pages.IndexPath
		}
		fmt.Fprintf(stdout, "Wrote %s%s from %s to %s.\n", count(out.Pages, "page", "pages"), index, store.Describe(), dir)
		return 0
	}
	out, err := pages.Import(ctx, store, dir, localWriter())
	for _, s := range out.Skipped {
		fmt.Fprintln(stderr, "fw-workbench: left out "+s)
	}
	if err != nil {
		fmt.Fprintf(stderr, "fw-workbench: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "Wrote %s from %s to %s; %s held the text already, and %s left out.\n",
		count(out.Written, "page", "pages"), dir, store.Describe(), count(out.Unchanged, "page", "pages"), count(len(out.Skipped), "file is", "files are"))
	return 0
}

// pagesBus says why the bus of cfg does not reach the wiki of the front,
// or "" when it does.
func pagesBus(cfg *fw.Config) string {
	switch d := strings.TrimSpace(cfg.Bus.Driver); d {
	case "nats":
		return ""
	case "", "mem":
		return "the bus of the configuration is mem, so the wiki lives in the memory of the front process and this verb cannot reach it; set bus.driver to nats"
	default:
		return "the bus of the configuration is " + d + ", and the wiki needs the bus nats; set bus.driver to nats"
	}
}

// count says how many of a thing there are: "1 page", "2 pages".
func count(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

// localWriter names the person who runs the verb as the kernel names the
// local principal: local:<user>.
func localWriter() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		return "local:" + u.Username
	}
	if n := os.Getenv("USER"); n != "" {
		return "local:" + n
	}
	return "local:user"
}
