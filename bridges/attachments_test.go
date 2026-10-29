package bridges

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/surface/native"
	"github.com/bdubs00/featherweight-workbench/fleet"
	"github.com/bdubs00/featherweight-workbench/internal/bucket"
)

// deliveredFiles decodes the files of a follow-up that the fake member got.
func deliveredFiles(t *testing.T, got seen) []native.FileBody {
	t.Helper()
	raw, _ := json.Marshal(got.Body)
	var body struct {
		Files []native.FileBody `json:"files"`
	}
	decodeJSON(t, string(raw), &body)
	return body.Files
}

// values returns every live value of a bucket, so a test can check what
// the replicas of the front share.
func values(t *testing.T, b bucket.Bucket) []string {
	t.Helper()
	ctx := context.Background()
	keys, err := b.Keys(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, k := range keys {
		e, err := b.Get(ctx, k.Key)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, string(e.Value))
	}
	return out
}

// A file of a person reaches the addressed member with its exact bytes, in
// a body larger than the bound of a body without files, and the entry, the
// channel, and the answer hold its name, size, and media type alone. A
// broadcast gives every active member the same file, and the bytes go back
// to the bound of the replica once each member has them.
func TestAttachmentFanout(t *testing.T) {
	s, _, fm, url, id := fanBridge(t, "")
	data := bytes.Repeat([]byte{0, 255, 1, 128}, 300000) // 1.2 MB of binary bytes
	body, _ := json.Marshal(PostSpec{To: []string{"reviewer"}, Files: []native.FileBody{{Name: "sample.bin", Data: data}}})
	if len(body) <= native.MaxBodyBytes {
		t.Fatalf("the body of %d bytes stays under the bound without files", len(body))
	}
	resp, raw := do(t, http.MethodPost, url+"/v1/bridges/"+id+"/messages", "alice", string(body))
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("post = %d %s", resp.StatusCode, raw)
	}
	var msg Message
	decodeJSON(t, raw, &msg)
	want := AttachedFile{Name: "sample.bin", Size: int64(len(data)), MediaType: "application/octet-stream"}
	if len(msg.Files) != 1 || msg.Files[0] != want || strings.Contains(raw, `"data"`) {
		t.Fatalf("the entry = %s", raw)
	}
	eventually(t, "the reviewer gets the file", func() bool { return len(fm.find(http.MethodPost, pathRev)) == 1 })
	got := fm.find(http.MethodPost, pathRev)[0]
	files := deliveredFiles(t, got)
	if len(files) != 1 || files[0].Name != "sample.bin" || !bytes.Equal(files[0].Data, data) || got.Auth != "Bearer "+fleetToken {
		t.Fatalf("the delivery changed its files or its credential: %d files, auth %q", len(files), got.Auth)
	}
	if content, _ := got.Body["content"].(string); !strings.HasPrefix(content, `[channel "Migrate logrotate" `+id+`] @reviewer from operator: `) {
		t.Fatalf("content = %q", content)
	}
	if from, _ := got.Body["from"].(map[string]any); from["kind"] != fw.FromPerson || from["name"] != "alice" || from["member"] != "reviewer" {
		t.Fatalf("from = %v", got.Body["from"])
	}
	if len(fm.find(http.MethodPost, pathMig)) != 0 {
		t.Fatal("a member that the message does not address got the file")
	}
	last := channel(t, s, id)
	if e := last[len(last)-1]; len(e.Files) != 1 || e.Files[0] != want {
		t.Fatalf("the channel lost the metadata: %+v", e)
	}
	// A broadcast with text and a file reaches both seats.
	resp, raw = do(t, http.MethodPost, url+"/v1/bridges/"+id+"/messages", "bob", `{"text":"Both of you","files":[{"name":"note.txt","data":"aGk="}]}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("broadcast = %d %s", resp.StatusCode, raw)
	}
	eventually(t, "both seats get the file", func() bool {
		return len(fm.find(http.MethodPost, pathRev)) == 2 && len(fm.find(http.MethodPost, pathMig)) == 1
	})
	for _, g := range []seen{fm.find(http.MethodPost, pathRev)[1], fm.find(http.MethodPost, pathMig)[0]} {
		if f := deliveredFiles(t, g); len(f) != 1 || string(f[0].Data) != "hi" {
			t.Fatalf("a broadcast delivery = %+v", f)
		}
	}
	eventually(t, "the replica holds no bytes", func() bool { return s.held.Load() == 0 })
	// What the replicas share, the record and the channel, holds no byte
	// of a file.
	encoded := base64.StdEncoding.EncodeToString(data[:3000])
	for _, b := range []bucket.Bucket{s.b.records, s.b.channels} {
		for _, v := range values(t, b) {
			if strings.Contains(v, encoded) || strings.Contains(v, `"data"`) {
				t.Fatalf("a bucket holds the bytes of a file: %.200s", v)
			}
		}
	}
}

// A post with files that cannot reach every member changes nothing: it
// answers 409, adds no entry, consumes no cap, and delivers to no member.
// Files that do not fit answer 400 or 413 before the fan-out.
func TestAttachmentRefusals(t *testing.T) {
	s, _, fm, url, id := fanBridge(t, `,"caps":{"maxFanoutPerMinute":1}`)
	endpoint := url + "/v1/bridges/" + id + "/messages"
	before := len(channel(t, s, id))
	for _, c := range []struct {
		body   string
		status int
		want   string
	}{
		{`{"files":[{"name":"empty","data":""}]}`, http.StatusBadRequest, "is empty"},
		{`{"files":[{"name":"bad","data":"!not-base64!"}]}`, http.StatusBadRequest, ""},
		{`{"files":[{"name":"../bad","data":"aGk="}]}`, http.StatusBadRequest, "without a path"},
		{`{"files":[{"name":"..","data":"aGk="}]}`, http.StatusBadRequest, "without a path"},
		{`{"files":[{"name":"a\u0007b","data":"aGk="}]}`, http.StatusBadRequest, "control character"},
		{`{"files":[` + strings.TrimSuffix(strings.Repeat(`{"name":"a","data":"aGk="},`, fw.MaxAttachments+1), ",") + `]}`, http.StatusBadRequest, "at most 20 files"},
		{`{"text":"Keep this","pinned":true,"files":[{"name":"a","data":"aGk="}]}`, http.StatusBadRequest, "take no files"},
		{`{"skill":{"name":"review"},"files":[{"name":"a","data":"aGk="}]}`, http.StatusBadRequest, "take no files"},
		{`{"text":"x","files":[{"name":"a","data":"aGk="}],"ask":true}`, http.StatusBadRequest, "unknown field"},
	} {
		resp, raw := do(t, http.MethodPost, endpoint, "alice", c.body)
		if resp.StatusCode != c.status || !strings.Contains(raw, c.want) {
			t.Errorf("%.60s = %d %s", c.body, resp.StatusCode, raw)
		}
	}
	file := `{"files":[{"name":"note.txt","data":"aGk="}]}`
	if resp, raw := do(t, http.MethodPost, endpoint, "carol", file); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("a caller who cannot use the bridge = %d %s", resp.StatusCode, raw)
	}
	// One delivery fits the minute, and two do not: the whole post fails.
	resp, raw := do(t, http.MethodPost, endpoint, "alice", file)
	if resp.StatusCode != http.StatusConflict || !strings.Contains(raw, "a cap of the bridge holds the message back") {
		t.Fatalf("capped = %d %s", resp.StatusCode, raw)
	}
	if len(channel(t, s, id)) != before || len(fm.find(http.MethodPost, pathMig))+len(fm.find(http.MethodPost, pathRev)) != 0 || s.held.Load() != 0 {
		t.Fatal("a refused post changed the channel, delivered to a member, or kept its bytes")
	}
	// The refused post consumed no cap, so a post to one member fits.
	resp, raw = do(t, http.MethodPost, endpoint, "alice", `{"to":["reviewer"],"files":[{"name":"note.txt","data":"aGk="}]}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("a post to one member = %d %s", resp.StatusCode, raw)
	}
	// The size of the files counts before the fan-out.
	if _, err := describeFiles([]native.FileBody{{Name: "large", Data: make([]byte, fw.MaxAttachmentBytes+1)}}); !errIs(err, ErrTooLarge) {
		t.Fatalf("files over the bound = %v", err)
	}
	if _, err := describeFiles([]native.FileBody{{Name: "a", Data: make([]byte, fw.MaxAttachmentBytes/2)}, {Name: "b", Data: make([]byte, fw.MaxAttachmentBytes/2)}}); err != nil {
		t.Fatalf("files at the bound = %v", err)
	}
}

