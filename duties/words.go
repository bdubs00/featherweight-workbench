package duties

// This file fills the placeholders of the message of a duty and says a
// duty in words. The placeholders are the only syntax in a message:
// {date}, {time}, {found}, {event}, {last.title}, {last.run},
// {last.state}, and {duty}. The front replaces them before the run goes
// on the queue, and any other text in braces stays as it is. The readback
// comes from the record alone.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/faces"
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

// readback says a duty in plain words to the person me: when it starts,
// what its check looks for, and who hears of its results, with you for
// me. It comes from the record, never from the words that a person typed.
// The technical parts, such as the address of a webhook, are in details.
func readback(d *Duty, me string) string {
	var parts []string
	switch w := d.When; {
	case w.Schedule != nil:
		plan, err := w.Schedule.Plan()
		if err != nil {
			return ""
		}
		parts = append(parts, plan.Readback())
	case w.Webhook != nil:
		parts = append(parts, webhookWords(w.Webhook))
	case w.Event != nil:
		parts = append(parts, eventWords(w.Event))
	}
	if c := d.CheckFirst; c != nil {
		parts = append(parts, checkWords(d, c))
	}
	if w := resultWords(d, me); w != "" {
		parts = append(parts, w)
	}
	switch {
	case d.Review && me != "" && (d.Owner == me || d.Owner == ""):
		parts = append(parts, youReview)
	case d.Review:
		parts = append(parts, reviewWords)
	}
	return strings.Join(parts, " ")
}

// worded returns a copy of the duty whose agents read as their display
// names, such as Net Auditor for net-auditor, for a readback that a person
// reads. The record keeps the names.
func (s *Service) worded(ctx context.Context, d Duty) Duty {
	d.Agent = s.agentWords(ctx, d.Agent)
	if e := d.When.Event; e != nil && e.Agent != "" {
		c := *e
		c.Agent = s.agentWords(ctx, e.Agent)
		d.When.Event = &c
	}
	return d
}

// agentWords is the display name of an agent, else its name.
func (s *Service) agentWords(ctx context.Context, agent string) string {
	st := faces.Of(s.a)
	if st == nil || agent == "" {
		return agent
	}
	f, err := st.Get(fw.WithPrincipal(ctx, nil), agent)
	if err != nil || strings.TrimSpace(f.DisplayName) == "" {
		return agent
	}
	return strings.TrimSpace(f.DisplayName)
}

// details says the parts of a duty that a developer reads, for the fold
// "For developers" of the page: the address, the credential, and the key
// of a webhook, and where a check finds its items and what makes an item
// new. It is "" for a duty without such parts.
func details(d *Duty) string {
	var parts []string
	if w := d.When.Webhook; w != nil {
		parts = append(parts, webhookDetails(d.Name, w))
	}
	if c := d.CheckFirst; c != nil {
		if t := checkDetails(c); t != "" {
			parts = append(parts, t)
		}
	}
	return strings.Join(parts, " ")
}

// webhookWords says the start of a webhook in plain words: who sends the
// event, whether it is signed, and which repeats the duty skips.
func webhookWords(w *Webhook) string {
	var s string
	switch w.Auth.Mode {
	case "hmac":
		who := appOfHeader(w.Auth.Header)
		if who == "" {
			who = "another app"
		}
		s = "When " + who + " sends an event, signed."
	case "bearer":
		s = "When another app sends an event with its key."
	default:
		s = "When another app sends an event. Nothing checks who sends it."
	}
	if w.Key != "" {
		return s + " Repeats of one " + keyNoun(w.Key) + " within a week are skipped."
	}
	return s + " Repeats of the same event within ten minutes are skipped."
}

// webhookDetails says the address of a webhook, its credential, and when
// a second call is a duplicate, for a developer.
func webhookDetails(name string, w *Webhook) string {
	s := "Another app calls POST /v1/duties/" + name + "/call"
	switch w.Auth.Mode {
	case "bearer":
		s += " with the bearer token of the duty."
	case "hmac":
		s += " with the HMAC-SHA256 signature of the body in the header " + w.Auth.Header + "."
	default:
		s += ", with no credential."
	}
	if w.Key != "" {
		return s + " A second call with the same " + w.Key + " within a week is a duplicate."
	}
	return s + " A second call with the same body within ten minutes is a duplicate."
}

// apps names the app that signs a webhook with a header of the form
// X-<word>-Signature, by the word in lower case.
var apps = map[string]string{
	"gitea":   "Gitea",
	"forgejo": "Forgejo",
	"gogs":    "Gogs",
	"hub":     "GitHub",
	"github":  "GitHub",
	"gitlab":  "GitLab",
}

// appOfHeader names the app that signs with the header, such as Gitea for
// X-Gitea-Signature and GitHub for X-Hub-Signature-256, or "" for a header
// that names no app that the server knows.
func appOfHeader(h string) string {
	parts := strings.Split(strings.ToLower(strings.TrimSpace(h)), "-")
	if len(parts) < 3 || parts[0] != "x" || parts[2] != "signature" {
		return ""
	}
	return apps[parts[1]]
}

