package bossmode

import (
	"bufio"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/surface/native"
	"github.com/bdubs00/featherweight-workbench/fleet"
)

// people takes the bearer token as the subject of a person, and
// staticToken as the static token of the fleet.
type people struct{}

// staticToken is the bearer token that people takes as the static token.
const staticToken = "the-static-token"

func (people) Authenticate(r *http.Request) (*fw.Principal, error) {
	tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if tok == "" || tok == r.Header.Get("Authorization") {
		return nil, fw.ErrUnauthorized
	}
	if tok == staticToken {
		return &fw.Principal{Subject: fw.KindToken, Kind: fw.KindToken}, nil
	}
	return &fw.Principal{Subject: tok, Kind: fw.KindUser}, nil
}

func (people) Challenge(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="fw"`)
	w.WriteHeader(http.StatusUnauthorized)
}

// front is a process with bossmode on, a fleet that reads the egress
// stream of the mem bus, and the native surface on a test server.
type front struct {
	a   *fw.Agent
	bus *fw.MemBus
	f   *fleet.Fleet
	h   *Hub
	url string
}

// newFront starts a front. mut changes the configuration before the
// start. The access is closed, so a person sees the runs that the person
// started, and everyone can run the profile manager.
func newFront(t *testing.T, mut func(*fw.Config)) *front {
	t.Helper()
	return newFrontOn(t, fw.NewMemBus(), mut)
}

// newFrontOn is newFront on the bus, so two fronts on one mem bus act as
// two replicas of one front.
func newFrontOn(t *testing.T, bus *fw.MemBus, mut func(*fw.Config)) *front {
	t.Helper()
	t.Setenv("FW_ATESPACE", "")
	t.Setenv(EnvEnabled, "on")
	cfg := fw.DefaultConfig()
	cfg.Home = t.TempDir()
	cfg.Access.Default = "closed"
	cfg.Agents = map[string]fw.AgentProfile{
		"default": {},
		"manager": {Access: &fw.Access{Operators: []string{"*"}}},
	}
	if mut != nil {
		mut(cfg)
	}
	a, err := fw.New(fw.WithConfig(cfg), fw.WithoutDefaults(), fw.WithAuthn(people{}),
		fw.WithModel(fw.ModelFunc{GenerateFn: func(context.Context, *fw.Request) (*fw.Response, error) {
			return &fw.Response{Content: []fw.Block{fw.Text("done")}, StopReason: fw.StopEndTurn}, nil
		}}),
		fw.WithLedger(fw.NewMemLedger()), fw.WithBus(bus), fw.WithSurfaces(native.New()),
		fw.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	if err != nil {
		t.Fatalf("fw.New: %v", err)
	}
	f, err := fleet.Open(context.Background(), a, fleet.Config{Name: "front", Egress: "fw.events", Interval: time.Hour, Expire: fleet.DefaultExpire})
	if err != nil {
		t.Fatalf("fleet.Open: %v", err)
	}
	fleet.Attach(a, f)
	h, err := a.Handler()
	if err != nil {
		t.Fatalf("Handler: %v", err)
	}
	srv := httptest.NewServer(h)
	cl, err := Start(context.Background(), a)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		srv.Close()
		if cl != nil {
			_ = cl.Close()
		}
		fleet.Attach(a, nil)
		_ = f.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		a.Close(ctx)
	})
	return &front{a: a, bus: bus, f: f, h: Of(a), url: srv.URL}
}

// event publishes one event of a worker on the egress stream and waits
// until the view applied it.
func (fr *front) event(t *testing.T, node, id string, seq uint64, typ string, data any) {
	t.Helper()
	e := fw.MustEvent(id, typ, data)
	e.Seq, e.Node = seq, node
	if err := fr.bus.Publish(context.Background(), "fw.events.team.default."+typ, e); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		r, err := fr.f.Run(context.Background(), id)
		if err == nil && r.Seq >= seq {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the view did not apply %s %d of %s", typ, seq, id)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// get makes a request with the bearer token of who, none for "".
func get(t *testing.T, url, who string, header ...string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if who != "" {
		req.Header.Set("Authorization", "Bearer "+who)
	}
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

// frame is one server-sent event, or a comment.
type frame struct {
	id, kind, data, comment string
}

// sse is an open stream that a test reads frame by frame.
type sse struct {
	resp   *http.Response
	frames chan frame
	cancel context.CancelFunc
}

// openStream opens the stream as who, with Last-Event-ID when last is
// not empty.
func openStream(t *testing.T, fr *front, who, last string) *sse {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, fr.url+"/v1/bossmode/stream", nil)
	req.Header.Set("Authorization", "Bearer "+who)
	if last != "" {
		req.Header.Set("Last-Event-ID", last)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		cancel()
		t.Fatalf("stream: status %d: %s", resp.StatusCode, b)
	}
	s := &sse{resp: resp, frames: make(chan frame, 8192), cancel: cancel}
	go func() {
		defer close(s.frames)
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		var f frame
		for sc.Scan() {
			line := sc.Text()
			switch {
			case line == "":
				if f != (frame{}) {
					s.frames <- f
				}
				f = frame{}
			case strings.HasPrefix(line, ":"):
				f.comment = strings.TrimSpace(strings.TrimPrefix(line, ":"))
			case strings.HasPrefix(line, "id: "):
				f.id = strings.TrimPrefix(line, "id: ")
			case strings.HasPrefix(line, "event: "):
				f.kind = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				f.data = strings.TrimPrefix(line, "data: ")
			}
		}
	}()
	t.Cleanup(s.close)
	return s
}

func (s *sse) close() {
	s.cancel()
	s.resp.Body.Close()
}

// next returns the next event, and skips the comments.
func (s *sse) next(t *testing.T) frame {
	t.Helper()
	for {
		select {
		case f, ok := <-s.frames:
			if !ok {
				t.Fatal("the stream ended")
			}
			if f.kind != "" {
				return f
			}
		case <-time.After(5 * time.Second):
			t.Fatal("no event arrived")
		}
	}
}

// until returns the events up to and with the first one of the kind.
func (s *sse) until(t *testing.T, kind string) []frame {
	t.Helper()
	var out []frame
	for {
		f := s.next(t)
		out = append(out, f)
		if f.kind == kind {
			return out
		}
	}
}

// newestID returns the id of the newest event of the hub.
func (h *Hub) newestID() uint64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.last
}
