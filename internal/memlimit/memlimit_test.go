package memlimit

import (
	"os"
	"path/filepath"
	"testing"
)

// The limit comes from the first file of the cgroup that exists: a number
// gives Share of it, and max, a value that means no limit, or GOMEMLIMIT
// gives none.
func TestApply(t *testing.T) {
	dir := t.TempDir()
	file := func(name, text string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	v2, max, v1, huge := file("v2", "268435456\n"), file("max", "max\n"), file("v1", "536870912\n"), file("huge", "9223372036854771712\n")
	missing := filepath.Join(dir, "missing")
	var got int64
	set := func(n int64) int64 { got = n; return 0 }
	for _, c := range []struct {
		name  string
		env   string
		files []string
		want  int64
	}{
		{"cgroup v2", "", []string{v2, v1}, 214748364},
		{"cgroup v1 when v2 is missing", "", []string{missing, v1}, 429496729},
		{"no limit in v2", "", []string{max, v1}, 0},
		{"no limit in v1", "", []string{missing, huge}, 0},
		{"no cgroup", "", []string{missing}, 0},
		{"GOMEMLIMIT wins", "200MiB", []string{v2}, 0},
	} {
		got = 0
		if n := apply(c.env, c.files, set); n != c.want || got != c.want {
			t.Errorf("%s: set %d, returned %d, want %d", c.name, got, n, c.want)
		}
	}
}
