// Package schedule computes the times of a duty from a schedule that
// matches how people say a time (docs/ARCHITECTURE.md section 22.2). A
// person never sees cron: a schedule is a small record, such as "weekdays
// at 09:00 in America/New_York", Parse reads the words of a person into
// that record with a fixed grammar, Readback says the record in words, and
// Next lists its times in its time zone. The package calls no model, so
// the same words always give the same schedule, and it reads no clock: the
// caller passes the time.
//
// Each time is a wall-clock time in the time zone of the schedule. A time
// that a change to summer time skips runs at the first minute after the
// gap, and a time that the change back repeats runs once, at its first
// occurrence.
package schedule

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Repeats of a schedule.
const (
	RepeatDaily    = "daily"    // every day at the times
	RepeatWeekdays = "weekdays" // Monday to Friday at the times
	RepeatWeekly   = "weekly"   // on the days, every everyWeeks weeks, at the times
	RepeatMonthly  = "monthly"  // on dayOfMonth at the times
	RepeatHourly   = "hourly"   // every hour, within between, on the days
	RepeatEvery    = "every"    // every step of every, within between, on the days
)

// Limits and defaults of a schedule.
const (
	// MinEvery and MaxEvery bound the step of the repeat every.
	MinEvery = 15 * time.Minute
	MaxEvery = 24 * time.Hour
	// MaxTimes is the most times of one day.
	MaxTimes = 12
	// MaxEveryWeeks is the longest step of the repeat weekly, in weeks.
	MaxEveryWeeks = 4
	// DefaultTime is the time of a day that a schedule with a day and no
	// time gets, such as "on the last day of the month".
	DefaultTime = "09:00"
	// DefaultZone is the time zone of a schedule that names none and has
	// no default of its caller.
	DefaultZone = "UTC"
)

// maxScanDays bounds the days that Next and Last read: two years holds
// five times of any schedule that the checks take.
const maxScanDays = 800

// Schedule is when a duty runs, as the record of a duty holds it. Repeat
// is one of the Repeat constants. Days are "mon" to "sun". EveryWeeks is
// 1 to 4 with weekly. DayOfMonth is "1" to "31", "last", or an ordinal and
// a day, such as "first monday" or "last friday", with monthly. Times are
// "HH:MM" in 24 hours. Every is a step from 15m to 24h, with every.
// Between is two times that bound hourly and every, both included.
// TimeZone is an IANA name. Start and End are dates, "2006-01-02", both
// included.
type Schedule struct {
	Repeat     string   `json:"repeat"`
	Days       []string `json:"days,omitempty"`
	EveryWeeks int      `json:"everyWeeks,omitempty"`
	DayOfMonth string   `json:"dayOfMonth,omitempty"`
	Times      []string `json:"times,omitempty"`
	Every      string   `json:"every,omitempty"`
	Between    []string `json:"between,omitempty"`
	TimeZone   string   `json:"timeZone,omitempty"`
	Start      string   `json:"start,omitempty"`
	End        string   `json:"end,omitempty"`
}

// dayNames are the days of a schedule, Monday first.
var dayNames = [7]string{"mon", "tue", "wed", "thu", "fri", "sat", "sun"}

// ordinals are the words of a weekday of the month, with -1 for last.
var ordinals = map[string]int{"first": 1, "second": 2, "third": 3, "fourth": 4, "last": -1}

// clockPattern is a time of the day in 24 hours.
var clockPattern = regexp.MustCompile(`^([01]?[0-9]|2[0-3]):([0-5][0-9])$`)

// fullDays are the days of a day of the month, such as "first monday".
var fullDays = [7]string{"monday", "tuesday", "wednesday", "thursday", "friday", "saturday", "sunday"}

// dayIndex returns the index of a day, Monday 0, or -1. It takes the
// usual ways to write a day, such as mon, Monday, or thurs.
func dayIndex(s string) int { return dayOf(strings.ToLower(strings.TrimSpace(s))) }

// clock reads "HH:MM" as minutes of the day.
func clock(s string) (int, bool) {
	m := clockPattern.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return 0, false
	}
	h, _ := strconv.Atoi(m[1])
	mi, _ := strconv.Atoi(m[2])
	return h*60 + mi, true
}

// clockText writes minutes of the day as "HH:MM".
func clockText(m int) string { return fmt.Sprintf("%02d:%02d", m/60, m%60) }

