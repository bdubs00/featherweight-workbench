package knowhow

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/fleet"
	"github.com/bdubs00/featherweight-workbench/internal/bucket"
)

// labelRun records a run of triage that reads the labels with a built-in
// tool, fails to add a label with a space, and passes without it.
func labelRun(run string, at time.Time, principal, input string) *recorder {
	return record(run, at).queued("triage", input, principal, map[string]string{fw.MetaDuty: "nightly"}).started("").
		call("c1", "labels_read", "builtin", `{"repo": "helpdesk"}`, `[{"id": 11, "name": "type/bug"}]`, false).
		call("c2", "label_add", "builtin", `{"issue": 42, "label": "needs info"}`, "error 422: a label name holds no space", true).
		call("c3", "label_add", "builtin", `{"issue": 42, "label": "needs-info"}`, "ok", false).
		finished("Labeled #42.", 0.02)
}

// The pass reads the finished runs behind its cursor and finds a gotcha in
// one run, a place in two runs, and an outcome when a receipt arrives
// after the end. A second pass reads nothing new, and the receipt makes
// the run read again.
func TestCandidatePass(t *testing.T) {
	fr := newFront(t, nil)
	ctx := context.Background()
	fr.arch.put(labelRun("01KRUN00000000000000000001", day0, "duty:nightly", "Ticket #42").out, day0)
	// A run that still works and a run that changed a moment ago wait.
	fr.arch.put(record("01KRUN00000000000000000009", day0).queued("triage", "x", "duty:nightly", nil).started("").out, day0)
	fr.clk.add(time.Hour)
	n, err := fr.s.CandidatePass(ctx)
	if err != nil || n != 1 {
		t.Fatalf("the pass read %d runs, %v", n, err)
	}
	list, _ := fr.s.Candidates(ctx, "triage", nil)
	if len(list) != 1 || list[0].Kind != KindGotcha || list[0].State != CandOpen || list[0].Origin != OriginTrusted || list[0].Scope != duty ||
		!strings.Contains(list[0].Evidence[0].Text, "needs-info") || list[0].Sources[0].Ref != "run:01KRUN00000000000000000001" {
		t.Fatalf("candidates = %+v", list)
	}
	if n, _ := fr.s.CandidatePass(ctx); n != 0 {
		t.Fatalf("a second pass read %d runs", n)
	}
	// A second run with the same calls: the same gotcha, and a place.
	fr.arch.put(labelRun("01KRUN00000000000000000002", day0.Add(time.Minute), "duty:nightly", "Ticket #43").out, day0.Add(time.Hour))
	fr.clk.add(time.Hour)
	if n, _ := fr.s.CandidatePass(ctx); n != 1 {
		t.Fatalf("the pass read %d runs", n)
	}
	list, _ = fr.s.Candidates(ctx, "triage", nil)
	kinds := map[string]Candidate{}
	for _, c := range list {
		kinds[c.Kind] = c
	}
	if len(list) != 2 || len(kinds[KindGotcha].Runs) != 2 || len(kinds[KindPlace].Runs) != 2 || kinds[KindPlace].State != CandOpen {
		t.Fatalf("candidates = %+v", list)
	}
	// A receipt arrives later; the summary of the run changes.
	rec := labelRun("01KRUN00000000000000000001", day0, "duty:nightly", "Ticket #42").receipt("accepted")
	fr.arch.put(rec.out, day0.Add(2*time.Hour))
	fr.clk.add(time.Hour)
	if n, _ := fr.s.CandidatePass(ctx); n != 1 {
		t.Fatalf("the receipt made the pass read %d runs", n)
	}
	list, _ = fr.s.Candidates(ctx, "triage", nil)
	var outcome *Candidate
	for i := range list {
		if list[i].Kind == KindOutcome {
			outcome = &list[i]
		}
	}
	if outcome == nil || !strings.Contains(outcome.Summary, "went well") || !strings.Contains(outcome.Evidence[0].Text, "a receipt of accepted") || len(fmt.Sprint(kinds[KindGotcha].Runs)) == 0 {
		t.Fatalf("outcome = %+v", outcome)
	}
	// Runs of another principal make candidates of their own scope; a run
	// without a principal makes none.
	fr.arch.put(labelRun("01KRUN00000000000000000003", day0, "bob", "Ticket #44").out, day0.Add(3*time.Hour))
	fr.arch.put(labelRun("01KRUN00000000000000000004", day0, "", "Ticket #45").out, day0.Add(3*time.Hour))
	fr.clk.add(time.Hour)
	if _, err := fr.s.CandidatePass(ctx); err != nil {
		t.Fatal(err)
	}
	list, _ = fr.s.Candidates(ctx, "triage", func(c Candidate) bool { return c.Scope.Principal == "bob" })
	if len(list) != 1 || list[0].Kind != KindGotcha || len(list[0].Runs) != 1 {
		t.Fatalf("the candidates of bob = %+v", list)
	}
	// Candidates drop after 14 days.
	fr.clk.add(15 * 24 * time.Hour)
	if n, err := fr.s.dropCandidates(ctx); err != nil || n != 4 {
		t.Fatalf("dropped %d, %v", n, err)
	}
}

