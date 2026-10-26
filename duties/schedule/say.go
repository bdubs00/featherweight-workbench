package schedule

// This file reads the words of a person into a schedule with a fixed
// grammar, so the same words always give the same schedule and no model
// takes part. The grammar reads clauses in any order:
//
//	a repeat   every day, daily, weekdays, every Monday and Thursday,
//	           every other Tuesday, every 2 weeks on Friday, every morning,
//	           every evening, hourly, every 2 hours, every 15 minutes,
//	           on the first of the month, on the last day of the month,
//	           on the first Monday of the month, every month on the 15th
//	times      at 9am, at 9:30am and 4pm, at noon, in the morning, 16:30
//	a window   between 8am and 6pm, from 8:00 to 18:00
//	days       on weekdays, on Monday and Thursday (with hourly and every)
//	dates      starting 2026-11-01, until December 31
//	a zone     UTC, America/New_York
//
// When a part does not read, the error names it: "Every Tuesday is clear.
// "after lunch" is not a time: pick one."

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Options of Parse.
type Options struct {
	// Zone is the time zone of a schedule whose words name none.
	Zone string
	// Now is the moment of the words. A date without a year is its next
	// occurrence from Now, and a repeat of several weeks counts its weeks
	// from the date of Now.
	Now time.Time
}

// SayError is a part of the words that the grammar cannot read. Clear is
// the part before it, which reads, Part the part that does not, and
// Problem says what is wrong with Part and what to do.
type SayError struct {
	Clear   string
	Part    string
	Problem string
}

func (e *SayError) Error() string {
	switch {
	case e.Part == "" && e.Clear == "":
		return e.Problem + "."
	case e.Part == "":
		return e.Clear + " " + e.Problem + "."
	case e.Clear == "":
		return fmt.Sprintf("%q %s.", e.Part, e.Problem)
	}
	return fmt.Sprintf("%s is clear. %q %s.", e.Clear, e.Part, e.Problem)
}

// Words of the problems.
const (
	notRepeat = "is not a repeat: say one, such as every day, weekdays, every Monday, every 2 hours, or the first of the month"
	notTime   = "is not a time: pick one"
	notDays   = "is not a day of the week: pick one, such as Monday or weekdays"
	notMonth  = "is not a day of the month: pick one, such as the 1st, the last day, or the first Monday"
	notDate   = "is not a date: write one such as 2026-12-31 or December 31"
	notPart   = "is not part of a schedule: leave it out or say it another way"
	twice     = "is a second repeat: keep one"
)

// token is one word of the input: raw as written, w in lower case.
type token struct{ raw, w string }

// tokenize splits the words. A comma is a word of its own, and a stop at
// the end goes.
func tokenize(s string) []token {
	s = strings.TrimRight(strings.TrimSpace(s), ".!? ")
	var out []token
	for _, f := range strings.Fields(s) {
		for f != "" {
			i := strings.IndexByte(f, ',')
			if i < 0 {
				out = append(out, token{f, strings.ToLower(f)})
				break
			}
			if i > 0 {
				out = append(out, token{f[:i], strings.ToLower(f[:i])})
			}
			out = append(out, token{",", ","})
			f = f[i+1:]
		}
	}
	return out
}

// parser reads one input.
type parser struct {
	t   []token
	i   int
	opt Options

	repeat     string
	repeatAt   int // the token where the repeat starts
	days       []int
	weeks      int
	dom        string
	times      []int
	timesAt    int // the token of the first clause of times, or -1
	timesEnd   int
	step       time.Duration
	between    []int
	start, end string
	zone       string
}

// Parse reads the words of a person into a schedule with the fixed
// grammar. The schedule is normalized with the zone of opt and passes its
// checks. A part that does not read gives a *SayError that names it.
func Parse(words string, opt Options) (Schedule, error) {
	p := &parser{t: tokenize(words), opt: opt, timesAt: -1, repeatAt: -1}
	if len(p.t) == 0 {
		return Schedule{}, &SayError{Problem: "Say when, such as every weekday at 9am"}
	}
	clauses := []func() (bool, error){p.repeatClause, p.monthClause, p.atClause, p.betweenClause, p.daysClause, p.dateClause, p.zoneClause, p.bareTime}
	for p.i < len(p.t) {
		if w := p.w(0); w == "and" || w == "," || w == "then" {
			p.i++
			continue
		}
		matched := false
		for _, c := range clauses {
			ok, err := c()
			if err != nil {
				return Schedule{}, err
			}
			if ok {
				matched = true
				break
			}
		}
		if !matched {
			return Schedule{}, p.fail(p.i, p.chunkEnd(p.i), p.want())
		}
	}
	return p.build()
}

