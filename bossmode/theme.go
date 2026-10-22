package bossmode

// This file holds the theme of the page for one deployment
// (docs/ARCHITECTURE.md sections 17.1 and 17.4): the accent, the name of
// the organization, and the logo, which the settings of the page carry,
// and the route GET /v1/bossmode/logo, which serves the logo. The service
// reads the logo once at its start; a logo that does not pass stops the
// start with the reason.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"image/png"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/surface/native"
	"github.com/bdubs00/featherweight-workbench/faces"
)

// LogoPath is the route of the logo of the theme.
const LogoPath = "/v1/bossmode/logo"

// logoPolicy is the content security policy of the logo, as of a face: a
// logo that a browser opens on its own loads nothing and runs nothing.
const logoPolicy = "default-src 'none'; style-src 'unsafe-inline'; sandbox"

// PageTheme is the theme in the settings of the page. Accent is the color
// of the theme, DefaultAccent without one. Name is the name of the
// organization, or "". Logo is LogoPath when the theme has a logo, else "".
type PageTheme struct {
	Accent string `json:"accent"`
	Name   string `json:"name"`
	Logo   string `json:"logo"`
}

// logo is the file of the logo as the route serves it: the body, which is
// the SVG that the sanitizer wrote or the PNG as it is, its content type,
// and its ETag, the SHA-256 of the body in quotes.
type logo struct {
	body  []byte
	ctype string
	etag  string
}

// readLogo reads the logo of a theme: a file that ends with .svg or .png,
// in any case, of at most MaxLogoBytes. An SVG must pass the sanitizer of
// the faces (faces.Sanitize), and the route serves what the sanitizer
// wrote. A PNG must decode as a PNG. It returns nil for a theme without a
// logo, and an error that names the key and the reason for a file that
// does not pass.
func readLogo(path string) (*logo, error) {
	if path == "" {
		return nil, nil
	}
	key := fmt.Sprintf("workbench.bossmode.theme.logo (%s) %q", EnvThemeLogo, path)
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("bossmode: %s: %w", key, err)
	}
	defer f.Close()
	if st, err := f.Stat(); err != nil {
		return nil, fmt.Errorf("bossmode: %s: %w", key, err)
	} else if !st.Mode().IsRegular() {
		return nil, fmt.Errorf("bossmode: %s is no file; name an SVG or a PNG file", key)
	}
	b, err := io.ReadAll(io.LimitReader(f, MaxLogoBytes+1))
	if err != nil {
		return nil, fmt.Errorf("bossmode: %s: %w", key, err)
	}
	if len(b) > MaxLogoBytes {
		return nil, fmt.Errorf("bossmode: %s holds more than %d KB; a logo holds at most %d KB", key, MaxLogoBytes>>10, MaxLogoBytes>>10)
	}
	lg := &logo{}
	switch strings.ToLower(filepath.Ext(path)) {
	case ".svg":
		clean, err := faces.Sanitize(b)
		if err != nil {
			return nil, fmt.Errorf("bossmode: %s: an SVG logo must pass the sanitizer of the faces: %w", key, err)
		}
		lg.body, lg.ctype = clean, "image/svg+xml"
	case ".png":
		if _, err := png.DecodeConfig(bytes.NewReader(b)); err != nil {
			return nil, fmt.Errorf("bossmode: %s is no PNG image: %w", key, err)
		}
		lg.body, lg.ctype = b, "image/png"
	default:
		return nil, fmt.Errorf("bossmode: %s is neither an SVG nor a PNG file; name a file that ends with .svg or .png", key)
	}
	sum := sha256.Sum256(lg.body)
	lg.etag = strconv.Quote(hex.EncodeToString(sum[:]))
	return lg, nil
}

// pageTheme is the theme of the settings: the accent and the name of the
// configuration, the default accent without one, and the route of the
// logo when the hub of the agent holds one.
func pageTheme(a *fw.Agent, t Theme) PageTheme {
	out := PageTheme{Accent: t.Accent, Name: t.Name}
	if out.Accent == "" {
		out.Accent = DefaultAccent
	}
	if h := Of(a); h != nil && h.logo != nil {
		out.Logo = LogoPath
	}
	return out
}

// logoRoute answers GET /v1/bossmode/logo with the logo of the theme, its
// content type, and its ETag. If-None-Match with the ETag answers 304. A
// theme without a logo answers 404.
func logoRoute(a *fw.Agent) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var lg *logo
		if h := Of(a); h != nil {
			lg = h.logo
		}
		if lg == nil {
			native.WriteProblem(w, r, http.StatusNotFound, "bossmode: the theme has no logo; set workbench.bossmode.theme.logo ("+EnvThemeLogo+")")
			return
		}
		h := w.Header()
		h.Set("Content-Type", lg.ctype)
		h.Set("ETag", lg.etag)
		h.Set("Cache-Control", "private, no-cache")
		h.Set("Content-Security-Policy", logoPolicy)
		h.Set("X-Content-Type-Options", "nosniff")
		http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(lg.body))
	}
}

// themeLine is the part of the doctor line about the theme, "" for a
// theme without a key. ok is false for a logo that does not pass, which
// stops the start of the server.
func themeLine(t Theme) (line string, ok bool) {
	if t == (Theme{}) {
		return "", true
	}
	var parts []string
	if t.Accent != "" {
		parts = append(parts, "the accent "+t.Accent)
	}
	if t.Name != "" {
		parts = append(parts, fmt.Sprintf("the name %q", t.Name))
	}
	if t.Logo != "" {
		lg, err := readLogo(t.Logo)
		if err != nil {
			return strings.TrimPrefix(err.Error(), "bossmode: "), false
		}
		parts = append(parts, fmt.Sprintf("the logo %s (%s, %d bytes)", t.Logo, lg.ctype, len(lg.body)))
	}
	return "the theme has " + joinAnd(parts), true
}

// joinAnd joins words with commas and a last "and".
func joinAnd(parts []string) string {
	switch len(parts) {
	case 0:
		return ""
	case 1:
		return parts[0]
	case 2:
		return parts[0] + " and " + parts[1]
	}
	return strings.Join(parts[:len(parts)-1], ", ") + ", and " + parts[len(parts)-1]
}
