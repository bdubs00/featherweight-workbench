package skills

// This file imports a skill into the skills directory of the front
// (docs/ARCHITECTURE.md section 20.7). A source is a URL, a skill of a
// marketplace, a pasted SKILL.md, or an upload of text files. Preview
// fetches the source and checks it, and installs nothing, so an admin
// reads what the agents will load first. Install runs the same checks and
// writes the files into the first skills directory of the kernel,
// $FW_HOME/skills/<name> by default (ImportRoot), with the marker
// .origin.json beside SKILL.md, which tells an imported skill from a
// configured one. The kernel reads the directory at each call and skips a
// hidden file, so a model loads the skill on its own and never sees the
// marker. Remove removes an imported skill, never a
// configured one. Nothing here runs a file of a skill.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/bdubs00/featherweight-kernel/fw"
	"gopkg.in/yaml.v3"
)

// Bounds of an imported skill.
const (
	// MaxImportFiles bounds the files of a skill, SKILL.md included.
	MaxImportFiles = 64
	// MaxImportBytes bounds the files of a skill together.
	MaxImportBytes = 1 << 20
	// maxPathBytes bounds the path of one file.
	maxPathBytes = 256
	// maxNameBytes bounds the name of an imported skill.
	maxNameBytes = 64
)

// OriginFile is the record of an imported skill, beside its SKILL.md. The
// kernel and the catalog hide it, because its name starts with a dot.
const OriginFile = ".origin.json"

// Errors of an import.
var (
	// ErrExists is a name that the catalog holds already.
	ErrExists = errors.New("skills: the name exists")
	// ErrRefused is a source whose checks block an install.
	ErrRefused = errors.New("skills: the import is refused")
	// ErrNotImported is a skill that no admin imported.
	ErrNotImported = errors.New("skills: not an imported skill")
	// ErrChanged is a source whose files differ from the preview that the
	// admin read: its digest is not the digest of the install.
	ErrChanged = errors.New("skills: the source changed since the preview")
)

// Source names where a skill comes from: exactly one of URL, Marketplace
// with Name, Text, and Files.
//
//   - URL is a SKILL.md, the directory of a skill in a repository, or a zip
//     archive.
//   - Marketplace and Name name a skill of a marketplace of the
//     configuration.
//   - Text is a SKILL.md that a person pasted.
//   - Files is an upload of the page: text files, each with a path in the
//     skill directory.
type Source struct {
	URL         string `json:"url,omitempty"`
	Marketplace string `json:"marketplace,omitempty"`
	Name        string `json:"name,omitempty"`
	Text        string `json:"text,omitempty"`
	Files       []File `json:"files,omitempty"`
}

// File is one file of an upload.
type File struct {
	Path string `json:"path"`
	Text string `json:"text"`
}

// FileInfo is one file of a preview.
type FileInfo struct {
	Path string `json:"path"`
	Size int    `json:"size"`
}

// Problem is one finding of the checks. Blocks is true when the finding
// stops an install, such as a missing name, and false for a note, such as
// a binary file that the import leaves out.
type Problem struct {
	Code   string `json:"code"`
	Path   string `json:"path,omitempty"`
	Text   string `json:"text"`
	Blocks bool   `json:"blocks"`
}

// Codes of a problem.
const (
	ProblemNoSkillFile  = "no-skill-file"
	ProblemNameMissing  = "name-missing"
	ProblemNameInvalid  = "name-invalid"
	ProblemNameExists   = "name-exists"
	ProblemTooLarge     = "too-large"
	ProblemTooManyFiles = "too-many-files"
	ProblemPathEscapes  = "path-escapes"
	ProblemBinary       = "binary"
	ProblemHidden       = "hidden"
	ProblemFrontMatter  = "front-matter"
)

// Record is the source of an imported skill, as .origin.json and a
// preview name it.
type Record struct {
	Kind        string     `json:"kind"` // url, marketplace, text, or files
	URL         string     `json:"url,omitempty"`
	Marketplace string     `json:"marketplace,omitempty"`
	Name        string     `json:"name,omitempty"`
	Digest      string     `json:"digest,omitempty"`
	InstalledBy string     `json:"installedBy,omitempty"`
	InstalledAt *time.Time `json:"installedAt,omitempty"`
	Files       []FileInfo `json:"files,omitempty"`
}

