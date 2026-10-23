package sources

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"strings"
	"testing"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// compiled builds a running source from one entry, without secrets.
func compiled(t *testing.T, s Source) *source {
	t.Helper()
	s = withDefaults(s)
	tpl, err := compileTemplates(s)
	if err != nil {
		t.Fatal(err)
	}
	return &source{cfg: s, tpl: tpl}
}

// ticketEvent is the body that a ticket system posts.
const ticketEvent = `{"id": 90071992547409931, "updated": "2026-10-19T10:00:00Z", "summary": "Printer  on fire",
	"priority": null, "requester": {"name": "Ada"}}`

func TestRender(t *testing.T) {
	src := compiled(t, Source{Name: "tickets", Agent: "triage",
		Input:    "Triage ticket {{.id}}: {{.summary}} for {{.requester.name}}. Priority {{index . \"priority\" | default \"normal\"}}.",
		Title:    "Ticket {{.id}}: {{.summary}}",
		Metadata: map[string]string{"ticket": "{{.id}}", "requester": "{{.requester.name | lower}}"},
		Key:      "{{.id}}/{{.updated}}"})
	events, err := decodeEvents([]byte(ticketEvent), "")
	if err != nil || len(events) != 1 {
		t.Fatalf("decodeEvents = %v, %v", events, err)
	}
	it, err := src.render(1, events[0])
	if err != nil {
		t.Fatal(err)
	}
	req := it.req
	if it.key != "90071992547409931/2026-10-19T10:00:00Z" || req.IdempotencyKey != "tickets/"+it.key {
		t.Fatalf("key = %q, idempotency key = %q", it.key, req.IdempotencyKey)
	}
	if req.Input != "Triage ticket 90071992547409931: Printer  on fire for Ada. Priority normal." {
		t.Fatalf("input = %q", req.Input)
	}
	want := map[string]string{fw.MetaTitle: "Ticket 90071992547409931: Printer on fire", fw.MetaBatch: "tickets", MetaSource: "tickets",
		MetaKey: it.key, "ticket": "90071992547409931", "requester": "ada"}
	for k, v := range want {
		if req.Metadata[k] != v {
			t.Fatalf("metadata %s = %q, want %q (%v)", k, req.Metadata[k], v, req.Metadata)
		}
	}
	if req.Agent != "triage" || req.Principal != "source:tickets" || req.PrincipalKind != fw.KindWorkload {
		t.Fatalf("request = %+v", req)
	}
}

// A source of the issues of Gitea titles each run "#<number> <title>", so
// the runs of the tickets differ by their title where their input starts
// with the same sentence.
func TestRenderTitleOfAnIssue(t *testing.T) {
	src := compiled(t, Source{Name: "tickets", Agent: "triage",
		Input: "A new ticket of the helpdesk {{.repository.full_name}} (event {{.action}}). Triage it.\nNumber: {{.issue.number}}",
		Title: "#{{.issue.number}} {{.issue.title}}",
		Key:   "{{.issue.number}}"})
	events, err := decodeEvents([]byte(`{"action": "opened", "issue": {"number": 42, "title": "The printer on floor 3 is down", "user": {"login": "ada"}}, "repository": {"full_name": "lab/helpdesk"}}`), "")
	if err != nil || len(events) != 1 {
		t.Fatalf("decodeEvents = %v, %v", events, err)
	}
	it, err := src.render(1, events[0])
	if err != nil {
		t.Fatal(err)
	}
	if got := it.req.Metadata[fw.MetaTitle]; got != "#42 The printer on floor 3 is down" {
		t.Fatalf("title = %q", got)
	}
}

// TestRenderDefaultInput gives the run the event as JSON when the source
// sets no input, with every digit of a large id.
func TestRenderDefaultInput(t *testing.T) {
	src := compiled(t, Source{Name: "tickets", Agent: "triage", Key: "{{.id}}"})
	events, _ := decodeEvents([]byte(ticketEvent), "")
	it, err := src.render(1, events[0])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(it.req.Input, `"id": 90071992547409931`) || !strings.Contains(it.req.Input, `"name": "Ada"`) {
		t.Fatalf("input = %s", it.req.Input)
	}
	if _, ok := it.req.Metadata[fw.MetaTitle]; ok {
		t.Fatalf("a title without a template: %v", it.req.Metadata)
	}
}

