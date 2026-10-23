package fleet

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/surface/native"
)

// callers maps a bearer token to a principal.
type callers map[string]*fw.Principal

func (c callers) Authenticate(r *http.Request) (*fw.Principal, error) {
	if p, ok := c[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")]; ok {
		return p, nil
	}
	return nil, fw.ErrUnauthorized
}

func (callers) Challenge(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusUnauthorized)
}

// people are the callers of the front of the tests of a person: alice
// owns the agent team, carol views it through the group ops, bob has no
// role, dave started a run of the agent bare and has no role either, and
// the static token of the fleet and a workload come with their own
// credential.
var people = callers{
	"alice-token":    {Subject: "alice", Kind: fw.KindUser},
	"carol-token":    {Subject: "carol", Kind: fw.KindUser, Groups: []string{"ops"}},
	"bob-token":      {Subject: "bob", Kind: fw.KindUser},
	"dave-token":     {Subject: "dave", Kind: fw.KindUser},
	"fleet-secret":   {Subject: fw.KindToken, Kind: fw.KindToken},
	"workload-token": {Subject: "spiffe://td/worker", Kind: fw.KindWorkload},
}

// memberCall is one request that reached the member through the router.
type memberCall struct {
	method, path, auth, person string
	body                       map[string]any
}

// personFront is a front in the rules mode that knows the member team/rev
// behind a fake router, which records each request and answers as the
// native API of the kernel answers: the run r1 of the agent team, which
// alice started and which waits for the approval a1, and the run r4 of the
// agent bare, which dave started. The token and the workload are admins of
// the front, so they see both runs.
type personFront struct {
	m   *member
	url string

	mu     sync.Mutex
	calls  []memberCall
	status int // the status of every answer of the member when set
}

func newPersonFront(t *testing.T) *personFront {
	t.Helper()
	t.Setenv("FW_ATESPACE", "")
	t.Setenv("FW_API_TOKEN", "fleet-secret")
	pf := &personFront{}
	now := time.Now()
	r1 := &fw.Run{ID: "r1", Agent: "team", State: fw.StateInputRequired, Principal: "alice", Seq: 5,
		Pending: &fw.Approval{ID: "a1", Tool: "bash", RequestedAt: now}, CreatedAt: now, UpdatedAt: now}
	r4 := &fw.Run{ID: "r4", Agent: "bare", State: fw.StateCompleted, Principal: "dave", Seq: 3, CreatedAt: now, UpdatedAt: now}
	router := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(headerTargetActor) != "team/rev" {
			http.Error(w, "unexpected actor", http.StatusBadRequest)
			return
		}
		c := memberCall{method: r.Method, path: r.URL.Path, auth: r.Header.Get("Authorization"), person: r.Header.Get(HeaderOnBehalfOf)}
		if b, _ := io.ReadAll(r.Body); len(b) > 0 {
			_ = json.Unmarshal(b, &c.body)
		}
		pf.mu.Lock()
		pf.calls = append(pf.calls, c)
		status := pf.status
		pf.mu.Unlock()
		if status != 0 {
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(map[string]any{"status": status, "detail": "forbidden: token run.operate needs an operator entry on the agent team"})
			return
		}
		run := *r1
		if strings.HasPrefix(r.URL.Path, "/v1/runs/r4") {
			run = *r4
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && (r.URL.Path == "/v1/runs/r1" || r.URL.Path == "/v1/runs/r4"):
			_ = json.NewEncoder(w).Encode(run)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/events"):
			_ = json.NewEncoder(w).Encode(map[string]any{"run": run, "events": []fw.Event{}})
		case r.Method == http.MethodPatch && strings.HasSuffix(r.URL.Path, "/budget"):
			_ = json.NewEncoder(w).Encode(map[string]any{"budget": map[string]any{"maxTurns": 5}, "run": run})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/messages"):
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(run)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/fork"):
			_ = json.NewEncoder(w).Encode(fw.Run{ID: "b1", Agent: run.Agent, State: fw.StateQueued, CreatedAt: now, UpdatedAt: now})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/runs":
			w.Header().Set("Location", "/v1/runs/n1")
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(fw.Run{ID: "n1", Agent: "team", State: fw.StateQueued, CreatedAt: now, UpdatedAt: now})
		case r.Method == http.MethodPost:
			_ = json.NewEncoder(w).Encode(run)
		default:
			http.Error(w, "unexpected request", http.StatusNotFound)
		}
	}))
	t.Cleanup(router.Close)
	pf.m = newMember(t, fw.NewMemBus(), "front", func(c *fw.Config, fc *Config) {
		rulesFront("closed")(c, fc)
		c.Access.Admins = append(c.Access.Admins, "kind:token", "kind:workload")
		c.AX.Router = strings.TrimPrefix(router.URL, "http://")
	}, fw.WithSurfaces(native.New()), fw.WithAuthn(people))
	inject(pf.m.f, "team/rev", "ax:team/rev", "team", MemberLive, now, r1, r4)
	h, err := pf.m.a.Handler()
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	pf.url = srv.URL
	return pf
}

