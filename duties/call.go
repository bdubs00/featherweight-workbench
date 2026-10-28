package duties

// This file serves the call of the webhook of a duty, POST
// /v1/duties/{name}/call (docs/ARCHITECTURE.md section 22.9). The route
// sits outside the authentication of the native API, because another app
// holds no token of a person: the duty checks its own credential, as a
// webhook source does (sources.Verifier). The body is the event, for the
// placeholder {event}. A call with the key of an earlier call within a
// week, or without a key the same body within ten minutes, is a
// duplicate.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/bdubs00/featherweight-kernel/surface/native"
	"github.com/bdubs00/featherweight-workbench/sources"
)

// Windows of the de-duplication of a call.
const (
	// CallKeyWindow is how long the key of a call blocks a second call.
	CallKeyWindow = 7 * 24 * time.Hour
	// CallBodyWindow is how long the body of a call without a key blocks a
	// second call with the same body.
	CallBodyWindow = 10 * time.Minute
	// callTimeout bounds the claim and the start of one call.
	callTimeout = 45 * time.Second
)

// serveCall answers POST /v1/duties/{name}/call: 202 with the run, 200
// with {"duplicate": true} or with the reason why the start skipped, 401
// for a wrong credential, 404 for no duty with a webhook of the name, and
// 503 when the store did not answer.
func (s *Service) serveCall(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), callTimeout)
	defer cancel()
	st, err := s.st.get(ctx, name)
	if err != nil || st.Duty.When.Webhook == nil {
		if err != nil && !errors.Is(err, ErrNotFound) {
			w.Header().Set("Retry-After", "5")
			native.WriteProblem(w, r, http.StatusServiceUnavailable, "The store of the duties did not answer.")
			return
		}
		native.WriteProblem(w, r, http.StatusNotFound, fmt.Sprintf("No duty with a webhook is named %q.", name))
		return
	}
	hook := st.Duty.When.Webhook
	secret := ""
	if hook.Auth.Mode != sources.AuthNone {
		v, err := s.secret(ctx, hook.Auth.Secret)
		if err != nil {
			s.log.Warn("duties: the secret of a webhook is not set, so its calls fail", "duty", name, "variable", hook.Auth.Secret)
			w.Header().Set("Retry-After", "60")
			native.WriteProblem(w, r, http.StatusServiceUnavailable, "The secret of the webhook is not set in this server.")
			return
		}
		secret = v
	}
	verify := sources.NewVerifier(hook.Auth.Mode, secret, hook.Auth.Header)
	if !verify.Headers(r) {
		if c := verify.Challenge(); c != "" {
			w.Header().Set("WWW-Authenticate", c)
		}
		s.log.Warn("duties: refused a call", "duty", name, "reason", "the bearer token was missing or wrong")
		native.WriteProblem(w, r, http.StatusUnauthorized, "The duty needs its bearer token.")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, native.MaxBodyBytes))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			native.WriteProblem(w, r, http.StatusRequestEntityTooLarge, fmt.Sprintf("The body is larger than %d bytes.", native.MaxBodyBytes))
			return
		}
		native.WriteProblem(w, r, http.StatusBadRequest, "The body did not read.")
		return
	}
	if !verify.Body(r, body) {
		s.log.Warn("duties: refused a call", "duty", name, "reason", "the signature was missing or wrong")
		native.WriteProblem(w, r, http.StatusUnauthorized, fmt.Sprintf("The duty needs the HMAC-SHA256 signature of the body in the header %s.", verify.SignatureHeader()))
		return
	}
	if st.templated() {
		// The templates of the entry make one run per event of the body,
		// with the claims of the sources, and the answer is the answer of
		// the entry (imported.go).
		if blocked, why := s.stopped(ctx, st); blocked {
			native.WriteJSON(w, http.StatusOK, map[string]string{"skipped": why})
			return
		}
		got := sources.ServeEvents(w, r.WithContext(ctx), s.a, st.sourceOf(s.heldBack), body)
		if len(got.Runs) > 0 {
			s.recordDelivery(ctx, st, StartCall, got)
		}
		s.log.Info("duties: a call delivered the events of a duty", "duty", name, "runs", len(got.Runs), "duplicates", got.Duplicates)
		return
	}
	if events, err := sources.Events(body, ""); err == nil {
		itemsOf(ctx, events)
	}
	key, window := "body:"+string(body), CallBodyWindow
	if hook.Key != "" {
		k, err := bodyKey(body, hook.Key)
		if err != nil {
			native.WriteProblem(w, r, http.StatusBadRequest, capital(err.Error())+".")
			return
		}
		key, window = "key:"+k, CallKeyWindow
	}
	if blocked, why := s.stopped(ctx, st); blocked {
		native.WriteJSON(w, http.StatusOK, map[string]string{"skipped": why})
		return
	}
	fresh, release, err := s.st.takeCall(ctx, name, key, window)
	if err != nil {
		w.Header().Set("Retry-After", "5")
		native.WriteProblem(w, r, http.StatusServiceUnavailable, "The store of the duties did not answer, so the call started nothing. Send it again.")
		return
	}
	if !fresh {
		native.WriteJSON(w, http.StatusOK, map[string]bool{"duplicate": true})
		return
	}
	out, err := s.begin(ctx, st, Trigger{Kind: StartCall, Slot: s.now().UTC(), Event: eventText(body)})
	if err != nil {
		release()
		w.Header().Set("Retry-After", "5")
		native.WriteProblem(w, r, http.StatusServiceUnavailable, "The store of the duties did not answer, so the call started nothing. Send it again.")
		return
	}
	if out.Failed != "" && !out.retrying {
		// Nothing goes again by itself, so the sender may.
		release()
	}
	s.log.Info("duties: a call started a duty", "duty", name, "run", out.Run, "skipped", out.Skipped, "failed", out.Failed)
	writeOutcome(w, r, out)
}

// bodyKey reads the key of a call at the dotted path in a JSON body.
func bodyKey(body []byte, path string) (string, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var doc map[string]any
	if err := dec.Decode(&doc); err != nil {
		return "", fmt.Errorf("the body is not a JSON object, and the duty reads its key %s from it", path)
	}
	k, ok := keyOf(doc, path)
	if !ok {
		return "", fmt.Errorf("the body has no %s, which is the key of a call of this duty", path)
	}
	return k, nil
}

// eventText is the body of a call for {event}: compact JSON, or the text
// as it is when it is not JSON.
func eventText(body []byte) string {
	var b bytes.Buffer
	if json.Compact(&b, body) == nil {
		return b.String()
	}
	return string(body)
}
