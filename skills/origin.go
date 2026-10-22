package skills

import "github.com/bdubs00/featherweight-kernel/fw"

// originOf is the origin of a skill as the kernel names it in GET
// /v1/skills (fw.Skill.OriginName): fw.SkillOriginMCP for a skill of an
// MCP server, and fw.SkillOriginLocal, which is OriginLocal, for a skill
// of the skills directories. The catalog then marks an imported skill
// with OriginImported.
func originOf(s fw.Skill) string { return s.OriginName() }

// skillURI is the URI that the kernel gives a skill, which a skill of an
// MCP server has, or "".
func skillURI(s fw.Skill) string { return s.URI }

// fromServer reports whether a skill comes from an MCP server. The kernel
// reads no file of such a skill ahead of need, and neither does the
// catalog.
func fromServer(s fw.Skill) bool { return s.Origin == fw.SkillOriginMCP }