// w returns the word k places after the cursor in lower case, or "".
func (p *parser) w(k int) string { return p.at(p.i + k) }

// at returns the word at j in lower case, or "".
func (p *parser) at(j int) string {
	if j < 0 || j >= len(p.t) {
		return ""
	}
	return p.t[j].w
}

// fail returns the error for the words from..to. The clear part leaves
// out an "and" or a comma at its end.
func (p *parser) fail(from, to int, problem string) error {
	clear := from
	for clear > 0 && (p.at(clear-1) == "and" || p.at(clear-1) == ",") {
		clear--
	}
	return &SayError{Clear: p.echo(0, clear, true), Part: p.echo(from, to, false), Problem: problem}
}

// echo writes the words from..to as the person wrote them. With clear, it
// starts with a capital and writes the names of days and months with
// capitals, as the readback does.
func (p *parser) echo(from, to int, clear bool) string {
	var b strings.Builder
	for j := max(from, 0); j < min(to, len(p.t)); j++ {
		raw := p.t[j].raw
		if clear && (dayOf(p.t[j].w) >= 0 || monthOf(p.t[j].w) > 0) && !strings.ContainsAny(raw, "-/") {
			raw = capital(raw)
		}
		if b.Len() > 0 && raw != "," {
			b.WriteByte(' ')
		}
		b.WriteString(raw)
	}
	s := strings.TrimRight(b.String(), ", ")
	if clear {
		s = capital(s)
	}
	return s
}

// capital writes the first letter of s as a capital.
func capital(s string) string {
	r, n := utf8.DecodeRuneInString(s)
	if n == 0 {
		return s
	}
	return string(unicode.ToUpper(r)) + s[n:]
}

// starts are the words that start a clause, so a part that does not read
// ends before them.
var starts = map[string]bool{"at": true, "between": true, "from": true, "on": true, "every": true, "each": true, "starting": true, "until": true, "and": true, ",": true}

// chunkEnd returns the end of the part that starts at from: the next word
// that starts a clause, or the end.
func (p *parser) chunkEnd(from int) int {
	j := from + 1
	for j < len(p.t) && !starts[p.at(j)] {
		j++
	}
	return j
}

// want says what the grammar wants for a part that does not read.
func (p *parser) want() string {
	switch {
	case p.repeat == "" && len(p.days) == 0 && len(p.times) == 0:
		return notRepeat
	case withTimes(p.repeat) && len(p.times) == 0:
		return notTime
	}
	return notPart
}

// setRepeat sets the repeat of the clause that starts at from, or fails
// for a second repeat.
func (p *parser) setRepeat(from int, repeat string) error {
	if p.repeat != "" {
		return p.fail(from, p.i, twice)
	}
	p.repeat, p.repeatAt = repeat, from
	return nil
}

// repeatClause reads a repeat: every ..., daily, hourly, weekly, monthly,
// weekdays, weekends, or days such as "mondays".
func (p *parser) repeatClause() (bool, error) {
	from := p.i
	switch w := p.w(0); w {
	case "every", "each":
		return true, p.every()
	case "daily":
		p.i++
		return true, p.setRepeat(from, RepeatDaily)
	case "hourly":
		p.i++
		return true, p.setRepeat(from, RepeatHourly)
	case "weekly":
		p.i++
		return true, p.setRepeat(from, RepeatWeekly)
	case "monthly":
		p.i++
		return true, p.setRepeat(from, RepeatMonthly)
	}
	if p.repeat != "" {
		return false, nil
	}
	switch p.w(0) {
	case "weekdays", "weekday":
		p.i++
		if err := p.setRepeat(from, RepeatWeekdays); err != nil {
			return true, err
		}
		p.partOfDay()
		return true, nil
	case "weekends", "weekend":
		p.i++
		p.days = []int{5, 6}
		return true, p.setRepeat(from, RepeatWeekly)
	}
	// Bare days, such as "mondays at 8" or "Monday and Thursday at 8:00".
	if days, j, ok := p.dayList(p.i); ok {
		p.i = j
		p.days = days
		if err := p.setRepeat(from, RepeatWeekly); err != nil {
			return true, err
		}
		p.partOfDay()
		return true, nil
	}
	return false, nil
}

