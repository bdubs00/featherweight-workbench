package bridges

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/fleet"
)

func ev(typ string, data any) fw.Event { return fw.MustEvent("", typ, data) }

// hookLog records what the hooks get.
type hookLog struct {
	mu       sync.Mutex
	messages map[uint64][]Message
	members  map[string][]Member
}

func hooked(t *testing.T) *hookLog {
	h := &hookLog{messages: map[uint64][]Message{}, members: map[string][]Member{}}
	OnMessage(func(_ string, m Message) {
		h.mu.Lock()
		h.messages[m.Seq] = append(h.messages[m.Seq], m)
		h.mu.Unlock()
	})
	OnMember(func(_ string, m Member) {
		h.mu.Lock()
		h.members[m.Handle] = append(h.members[m.Handle], m)
		h.mu.Unlock()
	})
	t.Cleanup(func() {
		hooks.mu.Lock()
		hooks.message, hooks.member = nil, nil
		hooks.mu.Unlock()
	})
	return h
}

func (h *hookLog) message(seq uint64) []Message {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]Message(nil), h.messages[seq]...)
}

func (h *hookLog) member(handle string) []Member {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]Member(nil), h.members[handle]...)
}

// The watch posts one line per fact of each run of a bridge: the start,
// the card of an approval, its decision with the card written again in
// place, the card of a question with its answer, the answer of a finished
// run as a message of the member, a failure, a cancel, and the sleep and
// the wake of a Task. A second look posts nothing again.
func TestSystemLines(t *testing.T) {
	h := hooked(t)
	a := newAgent(t, nil)
	s := newStore(t, a, nil)
	v, fm := withFleet(t, a)
	v.state("default/crew-0", fleet.MemberLive, "migrator", "reviewer")
	url := serve(t, a)
	it := open(t, url)
	ctx := context.Background()
	poll := func() []Message {
		t.Helper()
		before := len(channel(t, s, it.ID))
		s.poll(ctx)
		return channel(t, s, it.ID)[before:]
	}

	mig := workerRun("r-mig", it.ID, "migrator-1", "migrator", fw.StateRunning)
	mig.Activity = &fw.RunActivity{Turn: 2, Since: time.Now().UTC()}
	v.put(mig)
	got := poll()
	if len(got) != 1 || got[0].Event != EventStarted || got[0].Text != "migrator-1 started: Migrate the logrotate module" || got[0].Mirrors == nil || got[0].Mirrors.Type != fw.EvRunQueued {
		t.Fatalf("start = %+v", got)
	}
	if again := poll(); len(again) != 0 {
		t.Fatalf("a second look posts %+v", again)
	}
	if ms := h.member("migrator-1"); len(ms) == 0 || ms[len(ms)-1].Presence != PresenceWorking || ms[len(ms)-1].Activity == nil || ms[len(ms)-1].Activity.Phrase != "Thinking in turn 2" {
		t.Fatalf("member hooks = %+v", ms)
	}

	// An approval: a card with the decision of the result.
	mig.State, mig.Seq, mig.Activity = fw.StateInputRequired, 5, nil
	mig.Pending = &fw.Approval{ID: "ap1", Tool: "bash", Input: json.RawMessage(`{"command":"git push origin migrate/cron"}`), Reason: "rule: ask bash"}
	v.put(mig)
	got = poll()
	if len(got) != 1 || got[0].Event != EventNeedsYou || got[0].Text != "migrator-1 wants to run a command." || got[0].Decision == nil {
		t.Fatalf("card = %+v", got)
	}
	card := got[0]
	d := card.Decision
	if d.Run != "r-mig" || d.ID != "ap1" || d.State != DecisionPending || d.Label != "Allow" || d.Effect != `Runs "git push origin migrate/cron".` || !d.Always || card.Mirrors.Seq != 5 {
		t.Fatalf("decision = %+v", d)
	}
	if ms := h.member("migrator-1"); ms[len(ms)-1].Presence != PresenceNeedsYou {
		t.Fatalf("presence = %s", ms[len(ms)-1].Presence)
	}
	// The decision lands on another surface; the owner holds it.
	fm.setEvents("r-mig",
		ev(fw.EvRunQueued, fw.RunQueuedData{Agent: "migrator"}), ev(fw.EvRunStarted, nil), ev(fw.EvTurnStarted, fw.TurnData{Turn: 1}), ev(fw.EvToolRequested, nil),
		ev(fw.EvApprovalRequested, fw.ApprovalRequestedData{ApprovalID: "ap1", Tool: "bash"}),
		ev(fw.EvApprovalDecided, fw.ApprovalDecidedData{ApprovalID: "ap1", Decision: "allow", Decider: "bob", Note: "go"}))
	mig.State, mig.Seq, mig.Pending = fw.StateRunning, 6, nil
	v.put(mig)
	got = poll()
	if len(got) != 1 || got[0].Event != EventDecided || got[0].Text != "bob allowed the call of migrator-1, with the note: go." || got[0].Mirrors.Seq != 6 {
		t.Fatalf("decided = %+v", got)
	}
	msgs := channel(t, s, it.ID)
	rewritten := msgs[card.Seq-1]
	if rewritten.Seq != card.Seq || rewritten.Decision.State != DecisionAllow || rewritten.Decision.DecidedBy != "bob" || rewritten.Decision.Note != "go" || rewritten.Decision.DecidedAt == nil || rewritten.Decision.Effect == "" {
		t.Fatalf("the card after the decision = %+v", rewritten.Decision)
	}
	if hm := h.message(card.Seq); len(hm) != 2 || hm[1].Decision.State != DecisionAllow {
		t.Fatalf("the hook got the card %d times: %+v", len(hm), hm)
	}

	// A question, answered with a form.
	mig.State, mig.Seq = fw.StateInputRequired, 8
	mig.Pending = &fw.Approval{ID: "ap2", Tool: "ask_user", Input: json.RawMessage(`{"question":"Which audience?"}`), Schema: json.RawMessage(`{"type":"object","properties":{"audience":{"type":"string"}}}`), Reason: "question"}
	v.put(mig)
	got = poll()
	if len(got) != 1 || got[0].Event != EventAsked || got[0].Text != "migrator-1 asks: Which audience?" || !got[0].Decision.Question || got[0].Decision.Label != "Answer" || got[0].Decision.Always {
		t.Fatalf("question = %+v", got)
	}
	evs := append(fm.events["r-mig"],
		ev(fw.EvApprovalRequested, fw.ApprovalRequestedData{ApprovalID: "ap2"}),
		ev(fw.EvApprovalDecided, fw.ApprovalDecidedData{ApprovalID: "ap2", Decision: "allow", Decider: "alice", Content: json.RawMessage(`{"audience":"ops"}`)}))
	fm.setEvents("r-mig", evs...)
	mig.State, mig.Seq, mig.Pending = fw.StateRunning, 9, nil
	v.put(mig)
	got = poll()
	if len(got) != 1 || got[0].Text != "alice answered the question of migrator-1: ops." {
		t.Fatalf("answer = %+v", got)
	}

	// The answer of a finished turn is a message of the member, cut at 4 KB.
	long := strings.Repeat("a", 5000)
	evs = append(fm.events["r-mig"], ev(fw.EvRunFinished, fw.RunFinishedData{Outcome: fw.OutcomeCompleted, Output: long}))
	fm.setEvents("r-mig", evs...)
	finSeq := uint64(len(evs))
	mig.State, mig.Seq = fw.StateCompleted, finSeq
	v.put(mig)
	got = poll()
	if len(got) != 1 || got[0].From != (From{Kind: KindAgent, Handle: "migrator-1", Run: "r-mig"}) || got[0].Event != "" || !got[0].Cut || len(got[0].Text) != entryTextBytes ||
		got[0].Mirrors == nil || *got[0].Mirrors != (Mirror{Run: "r-mig", Seq: finSeq, Type: fw.EvRunFinished}) {
		t.Fatalf("finished = %+v", got)
	}
	if again := poll(); len(again) != 0 {
		t.Fatalf("a second look posts %+v", again)
	}
	// A follow-up turn ends again: a second message.
	evs = append(fm.events["r-mig"], ev(fw.EvMessageReceived, nil), ev(fw.EvRunFinished, fw.RunFinishedData{Outcome: fw.OutcomeCompleted, Output: "Second answer."}))
	fm.setEvents("r-mig", evs...)
	mig.Seq = uint64(len(evs))
	v.put(mig)
	if got = poll(); len(got) != 1 || got[0].Text != "Second answer." {
		t.Fatalf("second answer = %+v", got)
	}

	// A failure and a cancel.
	rev := workerRun("r-rev", it.ID, "reviewer", "reviewer", fw.StateFailed)
	fm.setEvents("r-rev", ev(fw.EvRunQueued, nil), ev(fw.EvRunFinished, fw.RunFinishedData{Outcome: fw.OutcomeFailed, Error: "the model refused"}))
	rev.Seq = 2
	v.put(rev)
	got = poll()
	if len(got) != 2 || got[0].Event != EventStarted || got[1].Event != EventFailed || got[1].Text != "reviewer failed: the model refused" {
		t.Fatalf("failure = %+v", got)
	}
	fm.setEvents("r-rev", ev(fw.EvRunQueued, nil), ev(fw.EvRunFinished, fw.RunFinishedData{Outcome: fw.OutcomeFailed, Error: "the model refused"}), ev(fw.EvMessageReceived, nil), ev(fw.EvRunFinished, fw.RunFinishedData{Outcome: fw.OutcomeCanceled}))
	rev.State, rev.Seq = fw.StateCanceled, 4
	v.put(rev)
	if got = poll(); len(got) != 1 || got[0].Event != EventCanceled || got[0].Text != "The run of reviewer was canceled." {
		t.Fatalf("cancel = %+v", got)
	}

	// The Task sleeps, then wakes.
	v.state("default/crew-0", fleet.MemberQuiet)
	got = poll()
	if len(got) != 2 || got[0].Event != EventAsleep || got[0].Text != "migrator-1 went to sleep. A message wakes its Task." {
		t.Fatalf("asleep = %+v", got)
	}
	if again := poll(); len(again) != 0 {
		t.Fatalf("a second look posts %+v", again)
	}
	if ms := h.member("migrator-1"); ms[len(ms)-1].Presence != PresenceAsleep {
		t.Fatalf("presence = %s", ms[len(ms)-1].Presence)
	}
	v.state("default/crew-0", fleet.MemberLive)
	if got = poll(); len(got) != 2 || got[0].Event != EventStarted || got[0].Text != "migrator-1 woke up." {
		t.Fatalf("awake = %+v", got)
	}
	// The record of the bridge counts the runs of each seat.
	resp, body := do(t, http.MethodGet, url+"/v1/bridges/"+it.ID, "alice", "")
	var got2 Item
	decodeJSON(t, body, &got2)
	if resp.StatusCode != http.StatusOK || got2.Members[0].Runs[0] != "r-mig" || got2.Members[0].Presence != PresenceIdle || got2.Members[1].Presence != PresenceIdle {
		t.Fatalf("record = %d %+v", resp.StatusCode, got2.Members)
	}
}

