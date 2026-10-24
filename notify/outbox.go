package notify

// This file is the outbox of the notifications. Each notification waits
// in the bucket under out.<id> until a replica sends it, so a restart of
// a replica loses none. Every replica reads the outbox. A replica claims a
// notification with a compare-and-swap on its entry before it sends it,
// as the engine of the duties claims a time, so only one replica sends
// each one. A notification during the quiet hours of its person waits
// until they end. A notification that the server refuses goes again a few
// times, and then the front drops it and logs why.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/duties"
	"github.com/bdubs00/featherweight-workbench/internal/bucket"
)

// Kinds of a notification beside the kinds of a notice of a duty.
const (
	KindDigest = "digest" // the daily digest
	KindTest   = "test"   // a test that a person sent
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

// message is a notification in the outbox: its person, its kind, its title
// and text, its priority from 1 to 5, when it was made, how often it went
// out, when it goes again, and the claim of the replica that sends it.
type message struct {
	ID       string    `json:"id"`
	To       string    `json:"to"`
	Kind     string    `json:"kind"`
	Title    string    `json:"title"`
	Text     string    `json:"text"`
	Priority int       `json:"priority,omitempty"`
	Created  time.Time `json:"created"`
	Attempts int       `json:"attempts,omitempty"`
	NextTry  time.Time `json:"nextTry,omitzero"`
	Claim    string    `json:"claim,omitempty"`
	ClaimAt  time.Time `json:"claimAt,omitzero"`
}

func outKey(id string) string { return "out." + id }

// wants reports whether settings send a kind of notification.
func wants(st Settings, kind string) bool {
	switch kind {
	case duties.NoticeNeedsYou, duties.NoticeDecided:
		return st.Send.Questions
	case duties.NoticeFailed, duties.NoticePaused, duties.NoticeCap:
		return st.Send.Failures
	case KindDigest:
		return st.Send.Digest
	case KindTest:
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
	}
	return "Duty " + n.Duty, text, 3
}

// accept puts a notice of a duty in the outbox of each of its people whose
// settings send its kind.
func (s *Service) accept(ctx context.Context, n duties.Notice) {
	title, text, prio := words(n)
	seen := map[string]bool{}
	for _, who := range n.Who {
		if who == "" || seen[who] {
			continue
		}
		seen[who] = true
		rec, err := s.load(ctx, who)
		if err != nil {
			s.log.Warn("notify: the settings of a person did not read, so a notification did not go out", "to", who, "duty", n.Duty, "error", err)
			continue
		}
		if rec == nil || s.off(rec.Settings) != "" || !wants(rec.Settings, n.Kind) {
			continue
		}
		if err := s.put(ctx, &message{To: who, Kind: n.Kind, Title: title, Text: text, Priority: prio}); err != nil {
			s.log.Warn("notify: a notification did not reach the outbox", "to", who, "duty", n.Duty, "error", err)
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
	st, err := s.settingsOf(ctx, m.To)
	if err != nil {
		return now.Add(time.Minute)
	}
	if why := s.off(st); why != "" || !wants(st, m.Kind) {
		s.drop(ctx, key, e.Revision, "the person turned this kind of notification off")
		return time.Time{}
	}
	if end, quiet := quietUntil(st.QuietHours, now); quiet {
		m.NextTry, m.Claim, m.ClaimAt = end.UTC(), "", time.Time{}
		if value, err := json.Marshal(m); err == nil {
			_, _ = s.b.Update(ctx, key, value, e.Revision)
		}
		return end
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
	err = s.post(ctx, st, &m)
	if err == nil {
		if derr := s.b.Delete(ctx, key, rev); derr != nil {
			s.log.Warn("notify: a notification that went out stays in the outbox", "id", m.ID, "error", derr)
		}
		s.log.Info("notify: a notification went out", "to", m.To, "kind", m.Kind, "host", hostOf(s.server(st)))
		return time.Time{}
	}
	m.Attempts++
	if m.Attempts >= maxAttempts {
		s.log.Warn("notify: a notification did not go out, and the front drops it", "to", m.To, "kind", m.Kind, "host", hostOf(s.server(st)), "attempts", m.Attempts, "error", err)
		_ = s.b.Delete(ctx, key, rev)
		return time.Time{}
	}
	s.log.Warn("notify: a notification did not go out; it goes again", "to", m.To, "kind", m.Kind, "host", hostOf(s.server(st)), "attempt", m.Attempts, "error", err)
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
		s.log.Info("notify: the front dropped a notification", "key", key, "reason", why)
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

// publish is the body of a JSON publish of ntfy.
type publish struct {
	Topic    string `json:"topic"`
	Title    string `json:"title,omitempty"`
	Message  string `json:"message"`
	Priority int    `json:"priority,omitempty"`
}

// post sends a notification to the ntfy server of the settings, as a JSON
// publish to the address of the server. The token goes as a bearer token.
// No error names the topic or the token.
func (s *Service) post(ctx context.Context, st Settings, m *message) error {
	if s.postFn != nil {
		return s.postFn(ctx, st, m)
	}
	server := s.server(st)
	body, err := json.Marshal(publish{Topic: st.Ntfy.Topic, Title: m.Title, Message: m.Text, Priority: m.Priority})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, server+"/", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("the address of the server does not read")
	}
	req.Header.Set("Content-Type", "application/json")
	if st.Ntfy.Token != "" {
		req.Header.Set("Authorization", "Bearer "+st.Ntfy.Token)
	}
	client := s.public
	if s.cfg.listed(hostOf(server)) {
		client = s.listed
	}
	resp, err := client.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return fmt.Errorf("the server %s did not answer: %w", hostOf(server), err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	if resp.StatusCode/100 == 2 {
		return nil
	}
	var ans struct {
		Error string `json:"error"`
	}
	detail := http.StatusText(resp.StatusCode)
	if json.Unmarshal(b, &ans) == nil && strings.TrimSpace(ans.Error) != "" {
		detail = cutText(strings.TrimSpace(ans.Error), 200)
	}
	return fmt.Errorf("the server %s answered %d: %s", hostOf(server), resp.StatusCode, detail)
}

// reasonOf words an error of a send for a person.
func reasonOf(err error) string {
	msg := strings.TrimSpace(err.Error())
	if msg == "" {
		return "no reason was given"
	}
	return capitalize(strings.TrimSuffix(msg, ".")) + "."
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