// stepText writes a step as hours and minutes, such as 15m, 2h, or 1h30m.
func stepText(d time.Duration) string {
	h, m := int(d/time.Hour), int(d%time.Hour/time.Minute)
	switch {
	case h == 0:
		return fmt.Sprintf("%dm", m)
	case m == 0:
		return fmt.Sprintf("%dh", h)
	}
	return fmt.Sprintf("%dh%dm", h, m)
}

// Normalize returns the schedule in its usual form: the names in lower
// case, the days in the order of the week, the times sorted, each value
// once, the step as hours and minutes, and the defaults: zone when the
// schedule names no time zone (DefaultZone when zone is empty), one week
// with weekly, and DefaultTime for a repeat with times that names none.
// A value that does not read stays as it is, so Check names it.
func (s Schedule) Normalize(zone string) Schedule {
	s.Repeat = strings.ToLower(strings.TrimSpace(s.Repeat))
	if len(s.Days) > 0 {
		seen := [7]bool{}
		var other []string
		for _, d := range s.Days {
			if i := dayIndex(d); i >= 0 {
				seen[i] = true
			} else {
				other = append(other, d)
			}
		}
		days := []string{}
		for i, ok := range seen {
			if ok {
				days = append(days, dayNames[i])
			}
		}
		s.Days = append(days, other...)
	}
	if s.Repeat == RepeatWeekly && s.EveryWeeks == 0 {
		s.EveryWeeks = 1
	}
	s.DayOfMonth = strings.Join(strings.Fields(strings.ToLower(s.DayOfMonth)), " ")
	if n, err := strconv.Atoi(s.DayOfMonth); err == nil {
		s.DayOfMonth = strconv.Itoa(n)
	} else if word, day, ok := strings.Cut(s.DayOfMonth, " "); ok && dayIndex(day) >= 0 {
		s.DayOfMonth = word + " " + fullDays[dayIndex(day)]
	}
	if len(s.Times) > 0 {
		var ms []int
		var other []string
		for _, t := range s.Times {
			if m, ok := clock(t); ok {
				ms = append(ms, m)
			} else {
				other = append(other, t)
			}
		}
		slices.Sort(ms)
		ms = slices.Compact(ms)
		times := make([]string, 0, len(ms)+len(other))
		for _, m := range ms {
			times = append(times, clockText(m))
		}
		s.Times = append(times, other...)
	}
	if withTimes(s.Repeat) && len(s.Times) == 0 {
		s.Times = []string{DefaultTime}
	}
	if s.Every != "" {
		if d, err := time.ParseDuration(strings.TrimSpace(s.Every)); err == nil && d > 0 && d%time.Minute == 0 {
			s.Every = stepText(d)
		}
	}
	for i, b := range s.Between {
		if m, ok := clock(b); ok {
			s.Between[i] = clockText(m)
		}
	}
	s.TimeZone = strings.TrimSpace(s.TimeZone)
	if s.TimeZone == "" {
		s.TimeZone = zone
	}
	if s.TimeZone == "" {
		s.TimeZone = DefaultZone
	}
	s.Start, s.End = strings.TrimSpace(s.Start), strings.TrimSpace(s.End)
	return s
}

// withTimes reports whether a repeat runs at the times of a day.
func withTimes(repeat string) bool {
	switch repeat {
	case RepeatDaily, RepeatWeekdays, RepeatWeekly, RepeatMonthly:
		return true
	}
	return false
}

