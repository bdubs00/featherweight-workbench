package bridges

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"runtime"
	"runtime/debug"
	"strings"
	"testing"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/surface/native"
)

// decodeOf runs decodePost on body and returns the spec, the status, and
// the answer.
func decodeOf(t *testing.T, body string) (PostSpec, int, string) {
	t.Helper()
	var spec PostSpec
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/bridges/b/messages", strings.NewReader(body))
	if !decodePost(w, r, &spec) {
		return spec, w.Code, w.Body.String()
	}
	return spec, http.StatusOK, ""
}

// The decode of a post refuses what the decoder of the kernel refuses: an
// unknown key at any depth, data after the object, and an empty body.
func TestDecodePost(t *testing.T) {
	spec, code, raw := decodeOf(t, `{"text":"hi","to":["reviewer"],"skill":{"name":"review","args":"x"},"files":[{"name":"a.txt","data":"aGk="}]}`)
	if code != http.StatusOK || spec.Text != "hi" || spec.Skill == nil || spec.Skill.Args != "x" || len(spec.Files) != 1 || string(spec.Files[0].Data) != "hi" {
		t.Fatalf("a valid post = %d %s %+v", code, raw, spec)
	}
	// A key matches a field without regard to case, as in encoding/json.
	if _, code, raw := decodeOf(t, `{"Text":"hi"}`); code != http.StatusOK {
		t.Fatalf("a key in another case = %d %s", code, raw)
	}
	for body, want := range map[string]string{
		`{"text":"hi","ask":true}`:                        `json: unknown field \"ask\"`,
		`{"files":[{"name":"a","data":"aGk=","size":2}]}`: `json: unknown field \"size\"`,
		`{"skill":{"name":"review","argz":"x"}}`:          `json: unknown field \"argz\"`,
		`{"text":"hi"} {"text":"again"}`:                  "the body holds data after the JSON object",
		"  \n ":                                           "the body is empty; send a JSON object",
		`{"text":`:                                        "the body is not valid",
		`{"files":[{"name":"a","data":"!not base64!"}]}`:  "the body is not valid",
		`{"to":"reviewer"}`:                               "the body is not valid",
	} {
		if _, code, raw := decodeOf(t, body); code != http.StatusBadRequest || !strings.Contains(raw, want) {
			t.Errorf("%s = %d %s, want %s", body, code, raw, want)
		}
	}
}

// A post of 32 MiB of files holds the body and the files once each while
// it decodes: well under the decoder of the kernel, which reads through a
// buffer that grows by doubling.
func TestDecodePostMemory(t *testing.T) {
	data := bytes.Repeat([]byte{1, 2, 3, 250}, fw.MaxAttachmentBytes/4)
	body, _ := json.Marshal(PostSpec{To: []string{"reviewer"}, Files: []native.FileBody{{Name: "a.bin", Data: data}}})
	allocated := func(f func()) uint64 {
		runtime.GC()
		old := debug.SetGCPercent(-1)
		defer debug.SetGCPercent(old)
		var a, b runtime.MemStats
		runtime.ReadMemStats(&a)
		f()
		runtime.ReadMemStats(&b)
		return b.TotalAlloc - a.TotalAlloc
	}
	ours := allocated(func() {
		var spec PostSpec
		r := httptest.NewRequest(http.MethodPost, "/v1/bridges/b/messages", bytes.NewReader(body))
		if !decodePost(httptest.NewRecorder(), r, &spec) || len(spec.Files) != 1 || !bytes.Equal(spec.Files[0].Data, data) {
			t.Fatal("the post did not decode")
		}
	})
	kernel := allocated(func() {
		var spec PostSpec
		dec := json.NewDecoder(bytes.NewReader(body))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&spec); err != nil {
			t.Fatal(err)
		}
	})
	if limit := uint64(len(body) + len(data) + 8<<20); ours > limit || ours*2 > kernel {
		t.Fatalf("the decode allocated %d MB for a body of %d MB (bound %d MB); the decoder of the kernel %d MB", ours>>20, len(body)>>20, limit>>20, kernel>>20)
	}
}