// every reads the words after "every" or "each".
func (p *parser) every() error {
	from := p.i
	p.i++
	set := func(repeat string, n int) error {
		p.i += n
		return p.setRepeat(from, repeat)
	}
	w := p.w(0)
	switch {
	case w == "day" || (w == "single" && p.w(1) == "day"):
		if w == "single" {
			p.i++
		}
		return set(RepeatDaily, 1)
	case w == "weekday" || w == "weekdays":
		if err := set(RepeatWeekdays, 1); err != nil {
			return err
		}
		p.partOfDay()
		return nil
	case w == "weekend" || w == "weekends":
		p.days = []int{5, 6}
		return set(RepeatWeekly, 1)
	case w == "morning" || w == "evening":
		p.addPart(w, p.i)
		return set(RepeatDaily, 1)
	case w == "hour":
		return set(RepeatHourly, 1)
	case w == "half" && (p.w(1) == "hour" || (p.w(1) == "an" && p.w(2) == "hour")):
		p.step = 30 * time.Minute
		if p.w(1) == "an" {
			p.i++
		}
		return set(RepeatEvery, 2)
	case w == "quarter" && p.w(1) == "hour":
		p.step = 15 * time.Minute
		return set(RepeatEvery, 2)
	case w == "minute":
		p.step = time.Minute
		return p.checkStep(from, 1)
	case w == "week":
		return set(RepeatWeekly, 1)
	case w == "month":
		return set(RepeatMonthly, 1)
	case w == "other":
		p.i++
		p.weeks = 2
		if p.w(0) == "week" {
			return set(RepeatWeekly, 1)
		}
		if days, j, ok := p.dayList(p.i); ok {
			p.i, p.days = j, days
			return set(RepeatWeekly, 0)
		}
		return p.fail(from, p.chunkEnd(p.i), "is not a repeat that this server knows: say every other Monday, or every 2 weeks on Friday")
	case (w == "second" || w == "third" || w == "fourth") && p.w(1) == "week":
		p.weeks = ordinals[w]
		return set(RepeatWeekly, 2)
	}
	if n, ok := ordinals[w]; ok {
		if d := dayOf(p.w(1)); d >= 0 {
			// "every first Monday of the month"
			p.i += 2
			p.dom = nthName(n) + " " + fullDays[d]
			p.ofMonth()
			return p.setRepeat(from, RepeatMonthly)
		}
	}
	if days, j, ok := p.dayList(p.i); ok {
		p.i, p.days = j, days
		if err := p.setRepeat(from, RepeatWeekly); err != nil {
			return err
		}
		p.partOfDay()
		return nil
	}
	if n, ok := number(w); ok {
		switch unit := p.w(1); {
		case isUnit(unit, "minute", "minutes", "min", "mins", "m"):
			p.step = time.Duration(n) * time.Minute
			return p.checkStep(from, 2)
		case isUnit(unit, "hour", "hours", "hr", "hrs", "h"):
			p.step = time.Duration(n) * time.Hour
			return p.checkStep(from, 2)
		case isUnit(unit, "week", "weeks"):
			p.i += 2
			if n < 1 || n > MaxEveryWeeks {
				return p.fail(from, p.i, "is too long: the longest repeat is every 4 weeks")
			}
			p.weeks = n
			return p.setRepeat(from, RepeatWeekly)
		case isUnit(unit, "day", "days", "month", "months", "year", "years"):
			p.i += 2
			return p.fail(from, p.i, "is not a repeat that this server knows: say every day, days of the week, or a day of the month")
		}
	}
	return p.fail(from, p.chunkEnd(from), notRepeat)
}

