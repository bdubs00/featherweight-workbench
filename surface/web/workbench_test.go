package web

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/surface/native"
	"github.com/bdubs00/featherweight-workbench/dashboard"
)

// answers is a model that gives the texts in order and then the last text
// on every call.
func answers(texts ...string) fw.Model {
	var n atomic.Int32
	return fw.ModelFunc{GenerateFn: func(context.Context, *fw.Request) (*fw.Response, error) {
		i := min(int(n.Add(1))-1, len(texts)-1)
		return endTurn(texts[i]), nil
	}}
}

// builderAccess lets alice operate the default agent, which runs the
// builder conversation. The configuration of manageAgent is closed.
var builderAccess = fw.WithProfile(fw.AgentProfile{Name: "default", Access: &fw.Access{Operators: []string{"alice"}}})

// keys returns the keys of a JSON object.
func keys(t *testing.T, b []byte) map[string]json.RawMessage {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("decode: %v: %s", err, b)
	}
	return m
}

func TestHomeRoute(t *testing.T) {
	ctx := testCtx(t)
	a := manageAgent(t, newScript(endTurn("one")).model())
	srv := serve(t, a)
	r, err := a.Run(fw.WithPrincipal(ctx, people["alice-token"]), "Review the build\nmore words", fw.AsAgent("reviewer"))
	if err != nil {
		t.Fatal(err)
	}

	resp, b := do(t, "GET", srv.URL+"/v1/home", "", "Authorization", asAlice)
	var h dashboard.Home
	decodeJSON(t, resp, b, http.StatusOK, &h)
	for _, k := range []string{"sentence", "glance", "needsYou", "inProgress", "recent", "others"} {
		if _, ok := keys(t, b)[k]; !ok {
			t.Errorf("home has no key %q: %s", k, b)
		}
	}
	if len(h.Recent) != 1 || h.Recent[0].ID != r.ID || h.Recent[0].Status != dashboard.StatusReady || h.Recent[0].Title != "Review the build" ||
		len(h.NeedsYou) != 0 || len(h.InProgress) != 0 || h.Others != 0 || h.Sentence == "" {
		t.Fatalf("alice home = %s", b)
	}
	// Bob views the run of alice, which is the work of another person for him.
	resp, b = do(t, "GET", srv.URL+"/v1/home", "", "Authorization", asBob)
	decodeJSON(t, resp, b, http.StatusOK, &h)
	if len(h.Recent) != 0 || h.Others != 1 || string(keys(t, b)["recent"]) != "[]" {
		t.Fatalf("bob home = %s", b)
	}
	resp, b = do(t, "GET", srv.URL+"/v1/home", "")
	decodeProblem(t, resp, b, http.StatusUnauthorized)
}

func TestHomeCoversTheFleet(t *testing.T) {
	p := newFleetPair(t)
	resp, b := do(t, "GET", p.viewURL+"/v1/home", "", "Authorization", asAlice)
	var h dashboard.Home
	decodeJSON(t, resp, b, http.StatusOK, &h)
	if len(h.NeedsYou) != 1 || h.NeedsYou[0].ID != p.remote.ID || h.NeedsYou[0].Member != "owner" ||
		len(h.Recent) != 1 || h.Recent[0].ID != p.local.ID {
		t.Fatalf("fleet home = %s", b)
	}
}

