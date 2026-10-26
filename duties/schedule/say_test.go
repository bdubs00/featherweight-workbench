package schedule

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

// now is Friday, October 23, 2026, at noon in New York.
var now = time.Date(2026, 10, 23, 16, 0, 0, 0, time.UTC)

var opt = Options{Zone: newYork, Now: now}

var weekdays = []string{"mon", "tue", "wed", "thu", "fri"}

// The phrases of the design, and their natural variants, give the same
// schedule each time, and the readback of each comes from the record.
func TestParse(t *testing.T) {
	for _, c := range []struct {
		say  string
		want Schedule
		back string
	}{
		{"every day at 9am", Schedule{Repeat: RepeatDaily, Times: []string{"09:00"}}, "Every day at 9:00 AM, New York time."},
		{"Daily at 9", Schedule{Repeat: RepeatDaily, Times: []string{"09:00"}}, ""},
		{"each day at 09:00.", Schedule{Repeat: RepeatDaily, Times: []string{"09:00"}}, ""},
		{"every day at noon", Schedule{Repeat: RepeatDaily, Times: []string{"12:00"}}, "Every day at 12:00 PM, New York time."},
		{"at 9am", Schedule{Repeat: RepeatDaily, Times: []string{"09:00"}}, ""},
		{"weekdays at 9:30am and 4pm", Schedule{Repeat: RepeatWeekdays, Times: []string{"09:30", "16:00"}}, "Every weekday at 9:30 AM and 4:00 PM, New York time."},
		{"every weekday at 9:30 am, 12 pm, and 4 pm", Schedule{Repeat: RepeatWeekdays, Times: []string{"09:30", "12:00", "16:00"}}, "Every weekday at 9:30 AM, 12:00 PM, and 4:00 PM, New York time."},
		{"on weekdays at 16:30", Schedule{Repeat: RepeatWeekly, Days: weekdays, EveryWeeks: 1, Times: []string{"16:30"}}, "Every weekday at 4:30 PM, New York time."},
		{"weekdays 9am", Schedule{Repeat: RepeatWeekdays, Times: []string{"09:00"}}, ""},
		{"every Monday and Thursday at 8:00", Schedule{Repeat: RepeatWeekly, Days: []string{"mon", "thu"}, EveryWeeks: 1, Times: []string{"08:00"}}, "Every Monday and Thursday at 8:00 AM, New York time."},
		{"mondays and thursdays at 8", Schedule{Repeat: RepeatWeekly, Days: []string{"mon", "thu"}, EveryWeeks: 1, Times: []string{"08:00"}}, ""},
		{"every mon, wed, and fri at 7:15pm", Schedule{Repeat: RepeatWeekly, Days: []string{"mon", "wed", "fri"}, EveryWeeks: 1, Times: []string{"19:15"}}, "Every Monday, Wednesday, and Friday at 7:15 PM, New York time."},
		{"every Monday to Friday at 9", Schedule{Repeat: RepeatWeekly, Days: weekdays, EveryWeeks: 1, Times: []string{"09:00"}}, "Every weekday at 9:00 AM, New York time."},
		{"every other Tuesday at 3pm", Schedule{Repeat: RepeatWeekly, Days: []string{"tue"}, EveryWeeks: 2, Times: []string{"15:00"}, Start: "2026-10-23"}, "Every other Tuesday at 3:00 PM, New York time, from October 27, 2026."},
		{"every 2 weeks on Friday at 10am", Schedule{Repeat: RepeatWeekly, Days: []string{"fri"}, EveryWeeks: 2, Times: []string{"10:00"}, Start: "2026-10-23"}, ""},
		{"every third week on monday", Schedule{Repeat: RepeatWeekly, Days: []string{"mon"}, EveryWeeks: 3, Times: []string{"09:00"}, Start: "2026-10-23"}, "Every 3 weeks on Monday at 9:00 AM, New York time, from October 26, 2026."},
		{"on the first of the month at 10am", Schedule{Repeat: RepeatMonthly, DayOfMonth: "1", Times: []string{"10:00"}}, "On the 1st of every month at 10:00 AM, New York time."},
		{"on the 15th of every month at noon", Schedule{Repeat: RepeatMonthly, DayOfMonth: "15", Times: []string{"12:00"}}, "On the 15th of every month at 12:00 PM, New York time."},
		{"monthly on the 1st", Schedule{Repeat: RepeatMonthly, DayOfMonth: "1", Times: []string{"09:00"}}, ""},
		{"every month on the 22nd at 6pm", Schedule{Repeat: RepeatMonthly, DayOfMonth: "22", Times: []string{"18:00"}}, ""},
		{"on the last day of the month", Schedule{Repeat: RepeatMonthly, DayOfMonth: "last", Times: []string{"09:00"}}, "On the last day of every month at 9:00 AM, New York time."},
		{"on the first Monday of the month at 9", Schedule{Repeat: RepeatMonthly, DayOfMonth: "first monday", Times: []string{"09:00"}}, "On the first Monday of every month at 9:00 AM, New York time."},
		{"every last friday of the month at 5pm", Schedule{Repeat: RepeatMonthly, DayOfMonth: "last friday", Times: []string{"17:00"}}, "On the last Friday of every month at 5:00 PM, New York time."},
		{"every 2 hours between 8am and 6pm", Schedule{Repeat: RepeatEvery, Every: "2h", Between: []string{"08:00", "18:00"}}, "Every 2 hours from 8:00 AM to 6:00 PM, New York time."},
		{"every two hours from 8:00 to 18:00 on weekdays", Schedule{Repeat: RepeatEvery, Every: "2h", Between: []string{"08:00", "18:00"}, Days: weekdays}, "Every 2 hours from 8:00 AM to 6:00 PM on weekdays, New York time."},
		{"every 15 minutes", Schedule{Repeat: RepeatEvery, Every: "15m"}, "Every 15 minutes, New York time."},
		{"every 15 min", Schedule{Repeat: RepeatEvery, Every: "15m"}, ""},
		{"every half hour", Schedule{Repeat: RepeatEvery, Every: "30m"}, "Every 30 minutes, New York time."},
		{"every 90 minutes", Schedule{Repeat: RepeatEvery, Every: "1h30m"}, "Every 1 hour and 30 minutes, New York time."},
		{"hourly on weekdays", Schedule{Repeat: RepeatHourly, Days: weekdays}, "Every hour on the hour on weekdays, New York time."},
		{"every hour between 9am and 5pm on mon and wed", Schedule{Repeat: RepeatHourly, Days: []string{"mon", "wed"}, Between: []string{"09:00", "17:00"}}, "Every hour from 9:00 AM to 5:00 PM on Monday and Wednesday, New York time."},
		{"every 1 hour", Schedule{Repeat: RepeatHourly}, ""},
		{"every morning", Schedule{Repeat: RepeatDaily, Times: []string{"09:00"}}, "Every day at 9:00 AM, New York time."},
		{"every evening", Schedule{Repeat: RepeatDaily, Times: []string{"18:00"}}, "Every day at 6:00 PM, New York time."},
		{"every weekday morning", Schedule{Repeat: RepeatWeekdays, Times: []string{"09:00"}}, ""},
		{"every Monday morning", Schedule{Repeat: RepeatWeekly, Days: []string{"mon"}, EveryWeeks: 1, Times: []string{"09:00"}}, "Every Monday at 9:00 AM, New York time."},
		{"every day in the evening", Schedule{Repeat: RepeatDaily, Times: []string{"18:00"}}, ""},
		{"every weekend at 10am", Schedule{Repeat: RepeatWeekly, Days: []string{"sat", "sun"}, EveryWeeks: 1, Times: []string{"10:00"}}, "Every Saturday and Sunday at 10:00 AM, New York time."},
		{"every day at 9am UTC", Schedule{Repeat: RepeatDaily, Times: []string{"09:00"}, TimeZone: "UTC"}, "Every day at 9:00 AM, UTC."},
		{"every day at 7am in Europe/London", Schedule{Repeat: RepeatDaily, Times: []string{"07:00"}, TimeZone: "Europe/London"}, "Every day at 7:00 AM, London time."},
		{"every day at 9am starting 2026-11-01 until December 31", Schedule{Repeat: RepeatDaily, Times: []string{"09:00"}, Start: "2026-11-01", End: "2026-12-31"}, "Every day at 9:00 AM, New York time, from November 1, 2026 until December 31, 2026."},
		{"every day at 9am until Jan 5th", Schedule{Repeat: RepeatDaily, Times: []string{"09:00"}, End: "2027-01-05"}, ""},
		// A time without am or pm that is not later than the one before
		// it is in the afternoon, also at the end of a window.
		{"every weekday at 9 and 5", Schedule{Repeat: RepeatWeekdays, Times: []string{"09:00", "17:00"}}, "Every weekday at 9:00 AM and 5:00 PM, New York time."},
		{"every day at 9, 12, and 5", Schedule{Repeat: RepeatDaily, Times: []string{"09:00", "12:00", "17:00"}}, ""},
		{"every day at 9 and 10", Schedule{Repeat: RepeatDaily, Times: []string{"09:00", "10:00"}}, ""},
		{"every day at 5", Schedule{Repeat: RepeatDaily, Times: []string{"05:00"}}, ""},
		{"every day at 9am and 5", Schedule{Repeat: RepeatDaily, Times: []string{"09:00", "17:00"}}, ""},
		{"every day at 9pm and 5am", Schedule{Repeat: RepeatDaily, Times: []string{"05:00", "21:00"}}, ""},
		{"every day at 17 and 9", Schedule{Repeat: RepeatDaily, Times: []string{"09:00", "17:00"}}, ""},
		{"every day at 09:00 and 05:00", Schedule{Repeat: RepeatDaily, Times: []string{"05:00", "09:00"}}, ""},
		{"hourly from 9 to 5 on weekdays", Schedule{Repeat: RepeatHourly, Days: weekdays, Between: []string{"09:00", "17:00"}}, "Every hour from 9:00 AM to 5:00 PM on weekdays, New York time."},
		{"every 2 hours between 8 and 6", Schedule{Repeat: RepeatEvery, Every: "2h", Between: []string{"08:00", "18:00"}}, ""},
		// Common phrases of a busy person.
		{"twice a day", Schedule{Repeat: RepeatDaily, Times: []string{"09:00", "17:00"}}, "Every day at 9:00 AM and 5:00 PM, New York time."},
		{"twice daily at 8 and 4", Schedule{Repeat: RepeatDaily, Times: []string{"08:00", "16:00"}}, ""},
		{"once a day", Schedule{Repeat: RepeatDaily, Times: []string{"09:00"}}, ""},
		{"mornings", Schedule{Repeat: RepeatDaily, Times: []string{"09:00"}}, "Every day at 9:00 AM, New York time."},
		{"evenings", Schedule{Repeat: RepeatDaily, Times: []string{"18:00"}}, ""},
		{"every Monday 9", Schedule{Repeat: RepeatWeekly, Days: []string{"mon"}, EveryWeeks: 1, Times: []string{"09:00"}}, "Every Monday at 9:00 AM, New York time."},
		{"weekdays 8 and 4", Schedule{Repeat: RepeatWeekdays, Times: []string{"08:00", "16:00"}}, ""},
	} {
		got, err := Parse(c.say, opt)
		if err != nil {
			t.Errorf("Parse(%q) = %v", c.say, err)
			continue
		}
		want := c.want.Normalize(newYork)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("Parse(%q) =\n%+v\nwant\n%+v", c.say, got, want)
			continue
		}
		if c.back != "" {
			if back := got.Readback(); back != c.back {
				t.Errorf("Readback of %q = %q, want %q", c.say, back, c.back)
			}
		}
	}
}

