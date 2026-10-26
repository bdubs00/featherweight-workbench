package notify

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/duties"
	"github.com/bdubs00/featherweight-workbench/internal/bucket"
)

// byHost returns the last request of a hook server for each host.
func byHost(h *hookServer) map[string]hit {
	out := map[string]hit{}
	for _, x := range h.list() {
		out[x.Host] = x
	}
	return out
}

// A notice reaches Slack, Discord, a Teams workflow, and a webhook, each
// with the body of its kind and the link to the run. The webhook carries
// the event of the contract, signed with its secret. No log line holds an
// address or a secret.
func TestSlackDiscordTeamsWebhook(t *testing.T) {
	h := newHook(t)
	f := newFront(t, "notifications:\n  link: https://fleet.example.com/bossmode/\n", true)
	through(f.s, h)
	f.duty(t, "report", nil)
	f.set(t, "alice", Body{Channels: []Channel{
		{ID: "room", Kind: KindSlack, Send: on, URL: new(slackURL)},
		{ID: "dc", Kind: KindDiscord, Send: on, URL: new("https://discord.com/api/webhooks/1/discordsecret")},
		{ID: "teams", Kind: KindTeams, Send: on, URL: new("https://prod-1.westus.logic.azure.com/workflows/w/triggers/manual/paths/invoke?sig=teamssecret")},
		{ID: "ha", Kind: KindWebhook, Send: on, URL: new("https://ha.example.com/api/webhook/hasecret"), Secret: new("hmac-key")},
	}})
	f.tellRun(t, duties.NoticeNeedsYou, "report", "01KRUN", "The agent triage asks to run git.push. Decide in Activity.")
	f.pass(t)
	hits := byHost(h)
	if len(hits) != 4 {
		t.Fatalf("the hooks got %+v", h.list())
	}
	link := "https://fleet.example.com/bossmode/#activity/01KRUN"
	slack := hits["hooks.slack.com"]
	if slack.Path != "/services/T0/B0/slacksecret" || string(slack.Body) != `{"text":"*Duty report needs you*\nThe agent triage asks to run git.push. Decide in Activity.\n\u003c`+link+`|Open in featherweight\u003e"}` || slack.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("slack got %s %s", slack.Path, slack.Body)
	}
	var dc struct {
		Content         string `json:"content"`
		AllowedMentions struct {
			Parse []string `json:"parse"`
		} `json:"allowed_mentions"`
	}
	if err := json.Unmarshal(hits["discord.com"].Body, &dc); err != nil || dc.Content != "**Duty report needs you**\nThe agent triage asks to run git.push. Decide in Activity.\n"+link || dc.AllowedMentions.Parse == nil || len(dc.AllowedMentions.Parse) != 0 {
		t.Fatalf("discord got %s", hits["discord.com"].Body)
	}
	var teams struct {
		Type        string `json:"type"`
		Attachments []struct {
			ContentType string `json:"contentType"`
			Content     struct {
				Type    string `json:"type"`
				Version string `json:"version"`
				Body    []struct {
					Type, Text, Weight string
				} `json:"body"`
				Actions []struct {
					Type, Title, URL string
				} `json:"actions"`
			} `json:"content"`
		} `json:"attachments"`
	}
	tm := hits["prod-1.westus.logic.azure.com"]
	if err := json.Unmarshal(tm.Body, &teams); err != nil || tm.Query != "sig=teamssecret" || teams.Type != "message" || len(teams.Attachments) != 1 {
		t.Fatalf("teams got %s", tm.Body)
	}
	card := teams.Attachments[0]
	if card.ContentType != "application/vnd.microsoft.card.adaptive" || card.Content.Type != "AdaptiveCard" || card.Content.Version != "1.4" || len(card.Content.Body) != 2 ||
		card.Content.Body[0].Text != "Duty report needs you" || card.Content.Body[0].Weight != "Bolder" || card.Content.Body[1].Text != "The agent triage asks to run git.push. Decide in Activity." ||
		len(card.Content.Actions) != 1 || card.Content.Actions[0].Type != "Action.OpenUrl" || card.Content.Actions[0].URL != link {
		t.Fatalf("the card = %+v", card)
	}
	ha := hits["ha.example.com"]
	e := decodeEvent(t, ha.Body)
	at := e.At
	e.At = time.Time{}
	if e != (Event{Kind: "needs-you", Title: "Duty report needs you", Text: "The agent triage asks to run git.push. Decide in Activity.", Duty: "report", Agent: "triage", Run: "01KRUN", Link: link, Priority: 4}) ||
		at.IsZero() || at.Nanosecond() != 0 || at.Location() != time.UTC || !strings.Contains(string(ha.Body), `"at":"`+at.Format(time.RFC3339)+`"`) {
		t.Fatalf("the event = %+v", e)
	}
	mac := hmac.New(sha256.New, []byte("hmac-key"))
	mac.Write(ha.Body)
	if got := ha.Header.Get(SignatureHeader); got != "sha256="+hex.EncodeToString(mac.Sum(nil)) || got != Sign(ha.Body, "hmac-key") {
		t.Fatalf("the signature = %q", got)
	}
	// A notice without a run links to the duty, and a test to the page.
	f.tell(t, duties.NoticePaused, "report", "3 runs failed in a row.")
	f.pass(t)
	if e := decodeEvent(t, byHost(h)["ha.example.com"].Body); e.Kind != "paused" || e.Link != "https://fleet.example.com/bossmode/#duties/report" || e.Run != "" {
		t.Fatalf("the event of a pause = %+v", e)
	}
	ans, err := f.s.Test(as("alice"), "ha")
	if err != nil || len(ans.Results) != 1 || !ans.Results[0].Sent {
		t.Fatalf("a test = %+v, %v", ans, err)
	}
	if e := decodeEvent(t, byHost(h)["ha.example.com"].Body); e.Kind != "test" || e.Link != "https://fleet.example.com/bossmode/" || e.Duty != "" || e.Priority != 3 {
		t.Fatalf("the event of a test = %+v", e)
	}
	// A webhook without a secret carries no signature.
	f.set(t, "alice", Body{Channels: []Channel{{ID: "ha", Kind: KindWebhook, Send: on, URL: new(SetValue), Secret: new("")}}})
	if ans, err := f.s.Test(as("alice"), ""); err != nil || !ans.Results[0].Sent {
		t.Fatalf("a test without a secret = %+v, %v", ans, err)
	}
	if last := h.list()[len(h.list())-1]; last.Header.Get(SignatureHeader) != "" {
		t.Fatalf("a webhook without a secret is signed: %v", last.Header)
	}
	noSecrets(t, "the log", f.logs.String(), "slacksecret", "discordsecret", "teamssecret", "hasecret", "hmac-key")
}

