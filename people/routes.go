package people

import (
	"errors"
	"net/http"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/surface/native"
)

// Me is the answer of GET and PUT /v1/people/me: the subject of the
// caller, its display name, and what the front knows of its name.
type Me struct {
	Subject string `json:"subject"`
	Display string `json:"display"`
	Claim   string `json:"claim,omitempty"`
	Email   string `json:"email,omitempty"`
	// Name is the name that the person set, or "".
	Name string `json:"name,omitempty"`
	// CanSet is false for the static token and on a front that keeps no
	// names.
	CanSet bool `json:"canSet"`
}

// meBody is the body of PUT /v1/people/me: the name to show, or "" to
// clear it.
type meBody struct {
	Name string `json:"name"`
}

// mount serves GET /v1/people/me and PUT /v1/people/me.
func mount(k *native.Kit) error {
	a := k.Agent()
	me := func(r *http.Request) (Me, bool) {
		p := fw.PrincipalFrom(r.Context())
		if p == nil || p.Subject == "" {
			return Me{}, false
		}
		Learn(r.Context(), a, p)
		rec := Of(a).Get(r.Context(), p.Subject)
		return Me{Subject: p.Subject, Display: Display(r.Context(), a, p.Subject, p.Name), Claim: rec.Claim, Email: rec.Email, Name: rec.Set,
			CanSet: Of(a) != nil && !IsToken(p.Subject) && p.Kind != fw.KindToken && p.Kind != fw.KindWorkload}, true
	}
	k.Handle("GET /v1/people/me", func(w http.ResponseWriter, r *http.Request) {
		m, ok := me(r)
		if !ok {
			native.WriteProblem(w, r, http.StatusUnauthorized, "sign in first")
			return
		}
		native.WriteJSON(w, http.StatusOK, m)
	})
	k.Handle("PUT /v1/people/me", func(w http.ResponseWriter, r *http.Request) {
		var body meBody
		if !k.Decode(w, r, &body) {
			return
		}
		m, ok := me(r)
		switch {
		case !ok:
			native.WriteProblem(w, r, http.StatusUnauthorized, "sign in first")
			return
		case !m.CanSet && Of(a) != nil:
			native.WriteProblem(w, r, http.StatusForbidden, "the admin token has a fixed name")
			return
		}
		if _, err := SetName(r.Context(), a, m.Subject, body.Name); err != nil {
			switch {
			case errors.Is(err, ErrOff):
				native.WriteProblem(w, r, http.StatusServiceUnavailable, "This server keeps no names, because its bus keeps no store.")
			case errors.Is(err, fw.ErrInvalidRequest):
				native.WriteProblem(w, r, http.StatusBadRequest, "A name holds at most 64 characters of one line.")
			default:
				k.Fail(w, r, err)
			}
			return
		}
		m, _ = me(r)
		native.WriteJSON(w, http.StatusOK, m)
	})
	return nil
}
