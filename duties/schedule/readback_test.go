package schedule

import "testing"

// The readback says the record in words, with its zone and its dates.
func TestReadback(t *testing.T) {
	for _, c := range []struct {
		s    Schedule
		want string
	}{
		{Schedule{Repeat: RepeatDaily, Times: []string{"09:00"}, TimeZone: newYork}, "Every day at 9:00 AM, New York time."},
		{Schedule{Repeat: RepeatWeekdays, Times: []string{"09:30", "16:00"}, TimeZone: newYork}, "Every weekday at 9:30 AM and 4:00 PM, New York time."},
		{Schedule{Repeat: RepeatWeekly, Days: []string{"mon", "thu"}, Times: []string{"08:00"}}, "Every Monday and Thursday at 8:00 AM, UTC."},
		{Schedule{Repeat: RepeatWeekly, Days: []string{"tue"}, EveryWeeks: 2, Times: []string{"15:00"}, TimeZone: newYork, Start: "2026-10-23"}, "Every other Tuesday at 3:00 PM, New York time, from October 27, 2026."},
		{Schedule{Repeat: RepeatWeekly, Days: []string{"tue", "fri"}, EveryWeeks: 4, Times: []string{"15:00"}}, "Every 4 weeks on Tuesday and Friday at 3:00 PM, UTC."},
		{Schedule{Repeat: RepeatWeekly, Days: weekdays, Times: []string{"00:00"}}, "Every weekday at 12:00 AM, UTC."},
		{Schedule{Repeat: RepeatDaily, Times: []string{"08:00", "12:00", "17:00"}}, "Every day at 8:00 AM, 12:00 PM, and 5:00 PM, UTC."},
		{Schedule{Repeat: RepeatMonthly, DayOfMonth: "1", Times: []string{"10:00"}}, "On the 1st of every month at 10:00 AM, UTC."},
		{Schedule{Repeat: RepeatMonthly, DayOfMonth: "2"}, "On the 2nd of every month at 9:00 AM, UTC."},
		{Schedule{Repeat: RepeatMonthly, DayOfMonth: "23"}, "On the 23rd of every month at 9:00 AM, UTC."},
		{Schedule{Repeat: RepeatMonthly, DayOfMonth: "11"}, "On the 11th of every month at 9:00 AM, UTC."},
		{Schedule{Repeat: RepeatMonthly, DayOfMonth: "last"}, "On the last day of every month at 9:00 AM, UTC."},
		{Schedule{Repeat: RepeatMonthly, DayOfMonth: "second wednesday", Times: []string{"14:00"}}, "On the second Wednesday of every month at 2:00 PM, UTC."},
		{Schedule{Repeat: RepeatMonthly, DayOfMonth: "31"}, "On the 31st of every month at 9:00 AM, UTC. A month without a 31st has no run."},
		{Schedule{Repeat: RepeatEvery, Every: "2h", Between: []string{"08:00", "18:00"}, TimeZone: "Europe/Berlin"}, "Every 2 hours from 8:00 AM to 6:00 PM, Berlin time."},
		{Schedule{Repeat: RepeatEvery, Every: "15m"}, "Every 15 minutes, UTC."},
		{Schedule{Repeat: RepeatEvery, Every: "1h"}, "Every hour, UTC."},
		{Schedule{Repeat: RepeatHourly, Days: []string{"sat", "sun"}}, "Every hour on the hour on weekends, UTC."},
		{Schedule{Repeat: RepeatHourly, Days: []string{"mon", "tue", "wed", "thu", "fri", "sat", "sun"}}, "Every hour on the hour, UTC."},
		{Schedule{Repeat: RepeatDaily, End: "2026-12-31", TimeZone: "America/Argentina/Buenos_Aires"}, "Every day at 9:00 AM, Buenos Aires time, until December 31, 2026."},
		{Schedule{Repeat: "yearly"}, `repeat is "yearly": pick daily, weekdays, weekly, monthly, hourly, or every.`},
	} {
		if got := c.s.Normalize("").Readback(); got != c.want {
			t.Errorf("Readback(%+v) =\n%q\nwant\n%q", c.s, got, c.want)
		}
	}
}

func TestTimeWords(t *testing.T) {
	p := plan(t, Schedule{Repeat: RepeatDaily, TimeZone: newYork})
	if got := p.TimeWords(utc(t, "2026-10-27T19:00:00Z")); got != "Tue Oct 27, 3:00 PM" {
		t.Fatalf("TimeWords = %q", got)
	}
}
