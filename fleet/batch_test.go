package fleet

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// stream applies events of one run to the view in the order given, as the
// egress stream of the node box:8080 delivers them, with the sequence
// numbers given.
func stream(t *testing.T, f *Fleet, id string, seqs []uint64, evs ...fw.Event) {
	t.Helper()
	for i, e := range evs {
		e.Subject, e.Seq, e.Node, e.Source = id, seqs[i], "box:8080", "fw://local/default/"+id
		if err := f.platform.onEvent(e); err != nil {
			t.Fatal(err)
		}
	}
}

// finishedRun is the lifecycle of one finished run of a batch with totals,
// as ada.
func finishedRun(batch, input string, totals fw.RunUsage) []fw.Event {
	return finishedRunOf("ada", batch, input, totals)
}

// finishedRunOf is finishedRun as another person.
func finishedRunOf(principal, batch, input string, totals fw.RunUsage) []fw.Event {
	return []fw.Event{
		fw.MustEvent("", fw.EvRunQueued, fw.RunQueuedData{Agent: "default", Input: input, Origin: fw.OriginBus, Principal: principal, Metadata: map[string]string{fw.MetaBatch: batch}}),
		fw.MustEvent("", fw.EvRunStarted, fw.RunStartedData{Model: "fake/m"}),
		fw.MustEvent("", fw.EvTurnStarted, fw.TurnData{Turn: 1}),
		fw.MustEvent("", fw.EvRunFinished, fw.RunFinishedData{Outcome: fw.OutcomeCompleted, Output: "ok", Totals: &totals}),
	}
}

// TestBatchListsEveryRun puts 60 finished runs of one job on one worker in
// the view. A record holds the newest FinishedRuns finished runs, so Runs
// lists 50, but Batch lists all 60. When the view drops the runs after the
// expire time, Batch counts them as forgotten, for a caller who can see the
// job.
func TestBatchListsEveryRun(t *testing.T) {
	ctx := context.Background()
	front := newMember(t, fw.NewMemBus(), "front", func(c *fw.Config, _ *Config) { c.Access.Default = "closed" })
	f := front.f
	totals := fw.RunUsage{Usage: fw.Usage{InputTokens: 100, OutputTokens: 10}, Turns: 1}
	seqs := []uint64{1, 2, 3, 4}
	for i := 0; i < 60; i++ {
		stream(t, f, fw.NewID(), seqs, finishedRun("big", fmt.Sprint("item ", i), totals)...)
	}
	stream(t, f, fw.NewID(), seqs, finishedRun("other", "else", totals)...)

	runs, err := f.Runs(ctx)
	if err != nil || len(runs) != FinishedRuns {
		t.Fatalf("Runs = %d, %v; want the cut of %d", len(runs), err, FinishedRuns)
	}
	b, err := f.Batch(ctx, "big")
	if err != nil || len(b.Runs) != 60 || b.Forgotten != 0 {
		t.Fatalf("Batch = %d runs, %d forgotten, %v", len(b.Runs), b.Forgotten, err)
	}
	for _, r := range b.Runs {
		if r.Member != "box:8080" || r.State != fw.StateCompleted || r.Usage != totals || r.Metadata[fw.MetaBatch] != "big" {
			t.Fatalf("run = %+v", r)
		}
	}
	if b, _ := f.Batch(as("bob"), "big"); len(b.Runs) != 0 {
		t.Fatalf("bob sees %d runs of the job of ada", len(b.Runs))
	}

	// The view forgets the finished runs after the expire time.
	f.platform.records(time.Now().Add(f.expire+time.Minute), false)
	if b, err := f.Batch(ctx, "big"); err != nil || len(b.Runs) != 0 || b.Forgotten != 60 {
		t.Fatalf("after expire: Batch = %d runs, %d forgotten, %v", len(b.Runs), b.Forgotten, err)
	}
	// A caller who can see no run of the job learns nothing of it.
	if b, _ := f.Batch(as("bob"), "big"); b.Forgotten != 0 {
		t.Fatalf("bob learns of %d forgotten runs", b.Forgotten)
	}
	// ada, who started the runs, learns of them.
	stream(t, f, fw.NewID(), seqs, finishedRun("big", "late", totals)...)
	if b, _ := f.Batch(as("ada"), "big"); len(b.Runs) != 1 || b.Forgotten != 60 {
		t.Fatalf("ada: Batch = %d runs, %d forgotten", len(b.Runs), b.Forgotten)
	}
}

