package skills

// This file reads the marketplaces of the configuration
// (docs/ARCHITECTURE.md section 20.7): each one is a manifest of JSON that
// lists skills, or a repository whose skills directories each hold a
// SKILL.md. The front keeps the index of each marketplace for ten minutes.

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// Bounds of an index.
const (
	// IndexTTL is how long the front keeps the index of a marketplace.
	IndexTTL = 10 * time.Minute
	// MaxIndexSkills bounds the skills of one index.
	MaxIndexSkills = 200
	// headBytes bounds the part of a SKILL.md that an index reads for the
	// name and the description.
	headBytes = 16 << 10
)

// Kinds of a marketplace.
const (
	KindManifest   = "manifest"
	KindRepository = "repository"
)

// Marketplace is one marketplace of the configuration.
type Marketplace struct {
	Name string `yaml:"name" json:"name"`
	URL  string `yaml:"url" json:"url"`
}

// IndexEntry is one skill that a marketplace offers. A manifest names the
// URL of the skill. A repository names the directory of the skill, Path.
type IndexEntry struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	URL         string `json:"url,omitempty"`
	Path        string `json:"path,omitempty"`
}

// Index is a marketplace with the skills that it offers. Error holds the
// reason when the front cannot read the marketplace; the other
// marketplaces still list. Notes say what the index left out.
type Index struct {
	Name      string       `json:"name"`
	URL       string       `json:"url"`
	Kind      string       `json:"kind"`
	Skills    []IndexEntry `json:"skills"`
	Notes     []string     `json:"notes,omitempty"`
	Error     string       `json:"error,omitempty"`
	FetchedAt time.Time    `json:"fetchedAt"`

	repo repo
	tree tree
}

// Market keeps the index of each marketplace for IndexTTL.
type Market struct {
	list  []Marketplace
	fetch *Fetcher
	now   func() time.Time

	mu    sync.Mutex
	cache map[string]Index
}

// NewMarket returns the marketplaces of a list.
func NewMarket(list []Marketplace, f *Fetcher) *Market {
	return &Market{list: list, fetch: f, now: time.Now, cache: map[string]Index{}}
}

// List returns the index of every marketplace, in the order of the
// configuration.
func (m *Market) List(ctx context.Context) []Index {
	out := make([]Index, 0, len(m.list))
	for _, mp := range m.list {
		out = append(out, m.index(ctx, mp))
	}
	return out
}

// find returns a marketplace by name and its index.
func (m *Market) find(ctx context.Context, name string) (Marketplace, Index, error) {
	for _, mp := range m.list {
		if mp.Name == name {
			idx := m.index(ctx, mp)
			if idx.Error != "" {
				return mp, idx, fetchErr(502, "the marketplace %s: %s", name, idx.Error)
			}
			return mp, idx, nil
		}
	}
	return Marketplace{}, Index{}, fmt.Errorf("%w: no marketplace has the name %s", ErrNotFoundSkill, name)
}

// index returns the index of a marketplace from the cache, or reads it.
// A failed read stays in the cache for a minute, so a host that is down
// costs one fetch a minute.
func (m *Market) index(ctx context.Context, mp Marketplace) Index {
	key := mp.Name + "\x00" + mp.URL
	m.mu.Lock()
	idx, ok := m.cache[key]
	m.mu.Unlock()
	now := m.now()
	ttl := IndexTTL
	if idx.Error != "" {
		ttl = time.Minute
	}
	if ok && now.Sub(idx.FetchedAt) < ttl {
		return idx
	}
	idx = m.read(ctx, mp)
	idx.FetchedAt = now.UTC()
	if idx.Error != "" && ctx.Err() != nil {
		// The caller went away, which says nothing about the host.
		return idx
	}
	m.mu.Lock()
	m.cache[key] = idx
	m.mu.Unlock()
	return idx
}

// read reads the index of a marketplace.
func (m *Market) read(ctx context.Context, mp Marketplace) Index {
	idx := Index{Name: mp.Name, URL: mp.URL, Skills: []IndexEntry{}}
	u, err := checkURL(mp.URL)
	if err != nil {
		idx.Error = errText(err)
		return idx
	}
	if r, ok := m.fetch.repoOf(u); ok {
		idx.Kind = KindRepository
		if err := m.readRepo(ctx, r, &idx); err != nil {
			idx.Error = errText(err)
		}
		return idx
	}
	idx.Kind = KindManifest
	b, _, err := m.fetch.get(ctx, u.String())
	if err != nil {
		idx.Error = errText(err)
		return idx
	}
	entries, err := manifestOf(b)
	if err != nil {
		idx.Error = errText(err)
		return idx
	}
	seen := map[string]bool{}
	for _, e := range entries {
		e.Name = strings.TrimSpace(e.Name)
		switch {
		case e.Name == "" || fw.ValidSkillName(e.Name) != e.Name:
			idx.Notes = append(idx.Notes, fmt.Sprintf("left out the entry %q, whose name is no name of a skill", e.Name))
			continue
		case seen[e.Name]:
			idx.Notes = append(idx.Notes, "left out a second entry named "+e.Name)
			continue
		}
		if _, err := checkURL(e.URL); err != nil {
			idx.Notes = append(idx.Notes, "left out "+e.Name+", whose url is no http or https URL")
			continue
		}
		if len(idx.Skills) >= MaxIndexSkills {
			idx.Notes = append(idx.Notes, fmt.Sprintf("listed the first %d skills", MaxIndexSkills))
			break
		}
		seen[e.Name] = true
		e.Description = cut(strings.TrimSpace(e.Description), 300)
		e.Path = ""
		idx.Skills = append(idx.Skills, e)
	}
	return idx
}

