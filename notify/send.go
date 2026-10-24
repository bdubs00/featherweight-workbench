package notify

// This file sends one notification to one channel (the contract of the
// build, section 11.2): a JSON publish to ntfy, a message to Slack,
// Discord, or a Teams workflow, the event of section 11.3 to a webhook, a
// plain mail through the SMTP server of the operator, or a run of an
// agent through the queue of the agent. No error and no log line holds an
// address, a secret, a token, or a password: an error names the host at
// most.

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/quotedprintable"
	"net"
	"net/http"
	"net/mail"
	"net/smtp"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/duties"
	"github.com/bdubs00/featherweight-workbench/faces"
	"github.com/bdubs00/featherweight-workbench/internal/bucket"
)

// MetaNotify is the key of the metadata of a run of the kind agent: the id
// of the channel that the run serves.
const MetaNotify = "workbench.notify"

// SignatureHeader is the header of the HMAC of the body of a webhook, as
// sha256=<hex>.
const SignatureHeader = "X-Featherweight-Signature"

// sendTimeout bounds one send.
const sendTimeout = 20 * time.Second

// target is one place that a notification goes to, with the values of its
// channel: the kind that sends, the id of the channel, the address, the
// secret, the ntfy server, topic, and token, the mail address, the agent,
// and the person of the channel with the time zone of the person.
type target struct {
	id, kind    string
	url, secret string
	ntfy        Ntfy
	to          string
	agent       *AgentChannel
	person      string
	personKind  string
	zone        string
}

// where names the place of a target for a log line or an error, without
// an address: the host, the mail server, or the agent.
func (t target) where() string {
	switch t.kind {
	case KindNtfy:
		return hostOf(t.ntfy.Server)
	case KindEmail:
		return "the mail server"
	case KindAgent:
		if t.agent != nil {
			return "the agent " + t.agent.Agent
		}
		return "an agent"
	}
	return hostOf(t.url)
}

// scrub takes every value of the target that is a secret out of a text,
// so that an answer of a server that repeats it does not leak it.
func (t target) scrub(s string) string {
	vals := []string{t.url, t.secret, t.ntfy.Token}
	if u, err := url.Parse(t.url); err == nil {
		vals = append(vals, u.EscapedPath(), u.Path, u.RawQuery)
	}
	for _, v := range vals {
		if len(v) >= 6 {
			s = strings.ReplaceAll(s, v, "[hidden]")
		}
	}
	return s
}

// permanentError is a send that cannot go out on a later try either, so
// the front drops the notification at once.
type permanentError struct{ err error }

func (e permanentError) Error() string { return e.err.Error() }
func (e permanentError) Unwrap() error { return e.err }

func permanent(format string, args ...any) error {
	return permanentError{fmt.Errorf(format, args...)}
}

// isPermanent reports whether an error of a send is permanent.
func isPermanent(err error) bool {
	var pe permanentError
	return errors.As(err, &pe)
}

// targetOf returns the target of a channel of a person.
func (s *Service) targetOf(ctx context.Context, ch Channel, rec *record) (target, error) {
	t := target{id: ch.ID, kind: ch.Kind}
	if rec != nil {
		t.person, t.personKind = rec.Subject, rec.Kind
		if q := rec.Settings.QuietHours; q != nil {
			t.zone = q.TimeZone
		}
	}
	if why := s.unusable(ch); why != "" {
		return t, permanent("%s", strings.TrimSuffix(why, "."))
	}
	switch ch.Kind {
	case KindNtfy:
		t.ntfy = *ch.Ntfy
		t.ntfy.Server = s.ntfyServer(ch.Ntfy)
	case KindSlack, KindDiscord, KindTeams:
		t.url = deref(ch.URL)
	case KindWebhook:
		t.url, t.secret = deref(ch.URL), deref(ch.Secret)
	case KindEmail:
		t.to = ch.Email.To
	case KindAgent:
		t.agent = ch.Agent
	case KindShared:
		st, err := s.sharedTarget(ctx, ch.Shared)
		if err != nil {
			return t, err
		}
		st.id, st.person, st.personKind, st.zone = t.id, t.person, t.personKind, t.zone
		return st, nil
	}
	return t, nil
}