func TestResultRoute(t *testing.T) {
	ctx := testCtx(t)
	a := manageAgent(t, newScript(endTurn("All checks pass."), endTurn("shared")).model())
	srv := serve(t, a)
	actx := fw.WithPrincipal(ctx, people["alice-token"])
	r, err := a.Run(actx, "Check the build", fw.AsAgent("reviewer"))
	if err != nil {
		t.Fatal(err)
	}
	resp, b := do(t, "GET", srv.URL+"/v1/runs/"+r.ID+"/result", "", "Authorization", asBob)
	var res dashboard.Result
	decodeJSON(t, resp, b, http.StatusOK, &res)
	if res.RunID != r.ID || res.Status != dashboard.StatusReady || res.Summary != "All checks pass." || res.Agent != "reviewer" || res.Title != "Check the build" {
		t.Fatalf("result = %s", b)
	}
	for _, k := range []string{"runId", "title", "agent", "status", "summary", "started", "changed"} {
		if _, ok := keys(t, b)[k]; !ok {
			t.Errorf("result has no key %q: %s", k, b)
		}
	}
	shared, err := a.Run(actx, "shared work", fw.AsAgent("shared"))
	if err != nil {
		t.Fatal(err)
	}
	resp, b = do(t, "GET", srv.URL+"/v1/runs/"+shared.ID+"/result", "", "Authorization", asBob)
	decodeProblem(t, resp, b, http.StatusForbidden)
	resp, b = do(t, "GET", srv.URL+"/v1/runs/01NOPE/result", "", "Authorization", asAlice)
	if p := decodeProblem(t, resp, b, http.StatusNotFound); !strings.Contains(p.Detail, "01NOPE") {
		t.Errorf("detail = %q", p.Detail)
	}
	resp, b = do(t, "GET", srv.URL+"/v1/runs/"+r.ID+"/result", "")
	decodeProblem(t, resp, b, http.StatusUnauthorized)
}

func TestResultReachesTheOwner(t *testing.T) {
	p := newFleetPair(t)
	before := len(p.fleet.forwarded())
	resp, b := do(t, "GET", p.viewURL+"/v1/runs/"+p.remote.ID+"/result", "", "Authorization", asAlice)
	var res dashboard.Result
	decodeJSON(t, resp, b, http.StatusOK, &res)
	if res.RunID != p.remote.ID || res.Status != dashboard.StatusNeedsYou || res.Pending == nil || res.Pending.ID != p.remote.Pending.ID {
		t.Fatalf("remote result = %s", b)
	}
	if h := p.fleet.forwarded(); len(h) != before+1 || h[len(h)-1].Get(fw.HeaderFleetVia) != "view" {
		t.Fatalf("forwarded = %v", h)
	}
}

func TestTranscriptPaging(t *testing.T) {
	ctx := testCtx(t)
	a := manageAgent(t, newScript(endTurn("one"), endTurn("two"), endTurn("three")).model())
	srv := serve(t, a)
	actx := fw.WithPrincipal(ctx, people["alice-token"])
	r, err := a.Run(actx, "first", fw.AsAgent("reviewer"))
	if err != nil {
		t.Fatal(err)
	}
	for _, msg := range []string{"second", "third"} {
		if err := a.Send(actx, r.ID, fw.Message{Role: fw.RoleUser, Content: []fw.Block{fw.Text(msg)}}, fw.OriginHTTP); err != nil {
			t.Fatal(err)
		}
		if _, err := a.Wait(ctx, r.ID); err != nil {
			t.Fatal(err)
		}
	}
	run, err := a.Get(ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	last := run.Seq
	if last < 8 {
		t.Fatalf("the run has %d events", last)
	}
	type page struct {
		Run   *fw.Run
		Items []dashboard.Item
		Next  uint64
		Prev  uint64
	}
	get := func(query string) page {
		t.Helper()
		resp, b := do(t, "GET", srv.URL+"/v1/runs/"+r.ID+"/transcript"+query, "", "Authorization", asAlice)
		var p page
		decodeJSON(t, resp, b, http.StatusOK, &p)
		if _, ok := keys(t, b)["prev"]; !ok {
			t.Fatalf("no key prev: %s", b)
		}
		return p
	}

	// Without from and before, the page is the recent end, and prev names
	// the page in front.
	p := get("?limit=3")
	if p.Next != 0 || p.Prev != last-2 || p.Run.ID != r.ID {
		t.Fatalf("tail: next %d prev %d, last %d", p.Next, p.Prev, last)
	}
	if p.Items[len(p.Items)-1].Kind != dashboard.ItemEnd {
		t.Fatalf("tail items = %+v", p.Items)
	}
	// before walks back to the first page, where prev is 0. The pages
	// together hold every item of the run.
	all := get("?limit=1000")
	if all.Prev != 0 || all.Next != 0 {
		t.Fatalf("whole: next %d prev %d", all.Next, all.Prev)
	}
	items := p.Items
	for steps := 0; p.Prev != 0; steps++ {
		at := p.Prev
		p = get("?limit=3&before=" + strconv.FormatUint(at, 10))
		if p.Next != at {
			t.Fatalf("before=%d: next = %d", at, p.Next)
		}
		items = append(append([]dashboard.Item(nil), p.Items...), items...)
		if steps > int(last) {
			t.Fatal("the pages do not end")
		}
	}
	var got, want []string
	for _, it := range items {
		got = append(got, it.Kind+":"+it.Text)
	}
	for _, it := range all.Items {
		want = append(want, it.Kind+":"+it.Text)
	}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("paged items:\n%v\nwant:\n%v", got, want)
	}
	// before=0 is the recent end too, and a before past the end has no
	// next.
	if p := get("?limit=3&before=0"); p.Prev != last-2 || p.Next != 0 {
		t.Fatalf("before=0: next %d prev %d", p.Next, p.Prev)
	}
	if p := get("?limit=3&before=" + strconv.FormatUint(last+10, 10)); p.Prev != last-2 || p.Next != 0 {
		t.Fatalf("before past the end: next %d prev %d", p.Next, p.Prev)
	}
	// from keeps its meaning, and prev names the events in front.
	if p := get("?from=1&limit=2"); p.Prev != 0 || p.Next != 3 {
		t.Fatalf("from=1: next %d prev %d", p.Next, p.Prev)
	}
	if p := get("?from=3&limit=2"); p.Prev != 3 || p.Next != 5 {
		t.Fatalf("from=3: next %d prev %d", p.Next, p.Prev)
	}
	for _, q := range []string{"?from=1&before=5", "?before=x", "?before=-1", "?from=2&before=0"} {
		resp, b := do(t, "GET", srv.URL+"/v1/runs/"+r.ID+"/transcript"+q, "", "Authorization", asAlice)
		decodeProblem(t, resp, b, http.StatusBadRequest)
	}
}

