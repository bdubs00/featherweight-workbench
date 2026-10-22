package bossmode

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/png"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/surface/native"
)

// writeFile writes a file into a new directory and returns its path.
func writeFile(t *testing.T, name string, body []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, body, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// pngOf is a PNG of one pixel.
func pngOf(t *testing.T) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := png.Encode(&b, image.NewRGBA(image.Rect(0, 0, 1, 1))); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// The theme of the deployment reaches the settings, and the route of the
// logo serves the SVG that the sanitizer wrote, with its content type and
// its ETag, to a caller with a credential.
func TestTheme(t *testing.T) {
	svg := `<?xml version="1.0" encoding="UTF-8"?><!-- the logo --><svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 8 8"><rect width="8" height="8" fill="#0A7"/></svg>`
	t.Setenv(EnvThemeAccent, "#0a7")
	t.Setenv(EnvThemeName, "Example Ltd")
	t.Setenv(EnvThemeLogo, writeFile(t, "logo.svg", []byte(svg)))
	fr := newFront(t, nil)
	var got Settings
	resp, body := get(t, fr.url+"/v1/bossmode", "ada")
	if resp.StatusCode != http.StatusOK || json.Unmarshal([]byte(body), &got) != nil {
		t.Fatalf("settings: %d %s", resp.StatusCode, body)
	}
	if got.Theme != (PageTheme{Accent: "#0a7", Name: "Example Ltd", Logo: LogoPath}) {
		t.Errorf("theme = %+v", got.Theme)
	}
	resp, body = get(t, fr.url+LogoPath, "ada")
	etag := resp.Header.Get("ETag")
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "image/svg+xml" || !strings.HasPrefix(etag, `"`) || len(etag) != 66 {
		t.Fatalf("logo: %d %v", resp.StatusCode, resp.Header)
	}
	if !strings.HasPrefix(body, "<svg") || strings.Contains(body, "the logo") || strings.Contains(body, "<?xml") {
		t.Errorf("the logo is not what the sanitizer wrote: %s", body)
	}
	if resp.Header.Get("Content-Security-Policy") != logoPolicy || resp.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("logo headers: %v", resp.Header)
	}
	if resp, _ := get(t, fr.url+LogoPath, "ada", "If-None-Match", etag); resp.StatusCode != http.StatusNotModified {
		t.Errorf("logo with its ETag: %d", resp.StatusCode)
	}
	if resp, _ := get(t, fr.url+LogoPath, ""); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("logo without a token: %d", resp.StatusCode)
	}
}

// A PNG logo goes out as it is, with image/png.
func TestThemePNG(t *testing.T) {
	data := pngOf(t)
	t.Setenv(EnvThemeLogo, writeFile(t, "logo.PNG", data))
	fr := newFront(t, nil)
	resp, body := get(t, fr.url+LogoPath, "ada")
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "image/png" || body != string(data) {
		t.Fatalf("logo: %d %v", resp.StatusCode, resp.Header)
	}
	_, body = get(t, fr.url+"/v1/bossmode", "ada")
	if !strings.Contains(body, `"theme":{"accent":"#FF4D00","name":"","logo":"/v1/bossmode/logo"}`) {
		t.Errorf("settings = %s", body)
	}
}

// Without a theme, the settings carry the defaults, and the route of the
// logo answers 404.
func TestThemeDefaults(t *testing.T) {
	fr := newFront(t, nil)
	resp, body := get(t, fr.url+LogoPath, "ada")
	if resp.StatusCode != http.StatusNotFound || !strings.Contains(body, "workbench.bossmode.theme.logo") {
		t.Errorf("logo without a theme: %d %s", resp.StatusCode, body)
	}
}

