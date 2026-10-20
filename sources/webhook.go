package sources

// This file serves the route of the webhook sources, POST
// /v1/sources/{name}. The body holds one event or an array of events.
// The answer is 202 with the run requests that the events made, 200 with
// a note when every event was a duplicate, and 503 with Retry-After when
// the store or the queue did not answer or another replica held an event:
// the sender sends the same request again, and every event that already
// made a run counts as a duplicate then.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/surface/native"
	"github.com/bdubs00/featherweight-workbench/fleet"
)

// RoutePattern is the route of the webhook sources.
const RoutePattern = "POST /v1/sources/{name}"

// maxBody is the largest body of a webhook request.
const maxBody = native.MaxBodyBytes

// deliverTimeout bounds the claims and the publish of one request.
const deliverTimeout = 30 * time.Second

// retryAfter is the value of Retry-After on a 503, in seconds.
const retryAfter = "5"

// extension mounts the route when the configuration names a webhook
// source. The route needs no credential of the native API: each source
// checks its own (auth.go). A configuration that does not load mounts
// nothing, and the service reports it.
func extension(k *native.Kit) error {
	a := k.Agent()
	list, err := Load(a.Config())
	if err != nil {
		return nil
	}
	webhooks := false
	for _, s := range list {
		webhooks = webhooks || s.Poll == nil
	}
	if !webhooks {
		return nil
	}
	k.HandlePublic(RoutePattern, func(w http.ResponseWriter, r *http.Request) {
		s := of(a)
		if s == nil {
			w.Header().Set("Retry-After", retryAfter)
			native.WriteProblem(w, r, http.StatusServiceUnavailable, "The sources of this process have not started yet.")
			return
		}
		s.ServeHTTP(w, r)
	})
	return nil
}

// answer is the body of a 202 or a 200. Runs lists the run requests that
// the events made, and Duplicates the events whose key made a run within
// the window. Event is the position of the event in the body, from 1.
type answer struct {
	Source     string     `json:"source"`
	Agent      string     `json:"agent"`
	Queue      string     `json:"queue"`
	Batch      string     `json:"batch"`
	Accepted   int        `json:"accepted"`
	Duplicate  int        `json:"duplicate"`
	Runs       []runReq   `json:"runs"`
	Duplicates []eventRef `json:"duplicates,omitempty"`
	Note       string     `json:"note,omitempty"`
}

// runReq is one run request that an event made.
type runReq struct {
	Event          int    `json:"event"`
	Key            string `json:"key"`
	IdempotencyKey string `json:"idempotencyKey"`
	Title          string `json:"title,omitempty"`
}

// eventRef names one event of a body.
type eventRef struct {
	Event int    `json:"event"`
	Key   string `json:"key"`
}

// ServeHTTP answers POST /v1/sources/{name}.
func (s *Sources) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	src := s.byName[name]
	if src == nil || src.poll != nil {
		native.WriteProblem(w, r, http.StatusNotFound, fmt.Sprintf("No webhook source is named %q.", name))
		return
	}
	now := s.now()
	// refuse counts n refused events with the reason and writes the
	// problem document. The reason and the detail hold no secret and no
	// part of the body.
	refuse := func(status, n int, reason, detail string) {
		src.count.refused(n, reason, now)
		s.log.Warn("sources: refused a request", "source", name, "status", status, "reason", reason)
		native.WriteProblem(w, r, status, detail)
	}
	if !src.verify.headers(r) {
		if c := src.verify.challenge(); c != "" {
			w.Header().Set("WWW-Authenticate", c)
		}
		refuse(http.StatusUnauthorized, 1, "the bearer token was missing or wrong", "The source needs its bearer token.")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			refuse(http.StatusRequestEntityTooLarge, 1, "the body was larger than the limit", fmt.Sprintf("The body is larger than %d bytes.", maxBody))
			return
		}
		refuse(http.StatusBadRequest, 1, "the body did not read", "The body did not read.")
		return
	}
	if !src.verify.body(r, body) {
		refuse(http.StatusUnauthorized, 1, "the signature was missing or wrong",
			fmt.Sprintf("The source needs the HMAC-SHA256 signature of the body in the header %s.", src.verify.header))
		return
	}
	events, err := decodeEvents(body, src.cfg.Items)
	if err != nil {
		refuse(http.StatusBadRequest, 1, err.Error(), "The events do not read: "+err.Error()+".")
		return
	}
	ans := answer{Source: name, Agent: src.cfg.Agent, Queue: src.queue, Batch: name, Runs: []runReq{}}
	if len(events) == 0 {
		ans.Note = "The body holds no event, so no run started."
		native.WriteJSON(w, http.StatusOK, ans)
		return
	}
	if len(events) > fleet.MaxEnqueue {
		refuse(http.StatusBadRequest, len(events), fmt.Sprintf("the body held %d events", len(events)),
			fmt.Sprintf("The body holds %d events; one request takes at most %d.", len(events), fleet.MaxEnqueue))
		return
	}
	items := make([]rendered, 0, len(events))
	for i, e := range events {
		it, err := src.render(i+1, e)
		if err != nil {
			refuse(http.StatusUnprocessableEntity, len(events), fmt.Sprintf("event %d: %s", i+1, err),
				fmt.Sprintf("Event %d does not fit the templates of the source, so no event of the request made a run: %s.", i+1, err))
			return
		}
		items = append(items, it)
	}
	// A caller that goes away does not stop the claims in the middle.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), deliverTimeout)
	defer cancel()
	d := s.deliver(ctx, src, items)
	for _, it := range d.accepted {
		ans.Runs = append(ans.Runs, runReq{Event: it.index, Key: it.key, IdempotencyKey: it.req.IdempotencyKey, Title: it.req.Metadata[fw.MetaTitle]})
	}
	for _, it := range d.duplicates {
		ans.Duplicates = append(ans.Duplicates, eventRef{Event: it.index, Key: it.key})
	}
	ans.Accepted, ans.Duplicate = len(d.accepted), len(d.duplicates)
	switch {
	case d.err != nil:
		cause := "The queue did not take the run requests"
		if errors.Is(d.err, ErrStore) {
			cause = "The de-duplication store did not answer"
		}
		w.Header().Set("Retry-After", retryAfter)
		native.WriteProblem(w, r, http.StatusServiceUnavailable, fmt.Sprintf(
			"%s, so %d of %d events made no run, and %d made a run. Send the request again: an event that made a run counts as a duplicate then.",
			cause, d.failed, len(items), len(d.accepted)))
	case len(d.busy) > 0:
		w.Header().Set("Retry-After", retryAfter)
		native.WriteProblem(w, r, http.StatusServiceUnavailable, fmt.Sprintf(
			"Another replica handles %d of %d events now, and %d made a run here. Send the request again: an event that made a run counts as a duplicate then.",
			len(d.busy), len(items), len(d.accepted)))
	case len(d.accepted) > 0:
		native.WriteJSON(w, http.StatusAccepted, ans)
	default:
		ans.Note = "Every event was a duplicate, so no run started."
		native.WriteJSON(w, http.StatusOK, ans)
	}
}