// Existing is the skill of the catalog, or the directory of the skills
// directory, that holds the name of a preview. Replaceable is true for an
// imported skill, whose marker sits beside its SKILL.md, which an install
// with replace takes the place of. Directory is true for a directory of
// that name that holds no skill of that name, such as a skill of the
// configuration whose front matter names it otherwise.
type Existing struct {
	Origin      string `json:"origin"`
	Replaceable bool   `json:"replaceable"`
	Digest      string `json:"digest,omitempty"`
	Directory   bool   `json:"directory,omitempty"`
}

// existsText words the skill or the directory that holds a name.
func existsText(name string, ex *Existing) string {
	switch {
	case ex.Replaceable:
		return "an imported skill has the name " + name + "; an install with replace takes its place"
	case ex.Directory:
		return "the skills directory holds a directory " + name + " without the marker of an import, and an import never writes over it"
	}
	return "a skill of the configuration has the name " + name + ", and an import never replaces it"
}

// Preview is what an install will write and what the checks found.
// Installable is true when no problem blocks an install; a name that an
// imported skill holds blocks it unless the install replaces it.
type Preview struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	FrontMatter map[string]any `json:"frontMatter"`
	Entry       Entry          `json:"entry"`
	Body        string         `json:"body"`
	Files       []FileInfo     `json:"files"`
	Digest      string         `json:"digest"`
	Source      Record         `json:"source"`
	Exists      *Existing      `json:"exists"`
	Problems    []Problem      `json:"problems"`
	Installable bool           `json:"installable"`

	files map[string][]byte
}

// Importer previews, installs, and removes the imported skills of one
// agent. Installs and removals of one importer run one at a time.
type Importer struct {
	a      *fw.Agent
	fetch  *Fetcher
	market *Market
	now    func() time.Time
	mu     sync.Mutex
}

// NewImporter returns the importer of an agent with the marketplaces of
// its configuration.
func NewImporter(a *fw.Agent, f *Fetcher, m *Market) *Importer {
	return &Importer{a: a, fetch: f, market: m, now: time.Now}
}

// Market returns the marketplaces of the importer.
func (im *Importer) Market() *Market { return im.market }

// Preview fetches a source and checks it. It installs nothing. It returns
// an error that wraps fw.ErrInvalidRequest for a source that names no
// source or two, and a *FetchError for a fetch that failed.
func (im *Importer) Preview(ctx context.Context, src Source) (Preview, error) {
	files, rec, hint, err := im.gather(ctx, src)
	if err != nil {
		return Preview{}, err
	}
	p := im.check(files, hint)
	p.Source = rec
	p.Source.Digest = p.Digest
	return p, nil
}