// TestPlatformQueuedOnce sends the same fw.run.queued again after a run
// finished, as a retry of the egress does, and delivers the events of
// another run out of order. A repeated fw.run.queued changes nothing, and
// a late one gives the run its identity and keeps the state of the later
// events.
func TestPlatformQueuedOnce(t *testing.T) {
	ctx := context.Background()
	front := newMember(t, fw.NewMemBus(), "front", nil)
	f := front.f
	totals := fw.RunUsage{Usage: fw.Usage{InputTokens: 5}, Turns: 1}
	evs := finishedRun("r", "item", totals)

	id := fw.NewID()
	stream(t, f, id, []uint64{1, 2, 3, 4, 1}, append(evs, evs[0])...)
	b, err := f.Batch(ctx, "r")
	if err != nil || len(b.Runs) != 1 || b.Runs[0].State != fw.StateCompleted || b.Runs[0].Usage != totals || b.Runs[0].Seq != 4 {
		t.Fatalf("after a repeated fw.run.queued: %+v, %v", b.Runs, err)
	}

	late := fw.NewID()
	stream(t, f, late, []uint64{4, 2, 1, 3, 1}, evs[3], evs[1], evs[0], evs[2], evs[0])
	r, err := f.Run(ctx, late)
	if err != nil || r.State != fw.StateCompleted || r.Usage != totals || r.Principal != "ada" || r.Agent != "default" || r.Metadata[fw.MetaBatch] != "r" {
		t.Fatalf("out of order: %+v, %v", r, err)
	}
	if runs, _ := f.Runs(as("ada")); len(runs) != 2 {
		t.Fatalf("ada sees %d runs", len(runs))
	}
}

// TestBatchForgottenPerPerson drops the runs of one batch name that two
// people use. Each person learns only of the dropped runs that the person
// could see, and an admin learns of all of them.
func TestBatchForgottenPerPerson(t *testing.T) {
	front := newMember(t, fw.NewMemBus(), "front", func(c *fw.Config, _ *Config) {
		c.Access.Default = "closed"
		c.Access.Admins = []string{"root"}
	})
	f := front.f
	totals := fw.RunUsage{Usage: fw.Usage{InputTokens: 100}, Turns: 1}
	seqs := []uint64{1, 2, 3, 4}
	for i := 0; i < 3; i++ {
		stream(t, f, fw.NewID(), seqs, finishedRunOf("bob", "nightly", fmt.Sprint("bob ", i), totals)...)
	}
	stream(t, f, fw.NewID(), seqs, finishedRunOf("ada", "nightly", "ada 0", totals)...)
	f.platform.records(time.Now().Add(f.expire+time.Minute), false)
	stream(t, f, fw.NewID(), seqs, finishedRunOf("ada", "nightly", "ada 1", totals)...)
	for _, tc := range []struct {
		who             string
		runs, forgotten int
	}{
		{"ada", 1, 1},
		{"bob", 0, 3},
		{"root", 1, 4},
		{"eve", 0, 0},
	} {
		b, err := f.Batch(as(tc.who), "nightly")
		if err != nil || len(b.Runs) != tc.runs || b.Forgotten != tc.forgotten {
			t.Errorf("%s: %d runs, %d forgotten, %v; want %d, %d", tc.who, len(b.Runs), b.Forgotten, err, tc.runs, tc.forgotten)
		}
	}
}

