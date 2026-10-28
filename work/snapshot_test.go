package work

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/bridges"
	"github.com/bdubs00/featherweight-workbench/duties"
)

// TestSnapshot prints the board of a snapshot of a real front, so a
// person can compare the board with what the front says today. It reads
// the answers of the routes from the folder WORK_SNAPSHOT: runs.json (GET
// /v1/fleet/runs), bridges.json (a list of GET /v1/bridges/{id}),
// inbox.json (GET /v1/inbox), duties.json (a list of GET
// /v1/duties/{name}), whoami.json, and now.txt. Without the variable,
// the test skips.
func TestSnapshot(t *testing.T) {
	dir := os.Getenv("WORK_SNAPSHOT")
	if dir == "" {
		t.Skip("WORK_SNAPSHOT names no folder")
	}
	read := func(name string, v any) {
		t.Helper()
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(b, v); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	var runs struct {
		Runs []struct {
			fw.Run
			Archived bool `json:"archived"`
		} `json:"runs"`
	}
	read("runs.json", &runs)
	var items []bridges.Item
	read("bridges.json", &items)
	var inbox struct {
		Items []struct {
			Bridge string    `json:"bridge"`
			Kind   string    `json:"kind"`
			Text   string    `json:"text"`
			Since  time.Time `json:"since"`
		} `json:"items"`
	}
	read("inbox.json", &inbox)
	var ds []duties.View
	read("duties.json", &ds)
	var who struct {
		Principal struct {
			Subject string `json:"subject"`
		} `json:"principal"`
	}
	read("whoami.json", &who)
	raw, err := os.ReadFile(filepath.Join(dir, "now.txt"))
	if err != nil {
		t.Fatal(err)
	}
	now, err := time.Parse(time.RFC3339, strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	in := Input{Me: who.Principal.Subject, Now: now, Mine: map[string]bool{}}
	for i := range runs.Runs {
		r := &runs.Runs[i]
		in.Runs = append(in.Runs, Run{Run: &r.Run, Archived: r.Archived})
		if r.Principal == in.Me {
			in.Mine[r.ID] = true
		}
	}
	in.Bridges = items
	for _, it := range items {
		for _, m := range it.Members {
			if m.Question == 0 {
				continue
			}
			w := bridges.Waiting{Bridge: it.ID, Title: it.Title, Handle: m.Handle, Agent: m.Agent, Question: m.Question}
			for _, x := range inbox.Items {
				if x.Bridge == it.ID && x.Kind == "question" {
					w.Text, w.At = x.Text, x.Since
				}
			}
			in.Waiting = append(in.Waiting, w)
		}
	}
	for _, v := range ds {
		in.Duties = append(in.Duties, v.Duty)
		for _, e := range v.History {
			in.Starts = append(in.Starts, Start{Duty: v.Duty, Entry: e})
		}
	}
	for _, all := range []bool{false, true} {
		in.All = all
		ans := Assemble(in)
		t.Logf("all=%v\n%s\n", all, Text(ans, now))
	}
}