// Install previews a source and writes the skill under the imported
// directory when no problem blocks it. digest is the digest of the preview
// that the admin read. A source that the front fetches, a URL or a skill
// of a marketplace, needs it, because the host can serve other files to
// the install than to the preview; a text and an upload hold their files
// in the request, and a digest that they give must match too. A digest
// that differs answers ErrChanged. A name that an imported skill holds
// answers ErrExists unless replace is true, and a name that a configured
// skill holds always does. by names the admin. It returns the entry of the
// catalog and the preview.
func (im *Importer) Install(ctx context.Context, src Source, replace bool, digest, by string) (Entry, Preview, error) {
	fetched := src.URL != "" || src.Marketplace != "" || src.Name != ""
	digest = strings.TrimSpace(digest)
	if fetched && digest == "" && src.Text == "" && len(src.Files) == 0 {
		return Entry{}, Preview{}, fmt.Errorf("%w: an install from a URL or a marketplace needs digest, the digest of the preview that you read", fw.ErrInvalidRequest)
	}
	p, err := im.Preview(ctx, src)
	if err != nil {
		return Entry{}, p, err
	}
	if digest != "" && digest != p.Digest {
		return Entry{}, p, fmt.Errorf("%w: the preview had the digest %s, and the source now gives %s; read the preview again", ErrChanged, digest, p.Digest)
	}
	im.mu.Lock()
	defer im.mu.Unlock()
	// The catalog can change between the preview and the lock.
	p.Exists = im.existing(p.Name)
	if p.Exists != nil && (!replace || !p.Exists.Replaceable) {
		return Entry{}, p, fmt.Errorf("%w: %s", ErrExists, existsText(p.Name, p.Exists))
	}
	if root := ImportRoot(im.a); isRegularFile(filepath.Join(root, fw.SkillFile)) {
		return Entry{}, p, fmt.Errorf("%w: the first skills directory %s holds a SKILL.md itself, so the kernel reads it as one skill and reads no skill below it; name another directory first in tools.skills", fw.ErrInvalidRequest, root)
	}
	var blocking []string
	for _, pr := range p.Problems {
		if pr.Blocks && pr.Code != ProblemNameExists {
			blocking = append(blocking, pr.Text)
		}
	}
	if len(blocking) > 0 {
		return Entry{}, p, fmt.Errorf("%w: %s", ErrRefused, strings.Join(blocking, "; "))
	}
	now := im.now().UTC()
	rec := p.Source
	rec.InstalledBy, rec.InstalledAt, rec.Files = by, &now, p.Files
	if err := im.write(p, rec); err != nil {
		return Entry{}, p, err
	}
	im.a.Logger().Info("skills: an admin installed a skill", "skill", p.Name, "source", rec.Kind, "url", rec.URL, "marketplace", rec.Marketplace, "digest", p.Digest, "replaced", p.Exists != nil, "by", by)
	e, ok := Named(im.a, p.Name)
	if !ok {
		return Entry{}, p, fmt.Errorf("skills: the catalog does not list the skill %s after the install", p.Name)
	}
	return e, p, nil
}

// Remove removes an imported skill. It returns an error that wraps
// ErrNotImported for a skill of the configuration, and ErrNotFoundSkill for
// a name that the catalog does not hold.
func (im *Importer) Remove(name, by string) error {
	if name == "" || fw.ValidSkillName(name) != name {
		return fmt.Errorf("%w: %q is no name of a skill", fw.ErrInvalidRequest, name)
	}
	im.mu.Lock()
	defer im.mu.Unlock()
	root := ImportRoot(im.a)
	if !isRegularFile(filepath.Join(root, name, OriginFile)) {
		_, named := Named(im.a, name)
		if _, err := os.Lstat(filepath.Join(root, name)); named || err == nil {
			return fmt.Errorf("%w: the skill %s has no marker of an import, so it comes from the configuration, and only an imported skill can be removed here", ErrNotImported, name)
		}
		return fmt.Errorf("%w: no imported skill has the name %s", ErrNotFoundSkill, name)
	}
	r, err := os.OpenRoot(root)
	if err != nil {
		return fmt.Errorf("skills: open %s: %w", root, err)
	}
	defer r.Close()
	gone := ".remove-" + name + "-" + randomSuffix()
	if err := r.Rename(name, gone); err != nil {
		return fmt.Errorf("skills: remove %s: %w", name, err)
	}
	if err := r.RemoveAll(gone); err != nil {
		im.a.Logger().Warn("skills: remove the files of a removed skill", "skill", name, "error", err)
	}
	im.a.Logger().Info("skills: an admin removed an imported skill", "skill", name, "by", by)
	return nil
}

// ErrNotFoundSkill is a name that no imported skill holds.
var ErrNotFoundSkill = errors.New("skills: not found")

// existing returns the skill of the catalog with the name, or the
// directory of that name in ImportRoot, or nil.
func (im *Importer) existing(name string) *Existing {
	if name == "" || fw.ValidSkillName(name) != name {
		return nil
	}
	e, ok := Named(im.a, name)
	if !ok {
		dir := filepath.Join(ImportRoot(im.a), name)
		if _, err := os.Lstat(dir); err != nil {
			return nil
		}
		// A directory that the catalog does not name: a skill of the
		// configuration whose front matter names it otherwise, or files
		// that no import wrote. Only an import with its marker is ever
		// replaced.
		if isRegularFile(filepath.Join(dir, OriginFile)) {
			return &Existing{Origin: OriginImported, Replaceable: true}
		}
		return &Existing{Origin: OriginLocal, Directory: true}
	}
	ex := &Existing{Origin: e.Origin, Replaceable: e.Origin == OriginImported}
	if ex.Replaceable {
		if b, err := os.ReadFile(filepath.Join(e.Dir(), OriginFile)); err == nil {
			var r Record
			if json.Unmarshal(b, &r) == nil {
				ex.Digest = r.Digest
			}
		}
	}
	return ex
}

