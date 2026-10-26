package fleet

// This file continues an earlier conversation (docs/ARCHITECTURE.md
// section 4.19). A conversation that a worker held ends when the pool
// replaces the worker, and the archive keeps it for reading only. A
// person who writes in it starts a new run with continues, the id of the
// earlier run: the server reads the earlier run for the person, puts a
// short summary of it in front of the message of the person, gives the
// new run the title of the earlier one, and marks it with
// MetaContinues. The new run then knows what the person and the agent
// said, and the kernel takes the request as any other start.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/surface/native"
)

// MetaContinues is the metadata key of a run that continues an earlier
// conversation. Its value is the id of the earlier run.
const MetaContinues = "workbench.continues"

// Bounds of the summary of an earlier conversation.
const (
	// SummaryMessages is the most messages of the person and of the agent
	// that a summary holds, the newest ones.
	SummaryMessages = 6
	// SummaryChars bounds a summary, its first line included, in
	// characters.
	SummaryChars = 2000
	// SummaryHead is the first line of a summary.
	SummaryHead = "Earlier in this conversation:"
	// continueEvents bounds the events of the earlier run that the
	// server reads from its member.
	continueEvents = 400
)

// Continuation is what a new run takes from an earlier conversation: the
// summary that goes in front of the message of the person, and the title
// of the earlier run.
type Continuation struct {
	Summary string
	Title   string
}

// Continue reads the earlier run id for the caller in ctx, from this
// process, from the member that holds it, or from the archive, and
// returns its summary and its title. A run that the caller cannot view is
// fw.ErrRunNotFound, as for a read of the run.
func Continue(ctx context.Context, a *fw.Agent, id string) (Continuation, error) {
	id = strings.TrimSpace(id)
	if id == "" || !indexKey.MatchString(id) {
		return Continuation{}, fmt.Errorf("%w: continues names no run", fw.ErrInvalidRequest)
	}
	run, events, err := earlierRun(ctx, a, id)
	if err != nil {
		return Continuation{}, err
	}
	return Continuation{Summary: summaryOf(events), Title: run.Title}, nil
}

// earlierRun returns the run id and its events, as the caller in ctx may
// read them.
func earlierRun(ctx context.Context, a *fw.Agent, id string) (*fw.Run, []fw.Event, error) {
	if run, err := a.Get(ctx, id); err == nil {
		var events []fw.Event
		for e, err := range a.Ledger().Read(ctx, id, 0) {
			if err != nil {
				return nil, nil, fmt.Errorf("fleet: read the run %s: %w", id, err)
			}
			events = append(events, e)
		}
		return run, events, nil
	} else if !errors.Is(err, fw.ErrRunNotFound) {
		return nil, nil, err
	}
	if v := Of(a); v != nil {
		run, err := v.Run(ctx, id)
		switch {
		case err == nil && run.Member != "":
			rctx, err := ForRun(ctx, a, id, fw.ActionRunView)
			if err != nil {
				return nil, nil, err
			}
			got, events, _, err := MemberEvents(rctx, v, run.Member, id, 0, continueEvents)
			if err != nil {
				return nil, nil, err
			}
			return got, events, nil
		case err != nil && !errors.Is(err, fw.ErrRunNotFound):
			return nil, nil, err
		}
	}
	run, events, ok, err := ArchivedLedger(ctx, a, id)
	switch {
	case err != nil:
		return nil, nil, err
	case !ok:
		return nil, nil, fmt.Errorf("%w: no run that you can view has the id %s", fw.ErrRunNotFound, id)
	}
	return run, TranscriptEvents(events), nil
}

// said is one message of an earlier conversation: of the person, or of
// the agent.
type said struct {
	agent bool
	text  string
}

// summaryOf writes the summary of the events of a run: SummaryHead, then
// the newest SummaryMessages messages of the person and of the agent, one
// line each, at most SummaryChars characters in all. A message of the
// person that holds the summary of an earlier conversation counts without
// it, so the summaries of a chain never nest. It is "" for a run without
// a message.
func summaryOf(events []fw.Event) string {
	var all []said
	add := func(agent bool, text string) {
		if !agent {
			text = withoutSummary(text)
		}
		if text = strings.Join(strings.Fields(text), " "); text != "" {
			all = append(all, said{agent: agent, text: text})
		}
	}
	for _, e := range events {
		switch e.Type {
		case fw.EvRunQueued:
			var d fw.RunQueuedData
			if e.Decode(&d) == nil {
				add(false, d.Input)
			}
		case fw.EvMessageReceived:
			var d fw.MessageReceivedData
			if e.Decode(&d) == nil && d.Role == fw.RoleUser {
				add(false, fw.TextOf(d.Content))
			}
		case fw.EvModelResponded:
			var d fw.ModelRespondedData
			if e.Decode(&d) == nil {
				add(true, fw.TextOf(d.Content))
			}
		}
	}
	if len(all) == 0 {
		return ""
	}
	if len(all) > SummaryMessages {
		all = all[len(all)-SummaryMessages:]
	}
	left := SummaryChars - utf8.RuneCountInString(SummaryHead)
	var lines []string
	for i := len(all) - 1; i >= 0 && left > 1; i-- {
		who := "The person: "
		if all[i].agent {
			who = "You: "
		}
		line := who + all[i].text
		n := utf8.RuneCountInString(line) + 1 // the line break before it
		if n > left {
			if len(lines) > 0 {
				break
			}
			// The newest message alone is too long: it keeps its start.
			r := []rune(line)
			line = string(r[:max(left-2, 0)]) + "…"
			n = left
		}
		lines = append([]string{line}, lines...)
		left -= n
	}
	return SummaryHead + "\n" + strings.Join(lines, "\n")
}

