package duties

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/sources"
)

// importBlock holds a webhook entry, a poll entry of another agent, and a
// locked webhook entry.
const importBlock = `
sources:
  - name: tickets
    agent: triage
    auth: {mode: none}
    input: "Triage the ticket {{.id}}: {{.title}}"
    title: "Ticket {{.id}}"
    metadata: {ticket: "{{.id}}"}
    key: "{{.id}}"
  - name: alerts
    agent: private
    poll: {url: "https://alerts.example.com/api?token=x", every: 2m}
    key: "{{.id}}"
  - name: pinned
    agent: triage
    locked: true
    auth: {mode: none}
    input: "Pinned {{.id}}"
    key: "{{.id}}"
`

// jobs collects the run requests that reach the queue of an agent on the
// bus of a test.
type jobs struct {
	mu   sync.Mutex
	reqs []fw.RunRequest
}

func collect(t *testing.T, bus fw.Bus, queue string) *jobs {
	t.Helper()
	j := &jobs{}
	stop, err := bus.Consume(context.Background(), queue, func(e fw.Event) error {
		var req fw.RunRequest
		if err := json.Unmarshal(e.Data, &req); err != nil {
			return err
		}
		j.mu.Lock()
		j.reqs = append(j.reqs, req)
		j.mu.Unlock()
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stop)
	return j
}

func (j *jobs) list() []fw.RunRequest {
	j.mu.Lock()
	defer j.mu.Unlock()
	return slices.Clone(j.reqs)
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out: %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// At its start the server copies each entry of workbench.sources into the
// store once: a webhook becomes a duty with when.webhook, a poll a duty
// with a schedule of its interval and an HTTP check, and both keep their
// agent. A read shows the origin and never a template, nor the query of
// the address of an entry. Two replicas import each entry once, a
// deleted duty does not come back, and a change of the entry changes no
// duty, except for a locked entry, which follows the file.
func TestImportSources(t *testing.T) {
	bus := fw.NewMemBus()
	f := newFrontOn(t, bus, importBlock, nil)
	g := newReplica(t, bus, importBlock, nil, f.store)
	ctx := context.Background()
	for _, x := range []*front{f, g} {
		for _, name := range []string{"tickets", "alerts", "pinned"} {
			if !sources.Adopted(x.a, name) {
				t.Fatalf("the duties do not run %s on a replica", name)
			}
		}
	}
	list, err := f.s.st.list(ctx)
	if err != nil || len(list) != 3 {
		t.Fatalf("the store holds %d duties, %v", len(list), err)
	}

	resp, b := send(t, http.MethodGet, f.url+"/v1/duties/tickets", "carol", "")
	var v View
	decodeJSON(t, b, &v)
	if resp.StatusCode != http.StatusOK || v.Origin == nil || v.Origin.Kind != OriginConfig || v.Origin.Name != "tickets" || !v.Template || v.Locked ||
		v.Source != SourceStore || v.When.Webhook == nil || v.When.Webhook.Auth.Mode != "none" || v.When.Webhook.Key != "id" || v.Agent != "triage" ||
		v.Do.Ask == nil || v.Do.Ask.Message != "" || v.Owner != "source:tickets" || v.RunAs != RunAsOwner {
		t.Fatalf("the duty of a webhook = %d %s", resp.StatusCode, b)
	}
	for _, hidden := range []string{"{{", "Triage the ticket", "Ticket ", "templated", "hookSecret"} {
		if strings.Contains(b, hidden) {
			t.Fatalf("a read shows %q: %s", hidden, b)
		}
	}
	_, b = send(t, http.MethodGet, f.url+"/v1/duties/alerts", "dave", "")
	decodeJSON(t, b, &v)
	if v.When.Schedule == nil || v.When.Schedule.Every != "2m" || v.CheckFirst == nil || v.CheckFirst.Look.HTTP.URL != "https://alerts.example.com/api" ||
		v.CheckFirst.Key != "id" || !strings.Contains(v.Readback, "Every 2m, it looks at alerts.example.com/api") || strings.Contains(b, "token=x") || len(v.Next) != 5 {
		t.Fatalf("the duty of a poll = %s", b)
	}
	if resp, _ := send(t, http.MethodGet, f.url+"/v1/duties/alerts", "carol", ""); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("a duty of another agent = %d", resp.StatusCode)
	}

	// A locked duty refuses every change, and the detail says why.
	for _, c := range []struct{ method, path, body string }{
		{http.MethodPut, "/pinned", mustJSON(t, daily("pinned"))},
		{http.MethodPatch, "/pinned", `{"paused": true}`},
		{http.MethodDelete, "/pinned", ""},
	} {
		if resp, b := send(t, c.method, f.url+"/v1/duties"+c.path, "root", c.body); resp.StatusCode != http.StatusConflict || !strings.Contains(b, LockedText) {
			t.Errorf("%s %s = %d %s", c.method, c.path, resp.StatusCode, b)
		}
	}
	_, b = send(t, http.MethodGet, f.url+"/v1/duties/pinned", "alice", "")
	if !strings.Contains(b, `"locked":true`) || !strings.Contains(b, `"template":true`) {
		t.Fatalf("the locked duty = %s", b)
	}

	// A person deletes an imported duty. The next start imports nothing,
	// and a changed entry changes no duty. A locked entry follows the file.
	if resp, b := send(t, http.MethodDelete, f.url+"/v1/duties/tickets", "alice", ""); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("a delete = %d %s", resp.StatusCode, b)
	}
	changed := strings.Replace(strings.Replace(importBlock, "every: 2m", "every: 5m", 1), `input: "Pinned {{.id}}"`, `input: "Pinned again {{.id}}"`, 1)
	h := newReplica(t, bus, changed, nil, f.store)
	if _, err := h.s.st.get(ctx, "tickets"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a deleted duty came back: %v", err)
	}
	if !sources.Adopted(h.a, "tickets") {
		t.Fatal("the sources run a deleted entry again")
	}
	if st, _ := h.s.st.get(ctx, "alerts"); st.Duty.When.Schedule.Every != "2m" {
		t.Fatalf("a change of the entry changed the duty: %+v", st.Duty.When.Schedule)
	}
	if st, _ := h.s.st.get(ctx, "pinned"); st.From.Input != "Pinned again {{.id}}" || !st.Locked {
		t.Fatalf("the locked duty did not follow the file: %+v", st.From)
	}
}