// The taint rule: the text of an MCP tool of another service, a block
// found, event, or source, and a message of another principal are
// untrusted. An untrusted candidate waits for two runs with different
// triggers; a second run of the same trigger keeps it waiting.
func TestTaint(t *testing.T) {
	fr := newFront(t, nil)
	ctx := context.Background()
	gitea := func(run, input, context string, at time.Time) *recorder {
		return record(run, at).queued("triage", input, "duty:nightly", nil).started(context).
			call("c1", "gitea.issue_write", "gitea", `{"label": "needs info"}`, "error: no space", true).
			call("c2", "gitea.issue_write", "gitea", `{"label": "needs-info"}`, "ok", false).
			finished("done", 0.01)
	}
	fr.arch.put(gitea("01KTNT00000000000000000001", "Ticket", "", day0).out, day0)
	fr.arch.put(gitea("01KTNT00000000000000000002", "Ticket", "", day0).out, day0)
	fr.clk.add(time.Hour)
	if _, err := fr.s.CandidatePass(ctx); err != nil {
		t.Fatal(err)
	}
	list, _ := fr.s.Candidates(ctx, "triage", nil)
	if len(list) != 1 || list[0].Origin != OriginUntrusted || list[0].State != CandGathering || len(list[0].Runs) != 2 {
		t.Fatalf("two runs of one trigger = %+v", list)
	}
	open, _ := fr.s.OpenCandidates(ctx, "triage", 10)
	if len(open.Candidates) != 0 {
		t.Fatal("the review sees an untrusted candidate of one trigger")
	}
	if _, err := fr.s.Keep(ctx, KeepRequest{Candidate: list[0].ID, Op: OpAdd, Name: "x.md", Text: "Gitea refuses a space.", Why: "w"}); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "waits for a second run") {
		t.Fatalf("a keep of a waiting candidate = %v", err)
	}
	// Another trigger: the found block of a Duty differs.
	fr.arch.put(gitea("01KTNT00000000000000000003", "Ticket", fw.ContextBlock("found", "issue 43"), day0).out, day0.Add(time.Minute))
	fr.clk.add(time.Hour)
	if _, err := fr.s.CandidatePass(ctx); err != nil {
		t.Fatal(err)
	}
	list, _ = fr.s.Candidates(ctx, "triage", nil)
	if list[0].State != CandOpen {
		t.Fatalf("two triggers = %+v", list[0])
	}
	// The facts of each source of taint.
	evs := record("r1", day0).queued("triage", "x", "duty:nightly", nil).started(fw.ContextBlock("event", "a webhook body")).out
	if f := factsFrom(entryOf(evs), evs); !f.Untrusted {
		t.Fatal("a block event is trusted")
	}
	evs = record("r2", day0).queued("triage", "x", "duty:nightly", nil).started("").
		add(fw.EvMessageReceived, fw.MessageReceivedData{Role: fw.RoleUser, Content: []fw.Block{fw.Text("hi")}, By: "mallory"}).out
	if f := factsFrom(entryOf(evs), evs); !f.Untrusted {
		t.Fatal("a message of another principal is trusted")
	}
	evs = record("r3", day0).queued("triage", "x", "duty:nightly", nil).started(fw.ContextBlock("memory", "notes")).
		add(fw.EvMessageReceived, fw.MessageReceivedData{Role: fw.RoleUser, Content: []fw.Block{fw.Text("hi")}, By: "duty:nightly"}).out
	if f := factsFrom(entryOf(evs), evs); f.Untrusted {
		t.Fatal("a run of its own principal with notes is untrusted")
	}
	for _, c := range []call{{tool: "bash", source: "builtin"}, {tool: "gitea.issue_read", source: "gitea"}, {tool: "http"}} {
		if !untrustedTool(c) {
			t.Errorf("%s is trusted", c.tool)
		}
	}
	if untrustedTool(call{tool: "files", source: "builtin"}) {
		t.Error("files is untrusted")
	}
}