// checkStep sets the repeat every after a step of n words, or fails for a
// step out of bounds.
func (p *parser) checkStep(from, n int) error {
	p.i += n
	switch {
	case p.step < MinEvery:
		return p.fail(from, p.i, "is too often: the shortest step is 15 minutes")
	case p.step > MaxEvery:
		return p.fail(from, p.i, "is too long: the longest step is 24 hours")
	case p.step == time.Hour:
		return p.setRepeat(from, RepeatHourly)
	}
	return p.setRepeat(from, RepeatEvery)
}

// isUnit reports whether w is one of the names of a unit.
func isUnit(w string, names ...string) bool { return slices.Contains(names, w) }

// partOfDay reads "morning" or "evening" after a repeat, as in "every
// Monday morning".
func (p *parser) partOfDay() {
	if w := p.w(0); w == "morning" || w == "mornings" || w == "evening" || w == "evenings" {
		p.addPart(strings.TrimSuffix(w, "s"), p.i)
		p.i++
	}
}

// addPart adds the time of a part of the day: the morning at 9:00 and the
// evening at 18:00.
func (p *parser) addPart(part string, at int) {
	m := 9 * 60
	if part == "evening" {
		m = 18 * 60
	}
	p.addTimes(at, at+1, m)
}

// addTimes adds times from the clause at from..to.
func (p *parser) addTimes(from, to int, ms ...int) {
	if p.timesAt < 0 {
		p.timesAt = from
	}
	p.timesEnd = to
	p.times = append(p.times, ms...)
}

// monthClause reads a day of the month: "on the 1st", "the first of the
// month", "on the last day of the month", "on the first Monday of the
// month", each with an optional "of the month", "of every month", or
// "each month".
func (p *parser) monthClause() (bool, error) {
	from, j := p.i, p.i
	if p.at(j) == "on" {
		j++
	}
	if p.at(j) != "the" {
		return false, nil
	}
	j++
	w := p.at(j)
	switch n, isOrd := ordinals[w]; {
	case w == "last" && (p.at(j+1) == "day" || p.at(j+1) == "of" || p.at(j+1) == ""):
		p.dom = "last"
		j++
		if p.at(j) == "day" {
			j++
		}
	case isOrd && dayOf(p.at(j+1)) >= 0:
		p.dom = nthName(n) + " " + fullDays[dayOf(p.at(j+1))]
		j += 2
	case w == "first" || w == "1st":
		p.dom = "1"
		j++
		if p.at(j) == "day" {
			j++
		}
	default:
		day, ok := dayNumber(w)
		if !ok {
			if p.at(from) == "on" {
				return true, p.fail(from, p.chunkEnd(j), notMonth)
			}
			return false, nil
		}
		p.dom = strconv.Itoa(day)
		j++
		if p.at(j) == "day" {
			j++
		}
	}
	p.i = j
	p.ofMonth()
	if p.repeat != "" && p.repeat != RepeatMonthly {
		return true, p.fail(from, p.i, twice)
	}
	p.repeat = RepeatMonthly
	if p.repeatAt < 0 {
		p.repeatAt = from
	}
	return true, nil
}

// ofMonth reads "of the month", "of every month", "of each month", "each
// month", or "every month" after a day of the month.
func (p *parser) ofMonth() {
	switch {
	case p.w(0) == "of" && (p.w(1) == "the" || p.w(1) == "every" || p.w(1) == "each") && p.w(2) == "month":
		p.i += 3
	case p.w(0) == "of" && p.w(1) == "month":
		p.i += 2
	case (p.w(0) == "each" || p.w(0) == "every") && p.w(1) == "month":
		p.i += 2
	}
}

// nthName is the word of an ordinal of a weekday of the month.
func nthName(n int) string {
	for word, v := range ordinals {
		if v == n {
			return word
		}
	}
	return ""
}

// atClause reads "at <time> and <time> ...", "in the morning", and "in the
// evening".
func (p *parser) atClause() (bool, error) {
	from := p.i
	if p.w(0) == "in" && p.w(1) == "the" && (p.w(2) == "morning" || p.w(2) == "evening") {
		p.i += 3
		p.addPart(p.at(from+2), from)
		p.timesEnd = p.i
		return true, nil
	}
	if p.w(0) != "at" {
		return false, nil
	}
	p.i++
	if p.w(0) == "the" && p.w(1) == "same" {
		return true, p.fail(from, p.chunkEnd(p.i), notTime)
	}
	ms, j, ok := p.timeList(p.i)
	if !ok {
		return true, p.fail(from, p.timeEnd(p.i), notTime)
	}
	p.i = j
	p.addTimes(from, j, ms...)
	return true, nil
}