// TestPlatformLateEnd delivers the end of a run after a later event. After
// a title annotation, which changes no state, the end still applies, and
// the usage of the annotation stays on top of the totals. After a
// follow-up, the run works again, so the late end changes nothing.
func TestPlatformLateEnd(t *testing.T) {
	ctx := context.Background()
	front := newMember(t, fw.NewMemBus(), "front", nil)
	f := front.f
	totals := fw.RunUsage{Usage: fw.Usage{InputTokens: 100}, Turns: 1}
	evs := finishedRun("late", "item", totals)
	title := fw.Usage{InputTokens: 60, OutputTokens: 6}
	annotated := fw.MustEvent("", fw.EvRunAnnotated, fw.RunAnnotatedData{Metadata: map[string]string{fw.MetaTitle: "T"}, Usage: &title, Provider: "fake", Model: "titles"})
	suspended := fw.MustEvent("", fw.EvRunSuspended, fw.RunSuspendedData{Reason: "shutdown", Totals: &totals})
	again := fw.MustEvent("", fw.EvMessageReceived, fw.MessageReceivedData{Role: fw.RoleUser, Content: []fw.Block{fw.Text("more")}})

	finished, paused, reopened := fw.NewID(), fw.NewID(), fw.NewID()
	stream(t, f, finished, []uint64{1, 2, 3, 5, 4}, evs[0], evs[1], evs[2], annotated, evs[3])
	stream(t, f, paused, []uint64{1, 2, 3, 5, 4}, evs[0], evs[1], evs[2], annotated, suspended)
	stream(t, f, reopened, []uint64{1, 2, 3, 5, 6, 4}, evs[0], evs[1], evs[2], again, evs[2], evs[3])

	want := fw.RunUsage{Usage: totals.Usage.Add(title), Turns: 1, UnpricedTokens: 66}
	for _, tc := range []struct {
		id    string
		state fw.RunState
		usage fw.RunUsage
		seq   uint64
	}{
		{finished, fw.StateCompleted, want, 4},
		{paused, fw.StateSuspended, want, 4},
		{reopened, fw.StateRunning, fw.RunUsage{Turns: 2}, 0},
	} {
		r, err := f.Run(ctx, tc.id)
		if err != nil || r.State != tc.state || r.Usage != tc.usage || r.UsageSeq != tc.seq || r.Title != "T" && tc.id != reopened {
			t.Errorf("run %s = state %s, usage %+v at %d, %v; want %s, %+v at %d", tc.id, r.State, r.Usage, r.UsageSeq, err, tc.state, tc.usage, tc.seq)
		}
	}
}

// TestPlatformLateQueuedState delivers a title annotation of a queued run
// before its fw.run.queued. No event set a state, so the run is queued.
func TestPlatformLateQueuedState(t *testing.T) {
	front := newMember(t, fw.NewMemBus(), "front", nil)
	f := front.f
	evs := finishedRun("noState", "item", fw.RunUsage{})
	title := fw.MustEvent("", fw.EvRunAnnotated, fw.RunAnnotatedData{Metadata: map[string]string{fw.MetaTitle: "T"}})
	id := fw.NewID()
	stream(t, f, id, []uint64{2, 1}, title, evs[0])
	b, err := f.Batch(as("ada"), "noState")
	if err != nil || len(b.Runs) != 1 || b.Runs[0].State != fw.StateQueued || b.Runs[0].Title != "T" || b.Runs[0].Agent != "default" {
		t.Fatalf("Batch = %+v, %v", b.Runs, err)
	}
}

// TestForgottenCopyKeepsNoText drops a finished run with a title and other
// metadata. The copy that the view keeps of it holds what the
// authorization and the batch read, and no text: no title, no input, no
// output, and no metadata beyond fw.batch.
func TestForgottenCopyKeepsNoText(t *testing.T) {
	front := newMember(t, fw.NewMemBus(), "front", nil)
	f := front.f
	totals := fw.RunUsage{Usage: fw.Usage{InputTokens: 100}, Turns: 1}
	evs := finishedRun("job", "the input of the run", totals)
	title := fw.MustEvent("", fw.EvRunAnnotated, fw.RunAnnotatedData{Metadata: map[string]string{fw.MetaTitle: "A title from a model", "note": "private"}})
	id := fw.NewID()
	stream(t, f, id, []uint64{1, 2, 3, 4, 5}, append(evs, title)...)
	f.platform.records(time.Now().Add(f.expire+time.Minute), false)
	runs := f.platform.forgotten("job")["box:8080"]
	if len(runs) != 1 {
		t.Fatalf("forgotten = %+v", runs)
	}
	c := runs[0]
	if c.ID != id || c.Principal != "ada" || c.Agent != "default" || c.State != fw.StateCompleted ||
		c.Title != "" || c.Input != "" || c.Output != "" || len(c.Metadata) != 1 || c.Metadata[fw.MetaBatch] != "job" {
		t.Fatalf("the forgotten copy = %+v", c)
	}
}

