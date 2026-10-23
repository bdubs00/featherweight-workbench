package bridges

import (
	"context"
	"net/http"
	"testing"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/fleet"
	"github.com/bdubs00/featherweight-workbench/pages"
)

// The pages of a bridge are the pages under bridges/<id>/ and the pages
// whose newest version a run of the bridge wrote, wherever they are: a
// seat, a helper of a seat, and a seat run that the view forgot but the
// seat lists. A page that a person wrote again since, and a page of a run
// of no bridge, are not the pages of the bridge.
func TestPagesOfABridge(t *testing.T) {
	a := newAgent(t, nil)
	s := newStore(t, a, nil)
	v, _ := withFleet(t, a)
	v.state("default/crew-0", fleet.MemberLive)
	url := serve(t, a)
	it := open(t, url)
	ctx := context.Background()
	v.put(workerRun("r-mig", it.ID, "migrator-1", "migrator", fw.StateRunning))
	v.put(workerRun("r-rev", it.ID, "reviewer", "reviewer", fw.StateCompleted))
	v.put(&fw.Run{ID: "r-help", Agent: "migrator", Parent: "r-mig", Member: "default/crew-0-c", State: fw.StateRunning, Metadata: map[string]string{fw.MetaBatch: BatchOf(it.ID)}})
	v.put(&fw.Run{ID: "r-x", Agent: "migrator", Member: "default/crew-1", State: fw.StateRunning})
	s.poll(ctx)
	w := pages.Of(a)
	write := func(path, by, run string) {
		t.Helper()
		if _, err := w.WriteAs(ctx, path, "# "+path+"\n\nby "+by+"\n", by, run, "write", pages.IfAny); err != nil {
			t.Fatal(err)
		}
	}
	write("k3s/monitoring/alerts.md", "agent:migrator", "r-mig")
	write("k3s/monitoring/grafana.md", "agent:migrator", "r-help")
	write("k3s/runbooks/upgrade.md", "agent:reviewer", "r-rev")
	write("k3s/runbooks/old.md", "agent:reviewer", "r-rev")
	write("k3s/runbooks/old.md", "alice", "")
	write("bridges/"+it.ID+"/report.md", "agent:manager", "mgr-elsewhere")
	write("notes/other.md", "agent:migrator", "r-x")
	// The view forgets the run of the reviewer, which its seat still lists.
	v.drop("r-rev")

	resp, body := do(t, http.MethodGet, url+"/v1/bridges/"+it.ID+"/pages", "alice", "")
	var got BridgePages
	decodeJSON(t, body, &got)
	if resp.StatusCode != http.StatusOK || got.Bridge != it.ID || got.Count != 4 || len(got.Pages) != 4 {
		t.Fatalf("pages = %d %s", resp.StatusCode, body)
	}
	want := []struct{ path, seat, run string }{
		{"bridges/" + it.ID + "/report.md", "", "mgr-elsewhere"},
		{"k3s/monitoring/alerts.md", "migrator-1", "r-mig"},
		{"k3s/monitoring/grafana.md", "migrator-1", "r-help"},
		{"k3s/runbooks/upgrade.md", "reviewer", "r-rev"},
	}
	for i, w := range want {
		p := got.Pages[i]
		if p.Path != w.path || p.Seat != w.seat || p.Run != w.run || p.Title == "" || p.Version == 0 || p.Hash == "" {
			t.Errorf("page %d = %+v, want %+v", i, p, w)
		}
	}
	if resp, _ := do(t, http.MethodGet, url+"/v1/bridges/"+it.ID+"/pages", "carol", ""); resp.StatusCode != http.StatusForbidden {
		t.Errorf("carol lists the pages: %d", resp.StatusCode)
	}
}
