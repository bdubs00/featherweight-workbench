package connections

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/surface/native"
	"github.com/bdubs00/featherweight-kernel/tokens"
	"github.com/bdubs00/featherweight-workbench/fleet"
)

// tokenMap authenticates a bearer token as a principal.
type tokenMap map[string]*fw.Principal

func (m tokenMap) Authenticate(r *http.Request) (*fw.Principal, error) {
	tok := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	if p, ok := m[tok]; ok && tok != "" {
		return p, nil
	}
	return nil, fw.ErrUnauthorized
}

func (m tokenMap) Challenge(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="fw"`)
	w.WriteHeader(http.StatusUnauthorized)
}

// fakeView is a fleet view that knows a few runs of other members.
type fakeView struct{ runs map[string]*fw.Run }

func (fakeView) Name() string                                    { return "front" }
func (fakeView) Members(context.Context) ([]fleet.Member, error) { return nil, nil }
func (v fakeView) Runs(context.Context) ([]*fw.Run, error)       { return nil, nil }
func (fakeView) Forward(context.Context, string, *http.Request) (*http.Response, error) {
	return nil, fleet.ErrNoRoute
}

func (v fakeView) Run(_ context.Context, id string) (*fw.Run, error) {
	if r, ok := v.runs[id]; ok {
		return r, nil
	}
	return nil, fw.ErrRunNotFound
}

// brokerEnv is a front with a fake connector, a fake fleet, and the token
// broker on its native listener.
type brokerEnv struct {
	a   *fw.Agent
	fk  *fakeConnector
	url string
}

func newBrokerEnv(t *testing.T) *brokerEnv {
	t.Helper()
	people := tokenMap{
		"member-token": {Subject: fw.KindToken, Kind: fw.KindToken},
		"alice-token":  {Subject: "alice", Kind: fw.KindUser},
	}
	fk := newFakeConnector()
	a := newAgent(t, nil, fw.WithAuthn(people), fw.WithSurfaces(native.New()))
	Install(a, New(a, fk))
	fleet.Attach(a, fakeView{runs: map[string]*fw.Run{"r1": {ID: "r1", Principal: "ada", Member: "alpha"}}})
	t.Cleanup(func() { fleet.Attach(a, nil) })
	h, err := a.Handler()
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return &brokerEnv{a: a, fk: fk, url: srv.URL}
}

// post sends a request of the broker protocol and returns the status and
// the body.
func (e *brokerEnv) post(t *testing.T, token, path string, body any) (int, []byte) {
	t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, e.url+path, bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out bytes.Buffer
	_, _ = out.ReadFrom(resp.Body)
	return resp.StatusCode, out.Bytes()
}

func reasonOf(t *testing.T, body []byte) string {
	t.Helper()
	var ref tokens.Refusal
	if err := json.Unmarshal(body, &ref); err != nil {
		t.Fatalf("refusal = %s", body)
	}
	return ref.Reason
}

func TestBroker(t *testing.T) {
	e := newBrokerEnv(t)
	ask := tokens.TokenRequest{Subject: "ada", Server: "tickets", Run: "r1"}

	// A person cannot ask for the token of anybody.
	if status, body := e.post(t, "alice-token", tokens.PathToken, ask); status != http.StatusForbidden || reasonOf(t, body) != tokens.ReasonForbidden {
		t.Fatalf("as a person: %d %s", status, body)
	}
	if status, _ := e.post(t, "nobody", tokens.PathToken, ask); status != http.StatusUnauthorized {
		t.Fatalf("without a credential: %d", status)
	}
	// A member asks for a person who has not connected.
	if status, body := e.post(t, "member-token", tokens.PathToken, ask); status != http.StatusConflict || reasonOf(t, body) != tokens.ReasonNotConnected {
		t.Fatalf("not connected: %d %s", status, body)
	}
	e.fk.mu.Lock()
	e.fk.connected["ada"] = true
	e.fk.mu.Unlock()
	status, body := e.post(t, "member-token", tokens.PathToken, ask)
	var ans tokens.TokenAnswer
	if status != http.StatusOK || json.Unmarshal(body, &ans) != nil || ans.Token != "at-ada" {
		t.Fatalf("token: %d %s", status, body)
	}
	// The run must belong to the person, and the fleet must know it.
	if status, body := e.post(t, "member-token", tokens.PathToken, tokens.TokenRequest{Subject: "bob", Server: "tickets", Run: "r1"}); status != http.StatusForbidden || reasonOf(t, body) != tokens.ReasonForbidden {
		t.Fatalf("another person: %d %s", status, body)
	}
	if status, body := e.post(t, "member-token", tokens.PathToken, tokens.TokenRequest{Subject: "ada", Server: "tickets", Run: "r2"}); status != http.StatusNotFound || reasonOf(t, body) != tokens.ReasonUnknownRun {
		t.Fatalf("unknown run: %d %s", status, body)
	}
	if status, body := e.post(t, "member-token", tokens.PathToken, tokens.TokenRequest{Subject: "ada", Server: "docs", Run: "r1"}); status != http.StatusNotFound || reasonOf(t, body) != tokens.ReasonUnknownService {
		t.Fatalf("unknown service: %d %s", status, body)
	}
	if status, body := e.post(t, "member-token", tokens.PathToken, tokens.TokenRequest{Subject: "ada"}); status != http.StatusBadRequest || reasonOf(t, body) != tokens.ReasonInvalid {
		t.Fatalf("incomplete: %d %s", status, body)
	}
	// Reject and forget reach the connector.
	if status, _ := e.post(t, "member-token", tokens.PathReject, tokens.RejectRequest{Subject: "ada", Server: "tickets", Token: "at-ada"}); status != http.StatusNoContent {
		t.Fatalf("reject: %d", status)
	}
	if status, _ := e.post(t, "alice-token", tokens.PathForget, tokens.ForgetRequest{Subject: "ada", Server: "tickets"}); status != http.StatusForbidden {
		t.Fatalf("forget as a person: %d", status)
	}
	if status, _ := e.post(t, "member-token", tokens.PathForget, tokens.ForgetRequest{Subject: "ada", Server: "tickets"}); status != http.StatusNoContent {
		t.Fatalf("forget: %d", status)
	}
	e.fk.mu.Lock()
	connected := e.fk.connected["ada"]
	e.fk.mu.Unlock()
	if connected {
		t.Fatal("forget left the grant")
	}
	// Without a fleet the broker cannot check the run.
	fleet.Attach(e.a, nil)
	if status, body := e.post(t, "member-token", tokens.PathToken, ask); status != http.StatusServiceUnavailable || reasonOf(t, body) != tokens.ReasonNoFleet {
		t.Fatalf("no fleet: %d %s", status, body)
	}
}

// TestKernelClientAgainstTheBroker drives the kernel client of the broker
// against the routes, as a member does.
func TestKernelClientAgainstTheBroker(t *testing.T) {
	ctx := context.Background()
	e := newBrokerEnv(t)
	t.Setenv("FW_TEST_MEMBER_TOKEN", "member-token")
	member := newAgent(t, nil)
	c, err := tokens.New(member, e.url, "FW_TEST_MEMBER_TOKEN")
	if err != nil {
		t.Fatal(err)
	}
	rctx := fw.WithRunInfo(ctx, fw.RunInfo{RunID: "r1", Principal: "ada", PrincipalKind: fw.KindUser})
	if _, err := c.Token(rctx, "ada", "tickets"); err == nil || !strings.Contains(err.Error(), "has not connected") {
		t.Fatalf("Token before = %v", err)
	}
	e.fk.mu.Lock()
	e.fk.connected["ada"] = true
	e.fk.mu.Unlock()
	if tok, err := c.Token(rctx, "ada", "tickets"); err != nil || tok != "at-ada" {
		t.Fatalf("Token = %q, %v", tok, err)
	}
	c.Forget(rctx, "ada", "tickets")
	e.fk.mu.Lock()
	connected := e.fk.connected["ada"]
	e.fk.mu.Unlock()
	if connected {
		t.Fatal("Forget did not reach the broker")
	}
}
