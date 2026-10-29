package people

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/surface/native"
	"github.com/bdubs00/featherweight-workbench/internal/bucket"
)

// The part of an email before the @ reads as words with capitals.
func TestReadable(t *testing.T) {
	for in, want := range map[string]string{
		"brandon.winston.a@example.com": "Brandon Winston A",
		"erin@example.com":              "Erin",
		"j_doe-smith+lab@example.org":   "J Doe Smith",
		"1234@example.com":              "",
	} {
		if got := Readable(in); got != want {
			t.Errorf("Readable(%q) = %q, want %q", in, got, want)
		}
	}
}

// The display name follows its order: the name claim, the name that the
// person set, the email in readable form, a hint that is a name, the
// subject when it is no id, and else "A person". The token is "Admin
// token", and a UUID never shows.
func TestDisplay(t *testing.T) {
	ctx := context.Background()
	s := NewStore(bucket.NewMem(bucketConfig))
	const uuid = "fd1e8a14-ac12-5a00-b4d6-bb6e50d15c10"
	if got := s.Display(ctx, uuid, ""); got != SomePerson {
		t.Fatalf("an unknown id = %q", got)
	}
	if got := s.Display(ctx, uuid, "brandon.winston.a@example.com"); got != "Brandon Winston A" {
		t.Fatalf("an id with the email of a record = %q", got)
	}
	s.learn(ctx, uuid, "", "erin@example.com")
	if got := s.Display(ctx, uuid, ""); got != "Erin" {
		t.Fatalf("the learned email = %q", got)
	}
	if _, err := s.change(ctx, uuid, func(r *Record) bool { r.Set = "E. Example"; return true }); err != nil {
		t.Fatal(err)
	}
	if got := s.Display(ctx, uuid, ""); got != "E. Example" {
		t.Fatalf("the name that the person set = %q", got)
	}
	s.learn(ctx, uuid, "Erin Example", "")
	if got := s.Display(ctx, uuid, "someone@example.com"); got != "Erin Example" {
		t.Fatalf("the name claim = %q", got)
	}
	for subject, want := range map[string]string{"token": AdminToken, "alice": "alice", "": SomePerson} {
		if got := s.Display(ctx, subject, ""); got != want {
			t.Errorf("Display(%q) = %q, want %q", subject, got, want)
		}
	}
	var none *Store
	if got := none.Display(ctx, "bob", "Bob Builder"); got != "Bob Builder" {
		t.Fatalf("a hint without a store = %q", got)
	}
	// A second replica reads the record from the bucket.
	other := NewStore(s.b)
	if got := other.Display(ctx, uuid, ""); got != "Erin Example" {
		t.Fatalf("the other replica = %q", got)
	}
}

// The name of a principal teaches the email or the name claim, the claims
// of an ID token teach both, and the static token teaches nothing.
func TestLearn(t *testing.T) {
	a := newAgent(t)
	ctx := context.Background()
	Learn(ctx, a, &fw.Principal{Subject: "u-1", Name: "erin@example.com", Kind: fw.KindUser})
	Learn(ctx, a, &fw.Principal{Subject: "u-2", Name: "Frank Miller", Kind: fw.KindUser})
	Learn(ctx, a, &fw.Principal{Subject: "token", Name: "Static", Kind: fw.KindToken})
	LearnClaims(ctx, a, "u-3", map[string]any{"name": "Gina Lee", "email": "gina@example.com", "sub": "u-3"})
	LearnClaims(ctx, a, "u-4", map[string]any{"given_name": "Hal", "family_name": "Jordan"})
	for subject, want := range map[string]string{"u-1": "Erin", "u-2": "Frank Miller", "token": AdminToken, "u-3": "Gina Lee", "u-4": "Hal Jordan"} {
		if got := Display(ctx, a, subject, ""); got != want {
			t.Errorf("Display(%q) = %q, want %q", subject, got, want)
		}
	}
	if rec := Of(a).Get(ctx, "token"); rec.Claim != "" || rec.Email != "" {
		t.Fatalf("the token taught %+v", rec)
	}
}

// A person reads and sets the name to show. The token reads Admin token
// and cannot set a name, and a name that is too long does not fit.
func TestMeRoute(t *testing.T) {
	a := newAgent(t)
	h, err := a.Handler()
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	do := func(method, token, body string) (int, string) {
		t.Helper()
		req, _ := http.NewRequest(method, srv.URL+"/v1/people/me", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if code, body := do("GET", "erin", ""); code != http.StatusOK || !strings.Contains(body, `"display":"Erin"`) || !strings.Contains(body, `"canSet":true`) {
		t.Fatalf("GET = %d %s", code, body)
	}
	if code, body := do("PUT", "erin", `{"name":"Erin E."}`); code != http.StatusOK || !strings.Contains(body, `"display":"Erin E."`) || !strings.Contains(body, `"name":"Erin E."`) {
		t.Fatalf("PUT = %d %s", code, body)
	}
	if code, body := do("PUT", "erin", `{"name":"`+strings.Repeat("x", MaxName+1)+`"}`); code != http.StatusBadRequest {
		t.Fatalf("a long name = %d %s", code, body)
	}
	if code, body := do("GET", "token", ""); code != http.StatusOK || !strings.Contains(body, `"display":"Admin token"`) || !strings.Contains(body, `"canSet":false`) {
		t.Fatalf("GET as the token = %d %s", code, body)
	}
	if code, body := do("PUT", "token", `{"name":"Root"}`); code != http.StatusForbidden {
		t.Fatalf("PUT as the token = %d %s", code, body)
	}
}

// signins takes the bearer token as the subject: token is the static
// token, and any other word a person with the email word@example.com.
type signins struct{}

func (signins) Authenticate(r *http.Request) (*fw.Principal, error) {
	tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	switch tok {
	case "", r.Header.Get("Authorization"):
		return nil, fw.ErrUnauthorized
	case "token":
		return &fw.Principal{Subject: "token", Kind: fw.KindToken}, nil
	}
	return &fw.Principal{Subject: "id-" + tok, Name: tok + "@example.com", Kind: fw.KindUser}, nil
}

func (signins) Challenge(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusUnauthorized)
}

// newAgent is a kernel with the store of the names on a bucket of the
// memory and the native surface.
func newAgent(t *testing.T) *fw.Agent {
	t.Helper()
	cfg := fw.DefaultConfig()
	cfg.Home = t.TempDir()
	a, err := fw.New(fw.WithConfig(cfg), fw.WithoutDefaults(), fw.WithAuthn(signins{}), fw.WithLedger(fw.NewMemLedger()),
		fw.WithBus(fw.NewMemBus()), fw.WithSurfaces(native.New()), fw.WithLogger(slog.New(slog.DiscardHandler)))
	if err != nil {
		t.Fatal(err)
	}
	Attach(a, NewStore(bucket.NewMem(bucketConfig)))
	t.Cleanup(func() {
		Attach(a, nil)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		a.Close(ctx)
	})
	return a
}