// do sends a request to the front as who and returns the status and the
// body of the answer. header holds pairs of a name and a value.
func (pf *personFront) do(t *testing.T, method, path, body, who string, header ...string) (int, string) {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, pf.url+path, rd)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+who)
	req.Header.Set("Accept", "application/json")
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
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// reached returns the requests that reached the member so far.
func (pf *personFront) reached() []memberCall {
	pf.mu.Lock()
	defer pf.mu.Unlock()
	return append([]memberCall(nil), pf.calls...)
}

// last returns the last request that reached the member.
func (pf *personFront) last(t *testing.T) memberCall {
	t.Helper()
	calls := pf.reached()
	if len(calls) == 0 {
		t.Fatal("no request reached the member")
	}
	return calls[len(calls)-1]
}

// answer makes the member answer every request with status, or as the
// kernel does for 0.
func (pf *personFront) answer(status int) {
	pf.mu.Lock()
	defer pf.mu.Unlock()
	pf.status = status
}

// detail is the detail of a problem document.
func detail(t *testing.T, body string) string {
	t.Helper()
	var p struct {
		Detail string `json:"detail"`
	}
	if err := json.Unmarshal([]byte(body), &p); err != nil || p.Detail == "" {
		t.Fatalf("no problem document: %s", body)
	}
	return p.Detail
}

// The front decides an action of a person on a run of another member, and
// the request reaches the member with the static token of the fleet and
// the header that names the person, never with the credential of the
// person: a decision, a cancel, a change of the budget, a message, a
// reaction, a fork, and a read of the events.
func TestRelayActsForAPersonWithTheToken(t *testing.T) {
	pf := newPersonFront(t)
	for _, c := range []struct {
		method, path, body string
		status             int
	}{
		{http.MethodPost, "/v1/runs/r1/approvals/a1", `{"decision":"allow","note":"fine"}`, http.StatusOK},
		{http.MethodPost, "/v1/runs/r1/cancel", `{}`, http.StatusOK},
		{http.MethodPatch, "/v1/runs/r1/budget", `{"maxTurns":5}`, http.StatusOK},
		{http.MethodPost, "/v1/runs/r1/messages", `{"content":"once more"}`, http.StatusAccepted},
		{http.MethodPost, "/v1/runs/r1/reactions", `{"seq":0,"reaction":"up"}`, http.StatusOK},
		{http.MethodPost, "/v1/runs/r1/fork", `{"input":"another way"}`, http.StatusOK},
		{http.MethodGet, "/v1/runs/r1/events?limit=5", "", http.StatusOK},
	} {
		code, body := pf.do(t, c.method, c.path, c.body, "alice-token")
		if code != c.status || !strings.Contains(body, `"member":"team/rev"`) {
			t.Fatalf("%s %s = %d %s", c.method, c.path, code, body)
		}
		got := pf.last(t)
		if got.method != c.method || got.path != strings.Split(c.path, "?")[0] || got.auth != "Bearer fleet-secret" || got.person != "alice" {
			t.Fatalf("%s %s reached the member as %+v", c.method, c.path, got)
		}
	}
	for _, c := range pf.reached() {
		if strings.Contains(c.auth, "alice-token") {
			t.Fatalf("the credential of alice reached the member: %+v", c)
		}
	}
	// The bodies go on as the person sent them, and a reaction names her.
	calls := pf.reached()
	if calls[0].body["decision"] != "allow" || calls[0].body["note"] != "fine" || calls[4].body["by"] != "alice" || calls[4].body["reaction"] != "up" {
		t.Fatalf("the bodies = %+v, %+v", calls[0].body, calls[4].body)
	}
}