// sharedTarget returns the target of a shared channel of the operator,
// with its address and its secret from the variables of the front.
func (s *Service) sharedTarget(ctx context.Context, name string) (target, error) {
	sc := s.cfg.sharedNamed(name)
	if sc == nil {
		return target{}, permanent("the shared channel %s does not exist in this front now", name)
	}
	t := target{id: duties.NotifyChannelPrefix + name, kind: sc.Kind}
	raw, err := s.secret(ctx, sc.URLEnv)
	if err != nil {
		return t, fmt.Errorf("the variable %s of the shared channel %s holds no address", sc.URLEnv, name)
	}
	secret := ""
	if sc.SecretEnv != "" {
		if secret, err = s.secret(ctx, sc.SecretEnv); err != nil {
			return t, fmt.Errorf("the variable %s of the shared channel %s holds no secret", sc.SecretEnv, name)
		}
	}
	if sc.Kind != KindNtfy {
		t.url, t.secret = raw, secret
		if why := s.addressProblem(sc.Kind, raw); why != "" {
			return t, permanent("the address of the shared channel %s does not fit: %s", name, strings.TrimSuffix(why, "."))
		}
		return t, nil
	}
	u, err := url.Parse(raw)
	topic := ""
	if err == nil {
		topic = strings.Trim(u.Path, "/")
	}
	if err != nil || serverProblem(u.Scheme+"://"+u.Host) != "" || !topicPattern.MatchString(topic) {
		return t, permanent("the address of the shared channel %s is not the address of an ntfy topic, such as https://ntfy.example.com/ops", name)
	}
	t.ntfy = Ntfy{Server: u.Scheme + "://" + u.Host, Topic: topic, Token: secret}
	return t, nil
}

// send sends a notification to a target.
func (s *Service) send(ctx context.Context, t target, m *message) error {
	if s.sendFn != nil {
		return s.sendFn(ctx, t, m)
	}
	ctx, cancel := context.WithTimeout(ctx, sendTimeout)
	defer cancel()
	link := s.cfg.linkOf(m)
	var err error
	switch t.kind {
	case KindNtfy:
		err = s.sendNtfy(ctx, t, m, link)
	case KindSlack:
		err = s.post(ctx, t.url, s.cfg.listed(hostOf(t.url)), slackBody(m, link), nil, "")
	case KindDiscord:
		err = s.post(ctx, t.url, s.cfg.listed(hostOf(t.url)), discordBody(m, link), nil, "")
	case KindTeams:
		err = s.post(ctx, t.url, s.cfg.listed(hostOf(t.url)), teamsBody(m, link), nil, "")
	case KindWebhook:
		err = s.post(ctx, t.url, s.cfg.listed(hostOf(t.url)), eventOf(m, link), nil, t.secret)
	case KindEmail:
		err = s.sendEmail(ctx, t, m, link)
	case KindAgent:
		err = s.sendAgent(ctx, t, m, link)
	default:
		err = permanent("the front does not send the kind %s", t.kind)
	}
	if err == nil {
		return nil
	}
	msg := t.scrub(err.Error())
	if isPermanent(err) {
		return permanentError{errors.New(msg)}
	}
	return errors.New(msg)
}

// publish is the body of a JSON publish of ntfy.
type publish struct {
	Topic    string `json:"topic"`
	Title    string `json:"title,omitempty"`
	Message  string `json:"message"`
	Priority int    `json:"priority,omitempty"`
	Click    string `json:"click,omitempty"`
}

// sendNtfy sends a JSON publish to the ntfy server of the target, with the
// token as a bearer token and the link as the address that a tap opens.
func (s *Service) sendNtfy(ctx context.Context, t target, m *message, link string) error {
	body := publish{Topic: t.ntfy.Topic, Title: m.Title, Message: m.Text, Priority: m.Priority, Click: link}
	header := map[string]string{}
	if t.ntfy.Token != "" {
		header["Authorization"] = "Bearer " + t.ntfy.Token
	}
	return s.post(ctx, t.ntfy.Server+"/", s.cfg.ntfyListed(hostOf(t.ntfy.Server)), body, header, "")
}

// slackEscape escapes the three characters that the text of Slack
// reserves.
func slackEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

// slackBody is the message of an incoming webhook of Slack: the title in
// bold, the text, and the link.
func slackBody(m *message, link string) map[string]string {
	text := "*" + slackEscape(m.Title) + "*\n" + slackEscape(m.Text)
	if link != "" {
		text += "\n<" + link + "|Open in featherweight>"
	}
	return map[string]string{"text": text}
}