// The test route sends to one channel, or to every channel with an empty
// body, also in the quiet hours and with every switch off. A channel that
// fails is a result with the reason in words and no address, and the
// answer is 200. A channel that the person does not have gets 400.
func TestTestRoute(t *testing.T) {
	h := newHook(t)
	f := newFront(t, "", true)
	through(f.s, h)
	f.clock.Set(time.Date(2026, 10, 23, 23, 0, 0, 0, time.UTC))
	f.set(t, "alice", Body{Channels: []Channel{
		{ID: "room", Kind: KindSlack, URL: new(slackURL)},
		{ID: "dc", Kind: KindDiscord, URL: new("https://discord.com/api/webhooks/1/discordsecret")},
	}, QuietHours: &QuietHours{From: "22:00", To: "07:00", TimeZone: "UTC"}})
	resp, body := send(t, http.MethodPost, f.url+"/v1/notifications/test", "alice", "")
	if resp.StatusCode != http.StatusOK || body != `{"results":[{"channel":"room","sent":true,"error":""},{"channel":"dc","sent":true,"error":""}]}`+"\n" {
		t.Fatalf("a test of every channel = %d %s", resp.StatusCode, body)
	}
	if hits := byHost(h); !strings.HasPrefix(string(hits["hooks.slack.com"].Body), `{"text":"*Test from featherweight*\nNotifications of featherweight reach you on Slack.`) {
		t.Fatalf("the test = %s", hits["hooks.slack.com"].Body)
	}
	h.setAnswer(http.StatusNotFound, "no_service")
	resp, body = send(t, http.MethodPost, f.url+"/v1/notifications/test", "alice", `{"channel": "room"}`)
	if resp.StatusCode != http.StatusOK || body != `{"results":[{"channel":"room","sent":false,"error":"The host hooks.slack.com answered 404: no_service."}]}`+"\n" {
		t.Fatalf("a failed test = %d %s", resp.StatusCode, body)
	}
	h.setAnswer(http.StatusBadRequest, `{"message": "Invalid Webhook Token", "code": 50027}`)
	_, body = send(t, http.MethodPost, f.url+"/v1/notifications/test", "alice", `{"channel": "dc"}`)
	if !strings.Contains(body, `"error":"The host discord.com answered 400: Invalid Webhook Token."`) {
		t.Fatalf("a refused test = %s", body)
	}
	resp, body = send(t, http.MethodPost, f.url+"/v1/notifications/test", "alice", `{"channel": "pager"}`)
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, "No channel of yours has this id: pager.") {
		t.Fatalf("a test of an unknown channel = %d %s", resp.StatusCode, body)
	}
	if resp, _ := send(t, http.MethodPost, f.url+"/v1/notifications/test", "alice", `{"channels": "room"}`); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("a body that does not read = %d", resp.StatusCode)
	}
	resp, body = send(t, http.MethodPost, f.url+"/v1/notifications/test", "bob", "")
	if resp.StatusCode != http.StatusOK || body != `{"results":[]}`+"\n" {
		t.Fatalf("a test without a channel = %d %s", resp.StatusCode, body)
	}
	noSecrets(t, "the log", f.logs.String(), "slacksecret", "discordsecret")
}

