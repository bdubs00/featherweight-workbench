// Package value answers what the work of the agents gave and what it took
// over a window of days: the pieces of work that ended, the verdicts of
// people on them, the actions of people that the work needed, the time
// from the ask to the first end and the part of it that waited for people,
// and the spend. It is the view of a sponsor, who asks whether the agents
// are worth their cost. It is no console of an operator: the metrics of
// the kernel and of the fleet stay in Prometheus.
//
// The package keeps no truth of its own beside the estimates of people.
// The runs and the facts of their work come from fleet.WorkSince, which
// reads the ledgers of this process and the archive of the runs of the
// workers, with the access rules of the runs. It mounts GET /v1/value and
// PUT /v1/value/estimates on the native surface and adds the MCP tools
// value and value_estimate, which call the same Go functions.
//
// The numbers claim only what the records hold. A result that no person
// and no receipt judged is unchecked, not good. A run with tokens without
// a price makes the spend a lower bound. The front measures no time that
// a person spent on the work, so a saving needs an estimate of a person:
// the minutes that a person takes for one piece of a line of work.
package value

import (
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/duties"
	"github.com/bdubs00/featherweight-workbench/fleet"
)

// Windows are the lengths of a window in days that the route takes.
var Windows = []int{7, 30, 90}

// DefaultDays is the window of a request that names none.
const DefaultDays = 30

// Kinds of a line of work.
const (
	KindDuty   = "duty"   // the runs of one duty
	KindSource = "source" // the runs that one source of events started
	KindAgent  = "agent"  // the other runs of one agent: asks of people, seats of bridges, jobs
)

// metaSource is the metadata of a run that a source of events started
// (sources.MetaSource).
const metaSource = "workbench.source"

// metaForkOf names the run of which a run is a branch (the kernel sets it
// on a fork).
const metaForkOf = "fw.forkOf"

// Answer is the value of the work for the caller over one window.
type Answer struct {
	Days     int       `json:"days"`
	Since    time.Time `json:"since"`
	Until    time.Time `json:"until"`
	Sentence string    `json:"sentence"`
	Work     Counts    `json:"work"`
	Checks   Checks    `json:"checks"`
	People   People    `json:"people"`
	Lead     Lead      `json:"lead"`
	Spend    Spend     `json:"spend"`
	// Estimated sums the work of the lines that have an estimate of a
	// person, or is nil when no line has one.
	Estimated *Estimated `json:"estimated,omitempty"`
	Periods   []Period   `json:"periods"`
	Lines     []Line     `json:"lines"`
	// Partial is true when Unknown pieces have no facts of their work, such
	// as runs that the archive kept before it learned the facts, or runs of
	// a fleet without an archive. Their actions, verdicts, and lead times
	// count as none.
	Partial bool `json:"partial"`
	Unknown int  `json:"unknown,omitempty"`
}

// Counts counts the pieces of work by their state now. A piece is a run
// that a person, a duty, a source, or a job asked for: a run without a
// parent that is no branch, no run of the manager or of the quick chat,
// and no run that the lists leave out. Done completed, Failed failed,
// Stopped was canceled, and Open works or waits. Lost never ended because
// a pool replaced its worker while it worked or waited (fleet.WorkRun).
type Counts struct {
	Pieces  int `json:"pieces"`
	Done    int `json:"done"`
	Failed  int `json:"failed"`
	Stopped int `json:"stopped"`
	Open    int `json:"open"`
	Lost    int `json:"lost"`
}

// Checks splits the done pieces by the verdict on their result. Accepted
// and SentBack are the first verdict of a person or of a receipt. Checked
// is a piece that no person judged and whose definition of done passed in
// the kernel (fw.run.finished with done checked). Unchecked is a piece
// that nobody and nothing judged. The verdict of a person wins over the
// checks of the kernel.
type Checks struct {
	Accepted  int `json:"accepted"`
	Checked   int `json:"checked"`
	SentBack  int `json:"sentBack"`
	Unchecked int `json:"unchecked"`
}

// People is what the pieces took from people. Actions counts the
// decisions and the messages of people. NoPerson counts the done pieces
// that needed no action of a person after the ask. Reworked counts the
// done pieces that a person wrote to after their first end. WaitSeconds
// sums the time that pieces waited for a decision of a person, the waits
// open now included, and WaitingNow counts the pieces that wait now.
// Handled counts the decisions that no person made: the manager, a
// timeout, or the front.
type People struct {
	Actions     int   `json:"actions"`
	NoPerson    int   `json:"noPerson"`
	Reworked    int   `json:"reworked"`
	WaitSeconds int64 `json:"waitSeconds"`
	WaitingNow  int   `json:"waitingNow"`
	Handled     int   `json:"handled"`
}

