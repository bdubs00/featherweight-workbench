package fleet

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/surface/native"
)

// A summary holds the newest six messages of the person and of the agent,
// one line each, at most 2,000 characters with its first line, and the
// summary of an earlier conversation in a message of the person counts
// without it.
func TestSummaryOf(t *testing.T) {
	start := time.Date(2026, 10, 26, 9, 0, 0, 0, time.UTC)
	ev := func(seq uint64, typ string, data any) fw.Event {
		return archiveEvent(t, "01RUN", "ax:team/chat-0", typ, seq, start, data)
	}
	model := func(seq uint64, text string) fw.Event {
		return ev(seq, fw.EvModelResponded, fw.ModelRespondedData{Content: []fw.Block{fw.Text(text)}})
	}
	user := func(seq uint64, text string) fw.Event {
		return ev(seq, fw.EvMessageReceived, fw.MessageReceivedData{Role: fw.RoleUser, Content: []fw.Block{fw.Text(text)}})
	}
	events := []fw.Event{
		ev(1, fw.EvRunQueued, fw.RunQueuedData{Input: SummaryHead + "\nThe person: old\nYou: older\n\nWhich hosts\n are down?"}),
		model(2, "None is down."),
		user(3, "And lab-2?"),
		model(4, ""),
		model(5, "lab-2 is up."),
		user(6, "Thanks."),
		model(7, "You are welcome."),
		user(8, "Bye."),
	}
	got := summaryOf(events)
	want := SummaryHead + "\nYou: None is down.\nThe person: And lab-2?\nYou: lab-2 is up.\nThe person: Thanks.\nYou: You are welcome.\nThe person: Bye."
	if got != want {
		t.Fatalf("summaryOf =\n%s\nwant\n%s", got, want)
	}
	if got := summaryOf(events[:2]); got != SummaryHead+"\nThe person: Which hosts are down?\nYou: None is down." {
		t.Fatalf("a summary of a chain = %q", got)
	}
	long := []fw.Event{
		ev(1, fw.EvRunQueued, fw.RunQueuedData{Input: strings.Repeat("a", 900)}),
		model(2, strings.Repeat("b", 900)),
		user(3, strings.Repeat("c", 900)),
		model(4, strings.Repeat("d", 3000)),
	}
	got = summaryOf(long)
	if n := utf8.RuneCountInString(got); n > SummaryChars || !strings.HasPrefix(got, SummaryHead+"\nYou: ddd") || !strings.HasSuffix(got, "…") {
		t.Fatalf("a long summary holds %d characters: %.80q", n, got)
	}
	got = summaryOf(long[:3])
	if n := utf8.RuneCountInString(got); n > SummaryChars || strings.Contains(got, "aaa") || !strings.Contains(got, "\nYou: bbb") {
		t.Fatalf("a summary of whole lines holds %d characters and %.60q", n, got)
	}
	if got := summaryOf(nil); got != "" {
		t.Fatalf("a summary of nothing = %q", got)
	}
}

// POST /v1/runs with continues starts a run with the summary of the
// earlier conversation in front of the message, the title of the earlier
// run, and the metadata workbench.continues, also for a run that only the
// archive holds. A run that the caller cannot find is 404.
func TestContinueAnArchivedConversation(t *testing.T) {
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
		return front.f.arch().summaries(context.Background())["01RUN"].Events == 6
	})
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
	post := func(body string) (int, string) {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/runs", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	code, body := post(`{"input": "And now?", "continues": "01RUN", "metadata": {"fw.view": "chat"}, "wait": true}`)
	var run fw.Run
	if err := json.Unmarshal([]byte(body), &run); code != http.StatusOK || err != nil {
		t.Fatalf("POST = %d %s", code, body)
	}
	want := SummaryHead + "\nThe person: Which hosts are down?\nYou: No host is down.\n\nAnd now?"
	// The mem bus keeps no order, so the archive can hold the title of the
	// input or of the annotation; the new run takes the one that it holds.
	old, ok, err := ArchivedRun(context.Background(), front.a, "01RUN")
	if err != nil || !ok || old.Title == "" {
		t.Fatalf("ArchivedRun = %+v, %v, %v", old, ok, err)
	}
	if run.Input != want || run.Title != old.Title || run.Metadata[MetaContinues] != "01RUN" || run.Metadata["fw.view"] != "chat" {
		t.Fatalf("the new run = input %q, title %q, metadata %v", run.Input, run.Title, run.Metadata)
	}
	// A title of the body wins, and a start without continues stays as it
	// was.
	if code, body := post(`{"input": "Again?", "title": "Hosts, again", "continues": "01RUN", "wait": true}`); code != http.StatusOK || !strings.Contains(body, `"title":"Hosts, again"`) {
		t.Fatalf("POST with a title = %d %s", code, body)
	}
	if code, body := post(`{"input": "Plain.", "wait": true}`); code != http.StatusOK || strings.Contains(body, MetaContinues) || strings.Contains(body, SummaryHead) {
		t.Fatalf("POST without continues = %d %s", code, body)
	}
	for body, want := range map[string]int{
		`{"input": "x", "continues": "01NONE"}`: http.StatusNotFound,
		`{"input": "x", "continues": 7}`:        http.StatusBadRequest,
		`{"continues": "01RUN"}`:                http.StatusBadRequest,
	} {
		if code, out := post(body); code != want {
			t.Errorf("POST %s = %d %s, want %d", body, code, out, want)
		}
	}
}