// An ntfy channel sends the JSON publish of ntfy with the token as a
// bearer token and the link as the address of a tap. An error never names
// the topic or the token.
func TestNtfy(t *testing.T) {
	n := newNtfy(t)
	f := newFront(t, "notifications:\n  ntfy:\n    server: "+n.URL+"\n  link: https://fleet.example.com/bossmode/\n", true)
	f.duty(t, "report", nil)
	f.set(t, "alice", Body{Channels: []Channel{{ID: "phone", Kind: KindNtfy, Send: on, Ntfy: &Ntfy{Topic: "alice-fw", Token: "tk_1"}}}})
	f.tellRun(t, duties.NoticeFailed, "report", "01KRUN", "The run failed.")
	f.pass(t)
	got, tokens := n.published()
	if len(got) != 1 || got[0] != (publish{Topic: "alice-fw", Title: "Duty report failed", Message: "The run failed.", Priority: 4, Click: "https://fleet.example.com/bossmode/#activity/01KRUN"}) || tokens[0] != "Bearer tk_1" {
		t.Fatalf("the publish = %+v %v", got, tokens)
	}
	n.setStatus(http.StatusForbidden)
	ans, err := f.s.Test(as("alice"), "phone")
	if err != nil || ans.Results[0].Sent || !strings.Contains(ans.Results[0].Error, "answered 403: unauthorized.") {
		t.Fatalf("a refused test = %+v, %v", ans, err)
	}
	noSecrets(t, "the error", ans.Results[0].Error, "alice-fw", "tk_1")
	noSecrets(t, "the log", f.logs.String(), "tk_1")
}

// A literal private address and the name localhost need a glob of
// workbench.notifications.hosts at the write. Another name that resolves
// to a loopback address passes the write, and the dialer refuses it at
// the send. A host that the operator
// lists is reached.
func TestPrivateAddress(t *testing.T) {
	h := newHook(t)
	port := h.Listener.Addr().(*net.TCPAddr).Port
	f := newFront(t, "", true)
	trust(f.s, h)
	direct := fmt.Sprintf("https://127.0.0.1:%d/hook", port)
	if got := f.problems(t, "alice", Body{Channels: []Channel{{Kind: KindWebhook, URL: new(direct)}}}); len(got) != 1 || !strings.HasPrefix(got[0], "channels[0].url: The host 127.0.0.1 is a private address.") {
		t.Fatalf("a private address = %q", got)
	}
	if got := f.problems(t, "alice", Body{Channels: []Channel{{Kind: KindWebhook, URL: new(fmt.Sprintf("https://localhost:%d/hook", port))}}}); len(got) != 1 || !strings.HasPrefix(got[0], "channels[0].url: The host localhost is a private address.") {
		t.Fatalf("localhost = %q", got)
	}
	// The write knows the literal addresses and the name localhost alone.
	// The absolute name localhost. resolves to a loopback address too.
	f.set(t, "alice", Body{Channels: []Channel{{ID: "ha", Kind: KindWebhook, Send: on, URL: new(fmt.Sprintf("https://localhost.:%d/hook", port))}}})
	ans, err := f.s.Test(as("alice"), "")
	if err != nil || ans.Results[0].Sent || !strings.Contains(ans.Results[0].Error, "loopback, private, or link-local") {
		t.Fatalf("a test of a name on a loopback address = %+v, %v", ans, err)
	}
	// A notice to such a channel goes once and drops, as a retry would
	// meet the same refusal.
	f.duty(t, "report", nil)
	f.tell(t, duties.NoticeFailed, "report", "It failed.")
	f.pass(t)
	if keys, _ := bucket.KeysUnder(context.Background(), f.b, "out"); len(keys) != 0 || len(h.list()) != 0 {
		t.Fatalf("the outbox holds %v, and the hook got %d", keys, len(h.list()))
	}
	g := newFront(t, "notifications:\n  hosts: [\"127.0.0.1\"]\n", true)
	trust(g.s, h)
	g.set(t, "alice", Body{Channels: []Channel{{ID: "ha", Kind: KindWebhook, Send: on, URL: new(direct)}}})
	if ans, err := g.s.Test(as("alice"), ""); err != nil || !ans.Results[0].Sent || len(h.list()) != 1 {
		t.Fatalf("a test of a listed host = %+v, %v", ans, err)
	}
	// An ntfy server of a person keeps its rule: any host at the write,
	// and no private address at the send.
	n := newNtfy(t)
	f.set(t, "alice", Body{Channels: []Channel{{ID: "phone", Kind: KindNtfy, Send: on, Ntfy: &Ntfy{Server: n.URL, Topic: "alice"}}}})
	if ans, _ := f.s.Test(as("alice"), ""); ans.Results[0].Sent || !strings.Contains(ans.Results[0].Error, "loopback, private, or link-local") {
		t.Fatalf("a test of a private ntfy server = %+v", ans)
	}
}