// Lead is the time from the ask of a done piece to its first end.
// MedianSeconds is the median over the Measured pieces with the facts.
// WaitShare is the part of the sum of those times that waited for a
// decision of a person, from 0 to 1. The time that a job waited in its
// queue before a worker took it is not in it.
type Lead struct {
	Measured      int     `json:"measured"`
	MedianSeconds int64   `json:"medianSeconds"`
	WaitShare     float64 `json:"waitShare"`
}

// Spend is the cost of the runs of the window at the prices of the
// workers: the pieces with their children and branches, and the runs of
// the manager and of the quick chat. PerDone divides USD by the done
// pieces, so the cost of failed work and of coordination is in it.
// FailedUSD is the cost of the pieces that failed or stopped, and
// CoordinationUSD the cost of the manager and of the quick chat. Unpriced
// counts the runs with tokens without a price: their cost is unknown, so
// USD is a lower bound (AtLeast).
type Spend struct {
	USD             float64 `json:"usd"`
	PerDone         float64 `json:"perDone"`
	FailedUSD       float64 `json:"failedUSD"`
	CoordinationUSD float64 `json:"coordinationUSD"`
	Unpriced        int     `json:"unpriced"`
	AtLeast         bool    `json:"atLeast"`
}

// Estimate is the minutes that a person says a person takes for one piece
// of a line of work, with who said it and when.
type Estimate struct {
	Minutes int       `json:"minutes"`
	By      string    `json:"by"`
	At      time.Time `json:"at"`
}

// Estimated is the work of the lines with an estimate, in the minutes of
// a person: each estimate times the done pieces of its line that no
// person sent back. Lines counts those lines, and Pieces their pieces.
// It is an estimate of people, never a measurement of the front.
type Estimated struct {
	Minutes int64 `json:"minutes"`
	Lines   int   `json:"lines"`
	Pieces  int   `json:"pieces"`
}

// Period is one part of the window for the chart: the pieces that were
// asked for from Start to End, by their state and verdict, and their
// spend.
type Period struct {
	Start     time.Time `json:"start"`
	End       time.Time `json:"end"`
	Done      int       `json:"done"`
	Accepted  int       `json:"accepted"`
	Checked   int       `json:"checked"`
	SentBack  int       `json:"sentBack"`
	Unchecked int       `json:"unchecked"`
	Failed    int       `json:"failed"`
	USD       float64   `json:"usd"`
}

// Line is one line of work: a duty, a source, or the other runs of an
// agent. Agent is the agent of its newest piece.
type Line struct {
	Key               string    `json:"key"`
	Kind              string    `json:"kind"`
	Name              string    `json:"name"`
	Agent             string    `json:"agent"`
	Counts            Counts    `json:"work"`
	Checks            Checks    `json:"checks"`
	Actions           int       `json:"actions"`
	NoPerson          int       `json:"noPerson"`
	MedianLeadSeconds int64     `json:"medianLeadSeconds"`
	USD               float64   `json:"usd"`
	PerDone           float64   `json:"perDone"`
	Unpriced          int       `json:"unpriced"`
	Estimate          *Estimate `json:"estimate,omitempty"`
	EstimatedMinutes  int64     `json:"estimatedMinutes,omitempty"`
	newest            time.Time
	leads             []int64
}

// Options shape a fold. Coordination names the agents whose runs
// coordinate the work and are no pieces: the manager and the quick chat.
// Estimates holds the estimates of people by the key of a line.
type Options struct {
	Now          time.Time
	Days         int
	Coordination map[string]bool
	Estimates    map[string]Estimate
}

// LineOf returns the key, the kind, and the name of the line of a run.
func LineOf(r *fw.Run) (key, kind, name string) {
	if d := duties.NameOf(r); d != "" {
		return KindDuty + ":" + d, KindDuty, d
	}
	if s := r.Metadata[metaSource]; s != "" {
		return KindSource + ":" + s, KindSource, s
	}
	return KindAgent + ":" + r.Agent, KindAgent, r.Agent
}

// piece is one piece of work while the fold runs.
type piece struct {
	r    *fleet.WorkRun
	line *Line
	usd  float64
}