// gather reads the files of a source, by their paths in the skill
// directory, and names the source. hint is a name for a skill whose front
// matter names none, such as the directory of the source.
func (im *Importer) gather(ctx context.Context, src Source) (map[string][]byte, Record, string, error) {
	n := 0
	for _, set := range []bool{src.URL != "", src.Marketplace != "" || src.Name != "", src.Text != "", len(src.Files) > 0} {
		if set {
			n++
		}
	}
	if n != 1 {
		return nil, Record{}, "", fmt.Errorf("%w: name exactly one source: url, marketplace with name, text, or files", fw.ErrInvalidRequest)
	}
	switch {
	case src.Text != "":
		if len(src.Text) > MaxImportBytes {
			return nil, Record{}, "", fetchErr(413, "the text holds more than %d bytes", MaxImportBytes)
		}
		return map[string][]byte{fw.SkillFile: []byte(src.Text)}, Record{Kind: "text"}, "", nil
	case len(src.Files) > 0:
		if len(src.Files) > MaxImportFiles {
			return nil, Record{}, "", fetchErr(413, "an upload holds at most %d files", MaxImportFiles)
		}
		files := map[string][]byte{}
		total := 0
		for _, f := range src.Files {
			total += len(f.Text)
			if total > MaxImportBytes {
				return nil, Record{}, "", fetchErr(413, "an upload holds at most %d bytes", MaxImportBytes)
			}
			if _, dup := files[f.Path]; dup {
				return nil, Record{}, "", fmt.Errorf("%w: the upload names %s twice", fw.ErrInvalidRequest, f.Path)
			}
			files[f.Path] = []byte(f.Text)
		}
		return stripTop(files), Record{Kind: "files"}, "", nil
	case src.URL != "":
		files, hint, err := im.fetchURL(ctx, src.URL)
		u, _ := checkURL(src.URL)
		rec := Record{Kind: "url"}
		if u != nil {
			rec.URL = u.Redacted()
		}
		return files, rec, hint, err
	}
	if src.Marketplace == "" || src.Name == "" {
		return nil, Record{}, "", fmt.Errorf("%w: a skill of a marketplace needs marketplace and name", fw.ErrInvalidRequest)
	}
	if im.market == nil {
		return nil, Record{}, "", fmt.Errorf("%w: no marketplace is configured", ErrNotFoundSkill)
	}
	m, idx, err := im.market.find(ctx, src.Marketplace)
	if err != nil {
		return nil, Record{}, "", err
	}
	var hit *IndexEntry
	for i := range idx.Skills {
		if idx.Skills[i].Name == src.Name {
			hit = &idx.Skills[i]
			break
		}
	}
	if hit == nil {
		return nil, Record{}, "", fmt.Errorf("%w: the marketplace %s offers no skill %s", ErrNotFoundSkill, src.Marketplace, src.Name)
	}
	rec := Record{Kind: "marketplace", Marketplace: m.Name, Name: hit.Name}
	var files map[string][]byte
	var hint string
	if hit.URL != "" {
		rec.URL = hit.URL
		files, hint, err = im.fetchURL(ctx, hit.URL)
	} else {
		r := idx.repo
		r.dir = hit.Path
		rec.URL = repoDirURL(r)
		files, err = im.fetchDir(ctx, r, idx.tree)
		hint = path.Base(hit.Path)
	}
	if hint == "" || hint == "." {
		hint = hit.Name
	}
	return files, rec, hint, err
}

// repoDirURL names a directory of a repository for the record.
func repoDirURL(r repo) string {
	if r.dir == "" {
		return r.web
	}
	if r.github {
		return r.web + "/tree/" + r.ref + "/" + r.dir
	}
	return r.web + "#" + r.ref + ":" + r.dir
}