// discordMax bounds the content of a message of Discord, in characters.
const discordMax = 2000

// discordBody is the message of a webhook of Discord: the title in bold,
// the text, and the link, with no mention that pings anybody.
func discordBody(m *message, link string) map[string]any {
	content := "**" + m.Title + "**\n" + m.Text
	tail := ""
	if link != "" {
		tail = "\n" + link
	}
	if n := discordMax - utf8.RuneCountInString(tail); utf8.RuneCountInString(content) > n {
		r := []rune(content)
		content = string(r[:n-1]) + "…"
	}
	return map[string]any{"content": content + tail, "allowed_mentions": map[string]any{"parse": []string{}}}
}

// teamsBody is the message of a webhook of a Teams workflow: one Adaptive
// Card with the title, the text, and a button that opens the link.
func teamsBody(m *message, link string) map[string]any {
	card := map[string]any{
		"$schema": "http://adaptivecards.io/schemas/adaptive-card.json",
		"type":    "AdaptiveCard",
		"version": "1.4",
		"body": []any{
			map[string]any{"type": "TextBlock", "text": m.Title, "weight": "Bolder", "size": "Medium", "wrap": true},
			map[string]any{"type": "TextBlock", "text": m.Text, "wrap": true},
		},
	}
	if link != "" {
		card["actions"] = []any{map[string]any{"type": "Action.OpenUrl", "title": "Open in featherweight", "url": link}}
	}
	return map[string]any{
		"type": "message",
		"attachments": []any{map[string]any{
			"contentType": "application/vnd.microsoft.card.adaptive",
			"contentUrl":  nil,
			"content":     card,
		}},
	}
}

// Event is the body of a webhook (the contract of the build, section
// 11.3). Kind is needs-you, decided, failed, paused, cap, digest, or test.
// A field that does not apply is "".
type Event struct {
	Kind     string    `json:"kind"`
	Title    string    `json:"title"`
	Text     string    `json:"text"`
	Duty     string    `json:"duty"`
	Agent    string    `json:"agent"`
	Run      string    `json:"run"`
	Link     string    `json:"link"`
	Priority int       `json:"priority"`
	At       time.Time `json:"at"`
}

// eventOf returns the event of a notification.
func eventOf(m *message, link string) Event {
	at := m.At
	if at.IsZero() {
		at = m.Created
	}
	return Event{Kind: m.Kind, Title: m.Title, Text: m.Text, Duty: m.Duty, Agent: m.Agent, Run: m.Run, Link: link, Priority: m.Priority, At: at.UTC().Truncate(time.Second)}
}

// Sign returns the value of SignatureHeader for a body and a secret:
// sha256= and the hex HMAC-SHA256 of the body.
func Sign(body []byte, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// post posts a body as JSON to an address with the client of its host: the
// open client for a host that the operator lists, else the client whose
// dialer refuses a private address. With a secret, the header
// SignatureHeader signs the body. An answer outside 2xx is an error that
// names the host and the status.
func (s *Service) post(ctx context.Context, address string, listed bool, body any, header map[string]string, secret string) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	host := hostOf(address)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, address, bytes.NewReader(b))
	if err != nil {
		return permanent("the address of the channel does not read")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "featherweight-notify")
	for k, v := range header {
		req.Header.Set(k, v)
	}
	if secret != "" {
		req.Header.Set(SignatureHeader, Sign(b, secret))
	}
	client := s.public
	if listed {
		client = s.listed
	}
	resp, err := client.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		if errors.Is(err, errPrivate) {
			return permanent("the host %s did not answer: %w", host, err)
		}
		return fmt.Errorf("the host %s did not answer: %w", host, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	if resp.StatusCode/100 == 2 {
		return nil
	}
	return fmt.Errorf("the host %s answered %d: %s", host, resp.StatusCode, detailOf(resp.StatusCode, data))
}

