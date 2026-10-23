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
// that another member owns, and starts a run on another member. The front
// decides for a person, and the request goes with the static token and
// names the person.
func TestRelayActs(t *testing.T) {
	ctx := testCtx(t)
	p := newFleetPair(t)
	base := p.viewURL + "/v1/runs/" + p.remote.ID
	forPerson := func(what, person string) {
		t.Helper()
		h := p.fleet.forwarded()
		if len(h) == 0 {
			t.Fatalf("%s: nothing reached the owner", what)
		}
		last := h[len(h)-1]
		if last.Get("Authorization") != asToken || last.Get(fleet.HeaderOnBehalfOf) != person {
			t.Fatalf("%s: the owner got %q for %q", what, last.Get("Authorization"), last.Get(fleet.HeaderOnBehalfOf))
		}
	}

	// An approval through the view reaches the owner with the token for
	// alice, so an owner that knows the header records alice.
	resp, b := do(t, "POST", base+"/approvals/"+p.remote.Pending.ID, `{"decision":"allow","note":"fine"}`, "Authorization", asAlice)
	if r := decodeRun(t, resp, b, http.StatusOK); r.Member != "owner" {
		t.Fatalf("decided run = %+v", r)
	}
	forPerson("the approval", "alice")
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
	if d.Decision != "allow" || d.Note != "fine" {
		t.Fatalf("decided = %+v", d)
	}

	// A follow-up, then a cancel, both on the owner.
	resp, b = do(t, "POST", base+"/messages", `{"content":"once more"}`, "Authorization", asAlice)
	if r := decodeRun(t, resp, b, http.StatusAccepted); r.Member != "owner" {
		t.Fatalf("sent run = %+v", r)
	}
	forPerson("the message", "alice")
	if _, err := p.owner.WaitFor(ctx, p.remote.ID, func(r *fw.Run) bool { return r.State.Terminal() }); err != nil {
		t.Fatal(err)
	}
	resp, b = do(t, "POST", base+"/cancel", `{}`, "Authorization", asAlice)
	decodeRun(t, resp, b, http.StatusOK)
	forPerson("the cancel", "alice")

	// Bob only views the reviewer agent, so the front refuses him, and
	// nothing reaches the owner.
	before := len(p.fleet.forwarded())
	resp, b = do(t, "POST", base+"/cancel", `{}`, "Authorization", asBob)
	if pr := decodeProblem(t, resp, b, http.StatusForbidden); !strings.Contains(pr.Detail, "you cannot cancel the run "+p.remote.ID) ||
		!strings.Contains(pr.Detail, "the operator role on the agent reviewer") || len(p.fleet.forwarded()) != before {
		t.Fatalf("bob cancels: %s, forwarded %d", pr.Detail, len(p.fleet.forwarded())-before)
	}

	// A start on another member goes there for alice, and the run is hers;
	// a start on this member stays.
	resp, b = do(t, "POST", p.viewURL+"/v1/runs?member=owner", `{"agent":"shared","input":"remote"}`, "Authorization", asAlice, native.HeaderIdempotencyKey, "k1")
	started := decodeRun(t, resp, b, http.StatusCreated)
	if started.Member != "owner" || resp.Header.Get("Location") != "/v1/runs/"+started.ID {
		t.Fatalf("started = %+v, location %q", started, resp.Header.Get("Location"))
	}
	if got, err := p.owner.Get(context.Background(), started.ID); err != nil || got.Principal != "alice" {
		t.Fatalf("the run on the owner = %+v, %v", got, err)
	}
	forPerson("the start", "alice")
	if h := p.fleet.forwarded(); h[len(h)-1].Get(native.HeaderIdempotencyKey) != "k1" || h[len(h)-1].Get("Content-Type") != "application/json" {
		t.Errorf("forwarded headers = %v", h[len(h)-1])
	}
	// Bob cannot run reviewer, so the front starts nothing for him.
	before = len(p.fleet.forwarded())
	resp, b = do(t, "POST", p.viewURL+"/v1/runs?member=owner", `{"agent":"reviewer","input":"remote"}`, "Authorization", asBob)
	if pr := decodeProblem(t, resp, b, http.StatusForbidden); !strings.Contains(pr.Detail, "you cannot start a run of the agent reviewer on owner") || len(p.fleet.forwarded()) != before {
		t.Fatalf("bob starts: %s, forwarded %d", pr.Detail, len(p.fleet.forwarded())-before)
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
	// The owner does not accept the token of the fleet: the person gets a
	// 403, not a 401 that would sign her out here, and the words are for an
	// operator.
	p.fleet.fail(nil, "Bearer stranger")
	resp, b = get()
	if pr := decodeProblem(t, resp, b, http.StatusForbidden); pr.Detail != "the worker owner refused the token of the fleet; check FW_API_TOKEN on that worker" {
		t.Errorf("detail = %q", pr.Detail)
	}
	if resp.Header.Get("WWW-Authenticate") != "" {
		t.Errorf("the 403 carries a challenge")
	}
	// The result and the transcript word it in the same way.
	for _, path := range []string{"/result", "/transcript"} {
		resp, b = do(t, "GET", p.viewURL+"/v1/runs/"+p.remote.ID+path, "", "Authorization", asAlice)
		if pr := decodeProblem(t, resp, b, http.StatusForbidden); pr.Detail != "the worker owner refused the token of the fleet; check FW_API_TOKEN on that worker" {
			t.Errorf("%s: detail = %q", path, pr.Detail)
		}
	}
	// A caller with a credential of its own, the token, gets the words
	// about its own credential, as before.
	resp, b = do(t, "GET", p.viewURL+"/v1/runs/"+p.remote.ID, "", "Authorization", asToken)
	if pr := decodeProblem(t, resp, b, http.StatusForbidden); !strings.Contains(pr.Detail, "did not accept your credential") {
		t.Errorf("detail for the token = %q", pr.Detail)
	}
	// The owner refuses an action of the token: the words say so.
	p.fleet.fail(nil, asBob)
	resp, b = do(t, "POST", p.viewURL+"/v1/runs/"+p.remote.ID+"/cancel", `{}`, "Authorization", asAlice)
	if pr := decodeProblem(t, resp, b, http.StatusForbidden); !strings.HasPrefix(pr.Detail, "the worker owner refused the token of the fleet: ") || !strings.Contains(pr.Detail, "bob") {
		t.Errorf("detail of a refusal of the owner = %q", pr.Detail)
	}
	// Without the token of the fleet, the front acts for no person.
	p.fleet.fail(nil, "")
	t.Setenv("FW_API_TOKEN", "")
	resp, b = get()
	if pr := decodeProblem(t, resp, b, http.StatusServiceUnavailable); !strings.Contains(pr.Detail, "the front holds no static token of the fleet: the secret FW_API_TOKEN is empty") {
		t.Errorf("detail without a token = %q", pr.Detail)
	}
}

// While the owner sleeps, the view answers a read of the run object, and
// nothing reaches the owner, so the read wakes no Task. The result, the
// transcript, the events, and every write still go to the owner. Once the
// owner runs, it answers the read itself again.
func TestRelayReadsFromTheViewWhileTheOwnerSleeps(t *testing.T) {
	p := newFleetPair(t)
	base := p.viewURL + "/v1/runs/" + p.remote.ID
	p.fleet.sleep("owner", fleet.MemberQuiet)

	resp, b := do(t, "GET", base, "", "Authorization", asAlice)
	r := decodeRun(t, resp, b, http.StatusOK)
	if r.ID != p.remote.ID || r.Member != "owner" || r.State != fw.StateInputRequired || r.Pending == nil || r.Pending.ID != p.remote.Pending.ID {
		t.Fatalf("run from the view = %+v", r)
	}
	if n := len(p.fleet.forwarded()); n != 0 {
		t.Fatalf("a read of a sleeping owner reached it %d times", n)
	}
	// The answer tells a person what the person can do: alice owns the
	// agent, and bob only views it.
	for who, want := range map[string]string{asAlice: `"can":{"view":true,"operate":true}`, asBob: `"can":{"view":true,"operate":false}`} {
		resp, b := do(t, "GET", base, "", "Authorization", who)
		if resp.StatusCode != http.StatusOK || !strings.Contains(string(b), want) {
			t.Fatalf("%s reads %d %s", who, resp.StatusCode, b)
		}
	}
	if resp, b := do(t, "GET", base, "", "Authorization", asToken); resp.StatusCode != http.StatusOK || strings.Contains(string(b), `"can"`) {
		t.Fatalf("the token reads %d %s", resp.StatusCode, b)
	}
	if n := len(p.fleet.forwarded()); n != 0 {
		t.Fatalf("the reads of a sleeping owner reached it %d times", n)
	}
	// The answers that live on the worker go there, one request or more
	// each, with the static token for alice.
	for _, path := range []string{"/result", "/transcript", "/events?limit=2"} {
		before := len(p.fleet.forwarded())
		resp, b := do(t, "GET", base+path, "", "Authorization", asAlice, "Accept", "application/json")
		if resp.StatusCode != http.StatusOK || len(p.fleet.forwarded()) == before {
			t.Fatalf("%s = %d %s, forwarded %d", path, resp.StatusCode, b, len(p.fleet.forwarded())-before)
		}
		for _, h := range p.fleet.forwarded()[before:] {
			if h.Get("Authorization") != asToken || h.Get(fleet.HeaderOnBehalfOf) != "alice" {
				t.Fatalf("%s reached the owner with %v", path, h)
			}
		}
	}
	before := len(p.fleet.forwarded())
	resp, b = do(t, "POST", base+"/approvals/"+p.remote.Pending.ID, `{"decision":"allow"}`, "Authorization", asAlice)
	if r := decodeRun(t, resp, b, http.StatusOK); r.Member != "owner" || len(p.fleet.forwarded()) != before+1 {
		t.Fatalf("decided run = %+v, forwarded %d", r, len(p.fleet.forwarded())-before)
	}
	if _, err := p.owner.WaitFor(testCtx(t), p.remote.ID, func(r *fw.Run) bool { return r.State.Terminal() }); err != nil {
		t.Fatal(err)
	}

	// A live owner answers the read itself.
	p.fleet.sleep("owner", fleet.MemberLive)
	before = len(p.fleet.forwarded())
	resp, b = do(t, "GET", base, "", "Authorization", asAlice)
	if r := decodeRun(t, resp, b, http.StatusOK); r.Member != "owner" || len(p.fleet.forwarded()) != before+1 {
		t.Fatalf("run of a live owner = %+v, forwarded %d", r, len(p.fleet.forwarded())-before)
	}
}