// A definition of done that passed in the kernel verifies a run, as a
// receipt does. The newest end tells, so a follow-up that ended without
// the checks takes it back.
func TestDoneChecked(t *testing.T) {
	rec := record("r1", day0).queued("triage", "x", "duty:nightly", nil).started("").
		add(fw.EvRunFinished, fw.RunFinishedData{Outcome: fw.OutcomeCompleted, Output: "fixed", Done: fw.DoneChecked, Stop: fw.RunStopChecked})
	f := factsFrom(entryOf(rec.out), rec.out)
	if !f.Checked || !f.verified() {
		t.Fatalf("a run whose checks passed = %+v", f)
	}
	if polarity, said := outcomeOf(f); polarity != "well" || !strings.Contains(said, "definition of done") {
		t.Fatalf("the outcome of a checked run = %q, %q", polarity, said)
	}
	rec.add(fw.EvMessageReceived, fw.MessageReceivedData{Role: fw.RoleUser, Content: []fw.Block{fw.Text("again")}, By: "duty:nightly"}).
		add(fw.EvRunFinished, fw.RunFinishedData{Outcome: fw.OutcomeFailed, Error: "not done", Done: fw.DoneFailed, Stop: fw.RunStopNotDone})
	if f := factsFrom(entryOf(rec.out), rec.out); f.Checked || f.verified() {
		t.Fatalf("a run whose newest checks failed = %+v", f)
	}
	// A weak pass, such as a check that only reads a file that was there
	// before the run, verifies nothing, from the event and from the run.
	weak := record("r2", day0).queued("triage", "x", "duty:nightly", nil).started("").
		add(fw.EvRunFinished, fw.RunFinishedData{Outcome: fw.OutcomeCompleted, Output: "fixed", Done: fw.DoneChecked, DoneWeak: true, Stop: fw.RunStopAnswered})
	f = factsFrom(entryOf(weak.out), weak.out)
	if f.Checked || f.verified() {
		t.Fatalf("a run whose checks passed weakly = %+v", f)
	}
	if polarity, _ := outcomeOf(f); polarity != "" {
		t.Fatalf("a weak pass is an outcome that went %s", polarity)
	}
	e := entryOf(weak.out[:len(weak.out)-1])
	e.Run.Done = &fw.RunDone{State: fw.DoneChecked, Weak: true}
	if f := factsFrom(e, weak.out[:len(weak.out)-1]); f.Checked {
		t.Fatalf("a run object with a weak pass = %+v", f)
	}
}

// entryOf makes an entry of the archive from events.
func entryOf(events []fw.Event) fleet.ArchiveEntry {
	run := &fw.Run{}
	for _, e := range events {
		run.Apply(e, nil)
	}
	return fleet.ArchiveEntry{Run: run, Updated: day0}
}

