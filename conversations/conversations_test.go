package conversations

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/surface/native"
	"github.com/bdubs00/featherweight-workbench/surface/mcp"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// people takes the bearer token as the subject of a person.
type people struct{}

func (people) Authenticate(r *http.Request) (*fw.Principal, error) {
	tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if tok == "" || tok == r.Header.Get("Authorization") {
		return nil, fw.ErrUnauthorized
	}
	return &fw.Principal{Subject: tok, Kind: fw.KindUser}, nil
}

func (people) Challenge(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusUnauthorized)
}

// front is a process with the conversations in memory and the native
// surface on a test server. alice owns the agent default, and bob sees
// none of its runs.
type front struct {
	a   *fw.Agent
	url string
}

func newFront(t *testing.T) *front {
	t.Helper()
	cfg := fw.DefaultConfig()
	cfg.Home = t.TempDir()
	cfg.Access.Default = "closed"
	cfg.Agents = map[string]fw.AgentProfile{"default": {Access: &fw.Access{Owners: []string{"alice"}}}}
	a, err := fw.New(fw.WithConfig(cfg), fw.WithoutDefaults(), fw.WithAuthn(people{}),
		fw.WithModel(fw.ModelFunc{GenerateFn: func(context.Context, *fw.Request) (*fw.Response, error) {
			return &fw.Response{Content: []fw.Block{fw.Text("done")}, StopReason: fw.StopEndTurn}, nil
		}}),
		fw.WithLedger(fw.NewMemLedger()), fw.WithBus(fw.NewMemBus()), fw.WithSurfaces(native.New()),
		fw.WithServices(Start), fw.WithLogger(slog.New(slog.DiscardHandler)))
	if err != nil {
		t.Fatalf("fw.New: %v", err)
	}
	h, err := a.Handler()
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(func() {
		srv.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		a.Close(ctx)
	})
	if Of(a) == nil {
		t.Fatal("the conversations are off")
	}
	return &front{a: a, url: srv.URL}
}

func as(who string) context.Context {
	return fw.WithPrincipal(context.Background(), &fw.Principal{Subject: who, Kind: fw.KindUser})
}