// TestPlatformEventAfterDrop drops a finished run, and then the source of a
// branch appends fw.run.child to it. The run comes back with its identity,
// not as a run with no agent and no person, and it leaves the view again
// after the expire time. A run that the view knows from an event alone, with
// no identity, is not listed, and it leaves after the expire time too.
func TestPlatformEventAfterDrop(t *testing.T) {
	ctx := context.Background()
	front := newMember(t, fw.NewMemBus(), "front", nil)
	f := front.f
	totals := fw.RunUsage{Usage: fw.Usage{InputTokens: 100}, Turns: 1}
	src := fw.NewID()
	stream(t, f, src, []uint64{1, 2, 3, 4}, finishedRun("job", "item", totals)...)
	f.platform.records(time.Now().Add(f.expire+time.Minute), false)
	f.platform.mu.Lock()
	_, held := f.platform.runs[src]
	f.platform.mu.Unlock()
	if held {
		t.Fatal("the view still has the source")
	}
	if b, _ := f.Batch(ctx, "job"); b.Forgotten != 1 {
		t.Fatalf("forgotten = %d", b.Forgotten)
	}
	branch := fw.NewID()
	stream(t, f, src, []uint64{5}, fw.MustEvent("", fw.EvRunChild, fw.RunChildData{Child: branch, Member: "ax:space/child", Principal: "ada", Kind: "branch"}))
	r, err := f.Run(as("ada"), src)
	if err != nil || r.State != fw.StateCompleted || r.Agent != "default" || r.Principal != "ada" || r.Usage != totals || r.Seq != 5 {
		t.Fatalf("the run after its fw.run.child = %+v, %v", r, err)
	}
	// The job holds the source again and the stand-in of its branch.
	if b, _ := f.Batch(ctx, "job"); len(b.Runs) != 2 || b.Forgotten != 0 {
		t.Fatalf("Batch = %d runs, %d forgotten", len(b.Runs), b.Forgotten)
	}

	stranger := fw.NewID()
	stream(t, f, stranger, []uint64{3}, fw.MustEvent("", fw.EvTurnStarted, fw.TurnData{Turn: 1}))
	runs, _ := f.Runs(ctx)
	for _, x := range runs {
		if x.ID == stranger {
			t.Fatalf("a run with no identity is listed: %+v", x)
		}
	}
	f.platform.records(time.Now().Add(3*f.expire), false)
	f.platform.mu.Lock()
	_, srcLeft := f.platform.runs[src]
	_, strangerLeft := f.platform.runs[stranger]
	f.platform.mu.Unlock()
	if srcLeft || strangerLeft {
		t.Fatalf("after the expire time: the source is in view %v, the run with no identity %v", srcLeft, strangerLeft)
	}
}