// keyNoun names the thing that the key of a webhook tells apart, from its
// dotted path: "issue" for issue.number, "pull request" for
// pull_request.id, "ticket" for ticket_id, and "event" for a key such as
// id.
func keyNoun(key string) string {
	parts := strings.Split(strings.TrimSpace(key), ".")
	last := parts[len(parts)-1]
	plain := map[string]bool{"id": true, "number": true, "key": true, "uuid": true, "guid": true, "sha": true, "num": true}
	noun := last
	if plain[strings.ToLower(last)] {
		noun = ""
		if len(parts) > 1 {
			noun = parts[len(parts)-2]
		}
	} else {
		for _, suffix := range []string{"_id", "_number", "-id", "Id", "ID", "Number"} {
			if strings.HasSuffix(noun, suffix) && len(noun) > len(suffix) {
				noun = strings.TrimSuffix(noun, suffix)
				break
			}
		}
	}
	noun = strings.TrimSpace(strings.NewReplacer("_", " ", "-", " ").Replace(splitCamel(noun)))
	if noun == "" || plain[strings.ToLower(noun)] {
		return "event"
	}
	return strings.ToLower(noun)
}

// splitCamel puts a space before each capital inside a word, so
// pullRequest reads as pull Request.
func splitCamel(s string) string {
	var b strings.Builder
	for i, r := range s {
		if i > 0 && r >= 'A' && r <= 'Z' {
			b.WriteByte(' ')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// eventWords says the start of an event: what happens, of which agent,
// under which path, and how long it waits first, such as "When a question
// of Triage waits longer than 1 hour."
func eventWords(e *Event) string {
	subject, verb := "", ""
	switch e.Kind {
	case EventBridgeClosed:
		subject, verb = "a bridge", "closes"
	case EventRunFailed:
		subject, verb = "a run", "fails after its retries"
	case EventQuestionWaiting:
		subject, verb = "a question", "waits"
	case EventPageChanged:
		subject, verb = "a page of the wiki", "changes"
	case EventRoutineReady:
		subject, verb = "a routine", "is ready for a sign-off"
	case EventCapReached:
		subject, verb = "a duty", "reaches its daily cost cap"
	}
	after := time.Duration(0)
	if strings.TrimSpace(e.After) != "" {
		after, _ = readDur(e.After)
	}
	var s string
	if subject == "" {
		s = "When the event " + e.Kind + " comes"
		if e.Agent != "" {
			s += ", for the agent " + e.Agent
		}
		if e.Path != "" {
			s += ", under " + e.Path
		}
	} else {
		if e.Agent != "" {
			subject += " of " + e.Agent
		}
		if e.Path != "" {
			subject += " under " + e.Path
		}
		s = "When " + subject + " " + verb
	}
	switch {
	case after <= 0:
		return s + "."
	case e.Kind == EventQuestionWaiting:
		// The start comes only while the question still waits.
		return s + " longer than " + spanWords(after) + "."
	}
	return s + ". The duty starts " + spanWords(after) + " later."
}

// spanWords says a span in words: "1 hour", "30 minutes", "1 hour and 30
// minutes", or "45 seconds".
func spanWords(d time.Duration) string {
	count := func(n int, one, many string) string { return fmt.Sprintf("%d %s", n, plural(n, one, many)) }
	if d < time.Minute {
		return count(int(d/time.Second), "second", "seconds")
	}
	h, m := int(d/time.Hour), int(d%time.Hour/time.Minute)
	switch {
	case h == 0:
		return count(m, "minute", "minutes")
	case m == 0:
		return count(h, "hour", "hours")
	}
	return count(h, "hour", "hours") + " and " + count(m, "minute", "minutes")
}

// checkWords says what a check looks for and when it wakes the agent, in
// plain words. Where the items are and what makes an item new are in
// checkDetails.
func checkWords(d *Duty, c *CheckFirst) string {
	var b strings.Builder
	switch {
	case c.Look.HTTP != nil:
		b.WriteString("First it looks at " + addressWords(c.Look.HTTP.URL))
	case c.Look.Tool != "":
		b.WriteString("First it calls the tool " + c.Look.Tool)
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
		b.WriteString("Only items that it did not see before wake " + d.Agent + ".")
	}
	if strings.Contains(d.Do.Message(), "{found}") {
		b.WriteString(" The run gets the list.")
	}
	return b.String()
}

// checkDetails says where a check finds its items and what makes an item
// new, for a developer.
func checkDetails(c *CheckFirst) string {
	var b strings.Builder
	if c.Items != "" {
		b.WriteString("The check reads the items at " + c.Items + " of the answer.")
	} else {
		b.WriteString("The check reads the answer as the list of items.")
	}
	if c.Wake.When != WakeAny && c.Wake.When != WakeAtLeast && c.Key != "" {
		b.WriteString(" An item is new by its " + c.Key + ".")
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
