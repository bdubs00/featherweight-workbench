package sources

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// ticketsBlock is a webhook source of a ticket system with a bearer token.
const ticketsBlock = `
sources:
  - name: tickets
    agent: default
    auth: {mode: bearer, secret: TICKETS_TOKEN}
    input: "Triage ticket {{.id}}: {{.summary}}"
    title: "Ticket {{.id}}"
    metadata: {ticket: "{{.id}}"}
    key: "{{.id}}/{{.updated}}"
`

const ticketsToken = "tok-tickets-7f3a"

func ticketJSON(id int, updated string) string {
	return fmt.Sprintf(`{"id": %d, "updated": %q, "summary": "Printer %d is on fire"}`, id, updated, id)
}

func decodeAnswer(t *testing.T, resp *http.Response, body string, status int) answer {
	t.Helper()
	if resp.StatusCode != status {
		t.Fatalf("status = %d, want %d: %s", resp.StatusCode, status, body)
	}
	var a answer
	if err := json.Unmarshal([]byte(body), &a); err != nil {
		t.Fatalf("decode: %v: %s", err, body)
	}
	return a
}

// TestWebhookMakesOneRunPerEvent posts the events of a ticket system. Each
// new event makes one run on the worker, as the source, in the batch of
// the source; the same event again makes none.
func TestWebhookMakesOneRunPerEvent(t *testing.T) {
	bus := fw.NewMemBus()
	worker := newWorker(t, bus, "fw.jobs.default")
	f := newFront(t, bus, ticketsBlock, secrets("TICKETS_TOKEN", ticketsToken), nil)
	url := f.url + "/v1/sources/tickets"
	auth := []string{"Authorization", "Bearer " + ticketsToken}

	resp, body := post(t, url, ticketJSON(1, "2026-10-19T10:00:00Z"), auth...)
	ans := decodeAnswer(t, resp, body, http.StatusAccepted)
	if ans.Source != "tickets" || ans.Batch != "tickets" || ans.Queue != "fw.jobs.default" || ans.Accepted != 1 || len(ans.Runs) != 1 ||
		ans.Runs[0].Key != "1/2026-10-19T10:00:00Z" || ans.Runs[0].IdempotencyKey != "tickets/1/2026-10-19T10:00:00Z" || ans.Runs[0].Title != "Ticket 1" {
		t.Fatalf("answer = %+v", ans)
	}
	eventually(t, "the worker runs the event", func() bool {
		runs := runsOf(t, worker)
		return len(runs) == 1 && runs[0].State.Terminal()
	})
	r := runsOf(t, worker)[0]
	if r.Principal != "source:tickets" || r.Input != "Triage ticket 1: Printer 1 is on fire" || r.Title != "Ticket 1" ||
		r.Metadata[fw.MetaBatch] != "tickets" || r.Metadata[MetaKey] != "1/2026-10-19T10:00:00Z" || r.Metadata["ticket"] != "1" ||
		r.Metadata["idempotencyKey"] != "tickets/1/2026-10-19T10:00:00Z" || r.Origin != fw.OriginBus {
		t.Fatalf("run = %+v", r)
	}

	// The same event again is a duplicate: 200 with a note, and no run.
	resp, body = post(t, url, ticketJSON(1, "2026-10-19T10:00:00Z"), auth...)
	ans = decodeAnswer(t, resp, body, http.StatusOK)
	if ans.Accepted != 0 || ans.Duplicate != 1 || len(ans.Duplicates) != 1 || !strings.Contains(ans.Note, "duplicate") || len(ans.Runs) != 0 {
		t.Fatalf("duplicate answer = %+v", ans)
	}
	// An array with the old event, a new version of it, and a new ticket.
	resp, body = post(t, url, "["+ticketJSON(1, "2026-10-19T10:00:00Z")+","+ticketJSON(1, "2026-10-19T11:00:00Z")+","+ticketJSON(2, "2026-10-19T10:00:00Z")+"]", auth...)
	ans = decodeAnswer(t, resp, body, http.StatusAccepted)
	if ans.Accepted != 2 || ans.Duplicate != 1 || ans.Duplicates[0].Event != 1 || ans.Runs[0].Event != 2 || ans.Runs[1].Event != 3 {
		t.Fatalf("array answer = %+v", ans)
	}
	eventually(t, "the worker runs the new events", func() bool { return len(runsOf(t, worker)) == 3 })
	time.Sleep(50 * time.Millisecond)
	if n := len(runsOf(t, worker)); n != 3 {
		t.Fatalf("the worker has %d runs, want 3", n)
	}
	st := statOf(t, f.a, "tickets")
	if st.Accepted != 3 || st.Duplicate != 2 || st.Refused != 0 || st.Kind != "webhook" || st.Agent != "default" || st.LastAcceptedAt.IsZero() {
		t.Fatalf("stat = %+v", st)
	}
}