// fetchURL reads the files of a URL: the directory of a skill in a
// repository, a zip archive, or one SKILL.md.
func (im *Importer) fetchURL(ctx context.Context, raw string) (map[string][]byte, string, error) {
	u, err := checkURL(raw)
	if err != nil {
		return nil, "", err
	}
	if r, ok := im.fetch.repoOf(u); ok {
		files, err := im.fetchDir(ctx, r, tree{})
		hint := path.Base(r.dir)
		if r.dir == "" {
			hint = r.name
		}
		return files, hint, err
	}
	b, ctype, err := im.fetch.get(ctx, u.String())
	if err != nil {
		return nil, "", err
	}
	if strings.HasPrefix(string(b), "PK\x03\x04") || strings.Contains(ctype, "zip") {
		t, err := unzipTree(ctx, b)
		if err != nil {
			return nil, "", err
		}
		dir, err := skillDir(t.paths)
		if err != nil {
			return nil, "", err
		}
		files := map[string][]byte{}
		for _, p := range t.paths {
			if rel, ok := under(p, dir); ok {
				files[rel] = t.text[p]
			}
		}
		hint := path.Base(dir)
		if dir == "" {
			hint = strings.TrimSuffix(path.Base(u.Path), ".zip")
		}
		return files, hint, nil
	}
	if strings.Contains(ctype, "html") {
		return nil, "", fetchErr(400, "%s is a web page, not a SKILL.md, a zip archive, or the directory of a skill in a repository", u.Redacted())
	}
	hint := ""
	if strings.EqualFold(path.Base(u.Path), fw.SkillFile) {
		hint = path.Base(path.Dir(u.Path))
	}
	return map[string][]byte{fw.SkillFile: b}, hint, nil
}

// fetchDir reads the files of the skill in the directory r.dir of a
// repository. t is a listing that the caller holds, or empty.
func (im *Importer) fetchDir(ctx context.Context, r repo, t tree) (map[string][]byte, error) {
	if t.paths == nil {
		var err error
		if t, err = im.fetch.list(ctx, r); err != nil {
			return nil, err
		}
	}
	var mine []string
	for _, p := range t.paths {
		if _, ok := under(p, r.dir); ok {
			mine = append(mine, p)
		}
	}
	has := false
	for _, p := range mine {
		if rel, _ := under(p, r.dir); rel == fw.SkillFile {
			has = true
		}
	}
	if !has {
		where := r.dir
		if where == "" {
			where = "the root"
		}
		return nil, fetchErr(400, "%s of the repository holds no SKILL.md; name the directory of one skill, or add the repository as a marketplace", where)
	}
	if len(mine) > MaxImportFiles {
		return nil, fetchErr(413, "the skill holds %d files, and an import takes at most %d", len(mine), MaxImportFiles)
	}
	files := map[string][]byte{}
	total := 0
	for _, p := range mine {
		b, err := im.fetch.read(ctx, r, t, p)
		if err != nil {
			return nil, err
		}
		total += len(b)
		if total > MaxImportBytes {
			return nil, fetchErr(413, "the skill holds more than %d bytes", MaxImportBytes)
		}
		rel, _ := under(p, r.dir)
		files[rel] = b
	}
	return files, nil
}

// under reports whether a path lies under a directory, and returns it
// relative to the directory.
func under(p, dir string) (string, bool) {
	if dir == "" {
		return p, true
	}
	rel, ok := strings.CutPrefix(p, strings.TrimSuffix(dir, "/")+"/")
	return rel, ok && rel != ""
}

// skillDir finds the directory of the one skill of an archive: the root
// when it holds SKILL.md, else the one directory that holds one.
func skillDir(paths []string) (string, error) {
	var dirs []string
	for _, p := range paths {
		if p == fw.SkillFile {
			return "", nil
		}
		if path.Base(p) == fw.SkillFile {
			dirs = append(dirs, path.Dir(p))
		}
	}
	switch len(dirs) {
	case 0:
		return "", fetchErr(400, "the archive holds no SKILL.md")
	case 1:
		return dirs[0], nil
	}
	sort.Strings(dirs)
	return "", fetchErr(400, "the archive holds %d skills (%s); an import takes one", len(dirs), strings.Join(dirs, ", "))
}

