package notify

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/duties"
	"github.com/bdubs00/featherweight-workbench/internal/bucket"
)

// A read without a server says why the notifications are off, and the
// front names no public server. A write keeps the token, a read never
// shows it, "set" keeps it, and "" clears it. A body that does not fit
// gets 400 with each problem behind the path of its field.
func TestSettingsRoutes(t *testing.T) {
	f := newFront(t, "", false)
	resp, body := send(t, http.MethodGet, f.url+"/v1/notifications", "alice", "")
	var ans map[string]any
	decodeJSON(t, body, &ans)
	if resp.StatusCode != http.StatusOK || ans["serverDefault"] != "" || !strings.Contains(ans["off"].(string), "No ntfy server is set") {
		t.Fatalf("a first read = %d %s", resp.StatusCode, body)
	}
	if send := ans["send"].(map[string]any); send["questions"] != true || send["failures"] != true || send["digest"] != false {
		t.Fatalf("the default send = %v", send)
	}
	put := `{"ntfy": {"server": "https://ntfy.example.com/", "topic": "alice-fw-8f3k", "token": "tk_secret"}, "send": {"digest": true}, "quietHours": {"from": "22:00", "to": "07:00"}}`
	resp, body = send(t, http.MethodPut, f.url+"/v1/notifications", "alice", put, HeaderTimeZone, "America/New_York")
	if resp.StatusCode != http.StatusOK || strings.Contains(body, "tk_secret") {
		t.Fatalf("a write = %d %s", resp.StatusCode, body)
	}
	var got Answer
	decodeJSON(t, body, &got)
	if got.Ntfy.Server != "https://ntfy.example.com" || got.Ntfy.Token != TokenSet || got.Off != "" || !got.Send.Questions || !got.Send.Digest ||
		got.QuietHours == nil || got.QuietHours.TimeZone != "America/New_York" || got.QuietHours.From != "22:00" {
		t.Fatalf("the answer of a write = %+v", got)
	}
	_, body = send(t, http.MethodGet, f.url+"/v1/notifications", "alice", "")
	if strings.Contains(body, "tk_secret") || !strings.Contains(body, `"token":"set"`) {
		t.Fatalf("a read = %s", body)
	}
	// A read is a valid body, and "set" keeps the token.
	resp, _ = send(t, http.MethodPut, f.url+"/v1/notifications", "alice", body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("a read as a body = %d", resp.StatusCode)
	}
	if st, _ := f.s.settingsOf(context.Background(), "alice"); st.Ntfy.Token != "tk_secret" {
		t.Fatalf("the token after set = %q", st.Ntfy.Token)
	}
	f.set(t, "alice", Body{Ntfy: Ntfy{Topic: "alice-fw-8f3k", Token: ""}})
	if st, _ := f.s.settingsOf(context.Background(), "alice"); st.Ntfy.Token != "" || st.QuietHours.TimeZone != "UTC" {
		t.Fatalf("the settings after a clear = %+v", st)
	}
	bad := `{"ntfy": {"server": "ftp://x", "topic": "no spaces", "token": "a b"}, "quietHours": {"from": "25:00", "timeZone": "Mars/Base"}}`
	resp, body = send(t, http.MethodPut, f.url+"/v1/notifications", "alice", bad)
	var doc struct {
		Problems []string `json:"problems"`
	}
	decodeJSON(t, body, &doc)
	if resp.StatusCode != http.StatusBadRequest || len(doc.Problems) != 6 {
		t.Fatalf("a bad write = %d %s", resp.StatusCode, body)
	}
	for i, prefix := range []string{"ntfy.server: ", "ntfy.topic: ", "ntfy.token: ", "quietHours.timeZone: ", "quietHours.from: ", "quietHours.to: "} {
		if !strings.HasPrefix(doc.Problems[i], prefix) {
			t.Errorf("problem %d = %q, want the prefix %q", i, doc.Problems[i], prefix)
		}
	}
	if resp, _ := send(t, http.MethodPut, f.url+"/v1/notifications", "alice", `{"ntfy": {}, "other": 1}`); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("an unknown field = %d", resp.StatusCode)
	}
	// Each person has settings of their own.
	_, body = send(t, http.MethodGet, f.url+"/v1/notifications", "bob", "")
	if strings.Contains(body, "alice-fw-8f3k") {
		t.Fatalf("bob reads the settings of alice: %s", body)
	}
}

