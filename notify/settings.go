package notify

// This file holds the settings of the notifications of a person (the
// contract of the build, section 5) and the Go functions that the routes
// and the MCP tools share: Get, Put, and Test. Each acts for the caller in
// ctx alone. A read never returns the token: it answers "set" or "".

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/internal/bucket"
)

// Settings are the notifications of one person: where they go, which
// kinds go, and the quiet hours.
type Settings struct {
	Ntfy       Ntfy        `json:"ntfy"`
	Send       Send        `json:"send"`
	QuietHours *QuietHours `json:"quietHours"`
}

// Ntfy is where the notifications of a person go: the server, "" for the
// server of workbench.notifications.ntfy.server, the topic, and the access
// token of the server, if it needs one.
type Ntfy struct {
	Server string `json:"server"`
	Topic  string `json:"topic"`
	Token  string `json:"token"`
}

// Send says which kinds of notification go: Questions for a run of a duty
// that waits for the person and for a decision while the person was away,
// Failures for a failed run, a duty that paused itself, and a cap that a
// duty reached, and Digest for the daily digest.
type Send struct {
	Questions bool `json:"questions"`
	Failures  bool `json:"failures"`
	Digest    bool `json:"digest"`
}

// QuietHours hold the notifications from From to To, both HH:MM in
// TimeZone, until they end. From and To are both empty for no quiet
// hours. TimeZone is the time zone of the person, for the quiet hours and
// for the digest.
type QuietHours struct {
	From     string `json:"from"`
	To       string `json:"to"`
	TimeZone string `json:"timeZone"`
}

// TokenSet is the value of ntfy.token in a read when a token is set. In a
// write it keeps the token that the front holds.
const TokenSet = "set"

// Answer is a read of the settings of the caller: the settings with the
// token as TokenSet or "", ServerDefault, the value of
// workbench.notifications.ntfy.server or "", and Off, the reason in words
// why no notification reaches the caller, or "" when they do.
type Answer struct {
	Settings
	ServerDefault string `json:"serverDefault"`
	Off           string `json:"off,omitempty"`
}

// Body is the body of a write: the shape of a read, so a read is a valid
// body. A write replaces the settings. A field of send that the body
// leaves out takes its default: questions and failures on, digest off.
// Without quietHours the person has no quiet hours, in the time zone of
// the write. ntfy.token TokenSet keeps the token, "" clears it, and any
// other value replaces it. ServerDefault and Off are taken and left out.
type Body struct {
	Ntfy          Ntfy        `json:"ntfy"`
	Send          *SendBody   `json:"send,omitempty"`
	QuietHours    *QuietHours `json:"quietHours,omitempty"`
	ServerDefault string      `json:"serverDefault,omitempty"`
	Off           string      `json:"off,omitempty"`
}

// SendBody is send in a write, with each field optional.
type SendBody struct {
	Questions *bool `json:"questions,omitempty"`
	Failures  *bool `json:"failures,omitempty"`
	Digest    *bool `json:"digest,omitempty"`
}

// defaultSend is send for a person who did not choose.
var defaultSend = Send{Questions: true, Failures: true}

// Errors of the functions of the notifications.
var (
	// ErrNobody means that the caller names no person, so it has no
	// settings. A route answers 403.
	ErrNobody = fmt.Errorf("%w: notifications belong to a person, and this call names nobody", fw.ErrForbidden)
	// ErrUpstream means that the notification did not go out: the server
	// failed, or the settings send nothing. A route answers 502.
	ErrUpstream = errors.New("notify: the notification did not go out")
)

// ProblemsError is a body that does not fit, with each problem in words.
// Each problem starts with the dotted path of its field and a colon.
type ProblemsError struct{ Problems []string }

func (e *ProblemsError) Error() string { return strings.Join(e.Problems, " ") }

// record is the entry of a person in the bucket: the subject, the
// settings with the token, and the time of the last change.
type record struct {
	Subject  string    `json:"subject"`
	Settings Settings  `json:"settings"`
	Changed  time.Time `json:"changed"`
}

// personKey is the key of the settings of a person: person.<subject in
// base64url>, so any subject fits a key.
func personKey(subject string) string {
	return "person." + base64.RawURLEncoding.EncodeToString([]byte(subject))
}

// topicPattern is the form of an ntfy topic.
var topicPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// clockPattern is a time of the day, HH:MM in 24 hours.
var clockPattern = regexp.MustCompile(`^([01][0-9]|2[0-3]):[0-5][0-9]$`)

