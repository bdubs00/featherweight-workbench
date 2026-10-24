package notify

// This file sends the daily digest: once a day for each person with a
// channel whose switches send it, to each such channel, at the end of the
// quiet hours of the person, or at 08:00 in the time zone of the person
// without quiet hours. It lists the
// starts of the duties of the person in the last day, each with its
// outcome and its cost. The duties of a person are the duties that the
// person owns and the duties whose notify lists name the person. One
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
	// maxDigestLines bounds the lines of the starts of a digest.
	maxDigestLines = 40
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
// the person in the day before now with its outcome and its cost, the
// starts that did not run as one count, the total, and the duties that
// paused themselves. It reports false for a person with no duty.
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
	runs, idle, more := 0, 0, 0
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
			if len(lines) >= maxDigestLines {
				more++
				continue
			}
			line := fmt.Sprintf("%s, %s: %s, $%.2f", r.Duty.Name, e.Slot.In(loc).Format("Mon 3:04 PM"), e.State, e.CostUSD)
			if e.State == duties.StateFailed && e.Reason != "" {
				line += ". " + cutText(e.Reason, 160)
			}
			lines = append(lines, line)
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
		b.WriteString("\n- " + l)
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