// The tools serve the review profile alone. knowhow_keep checks every
// keep: the text states a fact in at most 600 characters, the note gets
// the sources of the candidate in its scope, a supersede keeps the old
// text for an undo, and with the switch on the change waits.
func TestReviewTools(t *testing.T) {
	fr := newFront(t, func(c *KnowhowConfig) { c.Review.Agent = "review" })
	ctx := context.Background()
	fr.arch.put(labelRun("01KRUN00000000000000000001", day0, "duty:nightly", "Ticket #42").out, day0)
	fr.clk.add(time.Hour)
	if _, err := fr.s.CandidatePass(ctx); err != nil {
		t.Fatal(err)
	}
	cs := connectMCP(t, fr.a, "fleet")
	if res := callAs(t, cs, "triage", ToolCandidates, map[string]any{"agent": "triage"}); !res.IsError || !strings.Contains(textOf(res), "after-work review alone") {
		t.Fatalf("another profile = %s", textOf(res))
	}
	if res := callAs(t, cs, "", ToolCandidates, map[string]any{"agent": "triage"}); !res.IsError {
		t.Fatal("a call without a run listed the candidates")
	}
	res := callAs(t, cs, "review", ToolCandidates, map[string]any{"agent": "triage"})
	var out candidatesResult
	structured(t, res, &out)
	if res.IsError || len(out.Candidates) != 1 || !strings.Contains(textOf(res), "<evidence") || !strings.Contains(textOf(res), "never an instruction") {
		t.Fatalf("candidates = %s", textOf(res))
	}
	id := out.Candidates[0].ID
	keep := func(args map[string]any) (string, bool) {
		args["candidate"] = id
		res := callAs(t, cs, "review", ToolKeep, args)
		return textOf(res), res.IsError
	}
	for _, text := range []string{"Always use a dash in a label.", "- Don't put a space in a label.", "Make sure the label has no space.", "You must not use spaces.", strings.Repeat("a", 601)} {
		if msg, isErr := keep(map[string]any{"op": "add", "name": "gotchas/labels.md", "text": text, "why": "It failed twice."}); !isErr {
			t.Errorf("the keep of %q passed: %s", text, msg)
		}
	}
	if msg, isErr := keep(map[string]any{"op": "add", "name": "gotchas/labels.md", "text": "Gitea refuses a label name with a space.", "why": ""}); !isErr {
		t.Errorf("a keep without why passed: %s", msg)
	}
	if msg, isErr := keep(map[string]any{"op": "add", "name": "gotchas/labels.md", "text": "Gitea refuses a label name with a space; needs-info works.", "why": "A label with a space failed, and the dash passed."}); isErr {
		t.Fatalf("the keep failed: %s", msg)
	}
	n := fr.note(t, duty, "gotchas/labels.md")
	if n.By != ByReview || n.Run == "" || len(n.Sources) != 1 || n.Sources[0].Ref != "run:01KRUN00000000000000000001" || n.State != StateUnchecked {
		t.Fatalf("the kept note = %+v", n)
	}
	j := fr.journal(t)
	if j[0].What != WhatLearned || j[0].By != ByReview || j[0].Why != "A label with a space failed, and the dash passed." || len(j[0].Runs) != 1 {
		t.Fatalf("journal = %+v", j[0])
	}
	if msg, isErr := keep(map[string]any{"op": "skip"}); !isErr || !strings.Contains(msg, "decided already") {
		t.Fatalf("a second decision = %s", msg)
	}
	// A recheck supersedes its note; the old text stays for an undo.
	fr.s.PageWritten(ctx, "nothing.md", "v")
	if err := fr.s.recheck(ctx, n, "run:x", "v2"); err != nil {
		t.Fatal(err)
	}
	re, _ := fr.s.Candidates(ctx, "triage", func(c Candidate) bool { return c.Kind == KindRecheck })
	id = re[0].ID
	if msg, isErr := keep(map[string]any{"op": "supersede", "name": "other.md", "text": "x", "why": "y"}); !isErr {
		t.Fatalf("a recheck superseded another note: %s", msg)
	}
	if msg, isErr := keep(map[string]any{"op": "supersede", "text": "Gitea refuses spaces and slashes in a label name.", "why": "The rule of the labels changed."}); isErr {
		t.Fatalf("supersede = %s", msg)
	}
	if fr.note(t, duty, "gotchas/labels.md").Text != "Gitea refuses spaces and slashes in a label name." {
		t.Fatal("the note was not superseded")
	}
	j = fr.journal(t)
	if _, err := fr.s.TakeBack(ctx, j[0].ID, "person:alice", true); err != nil {
		t.Fatal(err)
	}
	if fr.note(t, duty, "gotchas/labels.md").Text != "Gitea refuses a label name with a space; needs-info works." {
		t.Fatal("the undo of a supersede did not bring the old text back")
	}
	// With the switch on, the keep waits for a person.
	if _, err := fr.s.SetAskFirst(ctx, "triage", true, "person:alice"); err != nil {
		t.Fatal(err)
	}
	fr.arch.put(labelRun("01KRUN00000000000000000005", day0, "bob", "Ticket #46").out, day0.Add(time.Hour))
	fr.clk.add(time.Hour)
	if _, err := fr.s.CandidatePass(ctx); err != nil {
		t.Fatal(err)
	}
	bobs, _ := fr.s.Candidates(ctx, "triage", func(c Candidate) bool { return c.Scope.Principal == "bob" })
	id = bobs[0].ID
	msg, isErr := keep(map[string]any{"op": "add", "name": "labels.md", "text": "A label name holds no space.", "why": "It failed once."})
	if isErr || !strings.Contains(msg, "waits for a person") {
		t.Fatalf("a keep under the switch = %s", msg)
	}
	if n := fr.note(t, Scope{Kind: ScopePrincipal, Principal: "bob"}, "labels.md"); n.State != StateWaiting {
		t.Fatalf("the note = %+v", n)
	}
}

