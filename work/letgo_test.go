package work

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// A person lets a stopped card go. The card names no card, a card that
// nobody can view, and a card that did not stop are refused. A viewer
// whose move the card is not cannot let it go. Alice can, and the card
// moves to done with the verdict let-go and her as the person. A second
// call does nothing, and the board shows the card in Done. An owner of the
// agent lets go a card of another person, and the tool does the same as
// the route.
func TestLetGoRoute(t *testing.T) {
	CacheFor = 0
	t.Cleanup(func() { CacheFor = 10 * time.Second })
	a, srv := newFront(t)
	stop := func(ctx context.Context) string {
		t.Helper()
		r, err := a.Start(ctx, "", fw.WithRequest(fw.RunRequest{Agent: "writer", Input: "deploy the notes", Budget: &fw.Budget{MaxTurns: 1}}))
		if err != nil {
			t.Fatal(err)
		}
		waitFor(t, a, r.ID, fw.StateFailed)
		return r.ID
	}
	failed := stop(as("alice"))
	done, err := a.Start(as("alice"), "", fw.WithRequest(fw.RunRequest{Agent: "writer", Input: "What changed?"}))
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, a, done.ID, fw.StateCompleted)
	card := "run:" + failed
	if c := cardOf(t, mustFor(t, a, "alice"), card); c.Stage != StageStopped || c.Next != NextYou {
		t.Fatalf("the stopped card = %+v", c)
	}
	for _, x := range []struct {
		who, body string
		code      int
	}{
		{"alice", `{"card":" "}`, http.StatusBadRequest},
		{"alice", `{"card":"run:nope"}`, http.StatusNotFound},
		{"carol", `{"card":"` + card + `"}`, http.StatusNotFound},
		{"bob", `{"card":"` + card + `"}`, http.StatusForbidden},
		{"alice", `{"card":"run:` + done.ID + `"}`, http.StatusConflict},
	} {
		if code, b := post(t, srv, x.who, "/v1/work/letgo", x.body); code != x.code {
			t.Fatalf("POST /v1/work/letgo %s as %s = %d %s, want %d", x.body, x.who, code, b, x.code)
		}
	}
	code, b := post(t, srv, "alice", "/v1/work/letgo", `{"card":"`+card+`"}`)
	var c Card
	if err := json.Unmarshal(b, &c); code != http.StatusOK || err != nil || c.Stage != StageDone || c.Verdict != VerdictLetGo || c.Reason != "Let go by you." || c.Ended == nil {
		t.Fatalf("the let-go = %d %s", code, b)
	}
	again, b2 := post(t, srv, "alice", "/v1/work/letgo", `{"card":"`+card+`"}`)
	var c2 Card
	if err := json.Unmarshal(b2, &c2); again != http.StatusOK || err != nil || !c2.Ended.Equal(*c.Ended) || c2.Verdict != VerdictLetGo {
		t.Fatalf("a second let-go = %d %s", again, b2)
	}
	if got := cardOf(t, mustFor(t, a, "alice"), card); got.Stage != StageDone || got.Verdict != VerdictLetGo {
		t.Fatalf("the card on the board = %+v", got)
	}
	all, err := Of(a).All(context.Background())
	if err != nil || len(all) != 1 || all[card].By != "alice" {
		t.Fatalf("the bucket = %+v, %v", all, err)
	}
	// A run of the process itself is no card of alice, but alice owns its
	// agent.
	other := stop(context.Background())
	if c := cardOf(t, mustForAll(t, a, "alice"), "run:"+other); c.Next != NextPerson {
		t.Fatalf("the card of another = %+v", c)
	}
	cs := connect(t, a, "alice")
	res, err := cs.CallTool(context.Background(), &sdk.CallToolParams{Name: ToolLetGo, Arguments: map[string]any{"card": "run:" + other}})
	if err != nil || res.IsError || !strings.HasPrefix(res.Content[0].(*sdk.TextContent).Text, "Let go: ") {
		t.Fatalf("the tool = %+v, %v", res, err)
	}
}

func mustForAll(t *testing.T, a *fw.Agent, who string) Answer {
	t.Helper()
	ans, err := For(as(who), a, time.Now(), Query{All: true})
	if err != nil {
		t.Fatal(err)
	}
	return ans
}
