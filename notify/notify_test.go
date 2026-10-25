package notify

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/duties"
	"github.com/bdubs00/featherweight-workbench/fleet"
	"github.com/bdubs00/featherweight-workbench/internal/bucket"
)

// ntfyBlock names the ntfy server of the front.
const ntfyBlock = "notifications:\n  ntfy:\n    server: https://ntfy.example.com\n"

// slackURL is an address of an incoming webhook of Slack.
const slackURL = "https://hooks.slack.com/services/T0/B0/slacksecret"

// problemPattern is a problem of a field: a path with indexes, a colon,
// and the text.
var problemPattern = regexp.MustCompile(`^[a-z][A-Za-z]*(\[[0-9]+\])?(\.[a-z][A-Za-z]*(\[[0-9]+\])?)*: \S`)

// problems writes the body for who and returns the problems of the write.
func (f *front) problems(t *testing.T, who string, body Body) []string {
	t.Helper()
	_, err := f.s.Put(as(who), body, "")
	var pe *ProblemsError
	if !errors.As(err, &pe) {
		t.Fatalf("Put = %v, want problems", err)
	}
	for _, p := range pe.Problems {
		if !problemPattern.MatchString(p) {
			t.Errorf("the problem %q does not start with the path of its field", p)
		}
	}
	return pe.Problems
}

// tellRun sends a notice of a duty about a run and waits until the
// notifications took it.
func (f *front) tellRun(t *testing.T, kind, duty, run, reason string) {
	t.Helper()
	if err := duties.Of(f.a).Tell(context.Background(), kind, duty, run, reason); err != nil {
		t.Fatal(err)
	}
	f.s.work.Wait()
}

// noSecrets fails the test when a text holds one of the secrets.
func noSecrets(t *testing.T, where, text string, secrets ...string) {
	t.Helper()
	for _, s := range secrets {
		if strings.Contains(text, s) {
			t.Errorf("%s holds the secret %q", where, s)
		}
	}
}

// A first read has no channel, offers the kinds of the front, and says
// why nothing reaches the person. A write keeps the write-only fields out
// of every answer, "set" keeps them, and "" clears them. A body that does
// not fit gets 400 with each problem behind the path of its field.
func TestSettingsRoutes(t *testing.T) {
	f := newFront(t, ntfyBlock, false)
	resp, body := send(t, http.MethodGet, f.url+"/v1/notifications", "alice", "")
	if resp.StatusCode != http.StatusOK || body != `{"channels":[],"quietHours":{"from":"","to":"","timeZone":"UTC"},"offered":[{"kind":"ntfy","label":"ntfy"},{"kind":"slack","label":"Slack"},{"kind":"discord","label":"Discord"},{"kind":"teams","label":"Microsoft Teams"},{"kind":"webhook","label":"Webhook"}],"shared":[],"serverDefault":"https://ntfy.example.com","off":"You have no channel. Add one, such as ntfy, Slack, or a webhook, and the front tells you of questions, failures, and the daily digest. Activity in the page always shows what needs you."}`+"\n" {
		t.Fatalf("a first read = %d %s", resp.StatusCode, body)
	}
	put := `{"channels": [
	  {"id": "phone", "kind": "ntfy", "label": "My phone", "send": {"questions": true, "failures": true}, "ntfy": {"topic": "alice-fw-8f3k", "token": "tk_secret"}},
	  {"id": "ha", "kind": "webhook", "label": "Home Assistant", "send": {"failures": true}, "url": "https://ha.example.com/api/webhook/fw-abc123", "secret": "hmac_secret"}
	], "quietHours": {"from": "22:00", "to": "07:00"}}`
	resp, body = send(t, http.MethodPut, f.url+"/v1/notifications", "alice", put, HeaderTimeZone, "America/New_York")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("a write = %d %s", resp.StatusCode, body)
	}
	noSecrets(t, "the answer of a write", body, "tk_secret", "hmac_secret", "fw-abc123")
	var got Answer
	decodeJSON(t, body, &got)
	if len(got.Channels) != 2 || got.Off != "" || got.QuietHours.TimeZone != "America/New_York" || got.QuietHours.From != "22:00" {
		t.Fatalf("the answer of a write = %+v", got)
	}
	if c := got.Channels[0]; c.Ntfy == nil || *c.Ntfy != (Ntfy{Topic: "alice-fw-8f3k", Token: SetValue}) || c.URL != nil || c.Label != "My phone" {
		t.Fatalf("the ntfy channel = %+v", c)
	}
	_, body = send(t, http.MethodGet, f.url+"/v1/notifications", "alice", "")
	noSecrets(t, "a read", body, "tk_secret", "hmac_secret", "fw-abc123")
	for _, want := range []string{
		`{"id":"phone","kind":"ntfy","label":"My phone","send":{"questions":true,"failures":true,"digest":false},"ntfy":{"server":"","topic":"alice-fw-8f3k","token":"set"}}`,
		`{"id":"ha","kind":"webhook","label":"Home Assistant","send":{"questions":false,"failures":true,"digest":false},"url":"set","secret":"set"}`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("a read = %s, want %s", body, want)
		}
	}
	// A read is a valid body, and "set" keeps every value.
	if resp, b := send(t, http.MethodPut, f.url+"/v1/notifications", "alice", body); resp.StatusCode != http.StatusOK {
		t.Fatalf("a read as a body = %d %s", resp.StatusCode, b)
	}
	rec, _ := f.s.load(context.Background(), "alice")
	if rec.channel("phone").Ntfy.Token != "tk_secret" || *rec.channel("ha").URL != "https://ha.example.com/api/webhook/fw-abc123" || *rec.channel("ha").Secret != "hmac_secret" || rec.Kind != fw.KindUser {
		t.Fatalf("the record after set = %+v", rec)
	}
	// Another value replaces a write-only field.
	f.set(t, "alice", Body{Channels: []Channel{*rec.channel("phone"), {ID: "ha", Kind: KindWebhook, Send: on, URL: new(SetValue), Secret: new("hmac_2")}}})
	if rec, _ := f.s.load(context.Background(), "alice"); *rec.channel("ha").Secret != "hmac_2" || rec.channel("phone").Ntfy.Token != "tk_secret" {
		t.Fatalf("the record after a new secret = %+v", rec)
	}
	// "" clears a write-only field.
	f.set(t, "alice", Body{Channels: []Channel{{ID: "phone", Kind: KindNtfy, Send: on, Ntfy: &Ntfy{Topic: "alice-fw-8f3k", Token: ""}}, {ID: "ha", Kind: KindWebhook, Send: on, URL: new(SetValue), Secret: new("")}}})
	rec, _ = f.s.load(context.Background(), "alice")
	if rec.channel("phone").Ntfy.Token != "" || *rec.channel("ha").Secret != "" || *rec.channel("ha").URL == "" || rec.Settings.QuietHours.From != "22:00" {
		t.Fatalf("the record after a clear = %+v", rec)
	}
	bad := `{"channels": [{"kind": "ntfy", "ntfy": {"server": "ftp://x", "topic": "no spaces", "token": "a b"}}, {"kind": "slack", "url": "https://example.com/hook"}, {"kind": "pager"}], "quietHours": {"from": "25:00", "timeZone": "Mars/Base"}}`
	resp, body = send(t, http.MethodPut, f.url+"/v1/notifications", "alice", bad)
	var doc struct {
		Problems []string `json:"problems"`
	}
	decodeJSON(t, body, &doc)
	want := []string{"channels[0].ntfy.server: ", "channels[0].ntfy.topic: ", "channels[0].ntfy.token: ", "channels[1].url: Write the address of an incoming webhook of Slack, on hooks.slack.com.", `channels[2].kind: This front does not offer "pager".`, "quietHours.timeZone: ", "quietHours.from: ", "quietHours.to: "}
	if resp.StatusCode != http.StatusBadRequest || len(doc.Problems) != len(want) {
		t.Fatalf("a bad write = %d %s", resp.StatusCode, body)
	}
	for i, prefix := range want {
		if !strings.HasPrefix(doc.Problems[i], prefix) {
			t.Errorf("problem %d = %q, want the prefix %q", i, doc.Problems[i], prefix)
		}
	}
	if resp, _ := send(t, http.MethodPut, f.url+"/v1/notifications", "alice", `{"channels": [], "other": 1}`); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("an unknown field = %d", resp.StatusCode)
	}
	// Each person has settings of their own.
	_, body = send(t, http.MethodGet, f.url+"/v1/notifications", "bob", "")
	if strings.Contains(body, "alice-fw-8f3k") {
		t.Fatalf("bob reads the settings of alice: %s", body)
	}
}

