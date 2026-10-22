package faces

import (
	"context"
	"sync"
	"testing"

	"github.com/bdubs00/featherweight-workbench/internal/bucket"
)

// A new SVG, a new display name, and a removal of the SVG call the hooks
// with the entry after the change; a change that changes nothing and a
// refused SVG call nothing.
func TestOnChange(t *testing.T) {
	var mu sync.Mutex
	var got []Face
	OnChange(func(f Face) {
		mu.Lock()
		got = append(got, f)
		mu.Unlock()
	})
	t.Cleanup(func() {
		hooks.mu.Lock()
		hooks.change = nil
		hooks.mu.Unlock()
	})
	s := New(bucket.NewMem(bucketConfig), quiet())
	ctx := context.Background()
	put, err := s.Put(ctx, "reviewer", []byte(fox), "alice")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Put(ctx, "reviewer", []byte(fox), "alice"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Put(ctx, "reviewer", []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`), "alice"); err == nil {
		t.Fatal("a face with a script landed")
	}
	named, err := s.Meta(ctx, "reviewer", "Rita", "Reads every diff", "alice")
	if err != nil {
		t.Fatal(err)
	}
	gone, err := s.Delete(ctx, "reviewer", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Meta(ctx, "reviewer", "", "", "alice"); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 4 {
		t.Fatalf("the hooks got %d faces, want 4: %+v", len(got), got)
	}
	if got[0].Hash != put.Hash || !got[0].Custom || got[1].Hash != named.Hash || got[1].DisplayName != "Rita" {
		t.Errorf("the first two = %+v", got[:2])
	}
	if got[2].Hash != gone.Hash || got[2].Custom || got[2].DisplayName != "Rita" {
		t.Errorf("the removal of the SVG = %+v", got[2])
	}
	if got[3].Custom || got[3].DisplayName != "" || got[3].Agent != "reviewer" {
		t.Errorf("the removal of the entry = %+v", got[3])
	}
}
