package sources

// This file lends the poll and the credential checks of the sources to
// the other packages of the workbench, such as the duties, which read an
// endpoint and check the caller of a webhook in the same way as a source.

import (
	"context"
	"net/http"
	"time"
)

// Verifier checks the credential of one request to a webhook, as a
// webhook source checks it (auth.go). Build it with NewVerifier.
type Verifier struct{ v verifier }

// NewVerifier returns the check of the mode AuthBearer, AuthHMAC, or
// AuthNone with the secret. header names the header of an HMAC signature,
// DefaultSignatureHeader when it is empty.
func NewVerifier(mode, secret, header string) Verifier {
	if mode == AuthHMAC && header == "" {
		header = DefaultSignatureHeader
	}
	return Verifier{v: verifier{mode: mode, secret: []byte(secret), header: header}}
}

// Headers reports whether the headers carry the bearer token. It is true
// for the other modes. It runs before the body is read.
func (v Verifier) Headers(r *http.Request) bool { return v.v.headers(r) }

// Body reports whether the signature header holds the HMAC-SHA256 of the
// body. It is true for the other modes.
func (v Verifier) Body(r *http.Request, body []byte) bool { return v.v.body(r, body) }

// Challenge is the WWW-Authenticate header of a 401, or "".
func (v Verifier) Challenge() string { return v.v.challenge() }

// SignatureHeader names the header of the HMAC signature.
func (v Verifier) SignatureHeader() string { return v.v.header }

// Fetch reads the answer of an endpoint with one GET, as a poll source
// reads it: at most limit bytes of a 2xx answer within timeout, with the
// credential value in header, or as "Authorization: Bearer <value>" when
// header is empty. A nil client follows a redirect only to the same
// scheme and host (SameHost). An error holds no address and no
// credential, so the caller names the host.
func Fetch(ctx context.Context, client *http.Client, url, header, value string, timeout time.Duration, limit int) ([]byte, error) {
	if client == nil {
		client = &http.Client{CheckRedirect: SameHost}
	}
	return get(ctx, client, url, header, value, timeout, limit)
}

// SameHost is the redirect rule of a poll, for http.Client.CheckRedirect:
// a redirect only to the same scheme and host, at most 5 times, so a
// credential in a header never reaches another host.
func SameHost(req *http.Request, via []*http.Request) error { return sameHost(req, via) }

// Events reads the items of a JSON document as a source reads its events:
// the value at the dotted path items, or the document itself when items
// is empty. An array holds one item per element, and an object is one
// item. Each item must be an object, and numbers keep every digit.
func Events(body []byte, items string) ([]map[string]any, error) { return decodeEvents(body, items) }
