// Package skills is the catalog of the skills of the front
// (docs/ARCHITECTURE.md section 20). It reads the skills that the kernel
// knows (fw.Agent.Skills), among them the skills that an admin imported
// into the first skills directory of the kernel, $FW_HOME/skills by
// default, which the marker .origin.json tells apart, and it parses the
// keys of the front matter of
// each SKILL.md that a slash command needs: user-invocable,
// disable-model-invocation, argument-hint, and arguments. It resolves the
// skills that the allowlist of a profile allows with the matching of the
// kernel (fw.SkillAllowed). It expands a skill for a slash command: the
// placeholders of the body take the arguments, and the message that a run
// gets starts with the line "Skill: <name>". It imports a skill from a
// marketplace, a URL, a pasted text, or an upload, after a preview
// (import.go). Nothing in the package calls a model.
package skills

import (
	"bufio"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/bdubs00/featherweight-kernel/fw"
	"gopkg.in/yaml.v3"
)

// Origins of a skill.
const (
	// OriginLocal is the origin of a skill of a skills directory, as the
	// kernel names it in GET /v1/skills (fw.SkillOriginLocal).
	OriginLocal = fw.SkillOriginLocal
	// OriginImported is the origin of a skill that an admin imported.
	OriginImported = "imported"
	// OriginMCP is the origin of a skill of an MCP server, whose entry
	// name Server holds (fw.SkillOriginMCP).
	OriginMCP = fw.SkillOriginMCP
)

// Front matter keys that a slash command reads.
const (
	KeyUserInvocable  = "user-invocable"
	KeyDisableModel   = "disable-model-invocation"
	KeyArgumentHint   = "argument-hint"
	KeyArguments      = "arguments"
	maxFrontMatter    = 64 << 10
	maxArgumentNames  = 32
	maxArgumentDetail = 300
)

// Entry is one skill of the catalog. Origin is OriginLocal, OriginImported,
// or OriginMCP, and Server names the tools.mcp entry of a skill of an MCP
// server, whose name is <server>/<name>. UserInvocable is false when the
// front matter says user-invocable: false, so no person hands the skill
// to a run. ModelInvocable is false when it says
// disable-model-invocation: true. Arguments names the arguments in order,
// for the placeholders $name. URI names the file of the skill: the URI of
// a server skill, else the path of SKILL.md relative to its skills
// directory. Files lists the other files of the skill directory. The
// catalog reads no file of a skill of a server ahead of need, so such an
// entry shows the defaults of the front matter keys; the slash command
// reads them when it reads the skill.
type Entry struct {
	Name           string     `json:"name"`
	Description    string     `json:"description"`
	Origin         string     `json:"origin"`
	Server         string     `json:"server,omitempty"`
	URI            string     `json:"uri"`
	UserInvocable  bool       `json:"userInvocable"`
	ModelInvocable bool       `json:"modelInvocable"`
	ArgumentHint   string     `json:"argumentHint"`
	Arguments      []Argument `json:"arguments"`
	Files          []string   `json:"files"`

	skill fw.Skill
}

// Argument is one named argument of a skill.
type Argument struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

// Ref is a skill as the view of an agent names it.
type Ref struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Origin      string `json:"origin"`
	Server      string `json:"server,omitempty"`
}

// ImportRoot returns the directory that an import writes into: the first
// skills directory that the kernel reads (fw.Config.SkillDirs), which is
// $FW_HOME/skills unless tools.skills names other directories. The kernel
// reads it one level deep at each call, so a model loads an imported
// skill on its own, as it loads a skill that a person puts there.
func ImportRoot(a *fw.Agent) string {
	if dirs := a.Config().SkillDirs(); len(dirs) > 0 && dirs[0] != "" {
		return dirs[0]
	}
	home := a.Config().Home
	if home == "" {
		home = fw.DefaultHome()
	}
	return filepath.Join(home, "skills")
}

// Catalog returns every skill that the front knows, the skills of the
// kernel, sorted by name. A skill in a directory of ImportRoot whose
// marker OriginFile sits beside its SKILL.md has the origin
// OriginImported. A value of the front matter with the wrong type takes
// its default, and the log says so.
func Catalog(a *fw.Agent) []Entry {
	return catalog(a, a.Logger())
}

