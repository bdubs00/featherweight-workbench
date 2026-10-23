package bridges

// This file holds the files that a person attaches to a message
// (docs/ARCHITECTURE.md section 18.5). The route takes them as POST
// /v1/runs takes them, each name with its bytes in base64. The channel
// keeps the name, the size, and the media type of each file, and never the
// bytes. The bytes go to the run of each member that gets the message,
// which keeps them in its workspace under attachments/. So a message with
// files goes out only when every member that it reaches has a run that
// takes it now: the channel holds no bytes to give a later run.

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync/atomic"
	"unicode"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/surface/native"
)

// maxHeldBytes bounds the bytes of the files that the deliveries of one
// replica hold until each member has them. A delivery that fails tries
// again for up to retryFor, so without the bound a few large posts to a
// worker that does not answer fill the memory of the front.
const maxHeldBytes = 2 * fw.MaxAttachmentBytes

// AttachedFile describes a file of a message. The channel and the stream
// hold it, and the workspace of each run that got the message holds the
// bytes.
type AttachedFile struct {
	Name      string `json:"name"`
	Size      int64  `json:"size"`
	MediaType string `json:"mediaType"`
}

// describeFiles checks the files of a message and describes them for the
// entry. The post checks each file against tools.files.maxFileBytes of the
// workers of each recipient (filesTooLarge), and the kernel of each
// recipient checks them again with the rules of its workspace.
func describeFiles(files []native.FileBody) ([]AttachedFile, error) {
	if len(files) > fw.MaxAttachments {
		return nil, invalid("a message holds at most %d files", fw.MaxAttachments)
	}
	var out []AttachedFile
	total := 0
	for _, f := range files {
		name := strings.TrimSpace(f.Name)
		if name == "" || name == "." || name == ".." || len(f.Name) > 255 || strings.ContainsAny(f.Name, `/\`) || strings.IndexFunc(f.Name, unicode.IsControl) >= 0 {
			return nil, invalid("a file needs a name of at most 255 bytes, without a path or a control character")
		}
		if len(f.Data) == 0 {
			return nil, invalid("the file %q is empty", f.Name)
		}
		total += len(f.Data)
		if total > fw.MaxAttachmentBytes {
			return nil, fmt.Errorf("%w: the files of a message hold at most %d MiB together", ErrTooLarge, fw.MaxAttachmentBytes>>20)
		}
		out = append(out, AttachedFile{Name: f.Name, Size: int64(len(f.Data)), MediaType: fw.SniffMediaType(f.Data)})
	}
	return out, nil
}

// upload is the files of one message, which its deliveries share. The post
// holds one reference and each delivery one more, and the last release
// gives the bytes back to the bound of the replica (maxHeldBytes).
type upload struct {
	s     *Store
	files []native.FileBody
	size  int64
	refs  atomic.Int64
}

// hold takes the files of a message under the bound of the replica, or
// refuses them with ErrConflict while earlier files wait for their
// members.
func (s *Store) hold(files []native.FileBody) (*upload, error) {
	u := &upload{s: s, files: files}
	for _, f := range files {
		u.size += int64(len(f.Data))
	}
	if s.held.Add(u.size) > maxHeldBytes {
		s.held.Add(-u.size)
		return nil, fmt.Errorf("%w: the front still sends the files of earlier messages: send these files in a minute", ErrConflict)
	}
	u.refs.Store(1)
	return u, nil
}

// take adds n references, one for each delivery.
func (u *upload) take(n int) { u.refs.Add(int64(n)) }

// release drops one reference.
func (u *upload) release() {
	if u.refs.Add(-1) == 0 {
		u.s.held.Add(-u.size)
	}
}

// filesRefused says why a message with files cannot go out to every member
// that it reaches, or returns "" when it can: fanPlan gave each of them a
// delivery to a run that the view holds now. A seating, a deferred
// delivery, a failed run, and a cap leave a member without the files, so
// post refuses the whole message before it changes the record.
func filesRefused(rec *record, msg Message, plan []delivery, runs map[string]*fw.Run) string {
	got := map[string]bool{}
	for _, d := range plan {
		if d.run != nil {
			got[d.handle] = true
		}
	}
	targets := recipients(rec, msg.To)
	var waiting, failed []string
	capped := false
	for _, m := range targets {
		if got[m.Handle] {
			continue
		}
		r := runs[m.newest()]
		switch {
		case r == nil:
			waiting = append(waiting, m.Handle)
		case r.State == fw.StateFailed && !limited(r):
			failed = append(failed, m.Handle)
		default:
			capped = true
		}
	}
	switch {
	case len(targets) == 0:
		return "the message reaches no member, so its files have nowhere to go: address a member"
	case len(waiting) == 1:
		return fmt.Sprintf("files go only to a member with a run, and %s has none yet: send them once it works, or address another member", waiting[0])
	case len(waiting) > 1:
		return fmt.Sprintf("files go only to a member with a run, and %s have none yet: send them once they work, or address other members", andList(waiting))
	case len(failed) == 1:
		return fmt.Sprintf("files go only to a run that takes messages, and the run of %s failed: give it a new task, or address another member", failed[0])
	case len(failed) > 1:
		return fmt.Sprintf("files go only to a run that takes messages, and the runs of %s failed: give them new tasks, or address other members", andList(failed))
	case capped:
		return "a cap of the bridge holds the message back, so its files did not go out: raise the cap, or send them later"
	}
	return ""
}

// filesTooLarge says which file is larger than the workspace of a member
// takes, or returns "": each delivery of plan goes to a run whose member
// takes files of at most limits[run id] bytes (fleet.MaxFileBytes). The
// kernel of the member would refuse such a file after the post, so post
// refuses the whole message before it changes the record.
func filesTooLarge(files []native.FileBody, plan []delivery, limits map[string]int64) string {
	for _, f := range files {
		var over []string
		limit := int64(0)
		for _, d := range plan {
			if d.run == nil {
				continue
			}
			max := limits[d.run.ID]
			if max <= 0 {
				max = fw.DefaultMaxFileBytes
			}
			if int64(len(f.Data)) > max && !slices.Contains(over, d.handle) {
				over = append(over, d.handle)
				if limit == 0 || max < limit {
					limit = max
				}
			}
		}
		switch len(over) {
		case 0:
			continue
		case 1:
			return fmt.Sprintf("the file %q holds %s, and %s takes files of at most %s (tools.files.maxFileBytes of its workers): send a smaller file", f.Name, mibWords(int64(len(f.Data))), over[0], mibWords(limit))
		default:
			return fmt.Sprintf("the file %q holds %s, and %s take files of at most %s (tools.files.maxFileBytes of their workers): send a smaller file", f.Name, mibWords(int64(len(f.Data))), andList(over), mibWords(limit))
		}
	}
	return ""
}

// mibWords writes a size in MiB, the unit of the limits of files, such as
// 8 MiB or 9.3 MiB.
func mibWords(n int64) string {
	if n%(1<<20) == 0 {
		return fmt.Sprintf("%d MiB", n>>20)
	}
	return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
}

// andList joins handles as a sentence does: a, a and b, or a, b, and c.
func andList(hs []string) string {
	switch len(hs) {
	case 0:
		return ""
	case 1:
		return hs[0]
	case 2:
		return hs[0] + " and " + hs[1]
	}
	return strings.Join(hs[:len(hs)-1], ", ") + ", and " + hs[len(hs)-1]
}

// attachedText names the files of an entry for an agent that reads the
// channel, or returns "" for an entry without files.
func attachedText(files []AttachedFile) string {
	if len(files) == 0 {
		return ""
	}
	names := make([]string, len(files))
	for i, f := range files {
		names[i] = fmt.Sprintf("%s (%s)", f.Name, sizeWords(f.Size))
	}
	return fmt.Sprintf("\n[Files: %s. Each run that got this message has them under attachments/ in its workspace.]", strings.Join(names, ", "))
}

// sizeWords writes a size as the kernel words it for the model: 812
// bytes, 14 KB, or 2.1 MB.
func sizeWords(n int64) string {
	switch {
	case n < 1000:
		return fmt.Sprintf("%d bytes", n)
	case n < 1000*1000:
		return fmt.Sprintf("%d KB", (n+500)/1000)
	}
	return fmt.Sprintf("%.1f MB", float64(n)/1e6)
}

// filesBody is the body of POST /v1/runs/{id}/messages with files: the
// other fields as JSON, then the files, whose bytes it writes as base64
// while the request reads them. The deliveries of a message share the
// bytes, so a delivery holds no second copy of them, neither in base64
// nor in a buffer of the encoder.
type filesBody struct {
	head  []byte // the JSON object of the other fields without its closing brace
	files []native.FileBody
}

// newFilesBody returns the body of fields, a JSON object, with files.
func newFilesBody(fields map[string]any, files []native.FileBody) (*filesBody, error) {
	head, err := json.Marshal(fields)
	if err != nil {
		return nil, err
	}
	head = bytes.TrimSuffix(head, []byte("}"))
	if len(head) > 1 {
		head = append(head, ',')
	}
	return &filesBody{head: append(head, `"files":[`...), files: files}, nil
}

// fileHead is the start of the JSON object of a file, up to the base64.
func fileHead(i int, f native.FileBody) []byte {
	name, _ := json.Marshal(f.Name)
	var b []byte
	if i > 0 {
		b = append(b, ',')
	}
	b = append(b, `{"name":`...)
	b = append(b, name...)
	return append(b, `,"data":"`...)
}

// Len returns the length of the body.
func (b *filesBody) Len() int64 {
	n := int64(len(b.head)) + 2 // ]}
	for i, f := range b.files {
		n += int64(len(fileHead(i, f))+base64.StdEncoding.EncodedLen(len(f.Data))) + 2 // "}
	}
	return n
}

// Reader returns a new reader of the body.
func (b *filesBody) Reader() io.Reader {
	parts := []io.Reader{bytes.NewReader(b.head)}
	for i, f := range b.files {
		parts = append(parts, bytes.NewReader(fileHead(i, f)), &base64Reader{data: f.Data}, strings.NewReader(`"}`))
	}
	return io.MultiReader(append(parts, strings.NewReader("]}"))...)
}

// base64Reader reads data as standard base64, one chunk at a time.
type base64Reader struct {
	data []byte
	buf  [4 << 10]byte
	out  []byte
}

func (r *base64Reader) Read(p []byte) (int, error) {
	if len(r.out) == 0 {
		if len(r.data) == 0 {
			return 0, io.EOF
		}
		// A chunk of a multiple of 3 bytes needs no padding, so only the
		// last chunk pads.
		n := min(len(r.data), len(r.buf)/4*3)
		base64.StdEncoding.Encode(r.buf[:], r.data[:n])
		r.out, r.data = r.buf[:base64.StdEncoding.EncodedLen(n)], r.data[n:]
	}
	n := copy(p, r.out)
	r.out = r.out[n:]
	return n, nil
}