func TestBuilderRoute(t *testing.T) {
	ctx := testCtx(t)
	a := manageAgent(t, answers("I reviewed it.", "Tell me more."), builderAccess)
	srv := serve(t, a)
	actx := fw.WithPrincipal(ctx, people["alice-token"])
	done, err := a.Run(actx, "Review pull request 42", fw.AsAgent("reviewer"))
	if err != nil {
		t.Fatal(err)
	}

	// input starts the builder conversation for the caller.
	resp, b := do(t, "POST", srv.URL+"/v1/builder", `{"input":"I need an agent that triages issues"}`, "Authorization", asAlice, native.HeaderIdempotencyKey, "b-1")
	r := decodeRun(t, resp, b, http.StatusCreated)
	if resp.Header.Get("Location") != "/v1/runs/"+r.ID || r.Metadata[dashboard.ViewKey] != dashboard.ViewBuilder || r.Title != "New agent" ||
		r.Input != "I need an agent that triages issues" || r.Principal != "alice" || r.Origin != fw.OriginHTTP {
		t.Fatalf("builder run = %s", b)
	}
	// The key makes a retry safe.
	resp, b = do(t, "POST", srv.URL+"/v1/builder", `{"input":"I need an agent that triages issues"}`, "Authorization", asAlice, native.HeaderIdempotencyKey, "b-1")
	if again := decodeRun(t, resp, b, http.StatusCreated); again.ID != r.ID {
		t.Fatalf("retry = %s, want run %s", again.ID, r.ID)
	}

	// fromRun builds the first message from the run.
	resp, b = do(t, "POST", srv.URL+"/v1/builder", `{"fromRun":"`+done.ID+`"}`, "Authorization", asAlice)
	saved := decodeRun(t, resp, b, http.StatusCreated)
	if saved.Metadata[dashboard.ViewKey] != dashboard.ViewBuilder || !strings.Contains(saved.Input, "Review pull request 42") || !strings.Contains(saved.Input, "I reviewed it.") {
		t.Fatalf("save as agent = %s", b)
	}

	for _, c := range []struct {
		body   string
		auth   string
		status int
	}{
		{`{}`, asAlice, http.StatusBadRequest},
		{`{"input":"  "}`, asAlice, http.StatusBadRequest},
		{`{"input":"x","fromRun":"` + done.ID + `"}`, asAlice, http.StatusBadRequest},
		{`{"input":"x","agent":"reviewer"}`, asAlice, http.StatusBadRequest},
		{``, asAlice, http.StatusBadRequest},
		{`{"fromRun":"01NOPE"}`, asAlice, http.StatusNotFound},
		{`{"input":"x"}`, "", http.StatusUnauthorized},
	} {
		resp, b := do(t, "POST", srv.URL+"/v1/builder", c.body, "Authorization", c.auth)
		p := decodeProblem(t, resp, b, c.status)
		if c.status == http.StatusNotFound && !strings.Contains(p.Detail, `"01NOPE"`) {
			t.Errorf("detail = %q", p.Detail)
		}
	}
	// A run that the caller cannot view is refused.
	shared, err := a.Run(actx, "shared work", fw.AsAgent("shared"))
	if err != nil {
		t.Fatal(err)
	}
	resp, b = do(t, "POST", srv.URL+"/v1/builder", `{"fromRun":"`+shared.ID+`"}`, "Authorization", asBob)
	decodeProblem(t, resp, b, http.StatusForbidden)
}

