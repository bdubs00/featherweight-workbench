package duties

// This file fills the placeholders of the message of a duty and says a
// duty in words. The placeholders are the only syntax in a message:
// {date}, {time}, {found}, {event}, {last.title}, {last.run},
// {last.state}, and {duty}. The front replaces them before the run goes
// on the queue, and any other text in braces stays as it is. The readback
// comes from the record alone.

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// Placeholders are the placeholders of a message.
var Placeholders = []string{"{date}", "{time}", "{found}", "{event}", "{last.title}", "{last.run}", "{last.state}", "{duty}"}

// none stands for a placeholder without a value.
const none = "none"

// placeholders returns the values of the placeholders for a start: the
// date and the time of the start in the zone of the duty, what the check
// found, the event of a call, the last run before the start, and the name
// of the duty.
func placeholders(d *Duty, e entry, last *RunEntry) map[string]string {
	loc := d.zone()
	v := map[string]string{
		"{date}":       e.Slot.In(loc).Format(time.DateOnly),
		"{time}":       e.Slot.In(loc).Format("15:04"),
		"{found}":      orNone(e.FoundText),
		"{event}":      orNone(e.Event),
		"{last.title}": none,
		"{last.run}":   none,
		"{last.state}": none,
		"{duty}":       d.Name,
	}
	if last != nil {
		v["{last.title}"], v["{last.run}"], v["{last.state}"] = orNone(last.Title), last.Run, last.State
	}
	return v
}

// titleValues are the values of the placeholders for the title of a run.
// The agent reads {found} and {event} as JSON, but a person reads the
// title, so the title takes the count of the items and the name of the
// event instead.
func titleValues(d *Duty, e entry, last *RunEntry) map[string]string {
	v := placeholders(d, e, last)
	if e.Found > 0 {
		v["{found}"] = fmt.Sprintf("%d %s", e.Found, plural(e.Found, "item", "items"))
	}
	v["{event}"] = eventName(e.Event)
	return v
}

// eventName names an event in a few words: the first of its title, path,
// name, and kind, else "an event".
func eventName(raw string) string {
	if strings.TrimSpace(raw) == "" {
		return none
	}
	var body map[string]any
	if json.Unmarshal([]byte(raw), &body) == nil {
		for _, k := range []string{"title", "path", "name", "kind"} {
			if s, ok := body[k].(string); ok && strings.TrimSpace(s) != "" {
				return strings.TrimSpace(s)
			}
		}
	}
	return "an event"
}

func orNone(s string) string {
	if strings.TrimSpace(s) == "" {
		return none
	}
	return s
}

// fill replaces the placeholders of a message in one pass, so a value
// that holds a placeholder stays as it is.
func fill(msg string, values map[string]string) string {
	pairs := make([]string, 0, 2*len(Placeholders))
	for _, p := range Placeholders {
		pairs = append(pairs, p, values[p])
	}
	return strings.NewReplacer(pairs...).Replace(msg)
}

// readback says a duty in words: when it starts, and what its check looks
// for. It comes from the record, never from the words that a person typed.
func readback(d *Duty) string {
	var parts []string
	switch w := d.When; {
	case w.Schedule != nil:
		plan, err := w.Schedule.Plan()
		if err != nil {
			return ""
		}
		parts = append(parts, plan.Readback())
	case w.Webhook != nil:
		parts = append(parts, webhookWords(d.Name, w.Webhook))
	case w.Event != nil:
		parts = append(parts, eventWords(w.Event))
	}
	if c := d.CheckFirst; c != nil {
		parts = append(parts, checkWords(d, c))
	}
	return strings.Join(parts, " ")
}

// webhookWords says the start of a webhook.
func webhookWords(name string, w *Webhook) string {
	s := "When another app calls POST /v1/duties/" + name + "/call"
	switch w.Auth.Mode {
	case "bearer":
		s += " with its bearer token."
	case "hmac":
		s += " with the HMAC signature of the body in " + w.Auth.Header + "."
	default:
		s += ", with no credential."
	}
	if w.Key != "" {
		return s + " A second call with the same " + w.Key + " within a week is a duplicate."
	}
	return s + " A second call with the same body within ten minutes is a duplicate."
}

// eventWords says the start of an event.
func eventWords(e *Event) string {
	var s string
	switch e.Kind {
	case EventBridgeClosed:
		s = "When a bridge closes"
	case EventRunFailed:
		s = "When a run fails after its retries"
	case EventQuestionWaiting:
		s = "When a question waits"
	case EventPageChanged:
		s = "When a page of the wiki changes"
	case EventRoutineReady:
		s = "When a routine is ready for a sign-off"
	case EventCapReached:
		s = "When a cap is reached"
	default:
		s = "When the event " + e.Kind + " comes"
	}
	if e.Agent != "" {
		s += ", for the agent " + e.Agent
	}
	if e.Path != "" {
		s += ", under " + e.Path
	}
	if e.After != "" {
		s += ", after " + e.After
	}
	return s + "."
}

// checkWords says what a check looks for and when it wakes the agent.
func checkWords(d *Duty, c *CheckFirst) string {
	var b strings.Builder
	switch {
	case c.Look.HTTP != nil:
		b.WriteString("First it looks at " + addressWords(c.Look.HTTP.URL))
	case c.Look.Tool != "":
		b.WriteString("First it calls the tool " + c.Look.Tool)
	}
	if c.Items != "" {
		b.WriteString(" for the items at " + c.Items)
	}
	if len(c.Where) > 0 {
		conds := make([]string, 0, len(c.Where))
		for _, w := range c.Where {
			conds = append(conds, whereWords(w))
		}
		b.WriteString(", where " + strings.Join(conds, " and "))
	}
	b.WriteString(". ")
	switch c.Wake.When {
	case WakeAny:
		b.WriteString("Any item wakes " + d.Agent + ".")
	case WakeAtLeast:
		fmt.Fprintf(&b, "%d items or more wake %s.", c.Wake.Count, d.Agent)
	default:
		b.WriteString("Only items that it did not see before wake " + d.Agent + ", by their " + c.Key + ".")
	}
	if strings.Contains(d.Do.Message(), "{found}") {
		b.WriteString(" The run gets the list.")
	}
	return b.String()
}

// addressWords writes an address without its query, which can hold a key.
func addressWords(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return hostOf(raw)
	}
	return u.Host + strings.TrimSuffix(u.EscapedPath(), "/")
}

// whereWords says one filter.
func whereWords(w Where) string {
	f := strings.TrimSpace(w.Field)
	switch {
	case len(w.Is) > 0:
		return f + " is " + string(w.Is)
	case len(w.IsNot) > 0:
		return f + " is not " + string(w.IsNot)
	case w.OneOf != nil:
		vals := make([]string, 0, len(w.OneOf))
		for _, v := range w.OneOf {
			vals = append(vals, string(v))
		}
		return f + " is " + strings.Join(vals, " or ")
	case len(w.Contains) > 0:
		return f + " contains " + string(w.Contains)
	case len(w.AtLeast) > 0:
		return f + " is at least " + string(w.AtLeast)
	case len(w.AtMost) > 0:
		return f + " is at most " + string(w.AtMost)
	}
	return f
}

// next returns the next n times of the schedule of a duty after now, in
// UTC. A duty without a schedule has none.
func next(d *Duty, now time.Time, n int) []time.Time {
	out := []time.Time{}
	if d.When.Schedule == nil {
		return out
	}
	plan, err := d.When.Schedule.Plan()
	if err != nil {
		return out
	}
	for _, t := range plan.Next(now, n) {
		out = append(out, t.UTC())
	}
	return out
}
