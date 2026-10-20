package sources

// This file checks the caller of a webhook source. The route sits outside
// the authentication of the native API, because an outside system holds
// no token of a person, so each source checks its own credential: a
// bearer token, or an HMAC-SHA256 signature of the body. Every compare
// takes constant time, and no secret enters a log line or an answer.

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"strings"
)

// verifier checks the credential of one request of a webhook source.
// The bearer check reads the headers only, so it runs before the body is
// read; the signature check needs the body.
type verifier struct {
	mode   string
	secret []byte // the token or the HMAC key; empty for none
	header string // the header of the signature
}

// headers reports whether the headers carry the bearer token. It is true
// for the other modes.
func (v verifier) headers(r *http.Request) bool {
	if v.mode != AuthBearer {
		return true
	}
	scheme, tok, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return false
	}
	// Equal digests have equal lengths, so the compare tells nothing of
	// the length of the token.
	got, want := sha256.Sum256([]byte(strings.TrimSpace(tok))), sha256.Sum256(v.secret)
	return subtle.ConstantTimeCompare(got[:], want[:]) == 1
}

// body reports whether the signature header holds the HMAC-SHA256 of the
// body. It is true for the other modes. The signature is hex or base64,
// with an optional "sha256=" in front.
func (v verifier) body(r *http.Request, body []byte) bool {
	if v.mode != AuthHMAC {
		return true
	}
	sig := strings.TrimSpace(r.Header.Get(v.header))
	if len(sig) > 7 && strings.EqualFold(sig[:7], "sha256=") {
		sig = sig[7:]
	}
	got := decodeSignature(sig)
	if got == nil {
		return false
	}
	mac := hmac.New(sha256.New, v.secret)
	mac.Write(body)
	return hmac.Equal(got, mac.Sum(nil))
}

// decodeSignature reads a signature in hex or in base64, or returns nil.
func decodeSignature(s string) []byte {
	if b, err := hex.DecodeString(s); err == nil && len(b) == sha256.Size {
		return b
	}
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if b, err := enc.DecodeString(s); err == nil && len(b) == sha256.Size {
			return b
		}
	}
	return nil
}

// challenge is the WWW-Authenticate header of a 401 of the mode, or "".
func (v verifier) challenge() string {
	if v.mode == AuthBearer {
		return `Bearer realm="fw-source"`
	}
	return ""
}
