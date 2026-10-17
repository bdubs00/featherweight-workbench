package web

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/surface/native"
	"github.com/bdubs00/featherweight-workbench/fleet"
)

// The relay of the fleet answers the run routes of the kernel for a run
// that another member owns, with the credential of the caller, and starts
// a run on another member.
func TestRelayActs(t *testing.T) {
	ctx := testCtx(t)
	p := newFleetPair(t)
	base := p.viewURL + "/v1/runs/" + p.remote.ID

	// An approval through the view reaches the owner, which records alice.
	resp, b := do(t, "POST", base+"/approvals/"+p.remote.Pending.ID, `{"decision":"allow","note":"fine"}`, "Authorization", asAlice)
	if r := decodeRun(t, resp, b, http.StatusOK); r.Member != "owner" {
		t.Fatalf("decided run = %+v", r)
	}
	r, err := p.owner.Wait(ctx, p.remote.ID)
	if err != nil || r.State != fw.StateCompleted || p.calls.Load() != 1 {
		t.Fatalf("owner run = %+v, %v, calls %d", r, err, p.calls.Load())
	}
	var d fw.ApprovalDecidedData
	for e, err := range p.owner.Ledger().Read(ctx, r.ID, 0) {
		if err != nil {
			t.Fatal(err)
		}
		if e.Type == fw.EvApprovalDecided {
			_ = e.Decode(&d)
		}
	}
	if d.Decider != "alice" || d.Note != "fine" {
		t.Fatalf("decided = %+v", d)
	}

	// A follow-up, then a cancel, both on the owner.
	resp, b = do(t, "POST", base+"/messages", `{"content":"once more"}`, "Authorization", asAlice)
	if r := decodeRun(t, resp, b, http.StatusAccepted); r.Member != "owner" {
		t.Fatalf("sent run = %+v", r)
	}
	if _, err := p.owner.WaitFor(ctx, p.remote.ID, func(r *fw.Run) bool { return r.State.Terminal() }); err != nil {
		t.Fatal(err)
	}
	resp, b = do(t, "POST", base+"/cancel", `{}`, "Authorization", asAlice)
	decodeRun(t, resp, b, http.StatusOK)

	// Bob only views the reviewer agent, so the owner refuses him.
	resp, b = do(t, "POST", base+"/cancel", `{}`, "Authorization", asBob)
	decodeProblem(t, resp, b, http.StatusForbidden)

	// A start on another member goes there; a start on this member stays.
	resp, b = do(t, "POST", p.viewURL+"/v1/runs?member=owner", `{"agent":"shared","input":"remote"}`, "Authorization", asAlice, native.HeaderIdempotencyKey, "k1")
	started := decodeRun(t, resp, b, http.StatusCreated)
	if started.Member != "owner" || resp.Header.Get("Location") != "/v1/runs/"+started.ID {
		t.Fatalf("started = %+v, location %q", started, resp.Header.Get("Location"))
	}
	if _, err := p.owner.Get(context.Background(), started.ID); err != nil {
		t.Fatalf("the owner lacks the run: %v", err)
	}
	if h := p.fleet.forwarded(); h[len(h)-1].Get(native.HeaderIdempotencyKey) != "k1" || h[len(h)-1].Get("Content-Type") != "application/json" {
		t.Errorf("forwarded headers = %v", h[len(h)-1])
	}
	resp, b = do(t, "POST", p.viewURL+"/v1/runs?member=view", `{"agent":"shared","input":"here"}`, "Authorization", asAlice)
	if r := decodeRun(t, resp, b, http.StatusCreated); r.Member != "" {
		t.Fatalf("local start = %+v", r)
	}
	resp, b = do(t, "POST", p.viewURL+"/v1/runs?member=owner", `{"input":"x"}`, "Authorization", asAlice, fleet.HeaderVia, "elsewhere")
	decodeProblem(t, resp, b, http.StatusBadGateway)
}

func TestRelayErrors(t *testing.T) {
	p := newFleetPair(t)
	get := func() (*http.Response, []byte) {
		return do(t, "GET", p.viewURL+"/v1/runs/"+p.remote.ID, "", "Authorization", asAlice)
	}
	p.fleet.fail(errors.Join(fleet.ErrNoRoute, errors.New("the member owner has no URL")), "")
	resp, b := get()
	if pr := decodeProblem(t, resp, b, http.StatusBadGateway); !strings.Contains(pr.Detail, "no URL") {
		t.Errorf("detail = %q", pr.Detail)
	}
	p.fleet.fail(fleet.ErrNoCredential, "")
	resp, b = get()
	decodeProblem(t, resp, b, http.StatusForbidden)
	p.fleet.fail(errors.New("dial tcp: connection refused"), "")
	resp, b = get()
	if pr := decodeProblem(t, resp, b, http.StatusBadGateway); !strings.Contains(pr.Detail, "did not answer") || strings.Contains(pr.Detail, "dial") {
		t.Errorf("detail = %q", pr.Detail)
	}
	// The owner does not accept the credential: the caller gets a 403, not a
	// 401 that would sign it out here.
	p.fleet.fail(nil, "Bearer stranger")
	resp, b = get()
	if pr := decodeProblem(t, resp, b, http.StatusForbidden); !strings.Contains(pr.Detail, "did not accept your credential") {
		t.Errorf("detail = %q", pr.Detail)
	}
	if resp.Header.Get("WWW-Authenticate") != "" {
		t.Errorf("the 403 carries a challenge")
	}
}
