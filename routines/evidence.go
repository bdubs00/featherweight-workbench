package routines

// This file is the evidence of a routine (docs/ARCHITECTURE.md section
// 21.4): what the runs of its current version showed, per day and per
// question, the last disagreements, the runs that came back, and the last
// result of a replay. The projection (project.go) applies the events of
// the egress stream to it. Each replica of the front reads the stream, so
// each event reaches every replica. The evidence keeps a mark per recent
// run, and an event that a mark shows as counted counts no more, so two
// replicas count an event once.

import (
	"slices"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// Bounds of the evidence.
const (
	// MaxDays is how many days of counts the evidence keeps. The totals
	// cover the whole version.
	MaxDays = 90
	// MaxDisagreements is how many disagreements the evidence keeps,
	// newest first.
	MaxDisagreements = 50
	// maxMarks is how many recent runs the evidence remembers. A receipt
	// for an older run finds no mark and counts nothing.
	maxMarks = 1000
)

// bands are the lower bounds of the bands of a reliability table, beside
// the act of the question.
var bands = []float64{0, 0.5, 0.7, 0.85, 0.95}

// Counts are the counts of a day or of a whole version. Decided counts the
// decisions of the routine beside the agent (rehearsal and canary runs),
// Acted those where it would have acted, and Agreed and Disagreed those of
// Acted that the comparison settled: every compared question agreed, or
// one did not. Handed counts every decision that did not apply, so the
// agent handled the run alone. Applied counts the live runs that the
// routine completed, and Failed the live runs where it started and
// stopped, so the agent took over.
type Counts struct {
	Decided   int `json:"decided"`
	Acted     int `json:"acted"`
	Agreed    int `json:"agreed"`
	Disagreed int `json:"disagreed"`
	Handed    int `json:"handed"`
	Applied   int `json:"applied"`
	Failed    int `json:"failed"`
}

// Day holds the counts of one day in UTC, such as 2026-10-23.
type Day struct {
	Day string `json:"day"`
	Counts
}

// Band is one row of a reliability table: the compared runs whose
// probability for the chosen answer was at least From and below To (at
// most 1 for the last band), and how many of them agreed with the agent.
type Band struct {
	From   float64 `json:"from"`
	To     float64 `json:"to"`
	Runs   int     `json:"runs"`
	Agreed int     `json:"agreed"`
}

// Answer is the answer of the routine to one question in one run, beside
// what the agent did.
type Answer struct {
	Question    string  `json:"question"`
	Said        string  `json:"said"`
	Observed    string  `json:"observed"`
	Agree       bool    `json:"agree"`
	Probability float64 `json:"probability,omitempty"`
}

// Disagreement is a run where the routine would have acted and one of its
// answers differed from what the agent did: the run, the time of the
// decision, the first line of the input, the answers, and the verification
// of the quality receipt of the run when one came (accepted, satisfied,
// rejected, or unknown).
type Disagreement struct {
	Run          string    `json:"run"`
	At           time.Time `json:"at"`
	Input        string    `json:"input"`
	Answers      []Answer  `json:"answers"`
	Verification string    `json:"verification,omitempty"`
}

// SentBack counts the runs that a quality receipt rejected: Applied those
// that the routine completed, and Agreed those where it rehearsed and
// agreed with the agent.
type SentBack struct {
	Applied int `json:"applied"`
	Agreed  int `json:"agreed"`
}

// Replay is the result of the last replay of the routine over recorded
// runs: Matched of Total runs handled the same way, at At.
type Replay struct {
	Matched int       `json:"matched"`
	Total   int       `json:"total"`
	At      time.Time `json:"at"`
}

// Evidence is what the runs of the current version of a routine showed.
// Totals cover the version, and Days its last MaxDays days, oldest first.
// Questions holds the reliability table of each question, and
// Disagreements the last MaxDisagreements disagreements, newest first.
type Evidence struct {
	Version       int               `json:"version"`
	Totals        Counts            `json:"totals"`
	Days          []Day             `json:"days"`
	Questions     map[string][]Band `json:"questions"`
	Disagreements []Disagreement    `json:"disagreements"`
	SentBack      SentBack          `json:"sentBack"`
	Replay        *Replay           `json:"replay,omitempty"`
	UpdatedAt     time.Time         `json:"updatedAt,omitzero"`

	// Marks remembers what each recent run counted, oldest first, so an
	// event counts once. The routes leave it out.
	Marks []mark `json:"marks,omitempty"`
}

// mark is what the evidence counted of one run. Day is the day of the
// decision, where every count of the run goes. Done names the compared
// questions, and Answers holds them while no disagreement entry does.
type mark struct {
	Run      string             `json:"run"`
	Day      string             `json:"day"`
	At       time.Time          `json:"at"`
	Mode     string             `json:"mode,omitempty"`
	Applies  bool               `json:"applies,omitempty"`
	P        map[string]float64 `json:"p,omitempty"`
	Applied  string             `json:"applied,omitempty"` // ok or failed
	Done     []string           `json:"done,omitempty"`
	Answers  []Answer           `json:"answers,omitempty"`
	Verdict  string             `json:"verdict,omitempty"` // agreed or disagreed
	Verified string             `json:"verified,omitempty"`
	Counted  bool               `json:"counted,omitempty"` // sentBack counted
}

// Verdicts of a run that the routine would have acted on.
const (
	verdictAgreed    = "agreed"
	verdictDisagreed = "disagreed"
)

// Modes of a decision, as the kernel records them.
const (
	modeShadow = fw.RoutineShadow
	modeCanary = fw.RoutineCanary
	modeLive   = fw.RoutineLive
)

// newEvidence returns empty evidence for a version.
func newEvidence(version int) *Evidence {
	return &Evidence{Version: version, Days: []Day{}, Questions: map[string][]Band{}, Disagreements: []Disagreement{}}
}

// Ready reports whether a rehearsing routine may go live by the rule of the
// configuration c: over its current version it would have acted on at
// least ReadyAfter runs, it agreed with the agent on at least
// ReadyAgreement of them, and no run where it agreed came back.
func (ev *Evidence) Ready(c Config) bool {
	if ev == nil {
		return false
	}
	t := ev.Totals
	return t.Acted >= c.ReadyAfter && t.Acted > 0 &&
		float64(t.Agreed) >= c.ReadyAgreement*float64(t.Acted) &&
		ev.SentBack.Agreed == 0
}

// public returns a copy of the evidence without the marks.
func (ev *Evidence) public() *Evidence {
	if ev == nil {
		return nil
	}
	c := *ev
	c.Marks = nil
	return &c
}

// decision is a decision of a routine in a run (fw.routine.decided).
// Probability holds, per question, the probability of the chosen answer.
type decision struct {
	mode        string
	applies     bool
	probability map[string]float64
}

// comparison is the comparison of one answer of a routine with what the
// agent did (fw.routine.compared).
type comparison struct {
	question, said, observed string
	agree                    bool
}

// shape is what the evidence needs of the body of a routine: the act of
// each question, and the questions that the kernel compares.
type shape struct {
	act      map[string]float64
	observed []string
}

// markOf returns the mark of a run, or nil.
func (ev *Evidence) markOf(run string) *mark {
	for i := len(ev.Marks) - 1; i >= 0; i-- {
		if ev.Marks[i].Run == run {
			return &ev.Marks[i]
		}
	}
	return nil
}

// day returns the counts of a day, which it adds when it is newer than
// the days that the evidence keeps, or nil for a day older than all of
// them once MaxDays are kept.
func (ev *Evidence) day(d string) *Counts {
	for i := range ev.Days {
		if ev.Days[i].Day == d {
			return &ev.Days[i].Counts
		}
	}
	if len(ev.Days) >= MaxDays && d < ev.Days[0].Day {
		return nil
	}
	ev.Days = append(ev.Days, Day{Day: d})
	slices.SortFunc(ev.Days, func(a, b Day) int {
		switch {
		case a.Day < b.Day:
			return -1
		case a.Day > b.Day:
			return 1
		}
		return 0
	})
	if len(ev.Days) > MaxDays {
		ev.Days = slices.Clone(ev.Days[len(ev.Days)-MaxDays:])
	}
	for i := range ev.Days {
		if ev.Days[i].Day == d {
			return &ev.Days[i].Counts
		}
	}
	return nil
}

// count changes one count of the totals and of the day of a run by n.
func (ev *Evidence) count(d string, n int, field func(*Counts) *int) {
	*field(&ev.Totals) += n
	if c := ev.day(d); c != nil {
		*field(c) += n
	}
}

// decided applies a decision of the routine in the run at the time at. It
// reports whether the evidence changed.
func (ev *Evidence) decided(run string, at time.Time, d decision) bool {
	if ev.markOf(run) != nil {
		return false
	}
	day := at.UTC().Format(time.DateOnly)
	m := mark{Run: run, Day: day, At: at.UTC(), Mode: d.mode, Applies: d.applies, P: d.probability}
	ev.Marks = append(ev.Marks, m)
	if len(ev.Marks) > maxMarks {
		ev.Marks = slices.Clone(ev.Marks[len(ev.Marks)-maxMarks:])
	}
	rehearsal := d.mode == modeShadow || d.mode == modeCanary
	if rehearsal {
		ev.count(day, 1, func(c *Counts) *int { return &c.Decided })
	}
	switch {
	case !d.applies:
		ev.count(day, 1, func(c *Counts) *int { return &c.Handed })
	case rehearsal:
		ev.count(day, 1, func(c *Counts) *int { return &c.Acted })
	}
	return true
}

// applied applies the end of a live routine in the run. It reports whether
// the evidence changed. A run without a mark counts on the day at.
func (ev *Evidence) applied(run string, at time.Time, ok bool) bool {
	m := ev.markOf(run)
	if m == nil {
		ev.decided(run, at, decision{mode: modeLive, applies: true})
		m = ev.markOf(run)
	}
	if m.Applied != "" {
		return false
	}
	if ok {
		m.Applied = "ok"
		ev.count(m.Day, 1, func(c *Counts) *int { return &c.Applied })
	} else {
		m.Applied = "failed"
		ev.count(m.Day, 1, func(c *Counts) *int { return &c.Failed })
	}
	ev.sendBack(m)
	return true
}

// compared applies the comparison of one answer in the run, with the first
// line of its input. A run without a decision counts nothing. It reports
// whether the evidence changed.
func (ev *Evidence) compared(run, input string, c comparison, s shape) bool {
	m := ev.markOf(run)
	if m == nil || slices.Contains(m.Done, c.question) {
		return false
	}
	m.Done = append(m.Done, c.question)
	p, known := m.P[c.question]
	a := Answer{Question: c.question, Said: c.said, Observed: c.observed, Agree: c.agree, Probability: p}
	if known {
		ev.band(c.question, p, c.agree, s.act[c.question])
	}
	if !m.Applies || (m.Mode != modeShadow && m.Mode != modeCanary) {
		return true
	}
	switch {
	case !c.agree && m.Verdict != verdictDisagreed:
		if m.Verdict == verdictAgreed {
			ev.count(m.Day, -1, func(c *Counts) *int { return &c.Agreed })
			if m.Counted && m.Applied != "ok" {
				// The run came back while it counted as agreed.
				ev.SentBack.Agreed--
				m.Counted = false
			}
		}
		ev.count(m.Day, 1, func(c *Counts) *int { return &c.Disagreed })
		m.Verdict = verdictDisagreed
		d := Disagreement{Run: run, At: m.At, Input: input, Answers: append(m.Answers, a), Verification: m.Verified}
		m.Answers = nil
		ev.Disagreements = append([]Disagreement{d}, ev.Disagreements...)
		slices.SortStableFunc(ev.Disagreements, func(x, y Disagreement) int { return y.At.Compare(x.At) })
		if len(ev.Disagreements) > MaxDisagreements {
			ev.Disagreements = slices.Clone(ev.Disagreements[:MaxDisagreements])
		}
	case m.Verdict == verdictDisagreed:
		for i := range ev.Disagreements {
			if ev.Disagreements[i].Run == run {
				ev.Disagreements[i].Answers = append(ev.Disagreements[i].Answers, a)
			}
		}
	default:
		if m.Verdict == "" {
			ev.count(m.Day, 1, func(c *Counts) *int { return &c.Agreed })
			m.Verdict = verdictAgreed
			ev.sendBack(m)
		}
		m.Answers = append(m.Answers, a)
	}
	if len(m.Done) >= len(s.observed) {
		// Every compared question came: the answers of an agreed run are
		// no longer needed.
		m.Answers = nil
	}
	return true
}

// band counts one compared answer in the reliability table of its
// question. The bands are those of bands, with the act of the question as
// one more bound.
func (ev *Evidence) band(question string, p float64, agree bool, act float64) {
	table := ev.Questions[question]
	if table == nil {
		bounds := slices.Clone(bands)
		if act > 0 && act < 1 && !slices.Contains(bounds, act) {
			bounds = append(bounds, act)
			slices.Sort(bounds)
		}
		for i, from := range bounds {
			to := 1.0
			if i+1 < len(bounds) {
				to = bounds[i+1]
			}
			table = append(table, Band{From: from, To: to})
		}
	}
	for i := range table {
		last := i == len(table)-1
		if p >= table[i].From && (p < table[i].To || last) {
			table[i].Runs++
			if agree {
				table[i].Agreed++
			}
			break
		}
	}
	if ev.Questions == nil {
		ev.Questions = map[string][]Band{}
	}
	ev.Questions[question] = table
}

// verified applies the verification of a quality receipt of the run, and
// counts a rejected run in SentBack. It reports whether the evidence
// changed.
func (ev *Evidence) verified(run, verification string) bool {
	m := ev.markOf(run)
	if m == nil || verification == "" || m.Verified == verification {
		return false
	}
	m.Verified = verification
	for i := range ev.Disagreements {
		if ev.Disagreements[i].Run == run {
			ev.Disagreements[i].Verification = verification
		}
	}
	ev.sendBack(m)
	return true
}

// sendBack counts a rejected run once in SentBack: as applied when the
// routine completed it, else as agreed when the routine agreed with the
// agent. A receipt can come before the end of the routine or before the
// comparison, so each of them calls it.
func (ev *Evidence) sendBack(m *mark) {
	if m.Verified != "rejected" || m.Counted {
		return
	}
	switch {
	case m.Applied == "ok":
		ev.SentBack.Applied++
		m.Counted = true
	case m.Verdict == verdictAgreed:
		ev.SentBack.Agreed++
		m.Counted = true
	}
}
