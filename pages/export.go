package pages

// This file copies the wiki to files and back, for the verbs pages export
// and pages import of fw-workbench (docs/ARCHITECTURE.md section 15.5).
// Both go through fw.DirFiles of the kernel, so a path never leads outside
// the directory, through a symbolic link either.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"strings"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// Exported says what Export wrote: the pages, and whether it wrote the
// index too.
type Exported struct {
	Pages int  `json:"pages"`
	Index bool `json:"index"`
}

// Export writes each page of the wiki as one file under dir, at its path,
// and the index as _index.md. It replaces a file of the same path and
// leaves every other file of dir as it is.
func Export(ctx context.Context, s *Store, dir string) (Exported, error) {
	var out Exported
	files, err := fw.DirFiles(dir)
	if err != nil {
		return out, fmt.Errorf("pages: export to %s: %w", dir, err)
	}
	infos, err := s.List(ctx)
	if err != nil {
		return out, err
	}
	for _, info := range infos {
		page, err := s.Read(ctx, info.Path, 0)
		if errors.Is(err, ErrNotFound) {
			continue // removed since the list
		}
		if err != nil {
			return out, err
		}
		if err := files.WriteFile(page.Path, []byte(page.Text)); err != nil {
			return out, fmt.Errorf("pages: export %s: %w", page.Path, err)
		}
		out.Pages++
	}
	index, err := s.Read(ctx, IndexPath, 0)
	switch {
	case err == nil:
		if err := files.WriteFile(IndexPath, []byte(index.Text)); err != nil {
			return out, fmt.Errorf("pages: export %s: %w", IndexPath, err)
		}
		out.Index = true
	case !errors.Is(err, ErrNotFound):
		return out, err
	}
	return out, nil
}

// Imported says what Import did: the pages that it wrote, the pages that
// held the text of their file already, and the files that it left out,
// each with the reason.
type Imported struct {
	Written   int      `json:"written"`
	Unchanged int      `json:"unchanged"`
	Skipped   []string `json:"skipped,omitempty"`
}

// Import writes each file under dir whose name ends with .md as a new
// version of the page at its path, by the writer by, with no condition. It
// leaves out _index.md, which the store writes, a directory or a file
// whose name starts with a dot, a symbolic link, a file that is no page
// path, and a file that the store refuses, and says why for each. A file
// that holds the text of its page already changes nothing.
func Import(ctx context.Context, s *Store, dir, by string) (Imported, error) {
	var out Imported
	files, err := fw.DirFiles(dir)
	if err != nil {
		return out, fmt.Errorf("pages: import from %s: %w", dir, err)
	}
	if _, err := files.Stat("."); err != nil {
		return out, fmt.Errorf("pages: import from %s: %w", dir, err)
	}
	summary := "import from " + path.Base(strings.TrimRight(dir, "/"))
	skip := func(name, why string) { out.Skipped = append(out.Skipped, name+": "+why) }
	err = fs.WalkDir(files, ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if name == "." {
			return nil
		}
		if strings.HasPrefix(d.Name(), ".") {
			if d.IsDir() {
				return fs.SkipDir
			}
			skip(name, "a hidden file")
			return nil
		}
		switch {
		case d.IsDir():
			return nil
		case d.Type()&fs.ModeSymlink != 0:
			skip(name, "a symbolic link")
			return nil
		case !d.Type().IsRegular():
			skip(name, "not a regular file")
			return nil
		case !strings.HasSuffix(name, ".md"):
			skip(name, "not a page, because its name does not end with .md")
			return nil
		case name == IndexPath:
			skip(name, "the index, which the store writes")
			return nil
		}
		if err := CheckPath(name); err != nil {
			skip(name, Reason(err))
			return nil
		}
		text, err := readBounded(files, name, s.cfg.MaxPageBytes)
		if err != nil {
			skip(name, err.Error())
			return nil
		}
		_, did, err := s.write(ctx, name, text, by, "", summary, IfAny)
		switch {
		case err == nil && did == unchanged:
			out.Unchanged++
		case err == nil:
			out.Written++
		case errors.Is(err, ErrInvalid), errors.Is(err, ErrLimit), errors.Is(err, ErrConflict):
			skip(name, Reason(err))
		default:
			return err
		}
		return nil
	})
	if err != nil {
		return out, fmt.Errorf("pages: import from %s: %w", dir, err)
	}
	return out, nil
}

// readBounded reads a file of at most limit bytes.
func readBounded(files fw.Files, name string, limit int) (string, error) {
	f, err := files.Open(name)
	if err != nil {
		return "", err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, int64(limit)+1))
	if err != nil {
		return "", err
	}
	if len(data) > limit {
		return "", fmt.Errorf("a page holds at most %d KB, and the file has more", limit>>10)
	}
	return string(data), nil
}