// allowExample is the line of workbench.notifications.email.allow that
// lets a channel mail any address at example.com.
const allowExample = "    allow: [\"*@example.com\"]\n"

// smtpBlock is the block of a front that mails through the server at
// addr, with more lines after it.
func smtpBlock(t *testing.T, addr, more string) string {
	t.Helper()
	host, port, _ := net.SplitHostPort(addr)
	return fmt.Sprintf("notifications:\n  link: https://fleet.example.com/bossmode/\n  email:\n    host: %s\n    port: %s\n    from: Featherweight <fw@example.com>\n%s", host, port, more)
}

// An email channel sends a plain mail through the SMTP server of the
// operator: in the clear when the server offers no STARTTLS and the front
// needs no login, with STARTTLS and a login when the server offers it,
// and never a password in the clear. Without a server, the front does
// not offer email.
func TestEmail(t *testing.T) {
	plain := newSMTP(t, nil)
	f := newFront(t, smtpBlock(t, plain.addr, allowExample), true)
	f.duty(t, "report", nil)
	if got := f.problems(t, "alice", Body{Channels: []Channel{{Kind: KindEmail, Email: &Email{To: "Alice <alice@example.com>"}}}}); len(got) != 1 || !strings.HasPrefix(got[0], "channels[0].email.to: Write one mail address alone") {
		t.Fatalf("a bad address = %q", got)
	}
	ans := f.set(t, "alice", Body{Channels: []Channel{{Kind: KindEmail, Send: on, Email: &Email{To: "alice@example.com"}}}})
	if ans.Channels[0].ID != "email" || ans.Channels[0].Label != "Email" || ans.Channels[0].Email.To != "alice@example.com" {
		t.Fatalf("the channel = %+v", ans.Channels[0])
	}
	f.tell(t, duties.NoticeFailed, "report", "The run failed: budget exceeded.")
	f.pass(t)
	mails := plain.list()
	if len(mails) != 1 {
		t.Fatalf("the server got %d mails", len(mails))
	}
	m := mails[0]
	if m.From != "fw@example.com" || len(m.To) != 1 || m.To[0] != "alice@example.com" || m.TLS || m.Auth != "" {
		t.Fatalf("the mail = %+v", m)
	}
	// The server reads the data with its line breaks as \n.
	for _, want := range []string{"From: Featherweight <fw@example.com>\n", "To: alice@example.com\n", "Subject: Duty report failed\n", "Content-Type: text/plain; charset=utf-8\n", "Content-Transfer-Encoding: quoted-printable\n", "Auto-Submitted: auto-generated\n", "\n\nThe run failed: budget exceeded.\n\nhttps://fleet.example.com/bossmode/#duties/report"} {
		if !strings.Contains(m.Data, want) {
			t.Errorf("the mail lacks %q:\n%s", want, m.Data)
		}
	}
	// With STARTTLS, the front logs in on the private connection.
	h := newHook(t)
	pool := x509.NewCertPool()
	pool.AddCert(h.Certificate())
	secure := newSMTP(t, h.TLS.Clone())
	t.Setenv("FW_TEST_SMTP_PASSWORD", "mail-pass")
	g := newFront(t, smtpBlock(t, secure.addr, allowExample+"    user: fw\n    passwordEnv: FW_TEST_SMTP_PASSWORD\n"), true)
	g.s.smtpTLS = func(host string) *tls.Config { return &tls.Config{ServerName: host, RootCAs: pool} }
	g.set(t, "alice", Body{Channels: []Channel{{ID: "mail", Kind: KindEmail, Email: &Email{To: "alice@example.com"}}}})
	if res, err := g.s.Test(as("alice"), "mail"); err != nil || !res.Results[0].Sent {
		t.Fatalf("a test with STARTTLS = %+v, %v", res, err)
	}
	mails = secure.list()
	if len(mails) != 1 || !mails[0].TLS || !strings.Contains(mails[0].Data, "Subject: Test from featherweight") {
		t.Fatalf("the mails = %+v", mails)
	}
	auth, _ := base64.StdEncoding.DecodeString(strings.TrimPrefix(mails[0].Auth, "PLAIN "))
	if string(auth) != "\x00fw\x00mail-pass" {
		t.Fatalf("the login = %q", auth)
	}
	noSecrets(t, "the log", g.logs.String(), "mail-pass")
	// A server without STARTTLS gets no password.
	k := newFront(t, smtpBlock(t, plain.addr, allowExample+"    user: fw\n    passwordEnv: FW_TEST_SMTP_PASSWORD\n"), true)
	k.set(t, "alice", Body{Channels: []Channel{{ID: "mail", Kind: KindEmail, Email: &Email{To: "alice@example.com"}}}})
	if res, _ := k.s.Test(as("alice"), "mail"); res.Results[0].Sent || res.Results[0].Error != "The mail server offers no STARTTLS, so this server does not send the password." {
		t.Fatalf("a login in the clear = %+v", res)
	}
	if len(plain.list()) != 1 {
		t.Fatalf("the plain server got %d mails", len(plain.list()))
	}
}

