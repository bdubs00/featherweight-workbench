package mcp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"strings"
	"unicode/utf8"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/surface/view"
	"github.com/bdubs00/featherweight-workbench/fleet"
	"github.com/bdubs00/featherweight-workbench/manager"
)

// toolRunFile is the management tool that reads a file that a run changed.
const toolRunFile = "run_file"

// maxRawBytes bounds the bytes of a file that run_file with raw and the
// resource fw://runs/{id}/files/{path} answer.
const maxRawBytes = 4 << 20

// filesPart is the part of a file URI between the run id and the path.
const filesPart = "/files/"

const runFileSchema = `{"type":"object","properties":{
	"runId":` + runIDSchema + `,
	"path":{"type":"string","minLength":1,"description":"The path of the file, relative to the workspace, as an item of kind file in run_events names it."},
	"raw":{"type":"boolean","description":"true answers the bytes of the file as an embedded resource (a blob in base64, with its media type), up to 4 MiB, to save or to download. The text names the size and the sha256."}
},"required":["runId","path"]}`

type runFileArgs struct {
	RunID string `json:"runId"`
	Path  string `json:"path"`
	Raw   bool   `json:"raw,omitempty"`
}

// rawFile is the structured result of run_file with raw: the file without
// its bytes, which travel in the embedded resource at URI.
type rawFile struct {
	Path      string `json:"path"`
	Op        string `json:"op,omitempty"`
	Tool      string `json:"tool,omitempty"`
	Size      int64  `json:"size"`
	MediaType string `json:"mediaType"`
	SHA256    string `json:"sha256"`
	URI       string `json:"uri"`
	Changed   bool   `json:"changed,omitempty"` // the file changed after the last change of the run
}

// addFileTool adds run_file. readOnly gives its annotations.
func (s *server) addFileTool(readOnly func() *sdk.ToolAnnotations) {
	sdk.AddTool(s.srv, &sdk.Tool{
		Name:        toolRunFile,
		Title:       "Run file",
		Description: "Return the current content of a file that a run changed, with the last change of the run to it. The items of kind file in run_events name the files. A text file comes back as text, and an image as an image. The result says when the file changed after the run. With raw true, the bytes of any file up to 4 MiB come back as an embedded resource, to save or to download.",
		InputSchema: json.RawMessage(runFileSchema),
		Annotations: readOnly(),
	}, s.runFile)
}

func (s *server) runFile(ctx context.Context, req *sdk.CallToolRequest, in runFileArgs) (*sdk.CallToolResult, any, error) {
	ctx, _, _ = manager.OnBehalf(ctx)
	ctx = withCredential(ctx, req)
	if in.Raw {
		return s.runFileRaw(ctx, in), nil, nil
	}
	v, err := view.FileViewOf(ctx, s.a, in.RunID, in.Path)
	if errors.Is(err, fw.ErrRunNotFound) {
		member, ferr := s.ownerOf(ctx, in.RunID)
		switch {
		case ferr != nil:
			err = ferr
		case member != "":
			v = view.FileView{}
			var mctx context.Context
			if mctx, err = fleet.ForRun(ctx, s.a, in.RunID, fw.ActionRunView); err == nil {
				err = s.callMember(mctx, member, "GET", "/v1/runs/"+url.PathEscape(in.RunID)+"/files/"+escapePath(in.Path), nil, "", &v)
			}
		}
	}
	switch {
	case errors.Is(err, view.ErrNotInRun):
		return errorResult(fmt.Sprintf("The run %s did not change the file %s. The items of kind file in run_events name the files that it changed.", in.RunID, in.Path)), nil, nil
	case errors.Is(err, fs.ErrPermission):
		return errorResult(fmt.Sprintf("The workspace does not open the file %s: %v", in.Path, err)), nil, nil
	case err != nil:
		return failResult(err), nil, nil
	}
	res := result(fileText(v), v)
	if len(v.Data) > 0 {
		res.Content = append(res.Content, &sdk.ImageContent{Data: v.Data, MIMEType: v.MediaType})
	}
	return res, nil, nil
}

