package bridges

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// The cap maxCostUSD bounds the cost of every run of a bridge together.
// At the cap, the channel says so once, nothing fans out, and each open
// run that the channel mirrors stops at a limit of its budget, once: a run
// with a cost at its cost so far, and a run without a priced cost at its
// turns so far. A raised cap delivers again.
func TestCostCapStopsTheBridge(t *testing.T) {
	s, v, fm, url, id := budgetBridge(t, `,"caps":{"maxCostUSD":1.5}`)
	resp, body := do(t, http.MethodGet, url+"/v1/bridges/"+id, "alice", "")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `"maxCostUSD":1.5`) {
		t.Fatalf("the bridge = %d %s", resp.StatusCode, body)
	}
	mig := workerRun("r-mig", id, "migrator-1", "migrator", fw.StateRunning)
	mig.Usage.CostUSD, mig.Usage.Turns = 1.2, 4
	rev := workerRun("r-rev", id, "reviewer", "reviewer", fw.StateInputRequired)
	rev.Usage.CostUSD, rev.Usage.Turns = 0, 3 // a model without a price
	rev.Usage.InputTokens = 900
	v.put(mig)
	v.put(rev)
	s.poll(context.Background())
	if n := len(fm.find(http.MethodPatch, "/v1/runs/r-mig/budget")); n != 0 {
		t.Fatalf("%d stops under the cap", n)
	}
	mig.Usage.CostUSD = 1.6
	v.put(mig)
	s.poll(context.Background())
	s.poll(context.Background())
	eventually(t, "both runs stop at a limit", func() bool {
		return len(fm.find(http.MethodPatch, "/v1/runs/r-mig/budget")) == 1 && len(fm.find(http.MethodPatch, "/v1/runs/r-rev/budget")) == 1
	})
	var got fw.Budget
	raw, _ := json.Marshal(fm.find(http.MethodPatch, "/v1/runs/r-mig/budget")[0].Body)
	if err := json.Unmarshal(raw, &got); err != nil || got != (fw.Budget{MaxCostUSD: 1.6}) {
		t.Fatalf("the stop of the migrator = %s, %v", raw, err)
	}
	raw, _ = json.Marshal(fm.find(http.MethodPatch, "/v1/runs/r-rev/budget")[0].Body)
	got = fw.Budget{}
	if err := json.Unmarshal(raw, &got); err != nil || got != (fw.Budget{MaxTurns: 3}) {
		t.Fatalf("the stop of the reviewer = %s, %v", raw, err)
	}
	if resp, out := do(t, http.MethodPost, url+"/v1/bridges/"+id+"/messages", "alice", `{"text":"Go on.","to":["reviewer"]}`); resp.StatusCode != http.StatusCreated {
		t.Fatalf("post = %d %s", resp.StatusCode, out)
	}
	time.Sleep(50 * time.Millisecond)
	if n := len(fm.find(http.MethodPost, pathRev)); n != 0 {
		t.Fatalf("%d follow-ups past the cap of cost", n)
	}
	resp, body = do(t, http.MethodGet, url+"/v1/bridges/"+id+"/messages", "alice", "")
	if resp.StatusCode != http.StatusOK || strings.Count(body, "The bridge reached its cap of $1.50: its runs cost $1.60. Its open runs stop at a limit") != 1 {
		t.Fatalf("the channel = %s", body)
	}
	if resp, out := do(t, http.MethodPatch, url+"/v1/bridges/"+id, "alice", `{"caps":{"maxCostUSD":-1}}`); resp.StatusCode != http.StatusBadRequest || !strings.Contains(out, "maxCostUSD") {
		t.Fatalf("a negative cap = %d %s", resp.StatusCode, out)
	}
	if resp, out := do(t, http.MethodPatch, url+"/v1/bridges/"+id, "alice", `{"caps":{"maxCostUSD":5}}`); resp.StatusCode != http.StatusOK || !strings.Contains(out, `"maxCostUSD":5`) {
		t.Fatalf("raise the cap = %d %s", resp.StatusCode, out)
	}
	rev.State = fw.StateCompleted
	v.put(rev)
	s.poll(context.Background())
	say(t, fm, url, id, `{"text":"Now go on.","to":["reviewer"]}`, pathRev, 1)
}

// The line at the cap names a cap below a whole cent to a tenth of a cent,
// so a cap of $0.005 does not read as $0.01 beside a cost of $0.01.
func TestCostCapLineWordsSmallCaps(t *testing.T) {
	for _, c := range []struct {
		limit, spent float64
		want         string
	}{
		{1.5, 1.6, "The bridge reached its cap of $1.50: its runs cost $1.60."},
		{0.005, 0.0071681, "The bridge reached its cap of $0.005: its runs cost $0.007."},
		{0.3, 0.3012, "The bridge reached its cap of $0.30: its runs cost $0.30."},
	} {
		rec := &record{}
		rec.Caps.MaxCostUSD = c.limit
		run := &fw.Run{ID: "r"}
		run.Usage.CostUSD = c.spent
		m, at := costCapLine(rec, map[string]*fw.Run{"r": run})
		if !at || m == nil || !strings.HasPrefix(m.Text, c.want) {
			t.Errorf("the line at a cap of %v and a cost of %v = %v %+v, want %q", c.limit, c.spent, at, m, c.want)
		}
	}
}
