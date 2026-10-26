package web

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/fleet"
)

// publishRun puts the egress of one finished run of the manager on the
// bus, as a worker publishes it.
func publishRun(t *testing.T, bus fw.Bus, run string, meta map[string]string, start time.Time) {
	t.Helper()
	node := "ax:team/manager-0"
	for i, x := range []struct {
		typ  string
		data any
	}{
		{fw.EvRunQueued, fw.RunQueuedData{Agent: "manager", Input: "Which hosts are down?", Metadata: meta, Origin: "native", Principal: "alice"}},
		{fw.EvRunStarted, map[string]any{}},
		{fw.EvToolRequested, fw.ToolRequestedData{Tool: "bash", CallID: "c1", Input: json.RawMessage(`{"command":"ss -tlnp"}`)}},
		{fw.EvToolResponded, fw.ToolRespondedData{Tool: "bash", CallID: "c1", Content: []fw.Block{{Type: fw.BlockText, Text: "LISTEN 0 4096 *:22"}}}},
		{fw.EvRunFinished, fw.RunFinishedData{Outcome: fw.OutcomeCompleted, Output: "No host is down."}},
	} {
		raw, err := json.Marshal(x.data)
		if err != nil {
			t.Fatal(err)
		}
		e := fw.Event{SpecVersion: fw.SpecVersion, ID: fmt.Sprintf("%s-%d", run, i+1), Type: x.typ, Time: start.Add(time.Duration(i) * time.Second), Subject: run, Seq: uint64(i + 1), Node: node, Data: raw}
		if err := bus.Publish(context.Background(), "fw.events.team.manager."+x.typ, e); err != nil {
			t.Fatal(err)
		}
	}
}

// A run whose worker was replaced comes back from the archive of the
// fleet: the list of GET /v1/fleet/runs names it with "archived": true and
// its metadata, and leaves out an internal run as before, and its
// transcript and its result come from the events of the archive. A
// message to it answers 409.
func TestArchivedRunRoutes(t *testing.T) {
	bus := fw.NewMemBus()
	a := newAgent(t, newScript().model(), fw.WithBus(bus))
	srv := serve(t, a)
	// A short expire time drops the finished runs from the view, as a
	// replace of their worker does.
	f, err := fleet.Open(context.Background(), a, fleet.Config{Name: "front", Egress: "fw.events", Expire: 50 * time.Millisecond, Interval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	fleet.Attach(a, f)
	t.Cleanup(func() { fleet.Attach(a, nil) })

	start := time.Now().Add(-time.Hour).UTC()
	publishRun(t, bus, "01CHAT", map[string]string{"fw.view": "quickchat", fw.MetaBatch: "bridge:01B"}, start)
	publishRun(t, bus, "01INTERNAL", map[string]string{fleet.MetaInternal: "duty"}, start.Add(time.Minute))
	var list struct {
		Runs  []json.RawMessage `json:"runs"`
		Total int               `json:"total"`
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		resp, b := do(t, "GET", srv.URL+"/v1/fleet/runs", "", "Authorization", "Bearer tok")
		decodeJSON(t, resp, b, 200, &list)
		if list.Total == 1 && strings.Contains(string(list.Runs[0]), `"archived":true`) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the list = %s", b)
		}
		time.Sleep(20 * time.Millisecond)
	}
	var got struct {
		fw.Run
		Archived bool `json:"archived"`
	}
	if err := json.Unmarshal(list.Runs[0], &got); err != nil || got.ID != "01CHAT" || got.Metadata["fw.view"] != "quickchat" || got.Metadata[fw.MetaBatch] != "bridge:01B" ||
		got.Agent != "manager" || got.Principal != "alice" || got.State != fw.StateCompleted || got.Member != "team/manager-0" || !got.Archived {
		t.Fatalf("the archived run = %s", list.Runs[0])
	}

	resp, b := do(t, "GET", srv.URL+"/v1/runs/01CHAT/transcript", "", "Authorization", "Bearer tok")
	var tr struct {
		Run   map[string]any   `json:"run"`
		Items []map[string]any `json:"items"`
	}
	decodeJSON(t, resp, b, 200, &tr)
	var kinds []string
	for _, it := range tr.Items {
		kinds = append(kinds, fmt.Sprint(it["kind"]))
	}
	if tr.Run["archived"] != true || strings.Join(kinds, " ") != "user tool result assistant end" || tr.Items[3]["text"] != "No host is down." {
		t.Fatalf("the transcript = %s", b)
	}
	resp, b = do(t, "GET", srv.URL+"/v1/runs/01CHAT/result", "", "Authorization", "Bearer tok")
	var res map[string]any
	decodeJSON(t, resp, b, 200, &res)
	if res["archived"] != true || res["status"] == "" {
		t.Fatalf("the result = %s", b)
	}
	resp, b = do(t, "POST", srv.URL+"/v1/runs/01CHAT/messages", `{"content":"and now?"}`, "Authorization", "Bearer tok")
	if resp.StatusCode != 409 || !strings.Contains(string(b), fleet.ReadOnlyRun) {
		t.Fatalf("a message = %d %s", resp.StatusCode, b)
	}
	if resp, b := do(t, "GET", srv.URL+"/v1/runs/01NONE/transcript", "", "Authorization", "Bearer tok"); resp.StatusCode != 404 {
		t.Fatalf("a run that nobody holds = %d %s", resp.StatusCode, b)
	}
}
