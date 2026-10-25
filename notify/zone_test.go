package notify

import (
	"context"
	"testing"
)

// The duties read the time zone that a person saved with the
// notifications, and nothing for a person who saved none.
func TestSavedZone(t *testing.T) {
	f := newFront(t, "", false)
	if z := savedZone(context.Background(), f.a, "alice"); z != "" {
		t.Fatalf("the zone of a person without settings = %q", z)
	}
	f.set(t, "alice", Body{QuietHours: &QuietHours{From: "22:00", To: "07:00", TimeZone: "America/Chicago"}})
	if z := savedZone(context.Background(), f.a, "alice"); z != "America/Chicago" {
		t.Fatalf("the saved zone = %q", z)
	}
}