// subjectOf returns the person of the caller in ctx, or ErrNobody.
func subjectOf(ctx context.Context) (string, error) {
	p := fw.PrincipalFrom(ctx)
	if p == nil || strings.TrimSpace(p.Subject) == "" {
		return "", ErrNobody
	}
	return p.Subject, nil
}

// load reads the settings of a person, or nil when the person has none.
func (s *Service) load(ctx context.Context, subject string) (*record, error) {
	e, err := s.b.Get(ctx, personKey(subject))
	if errors.Is(err, bucket.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("notify: read the settings of %s: %w", subject, err)
	}
	var rec record
	if err := json.Unmarshal(e.Value, &rec); err != nil {
		return nil, fmt.Errorf("notify: decode the settings of %s: %w", subject, err)
	}
	return &rec, nil
}

// settingsOf returns the settings of a person, with the defaults for a
// person who has none.
func (s *Service) settingsOf(ctx context.Context, subject string) (Settings, error) {
	rec, err := s.load(ctx, subject)
	if err != nil {
		return Settings{}, err
	}
	if rec == nil {
		return Settings{Send: defaultSend, QuietHours: &QuietHours{TimeZone: "UTC"}}, nil
	}
	return rec.Settings, nil
}

// server returns the server of the settings: the server of the person,
// else the server of the configuration, else "".
func (s *Service) server(st Settings) string {
	if st.Ntfy.Server != "" {
		return st.Ntfy.Server
	}
	return s.cfg.Ntfy.Server
}

// off says why no notification reaches a person with the settings, or "".
func (s *Service) off(st Settings) string {
	switch {
	case s.server(st) == "":
		return "No ntfy server is set. Name one in ntfy.server, or ask an operator to set workbench.notifications.ntfy.server. The front never picks a public server by itself."
	case st.Ntfy.Topic == "":
		return "No topic is set. Name the ntfy topic that your phone subscribes to in ntfy.topic."
	case !st.Send.Questions && !st.Send.Failures && !st.Send.Digest:
		return "Every kind of notification is off in send."
	}
	return ""
}

// answer turns settings into a read.
func (s *Service) answer(st Settings) Answer {
	a := Answer{Settings: st, ServerDefault: s.cfg.Ntfy.Server, Off: s.off(st)}
	if a.Ntfy.Token != "" {
		a.Ntfy.Token = TokenSet
	}
	if a.QuietHours != nil {
		q := *a.QuietHours
		a.QuietHours = &q
	}
	return a
}

// Get returns the settings of the caller in ctx.
func (s *Service) Get(ctx context.Context) (Answer, error) {
	subject, err := subjectOf(ctx)
	if err != nil {
		return Answer{}, err
	}
	st, err := s.settingsOf(ctx, subject)
	if err != nil {
		return Answer{}, err
	}
	return s.answer(st), nil
}

// Put replaces the settings of the caller in ctx with the body. zone is
// the time zone of quiet hours that name none: the header X-Time-Zone of
// the page, or the argument timeZone of the tool, else UTC. A body that
// does not fit fails with a *ProblemsError.
func (s *Service) Put(ctx context.Context, body Body, zone string) (Answer, error) {
	subject, err := subjectOf(ctx)
	if err != nil {
		return Answer{}, err
	}
	st, problems := s.settingsFrom(body, zone)
	if len(problems) > 0 {
		return Answer{}, &ProblemsError{Problems: problems}
	}
	var out Settings
	_, err = bucket.Modify(ctx, s.b, personKey(subject), storeAttempts, func(value []byte, exists bool) ([]byte, error) {
		next := st
		var old record
		if exists {
			_ = json.Unmarshal(value, &old)
		}
		switch strings.TrimSpace(body.Ntfy.Token) {
		case TokenSet:
			next.Ntfy.Token = old.Settings.Ntfy.Token
		case "":
			next.Ntfy.Token = ""
		}
		out = next
		return json.Marshal(record{Subject: subject, Settings: next, Changed: s.now().UTC()})
	})
	if err != nil {
		return Answer{}, fmt.Errorf("notify: write the settings of %s: %w", subject, err)
	}
	s.log.Info("notify: a person changed the notifications", "by", subject, "server", hostOf(s.server(out)), "off", s.off(out) != "")
	return s.answer(out), nil
}