// The front gives an id to a channel without one, takes the fields of
// another kind out, keeps at most ten channels, and holds each address to
// the rule of its kind. A body without channels keeps them, and a body
// without quiet hours keeps those.
func TestWriteRules(t *testing.T) {
	f := newFront(t, "notifications:\n  hosts: [\"*.lab.internal\", \"10.0.0.7\"]\n", false)
	ans := f.set(t, "alice", Body{Channels: []Channel{
		{Kind: KindSlack, Send: Send{Failures: true}, URL: new(slackURL), Ntfy: &Ntfy{Topic: "x"}, Email: &Email{To: "a@example.com"}, Shared: "x"},
		{ID: "slack", Kind: KindDiscord, Label: "Gaming", URL: new("https://discord.com/api/webhooks/1/discordsecret")},
		{Kind: KindSlack, URL: new(slackURL)},
		{Kind: KindTeams, URL: new("https://prod-1.westus.logic.azure.com:443/workflows/w/triggers/manual/paths/invoke?sig=teamssecret")},
		{Kind: KindWebhook, URL: new("https://ha.lab.internal/hook")},
		{Kind: KindWebhook, URL: new("https://10.0.0.7/hook")},
	}, QuietHours: &QuietHours{From: "22:00", To: "07:00", TimeZone: "Europe/Berlin"}})
	var ids []string
	for _, c := range ans.Channels {
		ids = append(ids, c.ID)
	}
	if strings.Join(ids, " ") != "slack-2 slack slack-3 teams webhook webhook-2" {
		t.Fatalf("the ids = %v", ids)
	}
	if c := ans.Channels[0]; c.Label != "Slack" || c.Ntfy != nil || c.Email != nil || c.Shared != "" || c.Secret != nil || c.Send != (Send{Failures: true}) {
		t.Fatalf("the first channel = %+v", c)
	}
	if ans.Channels[1].Label != "Gaming" || ans.Channels[2].Send.any() {
		t.Fatalf("the channels = %+v", ans.Channels)
	}
	// A body without channels keeps them, and one without quiet hours
	// keeps those.
	ans = f.set(t, "alice", Body{QuietHours: &QuietHours{From: "23:00", To: "06:00"}})
	if len(ans.Channels) != 6 || ans.QuietHours.TimeZone != "UTC" {
		t.Fatalf("a write of the quiet hours = %+v", ans)
	}
	ans = f.set(t, "alice", Body{Channels: ans.Channels})
	if ans.QuietHours.From != "23:00" || len(ans.Channels) != 6 {
		t.Fatalf("a write of the channels = %+v", ans)
	}
	rec, _ := f.s.load(context.Background(), "alice")
	if *rec.channel("slack").URL != "https://discord.com/api/webhooks/1/discordsecret" {
		t.Fatalf("the stored address = %q", *rec.channel("slack").URL)
	}
	for _, c := range []struct {
		ch   Channel
		want string
	}{
		{Channel{Kind: KindDiscord, URL: new("https://discord.com/channels/1/2")}, "channels[0].url: Write the address of a webhook of a Discord channel, on discord.com under /api/webhooks/."},
		{Channel{Kind: KindDiscord, URL: new("https://discordapp.evil.com/api/webhooks/1/x")}, "channels[0].url: Write the address of a webhook of a Discord channel"},
		{Channel{Kind: KindSlack, URL: new("http://hooks.slack.com/services/x")}, "channels[0].url: Write the https address of an incoming webhook of Slack, on hooks.slack.com."},
		{Channel{Kind: KindTeams, URL: new("http://prod-1.westus.logic.azure.com/workflows/w")}, "channels[0].url: Write the https address of a webhook of a Teams workflow."},
		{Channel{Kind: KindWebhook, URL: new("https://10.0.0.5/hook")}, "channels[0].url: The host 10.0.0.5 is a private address. An operator lists it in workbench.notifications.hosts first."},
		{Channel{Kind: KindWebhook, URL: new("https://u:p@example.com/hook")}, "channels[0].url: The address holds a user or a password."},
		{Channel{Kind: KindWebhook, URL: new("https://example.com/hook"), Secret: new("a b")}, "channels[0].secret: Write the secret alone"},
		{Channel{ID: "new", Kind: KindWebhook, URL: new(SetValue)}, "channels[0].url: No address is stored for this channel. Write the address."},
		{Channel{Kind: KindWebhook}, "channels[0].url: Write the address of the webhook."},
		{Channel{ID: "a b", Kind: KindWebhook, URL: new("https://example.com/hook")}, "channels[0].id: Write 1 to 40 of"},
		{Channel{Kind: KindWebhook, Label: strings.Repeat("x", 81), URL: new("https://example.com/hook")}, "channels[0].label: Write a label of at most 80 characters"},
		{Channel{Kind: KindEmail}, `channels[0].kind: This front does not offer "email". Pick one of ntfy, slack, discord, teams, webhook.`},
		{Channel{Kind: KindAgent}, `channels[0].kind: This front does not offer "agent".`},
		{Channel{Kind: KindShared}, `channels[0].kind: This front does not offer "shared".`},
		{Channel{}, "channels[0].kind: Pick a kind: ntfy, slack, discord, teams, webhook."},
		{Channel{Kind: KindNtfy, Ntfy: &Ntfy{Topic: "alice"}}, "channels[0].ntfy.server: No ntfy server is set."},
	} {
		got := f.problems(t, "alice", Body{Channels: []Channel{c.ch}})
		if len(got) != 1 || !strings.HasPrefix(got[0], c.want) {
			t.Errorf("problems of %+v = %q, want %q", c.ch, got, c.want)
		}
	}
	// A webhook on a host that the operator lists can use http, and one on
	// another host cannot.
	if _, err := f.s.Put(as("bob"), Body{Channels: []Channel{{Kind: KindWebhook, URL: new("http://sink.lab.internal:8080/hook")}}}, ""); err != nil {
		t.Errorf("an http webhook on a listed host = %v", err)
	}
	if got := f.problems(t, "alice", Body{Channels: []Channel{{Kind: KindWebhook, URL: new("http://example.com/hook")}}}); len(got) != 1 || got[0] != "channels[0].url: Write an https address." {
		t.Errorf("an http webhook on a public host = %q", got)
	}
	two := []Channel{{ID: "x", Kind: KindSlack, URL: new(slackURL)}, {ID: "x", Kind: KindSlack, URL: new(slackURL)}}
	if got := f.problems(t, "alice", Body{Channels: two}); len(got) != 1 || got[0] != "channels[1].id: The id x names two channels." {
		t.Errorf("two channels of one id = %q", got)
	}
	var eleven []Channel
	for range 11 {
		eleven = append(eleven, Channel{Kind: KindSlack, URL: new(slackURL)})
	}
	if got := f.problems(t, "alice", Body{Channels: eleven}); len(got) != 1 || got[0] != "channels: The list holds 11 channels, and the most is 10." {
		t.Errorf("eleven channels = %q", got)
	}
}