// An email channel sends only to an address that
// workbench.notifications.email.allow lists, without regard to case.
// Without the list, it sends only to the address of the person, which is
// the subject when the subject is a mail address. The save refuses any
// other address at the field, on the route and in the tool. The send
// refuses it too, after an operator changed the list, and the log names
// the domain of the address alone.
func TestEmailAllow(t *testing.T) {
	plain := newSMTP(t, nil)
	f := newFront(t, smtpBlock(t, plain.addr, ""), true)
	mail := func(to string) Body {
		return Body{Channels: []Channel{{ID: "mail", Kind: KindEmail, Send: on, Email: &Email{To: to}}}}
	}
	if got := f.problems(t, "alice", mail("alice@example.com")); len(got) != 1 || got[0] != "channels[0].email.to: Without workbench.notifications.email.allow, the server sends email only to your own address, and your sign-in gives it none. An operator can list the addresses in workbench.notifications.email.allow." {
		t.Fatalf("a person whose subject is no address = %q", got)
	}
	f.set(t, "carol@example.com", mail("Carol@Example.com"))
	if got := f.problems(t, "carol@example.com", mail("bob@example.com")); len(got) != 1 || got[0] != "channels[0].email.to: Without workbench.notifications.email.allow, the server sends email only to your own address, carol@example.com." {
		t.Fatalf("another address without the list = %q", got)
	}
	resp, body := send(t, http.MethodPut, f.url+"/v1/notifications", "carol@example.com", `{"channels":[{"kind":"email","email":{"to":"bob@example.com"}}]}`)
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, `"problems":["channels[0].email.to: Without workbench.notifications.email.allow, the server sends email only to your own address, carol@example.com."]`) {
		t.Fatalf("PUT of another address = %d %s", resp.StatusCode, body)
	}
	cs := connectMCP(t, f.a, "carol@example.com")
	if res := call(t, cs, ToolPut, map[string]any{"channels": []any{map[string]any{"kind": "email", "email": map[string]any{"to": "bob@example.com"}}}}); !res.IsError ||
		!strings.Contains(textOf(res), "- channels[0].email.to: Without workbench.notifications.email.allow, the server sends email only to your own address, carol@example.com.") {
		t.Fatalf("notifications_put of another address = %s", textOf(res))
	}

	// With the list, a glob or an address of the list matches.
	listed := "    allow: [\"*@example.com\", \"Ops@Lab.Internal\"]\n"
	b := bucket.NewMem(notifyBucket)
	g := newFrontOn(t, smtpBlock(t, plain.addr, listed), true, b)
	g.set(t, "alice", Body{Channels: []Channel{
		{ID: "mail", Kind: KindEmail, Send: on, Email: &Email{To: "bob@example.com"}},
		{ID: "ops", Kind: KindEmail, Send: on, Email: &Email{To: "ops@lab.internal"}},
	}})
	if got := g.problems(t, "alice", mail("x@y.org")); len(got) != 1 || got[0] != "channels[0].email.to: The server sends email only to the addresses that workbench.notifications.email.allow lists, and it lists no match for x@y.org." {
		t.Fatalf("an address that the list does not match = %q", got)
	}

	// Another replica whose list no longer matches example.com sends no
	// mail to bob, and the log holds no address of a channel.
	h := newFrontOn(t, smtpBlock(t, plain.addr, "    allow: [\"*@lab.internal\"]\n"), true, b)
	h.duty(t, "report", nil)
	h.tell(t, duties.NoticeFailed, "report", "The run failed: budget exceeded.")
	h.pass(t)
	mails := plain.list()
	if len(mails) != 1 || mails[0].To[0] != "ops@lab.internal" {
		t.Fatalf("the server got %+v", mails)
	}
	want := "The server sends email only to the addresses that workbench.notifications.email.allow lists, and it lists no match for the address at example.com."
	if res, err := h.s.Test(as("alice"), "mail"); err != nil || res.Results[0].Sent || res.Results[0].Error != want {
		t.Fatalf("a test of a channel that the list no longer matches = %+v, %v", res, err)
	}
	if logs := h.logs.String(); !strings.Contains(logs, "lists no match for the address at example.com") || !strings.Contains(logs, "drops it") {
		t.Fatalf("the log does not say why the mail did not go:\n%s", logs)
	}
	noSecrets(t, "the log", h.logs.String(), "bob@example.com", "ops@lab.internal")
	// Without the list, a channel to another address than the subject
	// does not send either.
	k := newFrontOn(t, smtpBlock(t, plain.addr, ""), true, b)
	if res, _ := k.s.Test(as("alice"), "ops"); res.Results[0].Sent || res.Results[0].Error != "Without workbench.notifications.email.allow, the server sends email only to the address of the person, and the channel names another address at lab.internal." {
		t.Fatalf("a test without the list = %+v", res)
	}
	if len(plain.list()) != 1 {
		t.Fatalf("the server got %d mails", len(plain.list()))
	}
}