// stripTop removes a top directory that every path of an upload shares,
// when SKILL.md sits right below it, as a person who uploads a folder
// sends it.
func stripTop(files map[string][]byte) map[string][]byte {
	if _, ok := files[fw.SkillFile]; ok {
		return files
	}
	top := ""
	for p := range files {
		first, _, ok := strings.Cut(strings.TrimPrefix(p, "./"), "/")
		if !ok || (top != "" && first != top) {
			return files
		}
		top = first
	}
	if _, ok := files[top+"/"+fw.SkillFile]; !ok {
		return files
	}
	out := make(map[string][]byte, len(files))
	for p, b := range files {
		out[strings.TrimPrefix(strings.TrimPrefix(p, "./"), top+"/")] = b
	}
	return out
}

// check runs the checks of an import on the files of a source.
func (im *Importer) check(files map[string][]byte, hint string) Preview {
	p := Preview{FrontMatter: map[string]any{}, Files: []FileInfo{}, Problems: []Problem{}, files: map[string][]byte{}}
	add := func(code, path, text string, blocks bool) {
		p.Problems = append(p.Problems, Problem{Code: code, Path: path, Text: text, Blocks: blocks})
	}
	paths := make([]string, 0, len(files))
	for k := range files {
		paths = append(paths, k)
	}
	sort.Strings(paths)
	total := 0
	for _, raw := range paths {
		b := files[raw]
		clean, code, why := checkPath(raw)
		switch code {
		case ProblemPathEscapes:
			add(code, raw, why, true)
			continue
		case ProblemHidden:
			add(code, raw, why, false)
			continue
		}
		if !utf8.Valid(b) || bytesContain(b, 0) {
			add(ProblemBinary, clean, "the file "+clean+" is no UTF-8 text, so the import leaves it out", false)
			continue
		}
		if len(b) > MaxImportBytes {
			add(ProblemTooLarge, clean, fmt.Sprintf("the file %s holds %d bytes, and a skill holds at most %d", clean, len(b), MaxImportBytes), true)
			continue
		}
		if _, dup := p.files[clean]; dup {
			add(ProblemPathEscapes, raw, "two files have the path "+clean, true)
			continue
		}
		total += len(b)
		p.files[clean] = b
		p.Files = append(p.Files, FileInfo{Path: clean, Size: len(b)})
	}
	for _, fi := range p.Files {
		f := fi.Path
		for dir := path.Dir(f); dir != "."; dir = path.Dir(dir) {
			if _, ok := p.files[dir]; ok {
				add(ProblemPathEscapes, f, "the path "+dir+" is a file and the directory of "+f, true)
			}
		}
	}
	if len(p.files) > MaxImportFiles {
		add(ProblemTooManyFiles, "", fmt.Sprintf("the skill holds %d files, and a skill holds at most %d", len(p.files), MaxImportFiles), true)
	}
	if total > MaxImportBytes {
		add(ProblemTooLarge, "", fmt.Sprintf("the files hold %d bytes together, and a skill holds at most %d", total, MaxImportBytes), true)
	}
	sort.Slice(p.Files, func(i, j int) bool { return p.Files[i].Path < p.Files[j].Path })
	p.Digest = digestOf(p.files)
	text, ok := p.files[fw.SkillFile]
	if !ok {
		add(ProblemNoSkillFile, fw.SkillFile, "the skill holds no SKILL.md of text at its root", true)
		p.Installable = false
		return p
	}
	fm, body := splitFrontMatter(string(text))
	p.Body = body
	if strings.TrimSpace(fm) != "" {
		var raw map[string]any
		if err := yaml.Unmarshal([]byte(fm), &raw); err == nil && raw != nil {
			p.FrontMatter = raw
		}
	}
	m := parseMeta(fm)
	for _, pr := range m.problems {
		add(ProblemFrontMatter, fw.SkillFile, pr, false)
	}
	// The kernel reads the name and the description one line at a time
	// (kernelMeta), so the name of the directory is the name that the
	// kernel and the catalog read back.
	raw, desc := kernelMeta(fm)
	name := raw
	if name == "" {
		name = strings.TrimSpace(hint)
	}
	switch {
	case name == "":
		add(ProblemNameMissing, fw.SkillFile, "the server matter names no skill; add name: to SKILL.md", true)
	case len(name) > maxNameBytes || fw.ValidSkillName(name) != name:
		add(ProblemNameInvalid, fw.SkillFile, fmt.Sprintf("the name %q is no name of a skill: at most %d letters, digits, dashes, underscores, and dots, with no dot first, on one line without a comment", name, maxNameBytes), true)
	}
	p.Name = name
	p.Description = desc
	p.Entry = Entry{Name: name, Description: desc, Origin: OriginImported, URI: name + "/" + fw.SkillFile, UserInvocable: true, ModelInvocable: true, Arguments: []Argument{}, Files: []string{}}
	m.apply(&p.Entry)
	for _, f := range p.Files {
		if f.Path != fw.SkillFile {
			p.Entry.Files = append(p.Entry.Files, f.Path)
		}
	}
	if p.Name != "" {
		if ex := im.existing(p.Name); ex != nil {
			p.Exists = ex
			add(ProblemNameExists, "", existsText(p.Name, ex), true)
		}
	}
	p.Installable = true
	for _, pr := range p.Problems {
		if pr.Blocks && !(pr.Code == ProblemNameExists && p.Exists != nil && p.Exists.Replaceable) {
			p.Installable = false
		}
	}
	return p
}

