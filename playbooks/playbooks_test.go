package playbooks

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
	"github.com/bdubs00/featherweight-workbench/bridges"
	"github.com/bdubs00/featherweight-workbench/fleet"
	"github.com/bdubs00/featherweight-workbench/surface/mcp"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// people takes the bearer token as the subject of a person.
type people struct{}

func (people) Authenticate(r *http.Request) (*fw.Principal, error) {
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || tok == "" {
		return nil, fw.ErrUnauthorized
	}
	return &fw.Principal{Subject: tok, Kind: fw.KindUser}, nil
}

func (people) Challenge(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusUnauthorized)
}

// newFront returns a process with the bridges and the playbooks in
// memory, served on the native surface. alice owns the agent migrator and
// bob operates it, carol has no role, and dave owns private.
func newFront(t *testing.T) (*fw.Agent, string) {
	t.Helper()
	t.Setenv("FW_ATESPACE", "")
	t.Setenv(fleet.EnvIntake, "fw.jobs")
	for _, env := range []string{bridges.EnvPlacement, bridges.EnvMaxMembers, bridges.EnvMaxMessages, bridges.EnvMaxFanoutPerMinute, bridges.EnvMaxPairMessages, bridges.EnvMaxTurns, bridges.EnvManager} {
		t.Setenv(env, "")
	}
	cfg := fw.DefaultConfig()
	cfg.Home = t.TempDir()
	cfg.Access.Default = "closed"
	cfg.Agents = map[string]fw.AgentProfile{
		"migrator": {Access: &fw.Access{Owners: []string{"alice"}, Operators: []string{"bob"}}},
		"private":  {Access: &fw.Access{Owners: []string{"dave"}}},
	}
	a, err := fw.New(fw.WithConfig(cfg), fw.WithoutDefaults(), fw.WithAuthn(people{}),
		fw.WithModel(fw.ModelFunc{GenerateFn: func(context.Context, *fw.Request) (*fw.Response, error) {
			return &fw.Response{Content: []fw.Block{fw.Text("done")}, StopReason: fw.StopEndTurn}, nil
		}}),
		fw.WithLedger(fw.NewMemLedger()), fw.WithBus(fw.NewMemBus()), fw.WithSurfaces(native.New()),
		fw.WithServices(bridges.Start, Start), fw.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	if err != nil {
		t.Fatal(err)
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
	if Of(a) == nil || bridges.Of(a) == nil {
		t.Fatal("the playbooks or the bridges are off")
	}
	return a, srv.URL
}

func send(t *testing.T, method, url, who, body string) (*http.Response, map[string]any) {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, _ := http.NewRequest(method, url, rd)
	req.Header.Set("Authorization", "Bearer "+who)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	var out map[string]any
	if len(b) > 0 {
		if err := json.Unmarshal(b, &out); err != nil {
			t.Fatalf("decode %s: %v", b, err)
		}
	}
	return resp, out
}

const weekly = `{"goal": "Patch the lab hosts and report.", "placement": "queue",
	"seats": [{"handle": "patcher", "agent": "migrator", "task": "Patch the hosts."}],
	"caps": {"maxMessages": 200, "maxTurns": 400}, "delegation": {"approve": true, "report": true}}`

// A playbook saves the plan of a bridge under a name: a write answers it
// with its revision and owner, a list leaves out its caps and its
// delegation, a stale revision gets 409, and a delete answers 204. Its
// owner and the operators of its agents read it, and nobody else does.
func TestPlaybookRoutes(t *testing.T) {
	_, url := newFront(t)
	one := url + "/v1/playbooks/weekly-patching"
	resp, v := send(t, http.MethodPut, one, "alice", weekly)
	if resp.StatusCode != http.StatusCreated || resp.Header.Get("Location") != "/v1/playbooks/weekly-patching" ||
		v["name"] != "weekly-patching" || v["owner"] != "alice" || v["revision"] == float64(0) || v["updatedAt"] == "" || v["caps"] == nil {
		t.Fatalf("the create = %d %v", resp.StatusCode, v)
	}
	rev := v["revision"].(float64)
	resp, l := send(t, http.MethodGet, url+"/v1/playbooks", "alice", "")
	list, _ := l["playbooks"].([]any)
	if resp.StatusCode != http.StatusOK || len(list) != 1 {
		t.Fatalf("the list = %d %v", resp.StatusCode, l)
	}
	item := list[0].(map[string]any)
	for _, k := range []string{"name", "goal", "placement", "seats", "revision", "owner", "updatedAt"} {
		if _, ok := item[k]; !ok {
			t.Errorf("a listed playbook lacks %s: %v", k, item)
		}
	}
	if _, ok := item["caps"]; ok {
		t.Errorf("a listed playbook has its caps: %v", item)
	}
	if resp, v := send(t, http.MethodGet, one, "bob", ""); resp.StatusCode != http.StatusOK || v["delegation"] == nil || v["caps"] == nil {
		t.Fatalf("bob reads = %d %v", resp.StatusCode, v)
	}
	if resp, _ := send(t, http.MethodGet, one, "carol", ""); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("carol reads = %d", resp.StatusCode)
	}
	if _, l := send(t, http.MethodGet, url+"/v1/playbooks", "carol", ""); len(l["playbooks"].([]any)) != 0 {
		t.Fatalf("carol lists = %v", l)
	}
	stale := strings.Replace(weekly, `"goal"`, `"revision": 1, "goal"`, 1)
	if rev == 1 {
		stale = strings.Replace(weekly, `"goal"`, `"revision": 99, "goal"`, 1)
	}
	if resp, _ := send(t, http.MethodPut, one, "alice", stale); resp.StatusCode != http.StatusConflict {
		t.Fatalf("a stale write = %d", resp.StatusCode)
	}
	fresh := strings.Replace(weekly, `"goal": "Patch the lab hosts and report."`, `"revision": `+jsonNumber(rev)+`, "goal": "Patch and report."`, 1)
	resp, v = send(t, http.MethodPut, one, "bob", fresh)
	if resp.StatusCode != http.StatusOK || v["goal"] != "Patch and report." || v["owner"] != "alice" || v["revision"].(float64) <= rev {
		t.Fatalf("bob changes = %d %v", resp.StatusCode, v)
	}
	if resp, _ := send(t, http.MethodPut, url+"/v1/playbooks/mine", "alice", strings.Replace(weekly, "migrator", "private", 1)); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("alice writes a seat of private = %d", resp.StatusCode)
	}
	if resp, _ := send(t, http.MethodDelete, one, "carol", ""); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("carol deletes = %d", resp.StatusCode)
	}
	if resp, _ := send(t, http.MethodDelete, one, "alice", ""); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("the delete = %d", resp.StatusCode)
	}
	if resp, _ := send(t, http.MethodGet, one, "alice", ""); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("a read after the delete = %d", resp.StatusCode)
	}
}

