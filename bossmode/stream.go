package bossmode

// This file serves GET /v1/bossmode/stream: one stream of server-sent
// events per tab of the page (docs/ARCHITECTURE.md section 17.3).

import (
	"bufio"
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/surface/native"
	"github.com/bdubs00/featherweight-workbench/fleet"
)

// streamRoute answers the stream of the agent, or 503 while the hub has
// not started.
func streamRoute(a *fw.Agent) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		h := Of(a)
		if h == nil {
			w.Header().Set("Retry-After", "5")
			native.WriteProblem(w, r, http.StatusServiceUnavailable, "bossmode: the stream of this process has not started yet")
			return
		}
		h.ServeStream(w, r)
	}
}

// ServeStream writes the events of the hub as server-sent events until the
// client goes away, the hub closes, or StreamLife ends. Each event has the
// fields id, a decimal counter of the process, event, the kind, and data,
// one line of JSON. Last-Event-ID resumes after that id from the ring of
// the newest RingSize events; an id that the ring no longer holds, or that
// is not an id of this process, gives one event reset with the data null
// and the id of the newest event, and the stream goes on from there. A
// comment line every KeepAlive keeps an idle stream open. Each event goes
// only to a caller who can see it: an event of the fleet as the view
// decides (fleet.Watcher.Visible), and an event of Publish as its Allow
// decides. A process serves at most MaxStreams streams and answers 429
// past that. The comment line ": connected" goes out at once after the
// headers, so a proxy that holds an answer until its first bytes passes
// the stream on at once.
func (h *Hub) ServeStream(w http.ResponseWriter, r *http.Request) {
	if !h.acquire() {
		w.Header().Set("Retry-After", "10")
		native.WriteProblem(w, r, http.StatusTooManyRequests,
			fmt.Sprintf("bossmode: this process serves %d streams already, the most that it serves; close a tab of the page or try again later", MaxStreams))
		return
	}
	defer h.release()
	ctx := r.Context()
	cursor, reset := h.resume(strings.TrimSpace(r.Header.Get("Last-Event-ID")))
	hd := w.Header()
	hd.Set("Content-Type", "text/event-stream")
	hd.Set("Cache-Control", "no-store")
	hd.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)
	// A write timeout of the server cuts a long stream, so the stream has none.
	_ = rc.SetWriteDeadline(time.Time{})
	bw := bufio.NewWriter(w)
	flush := func() bool {
		if bw.Flush() != nil {
			return false
		}
		_ = rc.Flush()
		return true
	}
	// A proxy such as Cloudflare holds an answer until its first bytes,
	// for about 4 seconds, and a stream may send no event for a long time.
	// The page skips a comment line.
	_, _ = bw.WriteString(": connected\n\n")
	if reset {
		writeFrame(bw, cursor, KindReset, []byte("null"))
	}
	if !flush() {
		return
	}
	tick := time.NewTicker(h.keepAlive)
	defer tick.Stop()
	end := time.NewTimer(h.life)
	defer end.Stop()
	failing := false
	for {
		events, last, lost, wake := h.since(cursor)
		wrote := false
		if lost {
			writeFrame(bw, last, KindReset, []byte("null"))
			cursor, wrote = last, true
		}
		for _, e := range events {
			cursor = e.id
			ok, err := h.visible(ctx, e)
			if err != nil {
				// A decision point that does not answer hides the event, and
				// the log says so once until a decision works again.
				if !failing {
					h.log.Warn("bossmode: the decision about an event failed, so the stream leaves it out", "kind", e.kind, "error", err)
				}
				failing = true
				continue
			}
			failing = false
			if ok {
				writeFrame(bw, e.id, e.kind, e.data)
				wrote = true
			}
		}
		if wrote {
			if !flush() {
				return
			}
			tick.Reset(h.keepAlive)
		}
		select {
		case <-ctx.Done():
			return
		case <-h.ctx.Done():
			return
		case <-end.C:
			return
		case <-tick.C:
			if _, err := bw.WriteString(": keep-alive\n\n"); err != nil || !flush() {
				return
			}
		case <-wake:
		}
	}
}

// visible decides whether the caller of ctx gets an event.
func (h *Hub) visible(ctx context.Context, e entry) (bool, error) {
	switch {
	case e.change != nil:
		wv, ok := fleet.Of(h.a).(fleet.Watcher)
		if !ok {
			return false, nil
		}
		return wv.Visible(ctx, *e.change)
	case e.allow != nil:
		return e.allow(ctx), nil
	}
	return true, nil
}

// writeFrame writes one event. JSON from encoding/json holds no line
// break, so the data fits on one line.
func writeFrame(bw *bufio.Writer, id uint64, kind string, data []byte) {
	bw.WriteString("id: ")
	bw.WriteString(strconv.FormatUint(id, 10))
	bw.WriteString("\nevent: ")
	bw.WriteString(kind)
	bw.WriteString("\ndata: ")
	bw.Write(data)
	bw.WriteString("\n\n")
}