// Check lists the problems of a normalized schedule in words, each as a
// sentence that names the field. A schedule without problems has a Plan.
func (s Schedule) Check() []string {
	var p []string
	add := func(format string, args ...any) { p = append(p, fmt.Sprintf(format, args...)) }
	switch s.Repeat {
	case RepeatDaily, RepeatWeekdays, RepeatWeekly, RepeatMonthly, RepeatHourly, RepeatEvery:
	case "":
		add("repeat is required: daily, weekdays, weekly, monthly, hourly, or every.")
	default:
		add("repeat is %q: pick daily, weekdays, weekly, monthly, hourly, or every.", s.Repeat)
	}
	for _, d := range s.Days {
		if dayIndex(d) < 0 {
			add("days holds %q, which is not a day: use mon, tue, wed, thu, fri, sat, or sun.", d)
		}
	}
	switch {
	case s.Repeat == RepeatWeekly && len(s.Days) == 0:
		add("weekly needs days, such as [\"mon\", \"thu\"].")
	case len(s.Days) > 0 && s.Repeat != RepeatWeekly && s.Repeat != RepeatHourly && s.Repeat != RepeatEvery:
		add("days goes with weekly, hourly, or every, and the repeat is %s.", s.Repeat)
	}
	switch {
	case s.Repeat == RepeatWeekly && (s.EveryWeeks < 1 || s.EveryWeeks > MaxEveryWeeks):
		add("everyWeeks is %d: pick 1 to %d.", s.EveryWeeks, MaxEveryWeeks)
	case s.Repeat != RepeatWeekly && s.EveryWeeks != 0:
		add("everyWeeks goes with weekly, and the repeat is %s.", s.Repeat)
	}
	switch {
	case s.Repeat == RepeatMonthly && s.DayOfMonth == "":
		add("monthly needs dayOfMonth: 1 to 31, last, or a day such as first monday.")
	case s.Repeat == RepeatMonthly:
		if _, err := readDayOfMonth(s.DayOfMonth); err != nil {
			add("dayOfMonth is %q: %s.", s.DayOfMonth, err)
		}
	case s.DayOfMonth != "":
		add("dayOfMonth goes with monthly, and the repeat is %s.", s.Repeat)
	}
	switch {
	case withTimes(s.Repeat):
		if len(s.Times) > MaxTimes {
			add("times holds %d times: a day takes at most %d.", len(s.Times), MaxTimes)
		}
		for _, t := range s.Times {
			if _, ok := clock(t); !ok {
				add("times holds %q, which is not a time of the day: write HH:MM in 24 hours, such as 09:00 or 16:30.", t)
			}
		}
	case len(s.Times) > 0 && s.Repeat != "":
		add("times goes with daily, weekdays, weekly, or monthly. The repeat %s runs at steps through the day, so set between instead.", s.Repeat)
	}
	switch {
	case s.Repeat == RepeatEvery && s.Every == "":
		add("every needs a step from 15m to 24h, such as 15m or 2h.")
	case s.Repeat == RepeatEvery:
		if _, err := readStep(s.Every); err != nil {
			add("every is %q: %s.", s.Every, err)
		}
	case s.Every != "":
		add("every goes with the repeat every, and the repeat is %s.", s.Repeat)
	}
	if len(s.Between) > 0 {
		switch {
		case s.Repeat != RepeatHourly && s.Repeat != RepeatEvery:
			add("between goes with hourly or every, and the repeat is %s.", s.Repeat)
		case len(s.Between) != 2:
			add("between holds %d times: give two, such as [\"08:00\", \"18:00\"].", len(s.Between))
		default:
			from, ok1 := clock(s.Between[0])
			to, ok2 := clock(s.Between[1])
			switch {
			case !ok1 || !ok2:
				add("between holds a value that is not a time of the day: write HH:MM in 24 hours.")
			case from >= to:
				add("between starts at %s and ends at %s: it must start before it ends on the same day.", s.Between[0], s.Between[1])
			}
		}
	}
	if _, err := loadZone(s.TimeZone); err != nil {
		add("timeZone is %q: %s.", s.TimeZone, err)
	}
	start, err1 := readDate(s.Start)
	if err1 != nil {
		add("start is %q: write a date as 2006-01-02.", s.Start)
	}
	end, err2 := readDate(s.End)
	if err2 != nil {
		add("end is %q: write a date as 2006-01-02.", s.End)
	}
	if err1 == nil && err2 == nil && !start.IsZero() && !end.IsZero() && end.Before(start) {
		add("end %s is before start %s.", s.End, s.Start)
	}
	return p
}

// loadZone loads an IANA time zone. Local names the zone of the host, so
// it is not a name of a schedule.
func loadZone(name string) (*time.Location, error) {
	if name == "" || strings.EqualFold(name, "Local") {
		return nil, fmt.Errorf("name an IANA time zone, such as America/New_York or UTC")
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil, fmt.Errorf("it is not an IANA time zone, such as America/New_York or UTC")
	}
	return loc, nil
}

// readDate reads a date, or the zero time for "".
func readDate(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	return time.Parse(time.DateOnly, s)
}

// readStep reads the step of the repeat every.
func readStep(s string) (time.Duration, error) {
	d, err := time.ParseDuration(strings.TrimSpace(s))
	switch {
	case err != nil:
		return 0, fmt.Errorf("write a step such as 15m, 2h, or 1h30m")
	case d%time.Minute != 0:
		return 0, fmt.Errorf("a step is a whole number of minutes")
	case d < MinEvery:
		return 0, fmt.Errorf("the shortest step is 15 minutes")
	case d > MaxEvery:
		return 0, fmt.Errorf("the longest step is 24 hours")
	}
	return d, nil
}

// monthDay is a day of the month: a number, the last day, or the nth
// weekday, with nth -1 for the last one.
type monthDay struct {
	day     int // 1 to 31, or 0
	last    bool
	nth     int // 1 to 4, or -1
	weekday int // Monday 0
}