// timeEnd returns the end of a time that does not read at j: the word,
// with "am" or "pm" after it.
func (p *parser) timeEnd(j int) int {
	if j >= len(p.t) {
		return j
	}
	if isMeridiem(p.at(j + 1)) {
		return j + 2
	}
	if _, err := strconv.Atoi(strings.TrimRight(p.at(j), ":.")); err != nil && !strings.ContainsAny(p.at(j), "0123456789") {
		return p.chunkEnd(j)
	}
	return j + 1
}

// timeList reads times joined by "and", ",", or "or" from j.
func (p *parser) timeList(j int) ([]int, int, bool) {
	m, k, ok, _ := p.timeAt(j)
	if !ok {
		return nil, j, false
	}
	out := []int{m}
	for {
		sep := k
		if p.at(sep) == "," && p.at(sep+1) == "and" {
			sep++
		}
		if w := p.at(sep); w != "and" && w != "," && w != "or" {
			return out, k, true
		}
		m, next, ok, _ := p.timeAt(sep + 1)
		if !ok {
			return out, k, true
		}
		out, k = append(out, m), next
	}
}

// timePattern is a time as a person writes it: 9, 9am, 9:30, 9.30pm.
var timePattern = regexp.MustCompile(`^(\d{1,2})(?:[:.](\d{2}))?(am|pm|a\.m\.|p\.m\.|a|p)?$`)

// isMeridiem reports whether w is am or pm.
func isMeridiem(w string) bool {
	switch w {
	case "am", "pm", "a.m.", "p.m.":
		return true
	}
	return false
}

// timeAt reads a time at j: minutes of the day, the next word, whether it
// reads, and whether it is clearly a time (am, pm, a colon, noon, or
// midnight) and not a bare number.
func (p *parser) timeAt(j int) (int, int, bool, bool) {
	switch p.at(j) {
	case "noon", "midday":
		return 12 * 60, j + 1, true, true
	case "midnight":
		return 0, j + 1, true, true
	}
	m := timePattern.FindStringSubmatch(p.at(j))
	if m == nil {
		return 0, j, false, false
	}
	h, _ := strconv.Atoi(m[1])
	mi := 0
	if m[2] != "" {
		mi, _ = strconv.Atoi(m[2])
	}
	suffix, k := m[3], j+1
	if suffix == "" && isMeridiem(p.at(k)) {
		suffix, k = p.at(k), k+1
	}
	if suffix == "" && p.at(k) == "o'clock" {
		k++
	}
	if mi > 59 {
		return 0, j, false, false
	}
	if suffix == "" {
		if h > 23 {
			return 0, j, false, false
		}
		return h*60 + mi, k, true, m[2] != ""
	}
	if h < 1 || h > 12 {
		return 0, j, false, false
	}
	h %= 12
	if strings.HasPrefix(suffix, "p") {
		h += 12
	}
	return h*60 + mi, k, true, true
}

// bareTime reads a time without "at", such as "weekdays 9am". Only a
// clear time counts, so a bare number is never a time.
func (p *parser) bareTime() (bool, error) {
	ms, j, ok := p.timeList(p.i)
	if !ok {
		return false, nil
	}
	if _, _, _, clear := p.timeAt(p.i); !clear {
		return false, nil
	}
	p.addTimes(p.i, j, ms...)
	p.i = j
	return true, nil
}

// betweenClause reads "between <time> and <time>" and "from <time> to
// <time>".
func (p *parser) betweenClause() (bool, error) {
	from := p.i
	var join []string
	switch p.w(0) {
	case "between":
		join = []string{"and", "-"}
	case "from":
		if _, _, ok, _ := p.timeAt(p.i + 1); !ok {
			return false, nil
		}
		join = []string{"to", "until", "till", "through", "-"}
	default:
		return false, nil
	}
	a, j, ok, _ := p.timeAt(p.i + 1)
	if !ok || !slices.Contains(join, p.at(j)) {
		return true, p.fail(from, p.chunkEnd(p.i+1), "is not a window of the day: say between 8am and 6pm")
	}
	b, k, ok, _ := p.timeAt(j + 1)
	if !ok {
		return true, p.fail(from, p.timeEnd(j+1), "is not a window of the day: say between 8am and 6pm")
	}
	if b <= a {
		return true, p.fail(from, k, "ends before it starts: say two times of one day, the earlier first")
	}
	if p.between != nil {
		return true, p.fail(from, k, "is a second window of the day: keep one")
	}
	p.between = []int{a, b}
	p.i = k
	return true, nil
}

