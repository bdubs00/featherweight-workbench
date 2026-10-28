package knowhow

import (
	"testing"

	"github.com/bdubs00/featherweight-workbench/duties"
	"github.com/bdubs00/featherweight-workbench/pages"
	"github.com/bdubs00/featherweight-workbench/routines"
	"github.com/bdubs00/featherweight-workbench/sources"
)

// The package sets the hooks of the seams when it loads.
func TestTheSeamsAreWired(t *testing.T) {
	if pages.OnWrite == nil || sources.OnItem == nil || duties.OnItem == nil || routines.OnMove == nil || routines.AskFirst == nil {
		t.Fatal("a hook of the seams is not set")
	}
}
