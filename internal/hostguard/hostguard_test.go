package hostguard

import (
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

// A glob matches the host in lower case, and a * crosses dots.
func TestMatch(t *testing.T) {
	for _, c := range []struct {
		globs []string
		host  string
		want  bool
	}{
		{[]string{"*.example.com"}, "a.b.example.com", true},
		{[]string{"*.example.com"}, "example.com", false},
		{[]string{"ntfy.lab.internal"}, "NTFY.lab.internal", true},
		{nil, "example.com", false},
	} {
		if got := Match(c.globs, c.host); got != c.want {
			t.Errorf("Match(%v, %q) = %v", c.globs, c.host, got)
		}
	}
	if g, ok := CleanGlob(" *.Example.COM "); !ok || g != "*.example.com" {
		t.Errorf("CleanGlob = %q, %v", g, ok)
	}
	for _, bad := range []string{"", "  ", "["} {
		if _, ok := CleanGlob(bad); ok {
			t.Errorf("CleanGlob(%q) passed", bad)
		}
	}
}

// Private names localhost and the literal private addresses, and Blocked
// refuses them.
func TestPrivate(t *testing.T) {
	for host, want := range map[string]bool{
		"localhost": true, "a.localhost": true, "127.0.0.1": true, "10.1.2.3": true, "192.168.0.9": true,
		"169.254.1.1": true, "[::1]": true, "fd00::1": true, "0.0.0.0": true,
		"example.com": false, "1.1.1.1": false, "2606:4700::1111": false,
	} {
		if got := Private(host); got != want {
			t.Errorf("Private(%q) = %v", host, got)
		}
	}
	if !Blocked(nil) || Blocked(net.ParseIP("8.8.8.8")) {
		t.Error("Blocked does not fit")
	}
}

// The guarded transport refuses a loopback address after the name
// resolves, and the open one reaches it.
func TestTransport(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	t.Cleanup(srv.Close)
	refuse := errors.New("refused")
	_, err := (&http.Client{Transport: Transport(refuse)}).Get(srv.URL)
	if !errors.Is(err, refuse) {
		t.Fatalf("the guarded transport = %v", err)
	}
	resp, err := (&http.Client{Transport: Transport(nil)}).Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("the open transport = %d", resp.StatusCode)
	}
}