// An agent channel starts one run of the agent as the person, through
// the queue of the agent, with the budget of the channel, the words of
// how and the message as its input, and the id of the channel in its
// metadata. A channel without a budget does not save, the person needs
// the run right on the agent, and at most perDay runs go out per person
// and day.
func TestAgentChannel(t *testing.T) {
	f := newFront(t, "notifications:\n  link: https://fleet.example.com/bossmode/\n  agent:\n    enabled: true\n    perDay: 2\n", true)
	f.duty(t, "report", nil)
	ch := Channel{ID: "signal", Kind: KindAgent, Label: "Signal", Send: Send{Questions: true}, Agent: &AgentChannel{Agent: "triage", How: "Send it to me on Signal."}}
	if got := f.problems(t, "alice", Body{Channels: []Channel{ch}}); len(got) != 1 || !strings.HasPrefix(got[0], "channels[0].agent.budget: A budget is required") {
		t.Fatalf("a channel without a budget = %q", got)
	}
	ch.Agent.Budget = &duties.Budget{MaxCostUSD: 0.02}
	if got := f.problems(t, "bob", Body{Channels: []Channel{ch}}); len(got) != 1 || got[0] != "channels[0].agent.agent: You have no right to run the agent triage." {
		t.Fatalf("a viewer = %q", got)
	}
	ghost := ch
	ghost.Agent = &AgentChannel{Agent: "ghost", How: "x", Budget: ch.Agent.Budget}
	if got := f.problems(t, "alice", Body{Channels: []Channel{ghost}}); len(got) != 1 || got[0] != "channels[0].agent.agent: ghost is not an agent of this server or of a pool of the fleet." {
		t.Fatalf("an unknown agent = %q", got)
	}
	ans := f.set(t, "alice", Body{Channels: []Channel{ch}})
	if a := ans.Channels[0].Agent; a == nil || a.Agent != "triage" || a.Budget.MaxCostUSD != 0.02 {
		t.Fatalf("the channel = %+v", ans.Channels[0])
	}
	f.tellRun(t, duties.NoticeNeedsYou, "report", "01KRUN", "The agent triage asks to run git.push.")
	f.pass(t)
	reqs := f.queue.list()
	if len(reqs) != 1 {
		t.Fatalf("the queue got %d runs", len(reqs))
	}
	r := reqs[0]
	if r.Agent != "triage" || r.Principal != "alice" || r.PrincipalKind != fw.KindUser || r.Budget == nil || r.Budget.MaxCostUSD != 0.02 ||
		r.Metadata[MetaNotify] != "signal" || !strings.HasPrefix(r.IdempotencyKey, "notify/") || r.ID == "" ||
		r.Input != "Send it to me on Signal.\n\nThe message:\n\nDuty report needs you\n\nThe agent triage asks to run git.push.\n\nhttps://fleet.example.com/bossmode/#activity/01KRUN" {
		t.Fatalf("the run = %+v", r)
	}
	// A failure does not reach the channel, whose switch is off.
	f.tell(t, duties.NoticeFailed, "report", "It failed.")
	f.pass(t)
	if n := len(f.queue.list()); n != 1 {
		t.Fatalf("the queue got %d runs", n)
	}
	// The second run of the day goes, and the third drops.
	for range 2 {
		f.tellRun(t, duties.NoticeNeedsYou, "report", "01KRUN", "Again.")
		f.pass(t)
	}
	if n := len(f.queue.list()); n != 2 {
		t.Fatalf("the queue got %d runs, past the cap", n)
	}
	if keys, _ := bucket.KeysUnder(context.Background(), f.b, "out"); len(keys) != 0 || !strings.Contains(f.logs.String(), "the most (workbench.notifications.agent.perDay)") {
		t.Fatalf("the outbox holds %v; the log = %s", keys, f.logs.String())
	}
	f.clock.Add(24 * time.Hour)
	f.tellRun(t, duties.NoticeNeedsYou, "report", "01KRUN", "A new day.")
	f.pass(t)
	if n := len(f.queue.list()); n != 3 {
		t.Fatalf("the queue got %d runs on the next day", n)
	}
	// Without agent.enabled, the front does not offer the kind.
	g := newFront(t, "", false)
	if got := g.problems(t, "alice", Body{Channels: []Channel{ch}}); len(got) != 1 || !strings.HasPrefix(got[0], `channels[0].kind: This server does not offer "agent".`) {
		t.Fatalf("an agent channel on a server without agents = %q", got)
	}
}

