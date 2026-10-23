package sources

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The lent checks behave as those of a source: the bearer token in the
// headers, the HMAC of the body, and the poll with its credential and its
// limit.
func TestLend(t *testing.T) {
	bearer := NewVerifier(AuthBearer, "s3cret", "")
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	if bearer.Headers(r) || bearer.Challenge() == "" {
		t.Fatal("a request without a token passed")
	}
	r.Header.Set("Authorization", "Bearer s3cret")
	if !bearer.Headers(r) || !bearer.Body(r, nil) {
		t.Fatal("the right token failed")
	}
	sig := NewVerifier(AuthHMAC, "key", "")
	if sig.SignatureHeader() != DefaultSignatureHeader {
		t.Fatalf("the header = %q", sig.SignatureHeader())
	}
	mac := hmac.New(sha256.New, []byte("key"))
	mac.Write([]byte("body"))
	r.Header.Set(DefaultSignatureHeader, "sha256="+hex.EncodeToString(mac.Sum(nil)))
	if !sig.Headers(r) || !sig.Body(r, []byte("body")) || sig.Body(r, []byte("other")) {
		t.Fatal("the HMAC check is wrong")
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Key") != "k" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"result": {"records": [{"n": 1}, {"n": 2}]}}`))
	}))
	defer srv.Close()
	ctx := context.Background()
	if _, err := Fetch(ctx, nil, srv.URL, "X-Key", "wrong", time.Second, 1<<10); err == nil || !strings.Contains(err.Error(), "answered 401") {
		t.Fatalf("Fetch with a wrong key = %v", err)
	}
	if _, err := Fetch(ctx, nil, srv.URL, "X-Key", "k", time.Second, 10); err == nil || !strings.Contains(err.Error(), "larger than 10 bytes") {
		t.Fatalf("Fetch over the limit = %v", err)
	}
	body, err := Fetch(ctx, nil, srv.URL, "X-Key", "k", time.Second, 1<<10)
	if err != nil {
		t.Fatal(err)
	}
	items, err := Events(body, "result.records")
	if err != nil || len(items) != 2 {
		t.Fatalf("Events = %v, %v", items, err)
	}
}