// A file larger than the workspace of a member takes, the
// tools.files.maxFileBytes of its workers, answers 413 before the fan-out:
// the kernel of the member would refuse it after the post. The post
// changes nothing, and a file at the limit goes out.
func TestAttachmentOverTheFileLimitOfAMember(t *testing.T) {
	s, _, fm, url, id := fanBridge(t, "")
	endpoint := url + "/v1/bridges/" + id + "/messages"
	before := len(channel(t, s, id))
	post := func(size int) (*http.Response, string) {
		body, _ := json.Marshal(PostSpec{To: []string{"reviewer"}, Files: []native.FileBody{{Name: "big.bin", Data: bytes.Repeat([]byte{7}, size)}}})
		return do(t, http.MethodPost, endpoint, "alice", string(body))
	}
	resp, raw := post(9 << 20)
	if resp.StatusCode != http.StatusRequestEntityTooLarge || !strings.Contains(raw, "big.bin") || !strings.Contains(raw, "holds 9 MiB, and reviewer takes files of at most 8 MiB (tools.files.maxFileBytes of its workers)") {
		t.Fatalf("a file over the limit of the member = %d %s", resp.StatusCode, raw)
	}
	if len(channel(t, s, id)) != before || len(fm.find(http.MethodPost, pathRev)) != 0 || s.held.Load() != 0 {
		t.Fatal("a refused post changed the channel, delivered to a member, or kept its bytes")
	}
	if resp, raw := post(fw.DefaultMaxFileBytes); resp.StatusCode != http.StatusCreated {
		t.Fatalf("a file at the limit = %d %.300s", resp.StatusCode, raw)
	}
	eventually(t, "the reviewer gets the file at the limit", func() bool { return len(fm.find(http.MethodPost, pathRev)) == 1 })
}