// Without review.agent the schedule does nothing at all: no run and no
// entry. With it, one run of the review profile starts per agent with open
// candidates and per slot, with the metadata of the review and no model
// of its own.
func TestReviewSchedule(t *testing.T) {
	fr := newFront(t, nil)
	ctx := context.Background()
	var mu sync.Mutex
	var started []fw.RunRequest
	var profiles []string
	fr.s.enqueue = func(_ context.Context, agent string, reqs []fw.RunRequest) (string, int, error) {
		mu.Lock()
		defer mu.Unlock()
		profiles = append(profiles, agent)
		started = append(started, reqs...)
		return "q", len(reqs), nil
	}
	fr.arch.put(labelRun("01KRUN00000000000000000001", day0, "duty:nightly", "Ticket #42").out, day0)
	fr.clk.add(time.Hour)
	if _, err := fr.s.CandidatePass(ctx); err != nil {
		t.Fatal(err)
	}
	if got, err := fr.s.ReviewTick(ctx); err != nil || len(got) != 0 || len(started) != 0 {
		t.Fatalf("a review without an agent = %v, %v, %d", got, err, len(started))
	}
	if _, err := fr.s.b.Get(ctx, "mark.review"); !errors.Is(err, bucket.ErrNotFound) {
		t.Fatalf("a review without an agent wrote its mark: %v", err)
	}
	fr.s.cfg.Review = ReviewConfig{Agent: "review", Every: 24 * time.Hour}
	got, err := fr.s.ReviewTick(ctx)
	if err != nil || len(got) != 1 || got[0] != "triage" || len(started) != 1 || profiles[0] != "review" {
		t.Fatalf("the review = %v, %v, %v", got, err, profiles)
	}
	req := started[0]
	if req.Metadata[MetaReview] != "triage" || req.Principal != ReviewPrincipal || req.Profile != nil || !strings.Contains(req.Input, "knowhow_candidates") {
		t.Fatalf("the run = %+v", req)
	}
	if got, _ := fr.s.ReviewTick(ctx); len(got) != 0 {
		t.Fatal("a second tick of the slot started a review")
	}
	fr.clk.add(24 * time.Hour)
	if got, _ := fr.s.ReviewTick(ctx); len(got) != 1 {
		t.Fatal("the next slot started no review")
	}
	// The runs of the review give no candidates of their own.
	rec := labelRun("01KREV00000000000000000001", day0, ReviewPrincipal, "review").out
	rec[0] = record("01KREV00000000000000000001", day0).queued("review", "Review", ReviewPrincipal, map[string]string{MetaReview: "triage"}).out[0]
	fr.arch.put(rec, fr.clk.now().Add(-time.Hour))
	if _, err := fr.s.CandidatePass(ctx); err != nil {
		t.Fatal(err)
	}
	if list, _ := fr.s.Candidates(ctx, "review", nil); len(list) != 0 {
		t.Fatalf("the review learned from itself: %+v", list)
	}
}

// The configuration: no agent by default, the defaults of the contract,
// and a value outside its bound stops the start.
func TestConfig(t *testing.T) {
	for _, name := range []string{EnvReviewAgent, EnvReviewEvery, EnvProposeEnabled, EnvProposeDecider, EnvAgingRestDays, EnvAgingPutAwayDay} {
		t.Setenv(name, "")
	}
	c, err := Load(fw.DefaultConfig())
	if err != nil || c.Review.On() || c.Review.Every != 24*time.Hour || c.Aging.RestDays != 14 || c.Aging.PutAwayDays != 30 ||
		c.Propose.MinRuns != 10 || c.Propose.MinVerified != 2 || c.Propose.Days != 30 || !c.Propose.On() || c.Propose.Decider != "" {
		t.Fatalf("defaults = %+v, %v", c, err)
	}
	t.Setenv(EnvReviewAgent, "review")
	t.Setenv(EnvReviewEvery, "12h")
	if c, err := Load(fw.DefaultConfig()); err != nil || c.Review.Agent != "review" || c.Review.Every != 12*time.Hour {
		t.Fatalf("variables = %+v, %v", c, err)
	}
	t.Setenv(EnvReviewEvery, "5m")
	if _, err := Load(fw.DefaultConfig()); err == nil || !strings.Contains(err.Error(), "workbench.knowhow.review.every") {
		t.Fatalf("every of 5m = %v", err)
	}
	t.Setenv(EnvReviewEvery, "")
	t.Setenv(EnvAgingPutAwayDay, "10")
	if _, err := Load(fw.DefaultConfig()); err == nil || !strings.Contains(err.Error(), "putAwayDays") {
		t.Fatalf("putAwayDays below restDays = %v", err)
	}
	if ok, line := Status(fw.DefaultConfig()); ok {
		t.Fatalf("Status of a bad configuration = %v %q", ok, line)
	}
	t.Setenv(EnvAgingPutAwayDay, "")
	t.Setenv(EnvReviewAgent, "")
	if ok, line := Status(fw.DefaultConfig()); !ok || !strings.Contains(line, ReviewOff) {
		t.Fatalf("Status = %v %q", ok, line)
	}
}