// TestWebhookRefuses refuses a caller without the token, a body that does
// not read, and an event that does not fit the templates, and makes no
// run for any of them. No answer and no counter holds the token.
func TestWebhookRefuses(t *testing.T) {
	bus := &countBus{Bus: fw.NewMemBus()}
	f := newFront(t, bus, ticketsBlock, secrets("TICKETS_TOKEN", ticketsToken), nil)
	url := f.url + "/v1/sources/tickets"
	auth := []string{"Authorization", "Bearer " + ticketsToken}

	resp, body := post(t, url, ticketJSON(1, "x"))
	if resp.StatusCode != http.StatusUnauthorized || resp.Header.Get("WWW-Authenticate") == "" {
		t.Fatalf("no token = %d %s", resp.StatusCode, body)
	}
	resp, body = post(t, url, ticketJSON(1, "x"), "Authorization", "Bearer "+ticketsToken+"x")
	if resp.StatusCode != http.StatusUnauthorized || strings.Contains(body, ticketsToken) {
		t.Fatalf("wrong token = %d %s", resp.StatusCode, body)
	}
	if resp, body := post(t, url, "id=1", auth...); resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, "not JSON") {
		t.Fatalf("not JSON = %d %s", resp.StatusCode, body)
	}
	// The second event has no updated field, so neither event runs.
	resp, body = post(t, url, "["+ticketJSON(5, "x")+`,{"id": 6, "summary": "s"}]`, auth...)
	if resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "Event 2") || !strings.Contains(body, "updated") {
		t.Fatalf("an event without a field = %d %s", resp.StatusCode, body)
	}
	many := make([]string, 501)
	for i := range many {
		many[i] = ticketJSON(i, "x")
	}
	if resp, body := post(t, url, "["+strings.Join(many, ",")+"]", auth...); resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, "at most 500") {
		t.Fatalf("501 events = %d %s", resp.StatusCode, body)
	}
	if resp, body := post(t, url, `{"big": "`+strings.Repeat("x", maxBody)+`"}`, auth...); resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("a large body = %d %.200s", resp.StatusCode, body)
	}
	if resp, body := post(t, f.url+"/v1/sources/nobody", ticketJSON(1, "x"), auth...); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown source = %d %s", resp.StatusCode, body)
	}
	if resp, body := post(t, url, "[]", auth...); resp.StatusCode != http.StatusOK || !strings.Contains(body, "no event") {
		t.Fatalf("no event = %d %s", resp.StatusCode, body)
	}
	if n := bus.sent.Load(); n != 0 {
		t.Fatalf("%d run requests went out", n)
	}
	st := statOf(t, f.a, "tickets")
	if st.Accepted != 0 || st.Refused != 2+1+2+501+1 || st.LastError == "" || strings.Contains(st.LastError, ticketsToken) {
		t.Fatalf("stat = %+v", st)
	}
	if strings.Contains(f.logs.String(), ticketsToken) {
		t.Fatal("a log line holds the token")
	}
}