// escapePath escapes each part of a slash-separated path.
func escapePath(p string) string {
	parts := strings.Split(strings.TrimPrefix(p, "/"), "/")
	for i, part := range parts {
		parts[i] = url.PathEscape(part)
	}
	return strings.Join(parts, "/")
}

// fileText is the text of a file view for a person or a model.
func fileText(v view.FileView) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s: %s", v.Path, v.Op)
	if v.Tool != "" {
		fmt.Fprintf(&b, " by the tool %s", v.Tool)
	}
	b.WriteString(".")
	switch {
	case v.Gone:
		b.WriteString(" The file does not exist now.")
		return b.String()
	case v.Changed:
		b.WriteString(" The file changed after the run. This is the current content.")
	}
	fmt.Fprintf(&b, " %d bytes, %s.", v.Size, v.MediaType)
	switch {
	case v.Large:
		b.WriteString(" The file is too large to show.")
	case v.Binary:
		b.WriteString(" The file is not text or an image.")
	case v.Text != "":
		b.WriteString("\n\n" + v.Text)
		if v.Cut {
			b.WriteString("\n[the file is longer; this is its start]")
		}
	}
	return b.String()
}

// runFileRaw answers run_file with raw: the bytes of the file as an
// embedded resource. The reader rules are those of run_file: the read of
// the run authorizes the caller, and the path must be one that the events
// of the run list. A run of another member comes from that member.
func (s *server) runFileRaw(ctx context.Context, in runFileArgs) *sdk.CallToolResult {
	f, data, err := view.ReadRunFileUpTo(ctx, s.a, in.RunID, in.Path, maxRawBytes)
	size := int64(len(data)) // the size of the file, or -1 when it is not known
	var bound *view.BoundError
	if errors.Is(err, fw.ErrRunNotFound) {
		member, ferr := s.ownerOf(ctx, in.RunID)
		switch {
		case ferr != nil:
			err = ferr
		case member != "":
			f = fw.RunFile{Path: strings.TrimPrefix(in.Path, "/")}
			var mctx context.Context
			if mctx, err = fleet.ForRun(ctx, s.a, in.RunID, fw.ActionRunView); err == nil {
				data, size, err = s.memberFile(mctx, member, in.RunID, in.Path)
			}
		}
	}
	switch {
	case errors.Is(err, view.ErrNotInRun):
		return errorResult(fmt.Sprintf("The run %s did not change the file %s. The items of kind file in run_events name the files that it changed.", in.RunID, in.Path))
	case errors.As(err, &bound):
		return errorResult(tooLarge(in.RunID, in.Path, bound.Size))
	case errors.Is(err, fs.ErrNotExist):
		return errorResult(fmt.Sprintf("The file %s does not exist now.", in.Path))
	case errors.Is(err, fw.ErrFileTooLarge):
		size = -1
		if info, serr := s.a.Files().Stat(f.Path); serr == nil {
			size = info.Size()
		}
		return errorResult(tooLarge(in.RunID, in.Path, size))
	case errors.Is(err, fs.ErrPermission):
		return errorResult(fmt.Sprintf("The workspace does not open the file %s: %v", in.Path, err))
	case err != nil:
		return failResult(err)
	case len(data) > maxRawBytes || size > maxRawBytes:
		return errorResult(tooLarge(in.RunID, in.Path, size))
	}
	sum := sha256.Sum256(data)
	out := rawFile{
		Path:      f.Path,
		Op:        f.Op,
		Tool:      f.Tool,
		Size:      int64(len(data)),
		MediaType: view.MediaType(data),
		SHA256:    hex.EncodeToString(sum[:]),
		URI:       fileURI(in.RunID, f.Path),
	}
	out.Changed = f.SHA256 != "" && f.SHA256 != out.SHA256
	text := fmt.Sprintf("%s: %d bytes, %s, sha256 %s.", out.Path, out.Size, out.MediaType, out.SHA256)
	if out.Changed {
		text += " The file changed after the run. These are the current bytes."
	}
	text += " The bytes are in the embedded resource " + out.URI + "."
	res := result(text, out)
	if data == nil {
		data = []byte{} // an empty file still has a blob
	}
	res.Content = append(res.Content, &sdk.EmbeddedResource{Resource: &sdk.ResourceContents{URI: out.URI, MIMEType: out.MediaType, Blob: data}})
	return res
}

