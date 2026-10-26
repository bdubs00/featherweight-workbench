package fleet

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/surface/native"
)

// The view and the archive keep the time of the newest fw.run.finished of
// a run that ended, and a later reaction moves updatedAt and not the end.
// A read of a run of the archive carries endedAt, also for a summary
// without it.
func TestEndedAt(t *testing.T) {
	t.Setenv("FW_ATESPACE", "")
	t.Setenv("FW_API_TOKEN", "fleet-secret")
	bus := fw.NewMemBus()
	front := newMember(t, bus, "front", nil, fw.WithSurfaces(native.New()))
	start := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	end := start.Add(5 * time.Second)
	node := "ax:team/manager-0"
	events := []fw.Event{
		archiveEvent(t, "01RUN", node, fw.EvRunQueued, 1, start, fw.RunQueuedData{Agent: "manager", Input: "Which hosts are down?", Principal: "alice"}),
		archiveEvent(t, "01RUN", node, fw.EvRunStarted, 2, start.Add(time.Second), map[string]any{}),
		archiveEvent(t, "01RUN", node, fw.EvRunFinished, 3, end, fw.RunFinishedData{Outcome: fw.OutcomeCompleted, Output: "None."}),
		archiveEvent(t, "01RUN", node, fw.EvRunReacted, 4, start.Add(time.Minute), fw.ReactionData{Seq: 3, Reaction: fw.ReactionUp, By: "alice"}),
	}
	for _, e := range events {
		if err := bus.Publish(context.Background(), "fw.events.team.manager."+e.Type, e); err != nil {
			t.Fatal(err)
		}
	}
	eventually(t, "the view and the archive keep the end", func() bool {
		got := EndedAt(context.Background(), front.a, []string{"01RUN"})
		r := front.f.arch().summaries(context.Background())["01RUN"]
		return got["01RUN"].Equal(end) && r.Ended.Equal(end) && r.Events == 4
	})
	if got := EndedAt(context.Background(), front.a, []string{"01NONE"}); len(got) != 0 {
		t.Fatalf("EndedAt of no run = %v", got)
	}
	// The worker was replaced: the archive answers, with endedAt.
	p := front.f.platform
	p.mu.Lock()
	p.forgetMember(p.members["team/manager-0"], time.Now())
	p.mu.Unlock()
	h, err := front.a.Handler()
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	resp, err := http.Get(srv.URL + "/v1/runs/01RUN")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var got struct {
		EndedAt   *time.Time `json:"endedAt"`
		UpdatedAt time.Time  `json:"updatedAt"`
		Archived  bool       `json:"archived"`
	}
	if err := json.Unmarshal(b, &got); err != nil || resp.StatusCode != http.StatusOK || !got.Archived || got.EndedAt == nil || !got.EndedAt.Equal(end) || !got.UpdatedAt.After(end) {
		t.Fatalf("GET = %d %s", resp.StatusCode, b)
	}
	// A summary of an older server holds no end: the events give it.
	arch := front.f.arch()
	arch.mu.Lock()
	r := arch.runs["01RUN"]
	r.Ended = time.Time{}
	arch.runs["01RUN"] = r
	arch.mu.Unlock()
	run, _, _ := ArchivedRun(context.Background(), front.a, "01RUN")
	if got := NewRelay(front.a).archivedEnd(context.Background(), run); got == nil || !got.Equal(end) {
		t.Fatalf("the end from the events = %v", got)
	}
}

// A run that works again after its end has no end until it ends again.
func TestEndedAfter(t *testing.T) {
	at := time.Date(2026, 10, 26, 9, 0, 0, 0, time.UTC)
	done := &fw.Run{State: fw.StateCompleted}
	if got := endedAfter(time.Time{}, done, fw.Event{Type: fw.EvRunFinished, Time: at}); !got.Equal(at) {
		t.Fatalf("an end = %v", got)
	}
	if got := endedAfter(at, done, fw.Event{Type: fw.EvRunReacted, Time: at.Add(time.Hour)}); !got.Equal(at) {
		t.Fatalf("a reaction moved the end to %v", got)
	}
	if got := endedAfter(at, &fw.Run{State: fw.StateRunning}, fw.Event{Type: fw.EvRunResumed, Time: at.Add(time.Hour)}); !got.IsZero() {
		t.Fatalf("a run that works again = %v", got)
	}
}