// settingsFrom reads a body into settings, with the token of the body, and
// lists what does not fit, each problem with the dotted path of its field.
func (s *Service) settingsFrom(body Body, zone string) (Settings, []string) {
	var p []string
	add := func(format string, args ...any) { p = append(p, fmt.Sprintf(format, args...)) }
	st := Settings{
		Ntfy: Ntfy{
			Server: strings.TrimRight(strings.TrimSpace(body.Ntfy.Server), "/"),
			Topic:  strings.TrimSpace(body.Ntfy.Topic),
			Token:  strings.TrimSpace(body.Ntfy.Token),
		},
		Send: defaultSend,
	}
	if st.Ntfy.Server != "" {
		switch {
		case len(st.Ntfy.Server) > 512:
			add("ntfy.server: the address is longer than 512 characters.")
		case serverProblem(st.Ntfy.Server) != "":
			add("ntfy.server: %s.", capitalize(serverProblem(st.Ntfy.Server)))
		case s.cfg.hostProblem(st.Ntfy.Server) != "":
			add("ntfy.server: %s.", capitalize(s.cfg.hostProblem(st.Ntfy.Server)))
		}
	}
	if st.Ntfy.Topic != "" && !topicPattern.MatchString(st.Ntfy.Topic) {
		add("ntfy.topic: write 1 to 64 of A-Z, a-z, 0-9, '-', and '_', such as alice-fw-8f3k.")
	}
	if t := st.Ntfy.Token; t != "" && t != TokenSet {
		if len(t) > 512 || strings.ContainsFunc(t, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) {
			add("ntfy.token: write the access token of the server alone, without a space or a line break, in at most 512 characters.")
		}
	}
	if b := body.Send; b != nil {
		for _, f := range []struct {
			set *bool
			to  *bool
		}{{b.Questions, &st.Send.Questions}, {b.Failures, &st.Send.Failures}, {b.Digest, &st.Send.Digest}} {
			if f.set != nil {
				*f.to = *f.set
			}
		}
	}
	q := QuietHours{TimeZone: strings.TrimSpace(zone)}
	if body.QuietHours != nil {
		q = QuietHours{From: strings.TrimSpace(body.QuietHours.From), To: strings.TrimSpace(body.QuietHours.To), TimeZone: strings.TrimSpace(body.QuietHours.TimeZone)}
		if q.TimeZone == "" {
			q.TimeZone = strings.TrimSpace(zone)
		}
	}
	if q.TimeZone == "" {
		q.TimeZone = "UTC"
	}
	if _, err := time.LoadLocation(q.TimeZone); err != nil || strings.EqualFold(q.TimeZone, "local") {
		add("quietHours.timeZone: %q is not a time zone; write one such as America/New_York.", q.TimeZone)
	}
	for _, f := range []struct{ name, v string }{{"from", q.From}, {"to", q.To}} {
		if f.v != "" && !clockPattern.MatchString(f.v) {
			add("quietHours.%s: %q is not a time; write HH:MM in 24 hours, such as 22:00.", f.name, f.v)
		}
	}
	switch {
	case (q.From == "") != (q.To == ""):
		add("quietHours.to: write both from and to, or neither for no quiet hours.")
	case q.From != "" && q.From == q.To:
		add("quietHours.to: from and to are the same time; write two times, or neither for no quiet hours.")
	}
	st.QuietHours = &q
	return st, p
}

// capitalize writes the first letter of s as a capital.
func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// Test sends one test notification to the caller in ctx now, also during
// the quiet hours. It returns an error that wraps ErrUpstream, with the
// reason in words, when the notification did not go out.
func (s *Service) Test(ctx context.Context) error {
	subject, err := subjectOf(ctx)
	if err != nil {
		return err
	}
	st, err := s.settingsOf(ctx, subject)
	if err != nil {
		return err
	}
	if s.server(st) == "" || st.Ntfy.Topic == "" {
		return fmt.Errorf("%w: %s", ErrUpstream, s.off(st))
	}
	m := &message{
		To: subject, Kind: KindTest, Title: "Test from featherweight",
		Text: "Notifications reach you here. The duties tell you of questions, failures, and a daily digest, as you chose.",
	}
	if err := s.post(ctx, st, m); err != nil {
		s.log.Warn("notify: a test notification did not go out", "to", subject, "host", hostOf(s.server(st)), "error", err)
		return fmt.Errorf("%w: %s", ErrUpstream, reasonOf(err))
	}
	s.log.Info("notify: a test notification went out", "to", subject, "host", hostOf(s.server(st)))
	return nil
}
