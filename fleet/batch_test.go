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
