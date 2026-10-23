package mcp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/surface/native"
	"github.com/bdubs00/featherweight-kernel/surface/view"
	"github.com/bdubs00/featherweight-workbench/fleet"
)

func TestRunFileTool(t *testing.T) {
	ctx := testCtx(t)
	ws, err := fw.DirFiles(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	png := "\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR"
	writer := fw.NewTool(fw.ToolSpec{Name: "writer"}, func(ctx context.Context, _ json.RawMessage) (fw.ToolResult, error) {
		for name, data := range map[string]string{"notes/plan.md": "# Plan\n", "dot.png": png} {
			if err := fw.FilesFrom(ctx).WriteFile(name, []byte(data)); err != nil {
				return fw.ErrorResult(err.Error()), nil
			}
		}
		return fw.TextResult("written"), nil
	})
	a := manageAgent(t, newScript(toolUse("c1", "writer", `{}`), endTurn("done")).model(), fw.WithFiles(ws), fw.WithTools(writer))
	srv := serveAgent(t, a, nil)
	r, err := a.Run(fw.WithPrincipal(ctx, people["alice-token"]), "write", fw.AsAgent("reviewer"))
	if err != nil || r.State != fw.StateCompleted {
		t.Fatalf("run = %+v, %v", r, err)
	}
	bob := as(t, ctx, srv.URL, "bob-token")

	res := callTool(t, ctx, bob, toolRunFile, map[string]any{"runId": r.ID, "path": "notes/plan.md"})
	var v view.FileView
	structured(t, res, &v)
	if res.IsError || v.Text != "# Plan\n" || v.Op != fw.FileCreated || !strings.Contains(textOf(res), "# Plan") {
		t.Fatalf("run_file = %+v, text %q", v, textOf(res))
	}
	res = callTool(t, ctx, bob, toolRunFile, map[string]any{"runId": r.ID, "path": "dot.png"})
	structured(t, res, &v)
	var img *sdk.ImageContent
	for _, c := range res.Content {
		if ic, ok := c.(*sdk.ImageContent); ok {
			img = ic
		}
	}
	if res.IsError || string(v.Data) != png || img == nil || img.MIMEType != "image/png" || string(img.Data) != png {
		t.Fatalf("image result = %+v, %+v", v, res.Content)
	}
	res = callTool(t, ctx, bob, toolRunFile, map[string]any{"runId": r.ID, "path": "../etc/passwd"})
	if !res.IsError || !strings.Contains(textOf(res), "did not change the file") {
		t.Fatalf("outside path = %q", textOf(res))
	}
}

func TestRunFileReachesTheOwner(t *testing.T) {
	ctx := testCtx(t)
	ws, err := fw.DirFiles(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	writer := fw.NewTool(fw.ToolSpec{Name: "writer"}, func(ctx context.Context, _ json.RawMessage) (fw.ToolResult, error) {
		return fw.TextResult("written"), fw.FilesFrom(ctx).WriteFile("docs/a b.md", []byte("remote text"))
	})
	owner := tokenOwner(t, newScript(toolUse("c1", "writer", `{}`), endTurn("done")).model(),
		fw.WithFiles(ws), fw.WithTools(writer), fw.WithSurfaces(native.New()))
	h, err := owner.Handler()
	if err != nil {
		t.Fatal(err)
	}
	ownerSrv := httptest.NewServer(h)
	t.Cleanup(ownerSrv.Close)
	r, err := owner.Run(fw.WithPrincipal(ctx, people["alice-token"]), "write", fw.AsAgent("reviewer"))
	if err != nil || r.State != fw.StateCompleted {
		t.Fatalf("owner run = %+v, %v", r, err)
	}
	fake := &fakeFleet{name: "viewer", owner: ownerSrv.URL, members: []fleet.Member{
		{Name: "viewer", Self: true, State: fleet.MemberLive, AccessMode: fw.AccessModeRules},
		{Name: "owner", State: fleet.MemberLive, CanForward: true, AccessMode: fw.AccessModeRules},
	}}
	fake.set(r)
	viewer := attach(manageAgent(t, newScript().model()), fake)
	fake.rules = viewer
	cs := as(t, ctx, serveAgent(t, viewer, nil).URL, "bob-token")
	res := callTool(t, ctx, cs, toolRunFile, map[string]any{"runId": r.ID, "path": "docs/a b.md"})
	var v view.FileView
	structured(t, res, &v)
	if res.IsError || v.Text != "remote text" || v.Path != "docs/a b.md" {
		t.Fatalf("remote run_file = %+v, %q", v, textOf(res))
	}
	if h := fake.lastForward(); h.Get("Accept") != "application/json" || h.Get("Authorization") != "Bearer fleet-token" || h.Get(fleet.HeaderOnBehalfOf) != "bob" {
		t.Errorf("forwarded headers = %v", h)
	}
}

// embedded returns the embedded resource of a result, or nil.
func embedded(res *sdk.CallToolResult) *sdk.ResourceContents {
	for _, c := range res.Content {
		if er, ok := c.(*sdk.EmbeddedResource); ok {
			return er.Resource
		}
	}
	return nil
}

// fileAgent is an agent whose run of the reviewer agent writes the files,
// and that run.
func fileAgent(t *testing.T, ctx context.Context, files map[string][]byte, opts ...fw.Option) (*fw.Agent, *fw.Run) {
	t.Helper()
	ws, err := fw.DirFiles(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	writer := fw.NewTool(fw.ToolSpec{Name: "writer"}, func(ctx context.Context, _ json.RawMessage) (fw.ToolResult, error) {
		for name, data := range files {
			if err := fw.FilesFrom(ctx).WriteFile(name, data); err != nil {
				return fw.ErrorResult(err.Error()), nil
			}
		}
		return fw.TextResult("written"), nil
	})
	// A second run, such as a run of the agent shared, writes the files
	// again.
	script := newScript(toolUse("c1", "writer", `{}`), endTurn("done"), toolUse("c2", "writer", `{}`), endTurn("done"))
	a := manageAgent(t, script.model(), append([]fw.Option{fw.WithFiles(ws), fw.WithTools(writer)}, opts...)...)
	r, err := a.Run(fw.WithPrincipal(ctx, people["alice-token"]), "write", fw.AsAgent("reviewer"))
	if err != nil || r.State != fw.StateCompleted {
		t.Fatalf("run = %+v, %v", r, err)
	}
	return a, r
}

func TestRunFileRaw(t *testing.T) {
	ctx := testCtx(t)
	png := []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR")
	big := bytes.Repeat([]byte("x"), maxRawBytes+1)
	a, r := fileAgent(t, ctx, map[string][]byte{"notes/plan.md": []byte("# Plan\n"), "dot.png": png, "big.txt": big, "empty.txt": {}})
	url := serveAgent(t, a, nil).URL
	bob := as(t, ctx, url, "bob-token")

	for name, data := range map[string][]byte{"dot.png": png, "notes/plan.md": []byte("# Plan\n"), "empty.txt": {}} {
		res := callTool(t, ctx, bob, toolRunFile, map[string]any{"runId": r.ID, "path": name, "raw": true})
		if res.IsError {
			t.Fatalf("raw %s = %s", name, textOf(res))
		}
		sum := sha256.Sum256(data)
		hexSum := hex.EncodeToString(sum[:])
		c := embedded(res)
		wantType := view.MediaType(data)
		if c == nil || !bytes.Equal(c.Blob, data) || c.MIMEType != wantType || c.Text != "" || c.URI != fileURI(r.ID, name) {
			t.Fatalf("raw %s resource = %+v", name, c)
		}
		var got rawFile
		structured(t, res, &got)
		if got.Path != name || got.Size != int64(len(data)) || got.SHA256 != hexSum || got.MediaType != wantType || got.Op != fw.FileCreated || got.Tool != "writer" || got.Changed {
			t.Fatalf("raw %s structured = %+v", name, got)
		}
		txt := textOf(res)
		if !strings.Contains(txt, name) || !strings.Contains(txt, hexSum) || !strings.Contains(txt, fmt.Sprintf("%d bytes", len(data))) {
			t.Errorf("raw %s text = %q", name, txt)
		}
	}
	if c := embedded(callTool(t, ctx, bob, toolRunFile, map[string]any{"runId": r.ID, "path": "dot.png"})); c != nil {
		t.Errorf("run_file without raw embeds the bytes: %+v", c)
	}

	// A file above the bound names its size and the bound.
	res := callTool(t, ctx, bob, toolRunFile, map[string]any{"runId": r.ID, "path": "big.txt", "raw": true})
	if txt := textOf(res); !res.IsError || !strings.Contains(txt, fmt.Sprintf("%d bytes", len(big))) || !strings.Contains(txt, fmt.Sprintf("%d bytes (4 MiB)", maxRawBytes)) || embedded(res) != nil {
		t.Fatalf("big raw = %q", txt)
	}
	// The reader rules of run_file hold: only a path that the run changed.
	for _, path := range []string{"../etc/passwd", "notes", "other.md"} {
		res = callTool(t, ctx, bob, toolRunFile, map[string]any{"runId": r.ID, "path": path, "raw": true})
		if !res.IsError || !strings.Contains(textOf(res), "did not change the file") || embedded(res) != nil {
			t.Errorf("raw %s = %q", path, textOf(res))
		}
	}
	res = callTool(t, ctx, bob, toolRunFile, map[string]any{"runId": "nope", "path": "dot.png", "raw": true})
	if !res.IsError || !strings.Contains(textOf(res), "No run has that id") {
		t.Errorf("unknown run = %q", textOf(res))
	}
	// A file that changed after the run says so.
	if err := a.Files().(interface {
		WriteFile(string, []byte) error
	}).WriteFile("notes/plan.md", []byte("# Plan 2\n")); err != nil {
		t.Fatal(err)
	}
	res = callTool(t, ctx, bob, toolRunFile, map[string]any{"runId": r.ID, "path": "notes/plan.md", "raw": true})
	var got rawFile
	structured(t, res, &got)
	if !got.Changed || !strings.Contains(textOf(res), "changed after the run") || string(embedded(res).Blob) != "# Plan 2\n" {
		t.Errorf("changed file = %+v: %s", got, textOf(res))
	}
}

func TestRunFileRawReachesTheOwner(t *testing.T) {
	ctx := testCtx(t)
	ws, err := fw.DirFiles(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	png := []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR")
	writer := fw.NewTool(fw.ToolSpec{Name: "writer"}, func(ctx context.Context, _ json.RawMessage) (fw.ToolResult, error) {
		if err := fw.FilesFrom(ctx).WriteFile("img/a b.png", png); err != nil {
			return fw.ErrorResult(err.Error()), nil
		}
		return fw.TextResult("written"), fw.FilesFrom(ctx).WriteFile("big.txt", bytes.Repeat([]byte("y"), maxRawBytes+10))
	})
	owner := tokenOwner(t, newScript(toolUse("c1", "writer", `{}`), endTurn("done")).model(),
		fw.WithFiles(ws), fw.WithTools(writer), fw.WithSurfaces(native.New()))
	h, err := owner.Handler()
	if err != nil {
		t.Fatal(err)
	}
	ownerSrv := httptest.NewServer(h)
	t.Cleanup(ownerSrv.Close)
	r, err := owner.Run(fw.WithPrincipal(ctx, people["alice-token"]), "write", fw.AsAgent("reviewer"))
	if err != nil || r.State != fw.StateCompleted {
		t.Fatalf("owner run = %+v, %v", r, err)
	}
	fake := &fakeFleet{name: "view", owner: ownerSrv.URL, members: []fleet.Member{
		{Name: "view", Self: true, State: fleet.MemberLive, AccessMode: fw.AccessModeRules},
		{Name: "owner", State: fleet.MemberLive, CanForward: true, AccessMode: fw.AccessModeRules},
	}}
	fake.set(r)
	view := attach(manageAgent(t, newScript().model()), fake)
	fake.rules = view
	cs := as(t, ctx, serveAgent(t, view, nil).URL, "bob-token")

	res := callTool(t, ctx, cs, toolRunFile, map[string]any{"runId": r.ID, "path": "img/a b.png", "raw": true})
	sum := sha256.Sum256(png)
	if c := embedded(res); res.IsError || c == nil || !bytes.Equal(c.Blob, png) || c.MIMEType != "image/png" || !strings.Contains(textOf(res), hex.EncodeToString(sum[:])) {
		t.Fatalf("remote raw = %+v: %s", c, textOf(res))
	}
	if got := fake.lastForward().Get("Accept"); got != "application/octet-stream" {
		t.Errorf("forwarded Accept = %q", got)
	}
	res = callTool(t, ctx, cs, toolRunFile, map[string]any{"runId": r.ID, "path": "big.txt", "raw": true})
	if txt := textOf(res); !res.IsError || !strings.Contains(txt, fmt.Sprintf("has %d bytes", maxRawBytes+10)) || !strings.Contains(txt, "4 MiB") {
		t.Fatalf("remote big raw = %q", txt)
	}
	res = callTool(t, ctx, cs, toolRunFile, map[string]any{"runId": r.ID, "path": "../etc/passwd", "raw": true})
	if !res.IsError || !strings.Contains(textOf(res), "the fleet member owner") {
		t.Fatalf("remote outside path = %q", textOf(res))
	}
	// The file resource reads the runs of this process only.
	if _, err := cs.ReadResource(ctx, &sdk.ReadResourceParams{URI: fileURI(r.ID, "img/a b.png")}); err == nil {
		t.Error("the file resource read a run of another member")
	}
}

func TestRunFileResource(t *testing.T) {
	ctx := testCtx(t)
	png := []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR")
	a, r := fileAgent(t, ctx, map[string][]byte{"docs/a b.md": []byte("# Doc\n"), "dot.png": png, "big.bin": bytes.Repeat([]byte{0}, maxRawBytes+1), "empty.txt": {}})
	url := serveAgent(t, a, nil).URL
	bob := as(t, ctx, url, "bob-token")
	read := func(cs *sdk.ClientSession, uri string) (*sdk.ResourceContents, error) {
		res, err := cs.ReadResource(ctx, &sdk.ReadResourceParams{URI: uri})
		if err != nil {
			return nil, err
		}
		return res.Contents[0], nil
	}
	// A path with its slashes raw or escaped.
	for _, uri := range []string{"fw://runs/" + r.ID + "/files/docs/a%20b.md", "fw://runs/" + r.ID + "/files/docs%2Fa%20b.md", fileURI(r.ID, "docs/a b.md")} {
		c, err := read(bob, uri)
		if err != nil || c.Text != "# Doc\n" || c.Blob != nil || c.MIMEType != "text/plain" || c.URI != uri {
			t.Fatalf("%s = %+v, %v", uri, c, err)
		}
	}
	c, err := read(bob, fileURI(r.ID, "dot.png"))
	if err != nil || !bytes.Equal(c.Blob, png) || c.Text != "" || c.MIMEType != "image/png" {
		t.Fatalf("png = %+v, %v", c, err)
	}
	if c, err := read(bob, fileURI(r.ID, "empty.txt")); err != nil || c.Blob == nil || len(c.Blob) != 0 {
		t.Fatalf("empty = %+v, %v", c, err)
	}
	// The bound and the reader rules of run_file hold.
	if _, err := read(bob, fileURI(r.ID, "big.bin")); err == nil || !strings.Contains(err.Error(), fmt.Sprint(maxRawBytes)) {
		t.Errorf("big = %v", err)
	}
	for _, uri := range []string{
		"fw://runs/" + r.ID + "/files/../etc/passwd",
		"fw://runs/" + r.ID + "/files/docs",
		"fw://runs/" + r.ID + "/files/",
		"fw://runs/nope/files/dot.png",
		"fw://runs//files/dot.png",
	} {
		if c, err := read(bob, uri); err == nil {
			t.Errorf("%s = %+v", uri, c)
		}
	}
	// A run that the caller cannot view stays closed, although the run
	// changed the file: alice, who started it, reads the same URI.
	hidden, err := a.Run(fw.WithPrincipal(ctx, people["alice-token"]), "x", fw.AsAgent("shared"))
	if err != nil || hidden.State != fw.StateCompleted {
		t.Fatalf("hidden run = %+v, %v", hidden, err)
	}
	alice := as(t, ctx, url, "alice-token")
	if c, err := read(alice, fileURI(hidden.ID, "dot.png")); err != nil || !bytes.Equal(c.Blob, png) {
		t.Fatalf("alice reads the file of her run: %+v, %v", c, err)
	}
	if c, err := read(bob, fileURI(hidden.ID, "dot.png")); err == nil {
		t.Errorf("bob read a file of a hidden run: %+v", c)
	}
}

// meterFiles is a workspace store that counts the bytes that the reads of
// each file take. Stat reports a size of 1 byte for a path in grows, as
// for a file that grows after Stat.
type meterFiles struct {
	fw.Files
	grows map[string]bool
	mu    sync.Mutex
	read  map[string]int64
}

type smallInfo struct{ fs.FileInfo }

func (smallInfo) Size() int64 { return 1 }

func (m *meterFiles) info(name string, info fs.FileInfo, err error) (fs.FileInfo, error) {
	if err == nil && m.grows[name] {
		return smallInfo{info}, nil
	}
	return info, err
}

func (m *meterFiles) RealName(name string) (string, error) { return fw.RealName(m.Files, name) }

func (m *meterFiles) Stat(name string) (fs.FileInfo, error) {
	info, err := m.Files.Stat(name)
	return m.info(name, info, err)
}

func (m *meterFiles) Open(name string) (fs.File, error) {
	f, err := m.Files.Open(name)
	if err != nil {
		return nil, err
	}
	return &meterFile{File: f, m: m, name: name}, nil
}

// taken returns the bytes that the reads of name took since the last call.
func (m *meterFiles) taken(name string) int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := m.read[name]
	delete(m.read, name)
	return n
}

type meterFile struct {
	fs.File
	m    *meterFiles
	name string
}

func (f *meterFile) Stat() (fs.FileInfo, error) {
	info, err := f.File.Stat()
	return f.m.info(f.name, info, err)
}

func (f *meterFile) Read(p []byte) (int, error) {
	n, err := f.File.Read(p)
	f.m.mu.Lock()
	f.m.read[f.name] += int64(n)
	f.m.mu.Unlock()
	return n, err
}

// A file above the raw bound never enters memory whole: Stat refuses it
// before the read, and a file that grows after Stat stops the read at the
// bound and one byte. tools.files.maxFileBytes (8 MiB by default) is
// larger than the bound, so only the bound stops these reads.
func TestRawReadStaysBounded(t *testing.T) {
	ctx := testCtx(t)
	dir, err := fw.DirFiles(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	meter := &meterFiles{Files: dir, grows: map[string]bool{"grows.bin": true}, read: map[string]int64{}}
	big := bytes.Repeat([]byte("b"), maxRawBytes+(1<<20))
	grows := bytes.Repeat([]byte("g"), maxRawBytes+(1<<20))
	a, r := fileAgent(t, ctx, map[string][]byte{"big.bin": big, "grows.bin": grows, "small.txt": []byte("small")}, fw.WithFiles(meter))
	cs := as(t, ctx, serveAgent(t, a, nil).URL, "bob-token")
	for _, name := range []string{"big.bin", "grows.bin", "small.txt"} {
		meter.taken(name)
	}

	res := callTool(t, ctx, cs, toolRunFile, map[string]any{"runId": r.ID, "path": "big.bin", "raw": true})
	if txt := textOf(res); !res.IsError || !strings.Contains(txt, fmt.Sprintf("has %d bytes", len(big))) || embedded(res) != nil {
		t.Fatalf("big raw = %q", txt)
	}
	if n := meter.taken("big.bin"); n != 0 {
		t.Errorf("run_file read %d bytes of a file above the bound", n)
	}
	res = callTool(t, ctx, cs, toolRunFile, map[string]any{"runId": r.ID, "path": "grows.bin", "raw": true})
	if txt := textOf(res); !res.IsError || !strings.Contains(txt, "larger than the bound") || embedded(res) != nil {
		t.Fatalf("grown raw = %q", txt)
	}
	if n := meter.taken("grows.bin"); n > maxRawBytes+1 {
		t.Errorf("run_file read %d bytes of a file that grew, more than the bound and one byte", n)
	}
	res = callTool(t, ctx, cs, toolRunFile, map[string]any{"runId": r.ID, "path": "small.txt", "raw": true})
	if c := embedded(res); res.IsError || c == nil || string(c.Blob) != "small" {
		t.Fatalf("small raw = %q", textOf(res))
	}

	if _, err := cs.ReadResource(ctx, &sdk.ReadResourceParams{URI: fileURI(r.ID, "big.bin")}); err == nil || !strings.Contains(err.Error(), fmt.Sprintf("has %d bytes", len(big))) {
		t.Errorf("big resource = %v", err)
	}
	if n := meter.taken("big.bin"); n != 0 {
		t.Errorf("the resource read %d bytes of a file above the bound", n)
	}
	if _, err := cs.ReadResource(ctx, &sdk.ReadResourceParams{URI: fileURI(r.ID, "grows.bin")}); err == nil || !strings.Contains(err.Error(), "larger than the bound") {
		t.Errorf("grown resource = %v", err)
	}
	if n := meter.taken("grows.bin"); n > maxRawBytes+1 {
		t.Errorf("the resource read %d bytes of a file that grew, more than the bound and one byte", n)
	}
}

func TestFileFromURI(t *testing.T) {
	for uri, want := range map[string][2]string{
		"fw://runs/01A/files/a.md":         {"01A", "a.md"},
		"fw://runs/01A/files/d/e/f.md":     {"01A", "d/e/f.md"},
		"fw://runs/01A/files/d%2Fa%20b.md": {"01A", "d/a b.md"},
		"fw://runs/01A/files/files/x":      {"01A", "files/x"},
		"fw://runs/01A/files/":             {},
		"fw://runs/01A/ledger":             {},
		"fw://runs/a%2Fb/files/x":          {},
		"fw://skills/x/files/y":            {},
		"fw://runs/01A/files/%zz":          {},
	} {
		id, path, ok := fileFromURI(uri)
		if ok != (want[0] != "") || id != want[0] || path != want[1] {
			t.Errorf("fileFromURI(%q) = %q, %q, %v", uri, id, path, ok)
		}
	}
	if got := fileURI("01A", "d/a b.md"); got != "fw://runs/01A/files/d/a%20b.md" {
		t.Errorf("fileURI = %q", got)
	}
}
