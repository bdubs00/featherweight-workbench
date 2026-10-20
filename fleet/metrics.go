package fleet

// This file adds the series of the fleet to the metrics of the front
// (docs/ARCHITECTURE.md section 4.14): the depth and the dead letters of
// each intake queue, the ends of the runs of the workers by outcome and by
// budget limit, and the approvals that wait on the workers. The kernel of
// a worker exports facts about its own runs; the front shows the fleet as
// the egress stream and the server of the bus tell it, so an alert needs
// no scrape of a Task, and a scrape of the front wakes nobody. The series
// reach Prometheus through /metrics and a collector through the OTLP push
// of the front, like every series of its registry.

import (
	"strings"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/surface/metrics"
	"github.com/prometheus/client_golang/prometheus"
)

func init() {
	metrics.RegisterCollector(NewCollector)
}

// maxEndSeries bounds the agent and member pairs of each run counter. A
// process that can publish on the egress subject can name any agent, so a
// pair beyond the bound counts under the agent and the member "other".
const maxEndSeries = 1024

// budgetPrefix starts the error of a run that a budget limit stopped.
const budgetPrefix = "budget exceeded: "

// budgetLimits are the values of the label limit: the budget fields that
// the kernel names when a limit stops a run.
var budgetLimits = map[string]bool{
	"maxTurns": true, "maxTokens": true, "maxCostUSD": true,
	"maxToolCalls": true, "deadline": true, "maxRepeats": true,
}

// endKey and stopKey are the labels of one series of the run counters.
type (
	endKey  struct{ agent, member, outcome string }
	stopKey struct{ agent, member, limit string }
)

// runEnds counts the ends of runs that the stream carried since the front
// started. The platform holds it under its lock.
type runEnds struct {
	finished map[endKey]uint64
	stops    map[stopKey]uint64
}

func newRunEnds() runEnds {
	return runEnds{finished: map[endKey]uint64{}, stops: map[stopKey]uint64{}}
}

// count counts one fw.run.finished of a run of agent on member: its
// outcome, and the limit when a budget stopped the run.
func (e *runEnds) count(agent, member string, ev fw.Event) {
	var d fw.RunFinishedData
	_ = ev.Decode(&d)
	outcome := d.Outcome
	switch outcome {
	case fw.OutcomeCompleted, fw.OutcomeFailed, fw.OutcomeCanceled:
	default:
		outcome = "other"
	}
	k := endKey{agent, member, outcome}
	if _, ok := e.finished[k]; !ok && len(e.finished) >= maxEndSeries {
		k.agent, k.member = "other", "other"
	}
	e.finished[k]++
	limit, ok := budgetLimit(d)
	if !ok {
		return
	}
	sk := stopKey{agent, member, limit}
	if _, ok := e.stops[sk]; !ok && len(e.stops) >= maxEndSeries {
		sk.agent, sk.member = "other", "other"
	}
	e.stops[sk]++
}

// budgetLimit returns the budget limit that stopped a run, from the error
// that the kernel writes: "budget exceeded: <limit>", then a detail for a
// limit of a run tree or a loop. A limit that this version does not know
// is "other".
func budgetLimit(d fw.RunFinishedData) (string, bool) {
	rest, ok := strings.CutPrefix(d.Error, budgetPrefix)
	if d.Outcome != fw.OutcomeFailed || !ok {
		return "", false
	}
	limit, _, _ := strings.Cut(strings.TrimSpace(rest), " ")
	if !budgetLimits[limit] {
		limit = "other"
	}
	return limit, true
}

// agentOf names the agent of a run for a label: the agent of the run,
// else the agent in the source of the event, "fw://<atespace>/<agent>/<run>".
func agentOf(r *fw.Run, e fw.Event) string {
	if r.Agent != "" {
		return r.Agent
	}
	if rest, ok := strings.CutPrefix(e.Source, "fw://"); ok {
		if parts := strings.Split(rest, "/"); len(parts) >= 3 {
			return strings.Join(parts[1:len(parts)-1], "/")
		}
	}
	return "unknown"
}

// Collector reports the fleet of an agent at scrape time, from the state
// that the front already holds: a scrape sends no request to a Task or to
// the server of the bus. Without a fleet, it reports nothing.
//
// The labels hold names that the deployment bounds: queue, agent, and
// member, and the fixed lists of outcome and limit. No label holds a run,
// a person, or a batch.
type Collector struct {
	a *fw.Agent

	pending, ackPending *prometheus.Desc
	dead, deadAt        *prometheus.Desc
	finished, stops     *prometheus.Desc
	approvals, oldest   *prometheus.Desc
}

