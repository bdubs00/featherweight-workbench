package bossmode

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/bridges"
	"github.com/bdubs00/featherweight-workbench/faces"
	"github.com/bdubs00/featherweight-workbench/fleet"
	"github.com/bdubs00/featherweight-workbench/pages"
)

// DoctorCheck is the line of fw doctor about bossmode (cli.DoctorCheck):
// the page, the manager, and the stores, in one line. It says off, or on
// with the page that it serves, whether the directory of the page holds
// index.html, the manager profile and where it is defined, and the open
// streams. Then, on or off, it gives the lines of the stores that bossmode
// reads: the wiki (pages.Status), the faces (faces.Status), and the
// bridges (bridges.Status). A directory without index.html, a manager
// that no profile of fw.agents and no pool defines, and a store whose
// configuration does not load fail the check. cmd/fw-workbench registers
// it. The line manager of the package manager says more about the pool of
// the manager.
func DoctorCheck(_ context.Context, cfg *fw.Config, _ fw.Secrets, _ time.Duration) (bool, string) {
	ok, line := pageCheck(cfg)
	for _, status := range []func(*fw.Config) (bool, string){pages.Status, faces.Status, bridges.Status} {
		good, text := status(cfg)
		ok = ok && good
		line += "; " + text
	}
	return ok, line
}

// pageCheck is the part of the doctor line about the page, the manager,
// and the streams.
func pageCheck(cfg *fw.Config) (bool, string) {
	c, err := Load(cfg)
	if err != nil {
		return false, err.Error()
	}
	if !c.Enabled {
		return true, "bossmode: off; set workbench.bossmode.enabled (" + EnvEnabled + ") to serve the page at /bossmode and its stream"
	}
	ok := true
	var parts []string
	switch {
	case c.Dir == "" && embeddedPlaceholder():
		parts = append(parts, "serves the placeholder page that the binary embeds; set workbench.bossmode.dir ("+EnvDir+") to the dist directory of the page, or build the binary with make page")
	case c.Dir == "":
		parts = append(parts, "serves the page that the binary embeds")
	default:
		if err := checkDir(c.Dir); err != nil {
			return false, err.Error()
		}
		if hasIndex(c.Dir) {
			parts = append(parts, "serves the page from "+c.Dir+", which holds index.html")
		} else {
			ok = false
			parts = append(parts, "serves the page from "+c.Dir+", which holds no index.html, so /bossmode answers 503; build the page there")
		}
	}
	if line, found := managerLine(cfg, c.Manager); found {
		parts = append(parts, line)
	} else {
		ok = false
		parts = append(parts, line)
	}
	parts = append(parts, streamsLine())
	return ok, "bossmode: " + strings.Join(parts, "; ")
}

// managerLine says where the profile of the manager is defined: in
// fw.agents of this process, or as the agent of a pool. found is false
// for a manager that neither defines.
func managerLine(cfg *fw.Config, manager string) (line string, found bool) {
	if manager == "" {
		return "names no manager (workbench.bossmode.manager)", true
	}
	var where []string
	if _, ok := cfg.Agents[manager]; ok {
		where = append(where, "a profile of fw.agents")
	}
	if fc, err := fleet.Load(cfg); err == nil {
		if list, err := fleet.LoadPools(cfg, fc); err == nil {
			for _, p := range list {
				if p.Agent == manager {
					where = append(where, "the agent of the pool "+p.Name)
				}
			}
		}
	}
	if len(where) == 0 {
		return fmt.Sprintf("the manager %s is no profile of fw.agents and the agent of no pool, so the page has no manager", manager), false
	}
	return "the manager " + manager + " is " + strings.Join(where, " and "), true
}

// streamsLine counts the open streams of the hubs of this process. The
// doctor runs in a process of its own, which serves no stream, so it
// names the gauge of the server.
func streamsLine() string {
	n, hubsSeen := 0, 0
	hubs.Range(func(_, v any) bool {
		hubsSeen++
		n += v.(*Hub).Streams()
		return true
	})
	if hubsSeen == 0 {
		return fmt.Sprintf("serves at most %d streams; the server counts its open streams in fw_bossmode_streams_open", MaxStreams)
	}
	return fmt.Sprintf("%d of at most %d streams open", n, MaxStreams)
}
