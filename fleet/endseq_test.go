package fleet

import (
	"encoding/json"
	"testing"
)

// A thumb on the run as a whole goes on the end of the run, and so does
// the undo of such a thumb, where the thumb is. A heart, a thumb on an
// event, and a run without an end keep the body, and the end is read only
// when needed.
func TestAtEnd(t *testing.T) {
	reads := 0
	end := func() uint64 { reads++; return 12 }
	seqOf := func(b []byte) uint64 {
		var d struct {
			Seq uint64 `json:"seq"`
		}
		if err := json.Unmarshal(b, &d); err != nil {
			t.Fatal(err)
		}
		return d.Seq
	}
	for body, want := range map[string]uint64{
		`{"reaction":"up"}`:                      12,
		`{"Seq":0,"reaction":"down","note":"x"}`: 12,
		`{"reaction":"heart"}`:                   0,
		`{"seq":4,"reaction":"up"}`:              4,
		`{"reaction":"up","remove":true}`:        12,
	} {
		if got := seqOf(AtEnd([]byte(body), end)); got != want {
			t.Errorf("AtEnd(%s) has seq %d, want %d", body, got, want)
		}
	}
	if reads != 3 {
		t.Fatalf("the end was read %d times, want 3", reads)
	}
	var note struct {
		Note string `json:"note"`
	}
	if err := json.Unmarshal(AtEnd([]byte(`{"reaction":"down","note":"Name the release."}`), end), &note); err != nil || note.Note != "Name the release." {
		t.Fatalf("the note = %+v, %v", note, err)
	}
	if b := AtEnd([]byte(`{"reaction":"up"}`), func() uint64 { return 0 }); string(b) != `{"reaction":"up"}` {
		t.Fatalf("a run without an end = %s", b)
	}
}
