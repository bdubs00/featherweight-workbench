package fleet

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/surface/native"
	"github.com/bdubs00/featherweight-workbench/internal/bucket"
	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// archiveEvent builds an event of the egress stream of a worker.
func archiveEvent(t *testing.T, run, node, typ string, seq uint64, at time.Time, data any) fw.Event {
	t.Helper()
	raw, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	return fw.Event{SpecVersion: fw.SpecVersion, ID: fmt.Sprintf("%s-%d", run, seq), Source: node, Type: typ, Time: at, Subject: run, Seq: seq, Node: node, Data: raw}
}

// conversation is the egress of one finished conversation of the manager
// in a quick chat of a bridge, as a worker publishes it.
func conversation(t *testing.T, run, node string, start time.Time) []fw.Event {
	t.Helper()
	meta := map[string]string{fw.MetaBatch: "bridge:01BRIDGE", MetaInternal: "duty", "fw.view": "quickchat"}
	return []fw.Event{
		archiveEvent(t, run, node, fw.EvRunQueued, 1, start, fw.RunQueuedData{Agent: "manager", Input: "Which hosts are down?", Metadata: meta, Origin: "native", Principal: "alice", Parent: "01PARENT", Depth: 1}),
		archiveEvent(t, run, node, fw.EvRunStarted, 2, start.Add(time.Second), map[string]any{}),
		archiveEvent(t, run, node, fw.EvToolRequested, 3, start.Add(2*time.Second), fw.ToolRequestedData{Tool: "bash", CallID: "c1", Input: json.RawMessage(`{"command":"ss -tlnp"}`)}),
		archiveEvent(t, run, node, fw.EvToolResponded, 4, start.Add(3*time.Second), fw.ToolRespondedData{Tool: "bash", CallID: "c1", Content: []fw.Block{{Type: fw.BlockText, Text: "LISTEN 0 4096 *:22"}}}),
		archiveEvent(t, run, node, fw.EvRunAnnotated, 5, start.Add(4*time.Second), fw.RunAnnotatedData{Metadata: map[string]string{fw.MetaTitle: "Hosts that are down"}}),
		archiveEvent(t, run, node, fw.EvRunFinished, 6, start.Add(5*time.Second), fw.RunFinishedData{Outcome: fw.OutcomeCompleted, Output: "No host is down."}),
	}
}

