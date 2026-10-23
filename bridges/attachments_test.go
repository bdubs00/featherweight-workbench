package bridges

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/surface/native"
)

func TestAttachmentFanout(t *testing.T) {
	s, _, fm, url, id := fanBridge(t, "")
	// This exceeds the ordinary JSON body limit and preserves binary bytes.
	data := bytes.Repeat([]byte{0, 255, 1, 128}, 300000)
	body, _ := json.Marshal(PostSpec{To: []string{"reviewer"}, Files: []native.FileBody{{Name: "sample.bin", Data: data}}})
	resp, raw := do(t, http.MethodPost, url+"/v1/bridges/"+id+"/messages", "alice", string(body))
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("post = %d %s", resp.StatusCode, raw)
	}
	var msg Message
	decodeJSON(t, raw, &msg)
	if len(msg.Files) != 1 || msg.Files[0].Size != int64(len(data)) || msg.Files[0].Name != "sample.bin" || strings.Contains(raw, `"data"`) {
		t.Fatalf("metadata = %s", raw)
	}
	eventually(t, "the reviewer gets the file", func() bool { return len(fm.find(http.MethodPost, pathRev)) == 1 })
	call := fm.find(http.MethodPost, pathRev)[0]
	encoded, _ := json.Marshal(call.Body)
	var delivered struct {
		Files []native.FileBody `json:"files"`
	}
	decodeJSON(t, string(encoded), &delivered)
	if len(delivered.Files) != 1 || !bytes.Equal(delivered.Files[0].Data, data) || call.Auth != "Bearer "+fleetToken || len(fm.find(http.MethodPost, pathMig)) != 0 {
		t.Fatal("the addressed delivery changed its files, authorization, or recipients")
	}
	stored := channel(t, s, id)
	last := stored[len(stored)-1]
	if len(last.Files) != 1 || last.Files[0] != msg.Files[0] {
		t.Fatalf("the channel lost the metadata: %+v", last)
	}
	// A broadcast reaches both seats with the same file.
	resp, raw = do(t, http.MethodPost, url+"/v1/bridges/"+id+"/messages", "bob", `{"text":"Both","files":[{"name":"note.txt","data":"aGk="}]}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("broadcast = %d %s", resp.StatusCode, raw)
	}
	eventually(t, "both seats get the file", func() bool {
		return len(fm.find(http.MethodPost, pathRev)) == 2 && len(fm.find(http.MethodPost, pathMig)) == 1
	})
}

func TestAttachmentRefusals(t *testing.T) {
	s, _, fm, url, id := fanBridge(t, `,"caps":{"maxFanoutPerMinute":1}`)
	endpoint := url + "/v1/bridges/" + id + "/messages"
	before := len(channel(t, s, id))
	for _, body := range []string{
		`{"files":[{"name":"empty","data":""}]}`,
		`{"files":[{"name":"bad","data":"!not-base64!"}]}`,
		`{"files":[{"name":"../bad","data":"aGk="}]}`,
		`{"files":[` + strings.TrimSuffix(strings.Repeat(`{"name":"a","data":"aGk="},`, 21), ",") + `]}`,
	} {
		resp, raw := do(t, http.MethodPost, endpoint, "alice", body)
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("invalid upload = %d %s", resp.StatusCode, raw)
		}
	}
	file := `{"files":[{"name":"note.txt","data":"aGk="}]}`
	resp, raw := do(t, http.MethodPost, endpoint, "carol", file)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("unauthorized = %d %s", resp.StatusCode, raw)
	}
	// One delivery fits, but two do not: refuse the whole attachment post.
	resp, raw = do(t, http.MethodPost, endpoint, "alice", file)
	if resp.StatusCode != http.StatusConflict || !strings.Contains(raw, "caps") {
		t.Fatalf("capped = %d %s", resp.StatusCode, raw)
	}
	if len(channel(t, s, id)) != before || len(fm.find(http.MethodPost, pathMig))+len(fm.find(http.MethodPost, pathRev)) != 0 {
		t.Fatal("a refused upload changed the channel or delivered to a subset")
	}
	// The refused post consumed no cap: an addressed send still fits.
	resp, raw = do(t, http.MethodPost, endpoint, "alice", `{"to":["reviewer"],"files":[{"name":"note.txt","data":"aGk="}]}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("retry = %d %s", resp.StatusCode, raw)
	}
	// Size validation precedes all fan-out.
	_, err := describeFiles([]native.FileBody{{Name: "large", Data: make([]byte, fw.MaxAttachmentBytes+1)}})
	if err == nil {
		t.Fatal("oversized files accepted")
	}
}

func TestAttachmentWaitsForARecipientRun(t *testing.T) {
	a := newAgent(t, nil)
	s := newStore(t, a, nil)
	url := serve(t, a)
	b := open(t, url)
	before := len(channel(t, s, b.ID))
	resp, raw := do(t, http.MethodPost, url+"/v1/bridges/"+b.ID+"/messages", "alice", `{"files":[{"name":"note.txt","data":"aGk="}]}`)
	if resp.StatusCode != http.StatusConflict || len(channel(t, s, b.ID)) != before {
		t.Fatalf("queued recipient = %d %s", resp.StatusCode, raw)
	}
}

func TestAttachmentReachesLocalWorkspace(t *testing.T) {
	root := t.TempDir()
	a := newAgent(t, func(c *fw.Config) { c.Tools.Files.Root = root })
	newStore(t, a, nil)
	url := serve(t, a)
	b := open(t, url)
	ctx := context.Background()
	r, err := a.Start(ctx, "Start", fw.AsAgent("migrator"), fw.WithMetadata(map[string]string{fw.MetaBatch: BatchOf(b.ID), MetaMember: "migrator-1"}))
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "the first turn ends", func() bool { got, _ := a.Get(ctx, r.ID); return got != nil && got.State == fw.StateCompleted })
	data := []byte("name,role\nAlice,reviewer\n")
	body := `{"to":["migrator-1"],"files":[{"name":"team.csv","data":"` + base64.StdEncoding.EncodeToString(data) + `"}]}`
	resp, raw := do(t, http.MethodPost, url+"/v1/bridges/"+b.ID+"/messages", "alice", body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("post = %d %s", resp.StatusCode, raw)
	}
	var ref *fw.FileRef
	eventually(t, "the ledger holds the attachment reference", func() bool {
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
	got, err := os.ReadFile(filepath.Join(root, ref.Path))
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("workspace bytes = %q, %v", got, err)
	}
}
