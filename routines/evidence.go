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
	"unicode/utf8"

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
// Acted that the comparison settled: every compared question and the
// comparison of the calls agreed, or one did not. Handed counts every
// decision that did not apply, so the agent handled the run alone. Applied
// counts the live runs that the routine completed, and Failed the live
// runs where it started and stopped, so the agent took over.
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

// Compared is the answer of the routine to one question in one run,
// beside what the agent did, with the probability of the chosen answer.
// For the comparison of the calls (fw.RoutineComparedData.Calls), Calls is
// true, Question is empty, Said and Observed are the input of the step of
// the routine and of the call of the agent that differ, and Reason says
// how they differ.
type Compared struct {
	Question    string  `json:"question"`
	Said        string  `json:"said"`
	Observed    string  `json:"observed"`
	Agree       bool    `json:"agree"`
	Probability float64 `json:"probability,omitempty"`
	Calls       bool    `json:"calls,omitempty"`
	Reason      string  `json:"reason,omitempty"`
}

// Disagreement is a run where the routine would have acted and one of its
// answers differed from what the agent did: the run, the time of the
// decision, the first line of the input, the answers, and the verification
// of the quality receipt of the run when one came (accepted, satisfied,
// rejected, or unknown).
type Disagreement struct {
	Run          string     `json:"run"`
	At           time.Time  `json:"at"`
	Input        string     `json:"input"`
	Answers      []Compared `json:"answers"`
	Verification string     `json:"verification,omitempty"`
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
// Risk is the risk that the newest decision of the kernel named,
// fw.RiskRead or fw.RiskWrite, or "" before the first one. Since holds the
// counts at the last step back, and the ready rule counts only what came
// after it.
type Evidence struct {
	Version       int               `json:"version"`
	Totals        Counts            `json:"totals"`
	Days          []Day             `json:"days"`
	Questions     map[string][]Band `json:"questions"`
	Disagreements []Disagreement    `json:"disagreements"`
	SentBack      SentBack          `json:"sentBack"`
	Replay        *Replay           `json:"replay,omitempty"`
	Risk          string            `json:"risk,omitempty"`
	Since         *Since            `json:"since,omitempty"`
	UpdatedAt     time.Time         `json:"updatedAt,omitzero"`

	// Marks remembers what each recent run counted, oldest first, so an
	// event counts once. The routes leave it out.
	Marks []mark `json:"marks,omitempty"`

	// stepBack is the first run of the events that this write applied
	// that steps a live routine back: a disagreement in a canary run, or
	// a run that the routine completed and a receipt rejected. The store
	// keeps no such field.
	stepBack *stepBack
}

// Since holds the totals and the runs sent back at the moment when a
// routine stepped back (docs/KNOWHOW.md section 6.5). The ready rule
// counts only the runs after it, so a routine that stepped back rehearses
// again before it is ready.
type Since struct {
	At       time.Time `json:"at"`
	Totals   Counts    `json:"totals"`
	SentBack SentBack  `json:"sentBack"`
}

// stepBack is a run that steps a live routine back, and why, in words.
type stepBack struct {
	run, why string
}

// Why words of a step back, for the note of the move.
const (
	whyCanary   = "a canary run disagreed with the agent"
	whySentBack = "a run that it completed was sent back"
	whyNotDone  = "its work on a run failed the checks of done"
)

// noteStepBack sets the run that steps the routine back, when none came
// before it in this write.
func (ev *Evidence) noteStepBack(run, why string) {
	if ev.stepBack == nil {
		ev.stepBack = &stepBack{run: run, why: why}
	}
}

// stepBackAt sets Since to the counts of now.
func (ev *Evidence) stepBackAt(at time.Time) {
	ev.Since = &Since{At: at.UTC(), Totals: ev.Totals, SentBack: ev.SentBack}
}

// current returns the counts and the agreed runs sent back since the last
// step back.
func (ev *Evidence) current() (Counts, int) {
	t, sent := ev.Totals, ev.SentBack.Agreed
	if ev.Since != nil {
		b := ev.Since.Totals
		t.Decided -= b.Decided
		t.Acted -= b.Acted
		t.Agreed -= b.Agreed
		t.Disagreed -= b.Disagreed
		t.Handed -= b.Handed
		t.Applied -= b.Applied
		t.Failed -= b.Failed
		sent -= ev.Since.SentBack.Agreed
	}
	return t, sent
}

// mark is what the evidence counted of one run. Day is the day of the
// decision, where every count of the run goes. Early is true while the
// mark holds comparisons that came before the decision, which a bus can
// deliver out of order: they count once the decision comes. Done names the
// compared questions, and P and Answers hold the probabilities and the
// answers while a comparison can still come.
type mark struct {
	Run      string             `json:"run"`
	Day      string             `json:"day"`
	At       time.Time          `json:"at"`
	Early    bool               `json:"early,omitempty"`
	Mode     string             `json:"mode,omitempty"`
	Applies  bool               `json:"applies,omitempty"`
	P        map[string]float64 `json:"p,omitempty"`
	Applied  string             `json:"applied,omitempty"` // ok or failed
	Done     []string           `json:"done,omitempty"`
	Answers  []Compared         `json:"answers,omitempty"`
	Input    string             `json:"input,omitempty"`
	Verdict  string             `json:"verdict,omitempty"` // agreed or disagreed
	Verified string             `json:"verified,omitempty"`
	Counted  bool               `json:"counted,omitempty"` // sentBack counted
}

// Verdicts of a run that the routine would have acted on.
const (
	verdictAgreed    = "agreed"
	verdictDisagreed = "disagreed"
)

// newEvidence returns empty evidence for a version.
func newEvidence(version int) *Evidence {
	return &Evidence{Version: version, Days: []Day{}, Questions: map[string][]Band{}, Disagreements: []Disagreement{}}
}

// Ready reports whether a rehearsing routine may go live by the rule of the
// configuration c: over its current version, since its last step back, it
// would have acted on at least the runs of ReadyAfter for its risk, each
// with its comparison, it agreed with the agent on at least ReadyAgreement
// of the runs that it would have acted on, and no run where it agreed came
// back. The decision of a run comes at its start and the comparison at
// its end, so a run that still works counts toward no bound and against
// the agreement: the routine never gets ready on a run whose comparison
// has not come.
func (ev *Evidence) Ready(c Config) bool {
	if ev == nil {
		return false
	}
	t, sentBack := ev.current()
	return t.compared() >= c.ReadyAfter.For(ev.Risk) && t.Acted > 0 &&
		float64(t.Agreed) >= c.ReadyAgreement*float64(t.Acted) &&
		sentBack == 0
}

// compared counts the runs that the routine would have acted on and whose
// comparison came: those that agreed and those that did not.
func (c Counts) compared() int { return c.Agreed + c.Disagreed }

// public returns a copy of the evidence without the marks.
func (ev *Evidence) public() *Evidence {
	if ev == nil {
		return nil
	}
	c := *ev
	c.Marks = nil
	return &c
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

// rehearsal reports whether a mode of a decision runs beside the agent.
func rehearsal(mode string) bool { return mode == fw.RoutineShadow || mode == fw.RoutineCanary }

// newMark adds the mark of a run decided at the time at.
func (ev *Evidence) newMark(run string, at time.Time) *mark {
	ev.Marks = append(ev.Marks, mark{Run: run, Day: at.UTC().Format(time.DateOnly), At: at.UTC()})
	if len(ev.Marks) > maxMarks {
		ev.Marks = slices.Clone(ev.Marks[len(ev.Marks)-maxMarks:])
	}
	return &ev.Marks[len(ev.Marks)-1]
}

// decided applies fw.routine.decided of the run at the time at, for the
// routine r. The comparisons that came before it count now. It reports
// whether the evidence changed.
func (ev *Evidence) decided(run string, at time.Time, d fw.RoutineDecidedData, r fw.Routine) bool {
	m := ev.markOf(run)
	switch {
	case m == nil:
		m = ev.newMark(run, at)
	case !m.Early:
		return false
	}
	m.Early, m.Mode, m.Applies = false, d.Mode, d.Applies
	m.Day, m.At = at.UTC().Format(time.DateOnly), at.UTC()
	if d.Risk != "" {
		ev.Risk = d.Risk
	}
	for q, a := range d.Answers {
		if m.P == nil {
			m.P = map[string]float64{}
		}
		m.P[q] = probabilityOf(a)
	}
	if rehearsal(d.Mode) {
		ev.count(m.Day, 1, func(c *Counts) *int { return &c.Decided })
	}
	switch {
	case !d.Applies:
		ev.count(m.Day, 1, func(c *Counts) *int { return &c.Handed })
	case rehearsal(d.Mode):
		ev.count(m.Day, 1, func(c *Counts) *int { return &c.Acted })
	}
	early := m.Answers
	m.Answers = nil
	for _, a := range early {
		a.Probability = m.P[a.Question]
		ev.settle(m, a, r)
	}
	ev.trim(m, r)
	return true
}

// probabilityOf is the probability of the chosen answer: of the option of
// a choice or a score, and the larger of P(yes) and P(no) for a noul.
func probabilityOf(a fw.Answer) float64 {
	if a.Type == fw.QuestionNoul {
		return max(a.Probability, 1-a.Probability)
	}
	if p, ok := a.Probabilities[a.Choice]; ok {
		return p
	}
	return a.Probability
}

// applied applies fw.routine.applied of the run at the time at. A run
// without a mark counts as a live decision on that day. It reports whether
// the evidence changed.
func (ev *Evidence) applied(run string, at time.Time, d fw.RoutineAppliedData) bool {
	m := ev.markOf(run)
	if m == nil || m.Early {
		ev.decided(run, at, fw.RoutineDecidedData{Mode: fw.RoutineLive, Applies: true}, fw.Routine{})
		m = ev.markOf(run)
	}
	if m.Applied != "" {
		return false
	}
	if d.OK {
		// A routine that completed gets no comparison, so the mark needs
		// no probability any more.
		m.Applied, m.P, m.Answers = "ok", nil, nil
		ev.count(m.Day, 1, func(c *Counts) *int { return &c.Applied })
	} else {
		m.Applied = "failed"
		ev.count(m.Day, 1, func(c *Counts) *int { return &c.Failed })
		if d.Done == fw.DoneFailed {
			// The checks of done of the run refuted the work of the
			// routine, as a receipt that sends a run back does.
			ev.noteStepBack(m.Run, whyNotDone)
		}
	}
	ev.sendBack(m)
	return true
}

// compared applies fw.routine.compared of the run, for the routine r,
// with the line of the run: its title, else the first line of its input.
// A comparison that comes before its decision waits in the mark of the
// run. The comparison of the calls has no question, so the mark keeps it
// under the empty name, which no question has. It reports whether the
// evidence changed.
func (ev *Evidence) compared(run, line string, at time.Time, d fw.RoutineComparedData, r fw.Routine) bool {
	m := ev.markOf(run)
	if m == nil {
		m = ev.newMark(run, at)
		m.Early = true
	}
	if slices.Contains(m.Done, d.Question) {
		return false
	}
	m.Done = append(m.Done, d.Question)
	m.Input = cut(line, maxAnswerBytes)
	a := Compared{Question: d.Question, Said: cut(d.Said, maxAnswerBytes), Observed: cut(d.Observed, maxAnswerBytes), Agree: d.Agree, Probability: m.P[d.Question],
		Calls: d.Calls, Reason: cut(d.Reason, maxAnswerBytes)}
	if m.Early {
		m.Answers = append(m.Answers, a)
		return true
	}
	ev.settle(m, a, r)
	ev.trim(m, r)
	return true
}

// settle counts one comparison of a run whose decision came: in the
// reliability table of its question, and, for a run that the routine
// would have acted on beside the agent, in the verdict of the run.
func (ev *Evidence) settle(m *mark, a Compared, r fw.Routine) {
	if _, known := m.P[a.Question]; known {
		ev.band(a.Question, a.Probability, a.Agree, r.Questions[a.Question].Act)
	}
	if !m.Applies || !rehearsal(m.Mode) {
		return
	}
	switch {
	case !a.Agree && m.Verdict != verdictDisagreed:
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
		if m.Mode == fw.RoutineCanary {
			ev.noteStepBack(m.Run, whyCanary)
		}
		d := Disagreement{Run: m.Run, At: m.At, Input: m.Input, Answers: append(m.Answers, a), Verification: m.Verified}
		m.Answers = nil
		ev.Disagreements = append([]Disagreement{d}, ev.Disagreements...)
		slices.SortStableFunc(ev.Disagreements, func(x, y Disagreement) int { return y.At.Compare(x.At) })
		if len(ev.Disagreements) > MaxDisagreements {
			ev.Disagreements = slices.Clone(ev.Disagreements[:MaxDisagreements])
		}
	case m.Verdict == verdictDisagreed:
		for i := range ev.Disagreements {
			if ev.Disagreements[i].Run == m.Run {
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
}

// trim drops what a mark no longer needs once every comparison that the
// kernel makes came: one per question with observe, and one of the calls
// for a rehearsal that applies. The mark keeps what makes an event count
// once, and no more.
func (ev *Evidence) trim(m *mark, r fw.Routine) {
	want := 0
	for _, q := range r.Questions {
		if q.Observe != nil {
			want++
		}
	}
	if m.Applies && rehearsal(m.Mode) && len(r.Steps)+len(r.After) > 0 {
		want++
	}
	if len(m.Done) >= want {
		m.P, m.Answers, m.Input = nil, nil, ""
	}
}

// maxAnswerBytes bounds the answer of the routine and the answer of the
// agent that the evidence keeps for one question.
const maxAnswerBytes = 200

// cut cuts s to n bytes at a rune.
func cut(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// fit trims the oldest marks until the evidence encodes in at most limit
// bytes, and returns the value. The counts stay: a trimmed mark only makes
// a late event of its run count nothing.
func (ev *Evidence) fit(limit int) ([]byte, error) {
	for {
		value, err := encode(ev)
		if err != nil || len(value) <= limit || len(ev.Marks) == 0 {
			return value, err
		}
		ev.Marks = slices.Clone(ev.Marks[max(1, len(ev.Marks)/8):])
	}
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
		ev.noteStepBack(m.Run, whySentBack)
	case m.Verdict == verdictAgreed:
		ev.SentBack.Agreed++
		m.Counted = true
	}
}
