package notify

// This file is the outbox of the notifications. A notice of a duty puts
// one notification in the outbox for each channel that it reaches: each
// channel of each person of the notice whose switches send its kind, and
// each shared channel that the notify list of the duty names. Each
// notification waits in the bucket under out.<id> until a replica sends
// it, so a restart of a replica loses none. Every replica reads the
// outbox. A replica claims a notification with a compare-and-swap on its
// entry before it sends it, as the engine of the duties claims a time, so
// only one replica sends each one. A notification during the quiet hours
// of its person waits until they end. A notification that its channel
// refuses goes again a few times, each with its own count, and then the
// front drops it and logs why.

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/duties"
	"github.com/bdubs00/featherweight-workbench/internal/bucket"
)

// Kinds of a notification beside the kinds of a notice of a duty.
const (
	EventDigest = "digest" // the daily digest
	EventTest   = "test"   // a test that a person sent
)

// Bounds of the outbox.
const (
	// claimFor is how long a claim holds before another replica takes the
	// notification over: the replica stopped in the middle.
	claimFor = 2 * time.Minute
	// maxAttempts is how many times a notification goes out before the
	// front drops it.
	maxAttempts = 5
	// maxAge is how long a notification waits at most, past the quiet
	// hours included.
	maxAge = 3 * 24 * time.Hour
	// maxText bounds the text of a notification, below the bound of ntfy.
	maxText = 3800
	// storeAttempts bounds the tries of a write that other writers race.
	storeAttempts = 8
)

// message is a notification in the outbox: its person and the id of the
// channel of the person, or the name of a shared channel that a duty
// names without a person; its kind, title, text, and priority from 1 to 5;
// the duty, the agent, and the run that it tells of; when it happened and
// when the front made it; how often it went out, when it goes again, and
// the claim of the replica that sends it.
type message struct {
	ID       string    `json:"id"`
	To       string    `json:"to,omitempty"`
	Channel  string    `json:"channel,omitempty"`
	Shared   string    `json:"shared,omitempty"`
	Kind     string    `json:"kind"`
	Title    string    `json:"title"`
	Text     string    `json:"text"`
	Duty     string    `json:"duty,omitempty"`
	Agent    string    `json:"agent,omitempty"`
	Run      string    `json:"run,omitempty"`
	Priority int       `json:"priority,omitempty"`
	At       time.Time `json:"at,omitzero"`
	Created  time.Time `json:"created"`
	Attempts int       `json:"attempts,omitempty"`
	NextTry  time.Time `json:"nextTry,omitzero"`
	Claim    string    `json:"claim,omitempty"`
	ClaimAt  time.Time `json:"claimAt,omitzero"`
}

func outKey(id string) string { return "out." + id }

// wants reports whether the switches of a channel send a kind of
// notification.
func wants(send Send, kind string) bool {
	switch kind {
	case duties.NoticeNeedsYou, duties.NoticeDecided:
		return send.Questions
	case duties.NoticeFailed, duties.NoticePaused, duties.NoticeCap:
		return send.Failures
	case duties.NoticeResult:
		// A person asked for the results of the duty by name, so a
		// channel that sends live notices sends them; a channel of the
		// digest alone does not.
		return send.Questions || send.Failures
	case EventDigest:
		return send.Digest
	case EventTest:
		return true
	}
	return false
}

// words writes a notice of a duty as a notification: a short title that
// names the duty, the reason as the text, and a priority.
func words(n duties.Notice) (title, text string, priority int) {
	text = strings.TrimSpace(n.Reason)
	switch n.Kind {
	case duties.NoticeNeedsYou:
		return "Duty " + n.Duty + " needs you", text, 4
	case duties.NoticeDecided:
		return "Duty " + n.Duty + ": decided while you were away", text, 3
	case duties.NoticeFailed:
		return "Duty " + n.Duty + " failed", text, 4
	case duties.NoticePaused:
		return "Duty " + n.Duty + " paused itself", text, 4
	case duties.NoticeCap:
		return "Duty " + n.Duty + " reached a daily cap", text, 3
	case duties.NoticeResult:
		return "Duty " + n.Duty + ": a result", text, 3
	}
	return "Duty " + n.Duty, text, 3
}