// The server of the configuration is the default of a person, a read
// names it, and the list of hosts bounds the server of a person.
func TestServerOfTheConfiguration(t *testing.T) {
	f := newFront(t, "notifications:\n  ntfy:\n    server: https://ntfy.lab.internal/\n    hosts: [\"*.example.com\"]\n", false)
	ans := f.set(t, "alice", Body{Ntfy: Ntfy{Topic: "alice"}})
	if ans.ServerDefault != "https://ntfy.lab.internal" || ans.Off != "" {
		t.Fatalf("the answer = %+v", ans)
	}
	if _, err := f.s.Put(as("alice"), Body{Ntfy: Ntfy{Server: "https://ntfy.sh", Topic: "alice"}}, ""); err == nil || !strings.Contains(err.Error(), "ntfy.server: The host ntfy.sh is not one that workbench.notifications.ntfy.hosts lists") {
		t.Fatalf("a server outside the hosts = %v", err)
	}
	if _, err := f.s.Put(as("alice"), Body{Ntfy: Ntfy{Server: "https://push.example.com", Topic: "alice"}}, ""); err != nil {
		t.Fatalf("a listed host = %v", err)
	}
	for _, block := range []string{
		"notifications:\n  ntfy:\n    server: ntfy.example.com\n",
		"notifications:\n  ntfy:\n    server: https://u:p@ntfy.example.com\n",
		"notifications:\n  ntfy:\n    hosts: [\"[\"]\n",
	} {
		cfg := fw.DefaultConfig()
		cfg.Extensions = workbench(t, block)
		if _, err := LoadConfig(cfg); err == nil || !strings.Contains(err.Error(), "workbench.notifications.ntfy") {
			t.Errorf("LoadConfig(%q) = %v", block, err)
		}
	}
}

// The test route sends one JSON publish to the server with the token as a
// bearer token, also in the quiet hours, and answers 502 with the reason
// when it does not go out.
func TestTestRoute(t *testing.T) {
	n := newNtfy(t)
	f := newFront(t, "notifications:\n  ntfy:\n    server: "+n.URL+"\n", true)
	resp, body := send(t, http.MethodPost, f.url+"/v1/notifications/test", "alice", "")
	if resp.StatusCode != http.StatusBadGateway || !strings.Contains(body, `"error":"No topic is set.`) {
		t.Fatalf("a test without a topic = %d %s", resp.StatusCode, body)
	}
	f.clock.Set(time.Date(2026, 10, 23, 23, 0, 0, 0, time.UTC))
	f.set(t, "alice", Body{Ntfy: Ntfy{Topic: "alice-fw", Token: "tk_1"}, QuietHours: &QuietHours{From: "22:00", To: "07:00", TimeZone: "UTC"}})
	resp, body = send(t, http.MethodPost, f.url+"/v1/notifications/test", "alice", "")
	if resp.StatusCode != http.StatusOK || body != "{\"sent\":true}\n" {
		t.Fatalf("a test = %d %s", resp.StatusCode, body)
	}
	got, tokens := n.published()
	if len(got) != 1 || got[0].Topic != "alice-fw" || got[0].Title != "Test from featherweight" || got[0].Message == "" || tokens[0] != "Bearer tk_1" {
		t.Fatalf("the publish = %+v %v", got, tokens)
	}
	n.setStatus(http.StatusForbidden)
	resp, body = send(t, http.MethodPost, f.url+"/v1/notifications/test", "alice", "")
	if resp.StatusCode != http.StatusBadGateway || !strings.Contains(body, "answered 403: unauthorized") || strings.Contains(body, "alice-fw") || strings.Contains(body, "tk_1") {
		t.Fatalf("a refused test = %d %s", resp.StatusCode, body)
	}
}