// daysClause reads days after a repeat: "on weekdays", "on Monday and
// Thursday", or bare "weekdays" after hourly.
func (p *parser) daysClause() (bool, error) {
	from, j := p.i, p.i
	if p.at(j) == "on" {
		j++
	}
	days, k, ok := p.dayList(j)
	if !ok {
		if p.at(from) == "on" && p.at(j) != "the" {
			return true, p.fail(from, p.chunkEnd(j), notDays)
		}
		return false, nil
	}
	switch {
	case p.repeat == RepeatWeekly && len(p.days) == 0, p.repeat == RepeatHourly, p.repeat == RepeatEvery, p.repeat == "":
		if len(p.days) > 0 {
			return true, p.fail(from, k, "is a second set of days: keep one")
		}
		p.days = days
	case p.repeat == RepeatDaily && slices.Equal(days, []int{0, 1, 2, 3, 4}):
		p.repeat = RepeatWeekdays
	default:
		return true, p.fail(from, k, "does not fit the repeat: say the days once, such as every Monday and Thursday")
	}
	p.i = k
	p.partOfDay()
	return true, nil
}

// dayList reads days from j: day names joined by "and", ",", or "or",
// ranges such as "Monday to Friday" or "mon-fri", "weekdays", and
// "weekends". The days come back in the order of the week, each once.
func (p *parser) dayList(j int) ([]int, int, bool) {
	var set [7]bool
	found := false
	for {
		w := p.at(j)
		switch {
		case w == "weekdays" || w == "weekday":
			for d := range 5 {
				set[d] = true
			}
			j++
		case w == "weekends" || w == "weekend":
			set[5], set[6] = true, true
			j++
		case dayOf(w) >= 0:
			first := dayOf(w)
			set[first] = true
			j++
			if r := p.at(j); (r == "to" || r == "through" || r == "thru" || r == "-") && dayOf(p.at(j+1)) >= 0 {
				for d := first; ; d = (d + 1) % 7 {
					set[d] = true
					if d == dayOf(p.at(j+1)) {
						break
					}
				}
				j += 2
			}
		case strings.Contains(w, "-") && dayOf(strings.SplitN(w, "-", 2)[0]) >= 0 && dayOf(strings.SplitN(w, "-", 2)[1]) >= 0:
			a, b := dayOf(strings.SplitN(w, "-", 2)[0]), dayOf(strings.SplitN(w, "-", 2)[1])
			for d := a; ; d = (d + 1) % 7 {
				set[d] = true
				if d == b {
					break
				}
			}
			j++
		default:
			var out []int
			for d, ok := range set {
				if ok {
					out = append(out, d)
				}
			}
			return out, j, found
		}
		found = true
		sep := j
		if p.at(sep) == "," && (p.at(sep+1) == "and" || p.at(sep+1) == "or") {
			sep++
		}
		if w := p.at(sep); (w == "and" || w == "," || w == "or") && isDayWord(p.at(sep+1)) {
			j = sep + 1
			continue
		}
		var out []int
		for d, ok := range set {
			if ok {
				out = append(out, d)
			}
		}
		return out, j, true
	}
}

// isDayWord reports whether w names days.
func isDayWord(w string) bool {
	return dayOf(w) >= 0 || w == "weekdays" || w == "weekday" || w == "weekends" || w == "weekend" ||
		(strings.Contains(w, "-") && dayOf(strings.SplitN(w, "-", 2)[0]) >= 0)
}

