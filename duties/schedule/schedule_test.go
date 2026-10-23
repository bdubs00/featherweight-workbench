package schedule

import (
	"slices"
	"strings"
	"testing"
	"time"
)

// plan returns the plan of a schedule or fails the test.
func plan(t *testing.T, s Schedule) *Plan {
	t.Helper()
	p, err := s.Plan()
	if err != nil {
		t.Fatalf("Plan(%+v) = %v", s, err)
	}
	return p
}

// utc reads an RFC 3339 time.
func utc(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// texts writes instants in UTC as RFC 3339.
func texts(ts []time.Time) []string {
	out := make([]string, 0, len(ts))
	for _, t := range ts {
		out = append(out, t.UTC().Format(time.RFC3339))
	}
	return out
}

func wantNext(t *testing.T, s Schedule, after string, n int, want ...string) {
	t.Helper()
	got := texts(plan(t, s).Next(utc(t, after), n))
	if !slices.Equal(got, want) {
		t.Fatalf("Next(%+v, %s) =\n%v\nwant\n%v", s, after, got, want)
	}
}

const newYork = "America/New_York"

// A time that the change to summer time skips runs at the first minute
// after the gap, once, and the day after keeps its time.
func TestNextSpringForward(t *testing.T) {
	// In 2026, New York moves from 2:00 EST to 3:00 EDT on March 8.
	wantNext(t, Schedule{Repeat: RepeatDaily, Times: []string{"02:30"}, TimeZone: newYork}, "2026-03-07T00:00:00Z", 3,
		"2026-03-07T07:30:00Z", // 2:30 EST
		"2026-03-08T07:00:00Z", // 3:00 EDT, the first minute after the gap
		"2026-03-09T06:30:00Z") // 2:30 EDT
	// Every 15 minutes: the four times of the gap and 3:00 run once, at 3:00.
	wantNext(t, Schedule{Repeat: RepeatEvery, Every: "15m", Between: []string{"01:30", "03:30"}, TimeZone: newYork}, "2026-03-08T00:00:00Z", 6,
		"2026-03-08T06:30:00Z", // 1:30 EST
		"2026-03-08T06:45:00Z", // 1:45 EST
		"2026-03-08T07:00:00Z", // 3:00 EDT
		"2026-03-08T07:15:00Z", // 3:15 EDT
		"2026-03-08T07:30:00Z", // 3:30 EDT
		"2026-03-09T05:30:00Z") // 1:30 EDT the next day
}

// A time that the change back repeats runs once, at its first occurrence.
func TestNextFallBack(t *testing.T) {
	// In 2026, New York moves from 2:00 EDT back to 1:00 EST on November 1.
	wantNext(t, Schedule{Repeat: RepeatDaily, Times: []string{"01:30"}, TimeZone: newYork}, "2026-10-31T12:00:00Z", 2,
		"2026-11-01T05:30:00Z", // 1:30 EDT, the first 1:30
		"2026-11-02T06:30:00Z") // 1:30 EST
	// The second 1:30 of the night does not run.
	wantNext(t, Schedule{Repeat: RepeatDaily, Times: []string{"01:30"}, TimeZone: newYork}, "2026-11-01T05:30:00Z", 1,
		"2026-11-02T06:30:00Z")
	wantNext(t, Schedule{Repeat: RepeatEvery, Every: "30m", Between: []string{"00:30", "03:00"}, TimeZone: newYork}, "2026-11-01T00:00:00Z", 7,
		"2026-11-01T04:30:00Z", // 0:30 EDT
		"2026-11-01T05:00:00Z", // 1:00 EDT
		"2026-11-01T05:30:00Z", // 1:30 EDT
		"2026-11-01T07:00:00Z", // 2:00 EST, an hour later: the repeated hour ran once
		"2026-11-01T07:30:00Z",
		"2026-11-01T08:00:00Z",
		"2026-11-02T05:30:00Z")
	// A start inside the repeated hour finds no second run of it.
	wantNext(t, Schedule{Repeat: RepeatHourly, TimeZone: newYork}, "2026-11-01T06:10:00Z", 1, "2026-11-01T07:00:00Z")
}

// London changes at 1:00 UTC, an hour that is not the hour of New York.
func TestNextLondon(t *testing.T) {
	wantNext(t, Schedule{Repeat: RepeatDaily, Times: []string{"01:15"}, TimeZone: "Europe/London"}, "2026-03-28T12:00:00Z", 2,
		"2026-03-29T01:00:00Z", // 1:15 GMT is in the gap: 2:00 BST
		"2026-03-30T00:15:00Z")
}

// The days of the month: a number that a month lacks, the last day in a
// leap year, and a weekday of the month.
func TestNextMonthEnds(t *testing.T) {
	wantNext(t, Schedule{Repeat: RepeatMonthly, DayOfMonth: "31", Times: []string{"09:00"}}, "2026-01-01T00:00:00Z", 5,
		"2026-01-31T09:00:00Z", "2026-03-31T09:00:00Z", "2026-05-31T09:00:00Z", "2026-07-31T09:00:00Z", "2026-08-31T09:00:00Z")
	wantNext(t, Schedule{Repeat: RepeatMonthly, DayOfMonth: "last", Times: []string{"09:00"}}, "2027-12-31T10:00:00Z", 3,
		"2028-01-31T09:00:00Z", "2028-02-29T09:00:00Z", "2028-03-31T09:00:00Z")
	wantNext(t, Schedule{Repeat: RepeatMonthly, DayOfMonth: "29"}, "2026-01-30T00:00:00Z", 1, "2026-03-29T09:00:00Z")
	wantNext(t, Schedule{Repeat: RepeatMonthly, DayOfMonth: "first monday", Times: []string{"09:00"}, TimeZone: newYork}, "2026-10-23T00:00:00Z", 3,
		"2026-11-02T14:00:00Z", "2026-12-07T14:00:00Z", "2027-01-04T14:00:00Z")
	wantNext(t, Schedule{Repeat: RepeatMonthly, DayOfMonth: "last friday", Times: []string{"17:00"}}, "2026-10-01T00:00:00Z", 3,
		"2026-10-30T17:00:00Z", "2026-11-27T17:00:00Z", "2026-12-25T17:00:00Z")
	wantNext(t, Schedule{Repeat: RepeatMonthly, DayOfMonth: "fourth thursday", Times: []string{"12:00"}}, "2026-11-01T00:00:00Z", 1,
		"2026-11-26T12:00:00Z")
}

// Every other week counts its weeks from the first day of the schedule on
// or after its start.
func TestNextEveryWeeks(t *testing.T) {
	wantNext(t, Schedule{Repeat: RepeatWeekly, Days: []string{"tue"}, EveryWeeks: 2, Times: []string{"15:00"}, TimeZone: newYork, Start: "2026-10-23"}, "2026-10-23T12:00:00Z", 4,
		"2026-10-27T19:00:00Z", "2026-11-10T20:00:00Z", "2026-11-24T20:00:00Z", "2026-12-08T20:00:00Z")
	wantNext(t, Schedule{Repeat: RepeatWeekly, Days: []string{"mon", "thu"}, EveryWeeks: 2, Times: []string{"08:00"}, Start: "2026-10-20"}, "2026-10-01T00:00:00Z", 5,
		"2026-10-22T08:00:00Z", "2026-11-02T08:00:00Z", "2026-11-05T08:00:00Z", "2026-11-16T08:00:00Z", "2026-11-19T08:00:00Z")
	wantNext(t, Schedule{Repeat: RepeatWeekly, Days: []string{"fri"}, EveryWeeks: 4, Times: []string{"10:00"}, Start: "2026-10-23"}, "2026-10-23T11:00:00Z", 2,
		"2026-11-20T10:00:00Z", "2026-12-18T10:00:00Z")
}

// Hourly and every run at steps within the window, both ends included, on
// the days of their filter.
func TestNextSteps(t *testing.T) {
	wantNext(t, Schedule{Repeat: RepeatEvery, Every: "2h", Between: []string{"08:00", "18:00"}}, "2026-10-23T07:00:00Z", 7,
		"2026-10-23T08:00:00Z", "2026-10-23T10:00:00Z", "2026-10-23T12:00:00Z", "2026-10-23T14:00:00Z", "2026-10-23T16:00:00Z", "2026-10-23T18:00:00Z", "2026-10-24T08:00:00Z")
	// Friday night: the next hour of a weekday is Monday at midnight.
	wantNext(t, Schedule{Repeat: RepeatHourly, Days: []string{"mon", "tue", "wed", "thu", "fri"}}, "2026-10-23T22:30:00Z", 2,
		"2026-10-23T23:00:00Z", "2026-10-26T00:00:00Z")
	wantNext(t, Schedule{Repeat: RepeatEvery, Every: "15m"}, "2026-10-23T09:07:00Z", 2, "2026-10-23T09:15:00Z", "2026-10-23T09:30:00Z")
	wantNext(t, Schedule{Repeat: RepeatEvery, Every: "7h"}, "2026-10-23T20:00:00Z", 2, "2026-10-23T21:00:00Z", "2026-10-24T00:00:00Z")
	wantNext(t, Schedule{Repeat: RepeatWeekdays, Times: []string{"16:00", "09:30"}}, "2026-10-23T12:00:00Z", 3,
		"2026-10-23T16:00:00Z", "2026-10-26T09:30:00Z", "2026-10-26T16:00:00Z")
}

// Start and end bound the dates, both included, in the time zone of the
// schedule.
func TestNextStartEnd(t *testing.T) {
	s := Schedule{Repeat: RepeatDaily, Times: []string{"23:30"}, TimeZone: newYork, Start: "2026-11-01", End: "2026-11-03"}
	wantNext(t, s, "2026-10-01T00:00:00Z", 5, "2026-11-02T04:30:00Z", "2026-11-03T04:30:00Z", "2026-11-04T04:30:00Z")
	wantNext(t, s, "2026-11-05T00:00:00Z", 5)
}

// Last finds the newest time at or before an instant.
func TestLast(t *testing.T) {
	p := plan(t, Schedule{Repeat: RepeatDaily, Times: []string{"09:00", "17:00"}})
	for _, c := range []struct{ at, want string }{
		{"2026-10-23T10:00:00Z", "2026-10-23T09:00:00Z"},
		{"2026-10-23T09:00:00Z", "2026-10-23T09:00:00Z"},
		{"2026-10-23T08:59:59Z", "2026-10-22T17:00:00Z"},
	} {
		got, ok := p.Last(utc(t, c.at))
		if !ok || got.UTC().Format(time.RFC3339) != c.want {
			t.Errorf("Last(%s) = %v, %v; want %s", c.at, got, ok, c.want)
		}
	}
	future := plan(t, Schedule{Repeat: RepeatDaily, Start: "2027-01-01"})
	if got, ok := future.Last(utc(t, "2026-10-23T10:00:00Z")); ok {
		t.Fatalf("Last before the start = %v", got)
	}
	ended := plan(t, Schedule{Repeat: RepeatDaily, End: "2026-10-01"})
	if got, ok := ended.Last(utc(t, "2026-10-23T10:00:00Z")); !ok || got.Format(time.DateOnly) != "2026-10-01" {
		t.Fatalf("Last after the end = %v, %v", got, ok)
	}
	// In the gap of New York the time of the gap ran at 3:00 EDT.
	ny := plan(t, Schedule{Repeat: RepeatDaily, Times: []string{"02:30"}, TimeZone: newYork})
	if got, _ := ny.Last(utc(t, "2026-03-08T12:00:00Z")); got.UTC().Format(time.RFC3339) != "2026-03-08T07:00:00Z" {
		t.Fatalf("Last on the day of the gap = %v", got)
	}
}

// Normalize fills the defaults and puts each value in its usual form.
func TestNormalize(t *testing.T) {
	s := Schedule{Repeat: " Weekly ", Days: []string{"Thu", "mon", "thu"}, Times: []string{"16:00", "9:30", "16:00"}, Every: ""}.Normalize("Europe/Paris")
	if s.Repeat != RepeatWeekly || !slices.Equal(s.Days, []string{"mon", "thu"}) || !slices.Equal(s.Times, []string{"09:30", "16:00"}) || s.EveryWeeks != 1 || s.TimeZone != "Europe/Paris" {
		t.Fatalf("Normalize = %+v", s)
	}
	if s := (Schedule{Repeat: RepeatMonthly, DayOfMonth: " First  MONDAY "}).Normalize(""); s.DayOfMonth != "first monday" || s.TimeZone != DefaultZone || !slices.Equal(s.Times, []string{DefaultTime}) {
		t.Fatalf("Normalize of monthly = %+v", s)
	}
	if s := (Schedule{Repeat: RepeatEvery, Every: "90m"}).Normalize(""); s.Every != "1h30m" || s.Times != nil {
		t.Fatalf("Normalize of every = %+v", s)
	}
}

// Check names each problem with its field.
func TestCheck(t *testing.T) {
	for _, c := range []struct {
		s    Schedule
		want string
	}{
		{Schedule{}, "repeat is required"},
		{Schedule{Repeat: "yearly"}, `repeat is "yearly"`},
		{Schedule{Repeat: RepeatWeekly}, "weekly needs days"},
		{Schedule{Repeat: RepeatWeekly, Days: []string{"funday"}}, `"funday", which is not a day`},
		{Schedule{Repeat: RepeatDaily, Days: []string{"mon"}}, "days goes with weekly, hourly, or every"},
		{Schedule{Repeat: RepeatWeekly, Days: []string{"mon"}, EveryWeeks: 5}, "everyWeeks is 5: pick 1 to 4"},
		{Schedule{Repeat: RepeatDaily, EveryWeeks: 2}, "everyWeeks goes with weekly"},
		{Schedule{Repeat: RepeatMonthly}, "monthly needs dayOfMonth"},
		{Schedule{Repeat: RepeatMonthly, DayOfMonth: "32"}, "a day of the month is 1 to 31"},
		{Schedule{Repeat: RepeatMonthly, DayOfMonth: "fifth monday"}, "first monday"},
		{Schedule{Repeat: RepeatDaily, DayOfMonth: "1"}, "dayOfMonth goes with monthly"},
		{Schedule{Repeat: RepeatDaily, Times: []string{"25:00"}}, `"25:00", which is not a time of the day`},
		{Schedule{Repeat: RepeatDaily, Times: strings.Split("00:00 01:00 02:00 03:00 04:00 05:00 06:00 07:00 08:00 09:00 10:00 11:00 12:00", " ")}, "at most 12"},
		{Schedule{Repeat: RepeatHourly, Times: []string{"09:00"}}, "set between instead"},
		{Schedule{Repeat: RepeatEvery}, "every needs a step"},
		{Schedule{Repeat: RepeatEvery, Every: "5m"}, "the shortest step is 15 minutes"},
		{Schedule{Repeat: RepeatEvery, Every: "25h"}, "the longest step is 24 hours"},
		{Schedule{Repeat: RepeatEvery, Every: "20m30s"}, "a whole number of minutes"},
		{Schedule{Repeat: RepeatDaily, Every: "1h"}, "every goes with the repeat every"},
		{Schedule{Repeat: RepeatDaily, Between: []string{"08:00", "18:00"}}, "between goes with hourly or every"},
		{Schedule{Repeat: RepeatHourly, Between: []string{"18:00", "08:00"}}, "must start before it ends"},
		{Schedule{Repeat: RepeatHourly, Between: []string{"08:00"}}, "give two"},
		{Schedule{Repeat: RepeatDaily, TimeZone: "Mars/Olympus"}, "not an IANA time zone"},
		{Schedule{Repeat: RepeatDaily, TimeZone: "Local"}, "name an IANA time zone"},
		{Schedule{Repeat: RepeatDaily, Start: "10/26/2026"}, "write a date as 2006-01-02"},
		{Schedule{Repeat: RepeatDaily, Start: "2026-12-01", End: "2026-11-01"}, "is before start"},
	} {
		got := strings.Join(c.s.Normalize("").Check(), " ")
		if !strings.Contains(got, c.want) {
			t.Errorf("Check(%+v) = %q, want %q", c.s, got, c.want)
		}
		if _, err := c.s.Plan(); err == nil {
			t.Errorf("Plan(%+v) has no error", c.s)
		}
	}
}