func catalog(a *fw.Agent, log *slog.Logger) []Entry {
	kernel := a.Skills()
	out := make([]Entry, 0, len(kernel))
	dirs := a.Config().SkillDirs()
	root := ImportRoot(a)
	for _, s := range kernel {
		origin := originOf(s)
		if imported(root, s) {
			origin = OriginImported
		}
		out = append(out, entryOf(s, origin, uriOf(s, dirs), log))
	}
	sortEntries(out)
	return out
}

// imported reports whether a skill of the kernel is an imported skill: a
// directory right under the root of the imports with the marker beside
// its SKILL.md.
func imported(root string, s fw.Skill) bool {
	if s.Dir == "" || filepath.Clean(filepath.Dir(s.Dir)) != filepath.Clean(root) {
		return false
	}
	return isRegularFile(filepath.Join(s.Dir, OriginFile))
}

// isRegularFile reports whether a path is a regular file and no symbolic
// link, such as the marker of an imported skill.
func isRegularFile(p string) bool {
	st, err := os.Lstat(p)
	return err == nil && st.Mode().IsRegular()
}

// Named returns the skill of the catalog with that name.
func Named(a *fw.Agent, name string) (Entry, bool) {
	for _, e := range Catalog(a) {
		if e.Name == name {
			return e, true
		}
	}
	return Entry{}, false
}

// Allowed returns the skills of the catalog that the allowlist of a
// profile allows, with the matching of the kernel (fw.SkillAllowed): a nil
// list allows every skill, and an empty list allows none.
func Allowed(a *fw.Agent, globs []string) []Ref {
	out := []Ref{}
	for _, e := range Catalog(a) {
		if fw.SkillAllowed(globs, e.Name) {
			out = append(out, Ref{Name: e.Name, Description: e.Description, Origin: e.Origin, Server: e.Server})
		}
	}
	return out
}

// Body returns the body of the SKILL.md file of the skill, without the
// front matter.
func (e Entry) Body() (string, error) {
	text, err := e.skill.Body()
	if err != nil {
		return "", fmt.Errorf("skills: %w", err)
	}
	_, body := splitFrontMatter(text)
	return body, nil
}

// read reads SKILL.md of the skill and returns the entry with the keys of
// its front matter, and the body. A skill of a server is read at the
// moment of use, through the kernel, which checks it.
func (e Entry) read(log *slog.Logger) (Entry, string, error) {
	text, err := e.skill.Body()
	if err != nil {
		return e, "", fmt.Errorf("%w: the skill %s: %v", ErrUnavailable, e.Name, err)
	}
	fm, body := splitFrontMatter(text)
	m := parseMeta(fm)
	for _, p := range m.problems {
		log.Warn("skills: a key of the front matter does not fit, so it takes its default", "skill", e.Name, "problem", p)
	}
	out := e
	out.UserInvocable, out.ModelInvocable, out.ArgumentHint, out.Arguments = true, true, "", []Argument{}
	m.apply(&out)
	return out, body, nil
}

// Dir returns the directory of the skill on disk.
func (e Entry) Dir() string { return e.skill.Dir }

func sortEntries(list []Entry) {
	for i := 1; i < len(list); i++ {
		for j := i; j > 0 && list[j].Name < list[j-1].Name; j-- {
			list[j], list[j-1] = list[j-1], list[j]
		}
	}
}

// uriOf is the path of SKILL.md relative to the skills directory that
// holds the skill.
func uriOf(s fw.Skill, dirs []string) string {
	if u := skillURI(s); u != "" {
		return u
	}
	for _, d := range dirs {
		if d == "" {
			continue
		}
		rel, err := filepath.Rel(d, s.Dir)
		if err != nil || rel == ".." || strings.HasPrefix(rel, "../") {
			continue
		}
		return filepath.ToSlash(filepath.Join(rel, fw.SkillFile))
	}
	return fw.SkillFile
}

