package fleet

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// The lists leave out a check run of the kernel and a run that the front
// starts for itself, and keep every other run in its order.
func TestListed(t *testing.T) {
	check := &fw.Run{ID: "c", Metadata: map[string]string{fw.MetaKind: fw.RunKindCheck}}
	own := &fw.Run{ID: "w", Metadata: map[string]string{MetaInternal: "duty-write-check:report"}}
	plain := &fw.Run{ID: "p"}
	other := &fw.Run{ID: "o", Metadata: map[string]string{fw.MetaBatch: "duty:report"}}
	if Listed(check) || Listed(own) || Listed(nil) || !Listed(plain) || !Listed(other) {
		t.Fatal("Listed decides wrong")
	}
	all := []*fw.Run{plain, check, other, own}
	if got := runIDs(ListedOnly(all)); fmt.Sprint(got) != "[p o]" {
		t.Fatalf("ListedOnly = %v", got)
	}
	if got := runIDs(ListedOnly([]*fw.Run{other, plain})); fmt.Sprint(got) != "[o p]" {
		t.Fatalf("ListedOnly of listed runs = %v", got)
	}
}

// A check run goes on a queue with an empty input, as the kernel takes it,
// and any other run with an empty input does not.
func TestEnqueueACheckRun(t *testing.T) {
	t.Setenv("FW_ATESPACE", "")
	t.Setenv(EnvIntake, "fw.jobs")
	withBusServer(t, nil, newMemJobs())
	bus := fw.NewMemBus()
	m := newMember(t, bus, "front", func(_ *fw.Config, fc *Config) { fc.Intake = "fw.jobs" })
	got := make(chan fw.RunRequest, 4)
	stop, err := bus.Subscribe(context.Background(), "fw.jobs.>", func(e fw.Event) error {
		var req fw.RunRequest
		if e.Decode(&req) == nil {
			got <- req
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stop)
	check := &fw.Check{Steps: []fw.CheckStep{{Tool: "tickets.list"}}}
	if _, sent, err := EnqueueFor(context.Background(), m.a, "default", "", []fw.RunRequest{{Check: check}}); err != nil || sent != 1 {
		t.Fatalf("a check run = %d, %v", sent, err)
	}
	if req := <-got; req.Check == nil || req.Check.Steps[0].Tool != "tickets.list" || req.Input != "" {
		t.Fatalf("the request = %+v", req)
	}
	if _, _, err := EnqueueFor(context.Background(), m.a, "default", "", []fw.RunRequest{{Input: " "}}); !errors.Is(err, fw.ErrInvalidRequest) {
		t.Fatalf("an empty run = %v", err)
	}
}

// A record keeps the newest finished runs that the lists show apart from
// the newest finished check runs, so many checks never push the work of
// people out of the view.
func TestPickRunsKeepsCheckRunsApart(t *testing.T) {
	now := time.Now()
	var runs []*fw.Run
	for i := 0; i < 30; i++ {
		runs = append(runs, &fw.Run{ID: fmt.Sprintf("c%02d", i), State: fw.StateCompleted, CreatedAt: now.Add(-time.Duration(i) * time.Second),
			Metadata: map[string]string{fw.MetaKind: fw.RunKindCheck}})
	}
	for i := 0; i < 20; i++ {
		runs = append(runs, &fw.Run{ID: fmt.Sprintf("f%02d", i), State: fw.StateCompleted, CreatedAt: now.Add(-time.Hour - time.Duration(i)*time.Minute)})
	}
	kept, omitted := pickRuns(runs)
	listed, hidden := 0, 0
	for _, r := range kept {
		if Listed(r) {
			listed++
		} else {
			hidden++
		}
	}
	if listed != 20 || hidden != HiddenFinishedRuns || omitted != 30-HiddenFinishedRuns {
		t.Fatalf("kept %d listed and %d hidden, omitted %d", listed, hidden, omitted)
	}
}

// Await reads a run of another member from the view, and once it settled
// it reads the whole run from the member with the static token of the
// fleet and no person. A run that waits for a person settles too.
// CancelRun cancels a run of another member with the same token.
func TestAwaitAndCancelARunOfAMember(t *testing.T) {
	pf := newPersonFront(t)
	ctx, cancel := context.WithTimeout(as("alice"), 5*time.Second)
	defer cancel()
	r, err := Await(ctx, pf.m.a, "r4")
	if err != nil || r.ID != "r4" || r.State != fw.StateCompleted || r.Member != "team/rev" {
		t.Fatalf("Await(r4) = %+v, %v", r, err)
	}
	if r, err := Await(ctx, pf.m.a, "r1"); err != nil || r.State != fw.StateInputRequired {
		t.Fatalf("Await(r1) = %+v, %v", r, err)
	}
	if err := CancelRun(ctx, pf.m.a, "r4"); err != nil {
		t.Fatalf("CancelRun = %v", err)
	}
	pf.mu.Lock()
	calls := append([]memberCall(nil), pf.calls...)
	pf.mu.Unlock()
	if len(calls) != 3 {
		t.Fatalf("calls = %+v", calls)
	}
	for _, c := range calls {
		if c.auth != "Bearer fleet-secret" || c.person != "" {
			t.Fatalf("a call of the server for itself = %+v", c)
		}
	}
	if calls[0].path != "/v1/runs/r4" || calls[2].method != "POST" || calls[2].path != "/v1/runs/r4/cancel" {
		t.Fatalf("calls = %+v", calls)
	}
}

// Await follows a run of this process to its end, and a run that no
// process holds waits until the context ends. CancelRun of such a run is
// fw.ErrRunNotFound.
func TestAwaitALocalRun(t *testing.T) {
	m := newMember(t, fw.NewMemBus(), "front", nil)
	r, err := m.a.Start(context.Background(), "Say done")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	got, err := Await(ctx, m.a, r.ID)
	if err != nil || got.State != fw.StateCompleted || got.Output != "done" {
		t.Fatalf("Await = %+v, %v", got, err)
	}
	short, stop := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer stop()
	if _, err := Await(short, m.a, "01NOBODY"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Await of a run that nobody holds = %v", err)
	}
	if err := CancelRun(context.Background(), m.a, "01NOBODY"); !errors.Is(err, fw.ErrRunNotFound) {
		t.Fatalf("CancelRun of a run that nobody holds = %v", err)
	}
}

// A check run that works keeps no member busy in the view.
func TestMembersCountNoCheckRun(t *testing.T) {
	m := newMember(t, fw.NewMemBus(), "front", nil)
	now := time.Now()
	inject(m.f, "team/w", "ax:team/w", "team", MemberLive, now,
		&fw.Run{ID: "c1", Agent: "default", State: fw.StateRunning, CreatedAt: now, Metadata: map[string]string{fw.MetaKind: fw.RunKindCheck}},
		&fw.Run{ID: "r1", Agent: "default", State: fw.StateRunning, CreatedAt: now})
	members, err := m.f.Members(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, mb := range members {
		if mb.Name == "team/w" {
			found = true
			if mb.Active != 1 {
				t.Fatalf("the member counts %d busy runs, want 1", mb.Active)
			}
		}
	}
	if !found {
		t.Fatalf("the view lacks the member: %+v", members)
	}
}
