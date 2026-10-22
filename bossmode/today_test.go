package bossmode

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// eventAt is front.event with the time of the event.
func (fr *front) eventAt(t *testing.T, at time.Time, node, id string, seq uint64, typ string, data any) {
	t.Helper()
	e := fw.MustEvent(id, typ, data)
	e.Seq, e.Node, e.Time = seq, node, at
	if err := fr.bus.Publish(context.Background(), "fw.events.team.default."+typ, e); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		r, err := fr.f.Run(context.Background(), id)
		if err == nil && r.Seq >= seq {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the view did not apply %s %d of %s", typ, seq, id)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// today reads the numbers of the day of an agent as who.
func today(t *testing.T, fr *front, who, agent string) (int, Today) {
	t.Helper()
	resp, body := get(t, fr.url+"/v1/bossmode/agents/"+agent+"/today", who)
	var out Today
	if resp.StatusCode == http.StatusOK {
		if err := json.Unmarshal([]byte(body), &out); err != nil {
			t.Fatalf("today = %s: %v", body, err)
		}
	}
	return resp.StatusCode, out
}

// The numbers of the day of an agent count the runs of the agent that the
// caller can see and that started today in the zone of the front, on this
// process and on the other members: their cost as the batch sums it,
// partial while a run has no final usage or tokens without a price, and
// the decisions of people on them. A run of yesterday and a run of another
// agent do not count. An agent that the caller cannot view answers 404.
func TestTodayOfAnAgent(t *testing.T) {
	fr := newFront(t, func(c *fw.Config) {
		c.Time.Zone = "UTC"
		c.Agents["private"] = fw.AgentProfile{Access: &fw.Access{Owners: []string{"dave"}}}
	})
	now := time.Now().UTC()
	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	const alpha = "ax:team/alpha"
	md := func(principal string) fw.RunQueuedData {
		return fw.RunQueuedData{Agent: "manager", Input: "do it", Principal: principal}
	}
	// ada: a run that asked twice, was decided twice, and finished with
	// its totals, and a run that still works without totals.
	fr.eventAt(t, now, alpha, "01DONE", 1, fw.EvRunQueued, md("ada"))
	fr.eventAt(t, now, alpha, "01DONE", 2, fw.EvRunStarted, fw.RunStartedData{})
	fr.eventAt(t, now, alpha, "01DONE", 3, fw.EvApprovalRequested, fw.ApprovalRequestedData{ApprovalID: "ap1", Tool: "bash"})
	fr.eventAt(t, now, alpha, "01DONE", 4, fw.EvApprovalDecided, fw.ApprovalDecidedData{ApprovalID: "ap1", Decision: "allow", Decider: "ada"})
	fr.eventAt(t, now, alpha, "01DONE", 5, fw.EvApprovalRequested, fw.ApprovalRequestedData{ApprovalID: "ap2", Tool: "bash"})
	fr.eventAt(t, now, alpha, "01DONE", 6, fw.EvApprovalDecided, fw.ApprovalDecidedData{ApprovalID: "ap2", Decision: "deny", Decider: "ada"})
	fr.eventAt(t, now, alpha, "01DONE", 7, fw.EvRunFinished, fw.RunFinishedData{Outcome: fw.OutcomeCompleted, Output: "done",
		Totals: &fw.RunUsage{Usage: fw.Usage{InputTokens: 100, OutputTokens: 20}, Turns: 2, CostUSD: 0.25}})
	fr.eventAt(t, now, alpha, "01WORK", 1, fw.EvRunQueued, md("ada"))
	fr.eventAt(t, now, alpha, "01WORK", 2, fw.EvRunStarted, fw.RunStartedData{})
	// Runs that do not count: yesterday, and another agent. A run of bob
	// counts, because everyone operates the agent manager, and an
	// operator sees every run of the agent.
	fr.eventAt(t, midnight.Add(-time.Hour), alpha, "01OLD", 1, fw.EvRunQueued, md("ada"))
	fr.eventAt(t, now, alpha, "01OTHER", 1, fw.EvRunQueued, fw.RunQueuedData{Agent: "default", Input: "x", Principal: "ada"})
	fr.eventAt(t, now, alpha, "01BOB", 1, fw.EvRunQueued, md("bob"))
	fr.eventAt(t, now, alpha, "01BOB", 2, fw.EvRunFinished, fw.RunFinishedData{Outcome: fw.OutcomeCompleted,
		Totals: &fw.RunUsage{Usage: fw.Usage{InputTokens: 10}, Turns: 1, CostUSD: 0.5}})
	// A run of ada on this process.
	resp, body := send(t, http.MethodPost, fr.url+"/v1/runs", "ada", `{"agent":"manager","input":"hello"}`)
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusAccepted && resp.StatusCode != http.StatusOK {
		t.Fatalf("a local run = %d %s", resp.StatusCode, body)
	}
	var local fw.Run
	_ = json.Unmarshal([]byte(body), &local)
	deadline := time.Now().Add(5 * time.Second)
	for {
		r, err := fr.a.Get(context.Background(), local.ID)
		if err == nil && r.State.Terminal() {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the local run %s did not finish: %+v, %v", local.ID, r, err)
		}
		time.Sleep(5 * time.Millisecond)
	}

	code, got := today(t, fr, "ada", "manager")
	if code != http.StatusOK || got.Agent != "manager" || got.Window != WindowToday || got.Zone != "UTC" || !got.Since.Equal(midnight) {
		t.Fatalf("today of ada = %d %+v", code, got)
	}
	if got.Runs != 4 || got.CostUSD != 0.75 || !got.Partial || got.Decisions != 2 || got.DecisionsPartial {
		t.Fatalf("today of ada = %+v, want 4 runs, $0.75, partial, 2 decisions", got)
	}
	// The run that worked finishes with its totals, and the sum is final.
	fr.eventAt(t, now, alpha, "01WORK", 3, fw.EvRunFinished, fw.RunFinishedData{Outcome: fw.OutcomeCompleted,
		Totals: &fw.RunUsage{Usage: fw.Usage{InputTokens: 30}, Turns: 1, CostUSD: 0.125}})
	if _, got = today(t, fr, "ada", "manager"); got.Runs != 4 || got.CostUSD != 0.875 || got.Partial {
		t.Fatalf("today of ada after the end = %+v", got)
	}
	// Tokens without a price leave the cost partial.
	fr.eventAt(t, now, alpha, "01BOB2", 1, fw.EvRunQueued, md("bob"))
	fr.eventAt(t, now, alpha, "01BOB2", 2, fw.EvRunFinished, fw.RunFinishedData{Outcome: fw.OutcomeCompleted,
		Totals: &fw.RunUsage{Usage: fw.Usage{InputTokens: 10}, Turns: 1, UnpricedTokens: 10}})
	if _, got = today(t, fr, "bob", "manager"); got.Runs != 5 || got.CostUSD != 0.875 || !got.Partial || got.Decisions != 2 {
		t.Fatalf("today of bob = %+v", got)
	}

	// An agent that the caller cannot view, an unknown agent, and a name
	// that names no agent answer 404, and a caller without a token 401.
	for _, c := range []struct{ who, agent string }{{"ada", "private"}, {"ada", "nobody"}, {"ada", "bad%20name"}} {
		if code, _ := today(t, fr, c.who, c.agent); code != http.StatusNotFound {
			t.Errorf("today of %s for %s = %d, want 404", c.who, c.agent, code)
		}
	}
	if code, _ := today(t, fr, "dave", "private"); code != http.StatusOK {
		t.Errorf("today of dave for private = %d, want 200", code)
	}
	if code, _ := today(t, fr, "", "manager"); code != http.StatusUnauthorized {
		t.Errorf("today without a token = %d, want 401", code)
	}
}

// A run whose start the view did not see on the stream, because the front
// began to listen later, can hold decisions that the view never saw, so
// the count says that it is not whole.
func TestTodayDecisionsWithoutTheStart(t *testing.T) {
	fr := newFront(t, nil)
	e := fw.MustEvent("01LATE", fw.EvApprovalDecided, fw.ApprovalDecidedData{ApprovalID: "ap1", Decision: "allow", Decider: "ada"})
	e.Seq, e.Node = 4, "ax:team/alpha"
	if err := fr.bus.Publish(context.Background(), "fw.events.team.default."+e.Type, e); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		n, whole := fr.f.Decisions("01LATE")
		if n == 1 {
			if whole {
				t.Fatal("the count of a run without its start is whole")
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("Decisions = %d, %v", n, whole)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if n, whole := fr.f.Decisions("01NONE"); n != 0 || whole {
		t.Fatalf("Decisions of an unknown run = %d, %v", n, whole)
	}
}