// manifestOf reads a manifest: a list of {name, description, url}, or an
// object whose key skills holds that list. The manifest of a plugin
// marketplace, with the key plugins, lists plugins and not skills, so the
// answer says to name its repository instead.
func manifestOf(b []byte) ([]IndexEntry, error) {
	var list []IndexEntry
	if err := json.Unmarshal(b, &list); err == nil {
		return list, nil
	}
	var obj struct {
		Skills  []IndexEntry    `json:"skills"`
		Plugins json.RawMessage `json:"plugins"`
	}
	if err := json.Unmarshal(b, &obj); err != nil {
		return nil, fetchErr(502, "the manifest is not JSON: a list of {name, description, url}, or an object with skills")
	}
	if obj.Skills == nil && len(obj.Plugins) > 0 {
		return nil, fetchErr(400, "the manifest lists plugins, not skills; name the repository of the plugin marketplace as the url, and the front reads its skills directories")
	}
	return obj.Skills, nil
}

// readRepo lists the skills of a repository: each directory skills/<name>
// that holds a SKILL.md, at any depth, under the directory of the URL. So a
// plugin marketplace with plugins/<plugin>/skills/<name>/SKILL.md lists the
// skills of its plugins. The index reads the head of each SKILL.md for the
// name and the description.
func (m *Market) readRepo(ctx context.Context, r repo, idx *Index) error {
	t, err := m.fetch.list(ctx, r)
	if err != nil {
		return err
	}
	idx.repo, idx.tree = r, t
	var dirs []string
	for _, p := range t.paths {
		rel, ok := under(p, r.dir)
		if !ok || path.Base(rel) != fw.SkillFile {
			continue
		}
		dir := path.Dir(p)
		if path.Base(path.Dir(dir)) != "skills" {
			continue
		}
		hidden := false
		for _, part := range strings.Split(dir, "/") {
			hidden = hidden || strings.HasPrefix(part, ".")
		}
		if !hidden {
			dirs = append(dirs, dir)
		}
	}
	sort.Strings(dirs)
	seen := map[string]string{}
	for _, dir := range dirs {
		if len(idx.Skills) >= MaxIndexSkills {
			idx.Notes = append(idx.Notes, fmt.Sprintf("listed the first %d skills", MaxIndexSkills))
			break
		}
		b, err := m.fetch.read(ctx, r, t, dir+"/"+fw.SkillFile)
		if err != nil {
			idx.Notes = append(idx.Notes, "left out "+dir+": "+errText(err))
			continue
		}
		if len(b) > headBytes {
			b = b[:headBytes]
		}
		fm, _ := splitFrontMatter(string(b))
		kname, desc := kernelMeta(fm)
		name := fw.ValidSkillName(kname)
		if name == "" {
			name = fw.ValidSkillName(path.Base(dir))
		}
		if name == "" {
			continue
		}
		if first, dup := seen[name]; dup {
			idx.Notes = append(idx.Notes, fmt.Sprintf("left out %s, whose name %s is the name of %s", dir, name, first))
			continue
		}
		seen[name] = dir
		idx.Skills = append(idx.Skills, IndexEntry{Name: name, Description: strings.TrimSpace(desc), Path: dir})
	}
	if len(dirs) == 0 {
		idx.Notes = append(idx.Notes, "the repository holds no skills/<name>/SKILL.md")
	}
	// The cache keeps the text of an archive for the files of its skills
	// alone.
	if t.text != nil {
		keep := map[string][]byte{}
		for _, e := range idx.Skills {
			for p, b := range t.text {
				if _, ok := under(p, e.Path); ok {
					keep[p] = b
				}
			}
		}
		idx.tree.text = keep
	}
	return nil
}

// errText is the words of an error for an index.
func errText(err error) string {
	return strings.TrimPrefix(err.Error(), "skills: ")
}

// CheckMarketplaces checks the marketplaces of a configuration: each has a
// name of letters, digits, dashes, underscores, and dots, a URL of http or
// https without a user name or a password, and a name of its own.
func CheckMarketplaces(list []Marketplace) error {
	seen := map[string]bool{}
	for i, mp := range list {
		name := strings.TrimSpace(mp.Name)
		switch {
		case name == "" || fw.ValidSkillName(name) != name:
			return fmt.Errorf("marketplaces[%d]: the name %q is not letters, digits, dashes, underscores, and dots", i, mp.Name)
		case seen[name]:
			return fmt.Errorf("marketplaces[%d]: the name %s is the name of an earlier marketplace", i, name)
		}
		seen[name] = true
		if _, err := checkURL(mp.URL); err != nil {
			return fmt.Errorf("marketplaces[%d] (%s): %s", i, name, errText(err))
		}
	}
	return nil
}

// ParseMarketplaces reads the marketplaces of a variable:
// name=url,name=url.
func ParseMarketplaces(v string) ([]Marketplace, error) {
	var out []Marketplace
	for _, part := range strings.Split(v, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		name, u, ok := strings.Cut(part, "=")
		if !ok {
			return nil, fmt.Errorf("%q is not name=url", part)
		}
		out = append(out, Marketplace{Name: strings.TrimSpace(name), URL: strings.TrimSpace(u)})
	}
	return out, nil
}

// Fetcher returns the fetcher of the importer, so a test reaches a server
// of its own.
func (im *Importer) Fetcher() *Fetcher { return im.fetch }