// A person whom the rules of the front refuse gets a 403 from the front
// with what the person needs on which agent, and the member gets no
// request. A viewer reads and acts on nothing, a person who sees no run
// gets 404, and a branch also needs the operator role on the agent.
func TestRelayRefusesAPersonAtTheFront(t *testing.T) {
	pf := newPersonFront(t)
	refused := func(method, path, body, who string, status int, want string) {
		t.Helper()
		before := len(pf.reached())
		code, b := pf.do(t, method, path, body, who)
		if code != status || (want != "" && detail(t, b) != want) {
			t.Fatalf("%s %s for %s = %d %s", method, path, who, code, b)
		}
		if strings.Contains(b, "issuer") || strings.Contains(b, "audience") {
			t.Fatalf("the refusal speaks of credentials: %s", b)
		}
		if n := len(pf.reached()) - before; n != 0 {
			t.Fatalf("%s %s for %s reached the member %d times", method, path, who, n)
		}
	}
	refused(http.MethodPost, "/v1/runs/r1/approvals/a1", `{"decision":"allow"}`, "carol-token", http.StatusForbidden,
		"you cannot answer an approval of the run r1: that needs the operator role on the agent team, or to be the one who started the run")
	refused(http.MethodPost, "/v1/runs/r1/cancel", `{}`, "carol-token", http.StatusForbidden,
		"you cannot cancel the run r1: that needs the operator role on the agent team, or to be the one who started the run")
	refused(http.MethodPatch, "/v1/runs/r1/budget", `{"maxTurns":5}`, "carol-token", http.StatusForbidden,
		"you cannot change the budget of the run r1: that needs the operator role on the agent team, or to be the one who started the run")
	refused(http.MethodPost, "/v1/runs/r1/messages", `{"content":"hi"}`, "carol-token", http.StatusForbidden,
		"you cannot send a message to the run r1: that needs the operator role on the agent team, or to be the one who started the run")
	refused(http.MethodPost, "/v1/runs/r1/fork", `{}`, "carol-token", http.StatusForbidden,
		"you cannot branch the run r1: that needs the operator role on the agent team, or to be the one who started the run")
	// Bob sees no run of team, so the run is not there for him.
	refused(http.MethodGet, "/v1/runs/r1/events", "", "bob-token", http.StatusNotFound, "")
	refused(http.MethodPost, "/v1/runs/r1/cancel", `{}`, "bob-token", http.StatusNotFound, "")
	// Dave started r4, so he operates it, but a branch is a new run of
	// bare, and he cannot run bare.
	refused(http.MethodPost, "/v1/runs/r4/fork", `{}`, "dave-token", http.StatusForbidden,
		"you cannot branch the run r4: a branch is a new run of the agent bare, and that needs the operator role on the agent bare")
	if code, b := pf.do(t, http.MethodPost, "/v1/runs/r4/cancel", `{}`, "dave-token"); code != http.StatusOK || pf.last(t).person != "dave" {
		t.Fatalf("dave cancels his run: %d %s", code, b)
	}
	// Carol views team, so she reads the events, with the token for her.
	if code, b := pf.do(t, http.MethodGet, "/v1/runs/r1/events", "", "carol-token"); code != http.StatusOK || pf.last(t).person != "carol" || pf.last(t).auth != "Bearer fleet-secret" {
		t.Fatalf("carol reads: %d %s, %+v", code, b, pf.last(t))
	}
	// A start needs the operator role on the agent at that member.
	refused(http.MethodPost, "/v1/runs?member=team/rev", `{"agent":"team","input":"x"}`, "carol-token", http.StatusForbidden,
		"you cannot start a run of the agent team on team/rev: that needs the operator role on the agent team")
	// A start that names no agent starts default, as the kernel does.
	refused(http.MethodPost, "/v1/runs?member=team/rev", `{"input":"x"}`, "alice-token", http.StatusForbidden,
		"you cannot start a run of the agent default on team/rev: that needs the operator role on the agent default")
	// The fields that only a trusted caller names: the sender of a message,
	// another person of a reaction, and the id of a run.
	refused(http.MethodPost, "/v1/runs/r1/messages", `{"content":"hi","from":{"kind":"agent","name":"x"}}`, "alice-token", http.StatusForbidden,
		"only the static token or a workload can name the sender of a message")
	refused(http.MethodPost, "/v1/runs/r1/messages", `{"content":"hi","FROM":{"kind":"agent","name":"x"}}`, "alice-token", http.StatusForbidden,
		"only the static token or a workload can name the sender of a message")
	refused(http.MethodPost, "/v1/runs/r1/reactions", `{"seq":0,"reaction":"up","by":"bob"}`, "alice-token", http.StatusForbidden,
		"only the static token or a workload can react for another person")
	refused(http.MethodPost, "/v1/runs?member=team/rev", `{"agent":"team","input":"x","id":"01K0000000000000000000000"}`, "alice-token", http.StatusForbidden,
		"only the static token or a workload can name the id of a run")
	refused(http.MethodPost, "/v1/runs?member=team/rev", `{"agent":"team","Agent":"bare","input":"x"}`, "alice-token", http.StatusBadRequest,
		"the body names the field agent more than once")
	refused(http.MethodPost, "/v1/runs/r1/messages", `{"content":"hi"} {"from":{"kind":"agent","name":"x"}}`, "alice-token", http.StatusBadRequest, "")
	// A sender of null names nobody, and a reaction of her own is hers.
	if code, b := pf.do(t, http.MethodPost, "/v1/runs/r1/messages", `{"content":"hi","from":null}`, "alice-token"); code != http.StatusAccepted || pf.last(t).body["from"] != nil {
		t.Fatalf("a message with no sender: %d %s, %+v", code, b, pf.last(t).body)
	}
	if code, b := pf.do(t, http.MethodPost, "/v1/runs/r1/reactions", `{"seq":0,"reaction":"up","By":"alice"}`, "alice-token"); code != http.StatusOK || pf.last(t).body["by"] != "alice" {
		t.Fatalf("a reaction of her own: %d %s, %+v", code, b, pf.last(t).body)
	}
}