// TestPlatformBackfillGivesIdentity starts a front while two runs of a job
// work on a sleeping Task. The stream reaches each run before the one read
// of the Task lands: one at the sequence number of the read, one at a newer
// one. The read gives both their identity, the newer state of the stream
// stays, and until the read the job counts both as runs of no known job.
func TestPlatformBackfillGivesIdentity(t *testing.T) {
	t.Setenv("FW_ATESPACE", "team")
	t.Setenv("FW_API_TOKEN", "fleet-secret")
	ctx := context.Background()
	ax := newFakeAX(t, map[string]map[string]string{"team": {"gamma": "Suspended"}})
	read := fw.RunUsage{Usage: fw.Usage{InputTokens: 300}, Turns: 2}
	same := &fw.Run{ID: "01GAMMA", Agent: "default", State: fw.StateRunning, Principal: "bob", Seq: 9, Usage: read,
		Metadata: map[string]string{fw.MetaBatch: "job"}, CreatedAt: time.Now().Add(-time.Minute), UpdatedAt: time.Now()}
	newer := &fw.Run{ID: "01DELTA", Agent: "default", State: fw.StateInputRequired, Principal: "bob", Seq: 9, Usage: read,
		Pending:  &fw.Approval{ID: "ap1", Tool: "shell"},
		Metadata: map[string]string{fw.MetaBatch: "job"}, CreatedAt: time.Now().Add(-time.Minute), UpdatedAt: time.Now()}
	router := fakeRouter(t, "team/gamma", []*fw.Run{same, newer})
	front := newMember(t, fw.NewMemBus(), "front", func(cfg *fw.Config, fc *Config) {
		fc.Atespaces = []string{"team"}
		cfg.AX.Server = ax.addr
		cfg.AX.Router = strings.TrimPrefix(router.URL, "http://")
		cfg.Access.Default = "closed"
	})
	p := front.f.platform
	eventually(t, "the front lists gamma", func() bool {
		p.mu.Lock()
		defer p.mu.Unlock()
		return p.members["team/gamma"] != nil
	})
	at := func(id string, seq uint64, typ string, data any) {
		e := fw.MustEvent(id, typ, data)
		e.Seq, e.Node, e.Source = seq, "ax:team/gamma", "fw://local/default/"+id
		if err := p.onEvent(e); err != nil {
			t.Fatal(err)
		}
	}
	at("01GAMMA", 9, fw.EvTurnStarted, fw.TurnData{Turn: 2})
	at("01DELTA", 10, fw.EvApprovalDecided, fw.ApprovalDecidedData{ApprovalID: "ap1", Decision: "allow"})
	at("01DELTA", 11, fw.EvTurnStarted, fw.TurnData{Turn: 3})
	if b, _ := front.f.Batch(ctx, "job"); len(b.Runs) != 0 || b.Unknown != 2 || b.UnknownActive != 2 {
		t.Fatalf("before the read: %d runs, %d unknown, %d unknown active", len(b.Runs), b.Unknown, b.UnknownActive)
	}
	p.mu.Lock()
	m := p.members["team/gamma"]
	p.mu.Unlock()
	p.backfill(ctx, m)

	b, err := front.f.Batch(as("bob"), "job")
	if err != nil || len(b.Runs) != 2 || b.Unknown != 0 {
		t.Fatalf("bob's job: %d runs, %d unknown, %v", len(b.Runs), b.Unknown, err)
	}
	got := map[string]*fw.Run{}
	for _, r := range b.Runs {
		got[r.ID] = r
	}
	if r := got["01GAMMA"]; r == nil || r.Principal != "bob" || r.State != fw.StateRunning || r.Usage != read || r.Seq != 9 {
		t.Fatalf("the run at the sequence number of the read = %+v", r)
	}
	if r := got["01DELTA"]; r == nil || r.Principal != "bob" || r.Agent != "default" || r.State != fw.StateRunning || r.Pending != nil ||
		r.Usage.Turns != 3 || r.Usage.InputTokens != 300 || r.Seq != 11 {
		t.Fatalf("the run that the stream reached after the read = %+v", r)
	}
}

// TestPlatformTwoPublishersOfABranch delivers the events of one branch
// from two processes: the record of its source, which took the start of
// the branch for failed, with Kept zero, and then the copies that the
// process of the branch took after all, with a finished source. The larger
// Kept wins, so the copies do not count as the work of the branch.
func TestPlatformTwoPublishersOfABranch(t *testing.T) {
	ctx := context.Background()
	front := newMember(t, fw.NewMemBus(), "front", nil)
	f := front.f
	srcTotals := fw.RunUsage{Usage: fw.Usage{InputTokens: 1000, OutputTokens: 100}, Turns: 3}
	id := fw.NewID()
	q := fw.RunQueuedData{Agent: "default", Input: "item", Principal: "ada", Metadata: map[string]string{fw.MetaBatch: "two"}}
	stream(t, f, id, []uint64{1, 2},
		fw.MustEvent("", fw.EvRunQueued, q),
		fw.MustEvent("", fw.EvRunFinished, fw.RunFinishedData{Outcome: fw.OutcomeFailed, Error: "did not start", Totals: &fw.RunUsage{}}))
	kq := q
	kq.Kept = 4
	for i, e := range []fw.Event{
		fw.MustEvent("", fw.EvRunQueued, kq),
		fw.MustEvent("", fw.EvRunStarted, fw.RunStartedData{Model: "fake/m"}),
		fw.MustEvent("", fw.EvTurnStarted, fw.TurnData{Turn: 1}),
		fw.MustEvent("", fw.EvRunFinished, fw.RunFinishedData{Outcome: fw.OutcomeCompleted, Output: "ok", Totals: &srcTotals}),
	} {
		e.Subject, e.Seq, e.Node, e.Source = id, uint64(i+1), "ax:space/child", "fw://local/default/"+id
		if err := f.platform.onEvent(e); err != nil {
			t.Fatal(err)
		}
	}
	b, err := f.Batch(ctx, "two")
	if err != nil || len(b.Runs) != 1 {
		t.Fatalf("Batch = %+v, %v", b, err)
	}
	if r := b.Runs[0]; r.Usage != (fw.RunUsage{}) || r.UsageSeq == 0 || r.State != fw.StateCompleted {
		t.Fatalf("the branch = state %s, usage %+v at %d", r.State, r.Usage, r.UsageSeq)
	}
}

