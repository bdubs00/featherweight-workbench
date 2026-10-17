// Package teams groups people and agents into teams, so that the workbench
// shows the work of a person and of the person's teams first. A team groups
// the view and grants no access. The access blocks of the kernel still
// decide who can do what. See docs/ARCHITECTURE.md section 8.
//
// The teams of a person are the groups of the principal that match the
// patterns of the configuration. The teams of an agent are the groups that
// its access block names as group:<name>. A run keeps the teams of the
// person who started it in its metadata, under MetaKey, which a run
// annotator of the kernel writes when the run is queued. A child run keeps
// the teams of its parent.
package teams

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/bdubs00/featherweight-kernel/fw"
)

const (
	// Block is the top-level key of fw.yaml that the workbench claims.
	Block = "workbench"
	// MetaKey is the metadata key of a run that holds the teams of the
	// person who started it, with commas between the names.
	MetaKey = "workbench.teams"
	// EnvTeams is the variable that sets the patterns, with commas between
	// them. It wins over the file.
	EnvTeams = "FW_WORKBENCH_TEAMS"
)

// Config is the part of the block that this package reads.
type Config struct {
	// Teams lists glob patterns over group names. A group that matches one
	// is a team. An empty list makes every group a team.
	Teams []string `yaml:"teams"`
}

func init() {
	fw.RegisterExtension(Block)
	fw.RegisterRunAnnotator(Annotate)
}

// Patterns returns the patterns of the configuration: EnvTeams when it is
// set, else the block of the file, else none, which makes every group a
// team. A block that does not fit the type gives none.
func Patterns(cfg *fw.Config) []string {
	if raw, ok := os.LookupEnv(EnvTeams); ok && strings.TrimSpace(raw) != "" {
		return split(raw)
	}
	if cfg == nil {
		return nil
	}
	var c Config
	if _, err := cfg.Extension(Block, &c); err != nil {
		return nil
	}
	return clean(c.Teams)
}

// Of returns the teams of a principal, sorted: the groups that match the
// patterns, or every group when there is no pattern. A nil principal, which
// is the process itself, has no team.
func Of(patterns []string, p *fw.Principal) []string {
	if p == nil || p.Subject == "" {
		return nil
	}
	var out []string
	for _, g := range p.Groups {
		if isTeam(patterns, g) {
			out = append(out, g)
		}
	}
	return sorted(out)
}

// OfAgent returns the teams of an agent, sorted: the groups that its access
// block names exactly, as group:<name>, and that match the patterns.
func OfAgent(patterns []string, access *fw.Access) []string {
	if access == nil {
		return nil
	}
	var out []string
	for _, list := range [][]string{access.Owners, access.Operators, access.Viewers} {
		for _, e := range list {
			g, ok := strings.CutPrefix(e, "group:")
			if ok && g != "" && !strings.ContainsAny(g, "*?[") && isTeam(patterns, g) {
				out = append(out, g)
			}
		}
	}
	return sorted(out)
}

// OfRun returns the teams that a run keeps in its metadata.
func OfRun(r *fw.Run) []string {
	if r == nil || r.Metadata == nil {
		return nil
	}
	return split(r.Metadata[MetaKey])
}

// Shares reports whether two lists of teams have a team in common.
func Shares(a, b []string) bool {
	for _, t := range a {
		if slices.Contains(b, t) {
			return true
		}
	}
	return false
}

// Annotate is the run annotator of the workbench (fw.RunAnnotator). A run
// that a person starts keeps the teams of the person under MetaKey. A child
// run that the process starts keeps the teams of its parent.
func Annotate(ctx context.Context, cfg *fw.Config, req *fw.RunRequest, parent *fw.Run) map[string]string {
	if p := fw.PrincipalFrom(ctx); p != nil && p.Subject != "" && p.Subject == req.Principal {
		if teams := Of(Patterns(cfg), p); len(teams) > 0 {
			return map[string]string{MetaKey: strings.Join(teams, ",")}
		}
		return nil
	}
	if parent != nil {
		if v := parent.Metadata[MetaKey]; v != "" {
			return map[string]string{MetaKey: v}
		}
	}
	return nil
}

// Values of by, the part of the runs that a list keeps.
const (
	ByMe     = "me"     // the runs that the caller started
	ByTeam   = "team"   // and the runs that a person on a team of the caller started
	ByOthers = "others" // every other run
)

// By keeps the runs of one part, for the principal in ctx and its teams.
// An empty by keeps every run. A context without a principal is the
// process itself: it started no run and has no team.
func By(ctx context.Context, cfg *fw.Config, runs []*fw.Run, by string) ([]*fw.Run, error) {
	by = strings.ToLower(strings.TrimSpace(by))
	switch by {
	case "":
		return runs, nil
	case ByMe, ByTeam, ByOthers:
	default:
		return nil, fmt.Errorf("%w: by is %q, not me, team, or others", fw.ErrInvalidRequest, by)
	}
	me, teams := "", []string(nil)
	if p := fw.PrincipalFrom(ctx); p != nil {
		me, teams = p.Subject, Of(Patterns(cfg), p)
	}
	out := make([]*fw.Run, 0, len(runs))
	for _, r := range runs {
		mine := me != "" && r.Principal == me
		near := mine || Shares(OfRun(r), teams)
		if (by == ByMe && mine) || (by == ByTeam && near) || (by == ByOthers && !near) {
			out = append(out, r)
		}
	}
	return out, nil
}

// isTeam reports whether a group counts as a team: it matches a pattern,
// and every group does when there is no pattern.
func isTeam(patterns []string, group string) bool {
	if len(patterns) == 0 {
		return true
	}
	for _, p := range patterns {
		if match(p, group) {
			return true
		}
	}
	return false
}

// match reports whether s matches the glob pattern, where "*" matches any
// text and "?" matches one character, as the access entries of the kernel
// match a subject.
func match(pattern, s string) bool {
	if pattern == "" {
		return s == ""
	}
	switch pattern[0] {
	case '*':
		for i := 0; i <= len(s); i++ {
			if match(pattern[1:], s[i:]) {
				return true
			}
		}
		return false
	case '?':
		return s != "" && match(pattern[1:], s[1:])
	}
	return s != "" && s[0] == pattern[0] && match(pattern[1:], s[1:])
}

func split(raw string) []string {
	var out []string
	for _, s := range strings.Split(raw, ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func clean(in []string) []string {
	var out []string
	for _, s := range in {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// sorted returns the names sorted, without duplicates or empty names.
func sorted(in []string) []string {
	out := clean(in)
	if len(out) == 0 {
		return nil
	}
	slices.Sort(out)
	return slices.Compact(out)
}