// dayForms are the ways to write a day.
var dayForms = map[string]int{
	"mon": 0, "monday": 0, "mondays": 0,
	"tue": 1, "tues": 1, "tuesday": 1, "tuesdays": 1,
	"wed": 2, "weds": 2, "wednesday": 2, "wednesdays": 2,
	"thu": 3, "thur": 3, "thurs": 3, "thursday": 3, "thursdays": 3,
	"fri": 4, "friday": 4, "fridays": 4,
	"sat": 5, "saturday": 5, "saturdays": 5,
	"sun": 6, "sunday": 6, "sundays": 6,
}

// dayOf returns the day that w names, Monday 0, or -1.
func dayOf(w string) int {
	if d, ok := dayForms[w]; ok {
		return d
	}
	return -1
}

// numberWords are the numbers that a person writes as words.
var numberWords = map[string]int{
	"one": 1, "two": 2, "three": 3, "four": 4, "five": 5, "six": 6, "seven": 7, "eight": 8, "nine": 9, "ten": 10,
	"eleven": 11, "twelve": 12, "fifteen": 15, "twenty": 20, "thirty": 30, "forty-five": 45, "ninety": 90,
}

// number reads a whole number, in digits or in words.
func number(w string) (int, bool) {
	if n, ok := numberWords[w]; ok {
		return n, true
	}
	n, err := strconv.Atoi(w)
	return n, err == nil && n > 0 && n < 10000
}

// dayNumberPattern is a day of the month: 1, 1st, 22nd, 31st.
var dayNumberPattern = regexp.MustCompile(`^([0-9]{1,2})(st|nd|rd|th)?$`)

// dayNumber reads a day of the month.
func dayNumber(w string) (int, bool) {
	m := dayNumberPattern.FindStringSubmatch(w)
	if m == nil {
		return 0, false
	}
	n, _ := strconv.Atoi(m[1])
	return n, n >= 1 && n <= 31
}

// dateClause reads the dates of a schedule: "starting <date>", "from
// <date>", "until <date>", "through <date>".
func (p *parser) dateClause() (bool, error) {
	from := p.i
	var isEnd bool
	n := 1
	switch p.w(0) {
	case "starting", "beginning", "from", "start":
	case "as":
		if p.w(1) != "of" {
			return false, nil
		}
		n = 2
	case "until", "till", "through", "ending", "to", "thru":
		isEnd = true
	default:
		return false, nil
	}
	if p.w(n) == "on" {
		n++
	}
	date, j, ok := p.dateAt(p.i + n)
	if !ok {
		return true, p.fail(from, p.chunkEnd(p.i+n-1), notDate)
	}
	p.i = j
	if isEnd {
		p.end = date
	} else {
		p.start = date
	}
	return true, nil
}

// monthForms are the ways to write a month.
var monthForms = map[string]time.Month{
	"jan": 1, "january": 1, "feb": 2, "february": 2, "mar": 3, "march": 3, "apr": 4, "april": 4,
	"may": 5, "jun": 6, "june": 6, "jul": 7, "july": 7, "aug": 8, "august": 8,
	"sep": 9, "sept": 9, "september": 9, "oct": 10, "october": 10, "nov": 11, "november": 11, "dec": 12, "december": 12,
}

// monthOf returns the month that w names, or 0.
func monthOf(w string) time.Month { return monthForms[w] }

// dateAt reads a date at j: 2026-12-31, December 31, Dec 31st 2026, or
// 31 December 2026. A date without a year is its next occurrence from
// the option Now.
func (p *parser) dateAt(j int) (string, int, bool) {
	if t, err := time.Parse(time.DateOnly, p.at(j)); err == nil {
		return t.Format(time.DateOnly), j + 1, true
	}
	var mo time.Month
	var day int
	k := j
	switch {
	case monthOf(p.at(k)) > 0:
		mo = monthOf(p.at(k))
		d, ok := dayNumber(p.at(k + 1))
		if !ok {
			return "", j, false
		}
		day, k = d, k+2
	default:
		d, ok := dayNumber(p.at(k))
		if !ok || monthOf(p.at(k+1)) == 0 {
			return "", j, false
		}
		day, mo, k = d, monthOf(p.at(k+1)), k+2
	}
	sep := k
	if p.at(sep) == "," {
		sep++
	}
	year := 0
	if y, err := strconv.Atoi(p.at(sep)); err == nil && y >= 2000 && y <= 9999 {
		year, k = y, sep+1
	}
	if year == 0 {
		if p.opt.Now.IsZero() {
			return "", j, false
		}
		loc, err := loadZone(p.zoneOr())
		if err != nil {
			loc = time.UTC
		}
		today := dateOf(p.opt.Now, loc)
		year = today.Year()
		if time.Date(year, mo, day, 0, 0, 0, 0, time.UTC).Before(today) {
			year++
		}
	}
	t := time.Date(year, mo, day, 0, 0, 0, 0, time.UTC)
	if t.Day() != day {
		return "", j, false
	}
	return t.Format(time.DateOnly), k, true
}

