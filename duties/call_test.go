package duties

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

// hookDuty starts on a call with the bearer token of HOOK_TOKEN.
func hookDuty(name string) Duty {
	return Duty{
		Name:   name,
		Agent:  "triage",
		When:   When{Webhook: &Webhook{Auth: WebhookAuth{Mode: "bearer", Secret: "HOOK_TOKEN"}}},
		Do:     Do{Ask: &Ask{Message: "Handle {event}"}},
		Limits: Limits{Budget: &Budget{MaxTurns: 5}, Overlap: OverlapAllow},
	}
}

// A call needs the bearer token of its duty. Its body is the event of the
// run, and the same body within ten minutes is a duplicate.
func TestCallBearer(t *testing.T) {
	f := newFront(t, checkBlock, nil)
	f.sec["HOOK_TOKEN"] = "tok"
	v := f.put(t, "alice", hookDuty("hook"))
	// A person reads plain words, and the address and the credential are
	// for a developer.
	if v.Readback != "When another app sends an event with its key. Repeats of the same event within ten minutes are skipped." || len(v.Next) != 0 ||
		v.ForDevelopers != "Another app calls POST /v1/duties/hook/call with the bearer token of the duty. A second call with the same body within ten minutes is a duplicate." {
		t.Fatalf("the duty = %q %q %v", v.Readback, v.ForDevelopers, v.Next)
	}
	url := f.url + "/v1/duties/hook/call"
	resp, _ := send(t, http.MethodPost, url, "", `{"id": 1}`)
	if resp.StatusCode != http.StatusUnauthorized || resp.Header.Get("WWW-Authenticate") == "" {
		t.Fatalf("a call without a token = %d", resp.StatusCode)
	}
	if resp, _ := send(t, http.MethodPost, url, "wrong", `{"id": 1}`); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a call with a wrong token = %d", resp.StatusCode)
	}
	resp, b := send(t, http.MethodPost, url, "tok", `{"id": 1}`)
	if resp.StatusCode != http.StatusAccepted || !strings.Contains(b, `"run":"`) {
		t.Fatalf("a call = %d %s", resp.StatusCode, b)
	}
	if req := f.queue.last(t); req.Input != "Handle "+BlockRef(ContextEvent) || blockOf(req, ContextEvent) != `{"id":1}` {
		t.Fatalf("the input = %q, the blocks %+v", req.Input, req.Context)
	}
	if h := f.history(t, "hook"); h[0].Start != StartCall {
		t.Fatalf("history = %+v", h[0])
	}
	if resp, b := send(t, http.MethodPost, url, "tok", `{"id": 1}`); resp.StatusCode != http.StatusOK || b != "{\"duplicate\":true}\n" {
		t.Fatalf("the same body again = %d %s", resp.StatusCode, b)
	}
	if resp, _ := send(t, http.MethodPost, url, "tok", `{"id": 2}`); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("another body = %d", resp.StatusCode)
	}
	f.clock.Add(CallBodyWindow + time.Second)
	if resp, _ := send(t, http.MethodPost, url, "tok", `{"id": 1}`); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("the same body after the window = %d", resp.StatusCode)
	}
	if resp, _ := send(t, http.MethodPost, f.url+"/v1/duties/nothing/call", "tok", `{}`); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("a call of no duty = %d", resp.StatusCode)
	}
	f.put(t, "alice", daily("report"))
	if resp, _ := send(t, http.MethodPost, f.url+"/v1/duties/report/call", "tok", `{}`); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("a call of a duty without a webhook = %d", resp.StatusCode)
	}
	if _, err := f.s.SetPaused(as("alice"), "hook", true); err != nil {
		t.Fatal(err)
	}
	if resp, b := send(t, http.MethodPost, url, "tok", `{"id": 3}`); resp.StatusCode != http.StatusOK || !strings.Contains(b, "The duty is paused.") {
		t.Fatalf("a call of a paused duty = %d %s", resp.StatusCode, b)
	}
}

