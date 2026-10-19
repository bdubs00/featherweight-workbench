package fleet

import (
	"context"
	"fmt"
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