// Fold folds the runs of a window into the answer. The runs are those
// that were asked for in the window and that the caller can view
// (fleet.WorkSince).
func Fold(runs []fleet.WorkRun, o Options) Answer {
	if o.Days <= 0 {
		o.Days = DefaultDays
	}
	until := o.Now.UTC()
	since := until.Add(-time.Duration(o.Days) * 24 * time.Hour)
	ans := Answer{Days: o.Days, Since: since, Until: until, Periods: periods(since, until, o.Days), Lines: []Line{}}

	byID := make(map[string]*fleet.WorkRun, len(runs))
	for i := range runs {
		if runs[i].Run != nil {
			byID[runs[i].Run.ID] = &runs[i]
		}
	}
	// rootOf follows the parents and the sources of branches to the run
	// that holds the cost of a run.
	rootOf := func(r *fw.Run) *fw.Run {
		for range 64 {
			up := r.Parent
			if up == "" {
				up = r.Metadata[metaForkOf]
			}
			next, ok := byID[up]
			if up == "" || !ok {
				return r
			}
			r = next.Run
		}
		return r
	}
	lines := map[string]*Line{}
	pieces := map[string]*piece{}
	for i := range runs {
		w := &runs[i]
		r := w.Run
		if r == nil || !fleet.Listed(r) || r.Parent != "" || r.Metadata[metaForkOf] != "" || o.Coordination[r.Agent] {
			continue
		}
		key, kind, name := LineOf(r)
		ln := lines[key]
		if ln == nil {
			ln = &Line{Key: key, Kind: kind, Name: name}
			lines[key] = ln
		}
		if r.CreatedAt.After(ln.newest) || ln.Agent == "" {
			ln.newest, ln.Agent = r.CreatedAt, r.Agent
		}
		pieces[r.ID] = &piece{r: w, line: ln}
	}
	// The spend: every run counts once, under the piece that it serves, or
	// under coordination.
	for i := range runs {
		r := runs[i].Run
		if r == nil {
			continue
		}
		u := r.Usage
		if u.UnpricedTokens > 0 || (u.CostUSD == 0 && u.InputTokens+u.OutputTokens+u.CacheReadTokens+u.CacheWriteTokens > 0) {
			ans.Spend.Unpriced++
			if p := pieces[rootOf(r).ID]; p != nil {
				p.line.Unpriced++
			}
		}
		ans.Spend.USD += u.CostUSD
		root := rootOf(r)
		if p := pieces[root.ID]; p != nil {
			p.usd += u.CostUSD
			continue
		}
		if o.Coordination[root.Agent] {
			ans.Spend.CoordinationUSD += u.CostUSD
		}
	}
	var leads []int64
	var leadSum, waitSum int64
	ids := make([]string, 0, len(pieces))
	for id := range pieces {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		p := pieces[id]
		r, wk, ln := p.r.Run, p.r.Work, p.line
		ans.Work.Pieces++
		ln.Counts.Pieces++
		ln.USD += p.usd
		if !wk.Facts {
			ans.Unknown++
		}
		ans.People.Actions += wk.Acts
		ans.People.Handled += wk.Handled
		ln.Actions += wk.Acts
		wait := wk.WaitMS / 1000
		if !p.r.Lost && !wk.Asked.IsZero() && r.State == fw.StateInputRequired && until.After(wk.Asked) {
			wait += int64(until.Sub(wk.Asked) / time.Second)
			ans.People.WaitingNow++
		}
		ans.People.WaitSeconds += wait
		per := periodOf(ans.Periods, r.CreatedAt)
		if per != nil {
			per.USD += p.usd
		}
		state := r.State
		if p.r.Lost {
			state = "lost"
		}
		switch state {
		case "lost":
			ans.Work.Lost++
			ln.Counts.Lost++
			ans.Spend.FailedUSD += p.usd
		case fw.StateCompleted:
			ans.Work.Done++
			ln.Counts.Done++
			if per != nil {
				per.Done++
			}
			switch wk.Verdict {
			case fleet.VerdictAccepted:
				ans.Checks.Accepted++
				ln.Checks.Accepted++
				if per != nil {
					per.Accepted++
				}
			case fleet.VerdictRejected:
				ans.Checks.SentBack++
				ln.Checks.SentBack++
				if per != nil {
					per.SentBack++
				}
			case "":
				if doneChecked(r, wk) {
					ans.Checks.Checked++
					ln.Checks.Checked++
					if per != nil {
						per.Checked++
					}
					break
				}
				fallthrough
			default:
				ans.Checks.Unchecked++
				ln.Checks.Unchecked++
				if per != nil {
					per.Unchecked++
				}
			}
			if wk.Facts && wk.Acts == 0 {
				ans.People.NoPerson++
				ln.NoPerson++
			}
			if wk.Rework > 0 {
				ans.People.Reworked++
			}
			if wk.Facts && !wk.Queued.IsZero() && !wk.FirstEnd.IsZero() && !wk.FirstEnd.Before(wk.Queued) {
				ms := wk.FirstEnd.Sub(wk.Queued).Milliseconds()
				leads = append(leads, ms)
				ln.leads = append(ln.leads, ms)
				leadSum += ms
				waitSum += min(wk.WaitMS, ms)
			}
		case fw.StateFailed:
			ans.Work.Failed++
			ln.Counts.Failed++
			ans.Spend.FailedUSD += p.usd
			if per != nil {
				per.Failed++
			}
		case fw.StateCanceled:
			ans.Work.Stopped++
			ln.Counts.Stopped++
			ans.Spend.FailedUSD += p.usd
		default:
			ans.Work.Open++
			ln.Counts.Open++
		}
	}
	ans.Partial = ans.Unknown > 0
	ans.Lead.Measured = len(leads)
	ans.Lead.MedianSeconds = median(leads) / 1000
	if leadSum > 0 {
		ans.Lead.WaitShare = round(float64(waitSum)/float64(leadSum), 3)
	}
	ans.Spend.AtLeast = ans.Spend.Unpriced > 0
	if ans.Work.Done > 0 {
		ans.Spend.PerDone = round(ans.Spend.USD/float64(ans.Work.Done), 4)
	}
	ans.Spend.USD = round(ans.Spend.USD, 4)
	ans.Spend.FailedUSD = round(ans.Spend.FailedUSD, 4)
	ans.Spend.CoordinationUSD = round(ans.Spend.CoordinationUSD, 4)
	for i := range ans.Periods {
		ans.Periods[i].USD = round(ans.Periods[i].USD, 4)
	}
	var est Estimated
	for _, ln := range lines {
		ln.MedianLeadSeconds = median(ln.leads) / 1000
		if ln.Counts.Done > 0 {
			ln.PerDone = round(ln.USD/float64(ln.Counts.Done), 4)
		}
		ln.USD = round(ln.USD, 4)
		if e, ok := o.Estimates[ln.Key]; ok && e.Minutes > 0 {
			c := e
			ln.Estimate = &c
			n := ln.Counts.Done - ln.Checks.SentBack
			ln.EstimatedMinutes = int64(e.Minutes) * int64(n)
			est.Minutes += ln.EstimatedMinutes
			est.Lines++
			est.Pieces += n
		}
		ans.Lines = append(ans.Lines, *ln)
	}
	if est.Lines > 0 {
		ans.Estimated = &est
	}
	sort.SliceStable(ans.Lines, func(i, j int) bool {
		a, b := ans.Lines[i], ans.Lines[j]
		if a.Counts.Done != b.Counts.Done {
			return a.Counts.Done > b.Counts.Done
		}
		if a.Counts.Pieces != b.Counts.Pieces {
			return a.Counts.Pieces > b.Counts.Pieces
		}
		return a.Key < b.Key
	})
	ans.Sentence = sentence(ans)
	return ans
}

