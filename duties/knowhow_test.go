package duties

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/internal/heldback"
)

// workbench.knowhow.heldBack is 0.05 by default, the file and the
// variable set it, and a share outside 0 to 1 names the key.
func TestLoadKnowhow(t *testing.T) {
	t.Setenv(EnvHeldBack, "")
	cfg := fw.DefaultConfig()
	if c, err := LoadKnowhow(cfg); err != nil || c.Share() != DefaultHeldBack {
		t.Fatalf("the default = %+v, %v", c, err)
	}
	cfg.Extensions = workbench(t, "knowhow: {heldBack: 0}\n")
	if c, err := LoadKnowhow(cfg); err != nil || c.Share() != 0 {
		t.Fatalf("0 = %+v, %v", c, err)
	}
	t.Setenv(EnvHeldBack, "0.1")
	if c, err := LoadKnowhow(cfg); err != nil || c.Share() != 0.1 {
		t.Fatalf("the variable did not win: %+v, %v", c, err)
	}
	t.Setenv(EnvHeldBack, "1.5")
	if _, err := LoadKnowhow(cfg); err == nil || !strings.Contains(err.Error(), "workbench.knowhow.heldBack") {
		t.Fatalf("a share above 1 = %v", err)
	}
}

// The engine marks a run held back with fw.knowhow: off when the stable
// hash of its id falls below the share, and marks none at a share of 0.
// The first start of the schedule goes through the engine; the request of
// many more starts shows the share.
func TestDutyRunsHeldBack(t *testing.T) {
	f := newFront(t, "", nil)
	f.put(t, "alice", daily("report"))
	f.s.heldBack = 1
	f.clock.Set(nine.Add(2 * time.Second))
	f.pass(t)
	if list := f.queue.list(); len(list) != 1 || list[0].Metadata[fw.MetaKnowhow] != fw.KnowhowOff {
		t.Fatalf("a share of 1 = %+v", list)
	}
	st, err := f.s.st.get(context.Background(), "report")
	if err != nil {
		t.Fatal(err)
	}
	f.s.heldBack = 0.5
	held := 0
	for range 200 {
		req := f.s.request(st, entry{Slot: nine}, nil)
		off := req.Metadata[fw.MetaKnowhow] == fw.KnowhowOff
		if off != heldback.Pick(req.ID, 0.5) {
			t.Fatalf("the mark of %s = %v, and the hash says %v", req.ID, off, !off)
		}
		if off {
			held++
		}
	}
	if held < 70 || held > 130 {
		t.Fatalf("a share of 0.5 held back %d of 200 runs", held)
	}
	f.s.heldBack = 0
	for range 50 {
		if req := f.s.request(st, entry{Slot: nine}, nil); req.Metadata[fw.MetaKnowhow] != "" {
			t.Fatalf("a share of 0 held a run back: %+v", req.Metadata)
		}
	}
}

// A duty whose templates make the runs marks them too, and names itself
// in workbench.duty, so a routine bound to the duty finds them.
func TestTemplatedDutyRunsHeldBack(t *testing.T) {
	bus := fw.NewMemBus()
	f := newFrontOn(t, bus, importBlock, nil)
	f.s.heldBack = 1
	got := collect(t, bus, "fw.jobs.triage")
	if resp, b := send(t, http.MethodPost, f.url+"/v1/duties/tickets/call", "", `{"id": 9, "title": "Printer"}`); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("a call = %d %s", resp.StatusCode, b)
	}
	waitFor(t, "the run request", func() bool { return len(got.list()) == 1 })
	if req := got.list()[0]; req.Metadata[fw.MetaKnowhow] != fw.KnowhowOff || req.Metadata[MetaDuty] != "tickets" {
		t.Fatalf("the run request = %+v", req.Metadata)
	}
}

// A run of a duty gets the output of its previous run in the block last,
// cut to 2 KB, and that output reaches neither its message nor its
// instructions. A held-back run gets no block last.
func TestDutyBlockLast(t *testing.T) {
	f := newFront(t, "", nil)
	d := daily("report")
	d.Do.Ask.Message = "Write the report of {date}. The last report is {last.title}."
	f.put(t, "alice", d)
	f.clock.Set(nine.Add(time.Second))
	f.pass(t)
	first := f.queue.last(t)
	if len(first.Context) != 0 {
		t.Fatalf("the first run has blocks: %+v", first.Context)
	}
	output := "The report: " + strings.Repeat("all hosts are up. ", 300)
	f.s.output = func(_ context.Context, id string) string {
		if id == first.ID {
			return output
		}
		return ""
	}
	f.view.set(first.ID, fw.StateCompleted, 0, func(r *fw.Run) { r.Title = "Report of Saturday" })
	f.pass(t)
	f.clock.Set(nine.AddDate(0, 0, 1).Add(time.Second))
	f.pass(t)
	second := f.queue.last(t)
	last := blockOf(second, ContextLast)
	if len(last) == 0 || len(last) > MaxLastBytes || !strings.HasPrefix(output, last) {
		t.Fatalf("the block last holds %d bytes: %.60q", len(last), last)
	}
	if strings.Contains(second.Input, "hosts are up") || strings.Contains(second.Instructions, "hosts are up") || second.Instructions != "" {
		t.Fatalf("the output of the previous run reached the message: %q, instructions %q", second.Input, second.Instructions)
	}
	if second.Input != "Write the report of 2026-10-25. The last report is Report of Saturday." {
		t.Fatalf("the second input = %q", second.Input)
	}

	// A held-back run gets no block last, and {last.*} stays.
	f.s.heldBack = 1
	f.view.set(second.ID, fw.StateCompleted, 0, func(r *fw.Run) { r.Title = "Report of Sunday" })
	f.s.output = func(context.Context, string) string { return output }
	f.pass(t)
	f.clock.Set(nine.AddDate(0, 0, 2).Add(time.Second))
	f.pass(t)
	third := f.queue.last(t)
	if third.Metadata[fw.MetaKnowhow] != fw.KnowhowOff || len(third.Context) != 0 ||
		third.Input != "Write the report of 2026-10-26. The last report is Report of Sunday." {
		t.Fatalf("the held-back run = input %q, blocks %+v, metadata %v", third.Input, third.Context, third.Metadata)
	}
}