// entryOf reads the front matter of a skill for the catalog. A skill of a
// server keeps the defaults, because the catalog reads no file of it ahead
// of need.
func entryOf(s fw.Skill, origin, uri string, log *slog.Logger) Entry {
	e := Entry{Name: s.Name, Description: s.Description, Origin: origin, Server: s.Server, URI: uri, UserInvocable: true, ModelInvocable: true, Arguments: []Argument{}, Files: []string{}, skill: s}
	files, err := s.Files()
	if err != nil {
		log.Warn("skills: list the files of a skill", "skill", s.Name, "error", err)
	} else if files != nil {
		e.Files = files
	}
	if fromServer(s) {
		return e
	}
	text, err := s.Body()
	if err != nil {
		log.Warn("skills: read a skill", "skill", s.Name, "error", err)
		return e
	}
	fm, _ := splitFrontMatter(text)
	e.Description = descriptionOf(fm, e.Description)
	m := parseMeta(fm)
	for _, p := range m.problems {
		log.Warn("skills: a key of the front matter does not fit, so it takes its default", "skill", s.Name, "problem", p)
	}
	m.apply(&e)
	return e
}

// splitFrontMatter splits a SKILL.md text into the front matter, without
// the lines ---, and the body. A text that does not start with a line ---
// has no front matter.
func splitFrontMatter(text string) (string, string) {
	t := strings.TrimPrefix(text, "\ufeff")
	first, rest, ok := strings.Cut(t, "\n")
	if strings.TrimSpace(first) != "---" {
		return "", text
	}
	if !ok {
		return "", ""
	}
	off := 0
	for off <= len(rest) {
		line, after, more := strings.Cut(rest[off:], "\n")
		if strings.TrimSpace(line) == "---" {
			fm := rest[:off]
			if !more {
				return fm, ""
			}
			return fm, strings.TrimLeft(after, "\r\n")
		}
		if !more {
			break
		}
		off += len(line) + 1
	}
	// No closing line: the kernel reads the whole text as front matter.
	return rest, ""
}

// meta is what the catalog reads from a front matter. problems holds one
// sentence for each value that does not fit.
type meta struct {
	userInvocable *bool
	disableModel  *bool
	argumentHint  *string
	arguments     []Argument
	problems      []string
}

func (m meta) apply(e *Entry) {
	if m.userInvocable != nil {
		e.UserInvocable = *m.userInvocable
	}
	if m.disableModel != nil {
		e.ModelInvocable = !*m.disableModel
	}
	if m.argumentHint != nil {
		e.ArgumentHint = *m.argumentHint
	}
	if m.arguments != nil {
		e.Arguments = m.arguments
	}
}

// argName is the shape of a named argument, so $name stands out in a body.
var argName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// parseMeta reads the keys of a front matter with the YAML decoder. A
// front matter that is no YAML mapping falls back to one key: value per
// line, as the kernel reads the name and the description.
func parseMeta(fm string) meta {
	var m meta
	if strings.TrimSpace(fm) == "" {
		return m
	}
	if len(fm) > maxFrontMatter {
		m.problems = append(m.problems, fmt.Sprintf("the front matter is larger than %d bytes, so every key takes its default", maxFrontMatter))
		return m
	}
	var raw map[string]any
	if err := yaml.Unmarshal([]byte(fm), &raw); err != nil {
		m.problems = append(m.problems, "the front matter is not YAML ("+oneLine(err.Error())+"), so the keys are read one per line")
		return lineMeta(fm, m)
	}
	if v, ok := raw[KeyUserInvocable]; ok {
		if b, ok := v.(bool); ok {
			m.userInvocable = &b
		} else {
			m.problems = append(m.problems, KeyUserInvocable+" is not true or false")
		}
	}
	if v, ok := raw[KeyDisableModel]; ok {
		if b, ok := v.(bool); ok {
			m.disableModel = &b
		} else {
			m.problems = append(m.problems, KeyDisableModel+" is not true or false")
		}
	}
	if v, ok := raw[KeyArgumentHint]; ok {
		if s, ok := hintOf(v); ok {
			m.argumentHint = &s
		} else {
			m.problems = append(m.problems, KeyArgumentHint+" is not a text")
		}
	}
	if v, ok := raw[KeyArguments]; ok {
		args, err := argumentsOf(v)
		if err != nil {
			m.problems = append(m.problems, KeyArguments+" "+err.Error())
		} else {
			m.arguments = args
		}
	}
	return m
}