func jsonNumber(f float64) string {
	b, _ := json.Marshal(f)
	return string(b)
}

// A playbook that does not fit gets 400 with each problem, each with the
// dotted path of its field, from the checks of the plan of a bridge.
func TestPlaybookProblems(t *testing.T) {
	_, url := newFront(t)
	bad := `{"goal": "x", "placement": "nowhere", "seats": [{"handle": "Bad Handle", "agent": "migrator", "task": ""}], "caps": {"maxTurns": 0}}`
	resp, v := send(t, http.MethodPut, url+"/v1/playbooks/Bad", "alice", bad)
	problems, _ := v["problems"].([]any)
	if resp.StatusCode != http.StatusBadRequest || len(problems) == 0 {
		t.Fatalf("a bad playbook = %d %v", resp.StatusCode, v)
	}
	got := map[string]bool{}
	for _, p := range problems {
		path, _, _ := strings.Cut(p.(string), ": ")
		got[path] = true
	}
	for _, want := range []string{"name", "placement", "seats[0].handle", "seats[0].task", "caps"} {
		if !got[want] {
			t.Errorf("no problem of %s: %v", want, problems)
		}
	}
	if resp, v := send(t, http.MethodPut, url+"/v1/playbooks/empty", "alice", `{"goal": "x", "seats": []}`); resp.StatusCode != http.StatusBadRequest ||
		!strings.Contains(v["detail"].(string), "seats: A bridge needs at least one member.") {
		t.Fatalf("a playbook without seats = %d %v", resp.StatusCode, v)
	}
}

