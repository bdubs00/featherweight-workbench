package bossmode

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/bridges"
	"github.com/bdubs00/featherweight-workbench/faces"
	"github.com/bdubs00/featherweight-workbench/fleet"
	"github.com/bdubs00/featherweight-workbench/pages"
)

// storeFront is a front whose profile manager only ada operates, with the
// wiki, the faces, and the bridges of the bus mem started on it.
func storeFront(t *testing.T) *front {
	t.Helper()
	t.Setenv(fleet.EnvIntake, "fw.jobs")
	for _, env := range []string{bridges.EnvPlacement, bridges.EnvMaxMembers, bridges.EnvMaxMessages, bridges.EnvMaxFanoutPerMinute, bridges.EnvMaxPairMessages, EnvManager} {
		t.Setenv(env, "")
	}
	fr := newFront(t, func(c *fw.Config) {
		c.Agents["manager"] = fw.AgentProfile{Access: &fw.Access{Operators: []string{"ada"}}}
	})
	for _, start := range []fw.Service{pages.Start, faces.Start, bridges.Start} {
		cl, err := start(context.Background(), fr.a)
		if err != nil || cl == nil {
			t.Fatalf("start a store: %v, %v", cl, err)
		}
		t.Cleanup(func() { _ = cl.Close() })
	}
	// A route of a store mounts when the store runs, as in a process that
	// serves: the services start before the listener.
	h, err := fr.a.Handler()
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	fr.url = srv.URL
	return fr
}

// kinds returns the events of a stream up to the mark, without the runs,
// the members, and the Tasks of the fleet.
func kinds(t *testing.T, s *sse, name string) []frame {
	t.Helper()
	mark(t, name)
	var out []frame
	for {
		f := s.next(t)
		if f.kind == "test.mark" {
			var d map[string]string
			_ = json.Unmarshal([]byte(f.data), &d)
			if d["mark"] == name {
				return out
			}
			continue
		}
		switch f.kind {
		case KindRun, KindMember, KindTask:
			continue
		}
		out = append(out, f)
	}
}

// send makes a request with a JSON body as who.
func send(t *testing.T, method, url, who, body string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+who)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

// A write and a removal of a page reach the stream of a caller who reads
// the wiki as page, with deleted for the removal, and a change of a face
// reaches a caller who can view the agent as face. A caller who can do
// neither gets neither.
func TestFeedPagesAndFaces(t *testing.T) {
	fr := storeFront(t)
	ada := openStream(t, fr, "ada", "")
	carol := openStream(t, fr, "carol", "")
	ctx := context.Background()
	page, err := pages.Of(fr.a).Write(ctx, "notes/plan.md", "# Plan\n\nOne.\n", "ada", "first", pages.IfAny)
	if err != nil {
		t.Fatal(err)
	}
	if err := pages.Of(fr.a).Delete(ctx, "notes/plan.md", "ada", ""); err != nil {
		t.Fatal(err)
	}
	face, err := faces.Of(fr.a).Meta(ctx, "manager", "Boss", "Runs the fleet", "ada")
	if err != nil {
		t.Fatal(err)
	}
	got := kinds(t, ada, "pages-and-faces")
	if len(got) != 3 || got[0].kind != KindPage || got[1].kind != KindPage || got[2].kind != KindFace {
		t.Fatalf("ada got %+v", got)
	}
	var written, removed struct {
		Path    string `json:"path"`
		Title   string `json:"title"`
		Hash    string `json:"hash"`
		By      string `json:"by"`
		At      string `json:"at"`
		Size    int    `json:"size"`
		Deleted bool   `json:"deleted"`
	}
	if err := json.Unmarshal([]byte(got[0].data), &written); err != nil || written.Path != "notes/plan.md" || written.Hash != page.Hash || written.Title != "Plan" || written.By != "ada" || written.At == "" || written.Size != page.Size || written.Deleted {
		t.Errorf("page = %s", got[0].data)
	}
	if err := json.Unmarshal([]byte(got[1].data), &removed); err != nil || removed.Path != "notes/plan.md" || !removed.Deleted {
		t.Errorf("removed page = %s", got[1].data)
	}
	var entry faces.Face
	if err := json.Unmarshal([]byte(got[2].data), &entry); err != nil || entry.Agent != "manager" || entry.DisplayName != "Boss" || entry.Hash != face.Hash || strings.Contains(got[2].data, "svg") {
		t.Errorf("face = %s", got[2].data)
	}
	if got := kinds(t, carol, "pages-and-faces-carol"); len(got) != 0 {
		t.Errorf("carol, who reads no wiki and views no agent, got %+v", got)
	}
}

