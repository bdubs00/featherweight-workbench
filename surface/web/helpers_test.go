package web

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/surface/native"
)

// script returns canned responses in order and records every request.
type script struct {
	mu    sync.Mutex
	resps []*fw.Response
	reqs  []*fw.Request
}

func newScript(resps ...*fw.Response) *script { return &script{resps: resps} }

func (s *script) model() fw.Model {
	return fw.ModelFunc{GenerateFn: func(_ context.Context, req *fw.Request) (*fw.Response, error) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.reqs = append(s.reqs, req)
		if len(s.resps) == 0 {
			return nil, errors.New("script: no response left")
		}
		r := s.resps[0]
		s.resps = s.resps[1:]
		return r, nil
	}}
}

func (s *script) requests() []*fw.Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*fw.Request(nil), s.reqs...)
}

func endTurn(text string) *fw.Response {
	return &fw.Response{Content: []fw.Block{fw.Text(text)}, StopReason: fw.StopEndTurn, Usage: fw.Usage{InputTokens: 10, OutputTokens: 5}}
}

func callTool(id, name, input string) *fw.Response {
	return &fw.Response{
		Content:    []fw.Block{fw.Text("calling"), {Type: fw.BlockToolUse, ID: id, Name: name, Input: json.RawMessage(input)}},
		StopReason: fw.StopToolUse,
		Usage:      fw.Usage{InputTokens: 10, OutputTokens: 5},
	}
}

// gatedModel blocks every call until gate closes. It signals each call on
// started when started is not nil.
func gatedModel(gate <-chan struct{}, started chan<- struct{}) fw.Model {
	return fw.ModelFunc{GenerateFn: func(ctx context.Context, _ *fw.Request) (*fw.Response, error) {
		if started != nil {
			select {
			case started <- struct{}{}:
			default:
			}
		}
		select {
		case <-gate:
			return endTurn("released"), nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}}
}

func rmTool(calls *atomic.Int32) fw.Tool {
	return fw.NewTool(fw.ToolSpec{Name: "rm", Description: "remove", Destructive: true}, func(context.Context, json.RawMessage) (fw.ToolResult, error) {
		calls.Add(1)
		return fw.TextResult("removed"), nil
	})
}

func newAgent(t *testing.T, m fw.Model, opts ...fw.Option) *fw.Agent {
	t.Helper()
	t.Setenv("FW_ATESPACE", "")
	base := []fw.Option{
		fw.WithoutDefaults(),
		fw.WithAuthn(fw.NoAuthn{}),
		fw.WithModel(m),
		fw.WithLedger(fw.NewMemLedger()),
		fw.WithBus(fw.NewMemBus()),
		fw.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))),
		fw.WithSurfaces(native.New()),
	}
	a, err := fw.New(append(append(base, fw.WithTitles(fw.TitlesInput)), opts...)...)
	if err != nil {
		t.Fatalf("fw.New: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		a.Close(ctx)
	})
	return a
}

func serve(t *testing.T, a *fw.Agent) *httptest.Server {
	t.Helper()
	h, err := a.Handler()
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

func testCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// do sends a request and returns the response with its body read.
func do(t *testing.T, method, url, body string, header ...string) (*http.Response, []byte) {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, url, rd)
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, b
}

func decodeRun(t *testing.T, resp *http.Response, b []byte, status int) *fw.Run {
	t.Helper()
	if resp.StatusCode != status {
		t.Fatalf("status = %d, want %d: %s", resp.StatusCode, status, b)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content type = %q", ct)
	}
	var r fw.Run
	if err := json.Unmarshal(b, &r); err != nil {
		t.Fatalf("decode run: %v: %s", err, b)
	}
	return &r
}

func decodeProblem(t *testing.T, resp *http.Response, b []byte, status int) native.Problem {
	t.Helper()
	if resp.StatusCode != status {
		t.Fatalf("status = %d, want %d: %s", resp.StatusCode, status, b)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/problem+json" {
		t.Fatalf("content type = %q: %s", ct, b)
	}
	var p native.Problem
	if err := json.Unmarshal(b, &p); err != nil {
		t.Fatalf("decode problem: %v: %s", err, b)
	}
	if p.Type != "about:blank" || p.Status != status || p.Title != http.StatusText(status) || p.Detail == "" || p.Instance == "" {
		t.Fatalf("problem = %+v", p)
	}
	return p
}

// frame is one server-sent event, or one comment line.
type frame struct {
	id, event, data string
	comment         string
}

type sseReader struct{ sc *bufio.Scanner }

func newSSE(r io.Reader) *sseReader {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	return &sseReader{sc: sc}
}

// next returns the next frame, or false at the end of the stream.
func (s *sseReader) next() (frame, bool) {
	var f frame
	seen := false
	for s.sc.Scan() {
		line := s.sc.Text()
		if line == "" {
			if seen {
				return f, true
			}
			continue
		}
		seen = true
		switch {
		case strings.HasPrefix(line, ":"):
			f.comment = strings.TrimSpace(line[1:])
		case strings.HasPrefix(line, "id: "):
			f.id = line[len("id: "):]
		case strings.HasPrefix(line, "event: "):
			f.event = line[len("event: "):]
		case strings.HasPrefix(line, "data: "):
			f.data = line[len("data: "):]
		}
	}
	return f, false
}

// events reads the remaining event frames until the stream ends.
func (s *sseReader) events(t *testing.T) []fw.Event {
	t.Helper()
	var out []fw.Event
	for {
		f, ok := s.next()
		if !ok {
			return out
		}
		if f.comment != "" {
			continue
		}
		out = append(out, checkFrame(t, f))
	}
}

func checkFrame(t *testing.T, f frame) fw.Event {
	t.Helper()
	var e fw.Event
	if err := json.Unmarshal([]byte(f.data), &e); err != nil {
		t.Fatalf("frame data: %v: %q", err, f.data)
	}
	if f.id != strconv.FormatUint(e.Seq, 10) || f.event != e.Type {
		t.Fatalf("frame id %q event %q, data seq %d type %s", f.id, f.event, e.Seq, e.Type)
	}
	return e
}

func openStream(t *testing.T, ctx context.Context, method, url, body string, header ...string) *http.Response {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rd)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("stream status = %d: %s", resp.StatusCode, b)
	}
	for k, v := range map[string]string{"Content-Type": "text/event-stream", "Cache-Control": "no-cache", "X-Accel-Buffering": "no"} {
		if got := resp.Header.Get(k); got != v {
			t.Fatalf("%s = %q, want %q", k, got, v)
		}
	}
	return resp
}

func checkContiguous(t *testing.T, evs []fw.Event, first uint64) {
	t.Helper()
	if len(evs) == 0 {
		t.Fatal("no events")
	}
	for i, e := range evs {
		if e.Seq != first+uint64(i) {
			t.Fatalf("event %d has seq %d, want %d", i, e.Seq, first+uint64(i))
		}
	}
	if last := evs[len(evs)-1]; last.Type != fw.EvRunFinished {
		t.Fatalf("last event = %s", last.Type)
	}
}

func collect(t *testing.T, a *fw.Agent, id string) []fw.Event {
	t.Helper()
	ch, err := a.Events(testCtx(t), id, 0)
	if err != nil {
		t.Fatal(err)
	}
	var out []fw.Event
	for e := range ch {
		out = append(out, e)
	}
	return out
}
