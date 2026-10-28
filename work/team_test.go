package work

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/surface/native"
	"github.com/bdubs00/featherweight-workbench/bridges"
	"github.com/bdubs00/featherweight-workbench/duties"
	"github.com/bdubs00/featherweight-workbench/fleet"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// newTeamFront is newFront with the bridges in memory and the services of
// the package: alice owns the agents writer and reviewer, and bob views
// them. A queue bridge reaches the queues fw.jobs.<agent>.
func newTeamFront(t *testing.T) (*fw.Agent, *httptest.Server) {
	t.Helper()
	t.Setenv("FW_ATESPACE", "")
	t.Setenv("FW_API_TOKEN", "fleet-secret")
	t.Setenv(fleet.EnvIntake, "fw.jobs")
	t.Setenv(bridges.EnvPlacement, "")
	t.Setenv(bridges.EnvManager, "")
	cfg := fw.DefaultConfig()
	cfg.Home = t.TempDir()
	cfg.Access.Default = "closed"
	cfg.Agents = map[string]fw.AgentProfile{
		"writer":   {Description: "Writes release notes.", Access: &fw.Access{Owners: []string{"alice"}, Viewers: []string{"bob"}}},
		"reviewer": {Description: "Reviews drafts.", Access: &fw.Access{Owners: []string{"alice"}, Viewers: []string{"bob"}}},
	}
	a, err := fw.New(fw.WithConfig(cfg), fw.WithoutDefaults(), fw.WithAuthn(people{}), fw.WithModel(model()),
		fw.WithLedger(fw.NewMemLedger()), fw.WithBus(fw.NewMemBus()), fw.WithSurfaces(native.New()),
		fw.WithServices(duties.Start, bridges.Start), fw.WithLogger(quiet()))
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
	return a, srv
}

// openTeam opens a queue bridge of alice with a writer and a reviewer.
func openTeam(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	code, b := post(t, srv, "alice", "/v1/bridges", `{"title":"Launch email","goal":"Write the launch email.","delegation":{"report":false},"members":[
		{"handle":"writer","agent":"writer","task":"Write the email"},{"handle":"reviewer","agent":"reviewer","task":"Review it"}]}`)
	var it bridges.Item
	if err := json.Unmarshal(b, &it); code != http.StatusCreated || err != nil {
		t.Fatalf("open = %d %s", code, b)
	}
	return it.ID
}

// The check of a team. A request names a run or a bridge, not both and
// not neither. Needs work on a bridge posts the note to every seat and
// keeps the bridge open; Looks right closes it, and its card is done and
// accepted. The tool does the same as the route, and a bridge that is not
// there is 404.
func TestReviewBridge(t *testing.T) {
	a, srv := newTeamFront(t)
	id := openTeam(t, srv)
	for _, body := range []string{`{"verdict":"up"}`, `{"run":"r1","bridge":"` + id + `","verdict":"up"}`, `{"bridge":"` + id + `","verdict":"down"}`, `{"bridge":"` + id + `","verdict":"maybe"}`} {
		if code, b := post(t, srv, "alice", "/v1/work/review", body); code != http.StatusBadRequest {
			t.Fatalf("POST /v1/work/review %s = %d %s", body, code, b)
		}
	}
	code, b := post(t, srv, "alice", "/v1/work/review", `{"bridge":"`+id+`","verdict":"down","note":"Say the price in the first line."}`)
	var ans ReviewAnswer
	if err := json.Unmarshal(b, &ans); code != http.StatusOK || err != nil || ans.Bridge != id || ans.Reaction != fw.ReactionDown || !ans.FollowUp || ans.Closed {
		t.Fatalf("Needs work on a bridge = %d %s", code, b)
	}
	page := channelOf(t, srv, id)
	found := false
	for _, m := range page {
		found = found || (m.Text == bridges.SendBackPrefix+"Say the price in the first line." && len(m.To) == 0)
	}
	if !found {
		t.Fatalf("the channel holds no note to every seat: %+v", page)
	}
	if code, b := post(t, srv, "bob", "/v1/work/review", `{"bridge":"`+id+`","verdict":"up"}`); code != http.StatusForbidden {
		t.Fatalf("a check by a viewer = %d %s", code, b)
	}
	cs := connect(t, a, "alice")
	res, err := cs.CallTool(context.Background(), &sdk.CallToolParams{Name: ToolReview, Arguments: map[string]any{"bridge": id, "verdict": "up"}})
	if err != nil || res.IsError || !strings.Contains(res.Content[0].(*sdk.TextContent).Text, "closed with the verdict accepted") {
		t.Fatalf("Looks right on a bridge = %s, %v", res.Content[0].(*sdk.TextContent).Text, err)
	}
	c := cardOf(t, mustFor(t, a, "alice"), "bridge:"+id)
	if c.Stage != StageDone || c.Verdict != VerdictAccepted || c.Reason != "Accepted." {
		t.Fatalf("the card of the checked bridge = %+v", c)
	}
	if code, b := post(t, srv, "alice", "/v1/work/review", `{"bridge":"nope","verdict":"up"}`); code != http.StatusNotFound {
		t.Fatalf("a bridge that is not there = %d %s", code, b)
	}
}

// channelOf reads the channel of a bridge as alice.
func channelOf(t *testing.T, srv *httptest.Server, id string) []bridges.Message {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/bridges/"+id+"/messages?limit=100", nil)
	req.Header.Set("Authorization", "Bearer alice")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var page bridges.Page
	if err := json.NewDecoder(resp.Body).Decode(&page); err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("the channel = %d, %v", resp.StatusCode, err)
	}
	return page.Messages
}