// readDayOfMonth reads dayOfMonth.
func readDayOfMonth(s string) (monthDay, error) {
	if s == "last" {
		return monthDay{last: true}, nil
	}
	if n, err := strconv.Atoi(s); err == nil {
		if n < 1 || n > 31 {
			return monthDay{}, fmt.Errorf("a day of the month is 1 to 31")
		}
		return monthDay{day: n}, nil
	}
	word, day, ok := strings.Cut(s, " ")
	nth, isOrd := ordinals[word]
	wd := dayIndex(day)
	if !ok || !isOrd || wd < 0 {
		return monthDay{}, fmt.Errorf("write 1 to 31, last, or first, second, third, fourth, or last with a day, such as first monday")
	}
	return monthDay{nth: nth, weekday: wd}, nil
}

// Plan is a schedule that passed its checks, ready to give its times.
type Plan struct {
	s       Schedule
	loc     *time.Location
	repeat  string
	days    [7]bool // the days of weekly, or the filter of hourly and every
	anyDay  bool    // hourly or every without days
	weeks   int
	anchor  time.Time // the Monday of the first week of everyWeeks
	month   monthDay
	slots   []int // the minutes of a day, ascending
	start   time.Time
	end     time.Time
	hasEnd  bool
	hasFrom bool
}

// Plan returns the plan of a schedule, or an error that lists its
// problems. The schedule is normalized first, without a default zone.
func (s Schedule) Plan() (*Plan, error) {
	s = s.Normalize("")
	if p := s.Check(); len(p) > 0 {
		return nil, fmt.Errorf("the schedule does not fit: %s", strings.Join(p, " "))
	}
	loc, _ := loadZone(s.TimeZone)
	p := &Plan{s: s, loc: loc, repeat: s.Repeat, weeks: max(s.EveryWeeks, 1)}
	for _, d := range s.Days {
		p.days[dayIndex(d)] = true
	}
	p.anyDay = len(s.Days) == 0
	if s.Repeat == RepeatMonthly {
		p.month, _ = readDayOfMonth(s.DayOfMonth)
	}
	p.start, _ = readDate(s.Start)
	p.hasFrom = !p.start.IsZero()
	p.end, _ = readDate(s.End)
	p.hasEnd = !p.end.IsZero()
	switch s.Repeat {
	case RepeatHourly, RepeatEvery:
		step := time.Hour
		if s.Repeat == RepeatEvery {
			step, _ = readStep(s.Every)
		}
		from, to := 0, 24*60-1
		if len(s.Between) == 2 {
			from, _ = clock(s.Between[0])
			to, _ = clock(s.Between[1])
		}
		for m := from; m <= to; m += int(step / time.Minute) {
			p.slots = append(p.slots, m)
		}
	default:
		for _, t := range s.Times {
			m, _ := clock(t)
			p.slots = append(p.slots, m)
		}
	}
	if p.repeat == RepeatWeekly && p.weeks > 1 {
		// The weeks count from the week of the first day of the schedule
		// on or after its start, so "every other Tuesday" from a Friday
		// runs on the next Tuesday. Without a start, they count from
		// Monday, January 1, 2001.
		from := p.start
		if from.IsZero() {
			from = time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)
		}
		for i := 0; i < 7 && !p.days[weekdayOf(from)]; i++ {
			from = from.AddDate(0, 0, 1)
		}
		p.anchor = mondayOf(from)
	}
	return p, nil
}

// Schedule returns the normalized schedule of the plan.
func (p *Plan) Schedule() Schedule { return p.s }

// Location returns the time zone of the plan.
func (p *Plan) Location() *time.Location { return p.loc }

// weekdayOf returns the day of a date, Monday 0.
func weekdayOf(d time.Time) int { return (int(d.Weekday()) + 6) % 7 }

// mondayOf returns the Monday of the week of a date.
func mondayOf(d time.Time) time.Time { return d.AddDate(0, 0, -weekdayOf(d)) }

