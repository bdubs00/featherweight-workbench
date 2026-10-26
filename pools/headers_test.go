package pools

import "testing"

// The first lines of the marker and of the fw.yaml of a worker are read by
// the front and are part of the hash of a pool, so their words never
// change. A change of them made every pool hold on 2026-10-26.
func TestHeadersStayTheSame(t *testing.T) {
	if got, want := markerHeader("p"), "# This file is written by the pool p of fw-workbench. It holds the newest generation of the pool that a front runs.\n"; got != want {
		t.Errorf("markerHeader = %q, want %q", got, want)
	}
	if got, want := header("p"), "# This file is rendered by the pool p of fw-workbench. Change the pool in the configuration of the front, not this file.\n"; got != want {
		t.Errorf("header = %q, want %q", got, want)
	}
}