// A shared channel of the operator takes its address and its secret from
// variables of the front. A read lists it by name and never shows its
// address. A duty reaches it as channel:<name> without a person, a person
// picks it by name, and one notice reaches it once.
func TestSharedChannels(t *testing.T) {
	h := newHook(t)
	t.Setenv("FW_TEST_OPS_URL", "https://hooks.slack.com/services/T0/B0/opssecret")
	t.Setenv("FW_TEST_HA_URL", "https://ha.example.com/hook/hasecret")
	t.Setenv("FW_TEST_HA_KEY", "ha-hmac")
	block := "notifications:\n  shared:\n    - {name: ops-room, label: Ops room, kind: slack, urlEnv: FW_TEST_OPS_URL}\n    - {name: ha, kind: webhook, urlEnv: FW_TEST_HA_URL, secretEnv: FW_TEST_HA_KEY}\n"
	f := newFront(t, block, true)
	through(f.s, h)
	resp, body := send(t, http.MethodGet, f.url+"/v1/notifications", "alice", "")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `"shared":[{"name":"ops-room","kind":"slack","label":"Ops room"},{"name":"ha","kind":"webhook","label":"ha"}]`) || !strings.Contains(body, `{"kind":"shared","label":"Shared channel"}`) {
		t.Fatalf("a read = %s", body)
	}
	noSecrets(t, "a read", body, "opssecret", "hasecret", "ha-hmac")
	// A duty names a shared channel, and the write checks the name.
	f.duty(t, "report", &duties.Notify{Failure: []string{"owner", "channel:ops-room"}, NeedsYou: []string{"owner"}})
	d := duties.Duty{Name: "other", Agent: "triage", When: duties.When{Webhook: &duties.Webhook{Auth: duties.WebhookAuth{Mode: "none"}}},
		Do: duties.Do{Ask: &duties.Ask{Message: "x"}}, Limits: duties.Limits{Budget: &duties.Budget{MaxCostUSD: 0.5}}, Notify: &duties.Notify{Failure: []string{"channel:nope"}}}
	_, err := duties.Of(f.a).Put(as("alice"), "other", duties.DutyBody{Duty: d}, duties.PutOptions{})
	if err == nil || !strings.Contains(err.Error(), "notify.failure: channel:nope is not a shared channel of this server.") {
		t.Fatalf("a duty with an unknown shared channel = %v", err)
	}
	// alice picks the same room, so the notice reaches it once.
	ans := f.set(t, "alice", Body{Channels: []Channel{{ID: "ops", Kind: KindShared, Send: on, Shared: "ops-room"}, {ID: "home", Kind: KindShared, Send: Send{Failures: true}, Shared: "ha"}}})
	if c := ans.Channels[0]; c.Label != "Ops room" || c.Shared != "ops-room" || c.URL != nil {
		t.Fatalf("the shared channel = %+v", c)
	}
	if got := f.problems(t, "alice", Body{Channels: []Channel{{Kind: KindShared, Shared: "nope"}}}); len(got) != 1 || got[0] != "channels[0].shared: nope is not a shared channel of this server." {
		t.Fatalf("an unknown shared channel = %q", got)
	}
	f.tell(t, duties.NoticeFailed, "report", "It failed.")
	f.pass(t)
	hits := h.list()
	if len(hits) != 2 {
		t.Fatalf("the hooks got %d requests: %+v", len(hits), hits)
	}
	got := byHost(h)
	if got["hooks.slack.com"].Path != "/services/T0/B0/opssecret" || !strings.HasPrefix(string(got["hooks.slack.com"].Body), `{"text":"*Duty report failed*`) {
		t.Fatalf("the room got %+v", got["hooks.slack.com"])
	}
	if ha := got["ha.example.com"]; ha.Header.Get(SignatureHeader) != Sign(ha.Body, "ha-hmac") || decodeEvent(t, ha.Body).Kind != "failed" {
		t.Fatalf("the webhook got %+v", ha)
	}
	// Without a person, the duty still reaches the room.
	f.set(t, "alice", Body{Channels: []Channel{}})
	f.tell(t, duties.NoticeFailed, "report", "It failed again.")
	f.pass(t)
	if hits := h.list(); len(hits) != 3 || hits[2].Host != "hooks.slack.com" {
		t.Fatalf("the hooks got %+v", hits)
	}
	// A variable without a value fails the send, and the error names the
	// variable and never an address.
	t.Setenv("FW_TEST_OPS_URL", "")
	f.set(t, "alice", Body{Channels: []Channel{{ID: "ops", Kind: KindShared, Shared: "ops-room"}}})
	res, err := f.s.Test(as("alice"), "ops")
	if err != nil || res.Results[0].Sent || res.Results[0].Error != "The variable FW_TEST_OPS_URL of the shared channel ops-room holds no address." {
		t.Fatalf("a test without the address = %+v, %v", res, err)
	}
	noSecrets(t, "the log", f.logs.String(), "opssecret", "hasecret", "ha-hmac")
}