// dateOf returns the date of t in loc, at midnight UTC, the form of a
// date in this package.
func dateOf(t time.Time, loc *time.Location) time.Time {
	y, m, d := t.In(loc).Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

// matches reports whether the schedule runs on a date.
func (p *Plan) matches(d time.Time) bool {
	if (p.hasFrom && d.Before(p.start)) || (p.hasEnd && d.After(p.end)) {
		return false
	}
	wd := weekdayOf(d)
	switch p.repeat {
	case RepeatDaily:
		return true
	case RepeatWeekdays:
		return wd < 5
	case RepeatWeekly:
		if !p.days[wd] {
			return false
		}
		if p.weeks <= 1 {
			return true
		}
		days := int(mondayOf(d).Sub(p.anchor).Hours() / 24)
		return days >= 0 && (days/7)%p.weeks == 0
	case RepeatMonthly:
		return p.month.on(d)
	}
	return p.anyDay || p.days[wd]
}

// on reports whether a date is the day of the month.
func (md monthDay) on(d time.Time) bool {
	last := time.Date(d.Year(), d.Month()+1, 0, 0, 0, 0, 0, time.UTC).Day()
	switch {
	case md.last:
		return d.Day() == last
	case md.day > 0:
		return d.Day() == md.day
	case weekdayOf(d) != md.weekday:
		return false
	case md.nth > 0:
		return (d.Day()-1)/7+1 == md.nth
	}
	return d.Day()+7 > last
}

// at returns the instant of the minute m of the date d in the zone of the
// plan, by the rules of the package: a time in the gap of a change to
// summer time gives the first instant after the gap, and a time that the
// change back repeats gives its first occurrence.
func (p *Plan) at(d time.Time, m int) time.Time {
	return resolve(d.Year(), d.Month(), d.Day(), m/60, m%60, p.loc)
}

// resolve returns the instant of a wall-clock time in loc by the rules of
// the package.
func resolve(y int, mo time.Month, d, h, mi int, loc *time.Location) time.Time {
	wall := time.Date(y, mo, d, h, mi, 0, 0, time.UTC)
	same := func(t time.Time) bool {
		ly, lmo, ld := t.Date()
		return ly == y && lmo == mo && ld == d && t.Hour() == h && t.Minute() == mi
	}
	var first, earliest time.Time
	found := false
	// The offsets of the zone a day before and a day after the wall time
	// cover every change of a real zone.
	for _, probe := range []time.Time{wall.Add(-26 * time.Hour), wall, wall.Add(26 * time.Hour)} {
		_, off := probe.In(loc).Zone()
		u := wall.Add(-time.Duration(off) * time.Second)
		if earliest.IsZero() || u.Before(earliest) {
			earliest = u
		}
		if same(u.In(loc)) && (!found || u.Before(first)) {
			first, found = u, true
		}
	}
	if found {
		return first
	}
	// The wall time falls in a gap. The earliest candidate lies before the
	// change, and the change is the first instant after the gap.
	if _, end := earliest.In(loc).ZoneBounds(); !end.IsZero() && end.After(earliest) {
		return end
	}
	return earliest
}

// Next returns up to n times of the plan after the instant after, in
// order, as instants. A schedule whose end has passed returns none.
func (p *Plan) Next(after time.Time, n int) []time.Time {
	var out []time.Time
	if n <= 0 {
		return out
	}
	d := dateOf(after, p.loc)
	if p.hasFrom && d.Before(p.start) {
		d = p.start
	}
	for range maxScanDays {
		if p.hasEnd && d.After(p.end) {
			break
		}
		if p.matches(d) {
			for _, m := range p.slots {
				t := p.at(d, m)
				if !t.After(after) || (len(out) > 0 && !t.After(out[len(out)-1])) {
					continue
				}
				out = append(out, t)
				if len(out) == n {
					return out
				}
			}
		}
		d = d.AddDate(0, 0, 1)
	}
	return out
}

// Last returns the newest time of the plan at or before the instant at,
// and false when the plan has none in the two years before at.
func (p *Plan) Last(at time.Time) (time.Time, bool) {
	d := dateOf(at, p.loc)
	if p.hasEnd && d.After(p.end) {
		d = p.end
	}
	for range maxScanDays {
		if p.hasFrom && d.Before(p.start) {
			break
		}
		if p.matches(d) {
			for i := len(p.slots) - 1; i >= 0; i-- {
				if t := p.at(d, p.slots[i]); !t.After(at) {
					return t, true
				}
			}
		}
		d = d.AddDate(0, 0, -1)
	}
	return time.Time{}, false
}

// FirstDay returns the first date of the plan on or after its start, at
// midnight UTC, or the zero time without a start. The readback names it.
func (p *Plan) FirstDay() time.Time {
	if !p.hasFrom {
		return time.Time{}
	}
	d := p.start
	for range maxScanDays {
		if p.hasEnd && d.After(p.end) {
			break
		}
		if p.matches(d) {
			return d
		}
		d = d.AddDate(0, 0, 1)
	}
	return p.start
}
