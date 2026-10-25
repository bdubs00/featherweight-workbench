// Package workers serves the worker templates and the pools of a front
// (docs/ARCHITECTURE.md section 14.10), so that an operator gets workers
// for an agent without an edit of fw.yaml and without a restart of the
// front. It mounts the routes /v1/pools and /v1/templates on the native
// surface (native.Extend) and adds the MCP tools pools_list, pool_put,
// pool_delete, and templates_list (mcp.RegisterTools). With a package
// mirror, it also serves the packages of a pool (section 14.11) with the
// routes /v1/pools/{name}/packages and the tools packages_get and
// packages_put. The package fleet keeps the pools, runs them, and decides
// who can change them. This package only carries the requests to it.
package workers

import (
	"errors"
	"fmt"
	"strings"

	"github.com/bdubs00/featherweight-kernel/surface/native"
	"github.com/bdubs00/featherweight-workbench/dashboard"
	"github.com/bdubs00/featherweight-workbench/fleet"
	"github.com/bdubs00/featherweight-workbench/pools"
	"github.com/bdubs00/featherweight-workbench/surface/mcp"
)

func init() {
	native.Extend(mount)
	mcp.RegisterTools(tools)
}

// poolText says in words what a pool is and why it is not as its entry
// wants it.
func poolText(st pools.Status) string {
	return strings.TrimPrefix(dashboard.PoolsText([]pools.Status{st}), "1 pool of the front:\n- ")
}

// templatesText lists the templates in words.
func templatesText(list []pools.TemplateInfo) string {
	if len(list) == 0 {
		return "The configuration names no worker template, so no runtime pool can be made here."
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d worker %s:", len(list), plural(len(list), "template", "templates"))
	for _, t := range list {
		fmt.Fprintf(&b, "\n- %s in the atespace %s", t.Name, t.Atespace)
		if t.Default {
			b.WriteString(", the default for a new agent")
		}
		if t.Model != "" {
			b.WriteString(", with the model " + t.Model)
		} else {
			b.WriteString(", with no model: the profile of an agent must name one")
		}
	}
	return b.String()
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// cause returns the words of an error of the fleet without the sentinel
// in front.
func cause(err error) string {
	msg := err.Error()
	for _, e := range []error{fleet.ErrPoolNotFound, fleet.ErrPoolExists, fleet.ErrPoolConflict, fleet.ErrPackagesOff} {
		if errors.Is(err, e) {
			msg = strings.TrimPrefix(msg, e.Error()+": ")
		}
	}
	return msg
}