// A record of the first shape, with ntfy and send beside the quiet hours,
// reads as a person with no channel and the same quiet hours, so nothing
// goes out until the person adds a channel. A notification of the first
// shape in the outbox names no channel, and the front drops it.
func TestOldRecord(t *testing.T) {
	f := newFront(t, ntfyBlock, false)
	ctx := context.Background()
	old := `{"subject":"alice","settings":{"ntfy":{"server":"","topic":"alice","token":"tk_old"},"send":{"questions":true,"failures":true,"digest":true},"quietHours":{"from":"22:00","to":"07:00","timeZone":"UTC"}},"changed":"2026-10-22T10:00:00Z"}`
	if _, err := f.b.Create(ctx, personKey("alice"), []byte(old)); err != nil {
		t.Fatal(err)
	}
	ans, err := f.s.Get(as("alice"))
	if err != nil || len(ans.Channels) != 0 || !strings.HasPrefix(ans.Off, "You have no channel.") || ans.QuietHours.From != "22:00" {
		t.Fatalf("a read of an old record = %+v, %v", ans, err)
	}
	f.duty(t, "report", nil)
	f.tell(t, duties.NoticeFailed, "report", "It failed.")
	if _, err := f.b.Create(ctx, outKey("OLD"), []byte(`{"id":"OLD","to":"alice","kind":"failed","title":"x","text":"y","created":"`+t0.Format(time.RFC3339)+`"}`)); err != nil {
		t.Fatal(err)
	}
	f.pass(t)
	if keys, _ := bucket.KeysUnder(ctx, f.b, "out"); len(keys) != 0 || len(f.out.list()) != 0 {
		t.Fatalf("the outbox holds %v, and %d went out", keys, len(f.out.list()))
	}
	ans = f.set(t, "alice", Body{Channels: []Channel{ntfyChannel("phone", "alice", on)}})
	if ans.QuietHours.From != "22:00" || ans.Off != "" {
		t.Fatalf("a write over an old record = %+v", ans)
	}
}