// A logo that does not pass stops the start with the reason: a missing
// file, a file of another kind, a file above 256 KB, an SVG that the
// sanitizer refuses, and a PNG that does not decode. An accent that is no
// color and a name that is too long stop the load.
func TestThemeStops(t *testing.T) {
	big := append([]byte(`<svg xmlns="http://www.w3.org/2000/svg">`), bytes.Repeat([]byte(" "), MaxLogoBytes)...)
	for name, c := range map[string]struct {
		file, body, want string
	}{
		"missing":   {"", "", "no such file"},
		"kind":      {"logo.gif", "GIF89a", "neither an SVG nor a PNG"},
		"too large": {"logo.svg", string(big), "at most 256 KB"},
		"script":    {"logo.svg", `<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`, "script"},
		"png":       {"logo.png", "not a png", "no PNG image"},
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "logo.svg")
			if c.file != "" {
				path = writeFile(t, c.file, []byte(c.body))
			}
			t.Setenv(EnvThemeLogo, path)
			err := startErr(t)
			if err == nil || !strings.Contains(err.Error(), c.want) || !strings.Contains(err.Error(), "workbench.bossmode.theme.logo") {
				t.Errorf("start = %v, want %q", err, c.want)
			}
		})
	}
	t.Setenv(EnvThemeLogo, "")
	for _, accent := range []string{"red", "#12345", "#0a7 ", "#GGGGGG", "#123456789"} {
		t.Setenv(EnvThemeAccent, accent)
		if accent == "#0a7 " {
			// A space around the value goes; the color stays.
			if err := startErr(t); err != nil {
				t.Errorf("accent %q: %v", accent, err)
			}
			continue
		}
		if err := startErr(t); err == nil || !strings.Contains(err.Error(), "workbench.bossmode.theme.accent") {
			t.Errorf("accent %q: %v", accent, err)
		}
	}
	t.Setenv(EnvThemeAccent, "")
	t.Setenv(EnvThemeName, strings.Repeat("é", MaxThemeName+1))
	if err := startErr(t); err == nil || !strings.Contains(err.Error(), "at most 64") {
		t.Errorf("a long name: %v", err)
	}
	t.Setenv(EnvThemeName, strings.Repeat("é", MaxThemeName))
	if err := startErr(t); err != nil {
		t.Errorf("a name of 64 characters: %v", err)
	}
	cfg := withBlock(t, "bossmode: {enabled: true, theme: {name: \"two\\nlines\"}}\n")
	t.Setenv(EnvThemeName, "")
	if _, err := Load(cfg); err == nil || !strings.Contains(err.Error(), "one line") {
		t.Errorf("a name of two lines: %v", err)
	}
}

// startErr starts the service of bossmode on a new agent with bossmode on
// and returns the error of the start.
func startErr(t *testing.T) error {
	t.Helper()
	t.Setenv(EnvEnabled, "on")
	cfg := fw.DefaultConfig()
	cfg.Home = t.TempDir()
	a, err := fw.New(fw.WithConfig(cfg), fw.WithoutDefaults(), fw.WithAuthn(people{}),
		fw.WithLedger(fw.NewMemLedger()), fw.WithBus(fw.NewMemBus()), fw.WithSurfaces(native.New()),
		fw.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		a.Close(ctx)
	})
	cl, err := Start(context.Background(), a)
	if cl != nil {
		_ = cl.Close()
	}
	return err
}

// The doctor line names the theme and fails for a logo that does not
// pass.
func TestDoctorTheme(t *testing.T) {
	ctx := context.Background()
	t.Setenv(EnvThemeAccent, "")
	t.Setenv(EnvThemeName, "")
	t.Setenv(EnvThemeLogo, "")
	logo := writeFile(t, "logo.png", pngOf(t))
	ok, line := DoctorCheck(ctx, withBlock(t, "bossmode: {enabled: true, theme: {accent: \"#123456\", name: Example, logo: "+logo+"}}\n"), nil, 0)
	if !strings.Contains(line, `the theme has the accent #123456, the name "Example", and the logo `+logo+" (image/png,") {
		t.Errorf("theme: %v %s", ok, line)
	}
	bad := writeFile(t, "logo.svg", []byte(`<svg xmlns="http://www.w3.org/2000/svg" onload="x()"/>`))
	if ok, line := DoctorCheck(ctx, withBlock(t, "bossmode: {enabled: true, theme: {logo: "+bad+"}}\n"), nil, 0); ok || !strings.Contains(line, "sanitizer") {
		t.Errorf("a logo that fails: %v %s", ok, line)
	}
	if _, line := DoctorCheck(ctx, withBlock(t, "bossmode: {enabled: true}\n"), nil, 0); strings.Contains(line, "theme") {
		t.Errorf("without a theme: %s", line)
	}
}
