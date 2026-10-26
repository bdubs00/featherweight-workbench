package notify

// This file holds the settings of the notifications of a person (the
// contract of the build, section 11.1) and the Go functions that the
// routes and the MCP tools share: Get, Put, and Test. Each acts for the
// caller in ctx alone. A person keeps a list of channels. Each channel is
// one place that the front reaches, with its own switches. The quiet
// hours are one setting per person. A read never returns an address, a
// secret, or a token of a channel: it answers "set" or "".

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/mail"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/duties"
	"github.com/bdubs00/featherweight-workbench/faces"
	"github.com/bdubs00/featherweight-workbench/fleet"
	"github.com/bdubs00/featherweight-workbench/internal/bucket"
	"github.com/bdubs00/featherweight-workbench/internal/hostguard"
)

// Kinds of a channel.
const (
	KindNtfy    = "ntfy"    // a JSON publish to an ntfy server
	KindSlack   = "slack"   // an incoming webhook of Slack
	KindDiscord = "discord" // a webhook of a Discord channel
	KindTeams   = "teams"   // a webhook of a Teams workflow, with an Adaptive Card
	KindWebhook = "webhook" // the event of the notification as JSON, signed with a secret
	KindEmail   = "email"   // a plain mail through the SMTP server of the operator
	KindAgent   = "agent"   // a run of an agent that delivers the message with its own tools
	KindShared  = "shared"  // a shared channel of the operator, by its name
)

// Settings are the notifications of one person: the channels and the
// quiet hours.
type Settings struct {
	Channels   []Channel   `json:"channels"`
	QuietHours *QuietHours `json:"quietHours"`
}

// Channel is one place that the front reaches for a person: its id, its
// kind, its label, the kinds of notification that it sends, and the
// fields of its kind. URL and Secret serve the kinds slack, discord,
// teams, and webhook. Ntfy, Email, Agent, and Shared serve the kind of
// their name. A read answers URL, Secret, and Ntfy.Token as SetValue or
// "". In a write, SetValue keeps the stored value of the channel with the
// same id, "" clears it, and any other value replaces it.
type Channel struct {
	ID     string        `json:"id"`
	Kind   string        `json:"kind"`
	Label  string        `json:"label"`
	Send   Send          `json:"send"`
	URL    *string       `json:"url,omitempty"`
	Secret *string       `json:"secret,omitempty"`
	Ntfy   *Ntfy         `json:"ntfy,omitempty"`
	Email  *Email        `json:"email,omitempty"`
	Agent  *AgentChannel `json:"agent,omitempty"`
	Shared string        `json:"shared,omitempty"`
}

// Ntfy is the place of a channel of the kind ntfy: the server, "" for the
// server of workbench.notifications.ntfy.server, the topic, and the
// access token of the server, if it needs one.
type Ntfy struct {
	Server string `json:"server"`
	Topic  string `json:"topic"`
	Token  string `json:"token"`
}

// Email is the place of a channel of the kind email: the address of the
// person.
type Email struct {
	To string `json:"to"`
}

// AgentChannel is the place of a channel of the kind agent: the agent
// that delivers the message, the words of how it delivers it, and the
// budget of each run, which is required.
type AgentChannel struct {
	Agent  string         `json:"agent"`
	How    string         `json:"how"`
	Budget *duties.Budget `json:"budget"`
}

// Send says which kinds of notification a channel sends: Questions for a
// run of a duty that waits for the person and for a decision while the
// person was away, Failures for a failed run, a duty that paused itself,
// and a cap that a duty reached, and Digest for the daily digest. A field
// that a write leaves out is off.
type Send struct {
	Questions bool `json:"questions"`
	Failures  bool `json:"failures"`
	Digest    bool `json:"digest"`
}

// any reports whether the channel sends some kind.
func (s Send) any() bool { return s.Questions || s.Failures || s.Digest }

