package fleet

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestWithMemberJSON(t *testing.T) {
	if got := string(withMemberJSON([]byte(`{"id":"r1","state":"running"}`), "m")); !strings.Contains(got, `"member":"m"`) {
		t.Errorf("run = %s", got)
	}
	var page struct {
		Run    map[string]any `json:"run"`
		Events []any          `json:"events"`
	}
	if err := json.Unmarshal(withMemberJSON([]byte(`{"run":{"id":"r1","state":"running"},"events":[]}`), "m"), &page); err != nil || page.Run["member"] != "m" || page.Events == nil {
		t.Errorf("page = %v, %v", page, err)
	}
	for _, in := range []string{`{"runs":[]}`, `[1,2]`, `not json`} {
		if got := string(withMemberJSON([]byte(in), "m")); got != in {
			t.Errorf("%s became %s", in, got)
		}
	}
}