// Without workbench.notifications.ntfy.hosts, the server of a person must
// be on a public address: the dialer refuses a loopback address after the
// name resolves.
func TestPrivateServerOfAPerson(t *testing.T) {
	n := newNtfy(t)
	f := newFront(t, "", true)
	f.set(t, "alice", Body{Ntfy: Ntfy{Server: n.URL, Topic: "alice"}})
	err := f.s.Test(as("alice"))
	if !errors.Is(err, ErrUpstream) || !strings.Contains(err.Error(), "loopback, private, or link-local") {
		t.Fatalf("Test = %v", err)
	}
	if got, _ := n.published(); len(got) != 0 {
		t.Fatalf("the server got %d publishes", len(got))
	}
}

// A notice of a duty reaches each person of its list whose settings send
// its kind, with a title that names the duty.
func TestNoticesReachPeople(t *testing.T) {
	f := newFront(t, "notifications:\n  ntfy:\n    server: https://ntfy.example.com\n", false)
	f.duty(t, "report", &duties.Notify{Failure: []string{"owner", "bob"}, NeedsYou: []string{"owner"}})
	f.set(t, "alice", Body{Ntfy: Ntfy{Topic: "alice", Token: "tk"}, Send: &SendBody{Failures: ptrBool(false)}})
	f.set(t, "bob", Body{Ntfy: Ntfy{Topic: "bob"}})
	f.tell(t, duties.NoticeNeedsYou, "report", "The agent triage asks to run git.push.")
	f.tell(t, duties.NoticeFailed, "report", "The run failed: budget exceeded.")
	f.pass(t)
	got := f.out.list()
	if len(got) != 2 {
		t.Fatalf("sent = %+v", got)
	}
	byTo := map[string]sent{}
	for _, s := range got {
		byTo[s.To] = s
	}
	if a := byTo["alice"]; a.Title != "Duty report needs you" || a.Text != "The agent triage asks to run git.push." || a.Topic != "alice" || a.Token != "tk" || a.Server != "https://ntfy.example.com" {
		t.Fatalf("alice got %+v", a)
	}
	if b := byTo["bob"]; b.Title != "Duty report failed" || b.Kind != duties.NoticeFailed {
		t.Fatalf("bob got %+v", b)
	}
	if keys, _ := bucket.KeysUnder(context.Background(), f.b, "out"); len(keys) != 0 {
		t.Fatalf("the outbox holds %v", keys)
	}
}

// A notification during the quiet hours waits in the bucket until they
// end. Another replica on the same bucket sends it once, after a restart
// of the first one.
func TestQuietHoursHold(t *testing.T) {
	b := bucket.NewMem(notifyBucket)
	f := newFrontOn(t, "notifications:\n  ntfy:\n    server: https://ntfy.example.com\n", false, b)
	f.duty(t, "report", nil)
	f.set(t, "alice", Body{Ntfy: Ntfy{Topic: "alice"}, QuietHours: &QuietHours{From: "22:00", To: "07:00", TimeZone: "America/New_York"}})
	night := time.Date(2026, 10, 24, 3, 0, 0, 0, time.UTC) // 23:00 in New York
	f.clock.Set(night)
	f.tell(t, duties.NoticeFailed, "report", "The run failed.")
	f.pass(t)
	if got := f.out.list(); len(got) != 0 {
		t.Fatalf("a notification went out in the quiet hours: %+v", got)
	}
	f.s.Close()
	g := newFrontOn(t, "notifications:\n  ntfy:\n    server: https://ntfy.example.com\n", false, b)
	g.clock.Set(night.Add(7*time.Hour + 59*time.Minute))
	g.pass(t)
	if got := g.out.list(); len(got) != 0 {
		t.Fatalf("a notification went out before the end of the quiet hours: %+v", got)
	}
	g.clock.Set(time.Date(2026, 10, 24, 11, 0, 0, 0, time.UTC)) // 07:00 in New York
	g.pass(t)
	g.pass(t)
	if got := g.out.list(); len(got) != 1 || got[0].Title != "Duty report failed" {
		t.Fatalf("after the quiet hours = %+v", got)
	}
}