// kernelMeta reads the name of a front matter as the kernel reads it
// (fw.SkillFrontMatter), and the description as the kernel reads it, or
// as YAML reads it when the description is a block scalar (blockText).
func kernelMeta(fm string) (name, description string) {
	name, description = fw.SkillFrontMatter("---\n" + fm + "\n---\n")
	return strings.TrimSpace(name), descriptionOf(fm, description)
}

// blockIndicator is the head of a YAML block scalar: | for a literal and
// > for a folded one, with an indentation digit and a chomping sign, + or
// -, in either order, and an optional comment.
var blockIndicator = regexp.MustCompile(`^[|>](?:[1-9][+-]?|[+-][1-9]?)?(?:\s+#.*)?$`)

// descriptionOf returns the description of a skill for the catalog, the
// index of a marketplace, and the preview of an import. The kernel reads
// one key: value per line, so for a block scalar, such as
//
//	description: >-
//	  Use this skill when ...
//
// it reads the indicator alone. The front then reads the description with
// the YAML decoder and keeps it on one line, cut to the bound of the
// kernel (fw.CutSkillDescription), so it has the shape of a description
// that the kernel reads. Any other description stays as the kernel read
// it.
func descriptionOf(fm, kernel string) string {
	kernel = strings.TrimSpace(kernel)
	if !blockIndicator.MatchString(kernel) || len(fm) > maxFrontMatter {
		return kernel
	}
	var raw map[string]any
	if yaml.Unmarshal([]byte(fm), &raw) != nil {
		return kernel
	}
	d, ok := raw["description"].(string)
	if !ok {
		return kernel
	}
	return strings.TrimSpace(fw.CutSkillDescription(strings.Join(strings.Fields(d), " ")))
}

func bytesContain(b []byte, c byte) bool {
	for _, x := range b {
		if x == c {
			return true
		}
	}
	return false
}

// checkPath cleans the path of a file of a skill. A path that leaves the
// skill directory, an absolute path, a backslash, and a control character
// give ProblemPathEscapes. A hidden part, such as .git or .origin.json,
// gives ProblemHidden, because the kernel hides such a file.
func checkPath(raw string) (string, string, string) {
	if raw == "" || len(raw) > maxPathBytes {
		return "", ProblemPathEscapes, fmt.Sprintf("the path %q is empty or longer than %d bytes", raw, maxPathBytes)
	}
	for _, r := range raw {
		if r < 0x20 || r == 0x7f || r == '\\' {
			return "", ProblemPathEscapes, fmt.Sprintf("the path %q holds a backslash or a control character", raw)
		}
	}
	if strings.HasPrefix(raw, "/") || !utf8.ValidString(raw) {
		return "", ProblemPathEscapes, fmt.Sprintf("the path %q is not relative to the skill directory", raw)
	}
	for _, part := range strings.Split(raw, "/") {
		if part == ".." {
			return "", ProblemPathEscapes, fmt.Sprintf("the path %q leaves the skill directory", raw)
		}
	}
	clean := path.Clean(raw)
	if clean == "." || strings.HasPrefix(clean, "../") {
		return "", ProblemPathEscapes, fmt.Sprintf("the path %q leaves the skill directory", raw)
	}
	for _, part := range strings.Split(clean, "/") {
		if len(part) > maxPartBytes {
			return "", ProblemPathEscapes, fmt.Sprintf("a part of the path %q is longer than %d bytes", raw, maxPartBytes)
		}
	}
	for _, part := range strings.Split(clean, "/") {
		if strings.HasPrefix(part, ".") {
			return clean, ProblemHidden, "the file " + clean + " is hidden, so the import leaves it out"
		}
	}
	return clean, "", ""
}