// A card whose run ended without a decision is written again as denied,
// and the watch reads nothing of a run that the channel already mirrored.
func TestCardOfAnEndedRun(t *testing.T) {
	a := newAgent(t, nil)
	s := newStore(t, a, nil)
	v, fm := withFleet(t, a)
	v.state("default/crew-0", fleet.MemberLive)
	url := serve(t, a)
	it := open(t, url)
	r := workerRun("r-mig", it.ID, "migrator-1", "migrator", fw.StateInputRequired)
	r.Pending = &fw.Approval{ID: "ap1", Tool: "files", Input: json.RawMessage(`{"command":"create","path":"app.env"}`), Reason: "rule: ask destructive"}
	v.put(r)
	s.poll(context.Background())
	cards := withEvent(channel(t, s, it.ID), EventNeedsYou)
	if len(cards) != 1 || cards[0].Decision.Always || cards[0].Decision.Effect != "Changes app.env." || cards[0].Text != "migrator-1 wants to change a file." {
		t.Fatalf("card = %+v", cards)
	}
	fm.setEvents("r-mig", ev(fw.EvRunQueued, nil), ev(fw.EvRunFinished, fw.RunFinishedData{Outcome: fw.OutcomeCanceled}))
	r.State, r.Pending, r.Seq = fw.StateCanceled, nil, 2
	v.put(r)
	s.poll(context.Background())
	msgs := channel(t, s, it.ID)
	card := msgs[cards[0].Seq-1]
	if card.Decision.State != DecisionDeny || card.Decision.Note == "" || len(withEvent(msgs, EventDecided)) != 0 || len(withEvent(msgs, EventCanceled)) != 1 {
		t.Fatalf("after the end: card %+v, lines %+v", card.Decision, msgs)
	}
	reads := len(fm.find(http.MethodGet, "/v1/runs/r-mig/events"))
	s.poll(context.Background())
	if n := len(fm.find(http.MethodGet, "/v1/runs/r-mig/events")); n != reads {
		t.Fatalf("the watch read the run again: %d reads after %d", n, reads)
	}
}