// Of two replicas on one bucket, only one sends a notification.
func TestOneReplicaSends(t *testing.T) {
	b := bucket.NewMem(notifyBucket)
	f := newFrontOn(t, "notifications:\n  ntfy:\n    server: https://ntfy.example.com\n", false, b)
	g := newFrontOn(t, "notifications:\n  ntfy:\n    server: https://ntfy.example.com\n", false, b)
	f.duty(t, "report", nil)
	f.set(t, "alice", Body{Ntfy: Ntfy{Topic: "alice"}})
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

// A notification that the server refuses goes again, and the front drops
// it after maxAttempts.
func TestRetryThenDrop(t *testing.T) {
	f := newFront(t, "notifications:\n  ntfy:\n    server: https://ntfy.example.com\n", false)
	f.duty(t, "report", nil)
	f.set(t, "alice", Body{Ntfy: Ntfy{Topic: "alice"}})
	f.out.setFail(errors.New("the server ntfy.example.com answered 500: Internal Server Error"))
	f.tell(t, duties.NoticeCap, "report", "The duty spent $2.00 today.")
	for range maxAttempts {
		f.pass(t)
		f.clock.Add(time.Hour)
	}
	if keys, _ := bucket.KeysUnder(context.Background(), f.b, "out"); len(keys) != 0 {
		t.Fatalf("the outbox holds %v after %d attempts", keys, maxAttempts)
	}
	f.out.setFail(nil)
	f.tell(t, duties.NoticeCap, "report", "The duty spent $2.00 today.")
	f.out.setFail(errors.New("down"))
	f.pass(t)
	f.out.setFail(nil)
	f.pass(t)
	if got := f.out.list(); len(got) != 0 {
		t.Fatalf("a retry went out before its time: %+v", got)
	}
	f.clock.Add(time.Minute)
	f.pass(t)
	if got := f.out.list(); len(got) != 1 || got[0].Title != "Duty report reached a daily cap" {
		t.Fatalf("the retry = %+v", got)
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
// replica, with each start of the last day, its outcome, and its cost.
func TestDigest(t *testing.T) {
	b := bucket.NewMem(notifyBucket)
	block := "notifications:\n  ntfy:\n    server: https://ntfy.example.com\n"
	f := newFrontOn(t, block, false, b)
	g := newFrontOn(t, block, false, b)
	now := time.Now().UTC()
	f.duty(t, "report", nil)
	if out, err := duties.Of(f.a).RunNow(as("alice"), "report"); err != nil || out.Run == "" {
		t.Fatalf("RunNow = %+v, %v", out, err)
	}
	f.set(t, "alice", Body{Ntfy: Ntfy{Topic: "alice"}, Send: &SendBody{Digest: ptrBool(true)}, QuietHours: &QuietHours{From: "22:00", To: "07:00", TimeZone: "UTC"}})
	f.set(t, "bob", Body{Ntfy: Ntfy{Topic: "bob"}, Send: &SendBody{Digest: ptrBool(true)}})
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
	if len(got) != 1 || got[0].To != "alice" || got[0].Kind != KindDigest || got[0].Title != "Your duties: the last day" ||
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

// A call without a person has no settings.
func TestNobody(t *testing.T) {
	f := newFront(t, "", false)
	if _, err := f.s.Get(context.Background()); !errors.Is(err, ErrNobody) || !errors.Is(err, fw.ErrForbidden) {
		t.Fatalf("Get without a person = %v", err)
	}
}