// tooLarge is the error text of a file above maxRawBytes. A size below 0
// is not known.
func tooLarge(id, path string, size int64) string {
	native := fmt.Sprintf("The native API serves it in full: GET /v1/runs/%s/files/%s.", url.PathEscape(id), escapePath(path))
	if size < 0 {
		return fmt.Sprintf("The file %s is larger than the bound of %d bytes (4 MiB), so it does not come back raw. %s", path, maxRawBytes, native)
	}
	return fmt.Sprintf("The file %s has %d bytes, more than the bound of %d bytes (4 MiB), so it does not come back raw. %s", path, size, maxRawBytes, native)
}

// fileURI is the resource URI of a file of a run.
func fileURI(id, path string) string {
	return runPrefix + url.PathEscape(id) + filesPart + escapePath(path)
}

// fileFromURI returns the run id and the path of fw://runs/{id}/files/{path}.
// The path can hold slashes, raw or escaped as %2F.
func fileFromURI(uri string) (id, path string, ok bool) {
	rest, ok := strings.CutPrefix(uri, runPrefix)
	if !ok {
		return "", "", false
	}
	rawID, rawPath, ok := strings.Cut(rest, filesPart)
	if !ok {
		return "", "", false
	}
	id, err := url.PathUnescape(rawID)
	if err != nil || id == "" || strings.Contains(id, "/") {
		return "", "", false
	}
	path, err = url.PathUnescape(rawPath)
	if err != nil || strings.TrimPrefix(path, "/") == "" {
		return "", "", false
	}
	return id, path, true
}

// readFile answers fw://runs/{id}/files/{path}: a file that a run changed,
// as text when it is UTF-8 text and as a blob otherwise. The reader rules
// and the bound are those of run_file with raw. It reads a run of this
// process only.
func (s *server) readFile(ctx context.Context, req *sdk.ReadResourceRequest) (*sdk.ReadResourceResult, error) {
	uri := req.Params.URI
	id, name, ok := fileFromURI(uri)
	if !ok {
		return nil, sdk.ResourceNotFoundError(uri)
	}
	_, data, err := view.ReadRunFileUpTo(ctx, s.a, id, name, maxRawBytes)
	var bound *view.BoundError
	switch {
	case errors.Is(err, fw.ErrRunNotFound), errors.Is(err, view.ErrNotInRun), errors.Is(err, fs.ErrNotExist):
		return nil, sdk.ResourceNotFoundError(uri)
	case errors.As(err, &bound) && bound.Size >= 0:
		return nil, fmt.Errorf("mcp: read %s: the file has %d bytes, more than the bound of %d bytes", uri, bound.Size, maxRawBytes)
	case errors.As(err, &bound):
		return nil, fmt.Errorf("mcp: read %s: the file is larger than the bound of %d bytes", uri, maxRawBytes)
	case errors.Is(err, fw.ErrFileTooLarge):
		return nil, fmt.Errorf("mcp: read %s: the file is larger than tools.files.maxFileBytes", uri)
	case err != nil:
		return nil, fmt.Errorf("mcp: read %s: %w", uri, err)
	}
	c := &sdk.ResourceContents{URI: uri, MIMEType: view.MediaType(data)}
	switch {
	case len(data) == 0:
		c.Blob = []byte{} // an empty file still has a blob
	case isText(data):
		c.Text = string(data)
	default:
		c.Blob = data
	}
	return &sdk.ReadResourceResult{Contents: []*sdk.ResourceContents{c}}, nil
}

// isText reports whether data is UTF-8 text without a NUL byte.
func isText(data []byte) bool {
	return utf8.Valid(data) && bytes.IndexByte(data, 0) < 0
}