// A person who writes a duty under the name of an entry keeps the name,
// and the sources go on running the entry.
func TestImportKeepsADutyOfAPerson(t *testing.T) {
	bus := fw.NewMemBus()
	f := newFrontOn(t, bus, "", nil)
	f.put(t, "alice", daily("tickets"))
	g := newReplica(t, bus, importBlock, nil, f.store)
	if sources.Adopted(g.a, "tickets") {
		t.Fatal("the duties took over an entry whose name a person holds")
	}
	st, err := g.s.st.get(context.Background(), "tickets")
	if err != nil || st.Origin != nil || st.Duty.Do.Ask.Message == "" {
		t.Fatalf("the duty of the person = %+v, %v", st, err)
	}
}

// An imported webhook duty runs as its entry ran: one run per event with
// the input, the title, the metadata, the key, and the principal of the
// entry, at its new address and at the old one, which answer the same. The
// same event again is a duplicate. A message that a person writes
// replaces the templates, and the next call gives the agent the event as
// data.
func TestImportedWebhookRunsAsBefore(t *testing.T) {
	bus := fw.NewMemBus()
	f := newFrontOn(t, bus, importBlock, nil)
	got := collect(t, bus, "fw.jobs.triage")
	post := func(path, body string) (int, map[string]any) {
		t.Helper()
		resp, b := send(t, http.MethodPost, f.url+path, "", body)
		var out map[string]any
		_ = json.Unmarshal([]byte(b), &out)
		return resp.StatusCode, out
	}
	code, ans := post("/v1/sources/tickets", `{"id": 7, "title": "Printer"}`)
	if code != http.StatusAccepted || ans["accepted"] != float64(1) || ans["batch"] != "tickets" {
		t.Fatalf("the old address = %d %v", code, ans)
	}
	waitFor(t, "the run request", func() bool { return len(got.list()) == 1 })
	req := got.list()[0]
	if req.Input != sources.InputOf("tickets", "Ticket 7") || blockOf(req, sources.ContextSource) != "Triage the ticket 7: Printer" || req.Metadata[fw.MetaTitle] != "Ticket 7" || req.Metadata["ticket"] != "7" ||
		req.Metadata[fw.MetaBatch] != "tickets" || req.Metadata[sources.MetaKey] != "7" || req.IdempotencyKey != "tickets/7" ||
		req.Principal != "source:tickets" || req.PrincipalKind != fw.KindWorkload || req.ID == "" {
		t.Fatalf("the run request = %+v", req)
	}
	code, ans = post("/v1/duties/tickets/call", `{"id": 7, "title": "Printer"}`)
	if code != http.StatusOK || ans["duplicate"] != float64(1) {
		t.Fatalf("the same event at the new address = %d %v", code, ans)
	}
	hist, err := f.s.st.history(context.Background(), "tickets", 10)
	if err != nil || len(hist) != 1 || hist[0].Run != req.ID || hist[0].State != StateQueued || hist[0].Start != StartCall {
		t.Fatalf("the history = %+v, %v", hist, err)
	}

	// A message of a person replaces the templates.
	_, b := send(t, http.MethodGet, f.url+"/v1/duties/tickets", "alice", "")
	var v View
	decodeJSON(t, b, &v)
	v.Do.Ask.Message = "Sort this ticket: {event}"
	v.Limits.Budget = &Budget{MaxCostUSD: 0.5}
	if resp, b := send(t, http.MethodPut, f.url+"/v1/duties/tickets", "alice", mustJSON(t, v)); resp.StatusCode != http.StatusOK || strings.Contains(b, `"template":true`) {
		t.Fatalf("a save with a message = %d %s", resp.StatusCode, b)
	}
	code, ans = post("/v1/sources/tickets", `{"id": 8, "title": "Screen"}`)
	if code != http.StatusAccepted || ans["run"] == nil {
		t.Fatalf("a call after the message = %d %v", code, ans)
	}
	if r := f.queue.last(t); r.Input != "Sort this ticket: "+BlockRef(ContextEvent) || !strings.HasPrefix(blockOf(r, ContextEvent), "{") || r.Metadata[fw.MetaBatch] != BatchOf("tickets") {
		t.Fatalf("the run of the message = %+v", r)
	}
}

