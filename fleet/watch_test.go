package fleet

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// next reads one change, or fails after a while.
func next(t *testing.T, ch <-chan Change) Change {
	t.Helper()
	select {
	case c, ok := <-ch:
		if !ok {
			t.Fatal("the watch closed")
		}
		return c
	case <-time.After(5 * time.Second):
		t.Fatal("no change arrived")
	}
	return Change{}
}

// drain reads the changes that arrive until none comes for a moment.
func drain(ch <-chan Change) []Change {
	var out []Change
	for {
		select {
		case c, ok := <-ch:
			if !ok {
				return out
			}
			out = append(out, c)
		case <-time.After(200 * time.Millisecond):
			return out
		}
	}
}

// slowFront is a front whose members stay live for the length of a test.
func slowFront(_ *fw.Config, fc *Config) { fc.Interval = time.Hour }

func queuedData(principal string) fw.RunQueuedData {
	return fw.RunQueuedData{Agent: "default", Input: "do it", Principal: principal}
}

// A watcher that does not read keeps the newest change of each key in the
// order in which the keys first came, so a run that changed five times
// arrives once, at its last state.
func TestWatchCoalesces(t *testing.T) {
	t.Setenv("FW_ATESPACE", "")
	front := newMember(t, fw.NewMemBus(), "front", slowFront)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := front.f.Watch(ctx)
	emit(t, front.f, "w1", "01RUNA", 1, fw.EvRunQueued, queuedData("ada"))
	emit(t, front.f, "w1", "01RUNA", 2, fw.EvRunStarted, fw.RunStartedData{})
	for turn := 1; turn <= 4; turn++ {
		emit(t, front.f, "w1", "01RUNA", uint64(2+turn), fw.EvTurnStarted, fw.TurnData{Turn: turn})
	}
	emit(t, front.f, "w1", "01RUNB", 1, fw.EvRunQueued, queuedData("ada"))
	got := drain(ch)
	if len(got) != 3 {
		t.Fatalf("changes = %+v, want the member, run A, and run B", got)
	}
	if got[0].Kind != ChangeMember || got[0].Member != "w1" || got[0].State != MemberLive {
		t.Errorf("first change = %+v, want w1 live", got[0])
	}
	if got[1].Kind != ChangeRun || got[1].Run.ID != "01RUNA" || got[1].Run.Seq != 6 || got[1].Run.Member != "w1" {
		t.Errorf("second change = %+v, want run A at seq 6 on w1", got[1])
	}
	if got[1].Activity != "Thinking in turn 4" {
		t.Errorf("activity of run A = %q", got[1].Activity)
	}
	if got[2].Kind != ChangeRun || got[2].Run.ID != "01RUNB" {
		t.Errorf("third change = %+v, want run B", got[2])
	}
}

// A consumer that never reads holds up nothing: the view takes every
// event, the watcher keeps WatchBuffer changes, and the oldest go first.
func TestWatchSlowConsumer(t *testing.T) {
	t.Setenv("FW_ATESPACE", "")
	front := newMember(t, fw.NewMemBus(), "front", slowFront)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	slow := front.f.Watch(ctx)
	total := WatchBuffer + 200
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := range total {
			emit(t, front.f, "w1", fmt.Sprintf("01RUN%05d", i), 1, fw.EvRunQueued, queuedData("ada"))
		}
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the view waited for a consumer that does not read")
	}
	got := drain(slow)
	// One change can be on its way already, beside the full buffer.
	if len(got) > WatchBuffer+1 || len(got) < WatchBuffer {
		t.Fatalf("got %d changes, want about %d", len(got), WatchBuffer)
	}
	last := got[len(got)-1]
	if last.Run == nil || last.Run.ID != fmt.Sprintf("01RUN%05d", total-1) {
		t.Errorf("the newest change is missing: last = %+v", last)
	}
	ids := map[string]bool{}
	for _, c := range got {
		if c.Run != nil {
			ids[c.Run.ID] = true
		}
	}
	if ids["01RUN00010"] {
		t.Error("an old change stayed in a full buffer")
	}
}

// The channel closes when the context ends, and when the fleet closes.
func TestWatchCloses(t *testing.T) {
	t.Setenv("FW_ATESPACE", "")
	front := newMember(t, fw.NewMemBus(), "front", slowFront)
	ctx, cancel := context.WithCancel(context.Background())
	ch := front.f.Watch(ctx)
	cancel()
	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("a change arrived after the end")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the channel did not close when the context ended")
	}
	if front.f.watched() {
		t.Error("the fleet still counts the watcher")
	}
	other := front.f.Watch(context.Background())
	_ = front.f.Close()
	select {
	case _, ok := <-other:
		if ok {
			t.Fatal("a change arrived after the close")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the channel did not close with the fleet")
	}
}

