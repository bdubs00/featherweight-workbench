package bridges

// This file finds the runs of a bridge and acts on them. The runs of a
// bridge are the runs of its batch: the runs of this process and the runs
// that the view of the fleet holds (docs/ARCHITECTURE.md section 4.9). An
// action on a run of another member goes to the Task that owns it through
// the forward of the fleet, with the static token of the fleet, so the
// owner records the front as the caller and the person in the body: the
// sender of a message, the person of a reaction. An action on a run of
// this process calls the kernel.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/surface/native"
	"github.com/bdubs00/featherweight-workbench/fleet"
)

// callTimeout bounds one call to the owner of a run. A Task that sleeps
// wakes for the call, which takes a few seconds.
const callTimeout = 60 * time.Second

// asFront returns a context for the work of the front itself: no
// principal, so the kernel and the view see every run, and no deadline of
// a request, so a background change finishes.
func (s *Store) asFront(ctx context.Context) context.Context {
	return fw.WithPrincipal(context.WithoutCancel(ctx), nil)
}

// runsOf returns the runs of a bridge, by id: the runs of the batch of
// this process and of the view of the fleet. ctx carries no principal, so
// the list holds every run of the batch.
func (s *Store) runsOf(ctx context.Context, id string) (map[string]*fw.Run, error) {
	batch := BatchOf(id)
	out := map[string]*fw.Run{}
	local, err := s.a.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("bridges: list the runs: %w", err)
	}
	for _, r := range local {
		if r.Metadata[fw.MetaBatch] == batch {
			out[r.ID] = r
		}
	}
	v := fleet.Of(s.a)
	if v == nil {
		return out, nil
	}
	var remote []*fw.Run
	if bv, ok := v.(fleet.BatchView); ok {
		b, err := bv.Batch(ctx, batch)
		if err != nil {
			return nil, fmt.Errorf("bridges: list the runs of %s: %w", batch, err)
		}
		remote = b.Runs
	} else {
		all, err := v.Runs(ctx)
		if err != nil {
			return nil, fmt.Errorf("bridges: list the runs of the fleet: %w", err)
		}
		for _, r := range all {
			if r.Metadata[fw.MetaBatch] == batch {
				remote = append(remote, r)
			}
		}
	}
	for _, r := range remote {
		if _, ok := out[r.ID]; !ok {
			out[r.ID] = r
		}
	}
	return out, nil
}

// runOf returns one run of this process or of the view, or nil.
func (s *Store) runOf(ctx context.Context, id string) *fw.Run {
	if id == "" {
		return nil
	}
	if r, err := s.a.Get(ctx, id); err == nil {
		return r
	}
	if v := fleet.Of(s.a); v != nil {
		if r, err := v.Run(ctx, id); err == nil {
			return r
		}
	}
	return nil
}