// A start of a person on another member carries the person in principal
// and principalKind, which the member takes from the static token, so the
// run belongs to the person. A principal in the body of the person does
// not count, in any case of its name.
func TestRelayStartsARunForAPerson(t *testing.T) {
	pf := newPersonFront(t)
	code, b := pf.do(t, http.MethodPost, "/v1/runs?member=team/rev", `{"agent":"team","input":"x","principal":"mallory","PrincipalKind":"token"}`, "alice-token",
		native.HeaderIdempotencyKey, "k1")
	if code != http.StatusCreated || !strings.Contains(b, `"member":"team/rev"`) {
		t.Fatalf("start = %d %s", code, b)
	}
	got := pf.last(t)
	if got.path != "/v1/runs" || got.auth != "Bearer fleet-secret" || got.person != "alice" {
		t.Fatalf("the start reached the member as %+v", got)
	}
	want := map[string]any{"agent": "team", "input": "x", "principal": "alice", "principalKind": fw.KindUser}
	if len(got.body) != len(want) {
		t.Fatalf("the body = %+v", got.body)
	}
	for k, v := range want {
		if got.body[k] != v {
			t.Fatalf("the body = %+v", got.body)
		}
	}
	// The token names the person itself, and its body goes on as it is.
	code, b = pf.do(t, http.MethodPost, "/v1/runs?member=team/rev", `{"agent":"team","input":"x","principal":"alice"}`, "fleet-secret")
	if got := pf.last(t); code != http.StatusCreated || got.auth != "Bearer fleet-secret" || got.person != "" || got.body["principal"] != "alice" || got.body["principalKind"] != nil {
		t.Fatalf("the start of the token = %d %s, %+v", code, b, got)
	}
}