// The same words give the same schedule every time.
func TestParseStable(t *testing.T) {
	a, _ := Parse("every other Tuesday at 3pm", opt)
	for range 20 {
		b, _ := Parse("every other Tuesday at 3pm", opt)
		if !reflect.DeepEqual(a, b) {
			t.Fatalf("two reads differ: %+v and %+v", a, b)
		}
	}
}

// A part that does not read is named in words, with the part before it
// that reads.
func TestParseErrors(t *testing.T) {
	for _, c := range []struct{ say, want string }{
		{"every Tuesday after lunch", `Every Tuesday is clear. "after lunch" is not a time: pick one.`},
		{"every tuesday at lunch", `Every Tuesday is clear. "at lunch" is not a time: pick one.`},
		{"every day at 25:00", `Every day is clear. "at 25:00" is not a time: pick one.`},
		{"every day at 13pm", `Every day is clear. "at 13pm" is not a time: pick one.`},
		{"", "Say when, such as every weekday at 9am."},
		{"tomorrow at 10", `"tomorrow" is one day, and a duty repeats: say a repeat, such as every day at 10am, or start one run with Run now.`},
		{"twice a week", `"twice a week" is not a repeat: say one, such as every day, weekdays, every Monday, every 2 hours, or the first of the month.`},
		{"whenever you like", `"whenever you like" is not a repeat: say one, such as every day, weekdays, every Monday, every 2 hours, or the first of the month.`},
		{"every blursday at 9", `"every blursday" is not a repeat: say one, such as every day, weekdays, every Monday, every 2 hours, or the first of the month.`},
		{"every 5 minutes", `"every 5 minutes" is too often: the shortest step is 15 minutes.`},
		{"every 30 hours", `"every 30 hours" is too long: the longest step is 24 hours.`},
		{"every 6 weeks on monday", `"every 6 weeks" is too long: the longest repeat is every 4 weeks.`},
		{"every 3 days", `"every 3 days" is not a repeat that this server knows: say every day, days of the week, or a day of the month.`},
		{"every other day", `"every other day" is not a repeat that this server knows: say every other Monday, or every 2 weeks on Friday.`},
		{"every day at 9am and every monday", `Every day at 9am is clear. "every monday" is a second repeat: keep one.`},
		{"every 2 hours at 9am", `Every 2 hours is clear. "at 9am" does not fit a repeat of steps through the day: say a window, such as between 8am and 6pm.`},
		{"every day between 8am and 6pm", `Every day between 8am and 6pm has a window of the day, which goes with hourly or every 2 hours: say times with at instead.`},
		{"every 2 hours between 6pm and 8am", `Every 2 hours is clear. "between 6pm and 8am" ends before it starts: say two times of one day, the earlier first.`},
		{"weekly", `Weekly needs days: say them, such as every Monday and Thursday.`},
		{"on the 40th of the month", `"on the 40th of the month" is not a day of the month: pick one, such as the 1st, the last day, or the first Monday.`},
		{"hourly on funday", `Hourly is clear. "on funday" is not a day of the week: pick one, such as Monday or weekdays.`},
		{"every day at 9am until someday", `Every day at 9am is clear. "until someday" is not a date: write one such as 2026-12-31 or December 31.`},
		{"every day at 9am in Mars/Olympus", `Every day at 9am is clear. "in Mars/Olympus" is not a time zone: name one such as America/New_York or UTC.`},
	} {
		_, err := Parse(c.say, opt)
		if err == nil || err.Error() != c.want {
			t.Errorf("Parse(%q) = %v\nwant %s", c.say, err, c.want)
		}
		if err != nil && !strings.Contains(c.want, "Say when") && SayErrorAs(err) == nil {
			t.Errorf("Parse(%q) gave no SayError: %T", c.say, err)
		}
	}
}
