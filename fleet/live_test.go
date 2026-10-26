package fleet

import (
	"bufio"
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
)

// The relay copies the live stream of a run of another member, GET
// /v1/runs/{id}/live of the kernel, through Fleet.Forward as the member
// writes it: the first event reaches the client while the member still
// holds the second one back, so the relay flushes after each read. The
// caller is a person, anonymous without authentication, so the stream
// comes with the static token for that person.
func TestRelayStreamsLive(t *testing.T) {
	t.Setenv("FW_ATESPACE", "")
	t.Setenv("FW_API_TOKEN", "fleet-secret")
	run := &fw.Run{ID: "r1", Agent: "default", State: fw.StateRunning, Seq: 4, CreatedAt: time.Now().Add(-time.Minute), UpdatedAt: time.Now()}
	next := make(chan struct{})
	router := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(headerTargetActor) != "team/rev" {
			http.Error(w, "unexpected actor", http.StatusBadRequest)
			return
		}
		switch {
		case r.URL.Path == "/v1/runs" && r.Header.Get("Authorization") == "Bearer fleet-secret":
			// The front reads the runs of the Task with the static token.
			_ = json.NewEncoder(w).Encode(map[string]any{"runs": []*fw.Run{run}})
		case r.URL.Path == "/v1/runs/r1/live" && r.Header.Get("Authorization") == "Bearer fleet-secret" &&
			r.Header.Get(HeaderOnBehalfOf) == fw.KindAnonymous && r.Header.Get(HeaderVia) == "front":
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			w.Header().Set("X-Accel-Buffering", "no")
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, "event: delta\ndata: {\"turn\":1,\"text\":\"Hel\"}\n\n")
			w.(http.Flusher).Flush()
			select {
			case <-next:
			case <-r.Context().Done():
				return
			}
			fmt.Fprint(w, "event: done\ndata: {\"turn\":1,\"stopReason\":\"end_turn\"}\n\n")
			w.(http.Flusher).Flush()
		default:
			http.Error(w, "unexpected request "+r.URL.Path, http.StatusBadRequest)
		}
	}))
	t.Cleanup(router.Close)
	ax := newFakeAX(t, map[string]map[string]string{"team": {"rev": "Running"}})
	front := newMember(t, fw.NewMemBus(), "front", func(c *fw.Config, fc *Config) {
		fc.Atespaces = []string{"team"}
		c.AX.Server = ax.addr
		c.AX.Router = strings.TrimPrefix(router.URL, "http://")
	}, fw.WithSurfaces(native.New()))
	eventually(t, "the server sees the run of the Task", func() bool {
		r, err := front.f.Run(context.Background(), "r1")
		return err == nil && r.Member == "team/rev"
	})
	h, err := front.a.Handler()
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/v1/runs/r1/live", nil)
	req.Header.Set("Authorization", "Bearer tok")
	req.Header.Set("Accept", "text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "text/event-stream" || resp.Header.Get("Cache-Control") != "no-cache" || resp.Header.Get("X-Accel-Buffering") != "no" {
		t.Fatalf("live = %d %v", resp.StatusCode, resp.Header)
	}
	events := make(chan string, 4)
	go func() {
		defer close(events)
		sc := bufio.NewScanner(resp.Body)
		var ev []string
		for sc.Scan() {
			if sc.Text() == "" {
				events <- strings.Join(ev, "\n")
				ev = nil
				continue
			}
			ev = append(ev, sc.Text())
		}
	}()
	wait := func(what string) string {
		t.Helper()
		select {
		case ev, ok := <-events:
			if !ok {
				t.Fatalf("the stream ended before %s", what)
			}
			return ev
		case <-time.After(5 * time.Second):
			t.Fatalf("no %s within 5 seconds: the relay holds the stream back", what)
		}
		return ""
	}
	// The member holds the second event back until the first one arrived.
	if ev := wait("first event"); ev != "event: delta\ndata: {\"turn\":1,\"text\":\"Hel\"}" {
		t.Fatalf("first event = %q", ev)
	}
	close(next)
	if ev := wait("second event"); ev != "event: done\ndata: {\"turn\":1,\"stopReason\":\"end_turn\"}" {
		t.Fatalf("second event = %q", ev)
	}
	select {
	case ev, ok := <-events:
		if ok {
			t.Fatalf("an event after the end of the stream: %q", ev)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the stream stays open after the member closed it")
	}
}