// A notice of a duty puts one notification in the outbox for each channel
// of each person of its list whose switches send its kind, with a title
// that names the duty. A person who turned a kind off on a channel gets
// nothing there.
func TestNoticesReachChannels(t *testing.T) {
	f := newFront(t, ntfyBlock, false)
	f.duty(t, "report", &duties.Notify{Failure: []string{"owner", "bob"}, NeedsYou: []string{"owner"}})
	f.set(t, "alice", Body{Channels: []Channel{
		{ID: "phone", Kind: KindNtfy, Send: Send{Questions: true}, Ntfy: &Ntfy{Topic: "alice", Token: "tk"}},
		{ID: "room", Kind: KindSlack, Send: Send{Failures: true}, URL: new(slackURL)},
	}})
	f.set(t, "bob", Body{Channels: []Channel{ntfyChannel("phone", "bob", Send{Failures: true}), ntfyChannel("quiet", "bob-2", Send{})}})
	f.tell(t, duties.NoticeNeedsYou, "report", "The agent triage asks to run git.push.")
	f.tell(t, duties.NoticeFailed, "report", "The run failed: budget exceeded.")
	keys, _ := bucket.KeysUnder(context.Background(), f.b, "out")
	if len(keys) != 3 {
		t.Fatalf("the outbox holds %v", keys)
	}
	f.pass(t)
	got := map[string]sent{}
	for _, s := range f.out.list() {
		got[s.To+"/"+s.Channel] = s
	}
	if len(got) != 3 {
		t.Fatalf("sent = %+v", f.out.list())
	}
	if a := got["alice/phone"]; a.Title != "Duty report needs you" || a.Text != "The agent triage asks to run git.push." || a.Ntfy != (Ntfy{Server: "https://ntfy.example.com", Topic: "alice", Token: "tk"}) {
		t.Fatalf("alice/phone got %+v", a)
	}
	if a := got["alice/room"]; a.Title != "Duty report failed" || a.Kind != KindSlack || a.URL != slackURL || a.Event != duties.NoticeFailed {
		t.Fatalf("alice/room got %+v", a)
	}
	if b := got["bob/phone"]; b.Title != "Duty report failed" || b.Ntfy.Topic != "bob" {
		t.Fatalf("bob/phone got %+v", b)
	}
	if keys, _ := bucket.KeysUnder(context.Background(), f.b, "out"); len(keys) != 0 {
		t.Fatalf("the outbox holds %v", keys)
	}
}

// A notification during the quiet hours waits in the bucket until they
// end, on every channel of the person. Another replica on the same bucket
// sends it once, after a restart of the first one.
func TestQuietHoursHold(t *testing.T) {
	b := bucket.NewMem(notifyBucket)
	f := newFrontOn(t, ntfyBlock, false, b)
	f.duty(t, "report", nil)
	f.set(t, "alice", Body{Channels: []Channel{ntfyChannel("phone", "alice", on), {ID: "room", Kind: KindSlack, Send: on, URL: new(slackURL)}}, QuietHours: &QuietHours{From: "22:00", To: "07:00", TimeZone: "America/New_York"}})
	night := time.Date(2026, 10, 24, 3, 0, 0, 0, time.UTC) // 23:00 in New York
	f.clock.Set(night)
	f.tell(t, duties.NoticeFailed, "report", "The run failed.")
	f.pass(t)
	if got := f.out.list(); len(got) != 0 {
		t.Fatalf("a notification went out in the quiet hours: %+v", got)
	}
	f.s.Close()
	g := newFrontOn(t, ntfyBlock, false, b)
	g.clock.Set(night.Add(7*time.Hour + 59*time.Minute))
	g.pass(t)
	if got := g.out.list(); len(got) != 0 {
		t.Fatalf("a notification went out before the end of the quiet hours: %+v", got)
	}
	g.clock.Set(time.Date(2026, 10, 24, 11, 0, 0, 0, time.UTC)) // 07:00 in New York
	g.pass(t)
	g.pass(t)
	if got := g.out.list(); len(got) != 2 || got[0].Title != "Duty report failed" || got[0].Channel == got[1].Channel {
		t.Fatalf("after the quiet hours = %+v", got)
	}
}

// Of two replicas on one bucket, only one sends a notification.
func TestOneReplicaSends(t *testing.T) {
	b := bucket.NewMem(notifyBucket)
	f := newFrontOn(t, ntfyBlock, false, b)
	g := newFrontOn(t, ntfyBlock, false, b)
	f.duty(t, "report", nil)
	f.set(t, "alice", Body{Channels: []Channel{ntfyChannel("phone", "alice", on)}})
	f.tell(t, duties.NoticePaused, "report", "3 runs failed in a row.")
	keys, _ := bucket.KeysUnder(context.Background(), b, "out")
	if len(keys) != 1 {
		t.Fatalf("the outbox holds %v", keys)
	}
	// The other replica claims it first and stops before it sends.
	e, _ := b.Get(context.Background(), keys[0])
	claimed := strings.Replace(string(e.Value), `"created"`, `"claim":"front-x","claimAt":"`+t0.Format(time.RFC3339Nano)+`","created"`, 1)
	if _, err := b.Update(context.Background(), keys[0], []byte(claimed), e.Revision); err != nil {
		t.Fatal(err)
	}
	f.pass(t)
	g.pass(t)
	if n := len(f.out.list()) + len(g.out.list()); n != 0 {
		t.Fatalf("a claimed notification went out %d times", n)
	}
	f.clock.Add(claimFor + time.Second)
	g.clock.Add(claimFor + time.Second)
	f.pass(t)
	g.pass(t)
	if n := len(f.out.list()) + len(g.out.list()); n != 1 {
		t.Fatalf("the notification went out %d times", n)
	}
}