func TestRenderRefuses(t *testing.T) {
	events, _ := decodeEvents([]byte(ticketEvent), "")
	cases := []struct {
		name string
		src  Source
		want string
	}{
		{"missing field", Source{Key: "{{.ticket}}"}, `no entry for key "ticket"`},
		{"null field", Source{Key: "{{.id}}-{{.priority}}"}, "null"},
		{"empty key", Source{Key: "{{if false}}x{{end}}"}, "empty key"},
		{"long key", Source{Key: strings.Repeat("k", MaxKeyBytes+1)}, "longer than"},
		{"empty input", Source{Key: "{{.id}}", Input: "{{if false}}x{{end}}"}, "empty input"},
		{"missing field in the title", Source{Key: "{{.id}}", Title: "{{.subject}}"}, "the template title failed"},
		{"too much text", Source{Key: "{{.id}}", Input: strings.Repeat("{{json .}}", 10000)}, "longer than"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			c.src.Name, c.src.Agent = "tickets", "triage"
			src := compiled(t, c.src)
			if _, err := src.render(1, events[0]); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("render = %v, want %q", err, c.want)
			}
		})
	}
}

func TestDecodeEvents(t *testing.T) {
	cases := []struct {
		name, body, items string
		n                 int
		want              string
	}{
		{"one object", `{"id": 1}`, "", 1, ""},
		{"an array", `[{"id": 1}, {"id": 2}]`, "", 2, ""},
		{"an empty array", `[]`, "", 0, ""},
		{"a path", `{"result": {"tickets": [{"id": 1}, {"id": 2}, {"id": 3}]}}`, "result.tickets", 3, ""},
		{"a path to one object", `{"event": {"id": 1}}`, "event", 1, ""},
		{"a path that is missing", `{"result": {}}`, "result.tickets", 0, `finds no "result.tickets"`},
		{"a path through a value", `{"result": 3}`, "result.tickets", 0, `stops at "result"`},
		{"an element that is no object", `[{"id": 1}, 2]`, "", 0, "event 2 is not a JSON object"},
		{"a string", `"id"`, "", 0, "not a JSON object or an array"},
		{"not JSON", `id=1`, "", 0, "not JSON"},
		{"two values", `{"id": 1} {"id": 2}`, "", 0, "more than one JSON value"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			events, err := decodeEvents([]byte(c.body), c.items)
			if c.want != "" {
				if err == nil || !strings.Contains(err.Error(), c.want) {
					t.Fatalf("decodeEvents = %v, want %q", err, c.want)
				}
				return
			}
			if err != nil || len(events) != c.n {
				t.Fatalf("decodeEvents = %d events, %v", len(events), err)
			}
		})
	}
}

func TestVerifyBearer(t *testing.T) {
	v := verifier{mode: AuthBearer, secret: []byte("tok-123")}
	for _, c := range []struct {
		header string
		ok     bool
	}{{"Bearer tok-123", true}, {"bearer tok-123", true}, {"Bearer tok-1234", false}, {"Bearer ", false}, {"tok-123", false}, {"", false}} {
		r, _ := http.NewRequest(http.MethodPost, "/", nil)
		if c.header != "" {
			r.Header.Set("Authorization", c.header)
		}
		if got := v.headers(r); got != c.ok {
			t.Errorf("headers(%q) = %v, want %v", c.header, got, c.ok)
		}
	}
	if !(verifier{mode: AuthNone}).headers(&http.Request{Header: http.Header{}}) {
		t.Fatal("none refuses a request")
	}
}

func TestVerifyHMAC(t *testing.T) {
	key, body := []byte("k3y"), []byte(`{"id": 1}`)
	mac := hmac.New(sha256.New, key)
	mac.Write(body)
	sum := mac.Sum(nil)
	v := verifier{mode: AuthHMAC, secret: key, header: "X-Signature-256"}
	for _, c := range []struct {
		sig string
		ok  bool
	}{
		{hex.EncodeToString(sum), true},
		{"sha256=" + hex.EncodeToString(sum), true},
		{"SHA256=" + strings.ToUpper(hex.EncodeToString(sum)), true},
		{base64.StdEncoding.EncodeToString(sum), true},
		{base64.RawURLEncoding.EncodeToString(sum), true},
		{hex.EncodeToString(sum[:31]), false},
		{"sha256=" + hex.EncodeToString(make([]byte, 32)), false},
		{"", false},
	} {
		r, _ := http.NewRequest(http.MethodPost, "/", nil)
		r.Header.Set("X-Signature-256", c.sig)
		if got := v.body(r, body); got != c.ok {
			t.Errorf("body(%q) = %v, want %v", c.sig, got, c.ok)
		}
	}
	r, _ := http.NewRequest(http.MethodPost, "/", nil)
	r.Header.Set("X-Signature-256", hex.EncodeToString(sum))
	if v.body(r, []byte(`{"id": 2}`)) {
		t.Fatal("the signature of another body passes")
	}
}
