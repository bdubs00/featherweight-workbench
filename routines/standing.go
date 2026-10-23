package routines

// This file holds the standings of a routine and the moves between them
// (docs/ARCHITECTURE.md section 21.2). Only a rehearsing and a live
// routine reach the profile of the runs of its agent.

import (
	"slices"
	"strings"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// Standings of a routine in the workbench.
const (
	Proposed   = "proposed"
	Rehearsing = "rehearsing"
	Live       = "live"
	Paused     = "paused"
	Retired    = "retired"
)

// Standings of a routine in a profile of the kernel. The kernel runs a
// rehearsing routine beside the agent, without acting, as its standing
// shadow.
const (
	kernelShadow = fw.RoutineShadow
	kernelLive   = fw.RoutineLive
)

// moves holds the standings that each standing can move to.
var moves = map[string][]string{
	Proposed:   {Rehearsing, Retired},
	Rehearsing: {Live, Paused, Retired},
	Live:       {Paused, Retired},
	Paused:     {Rehearsing, Live},
	Retired:    {},
}

// ValidStanding reports whether s is a standing of the workbench.
func ValidStanding(s string) bool {
	_, ok := moves[s]
	return ok
}

// Moves returns the standings that a routine in the standing from can move
// to, in the order of the table. A retired routine moves nowhere.
func Moves(from string) []string { return slices.Clone(moves[from]) }

// CanMove reports whether the table allows a move from one standing to
// another.
func CanMove(from, to string) bool { return slices.Contains(moves[from], to) }

// movesText words the moves of a standing for an error: "rehearsing or
// retired", or "none" for a retired routine.
func movesText(from string) string {
	list := moves[from]
	switch len(list) {
	case 0:
		return "none, because a retired routine stays retired"
	case 1:
		return list[0]
	}
	return strings.Join(list[:len(list)-1], ", ") + " or " + list[len(list)-1]
}

// kernelStanding returns the standing of the kernel that a routine in the
// standing s carries in a profile, and false for a standing that reaches
// no profile.
func kernelStanding(s string) (string, bool) {
	switch s {
	case Rehearsing:
		return kernelShadow, true
	case Live:
		return kernelLive, true
	}
	return "", false
}