// A body over the bound of the files answers 413, and a body without files
// keeps the bound of a body without files.
func TestAttachmentBodyBounds(t *testing.T) {
	_, _, _, url, id := fanBridge(t, "")
	endpoint := url + "/v1/bridges/" + id + "/messages"
	big := strings.Repeat("A", native.MaxFilesBodyBytes)
	if resp, raw := do(t, http.MethodPost, endpoint, "alice", `{"files":[{"name":"big","data":"`+big+`"}]}`); resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("a body over the bound of files = %d %.200s", resp.StatusCode, raw)
	}
	text := strings.Repeat("a", native.MaxBodyBytes)
	if resp, raw := do(t, http.MethodPost, endpoint, "alice", `{"text":"`+text+`"}`); resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("a body over the bound without files = %d %.200s", resp.StatusCode, raw)
	}
}

// A member without a run, a member whose run failed, and a message that
// reaches no member refuse files with what to do, and nothing seats.
func TestAttachmentNeedsARunOnEveryMember(t *testing.T) {
	a := newAgent(t, nil)
	s := newStore(t, a, nil)
	v, fm := withFleet(t, a)
	v.state("default/crew-0", fleet.MemberLive, "migrator", "reviewer")
	url := serve(t, a)
	b := open(t, url)
	endpoint := url + "/v1/bridges/" + b.ID + "/messages"
	file := `"files":[{"name":"note.txt","data":"aGk="}]`
	seatings := func() map[string]int {
		rec, _, err := s.b.get(context.Background(), b.ID)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]int{}
		for h, sm := range rec.Seats {
			if sm != nil {
				out[h] = sm.Seatings
			}
		}
		return out
	}
	before, seated := len(channel(t, s, b.ID)), seatings()
	resp, raw := do(t, http.MethodPost, endpoint, "alice", `{`+file+`}`)
	if resp.StatusCode != http.StatusConflict || !strings.Contains(raw, "migrator-1 and reviewer have none yet: send them once they work") {
		t.Fatalf("no runs = %d %s", resp.StatusCode, raw)
	}
	if got := seatings(); len(channel(t, s, b.ID)) != before || !maps.Equal(got, seated) {
		t.Fatalf("a refused post changed the channel or seated a member: %v, was %v", got, seated)
	}
	v.put(workerRun("r-mig", b.ID, "migrator-1", "migrator", fw.StateCompleted))
	failed := workerRun("r-rev", b.ID, "reviewer", "reviewer", fw.StateFailed)
	failed.Error = "model: the provider refused"
	v.put(failed)
	s.poll(context.Background())
	before = len(channel(t, s, b.ID))
	resp, raw = do(t, http.MethodPost, endpoint, "alice", `{`+file+`}`)
	if resp.StatusCode != http.StatusConflict || !strings.Contains(raw, "the run of reviewer failed: give it a new task, or address another member") {
		t.Fatalf("a failed run = %d %s", resp.StatusCode, raw)
	}
	if got := channel(t, s, b.ID); len(got) != before {
		t.Fatalf("a refused post added entries: %+v", got[before:])
	}
	if _, err := s.b.update(context.Background(), b.ID, func(rec *record) error {
		for i := range rec.Members {
			rec.Members[i].Muted = true
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if resp, raw := do(t, http.MethodPost, endpoint, "alice", `{`+file+`}`); resp.StatusCode != http.StatusConflict || !strings.Contains(raw, "reaches no member") {
		t.Fatalf("no member = %d %s", resp.StatusCode, raw)
	}
	// The member with a run takes the files when the message names it.
	if resp, raw := do(t, http.MethodPost, endpoint, "alice", `{"to":["migrator-1"],`+file+`}`); resp.StatusCode != http.StatusCreated {
		t.Fatalf("to a member with a run = %d %s", resp.StatusCode, raw)
	}
	eventually(t, "the member gets the file", func() bool { return len(fm.find(http.MethodPost, pathMig)) == 1 })
	if got := seatings(); !maps.Equal(got, seated) || len(fm.find(http.MethodPost, "/v1/runs")) != 0 {
		t.Fatalf("a post with files seated a member: %v, was %v", got, seated)
	}
}

// Files over the bound of the replica wait for the earlier files: the post
// answers 409 and keeps nothing.
func TestAttachmentHeldBound(t *testing.T) {
	s, _, fm, url, id := fanBridge(t, "")
	s.held.Store(maxHeldBytes - 1)
	resp, raw := do(t, http.MethodPost, url+"/v1/bridges/"+id+"/messages", "alice", `{"files":[{"name":"note.txt","data":"aGk="}]}`)
	if resp.StatusCode != http.StatusConflict || !strings.Contains(raw, "send these files in a minute") || s.held.Load() != maxHeldBytes-1 {
		t.Fatalf("over the bound = %d %s, held %d", resp.StatusCode, raw, s.held.Load())
	}
	s.held.Store(0)
	if resp, raw := do(t, http.MethodPost, url+"/v1/bridges/"+id+"/messages", "alice", `{"files":[{"name":"note.txt","data":"aGk="}]}`); resp.StatusCode != http.StatusCreated {
		t.Fatalf("under the bound = %d %s", resp.StatusCode, raw)
	}
	eventually(t, "both members get the file", func() bool {
		return len(fm.find(http.MethodPost, pathMig)) == 1 && len(fm.find(http.MethodPost, pathRev)) == 1
	})
	eventually(t, "the replica holds no bytes", func() bool { return s.held.Load() == 0 })
}

// A delivery that fails with an answer that can go away sends the same
// files again, and a delivery that gives up gives the bytes back too.
func TestAttachmentRetry(t *testing.T) {
	s, _, fm, url, id := fanBridge(t, "")
	fm.mu.Lock()
	fm.fail[pathRev] = 2
	fm.status[pathMig] = http.StatusBadRequest
	fm.detail[pathMig] = `the file "note.txt" is not accepted: the file is larger than 8 MiB`
	fm.mu.Unlock()
	resp, raw := do(t, http.MethodPost, url+"/v1/bridges/"+id+"/messages", "alice", `{"text":"Read it","files":[{"name":"note.txt","data":"aGk="}]}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("post = %d %s", resp.StatusCode, raw)
	}
	eventually(t, "the reviewer gets the file on the third try", func() bool { return len(fm.find(http.MethodPost, pathRev)) == 3 })
	for _, g := range fm.find(http.MethodPost, pathRev) {
		if f := deliveredFiles(t, g); len(f) != 1 || string(f[0].Data) != "hi" {
			t.Fatalf("a try = %+v", f)
		}
	}
	eventually(t, "the channel says that the migrator did not get the message", func() bool {
		for _, m := range withEvent(channel(t, s, id), EventFailed) {
			if strings.Contains(m.Text, "migrator-1 did not get message") && strings.Contains(m.Text, "larger than 8 MiB") {
				return true
			}
		}
		return false
	})
	eventually(t, "the replica holds no bytes", func() bool { return s.held.Load() == 0 })
}

// A seat whose run lives in the front itself gets the file in its
// workspace: the ledger holds a reference without the bytes, and no event
// of the egress carries them.
func TestAttachmentReachesLocalWorkspace(t *testing.T) {
	root := t.TempDir()
	a := newAgent(t, func(c *fw.Config) {
		c.Tools.Files.Root = root
		c.Bus.Egress = "fw.events"
	})
	var mu sync.Mutex
	var egress []string
	stop, err := a.Bus().Subscribe(context.Background(), "fw.events.>", func(e fw.Event) error {
		mu.Lock()
		defer mu.Unlock()
		raw, _ := json.Marshal(e)
		egress = append(egress, string(raw))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stop)
	newStore(t, a, nil)
	url := serve(t, a)
	b := open(t, url)
	ctx := context.Background()
	r, err := a.Start(ctx, "Start", fw.AsAgent("migrator"), fw.WithMetadata(map[string]string{fw.MetaBatch: BatchOf(b.ID), MetaMember: "migrator-1"}))
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "the first turn ends", func() bool { got, _ := a.Get(ctx, r.ID); return got != nil && got.State == fw.StateCompleted })
	data := []byte("name,role\nAlice,reviewer\nBob,migrator\n")
	encoded := base64.StdEncoding.EncodeToString(data)
	body := `{"to":["migrator-1"],"files":[{"name":"team.csv","data":"` + encoded + `"}]}`
	resp, raw := do(t, http.MethodPost, url+"/v1/bridges/"+b.ID+"/messages", "alice", body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("post = %d %s", resp.StatusCode, raw)
	}
	var ref *fw.FileRef
	eventually(t, "the ledger holds the reference of the file", func() bool {
		for e, err := range a.Ledger().Read(ctx, r.ID, 0) {
			if err != nil {
				return false
			}
			var d fw.MessageReceivedData
			if e.Type != fw.EvMessageReceived || e.Decode(&d) != nil {
				continue
			}
			for _, block := range d.Content {
				if block.Type == fw.BlockFile && block.File != nil {
					ref = block.File
					return len(block.Data) == 0 && d.From != nil && d.From.Name == "alice"
				}
			}
		}
		return false
	})
	if ref.Name != "team.csv" || !strings.HasPrefix(ref.Path, fw.AttachmentsDir+"/") && !strings.Contains(ref.Path, "/"+fw.AttachmentsDir+"/") {
		t.Fatalf("the reference = %+v", ref)
	}
	got, err := os.ReadFile(filepath.Join(root, ref.Path))
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("the bytes in the workspace = %q, %v", got, err)
	}
	eventually(t, "the egress carries the message", func() bool {
		mu.Lock()
		defer mu.Unlock()
		for _, e := range egress {
			if strings.Contains(e, fw.EvMessageReceived) && strings.Contains(e, "team.csv") {
				return true
			}
		}
		return false
	})
	mu.Lock()
	defer mu.Unlock()
	for _, e := range egress {
		if strings.Contains(e, encoded) || strings.Contains(e, "Alice,reviewer") {
			t.Fatalf("an event of the egress carries the bytes: %.300s", e)
		}
	}
}

// The bytes of a file reach the workspace of a seat run on another member:
// the front streams the body through the forward of the fleet, and the
// kernel of the member keeps the file under attachments/ with a reference
// in its ledger, from the person who sent it.
func TestAttachmentReachesARemoteWorkspace(t *testing.T) {
	root := t.TempDir()
	worker := newAgentWith(t, func(c *fw.Config) {
		c.Tools.Files.Root = root
		c.Access.Admins = append(c.Access.Admins, "kind:token")
	}, fw.WithAuthn(tokenMap{fleetToken: {Subject: fw.KindToken, Kind: fw.KindToken}}))
	ctx := context.Background()
	r, err := worker.Start(ctx, "Review the roles", fw.AsAgent("reviewer"))
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "the run of the worker ends its turn", func() bool { got, _ := worker.Get(ctx, r.ID); return got != nil && got.State == fw.StateCompleted })
	a := newAgent(t, nil)
	s := newStore(t, a, nil)
	v := newView(serve(t, worker))
	fleet.Attach(a, v)
	t.Cleanup(func() { fleet.Attach(a, nil) })
	v.state("default/crew-0", fleet.MemberLive, "migrator", "reviewer")
	url := serve(t, a)
	b := open(t, url)
	v.put(workerRun(r.ID, b.ID, "reviewer", "reviewer", fw.StateCompleted))
	s.poll(ctx)
	data := bytes.Repeat([]byte("0123456789abcdef\x00\xff"), 120000) // 2.1 MB
	body, _ := json.Marshal(PostSpec{Text: "The capture", To: []string{"reviewer"}, Files: []native.FileBody{{Name: "capture.bin", Data: data}}})
	if resp, raw := do(t, http.MethodPost, url+"/v1/bridges/"+b.ID+"/messages", "alice", string(body)); resp.StatusCode != http.StatusCreated {
		t.Fatalf("post = %d %.300s", resp.StatusCode, raw)
	}
	var ref *fw.FileRef
	var from *fw.MessageFrom
	eventually(t, "the ledger of the worker holds the reference of the file", func() bool {
		for e, err := range worker.Ledger().Read(ctx, r.ID, 0) {
			if err != nil {
				return false
			}
			var d fw.MessageReceivedData
			if e.Type != fw.EvMessageReceived || e.Decode(&d) != nil {
				continue
			}
			for _, block := range d.Content {
				if block.Type == fw.BlockFile && block.File != nil && len(block.Data) == 0 {
					ref, from = block.File, d.From
				}
			}
		}
		return ref != nil
	})
	if from == nil || from.Kind != fw.FromPerson || from.Name != "alice" || from.Member != "reviewer" || ref.Size != int64(len(data)) {
		t.Fatalf("the message reached the worker as %+v with %+v", from, ref)
	}
	got, err := os.ReadFile(filepath.Join(root, ref.Path))
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("the bytes in the workspace of the worker = %d bytes, %v", len(got), err)
	}
	eventually(t, "the replica holds no bytes", func() bool { return s.held.Load() == 0 })
}

// The body of a follow-up with files streams the base64 of the shared
// bytes: its length is the length that it reads, and the kernel decodes it
// into the same fields and bytes.
func TestFilesBody(t *testing.T) {
	var files []native.FileBody
	for i, n := range []int{1, 2, 3, 3071, 3072, 3073, 10000} {
		d := make([]byte, n)
		for j := range d {
			d[j] = byte(i*7 + j)
		}
		files = append(files, native.FileBody{Name: []string{`a "quoted" name`, "naïve.txt", "x", "y.bin", "z", "日本.csv", "</message>"}[i], Data: d})
	}
	for _, fields := range []map[string]any{{"content": "hi", "pinned": true, "from": fw.MessageFrom{Kind: fw.FromPerson, Name: "alice"}}, {}} {
		fb, err := newFilesBody(fields, files)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := io.ReadAll(fb.Reader())
		if err != nil || int64(len(raw)) != fb.Len() {
			t.Fatalf("read %d bytes of %d: %v", len(raw), fb.Len(), err)
		}
		var got struct {
			Content string            `json:"content"`
			Pinned  bool              `json:"pinned"`
			Files   []native.FileBody `json:"files"`
		}
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&struct {
			Content *string          `json:"content"`
			Pinned  *bool            `json:"pinned"`
			From    *fw.MessageFrom  `json:"from"`
			Files   *json.RawMessage `json:"files"`
		}{}); err != nil {
			t.Fatalf("the body is not the JSON of the route: %v\n%.300s", err, raw)
		}
		if err := json.Unmarshal(raw, &got); err != nil || got.Content != fields["content"] && fields["content"] != nil {
			t.Fatalf("decode = %v, %+v", err, got)
		}
		if len(got.Files) != len(files) {
			t.Fatalf("files = %d", len(got.Files))
		}
		for i := range files {
			if got.Files[i].Name != files[i].Name || !bytes.Equal(got.Files[i].Data, files[i].Data) {
				t.Fatalf("file %d = %q, %d bytes", i, got.Files[i].Name, len(got.Files[i].Data))
			}
		}
		// A second reader reads the same body, as a retry of the transport does.
		again, _ := io.ReadAll(fb.Reader())
		if !bytes.Equal(again, raw) {
			t.Fatal("a second reader reads another body")
		}
	}
}

// bridge_read names the files of an entry for an agent.
func TestAttachedText(t *testing.T) {
	if attachedText(nil) != "" {
		t.Fatal("an entry without files got a line")
	}
	got := attachedText([]AttachedFile{{Name: "team.csv", Size: 25}, {Name: "plan.pdf", Size: 2_100_000}})
	if want := "\n[Files: team.csv (25 bytes), plan.pdf (2.1 MB). Each run that got this message has them under attachments/ in its workspace.]"; got != want {
		t.Fatalf("got %q", got)
	}
	if andList([]string{"a", "b", "c"}) != "a, b, and c" || andList([]string{"a", "b"}) != "a and b" {
		t.Fatal("andList")
	}
}