func (f *front) patch(t *testing.T, who, run, body string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPatch, f.url+"/v1/conversations/"+run, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+who)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// A person renames and hides a conversation for their own lists alone:
// the route answers the run, the title of the person, and hidden; an
// empty title gives the run its own title back; a run that the person
// cannot view is 404; and the run itself does not change.
func TestPatch(t *testing.T) {
	f := newFront(t)
	r, err := f.a.Run(as("alice"), "Which hosts are down?")
	if err != nil {
		t.Fatal(err)
	}
	code, body := f.patch(t, "alice", r.ID, `{"title": "  Down   hosts "}`)
	var ans Answer
	if err := json.Unmarshal([]byte(body), &ans); code != http.StatusOK || err != nil || ans != (Answer{Run: r.ID, MyTitle: "Down hosts"}) {
		t.Fatalf("PATCH title = %d %s", code, body)
	}
	if code, body = f.patch(t, "alice", r.ID, `{"hidden": true}`); code != http.StatusOK || !strings.Contains(body, `"myTitle":"Down hosts"`) || !strings.Contains(body, `"hidden":true`) {
		t.Fatalf("PATCH hidden = %d %s", code, body)
	}
	prefs, err := Of(f.a).Prefs(as("alice"))
	if err != nil || prefs[r.ID].Title != "Down hosts" || !prefs[r.ID].Hidden {
		t.Fatalf("Prefs = %+v, %v", prefs, err)
	}
	if prefs, _ := Of(f.a).Prefs(as("carol")); len(prefs) != 0 {
		t.Fatalf("carol sees the prefs of alice: %+v", prefs)
	}
	run, _ := f.a.Get(as("alice"), r.ID)
	if run.Title == "Down hosts" {
		t.Fatal("the run took the title of the person")
	}
	// Marks of a list: a hidden run stays out unless the list asks for it.
	runs := []*fw.Run{run}
	if got, _ := Mark(as("alice"), f.a, runs, false); len(got) != 0 {
		t.Fatalf("a list without hidden runs = %v", got)
	}
	got, marks := Mark(as("alice"), f.a, runs, true)
	if len(got) != 1 || marks[r.ID] != (Listed{MyTitle: "Down hosts", Hidden: true}) {
		t.Fatalf("a list with hidden runs = %v %+v", got, marks)
	}
	if got, marks := Mark(as("carol"), f.a, runs, false); len(got) != 1 || len(marks) != 0 {
		t.Fatalf("the list of another person = %v %+v", got, marks)
	}
	// An empty title and hidden false leave nothing.
	if code, body = f.patch(t, "alice", r.ID, `{"title": "", "hidden": false}`); code != http.StatusOK || body != `{"run":"`+r.ID+`","hidden":false}`+"\n" {
		t.Fatalf("PATCH back = %d %q", code, body)
	}
	if prefs, _ := Of(f.a).Prefs(as("alice")); len(prefs) != 0 {
		t.Fatalf("an entry stays: %+v", prefs)
	}
	for _, c := range []struct {
		who, run, body string
		status         int
		want           string
	}{
		{"alice", r.ID, `{}`, http.StatusBadRequest, "send title, hidden, or both"},
		{"alice", r.ID, `{"title": "` + strings.Repeat("a", MaxTitleRunes+1) + `"}`, http.StatusBadRequest, "a title holds at most 100 characters"},
		{"alice", r.ID, `{"title": "a\u0007b"}`, http.StatusBadRequest, "without a control character"},
		{"alice", r.ID, `{"title": "x", "color": "red"}`, http.StatusBadRequest, "unknown field"},
		{"alice", "01NONE", `{"hidden": true}`, http.StatusNotFound, "no run that you can view has the id 01NONE"},
		{"bob", r.ID, `{"hidden": true}`, http.StatusNotFound, "no run that you can view"},
	} {
		if code, body := f.patch(t, c.who, c.run, c.body); code != c.status || !strings.Contains(body, c.want) {
			t.Errorf("%s PATCH %s = %d %s, want %d %q", c.who, c.body, code, body, c.status, c.want)
		}
	}
}

// conversation_patch does what the route does, for the person of the call.
func TestTool(t *testing.T) {
	f := newFront(t)
	r, err := f.a.Run(as("alice"), "Which hosts are down?")
	if err != nil {
		t.Fatal(err)
	}
	h, err := mcp.Handler(f.a)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	client := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "v1"}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	cs, err := client.Connect(ctx, &sdk.StreamableClientTransport{Endpoint: srv.URL + mcp.DefaultPath, HTTPClient: &http.Client{Transport: bearer{"alice"}}, MaxRetries: -1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	res, err := cs.CallTool(ctx, &sdk.CallToolParams{Name: ToolPatch, Arguments: map[string]any{"run": r.ID, "title": "Down hosts", "hidden": true}})
	if err != nil {
		t.Fatal(err)
	}
	text := res.Content[0].(*sdk.TextContent).Text
	if res.IsError || text != `The conversation `+r.ID+` is "Down hosts" in your lists, and it is hidden from them.` {
		t.Fatalf("conversation_patch = %s", text)
	}
	if prefs, _ := Of(f.a).Prefs(as("alice")); prefs[r.ID].Title != "Down hosts" {
		t.Fatalf("Prefs = %+v", prefs)
	}
	res, _ = cs.CallTool(ctx, &sdk.CallToolParams{Name: ToolPatch, Arguments: map[string]any{"run": "01NONE", "hidden": true}})
	if !res.IsError {
		t.Fatal("a run that alice cannot view")
	}
}

type bearer struct{ token string }

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return http.DefaultTransport.RoundTrip(r)
}