// detailOf words the body of an answer that failed: the field error or
// message of a JSON answer, or a short line of text, else the text of the
// status.
func detailOf(status int, data []byte) string {
	var ans struct {
		Error   json.RawMessage `json:"error"`
		Message string          `json:"message"`
	}
	if json.Unmarshal(data, &ans) == nil {
		var text string
		var nested struct {
			Message string `json:"message"`
		}
		switch {
		case json.Unmarshal(ans.Error, &text) == nil && strings.TrimSpace(text) != "":
			return cutText(strings.TrimSpace(text), 200)
		case json.Unmarshal(ans.Error, &nested) == nil && strings.TrimSpace(nested.Message) != "":
			return cutText(strings.TrimSpace(nested.Message), 200)
		case strings.TrimSpace(ans.Message) != "":
			return cutText(strings.TrimSpace(ans.Message), 200)
		}
	}
	line := strings.TrimSpace(string(data))
	if line != "" && len(line) <= 200 && !strings.ContainsAny(line, "\r\n<") && utf8.ValidString(line) {
		return line
	}
	return http.StatusText(status)
}

// sendEmail sends a plain mail through the SMTP server of the operator:
// STARTTLS when the server offers it, a login when the configuration
// names a user, and never a password in the clear.
func (s *Service) sendEmail(ctx context.Context, t target, m *message, link string) error {
	ec := s.cfg.Email
	if ec.Host == "" {
		return permanent("the front sends no mail now")
	}
	password := ""
	if ec.User != "" {
		v, err := s.secret(ctx, ec.PasswordEnv)
		if err != nil {
			return fmt.Errorf("the variable %s of the password of the mail server holds no value", ec.PasswordEnv)
		}
		password = v
	}
	from, err := mail.ParseAddress(ec.From)
	if err != nil {
		return permanent("the sender of workbench.notifications.email.from does not read")
	}
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(ec.Host, strconv.Itoa(s.cfg.smtpPort())))
	if err != nil {
		return fmt.Errorf("the mail server did not answer: %w", err)
	}
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	c, err := smtp.NewClient(conn, ec.Host)
	if err != nil {
		conn.Close()
		return fmt.Errorf("the mail server did not greet: %w", err)
	}
	defer c.Close()
	if ok, _ := c.Extension("STARTTLS"); ok {
		cfg := &tls.Config{ServerName: ec.Host, MinVersion: tls.VersionTLS12}
		if s.smtpTLS != nil {
			cfg = s.smtpTLS(ec.Host)
		}
		if err := c.StartTLS(cfg); err != nil {
			return fmt.Errorf("the mail server did not start TLS: %w", err)
		}
	}
	if ec.User != "" {
		if _, tlsOn := c.TLSConnectionState(); !tlsOn {
			return permanent("the mail server offers no STARTTLS, so the front does not send the password")
		}
		if err := c.Auth(smtp.PlainAuth("", ec.User, password, ec.Host)); err != nil {
			return fmt.Errorf("the mail server refused the login: %w", err)
		}
	}
	if err := c.Mail(from.Address); err != nil {
		return fmt.Errorf("the mail server refused the sender: %w", err)
	}
	if err := c.Rcpt(t.to); err != nil {
		return fmt.Errorf("the mail server refused the address: %w", err)
	}
	w, err := c.Data()
	if err != nil {
		return fmt.Errorf("the mail server refused the mail: %w", err)
	}
	if _, err := w.Write(mailOf(ec.From, from.Address, t.to, m, link, s.now())); err != nil {
		return fmt.Errorf("the mail did not go out: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("the mail server refused the mail: %w", err)
	}
	_ = c.Quit()
	return nil
}

// mailOf writes a plain mail: the headers and the text with the link, in
// quoted-printable UTF-8.
func mailOf(fromHeader, fromAddr, to string, m *message, link string, now time.Time) []byte {
	oneLine := func(s string) string { return strings.Join(strings.Fields(s), " ") }
	domain := "featherweight"
	if _, d, ok := strings.Cut(fromAddr, "@"); ok {
		domain = d
	}
	var b bytes.Buffer
	fmt.Fprintf(&b, "From: %s\r\n", oneLine(fromHeader))
	fmt.Fprintf(&b, "To: %s\r\n", to)
	fmt.Fprintf(&b, "Subject: %s\r\n", mime.QEncoding.Encode("utf-8", oneLine(m.Title)))
	fmt.Fprintf(&b, "Date: %s\r\n", now.UTC().Format(time.RFC1123Z))
	fmt.Fprintf(&b, "Message-ID: <%s@%s>\r\n", m.ID, domain)
	b.WriteString("MIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: quoted-printable\r\nAuto-Submitted: auto-generated\r\n\r\n")
	text := m.Text
	if link != "" {
		text += "\n\n" + link
	}
	qp := quotedprintable.NewWriter(&b)
	_, _ = qp.Write([]byte(strings.ReplaceAll(text, "\n", "\r\n")))
	_ = qp.Close()
	b.WriteString("\r\n")
	return b.Bytes()
}