// A duty with hmac checks the signature of the body, and a duty with a
// key de-duplicates the calls by the key of the body for a week.
func TestCallKeyAndSignature(t *testing.T) {
	f := newFront(t, checkBlock, nil)
	f.sec["HOOK_TOKEN"] = "key"
	d := hookDuty("signed")
	d.When.Webhook = &Webhook{Auth: WebhookAuth{Mode: "hmac", Secret: "HOOK_TOKEN"}, Key: "event.id"}
	v := f.put(t, "alice", d)
	if v.When.Webhook.Auth.Header != "X-Signature-256" || v.Readback != "When another app sends an event, signed. Repeats of one event within a week are skipped." ||
		v.ForDevelopers != "Another app calls POST /v1/duties/signed/call with the HMAC-SHA256 signature of the body in the header X-Signature-256. A second call with the same event.id within a week is a duplicate." {
		t.Fatalf("the duty = %+v %q %q", v.When.Webhook, v.Readback, v.ForDevelopers)
	}
	sign := func(body string) string {
		mac := hmac.New(sha256.New, []byte("key"))
		mac.Write([]byte(body))
		return "sha256=" + hex.EncodeToString(mac.Sum(nil))
	}
	url := f.url + "/v1/duties/signed/call"
	post := func(body, sig string) (*http.Response, string) {
		return send(t, http.MethodPost, url, "", body, "X-Signature-256", sig)
	}
	first := `{"event": {"id": "A"}, "n": 1}`
	if resp, _ := post(first, sign("other")); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a wrong signature = %d", resp.StatusCode)
	}
	if resp, b := post(first, sign(first)); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("a signed call = %d %s", resp.StatusCode, b)
	}
	again := `{"event": {"id": "A"}, "n": 2}`
	if resp, b := post(again, sign(again)); resp.StatusCode != http.StatusOK || !strings.Contains(b, "duplicate") {
		t.Fatalf("the same key = %d %s", resp.StatusCode, b)
	}
	none := `{"n": 3}`
	if resp, b := post(none, sign(none)); resp.StatusCode != http.StatusBadRequest || !strings.Contains(b, "The body has no event.id") {
		t.Fatalf("a body without the key = %d %s", resp.StatusCode, b)
	}
	f.clock.Add(CallKeyWindow + time.Hour)
	if resp, _ := post(again, sign(again)); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("the same key after a week = %d", resp.StatusCode)
	}
}

// A webhook names a secret of the list of the operator only.
func TestCallSecretRule(t *testing.T) {
	f := newFront(t, "", nil)
	f.sec["HOOK_TOKEN"] = "tok"
	_, err := f.s.Put(as("alice"), "hook", DutyBody{Duty: hookDuty("hook")}, PutOptions{})
	if err == nil || !strings.Contains(err.Error(), "when.webhook.auth.secret: HOOK_TOKEN is not in workbench.duties.secrets") {
		t.Fatalf("a secret off the list = %v", err)
	}
}

// A duty keeps its title in the words of the person beside its name, in
// one line, and refuses a title that is not one short line.
func TestDutyTitle(t *testing.T) {
	f := newFront(t, checkBlock, nil)
	f.sec["HOOK_TOKEN"] = "tok"
	d := hookDuty("walk-duty-1")
	d.Title = "  Walk   Duty 1 "
	if v := f.put(t, "alice", d); v.Title != "Walk Duty 1" || v.Name != "walk-duty-1" {
		t.Fatalf("the duty = %q %q", v.Title, v.Name)
	}
	if v, err := f.s.Get(as("alice"), "walk-duty-1"); err != nil || v.Title != "Walk Duty 1" {
		t.Fatalf("Get = %q, %v", v.Title, err)
	}
	for _, title := range []string{strings.Repeat("a", MaxTitleRunes+1), "Walk\u0007Duty"} {
		d.Title = title
		_, err := f.s.Put(as("alice"), d.Name, DutyBody{Duty: d}, PutOptions{})
		var pe *ProblemsError
		if !errors.As(err, &pe) || !strings.Contains(strings.Join(pe.Problems, " "), "title: A title is one line of at most 100 characters.") {
			t.Errorf("title %q = %v", title, err)
		}
	}
}