// The entries of a channel and the seats of a bridge reach the stream of
// a caller who can see the bridge as bridge.message and bridge.member, and
// no other caller.
func TestFeedBridges(t *testing.T) {
	fr := storeFront(t)
	ada := openStream(t, fr, "ada", "")
	carol := openStream(t, fr, "carol", "")
	resp, body := send(t, http.MethodPost, fr.url+"/v1/bridges", "ada", `{"title":"Feed","goal":"Feed the stream.","members":[{"handle":"boss-1","agent":"manager","task":"Watch the fleet"}]}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("open = %d %s", resp.StatusCode, body)
	}
	var it struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(body), &it); err != nil || it.ID == "" {
		t.Fatalf("open = %s", body)
	}
	if resp, body := send(t, http.MethodPost, fr.url+"/v1/bridges/"+it.ID+"/messages", "ada", `{"text":"Hello, team."}`); resp.StatusCode != http.StatusCreated {
		t.Fatalf("post = %d %s", resp.StatusCode, body)
	}
	got := kinds(t, ada, "bridges")
	var opened, said, seated bool
	for _, f := range got {
		switch f.kind {
		case KindBridgeMessage:
			var d struct {
				Bridge  string          `json:"bridge"`
				Message bridges.Message `json:"message"`
			}
			if err := json.Unmarshal([]byte(f.data), &d); err != nil || d.Bridge != it.ID {
				t.Fatalf("bridge.message = %s", f.data)
			}
			opened = opened || d.Message.Event == bridges.EventOpened
			said = said || (d.Message.From.Kind == bridges.KindPerson && d.Message.Text == "Hello, team.")
		case KindBridgeMember:
			var d struct {
				Bridge string         `json:"bridge"`
				Member bridges.Member `json:"member"`
			}
			if err := json.Unmarshal([]byte(f.data), &d); err != nil || d.Bridge != it.ID {
				t.Fatalf("bridge.member = %s", f.data)
			}
			seated = seated || d.Member.Handle == "boss-1"
		}
	}
	if !opened || !said || !seated {
		t.Fatalf("ada got the open %v, the message %v, the seat %v: %+v", opened, said, seated, got)
	}
	if got := kinds(t, carol, "bridges-carol"); len(got) != 0 {
		t.Errorf("carol, who cannot see the bridge, got %+v", got)
	}

	// A card whose decision is larger than an event goes without its input
	// and its schema, marked partial.
	big := bridges.Message{Seq: 99, From: bridges.From{Kind: bridges.KindSystem}, Text: "boss-1 wants to change a file.", Event: bridges.EventNeedsYou,
		Decision: &bridges.Decision{Run: "r-big", State: bridges.DecisionPending}}
	big.Decision.ID, big.Decision.Tool = "ap-1", "files"
	big.Decision.Input = json.RawMessage(`{"content":"` + strings.Repeat("x", MaxEventBytes) + `"}`)
	feedMessage(it.ID, big)
	got = kinds(t, ada, "partial")
	if len(got) != 1 || got[0].kind != KindBridgeMessage {
		t.Fatalf("the large card = %+v", got)
	}
	var d struct {
		Partial bool            `json:"partial"`
		Message bridges.Message `json:"message"`
	}
	if err := json.Unmarshal([]byte(got[0].data), &d); err != nil || !d.Partial || d.Message.Decision == nil || d.Message.Decision.ID != "ap-1" || len(d.Message.Decision.Input) != 0 {
		t.Fatalf("the large card = %s", got[0].data)
	}
}

// The activity of a run has one phrase: the run event of the stream, the
// seat of the bridge on the stream, and fleet.ActivityPhrase give the same
// words, because the bridges follow the changes of the fleet and word a
// run with fleet.ActivityPhrase.
func TestStreamAndRosterAgree(t *testing.T) {
	fr := storeFront(t)
	ada := openStream(t, fr, "ada", "")
	resp, body := send(t, http.MethodPost, fr.url+"/v1/bridges", "ada", `{"title":"Agree","goal":"Word the run.","members":[{"handle":"boss-1","agent":"manager","task":"Run the tests"}]}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("open = %d %s", resp.StatusCode, body)
	}
	var it struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal([]byte(body), &it)
	md := map[string]string{fw.MetaBatch: bridges.BatchOf(it.ID), bridges.MetaMember: "boss-1"}
	fr.event(t, "ax:team/alpha", "01SEAT", 1, fw.EvRunQueued, fw.RunQueuedData{Agent: "manager", Input: "Run the tests", Principal: "ada", Metadata: md})
	fr.event(t, "ax:team/alpha", "01SEAT", 2, fw.EvRunStarted, fw.RunStartedData{})
	fr.event(t, "ax:team/alpha", "01SEAT", 3, fw.EvTurnStarted, fw.TurnData{Turn: 1})
	fr.event(t, "ax:team/alpha", "01SEAT", 4, fw.EvToolRequested, fw.ToolRequestedData{Tool: "bash", CallID: "c1", Input: json.RawMessage(`{"command":"go test ./..."}`)})
	const want = `Ran "go test ./..."`
	run, member := "", ""
	deadline := time.Now().Add(10 * time.Second)
	for (run == "" || member == "") && time.Now().Before(deadline) {
		f := ada.next(t)
		switch f.kind {
		case KindRun:
			var r struct {
				ID  string `json:"id"`
				Seq int    `json:"seq"`
				Act *struct {
					Phrase string `json:"phrase"`
				} `json:"activity"`
			}
			if json.Unmarshal([]byte(f.data), &r) == nil && r.ID == "01SEAT" && r.Seq >= 4 && r.Act != nil {
				run = r.Act.Phrase
			}
		case KindBridgeMember:
			var d struct {
				Member bridges.Member `json:"member"`
			}
			if json.Unmarshal([]byte(f.data), &d) == nil && d.Member.Activity != nil && d.Member.Activity.Tool == "bash" {
				member = d.Member.Activity.Phrase
			}
		}
	}
	r, err := fr.f.Run(context.Background(), "01SEAT")
	if err != nil {
		t.Fatal(err)
	}
	if run != want || member != want || fleet.ActivityPhrase(r) != want {
		t.Fatalf("the run event says %q, the seat %q, and fleet.ActivityPhrase %q; want %q", run, member, fleet.ActivityPhrase(r), want)
	}
}