// A signed entry, as the tickets of the lab, answers at the old address as
// before: 401 without a signature and with a wrong one, 202 for a signed
// event, and 200 for the same event again. The signature is the plain hex
// of Gitea, in the header of the entry. The redrive of the lab needs NATS,
// so this entry leaves it out.
func TestImportedSignedWebhook(t *testing.T) {
	t.Setenv("FW_TEST_HOOK_KEY", "key")
	bus := fw.NewMemBus()
	f := newFrontOn(t, bus, `
sources:
  - name: tickets
    agent: triage
    auth: {mode: hmac, secret: FW_TEST_HOOK_KEY, header: X-Gitea-Signature}
    input: "Triage the ticket {{.issue.number}}"
    key: "{{.issue.number}}"
`, nil)
	f.sec["FW_TEST_HOOK_KEY"] = "key"
	got := collect(t, bus, "fw.jobs.triage")
	sign := func(body string) string {
		mac := hmac.New(sha256.New, []byte("key"))
		mac.Write([]byte(body))
		return hex.EncodeToString(mac.Sum(nil))
	}
	post := func(body string, header ...string) (int, map[string]any) {
		t.Helper()
		resp, b := send(t, http.MethodPost, f.url+"/v1/sources/tickets", "", body, header...)
		var out map[string]any
		_ = json.Unmarshal([]byte(b), &out)
		return resp.StatusCode, out
	}
	event := `{"action": "opened", "issue": {"number": 12}}`
	if code, _ := post(event); code != http.StatusUnauthorized {
		t.Fatalf("an unsigned event = %d", code)
	}
	if code, _ := post(event, "X-Gitea-Signature", sign("other")); code != http.StatusUnauthorized {
		t.Fatalf("a wrongly signed event = %d", code)
	}
	if code, ans := post(event, "X-Gitea-Signature", sign(event)); code != http.StatusAccepted || ans["accepted"] != float64(1) {
		t.Fatalf("a signed event = %d %v", code, ans)
	}
	edited := `{"action": "edited", "issue": {"number": 12}}`
	if code, ans := post(edited, "X-Gitea-Signature", sign(edited)); code != http.StatusOK || ans["duplicate"] != float64(1) {
		t.Fatalf("a later event of the ticket = %d %v", code, ans)
	}
	waitFor(t, "the run request", func() bool { return len(got.list()) == 1 })
	if r := got.list()[0]; blockOf(r, sources.ContextSource) != "Triage the ticket 12" || r.Input != sources.InputOf("tickets", "") || r.IdempotencyKey != "tickets/12" {
		t.Fatalf("the run request = %+v", r)
	}
	_, b := send(t, http.MethodGet, f.url+"/v1/duties/tickets", "alice", "")
	var v View
	decodeJSON(t, b, &v)
	if v.When.Webhook == nil || v.When.Webhook.Auth.Mode != "hmac" || v.When.Webhook.Auth.Secret != "FW_TEST_HOOK_KEY" || v.When.Webhook.Auth.Header != "X-Gitea-Signature" || v.When.Webhook.Key != "issue.number" {
		t.Fatalf("the duty = %+v", v.When.Webhook)
	}
}