// accept puts a notice of a duty in the outbox: one notification for each
// shared channel that the notify list names, and one for each channel of
// each person of the list whose switches send its kind. One notice
// reaches a shared channel once, also when several people pick it.
func (s *Service) accept(ctx context.Context, n duties.Notice) {
	title, text, prio := words(n)
	base := message{Kind: n.Kind, Title: title, Text: text, Priority: prio, Duty: n.Duty, Agent: n.Agent, Run: n.Run, At: n.At}
	shared := map[string]bool{}
	people := map[string]bool{}
	for _, who := range n.Who {
		name, ok := duties.ChannelOf(who)
		if !ok || shared[name] {
			continue
		}
		shared[name] = true
		if s.cfg.sharedNamed(name) == nil {
			s.log.Warn("notify: a duty names a shared channel that this server does not have", "duty", n.Duty, "channel", name)
			continue
		}
		m := base
		m.Shared = name
		if err := s.put(ctx, &m); err != nil {
			s.log.Warn("notify: a notification did not reach the outbox", "channel", who, "duty", n.Duty, "error", err)
		}
	}
	for _, who := range n.Who {
		if _, ok := duties.ChannelOf(who); ok || who == "" || people[who] {
			continue
		}
		people[who] = true
		rec, err := s.load(ctx, who)
		if err != nil {
			s.log.Warn("notify: the settings of a person did not read, so a notification did not go out", "to", who, "duty", n.Duty, "error", err)
			continue
		}
		if rec == nil {
			continue
		}
		for _, ch := range rec.Settings.Channels {
			if !wants(ch.Send, n.Kind) || s.unusable(ch) != "" {
				continue
			}
			if ch.Kind == KindShared {
				if shared[ch.Shared] {
					continue
				}
				shared[ch.Shared] = true
			}
			m := base
			m.To, m.Channel = who, ch.ID
			if err := s.put(ctx, &m); err != nil {
				s.log.Warn("notify: a notification did not reach the outbox", "to", who, "channel", ch.ID, "duty", n.Duty, "error", err)
			}
		}
	}
}

// put adds a notification to the outbox and wakes the loop.
func (s *Service) put(ctx context.Context, m *message) error {
	if m.ID == "" {
		m.ID = fw.NewID()
	}
	m.Created = s.now().UTC()
	m.Text = cutText(m.Text, maxText)
	value, err := json.Marshal(m)
	if err != nil {
		return err
	}
	if _, err := s.b.Create(ctx, outKey(m.ID), value); err != nil {
		return err
	}
	s.wake()
	return nil
}

// sendDue sends each notification of the outbox whose time came and that
// no other replica holds. It returns the earliest time at which a
// notification that waits goes, or the zero time.
func (s *Service) sendDue(ctx context.Context) time.Time {
	keys, err := bucket.KeysUnder(ctx, s.b, "out")
	if err != nil {
		s.log.Warn("notify: the outbox did not read", "error", err)
		return time.Time{}
	}
	var next time.Time
	for _, k := range keys {
		if ctx.Err() != nil {
			return next
		}
		if at := s.sendOne(ctx, k); !at.IsZero() && (next.IsZero() || at.Before(next)) {
			next = at
		}
	}
	return next
}

