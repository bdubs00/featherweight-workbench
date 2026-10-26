package dashboard

// This file holds the members and the pools of the fleet as text
// (docs/ARCHITECTURE.md, the fleet). The runs of the whole fleet come from
// package view of the kernel.

import (
	"fmt"
	"strings"
	"time"

	"github.com/bdubs00/featherweight-workbench/fleet"
	"github.com/bdubs00/featherweight-workbench/pools"
)

// MembersText describes the members of a fleet, one line each. Name is the
// name of this process.
func MembersText(name string, members []fleet.Member) string {
	var b strings.Builder
	fmt.Fprintf(&b, "This process is the fleet member %s.", name)
	if len(members) == 0 {
		b.WriteString("\nYou can see no member.")
		return b.String()
	}
	fmt.Fprintf(&b, "\n%d members you can see:", len(members))
	for _, m := range members {
		b.WriteString("\n" + MemberLine(m))
	}
	return b.String()
}

// MemberLine is one member on one line: its state, when it was last heard,
// its authorization mode, the reason when a storage failure blocked it, its
// agents with the role of the caller, its runs, and whether this process
// can act on its runs.
func MemberLine(m fleet.Member) string {
	var b strings.Builder
	b.WriteString("- " + m.Name)
	if m.Self {
		b.WriteString(" (this process)")
	}
	b.WriteString(": " + m.State)
	if !m.Self && !m.LastSeen.IsZero() {
		b.WriteString(", heard " + ago(m.LastSeen) + " ago")
	}
	if m.AccessMode != "" {
		b.WriteString(", access " + m.AccessMode)
	}
	if m.BlockedReason != "" {
		b.WriteString("; blocked by a storage failure (" + m.BlockedReason + "), so the server suspends its Task, and the next request restarts it")
	}
	if len(m.Agents) == 0 {
		b.WriteString("; no agent you can see")
	} else {
		names := make([]string, len(m.Agents))
		for i, a := range m.Agents {
			names[i] = a.Name
			if a.Role != "" {
				names[i] += " (you are " + a.Role + ")"
			}
		}
		b.WriteString("; agents " + strings.Join(names, ", "))
	}
	fmt.Fprintf(&b, "; %s, %d waiting for input", count(m.Active, "active run", "active runs"), m.InputRequired)
	switch {
	case m.Self:
	case m.CanForward:
		b.WriteString("; this process can act on its runs")
	default:
		b.WriteString("; read-only from here, because it has no address that this process can reach")
	}
	return b.String()
}

// PoolsText describes the pools of the front: per pool, where it comes
// from, the Tasks that it wants, the Tasks that exist and are ready, its
// queue, and one line per reason for a gap.
func PoolsText(ps []pools.Status) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s of the server:", count(len(ps), "group of workers (pool)", "groups of workers (pools)"))
	for _, p := range ps {
		from := ""
		switch {
		case p.Source == pools.SourceRuntime && p.Deleting:
			from = fmt.Sprintf(" (a runtime pool of the template %s, being deleted)", p.Template)
		case p.Source == pools.SourceRuntime:
			from = fmt.Sprintf(" (a runtime pool of the template %s)", p.Template)
		case p.Template != "":
			from = fmt.Sprintf(" (of the file, from the template %s)", p.Template)
		}
		fmt.Fprintf(&b, "\n- %s%s in %s: %d of %d %s ready, %d %s; agent %s on the queue %s",
			p.Name, from, p.Atespace, p.Ready, p.Wanted, word(p.Wanted, "worker", "workers"), p.Actual, word(p.Actual, "exists", "exist"), p.Agent, p.Intake)
		if p.CheckedAt.IsZero() {
			b.WriteString("; not read yet")
		} else {
			b.WriteString("; checked " + ago(p.CheckedAt) + " ago")
		}
		for _, g := range p.Gaps {
			b.WriteString("\n  - " + g)
		}
	}
	return b.String()
}

// word picks the singular or the plural form for n.
func word(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// ago is how long ago a time was, in one unit and in words, such as "5
// minutes", so a model that reads a text repeats no unit of the code.
func ago(t time.Time) string {
	d := time.Since(t)
	n, unit := 0, ""
	switch {
	case d < time.Minute:
		n, unit = max(0, int(d.Seconds())), "second"
	case d < time.Hour:
		n, unit = int(d.Minutes()), "minute"
	case d < 48*time.Hour:
		n, unit = int(d.Hours()), "hour"
	default:
		n, unit = int(d.Hours()/24), "day"
	}
	if n != 1 {
		unit += "s"
	}
	return fmt.Sprintf("%d %s", n, unit)
}
