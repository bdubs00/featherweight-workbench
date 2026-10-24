package away

// This file holds the seams of the watch on the fleet: the runs of the
// duties that wait, one run, the route of a decision and of a cancel, the
// queue of the manager, the decider of an approval in the ledger of its
// run, and the destructive mark of a tool. A run of this process goes to
// the kernel. A run of another member goes to the Task that holds it
// through the forward of the fleet, with the static token, as the routes
// of a person reach it (fleet relay).

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/duties"
	"github.com/bdubs00/featherweight-workbench/fleet"
)

// errGone means that the approval no longer waits: somebody decided it, or
// the run ended.
var errGone = errors.New("away: the approval no longer waits")

// callTimeout bounds one call to the Task that holds a run. A Task that
// sleeps wakes for the call, which takes a few seconds.
const callTimeout = 60 * time.Second

// asFront returns a context for the work of the front itself: no principal,
// so the kernel and the view see every run.
func asFront(ctx context.Context) context.Context { return fw.WithPrincipal(ctx, nil) }

// waitingOfFleet returns the runs of the duties that wait for a person:
// the runs of this process and of the view of the fleet.
func (s *Service) waitingOfFleet(ctx context.Context) ([]*fw.Run, error) {
	ctx = asFront(ctx)
	keep := func(r *fw.Run) bool {
		return r != nil && duties.NameOf(r) != "" && r.State == fw.StateInputRequired && r.Pending != nil
	}
	var out []*fw.Run
	local, err := s.a.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("away: list the runs: %w", err)
	}
	seen := map[string]bool{}
	for _, r := range local {
		if keep(r) {
			out = append(out, r)
			seen[r.ID] = true
		}
	}
	v := fleet.Of(s.a)
	if v == nil {
		return out, nil
	}
	remote, err := v.Runs(ctx)
	if err != nil {
		return out, fmt.Errorf("away: list the runs of the fleet: %w", err)
	}
	for _, r := range remote {
		if keep(r) && !seen[r.ID] {
			out = append(out, r)
		}
	}
	return out, nil
}

// runOfFleet returns one run of this process or of the view.
func (s *Service) runOfFleet(ctx context.Context, id string) (*fw.Run, error) {
	ctx = asFront(ctx)
	r, err := s.a.Get(ctx, id)
	if err == nil || !errors.Is(err, fw.ErrRunNotFound) {
		return r, err
	}
	v := fleet.Of(s.a)
	if v == nil {
		return nil, err
	}
	return v.Run(ctx, id)
}

// decideOnFleet decides an approval as the route of a decision does:
// POST /v1/runs/{id}/approvals/{approval} on the member that holds the
// run, or the kernel for a run of this process.
func (s *Service) decideOnFleet(ctx context.Context, r *fw.Run, approval, decision, note string) error {
	if r.Member == "" {
		err := s.a.Decide(asFront(ctx), r.ID, fw.ApprovalDecision{ApprovalID: approval, Decision: decision, Note: note})
		if errors.Is(err, fw.ErrNoPendingApproval) || errors.Is(err, fw.ErrRunNotFound) {
			return errGone
		}
		return err
	}
	path := "/v1/runs/" + url.PathEscape(r.ID) + "/approvals/" + url.PathEscape(approval)
	return s.callMember(ctx, r.Member, path, map[string]string{"decision": decision, "note": note})
}

// cancelOnFleet cancels a run as the route of a cancel does.
func (s *Service) cancelOnFleet(ctx context.Context, r *fw.Run) error {
	if r.Member == "" {
		err := s.a.Cancel(asFront(ctx), r.ID)
		if errors.Is(err, fw.ErrRunNotFound) {
			return errGone
		}
		return err
	}
	return s.callMember(ctx, r.Member, "/v1/runs/"+url.PathEscape(r.ID)+"/cancel", nil)
}

// callMember posts to a run route of a member with the static token of
// the fleet. An answer 404 or 409 means that the approval no longer
// waits.
func (s *Service) callMember(ctx context.Context, member, path string, body any) error {
	v := fleet.Of(s.a)
	if v == nil {
		return fmt.Errorf("away: %w: the fleet is off, so the front reaches no member", fleet.ErrNoRoute)
	}
	tok, err := fleet.Token(ctx, s.a.Config())
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(fw.WithCredential(asFront(ctx), "Bearer "+tok), callTimeout)
	defer cancel()
	var rd io.Reader = http.NoBody
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := v.Forward(ctx, member, req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	switch {
	case resp.StatusCode/100 == 2:
		return nil
	case resp.StatusCode == http.StatusNotFound, resp.StatusCode == http.StatusConflict:
		return errGone
	}
	var p struct {
		Detail string `json:"detail"`
	}
	detail := strings.TrimSpace(string(b))
	if json.Unmarshal(b, &p) == nil && p.Detail != "" {
		detail = p.Detail
	}
	return fmt.Errorf("away: the fleet member %s answered %d: %s", member, resp.StatusCode, trimmed(detail, 300))
}

// enqueueOnFleet puts a run request on the queue of the profile agent.
func (s *Service) enqueueOnFleet(ctx context.Context, agent string, req fw.RunRequest) error {
	_, sent, err := fleet.EnqueueFor(asFront(ctx), s.a, agent, "", []fw.RunRequest{req})
	if err == nil && sent == 0 {
		err = errors.New("no request went out")
	}
	return err
}

// deciderOnFleet reads who decided an approval from the ledger of its run:
// the subject of a person, and the verdict. It reports false when the
// ledger names no person, such as for the static token.
func (s *Service) deciderOnFleet(ctx context.Context, r *fw.Run, approval string) (string, string, bool) {
	var events []fw.Event
	if r.Member == "" {
		for e, err := range s.a.Ledger().Read(asFront(ctx), r.ID, 0) {
			if err != nil {
				return "", "", false
			}
			if e.Type == fw.EvApprovalDecided {
				events = append(events, e)
			}
		}
	} else {
		v := fleet.Of(s.a)
		tok, err := fleet.Token(ctx, s.a.Config())
		if v == nil || err != nil {
			return "", "", false
		}
		tctx, cancel := context.WithTimeout(fw.WithCredential(asFront(ctx), "Bearer "+tok), callTimeout)
		defer cancel()
		_, events, _, err = fleet.MemberEvents(tctx, v, r.Member, r.ID, 0, 100)
		if err != nil {
			return "", "", false
		}
	}
	for _, e := range events {
		if e.Type != fw.EvApprovalDecided {
			continue
		}
		var d fw.ApprovalDecidedData
		if json.Unmarshal(e.Data, &d) != nil || d.ApprovalID != approval {
			continue
		}
		if d.Decider == "" || (d.DeciderKind != "" && d.DeciderKind != fw.ActorPerson) {
			return "", "", false
		}
		return d.Decider, d.Decision, true
	}
	return "", "", false
}

// destructiveByPolicy reports whether the policy marks the tool of the
// approval destructive: the rule "ask destructive" asked for it, or the
// tool of this process with its name says so.
func (s *Service) destructiveByPolicy(ctx context.Context, r *fw.Run) bool {
	p := r.Pending
	if strings.EqualFold(strings.Join(strings.Fields(p.Reason), " "), "rule: ask destructive") {
		return true
	}
	specs, err := s.a.ToolSpecs(asFront(ctx))
	if err != nil {
		return false
	}
	for _, spec := range specs {
		if spec.Name == p.Tool && spec.Destructive {
			return true
		}
	}
	return false
}