// The tools call the functions of the routes with the same access.
func TestPlaybookTools(t *testing.T) {
	a, _ := newFront(t)
	cs := connectMCP(t, a, "alice")
	var pb map[string]any
	if err := json.Unmarshal([]byte(weekly), &pb); err != nil {
		t.Fatal(err)
	}
	res := call(t, cs, ToolPut, map[string]any{"name": "weekly", "playbook": pb})
	if res.IsError || !strings.Contains(textOf(res), "Created the playbook weekly: 1 seat, owner alice") {
		t.Fatalf("playbook_put = %s", textOf(res))
	}
	if res := call(t, cs, ToolPut, map[string]any{"name": "weekly", "playbook": pb, "revision": 999}); !res.IsError || !strings.Contains(textOf(res), "Conflict:") {
		t.Fatalf("a stale playbook_put = %s", textOf(res))
	}
	if res := call(t, cs, ToolList, map[string]any{}); res.IsError || !strings.Contains(textOf(res), "1 playbook:\n- weekly: 1 seat") {
		t.Fatalf("playbooks_list = %s", textOf(res))
	}
	if res := call(t, cs, ToolGet, map[string]any{"name": "weekly"}); res.IsError || !strings.Contains(textOf(res), "- patcher (migrator): Patch the hosts.") {
		t.Fatalf("playbook_get = %s", textOf(res))
	}
	carol := connectMCP(t, a, "carol")
	if res := call(t, carol, ToolGet, map[string]any{"name": "weekly"}); !res.IsError || !strings.Contains(textOf(res), "Not found:") {
		t.Fatalf("carol playbook_get = %s", textOf(res))
	}
	if res := call(t, cs, ToolDelete, map[string]any{"name": "weekly"}); res.IsError {
		t.Fatalf("playbook_delete = %s", textOf(res))
	}
	if res := call(t, cs, ToolGet, map[string]any{"name": "weekly"}); !res.IsError {
		t.Fatalf("playbook_get after the delete = %s", textOf(res))
	}
}

// Lookup reads a playbook for the front with no check of the caller, and
// Spec gives the plan of a bridge with a title.
func TestLookupAndSpec(t *testing.T) {
	a, url := newFront(t)
	if resp, _ := send(t, http.MethodPut, url+"/v1/playbooks/weekly", "alice", weekly); resp.StatusCode != http.StatusCreated {
		t.Fatalf("the create = %d", resp.StatusCode)
	}
	p, err := Of(a).Lookup(context.Background(), "weekly")
	if err != nil {
		t.Fatal(err)
	}
	spec := p.Spec("weekly: 2026-10-23")
	if spec.Title != "weekly: 2026-10-23" || len(spec.Members) != 1 || spec.Members[0].Handle != "patcher" || spec.Goal != "Patch the lab hosts and report." {
		t.Fatalf("the spec = %+v", spec)
	}
	if _, err := Of(a).Lookup(context.Background(), "none"); err == nil {
		t.Fatal("a lookup of no playbook found one")
	}
}

func connectMCP(t *testing.T, a *fw.Agent, token string) *sdk.ClientSession {
	t.Helper()
	h, err := mcp.Handler(a)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	hc := &http.Client{Transport: bearer{token: token}}
	client := sdk.NewClient(&sdk.Implementation{Name: "test-client", Version: "v1"}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	cs, err := client.Connect(ctx, &sdk.StreamableClientTransport{Endpoint: srv.URL + mcp.DefaultPath, HTTPClient: hc, MaxRetries: -1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

type bearer struct{ token string }

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return http.DefaultTransport.RoundTrip(r)
}

func call(t *testing.T, cs *sdk.ClientSession, name string, args any) *sdk.CallToolResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := cs.CallTool(ctx, &sdk.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("call %s: %v", name, err)
	}
	return res
}

func textOf(res *sdk.CallToolResult) string {
	var parts []string
	for _, c := range res.Content {
		if tc, ok := c.(*sdk.TextContent); ok {
			parts = append(parts, tc.Text)
		}
	}
	return strings.Join(parts, "\n")
}