// sorted returns runs in the order of their start, oldest first.
func sorted(runs map[string]*fw.Run) []*fw.Run {
	out := make([]*fw.Run, 0, len(runs))
	for _, r := range runs {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// taskStates returns the state of each member of the view by name, with
// TaskBlocked for a member that a storage failure blocked.
func (s *Store) taskStates(ctx context.Context) map[string]string {
	out := map[string]string{}
	v := fleet.Of(s.a)
	if v == nil {
		return out
	}
	members, err := v.Members(ctx)
	if err != nil {
		s.log.Debug("bridges: read the members of the fleet", "error", err)
		return out
	}
	for _, m := range members {
		state := m.State
		if m.BlockedReason != "" {
			state = TaskBlocked
		}
		out[m.Name] = state
	}
	return out
}

// taskOf returns the state of the Task that holds a run: live for a run
// of this process, the state of its member, or "" when the view does not
// know the member.
func taskOf(r *fw.Run, states map[string]string) string {
	if r == nil {
		return ""
	}
	if r.Member == "" {
		return fleet.MemberLive
	}
	return states[r.Member]
}

// seatTag reads the seat that the input of a child of a spawn bridge
// names in its first line: "[seat <handle>]".
func seatTag(input string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(input), "\n")
	rest, ok := strings.CutPrefix(strings.TrimSpace(line), "[seat ")
	if !ok {
		return ""
	}
	h, _, ok := strings.Cut(rest, "]")
	if !ok {
		return ""
	}
	return strings.TrimSpace(h)
}

// claim gives each run of the bridge that belongs to a seat and that no
// seat holds yet to its seat, and reports whether it changed the record.
// A run of a queue bridge names its seat in the metadata MetaMember. A
// child of the manager run of a spawn bridge names it in the first line of
// its input, else it takes the only seat of its agent that has no run.
func claim(rec *record, runs map[string]*fw.Run) bool {
	changed := false
	for _, r := range sorted(runs) {
		if rec.seatOfRun(r.ID) != "" {
			continue
		}
		h := r.Metadata[MetaMember]
		if h == "" && rec.Manager != nil && rec.Manager.Run != "" && r.Parent == rec.Manager.Run {
			h = seatTag(r.Input)
			if h == "" {
				var free []string
				for _, m := range rec.Members {
					if m.Agent == r.Agent && len(m.Runs) == 0 && !m.Fired {
						free = append(free, m.Handle)
					}
				}
				if len(free) == 1 {
					h = free[0]
				}
			}
		}
		m := rec.member(h)
		if m == nil || m.Fired {
			continue
		}
		m.Runs = append(m.Runs, r.ID)
		if len(m.Runs) > maxSeatRuns {
			m.Runs = m.Runs[len(m.Runs)-maxSeatRuns:]
		}
		changed = true
	}
	return changed
}

// maxSeatRuns bounds the runs that a seat lists.
const maxSeatRuns = 32

// seatOrAbove returns the seat of a run of the bridge, or of the nearest
// run above it that has one: a helper that a seat started belongs to that
// seat. It returns HandleManager for the manager run itself, and "" for a
// run of no seat, such as a child of the manager that holds no seat.
func seatOrAbove(rec *record, r *fw.Run, runs map[string]*fw.Run) string {
	for depth := 0; r != nil && depth < 16; depth++ {
		if h := rec.seatOfRun(r.ID); h != "" {
			return h
		}
		if h := r.Metadata[MetaMember]; h != "" && rec.member(h) != nil {
			return h
		}
		if rec.Manager != nil && r.ID == rec.Manager.Run {
			if depth == 0 {
				return HandleManager
			}
			return ""
		}
		if r.Parent == "" {
			return ""
		}
		r = runs[r.Parent]
	}
	return ""
}

// ownerError is an answer of the owner of a run other than 2xx.
type ownerError struct {
	member string
	status int
	detail string
}

func (e *ownerError) Error() string {
	return fmt.Sprintf("bridges: the fleet member %s answered %d: %s", e.member, e.status, e.detail)
}

// retryable reports whether a failed call to the owner of a run can work
// on a later try: the owner did not answer, answered 5xx or 429, or the
// view knows no route to it yet.
func retryable(err error) bool {
	var oe *ownerError
	if errors.As(err, &oe) {
		return oe.status >= 500 || oe.status == http.StatusTooManyRequests
	}
	if errors.Is(err, fleet.ErrNoCredential) || errors.Is(err, fw.ErrInvalidRequest) || errors.Is(err, fw.ErrForbidden) {
		return false
	}
	return !errors.Is(err, context.Canceled)
}

// asToken returns ctx with the static token of the fleet as the credential
// of a forward, and without a principal.
func (s *Store) asToken(ctx context.Context) (context.Context, error) {
	env := strings.TrimSpace(s.a.Config().Auth.TokenEnv)
	if env == "" {
		env = "FW_API_TOKEN"
	}
	tok, err := fw.EnvSecrets{}.Get(ctx, env)
	if err != nil || strings.TrimSpace(tok) == "" {
		return nil, fmt.Errorf("bridges: %w: no static token in the secret %s", fleet.ErrNoCredential, env)
	}
	return fw.WithCredential(s.asFront(ctx), "Bearer "+strings.TrimSpace(tok)), nil
}

// call sends a request to a member through the forward of the fleet, with
// the static token, and decodes a JSON answer into out when out is not
// nil. A body of files (filesBody) streams, and any other body goes as
// JSON.
func (s *Store) call(ctx context.Context, member, method, path string, body any, idem string, out any) error {
	v := fleet.Of(s.a)
	if v == nil {
		return fmt.Errorf("bridges: %w: the fleet is off, so the server reaches no member", fleet.ErrNoRoute)
	}
	ctx, err := s.asToken(ctx)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	var rd io.Reader = http.NoBody
	fb, streamed := body.(*filesBody)
	switch {
	case streamed:
		rd = fb.Reader()
	case body != nil:
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("bridges: encode the request for %s: %w", member, err)
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, path, rd)
	if err != nil {
		return fmt.Errorf("bridges: build the request for %s: %w", member, err)
	}
	if streamed {
		req.ContentLength = fb.Len()
		req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(fb.Reader()), nil }
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if idem != "" {
		req.Header.Set("Idempotency-Key", idem)
	}
	resp, err := v.Forward(ctx, member, req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return fmt.Errorf("bridges: read the answer of %s: %w", member, err)
	}
	if resp.StatusCode >= http.StatusMultipleChoices {
		var p struct {
			Detail string `json:"detail"`
		}
		detail := strings.TrimSpace(string(b))
		if json.Unmarshal(b, &p) == nil && p.Detail != "" {
			detail = p.Detail
		}
		detail, _ = cutText(detail, 500)
		return &ownerError{member: member, status: resp.StatusCode, detail: detail}
	}
	if out != nil && len(b) > 0 {
		if err := json.Unmarshal(b, out); err != nil {
			return fmt.Errorf("bridges: the answer of %s is not JSON: %w", member, err)
		}
	}
	return nil
}

// runPath is the path of a route of a run.
func runPath(id, rest string) string { return "/v1/runs/" + url.PathEscape(id) + rest }

// send gives a run a follow-up message from a party. A pinned follow-up
// survives every compaction of the run (fw.WithMessagePinned, and pinned
// in the body of the route of the owner).
func (s *Store) send(ctx context.Context, r *fw.Run, content string, from fw.MessageFrom, pinned bool) error {
	return s.sendWith(ctx, r, content, from, pinned, nil, nil)
}

// sendWith is send with new limits for the run: the kernel applies them
// before the message (fw.WithSendBudget, and budget in the body of the
// route of the owner), so the turn that the message starts has them. A
// nil budget changes no limit. files go with the message: file blocks for
// a run of the front itself, and files in the body of the route of the
// owner, so the kernel of the run keeps them in its workspace.
func (s *Store) sendWith(ctx context.Context, r *fw.Run, content string, from fw.MessageFrom, pinned bool, budget *fw.Budget, files *upload) error {
	if r.Member == "" {
		opts := []fw.SendOption{fw.WithMessageFrom(from)}
		if pinned {
			opts = append(opts, fw.WithMessagePinned())
		}
		if budget != nil {
			opts = append(opts, fw.WithSendBudget(*budget))
		}
		blocks := []fw.Block{fw.Text(content)}
		if files != nil {
			blocks = append(blocks, native.Attachments(files.files)...)
		}
		return s.a.Send(s.asFront(ctx), r.ID, fw.Message{Role: fw.RoleUser, Content: blocks}, fw.OriginHTTP, opts...)
	}
	fields := map[string]any{"content": content, "from": from}
	if pinned {
		fields["pinned"] = true
	}
	if budget != nil {
		fields["budget"] = budget
	}
	var body any = fields
	if files != nil {
		fb, err := newFilesBody(fields, files.files)
		if err != nil {
			return fmt.Errorf("bridges: encode the request for %s: %w", r.Member, err)
		}
		body = fb
	}
	return s.call(ctx, r.Member, http.MethodPost, runPath(r.ID, "/messages"), body, "", nil)
}

// react records the reaction of a person on an event of a run.
func (s *Store) react(ctx context.Context, r *fw.Run, d fw.ReactionData) error {
	if r.Member == "" {
		return s.a.React(s.asFront(ctx), r.ID, d)
	}
	body := map[string]any{"seq": d.Seq, "reaction": d.Reaction, "note": d.Note, "by": d.By}
	if d.Removed {
		// Only a kernel that takes a reaction back knows the key, so a
		// reaction that stays leaves it out.
		body["remove"] = true
	}
	return s.call(ctx, r.Member, http.MethodPost, runPath(r.ID, "/reactions"), body, "", nil)
}

// cancelRun cancels a run.
func (s *Store) cancelRun(ctx context.Context, r *fw.Run) error {
	if r.Member == "" {
		return s.a.Cancel(s.asFront(ctx), r.ID)
	}
	return s.call(ctx, r.Member, http.MethodPost, runPath(r.ID, "/cancel"), nil, "", nil)
}

// events returns the events of a run from the seq from on, at most limit,
// and with from 0 the last limit events.
func (s *Store) events(ctx context.Context, r *fw.Run, from uint64, limit int) ([]fw.Event, error) {
	if r.Member == "" {
		start := from
		if start == 0 && r.Seq > uint64(limit) {
			start = r.Seq - uint64(limit) + 1
		}
		var out []fw.Event
		for e, err := range s.a.Ledger().Read(s.asFront(ctx), r.ID, start) {
			if err != nil {
				return nil, fmt.Errorf("bridges: read the ledger of run %s: %w", r.ID, err)
			}
			out = append(out, e)
			if len(out) >= limit && from > 0 {
				break
			}
		}
		if from == 0 && len(out) > limit {
			out = out[len(out)-limit:]
		}
		return out, nil
	}
	v := fleet.Of(s.a)
	if v == nil {
		return nil, fmt.Errorf("bridges: %w: the fleet is off", fleet.ErrNoRoute)
	}
	tctx, err := s.asToken(ctx)
	if err != nil {
		return nil, err
	}
	tctx, cancel := context.WithTimeout(tctx, callTimeout)
	defer cancel()
	_, events, _, err := fleet.MemberEvents(tctx, v, r.Member, r.ID, from, limit)
	if err != nil {
		var me *fleet.MemberError
		if errors.As(err, &me) {
			return nil, &ownerError{member: me.Member, status: me.Status, detail: me.Detail}
		}
		return nil, err
	}
	return events, nil
}
