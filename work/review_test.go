package work

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/surface/native"
	"github.com/bdubs00/featherweight-workbench/duties"
	"github.com/bdubs00/featherweight-workbench/fleet"
)

// archivedFront is a front with a fleet and its archive on a bus of the
// memory, and a run of the agent writer of alice that waits for a check
// and whose worker the pool replaced: only the archive holds it.
func archivedFront(t *testing.T, run string) (*fw.Agent, *httptest.Server) {
	t.Helper()
	t.Setenv("FW_ATESPACE", "")
	bus := fw.NewMemBus()
	cfg := fw.DefaultConfig()
	cfg.Home = t.TempDir()
	cfg.Access.Default = "closed"
	cfg.Agents = map[string]fw.AgentProfile{
		"writer": {Description: "Writes release notes.", Access: &fw.Access{Owners: []string{"alice"}, Viewers: []string{"bob"}}},
	}
	a, err := fw.New(fw.WithConfig(cfg), fw.WithoutDefaults(), fw.WithAuthn(people{}), fw.WithModel(model()),
		fw.WithLedger(fw.NewMemLedger()), fw.WithBus(bus), fw.WithSurfaces(native.New()), fw.WithServices(duties.Start, Service), fw.WithLogger(quiet()))
	if err != nil {
		t.Fatal(err)
	}
	f, err := fleet.Open(context.Background(), a, fleet.Config{Name: "front", Egress: "fw.events", Expire: 50 * time.Millisecond, Interval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	fleet.Attach(a, f)
	h, err := a.Handler()
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(func() {
		srv.Close()
		fleet.Attach(a, nil)
		_ = f.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		a.Close(ctx)
	})
	start := time.Now().Add(-time.Hour).UTC()
	for i, x := range []struct {
		typ  string
		data any
	}{
		{fw.EvRunQueued, fw.RunQueuedData{Agent: "writer", Input: "Draft the notes", Metadata: map[string]string{fleet.MetaReview: "1"}, Origin: "native", Principal: "alice"}},
		{fw.EvRunStarted, map[string]any{}},
		{fw.EvRunFinished, fw.RunFinishedData{Outcome: fw.OutcomeCompleted, Output: "The notes of 2.4."}},
	} {
		raw, _ := json.Marshal(x.data)
		e := fw.Event{SpecVersion: fw.SpecVersion, ID: fmt.Sprintf("%s-%d", run, i+1), Type: x.typ, Time: start.Add(time.Duration(i) * time.Second), Subject: run, Seq: uint64(i + 1), Node: "ax:team/writer-0", Data: raw}
		if err := bus.Publish(context.Background(), "fw.events.team.writer."+x.typ, e); err != nil {
			t.Fatal(err)
		}
	}
	deadline := time.Now().Add(5 * time.Second)
	for !fleet.IsArchived(context.Background(), a, run) {
		if time.Now().After(deadline) {
			t.Fatalf("the archive does not hold the run %s alone", run)
		}
		time.Sleep(10 * time.Millisecond)
	}
	return a, srv
}

// A verdict on a result whose worker is gone lands, by the route of the
// check and by the route of the reaction that the page uses: the front
// keeps it, the card leaves To check, and the count drops. Needs work
// keeps its note, and no follow-up goes out, because no agent holds the
// run.
func TestReviewOfAnArchivedRun(t *testing.T) {
	a, srv := archivedFront(t, "01GONE")
	if c := cardOf(t, mustFor(t, a, "alice"), "run:01GONE"); c.Stage != StageReview || c.Next != NextYou {
		t.Fatalf("the card before = %+v", c)
	}
	if w := mustFor(t, a, "alice").Waiting; w.ToCheck != 1 {
		t.Fatalf("waiting before = %+v", w)
	}
	code, b := post(t, srv, "bob", "/v1/work/review", `{"run":"01GONE","verdict":"up"}`)
	if code != http.StatusForbidden {
		t.Fatalf("a check by a viewer = %d %s", code, b)
	}
	code, b = post(t, srv, "alice", "/v1/work/review", `{"run":"01GONE","verdict":"down","note":"Name the release."}`)
	if code != http.StatusOK || !strings.Contains(string(b), `"kept":true`) || strings.Contains(string(b), `"followUp":true`) {
		t.Fatalf("Needs work = %d %s", code, b)
	}
	if c := cardOf(t, mustFor(t, a, "alice"), "run:01GONE"); c.Stage != StageDone || c.Verdict != VerdictSentBack {
		t.Fatalf("the card after Needs work = %+v", c)
	}
	// The page reacts on the run itself, with seq 0.
	code, b = post(t, srv, "alice", "/v1/runs/01GONE/reactions", `{"seq":0,"reaction":"up"}`)
	if code != http.StatusOK || !strings.Contains(string(b), `"archived":true`) {
		t.Fatalf("Looks right by the reaction = %d %s", code, b)
	}
	ans := mustFor(t, a, "alice")
	if c := cardOf(t, ans, "run:01GONE"); c.Stage != StageDone || c.Verdict != VerdictAccepted {
		t.Fatalf("the card after Looks right = %+v", c)
	}
	if ans.Waiting.ToCheck != 0 || ans.Waiting.Count != 0 {
		t.Fatalf("waiting after = %+v", ans.Waiting)
	}
}