func TestWebhookHMAC(t *testing.T) {
	bus := &countBus{Bus: fw.NewMemBus()}
	f := newFront(t, bus, `
sources:
  - name: alerts
    agent: default
    auth: {mode: hmac, secret: ALERTS_KEY, header: X-Alert-Signature}
    items: alerts
    key: "{{.fingerprint}}"
`, secrets("ALERTS_KEY", "k3y"), nil)
	url := f.url + "/v1/sources/alerts"
	body := `{"alerts": [{"fingerprint": "a1", "status": "firing"}, {"fingerprint": "a2", "status": "firing"}]}`
	mac := hmac.New(sha256.New, []byte("k3y"))
	mac.Write([]byte(body))
	sig := "sha256=" + hex.EncodeToString(mac.Sum(nil))

	if resp, out := post(t, url, body, "X-Alert-Signature", "sha256=00"); resp.StatusCode != http.StatusUnauthorized || !strings.Contains(out, "X-Alert-Signature") {
		t.Fatalf("a wrong signature = %d %s", resp.StatusCode, out)
	}
	resp, out := post(t, url, body, "X-Alert-Signature", sig)
	if ans := decodeAnswer(t, resp, out, http.StatusAccepted); ans.Accepted != 2 {
		t.Fatalf("answer = %+v", ans)
	}
	// A body that someone changed keeps no valid signature.
	if resp, out := post(t, url, strings.Replace(body, "a2", "a3", 1), "X-Alert-Signature", sig); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a changed body = %d %s", resp.StatusCode, out)
	}
	if n := bus.sent.Load(); n != 2 {
		t.Fatalf("%d run requests went out, want 2", n)
	}
}

// TestWebhookWithoutCredential takes events only because the source says
// none.
func TestWebhookWithoutCredential(t *testing.T) {
	f := newFront(t, fw.NewMemBus(), `
sources:
  - {name: open, agent: default, auth: {mode: none}, key: "{{.id}}"}
`, secrets(), nil)
	resp, out := post(t, f.url+"/v1/sources/open", `{"id": 1}`)
	if ans := decodeAnswer(t, resp, out, http.StatusAccepted); ans.Accepted != 1 {
		t.Fatalf("answer = %+v", ans)
	}
	if !strings.Contains(f.logs.String(), "without a credential") {
		t.Fatal("the start did not warn of a source without a credential")
	}
}

// TestWebhookStoreDown publishes nothing without a claim: the store does
// not answer, so the route answers 503 and no run request goes out.
func TestWebhookStoreDown(t *testing.T) {
	bus := &countBus{Bus: fw.NewMemBus()}
	f := newFront(t, bus, ticketsBlock, secrets("TICKETS_TOKEN", ticketsToken), nil)
	swapStore(f, fakeStore{err: fmt.Errorf("%w: not connected to NATS", ErrStore)})
	resp, body := post(t, f.url+"/v1/sources/tickets", "["+ticketJSON(1, "x")+","+ticketJSON(2, "x")+"]", "Authorization", "Bearer "+ticketsToken)
	if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("Retry-After") == "" || !strings.Contains(body, "de-duplication store did not answer") ||
		!strings.Contains(body, "2 of 2 events made no run") {
		t.Fatalf("store down = %d %s", resp.StatusCode, body)
	}
	if n := bus.sent.Load(); n != 0 {
		t.Fatalf("%d run requests went out without a claim", n)
	}
	if st := statOf(t, f.a, "tickets"); st.Refused != 2 || !strings.Contains(st.LastError, "not connected") {
		t.Fatalf("stat = %+v", st)
	}
	// Another replica holds the event: 503, so the sender tries again.
	swapStore(f, fakeStore{err: ErrInFlight})
	resp, body = post(t, f.url+"/v1/sources/tickets", ticketJSON(1, "x"), "Authorization", "Bearer "+ticketsToken)
	if resp.StatusCode != http.StatusServiceUnavailable || !strings.Contains(body, "Another replica") {
		t.Fatalf("busy = %d %s", resp.StatusCode, body)
	}
	if st := statOf(t, f.a, "tickets"); st.Busy != 1 {
		t.Fatalf("stat = %+v", st)
	}
}