// A save of an imported duty without a message keeps its templates, also
// without a budget, and a deleted duty answers 404 at the old address.
func TestImportedSaveKeepsTheTemplates(t *testing.T) {
	bus := fw.NewMemBus()
	f := newFrontOn(t, bus, importBlock, nil)
	_, b := send(t, http.MethodGet, f.url+"/v1/duties/tickets", "alice", "")
	var body map[string]any
	if err := json.Unmarshal([]byte(b), &body); err != nil {
		t.Fatal(err)
	}
	body["agent"] = "triage"
	body["do"] = map[string]any{"ask": map[string]any{}}
	raw, _ := json.Marshal(body)
	if resp, b := send(t, http.MethodPut, f.url+"/v1/duties/tickets", "alice", string(raw)); resp.StatusCode != http.StatusOK || !strings.Contains(b, `"template":true`) || !strings.Contains(b, `"origin":{"kind":"config","name":"tickets"}`) {
		t.Fatalf("a save without a message = %d %s", resp.StatusCode, b)
	}
	if resp, b := send(t, http.MethodDelete, f.url+"/v1/duties/tickets", "root", ""); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("a delete of an admin = %d %s", resp.StatusCode, b)
	}
	if resp, b := send(t, http.MethodPost, f.url+"/v1/sources/tickets", "", `{"id": 9}`); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("the old address of a deleted duty = %d %s", resp.StatusCode, b)
	}
}