// failing makes the sends of a front to one channel fail.
func (f *front) failing(id string) {
	inner := f.s.sendFn
	f.s.sendFn = func(ctx context.Context, t target, m *message) error {
		if t.id == id {
			return errors.New("the host hooks.slack.com answered 500: Internal Server Error")
		}
		return inner(ctx, t, m)
	}
}

// Each notification of a channel has its own tries: a channel that fails
// goes again and the front drops it after maxAttempts, while the other
// channel of the same notice goes at once.
func TestRetryThenDrop(t *testing.T) {
	f := newFront(t, ntfyBlock, false)
	f.duty(t, "report", nil)
	f.set(t, "alice", Body{Channels: []Channel{ntfyChannel("phone", "alice", on), {ID: "room", Kind: KindSlack, Send: on, URL: new(slackURL)}}})
	f.failing("room")
	f.tell(t, duties.NoticeCap, "report", "The duty spent $2.00 today.")
	f.pass(t)
	if got := f.out.list(); len(got) != 1 || got[0].Channel != "phone" || got[0].Title != "Duty report reached a daily cap" {
		t.Fatalf("the first pass = %+v", got)
	}
	for range maxAttempts {
		f.clock.Add(time.Hour)
		f.pass(t)
	}
	if keys, _ := bucket.KeysUnder(context.Background(), f.b, "out"); len(keys) != 0 {
		t.Fatalf("the outbox holds %v after %d attempts", keys, maxAttempts)
	}
	if !strings.Contains(f.logs.String(), "the front drops it") || !strings.Contains(f.logs.String(), "attempts=5") {
		t.Fatalf("the log = %s", f.logs.String())
	}
	// A retry waits for its time.
	f.s.sendFn = f.out.send
	f.out.setFail(errors.New("down"))
	f.tell(t, duties.NoticeCap, "report", "Again.")
	f.pass(t)
	f.out.setFail(nil)
	f.pass(t)
	if got := f.out.list(); len(got) != 1 {
		t.Fatalf("a retry went out before its time: %+v", got)
	}
	f.clock.Add(time.Minute)
	f.pass(t)
	if got := f.out.list(); len(got) != 3 {
		t.Fatalf("the retries = %+v", got)
	}
}

// The quiet hours hold from from to to, also across midnight.
func TestQuietUntil(t *testing.T) {
	ny, _ := time.LoadLocation("America/New_York")
	q := &QuietHours{From: "22:00", To: "07:00", TimeZone: "America/New_York"}
	for _, c := range []struct {
		at    time.Time
		quiet bool
		until time.Time
	}{
		{time.Date(2026, 10, 23, 21, 59, 0, 0, ny), false, time.Time{}},
		{time.Date(2026, 10, 23, 22, 0, 0, 0, ny), true, time.Date(2026, 10, 24, 7, 0, 0, 0, ny)},
		{time.Date(2026, 10, 24, 6, 59, 0, 0, ny), true, time.Date(2026, 10, 24, 7, 0, 0, 0, ny)},
		{time.Date(2026, 10, 24, 7, 0, 0, 0, ny), false, time.Time{}},
	} {
		until, quiet := quietUntil(q, c.at)
		if quiet != c.quiet || !until.Equal(c.until) {
			t.Errorf("quietUntil(%v) = %v, %v; want %v, %v", c.at, until, quiet, c.until, c.quiet)
		}
	}
	day := &QuietHours{From: "12:00", To: "13:00", TimeZone: "UTC"}
	if until, quiet := quietUntil(day, time.Date(2026, 10, 23, 12, 30, 0, 0, time.UTC)); !quiet || until.Hour() != 13 {
		t.Errorf("quiet hours in a day = %v, %v", until, quiet)
	}
	if _, quiet := quietUntil(&QuietHours{TimeZone: "UTC"}, t0); quiet {
		t.Error("settings without quiet hours are quiet")
	}
}