// A header Fw-On-Behalf-Of of the caller never reaches the member: the
// front names the person it decided about, and for the token and a
// workload it names nobody.
func TestRelayNeverPassesOnBehalfOfOfTheCaller(t *testing.T) {
	pf := newPersonFront(t)
	for who, want := range map[string]memberCall{
		"alice-token":    {auth: "Bearer fleet-secret", person: "alice"},
		"fleet-secret":   {auth: "Bearer fleet-secret"},
		"workload-token": {auth: "Bearer workload-token"},
	} {
		code, b := pf.do(t, http.MethodPost, "/v1/runs/r1/cancel", `{}`, who, HeaderOnBehalfOf, "mallory")
		if got := pf.last(t); code != http.StatusOK || got.auth != want.auth || got.person != want.person {
			t.Fatalf("%s: %d %s, the member got %+v", who, code, b, got)
		}
	}
}

// The static token and a workload act on another member with their own
// credential, and the member decides, as before: no decision of the front,
// no header, and the words of a refusal of the credential are for the
// caller. For a person, a refusal of the member is a refusal of the token
// of the fleet, with words for an operator, and a front without the token
// acts for no person.
func TestRelayKeepsTheCredentialOfTheTokenAndOfAWorkload(t *testing.T) {
	pf := newPersonFront(t)
	for _, who := range []string{"fleet-secret", "workload-token"} {
		for _, path := range []string{"/v1/runs/r1/cancel", "/v1/runs/r4/fork"} {
			code, b := pf.do(t, http.MethodPost, path, `{}`, who)
			if got := pf.last(t); code != http.StatusOK || got.auth != "Bearer "+who || got.person != "" {
				t.Fatalf("%s %s: %d %s, the member got %+v", who, path, code, b, got)
			}
		}
	}
	// A reaction of the token names any person, as the member allows it.
	if code, b := pf.do(t, http.MethodPost, "/v1/runs/r1/reactions", `{"seq":0,"reaction":"up","by":"bob"}`, "fleet-secret"); code != http.StatusOK || pf.last(t).body["by"] != "bob" {
		t.Fatalf("a reaction of the token: %d %s, %+v", code, b, pf.last(t).body)
	}

	pf.answer(http.StatusUnauthorized)
	if code, b := pf.do(t, http.MethodPost, "/v1/runs/r1/cancel", `{}`, "fleet-secret"); code != http.StatusForbidden ||
		detail(t, b) != "the fleet member team/rev did not accept your credential; both members need the same issuer and audience, or the same token" {
		t.Fatalf("a 401 for the token = %d %s", code, b)
	}
	if code, b := pf.do(t, http.MethodPost, "/v1/runs/r1/cancel", `{}`, "alice-token"); code != http.StatusForbidden ||
		detail(t, b) != "the worker team/rev refused the token of the fleet; check FW_API_TOKEN on that worker" {
		t.Fatalf("a 401 for alice = %d %s", code, b)
	}
	pf.answer(http.StatusForbidden)
	if code, b := pf.do(t, http.MethodPost, "/v1/runs/r1/cancel", `{}`, "alice-token"); code != http.StatusForbidden ||
		detail(t, b) != "the worker team/rev refused the token of the fleet: forbidden: token run.operate needs an operator entry on the agent team" {
		t.Fatalf("a 403 for alice = %d %s", code, b)
	}
	pf.answer(0)

	t.Setenv("FW_API_TOKEN", "")
	before := len(pf.reached())
	code, b := pf.do(t, http.MethodPost, "/v1/runs/r1/cancel", `{}`, "alice-token")
	if code != http.StatusServiceUnavailable || !strings.HasPrefix(detail(t, b), "the front holds no static token of the fleet: the secret FW_API_TOKEN is empty") || len(pf.reached()) != before {
		t.Fatalf("without the token = %d %s", code, b)
	}
}