// TestPlatformBackfillRetries starts a front while a run works on a Task
// whose first read through the router fails. The front reads the Task again
// after a pause, so the run gets its identity from the read.
func TestPlatformBackfillRetries(t *testing.T) {
	t.Setenv("FW_ATESPACE", "team")
	t.Setenv("FW_API_TOKEN", "fleet-secret")
	ctx := context.Background()
	ax := newFakeAX(t, map[string]map[string]string{"team": {"gamma": "Running"}})
	run := &fw.Run{ID: "01GAMMA", Agent: "default", State: fw.StateRunning, Principal: "bob", Seq: 9,
		Metadata: map[string]string{fw.MetaBatch: "job"}, CreatedAt: time.Now().Add(-time.Minute), UpdatedAt: time.Now()}
	var mu sync.Mutex
	reads := 0
	router := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/runs" {
			_ = json.NewEncoder(w).Encode(map[string]any{"agents": []any{}})
			return
		}
		mu.Lock()
		reads++
		n := reads
		mu.Unlock()
		if n == 1 {
			http.Error(w, "the router is not ready", http.StatusBadGateway)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"runs": []*fw.Run{run}})
	}))
	t.Cleanup(router.Close)
	front := newMember(t, fw.NewMemBus(), "front", func(cfg *fw.Config, fc *Config) {
		fc.Atespaces = []string{"team"}
		cfg.AX.Server = ax.addr
		cfg.AX.Router = strings.TrimPrefix(router.URL, "http://")
	})
	eventually(t, "a later read gives the run its identity", func() bool {
		r, err := front.f.Run(ctx, "01GAMMA")
		return err == nil && r.Principal == "bob" && r.Metadata[fw.MetaBatch] == "job"
	})
	mu.Lock()
	defer mu.Unlock()
	if reads < 2 {
		t.Fatalf("reads of the runs = %d", reads)
	}
}

// TestPlatformBackfillClearsTheDroppedCopy reads a Task, drops its run as
// when the Task leaves the list, and reads the Task again when it comes
// back. The run is in view once, not also among the forgotten runs.
func TestPlatformBackfillClearsTheDroppedCopy(t *testing.T) {
	t.Setenv("FW_ATESPACE", "team")
	t.Setenv("FW_API_TOKEN", "fleet-secret")
	ctx := context.Background()
	ax := newFakeAX(t, map[string]map[string]string{"team": {"gamma": "Suspended"}})
	run := &fw.Run{ID: "01GAMMA", Agent: "default", State: fw.StateCompleted, Principal: "bob", Seq: 4, UsageSeq: 4,
		Usage: fw.RunUsage{Usage: fw.Usage{InputTokens: 7}}, Metadata: map[string]string{fw.MetaBatch: "job"},
		CreatedAt: time.Now().Add(-time.Minute), UpdatedAt: time.Now()}
	router := fakeRouter(t, "team/gamma", []*fw.Run{run})
	front := newMember(t, fw.NewMemBus(), "front", func(cfg *fw.Config, fc *Config) {
		fc.Atespaces = []string{"team"}
		cfg.AX.Server = ax.addr
		cfg.AX.Router = strings.TrimPrefix(router.URL, "http://")
	})
	p := front.f.platform
	eventually(t, "the front lists gamma", func() bool {
		p.mu.Lock()
		defer p.mu.Unlock()
		return p.members["team/gamma"] != nil
	})
	p.mu.Lock()
	m := p.members["team/gamma"]
	p.mu.Unlock()
	p.backfill(ctx, m)
	p.mu.Lock()
	p.drop("01GAMMA", p.runs["01GAMMA"], time.Now())
	p.mu.Unlock()
	p.backfill(ctx, m)
	if b, err := front.f.Batch(as("bob"), "job"); err != nil || len(b.Runs) != 1 || b.Forgotten != 0 {
		t.Fatalf("bob's job: %d runs, %d forgotten, %v", len(b.Runs), b.Forgotten, err)
	}
}