// The digest goes once a day at the end of the quiet hours, from one
// replica, to each channel whose switch sends it, with each start of the
// last day, its outcome, and its cost.
func TestDigest(t *testing.T) {
	b := bucket.NewMem(notifyBucket)
	f := newFrontOn(t, ntfyBlock, false, b)
	g := newFrontOn(t, ntfyBlock, false, b)
	now := time.Now().UTC()
	f.duty(t, "report", nil)
	if out, err := duties.Of(f.a).RunNow(as("alice"), "report"); err != nil || out.Run == "" {
		t.Fatalf("RunNow = %+v, %v", out, err)
	}
	f.set(t, "alice", Body{Channels: []Channel{ntfyChannel("phone", "alice", Send{Questions: true}), {ID: "room", Kind: KindSlack, Send: Send{Digest: true}, URL: new(slackURL)}}, QuietHours: &QuietHours{From: "22:00", To: "07:00", TimeZone: "UTC"}})
	f.set(t, "bob", Body{Channels: []Channel{ntfyChannel("phone", "bob", Send{Digest: true})}})
	seven := time.Date(now.Year(), now.Month(), now.Day(), 7, 0, 30, 0, time.UTC)
	for _, x := range []*front{f, g} {
		x.clock.Set(seven.Add(-time.Minute))
		x.pass(t)
	}
	if n := len(f.out.list()) + len(g.out.list()); n != 0 {
		t.Fatalf("%d digests went before their time", n)
	}
	for _, x := range []*front{f, g} {
		x.clock.Set(seven)
		x.s.lastDigest = time.Time{}
		x.pass(t)
		x.pass(t)
	}
	got := append(f.out.list(), g.out.list()...)
	if len(got) != 1 || got[0].To != "alice" || got[0].Channel != "room" || got[0].Event != EventDigest || got[0].Title != "Your duties: the last day" ||
		!strings.Contains(got[0].Text, "1 run of your duties in the last day, $0.00 in all:\n- report, ") || !strings.Contains(got[0].Text, ": queued, $0.00") {
		t.Fatalf("digests = %+v", got)
	}
	// The same day sends no second digest, and bob, who has no duty and
	// whose digest goes at 08:00, gets none.
	for _, x := range []*front{f, g} {
		x.clock.Set(seven.Add(2 * time.Hour))
		x.s.lastDigest = time.Time{}
		x.pass(t)
	}
	if n := len(f.out.list()) + len(g.out.list()); n != 1 {
		t.Fatalf("%d digests in one day", n)
	}
}