// A shared channel of the kind ntfy reads its server and its topic from
// the address of the topic, and its token from the secret.
func TestSharedNtfy(t *testing.T) {
	n := newNtfy(t)
	t.Setenv("FW_TEST_NTFY_URL", n.URL+"/team-ops")
	t.Setenv("FW_TEST_NTFY_TOKEN", "tk_team")
	f := newFront(t, "notifications:\n  hosts: [\"127.0.0.1\"]\n  shared:\n    - {name: team, kind: ntfy, urlEnv: FW_TEST_NTFY_URL, secretEnv: FW_TEST_NTFY_TOKEN}\n", true)
	f.set(t, "alice", Body{Channels: []Channel{{ID: "team", Kind: KindShared, Shared: "team"}}})
	if res, err := f.s.Test(as("alice"), ""); err != nil || !res.Results[0].Sent {
		t.Fatalf("a test = %+v, %v", res, err)
	}
	got, tokens := n.published()
	if len(got) != 1 || got[0].Topic != "team-ops" || tokens[0] != "Bearer tk_team" {
		t.Fatalf("the publish = %+v %v", got, tokens)
	}
}

// The text of Slack escapes the characters that Slack reserves, and the
// content of Discord stays within its bound with the link whole.
func TestBodies(t *testing.T) {
	m := &message{Title: "Duty <a&b> failed", Text: "x > y"}
	if got := slackBody(m, "")["text"]; got != "*Duty &lt;a&amp;b&gt; failed*\nx &gt; y" {
		t.Fatalf("the text of Slack = %q", got)
	}
	long := &message{Title: "Duty report failed", Text: strings.Repeat("é", 3000)}
	content := discordBody(long, "https://fleet.example.com/bossmode/#duties/report")["content"].(string)
	if n := len([]rune(content)); n != discordMax || !strings.HasSuffix(content, "…\nhttps://fleet.example.com/bossmode/#duties/report") {
		t.Fatalf("the content of Discord has %d characters and ends %q", n, content[len(content)-60:])
	}
}
