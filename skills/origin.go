package skills

import "github.com/bdubs00/featherweight-kernel/fw"

// originOf is the origin of a skill of the kernel: the name of the
// tools.mcp entry of a server skill, or OriginLocal.
func originOf(fw.Skill) string { return OriginLocal }

// skillURI is the URI that the kernel gives a skill, or "".
func skillURI(fw.Skill) string { return "" }