// The digest lists under each start the decisions about the approvals of
// its run while its owner was away, in the words of the page, at most
// three of them and a count of the rest. Many starts with decisions keep
// the digest within maxDigestLines lines and maxText bytes.
func TestDigestDecisions(t *testing.T) {
	f := newFront(t, ntfyBlock, false)
	svc := duties.Of(f.a)
	f.duty(t, "report", nil)
	out, err := svc.RunNow(as("alice"), "report")
	if err != nil || out.Run == "" {
		t.Fatalf("RunNow = %+v, %v", out, err)
	}
	for _, d := range []duties.Decided{
		{By: duties.DecidedByManager, Verdict: "allow", Tool: "bash", Reason: "it only reads"},
		{By: duties.DecidedByTimeout, Verdict: "deny", Tool: "git.push", Reason: "Nobody decided within 2h, so the front denied it, as whileAway of the duty report says."},
		{By: "alice", Verdict: "allow", Tool: "files"},
		{By: "bob@example.com", Verdict: "cancel", Tool: "deploy", Reason: "wrong host"},
		{By: duties.DecidedByManager, Verdict: "deny", Tool: "http"},
	} {
		if err := svc.RecordDecision(context.Background(), "report", out.Run, d); err != nil {
			t.Fatal(err)
		}
	}
	text, ok := f.s.digestText(context.Background(), "alice", time.UTC, time.Now())
	want := ": queued, $0.00\n" +
		"  - The manager allowed bash: it only reads.\n" +
		"  - Nobody answered in time, so the front denied git.push.\n" +
		"  - You allowed files.\n" +
		"  - and 2 more decisions"
	if !ok || !strings.HasPrefix(text, "1 run of your duties in the last day, $0.00 in all:\n- report, ") || !strings.HasSuffix(text, want) {
		t.Fatalf("the digest = %q", text)
	}

	// Many starts with many decisions stay within the bounds.
	d := duties.Duty{
		Name: "busy", Agent: "triage",
		When:   duties.When{Webhook: &duties.Webhook{Auth: duties.WebhookAuth{Mode: "none"}}},
		Do:     duties.Do{Ask: &duties.Ask{Message: "Work."}},
		Limits: duties.Limits{Budget: &duties.Budget{MaxCostUSD: 0.5}, Overlap: duties.OverlapAllow},
	}
	if _, err := svc.Put(as("alice"), "busy", duties.DutyBody{Duty: d}, duties.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	long := strings.Repeat("The call reads the files of the report and writes nothing else. ", 6)
	for range 20 {
		out, err := svc.RunNow(as("alice"), "busy")
		if err != nil || out.Run == "" {
			t.Fatalf("RunNow = %+v, %v", out, err)
		}
		for range 5 {
			if err := svc.RecordDecision(context.Background(), "busy", out.Run, duties.Decided{By: duties.DecidedByManager, Verdict: "allow", Tool: "bash", Reason: long}); err != nil {
				t.Fatal(err)
			}
		}
	}
	text, _ = f.s.digestText(context.Background(), "alice", time.UTC, time.Now())
	lines := strings.Split(text, "\n")
	listed := 0
	for _, l := range lines {
		if strings.HasPrefix(l, "- ") || strings.HasPrefix(l, "  - ") {
			listed++
		}
	}
	if listed > maxDigestLines+1 || len(text) > maxText || !strings.Contains(text, "\n- and ") || !strings.Contains(text, " more decisions") {
		t.Fatalf("a digest of %d lines and %d bytes:\n%s", listed, len(text), text)
	}
	if !strings.Contains(text, "  - The manager allowed bash: The call reads the files of the report and writes nothing else. The call reads") || !strings.Contains(text, "…\n") {
		t.Fatalf("a long reason does not end at a word:\n%s", text)
	}
}

// The words of a decision follow the page: who, the verdict, the tool,
// and the reason, which a timeout leaves out.
func TestDecidedWords(t *testing.T) {
	for _, x := range []struct {
		d    duties.Decided
		want string
	}{
		{duties.Decided{By: "manager", Verdict: "allow", Tool: "bash", Reason: "it only reads"}, "The manager allowed bash: it only reads."},
		{duties.Decided{By: "manager", Verdict: "deny", Tool: "shell.exec"}, "The manager denied shell.exec."},
		{duties.Decided{By: "timeout", Verdict: "deny", Tool: "git.push", Reason: "Nobody decided within 2h."}, "Nobody answered in time, so the front denied git.push."},
		{duties.Decided{By: "timeout", Verdict: "cancel", Tool: "git.push"}, "Nobody answered in time, so the front stopped the run at git.push."},
		{duties.Decided{By: "bob@example.com", Verdict: "cancel", Tool: "bash", Reason: "wrong host!"}, "Bob stopped the run at bash: wrong host!"},
		{duties.Decided{By: "alice@example.com", Verdict: "allow", Tool: "bash"}, "You allowed bash."},
		{duties.Decided{By: "carol", Verdict: "deny"}, "Carol denied a tool."},
		{duties.Decided{Verdict: "allow", Tool: "bash"}, "Someone allowed bash."},
	} {
		if got := decidedWords(x.d, "alice@example.com"); got != x.want {
			t.Errorf("decidedWords(%+v) = %q, want %q", x.d, got, x.want)
		}
	}
}

// A call without a person has no settings.
func TestNobody(t *testing.T) {
	f := newFront(t, "", false)
	if _, err := f.s.Get(context.Background()); !errors.Is(err, ErrNobody) || !errors.Is(err, fw.ErrForbidden) {
		t.Fatalf("Get without a person = %v", err)
	}
	if _, err := f.s.Test(context.Background(), ""); !errors.Is(err, ErrNobody) {
		t.Fatalf("Test without a person = %v", err)
	}
}

// The link of a notification goes to the run in Activity, else to the
// duty, else to the page, and it is empty without workbench.notifications.link.
func TestLink(t *testing.T) {
	c := Config{Link: "https://fleet.example.com/bossmode/"}
	for _, x := range []struct {
		m    message
		want string
	}{
		{message{Run: "01KRUN", Duty: "report"}, "https://fleet.example.com/bossmode/#activity/01KRUN"},
		{message{Duty: "report"}, "https://fleet.example.com/bossmode/#duties/report"},
		{message{}, "https://fleet.example.com/bossmode/"},
	} {
		if got := c.linkOf(&x.m); got != x.want {
			t.Errorf("linkOf(%+v) = %q, want %q", x.m, got, x.want)
		}
	}
	if got := (Config{}).linkOf(&message{Run: "01KRUN"}); got != "" {
		t.Errorf("a link without the key = %q", got)
	}
}

// A configuration that does not fit stops the start, and the error names
// the key.
func TestConfig(t *testing.T) {
	for block, want := range map[string]string{
		"notifications:\n  ntfy:\n    server: ntfy.example.com\n":                                                           "workbench.notifications.ntfy.server",
		"notifications:\n  ntfy:\n    server: https://u:p@ntfy.example.com\n":                                               "workbench.notifications.ntfy.server",
		"notifications:\n  ntfy:\n    hosts: [\"[\"]\n":                                                                     "workbench.notifications.ntfy.hosts[0]",
		"notifications:\n  hosts: [\"\"]\n":                                                                                 "workbench.notifications.hosts[0]",
		"notifications:\n  link: fleet.example.com\n":                                                                       "workbench.notifications.link",
		"notifications:\n  link: https://fleet.example.com/#x\n":                                                            "has a fragment",
		"notifications:\n  email:\n    host: smtp.example.com\n":                                                            "workbench.notifications.email.from",
		"notifications:\n  email:\n    host: smtp.example.com:587\n    from: fw@example.com\n":                              "workbench.notifications.email.host",
		"notifications:\n  email:\n    from: fw@example.com\n":                                                              "workbench.notifications.email.host is empty",
		"notifications:\n  email:\n    host: smtp.example.com\n    from: fw@example.com\n    user: fw\n":                    "workbench.notifications.email.passwordEnv",
		"notifications:\n  agent:\n    perDay: -1\n":                                                                        "workbench.notifications.agent.perDay",
		"notifications:\n  shared:\n    - {name: Ops, kind: slack, urlEnv: X}\n":                                            "workbench.notifications.shared[0].name",
		"notifications:\n  shared:\n    - {name: ops, kind: email, urlEnv: X}\n":                                            "workbench.notifications.shared[0].kind",
		"notifications:\n  shared:\n    - {name: ops, kind: slack}\n":                                                       "workbench.notifications.shared[0].urlEnv",
		"notifications:\n  shared:\n    - {name: ops, kind: slack, urlEnv: X}\n    - {name: ops, kind: slack, urlEnv: Y}\n": "names two shared channels",
	} {
		cfg := fw.DefaultConfig()
		cfg.Extensions = workbench(t, block)
		if _, err := LoadConfig(cfg); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("LoadConfig(%q) = %v, want %q", block, err, want)
		}
	}
	cfg := fw.DefaultConfig()
	cfg.Extensions = workbench(t, "notifications:\n  email:\n    host: smtp.example.com\n    from: fw@example.com\n  agent:\n    enabled: true\n  shared:\n    - {name: ops, kind: slack, urlEnv: OPS}\n")
	c, err := LoadConfig(cfg)
	if err != nil || c.perDay() != DefaultAgentPerDay || c.smtpPort() != DefaultSMTPPort || len(c.offered()) != 8 || c.Shared[0].Label != "ops" {
		t.Fatalf("LoadConfig = %+v, %v", c, err)
	}
}