// sendOne sends one notification of the outbox when its time came, and
// returns when it waits until, or the zero time.
func (s *Service) sendOne(ctx context.Context, key string) time.Time {
	e, err := s.b.Get(ctx, key)
	if err != nil {
		return time.Time{}
	}
	var m message
	if err := json.Unmarshal(e.Value, &m); err != nil {
		s.drop(ctx, key, e.Revision, "it does not decode")
		return time.Time{}
	}
	now := s.now()
	switch {
	case now.Sub(m.Created) > maxAge:
		s.drop(ctx, key, e.Revision, "it waited for more than three days")
		return time.Time{}
	case now.Before(m.NextTry):
		return m.NextTry
	case m.Claim != "" && now.Sub(m.ClaimAt) < claimFor:
		return m.ClaimAt.Add(claimFor)
	}
	var rec *record
	var ch *Channel
	switch {
	case m.To != "" && m.Channel != "":
		if rec, err = s.load(ctx, m.To); err != nil {
			return now.Add(time.Minute)
		}
		ch = rec.channel(m.Channel)
		switch {
		case ch == nil:
			s.drop(ctx, key, e.Revision, "the person removed the channel")
			return time.Time{}
		case !wants(ch.Send, m.Kind):
			s.drop(ctx, key, e.Revision, "the person turned this kind of notification off on the channel")
			return time.Time{}
		}
		if why := s.unusable(*ch); why != "" {
			s.drop(ctx, key, e.Revision, why)
			return time.Time{}
		}
		if end, quiet := quietUntil(rec.Settings.QuietHours, now); quiet {
			m.NextTry, m.Claim, m.ClaimAt = end.UTC(), "", time.Time{}
			if value, err := json.Marshal(m); err == nil {
				_, _ = s.b.Update(ctx, key, value, e.Revision)
			}
			return end
		}
	case m.To == "" && m.Shared != "":
	default:
		s.drop(ctx, key, e.Revision, "it names no channel, as a notification of an older server does")
		return time.Time{}
	}
	m.Claim, m.ClaimAt = s.replica, now.UTC()
	value, err := json.Marshal(m)
	if err != nil {
		return time.Time{}
	}
	rev, err := s.b.Update(ctx, key, value, e.Revision)
	if err != nil {
		// Another replica claimed it first, or the store did not answer.
		return time.Time{}
	}
	var t target
	if ch != nil {
		t, err = s.targetOf(ctx, *ch, rec)
	} else {
		t, err = s.sharedTarget(ctx, m.Shared)
	}
	if err == nil {
		err = s.send(ctx, t, &m)
	}
	channel := m.Channel
	if m.Shared != "" && m.To == "" {
		channel = duties.NotifyChannelPrefix + m.Shared
	}
	if err == nil {
		if derr := s.b.Delete(ctx, key, rev); derr != nil {
			s.log.Warn("notify: a notification that went out stays in the outbox", "id", m.ID, "error", derr)
		}
		s.log.Info("notify: a notification went out", "to", m.To, "channel", channel, "kind", t.kind, "event", m.Kind, "where", t.where())
		return time.Time{}
	}
	m.Attempts++
	if m.Attempts >= maxAttempts || isPermanent(err) {
		s.log.Warn("notify: a notification did not go out, and the server drops it", "to", m.To, "channel", channel, "kind", t.kind, "event", m.Kind, "where", t.where(), "attempts", m.Attempts, "error", err)
		_ = s.b.Delete(ctx, key, rev)
		return time.Time{}
	}
	s.log.Warn("notify: a notification did not go out; it goes again", "to", m.To, "channel", channel, "kind", t.kind, "event", m.Kind, "where", t.where(), "attempt", m.Attempts, "error", err)
	m.NextTry = now.Add(time.Duration(1<<(m.Attempts-1)) * time.Minute).UTC()
	m.Claim, m.ClaimAt = "", time.Time{}
	if value, err := json.Marshal(m); err == nil {
		_, _ = s.b.Update(ctx, key, value, rev)
	}
	return m.NextTry
}

// drop removes a notification from the outbox, and the log says why.
func (s *Service) drop(ctx context.Context, key string, rev uint64, why string) {
	if err := s.b.Delete(ctx, key, rev); err == nil {
		s.log.Info("notify: the server dropped a notification", "key", key, "reason", why)
	}
}

// quietUntil reports whether now falls in the quiet hours q, and when
// they end. Quiet hours from 22:00 to 07:00 cross midnight.
func quietUntil(q *QuietHours, now time.Time) (time.Time, bool) {
	if q == nil || q.From == "" || q.To == "" {
		return time.Time{}, false
	}
	loc, err := time.LoadLocation(q.TimeZone)
	if err != nil {
		loc = time.UTC
	}
	local := now.In(loc)
	from, ok1 := clockOn(local, q.From)
	to, ok2 := clockOn(local, q.To)
	if !ok1 || !ok2 {
		return time.Time{}, false
	}
	if from.Before(to) {
		if !local.Before(from) && local.Before(to) {
			return to, true
		}
		return time.Time{}, false
	}
	// The quiet hours cross midnight: they hold after from, and before to.
	switch {
	case !local.Before(from):
		return clockAfter(to, 1), true
	case local.Before(to):
		return to, true
	}
	return time.Time{}, false
}

// clockOn returns the time HH:MM on the day of t, in the zone of t.
func clockOn(t time.Time, hhmm string) (time.Time, bool) {
	c, err := time.Parse("15:04", hhmm)
	if err != nil {
		return time.Time{}, false
	}
	y, m, d := t.Date()
	return time.Date(y, m, d, c.Hour(), c.Minute(), 0, 0, t.Location()), true
}

// clockAfter returns the same time of the day, days later.
func clockAfter(t time.Time, days int) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d+days, t.Hour(), t.Minute(), 0, 0, t.Location())
}

// cutText cuts a text to at most n bytes, at a boundary of a character.
func cutText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && (s[n]&0xC0) == 0x80 {
		n--
	}
	return s[:n]
}