// QuietHours hold the notifications from From to To, both HH:MM in
// TimeZone, until they end. From and To are both empty for no quiet
// hours. TimeZone is the time zone of the person, for the quiet hours, the
// digest, and the day of the runs of the kind agent.
type QuietHours struct {
	From     string `json:"from"`
	To       string `json:"to"`
	TimeZone string `json:"timeZone"`
}

// SetValue is the value of a write-only field in a read when the front
// holds a value. In a write it keeps that value.
const SetValue = "set"

// MaxChannels bounds the channels of a person.
const MaxChannels = 10

// SharedChannel is a shared channel of the operator in a read: its name,
// its kind, and its label. A read never holds its address.
type SharedChannel struct {
	Name  string `json:"name"`
	Kind  string `json:"kind"`
	Label string `json:"label"`
}

// Answer is a read of the settings of the caller: the channels with each
// write-only field as SetValue or "", the quiet hours, and four fields
// that a write takes and leaves out: Offered, the kinds that this front
// serves now, Shared, the shared channels of the operator, ServerDefault,
// the ntfy server of a channel that names none (workbench.notifications.
// ntfy.server, or ""), and Off, the reason in words why no channel
// reaches the caller, or "".
type Answer struct {
	Channels      []Channel       `json:"channels"`
	QuietHours    *QuietHours     `json:"quietHours"`
	Offered       []Offer         `json:"offered"`
	Shared        []SharedChannel `json:"shared"`
	ServerDefault string          `json:"serverDefault"`
	Off           string          `json:"off"`
}

// Body is the body of a write: the shape of a read, so a read is a valid
// body. Channels replaces the channels, and a body without channels keeps
// them. QuietHours replaces the quiet hours, and a body without quietHours
// keeps them. Offered, Shared, ServerDefault, and Off are taken and left out.
type Body struct {
	Channels      []Channel       `json:"channels"`
	QuietHours    *QuietHours     `json:"quietHours,omitempty"`
	Offered       json.RawMessage `json:"offered,omitempty"`
	Shared        json.RawMessage `json:"shared,omitempty"`
	ServerDefault json.RawMessage `json:"serverDefault,omitempty"`
	Off           json.RawMessage `json:"off,omitempty"`
}

// Errors of the functions of the notifications.
var (
	// ErrNobody means that the caller names no person, so it has no
	// settings. A route answers 403.
	ErrNobody = fmt.Errorf("%w: notifications belong to a person, and this call names nobody", fw.ErrForbidden)
	// ErrUnknownChannel means that a test names a channel that the caller
	// does not have. A route answers 400.
	ErrUnknownChannel = errors.New("no channel of yours has this id")
)

// ProblemsError is a body that does not fit, with each problem in words.
// Each problem starts with the path of its field and a colon, such as
// channels[1].url: ...
type ProblemsError struct{ Problems []string }

func (e *ProblemsError) Error() string { return strings.Join(e.Problems, " ") }

// record is the entry of a person in the bucket: the subject, the kind of
// the principal, the settings with the values of the write-only fields,
// and the time of the last change. A record of the first shape, with
// ntfy and send beside the quiet hours, reads as a person with no channel
// and the same quiet hours.
type record struct {
	Subject  string    `json:"subject"`
	Kind     string    `json:"kind,omitempty"`
	Settings Settings  `json:"settings"`
	Changed  time.Time `json:"changed"`
}

// channel returns the channel of the id, or nil.
func (r *record) channel(id string) *Channel {
	if r == nil {
		return nil
	}
	for i := range r.Settings.Channels {
		if r.Settings.Channels[i].ID == id {
			return &r.Settings.Channels[i]
		}
	}
	return nil
}

// personKey is the key of the settings of a person: person.<subject in
// base64url>, so any subject fits a key.
func personKey(subject string) string {
	return "person." + base64.RawURLEncoding.EncodeToString([]byte(subject))
}

// Forms of the fields of a channel.
var (
	idPattern    = regexp.MustCompile(`^[A-Za-z0-9_-]{1,40}$`)
	topicPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	clockPattern = regexp.MustCompile(`^([01][0-9]|2[0-3]):[0-5][0-9]$`)
)