// agentCount is the count of the runs of the kind agent of a person on
// one day, under agents.<subject in base64url>.
type agentCount struct {
	Day  string `json:"day"`
	Runs int    `json:"runs"`
}

// agentKey is the key of the count of the runs of the kind agent of a
// person.
func agentKey(subject string) string {
	return "agents." + strings.TrimPrefix(personKey(subject), "person.")
}

// sendAgent starts one run of the agent of the channel as the person,
// through the queue of the agent, with the budget of the channel and the
// words of how and the notification as its input. At most perDay runs go
// out per person and day, in the time zone of the person.
func (s *Service) sendAgent(ctx context.Context, t target, m *message, link string) error {
	ag := t.agent
	switch {
	case !s.cfg.Agent.Enabled:
		return permanent("the front does not offer the kind agent now")
	case ag == nil || budgetProblem(ag.Budget) != "":
		return permanent("the channel has no budget, so it runs no agent")
	case t.person == "":
		return permanent("a run of an agent needs a person")
	}
	kind := t.personKind
	if kind == "" {
		kind = fw.KindUser
	}
	who := fw.WithPrincipal(ctx, &fw.Principal{Subject: t.person, Kind: kind})
	if err := faces.Check(who, s.a, ag.Agent, fw.ActionAgentRun); err != nil {
		return permanent("the person has no right to run the agent %s now", ag.Agent)
	}
	loc := time.UTC
	if l, err := time.LoadLocation(t.zone); err == nil && t.zone != "" {
		loc = l
	}
	day := s.now().In(loc).Format(time.DateOnly)
	limit, capped := s.cfg.perDay(), false
	_, err := bucket.Modify(ctx, s.b, agentKey(t.person), storeAttempts, func(value []byte, exists bool) ([]byte, error) {
		var c agentCount
		if exists {
			_ = json.Unmarshal(value, &c)
		}
		if c.Day != day {
			c = agentCount{Day: day}
		}
		if c.Runs >= limit {
			capped = true
			return nil, nil
		}
		c.Runs++
		return json.Marshal(c)
	})
	if err != nil {
		return fmt.Errorf("the count of the runs of the agent did not read: %w", err)
	}
	if capped {
		return permanent("%d runs of agents went out for this person today, the most (workbench.notifications.agent.perDay)", limit)
	}
	input := ag.How + "\n\nThe message:\n\n" + m.Title + "\n\n" + m.Text
	if link != "" {
		input += "\n\n" + link
	}
	b := ag.Budget
	req := fw.RunRequest{
		ID:             fw.NewID(),
		Agent:          ag.Agent,
		Input:          input,
		Budget:         &fw.Budget{MaxCostUSD: b.MaxCostUSD, MaxTurns: b.MaxTurns, MaxTokens: b.MaxTokens, MaxToolCalls: b.MaxToolCalls},
		Metadata:       map[string]string{MetaNotify: t.id, fw.MetaTitle: cutText("Notify: "+m.Title, 120)},
		Principal:      t.person,
		PrincipalKind:  kind,
		IdempotencyKey: "notify/" + m.ID + "/" + t.id,
	}
	_, sent, err := s.enqueue(ctx, ag.Agent, "", []fw.RunRequest{req})
	if err == nil && sent == 0 {
		err = errors.New("no request went out")
	}
	if err != nil {
		return fmt.Errorf("the queue of the agent %s did not take the run: %s", ag.Agent, reasonText(err))
	}
	return nil
}

// reasonText words an error without the prefix of its package and without
// a full stop at its end.
func reasonText(err error) string {
	msg := err.Error()
	for _, p := range []string{"fleet: ", "notify: ", fw.ErrInvalidRequest.Error() + ": "} {
		msg = strings.ReplaceAll(msg, p, "")
	}
	return strings.TrimSuffix(strings.TrimSpace(msg), ".")
}