// lineMeta reads the scalar keys of a front matter that is no YAML, one
// key: value per line.
func lineMeta(fm string, m meta) meta {
	sc := bufio.NewScanner(strings.NewReader(fm))
	sc.Buffer(make([]byte, 64<<10), maxFrontMatter)
	for sc.Scan() {
		key, value, ok := strings.Cut(sc.Text(), ":")
		if !ok || strings.HasPrefix(key, " ") || strings.HasPrefix(key, "\t") {
			continue
		}
		value = strings.Trim(strings.TrimSpace(value), `"'`)
		switch strings.TrimSpace(key) {
		case KeyUserInvocable:
			if b, ok := boolWord(value); ok {
				m.userInvocable = &b
			} else {
				m.problems = append(m.problems, KeyUserInvocable+" is not true or false")
			}
		case KeyDisableModel:
			if b, ok := boolWord(value); ok {
				m.disableModel = &b
			} else {
				m.problems = append(m.problems, KeyDisableModel+" is not true or false")
			}
		case KeyArgumentHint:
			v := value
			m.argumentHint = &v
		}
	}
	return m
}

func boolWord(s string) (bool, bool) {
	switch strings.ToLower(s) {
	case "true":
		return true, true
	case "false":
		return false, true
	}
	return false, false
}

// hintOf reads argument-hint: a text, a number, or a list of words, which
// the hint shows as [word] each, as the author wrote it.
func hintOf(v any) (string, bool) {
	switch x := v.(type) {
	case string:
		return strings.TrimSpace(x), true
	case int, int64, float64:
		return fmt.Sprint(x), true
	case []any:
		parts := make([]string, 0, len(x))
		for _, item := range x {
			switch w := item.(type) {
			case string, int, int64, float64:
				parts = append(parts, "["+strings.TrimSpace(fmt.Sprint(w))+"]")
			default:
				return "", false
			}
		}
		return strings.Join(parts, " "), true
	}
	return "", false
}

// argumentsOf reads arguments: a list of names, a list of objects with a
// name and an optional description, or one text of names with spaces
// between them. A name holds letters, digits, and underscores, and does
// not start with a digit.
func argumentsOf(v any) ([]Argument, error) {
	var items []any
	switch x := v.(type) {
	case nil:
		return []Argument{}, nil
	case string:
		for _, f := range strings.Fields(strings.ReplaceAll(x, ",", " ")) {
			items = append(items, f)
		}
	case []any:
		items = x
	default:
		return nil, errors.New("is not a list of names")
	}
	if len(items) > maxArgumentNames {
		return nil, fmt.Errorf("names more than %d arguments", maxArgumentNames)
	}
	out := make([]Argument, 0, len(items))
	seen := map[string]bool{}
	for _, item := range items {
		var a Argument
		switch x := item.(type) {
		case string:
			a.Name = strings.TrimSpace(x)
		case map[string]any:
			name, ok := x["name"].(string)
			if !ok {
				return nil, errors.New("holds an object without a name")
			}
			a.Name = strings.TrimSpace(name)
			if d, ok := x["description"]; ok {
				s, ok := d.(string)
				if !ok {
					return nil, fmt.Errorf("gives %s a description that is not a text", a.Name)
				}
				a.Description = cut(strings.TrimSpace(s), maxArgumentDetail)
			}
		default:
			return nil, errors.New("holds an item that is neither a name nor an object with a name")
		}
		switch {
		case !argName.MatchString(a.Name):
			return nil, fmt.Errorf("holds the name %q, and a name holds letters, digits, and underscores, with no digit first", a.Name)
		case a.Name == "ARGUMENTS":
			return nil, errors.New("holds the name ARGUMENTS, which the placeholder $ARGUMENTS takes")
		case seen[a.Name]:
			return nil, fmt.Errorf("names %s twice", a.Name)
		}
		seen[a.Name] = true
		out = append(out, a)
	}
	return out, nil
}

// oneLine is a text on one line, at most 300 bytes.
func oneLine(s string) string {
	return cut(strings.Join(strings.Fields(s), " "), 300)
}

// cut cuts a text to at most n bytes at a rune.
func cut(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && s[n]&0xC0 == 0x80 {
		n--
	}
	return s[:n]
}