// zoneClause reads a time zone: UTC, GMT, or an IANA name such as
// America/New_York, with an optional "in" before it and "time" after it.
func (p *parser) zoneClause() (bool, error) {
	j := p.i
	if p.at(j) == "in" {
		j++
	}
	if j >= len(p.t) {
		return false, nil
	}
	name := ""
	switch raw := p.t[j].raw; {
	case p.at(j) == "utc" || p.at(j) == "gmt":
		name = "UTC"
	case strings.Contains(raw, "/"):
		if _, err := loadZone(raw); err != nil {
			return true, p.fail(p.i, j+1, "is not a time zone: name one such as America/New_York or UTC")
		}
		name = raw
	default:
		return false, nil
	}
	if p.zone != "" {
		return true, p.fail(p.i, j+1, "is a second time zone: keep one")
	}
	p.zone = name
	p.i = j + 1
	if p.w(0) == "time" {
		p.i++
	}
	return true, nil
}

// zoneOr returns the zone of the words, else the zone of the options.
func (p *parser) zoneOr() string {
	if p.zone != "" {
		return p.zone
	}
	return p.opt.Zone
}

// build turns what the words said into a schedule and checks it.
func (p *parser) build() (Schedule, error) {
	if p.repeat == "" {
		switch {
		case len(p.days) > 0:
			p.repeat = RepeatWeekly
		case len(p.times) > 0:
			p.repeat = RepeatDaily
		default:
			return Schedule{}, p.fail(0, len(p.t), notRepeat)
		}
	}
	s := Schedule{Repeat: p.repeat, DayOfMonth: p.dom, TimeZone: p.zone, Start: p.start, End: p.end}
	for _, d := range p.days {
		s.Days = append(s.Days, dayNames[d])
	}
	switch p.repeat {
	case RepeatWeekly:
		if len(p.days) == 0 {
			return Schedule{}, &SayError{Clear: p.echo(0, len(p.t), true), Problem: "needs days: say them, such as every Monday and Thursday"}
		}
		s.EveryWeeks = max(p.weeks, 1)
	case RepeatMonthly:
		if s.DayOfMonth == "" {
			s.DayOfMonth = "1"
		}
	case RepeatEvery:
		s.Every = stepText(p.step)
	}
	if !withTimes(p.repeat) && len(p.times) > 0 {
		return Schedule{}, p.fail(p.timesAt, p.timesEnd, "does not fit a repeat of steps through the day: say a window, such as between 8am and 6pm")
	}
	for _, m := range p.times {
		s.Times = append(s.Times, clockText(m))
	}
	if p.between != nil {
		if withTimes(p.repeat) {
			return Schedule{}, &SayError{Clear: p.echo(0, len(p.t), true), Problem: "has a window of the day, which goes with hourly or every 2 hours: say times with at instead"}
		}
		s.Between = []string{clockText(p.between[0]), clockText(p.between[1])}
	}
	if s.EveryWeeks > 1 && s.Start == "" && !p.opt.Now.IsZero() {
		loc, err := loadZone(p.zoneOr())
		if err != nil {
			loc = time.UTC
		}
		s.Start = dateOf(p.opt.Now, loc).Format(time.DateOnly)
	}
	s = s.Normalize(p.opt.Zone)
	if problems := s.Check(); len(problems) > 0 {
		return Schedule{}, fmt.Errorf("%s", strings.Join(problems, " "))
	}
	return s, nil
}

// SayErrorAs returns the *SayError in err, or nil.
func SayErrorAs(err error) *SayError {
	var e *SayError
	if errors.As(err, &e) {
		return e
	}
	return nil
}
