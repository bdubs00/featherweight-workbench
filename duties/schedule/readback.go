package schedule

// This file says a schedule in words. The readback comes from the record
// alone, never from the words that a person typed, so a person reads what
// the duty will do: "Every other Tuesday at 3:00 PM, New York time."

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// longDays are the names of the days, Monday first.
var longDays = [7]string{"Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday", "Sunday"}

// Readback says the schedule in words, with its time zone, as one or two
// sentences. A schedule with problems says them instead.
func (s Schedule) Readback() string {
	p, err := s.Plan()
	if err != nil {
		return strings.Join(s.Normalize("").Check(), " ")
	}
	return p.Readback()
}

// Readback says the plan in words.
func (p *Plan) Readback() string {
	s := p.s
	var b strings.Builder
	switch s.Repeat {
	case RepeatDaily:
		b.WriteString("Every day " + timesText(s.Times))
	case RepeatWeekdays:
		b.WriteString("Every weekday " + timesText(s.Times))
	case RepeatWeekly:
		days := daysText(p.days, "and")
		switch {
		case days == "weekday" && p.weeks == 1:
			b.WriteString("Every weekday")
		case p.weeks == 1:
			b.WriteString("Every " + days)
		case p.weeks == 2:
			b.WriteString("Every other " + days)
		default:
			b.WriteString(fmt.Sprintf("Every %d weeks on %s", p.weeks, daysText(p.days, "and")))
		}
		b.WriteString(" " + timesText(s.Times))
	case RepeatMonthly:
		b.WriteString("On the " + monthDayText(p.month) + " of every month " + timesText(s.Times))
	case RepeatHourly, RepeatEvery:
		if s.Repeat == RepeatHourly {
			b.WriteString("Every hour")
		} else {
			step, _ := readStep(s.Every)
			b.WriteString("Every " + stepWords(step))
		}
		if len(s.Between) == 2 {
			from, _ := clock(s.Between[0])
			to, _ := clock(s.Between[1])
			b.WriteString(" from " + timeWords(from) + " to " + timeWords(to))
		} else if s.Repeat == RepeatHourly {
			b.WriteString(" on the hour")
		}
		if !p.anyDay {
			if d := filterText(p.days); d != "" {
				b.WriteString(" " + d)
			}
		}
	}
	b.WriteString(", " + ZoneWords(s.TimeZone))
	switch {
	case p.hasFrom && p.hasEnd:
		b.WriteString(", from " + dateWords(p.FirstDay()) + " until " + dateWords(p.end))
	case p.hasFrom:
		b.WriteString(", from " + dateWords(p.FirstDay()))
	case p.hasEnd:
		b.WriteString(", until " + dateWords(p.end))
	}
	b.WriteString(".")
	if s.Repeat == RepeatMonthly && p.month.day > 28 {
		fmt.Fprintf(&b, " A month without a %s has no run.", ordinalNumber(p.month.day))
	}
	return b.String()
}

// ZoneWords names a time zone for a person: "New York time" for
// America/New_York, and "UTC" for UTC.
func ZoneWords(zone string) string {
	switch zone {
	case "UTC", "Etc/UTC", "Etc/UCT", "UCT", "Etc/Universal", "Universal", "Zulu", "Etc/Zulu":
		return "UTC"
	}
	city := zone
	if i := strings.LastIndex(zone, "/"); i >= 0 {
		city = zone[i+1:]
	}
	return strings.ReplaceAll(city, "_", " ") + " time"
}

// TimeWords writes an instant as a person reads it in the zone of the
// plan, such as "Tue Oct 27, 3:00 PM".
func (p *Plan) TimeWords(t time.Time) string {
	return t.In(p.loc).Format("Mon Jan 2, 3:04 PM")
}

// timeWords writes minutes of the day on the clock of 12 hours, such as
// 9:00 AM.
func timeWords(m int) string {
	return time.Date(2000, 1, 1, m/60, m%60, 0, 0, time.UTC).Format("3:04 PM")
}

// timesText writes the times of a day after "at", such as "at 9:30 AM and
// 4:00 PM".
func timesText(times []string) string {
	words := make([]string, 0, len(times))
	for _, t := range times {
		m, _ := clock(t)
		words = append(words, timeWords(m))
	}
	return "at " + list(words, "and")
}

// daysText names the days of weekly: "weekday" for Monday to Friday, else
// the days, such as "Monday and Thursday".
func daysText(days [7]bool, joiner string) string {
	if days == [7]bool{true, true, true, true, true} {
		return "weekday"
	}
	var names []string
	for i, ok := range days {
		if ok {
			names = append(names, longDays[i])
		}
	}
	return list(names, joiner)
}

// filterText names the days of hourly and every: "on weekdays", "on
// weekends", "on Monday and Thursday", or "" for every day.
func filterText(days [7]bool) string {
	switch days {
	case [7]bool{true, true, true, true, true, true, true}:
		return ""
	case [7]bool{true, true, true, true, true}:
		return "on weekdays"
	case [7]bool{5: true, 6: true}:
		return "on weekends"
	}
	return "on " + daysText(days, "and")
}

// monthDayText names a day of the month, such as "1st", "last day", or
// "first Monday".
func monthDayText(md monthDay) string {
	switch {
	case md.last:
		return "last day"
	case md.day > 0:
		return ordinalNumber(md.day)
	}
	for word, n := range ordinals {
		if n == md.nth {
			return word + " " + longDays[md.weekday]
		}
	}
	return ""
}

// ordinalNumber writes a number as an ordinal, such as 1st or 22nd.
func ordinalNumber(n int) string {
	suffix := "th"
	switch {
	case n%100 >= 11 && n%100 <= 13:
	case n%10 == 1:
		suffix = "st"
	case n%10 == 2:
		suffix = "nd"
	case n%10 == 3:
		suffix = "rd"
	}
	return strconv.Itoa(n) + suffix
}

// stepWords writes a step, such as "15 minutes", "2 hours", or "1 hour
// and 30 minutes".
func stepWords(d time.Duration) string {
	h, m := int(d/time.Hour), int(d%time.Hour/time.Minute)
	unit := func(n int, one, many string) string {
		if n == 1 {
			return "1 " + one
		}
		return strconv.Itoa(n) + " " + many
	}
	switch {
	case h == 0:
		return unit(m, "minute", "minutes")
	case m == 0 && h == 1:
		return "hour"
	case m == 0:
		return unit(h, "hour", "hours")
	}
	return unit(h, "hour", "hours") + " and " + unit(m, "minute", "minutes")
}

// dateWords writes a date, such as "October 26, 2026".
func dateWords(d time.Time) string { return d.Format("January 2, 2006") }

// list joins words as a person lists them: "a", "a and b", "a, b, and c".
func list(words []string, joiner string) string {
	switch len(words) {
	case 0:
		return ""
	case 1:
		return words[0]
	case 2:
		return words[0] + " " + joiner + " " + words[1]
	}
	return strings.Join(words[:len(words)-1], ", ") + ", " + joiner + " " + words[len(words)-1]
}