// Bounds of the fields of a channel, in bytes.
const (
	maxURL    = 2048
	maxSecret = 512
	maxHow    = 2000
)

// subjectOf returns the person of the caller in ctx, or ErrNobody.
func subjectOf(ctx context.Context) (*fw.Principal, error) {
	p := fw.PrincipalFrom(ctx)
	if p == nil || strings.TrimSpace(p.Subject) == "" {
		return nil, ErrNobody
	}
	return p, nil
}

// subjectIn returns the subject of the caller in ctx, or "".
func subjectIn(ctx context.Context) string {
	if p := fw.PrincipalFrom(ctx); p != nil {
		return strings.TrimSpace(p.Subject)
	}
	return ""
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

// settingsOf returns the settings of a person: no channel and no quiet
// hours for a person who has none.
func (s *Service) settingsOf(ctx context.Context, subject string) (Settings, error) {
	rec, err := s.load(ctx, subject)
	if err != nil {
		return Settings{}, err
	}
	if rec == nil {
		return Settings{QuietHours: &QuietHours{TimeZone: "UTC"}}, nil
	}
	return rec.Settings, nil
}

// ntfyServer returns the ntfy server of a channel: the server of the
// person, else the server of the configuration, else "".
func (s *Service) ntfyServer(n *Ntfy) string {
	if n != nil && n.Server != "" {
		return n.Server
	}
	return s.cfg.Ntfy.Server
}

// unusable says why a channel cannot send now, beside its switches, or "".
func (s *Service) unusable(ch Channel) string {
	if !s.cfg.offers(ch.Kind) {
		return fmt.Sprintf("The server does not offer the kind %s now.", ch.Kind)
	}
	switch ch.Kind {
	case KindNtfy:
		switch {
		case s.ntfyServer(ch.Ntfy) == "":
			return "No ntfy server is set. Name one in ntfy.server, or ask an operator to set workbench.notifications.ntfy.server. It never picks a public ntfy server by itself."
		case ch.Ntfy == nil || ch.Ntfy.Topic == "":
			return "No topic is set. Name the ntfy topic that your phone subscribes to in ntfy.topic."
		}
	case KindSlack, KindDiscord, KindTeams, KindWebhook:
		if ch.URL == nil || *ch.URL == "" {
			return "No address is set."
		}
		if why := s.addressProblem(ch.Kind, *ch.URL); why != "" {
			return why
		}
	case KindEmail:
		if ch.Email == nil || ch.Email.To == "" {
			return "No address is set."
		}
	case KindAgent:
		if ch.Agent == nil || budgetProblem(ch.Agent.Budget) != "" {
			return "The channel has no budget, so it runs no agent."
		}
	case KindShared:
		if s.cfg.sharedNamed(ch.Shared) == nil {
			return fmt.Sprintf("The shared channel %s does not exist in this server now.", ch.Shared)
		}
	}
	return ""
}

// off says why no channel reaches a person with the settings, or "".
func (s *Service) off(st Settings) string {
	if len(st.Channels) == 0 {
		return "You have no channel. Add one, such as ntfy, Slack, or a webhook, and the server tells you of questions, failures, and the daily digest. Activity in the page always shows what needs you."
	}
	var why []string
	for _, ch := range st.Channels {
		r := s.unusable(ch)
		if r == "" && !ch.Send.any() {
			r = "It sends no kind of notification. Turn on questions, failures, or digest."
		}
		if r == "" {
			return ""
		}
		why = append(why, nameOf(ch)+": "+r)
	}
	return "No channel reaches you. " + strings.Join(why, " ")
}

// nameOf names a channel for a person: its label, else its id.
func nameOf(ch Channel) string {
	if ch.Label != "" {
		return ch.Label
	}
	return ch.ID
}

// masked returns a value of a write-only field for a read.
func masked(v string) *string {
	if v == "" {
		return new("")
	}
	return new(SetValue)
}

// answer turns settings into a read: each channel with the fields of its
// kind alone, and each write-only field as SetValue or "".
func (s *Service) answer(st Settings) Answer {
	a := Answer{Channels: []Channel{}, Offered: s.cfg.offered(), Shared: []SharedChannel{}, ServerDefault: s.cfg.Ntfy.Server, Off: s.off(st)}
	for _, ch := range st.Channels {
		out := Channel{ID: ch.ID, Kind: ch.Kind, Label: ch.Label, Send: ch.Send}
		switch ch.Kind {
		case KindSlack, KindDiscord, KindTeams:
			out.URL = masked(deref(ch.URL))
		case KindWebhook:
			out.URL, out.Secret = masked(deref(ch.URL)), masked(deref(ch.Secret))
		case KindNtfy:
			n := Ntfy{}
			if ch.Ntfy != nil {
				n = *ch.Ntfy
			}
			n.Token = *masked(n.Token)
			out.Ntfy = &n
		case KindEmail:
			e := Email{}
			if ch.Email != nil {
				e = *ch.Email
			}
			out.Email = &e
		case KindAgent:
			ag := AgentChannel{}
			if ch.Agent != nil {
				ag = *ch.Agent
				if ag.Budget != nil {
					b := *ag.Budget
					ag.Budget = &b
				}
			}
			out.Agent = &ag
		case KindShared:
			out.Shared = ch.Shared
		}
		a.Channels = append(a.Channels, out)
	}
	if a.Offered == nil {
		a.Offered = []Offer{}
	}
	for _, sc := range s.cfg.Shared {
		a.Shared = append(a.Shared, SharedChannel{Name: sc.Name, Kind: sc.Kind, Label: sc.Label})
	}
	if st.QuietHours != nil {
		q := *st.QuietHours
		a.QuietHours = &q
	} else {
		a.QuietHours = &QuietHours{TimeZone: "UTC"}
	}
	return a
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// Get returns the settings of the caller in ctx.
func (s *Service) Get(ctx context.Context) (Answer, error) {
	p, err := subjectOf(ctx)
	if err != nil {
		return Answer{}, err
	}
	st, err := s.settingsOf(ctx, p.Subject)
	if err != nil {
		return Answer{}, err
	}
	return s.answer(st), nil
}

// Put writes the settings of the caller in ctx from the body. zone is the
// time zone of quiet hours that name none: the header X-Time-Zone of the
// page, or the argument timeZone of the tool, else UTC. A body that does
// not fit fails with a *ProblemsError.
func (s *Service) Put(ctx context.Context, body Body, zone string) (Answer, error) {
	p, err := subjectOf(ctx)
	if err != nil {
		return Answer{}, err
	}
	var out Settings
	_, err = bucket.Modify(ctx, s.b, personKey(p.Subject), storeAttempts, func(value []byte, exists bool) ([]byte, error) {
		var old record
		if exists {
			_ = json.Unmarshal(value, &old)
		}
		st, problems := s.settingsFrom(ctx, body, old.Settings, zone)
		if len(problems) > 0 {
			return nil, &ProblemsError{Problems: problems}
		}
		out = st
		return json.Marshal(record{Subject: p.Subject, Kind: p.Kind, Settings: st, Changed: s.now().UTC()})
	})
	var pe *ProblemsError
	if errors.As(err, &pe) {
		return Answer{}, pe
	}
	if err != nil {
		return Answer{}, fmt.Errorf("notify: write the settings of %s: %w", p.Subject, err)
	}
	kinds := make([]string, 0, len(out.Channels))
	for _, ch := range out.Channels {
		kinds = append(kinds, ch.ID+":"+ch.Kind)
	}
	s.log.Info("notify: a person changed the notifications", "by", p.Subject, "channels", strings.Join(kinds, " "), "off", s.off(out) != "")
	return s.answer(out), nil
}

// settingsFrom reads a body into settings with the stored settings old,
// and lists what does not fit, each problem with the path of its field.
func (s *Service) settingsFrom(ctx context.Context, body Body, old Settings, zone string) (Settings, []string) {
	var p []string
	add := func(path, format string, args ...any) { p = append(p, path+": "+fmt.Sprintf(format, args...)) }
	st := Settings{Channels: old.Channels}
	if body.Channels != nil {
		st.Channels = s.channelsFrom(ctx, body.Channels, old.Channels, add)
	}
	switch {
	case body.QuietHours != nil:
		q := QuietHours{From: strings.TrimSpace(body.QuietHours.From), To: strings.TrimSpace(body.QuietHours.To), TimeZone: strings.TrimSpace(body.QuietHours.TimeZone)}
		if q.TimeZone == "" {
			q.TimeZone = strings.TrimSpace(zone)
		}
		st.QuietHours = &q
	case old.QuietHours != nil:
		q := *old.QuietHours
		st.QuietHours = &q
	default:
		st.QuietHours = &QuietHours{TimeZone: strings.TrimSpace(zone)}
	}
	q := st.QuietHours
	if q.TimeZone == "" {
		q.TimeZone = "UTC"
	}
	if _, err := time.LoadLocation(q.TimeZone); err != nil || strings.EqualFold(q.TimeZone, "local") {
		add("quietHours.timeZone", "%q is not a time zone. Write one such as America/New_York.", q.TimeZone)
	}
	for _, f := range []struct{ name, v string }{{"from", q.From}, {"to", q.To}} {
		if f.v != "" && !clockPattern.MatchString(f.v) {
			add("quietHours."+f.name, "%q is not a time. Write HH:MM in 24 hours, such as 22:00.", f.v)
		}
	}
	switch {
	case (q.From == "") != (q.To == ""):
		add("quietHours.to", "Write both from and to, or neither for no quiet hours.")
	case q.From != "" && q.From == q.To:
		add("quietHours.to", "From and to are the same time. Write two times, or neither for no quiet hours.")
	}
	return st, p
}

// channelsFrom reads the channels of a body. A write-only field SetValue
// takes the value of the stored channel with the same id. A channel
// without an id gets one from its kind.
func (s *Service) channelsFrom(ctx context.Context, in, old []Channel, add func(path, format string, args ...any)) []Channel {
	if len(in) > MaxChannels {
		add("channels", "The list holds %d channels, and the most is %d.", len(in), MaxChannels)
	}
	stored := map[string]Channel{}
	for _, ch := range old {
		stored[ch.ID] = ch
	}
	taken := map[string]int{}
	for i, ch := range in {
		id := strings.TrimSpace(ch.ID)
		if id == "" {
			continue
		}
		if _, dup := taken[id]; dup {
			add(fmt.Sprintf("channels[%d].id", i), "The id %s names two channels.", id)
		}
		taken[id] = i
	}
	out := make([]Channel, 0, len(in))
	for i, ch := range in {
		path := fmt.Sprintf("channels[%d]", i)
		c := Channel{ID: strings.TrimSpace(ch.ID), Kind: strings.ToLower(strings.TrimSpace(ch.Kind)), Label: strings.TrimSpace(ch.Label), Send: ch.Send}
		var prev Channel
		if c.ID != "" {
			if !idPattern.MatchString(c.ID) {
				add(path+".id", "Write 1 to 40 of A-Z, a-z, 0-9, '-', and '_', such as phone.")
			}
			prev = stored[c.ID]
		}
		if len(c.Label) > maxLabel || strings.ContainsFunc(c.Label, isControl) {
			add(path+".label", "Write a label of at most %d characters on one line.", maxLabel)
		}
		if !s.cfg.offers(c.Kind) {
			offered := []string{}
			for _, o := range s.cfg.offered() {
				offered = append(offered, o.Kind)
			}
			if c.Kind == "" {
				add(path+".kind", "Pick a kind: %s.", strings.Join(offered, ", "))
			} else {
				add(path+".kind", "This server does not offer %q. Pick one of %s.", c.Kind, strings.Join(offered, ", "))
			}
			out = append(out, c)
			continue
		}
		if c.Label == "" {
			c.Label = kindLabel(c.Kind)
		}
		switch c.Kind {
		case KindSlack, KindDiscord, KindTeams, KindWebhook:
			u := keep(ch.URL, prev.URL)
			switch {
			case u == "" && deref(ch.URL) == SetValue:
				add(path+".url", "No address is stored for this channel. Write the address.")
			case u == "":
				add(path+".url", "Write the address of the webhook.")
			case len(u) > maxURL:
				add(path+".url", "The address is longer than %d characters.", maxURL)
			default:
				if why := s.addressProblem(c.Kind, u); why != "" {
					add(path+".url", "%s", why)
				}
			}
			c.URL = new(u)
			if c.Kind == KindWebhook {
				sec := keep(ch.Secret, prev.Secret)
				if sec != "" && (len(sec) > maxSecret || strings.ContainsFunc(sec, isSpaceOrControl)) {
					add(path+".secret", "Write the secret alone, without a space or a line break, in at most %d characters.", maxSecret)
				}
				c.Secret = new(sec)
			}
		case KindNtfy:
			var n, pn Ntfy
			if ch.Ntfy != nil {
				n = *ch.Ntfy
			}
			if prev.Ntfy != nil {
				pn = *prev.Ntfy
			}
			n.Server = strings.TrimRight(strings.TrimSpace(n.Server), "/")
			n.Topic = strings.TrimSpace(n.Topic)
			n.Token = keep(new(n.Token), new(pn.Token))
			switch {
			case n.Server == "" && s.cfg.Ntfy.Server == "":
				add(path+".ntfy.server", "No ntfy server is set. Name one, or ask an operator to set workbench.notifications.ntfy.server. It never picks a public ntfy server by itself.")
			case n.Server == "":
			case len(n.Server) > 512:
				add(path+".ntfy.server", "The address is longer than 512 characters.")
			case serverProblem(n.Server) != "":
				add(path+".ntfy.server", "%s.", capitalize(serverProblem(n.Server)))
			case s.cfg.ntfyHostProblem(n.Server) != "":
				add(path+".ntfy.server", "%s.", capitalize(s.cfg.ntfyHostProblem(n.Server)))
			}
			if !topicPattern.MatchString(n.Topic) {
				add(path+".ntfy.topic", "Write 1 to 64 of A-Z, a-z, 0-9, '-', and '_', such as alice-fw-8f3k.")
			}
			if n.Token != "" && (len(n.Token) > maxSecret || strings.ContainsFunc(n.Token, isSpaceOrControl)) {
				add(path+".ntfy.token", "Write the access token of the server alone, without a space or a line break, in at most %d characters.", maxSecret)
			}
			c.Ntfy = &n
		case KindEmail:
			to := ""
			if ch.Email != nil {
				to = strings.TrimSpace(ch.Email.To)
			}
			a, err := mail.ParseAddress(to)
			switch {
			case to == "" || err != nil || a.Address != to || len(to) > maxAddress:
				add(path+".email.to", "Write one mail address alone, such as alice@example.com.")
			case s.cfg.mailProblem(to, subjectIn(ctx)) != "":
				add(path+".email.to", "%s", s.cfg.mailProblem(to, subjectIn(ctx)))
			}
			c.Email = &Email{To: to}
		case KindAgent:
			var ag AgentChannel
			if ch.Agent != nil {
				ag = *ch.Agent
			}
			ag.Agent, ag.How = strings.TrimSpace(ag.Agent), strings.TrimSpace(ag.How)
			if why := s.agentProblem(ctx, ag.Agent); why != "" {
				add(path+".agent.agent", "%s", why)
			}
			switch {
			case ag.How == "":
				add(path+".agent.how", "Say in words how the agent delivers the message, such as: Send it to me on Signal.")
			case len(ag.How) > maxHow:
				add(path+".agent.how", "The text is longer than %d characters.", maxHow)
			}
			if why := budgetProblem(ag.Budget); why != "" {
				add(path+".agent.budget", "%s", why)
			}
			if ag.Budget != nil {
				b := *ag.Budget
				ag.Budget = &b
			}
			c.Agent = &ag
		case KindShared:
			c.Shared = strings.TrimSpace(ch.Shared)
			sc := s.cfg.sharedNamed(c.Shared)
			switch {
			case c.Shared == "":
				add(path+".shared", "Name a shared channel of this server.")
			case sc == nil:
				add(path+".shared", "%s is not a shared channel of this server.", c.Shared)
			case ch.Label == "":
				c.Label = sc.Label
			}
		}
		out = append(out, c)
	}
	for i := range out {
		if out[i].ID != "" {
			continue
		}
		base := out[i].Kind
		if base == "" || !idPattern.MatchString(base) {
			base = "channel"
		}
		id := base
		for n := 2; ; n++ {
			if _, used := taken[id]; !used {
				break
			}
			id = fmt.Sprintf("%s-%d", base, n)
		}
		out[i].ID = id
		taken[id] = i
	}
	return out
}

// keep resolves a write-only field of a write: SetValue keeps the stored
// value, "" or no field clears it, and any other value replaces it.
func keep(v, stored *string) string {
	switch w := strings.TrimSpace(deref(v)); w {
	case SetValue:
		return deref(stored)
	default:
		return w
	}
}

// addressProblem says why the address of a channel of a kind does not
// fit, in words, or "". Slack is on hooks.slack.com, Discord on
// discord.com or discordapp.com under /api/webhooks/, and each of the
// four kinds uses https. A host on a private address needs a glob of
// workbench.notifications.hosts. A webhook or a Teams workflow on such a
// host can also use http, because the operator vouches for its network.
func (s *Service) addressProblem(kind, raw string) string {
	u, err := url.Parse(raw)
	plain := err == nil && u.Scheme == "http" && (kind == KindWebhook || kind == KindTeams) && s.cfg.listed(strings.ToLower(u.Hostname()))
	if err != nil || (u.Scheme != "https" && !plain) || u.Hostname() == "" {
		switch kind {
		case KindSlack:
			return "Write the https address of an incoming webhook of Slack, on hooks.slack.com."
		case KindDiscord:
			return "Write the https address of a webhook of a Discord channel, on discord.com under /api/webhooks/."
		case KindTeams:
			return "Write the https address of a webhook of a Teams workflow."
		}
		return "Write an https address."
	}
	host := strings.ToLower(u.Hostname())
	switch {
	case u.User != nil:
		return "The address holds a user or a password. Put a secret in secret instead."
	case kind == KindSlack && host != "hooks.slack.com":
		return "Write the address of an incoming webhook of Slack, on hooks.slack.com."
	case kind == KindDiscord && ((host != "discord.com" && host != "discordapp.com") || !strings.HasPrefix(u.EscapedPath(), "/api/webhooks/")):
		return "Write the address of a webhook of a Discord channel, on discord.com under /api/webhooks/."
	case hostguard.Private(host) && !s.cfg.listed(host):
		return fmt.Sprintf("The host %s is a private address. An operator lists it in workbench.notifications.hosts first.", host)
	}
	return ""
}

// agentProblem says why the caller in ctx cannot name the agent in a
// channel of the kind agent, or "".
func (s *Service) agentProblem(ctx context.Context, agent string) string {
	if agent == "" {
		return "Name the agent that delivers the message."
	}
	err := faces.Check(ctx, s.a, agent, fw.ActionAgentRun)
	switch {
	case errors.Is(err, fw.ErrProfileNotFound), errors.Is(err, fw.ErrInvalidRequest):
		return fmt.Sprintf("%s is not an agent of this server or of a pool of the fleet.", agent)
	case errors.Is(err, fw.ErrForbidden), errors.Is(err, fw.ErrUnauthorized):
		return fmt.Sprintf("You have no right to run the agent %s.", agent)
	case err != nil:
		return fmt.Sprintf("The agent %s does not answer: %s.", agent, reasonText(err))
	}
	if _, err := fleet.QueueFor(fw.WithPrincipal(ctx, nil), s.a, agent, ""); err != nil {
		return fmt.Sprintf("%s has no queue: %s.", agent, reasonText(err))
	}
	return ""
}

// budgetProblem says why the budget of a channel of the kind agent does
// not fit, or "".
func budgetProblem(b *duties.Budget) string {
	switch {
	case b == nil || (b.MaxCostUSD <= 0 && b.MaxTurns <= 0 && b.MaxTokens <= 0 && b.MaxToolCalls <= 0):
		return "A budget is required, with at least one limit above zero, such as {\"maxCostUSD\": 0.02}. A channel without a budget does not save, because a paid default would be a silent cost."
	case b.MaxCostUSD < 0 || b.MaxTurns < 0 || b.MaxTokens < 0 || b.MaxToolCalls < 0:
		return "A limit is below zero."
	}
	return ""
}

func isControl(r rune) bool { return unicode.IsControl(r) }

func isSpaceOrControl(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }

// capitalize writes the first letter of s as a capital.
func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// Result is the outcome of the test of one channel: its id, whether the
// test went out, and the reason in words when it did not.
type Result struct {
	Channel string `json:"channel"`
	Sent    bool   `json:"sent"`
	Error   string `json:"error"`
}

// TestAnswer is the answer of a test: one result per channel.
type TestAnswer struct {
	Results []Result `json:"results"`
}

// testTimeout bounds a test of every channel.
const testTimeout = 30 * time.Second

// Test sends one test notification now to the channel of the caller in
// ctx with the id channel, or to every channel of the caller when channel
// is "". A test goes also during the quiet hours and whatever the switches
// of the channel say. A channel that the caller does not have fails with
// ErrUnknownChannel. A send that failed is a result with the reason in
// words.
func (s *Service) Test(ctx context.Context, channel string) (TestAnswer, error) {
	p, err := subjectOf(ctx)
	if err != nil {
		return TestAnswer{}, err
	}
	rec, err := s.load(ctx, p.Subject)
	if err != nil {
		return TestAnswer{}, err
	}
	var list []Channel
	if rec != nil {
		list = rec.Settings.Channels
	}
	if channel = strings.TrimSpace(channel); channel != "" {
		ch := rec.channel(channel)
		if ch == nil {
			return TestAnswer{}, fmt.Errorf("%w: %s", ErrUnknownChannel, channel)
		}
		list = []Channel{*ch}
	}
	ctx, cancel := context.WithTimeout(ctx, testTimeout)
	defer cancel()
	ans := TestAnswer{Results: make([]Result, len(list))}
	var wg sync.WaitGroup
	for i, ch := range list {
		wg.Go(func() {
			ans.Results[i] = s.testOne(ctx, ch, rec)
		})
	}
	wg.Wait()
	return ans, nil
}

// testOne sends the test notification to one channel.
func (s *Service) testOne(ctx context.Context, ch Channel, rec *record) Result {
	now := s.now().UTC()
	m := &message{
		ID: fw.NewID(), To: rec.Subject, Channel: ch.ID, Kind: EventTest, Title: "Test from featherweight",
		Text:     fmt.Sprintf("Notifications of featherweight reach you on %s. The duties tell you here of what you chose: questions, failures, or the daily digest.", nameOf(ch)),
		Priority: 3, At: now, Created: now,
	}
	t, err := s.targetOf(ctx, ch, rec)
	if err == nil {
		err = s.send(ctx, t, m)
	}
	if err != nil {
		s.log.Warn("notify: a test notification did not go out", "to", rec.Subject, "channel", ch.ID, "kind", t.kind, "where", t.where(), "error", err)
		return Result{Channel: ch.ID, Error: reasonOf(err)}
	}
	s.log.Info("notify: a test notification went out", "to", rec.Subject, "channel", ch.ID, "kind", t.kind, "where", t.where())
	return Result{Channel: ch.ID, Sent: true}
}

// reasonOf words an error of a send for a person: a sentence with a
// capital and a full stop.
func reasonOf(err error) string {
	msg := reasonText(err)
	if msg == "" {
		return "No reason was given."
	}
	return capitalize(msg) + "."
}