// A view of a run is not enough to turn it into an agent: the builder run
// belongs to the caller and the teams of the caller, so the content of a
// run of alice never reaches the teams of bob. Save as agent takes a
// completed top-level run that the caller started, for a caller who can
// create agents.
func TestBuilderFromRunTerms(t *testing.T) {
	ctx := testCtx(t)
	cfg := fw.DefaultConfig()
	cfg.Home = t.TempDir()
	cfg.Access.Default = "closed"
	cfg.Access.Creators = []string{"alice", "bob"}
	a := manageAgent(t, answers("I reviewed it.", "ok"), fw.WithConfig(cfg),
		fw.WithProfile(fw.AgentProfile{Name: "default", Access: &fw.Access{Operators: []string{"alice", "bob"}}}))
	srv := serve(t, a)
	done, err := a.Run(fw.WithPrincipal(ctx, people["alice-token"]), "Review pull request 42", fw.AsAgent("reviewer"))
	if err != nil || done.State != fw.StateCompleted {
		t.Fatalf("run = %+v, %v", done, err)
	}
	// bob views the run of alice, he can create agents, and he operates
	// the agent of the builder.
	resp, b := do(t, "GET", srv.URL+"/v1/runs/"+done.ID, "", "Authorization", asBob)
	decodeRun(t, resp, b, http.StatusOK)
	resp, b = do(t, "POST", srv.URL+"/v1/builder", `{"fromRun":"`+done.ID+`"}`, "Authorization", asBob)
	if p := decodeProblem(t, resp, b, http.StatusForbidden); !strings.Contains(p.Detail, "only the person who started run") {
		t.Errorf("detail = %q", p.Detail)
	}
	// A builder run is not work to hand off.
	resp, b = do(t, "POST", srv.URL+"/v1/builder", `{"input":"An agent for triage"}`, "Authorization", asAlice)
	builder := decodeRun(t, resp, b, http.StatusCreated)
	if _, err := a.Wait(ctx, builder.ID); err != nil {
		t.Fatal(err)
	}
	resp, b = do(t, "POST", srv.URL+"/v1/builder", `{"fromRun":"`+builder.ID+`"}`, "Authorization", asAlice)
	if p := decodeProblem(t, resp, b, http.StatusForbidden); !strings.Contains(p.Detail, "agent builder") {
		t.Errorf("detail = %q", p.Detail)
	}
}

func TestBuilderAttachments(t *testing.T) {
	root := t.TempDir()
	cfg := fw.DefaultConfig()
	cfg.Home = filepath.Join(root, ".fw")
	cfg.Tools.Files.Root = root
	a := newAgent(t, newScript(endTurn("ok")).model(), fw.WithConfig(cfg))
	srv := serve(t, a)
	data := base64.StdEncoding.EncodeToString([]byte("name,role\n"))
	resp, b := do(t, "POST", srv.URL+"/v1/builder", `{"input":"Make an agent for this sheet","files":[{"name":"team.csv","data":"`+data+`"}]}`)
	r := decodeRun(t, resp, b, http.StatusCreated)
	var files []fw.FileRef
	for _, e := range collect(t, a, r.ID) {
		var q fw.RunQueuedData
		if e.Type == fw.EvRunQueued && e.Decode(&q) == nil {
			for _, f := range q.Files {
				files = append(files, *f.File)
			}
		}
	}
	if len(files) != 1 || files[0].Path != "attachments/team.csv" {
		t.Fatalf("files = %+v", files)
	}
}

