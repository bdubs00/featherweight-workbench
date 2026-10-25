package notify

// This file sends the daily digest: once a day for each person with a
// channel whose switches send it, to each such channel, at the end of the
// quiet hours of the person, or at 08:00 in the time zone of the person
// without quiet hours. It lists the
// starts of the duties of the person in the last day, each with its
// outcome and its cost, and under each start the decisions about the
// approvals of its run while its owner was away. The duties of a person
// are the duties that the person owns and the duties whose notify lists
// name the person. One
// replica sends each digest: it claims the day of the person in the
// bucket, under digest.<person>, with a compare-and-swap.

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/bdubs00/featherweight-workbench/duties"
	"github.com/bdubs00/featherweight-workbench/internal/bucket"
)

// Times of the digest.
const (
	// digestHour is the time of the digest of a person without quiet
	// hours, in the time zone of the person.
	digestHour = 8
	// digestLate is how late the digest can still go, after a front that
	// did not run at its time.
	digestLate = 6 * time.Hour
	// digestEvery is how often a replica looks for a digest to send.
	digestEvery = time.Minute
	// maxDigestLines bounds the lines of the starts of a digest and of
	// their decisions.
	maxDigestLines = 40
	// maxStartDecisions bounds the decisions under one start. A line
	// counts the rest.
	maxStartDecisions = 3
	// maxDecisionBytes bounds the text of the lines of a digest, in
	// bytes, while it adds a decision. The rest of maxText stays free for
	// the counts and the paused duties at the end.
	maxDecisionBytes = maxText - 1000
	// maxReasonBytes bounds the reason of one decision, in bytes.
	maxReasonBytes = 120
)

// digestAt returns the time of the digest of a person on the day of now,
// in the time zone of the person.
func digestAt(q *QuietHours, now time.Time) time.Time {
	loc := time.UTC
	if q != nil {
		if l, err := time.LoadLocation(q.TimeZone); err == nil {
			loc = l
		}
	}
	local := now.In(loc)
	if q != nil && q.To != "" {
		if t, ok := clockOn(local, q.To); ok {
			return t
		}
	}
	y, m, d := local.Date()
	return time.Date(y, m, d, digestHour, 0, 0, 0, loc)
}

// digestKey is the key of the day of the last digest of a person.
func digestKey(subject string) string {
	return "digest." + strings.TrimPrefix(personKey(subject), "person.")
}

// digests sends the digests that are due, at most once per digestEvery.
func (s *Service) digests(ctx context.Context) {
	now := s.now()
	s.mu.Lock()
	if now.Sub(s.lastDigest) < digestEvery {
		s.mu.Unlock()
		return
	}
	s.lastDigest = now
	s.mu.Unlock()
	keys, err := bucket.KeysUnder(ctx, s.b, "person")
	if err != nil {
		s.log.Warn("notify: the settings did not read, so no digest goes now", "error", err)
		return
	}
	for _, k := range keys {
		if ctx.Err() != nil {
			return
		}
		e, err := s.b.Get(ctx, k)
		if err != nil {
			continue
		}
		var rec record
		if json.Unmarshal(e.Value, &rec) != nil {
			continue
		}
		var to []Channel
		for _, ch := range rec.Settings.Channels {
			if ch.Send.Digest && s.unusable(ch) == "" {
				to = append(to, ch)
			}
		}
		if len(to) == 0 {
			continue
		}
		at := digestAt(rec.Settings.QuietHours, now)
		if now.Before(at) || now.Sub(at) > digestLate {
			continue
		}
		day := at.Format(time.DateOnly)
		won, err := bucket.Modify(ctx, s.b, digestKey(rec.Subject), storeAttempts, func(value []byte, exists bool) ([]byte, error) {
			if exists && string(value) == day {
				return nil, nil
			}
			return []byte(day), nil
		})
		if err != nil || !won {
			continue
		}
		text, ok := s.digestText(ctx, rec.Subject, at.Location(), now)
		if !ok {
			continue
		}
		for _, ch := range to {
			m := &message{To: rec.Subject, Channel: ch.ID, Kind: EventDigest, Title: "Your duties: the last day", Text: text, Priority: 2, At: now.UTC()}
			if err := s.put(ctx, m); err != nil {
				s.log.Warn("notify: a digest did not reach the outbox", "to", rec.Subject, "channel", ch.ID, "error", err)
			}
		}
	}
}