// doneChecked reports whether the definition of done of a run passed in
// the kernel: the facts of its work say so, else the run object of a newer
// kernel does.
func doneChecked(r *fw.Run, w fleet.Work) bool {
	if w.Done != "" {
		return w.Done == fw.DoneChecked
	}
	return r.Done != nil && r.Done.State == fw.DoneChecked
}

// periods returns the parts of a window: a day each for a window of a
// week or less, else a week each, the newest ending at until and the
// oldest starting at since.
func periods(since, until time.Time, days int) []Period {
	step := 7 * 24 * time.Hour
	if days <= 7 {
		step = 24 * time.Hour
	}
	var out []Period
	for end := until; end.After(since); end = end.Add(-step) {
		start := end.Add(-step)
		if start.Before(since) {
			start = since
		}
		out = append(out, Period{Start: start, End: end})
	}
	slices.Reverse(out)
	return out
}

// periodOf returns the period that holds t, or nil.
func periodOf(ps []Period, t time.Time) *Period {
	for i := range ps {
		if !t.Before(ps[i].Start) && t.Before(ps[i].End) {
			return &ps[i]
		}
	}
	if n := len(ps); n > 0 && t.Equal(ps[n-1].End) {
		return &ps[n-1]
	}
	return nil
}

// median returns the median of the values, or 0 for none.
func median(v []int64) int64 {
	if len(v) == 0 {
		return 0
	}
	s := slices.Clone(v)
	slices.Sort(s)
	n := len(s)
	if n%2 == 1 {
		return s[n/2]
	}
	return (s[n/2-1] + s[n/2]) / 2
}