// NewCollector returns the fleet collector of a (metrics.CollectorCtor). It
// reads the fleet of a at each scrape, so it works when the fleet starts
// after the registry.
func NewCollector(a *fw.Agent) prometheus.Collector {
	return &Collector{
		a: a,
		pending: prometheus.NewDesc("fw_fleet_queue_pending",
			"Jobs on the intake queue that no worker took yet, from the consumer of the queue on the bus server.",
			[]string{"queue"}, nil),
		ackPending: prometheus.NewDesc("fw_fleet_queue_ack_pending",
			"Jobs of the intake queue that a worker took and has not acknowledged yet: their runs work or wait.",
			[]string{"queue"}, nil),
		dead: prometheus.NewDesc("fw_fleet_dead_letters_total",
			"Dead-letter records of the intake queue on <queue>.dead: the records that the stream holds, or, when the stream does not store them, those that this front received since it started.",
			[]string{"queue"}, nil),
		deadAt: prometheus.NewDesc("fw_fleet_dead_letter_last_timestamp_seconds",
			"Unix time of the last dead-letter record of the intake queue. A queue without one has no series.",
			[]string{"queue"}, nil),
		finished: prometheus.NewDesc("fw_fleet_runs_finished_total",
			"Runs of the workers that finished, by agent, member, and outcome (completed, failed, canceled), from fw.run.finished on the egress stream since this front started.",
			[]string{"agent", "member", "outcome"}, nil),
		stops: prometheus.NewDesc("fw_fleet_runs_budget_exceeded_total",
			"Runs of the workers that a budget limit stopped, by agent, member, and limit (maxTurns, maxTokens, maxCostUSD, maxToolCalls, deadline, maxRepeats, other), from fw.run.finished on the egress stream. Each also counts as failed.",
			[]string{"agent", "member", "limit"}, nil),
		approvals: prometheus.NewDesc("fw_fleet_approvals_pending",
			"Runs of the workers that wait for an approval or an answer of a person, by agent.",
			[]string{"agent"}, nil),
		oldest: prometheus.NewDesc("fw_fleet_approvals_oldest_timestamp_seconds",
			"Unix time of the request of the oldest approval that waits on the workers, by agent. An agent without such an approval has no series.",
			[]string{"agent"}, nil),
	}
}

// Describe implements prometheus.Collector.
func (c *Collector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{c.pending, c.ackPending, c.dead, c.deadAt, c.finished, c.stops, c.approvals, c.oldest} {
		ch <- d
	}
}

// Collect implements prometheus.Collector.
func (c *Collector) Collect(ch chan<- prometheus.Metric) {
	f, ok := Of(c.a).(*Fleet)
	if !ok || f == nil {
		return
	}
	states, _ := f.queueStates()
	for name, st := range states {
		if st.depthKnown {
			ch <- prometheus.MustNewConstMetric(c.pending, prometheus.GaugeValue, float64(st.pending), name)
			ch <- prometheus.MustNewConstMetric(c.ackPending, prometheus.GaugeValue, float64(st.ackPending), name)
		}
		if st.deadKnown {
			ch <- prometheus.MustNewConstMetric(c.dead, prometheus.CounterValue, float64(st.dead), name)
			if !st.deadAt.IsZero() {
				ch <- prometheus.MustNewConstMetric(c.deadAt, prometheus.GaugeValue, unixSeconds(st.deadAt), name)
			}
		}
	}
	p := f.platform
	if p == nil {
		return
	}
	type waiting struct {
		n      int
		oldest time.Time
	}
	p.mu.Lock()
	finished := make(map[endKey]uint64, len(p.ends.finished))
	for k, v := range p.ends.finished {
		finished[k] = v
	}
	stops := make(map[stopKey]uint64, len(p.ends.stops))
	for k, v := range p.ends.stops {
		stops[k] = v
	}
	approvals := map[string]*waiting{}
	for _, m := range p.members {
		for ag := range m.agents {
			if approvals[ag] == nil {
				approvals[ag] = &waiting{}
			}
		}
	}
	for _, r := range p.runs {
		if !r.queued || r.run.State != fw.StateInputRequired || r.run.Pending == nil || r.run.Agent == "" {
			continue
		}
		ag := r.run.Agent
		if approvals[ag] == nil && len(approvals) >= maxEndSeries {
			ag = "other"
		}
		w := approvals[ag]
		if w == nil {
			w = &waiting{}
			approvals[ag] = w
		}
		w.n++
		if at := r.run.Pending.RequestedAt; !at.IsZero() && (w.oldest.IsZero() || at.Before(w.oldest)) {
			w.oldest = at
		}
	}
	p.mu.Unlock()
	for k, v := range finished {
		ch <- prometheus.MustNewConstMetric(c.finished, prometheus.CounterValue, float64(v), k.agent, k.member, k.outcome)
	}
	for k, v := range stops {
		ch <- prometheus.MustNewConstMetric(c.stops, prometheus.CounterValue, float64(v), k.agent, k.member, k.limit)
	}
	for ag, w := range approvals {
		ch <- prometheus.MustNewConstMetric(c.approvals, prometheus.GaugeValue, float64(w.n), ag)
		if !w.oldest.IsZero() {
			ch <- prometheus.MustNewConstMetric(c.oldest, prometheus.GaugeValue, unixSeconds(w.oldest), ag)
		}
	}
}

// unixSeconds is a time as a Unix timestamp in seconds, as a gauge holds it.
func unixSeconds(t time.Time) float64 { return float64(t.UnixNano()) / 1e9 }