// The run object that the front answers to a person names what the person
// can do, with one decision of the front and no request for it: alice
// operates r1, and carol only views it. The view answers while the member
// sleeps, and the member answers while it runs. The token gets no can.
func TestRelayTellsAPersonWhatSheCanDo(t *testing.T) {
	pf := newPersonFront(t)
	can := func(who string) *Can {
		t.Helper()
		code, b := pf.do(t, http.MethodGet, "/v1/runs/r1", "", who)
		var r struct {
			ID     string `json:"id"`
			Member string `json:"member"`
			Can    *Can   `json:"can"`
		}
		if err := json.Unmarshal([]byte(b), &r); code != http.StatusOK || err != nil || r.ID != "r1" || r.Member != "team/rev" {
			t.Fatalf("%s reads %d %s", who, code, b)
		}
		return r.Can
	}
	for _, state := range []string{MemberQuiet, MemberLive} {
		inject(pf.m.f, "team/rev", "ax:team/rev", "team", state, time.Now())
		before := len(pf.reached())
		if c := can("alice-token"); c == nil || !c.View || !c.Operate {
			t.Fatalf("%s: alice can %+v", state, c)
		}
		if c := can("carol-token"); c == nil || !c.View || c.Operate {
			t.Fatalf("%s: carol can %+v", state, c)
		}
		if c := can("fleet-secret"); c != nil {
			t.Fatalf("%s: the token can %+v", state, c)
		}
		reads := len(pf.reached()) - before
		switch {
		case state == MemberQuiet && reads != 0:
			t.Fatalf("the reads of a sleeping member reached it %d times", reads)
		case state == MemberLive && reads != 3:
			t.Fatalf("the reads of a live member reached it %d times", reads)
		}
		if state == MemberLive {
			calls := pf.reached()
			if got := calls[len(calls)-2]; got.auth != "Bearer fleet-secret" || got.person != "carol" {
				t.Fatalf("the read of carol reached the member as %+v", got)
			}
		}
	}
}

// IsPerson names a person: every principal but the process, the static
// token, and a workload.
func TestIsPerson(t *testing.T) {
	for _, c := range []struct {
		p    *fw.Principal
		want bool
	}{
		{nil, false},
		{&fw.Principal{Subject: "alice", Kind: fw.KindUser}, true},
		{&fw.Principal{Subject: fw.KindAnonymous, Kind: fw.KindAnonymous}, true},
		{&fw.Principal{Subject: "local:me", Kind: fw.KindLocal}, true},
		{&fw.Principal{Subject: fw.KindToken, Kind: fw.KindToken}, false},
		{&fw.Principal{Subject: "spiffe://td/w", Kind: fw.KindWorkload}, false},
	} {
		if got := IsPerson(c.p); got != c.want {
			t.Errorf("IsPerson(%+v) = %v", c.p, got)
		}
	}
}

// ForPerson gives the static token and the subject of the person, and it
// refuses a subject that no header carries as it is.
func TestForPerson(t *testing.T) {
	t.Setenv("FW_API_TOKEN", " fleet-secret\n")
	cfg := fw.DefaultConfig()
	ctx, err := ForPerson(context.Background(), cfg, &fw.Principal{Subject: "alice", Kind: fw.KindUser})
	if err != nil || fw.CredentialFrom(ctx) != "Bearer fleet-secret" || OnBehalfOf(ctx) != "alice" {
		t.Fatalf("ForPerson = %q %q, %v", fw.CredentialFrom(ctx), OnBehalfOf(ctx), err)
	}
	for _, s := range []string{"", " alice", "alice\n", "al\x00ice"} {
		if _, err := ForPerson(context.Background(), cfg, &fw.Principal{Subject: s, Kind: fw.KindUser}); err == nil {
			t.Errorf("ForPerson(%q) took the subject", s)
		}
	}
	cfg.Auth.TokenEnv = "OTHER_TOKEN"
	t.Setenv("OTHER_TOKEN", "")
	if _, err := ForPerson(context.Background(), cfg, &fw.Principal{Subject: "alice", Kind: fw.KindUser}); err == nil || !strings.Contains(err.Error(), "OTHER_TOKEN") {
		t.Fatalf("without the token = %v", err)
	}
}