// An imported poll duty polls at the interval of its entry, also below
// the shortest step of a schedule, and starts one run per new item with
// the templates of its entry.
func TestImportedPollRunsAsBefore(t *testing.T) {
	var mu sync.Mutex
	items := `[{"id": 1, "title": "One"}]`
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer poll-token" {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(items))
	}))
	t.Cleanup(api.Close)
	t.Setenv("ALERTS_TOKEN", "poll-token")
	block := `
sources:
  - name: alerts
    agent: triage
    poll: {url: "` + api.URL + `/open?state=new", every: 30s, secret: ALERTS_TOKEN}
    input: "Look at alert {{.id}}"
    title: "Alert {{.title}}"
    key: "{{.id}}"
`
	bus := fw.NewMemBus()
	f := newFrontOn(t, bus, block, nil)
	f.sec["ALERTS_TOKEN"] = "poll-token"
	got := collect(t, bus, "fw.jobs.triage")
	ctx := context.Background()
	f.s.pass(ctx)
	f.s.pollers.Wait()
	waitFor(t, "the run of the first item", func() bool { return len(got.list()) == 1 })
	if r := got.list()[0]; blockOf(r, sources.ContextSource) != "Look at alert 1" || r.Metadata[fw.MetaTitle] != "Alert One" || r.Metadata[fw.MetaBatch] != "alerts" {
		t.Fatalf("the run request = %+v", r)
	}
	// Within the interval, a pass does not poll. After it, the same item
	// is a duplicate, and a new item starts its own run.
	f.s.pass(ctx)
	f.s.pollers.Wait()
	mu.Lock()
	items = `[{"id": 1, "title": "One"}, {"id": 2, "title": "Two"}]`
	mu.Unlock()
	f.clock.Add(31 * time.Second)
	f.s.pass(ctx)
	f.s.pollers.Wait()
	waitFor(t, "the run of the new item", func() bool { return len(got.list()) == 2 })
	if r := got.list()[1]; blockOf(r, sources.ContextSource) != "Look at alert 2" {
		t.Fatalf("the second run request = %+v", r)
	}
	_, b := send(t, http.MethodGet, f.url+"/v1/duties/alerts", "alice", "")
	if !strings.Contains(b, "Every 30s, it looks at") || strings.Contains(b, "state=new") || !strings.Contains(b, `"every":"30s"`) {
		t.Fatalf("the read of the poll = %s", b)
	}
	hist, _ := f.s.st.history(ctx, "alerts", 10)
	if len(hist) != 2 || hist[0].Found != 2 || !strings.Contains(hist[0].Reason, "2 events started 1 run. 1 made a run before.") {
		t.Fatalf("the history = %+v", hist)
	}
	// A save that keeps the address keeps the address of the entry with
	// its query.
	var v View
	decodeJSON(t, b, &v)
	if resp, b := send(t, http.MethodPut, f.url+"/v1/duties/alerts", "alice", mustJSON(t, v)); resp.StatusCode != http.StatusOK {
		t.Fatalf("a save of the poll = %d %s", resp.StatusCode, b)
	}
	st, _ := f.s.st.get(ctx, "alerts")
	if st.Duty.CheckFirst.Look.HTTP.URL != api.URL+"/open?state=new" || !st.templated() {
		t.Fatalf("the saved poll = %+v", st.Duty.CheckFirst.Look.HTTP)
	}
	// A paused duty polls not.
	if _, err := f.s.SetPaused(as("alice"), "alerts", true); err != nil {
		t.Fatal(err)
	}
	f.clock.Add(time.Minute)
	f.s.pass(ctx)
	f.s.pollers.Wait()
	time.Sleep(50 * time.Millisecond)
	if n := len(got.list()); n != 2 {
		t.Fatalf("a paused duty polled: %d runs", n)
	}
}

// keyPath reads a key template that names one field.
func TestKeyPath(t *testing.T) {
	for in, want := range map[string]string{
		"{{.id}}":               "id",
		"{{ .issue.number }}":   "issue.number",
		"{{.repo}}#{{.number}}": "",
		"{{index . \"x\"}}":     "",
		"id":                    "",
		"{{- .event.id -}}":     "event.id",
	} {
		if got := keyPath(in); got != want {
			t.Errorf("keyPath(%q) = %q, want %q", in, got, want)
		}
	}
}