// A glob of workbench.notifications.email.allow matches an address
// without regard to case, and "*" matches any text, also none. A glob
// with a space does not load, and the list needs an SMTP server. Without
// the list, only the subject of the person matches, when it is an
// address.
func TestMailAllowed(t *testing.T) {
	c := Config{Email: EmailConfig{Allow: []string{"*@example.com", "ops@lab.internal", "team-*-alerts@*.example.org"}}}
	for to, want := range map[string]bool{
		"bob@example.com": true, "Bob@EXAMPLE.com": true, "@example.com": true, "bob@example.com.evil": false, "bob@sub.example.com": false,
		"ops@lab.internal": true, "OPS@lab.internal": true, "dev@lab.internal": false,
		"team-db-alerts@eu.example.org": true, "team--alerts@x.example.org": true, "team-db@eu.example.org": false, "": false,
	} {
		if got := c.mailAllowed(to, "alice"); got != want {
			t.Errorf("mailAllowed(%q) = %v, want %v", to, got, want)
		}
	}
	if !(Config{Email: EmailConfig{Allow: []string{"*"}}}).mailAllowed("anyone@anywhere.net", "") {
		t.Error("* does not match an address")
	}
	for block, want := range map[string]string{
		"notifications:\n  email:\n    host: smtp.example.com\n    from: fw@example.com\n    allow: [\"a b@example.com\"]\n": "workbench.notifications.email.allow[0]",
		"notifications:\n  email:\n    allow: [\"*@example.com\"]\n":                                                         "workbench.notifications.email.host is empty",
	} {
		cfg := fw.DefaultConfig()
		cfg.Extensions = workbench(t, block)
		if _, err := LoadConfig(cfg); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("LoadConfig(%q) = %v, want %q", block, err, want)
		}
	}
	cfg := fw.DefaultConfig()
	cfg.Extensions = workbench(t, "notifications:\n  email:\n    host: smtp.example.com\n    from: fw@example.com\n    allow: [\" *@Example.com \"]\n")
	if c, err := LoadConfig(cfg); err != nil || len(c.Email.Allow) != 1 || c.Email.Allow[0] != "*@example.com" {
		t.Fatalf("LoadConfig = %+v, %v", c.Email, err)
	}
	var none Config
	for _, x := range []struct {
		to, subject string
		want        bool
	}{
		{"alice@example.com", "alice@example.com", true},
		{"Alice@Example.com", "alice@example.com", true},
		{"alice@example.com", "Alice@Example.com", true},
		{"bob@example.com", "alice@example.com", false},
		{"alice@example.com", "alice", false},
		{"alice@example.com", "Alice <alice@example.com>", false},
	} {
		if got := none.mailAllowed(x.to, x.subject); got != x.want {
			t.Errorf("without the list, mailAllowed(%q, %q) = %v, want %v", x.to, x.subject, got, x.want)
		}
	}
}

// The doctor line names where the settings live, the kinds, the ntfy
// server of the front, the hosts, the link, and the shared channels. A
// configuration that does not fit, and a variable that holds no value,
// fail it.
func TestDoctorCheck(t *testing.T) {
	cfg := fw.DefaultConfig()
	cfg.Extensions = workbench(t, "notifications:\n  ntfy:\n    server: https://ntfy.lab.internal\n  shared:\n    - {name: ops, kind: slack, urlEnv: FW_TEST_OPS_URL}\n")
	t.Setenv("FW_TEST_OPS_URL", "https://hooks.slack.com/services/x")
	ok, line := DoctorCheck(context.Background(), cfg, nil, 0)
	if !ok || !strings.Contains(line, "the ntfy server of the front is ntfy.lab.internal") || !strings.Contains(line, "a channel reaches public hosts only") ||
		!strings.Contains(line, "a person picks ntfy, slack, discord, teams, webhook, shared") || !strings.Contains(line, "the shared channel ops (slack)") || strings.Contains(line, "services/x") {
		t.Fatalf("doctor = %v, %q", ok, line)
	}
	t.Setenv("FW_TEST_OPS_URL", "")
	if ok, line := DoctorCheck(context.Background(), cfg, nil, 0); ok || !strings.Contains(line, "FW_TEST_OPS_URL holds no address") {
		t.Fatalf("doctor without the address = %v, %q", ok, line)
	}
	cfg.Extensions = workbench(t, "notifications:\n  ntfy:\n    server: nope\n")
	if ok, _ := DoctorCheck(context.Background(), cfg, nil, 0); ok {
		t.Fatal("a server that does not read passed the doctor")
	}
}

// decodeEvent reads the body of a webhook.
func decodeEvent(t *testing.T, body []byte) Event {
	t.Helper()
	var e Event
	dec := json.NewDecoder(strings.NewReader(string(body)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&e); err != nil {
		t.Fatalf("the event %s: %v", body, err)
	}
	return e
}

// A replica without an intake serves the settings and runs no loop, so a
// front on the same bus with another configuration never takes a
// notification that it cannot send.
func TestStartWithoutIntake(t *testing.T) {
	t.Setenv(fleet.EnvIntake, "")
	cfg := fw.DefaultConfig()
	cfg.Home = t.TempDir()
	a, err := fw.New(fw.WithConfig(cfg), fw.WithoutDefaults(), fw.WithAuthn(fw.NoAuthn{}),
		fw.WithLedger(fw.NewMemLedger()), fw.WithBus(fw.NewMemBus()), fw.WithLogger(slog.New(slog.DiscardHandler)))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close(context.Background())
	c, err := Start(context.Background(), a)
	if err != nil || c == nil || Of(a) == nil {
		t.Fatalf("Start = %v, %v", c, err)
	}
	if s := Of(a); s.cancel != nil {
		t.Fatal("a replica without an intake runs the loop of the outbox")
	}
	if err := c.Close(); err != nil || Of(a) != nil {
		t.Fatalf("Close = %v, %v", err, Of(a))
	}
	t.Setenv(fleet.EnvIntake, "fw.jobs")
	c, err = Start(context.Background(), a)
	if err != nil || c == nil || Of(a).cancel == nil {
		t.Fatalf("Start with an intake = %v, %v", c, err)
	}
	_ = c.Close()
}