// A Task sends a task change when its phase changes and a member change
// when the state of the member changes, and both say gone when the Task
// leaves the list. A worker that only the stream knows turns quiet with
// time.
func TestWatchMembersAndTasks(t *testing.T) {
	t.Setenv("FW_ATESPACE", "team")
	t.Setenv("FW_API_TOKEN", "fleet-secret")
	ax := newFakeAX(t, map[string]map[string]string{"team": {"alpha": "Pending"}})
	router := fakeRouter(t, "team/alpha", nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var ch <-chan Change
	front := newMember(t, fw.NewMemBus(), "front", func(cfg *fw.Config, fc *Config) {
		fc.Atespaces = []string{"team"}
		cfg.AX.Server = ax.addr
		cfg.AX.Router = strings.TrimPrefix(router.URL, "http://")
		off := time.Duration(0)
		fc.SuspendAfter = &off
	})
	ch = front.f.Watch(ctx)
	want := func(kind, state, phase string, gone bool) {
		t.Helper()
		deadline := time.After(5 * time.Second)
		for {
			select {
			case c := <-ch:
				if c.Kind == kind && c.Member == "team/alpha" && c.State == state && c.Phase == phase && c.Gone == gone {
					return
				}
			case <-deadline:
				t.Fatalf("no %s change with state %q, phase %q, gone %v", kind, state, phase, gone)
			}
		}
	}
	ax.set("team", "alpha", "Running")
	want(ChangeTask, "", "Running", false)
	want(ChangeMember, MemberLive, "", false)
	ax.set("team", "alpha", "Suspended")
	want(ChangeTask, "", "Suspended", false)
	want(ChangeMember, MemberQuiet, "", false)
	ax.set("team", "alpha", "")
	want(ChangeMember, "", "", true)
	want(ChangeTask, "", "", true)

	// A worker outside ax is live with its event and quiet when its
	// events stop for QuietAfter intervals.
	emit(t, front.f, "w9", "01RUNW", 1, fw.EvRunQueued, queuedData("ada"))
	deadline := time.After(5 * time.Second)
	states := []string{}
	for !slices.Contains(states, MemberQuiet) {
		select {
		case c := <-ch:
			if c.Kind == ChangeMember && c.Member == "w9" {
				states = append(states, c.State)
			}
		case <-deadline:
			t.Fatalf("the states of w9 = %v, want live then quiet", states)
		}
	}
	if !slices.Equal(states, []string{MemberLive, MemberQuiet}) {
		t.Errorf("the states of w9 = %v, want live then quiet", states)
	}
}

// Visible decides for a run as the list of the runs decides for the same
// caller, and for a member as the list of the members.
func TestWatchVisible(t *testing.T) {
	t.Setenv("FW_ATESPACE", "")
	front := newMember(t, fw.NewMemBus(), "front", rulesFront("closed"))
	inject(front.f, "team/gamma", "ax:team/gamma", "team", MemberLive, time.Now(), rulesRuns()...)
	inject(front.f, "team/delta", "ax:team/delta", "team", MemberLive, time.Now(),
		&fw.Run{ID: "d1", Agent: "bare", State: fw.StateCompleted, Principal: "bob", CreatedAt: time.Now()})
	callers := map[string]context.Context{
		"alice": as("alice"), "bob": as("bob"), "carol": as("carol", "ops"),
		"root": as("root"), "dave": as("dave"), "none": context.Background(),
	}
	for name, ctx := range callers {
		runs, err := front.f.Runs(ctx)
		if err != nil {
			t.Fatal(err)
		}
		listed := runIDs(runs)
		for _, r := range append(rulesRuns(), &fw.Run{ID: "d1", Agent: "bare", Principal: "bob"}) {
			member := "team/gamma"
			if r.ID == "d1" {
				member = "team/delta"
			}
			ok, err := front.f.Visible(ctx, Change{Kind: ChangeRun, Member: member, Run: withMember(r, member)})
			if err != nil {
				t.Fatal(err)
			}
			if ok != slices.Contains(listed, r.ID) {
				t.Errorf("%s: Visible(%s) = %v, but the list says %v", name, r.ID, ok, listed)
			}
		}
		members, err := front.f.Members(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, member := range []string{"team/gamma", "team/delta"} {
			shown := slices.ContainsFunc(members, func(m Member) bool { return m.Name == member })
			for _, kind := range []string{ChangeMember, ChangeTask} {
				ok, err := front.f.Visible(ctx, Change{Kind: kind, Member: member})
				if err != nil {
					t.Fatal(err)
				}
				if ok != shown {
					t.Errorf("%s: Visible(%s %s) = %v, but the members say %v", name, kind, member, ok, shown)
				}
			}
		}
	}
}

// The view keeps the input of the tool call of a run, so the activity
// names the step; the input on the stream can be cut by the worker, and
// the view bounds what it keeps. The result of the call forgets it.
func TestWatchActivityPhrase(t *testing.T) {
	t.Setenv("FW_ATESPACE", "")
	front := newMember(t, fw.NewMemBus(), "front", slowFront)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := front.f.Watch(ctx)
	emit(t, front.f, "w1", "01RUNA", 1, fw.EvRunQueued, queuedData("ada"))
	emit(t, front.f, "w1", "01RUNA", 2, fw.EvRunStarted, fw.RunStartedData{})
	emit(t, front.f, "w1", "01RUNA", 3, fw.EvTurnStarted, fw.TurnData{Turn: 1})
	// The worker cut the command with bus.egressMaxBytes, and the input
	// has more strings than the view keeps whole.
	command := "go test ./... " + strings.Repeat("x", 4000) + " [cut 6000 of 10000 bytes]"
	input, _ := json.Marshal(map[string]any{"command": command, "notes": strings.Repeat("n", 3000), "more": strings.Repeat("m", 3000)})
	emit(t, front.f, "w1", "01RUNA", 4, fw.EvToolRequested, fw.ToolRequestedData{Tool: "bash", CallID: "call-1", Input: input})
	var c Change
	for c.Run == nil || c.Run.Seq != 4 {
		c = next(t, ch)
	}
	want := `Ran "go test ./... ` + strings.Repeat("x", 65) + `…"`
	if c.Activity != want {
		t.Errorf("activity = %q, want %q", c.Activity, want)
	}
	if got := ActivityPhrase(c.Run); got != want {
		t.Errorf("ActivityPhrase = %q, want %q", got, want)
	}
	if in, _ := front.f.toolInput("01RUNA", "call-1"); len(in) > ActivityInputLimit || !json.Valid(in) {
		t.Errorf("the view keeps %d bytes of the input, valid %v; want at most %d", len(in), json.Valid(in), ActivityInputLimit)
	}
	emit(t, front.f, "w1", "01RUNA", 5, fw.EvToolResponded, fw.ToolRespondedData{CallID: "call-1"})
	for c.Run == nil || c.Run.Seq != 5 {
		c = next(t, ch)
	}
	if c.Activity != "Thinking in turn 1" {
		t.Errorf("activity after the result = %q", c.Activity)
	}
	if _, kept := front.f.toolInput("01RUNA", "call-1"); kept {
		t.Error("the view keeps the input of a call that returned")
	}
	// A copy of the run with an old call names the tool alone.
	old := *c.Run
	old.Activity = &fw.RunActivity{Turn: 1, Tool: "bash", CallID: "call-0"}
	if got := ActivityPhrase(&old); got != "bash" {
		t.Errorf("ActivityPhrase of an old call = %q, want bash", got)
	}
}

func TestBoundInput(t *testing.T) {
	small := json.RawMessage(`{"command":"ls"}`)
	if got := boundInput(small, ActivityInputLimit); string(got) != string(small) {
		t.Errorf("a small input changed: %s", got)
	}
	big, _ := json.Marshal(map[string]any{"path": "a.go", "content": strings.Repeat("é", 5000)})
	got := boundInput(big, ActivityInputLimit)
	if len(got) > ActivityInputLimit || !json.Valid(got) {
		t.Fatalf("bounded input of %d bytes, valid %v", len(got), json.Valid(got))
	}
	var m map[string]string
	if err := json.Unmarshal(got, &m); err != nil || m["path"] != "a.go" {
		t.Errorf("bounded input = %s, %v", got, err)
	}
	wide := map[string]any{}
	for i := range 2000 {
		wide[fmt.Sprintf("k%04d", i)] = "v"
	}
	raw, _ := json.Marshal(wide)
	if got := boundInput(raw, ActivityInputLimit); got != nil {
		t.Errorf("an input that cannot fit gave %d bytes", len(got))
	}
	if got := boundInput(json.RawMessage(strings.Repeat("[", 5000)), ActivityInputLimit); got != nil {
		t.Errorf("an input that does not decode gave %s", got)
	}
}