// TestWebhookPublishFails releases the claim of an event whose request did
// not reach the queue, so the next delivery makes the run.
func TestWebhookPublishFails(t *testing.T) {
	bus := &countBus{Bus: fw.NewMemBus()}
	worker := newWorker(t, bus, "fw.jobs.default")
	f := newFront(t, bus, ticketsBlock, secrets("TICKETS_TOKEN", ticketsToken), nil)
	url, auth := f.url+"/v1/sources/tickets", []string{"Authorization", "Bearer " + ticketsToken}
	bus.fail.Store(true)
	resp, body := post(t, url, ticketJSON(1, "x"), auth...)
	if resp.StatusCode != http.StatusServiceUnavailable || !strings.Contains(body, "queue did not take") {
		t.Fatalf("publish fails = %d %s", resp.StatusCode, body)
	}
	bus.fail.Store(false)
	resp, body = post(t, url, ticketJSON(1, "x"), auth...)
	if ans := decodeAnswer(t, resp, body, http.StatusAccepted); ans.Accepted != 1 {
		t.Fatalf("the retry = %+v", ans)
	}
	eventually(t, "the retry runs", func() bool { return len(runsOf(t, worker)) == 1 })
	if st := statOf(t, f.a, "tickets"); st.Accepted != 1 || st.Refused != 1 || !strings.Contains(st.LastError, "bus is down") {
		t.Fatalf("stat = %+v", st)
	}
}

// TestTwoReplicasMakeOneRun runs two fronts on one bus with the claims in
// one JetStream server, as two replicas of the front. The sender fires the
// same event at both, many times at once. One run starts. Every other
// answer is a duplicate or a 503 that asks to send again, and a resend of
// those is a duplicate.
func TestTwoReplicasMakeOneRun(t *testing.T) {
	srv := runNATS(t)
	bus := &countBus{Bus: fw.NewMemBus()}
	worker := newWorker(t, bus, "fw.jobs.default")
	nats := func(c *fw.Config) { c.Bus.Driver, c.Bus.URL = "nats", srv.ClientURL() }
	fronts := []*front{
		newFront(t, bus, ticketsBlock, secrets("TICKETS_TOKEN", ticketsToken), nats),
		newFront(t, bus, ticketsBlock, secrets("TICKETS_TOKEN", ticketsToken), nats),
	}
	if !strings.Contains(fronts[0].s.store.Describe(), "JetStream") {
		t.Fatalf("store = %s", fronts[0].s.store.Describe())
	}
	auth := []string{"Authorization", "Bearer " + ticketsToken}
	event := ticketJSON(7, "2026-10-19T10:00:00Z")
	var mu sync.Mutex
	codes := map[int]int{}
	var wg sync.WaitGroup
	for i := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, _ := post(t, fronts[i%2].url+"/v1/sources/tickets", event, auth...)
			mu.Lock()
			codes[resp.StatusCode]++
			mu.Unlock()
		}()
	}
	wg.Wait()
	if codes[http.StatusAccepted] != 1 || codes[http.StatusOK]+codes[http.StatusServiceUnavailable] != 15 {
		t.Fatalf("answers = %v", codes)
	}
	for i := range 2 {
		resp, body := post(t, fronts[i].url+"/v1/sources/tickets", event, auth...)
		if ans := decodeAnswer(t, resp, body, http.StatusOK); ans.Duplicate != 1 {
			t.Fatalf("a resend = %+v", ans)
		}
	}
	eventually(t, "the worker runs the event", func() bool { return len(runsOf(t, worker)) == 1 })
	time.Sleep(50 * time.Millisecond)
	if n, sent := len(runsOf(t, worker)), bus.sent.Load(); n != 1 || sent != 1 {
		t.Fatalf("the worker has %d runs from %d requests, want 1 from 1", n, sent)
	}
	a, b := statOf(t, fronts[0].a, "tickets"), statOf(t, fronts[1].a, "tickets")
	if a.Accepted+b.Accepted != 1 || a.Duplicate+a.Busy+b.Duplicate+b.Busy != 17 {
		t.Fatalf("stats = %+v %+v", a, b)
	}
}
