package pools

// This file ships the local skills of the front to the workers of a pool
// (docs/ARCHITECTURE.md section 14.10). A skill is a directory with a
// SKILL.md file under the skills directories of the front. The merged
// render of a pool (WithOverlay) adds each skill that a profile of the pool
// allows to the Workspace of each worker, under the skills directory that
// the kernel reads in a Task, $FW_HOME/skills, which is /workspace/.fw/skills
// under ax. The hash of the pool covers the files, so a changed skill
// replaces the Tasks of the pool as a change of the overlay does. The
// skills belong to the front, as the overlay does, so the hash of the pool
// without its overlay leaves them out, and a changed skill raises no
// conflict of the generation.

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/bdubs00/featherweight-kernel/ax"
	"github.com/bdubs00/featherweight-kernel/fw"
)

// Bounds of the skills that a pool ships.
const (
	// MaxSkillBytes bounds the bytes of every skill file of one pool.
	MaxSkillBytes = 1 << 20
	// MaxSkillFiles bounds the files of one skill, SKILL.md included.
	MaxSkillFiles = 64
)

// SkillsPath is where the skills of the front land in the Workspace of a
// worker, relative to WorkspacePath: the skills directory under the home
// of the kernel in a Task.
const SkillsPath = ".fw/skills"

// taskHome is the home of the kernel in a Task.
const taskHome = WorkspacePath + "/.fw"

// ErrSkills means that the skills that a pool ships do not fit their
// bounds, or that the front cannot read them.
var ErrSkills = errors.New("pools: the skills of the pool do not fit")

// Skills returns the names of the skills of the front that the pool ships,
// sorted, after WithOverlay.
func (p *Pool) Skills() []string { return slices.Clone(p.skillNames) }

// shipSkills reads the skills of the front that a profile of the pool
// allows and sets them as files of the Workspace. A profile without a
// skills entry allows every skill, and an empty list allows none, as the
// kernel reads it (fw.SkillAllowed). It leaves out a hidden file, a
// symbolic link, and a file that is not text, and it names each one that
// it leaves out. A skill of more than MaxSkillFiles files, and skills of
// more than MaxSkillBytes bytes in all, are an error that wraps ErrSkills.
func (p *Pool) shipSkills() error {
	p.skillFiles, p.skillNames, p.skillSkipped = nil, nil, nil
	if len(p.front.SkillDirs) == 0 {
		return nil
	}
	// The kernel serves the skills that it can read, and so does the
	// front; a directory that it cannot read ships nothing.
	all, _ := fw.LoadSkills(p.front.SkillDirs...)
	profiles := p.Profiles()
	allowed := func(name string) bool {
		for _, prof := range profiles {
			if fw.SkillAllowed(prof.Skills, name) {
				return true
			}
		}
		return false
	}
	taken := map[string]bool{}
	for _, f := range p.Files {
		taken[f.Path] = true
	}
	total := 0
	for _, s := range all {
		if !allowed(s.Name) {
			continue
		}
		files, skipped, err := readSkill(s)
		if err != nil {
			return fmt.Errorf("%w: %s cannot read the skill %s of the front: %v", ErrSkills, p.key(), s.Name, err)
		}
		if len(files) > MaxSkillFiles {
			return fmt.Errorf("%w: %s: the skill %s holds %d files, and a pool ships at most %d files of one skill; make the skill smaller, or leave it out of the skills of the profiles of the pool", ErrSkills, p.key(), s.Name, len(files), MaxSkillFiles)
		}
		for _, f := range files {
			total += len(f.Content)
			if total > MaxSkillBytes {
				return fmt.Errorf("%w: %s: the skills that the profiles of the pool allow hold more than %d bytes, the most that a pool ships; narrow the skills of the profiles, or make the skills smaller", ErrSkills, p.key(), MaxSkillBytes)
			}
			f.Path = SkillsPath + "/" + s.Name + "/" + f.Path
			if taken[f.Path] {
				continue // a file of the pool of that path wins
			}
			p.skillFiles = append(p.skillFiles, f)
		}
		for _, sk := range skipped {
			p.skillSkipped = append(p.skillSkipped, s.Name+"/"+sk)
		}
		p.skillNames = append(p.skillNames, s.Name)
	}
	if len(p.skillFiles) > 0 {
		p.FW = withSkillsDir(p.FW)
	}
	return nil
}

// readSkill reads the files of a skill, SKILL.md first and then the others
// by path, relative to the directory of the skill. It opens the directory
// as an os.Root, so no file leads outside it, and it leaves out a hidden
// file or directory, a symbolic link, a file that is not regular, and a
// file that is not UTF-8 text, which it names in skipped. It stops reading
// past MaxSkillFiles files.
func readSkill(s fw.Skill) (files []ax.File, skipped []string, err error) {
	root, err := os.OpenRoot(s.Dir)
	if err != nil {
		return nil, nil, err
	}
	defer root.Close()
	fsys := root.FS()
	var paths []string
	err = fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == "." {
			return nil
		}
		if strings.HasPrefix(path.Base(p), ".") {
			if d.IsDir() {
				return fs.SkipDir
			}
			skipped = append(skipped, p)
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() {
			skipped = append(skipped, p)
			return nil
		}
		paths = append(paths, p)
		if len(paths) > MaxSkillFiles {
			return fs.SkipAll
		}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	sort.SliceStable(paths, func(i, j int) bool {
		if (paths[i] == fw.SkillFile) != (paths[j] == fw.SkillFile) {
			return paths[i] == fw.SkillFile
		}
		return paths[i] < paths[j]
	})
	for _, p := range paths {
		if len(files) >= MaxSkillFiles {
			files = append(files, ax.File{Path: p})
			continue // the count alone matters past the bound
		}
		b, err := fs.ReadFile(fsys, p)
		if err != nil {
			return nil, nil, err
		}
		if !utf8.Valid(b) {
			skipped = append(skipped, p)
			continue
		}
		files = append(files, ax.File{Path: filepath.ToSlash(p), Content: string(b)})
	}
	return files, skipped, nil
}

// withSkillsDir returns the block fw with the skills directory of the
// Workspace among the skills directories of the worker. A worker without
// tools.skills and without a home of its own reads that directory already,
// as the home of the kernel in a Task. Neither argument changes.
func withSkillsDir(base map[string]any) map[string]any {
	dir := WorkspacePath + "/" + SkillsPath
	tools, _ := base["tools"].(map[string]any)
	list := stringList(tools["skills"])
	home, _ := base["home"].(string)
	switch {
	case len(list) > 0:
		if slices.Contains(list, dir) {
			return base
		}
		list = append(list, dir)
	case strings.TrimSpace(home) != "" && path.Clean(strings.TrimSpace(home)) != taskHome:
		list = []string{path.Clean(strings.TrimSpace(home)) + "/skills", dir}
	default:
		return base
	}
	out := make(map[string]any, len(base))
	for k, v := range base {
		out[k] = v
	}
	t := map[string]any{}
	for k, v := range tools {
		t[k] = v
	}
	t["skills"] = list
	out["tools"] = t
	return out
}
