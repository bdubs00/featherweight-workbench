package fleet

// This file reads a run of another member for a surface of the front: the
// run object and a page of its events, through Forward. A worker serves the
// native API of the kernel alone, so the front builds its views for people,
// the result and the transcript, from the ledger of the owner.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// maxReadBytes bounds one answer of a member.
const maxReadBytes = 64 << 20

// MemberError is an error answer of a member. It unwraps to the error of
// the kernel that the status means, so a surface answers the caller with
// the same status: 404 is fw.ErrRunNotFound, 403 is fw.ErrForbidden, and
// 503 is fw.ErrAuthzUnavailable. Token is true when the request carried
// the static token for a person and the member refused it: the status is
// then 403, and the detail words the refusal for an operator
// (RefusedToken).
type MemberError struct {
	Member string
	Status int
	Detail string
	Token  bool
}

func (e *MemberError) Error() string {
	return fmt.Sprintf("fleet: the member %s answered %d: %s", e.Member, e.Status, e.Detail)
}

// Unwrap implements errors.Unwrap.
func (e *MemberError) Unwrap() error {
	switch e.Status {
	case http.StatusNotFound:
		return fw.ErrRunNotFound
	case http.StatusForbidden:
		return fw.ErrForbidden
	case http.StatusServiceUnavailable:
		return fw.ErrAuthzUnavailable
	}
	return nil
}

// errorOf turns an error answer into a MemberError, with the detail of a
// problem document when the body is one.
func errorOf(member string, status int, b []byte) *MemberError {
	var p struct {
		Detail string `json:"detail"`
	}
	detail := strings.TrimSpace(string(b))
	if json.Unmarshal(b, &p) == nil && p.Detail != "" {
		detail = p.Detail
	}
	if detail == "" {
		detail = http.StatusText(status)
	}
	if len(detail) > 500 {
		detail = detail[:500]
	}
	return &MemberError{Member: member, Status: status, Detail: detail}
}

// ReadJSON forwards a GET of path to a member with the credential in ctx
// and decodes the JSON answer into out. An error answer is a *MemberError.
// When ctx carries the static token for a person (ForRun), a 401 or a 403
// of the member is a 403 whose detail says that the worker refused the
// token of the fleet (RefusedToken).
func ReadJSON(ctx context.Context, v View, member, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, path, http.NoBody)
	if err != nil {
		return fmt.Errorf("fleet: build the request for the member %s: %w", member, err)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := v.Forward(ctx, member, req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxReadBytes))
	if err != nil {
		return fmt.Errorf("fleet: read the answer of the member %s: %w", member, err)
	}
	if resp.StatusCode >= http.StatusMultipleChoices {
		me := errorOf(member, resp.StatusCode, b)
		if env := TokenEnvOf(ctx); env != "" && (me.Status == http.StatusUnauthorized || me.Status == http.StatusForbidden) {
			me.Status, me.Detail, me.Token = http.StatusForbidden, RefusedToken(member, env, me.Status, me.Detail), true
		}
		return me
	}
	if err := json.Unmarshal(b, out); err != nil {
		return fmt.Errorf("fleet: the answer of the member %s is not JSON: %w", member, err)
	}
	return nil
}

// MemberRun reads a run of a member. The run carries the member of the
// view: the one it names, for a run that the member only knows about, else
// member.
func MemberRun(ctx context.Context, v View, member, id string) (*fw.Run, error) {
	var r fw.Run
	if err := ReadJSON(ctx, v, member, "/v1/runs/"+url.PathEscape(id), &r); err != nil {
		return nil, err
	}
	setMember(&r, member)
	return &r, nil
}

// MemberEvents reads a page of the events of a run of a member, as the
// events route of the kernel pages them: from the sequence number from, at
// most limit events, and with from 0 the last limit events. next is the
// sequence number where the next page starts, and 0 when none follows. The
// run comes with the page.
func MemberEvents(ctx context.Context, v View, member, id string, from uint64, limit int) (*fw.Run, []fw.Event, uint64, error) {
	if limit <= 0 {
		limit = 50
	}
	q := url.Values{}
	ask := limit
	if from > 0 {
		q.Set("from", strconv.FormatUint(from, 10))
		ask++ // one past the page marks where the next page starts
	}
	q.Set("limit", strconv.Itoa(ask))
	var ans struct {
		Run    *fw.Run    `json:"run"`
		Events []fw.Event `json:"events"`
	}
	if err := ReadJSON(ctx, v, member, "/v1/runs/"+url.PathEscape(id)+"/events?"+q.Encode(), &ans); err != nil {
		return nil, nil, 0, err
	}
	if ans.Run == nil {
		return nil, nil, 0, fmt.Errorf("fleet: the answer of the member %s holds no run", member)
	}
	setMember(ans.Run, member)
	events, next := ans.Events, uint64(0)
	if from > 0 && len(events) > limit {
		next = events[limit].Seq
		events = events[:limit]
	}
	if events == nil {
		events = []fw.Event{}
	}
	return ans.Run, events, next, nil
}

// MemberLedger reads every event of a run of a member, page by page.
func MemberLedger(ctx context.Context, v View, member, id string) (*fw.Run, []fw.Event, error) {
	const page = 500
	var all []fw.Event
	var run *fw.Run
	for from := uint64(1); ; {
		r, events, next, err := MemberEvents(ctx, v, member, id, from, page)
		if err != nil {
			return nil, nil, err
		}
		run = r
		all = append(all, events...)
		if next == 0 {
			return run, all, nil
		}
		from = next
	}
}

// setMember gives a run of a member the name of its member in the view.
func setMember(r *fw.Run, member string) {
	if r.Member != "" {
		r.Member = MemberName(r.Member)
	} else {
		r.Member = member
	}
}