// The archive keeps the summary of a run with the fields of a live run,
// its metadata included, so the lists leave out the same runs, and it
// keeps each event once, also when the stream delivers it again.
func TestArchiveKeepsARun(t *testing.T) {
	ctx := context.Background()
	a := newArchive(bucket.NewMem(ArchiveBucket), nil, quietLogger())
	start := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
	events := conversation(t, "01RUN", "ax:team/manager-0", start)
	for _, e := range append(events, events[2], events[5]) {
		if err := a.add(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	// The parent names the run as its child.
	if err := a.add(ctx, archiveEvent(t, "01OTHER", "ax:team/manager-0", fw.EvRunChild, 3, start, fw.RunChildData{Child: "01KID", Member: "team/manager-0"})); err != nil {
		t.Fatal(err)
	}
	r := a.summaries(ctx)["01RUN"]
	run := r.listed()
	if run == nil || r.Events != 6 {
		t.Fatalf("summary = %+v", r)
	}
	if run.ID != "01RUN" || run.Agent != "manager" || run.Principal != "alice" || run.Parent != "01PARENT" || run.Input != "Which hosts are down?" ||
		run.Title != "Hosts that are down" || run.State != fw.StateCompleted || run.Output != "No host is down." || run.Member != "team/manager-0" ||
		!run.CreatedAt.Equal(start) || run.UpdatedAt.Before(start.Add(5*time.Second)) {
		t.Fatalf("the archived run = %+v", run)
	}
	if run.Metadata[fw.MetaBatch] != "bridge:01BRIDGE" || run.Metadata[MetaInternal] != "duty" || run.Metadata["fw.view"] != "quickchat" {
		t.Fatalf("the metadata = %v", run.Metadata)
	}
	if Listed(run) {
		t.Fatal("the lists show an internal run of the archive")
	}
	got, err := a.events(ctx, "01RUN")
	if err != nil || len(got) != 6 || got[0].Type != fw.EvRunQueued || got[5].Type != fw.EvRunFinished {
		t.Fatalf("events = %d, %v", len(got), err)
	}
	if kid := a.summaries(ctx)["01KID"]; kid.Parent != "01OTHER" || kid.listed() != nil {
		t.Fatalf("the child = %+v", kid)
	}
	// A second replica reads the summaries that the first one wrote.
	b := newArchive(a.b, nil, quietLogger())
	b.refresh(ctx)
	if r2 := b.summaries(ctx)["01RUN"].listed(); r2 == nil || r2.Title != "Hosts that are down" {
		t.Fatalf("the other replica reads %+v", r2)
	}
}

// A transcript of the archive shows the question, the tool call and its
// result, and the answer of the run, although the stream carries no
// fw.model.responded.
func TestTranscriptEvents(t *testing.T) {
	events := TranscriptEvents(conversation(t, "01RUN", "ax:team/manager-0", time.Now()))
	var kinds []string
	for _, e := range events {
		kinds = append(kinds, e.Type)
	}
	want := []string{fw.EvRunQueued, fw.EvRunStarted, fw.EvModelResponded, fw.EvToolRequested, fw.EvToolResponded, fw.EvRunAnnotated, fw.EvModelResponded, fw.EvRunFinished}
	if strings.Join(kinds, " ") != strings.Join(want, " ") {
		t.Fatalf("events = %v", kinds)
	}
	var d fw.ModelRespondedData
	if err := events[6].Decode(&d); err != nil || d.Content[0].Text != "No host is down." || events[6].Seq != 6 {
		t.Fatalf("the answer = %+v, %v", d, err)
	}
	if err := events[2].Decode(&d); err != nil || d.Content[0].Name != "bash" || d.Content[0].ID != "c1" {
		t.Fatalf("the tool call = %+v, %v", d, err)
	}
}

// When no member holds a run any more, the routes of the run answer from
// the archive: the run and its events with "archived": true, a 409 for a
// message, and a 404 for its live view.
func TestArchiveAnswersForAReplacedWorker(t *testing.T) {
	t.Setenv("FW_ATESPACE", "")
	t.Setenv("FW_API_TOKEN", "fleet-secret")
	bus := fw.NewMemBus()
	front := newMember(t, bus, "front", nil, fw.WithSurfaces(native.New()))
	start := time.Now().Add(-time.Hour).UTC()
	for _, e := range conversation(t, "01RUN", "ax:team/manager-0", start) {
		if err := bus.Publish(context.Background(), "fw.events.team.manager."+e.Type, e); err != nil {
			t.Fatal(err)
		}
	}
	eventually(t, "the archive keeps the run", func() bool {
		r := front.f.arch().summaries(context.Background())["01RUN"]
		return r.Events == 6
	})
	if run, ok, err := ArchivedRun(context.Background(), front.a, "01RUN"); ok || err != nil {
		t.Fatalf("the archive answered for a run that a member holds: %+v, %v", run, err)
	}
	// The pool replaced the worker: its runs leave the view.
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
	do := func(method, path, body, accept string) (int, string) {
		t.Helper()
		req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer tok")
		req.Header.Set("Accept", accept)
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	code, body := do("GET", "/v1/runs/01RUN", "", "application/json")
	var got struct {
		fw.Run
		Archived bool `json:"archived"`
	}
	if err := json.Unmarshal([]byte(body), &got); code != http.StatusOK || err != nil || !got.Archived || got.Member != "team/manager-0" ||
		got.Metadata[fw.MetaBatch] != "bridge:01BRIDGE" || got.State != fw.StateCompleted {
		t.Fatalf("GET = %d %s", code, body)
	}
	code, body = do("GET", "/v1/runs/01RUN/events?limit=4", "", "application/json")
	var page struct {
		Run    json.RawMessage `json:"run"`
		Events []fw.Event      `json:"events"`
		Lines  []any           `json:"lines"`
	}
	if err := json.Unmarshal([]byte(body), &page); code != http.StatusOK || err != nil || len(page.Events) != 4 || page.Events[0].Seq != 3 || len(page.Lines) != 4 ||
		!strings.Contains(string(page.Run), `"archived":true`) {
		t.Fatalf("events = %d %s", code, body)
	}
	if code, body := do("GET", "/v1/runs/01RUN/events?from=5", "", "text/event-stream"); code != http.StatusOK || !strings.Contains(body, "id: 5\nevent: fw.run.annotated\n") || !strings.Contains(body, "id: 6\n") {
		t.Fatalf("event stream = %d %s", code, body)
	}
	if code, body := do("POST", "/v1/runs/01RUN/messages", `{"content":"and now?"}`, "application/json"); code != http.StatusConflict || !strings.Contains(body, ReadOnlyRun) {
		t.Fatalf("a message = %d %s", code, body)
	}
	if code, body := do("POST", "/v1/runs/01RUN/cancel", "", "application/json"); code != http.StatusConflict {
		t.Fatalf("a cancel = %d %s", code, body)
	}
	if code, body := do("GET", "/v1/runs/01RUN/live", "", "text/event-stream"); code != http.StatusNotFound {
		t.Fatalf("the live view = %d %s", code, body)
	}
	if code, _ := do("GET", "/v1/runs/01NONE", "", "application/json"); code != http.StatusNotFound {
		t.Fatalf("a run that nobody holds = %d", code)
	}
	list, err := ArchivedRuns(context.Background(), front.a, "", "")
	if err != nil || len(list) != 1 || list[0].ID != "01RUN" {
		t.Fatalf("ArchivedRuns = %v, %v", list, err)
	}
	if !IsArchived(context.Background(), front.a, "01RUN") {
		t.Fatal("IsArchived")
	}
}

// Two replicas of the front read the stream of the bus with one durable
// consumer from its start. The archive keeps every event of the stream
// once, also the events from before the first start of the front, and
// each run once.
func TestArchiveConsumerWithTwoReplicas(t *testing.T) {
	s, err := server.NewServer(&server.Options{Host: "127.0.0.1", Port: -1, JetStream: true, StoreDir: t.TempDir(), NoLog: true, NoSigs: true})
	if err != nil {
		t.Fatal(err)
	}
	go s.Start()
	t.Cleanup(s.Shutdown)
	if !s.ReadyForConnections(5 * time.Second) {
		t.Fatal("the NATS server did not start")
	}
	nc, err := nats.Connect(s.ClientURL())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(nc.Close)
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := js.CreateStream(ctx, jetstream.StreamConfig{Name: "FW", Subjects: []string{"fw.>"}, Retention: jetstream.LimitsPolicy}); err != nil {
		t.Fatal(err)
	}
	start := time.Now().Add(-48 * time.Hour).UTC()
	var all []fw.Event
	for i := range 5 {
		all = append(all, conversation(t, fmt.Sprintf("01RUN%d", i), fmt.Sprintf("ax:team/manager-%d", i%2), start.Add(time.Duration(i)*time.Minute))...)
	}
	publish := func(events []fw.Event) {
		for _, e := range events {
			raw, _ := json.Marshal(e)
			if _, err := js.Publish(ctx, "fw.events.team.manager."+e.Type, raw); err != nil {
				t.Fatal(err)
			}
		}
	}
	// Half of the events are older than the front, and an intake queue
	// shares the stream.
	publish(all[:15])
	if _, err := js.Publish(ctx, "fw.jobs.manager", []byte(`{"id":"job"}`)); err != nil {
		t.Fatal(err)
	}
	cfg := fw.DefaultConfig()
	cfg.Bus.Driver, cfg.Bus.URL = "nats", s.ClientURL()
	b, closeB, err := bucket.Open(ctx, cfg, ArchiveBucket, quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(closeB)
	var mu sync.Mutex
	handled := map[string]int{}
	replica := func() {
		a := newArchive(b, nil, quietLogger())
		stop, err := natsArchiveSource(ctx, cfg, "fw.events", func(e fw.Event) error {
			mu.Lock()
			handled[e.ID]++
			mu.Unlock()
			return a.add(ctx, e)
		}, quietLogger())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(stop)
	}
	replica()
	replica()
	publish(all[15:])
	eventually(t, "the replicas archive every event", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(handled) == len(all)
	})
	a := newArchive(b, nil, quietLogger())
	a.refresh(ctx)
	sums := a.summaries(ctx)
	for i := range 5 {
		id := fmt.Sprintf("01RUN%d", i)
		r := sums[id]
		if r.Events != 6 || r.listed() == nil || r.listed().State != fw.StateCompleted {
			t.Fatalf("the run %s = %+v", id, r)
		}
		events, err := a.events(ctx, id)
		if err != nil || len(events) != 6 {
			t.Fatalf("the events of %s = %d, %v", id, len(events), err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	for id, n := range handled {
		if n > 1 {
			t.Fatalf("the event %s reached the replicas %d times", id, n)
		}
	}
	info, err := js.Consumer(ctx, "FW", ArchiveConsumer)
	if err != nil {
		t.Fatal(err)
	}
	if ci, err := info.Info(ctx); err != nil || ci.Config.MaxAckPending != 1 || ci.Config.DeliverPolicy != jetstream.DeliverAllPolicy {
		t.Fatalf("the consumer = %+v, %v", ci, err)
	}
}