func TestBuilderReachesTheOwner(t *testing.T) {
	p := newFleetPair(t, builderAccess)
	// Only a completed run becomes an agent, so alice allows the call that
	// the remote run waits for first.
	resp, b := do(t, "POST", p.ownerURL+"/v1/runs/"+p.remote.ID+"/approvals/"+p.remote.Pending.ID, `{"decision":"allow"}`, "Authorization", asAlice)
	decodeRun(t, resp, b, http.StatusOK)
	if r, err := p.owner.Wait(testCtx(t), p.remote.ID); err != nil || r.State != fw.StateCompleted {
		t.Fatalf("remote run = %+v, %v", r, err)
	}
	before := len(p.fleet.forwarded())
	resp, b = do(t, "POST", p.viewURL+"/v1/builder", `{"fromRun":"`+p.remote.ID+`"}`, "Authorization", asAlice)
	r := decodeRun(t, resp, b, http.StatusCreated)
	if r.Member != "owner" || r.Metadata[dashboard.ViewKey] != dashboard.ViewBuilder || !strings.Contains(r.Input, "clean up") {
		t.Fatalf("remote builder run = %s", b)
	}
	if h := p.fleet.forwarded(); len(h) != before+1 || h[len(h)-1].Get("Authorization") != asAlice {
		t.Fatalf("forwarded = %v", h)
	}
	if _, err := p.owner.Get(testCtx(t), r.ID); err != nil {
		t.Fatalf("the owner has no builder run: %v", err)
	}
	// A run of this process stays here.
	resp, b = do(t, "POST", p.viewURL+"/v1/builder", `{"fromRun":"`+p.local.ID+`"}`, "Authorization", asAlice)
	if r := decodeRun(t, resp, b, http.StatusCreated); r.Member != "" || len(p.fleet.forwarded()) != before+1 {
		t.Fatalf("local builder run = %s", b)
	}
}

// requestOrigin takes the first value of each forwarded header, which is
// what the client sent through a chain of proxies.
func TestRequestOrigin(t *testing.T) {
	for _, c := range []struct {
		proto, host, want string
	}{
		{"", "", "http://fw.internal:8080"},
		{"https", "fw.example.com", "https://fw.example.com"},
		{"https, http", "fw.example.com, proxy.internal", "https://fw.example.com"},
		{" HTTPS ,http", " fw.example.com ,proxy.internal", "https://fw.example.com"},
		{",http", ",proxy.internal", "http://fw.internal:8080"},
	} {
		r := httptest.NewRequest(http.MethodGet, "http://fw.internal:8080/ui/signin", nil)
		if c.proto != "" {
			r.Header.Set("X-Forwarded-Proto", c.proto)
		}
		if c.host != "" {
			r.Header.Set("X-Forwarded-Host", c.host)
		}
		if got := requestOrigin(r); got != c.want {
			t.Errorf("proto %q, host %q: origin %q, want %q", c.proto, c.host, got, c.want)
		}
	}
}

func TestUsageIsDeprecated(t *testing.T) {
	a := manageAgent(t, newScript(endTurn("one")).model())
	srv := serve(t, a)
	if _, err := a.Run(fw.WithPrincipal(testCtx(t), people["alice-token"]), "review", fw.AsAgent("reviewer")); err != nil {
		t.Fatal(err)
	}
	resp, b := do(t, "GET", srv.URL+"/v1/usage", "", "Authorization", asAlice)
	var rep dashboard.Report
	decodeJSON(t, resp, b, http.StatusOK, &rep)
	if rep.Total.Runs != 1 {
		t.Fatalf("usage = %s", b)
	}
	if resp.Header.Get("Deprecation") != "true" {
		t.Errorf("Deprecation = %q", resp.Header.Get("Deprecation"))
	}
	if link := resp.Header.Get("Link"); link != "<https://github.com/bdubs00/featherweight-kernel/blob/main/docs/ARCHITECTURE.md#observability>; rel=\"deprecation\"; type=\"text/markdown\"" || !strings.Contains(link, `rel="deprecation"`) {
		t.Errorf("Link = %q", link)
	}
}