// maxPartBytes bounds one part of the path of a file, as a file system
// bounds a name.
const maxPartBytes = 255

// digestOf is the SHA-256 of the files of a skill: each path with its
// length and its bytes, in the order of the paths.
func digestOf(files map[string][]byte) string {
	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	h := sha256.New()
	for _, p := range paths {
		h.Write([]byte(p))
		h.Write([]byte{0})
		h.Write([]byte(strconv.Itoa(len(files[p]))))
		h.Write([]byte{0})
		h.Write(files[p])
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

// write writes the files of a preview into a new hidden directory of
// ImportRoot, which the kernel skips, and moves it into place, so the
// kernel and the catalog never list a skill that is half written. A skill
// that the install replaces moves aside first and is removed after; only a
// directory with the marker of an import is ever replaced.
func (im *Importer) write(p Preview, rec Record) error {
	root := ImportRoot(im.a)
	if err := os.MkdirAll(root, 0o755); err != nil {
		return fmt.Errorf("skills: create %s: %w", root, err)
	}
	r, err := os.OpenRoot(root)
	if err != nil {
		return fmt.Errorf("skills: open %s: %w", root, err)
	}
	defer r.Close()
	tmp := ".install-" + p.Name + "-" + randomSuffix()
	if err := r.Mkdir(tmp, 0o755); err != nil {
		return fmt.Errorf("skills: create the directory of %s: %w", p.Name, err)
	}
	fail := func(err error) error {
		_ = r.RemoveAll(tmp)
		return err
	}
	for _, f := range p.Files {
		target := tmp + "/" + f.Path
		if dir := path.Dir(target); dir != tmp {
			if err := r.MkdirAll(dir, 0o755); err != nil {
				return fail(fmt.Errorf("skills: write %s of %s: %w", f.Path, p.Name, err))
			}
		}
		if err := r.WriteFile(target, p.files[f.Path], 0o644); err != nil {
			return fail(fmt.Errorf("skills: write %s of %s: %w", f.Path, p.Name, err))
		}
	}
	b, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return fail(fmt.Errorf("skills: encode the record of %s: %w", p.Name, err))
	}
	if err := r.WriteFile(tmp+"/"+OriginFile, append(b, '\n'), 0o644); err != nil {
		return fail(fmt.Errorf("skills: write the record of %s: %w", p.Name, err))
	}
	old := ""
	if _, err := r.Lstat(p.Name); err == nil {
		if st, err := r.Lstat(p.Name + "/" + OriginFile); err != nil || !st.Mode().IsRegular() {
			return fail(fmt.Errorf("%w: %s", ErrExists, existsText(p.Name, &Existing{Directory: true})))
		}
		old = ".replaced-" + p.Name + "-" + randomSuffix()
		if err := r.Rename(p.Name, old); err != nil {
			return fail(fmt.Errorf("skills: move %s aside: %w", p.Name, err))
		}
	}
	if err := r.Rename(tmp, p.Name); err != nil {
		if old != "" {
			_ = r.Rename(old, p.Name)
		}
		return fail(fmt.Errorf("skills: move %s into place: %w", p.Name, err))
	}
	if old != "" {
		if err := r.RemoveAll(old); err != nil {
			im.a.Logger().Warn("skills: remove the files of a replaced skill", "skill", p.Name, "error", err)
		}
	}
	return nil
}

func randomSuffix() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