// round rounds f to n digits after the point.
func round(f float64, n int) float64 {
	p := 1.0
	for range n {
		p *= 10
	}
	if f >= 0 {
		return float64(int64(f*p+0.5)) / p
	}
	return -float64(int64(-f*p+0.5)) / p
}

// sentence words the answer for a person and for a model.
func sentence(a Answer) string {
	var b strings.Builder
	fmt.Fprintf(&b, "In the last %d days, ", a.Days)
	if a.Work.Pieces == 0 {
		b.WriteString("the agents finished no work that you can view.")
		return b.String()
	}
	fmt.Fprintf(&b, "the agents finished %s", plural(a.Work.Done, "piece", "pieces")+" of work")
	if a.Work.Failed+a.Work.Stopped > 0 {
		fmt.Fprintf(&b, ", and %d failed or stopped", a.Work.Failed+a.Work.Stopped)
	}
	b.WriteString(".")
	if a.Work.Lost > 0 {
		fmt.Fprintf(&b, " %s lost when a pool replaced the worker.", plural(a.Work.Lost, "piece was", "pieces were"))
	}
	if a.Work.Done > 0 {
		fmt.Fprintf(&b, " People accepted %d and sent back %d.", a.Checks.Accepted, a.Checks.SentBack)
		if a.Checks.Checked > 0 {
			fmt.Fprintf(&b, " The checks of the agents passed for %d more.", a.Checks.Checked)
		}
		fmt.Fprintf(&b, " %d %s unchecked.", a.Checks.Unchecked, isAre(a.Checks.Unchecked))
		fmt.Fprintf(&b, " %d of the %d finished without an action of a person.", a.People.NoPerson, a.Work.Done)
	}
	if a.People.WaitSeconds > 0 {
		fmt.Fprintf(&b, " Work waited %s for people.", Duration(a.People.WaitSeconds))
	}
	cost := Dollars(a.Spend.USD)
	if a.Spend.AtLeast {
		cost = "at least " + cost
	}
	fmt.Fprintf(&b, " The work cost %s", cost)
	if a.Work.Done > 0 {
		fmt.Fprintf(&b, ", %s for each finished piece", Dollars(a.Spend.PerDone))
	}
	b.WriteString(".")
	if a.Spend.Unpriced > 0 {
		fmt.Fprintf(&b, " %s tokens without a price.", plural(a.Spend.Unpriced, "run has", "runs have"))
	}
	if a.Estimated != nil {
		fmt.Fprintf(&b, " At the estimates of people, the finished work of %s would take a person %s.", plural(a.Estimated.Lines, "line", "lines"), Duration(a.Estimated.Minutes*60))
	}
	if a.Partial {
		fmt.Fprintf(&b, " %s no record of the actions of people.", plural(a.Unknown, "piece has", "pieces have"))
	}
	return b.String()
}

// Dollars words an amount in US dollars to the cent, as the page does: a
// cost above zero and below a cent is "under $0.01", never "$0.00", so a
// sponsor does not read a small cost as no cost.
func Dollars(v float64) string {
	if v > 0 && v < 0.005 {
		return "under $0.01"
	}
	return fmt.Sprintf("$%.2f", v)
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

func isAre(n int) string {
	if n == 1 {
		return "is"
	}
	return "are"
}

// Duration words a number of seconds as hours and minutes, such as
// "3 h 10 min", "12 min", or "40 s".
func Duration(s int64) string {
	switch {
	case s < 60:
		return fmt.Sprintf("%d s", s)
	case s < 3600:
		return fmt.Sprintf("%d min", s/60)
	case s%3600/60 == 0:
		return fmt.Sprintf("%d h", s/3600)
	}
	return fmt.Sprintf("%d h %d min", s/3600, s%3600/60)
}