// withoutSummary returns the text of a message without the summary of an
// earlier conversation at its start: SummaryHead and its lines, up to the
// first empty line.
func withoutSummary(text string) string {
	if !strings.HasPrefix(text, SummaryHead) {
		return text
	}
	if i := strings.Index(text, "\n\n"); i >= 0 {
		return text[i+2:]
	}
	return ""
}

// Continued returns the input, the title, and the metadata of a new run
// that continues the earlier run of c: the summary in front of the input,
// the title of the earlier run when title is empty, and MetaContinues
// with the id.
func (c Continuation) Continued(id, input, title string, metadata map[string]string) (string, string, map[string]string) {
	if c.Summary != "" {
		input = c.Summary + "\n\n" + input
	}
	if strings.TrimSpace(title) == "" {
		title = c.Title
	}
	md := make(map[string]string, len(metadata)+1)
	for k, v := range metadata {
		md[k] = v
	}
	md[MetaContinues] = id
	return input, title, md
}

// continueBody turns the body of POST /v1/runs with the field continues
// into the body of a start that the kernel takes (Continue, Continued).
// A body without continues stays as it is. It reports whether it changed
// the body.
func continueBody(ctx context.Context, a *fw.Agent, body []byte) ([]byte, bool, error) {
	var fields map[string]json.RawMessage
	if json.Unmarshal(body, &fields) != nil {
		// The kernel says what is wrong with the body.
		return body, false, nil
	}
	raw, ok := fields["continues"]
	if !ok {
		return body, false, nil
	}
	var id string
	if json.Unmarshal(raw, &id) != nil || strings.TrimSpace(id) == "" {
		return nil, false, fmt.Errorf("%w: continues is the id of the run of the earlier conversation", fw.ErrInvalidRequest)
	}
	id = strings.TrimSpace(id)
	var input, title string
	var metadata map[string]string
	for _, f := range []struct {
		key string
		v   any
	}{{"input", &input}, {"title", &title}, {"metadata", &metadata}} {
		if v, ok := fields[f.key]; ok && json.Unmarshal(v, f.v) != nil {
			return nil, false, fmt.Errorf("%w: the field %s does not read", fw.ErrInvalidRequest, f.key)
		}
	}
	if strings.TrimSpace(input) == "" {
		return nil, false, fmt.Errorf("%w: the field input is required and must not be empty", fw.ErrInvalidRequest)
	}
	c, err := Continue(ctx, a, id)
	if err != nil {
		return nil, false, err
	}
	input, title, metadata = c.Continued(id, input, title, metadata)
	delete(fields, "continues")
	for k, v := range map[string]any{"input": input, "title": title, "metadata": metadata} {
		b, err := json.Marshal(v)
		if err != nil {
			return nil, false, fmt.Errorf("fleet: encode the field %s: %w", k, err)
		}
		fields[k] = b
	}
	out, err := json.Marshal(fields)
	if err != nil {
		return nil, false, fmt.Errorf("fleet: encode the body: %w", err)
	}
	return out, true, nil
}

// takeContinues rewrites the body of POST /v1/runs with continues before
// the relay or the kernel reads it (continueBody). It reports false when
// it wrote the problem of the request.
func (rl *Relay) takeContinues(w http.ResponseWriter, r *http.Request) bool {
	body, ok := readBody(w, r)
	if !ok {
		return false
	}
	next, _, err := continueBody(r.Context(), rl.a, body)
	switch {
	case errors.Is(err, fw.ErrRunNotFound), errors.Is(err, fw.ErrForbidden):
		// A run that the caller cannot read is a run that it cannot find,
		// as for a read of the run.
		native.WriteProblem(w, r, http.StatusNotFound, "continues names no run that you can view")
		return false
	case err != nil:
		fail(w, r, err)
		return false
	}
	r.Body = io.NopCloser(bytes.NewReader(next))
	r.ContentLength = int64(len(next))
	return true
}
