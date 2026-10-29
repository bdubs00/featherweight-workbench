package heldback

import (
	"fmt"
	"testing"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// The choice is stable for an id, picks about the share of many ids as
// comparison runs, and none at a share of 0.
func TestPick(t *testing.T) {
	const n = 20000
	for _, share := range []float64{0, 0.05, 0.5, 1} {
		held := 0
		for i := range n {
			id := fmt.Sprintf("01RUN%06d", i)
			got := Pick(id, share)
			if got != Pick(id, share) {
				t.Fatalf("Pick(%s) changed between two calls", id)
			}
			if got {
				held++
			}
		}
		lo, hi := (share-0.01)*n, (share+0.01)*n
		if share == 0 && held != 0 || share == 1 && held != n || float64(held) < lo || float64(held) > hi {
			t.Errorf("share %g picked %d of %d as comparison runs", share, held, n)
		}
	}
	if Pick("", 1) {
		t.Error("an empty id is a comparison run")
	}
}

// Mark adds fw.knowhow: off to a request that Pick picks as a comparison
// run, and leaves the others as they are.
func TestMark(t *testing.T) {
	held, kept := fw.RunRequest{}, fw.RunRequest{}
	for i := 0; held.ID == "" || kept.ID == ""; i++ {
		id := fmt.Sprintf("01RUN%06d", i)
		if Pick(id, 0.05) {
			held.ID = id
		} else {
			kept.ID = id
		}
	}
	kept.Metadata = map[string]string{"a": "b"}
	if !Mark(&held, 0.05) || held.Metadata[fw.MetaKnowhow] != fw.KnowhowOff {
		t.Fatalf("the request of a comparison run = %+v", held)
	}
	if Mark(&kept, 0.05) || len(kept.Metadata) != 1 {
		t.Fatalf("a normal request = %+v", kept)
	}
}
