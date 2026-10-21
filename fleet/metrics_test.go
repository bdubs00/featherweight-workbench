package fleet

import (
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/prometheus/client_golang/prometheus"
)

// gather collects the series of a registry as "name{label=value,...}" to
// their values, the labels in the order of their names.
func gather(t *testing.T, reg *prometheus.Registry) map[string]float64 {
	t.Helper()
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]float64{}
	for _, mf := range mfs {
		for _, m := range mf.GetMetric() {
			var labels []string
			for _, l := range m.GetLabel() {
				labels = append(labels, l.GetName()+"="+l.GetValue())
			}
			sort.Strings(labels)
			v := m.GetGauge().GetValue()
			if m.GetCounter() != nil {
				v = m.GetCounter().GetValue()
			}
			out[mf.GetName()+"{"+strings.Join(labels, ",")+"}"] = v
		}
	}
	return out
}

// The front exports the depth and the dead letters of each queue, the ends
// of the runs of the workers by outcome and by budget limit, and the
// approvals that wait, from the stream and the server of the bus. A
// repeated end counts once, and no label holds a run or a person.
func TestFleetMetrics(t *testing.T) {
	t.Setenv("FW_ATESPACE", "")
	deadAt := time.Date(2026, 10, 19, 3, 4, 5, 0, time.UTC)
	q := &fakeQueues{
		filters: []string{"fw.>"},
		stored:  map[string]uint64{"fw.jobs.default.dead": 1},
		last:    map[string]time.Time{"fw.jobs.default.dead": deadAt},
		pending: func(int) map[string]uint64 { return map[string]uint64{"fw.jobs.default": 7} },
		acked:   map[string]uint64{"fw.jobs.default": 2},
	}
	withBusServer(t, q, nil)
	front := newMember(t, fw.NewMemBus(), "front", func(_ *fw.Config, fc *Config) { fc.Intake = "fw.jobs" })
	f := front.f
	queued := fw.MustEvent("", fw.EvRunQueued, fw.RunQueuedData{Agent: "default", Input: "x", Origin: fw.OriginBus, Principal: "ada"})
	started := fw.MustEvent("", fw.EvRunStarted, fw.RunStartedData{Model: "fake/m"})
	end := func(d fw.RunFinishedData) fw.Event { return fw.MustEvent("", fw.EvRunFinished, d) }
	seqs := []uint64{1, 2, 3}
	done := end(fw.RunFinishedData{Outcome: fw.OutcomeCompleted, Output: "ok"})
	stream(t, f, "01DONE", seqs, queued, started, done)
	stream(t, f, "01DONE", []uint64{3}, done) // the same end again
	stream(t, f, "01LATE", seqs, queued, started, end(fw.RunFinishedData{Outcome: fw.OutcomeFailed, Error: "budget exceeded: deadline"}))
	stream(t, f, "01LOOP", seqs, queued, started, end(fw.RunFinishedData{Outcome: fw.OutcomeFailed, Error: "budget exceeded: maxRepeats (the model made the same bash call 5 times in a row)"}))
	stream(t, f, "01BOOM", seqs, queued, started, end(fw.RunFinishedData{Outcome: fw.OutcomeFailed, Error: "model: boom"}))
	stream(t, f, "01STOP", seqs, queued, started, end(fw.RunFinishedData{Outcome: fw.OutcomeCanceled}))
	asked := fw.MustEvent("", fw.EvApprovalRequested, fw.ApprovalRequestedData{ApprovalID: "ap1", Tool: "bash", Reason: "needs ok"})
	asked.Time = time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	stream(t, f, "01WAIT", seqs, queued, started, asked)
	stream(t, f, "01WAIT2", seqs, queued, started, fw.MustEvent("", fw.EvApprovalRequested, fw.ApprovalRequestedData{ApprovalID: "ap2", Tool: "bash"}))

	reg := prometheus.NewRegistry()
	reg.MustRegister(NewCollector(front.a))
	eventually(t, "the front reads the queues and their dead letters", func() bool {
		states, _ := f.queueStates()
		st := states["fw.jobs.default"]
		return st.depthKnown && st.dead == 1 && st.deadAt.Equal(deadAt)
	})
	got := gather(t, reg)
	want := map[string]float64{
		`fw_fleet_queue_pending{queue=fw.jobs.default}`:                                       7,
		`fw_fleet_queue_ack_pending{queue=fw.jobs.default}`:                                   2,
		`fw_fleet_dead_letters_total{queue=fw.jobs.default}`:                                  1,
		`fw_fleet_dead_letter_last_timestamp_seconds{queue=fw.jobs.default}`:                  float64(deadAt.Unix()),
		`fw_fleet_runs_finished_total{agent=default,member=box:8080,outcome=completed}`:       1,
		`fw_fleet_runs_finished_total{agent=default,member=box:8080,outcome=failed}`:          3,
		`fw_fleet_runs_finished_total{agent=default,member=box:8080,outcome=canceled}`:        1,
		`fw_fleet_runs_budget_exceeded_total{agent=default,limit=deadline,member=box:8080}`:   1,
		`fw_fleet_runs_budget_exceeded_total{agent=default,limit=maxRepeats,member=box:8080}`: 1,
		`fw_fleet_approvals_pending{agent=default}`:                                           2,
		`fw_fleet_approvals_oldest_timestamp_seconds{agent=default}`:                          float64(asked.Time.Unix()),
		`fw_fleet_members_blocked{}`:                                                          0,
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %v, want %v", k, got[k], v)
		}
	}
	for k := range got {
		if _, ok := want[k]; !ok {
			t.Errorf("an extra series: %s = %v", k, got[k])
		}
		for _, secret := range []string{"ada", "01DONE", "01WAIT", "boom"} {
			if strings.Contains(k, secret) {
				t.Errorf("the series %s names %q", k, secret)
			}
		}
	}
}

// budgetLimit reads the limit from the error that the kernel writes when a
// budget stops a run, and names a limit that it does not know "other".
func TestBudgetLimit(t *testing.T) {
	for _, c := range []struct {
		d     fw.RunFinishedData
		limit string
		ok    bool
	}{
		{fw.RunFinishedData{Outcome: fw.OutcomeFailed, Error: "budget exceeded: maxTurns"}, "maxTurns", true},
		{fw.RunFinishedData{Outcome: fw.OutcomeFailed, Error: "budget exceeded: maxCostUSD of the run tree (this run and the runs under it used $2 of $1)"}, "maxCostUSD", true},
		{fw.RunFinishedData{Outcome: fw.OutcomeFailed, Error: "budget exceeded: wallclock"}, "other", true},
		{fw.RunFinishedData{Outcome: fw.OutcomeFailed, Error: "model: budget exceeded: deadline"}, "", false},
		{fw.RunFinishedData{Outcome: fw.OutcomeCompleted, Error: "budget exceeded: deadline"}, "", false},
	} {
		if limit, ok := budgetLimit(c.d); limit != c.limit || ok != c.ok {
			t.Errorf("budgetLimit(%q) = %q, %v; want %q, %v", c.d.Error, limit, ok, c.limit, c.ok)
		}
	}
}
