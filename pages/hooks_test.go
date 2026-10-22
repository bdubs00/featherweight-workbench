package pages

import (
	"context"
	"sync"
	"testing"
)

// change is one call of the OnChange hooks.
type change struct {
	info    PageInfo
	deleted bool
}

// hooked records the calls of the OnChange hooks for the length of a test.
func hooked(t *testing.T) func() []change {
	t.Helper()
	var mu sync.Mutex
	var got []change
	OnChange(func(info PageInfo, deleted bool) {
		mu.Lock()
		got = append(got, change{info, deleted})
		mu.Unlock()
	})
	t.Cleanup(func() {
		hooks.mu.Lock()
		hooks.change = nil
		hooks.mu.Unlock()
	})
	return func() []change {
		mu.Lock()
		defer mu.Unlock()
		return append([]change(nil), got...)
	}
}

// A write that creates or changes a page and a removal call the hooks
// once each; a write of the same text, a refused write, and the index call
// nothing.
func TestOnChange(t *testing.T) {
	got := hooked(t)
	s := memStore(Config{})
	ctx := context.Background()
	first := mustWrite(t, s, "notes/plan.md", "# Plan\n\nOne.\n", IfAny)
	mustWrite(t, s, "notes/plan.md", "# Plan\n\nOne.\n", IfAny)
	second := mustWrite(t, s, "notes/plan.md", "# Plan\n\nTwo.\n", first.Hash)
	if _, err := s.Write(ctx, "notes/plan.md", "# Plan\n\nThree.\n", "alice", "", first.Hash); err == nil {
		t.Fatal("a stale write landed")
	}
	if err := s.Delete(ctx, "notes/plan.md", "bob", ""); err != nil {
		t.Fatal(err)
	}
	list := got()
	if len(list) != 3 {
		t.Fatalf("the hooks got %d changes, want 3: %+v", len(list), list)
	}
	if list[0].deleted || list[0].info.Path != "notes/plan.md" || list[0].info.Hash != first.Hash || list[0].info.Title != "Plan" {
		t.Errorf("create = %+v", list[0])
	}
	if list[1].deleted || list[1].info.Hash != second.Hash {
		t.Errorf("change = %+v", list[1])
	}
	if !list[2].deleted || list[2].info.Path != "notes/plan.md" || list[2].info.By != "bob" || list[2].info.Hash != second.Hash || list[2].info.At.IsZero() {
		t.Errorf("removal = %+v", list[2])
	}
	for _, c := range list {
		if c.info.Path == IndexPath {
			t.Errorf("the index reached the hooks: %+v", c)
		}
	}
}