// digestText writes the digest of a person: each start of the duties of
// the person in the day before now with its outcome and its cost, and
// under it the decisions while its owner was away, the starts that did
// not run as one count, the total, and the duties that paused
// themselves. It reports false for a person with no duty.
func (s *Service) digestText(ctx context.Context, subject string, loc *time.Location, now time.Time) (string, bool) {
	svc := duties.Of(s.a)
	if svc == nil {
		return "", false
	}
	recs, err := svc.Records(ctx)
	if err != nil {
		s.log.Warn("notify: the duties did not read, so a digest says nothing", "to", subject, "error", err)
		return "", false
	}
	var lines, paused []string
	runs, idle, more, size := 0, 0, 0, 0
	total := 0.0
	mine := 0
	for _, r := range recs {
		if !names(r.Duty, subject) {
			continue
		}
		mine++
		if r.Duty.Paused && r.PausedReason != "" {
			paused = append(paused, fmt.Sprintf("%s paused itself: %s", r.Duty.Name, r.PausedReason))
		}
		starts, err := svc.Starts(ctx, r.Duty.Name, now.Add(-24*time.Hour))
		if err != nil {
			continue
		}
		slices.Reverse(starts)
		for _, e := range starts {
			total += e.CostUSD
			if e.Run == "" && e.State != duties.StateFailed {
				idle++
				continue
			}
			runs++
			// A start with decisions needs a second line, for at least
			// the count of its decisions.
			need := 1
			if len(e.Decided) > 0 {
				need = 2
			}
			if len(lines)+need > maxDigestLines {
				more++
				continue
			}
			line := fmt.Sprintf("- %s, %s: %s, $%.2f", r.Duty.Name, e.Slot.In(loc).Format("Mon 3:04 PM"), e.State, e.CostUSD)
			if e.State == duties.StateFailed && e.Reason != "" {
				line += ". " + cutText(e.Reason, 160)
			}
			lines = append(lines, line)
			size += len(line) + 1
			shown := 0
			for _, d := range e.Decided {
				w := "  - " + decidedWords(d, subject)
				if shown == maxStartDecisions || len(lines) >= maxDigestLines-1 || size+len(w) > maxDecisionBytes {
					break
				}
				lines = append(lines, w)
				size += len(w) + 1
				shown++
			}
			switch rest := len(e.Decided) - shown; {
			case rest == 1:
				lines = append(lines, "  - and 1 more decision")
			case rest > 1:
				lines = append(lines, fmt.Sprintf("  - and %d more decisions", rest))
			}
		}
	}
	if mine == 0 {
		return "", false
	}
	var b strings.Builder
	switch runs {
	case 0:
		b.WriteString("No duty of yours ran in the last day.")
	case 1:
		fmt.Fprintf(&b, "1 run of your duties in the last day, $%.2f in all:", total)
	default:
		fmt.Fprintf(&b, "%d runs of your duties in the last day, $%.2f in all:", runs, total)
	}
	for _, l := range lines {
		b.WriteString("\n" + l)
	}
	if more > 0 {
		fmt.Fprintf(&b, "\n- and %d more", more)
	}
	if idle > 0 {
		fmt.Fprintf(&b, "\n%d starts did not run, such as a check that found no work.", idle)
	}
	for _, p := range paused {
		b.WriteString("\n" + p)
	}
	return b.String(), true
}

// names reports whether a duty belongs to a person: the person owns it,
// or its notify lists name the person.
func names(d duties.Duty, subject string) bool {
	if d.Owner == subject {
		return true
	}
	if d.Notify == nil {
		return false
	}
	for _, list := range [][]string{d.Notify.Failure, d.Notify.NeedsYou} {
		if slices.Contains(list, subject) {
			return true
		}
	}
	return false
}

// decidedWords says in short words who decided an approval of a run while
// its owner was away, and how, as the page says it: "The manager allowed
// bash: it only reads." or "Nobody answered in time, so the front denied
// git.push." me is the person of the digest, who reads "You". The reason
// of a timeout only repeats the timeout, so it stays out.
func decidedWords(d duties.Decided, me string) string {
	tool := strings.TrimSpace(d.Tool)
	if tool == "" {
		tool = "a tool"
	}
	if d.By == duties.DecidedByTimeout {
		switch d.Verdict {
		case "cancel":
			return "Nobody answered in time, so the front stopped the run at " + tool + "."
		case "allow":
			return "Nobody answered in time, so the front allowed " + tool + "."
		}
		return "Nobody answered in time, so the front denied " + tool + "."
	}
	why := "."
	if r := strings.Join(strings.Fields(d.Reason), " "); r != "" {
		why = ": " + reasonWords(r)
	}
	var who string
	switch {
	case d.By == duties.DecidedByManager:
		who = "The manager"
	case me != "" && d.By == me:
		who = "You"
	default:
		who = capitalize(personName(d.By))
	}
	switch d.Verdict {
	case "allow":
		return who + " allowed " + tool + why
	case "cancel":
		return who + " stopped the run at " + tool + why
	}
	return who + " denied " + tool + why
}

// reasonWords cuts the reason of a decision to maxReasonBytes at a word,
// and ends it with a full stop.
func reasonWords(r string) string {
	if len(r) > maxReasonBytes {
		c := cutText(r, maxReasonBytes)
		if i := strings.LastIndexByte(c, ' '); i > maxReasonBytes/2 {
			c = c[:i]
		}
		return strings.TrimRight(c, " ,;:") + "…"
	}
	if strings.HasSuffix(r, ".") || strings.HasSuffix(r, "!") || strings.HasSuffix(r, "?") {
		return r
	}
	return r + "."
}

// personName is the short name of a subject, as the page shows it: the
// part before an @, or "someone" for no subject.
func personName(subject string) string {
	subject = strings.TrimSpace(subject)
	if subject == "" {
		return "someone"
	}
	if i := strings.IndexByte(subject, '@'); i > 0 {
		return subject[:i]
	}
	return subject
}
